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
