package metadata

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsPublicIPRejectsSpecialPurposeRanges(t *testing.T) {
	tests := []string{
		"0.1.2.3", "10.0.0.1", "100.64.0.1", "192.0.2.1", "198.51.100.1",
		"203.0.113.1", "240.0.0.1", "2001:db8::1", "fc00::1", "fe80::1",
	}
	for _, value := range tests {
		if IsPublicIP(net.ParseIP(value)) {
			t.Errorf("isPublicIP(%q) = true", value)
		}
	}
	for _, value := range []string{"1.1.1.1", "2001:4860:4860::8888"} {
		if !IsPublicIP(net.ParseIP(value)) {
			t.Errorf("isPublicIP(%q) = false", value)
		}
	}
}

func TestFetcherPreservesInitialFragment(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(httpHandler(t, ""))
	t.Cleanup(server.Close)
	fetcher := testFetcher(t, server)

	document, err := fetcher.Fetch(context.Background(), server.URL+"/page#section")
	if err != nil {
		t.Fatal(err)
	}
	if document.URL.Fragment != "section" {
		t.Fatalf("final fragment = %q, want %q", document.URL.Fragment, "section")
	}
}

func TestFetcherPreservesFragmentFromRedirectTarget(t *testing.T) {
	t.Parallel()
	final := httptest.NewServer(httpHandler(t, ""))
	t.Cleanup(final.Close)
	redirect := httptest.NewServer(httpHandler(t, final.URL+"/page#redirected"))
	t.Cleanup(redirect.Close)
	fetcher := testFetcher(t, redirect)

	document, err := fetcher.Fetch(context.Background(), redirect.URL+"/start#initial")
	if err != nil {
		t.Fatal(err)
	}
	if document.URL.Fragment != "redirected" {
		t.Fatalf("final fragment = %q, want %q", document.URL.Fragment, "redirected")
	}
}

func httpHandler(t *testing.T, location string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Fragment != "" {
			t.Errorf("server received fragment %q", request.URL.Fragment)
		}
		if location != "" {
			http.Redirect(writer, request, location, http.StatusFound)
			return
		}
		writer.WriteHeader(http.StatusOK)
	})
}

func testFetcher(t *testing.T, server *httptest.Server) *Fetcher {
	t.Helper()
	fetcher, err := NewFetcher(Config{
		RequestTimeout: time.Second,
		TotalTimeout:   5 * time.Second,
		MaxBodyBytes:   1 << 20,
		MaxRedirects:   5,
		UserAgent:      "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	redirectCheck := fetcher.client.CheckRedirect
	fetcher.client = server.Client()
	fetcher.client.CheckRedirect = redirectCheck
	return fetcher
}
