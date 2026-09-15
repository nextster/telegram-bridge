package db

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

const (
	alice = int64(111)
	bob   = int64(222)
)

func openTenancyStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), t.TempDir()+"/tenancy.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestWatchRulesAreIsolatedByOwner(t *testing.T) {
	ctx := context.Background()
	store := openTenancyStore(t)

	aliceRule, err := store.UpsertWatchRule(ctx, alice, Keyword{Phrase: "lamp", AnyTerms: []string{"desk lamp"}})
	if err != nil {
		t.Fatal(err)
	}
	bobRule, err := store.UpsertWatchRule(ctx, bob, Keyword{Phrase: "LAMP", AnyTerms: []string{"floor lamp"}})
	if err != nil {
		t.Fatal(err)
	}
	if aliceRule.ID == bobRule.ID {
		t.Fatal("rules with the same name for different owners share a row")
	}
	loaded, err := store.GetKeywordByPhrase(ctx, alice, "lamp")
	if err != nil || len(loaded.AnyTerms) != 1 || loaded.AnyTerms[0] != "desk lamp" {
		t.Fatalf("bob's rule changed alice's rule: %#v err=%v", loaded, err)
	}

	for _, value := range []string{"lamp", "LAMP"} {
		if deleted, err := store.DeleteKeyword(ctx, bob, value); err != nil || (value == "lamp" && deleted != 1) {
			t.Fatalf("bob delete %q deleted=%d err=%v", value, deleted, err)
		}
	}
	if deleted, err := store.DeleteKeyword(ctx, bob, "1"); err != nil || deleted != 0 {
		t.Fatalf("bob deleted by alice's rule id: deleted=%d err=%v", deleted, err)
	}
	if deleted, err := store.ApplyWatchRules(ctx, bob, nil, []string{"lamp"}); err != nil || deleted != 0 {
		t.Fatalf("bob import deleted alice's rule: deleted=%d err=%v", deleted, err)
	}
	if rules, err := store.ListKeywords(ctx, alice); err != nil || len(rules) != 1 || rules[0].ID != aliceRule.ID {
		t.Fatalf("alice rules = %#v err=%v", rules, err)
	}
	if rules, err := store.ListKeywords(ctx, bob); err != nil || len(rules) != 0 {
		t.Fatalf("bob sees rules %#v err=%v", rules, err)
	}
	if _, err := store.UpsertWatchRule(ctx, 0, Keyword{Phrase: "orphan", AnyTerms: []string{"x"}}); err == nil {
		t.Fatal("rule without owner was accepted")
	}
}

func TestEventsPeersAndStatsAreIsolatedByOwner(t *testing.T) {
	ctx := context.Background()
	store := openTenancyStore(t)

	message := Event{SourcePeerType: "channel", SourcePeerID: 42, MessageID: 7, Text: "desk lamp", Keyword: "lamp"}
	aliceEvent := message
	aliceEvent.OwnerUserID = alice
	bobEvent := message
	bobEvent.OwnerUserID = bob
	aliceStored, inserted, err := store.RecordEvent(ctx, aliceEvent)
	if err != nil || !inserted {
		t.Fatalf("alice event inserted=%v err=%v", inserted, err)
	}
	if _, inserted, err := store.RecordEvent(ctx, bobEvent); err != nil || !inserted {
		t.Fatalf("the same channel message must alert bob too: inserted=%v err=%v", inserted, err)
	}
	if _, ok, err := store.GetEvent(ctx, bob, aliceStored.ID); err != nil || ok {
		t.Fatalf("bob read alice's event: ok=%v err=%v", ok, err)
	}
	if events, err := store.ListEvents(ctx, bob, 10); err != nil || len(events) != 1 || events[0].OwnerUserID != bob {
		t.Fatalf("bob events = %#v err=%v", events, err)
	}
	if _, _, err := store.RecordEvent(ctx, message); err == nil {
		t.Fatal("event without owner was accepted")
	}

	for owner, hash := range map[int64]int64{alice: 1001, bob: 2002} {
		if err := store.UpsertMonitorPeer(ctx, MonitorPeer{OwnerUserID: owner, PeerType: "channel", PeerID: 42, AccessHash: hash, Title: "Market"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetMonitorPeerEnabled(ctx, bob, "channel", 42, true); err != nil {
		t.Fatal(err)
	}
	if enabled, err := store.IsMonitorPeerEnabled(ctx, alice, "channel", 42); err != nil || enabled {
		t.Fatalf("bob enabled alice's source: enabled=%v err=%v", enabled, err)
	}
	if peer, ok, err := store.GetMonitorPeer(ctx, alice, "channel", 42); err != nil || !ok || peer.AccessHash != 1001 {
		t.Fatalf("alice peer = %#v ok=%v err=%v", peer, ok, err)
	}
	if err := store.SetMonitorPeerEnabled(ctx, bob, "channel", 99, true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("enabling an unknown peer err=%v", err)
	}
	if stats, err := store.Stats(ctx, alice); err != nil || stats.Events != 1 || stats.Peers != 1 || stats.EnabledPeers != 0 {
		t.Fatalf("alice stats = %#v err=%v", stats, err)
	}
}

func TestPrivateArchiveCapIsPerOwner(t *testing.T) {
	ctx := context.Background()
	store := openTenancyStore(t)
	for _, owner := range []int64{alice, bob} {
		if err := store.UpsertPrivateDialog(ctx, PrivateDialog{OwnerUserID: owner, PeerID: 5, Title: "Friend"}); err != nil {
			t.Fatal(err)
		}
		for id := 1; id <= 3; id++ {
			if err := store.UpsertPrivateMessage(ctx, PrivateMessage{OwnerUserID: owner, MessageID: id, PeerID: 5, MessageDate: time.Now().UTC(), Text: "hi"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.PrunePrivateArchive(ctx, bob, time.Unix(1, 0), 1); err != nil {
		t.Fatal(err)
	}
	if stats, err := store.PrivateArchiveStats(ctx, alice); err != nil || stats.Messages != 3 {
		t.Fatalf("bob's prune evicted alice's archive: %#v err=%v", stats, err)
	}
	if stats, err := store.PrivateArchiveStats(ctx, bob); err != nil || stats.Messages != 1 {
		t.Fatalf("bob archive after cap = %#v err=%v", stats, err)
	}
}

func TestTokensChatsSessionsAndReceiptsAreIsolated(t *testing.T) {
	ctx := context.Background()
	store := openTenancyStore(t)
	now := time.Now()

	aliceToken, err := store.CreateAPIToken(ctx, alice, APITokenScopeNotify, "ci", "hash-a", now)
	if err != nil {
		t.Fatal(err)
	}
	if owner, ok, err := store.ResolveAPIToken(ctx, "hash-a", APITokenScopeNotify, now); err != nil || !ok || owner != alice {
		t.Fatalf("resolve owner=%d ok=%v err=%v", owner, ok, err)
	}
	if _, ok, err := store.ResolveAPIToken(ctx, "hash-a", APITokenScopeMCP, now); err != nil || ok {
		t.Fatalf("notify token accepted for MCP: ok=%v err=%v", ok, err)
	}
	if err := store.DeleteAPIToken(ctx, bob, aliceToken.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob deleted alice's token: err=%v", err)
	}
	if tokens, err := store.ListAPITokens(ctx, bob); err != nil || len(tokens) != 0 {
		t.Fatalf("bob sees tokens %#v err=%v", tokens, err)
	}

	if err := store.AddNotificationChat(ctx, alice, -1001, "Releases", now); err != nil {
		t.Fatal(err)
	}
	if allowed, err := store.IsNotificationChatAllowed(ctx, bob, -1001); err != nil || allowed {
		t.Fatalf("alice's allowlist applied to bob: allowed=%v err=%v", allowed, err)
	}
	if err := store.RemoveNotificationChat(ctx, bob, -1001); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob removed alice's chat: err=%v", err)
	}

	if err := store.SaveTelegramSession(ctx, alice, []byte("alice-session")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.LoadTelegramSession(ctx, bob); err != nil || ok {
		t.Fatalf("bob loaded a session: ok=%v err=%v", ok, err)
	}

	if _, created, err := store.ReserveNotification(ctx, alice, -1001, "release:1", "digest-a"); err != nil || !created {
		t.Fatalf("alice reserve created=%v err=%v", created, err)
	}
	if _, created, err := store.ReserveNotification(ctx, bob, -1001, "release:1", "digest-b"); err != nil || !created {
		t.Fatalf("bob's event id collided with alice's: created=%v err=%v", created, err)
	}
}

func TestOAuthGrantsAreListedAndRevokedPerUser(t *testing.T) {
	ctx := context.Background()
	store := openTenancyStore(t)
	now := time.Now()
	if err := store.CreateOAuthClient(ctx, OAuthClient{ClientID: "c", RedirectURIs: []string{"http://127.0.0.1/cb"}, AuthMethod: "none"}, 10, now); err != nil {
		t.Fatal(err)
	}
	for _, grant := range []struct {
		request, grant, code string
		user                 int64
	}{{"ra", "ga", "ca", alice}, {"rb", "gb", "cb", bob}} {
		if err := store.CreateOAuthRequest(ctx, OAuthRequest{ID: grant.request, BrowserHash: "b", ClientID: "c", RedirectURI: "http://127.0.0.1/cb", CodeChallenge: "x", Resource: "r", ApprovalCode: "42", ExpiresAt: now.Add(time.Minute)}, 10, now); err != nil {
			t.Fatal(err)
		}
		if _, err := store.BindOAuthRequest(ctx, grant.request, grant.user, now); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.DecideOAuthRequest(ctx, grant.request, true, grant.user, now); err != nil {
			t.Fatal(err)
		}
		if _, err := store.IssueOAuthCode(ctx, grant.request, grant.code, now.Add(time.Minute), now); err != nil {
			t.Fatal(err)
		}
		if err := store.ExchangeOAuthCode(ctx, grant.request, grant.code, OAuthGrant{ID: grant.grant, ClientID: "c", UserID: grant.user, Resource: "r"}, []OAuthToken{{Hash: "t" + grant.grant, Kind: "access", ExpiresAt: now.Add(time.Hour)}}, now); err != nil {
			t.Fatal(err)
		}
	}
	if grants, err := store.ListOAuthGrants(ctx, alice); err != nil || len(grants) != 1 || grants[0].ID != "ga" {
		t.Fatalf("alice grants = %#v err=%v", grants, err)
	}
	if _, revoked, err := store.RevokeOAuthGrant(ctx, bob, "ga", now); err != nil || revoked {
		t.Fatalf("bob revoked alice's grant: revoked=%v err=%v", revoked, err)
	}
	if err := store.RevokeOAuthGrantsForUser(ctx, bob, now); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := store.VerifyOAuthAccessToken(ctx, "tga", now); err != nil || !ok {
		t.Fatalf("revoking bob's grants affected alice: ok=%v err=%v", ok, err)
	}
	if _, _, ok, err := store.VerifyOAuthAccessToken(ctx, "tgb", now); err != nil || ok {
		t.Fatalf("bob's token survived logout revoke: ok=%v err=%v", ok, err)
	}
}

func TestLegacySingleAccountDatabaseIsAssignedToItsAccount(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/legacy.db"
	legacy, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String())
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE update_state (user_id INTEGER PRIMARY KEY, pts INTEGER NOT NULL, qts INTEGER NOT NULL, date INTEGER NOT NULL, seq INTEGER NOT NULL, updated_at TEXT NOT NULL)`,
		`INSERT INTO update_state VALUES (555, 1, 1, 1, 1, '2026-09-01T00:00:00Z')`,
		`CREATE TABLE keywords (id INTEGER PRIMARY KEY AUTOINCREMENT, phrase TEXT NOT NULL COLLATE NOCASE UNIQUE, enabled INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL)`,
		`CREATE TABLE keyword_terms (keyword_id INTEGER NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('any', 'all', 'exclude')), term TEXT NOT NULL COLLATE NOCASE, position INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(keyword_id, kind, term), FOREIGN KEY(keyword_id) REFERENCES keywords(id) ON DELETE CASCADE)`,
		`INSERT INTO keywords(id, phrase, enabled, created_at) VALUES (3, 'lamp', 1, '2026-09-01T00:00:00Z')`,
		`INSERT INTO keyword_terms VALUES (3, 'any', 'desk lamp', 0)`,
		`CREATE TABLE events (id INTEGER PRIMARY KEY AUTOINCREMENT, source_peer_type TEXT NOT NULL, source_peer_id INTEGER NOT NULL, message_id INTEGER NOT NULL, message_date TEXT NOT NULL, text TEXT NOT NULL, keyword TEXT NOT NULL, created_at TEXT NOT NULL, UNIQUE(source_peer_type, source_peer_id, message_id, keyword))`,
		`CREATE TABLE event_match_details (event_id INTEGER PRIMARY KEY, keyword_id INTEGER NOT NULL DEFAULT 0, reason TEXT NOT NULL DEFAULT '', score INTEGER NOT NULL DEFAULT 0, FOREIGN KEY(event_id) REFERENCES events(id) ON DELETE CASCADE)`,
		`INSERT INTO events VALUES (9, 'channel', 42, 7, '2026-09-01T00:00:00Z', 'desk lamp', 'lamp', '2026-09-01T00:00:00Z')`,
		`INSERT INTO event_match_details VALUES (9, 3, 'any: desk lamp', 100)`,
		`CREATE TABLE monitor_peers (peer_type TEXT NOT NULL, peer_id INTEGER NOT NULL, access_hash INTEGER NOT NULL DEFAULT 0, title TEXT NOT NULL DEFAULT '', username TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 0, discovered_at TEXT NOT NULL, updated_at TEXT NOT NULL, last_backfill_at TEXT NOT NULL DEFAULT '', PRIMARY KEY(peer_type, peer_id))`,
		`INSERT INTO monitor_peers VALUES ('channel', 42, 77, 'Market', '', 'channel', 1, '2026-09-01T00:00:00Z', '2026-09-01T00:00:00Z', '')`,
		`CREATE TABLE notification_receipts (chat_id INTEGER NOT NULL, event_id TEXT NOT NULL, digest TEXT NOT NULL, status TEXT NOT NULL CHECK(status IN ('pending', 'sent')), message_id INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, PRIMARY KEY(chat_id, event_id))`,
		`INSERT INTO notification_receipts VALUES (-1001, 'release:1', 'digest', 'sent', 5, '2026-09-01T00:00:00Z')`,
	} {
		if _, err := legacy.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	legacy.Close()

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rule, err := store.GetKeywordByPhrase(ctx, 555, "lamp")
	if err != nil || rule.ID != 3 || len(rule.AnyTerms) != 1 || rule.AnyTerms[0] != "desk lamp" {
		t.Fatalf("migrated rule = %#v err=%v (child terms must survive the rebuild)", rule, err)
	}
	event, ok, err := store.GetEvent(ctx, 555, 9)
	if err != nil || !ok || event.RuleID != 3 || event.MatchScore != 100 {
		t.Fatalf("migrated event = %#v ok=%v err=%v", event, ok, err)
	}
	if enabled, err := store.IsMonitorPeerEnabled(ctx, 555, "channel", 42); err != nil || !enabled {
		t.Fatalf("migrated peer enabled=%v err=%v", enabled, err)
	}
	if receipt, created, err := store.ReserveNotification(ctx, 555, -1001, "release:1", "digest"); err != nil || created || receipt.MessageID != 5 {
		t.Fatalf("migrated receipt = %#v created=%v err=%v", receipt, created, err)
	}
	if _, err := store.UpsertWatchRule(ctx, 555, Keyword{Phrase: "new", AnyTerms: []string{"x"}}); err != nil {
		t.Fatalf("autoincrement after rebuild: %v", err)
	}
	store.Close()
	if reopened, err := Open(ctx, path); err != nil {
		t.Fatalf("second open after migration: %v", err)
	} else {
		reopened.Close()
	}
}
