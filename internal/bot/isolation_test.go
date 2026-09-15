package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"

	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/monitor"
)

const (
	aliceUser = int64(111)
	bobUser   = int64(222)
)

// fakeAccounts marks which users have a connected Telegram account.
type fakeAccounts struct {
	connected map[int64]bool
	loggedOut []int64
}

func (f *fakeAccounts) Status(owner int64) monitor.Status {
	return monitor.Status{Configured: true, Authorized: f.connected[owner], UserID: owner}
}

func (f *fakeAccounts) Connected(_ context.Context, owner int64) (bool, error) {
	return f.connected[owner], nil
}

func (f *fakeAccounts) Logout(_ context.Context, owner int64) error {
	f.loggedOut = append(f.loggedOut, owner)
	delete(f.connected, owner)
	return nil
}

func command(from int64, text string) telego.Update {
	return startMessage(from, from, text)
}

func sentTo(calls []telegramCall) map[float64][]string {
	out := map[float64][]string{}
	for _, call := range calls {
		if call.Method != "sendMessage" {
			continue
		}
		chat, _ := call.Body["chat_id"].(float64)
		text, _ := call.Body["text"].(string)
		out[chat] = append(out[chat], text)
	}
	return out
}

func TestCommandsActOnlyOnTheSendersOwnData(t *testing.T) {
	service, store, api := newOAuthTestService(t)
	service.accounts = &fakeAccounts{connected: map[int64]bool{aliceUser: true, bobUser: true}}
	ctx := context.Background()

	for _, update := range []telego.Update{
		command(aliceUser, "/add desk lamp"),
		command(bobUser, "/keywords"),
		command(bobUser, "/del desk lamp"),
		command(bobUser, "/del 1"),
	} {
		if err := service.handleUpdate(ctx, update); err != nil {
			t.Fatal(err)
		}
	}
	if rules, err := store.ListKeywords(ctx, aliceUser); err != nil || len(rules) != 1 {
		t.Fatalf("bob changed alice's rules: %#v err=%v", rules, err)
	}
	for chat, texts := range sentTo(api.take()) {
		if chat == float64(bobUser) {
			for _, text := range texts {
				if strings.Contains(text, "desk lamp") {
					t.Fatalf("bob saw alice's rule: %q", text)
				}
			}
		}
	}

	group := startMessage(aliceUser, -100555, "/keywords")
	group.Message.Chat.Type = "supergroup"
	if err := service.handleUpdate(ctx, group); err != nil {
		t.Fatal(err)
	}
	calls := api.take()
	if len(calls) != 1 || !strings.Contains(calls[0].Body["text"].(string), "только в личном чате") {
		t.Fatalf("group command calls = %#v", calls)
	}
}

func TestAlertsAndStopButtonsBelongToTheEventOwner(t *testing.T) {
	service, store, api := newOAuthTestService(t)
	service.accounts = &fakeAccounts{connected: map[int64]bool{aliceUser: true, bobUser: true}}
	ctx := context.Background()
	for _, user := range []int64{aliceUser, bobUser} {
		if err := store.UpsertSubscriber(ctx, db.Subscriber{ChatID: user}); err != nil {
			t.Fatal(err)
		}
	}
	rule, err := store.UpsertWatchRule(ctx, aliceUser, db.Keyword{Phrase: "lamp", AnyTerms: []string{"lamp"}})
	if err != nil {
		t.Fatal(err)
	}
	event, _, err := store.RecordEvent(ctx, db.Event{OwnerUserID: aliceUser, SourcePeerType: "channel", SourcePeerID: 5, MessageID: 9, Text: "lamp for sale", Keyword: "lamp", RuleID: rule.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.NotifyEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	sent := sentTo(api.take())
	if len(sent[float64(aliceUser)]) != 1 || len(sent[float64(bobUser)]) != 0 {
		t.Fatalf("alert recipients = %#v, want only alice", sent)
	}

	if err := service.handleUpdate(ctx, oauthCallback(bobUser, bobUser, "kwdel:"+jsonNumber(event.ID))); err != nil {
		t.Fatal(err)
	}
	if rules, err := store.ListKeywords(ctx, aliceUser); err != nil || len(rules) != 1 {
		t.Fatalf("bob's stop button deleted alice's rule: %#v err=%v", rules, err)
	}
	if err := service.handleUpdate(ctx, oauthCallback(aliceUser, aliceUser, "kwdel:"+jsonNumber(event.ID))); err != nil {
		t.Fatal(err)
	}
	if rules, err := store.ListKeywords(ctx, aliceUser); err != nil || len(rules) != 0 {
		t.Fatalf("alice could not stop her own rule: %#v err=%v", rules, err)
	}

	if err := store.DeleteSubscriber(ctx, aliceUser); err != nil {
		t.Fatal(err)
	}
	api.take()
	if err := service.NotifyDeletedMessages(ctx, db.PrivateMessageDeletion{
		Dialog:   db.PrivateDialog{OwnerUserID: bobUser, PeerID: 7, Title: "Friend"},
		Messages: []db.PrivateMessage{{OwnerUserID: bobUser, MessageID: 1, Text: "secret"}},
	}); err != nil {
		t.Fatal(err)
	}
	if sent := sentTo(api.take()); len(sent) != 1 || len(sent[float64(bobUser)]) != 1 {
		t.Fatalf("deletion alert recipients = %#v, want only bob", sent)
	}
}

func TestConnectionsAndLogoutAreScopedToTheUser(t *testing.T) {
	service, store, api := newOAuthTestService(t)
	accounts := &fakeAccounts{connected: map[int64]bool{aliceUser: true, bobUser: true}}
	service.accounts = accounts
	ctx := context.Background()
	now := time.Now()
	createPendingOAuthRequest(t, store, "alice-req", "Alice Claude")
	if _, err := store.BindOAuthRequest(ctx, "alice-req", aliceUser, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.DecideOAuthRequest(ctx, "alice-req", true, aliceUser, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.IssueOAuthCode(ctx, "alice-req", "code", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if err := store.ExchangeOAuthCode(ctx, "alice-req", "code", db.OAuthGrant{ID: "grant-alice", ClientID: "tbc_alice-req", ClientName: "Alice Claude", UserID: aliceUser, Resource: "r"},
		[]db.OAuthToken{{Hash: "access", Kind: "access", ExpiresAt: now.Add(time.Hour)}}, now); err != nil {
		t.Fatal(err)
	}
	api.take()

	if err := service.handleUpdate(ctx, command(bobUser, "/connections")); err != nil {
		t.Fatal(err)
	}
	for _, text := range sentTo(api.take())[float64(bobUser)] {
		if strings.Contains(text, "Alice Claude") {
			t.Fatalf("bob saw alice's connection: %q", text)
		}
	}
	if err := service.handleUpdate(ctx, oauthCallback(bobUser, bobUser, oauthRevokeCallbackPrefix+"grant-alice")); err != nil {
		t.Fatal(err)
	}
	if grants, err := store.ListOAuthGrants(ctx, aliceUser); err != nil || len(grants) != 1 {
		t.Fatalf("bob revoked alice's connection: %#v err=%v", grants, err)
	}

	if err := service.handleUpdate(ctx, command(bobUser, "/logout")); err != nil {
		t.Fatal(err)
	}
	if len(accounts.loggedOut) != 1 || accounts.loggedOut[0] != bobUser || !accounts.connected[aliceUser] {
		t.Fatalf("logout affected the wrong accounts: %v", accounts.loggedOut)
	}
}

func jsonNumber(value int64) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
