package action_test

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/sompasauna/linkkilinko/internal/config"
	"github.com/sompasauna/linkkilinko/internal/store"
	"github.com/sompasauna/linkkilinko/pkg/action"
	"github.com/sompasauna/linkkilinko/pkg/core/link"
	"github.com/sompasauna/linkkilinko/pkg/core/moderation"
	"github.com/sompasauna/linkkilinko/pkg/core/preview"
)

const (
	testURL         = "https://example.com"
	testURLHost     = "example.com"
	testEntityType  = "url"
	testContentType = "text/html"
)

type fakeTelegram struct {
	deleteHook   func(chatID int64, messageID int) error
	sendHook     func(chatID int64, threadID int, text string) (int, error)
	deleteCalled []int
	sentCalled   []sentMsg
}

type sentMsg struct {
	chatID   int64
	threadID int
	text     string
}

func (f *fakeTelegram) Delete(_ context.Context, _ int64, messageID int) error {
	f.deleteCalled = append(f.deleteCalled, messageID)
	if f.deleteHook != nil {
		return f.deleteHook(0, messageID)
	}
	return nil
}

func (f *fakeTelegram) Send(_ context.Context, chatID int64, threadID int, text string, _ ...telego.MessageEntity) (int, error) {
	f.sentCalled = append(f.sentCalled, sentMsg{chatID, threadID, text})
	if f.sendHook != nil {
		return f.sendHook(chatID, threadID, text)
	}
	return len(f.sentCalled), nil
}

func (f *fakeTelegram) Copy(_ context.Context, _ int64, _, _ int, _ string, _ ...telego.MessageEntity) (int, error) {
	return len(f.sentCalled), nil
}

type fakeNotice struct{}

func (f fakeNotice) Render(key string, _ map[string]string) string {
	switch key {
	case moderation.NoticePreviewMissing:
		return "preview missing notice"
	default:
		return "notice"
	}
}

type fakeMetadata struct {
	docs map[string]preview.Document
}

func (f *fakeMetadata) Fetch(_ context.Context, rawURL string) (preview.Document, error) {
	if doc, ok := f.docs[rawURL]; ok {
		return doc, nil
	}
	return preview.Document{}, errors.New("not found")
}

func (f *fakeMetadata) FetchWithUserAgent(_ context.Context, rawURL, _ string) (preview.Document, error) {
	return f.Fetch(context.Background(), rawURL)
}

type fakeLinks struct{}

func (f *fakeLinks) Match(_ string) bool { return false }

func (f *fakeLinks) Resolve(_ context.Context, _ string) (link.Resolution, bool, error) {
	return link.Resolution{}, false, nil
}

type fakePreviews struct {
	inspectResult preview.Metadata
}

func (f *fakePreviews) Inspect(preview.Document) (preview.Metadata, string) {
	return f.inspectResult, "test"
}

func (f *fakePreviews) IsInconclusive(preview.Document) bool { return false }

type fakeStore struct {
	memberships     map[membershipKey]store.Membership
	grandfathered   map[membershipKey]bool
	canonicalActs   map[canonicalKey]store.CanonicalAction
	nextID          int64
	deleteRequested []int64
	sendPending     []int64
	outboxComplete  []struct{ actionID, messageID int64 }
}

type membershipKey struct {
	chatID int64
	userID int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		memberships:   make(map[membershipKey]store.Membership),
		grandfathered: make(map[membershipKey]bool),
		canonicalActs: make(map[canonicalKey]store.CanonicalAction),
	}
}

func (f *fakeStore) Membership(_ context.Context, chatID, userID int64) (store.Membership, bool, error) {
	key := membershipKey{chatID, userID}
	if mem, ok := f.memberships[key]; ok {
		return mem, true, nil
	}
	if f.grandfathered[key] {
		return store.Membership{Grandfathered: true}, true, nil
	}
	return store.Membership{}, false, nil
}

func (f *fakeStore) Grandfather(_ context.Context, chatID, userID int64) error {
	f.grandfathered[membershipKey{chatID, userID}] = true
	return nil
}

func (f *fakeStore) FindCanonical(_ context.Context, chatID int64, threadID int, senderID int64, rule, _, fingerprint string) (store.CanonicalAction, bool, error) {
	key := canonicalKey{chatID: chatID, thread: threadID, userID: senderID, rule: rule, fp: fingerprint}
	if a, ok := f.canonicalActs[key]; ok {
		return a, true, nil
	}
	return store.CanonicalAction{}, false, nil
}

func (f *fakeStore) CreateCanonical(_ context.Context, a store.CanonicalAction) (store.CanonicalAction, bool, error) {
	key := canonicalKey{chatID: a.ChatID, thread: a.ThreadID, userID: a.UserID, rule: a.Rule, fp: a.Fingerprint}
	if existing, ok := f.canonicalActs[key]; ok {
		return existing, false, nil
	}
	f.nextID++
	a.ID = f.nextID
	f.canonicalActs[key] = a
	return a, true, nil
}

func (f *fakeStore) RecordSuppressed(_ context.Context, _ int64, _ int) error { return nil }
func (f *fakeStore) MarkDeleteRequested(_ context.Context, actionID int64) error {
	f.deleteRequested = append(f.deleteRequested, actionID)
	return nil
}

func (f *fakeStore) MarkSendPending(_ context.Context, actionID int64) error {
	f.sendPending = append(f.sendPending, actionID)
	return nil
}
func (f *fakeStore) MarkOutboxCopied(_ context.Context, _ int64, _ int) error { return nil }
func (f *fakeStore) MarkOutboxComplete(_ context.Context, actionID int64, responseMessageID int) error {
	f.outboxComplete = append(f.outboxComplete, struct{ actionID, messageID int64 }{actionID, int64(responseMessageID)})
	return nil
}

func (f *fakeStore) PendingOutbox(_ context.Context, _ time.Time) ([]store.OutboxEntry, error) {
	return nil, nil
}
func (f *fakeStore) ReleaseOutboxLease(_ context.Context, _ int64) error                 { return nil }
func (f *fakeStore) ReplaceOutboxPayload(_ context.Context, _ int64, _ string) error     { return nil }
func (f *fakeStore) MarkOutboxError(_ context.Context, _ int64, _ string, _ error) error { return nil }
func (f *fakeStore) MarkOutboxErrorAfter(_ context.Context, _ int64, _ string, _ error, _ time.Duration) error {
	return nil
}
func (f *fakeStore) MarkOutboxDead(_ context.Context, _ int64, _ error) error { return nil }

type canonicalKey struct {
	chatID int64
	thread int
	userID int64
	rule   string
	fp     string
}

func newTestApp(tc *fakeTelegram, st *fakeStore, md *fakeMetadata, pv *fakePreviews) *action.Application {
	cfg := config.Config{
		Moderation: config.ModerationConfig{NewcomerSandbox: config.Duration(48 * time.Hour)},
		Metadata: config.MetadataConfig{
			RequestTimeout:    config.Duration(5 * time.Second),
			TotalTimeout:      config.Duration(10 * time.Second),
			MaxHTMLBytes:      2 << 20,
			MaxRedirects:      5,
			UserAgent:         "test",
			FacebookUserAgent: "test-fb",
		},
	}
	return action.New(cfg, tc, st, md, &fakeLinks{}, pv, fakeNotice{}, time.Now)
}

func TestNewcomerShortCircuitsResolver(t *testing.T) {
	t.Parallel()
	tc, st, md, pv := &fakeTelegram{}, newFakeStore(), &fakeMetadata{docs: map[string]preview.Document{}}, &fakePreviews{}
	app := newTestApp(tc, st, md, pv)
	st.memberships[membershipKey{1, 100}] = store.Membership{ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-1 * time.Hour)}
	input := moderation.Input{
		ChatID: 1, MessageID: 10, SenderID: 100,
		Text:     "https://google.com/share?url=" + testURL,
		Entities: []moderation.Entity{{Type: testEntityType, Offset: 0, Length: 30, URL: "https://google.com/share?url=" + testURL}},
	}
	_ = app.Process(context.Background(), input)
	if len(md.docs) > 0 {
		t.Error("metadata fetched for newcomer; resolver should be short-circuited")
	}
}

func TestNewcomerShortCircuitsFetcher(t *testing.T) {
	t.Parallel()
	tc, st, md, pv := &fakeTelegram{}, newFakeStore(), &fakeMetadata{docs: map[string]preview.Document{}}, &fakePreviews{}
	app := newTestApp(tc, st, md, pv)
	st.memberships[membershipKey{1, 100}] = store.Membership{ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-1 * time.Hour)}
	input := moderation.Input{
		ChatID: 1, MessageID: 10, SenderID: 100,
		Text:     testURL,
		Entities: []moderation.Entity{{Type: testEntityType, Offset: 0, Length: len(testURL), URL: testURL}},
	}
	_ = app.Process(context.Background(), input)
	if len(md.docs) > 0 {
		t.Error("metadata fetched for newcomer; fetcher should be short-circuited")
	}
}

func TestEstablishedUserLinkOnlyNoMetadataDeleted(t *testing.T) {
	t.Parallel()
	tc, st, md, pv := &fakeTelegram{}, newFakeStore(), &fakeMetadata{
		docs: map[string]preview.Document{testURL: {URL: &url.URL{Host: testURLHost}, Body: []byte("<html></html>"), ContentType: testContentType}},
	}, &fakePreviews{}
	app := newTestApp(tc, st, md, pv)
	st.memberships[membershipKey{1, 100}] = store.Membership{ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-100 * time.Hour)}
	input := moderation.Input{
		ChatID: 1, MessageID: 10, SenderID: 100,
		Text:     testURL,
		Entities: []moderation.Entity{{Type: testEntityType, Offset: 0, Length: len(testURL), URL: testURL}},
	}
	_ = app.Process(context.Background(), input)
	if len(tc.deleteCalled) == 0 {
		t.Error("link-only message with no useful metadata should be deleted")
	}
}

func TestEstablishedUserLinkOnlyWithMetadataKept(t *testing.T) {
	t.Parallel()
	tc, st, md, pv := &fakeTelegram{}, newFakeStore(), &fakeMetadata{
		docs: map[string]preview.Document{testURL: {URL: &url.URL{Host: testURLHost}, Body: []byte("<html></html>"), ContentType: testContentType}},
	}, &fakePreviews{inspectResult: preview.Metadata{Title: "Example Page"}}
	app := newTestApp(tc, st, md, pv)
	st.memberships[membershipKey{1, 100}] = store.Membership{ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-100 * time.Hour)}
	input := moderation.Input{
		ChatID: 1, MessageID: 10, SenderID: 100,
		Text:     testURL,
		Entities: []moderation.Entity{{Type: testEntityType, Offset: 0, Length: len(testURL), URL: testURL}},
	}
	_ = app.Process(context.Background(), input)
	if len(tc.deleteCalled) > 0 {
		t.Error("message with useful metadata should not be deleted")
	}
}

func TestDeleteFailureDoesNotSend(t *testing.T) {
	t.Parallel()
	tc := &fakeTelegram{}
	tc.deleteHook = func(_ int64, _ int) error { return errors.New("permission denied") }
	st, md, pv := newFakeStore(), &fakeMetadata{
		docs: map[string]preview.Document{testURL: {URL: &url.URL{Host: testURLHost}, Body: []byte("<html></html>"), ContentType: testContentType}},
	}, &fakePreviews{}
	app := newTestApp(tc, st, md, pv)
	st.memberships[membershipKey{1, 100}] = store.Membership{ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-100 * time.Hour)}
	input := moderation.Input{
		ChatID: 1, MessageID: 10, SenderID: 100,
		Text:     testURL,
		Entities: []moderation.Entity{{Type: testEntityType, Offset: 0, Length: len(testURL), URL: testURL}},
	}
	_ = app.Process(context.Background(), input)
	if len(tc.sentCalled) > 0 {
		t.Error("should not send after delete failure")
	}
}

func TestForumTopicPlacement(t *testing.T) {
	t.Parallel()
	tc, st, md, pv := &fakeTelegram{}, newFakeStore(), &fakeMetadata{
		docs: map[string]preview.Document{testURL: {URL: &url.URL{Host: testURLHost}, Body: []byte("<html></html>"), ContentType: testContentType}},
	}, &fakePreviews{}
	app := newTestApp(tc, st, md, pv)
	st.memberships[membershipKey{1, 100}] = store.Membership{ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-100 * time.Hour)}
	threadID := 42
	input := moderation.Input{
		ChatID: 1, ThreadID: threadID, MessageID: 10, SenderID: 100,
		Text:     testURL,
		Entities: []moderation.Entity{{Type: testEntityType, Offset: 0, Length: len(testURL), URL: testURL}},
	}
	_ = app.Process(context.Background(), input)
	if len(tc.sentCalled) == 0 {
		t.Fatal("expected a sent message")
	}
	if tc.sentCalled[0].threadID != threadID {
		t.Errorf("sent to thread %d, want %d", tc.sentCalled[0].threadID, threadID)
	}
}

func TestRepostReusesPendingOutboxItem(t *testing.T) {
	t.Parallel()
	tc := &fakeTelegram{}
	st, md, pv := newFakeStore(), &fakeMetadata{
		docs: map[string]preview.Document{testURL: {URL: &url.URL{Host: testURLHost}, Body: []byte("<html></html>"), ContentType: testContentType}},
	}, &fakePreviews{}
	app := newTestApp(tc, st, md, pv)
	st.memberships[membershipKey{1, 100}] = store.Membership{ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-100 * time.Hour)}
	input := moderation.Input{
		ChatID: 1, MessageID: 10, SenderID: 100,
		Text:     testURL,
		Entities: []moderation.Entity{{Type: testEntityType, Offset: 0, Length: len(testURL), URL: testURL}},
	}
	_ = app.Process(context.Background(), input)
	if len(st.deleteRequested) == 0 {
		t.Fatal("first message should have delete requested")
	}
	canonicalBefore := len(st.canonicalActs)
	tc.deleteCalled = nil
	tc.sentCalled = nil
	st.deleteRequested = nil
	st.sendPending = nil
	_ = app.Process(context.Background(), input)
	if len(st.canonicalActs) != canonicalBefore {
		t.Errorf("canonical action count changed unexpectedly")
	}
	if len(st.deleteRequested) > 0 {
		t.Error("repost should not create new delete request")
	}
}

func TestMarkupInjectionResistance(t *testing.T) {
	t.Parallel()
	tc, st, md, pv := &fakeTelegram{}, newFakeStore(), &fakeMetadata{
		docs: map[string]preview.Document{"https://evil.com": {URL: &url.URL{Host: "evil.com"}, Body: []byte("<html></html>"), ContentType: "text/html"}},
	}, &fakePreviews{}
	app := newTestApp(tc, st, md, pv)
	st.memberships[membershipKey{1, 100}] = store.Membership{ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-100 * time.Hour)}
	maliciousText := "https://evil.com <script>alert('xss')</script>"
	input := moderation.Input{
		ChatID: 1, MessageID: 10, SenderID: 100,
		Text:     maliciousText,
		Entities: []moderation.Entity{{Type: testEntityType, Offset: 0, Length: len(maliciousText), URL: maliciousText}},
	}
	_ = app.Process(context.Background(), input)
	for _, msg := range tc.sentCalled {
		if len(msg.text) > 0 && contains(msg.text, "<script>") {
			t.Error("sent text contains potential markup injection")
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsAt(s, substr, 0))
}

func containsAt(s, substr string, start int) bool {
	for i := start; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
