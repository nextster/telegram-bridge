package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// OAuth rows store only SHA-256 hashes of client secrets, browser secrets,
// authorization codes, and tokens. Times are Unix seconds.

var (
	ErrOAuthLimit        = errors.New("oauth limit reached")
	ErrOAuthInvalidGrant = errors.New("oauth grant is invalid")
	// ErrOAuthTokenReuse means a rotated refresh token was presented again and
	// its grant has been revoked.
	ErrOAuthTokenReuse = errors.New("oauth refresh token reused")
)

// usedTokenRetention keeps rotated refresh token hashes so that reuse can be
// detected.
const usedTokenRetention = 7 * 24 * time.Hour

type OAuthRefreshPolicy struct {
	// ReuseGrace tolerates a concurrent retry of the same refresh token.
	ReuseGrace       time.Duration
	MaxGrantLifetime time.Duration
}

const (
	OAuthRequestPending   = "pending"
	OAuthRequestApproved  = "approved"
	OAuthRequestDenied    = "denied"
	OAuthRequestExchanged = "exchanged"
)

type OAuthClient struct {
	ClientID     string
	SecretHash   string
	Name         string
	RedirectURIs []string
	AuthMethod   string
	CreatedAt    time.Time
}

type OAuthRequest struct {
	ID            string
	BrowserHash   string
	ClientID      string
	RedirectURI   string
	State         string
	CodeChallenge string
	Resource      string
	ApprovalCode  string
	ClientIP      string
	UserAgent     string
	Status        string
	// BoundUserID is the Telegram user who signed in with Telegram in the
	// browser that opened the request. Only that user can approve it.
	BoundUserID   int64
	DecidedBy     int64
	CodeHash      string
	CodeExpiresAt time.Time
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

type OAuthGrant struct {
	ID         string
	ClientID   string
	ClientName string
	UserID     int64
	Resource   string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

type OAuthToken struct {
	Hash      string
	Kind      string
	ExpiresAt time.Time
}

var oauthSchema = []string{
	`CREATE TABLE IF NOT EXISTS oauth_clients (
		client_id TEXT PRIMARY KEY,
		secret_hash TEXT NOT NULL DEFAULT '',
		client_name TEXT NOT NULL DEFAULT '',
		redirect_uris TEXT NOT NULL,
		auth_method TEXT NOT NULL,
		created_at INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS oauth_requests (
		id TEXT PRIMARY KEY,
		browser_hash TEXT NOT NULL,
		client_id TEXT NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
		redirect_uri TEXT NOT NULL,
		state TEXT NOT NULL,
		code_challenge TEXT NOT NULL,
		resource TEXT NOT NULL,
		approval_code TEXT NOT NULL,
		client_ip TEXT NOT NULL DEFAULT '',
		user_agent TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL CHECK(status IN ('pending','approved','denied','exchanged')),
		bound_user_id INTEGER NOT NULL DEFAULT 0,
		login_state_hash TEXT NOT NULL DEFAULT '',
		login_nonce TEXT NOT NULL DEFAULT '',
		login_verifier TEXT NOT NULL DEFAULT '',
		decided_by INTEGER NOT NULL DEFAULT 0,
		code_hash TEXT NOT NULL DEFAULT '',
		code_expires_at INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_oauth_requests_code ON oauth_requests(code_hash) WHERE code_hash != ''`,
	`CREATE TABLE IF NOT EXISTS oauth_grants (
		id TEXT PRIMARY KEY,
		client_id TEXT NOT NULL,
		client_name TEXT NOT NULL DEFAULT '',
		user_id INTEGER NOT NULL,
		resource TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		last_used_at INTEGER NOT NULL DEFAULT 0,
		revoked_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS oauth_tokens (
		token_hash TEXT PRIMARY KEY,
		grant_id TEXT NOT NULL REFERENCES oauth_grants(id) ON DELETE CASCADE,
		kind TEXT NOT NULL CHECK(kind IN ('access','refresh')),
		expires_at INTEGER NOT NULL,
		used_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_oauth_tokens_grant ON oauth_tokens(grant_id)`,
}

// migrateOAuthRequests drops an oauth_requests table created before requests
// were bound to a Telegram user. Requests expire within minutes, so nothing of
// value is lost.
func (s *Store) migrateOAuthRequests(ctx context.Context) error {
	var bound int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('oauth_requests') WHERE name = 'bound_user_id'`).Scan(&bound)
	if err != nil {
		return fmt.Errorf("inspect oauth requests: %w", err)
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'oauth_requests'`).Scan(&exists); err != nil {
		return fmt.Errorf("inspect oauth requests: %w", err)
	}
	if exists == 0 || bound == 1 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE oauth_requests`); err != nil {
		return fmt.Errorf("drop old oauth requests: %w", err)
	}
	return nil
}

// CreateOAuthClient stores a dynamically registered client. maxClients bounds
// the table so the public registration endpoint cannot grow it without limit.
func (s *Store) CreateOAuthClient(ctx context.Context, client OAuthClient, maxClients int, now time.Time) error {
	uris, err := json.Marshal(client.RedirectURIs)
	if err != nil {
		return fmt.Errorf("encode oauth redirect uris: %w", err)
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := pruneOAuthTx(ctx, tx, now); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_clients`).Scan(&count); err != nil {
			return fmt.Errorf("count oauth clients: %w", err)
		}
		if maxClients > 0 && count >= maxClients {
			return ErrOAuthLimit
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO oauth_clients(client_id, secret_hash, client_name, redirect_uris, auth_method, created_at)
			VALUES(?, ?, ?, ?, ?, ?)
		`, client.ClientID, client.SecretHash, client.Name, string(uris), client.AuthMethod, now.Unix())
		if err != nil {
			return fmt.Errorf("create oauth client: %w", err)
		}
		return nil
	})
}

func (s *Store) GetOAuthClient(ctx context.Context, clientID string) (OAuthClient, bool, error) {
	var client OAuthClient
	var uris string
	var createdAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT client_id, secret_hash, client_name, redirect_uris, auth_method, created_at
		FROM oauth_clients WHERE client_id = ?
	`, clientID).Scan(&client.ClientID, &client.SecretHash, &client.Name, &uris, &client.AuthMethod, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthClient{}, false, nil
	}
	if err != nil {
		return OAuthClient{}, false, fmt.Errorf("get oauth client: %w", err)
	}
	if err := json.Unmarshal([]byte(uris), &client.RedirectURIs); err != nil {
		return OAuthClient{}, false, fmt.Errorf("decode oauth redirect uris: %w", err)
	}
	client.CreatedAt = time.Unix(createdAt, 0).UTC()
	return client, true, nil
}

// CreateOAuthRequest stores a pending authorization request. maxPending bounds
// how many approval prompts can be outstanding at once.
func (s *Store) CreateOAuthRequest(ctx context.Context, request OAuthRequest, maxPending int, now time.Time) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := pruneOAuthTx(ctx, tx, now); err != nil {
			return err
		}
		var pending int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM oauth_requests WHERE status = 'pending' AND expires_at > ?
		`, now.Unix()).Scan(&pending); err != nil {
			return fmt.Errorf("count pending oauth requests: %w", err)
		}
		if maxPending > 0 && pending >= maxPending {
			return ErrOAuthLimit
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO oauth_requests(id, browser_hash, client_id, redirect_uri, state, code_challenge, resource,
				approval_code, client_ip, user_agent, status, created_at, expires_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)
		`, request.ID, request.BrowserHash, request.ClientID, request.RedirectURI, request.State, request.CodeChallenge,
			request.Resource, request.ApprovalCode, request.ClientIP, request.UserAgent, now.Unix(), request.ExpiresAt.Unix())
		if err != nil {
			return fmt.Errorf("create oauth request: %w", err)
		}
		return nil
	})
}

func (s *Store) GetOAuthRequest(ctx context.Context, id string) (OAuthRequest, bool, error) {
	return scanOAuthRequest(s.db.QueryRowContext(ctx, oauthRequestSelect+` WHERE id = ?`, id))
}

func (s *Store) GetOAuthRequestByCode(ctx context.Context, codeHash string) (OAuthRequest, bool, error) {
	if codeHash == "" {
		return OAuthRequest{}, false, nil
	}
	return scanOAuthRequest(s.db.QueryRowContext(ctx, oauthRequestSelect+` WHERE code_hash = ?`, codeHash))
}

// DecideOAuthRequest records the approving user's decision. It changes only a
// pending, unexpired request bound to that user and reports whether that
// happened.
func (s *Store) DecideOAuthRequest(ctx context.Context, id string, approve bool, userID int64, now time.Time) (OAuthRequest, bool, error) {
	status := OAuthRequestDenied
	if approve {
		status = OAuthRequestApproved
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE oauth_requests SET status = ?, decided_by = ?
		WHERE id = ? AND status = 'pending' AND expires_at > ? AND bound_user_id = ? AND bound_user_id > 0
	`, status, userID, id, now.Unix(), userID)
	if err != nil {
		return OAuthRequest{}, false, fmt.Errorf("decide oauth request: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return OAuthRequest{}, false, fmt.Errorf("decide oauth request: %w", err)
	}
	request, ok, err := s.GetOAuthRequest(ctx, id)
	if err != nil || !ok {
		return OAuthRequest{}, false, err
	}
	return request, changed == 1, nil
}

// StartOAuthTelegramLogin stores the state, nonce, and PKCE verifier of a
// Telegram sign-in for a pending request, replacing an earlier attempt.
func (s *Store) StartOAuthTelegramLogin(ctx context.Context, id, stateHash, nonce, verifier string, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE oauth_requests SET login_state_hash = ?, login_nonce = ?, login_verifier = ?
		WHERE id = ? AND status = 'pending' AND expires_at > ?
	`, stateHash, nonce, verifier, id, now.Unix())
	if err != nil {
		return false, fmt.Errorf("start oauth telegram login: %w", err)
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

// FinishOAuthTelegramLogin consumes the sign-in state of a pending request
// once and returns its nonce and PKCE verifier.
func (s *Store) FinishOAuthTelegramLogin(ctx context.Context, id, stateHash string, now time.Time) (string, string, bool, error) {
	var nonce, verifier string
	var ok bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT login_nonce, login_verifier FROM oauth_requests
			WHERE id = ? AND login_state_hash = ? AND login_state_hash != '' AND status = 'pending' AND expires_at > ?
		`, id, stateHash, now.Unix()).Scan(&nonce, &verifier)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find oauth telegram login: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE oauth_requests SET login_state_hash = '', login_nonce = '', login_verifier = '' WHERE id = ?
		`, id); err != nil {
			return fmt.Errorf("finish oauth telegram login: %w", err)
		}
		ok = true
		return nil
	})
	return nonce, verifier, ok, err
}

// BindOAuthRequest records the Telegram user who signed in for a pending
// request. Signing in again from the same browser replaces the user.
func (s *Store) BindOAuthRequest(ctx context.Context, id string, userID int64, now time.Time) (bool, error) {
	if userID <= 0 {
		return false, errors.New("bound user is required")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE oauth_requests SET bound_user_id = ?
		WHERE id = ? AND status = 'pending' AND expires_at > ?
	`, userID, id, now.Unix())
	if err != nil {
		return false, fmt.Errorf("bind oauth request: %w", err)
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

// IssueOAuthCode attaches the authorization code to an approved request. It
// succeeds once, and only before the request expires.
func (s *Store) IssueOAuthCode(ctx context.Context, id, codeHash string, expiresAt, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE oauth_requests SET code_hash = ?, code_expires_at = ?
		WHERE id = ? AND status = 'approved' AND code_hash = '' AND expires_at > ?
	`, codeHash, expiresAt.Unix(), id, now.Unix())
	if err != nil {
		return false, fmt.Errorf("issue oauth code: %w", err)
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

// ExchangeOAuthCode consumes an approved code exactly once and creates the
// grant with its first tokens in the same transaction.
func (s *Store) ExchangeOAuthCode(ctx context.Context, requestID, codeHash string, grant OAuthGrant, tokens []OAuthToken, now time.Time) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE oauth_requests SET status = 'exchanged'
			WHERE id = ? AND code_hash = ? AND status = 'approved' AND code_expires_at > ?
		`, requestID, codeHash, now.Unix())
		if err != nil {
			return fmt.Errorf("exchange oauth code: %w", err)
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			return ErrOAuthInvalidGrant
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO oauth_grants(id, client_id, client_name, user_id, resource, created_at, last_used_at)
			VALUES(?, ?, ?, ?, ?, ?, ?)
		`, grant.ID, grant.ClientID, grant.ClientName, grant.UserID, grant.Resource, now.Unix(), now.Unix()); err != nil {
			return fmt.Errorf("create oauth grant: %w", err)
		}
		return insertOAuthTokensTx(ctx, tx, grant.ID, tokens)
	})
}

// RefreshOAuthTokens rotates a refresh token. The presented token is marked
// used and the new pair is stored in the same transaction. Presenting a used
// token after policy.ReuseGrace revokes the grant and returns
// ErrOAuthTokenReuse together with the revoked grant.
func (s *Store) RefreshOAuthTokens(ctx context.Context, refreshHash, clientID string, tokens []OAuthToken, policy OAuthRefreshPolicy, now time.Time) (OAuthGrant, error) {
	var grant OAuthGrant
	reused := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var createdAt, lastUsedAt, expiresAt, usedAt int64
		err := tx.QueryRowContext(ctx, `
			SELECT g.id, g.client_id, g.client_name, g.user_id, g.resource, g.created_at, g.last_used_at, t.expires_at, t.used_at
			FROM oauth_tokens t JOIN oauth_grants g ON g.id = t.grant_id
			WHERE t.token_hash = ? AND t.kind = 'refresh' AND g.revoked_at = 0 AND g.client_id = ?
		`, refreshHash, clientID).Scan(&grant.ID, &grant.ClientID, &grant.ClientName, &grant.UserID, &grant.Resource, &createdAt, &lastUsedAt, &expiresAt, &usedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrOAuthInvalidGrant
		}
		if err != nil {
			return fmt.Errorf("find oauth refresh token: %w", err)
		}
		grant.CreatedAt = time.Unix(createdAt, 0).UTC()
		grant.LastUsedAt = time.Unix(lastUsedAt, 0).UTC()
		if usedAt != 0 {
			if now.Sub(time.Unix(usedAt, 0)) <= policy.ReuseGrace {
				return ErrOAuthInvalidGrant
			}
			reused = true
			return revokeOAuthGrantTx(ctx, tx, grant.ID, now)
		}
		if expiresAt <= now.Unix() || (policy.MaxGrantLifetime > 0 && now.Sub(grant.CreatedAt) > policy.MaxGrantLifetime) {
			return ErrOAuthInvalidGrant
		}
		result, err := tx.ExecContext(ctx, `UPDATE oauth_tokens SET used_at = ? WHERE token_hash = ? AND used_at = 0`, now.Unix(), refreshHash)
		if err != nil {
			return fmt.Errorf("use oauth refresh token: %w", err)
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			return ErrOAuthInvalidGrant
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE grant_id = ? AND expires_at <= ?`, grant.ID, now.Unix()); err != nil {
			return fmt.Errorf("prune oauth grant tokens: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE oauth_grants SET last_used_at = ? WHERE id = ?`, now.Unix(), grant.ID); err != nil {
			return fmt.Errorf("touch oauth grant: %w", err)
		}
		grant.LastUsedAt = now.UTC()
		return insertOAuthTokensTx(ctx, tx, grant.ID, tokens)
	})
	if err == nil && reused {
		return grant, ErrOAuthTokenReuse
	}
	return grant, err
}

// VerifyOAuthAccessToken resolves an unexpired access token of an active grant.
func (s *Store) VerifyOAuthAccessToken(ctx context.Context, tokenHash string, now time.Time) (OAuthGrant, time.Time, bool, error) {
	var grant OAuthGrant
	var createdAt, lastUsedAt, expiresAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT g.id, g.client_id, g.client_name, g.user_id, g.resource, g.created_at, g.last_used_at, t.expires_at
		FROM oauth_tokens t JOIN oauth_grants g ON g.id = t.grant_id
		WHERE t.token_hash = ? AND t.kind = 'access' AND t.expires_at > ? AND g.revoked_at = 0
	`, tokenHash, now.Unix()).Scan(&grant.ID, &grant.ClientID, &grant.ClientName, &grant.UserID, &grant.Resource, &createdAt, &lastUsedAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthGrant{}, time.Time{}, false, nil
	}
	if err != nil {
		return OAuthGrant{}, time.Time{}, false, fmt.Errorf("verify oauth access token: %w", err)
	}
	grant.CreatedAt = time.Unix(createdAt, 0).UTC()
	grant.LastUsedAt = time.Unix(lastUsedAt, 0).UTC()
	if now.Unix()-lastUsedAt >= 60 {
		if _, err := s.db.ExecContext(ctx, `UPDATE oauth_grants SET last_used_at = ? WHERE id = ?`, now.Unix(), grant.ID); err != nil {
			return OAuthGrant{}, time.Time{}, false, fmt.Errorf("touch oauth grant: %w", err)
		}
		grant.LastUsedAt = now.UTC()
	}
	return grant, time.Unix(expiresAt, 0).UTC(), true, nil
}

func (s *Store) ListOAuthGrants(ctx context.Context, userID int64) ([]OAuthGrant, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, client_id, client_name, user_id, resource, created_at, last_used_at
		FROM oauth_grants WHERE user_id = ? AND revoked_at = 0 ORDER BY last_used_at DESC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("list oauth grants: %w", err)
	}
	defer rows.Close()
	var grants []OAuthGrant
	for rows.Next() {
		var grant OAuthGrant
		var createdAt, lastUsedAt int64
		if err := rows.Scan(&grant.ID, &grant.ClientID, &grant.ClientName, &grant.UserID, &grant.Resource, &createdAt, &lastUsedAt); err != nil {
			return nil, fmt.Errorf("scan oauth grant: %w", err)
		}
		grant.CreatedAt = time.Unix(createdAt, 0).UTC()
		grant.LastUsedAt = time.Unix(lastUsedAt, 0).UTC()
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// RevokeOAuthGrant revokes one of the user's grants. Grants of other users are
// reported as not found.
func (s *Store) RevokeOAuthGrant(ctx context.Context, userID int64, grantID string, now time.Time) (OAuthGrant, bool, error) {
	var grant OAuthGrant
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var createdAt, lastUsedAt int64
		err := tx.QueryRowContext(ctx, `
			SELECT id, client_id, client_name, user_id, resource, created_at, last_used_at
			FROM oauth_grants WHERE id = ? AND user_id = ? AND revoked_at = 0
		`, grantID, userID).Scan(&grant.ID, &grant.ClientID, &grant.ClientName, &grant.UserID, &grant.Resource, &createdAt, &lastUsedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrOAuthInvalidGrant
		}
		if err != nil {
			return fmt.Errorf("find oauth grant: %w", err)
		}
		grant.CreatedAt = time.Unix(createdAt, 0).UTC()
		grant.LastUsedAt = time.Unix(lastUsedAt, 0).UTC()
		return revokeOAuthGrantTx(ctx, tx, grantID, now)
	})
	if errors.Is(err, ErrOAuthInvalidGrant) {
		return OAuthGrant{}, false, nil
	}
	return grant, err == nil, err
}

// RevokeOAuthToken revokes the whole grant that owns a token (RFC 7009).
// Tokens of other clients are ignored.
func (s *Store) RevokeOAuthToken(ctx context.Context, tokenHash, clientID string, now time.Time) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var grantID string
		err := tx.QueryRowContext(ctx, `
			SELECT g.id FROM oauth_tokens t JOIN oauth_grants g ON g.id = t.grant_id
			WHERE t.token_hash = ? AND g.client_id = ?
		`, tokenHash, clientID).Scan(&grantID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find oauth token: %w", err)
		}
		return revokeOAuthGrantTx(ctx, tx, grantID, now)
	})
}

const oauthRequestSelect = `
	SELECT id, browser_hash, client_id, redirect_uri, state, code_challenge, resource, approval_code, client_ip, user_agent, status,
		bound_user_id, decided_by, code_hash, code_expires_at, created_at, expires_at
	FROM oauth_requests`

func scanOAuthRequest(row *sql.Row) (OAuthRequest, bool, error) {
	var request OAuthRequest
	var codeExpiresAt, createdAt, expiresAt int64
	err := row.Scan(&request.ID, &request.BrowserHash, &request.ClientID, &request.RedirectURI, &request.State,
		&request.CodeChallenge, &request.Resource, &request.ApprovalCode, &request.ClientIP, &request.UserAgent, &request.Status, &request.BoundUserID, &request.DecidedBy,
		&request.CodeHash, &codeExpiresAt, &createdAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthRequest{}, false, nil
	}
	if err != nil {
		return OAuthRequest{}, false, fmt.Errorf("get oauth request: %w", err)
	}
	request.CodeExpiresAt = time.Unix(codeExpiresAt, 0).UTC()
	request.CreatedAt = time.Unix(createdAt, 0).UTC()
	request.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	return request, true, nil
}

func insertOAuthTokensTx(ctx context.Context, tx *sql.Tx, grantID string, tokens []OAuthToken) error {
	for _, token := range tokens {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO oauth_tokens(token_hash, grant_id, kind, expires_at) VALUES(?, ?, ?, ?)
		`, token.Hash, grantID, token.Kind, token.ExpiresAt.Unix()); err != nil {
			return fmt.Errorf("create oauth token: %w", err)
		}
	}
	return nil
}

func revokeOAuthGrantTx(ctx context.Context, tx *sql.Tx, grantID string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE oauth_grants SET revoked_at = ? WHERE id = ? AND revoked_at = 0`, now.Unix(), grantID); err != nil {
		return fmt.Errorf("revoke oauth grant: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE grant_id = ?`, grantID); err != nil {
		return fmt.Errorf("delete revoked oauth tokens: %w", err)
	}
	return nil
}

// pruneOAuthTx removes finished requests, dead tokens, revoked grants, and
// registrations that never produced a grant within a day.
func pruneOAuthTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	statements := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM oauth_requests WHERE expires_at < ? OR (status IN ('denied','exchanged') AND created_at < ?)`, []any{now.Add(-time.Hour).Unix(), now.Add(-time.Hour).Unix()}},
		{`DELETE FROM oauth_tokens WHERE expires_at < ? OR (used_at != 0 AND used_at < ?)`, []any{now.Unix(), now.Add(-usedTokenRetention).Unix()}},
		{`DELETE FROM oauth_grants WHERE revoked_at != 0 AND revoked_at < ?`, []any{now.Add(-30 * 24 * time.Hour).Unix()}},
		{`DELETE FROM oauth_grants WHERE revoked_at = 0 AND NOT EXISTS (
			SELECT 1 FROM oauth_tokens t WHERE t.grant_id = oauth_grants.id AND t.used_at = 0)`, nil},
		{`DELETE FROM oauth_clients WHERE created_at < ?
			AND NOT EXISTS (SELECT 1 FROM oauth_grants g WHERE g.client_id = oauth_clients.client_id AND g.revoked_at = 0)
			AND NOT EXISTS (SELECT 1 FROM oauth_requests r WHERE r.client_id = oauth_clients.client_id)`, []any{now.Add(-24 * time.Hour).Unix()}},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return fmt.Errorf("prune oauth rows: %w", err)
		}
	}
	return nil
}

func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// RevokeOAuthGrantsForUser revokes every connection of a user, for example on
// logout, including approvals whose code has not been exchanged yet.
func (s *Store) RevokeOAuthGrantsForUser(ctx context.Context, userID int64, now time.Time) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE oauth_requests SET status = 'denied', code_hash = '' WHERE decided_by = ? AND status = 'approved'`, userID); err != nil {
			return fmt.Errorf("deny user oauth approvals: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE grant_id IN (SELECT id FROM oauth_grants WHERE user_id = ?)`, userID); err != nil {
			return fmt.Errorf("delete user oauth tokens: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE oauth_grants SET revoked_at = ? WHERE user_id = ? AND revoked_at = 0`, now.Unix(), userID); err != nil {
			return fmt.Errorf("revoke user oauth grants: %w", err)
		}
		return nil
	})
}
