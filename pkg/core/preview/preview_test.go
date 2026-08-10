package preview_test

import (
	"net/url"
	"testing"

	"github.com/sompasauna/linkkilinko/pkg/core/preview"
)

const (
	testFacebookHost = "mbasic.facebook.com"
	testHTTPS        = "https"
	testPath         = "/somepage"
	testContentType  = "text/html"
)

func TestGenericHTMLProviderExtractsOpenGraph(t *testing.T) {
	t.Parallel()
	registry, err := preview.NewRegistry(preview.GenericHTMLProvider{})
	if err != nil {
		t.Fatal(err)
	}
	documentURL, err := url.Parse("https://example.test/article")
	if err != nil {
		t.Fatal(err)
	}
	metadata, provider := registry.Inspect(preview.Document{
		URL:         documentURL,
		ContentType: "text/html; charset=utf-8",
		Body:        []byte(`<meta property="og:site_name" content="Example"><meta property="og:title" content="A title"><meta property="og:description" content="A description">`),
	})
	if provider != "generic-html" || !metadata.Useful() || metadata.Title != "A title" || metadata.SiteName != "Example" {
		t.Fatalf("provider=%q metadata=%#v", provider, metadata)
	}
}

func TestFacebookProviderMatchesExactHosts(t *testing.T) {
	t.Parallel()
	fb := &preview.FacebookProvider{}
	exactHosts := []string{
		"facebook.com",
		"www.facebook.com",
		"m.facebook.com",
		"mbasic.facebook.com",
		"fb.watch",
		"fb.me",
	}
	for _, host := range exactHosts {
		doc := preview.Document{URL: &url.URL{Scheme: testHTTPS, Host: host, Path: testPath}}
		if !fb.Match(doc) {
			t.Errorf("Match(%q) = false, want true", host)
		}
	}
}

func TestFacebookProviderRejectsLookalikes(t *testing.T) {
	t.Parallel()
	fb := &preview.FacebookProvider{}
	lookalikes := []string{
		"facebook.com.attacker.example",
		"www.facebook.com.example.net",
		"mfacebook.com",
		"facebook.co",
	}
	for _, host := range lookalikes {
		doc := preview.Document{URL: &url.URL{Scheme: testHTTPS, Host: host, Path: "/"}}
		if fb.Match(doc) {
			t.Errorf("Match(%q) = true, want false", host)
		}
	}
}

func TestFacebookProviderInconclusiveWhenNoUsefulMetadata(t *testing.T) {
	t.Parallel()
	fb := &preview.FacebookProvider{}
	doc := preview.Document{
		URL:         &url.URL{Scheme: testHTTPS, Host: testFacebookHost, Path: testPath},
		ContentType: testContentType,
		Body:        []byte(`<html><body>no metadata here</body></html>`),
	}
	metadata := fb.Extract(doc)
	if metadata.Useful() {
		t.Fatal("metadata should not be useful for this document")
	}
	if !fb.Inconclusive() {
		t.Error("Inconclusive() = false, want true when Extract returns no useful metadata")
	}
}

func TestFacebookProviderNotInconclusiveWhenUsefulMetadata(t *testing.T) {
	t.Parallel()
	fb := &preview.FacebookProvider{}
	doc := preview.Document{
		URL:         &url.URL{Scheme: testHTTPS, Host: testFacebookHost, Path: testPath},
		ContentType: testContentType,
		Body:        []byte(`<meta property="og:title" content="A Title"><meta property="og:description" content="A Description">`),
	}
	metadata := fb.Extract(doc)
	if !metadata.Useful() {
		t.Fatal("metadata should be useful for this document")
	}
	if fb.Inconclusive() {
		t.Error("Inconclusive() = true, want false when Extract returns useful metadata")
	}
}

func TestRegistryFacebookBeforeGenericHTML(t *testing.T) {
	t.Parallel()
	registry, err := preview.NewRegistry(&preview.FacebookProvider{}, preview.GenericHTMLProvider{})
	if err != nil {
		t.Fatal(err)
	}
	doc := preview.Document{
		URL:         &url.URL{Scheme: testHTTPS, Host: "facebook.com", Path: testPath},
		ContentType: testContentType,
		Body:        []byte(`<meta property="og:title" content="FB Title">`),
	}
	_, provider := registry.Inspect(doc)
	if provider != "facebook" {
		t.Errorf("provider = %q, want facebook (FacebookProvider should take priority over GenericHTMLProvider)", provider)
	}
}

func TestRegistryIsInconclusiveForFacebookWithNoMetadata(t *testing.T) {
	t.Parallel()
	registry, err := preview.NewRegistry(&preview.FacebookProvider{}, preview.GenericHTMLProvider{})
	if err != nil {
		t.Fatal(err)
	}
	doc := preview.Document{
		URL:         &url.URL{Scheme: testHTTPS, Host: testFacebookHost, Path: testPath},
		ContentType: testContentType,
		Body:        []byte(`<html><body>nothing useful</body></html>`),
	}
	if !registry.IsInconclusive(doc) {
		t.Error("IsInconclusive() = false, want true for Facebook URL with no useful metadata")
	}
}

func TestRegistryIsNotInconclusiveForFacebookWithUsefulMetadata(t *testing.T) {
	t.Parallel()
	registry, err := preview.NewRegistry(&preview.FacebookProvider{}, preview.GenericHTMLProvider{})
	if err != nil {
		t.Fatal(err)
	}
	doc := preview.Document{
		URL:         &url.URL{Scheme: testHTTPS, Host: testFacebookHost, Path: testPath},
		ContentType: testContentType,
		Body:        []byte(`<meta property="og:title" content="Real Title"><meta property="og:description" content="Real Description">`),
	}
	if registry.IsInconclusive(doc) {
		t.Error("IsInconclusive() = true, want false for Facebook URL with useful metadata")
	}
}
