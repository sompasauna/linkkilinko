package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/mymmrac/telego"
	"github.com/sompasauna/linkkilinko/internal/config"
	"github.com/sompasauna/linkkilinko/internal/store"
)

// fakeTelegramClient lets tests control HasDeletePermission and Ping without
// reaching the live Bot API. handleBotMembership only consults the
// delete-permission answer, so the rest of the interface is stubbed.
type fakeTelegramClient struct {
	canDelete bool
	err       error
	pings     int
}

func (f *fakeTelegramClient) HasDeletePermission(_ context.Context, _ int64) (bool, error) {
	return f.canDelete, f.err
}

func (f *fakeTelegramClient) Ping(_ context.Context) error {
	f.pings++
	return nil
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "test.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newTestApplication(t *testing.T, st *store.Store, client telegramClient) *application {
	t.Helper()
	return &application{
		config: config.Config{},
		client: client,
		state:  st,
	}
}

// adminUpdateFor constructs a my_chat_member update where the bot has just
// been promoted to administrator of chatID, performed by actorID.
func adminUpdateFor(chatID int64, actorID int64) telego.ChatMemberUpdated {
	botUser := telego.User{ID: 999, IsBot: true, FirstName: "bot"}
	fromUser := telego.User{ID: actorID, FirstName: "operator"}
	return telego.ChatMemberUpdated{
		Chat: telego.Chat{ID: chatID, Type: telego.ChatTypeSupergroup},
		From: fromUser,
		NewChatMember: &telego.ChatMemberAdministrator{
			Status:      telego.MemberStatusAdministrator,
			User:        botUser,
			CanBeEdited: true,
		},
		OldChatMember: &telego.ChatMemberMember{
			Status: telego.MemberStatusMember,
			User:   botUser,
		},
	}
}

func TestHandleBotMembershipApprovesForOwnerWithDeletePermission(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	if _, err := st.RegisterOwner(context.Background(), 42); err != nil {
		t.Fatalf("RegisterOwner: %v", err)
	}
	client := &fakeTelegramClient{canDelete: true}
	app := newTestApplication(t, st, client)
	update := adminUpdateFor(-1001234567890, 42)
	if err := app.handleBotMembership(context.Background(), update); err != nil {
		t.Fatalf("handleBotMembership: %v", err)
	}
	active, err := st.ApprovedChat(context.Background(), update.Chat.ID)
	if err != nil {
		t.Fatalf("ApprovedChat: %v", err)
	}
	if !active {
		t.Fatalf("chat %d must be approved when the owner promotes the bot to administrator", update.Chat.ID)
	}
}

func TestHandleBotMembershipIgnoresNonOwnerPromotion(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	if _, err := st.RegisterOwner(context.Background(), 42); err != nil {
		t.Fatalf("RegisterOwner: %v", err)
	}
	client := &fakeTelegramClient{canDelete: true}
	app := newTestApplication(t, st, client)
	update := adminUpdateFor(-1009876543210, 99)
	if err := app.handleBotMembership(context.Background(), update); err != nil {
		t.Fatalf("handleBotMembership: %v", err)
	}
	active, err := st.ApprovedChat(context.Background(), update.Chat.ID)
	if err != nil {
		t.Fatalf("ApprovedChat: %v", err)
	}
	if active {
		t.Fatalf("chat %d must not be approved when a non-owner promotes the bot", update.Chat.ID)
	}
}

func TestHandleBotMembershipRejectsWithoutDeletePermission(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	if _, err := st.RegisterOwner(context.Background(), 42); err != nil {
		t.Fatalf("RegisterOwner: %v", err)
	}
	client := &fakeTelegramClient{canDelete: false}
	app := newTestApplication(t, st, client)
	update := adminUpdateFor(-1001234567890, 42)
	if err := app.handleBotMembership(context.Background(), update); err != nil {
		t.Fatalf("handleBotMembership: %v", err)
	}
	active, err := st.ApprovedChat(context.Background(), update.Chat.ID)
	if err != nil {
		t.Fatalf("ApprovedChat: %v", err)
	}
	if active {
		t.Fatalf("chat %d must not be approved when the bot lacks delete permission", update.Chat.ID)
	}
}

func TestHandleBotMembershipSurfacesPermissionCheckError(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	if _, err := st.RegisterOwner(context.Background(), 42); err != nil {
		t.Fatalf("RegisterOwner: %v", err)
	}
	client := &fakeTelegramClient{err: errors.New("telegram rate limit")}
	app := newTestApplication(t, st, client)
	update := adminUpdateFor(-1001234567890, 42)
	if err := app.handleBotMembership(context.Background(), update); err == nil {
		t.Fatal("expected handleBotMembership to surface HasDeletePermission error")
	}
}
