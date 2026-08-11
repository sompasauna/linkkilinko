package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mymmrac/telego"
	"github.com/sompasauna/linkkilinko/internal/store"
)

func TestResponseEntitiesUsesUTF16OffsetsAndUserID(t *testing.T) {
	text := "Käyttäjä 🙂 lähetti linkin"
	entities := responseEntities(text, "🙂", 42)
	if len(entities) != 1 {
		t.Fatalf("entities = %#v, want one text mention", entities)
	}
	entity := entities[0]
	if entity.Type != telego.EntityTypeTextMention || entity.Offset != 9 || entity.Length != 2 {
		t.Fatalf("entity = %#v, want UTF-16 mention span", entity)
	}
	if entity.User == nil || entity.User.ID != 42 {
		t.Fatalf("entity user = %#v, want user 42", entity.User)
	}
}

func TestResponsePayloadRoundTrip(t *testing.T) {
	original := responsePayload{
		Text:      "notice",
		Operation: operationText,
		Entities:  responseEntities("notice", "notice", 9),
	}
	encoded, err := encodeResponsePayload(original)
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeResponsePayload(encoded)
	if decoded.Text != original.Text || decoded.Operation != original.Operation || len(decoded.Entities) != 1 || decoded.Entities[0].User.ID != 9 {
		t.Fatalf("decoded = %#v, want %#v", decoded, original)
	}
}

func TestOwnerMatchesOnlyPersistedOwner(t *testing.T) {
	tests := []struct {
		name  string
		found bool
		owner int64
		actor int64
		want  bool
	}{
		{name: "matching owner", found: true, owner: 42, actor: 42, want: true},
		{name: "different user", found: true, owner: 42, actor: 99},
		{name: "no owner", found: false, owner: 42, actor: 42},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ownerMatches(test.found, test.owner, test.actor); got != test.want {
				t.Fatalf("ownerMatches() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestGroupChatRecognition(t *testing.T) {
	if !isGroupChat(telego.ChatTypeGroup) || !isGroupChat(telego.ChatTypeSupergroup) {
		t.Fatal("group and supergroup chats must be recognized")
	}
	if isGroupChat(telego.ChatTypePrivate) {
		t.Fatal("private chats must not be recognized as groups")
	}
}

// openOperatorStore returns an in-memory store populated for operator-mode
// tests. Returning a cleanup keeps the helper aligned with the rest of the
// suite and prevents leaked handles during parallel runs.
func openOperatorStore(t *testing.T) *store.Store {
	t.Helper()
	state, err := store.Open(context.Background(), "file::memory:?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	return state
}

// TestOperatorResetOwnerAndRebootstrap is the t-017 done-when requirement
// expressed at the operator layer: -reset-owner clears the persisted owner
// and the next RegisterOwner call succeeds with a different user id, while
// approved chats recorded under the previous owner survive.
func TestOperatorResetOwnerAndRebootstrap(t *testing.T) {
	ctx := context.Background()
	state := openOperatorStore(t)
	if _, err := state.RegisterOwner(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if err := state.ApproveChat(ctx, -1001, 42); err != nil {
		t.Fatal(err)
	}
	if err := applyResetOwner(ctx, state); err != nil {
		t.Fatalf("applyResetOwner: %v", err)
	}
	if _, found, err := state.Owner(ctx); err != nil || found {
		t.Fatalf("owner after reset: found=%v err=%v, want no owner", found, err)
	}
	if approved, err := state.ApprovedChat(ctx, -1001); err != nil || !approved {
		t.Fatalf("approved chat must survive owner reset: approved=%v err=%v", approved, err)
	}
	registered, err := state.RegisterOwner(ctx, 99)
	if err != nil || !registered {
		t.Fatalf("post-reset RegisterOwner = (%v, %v), want (true, nil)", registered, err)
	}
}

// TestOperatorResetOwnerWithoutOwnerFails ensures the operator action
// exits non-zero with a clear message when there is no owner to reset.
func TestOperatorResetOwnerWithoutOwnerFails(t *testing.T) {
	state := openOperatorStore(t)
	err := applyResetOwner(context.Background(), state)
	if err == nil {
		t.Fatal("expected an error when no owner is registered")
	}
	if !strings.Contains(err.Error(), "no bot owner registered") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestOperatorApproveChatRequiresOwner covers the migration case: an
// operator must register an owner before -approve-chat can succeed.
func TestOperatorApproveChatRequiresOwner(t *testing.T) {
	state := openOperatorStore(t)
	err := applyApproveChat(context.Background(), state, -1001)
	if err == nil {
		t.Fatal("expected an error when approving a chat without an owner")
	}
}

// TestOperatorApproveChatUsesOwner covers the success path: -approve-chat
// records the chat as approved with the existing owner as the approver.
func TestOperatorApproveChatUsesOwner(t *testing.T) {
	ctx := context.Background()
	state := openOperatorStore(t)
	if _, err := state.RegisterOwner(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if err := applyApproveChat(ctx, state, -1001); err != nil {
		t.Fatalf("applyApproveChat: %v", err)
	}
	approved, err := state.ApprovedChat(ctx, -1001)
	if err != nil || !approved {
		t.Fatalf("ApprovedChat(-1001) = (%v, %v), want (true, nil)", approved, err)
	}
}

// TestOperatorRejectsMutuallyExclusiveFlags ensures runOperator does not
// silently no-op when both flags are passed (or neither flag is set).
func TestOperatorRejectsMutuallyExclusiveFlags(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	if err := runOperator(ctx, filepath.Join(directory, "missing.yaml"), true, true, 0); err == nil {
		t.Fatal("expected an error when both flags are set")
	}
}
