package link_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/sompasauna/linkkilinko/pkg/core/link"
)

type fakeFetcher struct {
	result link.FetchResult
	err    error
}

func (f fakeFetcher) Fetch(context.Context, string) (link.FetchResult, error) {
	return f.result, f.err
}

func TestGoogleAMPResolverUsesCanonicalLink(t *testing.T) {
	t.Parallel()
	parsed, err := url.Parse("https://news.example/article")
	if err != nil {
		t.Fatal(err)
	}
	_, amp, err := link.NewGoogleResolvers(fakeFetcher{
		result: link.FetchResult{
			URL:  mustURL(t, "https://amp.google.com/story"),
			Body: []byte(`<link rel="canonical" href="https://news.example/article">`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := amp.Resolve(context.Background(), mustURL(t, "https://amp.google.com/story"))
	if err != nil || resolution.Destination.String() != parsed.String() {
		t.Fatalf("resolution = %#v, err=%v", resolution, err)
	}
}

// The share resolver owns share.google.com only, so an AMP page reaching it
// must not gain the canonical-link inspection that is confined to google-amp.
func TestGoogleShareResolverIgnoresCanonicalLink(t *testing.T) {
	t.Parallel()
	share, _, err := link.NewGoogleResolvers(fakeFetcher{
		result: link.FetchResult{
			URL:  mustURL(t, "https://news.example/redirected"),
			Body: []byte(`<link rel="canonical" href="https://news.example/article">`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := share.Resolve(context.Background(), mustURL(t, "https://share.google.com/x"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resolution.Destination.String(), "https://news.example/redirected"; got != want {
		t.Fatalf("destination = %q, want the redirect target %q", got, want)
	}
	if resolution.Resolver != "google-share" {
		t.Fatalf("resolver = %q, want google-share", resolution.Resolver)
	}
}

func TestGoogleResolversRequireExactHost(t *testing.T) {
	t.Parallel()
	share, amp, err := link.NewGoogleResolvers(fakeFetcher{})
	if err != nil {
		t.Fatal(err)
	}
	if share.Match(mustURL(t, "https://share.google.com.attacker.test/x")) {
		t.Fatal("share resolver matched attacker-controlled suffix")
	}
	if amp.Match(mustURL(t, "https://amp.google.com.attacker.test/x")) {
		t.Fatal("amp resolver matched attacker-controlled suffix")
	}
	if share.Match(mustURL(t, "https://amp.google.com/story")) {
		t.Fatal("share resolver claimed the amp host")
	}
	if amp.Match(mustURL(t, "https://share.google.com/x")) {
		t.Fatal("amp resolver claimed the share host")
	}
}

func TestGoogleResolverRejectsReservedCanonicalDestination(t *testing.T) {
	t.Parallel()
	_, amp, err := link.NewGoogleResolvers(fakeFetcher{result: link.FetchResult{
		URL:  mustURL(t, "https://amp.google.com/story"),
		Body: []byte(`<link rel="canonical" href="https://192.0.2.1/private">`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := amp.Resolve(context.Background(), mustURL(t, "https://amp.google.com/story")); err == nil {
		t.Fatal("expected reserved canonical destination to be rejected")
	}
}

// Distinct resolver names, so only the host-ambiguity check can reject these
// registrations — the duplicate-name check must not be what fails.
const (
	firstResolver  = "first"
	secondResolver = "second"
	shareHost      = "share.google.com"
)

// hostResolver is a minimal HostMatcher used to drive the startup ambiguity
// check without depending on the Google resolvers' host assignments.
type hostResolver struct {
	name  string
	hosts []string
}

func (r hostResolver) Name() string { return r.name }

func (r hostResolver) Hosts() []string { return r.hosts }

func (r hostResolver) Match(u *url.URL) bool {
	if u == nil {
		return false
	}
	for _, host := range r.hosts {
		if strings.EqualFold(u.Hostname(), host) {
			return true
		}
	}
	return false
}

func (r hostResolver) Resolve(context.Context, *url.URL) (link.Resolution, error) {
	return link.Resolution{}, link.ErrNoResolution
}

func TestNewRegistryRejectsAmbiguousHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		resolvers []link.Resolver
	}{
		{
			name: "two names claim one host",
			resolvers: []link.Resolver{
				hostResolver{name: firstResolver, hosts: []string{shareHost}},
				hostResolver{name: secondResolver, hosts: []string{shareHost}},
			},
		},
		{
			name: "overlap is only one host of several",
			resolvers: []link.Resolver{
				hostResolver{name: firstResolver, hosts: []string{"a.example", "amp.google.com"}},
				hostResolver{name: secondResolver, hosts: []string{"amp.google.com", "b.example"}},
			},
		},
		{
			name: "host casing and trailing dot do not hide the overlap",
			resolvers: []link.Resolver{
				hostResolver{name: firstResolver, hosts: []string{"Share.Google.Com."}},
				hostResolver{name: secondResolver, hosts: []string{shareHost}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := link.NewRegistry(test.resolvers...); err == nil {
				t.Fatal("expected two resolvers claiming the same host to be rejected")
			}
		})
	}
}

// The shipped registration must survive the ambiguity check; google-share and
// google-amp own disjoint hosts.
func TestNewRegistryAcceptsSplitGoogleResolvers(t *testing.T) {
	t.Parallel()
	share, amp, err := link.NewGoogleResolvers(fakeFetcher{})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := link.NewRegistry(share, amp)
	if err != nil {
		t.Fatalf("registry rejected the shipped resolvers: %v", err)
	}
	if !registry.Match("https://share.google.com/x") || !registry.Match("https://amp.google.com/story") {
		t.Fatal("registry does not own both Google wrapper hosts")
	}
	if share.Name() != "google-share" || amp.Name() != "google-amp" {
		t.Fatalf("resolver names = %q, %q; want google-share, google-amp", share.Name(), amp.Name())
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
