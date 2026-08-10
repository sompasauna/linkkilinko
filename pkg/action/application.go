// Package action composes moderation policy with durable side effects.
package action

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mymmrac/telego"
	"github.com/sompasauna/linkkilinko/internal/config"
	"github.com/sompasauna/linkkilinko/internal/store"
	"github.com/sompasauna/linkkilinko/internal/telegram"
	"github.com/sompasauna/linkkilinko/pkg/core/link"
	"github.com/sompasauna/linkkilinko/pkg/core/moderation"
	"github.com/sompasauna/linkkilinko/pkg/core/preview"
)

const behaviorVersion = "v0.3"

const (
	operationText      = "text"
	operationCopyMedia = "copy_media"
	messageTextLimit   = 3900
	captionTextLimit   = 1000
)

// Application composes the pure core packages with the Telegram, storage,
// metadata, and notice ports to run the moderation workflow.
type Application struct {
	config         config.Config
	allowedChats   map[int64]struct{}
	client         TelegramPort
	state          StorePort
	metadata       MetadataPort
	links          LinkPort
	previews       PreviewPort
	notices        NoticePort
	now            func() time.Time
	updatesSeen    atomic.Uint64
	lastUpdateUnix atomic.Int64
	outboxNudge    chan struct{}
}

// TelegramPort contains the Telegram side effects required by moderation.
type TelegramPort interface {
	Delete(ctx context.Context, chatID int64, messageID int) error
	Send(ctx context.Context, chatID int64, threadID int, text string, entities ...telego.MessageEntity) (int, error)
	Copy(ctx context.Context, chatID int64, threadID, sourceMessageID int, caption string, entities ...telego.MessageEntity) (int, error)
}

// MembershipPort contains the durable membership state operations.
type MembershipPort interface {
	Membership(ctx context.Context, chatID, userID int64) (store.Membership, bool, error)
	Grandfather(ctx context.Context, chatID, userID int64) error
}

// CanonicalPort contains the durable canonical-action operations that drive
// duplicate suppression.
type CanonicalPort interface {
	FindCanonical(ctx context.Context, chatID int64, threadID int, senderID int64, rule, behaviorVersion, fingerprint string) (store.CanonicalAction, bool, error)
	CreateCanonical(ctx context.Context, action store.CanonicalAction) (store.CanonicalAction, bool, error)
	RecordSuppressed(ctx context.Context, actionID int64, messageID int) error
}

// OutboxPort contains the durable outbox operations that deliver moderation
// responses exactly once.
type OutboxPort interface {
	MarkDeleteRequested(ctx context.Context, actionID int64) error
	MarkSendPending(ctx context.Context, actionID int64) error
	MarkOutboxCopied(ctx context.Context, actionID int64, messageID int) error
	MarkOutboxComplete(ctx context.Context, actionID int64, responseMessageID int) error
	PendingOutbox(ctx context.Context, now time.Time) ([]store.OutboxEntry, error)
	ReleaseOutboxLease(ctx context.Context, actionID int64) error
	ReplaceOutboxPayload(ctx context.Context, actionID int64, payload string) error
	MarkOutboxError(ctx context.Context, actionID int64, state string, operationErr error) error
	MarkOutboxErrorAfter(ctx context.Context, actionID int64, state string, operationErr error, delay time.Duration) error
	MarkOutboxDead(ctx context.Context, actionID int64, operationErr error) error
}

// StorePort contains the durable moderation state operations.
type StorePort interface {
	MembershipPort
	CanonicalPort
	OutboxPort
}

// MetadataPort provides bounded URL retrieval.
type MetadataPort interface {
	Fetch(ctx context.Context, rawURL string) (preview.Document, error)
	FetchWithUserAgent(ctx context.Context, rawURL, userAgent string) (preview.Document, error)
}

// LinkPort resolves tracked links.
type LinkPort interface {
	MatchName(rawURL string) (string, bool)
	Resolve(ctx context.Context, rawURL string) (link.Resolution, bool, error)
}

// PreviewPort extracts metadata and identifies inconclusive providers.
type PreviewPort interface {
	Inspect(document preview.Document) (preview.Metadata, string)
	IsInconclusive(document preview.Document) bool
}

// NoticePort renders a validated user-visible message.
type NoticePort interface {
	Render(key string, params map[string]string) string
}

// Process runs the moderation workflow for one already-claimed message in the
// SPEC-mandated stage order: newcomer sandbox, tracked-link rewriting, then
// preview policy.
func (a *Application) Process(ctx context.Context, input moderation.Input) error {
	membership, found, err := a.state.Membership(ctx, input.ChatID, input.SenderID)
	if err != nil {
		return err
	}
	if !found {
		if err := a.state.Grandfather(ctx, input.ChatID, input.SenderID); err != nil {
			return err
		}
		membership, _, err = a.state.Membership(ctx, input.ChatID, input.SenderID)
		if err != nil {
			return err
		}
	}
	if plan, ok := moderation.NewcomerPlan(input, membership.JoinedAt, a.clock(), time.Duration(a.config.Moderation.NewcomerSandbox)); ok {
		return a.applyPlan(ctx, input, plan)
	}
	return a.moderateLinks(ctx, input)
}

func (a *Application) moderateLinks(ctx context.Context, input moderation.Input) error {
	urls := moderation.ExtractURLs(input)
	if len(urls) == 0 {
		return nil
	}
	destinations := make(map[int]string)
	matchedWrapper := false
	for index, candidate := range urls {
		resolverName, matched := a.links.MatchName(candidate.Target)
		if !matched {
			continue
		}
		matchedWrapper = true
		fingerprint := moderation.Fingerprint(input, resolverName)
		if canonical, found, err := a.state.FindCanonical(ctx, input.ChatID, input.ThreadID, input.SenderID, resolverName, behaviorVersion, fingerprint); err != nil {
			return err
		} else if found {
			return a.suppressDuplicate(ctx, input, canonical)
		}
		resolution, matched, err := a.links.Resolve(ctx, candidate.Target)
		if err != nil {
			slog.Warn("link resolution failed", "host", safeHost(candidate.Target), "error", err)
			return nil
		}
		if !matched || resolution.Destination == nil || resolution.Destination.String() == candidate.Target {
			continue
		}
		destinations[index] = resolution.Destination.String()
	}
	if matchedWrapper && len(destinations) > 0 {
		return a.handleResolvedWrappers(ctx, input, urls, destinations)
	}

	if !moderation.IsLinkOnly(input) {
		return nil
	}
	fingerprint := moderation.Fingerprint(input, "link-preview")
	if canonical, found, err := a.state.FindCanonical(ctx, input.ChatID, input.ThreadID, input.SenderID, "link-preview", behaviorVersion, fingerprint); err != nil {
		return err
	} else if found {
		return a.suppressDuplicate(ctx, input, canonical)
	}
	return a.moderatePreview(ctx, input, urls)
}

func (a *Application) handleResolvedWrappers(ctx context.Context, input moderation.Input, urls []moderation.URL, destinations map[int]string) error {
	replaced, err := moderation.ReplaceURLSpans(messageText(input), urls, destinations)
	if err != nil {
		return fmt.Errorf("rewrite links: %w", err)
	}
	if moderation.IsLinkOnly(input) {
		resolved := resolvedURLs(urls, destinations)
		fingerprint := moderation.Fingerprint(input, "link-preview")
		canonical, found, findErr := a.state.FindCanonical(ctx, input.ChatID, input.ThreadID, input.SenderID, "link-preview", behaviorVersion, fingerprint)
		if findErr != nil {
			return findErr
		}
		if found {
			return a.suppressDuplicate(ctx, input, canonical)
		}
		previewPlan, actionable := a.previewPlan(ctx, input, resolved)
		if actionable && (previewPlan.Action == moderation.ActionDelete || input.PreviewDisabled) {
			return a.applyPlan(ctx, input, previewPlan)
		}
	}
	rule := "google-share"
	for index := range destinations {
		if resolutionRule, matched := a.links.MatchName(urls[index].Target); matched {
			rule = resolutionRule
			break
		}
	}
	plan := moderation.Plan{
		Action:      moderation.ActionReplace,
		Rule:        rule,
		Fingerprint: moderation.Fingerprint(input, rule),
		NoticeKey:   moderation.NoticeGoogleWrapper,
		Params:      map[string]string{"sender": input.SenderName, "content": replaced},
	}
	return a.applyPlan(ctx, input, plan)
}

func (a *Application) moderatePreview(ctx context.Context, input moderation.Input, urls []moderation.URL) error {
	plan, actionable := a.previewPlan(ctx, input, urls)
	if !actionable {
		return nil
	}
	return a.applyPlan(ctx, input, plan)
}

func (a *Application) previewPlan(ctx context.Context, input moderation.Input, urls []moderation.URL) (moderation.Plan, bool) {
	var previews []preview.Metadata
	for _, candidate := range urls {
		document, err := a.fetchMetadata(ctx, candidate.Target)
		if err != nil {
			slog.Warn("link metadata fetch failed", "host", safeHost(candidate.Target), "error", err)
			return moderation.Plan{}, false
		}
		metadata, _ := a.previews.Inspect(document)
		if a.previews.IsInconclusive(document) && !metadata.Useful() {
			return moderation.Plan{}, false
		}
		previews = append(previews, metadata)
	}
	useful := len(previews) > 0
	for _, metadata := range previews {
		if metadata.Useful() {
			continue
		}
		useful = false
		break
	}
	plan := moderation.Plan{
		Rule:        "link-preview",
		Fingerprint: moderation.Fingerprint(input, "link-preview"),
	}
	switch {
	case useful && input.PreviewDisabled:
		plan.Action = moderation.ActionReplace
		plan.NoticeKey = moderation.NoticePreviewEnriched
		plan.Params = map[string]string{
			"sender":   input.SenderName,
			"url":      urls[0].Target,
			"metadata": previewMetadataLines(previews),
		}
	case !useful:
		plan.Action = moderation.ActionDelete
		plan.NoticeKey = moderation.NoticePreviewMissing
	default:
		return moderation.Plan{}, false
	}
	return plan, true
}

func (a *Application) fetchMetadata(ctx context.Context, rawURL string) (preview.Document, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return preview.Document{}, err
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	switch host {
	case "facebook.com", "www.facebook.com", "m.facebook.com", "mbasic.facebook.com", "fb.watch", "fb.me":
		parsed.Host = "mbasic.facebook.com"
		return a.metadata.FetchWithUserAgent(ctx, parsed.String(), a.config.Metadata.FacebookUserAgent)
	}
	return a.metadata.Fetch(ctx, rawURL)
}

func resolvedURLs(urls []moderation.URL, destinations map[int]string) []moderation.URL {
	resolved := make([]moderation.URL, len(urls))
	copy(resolved, urls)
	for index, destination := range destinations {
		if index >= 0 && index < len(resolved) {
			resolved[index].Target = destination
			resolved[index].Raw = destination
		}
	}
	return resolved
}

func (a *Application) applyPlan(ctx context.Context, input moderation.Input, plan moderation.Plan) error {
	if plan.Action == moderation.ActionAllow || plan.Rule == "" || plan.Fingerprint == "" {
		return nil
	}
	canonical, found, err := a.state.FindCanonical(ctx, input.ChatID, input.ThreadID, input.SenderID, plan.Rule, behaviorVersion, plan.Fingerprint)
	if err != nil {
		return err
	}
	if found {
		return a.suppressDuplicate(ctx, input, canonical)
	}
	text := truncateRunes(a.notices.Render(plan.NoticeKey, plan.Params), messageTextLimit)
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("notice %q rendered empty for rule %s", plan.NoticeKey, plan.Rule)
	}
	payload := responsePayload{Text: text, Entities: responseEntities(text, input.SenderName, input.SenderID), Operation: operationText}
	if strings.HasPrefix(plan.Rule, "google-") && input.MediaKind != "" && input.Caption != "" {
		payload.Operation = operationCopyMedia
		payload.Text = truncateRunes(text, captionTextLimit)
		payload.Entities = responseEntities(payload.Text, input.SenderName, input.SenderID)
		payload.FallbackText = text
		payload.FallbackItems = payload.Entities
	}
	encodedPayload, err := encodeResponsePayload(payload)
	if err != nil {
		return err
	}
	action, created, err := a.state.CreateCanonical(ctx, store.CanonicalAction{
		ChatID: input.ChatID, ThreadID: input.ThreadID, UserID: input.SenderID,
		Rule: plan.Rule, BehaviorVersion: behaviorVersion, Fingerprint: plan.Fingerprint,
		Payload: encodedPayload, SourceMessageID: input.MessageID,
	})
	if err != nil {
		return err
	}
	if !created {
		return a.suppressDuplicate(ctx, input, action)
	}
	a.nudgeOutbox()
	if payload.Operation == operationCopyMedia {
		return a.applyMediaAction(ctx, input, action, payload)
	}
	return a.applyTextAction(ctx, input, action, payload)
}

func (a *Application) applyTextAction(ctx context.Context, input moderation.Input, action store.CanonicalAction, payload responsePayload) error {
	if err := a.state.MarkDeleteRequested(ctx, action.ID); err != nil {
		return err
	}
	if err := a.client.Delete(ctx, input.ChatID, input.MessageID); err != nil && !telegram.IsMessageNotFound(err) {
		if telegram.IsDeletePermissionError(err) {
			slog.Error("bot cannot delete moderated message", "subsystem", "telegram", "chat_id", input.ChatID, "message_id", input.MessageID, "error", err)
		}
		_ = a.markOutboxError(ctx, action.ID, "delete_requested", err)
		slog.Warn("message deletion failed; action queued", "chat_id", input.ChatID, "message_id", input.MessageID, "error", err)
		return nil
	}
	if err := a.state.MarkSendPending(ctx, action.ID); err != nil {
		return err
	}
	responseID, err := a.client.Send(ctx, input.ChatID, input.ThreadID, payload.Text, payload.Entities...)
	if err != nil {
		_ = a.markOutboxError(ctx, action.ID, "send_pending", err)
		slog.Warn("moderation response failed; action queued", "action_id", action.ID, "error", err)
		return nil
	}
	return a.state.MarkOutboxComplete(ctx, action.ID, responseID)
}

func (a *Application) applyMediaAction(ctx context.Context, input moderation.Input, action store.CanonicalAction, payload responsePayload) error {
	responseID, err := a.client.Copy(ctx, input.ChatID, input.ThreadID, input.MessageID, payload.Text, payload.Entities...)
	if err != nil {
		if !telegram.IsPermanentError(err) {
			_ = a.markOutboxError(ctx, action.ID, "planned", err)
			return nil
		}
		payload.Operation = operationText
		payload.Text = payload.FallbackText
		payload.Entities = payload.FallbackItems
		encoded, encodeErr := encodeResponsePayload(payload)
		if encodeErr != nil {
			return encodeErr
		}
		if replaceErr := a.state.ReplaceOutboxPayload(ctx, action.ID, encoded); replaceErr != nil {
			return replaceErr
		}
		return a.applyTextAction(ctx, input, action, payload)
	}
	if err := a.state.MarkOutboxCopied(ctx, action.ID, responseID); err != nil {
		return err
	}
	if err := a.client.Delete(ctx, input.ChatID, input.MessageID); err != nil && !telegram.IsMessageNotFound(err) {
		_ = a.markOutboxError(ctx, action.ID, "delete_requested", err)
		slog.Warn("message deletion failed after media copy; action queued", "action_id", action.ID, "error", err)
		return nil
	}
	return a.state.MarkOutboxComplete(ctx, action.ID, responseID)
}

func (a *Application) suppressDuplicate(ctx context.Context, input moderation.Input, canonical store.CanonicalAction) error {
	if err := a.client.Delete(ctx, input.ChatID, input.MessageID); err != nil {
		if telegram.IsDeletePermissionError(err) {
			slog.Error("bot cannot delete duplicate message", "subsystem", "telegram", "chat_id", input.ChatID, "message_id", input.MessageID, "error", err)
			return nil
		}
		if !telegram.IsMessageNotFound(err) {
			return err
		}
	}
	return a.state.RecordSuppressed(ctx, canonical.ID, input.MessageID)
}

func (a *Application) retryOutbox(ctx context.Context) error {
	entries, err := a.state.PendingOutbox(ctx, a.clock())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		actionID := entry.CanonicalActionID
		payload := decodeResponsePayload(entry.Payload)
		if payload.Operation == operationCopyMedia && entry.ResponseMessageID == 0 {
			prepared, responseID, ready, prepareErr := a.prepareMediaOutbox(ctx, entry, payload)
			if prepareErr != nil {
				_ = a.state.ReleaseOutboxLease(ctx, actionID)
				return prepareErr
			}
			payload = prepared
			if !ready {
				_ = a.state.ReleaseOutboxLease(ctx, actionID)
				continue
			}
			entry.ResponseMessageID = responseID
		}
		if entry.State == "planned" || entry.State == "delete_requested" {
			if entry.SourceMessageID <= 0 {
				_ = a.state.ReleaseOutboxLease(ctx, actionID)
				continue
			}
			if err := a.client.Delete(ctx, entry.ChatID, entry.SourceMessageID); err != nil && !telegram.IsMessageNotFound(err) {
				_ = a.markOutboxError(ctx, entry.CanonicalActionID, "delete_requested", err)
				_ = a.state.ReleaseOutboxLease(ctx, actionID)
				continue
			}
			if err := a.state.MarkSendPending(ctx, entry.CanonicalActionID); err != nil {
				_ = a.state.ReleaseOutboxLease(ctx, actionID)
				return err
			}
		}
		if entry.ResponseMessageID != 0 {
			if err := a.state.MarkOutboxComplete(ctx, entry.CanonicalActionID, entry.ResponseMessageID); err != nil {
				_ = a.state.ReleaseOutboxLease(ctx, actionID)
				return err
			}
			continue
		}
		responseID, err := a.client.Send(ctx, entry.ChatID, entry.ThreadID, payload.Text, payload.Entities...)
		if err != nil {
			_ = a.markOutboxError(ctx, entry.CanonicalActionID, "send_pending", err)
			_ = a.state.ReleaseOutboxLease(ctx, actionID)
			continue
		}
		if err := a.state.MarkOutboxComplete(ctx, entry.CanonicalActionID, responseID); err != nil {
			_ = a.state.ReleaseOutboxLease(ctx, actionID)
			return err
		}
	}
	return nil
}

func (a *Application) prepareMediaOutbox(ctx context.Context, entry store.OutboxEntry, payload responsePayload) (responsePayload, int, bool, error) {
	responseID, err := a.client.Copy(ctx, entry.ChatID, entry.ThreadID, entry.SourceMessageID, payload.Text, payload.Entities...)
	if err == nil {
		if markErr := a.state.MarkOutboxCopied(ctx, entry.CanonicalActionID, responseID); markErr != nil {
			return payload, 0, false, markErr
		}
		return payload, responseID, true, nil
	}
	if !telegram.IsPermanentError(err) {
		_ = a.markOutboxError(ctx, entry.CanonicalActionID, "planned", err)
		return payload, 0, false, nil
	}
	payload.Operation = operationText
	payload.Text = payload.FallbackText
	payload.Entities = payload.FallbackItems
	encoded, encodeErr := encodeResponsePayload(payload)
	if encodeErr != nil {
		return payload, 0, false, encodeErr
	}
	if replaceErr := a.state.ReplaceOutboxPayload(ctx, entry.CanonicalActionID, encoded); replaceErr != nil {
		return payload, 0, false, replaceErr
	}
	return payload, 0, true, nil
}

func (a *Application) outboxLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.retryOutbox(ctx); err != nil {
				slog.Warn("outbox worker failed", "error", err)
			}
		case <-a.outboxNudge:
			if err := a.retryOutbox(ctx); err != nil {
				slog.Warn("nudged outbox worker failed", "error", err)
			}
		}
	}
}

func (a *Application) nudgeOutbox() {
	select {
	case a.outboxNudge <- struct{}{}:
	default:
	}
}

func (a *Application) clock() time.Time {
	if a.now != nil {
		return a.now().UTC()
	}
	return time.Now().UTC()
}

func (a *Application) markOutboxError(ctx context.Context, actionID int64, state string, operationErr error) error {
	if telegram.IsPermanentError(operationErr) {
		slog.Error("moderation outbox dead-lettered", "action_id", actionID, "state", state, "error", operationErr)
		return a.state.MarkOutboxDead(ctx, actionID, operationErr)
	}
	if delay, ok := telegram.RetryAfter(operationErr); ok {
		return a.state.MarkOutboxErrorAfter(ctx, actionID, state, operationErr, delay)
	}
	return a.state.MarkOutboxError(ctx, actionID, state, operationErr)
}

type responsePayload struct {
	Text          string                 `json:"text"`
	Entities      []telego.MessageEntity `json:"entities,omitempty"`
	Operation     string                 `json:"operation"`
	FallbackText  string                 `json:"fallback_text,omitempty"`
	FallbackItems []telego.MessageEntity `json:"fallback_entities,omitempty"`
}

func encodeResponsePayload(payload responsePayload) (string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode response payload: %w", err)
	}
	return string(encoded), nil
}

func decodeResponsePayload(raw string) responsePayload {
	var payload responsePayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return responsePayload{Text: raw, Operation: operationText}
	}
	if payload.Operation == "" {
		payload.Operation = operationText
	}
	return payload
}

func responseEntities(text, senderName string, senderID int64) []telego.MessageEntity {
	if senderID <= 0 || senderName == "" {
		return nil
	}
	before, _, found := strings.Cut(text, senderName)
	if !found {
		return nil
	}
	return []telego.MessageEntity{{
		Type:   telego.EntityTypeTextMention,
		Offset: utf16Units(before),
		Length: utf16Units(senderName),
		User:   &telego.User{ID: senderID, FirstName: strings.TrimPrefix(senderName, "@")},
	}}
}

func utf16Units(value string) int {
	units := 0
	for _, r := range value {
		if r > 0xffff {
			units += 2
		} else {
			units++
		}
	}
	return units
}

func messageText(in moderation.Input) string {
	if in.Text != "" {
		return in.Text
	}
	return in.Caption
}

func previewMetadataLines(metadata []preview.Metadata) string {
	var b strings.Builder
	for _, item := range metadata {
		for _, value := range []string{item.SiteName, item.Title, item.Description} {
			if strings.TrimSpace(value) != "" {
				b.WriteString(strings.TrimSpace(value))
				b.WriteByte('\n')
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return strings.TrimSpace(string(runes[:limit])) + "…"
}

func safeHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid-url"
	}
	return u.Hostname()
}

// New constructs the moderation workflow over the supplied ports. The now
// function is injected so policy timing stays deterministic under test.
func New(cfg config.Config, client TelegramPort, state StorePort, metadataFetcher MetadataPort, links LinkPort, previews PreviewPort, notices NoticePort, now func() time.Time) *Application {
	allowedChats := make(map[int64]struct{}, len(cfg.Telegram.AllowedChatIDs))
	for _, chatID := range cfg.Telegram.AllowedChatIDs {
		allowedChats[chatID] = struct{}{}
	}
	return &Application{
		config: cfg, allowedChats: allowedChats, client: client, state: state,
		metadata: metadataFetcher, links: links, previews: previews, notices: notices,
		now: now, outboxNudge: make(chan struct{}, 1),
	}
}

// OutboxLoop delivers pending moderation responses until ctx is cancelled,
// waking on either the retry ticker or a nudge from a newly created action.
func (a *Application) OutboxLoop(ctx context.Context) { a.outboxLoop(ctx) }

// RecordUpdate notes that an update was observed, for the health endpoint.
func (a *Application) RecordUpdate() {
	a.updatesSeen.Add(1)
	a.lastUpdateUnix.Store(time.Now().Unix())
}
