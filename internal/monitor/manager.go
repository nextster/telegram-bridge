package monitor

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"

	"github.com/nextster/telegram-bridge/internal/config"
	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/media"
	"github.com/nextster/telegram-bridge/internal/notify"
)

var (
	ErrNotConnected    = errors.New("telegram account is not connected; send /login to the bot")
	ErrAccountMismatch = errors.New("the logged-in Telegram account is not the one that requested the login")
	ErrLoginInProgress = errors.New("a login for this account is already in progress")
	ErrLoginBusy       = errors.New("too many logins in progress; try again in a minute")
)

const (
	maxConcurrentLogins = 5
	accountStartSpacing = 2 * time.Second
	accountRetryMin     = 30 * time.Second
	accountRetryMax     = 5 * time.Minute
)

// SessionVault encrypts Telegram sessions with AES-256-GCM. The owner ID is
// authenticated data, so a ciphertext cannot be moved to another account.
type SessionVault struct {
	store *db.Store
	aead  cipher.AEAD
}

func NewSessionVault(store *db.Store, key []byte) (*SessionVault, error) {
	if len(key) != 32 {
		return nil, errors.New("session key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &SessionVault{store: store, aead: aead}, nil
}

func (v *SessionVault) additionalData(owner int64) []byte {
	return []byte("telegram-bridge-session:" + strconv.FormatInt(owner, 10))
}

func (v *SessionVault) Save(ctx context.Context, owner int64, data []byte) error {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	sealed := v.aead.Seal(nonce, nonce, data, v.additionalData(owner))
	return v.store.SaveTelegramSession(ctx, owner, sealed)
}

func (v *SessionVault) Load(ctx context.Context, owner int64) ([]byte, error) {
	sealed, ok, err := v.store.LoadTelegramSession(ctx, owner)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, session.ErrNotFound
	}
	size := v.aead.NonceSize()
	if len(sealed) < size {
		return nil, errors.New("stored telegram session is corrupt")
	}
	data, err := v.aead.Open(nil, sealed[:size], sealed[size:], v.additionalData(owner))
	if err != nil {
		return nil, errors.New("stored telegram session cannot be decrypted with the configured key")
	}
	return data, nil
}

func (v *SessionVault) Delete(ctx context.Context, owner int64) error {
	return v.store.DeleteTelegramSession(ctx, owner)
}

// Storage adapts the vault to gotd's session storage for one owner.
func (v *SessionVault) Storage(owner int64) session.Storage {
	return vaultStorage{vault: v, owner: owner}
}

type vaultStorage struct {
	vault *SessionVault
	owner int64
}

func (s vaultStorage) LoadSession(ctx context.Context) ([]byte, error) {
	return s.vault.Load(ctx, s.owner)
}

func (s vaultStorage) StoreSession(ctx context.Context, data []byte) error {
	return s.vault.Save(ctx, s.owner, data)
}

type accountRuntime struct {
	service *Service
	cancel  context.CancelFunc
	done    chan struct{}
}

// UserNotifier delivers a private system message to one user.
type UserNotifier interface {
	NotifyUser(ctx context.Context, userID int64, text string) error
}

// Manager owns every connected Telegram account. Accounts are keyed by the
// Telegram user ID, which is also the only identity other components use.
type Manager struct {
	cfg      config.Config
	store    *db.Store
	notifier notify.Notifier
	vault    *SessionVault

	mu       sync.RWMutex
	runCtx   context.Context
	accounts map[int64]*accountRuntime

	loginMu    sync.Mutex
	loginOwner map[int64]bool
}

func NewManager(cfg config.Config, store *db.Store, notifier notify.Notifier, vault *SessionVault) *Manager {
	if notifier == nil {
		notifier = notify.Nop{}
	}
	return &Manager{
		cfg:        cfg,
		store:      store,
		notifier:   notifier,
		vault:      vault,
		accounts:   map[int64]*accountRuntime{},
		loginOwner: map[int64]bool{},
	}
}

// Run starts every stored account and keeps them running until ctx ends.
func (m *Manager) Run(ctx context.Context) error {
	m.mu.Lock()
	m.runCtx = ctx
	m.mu.Unlock()
	owners, err := m.store.ListTelegramSessionOwners(ctx)
	if err != nil {
		return err
	}
	for index, owner := range owners {
		if index > 0 {
			if err := sleepContext(ctx, accountStartSpacing); err != nil {
				break
			}
		}
		m.start(owner)
	}
	<-ctx.Done()
	m.mu.Lock()
	runtimes := make([]*accountRuntime, 0, len(m.accounts))
	for _, runtime := range m.accounts {
		runtimes = append(runtimes, runtime)
	}
	m.mu.Unlock()
	for _, runtime := range runtimes {
		<-runtime.done
	}
	return nil
}

func (m *Manager) start(owner int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runCtx == nil || m.runCtx.Err() != nil {
		return
	}
	if _, running := m.accounts[owner]; running {
		return
	}
	ctx, cancel := context.WithCancel(m.runCtx)
	runtime := &accountRuntime{
		service: newAccountService(m.cfg, m.store, m.notifier, owner, m.vault.Storage(owner)),
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	m.accounts[owner] = runtime
	go m.runAccount(ctx, owner, runtime)
}

func (m *Manager) runAccount(ctx context.Context, owner int64, runtime *accountRuntime) {
	defer close(runtime.done)
	defer func() {
		m.mu.Lock()
		if m.accounts[owner] == runtime {
			delete(m.accounts, owner)
		}
		m.mu.Unlock()
	}()
	delay := accountRetryMin
	for {
		err := runtime.service.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errSessionRevoked) || errors.Is(err, errAccountMismatch) {
			log.Printf("telegram account %d stopped: %v", owner, err)
			if !m.discardRevokedSession(context.WithoutCancel(ctx), owner, runtime) {
				return
			}
			m.notifyUser(context.WithoutCancel(ctx), owner, "Сессия Telegram отключена или устарела. Чтобы снова пользоваться мостом, отправьте /login.")
			return
		}
		if err != nil {
			runtime.service.setLastRunError(err.Error())
			log.Printf("telegram account %d disconnected: %v; retrying in %s", owner, err, delay)
		}
		if sleepContext(ctx, delay) != nil {
			return
		}
		if delay *= 2; delay > accountRetryMax {
			delay = accountRetryMax
		}
	}
}

// discardRevokedSession deletes the session of a runtime that Telegram
// rejected, unless a new login has replaced that runtime in the meantime. The
// check and the delete hold m.mu, as replaceSession does.
func (m *Manager) discardRevokedSession(ctx context.Context, owner int64, runtime *accountRuntime) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.accounts[owner] != runtime {
		return false
	}
	if err := m.vault.Delete(ctx, owner); err != nil {
		log.Printf("delete revoked session %d failed: %v", owner, err)
	}
	return true
}

// replaceSession stores a new session and detaches the running account in
// one step, then starts the account again.
func (m *Manager) replaceSession(ctx context.Context, owner int64, data []byte) error {
	m.mu.Lock()
	err := m.vault.Save(ctx, owner, data)
	runtime, running := m.accounts[owner]
	if err == nil && running {
		delete(m.accounts, owner)
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if running {
		runtime.cancel()
		<-runtime.done
	}
	m.start(owner)
	return nil
}

func (m *Manager) notifyUser(ctx context.Context, owner int64, text string) {
	if notifier, ok := m.notifier.(UserNotifier); ok {
		if err := notifier.NotifyUser(ctx, owner, text); err != nil {
			log.Printf("notify user %d failed: %v", owner, err)
		}
	}
}

// Account returns the connected account of owner.
func (m *Manager) Account(owner int64) (*Service, error) {
	if owner <= 0 {
		return nil, ErrNotConnected
	}
	m.mu.RLock()
	runtime, ok := m.accounts[owner]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrNotConnected
	}
	if _, userID, err := runtime.service.readyAPI(); err != nil || userID != owner {
		return nil, ErrNotConnected
	}
	return runtime.service, nil
}

// Status reports the account of owner only.
func (m *Manager) Status(owner int64) Status {
	status := Status{Configured: m.cfg.HasTelegramUserAPI()}
	m.mu.RLock()
	runtime, ok := m.accounts[owner]
	m.mu.RUnlock()
	if !ok {
		return status
	}
	accountStatus := runtime.service.Status()
	if accountStatus.UserID != 0 && accountStatus.UserID != owner {
		return status
	}
	return accountStatus
}

// Connected reports whether owner has a stored session, even while it is
// reconnecting.
func (m *Manager) Connected(ctx context.Context, owner int64) (bool, error) {
	if owner <= 0 {
		return false, nil
	}
	_, ok, err := m.store.LoadTelegramSession(ctx, owner)
	return ok, err
}

// LiveAccounts lists accounts that can currently reach Telegram.
func (m *Manager) LiveAccounts() []int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	owners := make([]int64, 0, len(m.accounts))
	for owner, runtime := range m.accounts {
		if _, userID, err := runtime.service.readyAPI(); err == nil && userID == owner {
			owners = append(owners, owner)
		}
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i] < owners[j] })
	return owners
}

// Attachment implements media.Source for the given account only.
func (m *Manager) Attachment(ctx context.Context, accountID int64, chat string, id int) (media.Attachment, error) {
	account, err := m.Account(accountID)
	if err != nil {
		return media.Attachment{}, media.Fail("telegram_account_unavailable")
	}
	return account.Attachment(ctx, chat, id)
}

// Download implements media.Source for the given account only.
func (m *Manager) Download(ctx context.Context, accountID int64, expected media.Attachment, output io.Writer) error {
	if expected.AccountID != accountID {
		return media.Fail("telegram_account_changed")
	}
	account, err := m.Account(accountID)
	if err != nil {
		return media.Fail("telegram_account_unavailable")
	}
	return account.Download(ctx, expected, output)
}

func (m *Manager) beginLogin(owner int64) (func(), error) {
	m.loginMu.Lock()
	defer m.loginMu.Unlock()
	if m.loginOwner[owner] {
		return nil, ErrLoginInProgress
	}
	if len(m.loginOwner) >= maxConcurrentLogins {
		return nil, ErrLoginBusy
	}
	m.loginOwner[owner] = true
	return func() {
		m.loginMu.Lock()
		delete(m.loginOwner, owner)
		m.loginMu.Unlock()
	}, nil
}

// Login authorizes a new session for owner. The session is kept in memory
// until Telegram confirms it belongs to owner; a session for any other account
// is logged out and discarded.
func (m *Manager) Login(ctx context.Context, owner int64, opts LoginOptions) (LoginResult, error) {
	if !m.cfg.HasTelegramUserAPI() {
		return LoginResult{}, errors.New("telegram user API is not configured")
	}
	if owner <= 0 {
		return LoginResult{}, errors.New("login owner is required")
	}
	if opts.Prompt == nil {
		return LoginResult{}, errors.New("login prompt is required")
	}
	if _, err := m.Account(owner); err == nil {
		return LoginResult{UserID: owner, AlreadyAuthorized: true}, nil
	}
	done, err := m.beginLogin(owner)
	if err != nil {
		return LoginResult{}, err
	}
	defer done()

	memory := &session.StorageMemory{}
	client := telegram.NewClient(m.cfg.TelegramAPIID, m.cfg.TelegramAPIHash, telegram.Options{
		SessionStorage: memory,
		Device:         clientDevice(),
	})
	var userID int64
	err = client.Run(ctx, func(ctx context.Context) error {
		authenticator := &promptAuthenticator{phone: strings.TrimSpace(opts.Phone), prompt: opts.Prompt}
		if err := auth.NewFlow(authenticator, auth.SendCodeOptions{}).Run(ctx, client.Auth()); err != nil {
			return fmt.Errorf("telegram login flow: %w", err)
		}
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("check Telegram auth status after login: %w", err)
		}
		if !status.Authorized || status.User == nil {
			return errors.New("telegram login did not authorize the session")
		}
		userID = status.User.ID
		if userID != owner {
			if _, logoutErr := client.API().AuthLogOut(ctx); logoutErr != nil {
				log.Printf("log out mismatched login session failed: %v", logoutErr)
			}
			return ErrAccountMismatch
		}
		return nil
	})
	if err != nil {
		return LoginResult{}, err
	}
	data, err := memory.Bytes(nil)
	if err != nil {
		return LoginResult{}, fmt.Errorf("read new telegram session: %w", err)
	}
	if err := m.replaceSession(ctx, owner, data); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{UserID: owner}, nil
}

func (m *Manager) stop(owner int64) {
	m.mu.Lock()
	runtime, ok := m.accounts[owner]
	if ok {
		delete(m.accounts, owner)
	}
	m.mu.Unlock()
	if ok {
		runtime.cancel()
		<-runtime.done
	}
}

// Logout terminates the Telegram session of owner on Telegram's side, deletes
// it, and revokes every MCP connection and API token of that user.
func (m *Manager) Logout(ctx context.Context, owner int64) error {
	if owner <= 0 {
		return errors.New("logout owner is required")
	}
	m.stop(owner)
	m.logOutStoredSession(ctx, owner)
	now := time.Now()
	if err := m.vault.Delete(ctx, owner); err != nil {
		return err
	}
	if err := m.store.RevokeOAuthGrantsForUser(ctx, owner, now); err != nil {
		return err
	}
	return m.store.DeleteAPITokensForUser(ctx, owner)
}

// logOutStoredSession ends the stored session on Telegram's side with a
// short-lived client, so it works whether or not the account was connected.
func (m *Manager) logOutStoredSession(ctx context.Context, owner int64) {
	data, err := m.vault.Load(ctx, owner)
	if err != nil {
		if !errors.Is(err, session.ErrNotFound) {
			log.Printf("load session %d for logout failed: %v", owner, err)
		}
		return
	}
	memory := &session.StorageMemory{}
	if err := memory.StoreSession(ctx, data); err != nil {
		log.Printf("load session %d for logout failed: %v", owner, err)
		return
	}
	logoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client := telegram.NewClient(m.cfg.TelegramAPIID, m.cfg.TelegramAPIHash, telegram.Options{SessionStorage: memory, Device: clientDevice()})
	err = client.Run(logoutCtx, func(ctx context.Context) error {
		_, err := client.API().AuthLogOut(ctx)
		return err
	})
	if err != nil {
		log.Printf("telegram logout for %d failed: %v", owner, err)
	}
}

// ImportLegacySession moves the single-account session file into the vault
// under the account it belongs to. It returns that account's user ID.
func (m *Manager) ImportLegacySession(ctx context.Context) (int64, error) {
	path := strings.TrimSpace(m.cfg.SessionPath)
	if path == "" || !m.cfg.HasTelegramUserAPI() {
		return 0, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read legacy telegram session: %w", err)
	}
	// gotd's file storage keeps the raw session bytes, so the file content is
	// the session itself.
	memory := &session.StorageMemory{}
	if err := memory.StoreSession(ctx, data); err != nil {
		return 0, fmt.Errorf("load legacy telegram session: %w", err)
	}
	client := telegram.NewClient(m.cfg.TelegramAPIID, m.cfg.TelegramAPIHash, telegram.Options{SessionStorage: memory, Device: clientDevice()})
	var owner int64
	err = client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return err
		}
		if status.Authorized && status.User != nil {
			owner = status.User.ID
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("check legacy telegram session: %w", err)
	}
	suffix := ".unauthorized"
	if owner > 0 {
		sessionData, err := memory.Bytes(nil)
		if err != nil {
			return 0, err
		}
		if err := m.vault.Save(ctx, owner, sessionData); err != nil {
			return 0, err
		}
		suffix = ".imported"
	}
	if err := os.Rename(path, path+suffix); err != nil {
		return owner, fmt.Errorf("retire legacy telegram session file: %w", err)
	}
	log.Printf("legacy telegram session file retired as %s%s (account %d)", path, suffix, owner)
	return owner, nil
}

func clientDevice() telegram.DeviceConfig {
	return telegram.DeviceConfig{DeviceModel: "Telegram Bridge", SystemVersion: "server", AppVersion: "1.0"}
}

// NotificationSender returns the connected account of accountID for the
// notification API.
func (m *Manager) NotificationSender(_ context.Context, accountID int64) (notify.NotificationSender, error) {
	account, err := m.Account(accountID)
	if err != nil {
		return nil, err
	}
	return account, nil
}
