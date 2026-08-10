// Package link provides registry-based URL resolution and policy matching.
package link

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// ErrNoResolution indicates that a matching resolver could not find a safe
// public destination.
var ErrNoResolution = errors.New("no safe URL resolution")

// FetchResult is the bounded response returned by a metadata-capable fetcher.
type FetchResult struct {
	URL         *url.URL
	Body        []byte
	ContentType string
	StatusCode  int
}

// Fetcher is consumed by resolvers and is implemented by the hardened HTTP
// adapter in internal/metadata.
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string) (FetchResult, error)
}

// Resolver matches and resolves a family of URLs.
type Resolver interface {
	Name() string
	Match(candidate *url.URL) bool
	Resolve(ctx context.Context, candidate *url.URL) (Resolution, error)
}

// Resolution describes a successfully transformed URL.
type Resolution struct {
	Original    *url.URL
	Destination *url.URL
	Resolver    string
}

// Registry dispatches URLs to registered resolvers in order.
type Registry struct {
	resolvers []Resolver
}

// NewRegistry validates and returns a resolver registry.
func NewRegistry(resolvers ...Resolver) (Registry, error) {
	seen := make(map[string]struct{}, len(resolvers))
	for _, resolver := range resolvers {
		if resolver == nil {
			return Registry{}, errors.New("link: nil resolver")
		}
		name := resolver.Name()
		if name == "" {
			return Registry{}, errors.New("link: resolver name is empty")
		}
		if _, ok := seen[name]; ok {
			return Registry{}, fmt.Errorf("link: duplicate resolver %q", name)
		}
		seen[name] = struct{}{}
	}
	return Registry{resolvers: append([]Resolver(nil), resolvers...)}, nil
}

// Resolve dispatches rawURL to the first matching resolver.
func (r Registry) Resolve(ctx context.Context, rawURL string) (Resolution, bool, error) {
	parsed, err := parseHTTPURL(rawURL)
	if err != nil {
		return Resolution{}, false, err
	}
	for _, resolver := range r.resolvers {
		if !resolver.Match(parsed) {
			continue
		}
		resolution, resolveErr := resolver.Resolve(ctx, parsed)
		if resolveErr != nil {
			return Resolution{}, true, fmt.Errorf("link: resolve with %s: %w", resolver.Name(), resolveErr)
		}
		return resolution, true, nil
	}
	return Resolution{}, false, nil
}

// Match reports whether any registered resolver owns rawURL.
func (r Registry) Match(rawURL string) bool {
	parsed, err := parseHTTPURL(rawURL)
	if err != nil {
		return false
	}
	for _, resolver := range r.resolvers {
		if resolver.Match(parsed) {
			return true
		}
	}
	return false
}

// GoogleResolver resolves share.google.com and amp.google.com wrappers.
type GoogleResolver struct {
	fetcher Fetcher
}

// NewGoogleResolver constructs a resolver using fetcher for redirects and
// canonical-link inspection.
func NewGoogleResolver(fetcher Fetcher) (GoogleResolver, error) {
	if fetcher == nil {
		return GoogleResolver{}, errors.New("link: nil Google fetcher")
	}
	return GoogleResolver{fetcher: fetcher}, nil
}

// Name returns the stable resolver name.
func (r GoogleResolver) Name() string { return "google-wrapper" }

// Match reports whether u is hosted by one of the supported Google wrappers.
func (r GoogleResolver) Match(u *url.URL) bool {
	if u == nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	return host == "share.google.com" || host == "amp.google.com"
}

// Resolve follows a bounded safe redirect and AMP canonical link.
func (r GoogleResolver) Resolve(ctx context.Context, original *url.URL) (Resolution, error) {
	result, err := r.fetcher.Fetch(ctx, original.String())
	if err != nil {
		return Resolution{}, err
	}
	if result.URL == nil {
		return Resolution{}, ErrNoResolution
	}
	destination := result.URL
	if isGoogleWrapper(destination) {
		if canonical := canonicalURLFor(result.Body, result.URL); canonical != nil && !isGoogleWrapper(canonical) {
			destination = canonical
		}
	}
	if isGoogleWrapper(destination) || !isPublicHTTPURL(destination) {
		return Resolution{}, ErrNoResolution
	}
	return Resolution{Original: cloneURL(original), Destination: cloneURL(destination), Resolver: r.Name()}, nil
}

var canonicalPattern = regexp.MustCompile(`(?is)<link\b[^>]*\brel\s*=\s*["'][^"']*\bcanonical\b[^"']*["'][^>]*\bhref\s*=\s*["']([^"']+)["']`)

func canonicalURLFor(body []byte, base *url.URL) *url.URL {
	match := canonicalPattern.FindSubmatch(body)
	if len(match) != 2 {
		return nil
	}
	raw := strings.TrimSpace(string(match[1]))
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	if base != nil && !u.IsAbs() {
		u = base.ResolveReference(u)
	}
	if _, err := parseHTTPURL(u.String()); err != nil {
		return nil
	}
	return u
}

func parseHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return nil, fmt.Errorf("unsupported URL %q", raw)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u, nil
}

func isPublicHTTPURL(u *url.URL) bool {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return false
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
			return false
		}
		for _, network := range blockedDestinationNetworks {
			if network.Contains(ip) {
				return false
			}
		}
	}
	return true
}

var blockedDestinationNetworks = mustDestinationNetworks(
	"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24",
	"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
	"2001:db8::/32", "3fff::/20",
)

func mustDestinationNetworks(values ...string) []*net.IPNet {
	result := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			panic("link: invalid destination network " + value)
		}
		result = append(result, network)
	}
	return result
}

func isGoogleWrapper(u *url.URL) bool {
	if u == nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	return host == "share.google.com" || host == "amp.google.com"
}

func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	copyURL := *u
	return &copyURL
}
