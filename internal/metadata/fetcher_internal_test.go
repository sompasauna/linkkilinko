package metadata

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
	"time"
)

const testUserAgent = "test"

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
		UserAgent:      testUserAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	redirectCheck := fetcher.client.CheckRedirect
	fetcher.client = server.Client()
	fetcher.client.CheckRedirect = redirectCheck
	return fetcher
}

func TestFetcherMultiAddressHostWithinTotalBudget(t *testing.T) {
	t.Parallel()
	fetcher, err := NewFetcher(Config{
		RequestTimeout: 50 * time.Millisecond,
		TotalTimeout:   250 * time.Millisecond,
		MaxBodyBytes:   1 << 20,
		MaxRedirects:   5,
		UserAgent:      testUserAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	lc := &net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ln.Close()
	url := "http://unreachable-host:" + strconv.Itoa(12345)
	ctx := context.Background()
	start := time.Now()
	_, fetchErr := fetcher.Fetch(ctx, url)
	elapsed := time.Since(start)
	if fetchErr == nil {
		t.Fatal("expected error for unreachable host, got nil")
	}
	if elapsed >= 300*time.Millisecond {
		t.Errorf("fetch took %v, should fail before totalTimeout (250ms)", elapsed)
	}
}

func TestFetcherNewFetcherRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	_, err := NewFetcher(Config{
		RequestTimeout: 0,
		TotalTimeout:   time.Second,
		MaxBodyBytes:   1 << 20,
		MaxRedirects:   5,
		UserAgent:      testUserAgent,
	})
	if err == nil {
		t.Fatal("expected error for zero request timeout")
	}
	if err.Error() != "metadata: request timeout must be positive" {
		t.Errorf("unexpected error: %v", err)
	}
	_, err = NewFetcher(Config{
		RequestTimeout: time.Second,
		TotalTimeout:   time.Second,
		MaxBodyBytes:   1 << 20,
		MaxRedirects:   5,
		UserAgent:      testUserAgent,
	})
	if err == nil {
		t.Fatal("expected error for request >= total timeout")
	}
	if err.Error() != "metadata: request timeout must be less than total timeout" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFetcherUsesRequestTimeoutForTransport(t *testing.T) {
	t.Parallel()
	fetcher, err := NewFetcher(Config{
		RequestTimeout: 1 * time.Second,
		TotalTimeout:   10 * time.Second,
		MaxBodyBytes:   1 << 20,
		MaxRedirects:   5,
		UserAgent:      testUserAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	lc := &net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addrPort, err := netip.ParseAddrPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			if _, err := conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")); err != nil {
				return
			}
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, conn)
		}
	}()
	ctx := context.Background()
	_, fetchErr := fetcher.Fetch(ctx, "http://"+addrPort.String())
	if fetchErr == nil {
		t.Fatal("expected error for slow server, got nil")
	}
}
