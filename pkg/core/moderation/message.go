// Package moderation contains deterministic message classification and policy
// decisions for linkkilinko.
package moderation

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

const (
	textLinkEntity = "text_link"
	urlEntity      = "url"
)

// Entity describes a Telegram message entity using Telegram's UTF-16 offsets.
type Entity struct {
	Type   string
	Offset int
	Length int
	URL    string
}

// Input is the transport-free subset of a Telegram message used by policy.
type Input struct {
	ChatID          int64
	ThreadID        int
	MessageID       int
	EditDate        int64
	SenderID        int64
	SenderName      string
	SenderIsBot     bool
	Text            string
	Entities        []Entity
	Caption         string
	CaptionEntities []Entity
	MediaKind       string
	MediaUniqueID   string
	MediaGroupID    string
	PreviewDisabled bool
}

// ReplaceURLSpans replaces URL entity spans without relying on visible text
// matching. This handles text_link entities and repeated URLs safely.
func ReplaceURLSpans(text string, urls []URL, destinations map[int]string) (string, error) {
	type replacement struct {
		start, end int
		value      string
	}
	replacements := make([]replacement, 0, len(destinations))
	for index, destination := range destinations {
		if index < 0 || index >= len(urls) || strings.TrimSpace(destination) == "" {
			continue
		}
		start, end, ok := utf16Span(text, urls[index].Offset, urls[index].Length)
		if !ok || start > end {
			return "", fmt.Errorf("moderation: invalid URL entity span %d", index)
		}
		replacements = append(replacements, replacement{start: start, end: end, value: destination})
	}
	sort.Slice(replacements, func(i, j int) bool { return replacements[i].start > replacements[j].start })
	for i := range replacements {
		if i > 0 && replacements[i].end > replacements[i-1].start {
			return "", errors.New("moderation: overlapping URL entity spans")
		}
	}
	result := text
	for _, item := range replacements {
		result = result[:item.start] + item.value + result[item.end:]
	}
	return result, nil
}

// URL is a URL extracted from a message and its display span.
type URL struct {
	Raw       string
	Target    string
	Offset    int
	Length    int
	FromText  bool
	FromLabel bool
}

// Action identifies the externally visible moderation operation.
type Action string

const (
	// ActionAllow leaves the source message untouched.
	ActionAllow Action = "allow"
	// ActionDelete removes the source and sends a notice.
	ActionDelete Action = "delete"
	// ActionReplace removes the source and sends replacement text.
	ActionReplace Action = "replace"
)

// Notice keys identify the user-visible message a plan renders. Policy never
// carries rendered text: the composition layer resolves a key and its
// parameters through a language catalog.
const (
	// NoticeNewcomerSandbox explains the 48-hour link and media restriction.
	NoticeNewcomerSandbox = "newcomer.sandbox"
	// NoticeGoogleWrapper introduces a replaced tracking or wrapper link and
	// takes the {sender} and {content} parameters.
	NoticeGoogleWrapper = "google.wrapper.replacement"
	// NoticePreviewMissing explains that a link-only post lacked preview data.
	NoticePreviewMissing = "preview.missing"
	// NoticePreviewEnriched introduces fetched preview data and takes the
	// {sender}, {url}, and {metadata} parameters.
	NoticePreviewEnriched = "preview.enriched"
)

// NoticeKeys returns every notice key a plan can emit. A catalog must define
// all of them.
func NoticeKeys() []string {
	return []string{NoticeNewcomerSandbox, NoticeGoogleWrapper, NoticePreviewMissing, NoticePreviewEnriched}
}

// Plan describes a moderation action without performing any side effects.
type Plan struct {
	Action      Action
	Rule        string
	Fingerprint string
	NoticeKey   string
	Params      map[string]string
}

// ExtractURLs returns HTTP(S) URLs from text or caption entities.
func ExtractURLs(in Input) []URL {
	text := in.Text
	entities := in.Entities
	if text == "" {
		text = in.Caption
		entities = in.CaptionEntities
	}
	if text == "" {
		return nil
	}

	urls := make([]URL, 0, len(entities))
	for _, entity := range entities {
		if entity.Type != urlEntity && entity.Type != textLinkEntity {
			continue
		}
		value := entity.URL
		fromText := entity.Type == urlEntity
		if fromText {
			start, end, ok := utf16Span(text, entity.Offset, entity.Length)
			if !ok {
				continue
			}
			value = text[start:end]
		}
		value = strings.TrimSpace(value)
		target, ok := canonicalHTTPTarget(value, fromText)
		if !ok {
			continue
		}
		urls = append(urls, URL{
			Raw:       value,
			Target:    target,
			Offset:    entity.Offset,
			Length:    entity.Length,
			FromText:  fromText,
			FromLabel: entity.Type == textLinkEntity,
		})
	}
	return urls
}

// HasLinkOrMedia reports whether a message is subject to the newcomer rule.
func HasLinkOrMedia(in Input) bool {
	return len(ExtractURLs(in)) > 0 || in.MediaKind != "" || in.MediaGroupID != ""
}

// IsLinkOnly reports whether a non-media message contains no meaningful text
// besides URL entities.
func IsLinkOnly(in Input) bool {
	if in.MediaKind != "" {
		return false
	}
	urls := ExtractURLs(in)
	if len(urls) == 0 {
		return false
	}
	text := in.Text
	if text == "" {
		text = in.Caption
	}
	if text == "" {
		return false
	}

	// Only spans ExtractURLs accepted are removed. An entity present in the
	// message but rejected by extraction (malformed port, disallowed scheme,
	// corrupt span, ...) is not evidence of a URL and must not be erased from
	// this determination merely because some other entity in the same
	// message was accepted.
	spans := make([]textSpan, 0, len(urls))
	for _, candidate := range urls {
		start, end, ok := utf16Span(text, candidate.Offset, candidate.Length)
		if ok {
			spans = append(spans, textSpan{start: start, end: end})
		}
	}
	remaining := removeSpans(text, spans)
	for _, r := range remaining {
		if !unicode.IsSpace(r) && !unicode.IsPunct(r) {
			return false
		}
	}
	return true
}

// Fingerprint returns a stable digest for a sender's candidate moderation
// action. The sender and chat are deliberately not included; storage scopes
// the digest to those fields.
func Fingerprint(in Input, rule string) string {
	urls := ExtractURLs(in)
	text := normalizedText(in.Text, urls)
	caption := normalizedText(in.Caption, urls)
	var b strings.Builder
	fmt.Fprintf(&b, "rule=%s\ntext=%s\ncaption=%s\nmedia=%s\nmedia_id=%s\nmedia_group=%s\n", rule, text, caption, in.MediaKind, in.MediaUniqueID, in.MediaGroupID)
	for _, candidate := range urls {
		fmt.Fprintf(&b, "url=%s\n", normalizeURL(candidate.Target))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func normalizedText(text string, urls []URL) string {
	text = norm.NFKC.String(strings.TrimSpace(text))
	for _, candidate := range urls {
		text = strings.Replace(text, candidate.Raw, normalizeURL(candidate.Target), 1)
	}
	return text
}

// NewcomerPlan returns a delete plan when the sender is inside the sandbox.
func NewcomerPlan(in Input, joinedAt, now time.Time, window time.Duration) (Plan, bool) {
	if joinedAt.IsZero() || now.Before(joinedAt) || now.Sub(joinedAt) >= window || !HasLinkOrMedia(in) {
		return Plan{}, false
	}
	return Plan{
		Action:      ActionDelete,
		Rule:        "newcomer-sandbox",
		Fingerprint: Fingerprint(in, "newcomer-sandbox"),
		NoticeKey:   NoticeNewcomerSandbox,
	}, true
}

// canonicalHTTPTarget validates raw as an HTTP(S) URL and returns the
// canonical target used for resolution, metadata retrieval, and
// fingerprinting.
//
// A Bot API url entity (schemeless is true only for that entity type) whose
// visible text carries no explicit scheme is treated as authoritative
// evidence that its span is a URL: https:// is prefixed before validation,
// so a scheme-less spelling and its explicit HTTPS spelling produce the same
// canonical target. An explicit non-HTTP scheme, such as ftp:// or a
// Telegram deep link, is rejected outright and is never reinterpreted as
// HTTPS.
func canonicalHTTPTarget(raw string, schemeless bool) (string, bool) {
	target := raw
	if schemeless && !hasExplicitScheme(raw) {
		target = "https://" + raw
	}
	if !validHTTPURL(target) {
		return "", false
	}
	return target, true
}

func validHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if port := u.Port(); port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return false
		}
	}
	return true
}

// hasExplicitScheme reports whether raw begins with an RFC 3986 scheme
// followed by "://". Telegram's url entity recognizer also produces
// scheme-less text such as "example.com:8080/path", where the colon
// precedes a port rather than a scheme; requiring the "//" keeps that case
// from being misread as an explicit scheme named "example.com".
func hasExplicitScheme(raw string) bool {
	i := strings.Index(raw, "://")
	if i <= 0 {
		return false
	}
	return isSchemeToken(raw[:i])
}

func isSchemeToken(s string) bool {
	for i, r := range s {
		switch {
		case i == 0 && isASCIILetter(r):
		case i > 0 && (isASCIILetter(r) || (r >= '0' && r <= '9') || r == '+' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	return true
}

func isASCIILetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func normalizeURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return strings.TrimSpace(raw)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if (u.Scheme == "http" && u.Port() == "80") || (u.Scheme == "https" && u.Port() == "443") {
		u.Host = u.Hostname()
	}
	return u.String()
}

type textSpan struct {
	start int
	end   int
}

func removeSpans(text string, spans []textSpan) []rune {
	removed := make([]bool, len([]rune(text)))
	runes := []rune(text)
	for _, span := range spans {
		start := len([]rune(text[:span.start]))
		end := len([]rune(text[:span.end]))
		if start < 0 || end > len(runes) || start >= end {
			continue
		}
		for i := start; i < end; i++ {
			removed[i] = true
		}
	}
	remaining := make([]rune, 0, len(runes))
	for i, r := range runes {
		if !removed[i] {
			remaining = append(remaining, r)
		}
	}
	return remaining
}

func utf16Span(text string, offset, length int) (int, int, bool) {
	if offset < 0 || length <= 0 {
		return 0, 0, false
	}
	units := 0
	start := -1
	end := -1
	for byteIndex, r := range text {
		if units == offset {
			start = byteIndex
		}
		units += utf16Width(r)
		if units == offset+length {
			end = byteIndex + len(string(r))
			break
		}
	}
	if start == -1 && units == offset {
		start = len(text)
	}
	if end == -1 && units == offset+length {
		end = len(text)
	}
	return start, end, start >= 0 && end > start
}

func utf16Width(r rune) int {
	if r > 0xffff {
		return 2
	}
	return 1
}
