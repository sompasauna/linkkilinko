// Package action composes moderation policy with durable side effects.
package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
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

// Terminal outcome categories used in moderation decision summaries so log
// queries can filter on outcome without parsing free-form text.
const (
	outcomeAllow           = "allow"
	outcomeReplace         = "replace"
	outcomeDelete          = "delete"
	outcomeDuplicate       = "duplicate_suppressed"
	outcomeFailOpen        = "fail_open"
	outcomeResolverNoMatch = "resolver_no_match"
	outcomePreviewUseful   = "preview_useful"
)

// previewProviderLabel is a debug-friendly identifier for the preview
// provider in use, kept in logs only.
const previewProviderLabel = "generic_html"

const (
	operationText       = "text"
	operationCopyMedia  = "copy_media"
	messageTextLimit    = 3900
	captionTextLimit    = 1000
	placeholderSender   = "sender"
	placeholderContent  = "content"
	placeholderURL      = "url"
	placeholderMetadata = "metadata"
)

// Stable field names used across subsystem-scoped moderation logs so an
// operator can query one message from the Telegram adapter through the outbox.
const (
	fieldSubsystem           = "subsystem"
	fieldUpdateID            = "update_id"
	fieldChatID              = "chat_id"
	fieldThreadID            = "thread_id"
	fieldMessageID           = "message_id"
	fieldSenderID            = "sender_id"
	fieldURLCount            = "url_count"
	fieldURLIndex            = "url_index"
	fieldURLHost             = "url_host"
	fieldURLHasQuery         = "url_has_query"
	fieldLinkOnly            = "link_only"
	fieldPreviewOptions      = "preview_options"
	fieldPreviewDisabled     = "preview_disabled"
	fieldRule                = "rule"
	fieldOutcome             = "outcome"
	fieldDurationMS          = "duration_ms"
	fieldResolverName        = "resolver_name"
	fieldResolverMatched     = "resolver_matched"
	fieldResolutionOutcome   = "resolution_outcome"
	fieldDestinationHost     = "destination_host"
	fieldMetadataProvider    = "metadata_provider"
	fieldPreviewUseful       = "preview_useful"
	fieldPreviewInconclusive = "preview_inconclusive"
	fieldFetchErrorClass     = "fetch_error_class"
	fieldMultiLink           = "multi_link"
	fieldURLIndexes          = "url_indexes"
	fieldFailureOpen         = "fail_open"
)

// Application composes the pure core packages with the Telegram, storage,
// metadata, and notice ports to run the moderation workflow.
type Application struct {
	config         config.Config
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
	logger         *slog.Logger
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
}

// LinkPort resolves tracked links.
type LinkPort interface {
	MatchName(rawURL string) (string, bool)
	Resolve(ctx context.Context, rawURL string) (link.Resolution, bool, error)
}

// PreviewPort extracts metadata and identifies inconclusive providers.
type PreviewPort interface {
	Inspect(document preview.Document) (preview.Metadata, string)
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
	started := a.now()
	trace := newURLTrace(input, urls, a.now)
	destinations := make(map[int]string)
	matchedWrapper := false
	for index, candidate := range urls {
		resolverName, matched := a.links.MatchName(candidate.Target)
		trace.recordResolver(index, resolverName, matched)
		if !matched {
			continue
		}
		matchedWrapper = true
		fingerprint := moderation.Fingerprint(input, resolverName)
		if canonical, found, err := a.state.FindCanonical(ctx, input.ChatID, input.ThreadID, input.SenderID, resolverName, behaviorVersion, fingerprint); err != nil {
			return err
		} else if found {
			trace.recordOutcome(index, outcomeDuplicate)
			a.emitTerminalDecision(trace, started)
			return a.suppressDuplicate(ctx, input, canonical)
		}
		resolution, matched, err := a.links.Resolve(ctx, candidate.Target)
		if err != nil {
			a.logger.Warn("link resolution failed",
				fieldChatID, input.ChatID,
				fieldMessageID, input.MessageID,
				fieldURLIndex, index,
				fieldURLHost, safeHost(candidate.Target),
				"error", err,
			)
			trace.recordOutcome(index, outcomeFailOpen)
			a.emitTerminalDecision(trace, started)
			return nil
		}
		if !matched || resolution.Destination == nil || resolution.Destination.String() == candidate.Target {
			trace.recordOutcome(index, outcomeResolverNoMatch)
			continue
		}
		destinations[index] = resolution.Destination.String()
		trace.recordResolution(index, resolution.Destination.String(), true)
	}
	if matchedWrapper && len(destinations) > 0 {
		trace.markResolverPhase()
		if err := a.handleResolvedWrappersTraced(ctx, input, urls, destinations, trace); err != nil {
			return err
		}
		a.emitTerminalDecision(trace, started)
		return nil
	}

	if !moderation.IsLinkOnly(input) {
		trace.recordMessageOutcome(outcomeAllow, "non_link_only")
		a.emitTerminalDecision(trace, started)
		return nil
	}
	fingerprint := moderation.Fingerprint(input, "link-preview")
	if canonical, found, err := a.state.FindCanonical(ctx, input.ChatID, input.ThreadID, input.SenderID, "link-preview", behaviorVersion, fingerprint); err != nil {
		return err
	} else if found {
		trace.recordMessageOutcome(outcomeDuplicate, "link_preview")
		a.emitTerminalDecision(trace, started)
		return a.suppressDuplicate(ctx, input, canonical)
	}
	plan, actionable := a.moderatePreviewTraced(ctx, input, urls, trace)
	if actionable {
		a.emitTerminalDecision(trace, started)
		return a.applyPlan(ctx, input, plan)
	}
	a.emitTerminalDecision(trace, started)
	return nil
}

func (a *Application) handleResolvedWrappersTraced(ctx context.Context, input moderation.Input, urls []moderation.URL, destinations map[int]string, trace *urlTrace) error {
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
			trace.recordMessageOutcome(outcomeDuplicate, "link-preview")
			return a.suppressDuplicate(ctx, input, canonical)
		}
		previewPlan, actionable := a.moderatePreviewTraced(ctx, input, resolved, trace)
		if actionable && (previewPlan.Action == moderation.ActionDelete || input.PreviewDisabled) {
			return a.applyPlan(ctx, input, previewPlan)
		}
	}
	rule := "google-share"
	lowestIndex := len(urls)
	for index := range destinations {
		if index < lowestIndex {
			if resolutionRule, matched := a.links.MatchName(urls[index].Target); matched {
				rule = resolutionRule
				lowestIndex = index
			}
		}
	}
	plan := moderation.Plan{
		Action:      moderation.ActionReplace,
		Rule:        rule,
		Fingerprint: moderation.Fingerprint(input, rule),
		NoticeKey:   moderation.NoticeGoogleWrapper,
		Params:      map[string]string{placeholderSender: input.SenderName, placeholderContent: replaced},
	}
	for index := range destinations {
		if trace.decisions[index].outcome == "" {
			trace.recordOutcome(index, outcomeReplace)
		}
	}
	trace.recordMessageOutcome(outcomeReplace, rule)
	return a.applyPlan(ctx, input, plan)
}

// moderatePreviewTraced runs the preview-policy decision and records per-URL
// reasoning onto trace. The fetch failure class is captured so the terminal
// decision log distinguishes fail-open from definitive no-metadata.
func (a *Application) moderatePreviewTraced(ctx context.Context, input moderation.Input, urls []moderation.URL, trace *urlTrace) (moderation.Plan, bool) {
	trace.markPreviewPhase()
	var previews []preview.Metadata
	for index, candidate := range urls {
		document, err := a.fetchMetadata(ctx, candidate.Target)
		if err != nil {
			a.logger.Warn("link metadata fetch failed",
				fieldChatID, input.ChatID,
				fieldMessageID, input.MessageID,
				fieldURLIndex, index,
				fieldURLHost, safeHost(candidate.Target),
				fieldFetchErrorClass, classifyFetchError(err),
				"error", err,
			)
			trace.recordMetadata(index, previewProviderLabel, false, true, classifyFetchError(err))
			trace.recordOutcome(index, outcomeFailOpen)
			trace.recordMessageOutcome(outcomeFailOpen, "link-preview")
			return moderation.Plan{}, false
		}
		metadata, _ := a.previews.Inspect(document)
		previews = append(previews, metadata)
		trace.recordMetadata(index, previewProviderLabel, metadata.Useful(), false, "")
		trace.recordOutcome(index, outcomePreviewUseful)
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
			placeholderSender:   input.SenderName,
			placeholderURL:      urls[0].Target,
			placeholderMetadata: previewMetadataLines(previews),
		}
		trace.recordMessageOutcome(outcomeReplace, "link-preview")
	case !useful:
		plan.Action = moderation.ActionDelete
		plan.NoticeKey = moderation.NoticePreviewMissing
		plan.Params = map[string]string{
			placeholderSender: input.SenderName,
		}
		trace.recordMessageOutcome(outcomeDelete, "link-preview")
	default:
		return moderation.Plan{}, false
	}
	return plan, true
}

func (a *Application) fetchMetadata(ctx context.Context, rawURL string) (preview.Document, error) {
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

// classifyFetchError reduces a metadata fetch error to a short stable label
// so logs and tests can reason about transient versus permanent failures.
func classifyFetchError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "transient_timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "transient_cancelled"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "transient_timeout"
	}
	return "permanent"
}

// urlDecision is the per-URL reasoning captured during moderation. It is
// recorded locally so a single terminal decision log can summarize every URL
// without re-walking the message.
type urlDecision struct {
	index               int
	raw                 string
	host                string
	hasQuery            bool
	resolverName        string
	resolverMatched     bool
	resolved            bool
	destinationHost     string
	metadataProvider    string
	previewUseful       bool
	previewInconclusive bool
	fetchErrorClass     string
	outcome             string
}

// urlTrace carries the per-message reasoning state and exposes a method to
// emit the terminal decision summary.
type urlTrace struct {
	input          moderation.Input
	urls           []moderation.URL
	decisions      []urlDecision
	resolverPhase  bool
	previewPhase   bool
	messageOutcome string
	messageReason  string
	startedAt      time.Time
	now            func() time.Time
}

func newURLTrace(input moderation.Input, urls []moderation.URL, now func() time.Time) *urlTrace {
	decisions := make([]urlDecision, len(urls))
	for index, candidate := range urls {
		decisions[index] = urlDecision{
			index:    index,
			raw:      candidate.Target,
			host:     safeHost(candidate.Target),
			hasQuery: urlHasQuery(candidate.Target),
		}
	}
	return &urlTrace{
		input:     input,
		urls:      urls,
		decisions: decisions,
		now:       now,
		startedAt: now(),
	}
}

func (t *urlTrace) recordResolver(index int, name string, matched bool) {
	t.decisions[index].resolverName = name
	t.decisions[index].resolverMatched = matched
}

func (t *urlTrace) recordResolution(index int, destination string, matched bool) {
	t.decisions[index].resolved = matched
	t.decisions[index].destinationHost = safeHost(destination)
}

func (t *urlTrace) recordMetadata(index int, provider string, useful, inconclusive bool, errClass string) {
	t.decisions[index].metadataProvider = provider
	t.decisions[index].previewUseful = useful
	t.decisions[index].previewInconclusive = inconclusive
	t.decisions[index].fetchErrorClass = errClass
}

func (t *urlTrace) recordOutcome(index int, outcome string) {
	t.decisions[index].outcome = outcome
}

func (t *urlTrace) markResolverPhase() {
	t.resolverPhase = true
}

func (t *urlTrace) markPreviewPhase() {
	t.previewPhase = true
}

func (t *urlTrace) recordMessageOutcome(outcome, reason string) {
	t.messageOutcome = outcome
	t.messageReason = reason
}

// urlIndexesWith returns the indexes whose recorded outcome equals want. Used
// to make multi-link reasoning explicit in the terminal log.
func (t *urlTrace) urlIndexesWith(outcome string) []int {
	var indexes []int
	for _, decision := range t.decisions {
		if decision.outcome == outcome {
			indexes = append(indexes, decision.index)
		}
	}
	return indexes
}

func urlHasQuery(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.RawQuery != ""
}

// emitTerminalDecision writes the per-message decision summary plus the
// correlated per-URL reasoning logs. Sensitive URL data is reduced to host
// plus a query-presence flag; full URLs are not recorded.
func (a *Application) emitTerminalDecision(trace *urlTrace, started time.Time) {
	if trace == nil {
		return
	}
	durationMS := trace.now().Sub(started).Milliseconds()
	replaceIndexes := trace.urlIndexesWith(outcomeReplace)
	deleteIndexes := trace.urlIndexesWith(outcomeDelete)
	failOpenIndexes := trace.urlIndexesWith(outcomeFailOpen)
	previewOptionsPresent := trace.input.PreviewDisabled || hasPreviewEntities(trace.input.Entities) || hasPreviewEntities(trace.input.CaptionEntities)
	summary := a.logger.With(
		fieldChatID, trace.input.ChatID,
		fieldThreadID, trace.input.ThreadID,
		fieldMessageID, trace.input.MessageID,
		fieldSenderID, trace.input.SenderID,
		fieldURLCount, len(trace.decisions),
		fieldLinkOnly, moderation.IsLinkOnly(trace.input),
		fieldPreviewOptions, previewOptionsPresent,
		fieldPreviewDisabled, trace.input.PreviewDisabled,
		fieldMultiLink, len(trace.decisions) > 1,
		fieldOutcome, trace.messageOutcome,
		fieldRule, trace.messageReason,
		fieldDurationMS, durationMS,
	)
	switch {
	case len(replaceIndexes) > 0:
		summary.Info("moderation decision",
			fieldOutcome, outcomeReplace,
			fieldURLIndexes, replaceIndexes,
		)
	case len(deleteIndexes) > 0:
		summary.Info("moderation decision",
			fieldOutcome, outcomeDelete,
			fieldURLIndexes, deleteIndexes,
		)
	case len(failOpenIndexes) > 0:
		summary.Warn("moderation decision",
			fieldOutcome, outcomeFailOpen,
			fieldURLIndexes, failOpenIndexes,
		)
	default:
		summary.Info("moderation decision")
	}
	for _, decision := range trace.decisions {
		a.logger.Info("url reasoning",
			fieldChatID, trace.input.ChatID,
			fieldMessageID, trace.input.MessageID,
			fieldURLIndex, decision.index,
			fieldURLHost, decision.host,
			fieldURLHasQuery, decision.hasQuery,
			fieldResolverName, decision.resolverName,
			fieldResolverMatched, decision.resolverMatched,
			fieldResolutionOutcome, resolutionOutcomeLabel(decision),
			fieldDestinationHost, decision.destinationHost,
			fieldMetadataProvider, decision.metadataProvider,
			fieldPreviewUseful, decision.previewUseful,
			fieldPreviewInconclusive, decision.previewInconclusive,
			fieldFetchErrorClass, decision.fetchErrorClass,
			fieldOutcome, decision.outcome,
		)
	}
}

func resolutionOutcomeLabel(decision urlDecision) string {
	switch {
	case !decision.resolverMatched:
		return "no_resolver_match"
	case decision.resolved:
		return "resolved"
	default:
		return "resolver_did_not_apply"
	}
}

func hasPreviewEntities(entities []moderation.Entity) bool {
	for _, entity := range entities {
		if entity.Type == "text_link" || entity.Type == "url" {
			return true
		}
	}
	return false
}

// New constructs the moderation workflow over the supplied ports. The now
// function is injected so policy timing stays deterministic under test.
func New(cfg config.Config, client TelegramPort, state StorePort, metadataFetcher MetadataPort, links LinkPort, previews PreviewPort, notices NoticePort, now func() time.Time) *Application {
	return &Application{
		config: cfg, client: client, state: state,
		metadata: metadataFetcher, links: links, previews: previews, notices: notices,
		now: now, outboxNudge: make(chan struct{}, 1),
		logger: slog.Default().With(fieldSubsystem, "moderation"),
	}
}

// SetLogger replaces the subsystem-scoped moderation logger. Callers that do
// not configure a logger get slog.Default() with subsystem=moderation.
func (a *Application) SetLogger(logger *slog.Logger) {
	if logger == nil {
		a.logger = slog.Default().With(fieldSubsystem, "moderation")
		return
	}
	a.logger = logger.With(fieldSubsystem, "moderation")
}

// OutboxLoop delivers pending moderation responses until ctx is cancelled,
// waking on either the retry ticker or a nudge from a newly created action.
func (a *Application) OutboxLoop(ctx context.Context) { a.outboxLoop(ctx) }

// RecordUpdate notes that an update was observed, for the health endpoint.
func (a *Application) RecordUpdate() {
	a.updatesSeen.Add(1)
	a.lastUpdateUnix.Store(time.Now().Unix())
}
