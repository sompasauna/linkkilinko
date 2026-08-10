// Package metadata provides bounded and SSRF-aware HTTP fetching.
package metadata

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sompasauna/linkkilinko/pkg/core/preview"
)

// Config controls the hard limits applied to metadata requests.
type Config struct {
	RequestTimeout time.Duration
	TotalTimeout   time.Duration
	MaxBodyBytes   int64
	MaxRedirects   int
	UserAgent      string
}

// ipResolver resolves a host to one or more IP addresses. Production code
// uses *net.Resolver; tests substitute a static mapping.
type ipResolver interface {
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
}

// Fetcher downloads bounded public HTTP(S) documents.
type Fetcher struct {
	client         *http.Client
	maxBodyBytes   int64
	maxRedirects   int
	userAgent      string
	resolver       ipResolver
	requestTimeout time.Duration
	totalTimeout   time.Duration
	isPublic       func(net.IP) bool
}

type fragmentContextKey struct{}

type fragmentState struct {
	value string
}

// NewFetcher constructs a hardened metadata fetcher.
func NewFetcher(config Config) (*Fetcher, error) {
	if config.RequestTimeout <= 0 {
		return nil, errors.New("metadata: request timeout must be positive")
	}
	if config.TotalTimeout <= 0 {
		return nil, errors.New("metadata: total timeout must be positive")
	}
	if config.RequestTimeout >= config.TotalTimeout {
		return nil, errors.New("metadata: request timeout must be less than total timeout")
	}
	if config.MaxBodyBytes <= 0 {
		return nil, errors.New("metadata: max body bytes must be positive")
	}
	if config.MaxRedirects < 0 {
		return nil, errors.New("metadata: max redirects must not be negative")
	}
	if strings.TrimSpace(config.UserAgent) == "" {
		return nil, errors.New("metadata: user agent must not be empty")
	}
	fetcher := &Fetcher{
		maxBodyBytes:   config.MaxBodyBytes,
		maxRedirects:   config.MaxRedirects,
		userAgent:      config.UserAgent,
		resolver:       net.DefaultResolver,
		requestTimeout: config.RequestTimeout,
		totalTimeout:   config.TotalTimeout,
		isPublic:       isPublicIP,
	}
	fetcher.client = &http.Client{
		Transport: &http.Transport{
			DialContext:           fetcher.dialContext,
			TLSHandshakeTimeout:   config.RequestTimeout,
			ResponseHeaderTimeout: config.RequestTimeout,
			ExpectContinueTimeout: config.RequestTimeout,
			IdleConnTimeout:       config.RequestTimeout,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > config.MaxRedirects {
				return fmt.Errorf("metadata: redirect limit %d exceeded", config.MaxRedirects)
			}
			if err := validateURL(req.URL); err != nil {
				return fmt.Errorf("metadata: unsafe redirect: %w", err)
			}
			if state, ok := req.Context().Value(fragmentContextKey{}).(*fragmentState); ok && req.URL.Fragment != "" {
				state.value = req.URL.Fragment
			}
			// Fragments are never transmitted, so strip before dialing.
			req.URL.Fragment = ""
			return nil
		},
	}
	return fetcher, nil
}

// Fetch gets one bounded public document and returns its final URL.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (preview.Document, error) {
	return f.fetch(ctx, rawURL, f.userAgent)
}

// FetchWithUserAgent performs the same hardened request with a caller-selected
// crawler identity. All URL and network safety checks remain unchanged.
func (f *Fetcher) FetchWithUserAgent(ctx context.Context, rawURL, userAgent string) (preview.Document, error) {
	if strings.TrimSpace(userAgent) == "" {
		return preview.Document{}, errors.New("metadata: user agent must not be empty")
	}
	return f.fetch(ctx, rawURL, userAgent)
}

func (f *Fetcher) fetch(ctx context.Context, rawURL, userAgent string) (preview.Document, error) {
	parsedURL, err := parseURL(rawURL)
	if err != nil {
		return preview.Document{}, err
	}
	if err := validateURL(parsedURL); err != nil {
		return preview.Document{}, err
	}
	totalCtx, totalCancel := context.WithTimeout(ctx, f.totalTimeout)
	defer totalCancel()
	state := &fragmentState{value: parsedURL.Fragment}
	parsedURL.Fragment = ""
	totalCtx = context.WithValue(totalCtx, fragmentContextKey{}, state)
	requestCtx, requestCancel := context.WithTimeout(totalCtx, f.requestTimeout)
	defer requestCancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return preview.Document{}, fmt.Errorf("metadata: create request: %w", err)
	}
	request.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,text/plain;q=0.2")
	request.Header.Set("User-Agent", userAgent)
	response, err := f.client.Do(request)
	if err != nil {
		return preview.Document{}, fmt.Errorf("metadata: fetch %s: %w", parsedURL.Hostname(), err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return preview.Document{}, fmt.Errorf("metadata: upstream status %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, f.maxBodyBytes+1))
	if err != nil {
		return preview.Document{}, fmt.Errorf("metadata: read response: %w", err)
	}
	if int64(len(body)) > f.maxBodyBytes {
		return preview.Document{}, fmt.Errorf("metadata: response exceeds %d bytes", f.maxBodyBytes)
	}
	finalURL := *response.Request.URL
	finalURL.Fragment = state.value
	return preview.Document{
		URL:         &finalURL,
		Body:        body,
		ContentType: response.Header.Get("Content-Type"),
	}, nil
}

func (f *Fetcher) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("split dial address: %w", err)
	}
	ips, err := f.resolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	var lastErr error
	start := time.Now()
	for _, ip := range ips {
		if !f.isPublic(ip) {
			continue
		}
		remaining := remainingBudget(ctx, start, f.requestTimeout)
		if remaining <= 0 {
			break
		}
		dialer := &net.Dialer{Timeout: remaining, KeepAlive: f.requestTimeout}
		connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return connection, nil
		}
		lastErr = dialErr
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("dial public address for %s: %w", host, lastErr)
	}
	return nil, fmt.Errorf("host %s has no public address", host)
}

// remainingBudget returns the lesser of the per-request budget minus elapsed
// time and any deadline already imposed on ctx. It is used so the dialer
// candidate loop cannot multiply its timeout across resolved addresses.
func remainingBudget(ctx context.Context, start time.Time, perAttempt time.Duration) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0
		}
		if remaining < perAttempt {
			return remaining
		}
	}
	elapsed := time.Since(start)
	if elapsed >= perAttempt {
		return 0
	}
	return perAttempt - elapsed
}

func parseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("metadata: parse URL: %w", err)
	}
	return u, nil
}

func validateURL(u *url.URL) error {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return errors.New("metadata: only HTTP(S) URLs with a host are supported")
	}
	if u.User != nil {
		return errors.New("metadata: URL credentials are not supported")
	}
	if _, err := strconv.Atoi(u.Port()); u.Port() != "" && err != nil {
		return fmt.Errorf("metadata: invalid port: %w", err)
	}
	return nil
}

func isPublicIP(address net.IP) bool {
	if address == nil || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsUnspecified() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() {
		return false
	}
	for _, network := range blockedNetworks {
		if network.Contains(address) {
			return false
		}
	}
	return true
}

// IsPublicIP reports whether an address is suitable for outbound metadata fetching.
func IsPublicIP(address net.IP) bool { return isPublicIP(address) }

var blockedNetworks = mustNetworks(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "100::/64", "2001:2::/48", "2001:db8::/32",
	"2001:10::/28", "2001:20::/28", "3fff::/20", "fc00::/7", "fe80::/10",
)

func mustNetworks(values ...string) []*net.IPNet {
	result := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		network := ipNet(value)
		if network == nil {
			panic("metadata: invalid blocked network " + value)
		}
		result = append(result, network)
	}
	return result
}

func ipNet(cidr string) *net.IPNet {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil
	}
	return network
}
