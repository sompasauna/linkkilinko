// Package store persists membership, idempotency, and moderation action state.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // register the SQLite database/sql driver
)

// outboxLeaseDuration bounds how long a claimed outbox entry is hidden from
// other callers before it is treated as abandoned and reclaimable.
const outboxLeaseDuration = time.Minute

// canonicalActionTTL bounds how long a moderation response suppresses an
// otherwise identical repost. Update idempotency remains durable; this only
// limits the repost-suppression window.
const canonicalActionTTL = 4 * time.Hour
const knownGoodPreviewTTL = 30 * 24 * time.Hour

var neverTrustedPreviewDomains = []string{"facebook.com", "fb.com", "share.google", "goo.gl"}

// Membership is the latest observed status for a chat member.
type Membership struct {
	ChatID        int64
	UserID        int64
	JoinedAt      time.Time
	Status        string
	Grandfathered bool
}

// CanonicalAction is the first visible action for a material violation.
type CanonicalAction struct {
	ID                int64
	ChatID            int64
	ThreadID          int
	UserID            int64
	Rule              string
	BehaviorVersion   string
	Fingerprint       string
	ResponseMessageID int
	ResponseState     string
	Payload           string
	SourceMessageID   int
}

// OutboxEntry is a durable moderation side-effect plan.
type OutboxEntry struct {
	ID                int64
	CanonicalActionID int64
	ChatID            int64
	ThreadID          int
	SourceMessageID   int
	Payload           string
	State             string
	Attempts          int
	NextAttemptAt     time.Time
	ResponseMessageID int
	LeaseUntil        time.Time
}

// Store is a concurrency-safe database/sql handle for linkkilinko state.
type Store struct {
	db *sql.DB
}

// KnownGoodPreviewDomain reports whether a host has recently produced useful
// preview metadata. Hosts are matched exactly; a trusted parent never trusts
// an unrelated subdomain.
func (s *Store) KnownGoodPreviewDomain(ctx context.Context, host string) (bool, error) {
	host = normalizePreviewDomain(host)
	if host == "" || neverTrustsPreviewDomain(host) {
		return false, nil
	}
	var exists int
	checkedAfter := time.Now().Add(-knownGoodPreviewTTL).Unix()
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM known_good_preview_domains
		WHERE host = ? AND observed_at >= ?`, host, checkedAfter).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: check known preview domain: %w", err)
	}
	return exists == 1, nil
}

// RecordKnownGoodPreviewDomain stores a host after a successful useful
// metadata probe. Tracking exclusions can never enter this table.
func (s *Store) RecordKnownGoodPreviewDomain(ctx context.Context, host string) error {
	host = normalizePreviewDomain(host)
	if host == "" || neverTrustsPreviewDomain(host) {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO known_good_preview_domains(host, observed_at) VALUES (?, ?)
		ON CONFLICT(host) DO UPDATE SET observed_at = excluded.observed_at`, host, time.Now().Unix()); err != nil {
		return fmt.Errorf("store: record known preview domain: %w", err)
	}
	return nil
}

func normalizePreviewDomain(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

func neverTrustsPreviewDomain(host string) bool {
	for _, excluded := range neverTrustedPreviewDomains {
		if host == excluded || strings.HasSuffix(host, "."+excluded) {
			return true
		}
	}
	return false
}

// RegisterOwner stores the first bootstrap owner and reports whether this call
// registered it. Existing owners are never replaced.
func (s *Store) RegisterOwner(ctx context.Context, userID int64) (bool, error) {
	if userID <= 0 {
		return false, errors.New("store: owner user id must be positive")
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO bot_owner(id, user_id, registered_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO NOTHING`, userID, time.Now().Unix())
	if err != nil {
		return false, fmt.Errorf("store: register owner: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: register owner rows: %w", err)
	}
	return rows == 1, nil
}

// Owner returns the durably registered bootstrap owner.
func (s *Store) Owner(ctx context.Context) (int64, bool, error) {
	var userID int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id FROM bot_owner WHERE id = 1`).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: read owner: %w", err)
	}
	return userID, true, nil
}

// ResetOwner clears the persisted bootstrap owner so the next private
// /start registers a fresh one. Approved chat rows are not affected: an
// operator who resets an owner to recover from a wrong claim keeps every
// group the bot was already trusted to moderate. The returned bool is true
// only when an owner row actually existed and was removed.
func (s *Store) ResetOwner(ctx context.Context) (int64, bool, error) {
	previous, found, err := s.Owner(ctx)
	if err != nil {
		return 0, false, err
	}
	if !found {
		return 0, false, nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM bot_owner WHERE id = 1`); err != nil {
		return previous, false, fmt.Errorf("store: reset owner: %w", err)
	}
	return previous, true, nil
}

// ApproveChat records a group approved by the bootstrap owner.
func (s *Store) ApproveChat(ctx context.Context, chatID, approvedBy int64) error {
	if chatID == 0 || approvedBy <= 0 {
		return errors.New("store: invalid approved chat or owner")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO approved_chats(chat_id, approved_by, approved_at) VALUES (?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET approved_by = excluded.approved_by,
		approved_at = excluded.approved_at`, chatID, approvedBy, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("store: approve chat: %w", err)
	}
	return nil
}

// ApprovedChat reports whether a group has been durably approved.
func (s *Store) ApprovedChat(ctx context.Context, chatID int64) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM approved_chats WHERE chat_id = ?`, chatID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: check approved chat: %w", err)
	}
	return exists == 1, nil
}

// ApprovedChatCount returns the number of durably approved groups.
func (s *Store) ApprovedChatCount(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approved_chats`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: count approved chats: %w", err)
	}
	return count, nil
}

// Open opens a SQLite database and applies the current schema transactionally.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: database path is empty")
	}
	if !strings.Contains(path, ":") {
		directory := filepath.Dir(path)
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return nil, fmt.Errorf("store: create database directory: %w", err)
		}
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	database.SetMaxOpenConns(1)
	store := &Store{db: database}
	if _, err := database.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("store: enable foreign keys: %w", err)
	}
	if err := store.migrate(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	return store, nil
}

// Close releases the database connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Ping verifies that the SQLite handle is usable.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("store: database is nil")
	}
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("store: ping database: %w", err)
	}
	return nil
}

// OutboxBacklog returns the number of moderation actions awaiting completion.
func (s *Store) OutboxBacklog(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM moderation_outbox WHERE state <> 'complete'`).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count outbox backlog: %w", err)
	}
	return count, nil
}

// ClaimUpdate records one Telegram message version and reports whether it was
// newly claimed.
func (s *Store) ClaimUpdate(ctx context.Context, chatID int64, messageID int, editDate int64) (bool, error) {
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO processed_updates(chat_id, message_id, edit_date, state, updated_at)
		VALUES (?, ?, ?, 'processing', ?)
		ON CONFLICT(chat_id, message_id, edit_date) DO UPDATE SET
			state = 'processing', updated_at = excluded.updated_at
		WHERE processed_updates.state NOT IN ('complete', 'failed')
		  AND processed_updates.updated_at < excluded.updated_at - 300`, chatID, messageID, editDate, now)
	if err != nil {
		return false, fmt.Errorf("store: claim update: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: claim update rows: %w", err)
	}
	return rows == 1, nil
}

// CompleteUpdate marks a successfully handled Telegram message version.
func (s *Store) CompleteUpdate(ctx context.Context, chatID int64, messageID int, editDate int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE processed_updates SET state = 'complete', updated_at = ?
		WHERE chat_id = ? AND message_id = ? AND edit_date = ?`, time.Now().Unix(), chatID, messageID, editDate)
	if err != nil {
		return fmt.Errorf("store: complete update: %w", err)
	}
	return nil
}

// FailUpdate marks a claimed update terminal and preserves an operator-facing error.
func (s *Store) FailUpdate(ctx context.Context, chatID int64, messageID int, editDate int64, operationErr error) error {
	message := "update failed"
	if operationErr != nil {
		message = operationErr.Error()
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE processed_updates SET state = 'failed', decision = ?, updated_at = ?
		WHERE chat_id = ? AND message_id = ? AND edit_date = ?`, message, time.Now().Unix(), chatID, messageID, editDate)
	if err != nil {
		return fmt.Errorf("store: fail update: %w", err)
	}
	return nil
}

// RecordMembership stores an observed member transition. Active status changes
// preserve the first join timestamp for the current continuous membership.
func (s *Store) RecordMembership(ctx context.Context, chatID, userID int64, status string, eventAt time.Time, active bool) error {
	if active {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO memberships(chat_id, user_id, joined_at, status, observed_at, grandfathered)
			VALUES (?, ?, ?, ?, ?, 0)
			ON CONFLICT(chat_id, user_id) DO UPDATE SET
				joined_at = CASE
					WHEN memberships.status IN ('left', 'kicked') THEN excluded.joined_at
					ELSE memberships.joined_at
				END,
				status = excluded.status,
				observed_at = excluded.observed_at,
				grandfathered = CASE WHEN memberships.status IN ('left', 'kicked') THEN 0 ELSE memberships.grandfathered END`, chatID, userID, eventAt.Unix(), status, time.Now().Unix())
		if err != nil {
			return fmt.Errorf("store: record active membership: %w", err)
		}
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memberships(chat_id, user_id, joined_at, status, observed_at, grandfathered)
		VALUES (?, ?, 0, ?, ?, 0)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET status = excluded.status, observed_at = excluded.observed_at`, chatID, userID, status, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("store: record inactive membership: %w", err)
	}
	return nil
}

// Grandfather records that a member was first observed without a join event.
func (s *Store) Grandfather(ctx context.Context, chatID, userID int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memberships(chat_id, user_id, joined_at, status, observed_at, grandfathered)
		VALUES (?, ?, 0, 'member', ?, 1)
		ON CONFLICT(chat_id, user_id) DO NOTHING`, chatID, userID, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("store: grandfather member: %w", err)
	}
	return nil
}

// Membership returns the latest known membership, or (false, nil) when none
// has been observed.
func (s *Store) Membership(ctx context.Context, chatID, userID int64) (Membership, bool, error) {
	var membership Membership
	var joinedAt, grandfathered int64
	err := s.db.QueryRowContext(ctx, `
		SELECT chat_id, user_id, joined_at, status, grandfathered
		FROM memberships WHERE chat_id = ? AND user_id = ?`, chatID, userID).Scan(
		&membership.ChatID, &membership.UserID, &joinedAt, &membership.Status, &grandfathered)
	if errors.Is(err, sql.ErrNoRows) {
		return Membership{}, false, nil
	}
	if err != nil {
		return Membership{}, false, fmt.Errorf("store: read membership: %w", err)
	}
	if joinedAt > 0 {
		membership.JoinedAt = time.Unix(joinedAt, 0).UTC()
	}
	membership.Grandfathered = grandfathered != 0
	return membership, true, nil
}

// FindCanonical looks up an active canonical action for a normalized repost.
func (s *Store) FindCanonical(ctx context.Context, chatID int64, threadID int, userID int64, rule, behaviorVersion, fingerprint string) (CanonicalAction, bool, error) {
	var action CanonicalAction
	createdAfter := time.Now().Add(-canonicalActionTTL).Unix()
	err := s.db.QueryRowContext(ctx, `
		SELECT id, chat_id, thread_id, user_id, rule, behavior_version, fingerprint,
		       response_message_id, response_state, payload
		FROM canonical_actions
		WHERE chat_id = ? AND thread_id = ? AND user_id = ? AND rule = ?
		  AND behavior_version = ? AND fingerprint = ? AND active = 1
		  AND created_at >= ?`,
		chatID, threadID, userID, rule, behaviorVersion, fingerprint, createdAfter).Scan(
		&action.ID, &action.ChatID, &action.ThreadID, &action.UserID, &action.Rule,
		&action.BehaviorVersion, &action.Fingerprint, &action.ResponseMessageID,
		&action.ResponseState, &action.Payload)
	if errors.Is(err, sql.ErrNoRows) {
		return CanonicalAction{}, false, nil
	}
	if err != nil {
		return CanonicalAction{}, false, fmt.Errorf("store: find canonical action: %w", err)
	}
	return action, true, nil
}

// CreateCanonical persists a visible action plan and reports whether it won the
// unique canonical-action race.
func (s *Store) CreateCanonical(ctx context.Context, action CanonicalAction) (CanonicalAction, bool, error) {
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CanonicalAction{}, false, fmt.Errorf("store: begin canonical action: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	createdAt := time.Now().Unix()
	insert := func() (sql.Result, error) {
		return transaction.ExecContext(ctx, `
		INSERT INTO canonical_actions(
			chat_id, thread_id, user_id, rule, behavior_version, fingerprint,
			response_message_id, response_state, payload, active, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, 'pending', ?, 1, ?) 
		ON CONFLICT(chat_id, thread_id, user_id, rule, behavior_version, fingerprint) DO NOTHING`,
			action.ChatID, action.ThreadID, action.UserID, action.Rule, action.BehaviorVersion,
			action.Fingerprint, action.Payload, createdAt)
	}
	result, err := insert()
	if err != nil {
		return CanonicalAction{}, false, fmt.Errorf("store: create canonical action: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return CanonicalAction{}, false, fmt.Errorf("store: create canonical rows: %w", err)
	}
	if rows == 0 {
		var existingID, existingCreatedAt int64
		if err := transaction.QueryRowContext(ctx, `
			SELECT id, created_at FROM canonical_actions
			WHERE chat_id = ? AND thread_id = ? AND user_id = ? AND rule = ?
			  AND behavior_version = ? AND fingerprint = ?`,
			action.ChatID, action.ThreadID, action.UserID, action.Rule,
			action.BehaviorVersion, action.Fingerprint).Scan(&existingID, &existingCreatedAt); err != nil {
			return CanonicalAction{}, false, fmt.Errorf("store: read conflicting canonical action: %w", err)
		}
		if existingCreatedAt < createdAt-int64(canonicalActionTTL/time.Second) {
			if _, err := transaction.ExecContext(ctx, `
				UPDATE canonical_actions
				SET active = 0, fingerprint = fingerprint || ':expired:' || id
				WHERE id = ?`, existingID); err != nil {
				return CanonicalAction{}, false, fmt.Errorf("store: expire canonical action: %w", err)
			}
			result, err = insert()
			if err != nil {
				return CanonicalAction{}, false, fmt.Errorf("store: recreate canonical action: %w", err)
			}
			rows, err = result.RowsAffected()
			if err != nil {
				return CanonicalAction{}, false, fmt.Errorf("store: recreate canonical rows: %w", err)
			}
		}
		if rows == 0 {
			_ = transaction.Rollback()
			existing, _, findErr := s.FindCanonical(ctx, action.ChatID, action.ThreadID, action.UserID, action.Rule, action.BehaviorVersion, action.Fingerprint)
			return existing, false, findErr
		}
	}
	var created CanonicalAction
	if err := transaction.QueryRowContext(ctx, `
		SELECT id, chat_id, thread_id, user_id, rule, behavior_version, fingerprint,
		       response_message_id, response_state, payload
		FROM canonical_actions WHERE chat_id = ? AND thread_id = ? AND user_id = ? AND rule = ?
		  AND behavior_version = ? AND fingerprint = ? AND active = 1`,
		action.ChatID, action.ThreadID, action.UserID, action.Rule, action.BehaviorVersion, action.Fingerprint).Scan(
		&created.ID, &created.ChatID, &created.ThreadID, &created.UserID, &created.Rule,
		&created.BehaviorVersion, &created.Fingerprint, &created.ResponseMessageID,
		&created.ResponseState, &created.Payload); err != nil {
		return CanonicalAction{}, false, fmt.Errorf("store: read created canonical action: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO moderation_outbox(canonical_action_id, chat_id, thread_id, source_message_id, payload, state, attempts, next_attempt_at, created_at)
		VALUES (?, ?, ?, ?, ?, 'planned', 0, ?, ?)`, created.ID, created.ChatID, created.ThreadID, action.SourceMessageID, created.Payload, time.Now().Unix(), time.Now().Unix()); err != nil {
		return CanonicalAction{}, false, fmt.Errorf("store: create moderation outbox: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return CanonicalAction{}, false, fmt.Errorf("store: commit canonical action: %w", err)
	}
	return created, true, nil
}

// AttachSourceMessage records the source message associated with a newly planned action.
func (s *Store) AttachSourceMessage(ctx context.Context, actionID int64, messageID int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE moderation_outbox SET source_message_id = ? WHERE canonical_action_id = ?`, messageID, actionID)
	if err != nil {
		return fmt.Errorf("store: attach source message: %w", err)
	}
	return nil
}

// ReplaceOutboxPayload changes the retry payload before any Telegram send.
func (s *Store) ReplaceOutboxPayload(ctx context.Context, actionID int64, payload string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE moderation_outbox SET payload = ? WHERE canonical_action_id = ?`, payload, actionID)
	if err != nil {
		return fmt.Errorf("store: replace outbox payload: %w", err)
	}
	return nil
}

// MarkOutboxCopied records a copied media response that still needs source deletion.
func (s *Store) MarkOutboxCopied(ctx context.Context, actionID int64, responseMessageID int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE moderation_outbox SET state = 'delete_requested', response_message_id = ?, next_attempt_at = ?
		WHERE canonical_action_id = ?`, responseMessageID, time.Now().Unix(), actionID)
	if err != nil {
		return fmt.Errorf("store: mark copied outbox: %w", err)
	}
	return nil
}

func (s *Store) updateOutbox(ctx context.Context, actionID int64, state string, errText string) error {
	backoff, err := s.outboxBackoff(ctx, actionID)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE moderation_outbox SET state = ?, attempts = attempts + 1, last_error = ?, next_attempt_at = ?
		WHERE canonical_action_id = ?`, state, errText, time.Now().Add(backoff).Unix(), actionID)
	if err != nil {
		return fmt.Errorf("store: update outbox: %w", err)
	}
	return nil
}

// outboxBackoff computes the next exponential backoff from the action's
// current attempt count.
func (s *Store) outboxBackoff(ctx context.Context, actionID int64) (time.Duration, error) {
	var attempts int
	if err := s.db.QueryRowContext(ctx, `SELECT attempts FROM moderation_outbox WHERE canonical_action_id = ?`, actionID).Scan(&attempts); err != nil {
		return 0, fmt.Errorf("store: read outbox attempts: %w", err)
	}
	return time.Second << min(attempts, 10), nil
}

// MarkDeleteRequested records that deletion has started.
func (s *Store) MarkDeleteRequested(ctx context.Context, actionID int64) error {
	return s.updateOutbox(ctx, actionID, "delete_requested", "")
}

// MarkSendPending records that the source was deleted and the response remains to be sent.
func (s *Store) MarkSendPending(ctx context.Context, actionID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE moderation_outbox SET state = 'send_pending', next_attempt_at = ? WHERE canonical_action_id = ?`, time.Now().Unix(), actionID)
	if err != nil {
		return fmt.Errorf("store: mark send pending: %w", err)
	}
	return nil
}

// PendingOutbox returns due actions that need Telegram side effects. The
// claim uses a freshly generated random token rather than a value derived
// from now: two callers racing within the same wall-clock second must not
// compute the same claim value and both see the rows the other just took.
func (s *Store) PendingOutbox(ctx context.Context, now time.Time) ([]OutboxEntry, error) {
	token, err := newLeaseToken()
	if err != nil {
		return nil, fmt.Errorf("store: generate lease token: %w", err)
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin claim outbox: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	leaseUntil := now.Add(outboxLeaseDuration).Unix()
	if _, err := transaction.ExecContext(ctx, `
		UPDATE moderation_outbox SET lease_until = ?, lease_token = ?
		WHERE state NOT IN ('complete', 'dead') AND next_attempt_at <= ? AND lease_until <= ?`, leaseUntil, token, now.Unix(), now.Unix()); err != nil {
		return nil, fmt.Errorf("store: claim outbox: %w", err)
	}
	rows, err := transaction.QueryContext(ctx, `
		SELECT id, canonical_action_id, chat_id, thread_id, source_message_id, payload, state, attempts, next_attempt_at, response_message_id, lease_until
		FROM moderation_outbox WHERE lease_token = ? AND state NOT IN ('complete', 'dead') ORDER BY id`, token)
	if err != nil {
		return nil, fmt.Errorf("store: list claimed outbox: %w", err)
	}
	defer rows.Close()
	var entries []OutboxEntry
	for rows.Next() {
		var entry OutboxEntry
		var next int64
		var lease int64
		if err := rows.Scan(&entry.ID, &entry.CanonicalActionID, &entry.ChatID, &entry.ThreadID, &entry.SourceMessageID, &entry.Payload, &entry.State, &entry.Attempts, &next, &entry.ResponseMessageID, &lease); err != nil {
			return nil, fmt.Errorf("store: scan outbox: %w", err)
		}
		entry.NextAttemptAt = time.Unix(next, 0).UTC()
		entry.LeaseUntil = time.Unix(lease, 0).UTC()
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list outbox rows: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit outbox claim: %w", err)
	}
	return entries, nil
}

// ReleaseOutboxLease makes an unfinished action eligible for another worker.
func (s *Store) ReleaseOutboxLease(ctx context.Context, actionID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE moderation_outbox SET lease_until = 0, lease_token = '' WHERE canonical_action_id = ?`, actionID)
	if err != nil {
		return fmt.Errorf("store: release outbox lease: %w", err)
	}
	return nil
}

// newLeaseToken returns a random claim token. It must never be derived from
// the current time: a value derived from now lets two callers racing within
// the same wall-clock second compute an identical token and both observe the
// rows the other just claimed.
func newLeaseToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("store: read random lease token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// MarkOutboxComplete records a successful response and closes the canonical action.
func (s *Store) MarkOutboxComplete(ctx context.Context, actionID int64, responseMessageID int) error {
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin complete outbox: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, `UPDATE moderation_outbox SET state = 'complete', response_message_id = ?, last_error = '', lease_until = 0 WHERE canonical_action_id = ?`, responseMessageID, actionID); err != nil {
		return fmt.Errorf("store: complete outbox: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE canonical_actions SET response_message_id = ?, response_state = 'sent' WHERE id = ?`, responseMessageID, actionID); err != nil {
		return fmt.Errorf("store: complete canonical action: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("store: commit complete outbox: %w", err)
	}
	return nil
}

// MarkOutboxError leaves an action retryable and records a bounded diagnostic.
func (s *Store) MarkOutboxError(ctx context.Context, actionID int64, state string, operationErr error) error {
	return s.markOutboxError(ctx, actionID, state, operationErr, 0)
}

// MarkOutboxErrorAfter leaves an action retryable after an externally requested delay.
func (s *Store) MarkOutboxErrorAfter(ctx context.Context, actionID int64, state string, operationErr error, delay time.Duration) error {
	return s.markOutboxError(ctx, actionID, state, operationErr, delay)
}

// markOutboxError leaves an action retryable at next_attempt_at and releases
// its lease in the same statement as the state write, so a retry due sooner
// than the lease's natural expiry is not blocked behind a stale claim.
func (s *Store) markOutboxError(ctx context.Context, actionID int64, state string, operationErr error, delay time.Duration) error {
	message := ""
	if operationErr != nil {
		message = operationErr.Error()
		if len(message) > 512 {
			message = message[:512]
		}
	}
	if delay <= 0 {
		backoff, err := s.outboxBackoff(ctx, actionID)
		if err != nil {
			return err
		}
		delay = backoff
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE moderation_outbox SET state = ?, attempts = attempts + 1, last_error = ?, next_attempt_at = ?, lease_until = 0, lease_token = ''
		WHERE canonical_action_id = ?`, state, message, time.Now().Add(delay).Unix(), actionID)
	if err != nil {
		return fmt.Errorf("store: update delayed outbox: %w", err)
	}
	return nil
}

// MarkOutboxDead records an action that cannot be completed automatically.
func (s *Store) MarkOutboxDead(ctx context.Context, actionID int64, operationErr error) error {
	message := ""
	if operationErr != nil {
		message = operationErr.Error()
		if len(message) > 512 {
			message = message[:512]
		}
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin dead-letter outbox: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, `
		UPDATE moderation_outbox SET state = 'dead', attempts = attempts + 1, last_error = ?, next_attempt_at = ?, lease_until = 0
		WHERE canonical_action_id = ?`, message, time.Now().Unix(), actionID); err != nil {
		return fmt.Errorf("store: dead-letter outbox: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE canonical_actions SET response_state = 'dead' WHERE id = ?`, actionID); err != nil {
		return fmt.Errorf("store: mark canonical action dead: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("store: commit dead-letter outbox: %w", err)
	}
	return nil
}

// MarkResponseSent records the Telegram message id for a canonical response.
func (s *Store) MarkResponseSent(ctx context.Context, actionID int64, messageID int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE canonical_actions SET response_message_id = ?, response_state = 'sent'
		WHERE id = ?`, messageID, actionID)
	if err != nil {
		return fmt.Errorf("store: mark response sent: %w", err)
	}
	return nil
}

// RecordSuppressed records a silently deleted repost for auditing.
func (s *Store) RecordSuppressed(ctx context.Context, actionID int64, messageID int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO suppressed_reposts(canonical_action_id, source_message_id, delete_state, observed_at)
		VALUES (?, ?, 'deleted', ?)`, actionID, messageID, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("store: record suppressed repost: %w", err)
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS bot_owner (
			id INTEGER PRIMARY KEY CHECK(id = 1),
			user_id INTEGER NOT NULL UNIQUE,
			registered_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS approved_chats (
			chat_id INTEGER PRIMARY KEY,
			approved_by INTEGER NOT NULL,
			approved_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS known_good_preview_domains (
			host TEXT PRIMARY KEY,
			observed_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS memberships (
			chat_id INTEGER NOT NULL,
			user_id INTEGER NOT NULL,
			joined_at INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL,
			observed_at INTEGER NOT NULL,
			grandfathered INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(chat_id, user_id)
		)`,
		`CREATE TABLE IF NOT EXISTS processed_updates (
			chat_id INTEGER NOT NULL,
			message_id INTEGER NOT NULL,
			edit_date INTEGER NOT NULL,
			state TEXT NOT NULL,
			updated_at INTEGER NOT NULL,
			decision TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(chat_id, message_id, edit_date)
		)`,
		`CREATE TABLE IF NOT EXISTS canonical_actions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id INTEGER NOT NULL,
			thread_id INTEGER NOT NULL,
			user_id INTEGER NOT NULL,
			rule TEXT NOT NULL,
			behavior_version TEXT NOT NULL,
			fingerprint TEXT NOT NULL,
			response_message_id INTEGER NOT NULL DEFAULT 0,
			response_state TEXT NOT NULL,
			payload TEXT NOT NULL,
			active INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			UNIQUE(chat_id, thread_id, user_id, rule, behavior_version, fingerprint)
		)`,
		`CREATE TABLE IF NOT EXISTS suppressed_reposts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			canonical_action_id INTEGER NOT NULL,
			source_message_id INTEGER NOT NULL,
			delete_state TEXT NOT NULL,
			observed_at INTEGER NOT NULL,
			FOREIGN KEY(canonical_action_id) REFERENCES canonical_actions(id)
		)`,
		`CREATE TABLE IF NOT EXISTS moderation_outbox (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			canonical_action_id INTEGER NOT NULL UNIQUE,
			chat_id INTEGER NOT NULL,
			thread_id INTEGER NOT NULL,
			source_message_id INTEGER NOT NULL DEFAULT 0,
			payload TEXT NOT NULL,
			state TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt_at INTEGER NOT NULL,
			last_error TEXT NOT NULL DEFAULT '',
			response_message_id INTEGER NOT NULL DEFAULT 0,
			lease_until INTEGER NOT NULL DEFAULT 0,
			lease_token TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			FOREIGN KEY(canonical_action_id) REFERENCES canonical_actions(id)
		)`,
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	for _, statement := range statements {
		if _, err := transaction.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("store: migrate schema: %w", err)
		}
	}
	// Add columns introduced after the initial schema without requiring a
	// destructive migration for an existing installation.
	for _, statement := range []string{
		`ALTER TABLE processed_updates ADD COLUMN decision TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE moderation_outbox ADD COLUMN lease_until INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE moderation_outbox ADD COLUMN lease_token TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := transaction.ExecContext(ctx, statement); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return fmt.Errorf("store: migrate added columns: %w", err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("store: commit migration: %w", err)
	}
	return nil
}
