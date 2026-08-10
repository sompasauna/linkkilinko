package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sompasauna/linkkilinko/internal/store"
)

// testNoticePayload is a placeholder outbox payload shared by tests that
// only need a canonical action to exist, not any particular payload text.
const testNoticePayload = "notice"

// newOutboxTestAction returns a minimal canonical action fixture for tests
// that only need a single pending outbox entry to exist.
func newOutboxTestAction() store.CanonicalAction {
	return store.CanonicalAction{ChatID: 1, UserID: 2, Rule: "rule", BehaviorVersion: "v1", Fingerprint: "fp", Payload: testNoticePayload}
}

func TestOwnerBootstrapAndApprovedChatPersistAcrossRestart(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "state.sqlite")
	state, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := state.RegisterOwner(ctx, 42)
	if err != nil || !registered {
		t.Fatalf("first owner registration = %v, err=%v", registered, err)
	}
	registered, err = state.RegisterOwner(ctx, 99)
	if err != nil || registered {
		t.Fatalf("second owner registration = %v, err=%v", registered, err)
	}
	if err := state.ApproveChat(ctx, -1001, 42); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}

	state, err = store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	owner, found, err := state.Owner(ctx)
	if err != nil || !found || owner != 42 {
		t.Fatalf("owner=%d found=%v err=%v", owner, found, err)
	}
	approved, err := state.ApprovedChat(ctx, -1001)
	if err != nil || !approved {
		t.Fatalf("approved=%v err=%v", approved, err)
	}
}

func TestUnapprovedChatIsInert(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(ctx, "file::memory:?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	approved, err := state.ApprovedChat(ctx, 9999)
	if err != nil || approved {
		t.Fatalf("unapproved chat should be inert: approved=%v err=%v", approved, err)
	}
}

func TestMembershipRejoinStartsNewWindow(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(ctx, "file::memory:?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = state.Close() }()
	first := time.Unix(100, 0).UTC()
	second := time.Unix(200, 0).UTC()
	if err := state.RecordMembership(ctx, 1, 2, "member", first, true); err != nil {
		t.Fatal(err)
	}
	if err := state.RecordMembership(ctx, 1, 2, "left", second, false); err != nil {
		t.Fatal(err)
	}
	if err := state.RecordMembership(ctx, 1, 2, "member", second, true); err != nil {
		t.Fatal(err)
	}
	membership, found, err := state.Membership(ctx, 1, 2)
	if err != nil || !found || !membership.JoinedAt.Equal(second) {
		t.Fatalf("membership=%#v found=%v err=%v", membership, found, err)
	}
}

func TestCanonicalCreationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(ctx, "file::memory:?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = state.Close() }()
	action := newOutboxTestAction()
	first, created, err := state.CreateCanonical(ctx, action)
	if err != nil || !created || first.ID == 0 {
		t.Fatalf("first=%#v created=%v err=%v", first, created, err)
	}
	second, created, err := state.CreateCanonical(ctx, action)
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("second=%#v created=%v err=%v", second, created, err)
	}
	if err := state.AttachSourceMessage(ctx, first.ID, 44); err != nil {
		t.Fatal(err)
	}
	entries, err := state.PendingOutbox(ctx, time.Now().Add(time.Second))
	if err != nil || len(entries) != 1 || entries[0].SourceMessageID != 44 {
		t.Fatalf("outbox=%#v err=%v", entries, err)
	}
	if err := state.MarkSendPending(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := state.MarkOutboxComplete(ctx, first.ID, 99); err != nil {
		t.Fatal(err)
	}
	entries, err = state.PendingOutbox(ctx, time.Now().Add(time.Second))
	if err != nil || len(entries) != 0 {
		t.Fatalf("completed outbox=%#v err=%v", entries, err)
	}
}

// Splitting google-wrapper into stable resolver rules renames a value
// that is persisted in canonical_actions.rule and is part of the uniqueness
// key driving repost suppression. The rename must ship with a behavior_version
// bump so v0.1 rows are cleanly superseded rather than half-matching. This
// pins behavior_version as the discriminator: it fails if the bump is reverted
// while the split rule names stay.
func TestCanonicalBehaviorVersionSupersedesOldRule(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(ctx, "file::memory:?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = state.Close() }()

	const (
		chatID   = int64(9001)
		threadID = 0
		userID   = int64(9002)
		oldRule  = "google-wrapper"
		newRule  = "google-share"
		oldVer   = "v0.1"
		newVer   = "v0.2"
		fp       = "shared-fingerprint"
	)
	legacy := store.CanonicalAction{
		ChatID: chatID, ThreadID: threadID, UserID: userID,
		Rule: oldRule, BehaviorVersion: oldVer, Fingerprint: fp, Payload: "legacy notice",
	}
	stored, created, err := state.CreateCanonical(ctx, legacy)
	if err != nil || !created {
		t.Fatalf("seed legacy action: created=%v err=%v", created, err)
	}

	// The legacy row is superseded, not destroyed: a v0.1 lookup still sees it.
	found, ok, err := state.FindCanonical(ctx, chatID, threadID, userID, oldRule, oldVer, fp)
	if err != nil || !ok || found.ID != stored.ID {
		t.Fatalf("legacy lookup: action=%#v found=%v err=%v", found, ok, err)
	}

	// The renamed rule at the new version must not match the legacy row.
	if _, ok, err := state.FindCanonical(ctx, chatID, threadID, userID, newRule, newVer, fp); err != nil || ok {
		t.Fatalf("v0.2 %s lookup matched a v0.1 %s row: found=%v err=%v", newRule, oldRule, ok, err)
	}

	// The version alone is enough: even at the unchanged rule name, a v0.2
	// lookup must miss. Without this the test would pass on the rename alone.
	if _, ok, err := state.FindCanonical(ctx, chatID, threadID, userID, oldRule, newVer, fp); err != nil || ok {
		t.Fatalf("v0.2 lookup matched a v0.1 row at the same rule: found=%v err=%v", ok, err)
	}

	// A v0.2 write must win the uniqueness race outright rather than colliding
	// with the legacy row and returning its stale response_message_id.
	current := store.CanonicalAction{
		ChatID: chatID, ThreadID: threadID, UserID: userID,
		Rule: newRule, BehaviorVersion: newVer, Fingerprint: fp, Payload: "current notice",
	}
	fresh, created, err := state.CreateCanonical(ctx, current)
	if err != nil || !created {
		t.Fatalf("v0.2 action did not win the uniqueness race: created=%v err=%v", created, err)
	}
	if fresh.ID == stored.ID {
		t.Fatalf("v0.2 action reused the legacy row id %d", stored.ID)
	}
	if fresh.Payload != current.Payload {
		t.Fatalf("payload = %q, want the freshly planned %q", fresh.Payload, current.Payload)
	}

	// Suppression works normally within v0.2.
	repeat, created, err := state.CreateCanonical(ctx, current)
	if err != nil || created || repeat.ID != fresh.ID {
		t.Fatalf("v0.2 repost: action=%#v created=%v err=%v", repeat, created, err)
	}
}

// TestPendingOutboxClaimIsExclusive pins the fix for t-002: the claim used to
// be a value derived from now (now.Add(time.Minute).Unix()), so two calls
// made within the same wall-clock second computed an identical value. The
// second call's UPDATE correctly matched no rows, but its SELECT matched by
// value equality anyway and returned the row the first call had just
// claimed. Calling PendingOutbox twice with the exact same now reproduces
// that without goroutines: only the first call may see the entry.
func TestPendingOutboxClaimIsExclusive(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(ctx, "file::memory:?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = state.Close() }()
	action := newOutboxTestAction()
	created, ok, err := state.CreateCanonical(ctx, action)
	if err != nil || !ok {
		t.Fatalf("create canonical: created=%#v ok=%v err=%v", created, ok, err)
	}
	if err := state.AttachSourceMessage(ctx, created.ID, 44); err != nil {
		t.Fatal(err)
	}
	frozen := time.Now().Add(time.Second)
	first, err := state.PendingOutbox(ctx, frozen)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim: entries=%#v err=%v", first, err)
	}
	second, err := state.PendingOutbox(ctx, frozen)
	if err != nil || len(second) != 0 {
		t.Fatalf("second claim with identical now must see nothing already claimed: entries=%#v err=%v", second, err)
	}
}

// TestMarkOutboxErrorReleasesLeaseForRequestedBackoff pins Do-item 2 of
// t-002: a retryable failure must not leak the one-minute claim lease past a
// shorter requested backoff, or the retry sits idle until the lease expires
// on its own instead of at the caller's requested delay.
func TestMarkOutboxErrorReleasesLeaseForRequestedBackoff(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(ctx, "file::memory:?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = state.Close() }()
	action := newOutboxTestAction()
	created, ok, err := state.CreateCanonical(ctx, action)
	if err != nil || !ok {
		t.Fatalf("create canonical: created=%#v ok=%v err=%v", created, ok, err)
	}
	if err := state.AttachSourceMessage(ctx, created.ID, 44); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := state.PendingOutbox(ctx, now); err != nil {
		t.Fatal(err)
	}
	const requested = 5 * time.Second
	if err := state.MarkOutboxErrorAfter(ctx, created.ID, "send_pending", errors.New("temporary"), requested); err != nil {
		t.Fatal(err)
	}
	// Before the requested delay elapses there is nothing to retry yet.
	if entries, err := state.PendingOutbox(ctx, now.Add(requested-time.Second)); err != nil || len(entries) != 0 {
		t.Fatalf("premature retry: entries=%#v err=%v", entries, err)
	}
	// At the requested delay the entry must be retryable; a leaked one-minute
	// lease would keep it hidden well past this point.
	entries, err := state.PendingOutbox(ctx, now.Add(requested+time.Second))
	if err != nil || len(entries) != 1 {
		t.Fatalf("retry at requested backoff: entries=%#v err=%v", entries, err)
	}
}

func TestProcessedUpdateCanBeCompletedAndReclaimed(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(ctx, "file::memory:?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	claimed, err := state.ClaimUpdate(ctx, 1, 2, 0)
	if err != nil || !claimed {
		t.Fatalf("first claim = %v, err=%v", claimed, err)
	}
	claimed, err = state.ClaimUpdate(ctx, 1, 2, 0)
	if err != nil || claimed {
		t.Fatalf("duplicate claim = %v, err=%v", claimed, err)
	}
	if err := state.CompleteUpdate(ctx, 1, 2, 0); err != nil {
		t.Fatal(err)
	}
	claimed, err = state.ClaimUpdate(ctx, 1, 2, 0)
	if err != nil || claimed {
		t.Fatalf("completed claim = %v, err=%v", claimed, err)
	}
}
