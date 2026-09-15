package monitor

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/session"
	"github.com/gotd/td/tg"

	"github.com/nextster/telegram-bridge/internal/config"
	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/media"
)

const (
	aliceID = int64(111)
	bobID   = int64(222)
)

func testVault(t *testing.T) (*db.Store, *SessionVault) {
	t.Helper()
	store, err := db.Open(context.Background(), t.TempDir()+"/manager.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	vault, err := NewSessionVault(store, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return store, vault
}

func TestSessionVaultEncryptsAndBindsSessionsToOwner(t *testing.T) {
	ctx := context.Background()
	store, vault := testVault(t)
	if err := vault.Storage(aliceID).StoreSession(ctx, []byte(`{"auth_key":"alice-secret"}`)); err != nil {
		t.Fatal(err)
	}
	sealed, ok, err := store.LoadTelegramSession(ctx, aliceID)
	if err != nil || !ok || bytes.Contains(sealed, []byte("alice-secret")) {
		t.Fatalf("session stored in plaintext or missing: ok=%v err=%v", ok, err)
	}
	if data, err := vault.Storage(aliceID).LoadSession(ctx); err != nil || string(data) != `{"auth_key":"alice-secret"}` {
		t.Fatalf("round trip data=%q err=%v", data, err)
	}
	if _, err := vault.Storage(bobID).LoadSession(ctx); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("bob loaded a session: %v", err)
	}
	// Copying alice's ciphertext to bob's row must not produce a usable session.
	if err := store.SaveTelegramSession(ctx, bobID, sealed); err != nil {
		t.Fatal(err)
	}
	if _, err := vault.Storage(bobID).LoadSession(ctx); err == nil {
		t.Fatal("a session moved to another owner decrypted successfully")
	}
	otherKey, err := NewSessionVault(store, bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherKey.Storage(aliceID).LoadSession(ctx); err == nil {
		t.Fatal("a session decrypted with the wrong key")
	}
}

func connectedTestAccount(store *db.Store, owner int64) *accountRuntime {
	service := &Service{
		cfg:        config.Config{TelegramAPIID: 1, TelegramAPIHash: "fake"},
		store:      store,
		owner:      owner,
		handler:    NewHandler(store, nil),
		api:        tg.NewClient(nil),
		userID:     owner,
		authorized: true,
	}
	return &accountRuntime{service: service, cancel: func() {}, done: make(chan struct{})}
}

func TestRevokedRuntimeCannotDeleteASessionFromANewLogin(t *testing.T) {
	ctx := context.Background()
	store, vault := testVault(t)
	manager := NewManager(config.Config{TelegramAPIID: 1, TelegramAPIHash: "fake"}, store, nil, vault)
	stale := connectedTestAccount(store, aliceID)
	close(stale.done)
	manager.accounts[aliceID] = stale

	if err := manager.replaceSession(ctx, aliceID, []byte("new session")); err != nil {
		t.Fatal(err)
	}
	if manager.discardRevokedSession(ctx, aliceID, stale) {
		t.Fatal("a replaced runtime deleted the session")
	}
	if data, err := vault.Load(ctx, aliceID); err != nil || string(data) != "new session" {
		t.Fatalf("session after replacement = %q, %v", data, err)
	}

	current := connectedTestAccount(store, aliceID)
	manager.accounts[aliceID] = current
	if !manager.discardRevokedSession(ctx, aliceID, current) {
		t.Fatal("the current revoked runtime kept its session")
	}
	if _, err := vault.Load(ctx, aliceID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("revoked session still stored: %v", err)
	}
}

func TestManagerRoutesOnlyToTheRequestedAccount(t *testing.T) {
	store, vault := testVault(t)
	manager := NewManager(config.Config{TelegramAPIID: 1, TelegramAPIHash: "fake"}, store, nil, vault)
	manager.accounts[aliceID] = connectedTestAccount(store, aliceID)

	if account, err := manager.Account(aliceID); err != nil || account.owner != aliceID {
		t.Fatalf("alice account=%v err=%v", account, err)
	}
	for _, owner := range []int64{bobID, 0, -aliceID} {
		if _, err := manager.Account(owner); !errors.Is(err, ErrNotConnected) {
			t.Fatalf("Account(%d) err=%v", owner, err)
		}
	}
	if status := manager.Status(bobID); status.Authorized || status.UserID != 0 {
		t.Fatalf("bob sees alice's status: %#v", status)
	}
	if live := manager.LiveAccounts(); len(live) != 1 || live[0] != aliceID {
		t.Fatalf("live accounts = %v", live)
	}

	// A runtime whose session reports a different user is never served.
	manager.accounts[bobID] = connectedTestAccount(store, aliceID)
	if _, err := manager.Account(bobID); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("mismatched runtime served for bob: %v", err)
	}
	if sender, err := manager.NotificationSender(context.Background(), bobID); err == nil || sender != nil {
		t.Fatalf("bob got a notification sender: %v", err)
	}

	if err := manager.Download(context.Background(), bobID, media.Attachment{AccountID: aliceID}, &bytes.Buffer{}); err == nil {
		t.Fatal("download of alice's attachment through bob's account was allowed")
	}
	if _, err := manager.Attachment(context.Background(), bobID, "user:1", 1); err == nil {
		t.Fatal("attachment lookup through a disconnected account succeeded")
	}
}
