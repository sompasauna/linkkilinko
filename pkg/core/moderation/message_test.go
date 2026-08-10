package moderation_test

import (
	"testing"
	"time"

	"github.com/sompasauna/linkkilinko/pkg/core/moderation"
)

const entityURL = "url"

func TestExtractURLsHandlesUTF16Offsets(t *testing.T) {
	t.Parallel()
	in := moderation.Input{
		Text:     "🙂 https://example.test/path",
		Entities: []moderation.Entity{{Type: entityURL, Offset: 3, Length: 25}},
	}
	urls := moderation.ExtractURLs(in)
	if len(urls) != 1 || urls[0].Raw != "https://example.test/path" {
		t.Fatalf("ExtractURLs() = %#v", urls)
	}
}

func TestIsLinkOnlyIgnoresURLAndPunctuation(t *testing.T) {
	t.Parallel()
	in := moderation.Input{
		Text:     "(https://example.test)",
		Entities: []moderation.Entity{{Type: entityURL, Offset: 1, Length: 20}},
	}
	if !moderation.IsLinkOnly(in) {
		t.Fatal("expected link-only message")
	}
}

func TestNewcomerPlanBoundary(t *testing.T) {
	t.Parallel()
	joined := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	in := moderation.Input{
		Text:     "https://example.test",
		Entities: []moderation.Entity{{Type: entityURL, Offset: 0, Length: 20}},
	}
	if _, ok := moderation.NewcomerPlan(in, joined, joined.Add(48*time.Hour), 48*time.Hour); ok {
		t.Fatal("expected the exact 48-hour boundary to be allowed")
	}
	if _, ok := moderation.NewcomerPlan(in, joined, joined.Add(47*time.Hour), 48*time.Hour); !ok {
		t.Fatal("expected a member inside the window to be blocked")
	}
}

func TestFingerprintNormalizesEquivalentURLs(t *testing.T) {
	t.Parallel()
	first := moderation.Input{
		Text:     "https://EXAMPLE.test:443/path",
		Entities: []moderation.Entity{{Type: entityURL, Offset: 0, Length: 29}},
	}
	second := moderation.Input{
		Text:     " https://example.test/path ",
		Entities: []moderation.Entity{{Type: entityURL, Offset: 1, Length: 25}},
	}
	if moderation.Fingerprint(first, "rule") != moderation.Fingerprint(second, "rule") {
		t.Fatal("expected equivalent URLs to share a fingerprint")
	}
}

func TestReplaceURLSpansUsesEntityOffsets(t *testing.T) {
	first := "https://share.google/a"
	second := "https://amp.google/b"
	in := moderation.Input{
		Text:     "katso " + first + " ja " + second,
		Entities: []moderation.Entity{{Type: entityURL, Offset: 6, Length: len(first)}, {Type: entityURL, Offset: 6 + len(first) + 4, Length: len(second)}},
	}
	urls := moderation.ExtractURLs(in)
	got, err := moderation.ReplaceURLSpans(in.Text, urls, map[int]string{0: "https://example.test/a", 1: "https://example.test/b"})
	if err != nil {
		t.Fatal(err)
	}
	want := "katso https://example.test/a ja https://example.test/b"
	if got != want {
		t.Fatalf("replacement = %q, want %q", got, want)
	}
}

func TestReplaceURLSpansReplacesTextLinkLabel(t *testing.T) {
	in := moderation.Input{Text: "avaa tämän", Entities: []moderation.Entity{{Type: "text_link", Offset: 5, Length: 5, URL: "https://share.google/a"}}}
	urls := moderation.ExtractURLs(in)
	got, err := moderation.ReplaceURLSpans(in.Text, urls, map[int]string{0: "https://example.test/a"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "avaa https://example.test/a"; got != want {
		t.Fatalf("replacement = %q, want %q", got, want)
	}
}
