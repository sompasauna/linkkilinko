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

const (
	ampRule         = "amp"
	googleShareHost = "share.google"
	legacyShortHost = "goo.gl"
	trackingRule    = "tracking-parameter"
	httpScheme      = "http"
	httpsScheme     = "https"
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

// HostMatcher exposes exact host ownership for startup ambiguity checks.
type HostMatcher interface {
	Resolver
	Hosts() []string
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

// MatchName returns the stable name of the first resolver matching rawURL.
func (r Registry) MatchName(rawURL string) (string, bool) {
	parsed, err := parseHTTPURL(rawURL)
	if err != nil {
		return "", false
	}
	for _, resolver := range r.resolvers {
		if resolver.Match(parsed) {
			return resolver.Name(), true
		}
	}
	return "", false
}

// NewRegistry validates and returns a resolver registry.
func NewRegistry(resolvers ...Resolver) (Registry, error) {
	seen := make(map[string]struct{}, len(resolvers))
	hosts := make(map[string]string)
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
		if matcher, ok := resolver.(HostMatcher); ok {
			for _, host := range matcher.Hosts() {
				host = strings.ToLower(strings.TrimSuffix(host, "."))
				if owner, exists := hosts[host]; exists && owner != name {
					return Registry{}, fmt.Errorf("link: ambiguous host %q claimed by %q and %q", host, owner, name)
				}
				hosts[host] = name
			}
		}
	}
	return Registry{resolvers: append([]Resolver(nil), resolvers...)}, nil
}

// Resolve dispatches rawURL to the first matching resolver.
func (r Registry) Resolve(ctx context.Context, rawURL string) (Resolution, bool, error) {
	parsed, err := parseHTTPURL(rawURL)
	if err != nil {
		return Resolution{}, false, err
	}

	original := cloneURL(parsed)
	matched := false
	resolverName := ""
	for steps := 0; steps <= len(r.resolvers); steps++ {
		var resolver Resolver
		for _, candidate := range r.resolvers {
			if candidate.Match(parsed) {
				resolver = candidate
				break
			}
		}
		if resolver == nil {
			if !matched {
				return Resolution{}, false, nil
			}
			return Resolution{Original: original, Destination: cloneURL(parsed), Resolver: resolverName}, true, nil
		}

		matched = true
		if resolverName == "" {
			resolverName = resolver.Name()
		}
		resolution, resolveErr := resolver.Resolve(ctx, parsed)
		if resolveErr != nil {
			return Resolution{}, true, fmt.Errorf("link: resolve with %s: %w", resolver.Name(), resolveErr)
		}
		if resolution.Destination == nil {
			return Resolution{}, true, fmt.Errorf("link: resolver %s returned no destination", resolver.Name())
		}
		if resolution.Destination.String() == parsed.String() {
			return Resolution{Original: original, Destination: cloneURL(parsed), Resolver: resolverName}, true, nil
		}
		parsed = cloneURL(resolution.Destination)
	}
	return Resolution{}, true, errors.New("link: resolver chain exceeded its limit")
}

// Match reports whether any registered resolver owns rawURL.
func (r Registry) Match(rawURL string) bool {
	_, matched := r.MatchName(rawURL)
	return matched
}

// GoogleShareResolver resolves Google share and legacy short links.
type GoogleShareResolver struct{ fetcher Fetcher }

// AMPResolver resolves Google cache and publisher-hosted AMP URLs.
type AMPResolver struct{ fetcher Fetcher }

// TrackingParameterResolver removes known tracking parameters while retaining
// query parameters that affect the destination or its presentation.
type TrackingParameterResolver struct{}

// NewGoogleResolvers constructs the Google share and AMP resolvers.
func NewGoogleResolvers(fetcher Fetcher) (GoogleShareResolver, AMPResolver, error) {
	if fetcher == nil {
		return GoogleShareResolver{}, AMPResolver{}, errors.New("link: nil Google fetcher")
	}
	return GoogleShareResolver{fetcher: fetcher}, AMPResolver{fetcher: fetcher}, nil
}

// Name returns the stable resolver name.
func (GoogleShareResolver) Name() string { return "google-share" }

// Name returns the stable resolver name.
func (AMPResolver) Name() string { return ampRule }

// Name returns the stable resolver name.
func (TrackingParameterResolver) Name() string { return trackingRule }

// Match reports whether u contains a known tracking parameter for its host.
func (TrackingParameterResolver) Match(u *url.URL) bool {
	return len(trackingParameters(u)) > 0
}

// Resolve removes known tracking parameters without making a network request.
func (TrackingParameterResolver) Resolve(_ context.Context, original *url.URL) (Resolution, error) {
	if original == nil {
		return Resolution{}, ErrNoResolution
	}
	destination := cloneURL(original)
	query := destination.Query()
	for parameter := range trackingParameters(original) {
		query.Del(parameter)
	}
	destination.RawQuery = query.Encode()
	if destination.RawQuery == "" {
		destination.ForceQuery = false
	}
	return Resolution{
		Original:    cloneURL(original),
		Destination: destination,
		Resolver:    trackingRule,
	}, nil
}

// Hosts returns the exact hosts owned by the share resolver.
func (GoogleShareResolver) Hosts() []string { return []string{googleShareHost, legacyShortHost} }

// Match reports whether u is a Google share or legacy short link.
func (GoogleShareResolver) Match(u *url.URL) bool {
	if u == nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	return host == googleShareHost || host == legacyShortHost
}

// Resolve follows the bounded redirect chain to the shared destination.
func (r GoogleShareResolver) Resolve(ctx context.Context, original *url.URL) (Resolution, error) {
	result, err := r.fetcher.Fetch(ctx, original.String())
	if err != nil {
		return Resolution{}, err
	}
	if result.URL == nil {
		return Resolution{}, ErrNoResolution
	}
	destination := result.URL
	if isGoogleShare(destination) || !isPublicHTTPURL(destination) {
		return Resolution{}, ErrNoResolution
	}
	return Resolution{Original: cloneURL(original), Destination: cloneURL(destination), Resolver: r.Name()}, nil
}

// Match reports whether u has a recognized AMP cache or publisher URL shape.
func (AMPResolver) Match(u *url.URL) bool { return isAMPURL(u) }

// Resolve unwraps deterministic cache URLs, then prefers a publisher canonical
// URL from the safely fetched page.
func (r AMPResolver) Resolve(ctx context.Context, original *url.URL) (Resolution, error) {
	if destination := unwrapAMPCache(original); destination != nil && isPublicHTTPURL(destination) {
		return Resolution{Original: cloneURL(original), Destination: destination, Resolver: r.Name()}, nil
	}
	result, err := r.fetcher.Fetch(ctx, original.String())
	if err != nil {
		return Resolution{}, err
	}
	if result.URL == nil {
		return Resolution{}, ErrNoResolution
	}
	destination := result.URL
	if canonical := canonicalURLFor(result.Body, result.URL); canonical != nil && !isAMPURL(canonical) {
		destination = canonical
	}
	if isAMPURL(destination) || !isPublicHTTPURL(destination) {
		return Resolution{}, ErrNoResolution
	}
	return Resolution{Original: cloneURL(original), Destination: cloneURL(destination), Resolver: r.Name()}, nil
}

var (
	linkPattern      = regexp.MustCompile(`(?is)<link\b[^>]*>`)
	canonicalPattern = regexp.MustCompile(`(?is)\brel\s*=\s*["'][^"']*\bcanonical\b[^"']*["']`)
	hrefPattern      = regexp.MustCompile(`(?is)\bhref\s*=\s*["']([^"']+)["']`)
)

func canonicalURLFor(body []byte, base *url.URL) *url.URL {
	var match []byte
	for _, tag := range linkPattern.FindAll(body, -1) {
		if canonicalPattern.Match(tag) {
			href := hrefPattern.FindSubmatch(tag)
			if len(href) == 2 {
				match = href[1]
				break
			}
		}
	}
	if len(match) == 0 {
		return nil
	}
	raw := strings.TrimSpace(string(match))
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
	if (u.Scheme != httpScheme && u.Scheme != httpsScheme) || u.Hostname() == "" || u.User != nil {
		return nil, fmt.Errorf("unsupported URL %q", raw)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u, nil
}

func isPublicHTTPURL(u *url.URL) bool {
	if u == nil || (u.Scheme != httpScheme && u.Scheme != httpsScheme) || u.Hostname() == "" || u.User != nil {
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

func isGoogleShare(u *url.URL) bool {
	if u == nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	return host == googleShareHost || host == legacyShortHost
}

func isAMPURL(u *url.URL) bool {
	if u == nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	path := strings.ToLower(u.EscapedPath())
	query := strings.ToLower(u.RawQuery)
	if strings.HasSuffix(host, ".cdn.ampproject.org") || strings.HasSuffix(host, ".ampproject.net") {
		return true
	}
	switch {
	case (host == "google.com" || host == "www.google.com") && strings.HasPrefix(path, "/amp/"):
		return true
	case strings.HasPrefix(host, "www.google."):
		return false
	case strings.HasPrefix(host, "amp."):
		return true
	}
	return path == "/amp" || strings.HasPrefix(path, "/amp/") || strings.HasSuffix(path, "/amp") ||
		strings.Contains(path, "/amp/") || strings.HasSuffix(path, ".amp") || hasAMPQuery(query)
}

func hasAMPQuery(query string) bool {
	for pair := range strings.SplitSeq(query, "&") {
		name, value, _ := strings.Cut(pair, "=")
		if name == "amp" || strings.HasPrefix(name, "amp_") || strings.HasSuffix(name, "_amp") ||
			(name == "output" && value == "amp") {
			return true
		}
	}
	return false
}

var genericTrackingParameters = map[string]struct{}{
	"fbclid":  {},
	"gclid":   {},
	"dclid":   {},
	"gbraid":  {},
	"wbraid":  {},
	"mc_cid":  {},
	"mc_eid":  {},
	"msclkid": {},
	"ttclid":  {},
	"yclid":   {},
}

func trackingParameters(candidate *url.URL) map[string]struct{} {
	if candidate == nil {
		return nil
	}
	parameters := make(map[string]struct{})
	query := candidate.Query()
	for parameter := range query {
		if _, ok := genericTrackingParameters[parameter]; ok || strings.HasPrefix(parameter, "utm_") {
			parameters[parameter] = struct{}{}
		}
	}
	host := strings.ToLower(strings.TrimSuffix(candidate.Hostname(), "."))
	for parameter := range query {
		if hostTracksParameter(host, parameter) {
			parameters[parameter] = struct{}{}
		}
	}
	return parameters
}

func hostTracksParameter(host, parameter string) bool {
	switch {
	case host == "is.fi" || strings.HasSuffix(host, ".is.fi"):
		return parameter == "shem"
	case host == "youtube.com" || host == "www.youtube.com" || host == "m.youtube.com" || host == "youtu.be":
		return parameter == "si" || parameter == "pp" || parameter == "s"
	case host == "open.spotify.com":
		return parameter == "si"
	case host == "instagram.com" || host == "www.instagram.com":
		return parameter == "igshid"
	default:
		return false
	}
}

func unwrapAMPCache(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	path := u.EscapedPath()
	var rest string
	switch {
	case (host == "google.com" || host == "www.google.com") && strings.HasPrefix(path, "/amp/"):
		rest = strings.TrimPrefix(path, "/amp/")
	case strings.HasSuffix(host, ".cdn.ampproject.org"):
		if after, ok := strings.CutPrefix(path, "/c/"); ok {
			rest = after
		} else if after, ok := strings.CutPrefix(path, "/v/"); ok {
			rest = after
		} else {
			return nil
		}
	default:
		return nil
	}
	scheme := httpScheme
	if strings.HasPrefix(rest, "s/") {
		scheme = httpsScheme
		rest = strings.TrimPrefix(rest, "s/")
	}
	if rest == "" || strings.HasPrefix(rest, "/") {
		return nil
	}
	destination, err := parseHTTPURL(scheme + "://" + rest)
	if err != nil {
		return nil
	}
	return destination
}

func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	copyURL := *u
	return &copyURL
}
