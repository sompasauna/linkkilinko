// Package preview extracts bounded, transport-free page preview metadata.
package preview

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/html"
)

// Document is the bounded response supplied to metadata providers.
type Document struct {
	URL         *url.URL
	Body        []byte
	ContentType string
}

// Metadata is the user-visible subset of page metadata.
type Metadata struct {
	CanonicalURL string
	SiteName     string
	Title        string
	Description  string
}

// Useful reports whether metadata is sufficient for a human-readable preview.
func (m Metadata) Useful() bool {
	return strings.TrimSpace(m.Title) != "" || (strings.TrimSpace(m.SiteName) != "" && strings.TrimSpace(m.Description) != "")
}

// Provider extracts metadata from a bounded document.
type Provider interface {
	Name() string
	Match(document Document) bool
	Extract(document Document) Metadata
}

// InconclusiveProvider marks a provider result that must not be treated as a
// definitive absence of metadata.
type InconclusiveProvider interface{ Inconclusive() bool }

// Registry dispatches documents to metadata providers.
type Registry struct {
	providers []Provider
}

// NewRegistry validates and returns a metadata provider registry.
func NewRegistry(providers ...Provider) (Registry, error) {
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		if provider == nil {
			return Registry{}, errors.New("preview: nil provider")
		}
		name := provider.Name()
		if name == "" {
			return Registry{}, errors.New("preview: provider name is empty")
		}
		if _, ok := seen[name]; ok {
			return Registry{}, fmt.Errorf("preview: duplicate provider %q", name)
		}
		seen[name] = struct{}{}
	}
	return Registry{providers: append([]Provider(nil), providers...)}, nil
}

// Inspect extracts metadata using the first matching provider.
func (r Registry) Inspect(document Document) (Metadata, string) {
	for _, provider := range r.providers {
		if provider.Match(document) {
			return provider.Extract(document), provider.Name()
		}
	}
	return Metadata{}, ""
}

// IsInconclusive reports whether the selected provider could not safely decide.
// For InconclusiveProvider, this calls Extract to determine the actual state.
func (r Registry) IsInconclusive(document Document) bool {
	for _, provider := range r.providers {
		if provider.Match(document) {
			if inconclusive, ok := provider.(InconclusiveProvider); ok {
				if ip, ok := provider.(interface {
					Extract(document Document) Metadata
				}); ok {
					metadata := ip.Extract(document)
					return inconclusive.Inconclusive() || !metadata.Useful()
				}
				return inconclusive.Inconclusive()
			}
			return false
		}
	}
	return false
}

// FacebookProvider extracts metadata from Facebook-family documents. The
// caller supplies the document fetched through the mobile-host policy.
type FacebookProvider struct {
	inconclusive bool
}

var facebookHosts = []string{
	"facebook.com",
	"www.facebook.com",
	"m.facebook.com",
	"mbasic.facebook.com",
	"fb.watch",
	"fb.me",
}

// Name returns the stable provider name.
func (p *FacebookProvider) Name() string { return "facebook" }

// Match reports whether document is hosted by the Facebook host family, using
// exact-host comparison so lookalikes such as facebook.com.example never match.
func (p *FacebookProvider) Match(document Document) bool {
	if document.URL == nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(document.URL.Hostname(), "."))
	return slices.Contains(facebookHosts, host)
}

// Extract reads the Open Graph and HTML fallback fields Facebook serves to
// anonymous crawlers on its mobile hosts. It reports whether extraction
// succeeded by updating the inconclusive state.
func (p *FacebookProvider) Extract(document Document) Metadata {
	m := GenericHTMLProvider{}.Extract(document)
	p.inconclusive = !m.Useful()
	return m
}

// Inconclusive reports whether the last Extract call produced no useful
// metadata, so a failed probe leaves the message alone instead of deleting it.
func (p *FacebookProvider) Inconclusive() bool { return p.inconclusive }

// GenericHTMLProvider extracts Open Graph, Twitter, and HTML fallback fields.
type GenericHTMLProvider struct{}

// Name returns the stable provider name.
func (GenericHTMLProvider) Name() string { return "generic-html" }

// Match reports whether document is an HTML document.
func (GenericHTMLProvider) Match(document Document) bool {
	contentType := strings.ToLower(document.ContentType)
	return contentType == "" || strings.Contains(contentType, "text/html") || strings.Contains(contentType, "application/xhtml+xml")
}

// Extract parses bounded HTML without executing scripts or loading resources.
func (GenericHTMLProvider) Extract(document Document) Metadata {
	root, err := html.Parse(strings.NewReader(string(document.Body)))
	if err != nil {
		return Metadata{}
	}
	metadata := Metadata{}
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			readNodeMetadata(node, &metadata)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	if metadata.CanonicalURL == "" && document.URL != nil {
		metadata.CanonicalURL = document.URL.String()
	}
	return metadata
}

func readNodeMetadata(node *html.Node, metadata *Metadata) {
	attrs := make(map[string]string, len(node.Attr))
	for _, attr := range node.Attr {
		attrs[strings.ToLower(attr.Key)] = strings.TrimSpace(attr.Val)
	}
	switch strings.ToLower(node.Data) {
	case "meta":
		property := strings.ToLower(attrs["property"])
		if property == "" {
			property = strings.ToLower(attrs["name"])
		}
		content := attrs["content"]
		switch property {
		case "og:title":
			metadata.Title = firstNonEmpty(metadata.Title, content)
		case "og:description":
			metadata.Description = firstNonEmpty(metadata.Description, content)
		case "og:site_name":
			metadata.SiteName = firstNonEmpty(metadata.SiteName, content)
		case "og:url":
			metadata.CanonicalURL = firstNonEmpty(metadata.CanonicalURL, content)
		case "twitter:title":
			metadata.Title = firstNonEmpty(metadata.Title, content)
		case "twitter:description", "description":
			metadata.Description = firstNonEmpty(metadata.Description, content)
		}
	case "title":
		metadata.Title = firstNonEmpty(metadata.Title, strings.TrimSpace(textContent(node)))
	case "link":
		if strings.EqualFold(attrs["rel"], "canonical") {
			metadata.CanonicalURL = firstNonEmpty(metadata.CanonicalURL, attrs["href"])
		}
	}
}

func textContent(node *html.Node) string {
	var builder strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.TextNode {
			builder.WriteString(current.Data)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return builder.String()
}

func firstNonEmpty(existing, candidate string) string {
	if strings.TrimSpace(existing) != "" {
		return existing
	}
	return strings.TrimSpace(candidate)
}
