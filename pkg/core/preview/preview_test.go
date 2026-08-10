package preview_test

import (
	"net/url"
	"testing"

	"github.com/sompasauna/linkkilinko/pkg/core/preview"
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
