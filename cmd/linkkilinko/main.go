// Command linkkilinko runs the Telegram moderation bot.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
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
	"github.com/sompasauna/linkkilinko/pkg/action"
	"github.com/sompasauna/linkkilinko/pkg/core/link"
	"github.com/sompasauna/linkkilinko/pkg/core/moderation"
	"github.com/sompasauna/linkkilinko/pkg/core/preview"
)

const (
	operationText      = "text"
	operationCopyMedia = "copy_media"
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
	notices, err := notice.LoadWithOverride(runtimeConfig.Moderation.NoticeLanguage, runtimeConfig.Moderation.NoticeCatalog)
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
	googleShare, googleAMP, err := link.NewGoogleResolvers(metadataLinkFetcher{fetcher: metadataFetcher})
	if err != nil {
		return err
	}
	linkRegistry, err := link.NewRegistry(googleShare, googleAMP)
	if err != nil {
		return err
	}
	previewRegistry, err := preview.NewRegistry(&preview.FacebookProvider{}, preview.GenericHTMLProvider{})
	if err != nil {
		return err
	}
	client, err := telegram.New(runtimeConfig.Telegram.Token)
	if err != nil {
		return err
	}
	workflow := action.New(runtimeConfig, client, state, metadataFetcher, linkRegistry, previewRegistry, notices, time.Now)
	allowedChats := make(map[int64]struct{}, len(runtimeConfig.Telegram.AllowedChatIDs))
	for _, chatID := range runtimeConfig.Telegram.AllowedChatIDs {
		allowedChats[chatID] = struct{}{}
	}
	app := &application{
		config:       runtimeConfig,
		allowedChats: allowedChats,
		client:       client,
		state:        state,
		workflow:     workflow,
	}
	stopHealth := startHealthServer(ctx, app, runtimeConfig.Operational.HealthListen)
	defer stopHealth()
	go workflow.OutboxLoop(ctx)
	return client.Run(ctx, app.handleUpdate)
}

type application struct {
	config         config.Config
	allowedChats   map[int64]struct{}
	client         *telegram.Client
	state          *store.Store
	workflow       *action.Application
	updatesSeen    atomic.Uint64
	lastUpdateUnix atomic.Int64
}

func (a *application) handleUpdate(ctx context.Context, update telego.Update) error {
	a.updatesSeen.Add(1)
	a.lastUpdateUnix.Store(time.Now().Unix())
	if update.Message != nil && update.Message.Chat.Type == telego.ChatTypePrivate {
		return a.handlePrivateMessage(ctx, *update.Message)
	}
	if update.EditedMessage != nil && update.EditedMessage.Chat.Type == telego.ChatTypePrivate {
		return nil
	}
	if update.ChatMember != nil {
		return a.handleMembership(ctx, *update.ChatMember)
	}
	if update.MyChatMember != nil {
		return a.handleBotMembership(ctx, *update.MyChatMember)
	}
	message := update.Message
	if message == nil {
		message = update.EditedMessage
	}
	if message == nil || message.MessageID <= 0 {
		return nil
	}
	active, err := a.activeChat(ctx, message.Chat.ID)
	if err != nil {
		return err
	}
	if !active {
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
	input := messageInput(*message)
	claimed, err := a.state.ClaimUpdate(ctx, input.ChatID, input.MessageID, input.EditDate)
	if err != nil || !claimed {
		return err
	}
	if err := a.workflow.Process(ctx, input); err != nil {
		if failErr := a.state.FailUpdate(ctx, input.ChatID, input.MessageID, input.EditDate, err); failErr != nil {
			slog.Error("failed to record terminal update failure", "subsystem", "store", "chat_id", input.ChatID, "message_id", input.MessageID, "error", failErr)
		}
		return err
	}
	return a.state.CompleteUpdate(ctx, input.ChatID, input.MessageID, input.EditDate)
}

func (a *application) handlePrivateMessage(ctx context.Context, message telego.Message) error {
	if message.From == nil || strings.TrimSpace(message.Text) != "/start" {
		return nil
	}
	registered, err := a.state.RegisterOwner(ctx, message.From.ID)
	if err != nil {
		return err
	}
	if registered {
		slog.Info("registered bot owner", "subsystem", "moderation", "user_id", message.From.ID)
		return nil
	}
	owner, _, err := a.state.Owner(ctx)
	if err != nil {
		return err
	}
	if owner != message.From.ID {
		slog.Warn("rejected owner bootstrap claim", "subsystem", "moderation", "user_id", message.From.ID)
	}
	return nil
}

func (a *application) handleBotMembership(ctx context.Context, update telego.ChatMemberUpdated) error {
	owner, found, err := a.state.Owner(ctx)
	if err != nil {
		return err
	}
	if !ownerMatches(found, owner, update.From.ID) || update.NewChatMember == nil {
		slog.Warn("ignored unauthorized group approval", "subsystem", "moderation", "chat_id", update.Chat.ID, "user_id", update.From.ID)
		return nil
	}
	if !isGroupChat(update.Chat.Type) {
		return nil
	}
	if update.NewChatMember.MemberStatus() != telego.MemberStatusAdministrator && update.NewChatMember.MemberStatus() != telego.MemberStatusCreator {
		slog.Warn("group approval pending bot administrator status", "subsystem", "moderation", "chat_id", update.Chat.ID)
		return nil
	}
	canDelete, err := a.client.HasDeletePermission(ctx, update.Chat.ID)
	if err != nil {
		return err
	}
	if !canDelete {
		slog.Error("group approval rejected: bot lacks delete permission", "subsystem", "telegram", "chat_id", update.Chat.ID)
		return nil
	}
	if err := a.state.ApproveChat(ctx, update.Chat.ID, owner); err != nil {
		return err
	}
	slog.Info("approved group", "subsystem", "moderation", "chat_id", update.Chat.ID, "user_id", owner)
	return nil
}

func ownerMatches(found bool, ownerID, actorID int64) bool {
	return found && ownerID > 0 && ownerID == actorID
}

func isGroupChat(chatType string) bool {
	return chatType == telego.ChatTypeGroup || chatType == telego.ChatTypeSupergroup
}

func (a *application) activeChat(ctx context.Context, chatID int64) (bool, error) {
	if _, ok := a.allowedChats[chatID]; ok {
		return true, nil
	}
	return a.state.ApprovedChat(ctx, chatID)
}

func (a *application) handleMembership(ctx context.Context, update telego.ChatMemberUpdated) error {
	active, err := a.activeChat(ctx, update.Chat.ID)
	if err != nil || !active || update.NewChatMember == nil {
		return err
	}
	status := update.NewChatMember.MemberStatus()
	activeMember := status == telego.MemberStatusCreator || status == telego.MemberStatusAdministrator || status == telego.MemberStatusMember || status == telego.MemberStatusRestricted
	member := update.NewChatMember.MemberUser()
	return a.state.RecordMembership(ctx, update.Chat.ID, member.ID, status, time.Unix(update.Date, 0).UTC(), activeMember)
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
