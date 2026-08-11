package notice_test

import (
	"os"
	"path/filepath"
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
	if !strings.Contains(rendered, "Linkki, joka kerää käyttäjien tietoja mainontaa ja seurantaa varten, korvattiin suoralla linkillä.") {
		t.Fatalf("expected the SPEC replacement introduction in %q", rendered)
	}
}

func TestRenderNewcomerSandboxIdentifiesSender(t *testing.T) {
	t.Parallel()
	catalog, err := notice.Load("fi")
	if err != nil {
		t.Fatalf("load fi: %v", err)
	}
	rendered := catalog.Render(moderation.NoticeNewcomerSandbox, map[string]string{"sender": "@kayttaja"})
	if !strings.Contains(rendered, "@kayttaja") {
		t.Errorf("Render(%q, sender) = %q, want sender mention", moderation.NoticeNewcomerSandbox, rendered)
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

func TestLoadWithOverrideOverlaysOneKey(t *testing.T) {
	overridePath := writeOverride(t, "google.wrapper.replacement: Custom {sender} {content}\n")
	catalog, err := notice.LoadWithOverride("fi", overridePath)
	if err != nil {
		t.Fatalf("load override: %v", err)
	}
	if got := catalog.Render(moderation.NoticeGoogleWrapper, map[string]string{"sender": "A", "content": "B"}); got != "Custom A B" {
		t.Fatalf("overridden message = %q, want %q", got, "Custom A B")
	}
	if got := catalog.Render(moderation.NoticeNewcomerSandbox, nil); strings.TrimSpace(got) == "" {
		t.Fatal("overlay removed an unspecified embedded notice")
	}
}

func TestLoadWithOverrideRejectsInvalidEntries(t *testing.T) {
	tests := []struct {
		name     string
		override string
		want     string
	}{
		{name: "unknown key", override: "typo.key: text\n", want: "typo.key"},
		{name: "empty value", override: "preview.missing: \"\"\n", want: "preview.missing"},
		{name: "unsupported placeholder", override: "newcomer.sandbox: '{url}'\n", want: "{url}"},
		{name: "malformed yaml", override: "preview.missing: [\n", want: "parse override"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := notice.LoadWithOverride("fi", writeOverride(t, test.override))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func writeOverride(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
