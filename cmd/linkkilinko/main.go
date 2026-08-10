// Command linkkilinko runs the Telegram moderation bot.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mymmrac/telego"
	"github.com/sompasauna/linkkilinko/internal/config"
	"github.com/sompasauna/linkkilinko/internal/metadata"
	"github.com/sompasauna/linkkilinko/internal/notice"
	"github.com/sompasauna/linkkilinko/internal/store"
	"github.com/sompasauna/linkkilinko/internal/telegram"
	"github.com/sompasauna/linkkilinko/pkg/core/link"
	"github.com/sompasauna/linkkilinko/pkg/core/moderation"
	"github.com/sompasauna/linkkilinko/pkg/core/preview"
)

const behaviorVersion = "v0.1"

const (
	operationText      = "text"
	operationCopyMedia = "copy_media"
)

// Telegram caps message text at 4096 and media captions at 1024 characters.
// Stay below both so entity offsets never land past a truncated tail.
const (
	messageTextLimit = 3900
	captionTextLimit = 1000
)

func main() {
	configPath := flag.String("config", envOr("LINKKILINKO_CONFIG", "config.yaml"), "YAML configuration path")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	runErr := run(context.Background(), *configPath)
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		logger.Error("linkkilinko stopped", "error", runErr)
		os.Exit(1)
	}
}

func run(parent context.Context, configPath string) error {
	runtimeConfig, err := config.Load(configPath)
	if err != nil {
		return err
	}
	notices, err := notice.Load(runtimeConfig.Moderation.NoticeLanguage)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	state, err := store.Open(ctx, runtimeConfig.Database.Path)
	if err != nil {
		return err
	}
	defer func() { _ = state.Close() }()

	metadataFetcher, err := metadata.NewFetcher(metadata.Config{
		RequestTimeout: time.Duration(runtimeConfig.Metadata.RequestTimeout),
		TotalTimeout:   time.Duration(runtimeConfig.Metadata.TotalTimeout),
		MaxBodyBytes:   runtimeConfig.Metadata.MaxHTMLBytes,
		MaxRedirects:   runtimeConfig.Metadata.MaxRedirects,
		UserAgent:      runtimeConfig.Metadata.UserAgent,
	})
	if err != nil {
		return err
	}
	googleResolver, err := link.NewGoogleResolver(metadataLinkFetcher{fetcher: metadataFetcher})
	if err != nil {
		return err
	}
	linkRegistry, err := link.NewRegistry(googleResolver)
	if err != nil {
		return err
	}
	previewRegistry, err := preview.NewRegistry(preview.GenericHTMLProvider{})
	if err != nil {
		return err
	}
	client, err := telegram.New(runtimeConfig.Telegram.Token)
	if err != nil {
		return err
	}
	allowedChats := make(map[int64]struct{}, len(runtimeConfig.Telegram.AllowedChatIDs))
	for _, chatID := range runtimeConfig.Telegram.AllowedChatIDs {
		allowedChats[chatID] = struct{}{}
	}
	app := &application{
		config:       runtimeConfig,
		allowedChats: allowedChats,
		client:       client,
		state:        state,
		metadata:     metadataFetcher,
		links:        linkRegistry,
		previews:     previewRegistry,
		notices:      notices,
		now:          time.Now,
	}
	stopHealth := startHealthServer(ctx, app, runtimeConfig.Operational.HealthListen)
	defer stopHealth()
	go app.outboxLoop(ctx)
	return client.Run(ctx, app.handleUpdate)
}

type application struct {
	config         config.Config
	allowedChats   map[int64]struct{}
	client         *telegram.Client
	state          *store.Store
	metadata       *metadata.Fetcher
	links          link.Registry
	previews       preview.Registry
	notices        notice.Catalog
	now            func() time.Time
	updatesSeen    atomic.Uint64
	lastUpdateUnix atomic.Int64
}

func (a *application) handleUpdate(ctx context.Context, update telego.Update) error {
	a.updatesSeen.Add(1)
	a.lastUpdateUnix.Store(time.Now().Unix())
	if err := a.retryOutbox(ctx); err != nil {
		slog.Warn("outbox retry failed", "error", err)
	}
	if update.ChatMember != nil {
		return a.handleMembership(ctx, *update.ChatMember)
	}
	if update.MyChatMember != nil {
		return nil
	}
	message := update.Message
	if message == nil {
		message = update.EditedMessage
	}
	if message == nil || message.MessageID <= 0 {
		return nil
	}
	if _, ok := a.allowedChats[message.Chat.ID]; !ok {
		return nil
	}
	if len(message.NewChatMembers) > 0 {
		for _, member := range message.NewChatMembers {
			if err := a.state.RecordMembership(ctx, message.Chat.ID, member.ID, telego.MemberStatusMember, time.Unix(message.Date, 0).UTC(), true); err != nil {
				return err
			}
		}
		return nil
	}
	if message.From == nil || message.From.IsBot {
		return nil
	}
	editDate := message.EditDate
	claimed, err := a.state.ClaimUpdate(ctx, message.Chat.ID, message.MessageID, editDate)
	if err != nil || !claimed {
		return err
	}
	input := messageInput(*message)
	if err := a.processMessage(ctx, input); err != nil {
		return err
	}
	return a.state.CompleteUpdate(ctx, input.ChatID, input.MessageID, input.EditDate)
}

func (a *application) processMessage(ctx context.Context, input moderation.Input) error {
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

func (a *application) handleMembership(ctx context.Context, update telego.ChatMemberUpdated) error {
	if _, ok := a.allowedChats[update.Chat.ID]; !ok || update.NewChatMember == nil {
		return nil
	}
	status := update.NewChatMember.MemberStatus()
	active := status == telego.MemberStatusCreator || status == telego.MemberStatusAdministrator || status == telego.MemberStatusMember || status == telego.MemberStatusRestricted
	member := update.NewChatMember.MemberUser()
	return a.state.RecordMembership(ctx, update.Chat.ID, member.ID, status, time.Unix(update.Date, 0).UTC(), active)
}

func (a *application) moderateLinks(ctx context.Context, input moderation.Input) error {
	urls := moderation.ExtractURLs(input)
	if len(urls) == 0 {
		return nil
	}
	destinations := make(map[int]string)
	matchedWrapper := false
	for index, candidate := range urls {
		if !a.links.Match(candidate.Target) {
			continue
		}
		matchedWrapper = true
		fingerprint := moderation.Fingerprint(input, "google-wrapper")
		if canonical, found, err := a.state.FindCanonical(ctx, input.ChatID, input.ThreadID, input.SenderID, "google-wrapper", behaviorVersion, fingerprint); err != nil {
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

func (a *application) handleResolvedWrappers(ctx context.Context, input moderation.Input, urls []moderation.URL, destinations map[int]string) error {
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
	plan := moderation.Plan{
		Action:      moderation.ActionReplace,
		Rule:        "google-wrapper",
		Fingerprint: moderation.Fingerprint(input, "google-wrapper"),
		NoticeKey:   moderation.NoticeGoogleWrapper,
		Params:      map[string]string{"sender": input.SenderName, "content": replaced},
	}
	return a.applyPlan(ctx, input, plan)
}

func (a *application) moderatePreview(ctx context.Context, input moderation.Input, urls []moderation.URL) error {
	plan, actionable := a.previewPlan(ctx, input, urls)
	if !actionable {
		return nil
	}
	return a.applyPlan(ctx, input, plan)
}

func (a *application) previewPlan(ctx context.Context, input moderation.Input, urls []moderation.URL) (moderation.Plan, bool) {
	var previews []preview.Metadata
	for _, candidate := range urls {
		document, err := a.metadata.Fetch(ctx, candidate.Target)
		if err != nil {
			slog.Warn("link metadata fetch failed", "host", safeHost(candidate.Target), "error", err)
			return moderation.Plan{}, false
		}
		metadata, _ := a.previews.Inspect(document)
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

func (a *application) applyPlan(ctx context.Context, input moderation.Input, plan moderation.Plan) error {
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
	if plan.Rule == "google-wrapper" && input.MediaKind != "" && input.Caption != "" {
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
	if payload.Operation == operationCopyMedia {
		return a.applyMediaAction(ctx, input, action, payload)
	}
	return a.applyTextAction(ctx, input, action, payload)
}

func (a *application) applyTextAction(ctx context.Context, input moderation.Input, action store.CanonicalAction, payload responsePayload) error {
	if err := a.state.MarkDeleteRequested(ctx, action.ID); err != nil {
		return err
	}
	if err := a.client.Delete(ctx, input.ChatID, input.MessageID); err != nil && !telegram.IsMessageNotFound(err) {
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

func (a *application) applyMediaAction(ctx context.Context, input moderation.Input, action store.CanonicalAction, payload responsePayload) error {
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

func (a *application) suppressDuplicate(ctx context.Context, input moderation.Input, canonical store.CanonicalAction) error {
	if err := a.client.Delete(ctx, input.ChatID, input.MessageID); err != nil {
		if !telegram.IsMessageNotFound(err) {
			return err
		}
	}
	return a.state.RecordSuppressed(ctx, canonical.ID, input.MessageID)
}

func (a *application) retryOutbox(ctx context.Context) error {
	entries, err := a.state.PendingOutbox(ctx, a.clock())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		payload := decodeResponsePayload(entry.Payload)
		if payload.Operation == operationCopyMedia && entry.ResponseMessageID == 0 {
			prepared, responseID, ready, prepareErr := a.prepareMediaOutbox(ctx, entry, payload)
			if prepareErr != nil {
				return prepareErr
			}
			payload = prepared
			if !ready {
				continue
			}
			entry.ResponseMessageID = responseID
		}
		if entry.State == "planned" || entry.State == "delete_requested" {
			if entry.SourceMessageID <= 0 {
				continue
			}
			if err := a.client.Delete(ctx, entry.ChatID, entry.SourceMessageID); err != nil && !telegram.IsMessageNotFound(err) {
				_ = a.markOutboxError(ctx, entry.CanonicalActionID, "delete_requested", err)
				continue
			}
			if err := a.state.MarkSendPending(ctx, entry.CanonicalActionID); err != nil {
				return err
			}
		}
		if entry.ResponseMessageID != 0 {
			if err := a.state.MarkOutboxComplete(ctx, entry.CanonicalActionID, entry.ResponseMessageID); err != nil {
				return err
			}
			continue
		}
		responseID, err := a.client.Send(ctx, entry.ChatID, entry.ThreadID, payload.Text, payload.Entities...)
		if err != nil {
			_ = a.markOutboxError(ctx, entry.CanonicalActionID, "send_pending", err)
			continue
		}
		if err := a.state.MarkOutboxComplete(ctx, entry.CanonicalActionID, responseID); err != nil {
			return err
		}
	}
	return nil
}

func (a *application) prepareMediaOutbox(ctx context.Context, entry store.OutboxEntry, payload responsePayload) (responsePayload, int, bool, error) {
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

func (a *application) outboxLoop(ctx context.Context) {
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
		}
	}
}

func (a *application) clock() time.Time {
	if a.now != nil {
		return a.now().UTC()
	}
	return time.Now().UTC()
}

func (a *application) markOutboxError(ctx context.Context, actionID int64, state string, operationErr error) error {
	if telegram.IsPermanentError(operationErr) {
		slog.Error("moderation outbox dead-lettered", "action_id", actionID, "state", state, "error", operationErr)
		return a.state.MarkOutboxDead(ctx, actionID, operationErr)
	}
	if delay, ok := telegram.RetryAfter(operationErr); ok {
		return a.state.MarkOutboxErrorAfter(ctx, actionID, state, operationErr, delay)
	}
	return a.state.MarkOutboxError(ctx, actionID, state, operationErr)
}

func safeHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid-url"
	}
	return u.Hostname()
}

func messageInput(message telego.Message) moderation.Input {
	from := *message.From
	return moderation.Input{
		ChatID:          message.Chat.ID,
		ThreadID:        message.MessageThreadID,
		MessageID:       message.MessageID,
		EditDate:        message.EditDate,
		SenderID:        from.ID,
		SenderName:      senderName(from),
		SenderIsBot:     from.IsBot,
		Text:            message.Text,
		Entities:        entities(message.Entities),
		Caption:         message.Caption,
		CaptionEntities: entities(message.CaptionEntities),
		MediaKind:       mediaKind(message),
		MediaUniqueID:   mediaUniqueID(message),
		MediaGroupID:    message.MediaGroupID,
		PreviewDisabled: message.LinkPreviewOptions != nil && message.LinkPreviewOptions.IsDisabled,
	}
}

func entities(source []telego.MessageEntity) []moderation.Entity {
	result := make([]moderation.Entity, 0, len(source))
	for _, entity := range source {
		result = append(result, moderation.Entity{Type: entity.Type, Offset: entity.Offset, Length: entity.Length, URL: entity.URL})
	}
	return result
}

func mediaKind(message telego.Message) string {
	switch {
	case message.Animation != nil:
		return "animation"
	case message.Audio != nil:
		return "audio"
	case message.Document != nil:
		return "document"
	case message.LivePhoto != nil:
		return "live_photo"
	case len(message.Photo) > 0:
		return "photo"
	case message.Sticker != nil:
		return "sticker"
	case message.Story != nil:
		return "story"
	case message.Video != nil:
		return "video"
	case message.VideoNote != nil:
		return "video_note"
	case message.Voice != nil:
		return "voice"
	case message.PaidMedia != nil:
		return "paid_media"
	case message.MediaGroupID != "":
		return "media_group"
	default:
		return ""
	}
}

func mediaUniqueID(message telego.Message) string {
	switch {
	case message.Animation != nil:
		return message.Animation.FileUniqueID
	case message.Audio != nil:
		return message.Audio.FileUniqueID
	case message.Document != nil:
		return message.Document.FileUniqueID
	case message.LivePhoto != nil:
		return message.LivePhoto.FileUniqueID
	case len(message.Photo) > 0:
		return message.Photo[len(message.Photo)-1].FileUniqueID
	case message.Video != nil:
		return message.Video.FileUniqueID
	case message.Voice != nil:
		return message.Voice.FileUniqueID
	case message.Sticker != nil:
		return message.Sticker.FileUniqueID
	case message.VideoNote != nil:
		return message.VideoNote.FileUniqueID
	case message.Story != nil:
		return fmt.Sprintf("story:%d", message.Story.ID)
	default:
		return ""
	}
}

func messageText(in moderation.Input) string {
	if in.Text != "" {
		return in.Text
	}
	return in.Caption
}

func senderName(user telego.User) string {
	if user.Username != "" {
		return "@" + user.Username
	}
	name := strings.TrimSpace(strings.Join([]string{user.FirstName, user.LastName}, " "))
	if name != "" {
		return name
	}
	return fmt.Sprintf("käyttäjä %d", user.ID)
}

// previewMetadataLines renders compact metadata for each URL in original order
// as the {metadata} parameter of the enriched-preview notice.
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

type metadataLinkFetcher struct {
	fetcher *metadata.Fetcher
}

func (f metadataLinkFetcher) Fetch(ctx context.Context, rawURL string) (link.FetchResult, error) {
	document, err := f.fetcher.Fetch(ctx, rawURL)
	if err != nil {
		return link.FetchResult{}, err
	}
	return link.FetchResult{URL: document.URL, Body: document.Body, ContentType: document.ContentType, StatusCode: 200}, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
