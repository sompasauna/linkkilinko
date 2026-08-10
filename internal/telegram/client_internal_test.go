package telegram

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mymmrac/telego"
)

// feedUpdates closes a channel after delivering the supplied updates so the
// processUpdates loop drains cleanly without waiting for long polling.
func feedUpdates(updates []telego.Update) chan telego.Update {
	ch := make(chan telego.Update, len(updates))
	for _, update := range updates {
		ch <- update
	}
	close(ch)
	return ch
}

// makeMessageUpdate produces a minimal telego.Update carrying one message so
// extractUpdateIdentity returns a non-zero chat/message id.
func makeMessageUpdate(id int) telego.Update {
	const chatID = 100
	return telego.Update{
		UpdateID: id,
		Message: &telego.Message{
			MessageID: id,
			Chat:      telego.Chat{ID: chatID},
		},
	}
}

func TestProcessUpdatesContinuesAfterHandlerError(t *testing.T) {
	t.Parallel()
	client := &Client{}
	var (
		processed atomic.Int32
		wg        sync.WaitGroup
	)
	handler := func(_ context.Context, update telego.Update) error {
		defer wg.Done()
		processed.Add(1)
		if update.UpdateID == 1 {
			return errors.New("simulated handler failure")
		}
		return nil
	}
	wg.Add(2)
	updates := feedUpdates([]telego.Update{
		makeMessageUpdate(1),
		makeMessageUpdate(2),
	})
	client.processUpdates(context.Background(), updates, handler)
	wg.Wait()
	if got := processed.Load(); got != 2 {
		t.Errorf("processed = %d, want 2; second update must still be dispatched after the first errored", got)
	}
}

func TestProcessUpdatesContinuesAfterHandlerPanic(t *testing.T) {
	t.Parallel()
	client := &Client{}
	var (
		processed atomic.Int32
		wg        sync.WaitGroup
	)
	handler := func(_ context.Context, update telego.Update) error {
		defer wg.Done()
		if update.UpdateID == 1 {
			panic("simulated handler panic")
		}
		processed.Add(1)
		return nil
	}
	wg.Add(2)
	updates := feedUpdates([]telego.Update{
		makeMessageUpdate(1),
		makeMessageUpdate(2),
	})
	client.processUpdates(context.Background(), updates, handler)
	wg.Wait()
	if got := processed.Load(); got != 1 {
		t.Errorf("processed = %d, want 1; second update must still be dispatched after the first panicked", got)
	}
}
