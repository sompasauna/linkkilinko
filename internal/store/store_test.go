package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sompasauna/linkkilinko/internal/store"
)

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
