package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sompasauna/linkkilinko/internal/config"
)

func TestLoadAppliesDefaultsAndEnvironmentToken(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(path, []byte("telegram:\n  token: token\ndatabase:\n  path: state.sqlite\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LINKKILINKO_TELEGRAM_TOKEN", "token")
	runtimeConfig, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeConfig.Telegram.Token != "token" || time.Duration(runtimeConfig.Moderation.NewcomerSandbox) != 48*time.Hour {
		t.Fatalf("config=%#v", runtimeConfig)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("telegram:\n  token: token\nunknown: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err == nil {
		t.Fatal("expected unknown configuration field to fail")
	}
}

// TestLoadDatabasePathIgnoresMissingToken confirms that the operator-mode
// loader returns the database path without requiring a Telegram token.
func TestLoadDatabasePathIgnoresMissingToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("database:\n  path: /var/lib/linkkilinko/state.sqlite\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LINKKILINKO_TELEGRAM_TOKEN", "")
	got, err := config.LoadDatabasePath(path)
	if err != nil {
		t.Fatalf("LoadDatabasePath: %v", err)
	}
	if got != "/var/lib/linkkilinko/state.sqlite" {
		t.Fatalf("database path = %q, want /var/lib/linkkilinko/state.sqlite", got)
	}
}

// TestLoadDatabasePathRejectsEmptyPath ensures the operator-mode loader
// still rejects the misconfiguration that Load rejects.
func TestLoadDatabasePathRejectsEmptyPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("telegram:\n  token: token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadDatabasePath(path); err == nil {
		t.Fatal("expected empty database path to fail")
	}
}

// TestLoadDatabasePathRejectsUnknownFields confirms the operator loader
// shares the strict unknown-field policy with Load.
func TestLoadDatabasePathRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("database:\n  path: state.sqlite\nunknown: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadDatabasePath(path); err == nil {
		t.Fatal("expected unknown configuration field to fail")
	}
}
