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

func TestAMPResolverUsesCanonicalLink(t *testing.T) {
	t.Parallel()
	_, amp, err := link.NewGoogleResolvers(fakeFetcher{result: link.FetchResult{
		URL:  mustURL(t, "https://amp.news.example/story"),
		Body: []byte(`<link href="https://news.example/article" rel="canonical">`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := amp.Resolve(context.Background(), mustURL(t, "https://amp.news.example/story"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resolution.Destination.String(), "https://news.example/article"; got != want {
		t.Errorf("AMPResolver.Resolve() destination = %q, want %q", got, want)
	}
}

func TestAMPResolverUnwrapsCachesWithoutFetch(t *testing.T) {
	t.Parallel()
	_, amp, err := link.NewGoogleResolvers(fakeFetcher{err: context.Canceled})
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"https://www.google.com/amp/s/news.example/story":                "https://news.example/story",
		"https://news-example.cdn.ampproject.org/c/s/news.example/story": "https://news.example/story",
	}
	for input, want := range tests {
		resolution, resolveErr := amp.Resolve(context.Background(), mustURL(t, input))
		if resolveErr != nil {
			t.Errorf("AMPResolver.Resolve(%q) error = %v", input, resolveErr)
			continue
		}
		if got := resolution.Destination.String(); got != want {
			t.Errorf("AMPResolver.Resolve(%q) destination = %q, want %q", input, got, want)
		}
	}
}

func TestGoogleShareResolverFollowsRedirect(t *testing.T) {
	t.Parallel()
	share, _, err := link.NewGoogleResolvers(fakeFetcher{result: link.FetchResult{
		URL: mustURL(t, "https://news.example/redirected"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := share.Resolve(context.Background(), mustURL(t, "https://share.google/abc"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resolution.Destination.String(), "https://news.example/redirected"; got != want {
		t.Errorf("GoogleShareResolver.Resolve() destination = %q, want %q", got, want)
	}
}

func TestRegistryAppliesTrackingScrubberAfterWrapperResolution(t *testing.T) {
	t.Parallel()
	const wrapperResolver = "google-share"
	share, amp, err := link.NewGoogleResolvers(fakeFetcher{result: link.FetchResult{
		URL: mustURL(t, "https://is.fi/story?shem=share-id&utm_source=telegram"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := link.NewRegistry(share, amp, link.TrackingParameterResolver{})
	if err != nil {
		t.Fatal(err)
	}

	resolution, matched, err := registry.Resolve(context.Background(), "https://share.google/abc")
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("registry did not match wrapper URL")
	}
	if got, want := resolution.Destination.String(), "https://is.fi/story"; got != want {
		t.Errorf("destination = %q, want %q", got, want)
	}
	if got, want := resolution.Resolver, wrapperResolver; got != want {
		t.Errorf("resolver = %q, want %q", got, want)
	}
}

func TestTrackingParameterResolverRemovesKnownParameters(t *testing.T) {
	t.Parallel()
	resolver := link.TrackingParameterResolver{}
	tests := map[string]string{
		"https://www.is.fi/hyvaolo/art-2000011975993.html?shem=dsdf,sharefoc": "https://www.is.fi/hyvaolo/art-2000011975993.html",
		"https://youtu.be/video?t=90s&si=share-id":                            "https://youtu.be/video?t=90s",
		"https://youtube.com/watch?v=video&list=playlist&pp=share":            "https://youtube.com/watch?list=playlist&v=video",
		"https://example.com/article?shem=share-id&view=full":                 "https://example.com/article?view=full",
		"https://example.com/article?utm_source=newsletter&view=full":         "https://example.com/article?view=full",
	}
	for input, want := range tests {
		candidate := mustURL(t, input)
		if !resolver.Match(candidate) {
			t.Errorf("TrackingParameterResolver.Match(%q) = false, want true", input)
			continue
		}
		resolution, err := resolver.Resolve(context.Background(), candidate)
		if err != nil {
			t.Errorf("TrackingParameterResolver.Resolve(%q) error = %v", input, err)
			continue
		}
		if got := resolution.Destination.String(); got != want {
			t.Errorf("TrackingParameterResolver.Resolve(%q) destination = %q, want %q", input, got, want)
		}
	}
}

func TestTrackingParameterResolverPreservesUnknownParameters(t *testing.T) {
	t.Parallel()
	resolver := link.TrackingParameterResolver{}
	for _, raw := range []string{
		"https://example.com/search?s=golang",
		"https://youtube.com/watch?v=video&t=90s",
		"https://is.fi/article?foo=bar",
	} {
		if resolver.Match(mustURL(t, raw)) {
			t.Errorf("TrackingParameterResolver.Match(%q) = true, want false", raw)
		}
	}
}

func TestResolversMatchRealURLFamilies(t *testing.T) {
	t.Parallel()
	share, amp, err := link.NewGoogleResolvers(fakeFetcher{})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"https://share.google/abc", "https://goo.gl/maps/abc"} {
		if !share.Match(mustURL(t, raw)) {
			t.Errorf("GoogleShareResolver.Match(%q) = false, want true", raw)
		}
	}
	for _, raw := range []string{
		"https://www.google.com/amp/s/news.example/story",
		"https://news-example.cdn.ampproject.org/c/s/news.example/story",
		"https://amp.news.example/story",
		"https://news.example/story/amp",
		"https://news.example/story?output=amp",
	} {
		if !amp.Match(mustURL(t, raw)) {
			t.Errorf("AMPResolver.Match(%q) = false, want true", raw)
		}
	}
	for _, raw := range []string{
		"https://share.google.attacker.test/x",
		"https://goo.gl.attacker.test/x",
		"https://news.example/champion",
		"https://www.google.com/search?q=amp",
		"https://www.google.evil/amp/s/news.example/story",
	} {
		if share.Match(mustURL(t, raw)) || amp.Match(mustURL(t, raw)) {
			t.Errorf("resolvers matched unrelated URL %q", raw)
		}
	}
}

type hostResolver struct {
	name  string
	hosts []string
}

func (r hostResolver) Name() string    { return r.name }
func (r hostResolver) Hosts() []string { return r.hosts }
func (r hostResolver) Match(u *url.URL) bool {
	for _, host := range r.hosts {
		if u != nil && strings.EqualFold(u.Hostname(), host) {
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
	_, err := link.NewRegistry(
		hostResolver{name: "first", hosts: []string{"share.google"}},
		hostResolver{name: "second", hosts: []string{"Share.Google."}},
	)
	if err == nil {
		t.Fatal("NewRegistry() error = nil, want ambiguous-host error")
	}
}

func TestRegistryReportsMatchedRule(t *testing.T) {
	t.Parallel()
	share, amp, err := link.NewGoogleResolvers(fakeFetcher{})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := link.NewRegistry(share, amp, link.TrackingParameterResolver{})
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"https://share.google/x":                         "google-share",
		"https://goo.gl/x":                               "google-share",
		"https://www.google.com/amp/s/example.com/story": "amp",
		"https://amp.example.com/story":                  "amp",
		"https://youtu.be/video?si=share-id":             "tracking-parameter",
	}
	for raw, want := range tests {
		got, matched := registry.MatchName(raw)
		if !matched || got != want {
			t.Errorf("Registry.MatchName(%q) = (%q, %t), want (%q, true)", raw, got, matched, want)
		}
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
