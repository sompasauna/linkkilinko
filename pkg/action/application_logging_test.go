package action_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"testing"
	"time"

	"github.com/sompasauna/linkkilinko/internal/store"
	"github.com/sompasauna/linkkilinko/pkg/action"
	"github.com/sompasauna/linkkilinko/pkg/core/moderation"
	"github.com/sompasauna/linkkilinko/pkg/core/preview"
)

const (
	githubURL   = "https://github.com/owner/repo"
	githubHost  = "github.com"
	exampleURL  = "https://example.com/"
	exampleHost = "example.com"
	repoTitle   = "Repo"
	alice       = "alice"
)

// capturedLogHandler is a slog.Handler that records every record as JSON in
// memory. Tests assert on the structured fields without parsing free-form
// message strings.
type capturedLogHandler struct {
	records []map[string]any
	bounds  []slog.Attr
}

func (h *capturedLogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *capturedLogHandler) Handle(_ context.Context, r slog.Record) error {
	entry := make(map[string]any)
	entry["level"] = r.Level.String()
	entry["message"] = r.Message
	for _, attr := range h.bounds {
		entry[attr.Key] = attr.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		entry[a.Key] = a.Value.Any()
		return true
	})
	h.records = append(h.records, entry)
	return nil
}

func (h *capturedLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// Pre-bind attrs onto this handler rather than returning a new one, so
	// every chained With() in the production code writes to the same backing
	// records slice and tests see the captured entries.
	h.bounds = append(h.bounds, attrs...)
	return h
}

func (h *capturedLogHandler) WithGroup(_ string) slog.Handler { return h }

// findRecord returns the first captured record matching message and the
// supplied predicate. It fails the test if no match is found.
func (h *capturedLogHandler) findRecord(t *testing.T, message string, predicate func(map[string]any) bool) map[string]any {
	t.Helper()
	for _, record := range h.records {
		if got, _ := record["message"].(string); got != message {
			continue
		}
		if predicate(record) {
			return record
		}
	}
	t.Fatalf("no %q record matched predicate", message)
	return nil
}

func newLoggedApp(t *testing.T, links *fakeLinks, md *fakeMetadata, pv *fakePreviews) (*action.Application, *capturedLogHandler) {
	t.Helper()
	handler := &capturedLogHandler{}
	logger := slog.New(handler)
	tc := &fakeTelegram{}
	st := newFakeStore()
	app := newTestAppWithLinks(tc, st, md, pv, links)
	app.SetLogger(logger)
	st.memberships[membershipKey{1, 100}] = store.Membership{ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-100 * time.Hour)}
	return app, handler
}

func TestModerationLogsAllowedOrdinaryURL(t *testing.T) {
	t.Parallel()
	links := &fakeLinks{}
	md := &fakeMetadata{docs: map[string]preview.Document{
		githubURL: {URL: mustParseURL(t, githubURL), Body: []byte("<title>Repo</title>"), ContentType: testContentType},
	}}
	pv := &fakePreviews{inspectResult: preview.Metadata{Title: repoTitle, Host: githubHost, TitleFallback: true}}
	app, handler := newLoggedApp(t, links, md, pv)
	input := moderation.Input{
		ChatID: 1, ThreadID: 2, MessageID: 10, SenderID: 100, SenderName: alice,
		Text:     githubURL,
		Entities: []moderation.Entity{{Type: testEntityType, Offset: 0, Length: len(githubURL), URL: githubURL}},
	}
	if err := app.Process(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	summary := handler.findRecord(t, "moderation decision", func(r map[string]any) bool {
		return r["chat_id"] == int64(1) && r["message_id"] == int64(10)
	})
	if got := summary["subsystem"]; got != "moderation" {
		t.Errorf("subsystem = %v, want moderation", got)
	}
	if got := summary["url_count"]; got != int64(1) {
		t.Errorf("url_count = %v, want 1", got)
	}
	if got := summary["multi_link"]; got != false {
		t.Errorf("multi_link = %v, want false", got)
	}
	if _, ok := summary["duration_ms"]; !ok {
		t.Errorf("duration_ms missing from summary: %+v", summary)
	}
	reasoning := handler.findRecord(t, "url reasoning", func(r map[string]any) bool {
		return r["url_index"] == int64(0)
	})
	if got := reasoning["url_host"]; got != githubHost {
		t.Errorf("url_host = %v, want github.com", got)
	}
	if got := reasoning["preview_useful"]; got != true {
		t.Errorf("preview_useful = %v, want true", got)
	}
}

func TestModerationLogsPreviewDisabled(t *testing.T) {
	t.Parallel()
	links := &fakeLinks{}
	md := &fakeMetadata{docs: map[string]preview.Document{
		githubURL: {URL: mustParseURL(t, githubURL), Body: []byte("<title>Repo</title>"), ContentType: testContentType},
	}}
	pv := &fakePreviews{inspectResult: preview.Metadata{Title: repoTitle, Host: githubHost, TitleFallback: true}}
	app, handler := newLoggedApp(t, links, md, pv)
	input := moderation.Input{
		ChatID: 1, ThreadID: 2, MessageID: 11, SenderID: 100, SenderName: alice,
		Text:            githubURL,
		Entities:        []moderation.Entity{{Type: testEntityType, Offset: 0, Length: len(githubURL), URL: githubURL}},
		PreviewDisabled: true,
	}
	if err := app.Process(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	summary := handler.findRecord(t, "moderation decision", func(r map[string]any) bool {
		return r["message_id"] == int64(11)
	})
	if got := summary["outcome"]; got != "replace" {
		t.Errorf("outcome = %v, want replace", got)
	}
	if got := summary["preview_disabled"]; got != true {
		t.Errorf("preview_disabled = %v, want true", got)
	}
}

func TestModerationLogsDefinitiveNoMetadata(t *testing.T) {
	t.Parallel()
	links := &fakeLinks{}
	md := &fakeMetadata{docs: map[string]preview.Document{
		exampleURL: {URL: mustParseURL(t, exampleURL), Body: []byte("<html></html>"), ContentType: testContentType},
	}}
	pv := &fakePreviews{inspectResult: preview.Metadata{}}
	app, handler := newLoggedApp(t, links, md, pv)
	input := moderation.Input{
		ChatID: 1, MessageID: 12, SenderID: 100, SenderName: alice,
		Text:     exampleURL,
		Entities: []moderation.Entity{{Type: testEntityType, Offset: 0, Length: 20, URL: exampleURL}},
	}
	if err := app.Process(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	summary := handler.findRecord(t, "moderation decision", func(r map[string]any) bool {
		return r["message_id"] == int64(12)
	})
	if got := summary["outcome"]; got != "delete" {
		t.Errorf("outcome = %v, want delete", got)
	}
	if got := summary["link_only"]; got != true {
		t.Errorf("link_only = %v, want true", got)
	}
}

func TestModerationLogsTransientMetadataFailure(t *testing.T) {
	t.Parallel()
	links := &fakeLinks{}
	md := &fakeMetadata{docs: map[string]preview.Document{}}
	md.err = context.DeadlineExceeded
	pv := &fakePreviews{}
	app, handler := newLoggedApp(t, links, md, pv)
	input := moderation.Input{
		ChatID: 1, MessageID: 13, SenderID: 100, SenderName: alice,
		Text:     exampleURL,
		Entities: []moderation.Entity{{Type: testEntityType, Offset: 0, Length: 20, URL: exampleURL}},
	}
	if err := app.Process(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	summary := handler.findRecord(t, "moderation decision", func(r map[string]any) bool {
		return r["message_id"] == int64(13)
	})
	if got := summary["outcome"]; got != "fail_open" {
		t.Errorf("outcome = %v, want fail_open", got)
	}
	reasoning := handler.findRecord(t, "url reasoning", func(r map[string]any) bool {
		return r["message_id"] == int64(13) && r["url_index"] == int64(0)
	})
	if got := reasoning["fetch_error_class"]; got != "transient_timeout" {
		t.Errorf("fetch_error_class = %v, want transient_timeout", got)
	}
}

func TestModerationLogsMixedMultiLink(t *testing.T) {
	t.Parallel()
	googleURL := "https://google.com/share?url=" + testURL
	destinations := map[string]string{googleURL: testURL}
	links := &fakeLinks{destinations: destinations}
	md := &fakeMetadata{docs: map[string]preview.Document{
		testURL: {URL: mustParseURL(t, testURL), Body: []byte("<title>Example</title>"), ContentType: testContentType},
	}}
	pv := &fakePreviews{inspectResult: preview.Metadata{Title: "Example", Host: exampleHost, TitleFallback: true}}
	app, handler := newLoggedApp(t, links, md, pv)
	input := moderation.Input{
		ChatID: 1, ThreadID: 2, MessageID: 14, SenderID: 100, SenderName: alice,
		Text: googleURL + " and a normal sentence",
		Entities: []moderation.Entity{
			{Type: testEntityType, Offset: 0, Length: len(googleURL), URL: googleURL},
		},
	}
	if err := app.Process(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	summary := handler.findRecord(t, "moderation decision", func(r map[string]any) bool {
		return r["message_id"] == int64(14)
	})
	if got := summary["outcome"]; got != "replace" {
		t.Errorf("outcome = %v, want replace", got)
	}
	if got := summary["multi_link"]; got != false {
		t.Errorf("multi_link = %v, want false (single URL)", got)
	}
	reasoning := handler.findRecord(t, "url reasoning", func(r map[string]any) bool {
		return r["message_id"] == int64(14) && r["url_index"] == int64(0)
	})
	if got := reasoning["resolver_name"]; got != "google-share" {
		t.Errorf("resolver_name = %v, want google-share", got)
	}
	if got := reasoning["destination_host"]; got != exampleHost {
		t.Errorf("destination_host = %v, want example.com", got)
	}
	if got := reasoning["resolution_outcome"]; got != "resolved" {
		t.Errorf("resolution_outcome = %v, want resolved", got)
	}
}

func TestModerationLogJSONDoesNotLeakQueryValues(t *testing.T) {
	t.Parallel()
	links := &fakeLinks{}
	md := &fakeMetadata{docs: map[string]preview.Document{}}
	pv := &fakePreviews{inspectResult: preview.Metadata{Title: "Example", Host: exampleHost, TitleFallback: true}}
	handler := &capturedLogHandler{}
	logger := slog.New(handler)
	tc := &fakeTelegram{}
	st := newFakeStore()
	app := newTestAppWithLinks(tc, st, md, pv, links)
	app.SetLogger(logger)
	st.memberships[membershipKey{1, 100}] = store.Membership{ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-100 * time.Hour)}
	input := moderation.Input{
		ChatID: 1, MessageID: 15, SenderID: 100, SenderName: alice,
		Text:     "https://example.com/?secret=abc123",
		Entities: []moderation.Entity{{Type: testEntityType, Offset: 0, Length: 32, URL: "https://example.com/?secret=abc123"}},
	}
	if err := app.Process(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(handler.records)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("secret=abc123")) {
		t.Errorf("query value leaked into log output: %s", encoded)
	}
}

func TestModerationLogRedactsQueryPresence(t *testing.T) {
	t.Parallel()
	links := &fakeLinks{}
	md := &fakeMetadata{docs: map[string]preview.Document{}}
	pv := &fakePreviews{}
	handler := &capturedLogHandler{}
	logger := slog.New(handler)
	tc := &fakeTelegram{}
	st := newFakeStore()
	app := newTestAppWithLinks(tc, st, md, pv, links)
	app.SetLogger(logger)
	st.memberships[membershipKey{1, 100}] = store.Membership{ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-100 * time.Hour)}
	input := moderation.Input{
		ChatID: 1, MessageID: 16, SenderID: 100,
		Text:     "https://example.com/?token=should-stay-out",
		Entities: []moderation.Entity{{Type: testEntityType, Offset: 0, Length: 40, URL: "https://example.com/?token=should-stay-out"}},
	}
	_ = app.Process(context.Background(), input)
	reasoning := handler.findRecord(t, "url reasoning", func(r map[string]any) bool {
		return r["message_id"] == int64(16)
	})
	if got := reasoning["url_has_query"]; got != true {
		t.Errorf("url_has_query = %v, want true", got)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
