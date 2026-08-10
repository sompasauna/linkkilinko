package telegram_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/mymmrac/telego/telegoapi"
	"github.com/sompasauna/linkkilinko/internal/telegram"
)

func TestRetryAfterUnwrapsTelegramRateLimit(t *testing.T) {
	err := fmt.Errorf("send failed: %w", &telegoapi.Error{
		ErrorCode:  429,
		Parameters: &telegoapi.ResponseParameters{RetryAfter: 7},
	})
	delay, ok := telegram.RetryAfter(err)
	if !ok || delay != 7*time.Second {
		t.Fatalf("RetryAfter() = %v, %v; want 7s, true", delay, ok)
	}
}

func TestPermanentTelegramErrorsExcludeRateLimits(t *testing.T) {
	permanent := &telegoapi.Error{ErrorCode: 400, Description: "bad request"}
	if !telegram.IsPermanentError(permanent) {
		t.Fatal("expected 400 error to be permanent")
	}
	rateLimit := &telegoapi.Error{ErrorCode: 429}
	if telegram.IsPermanentError(rateLimit) {
		t.Fatal("expected 429 error to remain retryable")
	}
}
