package preview_test

import (
	"net/url"
	"testing"

	"github.com/sompasauna/linkkilinko/pkg/core/preview"
)

const (
	testHTTPS       = "https"
	testPath        = "/somepage"
	testContentType = "text/html"
	testExampleHost = "example.com"
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

func TestBareSiteNameTitleNotUseful(t *testing.T) {
	t.Parallel()
	registry, err := preview.NewRegistry(preview.GenericHTMLProvider{})
	if err != nil {
		t.Fatal(err)
	}
	doc := preview.Document{
		URL:         &url.URL{Scheme: testHTTPS, Host: testExampleHost, Path: "/"},
		ContentType: testContentType,
		Body:        []byte(`<title>Example</title>`),
	}
	metadata, _ := registry.Inspect(doc)
	if metadata.Useful() {
		t.Error("title=sitename with no description should not be useful")
	}
}

func TestDistinctHTMLTitleIsUseful(t *testing.T) {
	t.Parallel()
	registry, err := preview.NewRegistry(preview.GenericHTMLProvider{})
	if err != nil {
		t.Fatal(err)
	}
	doc := preview.Document{
		URL:         &url.URL{Scheme: testHTTPS, Host: testExampleHost, Path: "/article"},
		ContentType: testContentType,
		Body:        []byte(`<title>Example Blog: Article Title</title>`),
	}
	metadata, _ := registry.Inspect(doc)
	if !metadata.Useful() {
		t.Error("distinct HTML title with no OG tags should be useful")
	}
}

func TestBareSiteNameWithDescriptionIsUseful(t *testing.T) {
	t.Parallel()
	registry, err := preview.NewRegistry(preview.GenericHTMLProvider{})
	if err != nil {
		t.Fatal(err)
	}
	doc := preview.Document{
		URL:         &url.URL{Scheme: testHTTPS, Host: testExampleHost, Path: "/"},
		ContentType: testContentType,
		Body:        []byte(`<title>Example</title><meta name="description" content="A great website">`),
	}
	metadata, _ := registry.Inspect(doc)
	if !metadata.Useful() {
		t.Error("title=sitename with description should be useful")
	}
}

func TestBareSiteNameFacebookIsNotUseful(t *testing.T) {
	t.Parallel()
	registry, err := preview.NewRegistry(preview.GenericHTMLProvider{})
	if err != nil {
		t.Fatal(err)
	}
	doc := preview.Document{
		URL:         &url.URL{Scheme: testHTTPS, Host: "mbasic.facebook.com", Path: "/share/18uiPcLZw1"},
		ContentType: testContentType,
		Body:        []byte(`<title>Facebook</title>`),
	}
	metadata, _ := registry.Inspect(doc)
	if metadata.Useful() {
		t.Error("Facebook login wall title should not be useful")
	}
}
