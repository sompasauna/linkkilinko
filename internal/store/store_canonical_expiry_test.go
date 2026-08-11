package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/sompasauna/linkkilinko/internal/store"
	_ "modernc.org/sqlite"
)

func TestFindCanonicalExpiresAfterSuppressionWindow(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "state.sqlite")
	database, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	action := store.CanonicalAction{
		ChatID: 1, UserID: 2, Rule: "rule", BehaviorVersion: "v1",
		Fingerprint: "fingerprint", Payload: `{"text":"notice"}`,
	}
	created, ok, err := database.CreateCanonical(ctx, action)
	if err != nil || !ok {
		t.Fatalf("CreateCanonical() = (%#v, %v, %v), want created action", created, ok, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	updateTimestamp(t, databasePath,
		`UPDATE canonical_actions SET created_at = ? WHERE id = ?`,
		time.Now().Add(-5*time.Hour).Unix(), created.ID)
	database, err = store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	_, found, err := database.FindCanonical(ctx, 1, 0, 2, "rule", "v1", "fingerprint")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("FindCanonical() found an expired action, want no match")
	}

	refreshed, createdAgain, err := database.CreateCanonical(ctx, action)
	if err != nil || !createdAgain || refreshed.ID == created.ID {
		t.Fatalf("CreateCanonical() after expiry = (%#v, %v, %v), want a new action", refreshed, createdAgain, err)
	}
}

func TestKnownGoodPreviewDomainExcludesTrackingHosts(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "state.sqlite")
	database, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if err := database.RecordKnownGoodPreviewDomain(ctx, "GitHub.com."); err != nil {
		t.Fatal(err)
	}
	known, err := database.KnownGoodPreviewDomain(ctx, "github.com")
	if err != nil || !known {
		t.Fatalf("KnownGoodPreviewDomain(github.com) = (%v, %v), want true", known, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	updateTimestamp(t, databasePath, `
		UPDATE known_good_preview_domains SET observed_at = ? WHERE host = ?`,
		time.Now().Add(-31*24*time.Hour).Unix(), "github.com")
	database, err = store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	known, err = database.KnownGoodPreviewDomain(ctx, "github.com")
	if err != nil || known {
		t.Fatalf("KnownGoodPreviewDomain(stale github.com) = (%v, %v), want false", known, err)
	}
	for _, host := range []string{"facebook.com", "sub.facebook.com", "share.google", "x.share.google"} {
		if err := database.RecordKnownGoodPreviewDomain(ctx, host); err != nil {
			t.Fatal(err)
		}
		known, err := database.KnownGoodPreviewDomain(ctx, host)
		if err != nil {
			t.Fatal(err)
		}
		if known {
			t.Fatalf("KnownGoodPreviewDomain(%q) = true, want false", host)
		}
	}
}

func updateTimestamp(t *testing.T, databasePath, query string, args ...any) {
	t.Helper()
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}
