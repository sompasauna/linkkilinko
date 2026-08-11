// Package preview extracts bounded, transport-free page preview metadata.
package preview

import (
	"errors"
	"fmt"
	"net/url"
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
	CanonicalURL  string
	SiteName      string
	Title         string
	Description   string
	TitleFallback bool
	Host          string
}

// Useful reports whether metadata is sufficient for a human-readable preview.
// A non-empty title is sufficient on its own when it comes from Open Graph
// or Twitter card metadata; an HTML-<title>-only title is considered useful
// only when accompanied by a description, or when it differs meaningfully
// from the site name or host. A non-empty site name plus a non-empty
// description is also sufficient, with or without a title.
func (m Metadata) Useful() bool {
	siteName := strings.TrimSpace(m.SiteName)
	description := strings.TrimSpace(m.Description)
	title := strings.TrimSpace(m.Title)
	if title == "" {
		return siteName != "" && description != ""
	}
	if !m.TitleFallback || description != "" {
		return true
	}
	if siteName != "" && strings.EqualFold(title, siteName) {
		return false
	}
	host := strings.TrimSpace(m.Host)
	if host != "" && isBareSiteName(title, host) {
		return false
	}
	return true
}

func isBareSiteName(title, host string) bool {
	titleLower := strings.ToLower(title)
	hostLower := strings.ToLower(host)
	if strings.EqualFold(titleLower, hostLower) {
		return true
	}
	if strings.HasPrefix(hostLower, titleLower+".") {
		return true
	}
	labels := strings.Split(hostLower, ".")
	if len(labels) >= 2 {
		siteDomain := strings.Join(labels[len(labels)-2:], ".")
		if strings.EqualFold(titleLower, siteDomain) {
			return true
		}
		if titleLower == labels[len(labels)-2] {
			return true
		}
	}
	return false
}

// Provider extracts metadata from a bounded document.
type Provider interface {
	Name() string
	Match(document Document) bool
	Extract(document Document) Metadata
}

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
	if document.URL != nil {
		metadata.Host = document.URL.Hostname()
	}
	hadOpenGraphTitle := false
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			readNodeMetadata(node, &metadata, &hadOpenGraphTitle)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	if !hadOpenGraphTitle && metadata.Title != "" {
		metadata.TitleFallback = true
	}
	if metadata.CanonicalURL == "" && document.URL != nil {
		metadata.CanonicalURL = document.URL.String()
	}
	return metadata
}

func readNodeMetadata(node *html.Node, metadata *Metadata, hadOpenGraphTitle *bool) {
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
			*hadOpenGraphTitle = true
		case "og:description":
			metadata.Description = firstNonEmpty(metadata.Description, content)
		case "og:site_name":
			metadata.SiteName = firstNonEmpty(metadata.SiteName, content)
		case "og:url":
			metadata.CanonicalURL = firstNonEmpty(metadata.CanonicalURL, content)
		case "twitter:title":
			metadata.Title = firstNonEmpty(metadata.Title, content)
			*hadOpenGraphTitle = true
		case "twitter:description", "description":
			metadata.Description = firstNonEmpty(metadata.Description, content)
		}
	case "title":
		if metadata.Title == "" {
			metadata.Title = strings.TrimSpace(textContent(node))
		}
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
