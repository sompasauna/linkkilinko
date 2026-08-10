package notice_test

import (
	"strings"
	"testing"

	"github.com/sompasauna/linkkilinko/internal/notice"
	"github.com/sompasauna/linkkilinko/pkg/core/moderation"
)

func TestEveryShippedCatalogDefinesEveryNoticeKey(t *testing.T) {
	t.Parallel()
	languages, err := notice.Languages()
	if err != nil {
		t.Fatalf("list languages: %v", err)
	}
	if len(languages) == 0 {
		t.Fatal("expected at least one embedded catalog")
	}
	for _, language := range languages {
		catalog, err := notice.Load(language)
		if err != nil {
			t.Fatalf("load %q: %v", language, err)
		}
		for _, key := range moderation.NoticeKeys() {
			if strings.TrimSpace(catalog.Render(key, nil)) == "" {
				t.Errorf("catalog %q renders %q as empty", language, key)
			}
		}
	}
}

func TestLoadRejectsUnknownLanguage(t *testing.T) {
	t.Parallel()
	if _, err := notice.Load("xx"); err == nil {
		t.Fatal("expected an unsupported language to fail at load time")
	}
	if _, err := notice.Load("  "); err == nil {
		t.Fatal("expected an empty language to fail at load time")
	}
}

func TestRenderSubstitutesParamsWithoutRescanning(t *testing.T) {
	t.Parallel()
	catalog, err := notice.Load("fi")
	if err != nil {
		t.Fatalf("load fi: %v", err)
	}
	rendered := catalog.Render(moderation.NoticeGoogleWrapper, map[string]string{
		"sender":  "@kayttaja",
		"content": "{sender} https://example.test",
	})
	if !strings.Contains(rendered, "@kayttaja") {
		t.Fatalf("expected the sender in %q", rendered)
	}
	if strings.Count(rendered, "@kayttaja") != 1 {
		t.Fatalf("a placeholder inside a parameter value must not be substituted again: %q", rendered)
	}
	if !strings.Contains(rendered, "Käyttäjien dataa mainonta- ja seurantatarkoituksiin keräävä linkki vaihdettu suoraan linkkiin.") {
		t.Fatalf("expected the SPEC replacement introduction in %q", rendered)
	}
}

func TestRenderUnknownKeyIsEmpty(t *testing.T) {
	t.Parallel()
	catalog, err := notice.Load("fi")
	if err != nil {
		t.Fatalf("load fi: %v", err)
	}
	if got := catalog.Render("no.such.key", nil); got != "" {
		t.Fatalf("expected an empty render for an unknown key, got %q", got)
	}
}
