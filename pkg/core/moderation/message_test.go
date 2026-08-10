package moderation_test

import (
	"testing"
	"time"

	"github.com/sompasauna/linkkilinko/pkg/core/moderation"
)

const entityURL = "url"

// schemelessTestURL is a Bot API url entity's visible text with no explicit
// scheme, shared by the t-019 regression tests below.
const schemelessTestURL = "github.com/owner/repository"

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

// TestExtractURLsRecognizesSchemelessEntity is t-019 regression coverage
// item 1: a Bot API url entity with no explicit scheme, such as
// "github.com/owner/repository", keeps its visible text as Raw and gets an
// https:// canonical Target.
func TestExtractURLsRecognizesSchemelessEntity(t *testing.T) {
	t.Parallel()
	raw := schemelessTestURL
	in := moderation.Input{
		Text:     raw,
		Entities: []moderation.Entity{{Type: entityURL, Offset: 0, Length: len(raw)}},
	}
	urls := moderation.ExtractURLs(in)
	if len(urls) != 1 {
		t.Fatalf("ExtractURLs() = %#v", urls)
	}
	if urls[0].Raw != raw {
		t.Errorf("Raw = %q, want unchanged visible text %q", urls[0].Raw, raw)
	}
	if want := "https://" + raw; urls[0].Target != want {
		t.Errorf("Target = %q, want %q", urls[0].Target, want)
	}
}

// TestExtractURLsSchemelessHandlesUTF16OffsetsWithEmoji is regression
// coverage item 3: an emoji ahead of a scheme-less URL uses two UTF-16 code
// units, and the entity offset must still resolve to the correct span.
func TestExtractURLsSchemelessHandlesUTF16OffsetsWithEmoji(t *testing.T) {
	t.Parallel()
	raw := schemelessTestURL
	in := moderation.Input{
		Text:     "🙂 " + raw,
		Entities: []moderation.Entity{{Type: entityURL, Offset: 3, Length: len(raw)}},
	}
	urls := moderation.ExtractURLs(in)
	if len(urls) != 1 || urls[0].Raw != raw || urls[0].Target != "https://"+raw {
		t.Fatalf("ExtractURLs() = %#v", urls)
	}
}

// TestExtractURLsKeepsExplicitSchemesUnchanged is regression coverage item 4.
func TestExtractURLsKeepsExplicitSchemesUnchanged(t *testing.T) {
	t.Parallel()
	for _, scheme := range []string{"http", "https"} {
		raw := scheme + "://example.test/path"
		in := moderation.Input{
			Text:     raw,
			Entities: []moderation.Entity{{Type: entityURL, Offset: 0, Length: len(raw)}},
		}
		urls := moderation.ExtractURLs(in)
		if len(urls) != 1 || urls[0].Raw != raw || urls[0].Target != raw {
			t.Fatalf("scheme %s: ExtractURLs() = %#v", scheme, urls)
		}
	}
}

// TestExtractURLsRejectsUnsafeOrMalformedEntities is regression coverage
// item 5: explicit non-HTTP schemes, credentials, empty hosts, and corrupt
// entity spans are all rejected rather than promoted to HTTPS.
func TestExtractURLsRejectsUnsafeOrMalformedEntities(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
	}{
		{"explicit ftp", "ftp://example.test/path"},
		{"telegram deep link", "tg://resolve?domain=foo"},
		{"credentials", "https://user:pass@example.test/path"},
		{"empty host", "https:///path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := moderation.Input{
				Text:     tc.text,
				Entities: []moderation.Entity{{Type: entityURL, Offset: 0, Length: len(tc.text)}},
			}
			if urls := moderation.ExtractURLs(in); len(urls) != 0 {
				t.Fatalf("ExtractURLs() = %#v, want none rejected", urls)
			}
		})
	}
	t.Run("corrupt entity span", func(t *testing.T) {
		t.Parallel()
		in := moderation.Input{
			Text:     "short",
			Entities: []moderation.Entity{{Type: entityURL, Offset: 100, Length: 10}},
		}
		if urls := moderation.ExtractURLs(in); len(urls) != 0 {
			t.Fatalf("ExtractURLs() = %#v, want none for a corrupt span", urls)
		}
	})
}

// TestExtractURLsRejectsMalformedPort covers decision item 4: a malformed
// port is rejected the same way for an explicit URL and for a scheme-less
// one promoted to HTTPS.
func TestExtractURLsRejectsMalformedPort(t *testing.T) {
	t.Parallel()
	cases := []string{
		"https://example.test:99999999/path", // out of uint16 range
		"example.test:99999999/path",         // scheme-less, same malformed port
	}
	for _, raw := range cases {
		in := moderation.Input{
			Text:     raw,
			Entities: []moderation.Entity{{Type: entityURL, Offset: 0, Length: len(raw)}},
		}
		if urls := moderation.ExtractURLs(in); len(urls) != 0 {
			t.Fatalf("%q: ExtractURLs() = %#v, want rejected malformed port", raw, urls)
		}
	}
}

// TestExtractURLsMixedExplicitAndSchemelessPreservesSpans is regression
// coverage item 8: an explicit URL and a scheme-less one in the same message
// each keep their own visible Raw span while both get a usable Target.
func TestExtractURLsMixedExplicitAndSchemelessPreservesSpans(t *testing.T) {
	t.Parallel()
	explicit := "https://example.test/explicit"
	schemeless := "example.org/schemeless"
	text := explicit + " ja " + schemeless
	in := moderation.Input{
		Text: text,
		Entities: []moderation.Entity{
			{Type: entityURL, Offset: 0, Length: len(explicit)},
			{Type: entityURL, Offset: len(explicit) + 4, Length: len(schemeless)},
		},
	}
	urls := moderation.ExtractURLs(in)
	if len(urls) != 2 {
		t.Fatalf("ExtractURLs() = %#v", urls)
	}
	if urls[0].Raw != explicit || urls[0].Target != explicit {
		t.Errorf("explicit URL = %#v", urls[0])
	}
	if urls[1].Raw != schemeless || urls[1].Target != "https://"+schemeless {
		t.Errorf("schemeless URL = %#v", urls[1])
	}
}

// TestFingerprintSchemelessAndExplicitHTTPSMatch is regression coverage item
// 9: a scheme-less URL and its explicit HTTPS spelling must fingerprint
// identically, or a repost in the other spelling would bypass suppression.
func TestFingerprintSchemelessAndExplicitHTTPSMatch(t *testing.T) {
	t.Parallel()
	schemeless := schemelessTestURL
	explicit := "https://" + schemeless
	first := moderation.Input{
		Text:     schemeless,
		Entities: []moderation.Entity{{Type: entityURL, Offset: 0, Length: len(schemeless)}},
	}
	second := moderation.Input{
		Text:     explicit,
		Entities: []moderation.Entity{{Type: entityURL, Offset: 0, Length: len(explicit)}},
	}
	if moderation.Fingerprint(first, "rule") != moderation.Fingerprint(second, "rule") {
		t.Fatal("expected scheme-less and explicit HTTPS spellings to share a fingerprint")
	}
}

// TestIsLinkOnlyAcceptsQuotedAndParenthesizedSchemelessURL is regression
// coverage item 2.
func TestIsLinkOnlyAcceptsQuotedAndParenthesizedSchemelessURL(t *testing.T) {
	t.Parallel()
	raw := schemelessTestURL
	cases := []struct {
		name           string
		text           string
		offset, length int
	}{
		{"quoted", `"` + raw + `"`, 1, len(raw)},
		{"parenthesized", "(" + raw + ")", 1, len(raw)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := moderation.Input{
				Text:     tc.text,
				Entities: []moderation.Entity{{Type: entityURL, Offset: tc.offset, Length: tc.length}},
			}
			if !moderation.IsLinkOnly(in) {
				t.Fatalf("expected link-only message for %q", tc.text)
			}
		})
	}
}

// TestIsLinkOnlyKeepsRejectedEntitySpanAsText guards against a regression
// introduced alongside scheme-less recognition: a message can now contain
// one entity ExtractURLs accepts and one it rejects (a malformed port, here).
// Only the accepted span may be erased. If the rejected entity's text were
// erased too, the malformed-port text would silently vanish and the message
// would be misclassified as link-only.
func TestIsLinkOnlyKeepsRejectedEntitySpanAsText(t *testing.T) {
	t.Parallel()
	rejected := "example.test:99999999/path" // malformed port, rejected by ExtractURLs
	accepted := "https://real.test"
	text := rejected + " " + accepted
	in := moderation.Input{
		Text: text,
		Entities: []moderation.Entity{
			{Type: entityURL, Offset: 0, Length: len(rejected)},
			{Type: entityURL, Offset: len(rejected) + 1, Length: len(accepted)},
		},
	}
	if moderation.IsLinkOnly(in) {
		t.Fatal("rejected entity's text must remain as meaningful content, not be erased as a URL span")
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
