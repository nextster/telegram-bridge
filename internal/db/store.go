package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/telegram/updates"
	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Subscriber struct {
	ChatID     int64
	Username   string
	FirstName  string
	LastName   string
	CreatedAt  time.Time
	LastSeenAt time.Time
}

type Keyword struct {
	ID                  int64
	Phrase              string
	Enabled             bool
	AnyTerms            []string
	AllTerms            []string
	RequiredAnyGroups   [][]string
	PreferredTerms      []string
	ExcludeTerms        []string
	Note                string
	ExcludeCompleteBike bool
	Sources             []RuleSource
	CreatedAt           time.Time
}

type RuleSource struct {
	PeerType string
	PeerID   int64
}

type Event struct {
	ID             int64
	SourcePeerType string
	SourcePeerID   int64
	MessageID      int
	MessageDate    time.Time
	Text           string
	Keyword        string
	RuleID         int64
	MatchReason    string
	MatchScore     int
	RuleNote       string
	CreatedAt      time.Time
}

type MonitorPeer struct {
	PeerType       string
	PeerID         int64
	AccessHash     int64
	Title          string
	Username       string
	Kind           string
	Enabled        bool
	DiscoveredAt   time.Time
	UpdatedAt      time.Time
	LastBackfillAt time.Time
}

type Stats struct {
	Subscribers  int
	Keywords     int
	Events       int
	Peers        int
	EnabledPeers int
}

type LoginToken struct {
	Token     string
	ChatID    int64
	Phone     string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    time.Time
}

func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	dsn := &url.URL{Scheme: "file", Path: path}
	query := dsn.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "foreign_keys(1)")
	dsn.RawQuery = query.Encode()
	handle, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	handle.SetMaxOpenConns(4)
	handle.SetMaxIdleConns(4)

	store := &Store{db: handle}
	if err := store.pingAndMigrate(ctx); err != nil {
		_ = handle.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) pingAndMigrate(ctx context.Context) error {
	pragmas := []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA foreign_keys = ON",
	}
	for _, stmt := range pragmas {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlite pragma %q: %w", stmt, err)
		}
	}

	for _, stmt := range schema {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlite migrate: %w", err)
		}
	}
	return nil
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS subscribers (
		chat_id INTEGER PRIMARY KEY,
		username TEXT NOT NULL DEFAULT '',
		first_name TEXT NOT NULL DEFAULT '',
		last_name TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		last_seen_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS keywords (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		phrase TEXT NOT NULL COLLATE NOCASE UNIQUE,
		enabled INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS keyword_terms (
		keyword_id INTEGER NOT NULL,
		kind TEXT NOT NULL CHECK(kind IN ('any', 'all', 'exclude')),
		term TEXT NOT NULL COLLATE NOCASE,
		position INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY(keyword_id, kind, term),
		FOREIGN KEY(keyword_id) REFERENCES keywords(id) ON DELETE CASCADE
	)`,
	`CREATE INDEX IF NOT EXISTS idx_keyword_terms_keyword ON keyword_terms(keyword_id, kind, position)`,
	`CREATE TABLE IF NOT EXISTS keyword_required_any_terms (
		keyword_id INTEGER NOT NULL,
		group_position INTEGER NOT NULL,
		term_position INTEGER NOT NULL DEFAULT 0,
		term TEXT NOT NULL COLLATE NOCASE,
		PRIMARY KEY(keyword_id, group_position, term),
		FOREIGN KEY(keyword_id) REFERENCES keywords(id) ON DELETE CASCADE
	)`,
	`CREATE INDEX IF NOT EXISTS idx_keyword_required_any_terms_keyword
		ON keyword_required_any_terms(keyword_id, group_position, term_position)`,
	`CREATE TABLE IF NOT EXISTS keyword_preferred_terms (
		keyword_id INTEGER NOT NULL,
		term TEXT NOT NULL COLLATE NOCASE,
		position INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY(keyword_id, term),
		FOREIGN KEY(keyword_id) REFERENCES keywords(id) ON DELETE CASCADE
	)`,
	`CREATE INDEX IF NOT EXISTS idx_keyword_preferred_terms_keyword
		ON keyword_preferred_terms(keyword_id, position)`,
	`CREATE TABLE IF NOT EXISTS keyword_rule_meta (
		keyword_id INTEGER PRIMARY KEY,
		operator_note TEXT NOT NULL DEFAULT '',
		FOREIGN KEY(keyword_id) REFERENCES keywords(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS keyword_rule_options (
		keyword_id INTEGER PRIMARY KEY,
		exclude_complete_bike INTEGER NOT NULL DEFAULT 0,
		FOREIGN KEY(keyword_id) REFERENCES keywords(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS keyword_sources (
		keyword_id INTEGER NOT NULL,
		peer_type TEXT NOT NULL,
		peer_id INTEGER NOT NULL,
		PRIMARY KEY(keyword_id, peer_type, peer_id),
		FOREIGN KEY(keyword_id) REFERENCES keywords(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_peer_type TEXT NOT NULL,
		source_peer_id INTEGER NOT NULL,
		message_id INTEGER NOT NULL,
		message_date TEXT NOT NULL,
		text TEXT NOT NULL,
		keyword TEXT NOT NULL,
		created_at TEXT NOT NULL,
		UNIQUE(source_peer_type, source_peer_id, message_id, keyword)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_events_created_at ON events(created_at DESC)`,
	`CREATE TABLE IF NOT EXISTS event_match_details (
		event_id INTEGER PRIMARY KEY,
		keyword_id INTEGER NOT NULL DEFAULT 0,
		reason TEXT NOT NULL DEFAULT '',
		score INTEGER NOT NULL DEFAULT 0,
		FOREIGN KEY(event_id) REFERENCES events(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS monitor_peers (
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
		PRIMARY KEY(peer_type, peer_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_monitor_peers_enabled ON monitor_peers(enabled, title COLLATE NOCASE)`,
	`CREATE TABLE IF NOT EXISTS update_state (
		user_id INTEGER PRIMARY KEY,
		pts INTEGER NOT NULL,
		qts INTEGER NOT NULL,
		date INTEGER NOT NULL,
		seq INTEGER NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS channel_state (
		user_id INTEGER NOT NULL,
		channel_id INTEGER NOT NULL,
		pts INTEGER NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY(user_id, channel_id)
	)`,
	`CREATE TABLE IF NOT EXISTS channel_access_hashes (
		user_id INTEGER NOT NULL,
		channel_id INTEGER NOT NULL,
		access_hash INTEGER NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY(user_id, channel_id)
	)`,
	`CREATE TABLE IF NOT EXISTS user_access_hashes (
		user_id INTEGER NOT NULL,
		target_user_id INTEGER NOT NULL,
		access_hash INTEGER NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY(user_id, target_user_id)
	)`,
	`CREATE TABLE IF NOT EXISTS login_phones (
		chat_id INTEGER PRIMARY KEY,
		phone TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS login_tokens (
		token TEXT PRIMARY KEY,
		chat_id INTEGER NOT NULL,
		phone TEXT NOT NULL,
		created_at TEXT NOT NULL,
		expires_at TEXT NOT NULL,
		used_at TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_login_tokens_chat ON login_tokens(chat_id, created_at DESC)`,
	`CREATE TABLE IF NOT EXISTS private_dialogs (
		owner_user_id INTEGER NOT NULL,
		peer_id INTEGER NOT NULL,
		access_hash INTEGER NOT NULL DEFAULT 0,
		title TEXT NOT NULL DEFAULT '',
		username TEXT NOT NULL DEFAULT '',
		is_bot INTEGER NOT NULL DEFAULT 0,
		discovered_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		last_backfill_at TEXT NOT NULL DEFAULT '',
		PRIMARY KEY(owner_user_id, peer_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_private_dialogs_backfill
		ON private_dialogs(owner_user_id, is_bot, last_backfill_at, updated_at DESC)`,
	`CREATE TABLE IF NOT EXISTS private_messages (
		owner_user_id INTEGER NOT NULL,
		message_id INTEGER NOT NULL,
		peer_id INTEGER NOT NULL,
		sender_id INTEGER NOT NULL DEFAULT 0,
		message_date TEXT NOT NULL,
		edit_date TEXT NOT NULL DEFAULT '',
		revision_date TEXT NOT NULL,
		text TEXT NOT NULL DEFAULT '',
		media_type TEXT NOT NULL DEFAULT '',
		outgoing INTEGER NOT NULL DEFAULT 0 CHECK(outgoing IN (0, 1)),
		first_seen_at TEXT NOT NULL,
		last_seen_at TEXT NOT NULL,
		deleted_at TEXT NOT NULL DEFAULT '',
		PRIMARY KEY(owner_user_id, message_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_private_messages_peer
		ON private_messages(owner_user_id, peer_id, message_date DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_private_messages_deleted
		ON private_messages(owner_user_id, deleted_at, peer_id)`,
	`CREATE TABLE IF NOT EXISTS private_message_deletions (
			owner_user_id INTEGER NOT NULL,
			message_id INTEGER NOT NULL,
			observed_at TEXT NOT NULL,
			notified_at TEXT NOT NULL DEFAULT '',
			attempts INTEGER NOT NULL DEFAULT 0,
			last_attempt_at TEXT NOT NULL DEFAULT '',
			next_attempt_at TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(owner_user_id, message_id)
		)`,
	`CREATE INDEX IF NOT EXISTS idx_private_message_deletions_pending
		ON private_message_deletions(owner_user_id, notified_at, observed_at)`,
	`CREATE TABLE IF NOT EXISTS codex_projects (
		slug TEXT PRIMARY KEY COLLATE NOCASE,
		title TEXT NOT NULL,
		telegram_channel_id INTEGER NOT NULL,
		telegram_access_hash INTEGER NOT NULL,
		telegram_chat_id INTEGER NOT NULL UNIQUE,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS codex_threads (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project_slug TEXT NOT NULL COLLATE NOCASE,
		telegram_chat_id INTEGER NOT NULL,
		telegram_topic_id INTEGER NOT NULL,
		title TEXT NOT NULL,
		codex_thread_id TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		UNIQUE(telegram_chat_id, telegram_topic_id),
		FOREIGN KEY(project_slug) REFERENCES codex_projects(slug) ON DELETE CASCADE
	)`,
	`CREATE INDEX IF NOT EXISTS idx_codex_threads_codex_id ON codex_threads(codex_thread_id)`,
	`CREATE TABLE IF NOT EXISTS codex_thread_aliases (
		thread_id INTEGER NOT NULL,
		codex_thread_id TEXT NOT NULL UNIQUE,
		observed_at TEXT NOT NULL,
		FOREIGN KEY(thread_id) REFERENCES codex_threads(id) ON DELETE CASCADE
	)`,
	`CREATE INDEX IF NOT EXISTS idx_codex_thread_aliases_thread ON codex_thread_aliases(thread_id)`,
	`INSERT OR IGNORE INTO codex_thread_aliases(thread_id, codex_thread_id, observed_at)
		SELECT id, codex_thread_id, updated_at FROM codex_threads WHERE codex_thread_id != ''`,
	`CREATE TABLE IF NOT EXISTS codex_deleted_topics (
		thread_id INTEGER PRIMARY KEY,
		deleted_at TEXT NOT NULL,
		FOREIGN KEY(thread_id) REFERENCES codex_threads(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS codex_thread_mirrors (
		thread_id INTEGER PRIMARY KEY,
		cwd TEXT NOT NULL DEFAULT '',
		telegram_message_id INTEGER NOT NULL DEFAULT 0,
		content_hash TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL,
		FOREIGN KEY(thread_id) REFERENCES codex_threads(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS codex_read_receipts (
		thread_id INTEGER PRIMARY KEY,
		requested_at TEXT NOT NULL,
		delivered_at TEXT NOT NULL DEFAULT '',
		FOREIGN KEY(thread_id) REFERENCES codex_threads(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS codex_outbound_messages (
		telegram_chat_id INTEGER NOT NULL,
		telegram_topic_id INTEGER NOT NULL,
		telegram_message_id INTEGER NOT NULL DEFAULT 0,
		text TEXT NOT NULL,
		created_at TEXT NOT NULL,
		PRIMARY KEY(telegram_chat_id, telegram_topic_id, telegram_message_id)
	)`,
	`CREATE TABLE IF NOT EXISTS codex_jobs (
		id TEXT PRIMARY KEY,
		thread_id INTEGER NOT NULL,
		prompt TEXT NOT NULL,
		status TEXT NOT NULL CHECK(status IN ('queued', 'claimed', 'running', 'succeeded', 'failed')),
		worker_id TEXT NOT NULL DEFAULT '',
		lease_token TEXT NOT NULL DEFAULT '',
		lease_expires_at TEXT NOT NULL DEFAULT '',
		result TEXT NOT NULL DEFAULT '',
		error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		started_at TEXT NOT NULL DEFAULT '',
		finished_at TEXT NOT NULL DEFAULT '',
		FOREIGN KEY(thread_id) REFERENCES codex_threads(id) ON DELETE CASCADE
	)`,
	`CREATE INDEX IF NOT EXISTS idx_codex_jobs_claim ON codex_jobs(status, created_at)`,
}

func (s *Store) UpsertSubscriber(ctx context.Context, sub Subscriber) error {
	now := nowText()
	if sub.CreatedAt.IsZero() {
		sub.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO subscribers(chat_id, username, first_name, last_name, created_at, last_seen_at)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET
			username = excluded.username,
			first_name = excluded.first_name,
			last_name = excluded.last_name,
			last_seen_at = excluded.last_seen_at
	`, sub.ChatID, sub.Username, sub.FirstName, sub.LastName, formatTime(sub.CreatedAt), now)
	if err != nil {
		return fmt.Errorf("upsert subscriber: %w", err)
	}
	return nil
}

func (s *Store) DeleteSubscriber(ctx context.Context, chatID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM subscribers WHERE chat_id = ?`, chatID)
	if err != nil {
		return fmt.Errorf("delete subscriber: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM login_phones WHERE chat_id = ?`, chatID); err != nil {
		return fmt.Errorf("delete subscriber login phone: %w", err)
	}
	return nil
}

func (s *Store) SaveLoginPhone(ctx context.Context, chatID int64, phone string) error {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return errors.New("login phone is empty")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO login_phones(chat_id, phone, updated_at)
		VALUES(?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET
			phone = excluded.phone,
			updated_at = excluded.updated_at
	`, chatID, phone, nowText())
	if err != nil {
		return fmt.Errorf("save login phone: %w", err)
	}
	return nil
}

func (s *Store) LoginPhone(ctx context.Context, chatID int64) (string, bool, error) {
	var phone string
	err := s.db.QueryRowContext(ctx, `SELECT phone FROM login_phones WHERE chat_id = ?`, chatID).Scan(&phone)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get login phone: %w", err)
	}
	phone = strings.TrimSpace(phone)
	return phone, phone != "", nil
}

func (s *Store) CreateLoginToken(ctx context.Context, chatID int64, phone string, ttl time.Duration) (LoginToken, error) {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return LoginToken{}, errors.New("login phone is empty")
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM login_tokens WHERE expires_at < ? OR used_at != ''`, nowText()); err != nil {
		return LoginToken{}, fmt.Errorf("delete old login tokens: %w", err)
	}

	now := time.Now().UTC()
	item := LoginToken{
		ChatID:    chatID,
		Phone:     phone,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	token, err := randomLoginToken()
	if err != nil {
		return LoginToken{}, err
	}
	item.Token = token

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO login_tokens(token, chat_id, phone, created_at, expires_at, used_at)
		VALUES(?, ?, ?, ?, ?, '')
	`, item.Token, item.ChatID, item.Phone, formatTime(item.CreatedAt), formatTime(item.ExpiresAt))
	if err != nil {
		return LoginToken{}, fmt.Errorf("create login token: %w", err)
	}
	return item, nil
}

func (s *Store) GetLoginToken(ctx context.Context, token string) (LoginToken, bool, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return LoginToken{}, false, nil
	}
	var item LoginToken
	var createdAt, expiresAt, usedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT token, chat_id, phone, created_at, expires_at, used_at
		FROM login_tokens
		WHERE token = ?
	`, token).Scan(&item.Token, &item.ChatID, &item.Phone, &createdAt, &expiresAt, &usedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return LoginToken{}, false, nil
	}
	if err != nil {
		return LoginToken{}, false, fmt.Errorf("get login token: %w", err)
	}
	item.Phone = strings.TrimSpace(item.Phone)
	item.CreatedAt = parseDBTime(createdAt)
	item.ExpiresAt = parseDBTime(expiresAt)
	item.UsedAt = parseDBTime(usedAt)
	return item, true, nil
}

func (s *Store) MarkLoginTokenUsed(ctx context.Context, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("login token is empty")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE login_tokens SET used_at = ? WHERE token = ?`, nowText(), token)
	if err != nil {
		return fmt.Errorf("mark login token used: %w", err)
	}
	return nil
}

func (s *Store) ListSubscribers(ctx context.Context) ([]Subscriber, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT chat_id, username, first_name, last_name, created_at, last_seen_at
		FROM subscribers
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list subscribers: %w", err)
	}
	defer rows.Close()

	var out []Subscriber
	for rows.Next() {
		var sub Subscriber
		var createdAt, lastSeenAt string
		if err := rows.Scan(&sub.ChatID, &sub.Username, &sub.FirstName, &sub.LastName, &createdAt, &lastSeenAt); err != nil {
			return nil, fmt.Errorf("scan subscriber: %w", err)
		}
		sub.CreatedAt = parseDBTime(createdAt)
		sub.LastSeenAt = parseDBTime(lastSeenAt)
		out = append(out, sub)
	}
	return out, rows.Err()
}

func (s *Store) FirstSubscriber(ctx context.Context) (Subscriber, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT chat_id, username, first_name, last_name, created_at, last_seen_at
		FROM subscribers
		ORDER BY created_at ASC
		LIMIT 1
	`)

	var sub Subscriber
	var createdAt, lastSeenAt string
	err := row.Scan(&sub.ChatID, &sub.Username, &sub.FirstName, &sub.LastName, &createdAt, &lastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Subscriber{}, false, nil
	}
	if err != nil {
		return Subscriber{}, false, fmt.Errorf("first subscriber: %w", err)
	}
	sub.CreatedAt = parseDBTime(createdAt)
	sub.LastSeenAt = parseDBTime(lastSeenAt)
	return sub, true, nil
}

func (s *Store) AddKeyword(ctx context.Context, phrase string) (Keyword, error) {
	phrase = strings.TrimSpace(phrase)
	if phrase == "" {
		return Keyword{}, errors.New("keyword is empty")
	}

	createdAt := nowText()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO keywords(phrase, enabled, created_at)
		VALUES(?, 1, ?)
		ON CONFLICT(phrase) DO UPDATE SET enabled = 1
	`, phrase, createdAt)
	if err != nil {
		return Keyword{}, fmt.Errorf("add keyword: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected > 0 {
		keyword, getErr := s.GetKeywordByPhrase(ctx, phrase)
		if getErr != nil {
			return Keyword{}, getErr
		}
		if len(keyword.AnyTerms)+len(keyword.AllTerms)+len(keyword.RequiredAnyGroups) == 0 {
			if _, err := s.db.ExecContext(ctx, `
				INSERT OR IGNORE INTO keyword_terms(keyword_id, kind, term, position)
				VALUES(?, 'any', ?, 0)
			`, keyword.ID, phrase); err != nil {
				return Keyword{}, fmt.Errorf("add keyword term: %w", err)
			}
		}
	}

	return s.GetKeywordByPhrase(ctx, phrase)
}

func (s *Store) UpsertWatchRule(ctx context.Context, rule Keyword) (Keyword, error) {
	rule, err := prepareWatchRule(rule)
	if err != nil {
		return Keyword{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Keyword{}, fmt.Errorf("begin watch rule update: %w", err)
	}
	defer tx.Rollback()
	if err := upsertWatchRuleTx(ctx, tx, &rule); err != nil {
		return Keyword{}, err
	}
	if err := tx.Commit(); err != nil {
		return Keyword{}, fmt.Errorf("commit watch rule update: %w", err)
	}
	return s.GetKeywordByPhrase(ctx, rule.Phrase)
}

// ApplyWatchRules applies a declarative rule update in one transaction. New
// rules are written before the explicitly named old rules are deleted.
func (s *Store) ApplyWatchRules(ctx context.Context, rules []Keyword, deleteValues []string) (int64, error) {
	prepared := make([]Keyword, 0, len(rules))
	names := make(map[string]bool, len(rules))
	for _, rule := range rules {
		item, err := prepareWatchRule(rule)
		if err != nil {
			return 0, err
		}
		key := strings.ToLower(item.Phrase)
		if names[key] {
			return 0, fmt.Errorf("duplicate rule name %q", item.Phrase)
		}
		names[key] = true
		prepared = append(prepared, item)
	}
	deletes := make([]string, 0, len(deleteValues))
	for _, value := range deleteValues {
		value = strings.TrimSpace(value)
		if value == "" {
			return 0, errors.New("delete rule name is empty")
		}
		if names[strings.ToLower(value)] {
			return 0, fmt.Errorf("rule %q cannot be imported and deleted together", value)
		}
		deletes = append(deletes, value)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin watch rules update: %w", err)
	}
	defer tx.Rollback()
	for i := range prepared {
		if err := upsertWatchRuleTx(ctx, tx, &prepared[i]); err != nil {
			return 0, err
		}
	}
	var deleted int64
	for _, value := range deletes {
		var res sql.Result
		if id, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil {
			res, err = tx.ExecContext(ctx, `DELETE FROM keywords WHERE id = ?`, id)
		} else {
			res, err = tx.ExecContext(ctx, `DELETE FROM keywords WHERE phrase = ? COLLATE NOCASE`, value)
		}
		if err != nil {
			return 0, fmt.Errorf("delete old watch rule %q: %w", value, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("count deleted watch rule %q: %w", value, err)
		}
		deleted += affected
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit watch rules update: %w", err)
	}
	return deleted, nil
}

func prepareWatchRule(rule Keyword) (Keyword, error) {
	rule.Phrase = strings.TrimSpace(rule.Phrase)
	if rule.Phrase == "" {
		return Keyword{}, errors.New("rule name is empty")
	}
	rule.AnyTerms = cleanTerms(rule.AnyTerms)
	rule.AllTerms = cleanTerms(rule.AllTerms)
	rule.RequiredAnyGroups = cleanTermGroups(rule.RequiredAnyGroups)
	rule.PreferredTerms = cleanTerms(rule.PreferredTerms)
	rule.ExcludeTerms = cleanTerms(rule.ExcludeTerms)
	rule.Note = strings.TrimSpace(rule.Note)
	rule.Sources = cleanRuleSources(rule.Sources)
	if len(rule.AnyTerms)+len(rule.AllTerms)+len(rule.RequiredAnyGroups) == 0 {
		return Keyword{}, errors.New("rule needs at least one any, all, or required-any group")
	}
	if len([]rune(rule.Note)) > 800 {
		return Keyword{}, errors.New("rule note is longer than 800 characters")
	}
	return rule, nil
}

func upsertWatchRuleTx(ctx context.Context, tx *sql.Tx, rule *Keyword) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO keywords(phrase, enabled, created_at)
		VALUES(?, 1, ?)
		ON CONFLICT(phrase) DO UPDATE SET enabled = 1
	`, rule.Phrase, nowText()); err != nil {
		return fmt.Errorf("upsert watch rule: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM keywords WHERE phrase = ? COLLATE NOCASE`, rule.Phrase).Scan(&rule.ID); err != nil {
		return fmt.Errorf("resolve watch rule id: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM keyword_terms WHERE keyword_id = ?`, rule.ID); err != nil {
		return fmt.Errorf("clear watch rule terms: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM keyword_required_any_terms WHERE keyword_id = ?`, rule.ID); err != nil {
		return fmt.Errorf("clear watch rule required-any groups: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM keyword_preferred_terms WHERE keyword_id = ?`, rule.ID); err != nil {
		return fmt.Errorf("clear watch rule preferred terms: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM keyword_rule_meta WHERE keyword_id = ?`, rule.ID); err != nil {
		return fmt.Errorf("clear watch rule note: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM keyword_rule_options WHERE keyword_id = ?`, rule.ID); err != nil {
		return fmt.Errorf("clear watch rule options: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM keyword_sources WHERE keyword_id = ?`, rule.ID); err != nil {
		return fmt.Errorf("clear watch rule sources: %w", err)
	}
	for kind, terms := range map[string][]string{"any": rule.AnyTerms, "all": rule.AllTerms, "exclude": rule.ExcludeTerms} {
		for position, term := range terms {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO keyword_terms(keyword_id, kind, term, position) VALUES(?, ?, ?, ?)
			`, rule.ID, kind, term, position); err != nil {
				return fmt.Errorf("add %s watch rule term: %w", kind, err)
			}
		}
	}
	for groupPosition, terms := range rule.RequiredAnyGroups {
		for termPosition, term := range terms {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO keyword_required_any_terms(keyword_id, group_position, term_position, term)
				VALUES(?, ?, ?, ?)
			`, rule.ID, groupPosition, termPosition, term); err != nil {
				return fmt.Errorf("add required-any watch rule term: %w", err)
			}
		}
	}
	for position, term := range rule.PreferredTerms {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO keyword_preferred_terms(keyword_id, term, position) VALUES(?, ?, ?)
		`, rule.ID, term, position); err != nil {
			return fmt.Errorf("add preferred watch rule term: %w", err)
		}
	}
	if rule.Note != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO keyword_rule_meta(keyword_id, operator_note) VALUES(?, ?)
		`, rule.ID, rule.Note); err != nil {
			return fmt.Errorf("add watch rule note: %w", err)
		}
	}
	if rule.ExcludeCompleteBike {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO keyword_rule_options(keyword_id, exclude_complete_bike) VALUES(?, 1)
		`, rule.ID); err != nil {
			return fmt.Errorf("add watch rule options: %w", err)
		}
	}
	for _, source := range rule.Sources {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO keyword_sources(keyword_id, peer_type, peer_id) VALUES(?, ?, ?)
		`, rule.ID, source.PeerType, source.PeerID); err != nil {
			return fmt.Errorf("add watch rule source: %w", err)
		}
	}
	return nil
}

func (s *Store) GetKeywordByPhrase(ctx context.Context, phrase string) (Keyword, error) {
	var keyword Keyword
	var enabled int
	var createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, phrase, enabled, created_at
		FROM keywords
		WHERE phrase = ?
	`, phrase).Scan(&keyword.ID, &keyword.Phrase, &enabled, &createdAt)
	if err != nil {
		return Keyword{}, fmt.Errorf("get keyword: %w", err)
	}
	keyword.Enabled = enabled == 1
	keyword.CreatedAt = parseDBTime(createdAt)
	if err := s.loadKeywordDetails(ctx, &keyword); err != nil {
		return Keyword{}, err
	}
	return keyword, nil
}

func (s *Store) DeleteKeyword(ctx context.Context, value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("keyword is empty")
	}

	var (
		res sql.Result
		err error
	)
	if id, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil {
		res, err = s.db.ExecContext(ctx, `DELETE FROM keywords WHERE id = ?`, id)
	} else {
		res, err = s.db.ExecContext(ctx, `DELETE FROM keywords WHERE phrase = ? COLLATE NOCASE`, value)
	}
	if err != nil {
		return 0, fmt.Errorf("delete keyword: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete keyword rows affected: %w", err)
	}
	return rows, nil
}

func (s *Store) ListKeywords(ctx context.Context) ([]Keyword, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, phrase, enabled, created_at
		FROM keywords
		WHERE enabled = 1
		ORDER BY phrase COLLATE NOCASE
	`)
	if err != nil {
		return nil, fmt.Errorf("list keywords: %w", err)
	}
	defer rows.Close()

	var out []Keyword
	for rows.Next() {
		var item Keyword
		var enabled int
		var createdAt string
		if err := rows.Scan(&item.ID, &item.Phrase, &enabled, &createdAt); err != nil {
			return nil, fmt.Errorf("scan keyword: %w", err)
		}
		item.Enabled = enabled == 1
		item.CreatedAt = parseDBTime(createdAt)
		if err := s.loadKeywordDetails(ctx, &item); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) loadKeywordDetails(ctx context.Context, keyword *Keyword) error {
	keyword.AnyTerms = nil
	keyword.AllTerms = nil
	keyword.RequiredAnyGroups = nil
	keyword.PreferredTerms = nil
	keyword.ExcludeTerms = nil
	keyword.Note = ""
	keyword.ExcludeCompleteBike = false
	keyword.Sources = nil

	rows, err := s.db.QueryContext(ctx, `
		SELECT kind, term FROM keyword_terms WHERE keyword_id = ? ORDER BY kind, position
	`, keyword.ID)
	if err != nil {
		return fmt.Errorf("list watch rule terms: %w", err)
	}
	for rows.Next() {
		var kind, term string
		if err := rows.Scan(&kind, &term); err != nil {
			rows.Close()
			return fmt.Errorf("scan watch rule term: %w", err)
		}
		switch kind {
		case "any":
			keyword.AnyTerms = append(keyword.AnyTerms, term)
		case "all":
			keyword.AllTerms = append(keyword.AllTerms, term)
		case "exclude":
			keyword.ExcludeTerms = append(keyword.ExcludeTerms, term)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}

	groupRows, err := s.db.QueryContext(ctx, `
		SELECT group_position, term
		FROM keyword_required_any_terms
		WHERE keyword_id = ?
		ORDER BY group_position, term_position
	`, keyword.ID)
	if err != nil {
		return fmt.Errorf("list watch rule required-any groups: %w", err)
	}
	lastGroup := -1
	for groupRows.Next() {
		var groupPosition int
		var term string
		if err := groupRows.Scan(&groupPosition, &term); err != nil {
			groupRows.Close()
			return fmt.Errorf("scan watch rule required-any term: %w", err)
		}
		if groupPosition != lastGroup {
			keyword.RequiredAnyGroups = append(keyword.RequiredAnyGroups, nil)
			lastGroup = groupPosition
		}
		last := len(keyword.RequiredAnyGroups) - 1
		keyword.RequiredAnyGroups[last] = append(keyword.RequiredAnyGroups[last], term)
	}
	if err := groupRows.Close(); err != nil {
		return err
	}

	preferredRows, err := s.db.QueryContext(ctx, `
		SELECT term FROM keyword_preferred_terms WHERE keyword_id = ? ORDER BY position
	`, keyword.ID)
	if err != nil {
		return fmt.Errorf("list watch rule preferred terms: %w", err)
	}
	for preferredRows.Next() {
		var term string
		if err := preferredRows.Scan(&term); err != nil {
			preferredRows.Close()
			return fmt.Errorf("scan watch rule preferred term: %w", err)
		}
		keyword.PreferredTerms = append(keyword.PreferredTerms, term)
	}
	if err := preferredRows.Close(); err != nil {
		return err
	}

	err = s.db.QueryRowContext(ctx, `
		SELECT operator_note FROM keyword_rule_meta WHERE keyword_id = ?
	`, keyword.ID).Scan(&keyword.Note)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("get watch rule note: %w", err)
	}

	var excludeCompleteBike int
	err = s.db.QueryRowContext(ctx, `
		SELECT exclude_complete_bike FROM keyword_rule_options WHERE keyword_id = ?
	`, keyword.ID).Scan(&excludeCompleteBike)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("get watch rule options: %w", err)
	}
	keyword.ExcludeCompleteBike = excludeCompleteBike == 1

	if len(keyword.AnyTerms)+len(keyword.AllTerms)+len(keyword.RequiredAnyGroups) == 0 {
		keyword.AnyTerms = []string{keyword.Phrase}
	}
	sourceRows, err := s.db.QueryContext(ctx, `
		SELECT peer_type, peer_id FROM keyword_sources WHERE keyword_id = ? ORDER BY peer_type, peer_id
	`, keyword.ID)
	if err != nil {
		return fmt.Errorf("list watch rule sources: %w", err)
	}
	defer sourceRows.Close()
	for sourceRows.Next() {
		var source RuleSource
		if err := sourceRows.Scan(&source.PeerType, &source.PeerID); err != nil {
			return fmt.Errorf("scan watch rule source: %w", err)
		}
		keyword.Sources = append(keyword.Sources, source)
	}
	return sourceRows.Err()
}

func cleanTerms(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if value == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

func cleanTermGroups(groups [][]string) [][]string {
	out := make([][]string, 0, len(groups))
	for _, group := range groups {
		if terms := cleanTerms(group); len(terms) > 0 {
			out = append(out, terms)
		}
	}
	return out
}

func cleanRuleSources(values []RuleSource) []RuleSource {
	seen := make(map[string]bool, len(values))
	out := make([]RuleSource, 0, len(values))
	for _, value := range values {
		value.PeerType = strings.TrimSpace(value.PeerType)
		if value.PeerType == "" || value.PeerID <= 0 {
			continue
		}
		key := value.PeerType + ":" + strconv.FormatInt(value.PeerID, 10)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

func (s *Store) RecordEvent(ctx context.Context, event Event) (Event, bool, error) {
	if event.MessageDate.IsZero() {
		event.MessageDate = time.Now().UTC()
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, false, fmt.Errorf("begin event record: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO events(source_peer_type, source_peer_id, message_id, message_date, text, keyword, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)
	`, event.SourcePeerType, event.SourcePeerID, event.MessageID, formatTime(event.MessageDate), event.Text, event.Keyword, formatTime(event.CreatedAt))
	if err != nil {
		return Event{}, false, fmt.Errorf("record event: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Event{}, false, fmt.Errorf("record event rows affected: %w", err)
	}
	if affected == 0 {
		return event, false, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Event{}, false, fmt.Errorf("record event last insert id: %w", err)
	}
	event.ID = id
	if event.RuleID != 0 || event.MatchReason != "" || event.MatchScore != 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO event_match_details(event_id, keyword_id, reason, score)
			VALUES(?, ?, ?, ?)
		`, event.ID, event.RuleID, event.MatchReason, event.MatchScore); err != nil {
			return Event{}, false, fmt.Errorf("record event match details: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Event{}, false, fmt.Errorf("commit event record: %w", err)
	}
	return event, true, nil
}

func (s *Store) GetEvent(ctx context.Context, id int64) (Event, bool, error) {
	var item Event
	var messageDate, createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT e.id, e.source_peer_type, e.source_peer_id, e.message_id, e.message_date, e.text, e.keyword,
		       COALESCE(d.keyword_id, 0), COALESCE(d.reason, ''), COALESCE(d.score, 0), e.created_at
		FROM events e
		LEFT JOIN event_match_details d ON d.event_id = e.id
		WHERE e.id = ?
	`, id).Scan(&item.ID, &item.SourcePeerType, &item.SourcePeerID, &item.MessageID, &messageDate, &item.Text, &item.Keyword,
		&item.RuleID, &item.MatchReason, &item.MatchScore, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("get event: %w", err)
	}
	item.MessageDate = parseDBTime(messageDate)
	item.CreatedAt = parseDBTime(createdAt)
	return item, true, nil
}

func (s *Store) ListEvents(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.source_peer_type, e.source_peer_id, e.message_id, e.message_date, e.text, e.keyword,
		       COALESCE(d.keyword_id, 0), COALESCE(d.reason, ''), COALESCE(d.score, 0), e.created_at
		FROM events e
		LEFT JOIN event_match_details d ON d.event_id = e.id
		ORDER BY e.created_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var item Event
		var messageDate, createdAt string
		if err := rows.Scan(&item.ID, &item.SourcePeerType, &item.SourcePeerID, &item.MessageID, &messageDate, &item.Text, &item.Keyword,
			&item.RuleID, &item.MatchReason, &item.MatchScore, &createdAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		item.MessageDate = parseDBTime(messageDate)
		item.CreatedAt = parseDBTime(createdAt)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) UpsertMonitorPeer(ctx context.Context, peer MonitorPeer) error {
	if peer.PeerType == "" {
		return errors.New("peer type is empty")
	}
	if peer.PeerID == 0 {
		return errors.New("peer id is empty")
	}
	now := time.Now().UTC()
	if peer.DiscoveredAt.IsZero() {
		peer.DiscoveredAt = now
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO monitor_peers(peer_type, peer_id, access_hash, title, username, kind, enabled, discovered_at, updated_at, last_backfill_at)
		VALUES(?, ?, ?, ?, ?, ?, 0, ?, ?, '')
		ON CONFLICT(peer_type, peer_id) DO UPDATE SET
			access_hash = excluded.access_hash,
			title = excluded.title,
			username = excluded.username,
			kind = excluded.kind,
			updated_at = excluded.updated_at
	`, peer.PeerType, peer.PeerID, peer.AccessHash, peer.Title, peer.Username, peer.Kind, formatTime(peer.DiscoveredAt), formatTime(now))
	if err != nil {
		return fmt.Errorf("upsert monitor peer: %w", err)
	}
	return nil
}

func (s *Store) ListMonitorPeers(ctx context.Context, enabledOnly bool) ([]MonitorPeer, error) {
	query := `
		SELECT peer_type, peer_id, access_hash, title, username, kind, enabled, discovered_at, updated_at, last_backfill_at
		FROM monitor_peers
	`
	if enabledOnly {
		query += ` WHERE enabled = 1`
	}
	query += ` ORDER BY enabled DESC, title COLLATE NOCASE, peer_id`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list monitor peers: %w", err)
	}
	defer rows.Close()

	var out []MonitorPeer
	for rows.Next() {
		peer, err := scanMonitorPeer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, peer)
	}
	return out, rows.Err()
}

func (s *Store) GetMonitorPeer(ctx context.Context, peerType string, peerID int64) (MonitorPeer, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT peer_type, peer_id, access_hash, title, username, kind, enabled, discovered_at, updated_at, last_backfill_at
		FROM monitor_peers
		WHERE peer_type = ? AND peer_id = ?
	`, peerType, peerID)
	peer, err := scanMonitorPeer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MonitorPeer{}, false, nil
	}
	if err != nil {
		return MonitorPeer{}, false, err
	}
	return peer, true, nil
}

func (s *Store) SetMonitorPeerEnabled(ctx context.Context, peerType string, peerID int64, enabled bool) error {
	value := 0
	if enabled {
		value = 1
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE monitor_peers
		SET enabled = ?, updated_at = ?
		WHERE peer_type = ? AND peer_id = ?
	`, value, nowText(), peerType, peerID)
	if err != nil {
		return fmt.Errorf("set monitor peer enabled: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set monitor peer enabled rows affected: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) IsMonitorPeerEnabled(ctx context.Context, peerType string, peerID int64) (bool, error) {
	var enabled int
	err := s.db.QueryRowContext(ctx, `
		SELECT enabled
		FROM monitor_peers
		WHERE peer_type = ? AND peer_id = ?
	`, peerType, peerID).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("is monitor peer enabled: %w", err)
	}
	return enabled == 1, nil
}

func (s *Store) SetMonitorPeerBackfilled(ctx context.Context, peerType string, peerID int64, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE monitor_peers
		SET last_backfill_at = ?, updated_at = ?
		WHERE peer_type = ? AND peer_id = ?
	`, formatTime(at), nowText(), peerType, peerID)
	if err != nil {
		return fmt.Errorf("set monitor peer backfilled: %w", err)
	}
	return nil
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var stats Stats
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM subscribers`).Scan(&stats.Subscribers); err != nil {
		return Stats{}, fmt.Errorf("count subscribers: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM keywords WHERE enabled = 1`).Scan(&stats.Keywords); err != nil {
		return Stats{}, fmt.Errorf("count keywords: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&stats.Events); err != nil {
		return Stats{}, fmt.Errorf("count events: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM monitor_peers`).Scan(&stats.Peers); err != nil {
		return Stats{}, fmt.Errorf("count monitor peers: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM monitor_peers WHERE enabled = 1`).Scan(&stats.EnabledPeers); err != nil {
		return Stats{}, fmt.Errorf("count enabled monitor peers: %w", err)
	}
	return stats, nil
}

func (s *Store) GetState(ctx context.Context, userID int64) (updates.State, bool, error) {
	var state updates.State
	err := s.db.QueryRowContext(ctx, `
		SELECT pts, qts, date, seq
		FROM update_state
		WHERE user_id = ?
	`, userID).Scan(&state.Pts, &state.Qts, &state.Date, &state.Seq)
	if errors.Is(err, sql.ErrNoRows) {
		return updates.State{}, false, nil
	}
	if err != nil {
		return updates.State{}, false, fmt.Errorf("get update state: %w", err)
	}
	return state, true, nil
}

func (s *Store) SetState(ctx context.Context, userID int64, state updates.State) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO update_state(user_id, pts, qts, date, seq, updated_at)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			pts = excluded.pts,
			qts = excluded.qts,
			date = excluded.date,
			seq = excluded.seq,
			updated_at = excluded.updated_at
	`, userID, state.Pts, state.Qts, state.Date, state.Seq, nowText())
	if err != nil {
		return fmt.Errorf("set update state: %w", err)
	}
	return nil
}

func (s *Store) SetPts(ctx context.Context, userID int64, pts int) error {
	return s.updateStateField(ctx, userID, "pts", pts)
}

func (s *Store) SetQts(ctx context.Context, userID int64, qts int) error {
	return s.updateStateField(ctx, userID, "qts", qts)
}

func (s *Store) SetDate(ctx context.Context, userID int64, date int) error {
	return s.updateStateField(ctx, userID, "date", date)
}

func (s *Store) SetSeq(ctx context.Context, userID int64, seq int) error {
	return s.updateStateField(ctx, userID, "seq", seq)
}

func (s *Store) SetDateSeq(ctx context.Context, userID int64, date, seq int) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE update_state
		SET date = ?, seq = ?, updated_at = ?
		WHERE user_id = ?
	`, date, seq, nowText(), userID)
	return checkStateUpdate(res, err)
}

func (s *Store) GetChannelPts(ctx context.Context, userID, channelID int64) (int, bool, error) {
	var pts int
	err := s.db.QueryRowContext(ctx, `
		SELECT pts
		FROM channel_state
		WHERE user_id = ? AND channel_id = ?
	`, userID, channelID).Scan(&pts)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("get channel pts: %w", err)
	}
	return pts, true, nil
}

func (s *Store) SetChannelPts(ctx context.Context, userID, channelID int64, pts int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO channel_state(user_id, channel_id, pts, updated_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(user_id, channel_id) DO UPDATE SET
			pts = excluded.pts,
			updated_at = excluded.updated_at
	`, userID, channelID, pts, nowText())
	if err != nil {
		return fmt.Errorf("set channel pts: %w", err)
	}
	return nil
}

func (s *Store) ForEachChannels(ctx context.Context, userID int64, f func(ctx context.Context, channelID int64, pts int) error) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT channel_id, pts
		FROM channel_state
		WHERE user_id = ?
	`, userID)
	if err != nil {
		return fmt.Errorf("list channel pts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var channelID int64
		var pts int
		if err := rows.Scan(&channelID, &pts); err != nil {
			return fmt.Errorf("scan channel pts: %w", err)
		}
		if err := f(ctx, channelID, pts); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) SetChannelAccessHash(ctx context.Context, userID, channelID, accessHash int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO channel_access_hashes(user_id, channel_id, access_hash, updated_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(user_id, channel_id) DO UPDATE SET
			access_hash = excluded.access_hash,
			updated_at = excluded.updated_at
	`, userID, channelID, accessHash, nowText())
	if err != nil {
		return fmt.Errorf("set channel access hash: %w", err)
	}
	return nil
}

func (s *Store) GetChannelAccessHash(ctx context.Context, userID, channelID int64) (int64, bool, error) {
	var accessHash int64
	err := s.db.QueryRowContext(ctx, `
		SELECT access_hash
		FROM channel_access_hashes
		WHERE user_id = ? AND channel_id = ?
	`, userID, channelID).Scan(&accessHash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("get channel access hash: %w", err)
	}
	return accessHash, true, nil
}

func (s *Store) SetUserAccessHash(ctx context.Context, userID, targetUserID, accessHash int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_access_hashes(user_id, target_user_id, access_hash, updated_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(user_id, target_user_id) DO UPDATE SET
			access_hash = excluded.access_hash,
			updated_at = excluded.updated_at
	`, userID, targetUserID, accessHash, nowText())
	if err != nil {
		return fmt.Errorf("set user access hash: %w", err)
	}
	return nil
}

func (s *Store) GetUserAccessHash(ctx context.Context, userID, targetUserID int64) (int64, bool, error) {
	var accessHash int64
	err := s.db.QueryRowContext(ctx, `
		SELECT access_hash
		FROM user_access_hashes
		WHERE user_id = ? AND target_user_id = ?
	`, userID, targetUserID).Scan(&accessHash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("get user access hash: %w", err)
	}
	return accessHash, true, nil
}

func (s *Store) updateStateField(ctx context.Context, userID int64, field string, value int) error {
	switch field {
	case "pts", "qts", "date", "seq":
	default:
		return fmt.Errorf("invalid state field %q", field)
	}
	res, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE update_state
		SET %s = ?, updated_at = ?
		WHERE user_id = ?
	`, field), value, nowText(), userID)
	return checkStateUpdate(res, err)
}

func checkStateUpdate(res sql.Result, err error) error {
	if err != nil {
		return fmt.Errorf("update state: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update state rows affected: %w", err)
	}
	if rows == 0 {
		return errors.New("update state does not exist")
	}
	return nil
}

func nowText() string {
	return formatTime(time.Now().UTC())
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseDBTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func randomLoginToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate login token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

type monitorPeerScanner interface {
	Scan(dest ...any) error
}

func scanMonitorPeer(scanner monitorPeerScanner) (MonitorPeer, error) {
	var peer MonitorPeer
	var enabled int
	var discoveredAt, updatedAt, lastBackfillAt string
	if err := scanner.Scan(
		&peer.PeerType,
		&peer.PeerID,
		&peer.AccessHash,
		&peer.Title,
		&peer.Username,
		&peer.Kind,
		&enabled,
		&discoveredAt,
		&updatedAt,
		&lastBackfillAt,
	); err != nil {
		return MonitorPeer{}, fmt.Errorf("scan monitor peer: %w", err)
	}
	peer.Enabled = enabled == 1
	peer.DiscoveredAt = parseDBTime(discoveredAt)
	peer.UpdatedAt = parseDBTime(updatedAt)
	peer.LastBackfillAt = parseDBTime(lastBackfillAt)
	return peer, nil
}
