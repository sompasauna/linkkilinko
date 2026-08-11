package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestFindCanonicalExpiresAfterSuppressionWindow(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	action := CanonicalAction{
		ChatID: 1, UserID: 2, Rule: "rule", BehaviorVersion: "v1",
		Fingerprint: "fingerprint", Payload: `{"text":"notice"}`,
	}
	created, ok, err := database.CreateCanonical(ctx, action)
	if err != nil || !ok {
		t.Fatalf("CreateCanonical() = (%#v, %v, %v), want created action", created, ok, err)
	}
	if _, err := database.db.ExecContext(ctx,
		`UPDATE canonical_actions SET created_at = ? WHERE id = ?`,
		time.Now().Add(-canonicalActionTTL-time.Second).Unix(), created.ID); err != nil {
		t.Fatal(err)
	}

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
	database, err := Open(ctx, filepath.Join(t.TempDir(), "state.sqlite"))
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
	if _, err := database.db.ExecContext(ctx, `
		UPDATE known_good_preview_domains SET observed_at = ? WHERE host = ?`,
		time.Now().Add(-knownGoodPreviewTTL-time.Second).Unix(), "github.com"); err != nil {
		t.Fatal(err)
	}
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
