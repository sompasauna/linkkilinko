// Package telegram contains the small Telegram transport boundary used by the
// linkkilinko runtime.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
)

// Client wraps the Telegram Bot API methods needed by moderation.
type Client struct {
	bot *telego.Bot
}

// Ping verifies that the bot token can reach Telegram's Bot API.
func (c *Client) Ping(ctx context.Context) error {
	if c == nil || c.bot == nil {
		return errors.New("telegram: client is nil")
	}
	if _, err := c.bot.GetMe(ctx); err != nil {
		return fmt.Errorf("telegram: ping bot API: %w", err)
	}
	return nil
}

// HasDeletePermission verifies that the bot is an administrator able to delete
// other members' messages in chatID.
func (c *Client) HasDeletePermission(ctx context.Context, chatID int64) (bool, error) {
	if c == nil || c.bot == nil {
		return false, errors.New("telegram: client is nil")
	}
	member, err := c.bot.GetChatMember(ctx, &telego.GetChatMemberParams{
		ChatID: telego.ChatID{ID: chatID},
		UserID: c.bot.ID(),
	})
	if err != nil {
		return false, fmt.Errorf("telegram: check bot permissions: %w", err)
	}
	administrator, ok := member.(*telego.ChatMemberAdministrator)
	return ok && administrator.CanDeleteMessages, nil
}

// New creates a Telegram client after validating the bot token with telego.
func New(token string) (*Client, error) {
	bot, err := telego.NewBot(token)
	if err != nil {
		return nil, fmt.Errorf("telegram: create bot: %w", err)
	}
	return &Client{bot: bot}, nil
}

// Run receives the explicitly subscribed update kinds until ctx is cancelled.
// Errors from long polling are logged and retried with bounded exponential backoff;
// only context cancellation and a closed database may stop Run.
func (c *Client) Run(ctx context.Context, handler func(context.Context, telego.Update) error) error {
	if c == nil || c.bot == nil {
		return errors.New("telegram: client is nil")
	}
	if handler == nil {
		return errors.New("telegram: update handler is nil")
	}
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		updates, err := c.bot.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{
			Timeout: 30,
			AllowedUpdates: []string{
				telego.MessageUpdates,
				telego.EditedMessageUpdates,
				telego.ChatMemberUpdates,
				telego.MyChatMemberUpdates,
			},
		})
		if err != nil {
			slog.Error("telegram long polling failed; retrying", "subsystem", "telegram", "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		backoff = time.Second
		c.processUpdates(ctx, updates, handler)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("telegram update stream closed; reconnecting", "subsystem", "telegram")
	}
}

// processUpdates drains the supplied update channel, dispatching each update
// to handler with a recover on the per-update boundary. Handler errors and
// panics are logged with update identity and the loop continues; only ctx
// cancellation stops it.
func (c *Client) processUpdates(ctx context.Context, updates <-chan telego.Update, handler func(context.Context, telego.Update) error) {
	for update := range updates {
		chatID, messageID := extractUpdateIdentity(update)
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					slog.Error("telegram update handler panicked", "subsystem", "telegram", "update_id", update.UpdateID, "chat_id", chatID, "message_id", messageID, "panic", recovered)
				}
			}()
			if err := handler(ctx, update); err != nil {
				slog.Error("telegram update handler failed; continuing", "subsystem", "telegram", "update_id", update.UpdateID, "chat_id", chatID, "message_id", messageID, "error", err)
			}
		}()
	}
}

// Delete removes one message from a chat.
func (c *Client) Delete(ctx context.Context, chatID int64, messageID int) error {
	if c == nil || c.bot == nil {
		return errors.New("telegram: client is nil")
	}
	if err := c.bot.DeleteMessage(ctx, &telego.DeleteMessageParams{
		ChatID:    telego.ChatID{ID: chatID},
		MessageID: messageID,
	}); err != nil {
		return fmt.Errorf("telegram: delete message %d: %w", messageID, err)
	}
	return nil
}

// Send sends plain text into a chat topic and returns the new message id.
func (c *Client) Send(ctx context.Context, chatID int64, threadID int, text string, entities ...telego.MessageEntity) (int, error) {
	if c == nil || c.bot == nil {
		return 0, errors.New("telegram: client is nil")
	}
	message, err := c.bot.SendMessage(ctx, &telego.SendMessageParams{
		ChatID:          telego.ChatID{ID: chatID},
		MessageThreadID: threadID,
		Text:            text,
		Entities:        entities,
		// Bot-authored notices and replacements must not create a second
		// preview for a URL they mention. The original user message is the
		// message whose native preview policy we evaluate.
		LinkPreviewOptions: &telego.LinkPreviewOptions{IsDisabled: true},
	})
	if err != nil {
		return 0, fmt.Errorf("telegram: send message: %w", err)
	}
	return message.MessageID, nil
}

// Copy copies a message into the same chat topic and optionally replaces its caption.
func (c *Client) Copy(ctx context.Context, chatID int64, threadID int, sourceMessageID int, caption string, entities ...telego.MessageEntity) (int, error) {
	if c == nil || c.bot == nil {
		return 0, errors.New("telegram: client is nil")
	}
	messageID, err := c.bot.CopyMessage(ctx, &telego.CopyMessageParams{
		ChatID:          telego.ChatID{ID: chatID},
		MessageThreadID: threadID,
		FromChatID:      telego.ChatID{ID: chatID},
		MessageID:       sourceMessageID,
		Caption:         caption,
		CaptionEntities: entities,
	})
	if err != nil {
		return 0, fmt.Errorf("telegram: copy message %d: %w", sourceMessageID, err)
	}
	if messageID == nil {
		return 0, errors.New("telegram: copy message returned no message id")
	}
	return messageID.MessageID, nil
}

// IsMessageNotFound reports whether Telegram already considers a message absent.
func IsMessageNotFound(err error) bool {
	var apiErr *telegoapi.Error
	return errors.As(err, &apiErr) && apiErr.ErrorCode == 400 && strings.Contains(strings.ToLower(apiErr.Description), "message to delete not found")
}

// IsDeletePermissionError identifies Telegram responses that require operator
// intervention rather than an automatic retry loop.
func IsDeletePermissionError(err error) bool {
	var apiErr *telegoapi.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	description := strings.ToLower(apiErr.Description)
	return apiErr.ErrorCode == 400 && (strings.Contains(description, "can't be deleted") || strings.Contains(description, "cannot be deleted") || strings.Contains(description, "not enough rights") || strings.Contains(description, "administrator rights"))
}

// IsPermanentError reports Telegram client errors that should not be retried forever.
func IsPermanentError(err error) bool {
	var apiErr *telegoapi.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.ErrorCode >= 400 && apiErr.ErrorCode < 500 && apiErr.ErrorCode != 429
}

// RetryAfter returns Telegram's requested retry delay when present.
func RetryAfter(err error) (time.Duration, bool) {
	var apiErr *telegoapi.Error
	if !errors.As(err, &apiErr) || apiErr.Parameters == nil || apiErr.Parameters.RetryAfter <= 0 {
		return 0, false
	}
	return time.Duration(apiErr.Parameters.RetryAfter) * time.Second, true
}

func extractUpdateIdentity(update telego.Update) (chatID int64, messageID int) {
	if update.Message != nil {
		return update.Message.Chat.ID, update.Message.MessageID
	}
	if update.EditedMessage != nil {
		return update.EditedMessage.Chat.ID, update.EditedMessage.MessageID
	}
	if update.ChatMember != nil {
		return update.ChatMember.Chat.ID, 0
	}
	if update.MyChatMember != nil {
		return update.MyChatMember.Chat.ID, 0
	}
	return 0, 0
}
