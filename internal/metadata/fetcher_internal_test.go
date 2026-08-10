package metadata

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// testFetcher returns a fetcher configured against the supplied httptest
// server. The server binds to a loopback address, so the fetcher's resolver
// is replaced with one that resolves any host to that loopback address, and
// the public-IP check is loosened to admit loopback. Production code paths
// remain unchanged because the relaxed check is only set here.
func testFetcher(t *testing.T, server *httptest.Server) *Fetcher {
	t.Helper()
	return newLoopbackFetcher(t, Config{
		RequestTimeout: time.Second,
		TotalTimeout:   5 * time.Second,
		MaxBodyBytes:   1 << 20,
		MaxRedirects:   5,
		UserAgent:      testUserAgent,
	}, []net.IP{loopbackIP(t, server)})
}

// newLoopbackFetcher builds a fetcher whose resolver maps every hostname to
// the supplied loopback addresses and whose IP allowlist is widened to accept
// loopback for the duration of a test. The returned fetcher uses the same
// hardening as production code: per-request timeout, total timeout, redirect
// limit, body limit, and response-header deadline.
func newLoopbackFetcher(t *testing.T, cfg Config, ips []net.IP) *Fetcher {
	t.Helper()
	fetcher, err := NewFetcher(cfg)
	if err != nil {
		t.Fatal(err)
	}
	transport := fetcher.client.Transport
	fetcher.client = &http.Client{
		Transport:     transport,
		CheckRedirect: fetcher.client.CheckRedirect,
	}
	fetcher.resolver = staticResolver(ips)
	fetcher.isPublic = loopbackAllowed
	return fetcher
}

func staticResolver(ips []net.IP) ipResolver {
	return stubResolver{ips: ips}
}

type stubResolver struct {
	ips []net.IP
}

func (s stubResolver) LookupIP(_ context.Context, network, host string) ([]net.IP, error) {
	results := make([]net.IP, 0, len(s.ips))
	for _, ip := range s.ips {
		if network == "ip4" && ip.To4() == nil {
			continue
		}
		if network == "ip6" && ip.To4() != nil {
			continue
		}
		results = append(results, ip)
	}
	if len(results) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return results, nil
}

func loopbackAllowed(ip net.IP) bool {
	if ip.IsLoopback() {
		return true
	}
	return isPublicIP(ip)
}

func loopbackIP(t *testing.T, server *httptest.Server) net.IP {
	t.Helper()
	host, _, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split server addr: %v", err)
	}
	return net.ParseIP(host)
}

func TestFetcherAbandonsAcceptAndNeverRespondAtResponseHeader(t *testing.T) {
	t.Parallel()
	lc := &net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	addrPort := ln.Addr().String()
	host, port, err := net.SplitHostPort(addrPort)
	if err != nil {
		t.Fatal(err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		t.Fatalf("parse loopback %q", host)
	}
	stop := make(chan struct{})
	defer close(stop)
	go acceptAndHang(t, ln, stop)
	requestTimeout := 80 * time.Millisecond
	totalTimeout := time.Second
	fetcher := newLoopbackFetcher(t, Config{
		RequestTimeout: requestTimeout,
		TotalTimeout:   totalTimeout,
		MaxBodyBytes:   1 << 20,
		MaxRedirects:   5,
		UserAgent:      testUserAgent,
	}, []net.IP{ip})
	target := "http://" + net.JoinHostPort("fetcher-host", port)
	start := time.Now()
	_, fetchErr := fetcher.Fetch(context.Background(), target)
	elapsed := time.Since(start)
	if fetchErr == nil {
		t.Fatal("expected error for server that accepts and never responds, got nil")
	}
	if !errors.Is(fetchErr, context.DeadlineExceeded) {
		t.Errorf("fetchErr = %v, want context.DeadlineExceeded", fetchErr)
	}
	upper := requestTimeout + 500*time.Millisecond
	if elapsed > upper {
		t.Errorf("fetch took %v, must abandon near requestTimeout %v (upper bound %v)", elapsed, requestTimeout, upper)
	}
	if elapsed >= totalTimeout {
		t.Errorf("fetch took %v, must not wait for totalTimeout %v", elapsed, totalTimeout)
	}
}

func acceptAndHang(t *testing.T, ln net.Listener, stop <-chan struct{}) {
	t.Helper()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			select {
			case <-stop:
			case <-time.After(2 * time.Second):
			}
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _ = io.Copy(io.Discard, c)
		}(conn)
	}
}

func TestFetcherRedirectChainBeyondTotalTimeoutIsTransient(t *testing.T) {
	t.Parallel()
	var serverURL string
	hops := int64(0)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		atomic.AddInt64(&hops, 1)
		http.Redirect(writer, request, serverURL, http.StatusFound)
	}))
	t.Cleanup(server.Close)
	serverURL = server.URL
	requestTimeout := 200 * time.Millisecond
	totalTimeout := 250 * time.Millisecond
	fetcher := newLoopbackFetcher(t, Config{
		RequestTimeout: requestTimeout,
		TotalTimeout:   totalTimeout,
		MaxBodyBytes:   1 << 20,
		MaxRedirects:   10000,
		UserAgent:      testUserAgent,
	}, []net.IP{loopbackIP(t, server)})
	ctx, cancel := context.WithTimeout(context.Background(), totalTimeout+time.Second)
	defer cancel()
	start := time.Now()
	_, fetchErr := fetcher.Fetch(ctx, server.URL)
	elapsed := time.Since(start)
	if fetchErr == nil {
		t.Fatal("expected error for redirect chain beyond total timeout, got nil")
	}
	if !errors.Is(fetchErr, context.DeadlineExceeded) {
		t.Errorf("fetchErr = %v, want context.DeadlineExceeded", fetchErr)
	}
	upper := totalTimeout + 250*time.Millisecond
	if elapsed > upper {
		t.Errorf("fetch took %v, should fail near totalTimeout %v (upper bound %v)", elapsed, totalTimeout, upper)
	}
	if atomic.LoadInt64(&hops) <= 1 {
		t.Errorf("redirect chain only made %d hops; want multiple hops before total timeout", hops)
	}
}

func TestFetcherMultiAddressHostWithinTotalBudget(t *testing.T) {
	t.Parallel()
	addresses := []net.IP{
		net.ParseIP("127.0.0.1"),
		net.ParseIP("127.0.0.2"),
		net.ParseIP("127.0.0.3"),
	}
	requestTimeout := 100 * time.Millisecond
	totalTimeout := 250 * time.Millisecond
	fetcher := newLoopbackFetcher(t, Config{
		RequestTimeout: requestTimeout,
		TotalTimeout:   totalTimeout,
		MaxBodyBytes:   1 << 20,
		MaxRedirects:   5,
		UserAgent:      testUserAgent,
	}, addresses)
	start := time.Now()
	_, fetchErr := fetcher.Fetch(context.Background(), "http://multi-address-host:1")
	elapsed := time.Since(start)
	if fetchErr == nil {
		t.Fatal("expected error for unreachable host, got nil")
	}
	upper := totalTimeout + 100*time.Millisecond
	if elapsed > upper {
		t.Errorf("fetch took %v, must fail within total budget %v (upper bound %v)", elapsed, totalTimeout, upper)
	}
	if !errors.Is(fetchErr, context.DeadlineExceeded) && !strings.Contains(fetchErr.Error(), "dial") {
		t.Errorf("expected fetchErr to be a dial or deadline error; got %v", fetchErr)
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
