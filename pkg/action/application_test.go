package action_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
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
	testShareRule   = "google-share"

	outboxStateSendPending = "send_pending"
	outboxStateDead        = "dead"
)

type fakeTelegram struct {
	deleteHook   func(chatID int64, messageID int) error
	sendHook     func(chatID int64, threadID int, text string) (int, error)
	deleteCalled []int
	sentCalled   []sentMsg
	repliedTo    []int
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

func (f *fakeTelegram) Reply(ctx context.Context, chatID int64, threadID, _ int, text string, entities ...telego.MessageEntity) (int, error) {
	f.repliedTo = append(f.repliedTo, 1)
	return f.Send(ctx, chatID, threadID, text, entities...)
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
	err  error
}

func (f *fakeMetadata) Fetch(_ context.Context, rawURL string) (preview.Document, error) {
	if f.err != nil {
		return preview.Document{}, f.err
	}
	if doc, ok := f.docs[rawURL]; ok {
		return doc, nil
	}
	return preview.Document{}, errors.New("not found")
}

// fakeLinks matches nothing by default; tests that exercise the wrapper path
// populate destinations with the resolution each tracked URL yields.
type fakeLinks struct {
	destinations map[string]string
}

func (f *fakeLinks) MatchName(rawURL string) (string, bool) {
	_, ok := f.destinations[rawURL]
	if !ok {
		return "", false
	}
	parsed, err := url.Parse(rawURL)
	if err == nil && (parsed.Hostname() == "www.google.com" || parsed.Hostname() == "amp.example.com") {
		return "amp", true
	}
	return testShareRule, true
}

func (f *fakeLinks) Resolve(_ context.Context, rawURL string) (link.Resolution, bool, error) {
	target, ok := f.destinations[rawURL]
	if !ok {
		return link.Resolution{}, false, nil
	}
	original, err := url.Parse(rawURL)
	if err != nil {
		return link.Resolution{}, false, err
	}
	destination, err := url.Parse(target)
	if err != nil {
		return link.Resolution{}, false, err
	}
	name, _ := f.MatchName(rawURL)
	return link.Resolution{Original: original, Destination: destination, Resolver: name}, true, nil
}

type fakePreviews struct {
	inspectResult preview.Metadata
}

func (f *fakePreviews) Inspect(preview.Document) (preview.Metadata, string) {
	return f.inspectResult, "test"
}

type fakeStore struct {
	memberships     map[membershipKey]store.Membership
	grandfathered   map[membershipKey]bool
	knownGood       map[string]bool
	canonicalActs   map[canonicalKey]store.CanonicalAction
	nextID          int64
	deleteRequested []int64
	sendPending     []int64
	outboxComplete  []struct{ actionID, messageID int64 }
	outbox          map[int64]outboxEntry
}

type outboxEntry struct {
	state             string
	attempts          int
	nextAttemptAt     time.Time
	leaseUntil        time.Time
	responseMessageID int
	payload           string
	canonicalActionID int64
	chatID            int64
	threadID          int
	sourceMessageID   int
}

type membershipKey struct {
	chatID int64
	userID int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		memberships:   make(map[membershipKey]store.Membership),
		grandfathered: make(map[membershipKey]bool),
		knownGood:     make(map[string]bool),
		canonicalActs: make(map[canonicalKey]store.CanonicalAction),
		outbox:        make(map[int64]outboxEntry),
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

func (f *fakeStore) KnownGoodPreviewDomain(_ context.Context, host string) (bool, error) {
	return f.knownGood[host], nil
}

func (f *fakeStore) RecordKnownGoodPreviewDomain(_ context.Context, host string) error {
	f.knownGood[host] = true
	return nil
}

func (f *fakeStore) FindCanonical(_ context.Context, chatID int64, threadID int, senderID int64, rule, behaviorVersion, fingerprint string) (store.CanonicalAction, bool, error) {
	key := canonicalKey{chatID: chatID, thread: threadID, userID: senderID, rule: rule, version: behaviorVersion, fp: fingerprint}
	if a, ok := f.canonicalActs[key]; ok {
		return a, true, nil
	}
	return store.CanonicalAction{}, false, nil
}

func (f *fakeStore) CreateCanonical(_ context.Context, a store.CanonicalAction) (store.CanonicalAction, bool, error) {
	key := canonicalKeyFor(a)
	if existing, ok := f.canonicalActs[key]; ok {
		return existing, false, nil
	}
	f.nextID++
	a.ID = f.nextID
	f.canonicalActs[key] = a
	f.outbox[a.ID] = outboxEntry{
		state:             "planned",
		payload:           a.Payload,
		canonicalActionID: a.ID,
		chatID:            a.ChatID,
		threadID:          a.ThreadID,
		sourceMessageID:   a.SourceMessageID,
		leaseUntil:        time.Now().Add(time.Minute),
	}
	return a, true, nil
}

func (f *fakeStore) RecordSuppressed(_ context.Context, _ int64, _ int) error { return nil }
func (f *fakeStore) MarkDeleteRequested(_ context.Context, actionID int64) error {
	f.deleteRequested = append(f.deleteRequested, actionID)
	e := f.outbox[actionID]
	e.state = "delete_requested"
	e.leaseUntil = time.Now().Add(time.Minute)
	e.canonicalActionID = actionID
	f.outbox[actionID] = e
	return nil
}

func (f *fakeStore) MarkSendPending(_ context.Context, actionID int64) error {
	f.sendPending = append(f.sendPending, actionID)
	e := f.outbox[actionID]
	e.state = outboxStateSendPending
	e.leaseUntil = time.Now().Add(time.Minute)
	f.outbox[actionID] = e
	return nil
}

func (f *fakeStore) MarkOutboxCopied(_ context.Context, actionID int64, responseMessageID int) error {
	e := f.outbox[actionID]
	e.responseMessageID = responseMessageID
	f.outbox[actionID] = e
	return nil
}

func (f *fakeStore) MarkOutboxComplete(_ context.Context, actionID int64, responseMessageID int) error {
	f.outboxComplete = append(f.outboxComplete, struct{ actionID, messageID int64 }{actionID, int64(responseMessageID)})
	e := f.outbox[actionID]
	e.state = "complete"
	e.responseMessageID = responseMessageID
	e.leaseUntil = time.Time{}
	f.outbox[actionID] = e
	return nil
}

func (f *fakeStore) PendingOutbox(_ context.Context, now time.Time) ([]store.OutboxEntry, error) {
	var entries []store.OutboxEntry
	for id, e := range f.outbox {
		if e.state == "complete" || e.state == outboxStateDead {
			continue
		}
		if e.leaseUntil.After(now) {
			continue
		}
		entries = append(entries, store.OutboxEntry{
			ID:                id,
			CanonicalActionID: e.canonicalActionID,
			ChatID:            e.chatID,
			ThreadID:          e.threadID,
			SourceMessageID:   e.sourceMessageID,
			Payload:           e.payload,
			State:             e.state,
			Attempts:          e.attempts,
			NextAttemptAt:     e.nextAttemptAt,
			ResponseMessageID: e.responseMessageID,
			LeaseUntil:        e.leaseUntil,
		})
	}
	return entries, nil
}

func (f *fakeStore) ReleaseOutboxLease(_ context.Context, actionID int64) error {
	e := f.outbox[actionID]
	e.leaseUntil = time.Time{}
	f.outbox[actionID] = e
	return nil
}

func (f *fakeStore) ReplaceOutboxPayload(_ context.Context, actionID int64, payload string) error {
	e := f.outbox[actionID]
	e.payload = payload
	f.outbox[actionID] = e
	return nil
}

func (f *fakeStore) MarkOutboxError(_ context.Context, actionID int64, state string, _ error) error {
	e := f.outbox[actionID]
	e.attempts++
	e.state = state
	e.nextAttemptAt = time.Now()
	e.leaseUntil = time.Time{}
	f.outbox[actionID] = e
	return nil
}

func (f *fakeStore) MarkOutboxErrorAfter(_ context.Context, actionID int64, state string, _ error, delay time.Duration) error {
	e := f.outbox[actionID]
	e.attempts++
	e.state = state
	e.nextAttemptAt = time.Now().Add(delay)
	e.leaseUntil = time.Now().Add(delay).Add(time.Minute)
	f.outbox[actionID] = e
	return nil
}

func (f *fakeStore) MarkOutboxDead(_ context.Context, actionID int64, _ error) error {
	e := f.outbox[actionID]
	e.state = outboxStateDead
	e.leaseUntil = time.Time{}
	f.outbox[actionID] = e
	return nil
}

// canonicalKey mirrors the real store's
// UNIQUE(chat_id, thread_id, user_id, rule, behavior_version, fingerprint).
// behavior_version belongs here: without it the fake would suppress across a
// version bump that the real schema treats as a distinct row.
type canonicalKey struct {
	chatID  int64
	thread  int
	userID  int64
	rule    string
	version string
	fp      string
}

func canonicalKeyFor(a store.CanonicalAction) canonicalKey {
	return canonicalKey{
		chatID:  a.ChatID,
		thread:  a.ThreadID,
		userID:  a.UserID,
		rule:    a.Rule,
		version: a.BehaviorVersion,
		fp:      a.Fingerprint,
	}
}

func newTestApp(tc *fakeTelegram, st *fakeStore, md *fakeMetadata, pv *fakePreviews) *action.Application {
	return newTestAppWithLinks(tc, st, md, pv, &fakeLinks{})
}

func newTestAppWithLinks(tc *fakeTelegram, st *fakeStore, md *fakeMetadata, pv *fakePreviews, links *fakeLinks) *action.Application {
	cfg := config.Config{
		Moderation: config.ModerationConfig{NewcomerSandbox: config.Duration(48 * time.Hour)},
		Metadata: config.MetadataConfig{
			RequestTimeout: config.Duration(5 * time.Second),
			TotalTimeout:   config.Duration(10 * time.Second),
			MaxHTMLBytes:   2 << 20,
			MaxRedirects:   5,
			UserAgent:      "test",
		},
	}
	return action.New(cfg, tc, st, md, links, pv, fakeNotice{}, time.Now)
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

// TestNewcomerCannotBypassSandboxWithSchemelessURL is t-019 regression
// coverage item 6: a scheme-less url entity (recognized by Telegram without
// an http/https prefix) must trip the newcomer sandbox exactly like its
// explicit-scheme spelling, and must not reach the resolver or fetcher.
func TestNewcomerCannotBypassSandboxWithSchemelessURL(t *testing.T) {
	t.Parallel()
	tc, st, md, pv := &fakeTelegram{}, newFakeStore(), &fakeMetadata{docs: map[string]preview.Document{}}, &fakePreviews{}
	app := newTestApp(tc, st, md, pv)
	st.memberships[membershipKey{1, 100}] = store.Membership{ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-1 * time.Hour)}
	schemeless := "example.com/path"
	input := moderation.Input{
		ChatID: 1, MessageID: 10, SenderID: 100,
		Text:     schemeless,
		Entities: []moderation.Entity{{Type: testEntityType, Offset: 0, Length: len(schemeless)}},
	}
	_ = app.Process(context.Background(), input)
	if len(tc.deleteCalled) == 0 {
		t.Error("newcomer posting a scheme-less link should still be sandboxed")
	}
	if len(md.docs) > 0 {
		t.Error("metadata fetched for newcomer; scheme-less URL should still short-circuit the fetcher")
	}
}

// TestEstablishedSchemelessLinkOnlyReachesMetadataAndPreviewPolicy is t-019
// regression coverage item 7: an established sender's link-only post using a
// scheme-less url entity must reach the metadata and preview policy stage,
// keyed by its canonical https target, the same as an explicit spelling.
func TestEstablishedSchemelessLinkOnlyReachesMetadataAndPreviewPolicy(t *testing.T) {
	t.Parallel()
	schemeless := "example.com/path"
	canonical := "https://" + schemeless
	tc, st, md, pv := &fakeTelegram{}, newFakeStore(), &fakeMetadata{
		docs: map[string]preview.Document{canonical: {URL: &url.URL{Host: testURLHost}, Body: []byte("<html></html>"), ContentType: testContentType}},
	}, &fakePreviews{inspectResult: preview.Metadata{Title: "Example Page"}}
	app := newTestApp(tc, st, md, pv)
	st.memberships[membershipKey{1, 100}] = store.Membership{ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-100 * time.Hour)}
	input := moderation.Input{
		ChatID: 1, MessageID: 10, SenderID: 100,
		Text:     schemeless,
		Entities: []moderation.Entity{{Type: testEntityType, Offset: 0, Length: len(schemeless)}},
	}
	_ = app.Process(context.Background(), input)
	if len(tc.deleteCalled) > 0 {
		t.Error("scheme-less link-only post with useful metadata should not be deleted")
	}
}

func TestDisabledPreviewKeepsOriginalAndRepliesWithMetadata(t *testing.T) {
	t.Parallel()
	tc, st, md, pv := &fakeTelegram{}, newFakeStore(), &fakeMetadata{
		docs: map[string]preview.Document{testURL: {
			URL: &url.URL{Host: testURLHost}, Body: []byte("<html></html>"), ContentType: testContentType,
		}},
	}, &fakePreviews{inspectResult: preview.Metadata{Title: "Example Page"}}
	app := newTestApp(tc, st, md, pv)
	st.memberships[membershipKey{1, 100}] = store.Membership{
		ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-100 * time.Hour),
	}
	input := moderation.Input{
		ChatID: 1, MessageID: 10, SenderID: 100, PreviewDisabled: true,
		Text: testURL, Entities: []moderation.Entity{{
			Type: testEntityType, Offset: 0, Length: len(testURL), URL: testURL,
		}},
	}
	if err := app.Process(context.Background(), input); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(tc.deleteCalled) != 0 {
		t.Fatal("disabled-preview metadata reply deleted the original message")
	}
	if len(tc.repliedTo) != 1 {
		t.Fatalf("metadata replies = %d, want 1", len(tc.repliedTo))
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

// Resolver rule-name changes affect values stored
// in canonical_actions.rule, so it must ship with a behaviorVersion bump.
// The legacy row here is seeded at the *new* rule name and the old version: if
// behaviorVersion were reverted to v0.1 the application would find it and
// silently suppress, so this fails on a missing bump rather than on the rename.
func TestGoogleWrapperSupersedesLegacyBehaviorVersion(t *testing.T) {
	t.Parallel()
	const (
		wrapped = "https://share.google/abc"
		target  = "https://news.example/article"
	)
	tc, st, md, pv := &fakeTelegram{}, newFakeStore(), &fakeMetadata{docs: map[string]preview.Document{}}, &fakePreviews{}
	links := &fakeLinks{destinations: map[string]string{wrapped: target}}
	app := newTestAppWithLinks(tc, st, md, pv, links)
	st.memberships[membershipKey{1, 100}] = store.Membership{ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-100 * time.Hour)}
	input := moderation.Input{
		ChatID: 1, MessageID: 10, SenderID: 100,
		Text:     "look " + wrapped,
		Entities: []moderation.Entity{{Type: testEntityType, Offset: 5, Length: len(wrapped), URL: wrapped}},
	}

	legacy := store.CanonicalAction{
		ID: 4242, ChatID: 1, UserID: 100,
		Rule: testShareRule, BehaviorVersion: "v0.1",
		Fingerprint: moderation.Fingerprint(input, testShareRule),
		Payload:     "legacy payload",
	}
	st.canonicalActs[canonicalKeyFor(legacy)] = legacy

	if err := app.Process(context.Background(), input); err != nil {
		t.Fatalf("process: %v", err)
	}

	var current store.CanonicalAction
	for _, act := range st.canonicalActs {
		if act.ID != legacy.ID {
			current = act
		}
	}
	if current.ID == 0 {
		t.Fatal("no new canonical action created; the legacy v0.1 row was reused")
	}
	if current.BehaviorVersion == legacy.BehaviorVersion {
		t.Fatalf("behavior version = %q; the rule rename must ship with a bump", current.BehaviorVersion)
	}
	if current.Rule != testShareRule {
		t.Fatalf("rule = %q, want google-share", current.Rule)
	}
	if len(st.deleteRequested) == 0 {
		t.Error("expected the wrapper message to be moderated rather than suppressed")
	}
}

// AMP URLs must be recorded under their own rule name so the two resolvers
// keep separate suppression state.
func TestAMPUsesItsOwnRuleName(t *testing.T) {
	t.Parallel()
	const (
		wrapped = "https://www.google.com/amp/s/news.example/article"
		target  = "https://news.example/article"
	)
	tc, st, md, pv := &fakeTelegram{}, newFakeStore(), &fakeMetadata{docs: map[string]preview.Document{}}, &fakePreviews{}
	links := &fakeLinks{destinations: map[string]string{wrapped: target}}
	app := newTestAppWithLinks(tc, st, md, pv, links)
	st.memberships[membershipKey{1, 100}] = store.Membership{ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-100 * time.Hour)}
	input := moderation.Input{
		ChatID: 1, MessageID: 10, SenderID: 100,
		Text:     "look " + wrapped,
		Entities: []moderation.Entity{{Type: testEntityType, Offset: 5, Length: len(wrapped), URL: wrapped}},
	}
	if err := app.Process(context.Background(), input); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(st.canonicalActs) != 1 {
		t.Fatalf("canonical actions = %d, want 1", len(st.canonicalActs))
	}
	for _, act := range st.canonicalActs {
		if act.Rule != "amp" {
			t.Fatalf("rule = %q, want amp", act.Rule)
		}
	}
}

// MixedURLsUseLowestIndexRule ensures that when a message contains multiple
// tracked URL types, the rule is derived from the first matching URL in message
// order rather than map iteration order.
func TestMixedURLsUseLowestIndexRule(t *testing.T) {
	t.Parallel()
	const (
		shareURL    = "https://share.google/abc"
		ampURL      = "https://www.google.com/amp/s/news.example/article"
		shareTarget = "https://example.com/article"
		ampTarget   = "https://news.example/article"
	)
	tc, st, md, pv := &fakeTelegram{}, newFakeStore(), &fakeMetadata{docs: map[string]preview.Document{}}, &fakePreviews{}
	links := &fakeLinks{destinations: map[string]string{shareURL: shareTarget, ampURL: ampTarget}}
	app := newTestAppWithLinks(tc, st, md, pv, links)
	st.memberships[membershipKey{1, 100}] = store.Membership{ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-100 * time.Hour)}
	input := moderation.Input{
		ChatID: 1, MessageID: 10, SenderID: 100,
		Text: shareURL + " " + ampURL,
		Entities: []moderation.Entity{
			{Type: testEntityType, Offset: 0, Length: len(shareURL), URL: shareURL},
			{Type: testEntityType, Offset: len(shareURL) + 1, Length: len(ampURL), URL: ampURL},
		},
	}
	if err := app.Process(context.Background(), input); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(st.canonicalActs) != 1 {
		t.Fatalf("canonical actions = %d, want 1", len(st.canonicalActs))
	}
	for _, act := range st.canonicalActs {
		if act.Rule != "google-share" {
			t.Errorf("rule = %q, want google-share (from first URL)", act.Rule)
		}
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

func TestTrackedLinkRewritingBeforePreviewPolicy(t *testing.T) {
	t.Parallel()
	const (
		wrapper = "https://share.google/abc"
		target  = "https://example.com/no-metadata"
	)
	tc, st, md, pv := &fakeTelegram{}, newFakeStore(), &fakeMetadata{docs: map[string]preview.Document{}}, &fakePreviews{}
	links := &fakeLinks{destinations: map[string]string{wrapper: target}}
	app := newTestAppWithLinks(tc, st, md, pv, links)
	st.memberships[membershipKey{1, 100}] = store.Membership{ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-100 * time.Hour)}
	input := moderation.Input{
		ChatID: 1, MessageID: 10, SenderID: 100,
		Text:     wrapper,
		Entities: []moderation.Entity{{Type: testEntityType, Offset: 0, Length: len(wrapper), URL: wrapper}},
	}
	if err := app.Process(context.Background(), input); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(tc.deleteCalled) == 0 {
		t.Fatal("wrapper link should be replaced, requiring original deletion")
	}
	if len(md.docs) > 0 {
		t.Error("metadata should not be fetched for wrapper URL; rewriting happens first")
	}
}

func TestSendFailureAfterSuccessfulDelete(t *testing.T) {
	t.Parallel()
	tc := &fakeTelegram{}
	sendErr := errors.New("rate limit")
	tc.deleteHook = func(_ int64, _ int) error { return nil }
	tc.sendHook = func(_ int64, _ int, _ string) (int, error) { return 0, sendErr }
	st, md, pv := newFakeStore(), &fakeMetadata{
		docs: map[string]preview.Document{testURL: {URL: &url.URL{Host: testURLHost}, Body: []byte("<html><title>Title</title></html>"), ContentType: testContentType}},
	}, &fakePreviews{}
	app := newTestApp(tc, st, md, pv)
	st.memberships[membershipKey{1, 100}] = store.Membership{ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-100 * time.Hour)}
	input := moderation.Input{
		ChatID: 1, MessageID: 10, SenderID: 100,
		Text:     testURL,
		Entities: []moderation.Entity{{Type: testEntityType, Offset: 0, Length: len(testURL), URL: testURL}},
	}
	if err := app.Process(context.Background(), input); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(tc.deleteCalled) == 0 {
		t.Fatal("delete should have been called")
	}
	if len(st.sendPending) == 0 {
		t.Fatal("send should have been marked pending")
	}
	entries, err := st.PendingOutbox(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("pending outbox: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("outbox should retain the failed item for retry")
	}
	if entries[0].State != outboxStateSendPending {
		t.Errorf("state = %q, want send_pending", entries[0].State)
	}
}

// TestLoginWallTitleSkipsEnrichedReplace is the regression coverage for the
// bare-site-name login-wall shape described in t-014: even when the sender
// disabled Telegram's link preview, a fetched document whose only metadata is
// a login-wall-shaped <title> (e.g. <title>Facebook</title> on
// mbasic.facebook.com with no description) must not take the
// preview-enriched replace branch. Useful() reports false for that shape and
// the message must fall to the no-metadata delete branch instead, so the user
// sees the preview-missing notice rather than a bot-authored reposting of the
// login wall's title.
func TestLoginWallTitleSkipsEnrichedReplace(t *testing.T) {
	t.Parallel()
	const (
		loginWallURL  = "https://mbasic.facebook.com/share/18uiPcLZw1"
		loginWallHost = "mbasic.facebook.com"
	)
	tc, st, md, pv := &fakeTelegram{}, newFakeStore(), &fakeMetadata{
		docs: map[string]preview.Document{
			loginWallURL: {
				URL:         &url.URL{Scheme: "https", Host: loginWallHost, Path: "/share/18uiPcLZw1"},
				Body:        []byte(`<title>Facebook</title>`),
				ContentType: testContentType,
			},
		},
	}, &fakePreviews{
		// Mirrors what preview.Registry.Inspect extracts from the body above:
		// a HTML-only <title> that matches the registrable domain, with no
		// description, so Useful() reports false.
		inspectResult: preview.Metadata{Title: "Facebook", Host: loginWallHost, TitleFallback: true},
	}
	app := newTestApp(tc, st, md, pv)
	st.memberships[membershipKey{1, 100}] = store.Membership{ChatID: 1, UserID: 100, JoinedAt: time.Now().Add(-100 * time.Hour)}
	input := moderation.Input{
		ChatID: 1, MessageID: 30, SenderID: 100, SenderName: "alice",
		Text:            loginWallURL,
		Entities:        []moderation.Entity{{Type: testEntityType, Offset: 0, Length: len(loginWallURL), URL: loginWallURL}},
		PreviewDisabled: true,
	}
	if err := app.Process(context.Background(), input); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(tc.deleteCalled) == 0 {
		t.Fatal("login-wall link-only message should be deleted by the no-metadata path")
	}
	if len(tc.sentCalled) == 0 {
		t.Fatal("expected the preview-missing notice to be sent")
	}
	if text := tc.sentCalled[0].text; text != "preview missing notice" {
		t.Errorf("sent text = %q, want %q (enriched-replace branch must not fire for login-wall titles)", text, "preview missing notice")
	}
	if len(st.canonicalActs) == 0 {
		t.Fatal("canonical action should be created for the no-metadata delete plan")
	}
	for _, act := range st.canonicalActs {
		if !strings.Contains(act.Payload, "preview missing notice") {
			t.Errorf("canonical payload should carry the preview-missing notice text, got %q", act.Payload)
		}
	}
}

func TestDeadLetterAfterRepeatedFailures(t *testing.T) {
	t.Parallel()
	st := newFakeStore()
	st.outbox[1] = outboxEntry{
		state:             outboxStateSendPending,
		attempts:          2,
		nextAttemptAt:     time.Now().Add(-time.Hour),
		leaseUntil:        time.Now().Add(-time.Hour),
		responseMessageID: 0,
		payload:           `{"text":"test","operation":"text"}`,
		canonicalActionID: 1,
		chatID:            1,
		threadID:          0,
		sourceMessageID:   10,
	}
	if err := st.MarkOutboxDead(context.Background(), 1, errors.New("permanent failure")); err != nil {
		t.Fatalf("mark outbox dead: %v", err)
	}
	e := st.outbox[1]
	if e.state != outboxStateDead {
		t.Errorf("state = %q, want dead", e.state)
	}
	if !e.leaseUntil.IsZero() {
		t.Error("lease should be released after dead letter")
	}
	entries, err := st.PendingOutbox(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("pending outbox: %v", err)
	}
	for _, e := range entries {
		if e.CanonicalActionID == 1 {
			t.Error("dead entry should not be returned by pending outbox")
		}
	}
}

func TestRestartRecoveryAtEachDurableState(t *testing.T) {
	t.Parallel()
	now := time.Now()
	testCases := []struct {
		name  string
		state string
		lease time.Time
	}{
		{name: "pending_planned", state: "planned", lease: now.Add(-time.Hour)},
		{name: "pending_send_pending", state: outboxStateSendPending, lease: now.Add(-time.Hour)},
		{name: "pending_lease_expired", state: outboxStateSendPending, lease: now.Add(-time.Hour)},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := newFakeStore()
			st.outbox[1] = outboxEntry{
				state:             tc.state,
				attempts:          0,
				nextAttemptAt:     now.Add(-time.Hour),
				leaseUntil:        tc.lease,
				responseMessageID: 0,
				payload:           `{"text":"test","operation":"text"}`,
				canonicalActionID: 1,
				chatID:            1,
				threadID:          0,
				sourceMessageID:   10,
			}
			entries, err := st.PendingOutbox(context.Background(), now)
			if err != nil {
				t.Fatalf("pending outbox: %v", err)
			}
			found := false
			for _, e := range entries {
				if e.CanonicalActionID == 1 {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("state=%q lease=%v should be recovered after restart", tc.state, tc.lease)
			}
		})
	}
}
