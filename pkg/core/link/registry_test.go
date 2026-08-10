package link_test

import (
	"context"
	"net/url"
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

func TestGoogleResolverUsesCanonicalLink(t *testing.T) {
	t.Parallel()
	parsed, err := url.Parse("https://news.example/article")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := link.NewGoogleResolver(fakeFetcher{
		result: link.FetchResult{
			URL:  mustURL(t, "https://amp.google.com/story"),
			Body: []byte(`<link rel="canonical" href="https://news.example/article">`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := resolver.Resolve(context.Background(), mustURL(t, "https://amp.google.com/story"))
	if err != nil || resolution.Destination.String() != parsed.String() {
		t.Fatalf("resolution = %#v, err=%v", resolution, err)
	}
}

func TestGoogleResolverRequiresExactHost(t *testing.T) {
	t.Parallel()
	resolver, err := link.NewGoogleResolver(fakeFetcher{})
	if err != nil {
		t.Fatal(err)
	}
	if resolver.Match(mustURL(t, "https://share.google.com.attacker.test/x")) {
		t.Fatal("matched attacker-controlled suffix")
	}
}

func TestGoogleResolverRejectsReservedCanonicalDestination(t *testing.T) {
	resolver, err := link.NewGoogleResolver(fakeFetcher{result: link.FetchResult{
		URL:  mustURL(t, "https://amp.google.com/story"),
		Body: []byte(`<link rel="canonical" href="https://192.0.2.1/private">`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(context.Background(), mustURL(t, "https://amp.google.com/story")); err == nil {
		t.Fatal("expected reserved canonical destination to be rejected")
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
