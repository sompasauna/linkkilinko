package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/sompasauna/linkkilinko/internal/store"
)

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
	action := store.CanonicalAction{ChatID: 1, UserID: 2, Rule: "rule", BehaviorVersion: "v1", Fingerprint: "fp", Payload: "notice"}
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

// Splitting google-wrapper into google-share and google-amp renames a value
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
