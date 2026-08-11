package action

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/sompasauna/linkkilinko/internal/config"
	"github.com/sompasauna/linkkilinko/internal/store"
)

// barrierTelegram counts Send calls and lets the test synchronize two
// concurrent retryOutbox callers so both reach the send step before either
// completes the outbox item. A real race between two callers racing
// PendingOutbox only manifests if both get past the claim before either one
// finishes; a sequential call pair would not reproduce it, because the first
// call's MarkOutboxComplete excludes the entry from the second call's SELECT.
type barrierTelegram struct {
	sends   atomic.Int64
	ready   chan struct{}
	once    sync.Once
	proceed chan struct{}
}

func (b *barrierTelegram) Delete(context.Context, int64, int) error { return nil }

func (b *barrierTelegram) Send(context.Context, int64, int, string, ...telego.MessageEntity) (int, error) {
	b.sends.Add(1)
	b.once.Do(func() { close(b.ready) })
	select {
	case <-b.proceed:
	case <-time.After(2 * time.Second):
	}
	return 1, nil
}

func (b *barrierTelegram) Reply(ctx context.Context, chatID int64, threadID, _ int, text string, entities ...telego.MessageEntity) (int, error) {
	return b.Send(ctx, chatID, threadID, text, entities...)
}

func (b *barrierTelegram) Copy(context.Context, int64, int, int, string, ...telego.MessageEntity) (int, error) {
	return 1, nil
}

// TestConcurrentRetryOutboxSendsExactlyOnce is the Done-when regression for
// t-002: two goroutines driving retryOutbox against one pending outbox entry
// must produce exactly one Send. It exercises the real *store.Store, not a
// fake StorePort, because the defect is in SQL claim semantics that a mock
// cannot reproduce.
func TestConcurrentRetryOutboxSendsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(ctx, "file::memory:?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = state.Close() }()

	action := store.CanonicalAction{ChatID: 1, UserID: 2, Rule: "rule", BehaviorVersion: "v1", Fingerprint: "fp", Payload: `{"text":"hi"}`}
	created, ok, err := state.CreateCanonical(ctx, action)
	if err != nil || !ok {
		t.Fatalf("create canonical: created=%#v ok=%v err=%v", created, ok, err)
	}
	// send_pending with no source message skips the delete step so retryOutbox
	// goes straight to the send that this test is racing.
	if err := state.MarkSendPending(ctx, created.ID); err != nil {
		t.Fatal(err)
	}

	client := &barrierTelegram{ready: make(chan struct{}), proceed: make(chan struct{})}
	app := New(config.Config{}, client, state, nil, nil, nil, nil, nil)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			errs <- app.retryOutbox(ctx)
		}()
	}
	// Let one Send arrive at the barrier, then release both: if a second
	// caller had also claimed the row, this unblocks its Send too instead of
	// deadlocking on the goroutine that never got in.
	select {
	case <-client.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("no Send reached the barrier")
	}
	close(client.proceed)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	if got := client.sends.Load(); got != 1 {
		t.Fatalf("Send called %d times, want exactly 1", got)
	}
}
