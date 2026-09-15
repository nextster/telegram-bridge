package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Every row that belongs to a Telegram account carries owner_user_id (or
// account_id), the Telegram user ID of that account. Callers must always pass
// the authenticated user's ID; there is no global or administrator scope.

var ErrNotFound = errors.New("not found")

const (
	APITokenScopeMCP    = "mcp"
	APITokenScopeNotify = "notify"
)

type APIToken struct {
	ID          int64
	OwnerUserID int64
	Scope       string
	Name        string
	CreatedAt   time.Time
	LastUsedAt  time.Time
}

type NotificationChat struct {
	OwnerUserID int64
	ChatID      int64
	Title       string
	CreatedAt   time.Time
}

var tenancySchema = []string{
	`CREATE TABLE IF NOT EXISTS telegram_sessions (
		owner_user_id INTEGER PRIMARY KEY,
		data BLOB NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS api_tokens (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		token_hash TEXT NOT NULL UNIQUE,
		owner_user_id INTEGER NOT NULL,
		scope TEXT NOT NULL CHECK(scope IN ('mcp','notify')),
		name TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		last_used_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_api_tokens_owner ON api_tokens(owner_user_id, scope)`,
	`CREATE TABLE IF NOT EXISTS notification_chats (
		owner_user_id INTEGER NOT NULL,
		chat_id INTEGER NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		PRIMARY KEY(owner_user_id, chat_id)
	)`,
}

type tableRebuild struct {
	table  string
	column string
	create string
	copy   string
}

// Tables created before multi-user support lacked owner columns. Each is rebuilt
// once and its rows are assigned to the single account that used the service.
var legacyTenancyRebuilds = []tableRebuild{
	{
		table:  "keywords",
		column: "owner_user_id",
		create: `CREATE TABLE keywords_multiuser (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			owner_user_id INTEGER NOT NULL,
			phrase TEXT NOT NULL COLLATE NOCASE,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			UNIQUE(owner_user_id, phrase)
		)`,
		copy: `INSERT INTO keywords_multiuser(id, owner_user_id, phrase, enabled, created_at)
			SELECT id, ?, phrase, enabled, created_at FROM keywords`,
	},
	{
		table:  "events",
		column: "owner_user_id",
		create: `CREATE TABLE events_multiuser (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			owner_user_id INTEGER NOT NULL,
			source_peer_type TEXT NOT NULL,
			source_peer_id INTEGER NOT NULL,
			message_id INTEGER NOT NULL,
			message_date TEXT NOT NULL,
			text TEXT NOT NULL,
			keyword TEXT NOT NULL,
			created_at TEXT NOT NULL,
			UNIQUE(owner_user_id, source_peer_type, source_peer_id, message_id, keyword)
		)`,
		copy: `INSERT INTO events_multiuser(id, owner_user_id, source_peer_type, source_peer_id, message_id, message_date, text, keyword, created_at)
			SELECT id, ?, source_peer_type, source_peer_id, message_id, message_date, text, keyword, created_at FROM events`,
	},
	{
		table:  "monitor_peers",
		column: "owner_user_id",
		create: `CREATE TABLE monitor_peers_multiuser (
			owner_user_id INTEGER NOT NULL,
			peer_type TEXT NOT NULL,
			peer_id INTEGER NOT NULL,
			access_hash INTEGER NOT NULL DEFAULT 0,
			title TEXT NOT NULL DEFAULT '',
			username TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 0,
			discovered_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			last_backfill_at TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(owner_user_id, peer_type, peer_id)
		)`,
		copy: `INSERT INTO monitor_peers_multiuser(owner_user_id, peer_type, peer_id, access_hash, title, username, kind, enabled, discovered_at, updated_at, last_backfill_at)
			SELECT ?, peer_type, peer_id, access_hash, title, username, kind, enabled, discovered_at, updated_at, last_backfill_at FROM monitor_peers`,
	},
	{
		table:  "notification_receipts",
		column: "account_id",
		create: `CREATE TABLE notification_receipts_multiuser (
			account_id INTEGER NOT NULL,
			chat_id INTEGER NOT NULL,
			event_id TEXT NOT NULL,
			digest TEXT NOT NULL,
			status TEXT NOT NULL CHECK(status IN ('pending', 'sent')),
			message_id INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			PRIMARY KEY(account_id, chat_id, event_id)
		)`,
		copy: `INSERT INTO notification_receipts_multiuser(account_id, chat_id, event_id, digest, status, message_id, created_at)
			SELECT ?, chat_id, event_id, digest, status, message_id, created_at FROM notification_receipts`,
	},
}

// migrateLegacyTenancy rebuilds single-account tables on one connection with
// foreign keys disabled, so dropping a parent table does not cascade into its
// child rows (for example keyword terms and event match details).
func (s *Store) migrateLegacyTenancy(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("tenancy migration connection: %w", err)
	}
	defer conn.Close()

	var pending []tableRebuild
	for _, rebuild := range legacyTenancyRebuilds {
		exists, hasColumn, err := tableHasColumn(ctx, conn, rebuild.table, rebuild.column)
		if err != nil {
			return err
		}
		if exists && !hasColumn {
			pending = append(pending, rebuild)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	owner, err := legacyAccountID(ctx, conn)
	if err != nil {
		return err
	}

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for tenancy migration: %w", err)
	}
	defer conn.ExecContext(context.Background(), `PRAGMA foreign_keys = ON`)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tenancy migration: %w", err)
	}
	defer tx.Rollback()
	for _, rebuild := range pending {
		temporary := rebuild.table + "_multiuser"
		if _, err := tx.ExecContext(ctx, rebuild.create); err != nil {
			return fmt.Errorf("create %s: %w", temporary, err)
		}
		if _, err := tx.ExecContext(ctx, rebuild.copy, owner); err != nil {
			return fmt.Errorf("copy %s: %w", rebuild.table, err)
		}
		if _, err := tx.ExecContext(ctx, `DROP TABLE `+rebuild.table); err != nil {
			return fmt.Errorf("drop legacy %s: %w", rebuild.table, err)
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE `+temporary+` RENAME TO `+rebuild.table); err != nil {
			return fmt.Errorf("rename %s: %w", temporary, err)
		}
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check foreign keys after tenancy migration: %w", err)
	}
	violation := rows.Next()
	if err := rows.Close(); err != nil {
		return err
	}
	if violation {
		return errors.New("tenancy migration would break foreign keys")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tenancy migration: %w", err)
	}
	return nil
}

func tableHasColumn(ctx context.Context, conn *sql.Conn, table, column string) (bool, bool, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, false, fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()
	exists := false
	for rows.Next() {
		exists = true
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, false, err
		}
		if strings.EqualFold(name, column) {
			return true, true, rows.Err()
		}
	}
	return exists, false, rows.Err()
}

// legacyAccountID finds the one Telegram account of a single-user database.
// Rows stay unowned (0, visible to nobody) when no account ever logged in.
func legacyAccountID(ctx context.Context, conn *sql.Conn) (int64, error) {
	queries := []string{
		`SELECT user_id FROM update_state ORDER BY updated_at DESC LIMIT 1`,
		`SELECT owner_user_id FROM private_dialogs ORDER BY updated_at DESC LIMIT 1`,
		`SELECT account_id FROM media_jobs ORDER BY updated_at DESC LIMIT 1`,
	}
	for _, query := range queries {
		var id int64
		err := conn.QueryRowContext(ctx, query).Scan(&id)
		if err == nil && id > 0 {
			return id, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) && !strings.Contains(err.Error(), "no such table") {
			return 0, fmt.Errorf("find legacy account: %w", err)
		}
	}
	return 0, nil
}

// SaveTelegramSession stores an already encrypted session blob.
func (s *Store) SaveTelegramSession(ctx context.Context, ownerUserID int64, data []byte) error {
	if ownerUserID <= 0 {
		return errors.New("session owner is required")
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO telegram_sessions(owner_user_id, data, created_at, updated_at) VALUES(?, ?, ?, ?)
		ON CONFLICT(owner_user_id) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at
	`, ownerUserID, data, now, now)
	if err != nil {
		return fmt.Errorf("save telegram session: %w", err)
	}
	return nil
}

func (s *Store) LoadTelegramSession(ctx context.Context, ownerUserID int64) ([]byte, bool, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM telegram_sessions WHERE owner_user_id = ?`, ownerUserID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load telegram session: %w", err)
	}
	return data, true, nil
}

func (s *Store) DeleteTelegramSession(ctx context.Context, ownerUserID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM telegram_sessions WHERE owner_user_id = ?`, ownerUserID); err != nil {
		return fmt.Errorf("delete telegram session: %w", err)
	}
	return nil
}

func (s *Store) ListTelegramSessionOwners(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT owner_user_id FROM telegram_sessions ORDER BY created_at, owner_user_id`)
	if err != nil {
		return nil, fmt.Errorf("list telegram sessions: %w", err)
	}
	defer rows.Close()
	var owners []int64
	for rows.Next() {
		var owner int64
		if err := rows.Scan(&owner); err != nil {
			return nil, err
		}
		owners = append(owners, owner)
	}
	return owners, rows.Err()
}

// CreateAPIToken stores the hash of a new personal token.
func (s *Store) CreateAPIToken(ctx context.Context, ownerUserID int64, scope, name, tokenHash string, now time.Time) (APIToken, error) {
	if ownerUserID <= 0 || tokenHash == "" {
		return APIToken{}, errors.New("token owner and hash are required")
	}
	if scope != APITokenScopeMCP && scope != APITokenScopeNotify {
		return APIToken{}, fmt.Errorf("unknown token scope %q", scope)
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO api_tokens(token_hash, owner_user_id, scope, name, created_at) VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(token_hash) DO NOTHING
	`, tokenHash, ownerUserID, scope, strings.TrimSpace(name), now.Unix())
	if err != nil {
		return APIToken{}, fmt.Errorf("create api token: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return APIToken{}, errors.New("api token already exists")
	}
	id, err := result.LastInsertId()
	if err != nil {
		return APIToken{}, err
	}
	return APIToken{ID: id, OwnerUserID: ownerUserID, Scope: scope, Name: strings.TrimSpace(name), CreatedAt: now.UTC()}, nil
}

// ResolveAPIToken returns the owner of a token hash valid for scope.
func (s *Store) ResolveAPIToken(ctx context.Context, tokenHash, scope string, now time.Time) (int64, bool, error) {
	var owner, lastUsed int64
	err := s.db.QueryRowContext(ctx, `
		SELECT owner_user_id, last_used_at FROM api_tokens WHERE token_hash = ? AND scope = ?
	`, tokenHash, scope).Scan(&owner, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("resolve api token: %w", err)
	}
	if now.Unix()-lastUsed >= 60 {
		if _, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE token_hash = ?`, now.Unix(), tokenHash); err != nil {
			return 0, false, fmt.Errorf("touch api token: %w", err)
		}
	}
	return owner, owner > 0, nil
}

func (s *Store) ListAPITokens(ctx context.Context, ownerUserID int64) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, owner_user_id, scope, name, created_at, last_used_at
		FROM api_tokens WHERE owner_user_id = ? ORDER BY created_at DESC, id DESC
	`, ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}
	defer rows.Close()
	var tokens []APIToken
	for rows.Next() {
		var token APIToken
		var createdAt, lastUsed int64
		if err := rows.Scan(&token.ID, &token.OwnerUserID, &token.Scope, &token.Name, &createdAt, &lastUsed); err != nil {
			return nil, err
		}
		token.CreatedAt = time.Unix(createdAt, 0).UTC()
		if lastUsed > 0 {
			token.LastUsedAt = time.Unix(lastUsed, 0).UTC()
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

// DeleteAPIToken removes one of the owner's tokens and reports ErrNotFound for
// tokens that do not exist or belong to someone else.
func (s *Store) DeleteAPIToken(ctx context.Context, ownerUserID, id int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ? AND owner_user_id = ?`, id, ownerUserID)
	if err != nil {
		return fmt.Errorf("delete api token: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteAPITokensForUser(ctx context.Context, ownerUserID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE owner_user_id = ?`, ownerUserID); err != nil {
		return fmt.Errorf("delete user api tokens: %w", err)
	}
	return nil
}

func (s *Store) AddNotificationChat(ctx context.Context, ownerUserID, chatID int64, title string, now time.Time) error {
	if ownerUserID <= 0 || chatID == 0 {
		return errors.New("notification chat owner and id are required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notification_chats(owner_user_id, chat_id, title, created_at) VALUES(?, ?, ?, ?)
		ON CONFLICT(owner_user_id, chat_id) DO UPDATE SET title = excluded.title
	`, ownerUserID, chatID, strings.TrimSpace(title), now.Unix())
	if err != nil {
		return fmt.Errorf("add notification chat: %w", err)
	}
	return nil
}

func (s *Store) RemoveNotificationChat(ctx context.Context, ownerUserID, chatID int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM notification_chats WHERE owner_user_id = ? AND chat_id = ?`, ownerUserID, chatID)
	if err != nil {
		return fmt.Errorf("remove notification chat: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListNotificationChats(ctx context.Context, ownerUserID int64) ([]NotificationChat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT owner_user_id, chat_id, title, created_at FROM notification_chats
		WHERE owner_user_id = ? ORDER BY created_at, chat_id
	`, ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("list notification chats: %w", err)
	}
	defer rows.Close()
	var chats []NotificationChat
	for rows.Next() {
		var chat NotificationChat
		var createdAt int64
		if err := rows.Scan(&chat.OwnerUserID, &chat.ChatID, &chat.Title, &createdAt); err != nil {
			return nil, err
		}
		chat.CreatedAt = time.Unix(createdAt, 0).UTC()
		chats = append(chats, chat)
	}
	return chats, rows.Err()
}

func (s *Store) IsNotificationChatAllowed(ctx context.Context, ownerUserID, chatID int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM notification_chats WHERE owner_user_id = ? AND chat_id = ?`, ownerUserID, chatID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check notification chat: %w", err)
	}
	return true, nil
}

func (s *Store) IsSubscribed(ctx context.Context, userID int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM subscribers WHERE chat_id = ?`, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check subscriber: %w", err)
	}
	return true, nil
}
