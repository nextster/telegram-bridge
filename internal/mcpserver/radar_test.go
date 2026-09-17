package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/monitor"
)

const (
	radarAlice = int64(111)
	radarBob   = int64(222)
)

// radarTestAccount imports its owner's chats on SyncDialogs, like the real
// account does from Telegram.
type radarTestAccount struct {
	historyReader
	store *db.Store
	owner int64
	peers []db.MonitorPeer
	syncs int
	days  int
}

func (a *radarTestAccount) SyncDialogs(ctx context.Context) (int, error) {
	a.syncs++
	for _, peer := range a.peers {
		peer.OwnerUserID = a.owner
		if err := a.store.UpsertMonitorPeer(ctx, peer); err != nil {
			return 0, err
		}
	}
	return len(a.peers), nil
}

func (a *radarTestAccount) Backfill(_ context.Context, days int) (monitor.BackfillResult, error) {
	a.days = days
	return monitor.BackfillResult{Peers: 1, Scanned: 10, Matched: 2, Inserted: 1}, nil
}

func newRadarServer(t *testing.T) (*Server, *db.Store, map[int64]*radarTestAccount) {
	t.Helper()
	store, err := db.Open(context.Background(), t.TempDir()+"/radar.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	accounts := map[int64]*radarTestAccount{
		radarAlice: {store: store, owner: radarAlice, peers: []db.MonitorPeer{{PeerType: "channel", PeerID: 500, Title: "Baraholka Tbilisi", Username: "baraholka_tbi", Kind: "channel"}}},
		radarBob:   {store: store, owner: radarBob, peers: []db.MonitorPeer{{PeerType: "channel", PeerID: 600, Title: "Bob Channel", Kind: "channel"}}},
	}
	resolver := map[int64]Monitor{}
	for owner, account := range accounts {
		resolver[owner] = account
	}
	server := New(Options{Accounts: accountsFor(resolver), Radar: store})
	return server, store, accounts
}

func TestRadarToolsManageOnlyTheCallersRadar(t *testing.T) {
	server, store, accounts := newRadarServer(t)
	ctx := context.Background()
	alice, bob := testRequest(radarAlice), testRequest(radarBob)

	// Alice finds the flea market among her chats and watches it.
	_, sources, err := server.listSources(ctx, alice, listSourcesInput{Refresh: true, Query: "baraholka"})
	if err != nil || len(sources.Sources) != 1 || sources.Sources[0].Chat != "channel:500" || sources.Sources[0].Monitored {
		t.Fatalf("alice sources = %+v err=%v", sources, err)
	}
	_, rule, err := server.saveWatchRule(ctx, alice, saveWatchRuleInput{Name: "bike rack", Any: []string{"велобагажник", "bike rack"}, Exclude: []string{"продано"}, Sources: []string{"channel:500"}})
	if err != nil || rule.ID == 0 || rule.Name != "bike rack" || len(rule.Sources) != 1 || rule.Sources[0] != "channel:500" {
		t.Fatalf("saved rule = %+v err=%v", rule, err)
	}
	if enabled, _ := store.IsMonitorPeerEnabled(ctx, radarAlice, "channel", 500); !enabled {
		t.Fatal("a source named on a rule was not turned on")
	}

	// Bob sees none of it and cannot use, change, or delete it.
	if _, rules, err := server.listWatchRules(ctx, bob, listWatchRulesInput{}); err != nil || len(rules.Rules) != 0 {
		t.Fatalf("bob rules = %+v err=%v", rules, err)
	}
	if _, bobSources, err := server.listSources(ctx, bob, listSourcesInput{}); err != nil || len(bobSources.Sources) != 0 {
		t.Fatalf("bob sources before refresh = %+v err=%v", bobSources, err)
	}
	if _, _, err := server.saveWatchRule(ctx, bob, saveWatchRuleInput{Name: "steal", Any: []string{"x"}, Sources: []string{"channel:500"}}); err == nil || !strings.Contains(err.Error(), "100 most recent") {
		t.Fatalf("bob scoped a rule to alice's chat: %v", err)
	}
	if _, _, err := server.setSourceMonitoring(ctx, bob, setSourceMonitoringInput{Chat: "channel:500", Enabled: false}); err == nil {
		t.Fatal("bob changed alice's source")
	}
	if enabled, _ := store.IsMonitorPeerEnabled(ctx, radarAlice, "channel", 500); !enabled {
		t.Fatal("alice's source was turned off by bob")
	}
	for _, value := range []string{"bike rack", "1"} {
		if _, deleted, err := server.deleteWatchRule(ctx, bob, deleteWatchRuleInput{Rule: value}); err != nil || deleted.Deleted {
			t.Fatalf("bob deleted alice's rule %q: %+v err=%v", value, deleted, err)
		}
	}
	if _, rules, _ := server.listWatchRules(ctx, alice, listWatchRulesInput{}); len(rules.Rules) != 1 {
		t.Fatalf("alice rules after bob's attempts = %+v", rules)
	}
	if accounts[radarBob].syncs == 0 {
		t.Fatal("unknown chats were not looked up in bob's own account")
	}

	// Matches belong to their owner.
	if _, _, err := store.RecordEvent(ctx, db.Event{OwnerUserID: radarAlice, SourcePeerType: "channel", SourcePeerID: 500, MessageID: 7, Keyword: "bike rack", Text: "продам велобагажник"}); err != nil {
		t.Fatal(err)
	}
	if _, matches, err := server.listMatches(ctx, alice, listMatchesInput{Rule: "Bike Rack"}); err != nil || len(matches.Matches) != 1 || matches.Matches[0].Chat != "channel:500" {
		t.Fatalf("alice matches = %+v err=%v", matches, err)
	}
	if _, matches, err := server.listMatches(ctx, bob, listMatchesInput{}); err != nil || len(matches.Matches) != 0 {
		t.Fatalf("bob matches = %+v err=%v", matches, err)
	}

	if _, deleted, err := server.deleteWatchRule(ctx, alice, deleteWatchRuleInput{Rule: "bike rack"}); err != nil || !deleted.Deleted {
		t.Fatalf("alice delete = %+v err=%v", deleted, err)
	}
}

func TestRadarAlertsStatusAndScan(t *testing.T) {
	server, store, accounts := newRadarServer(t)
	ctx := context.Background()
	alice := testRequest(radarAlice)

	if _, rules, err := server.listWatchRules(ctx, alice, listWatchRulesInput{}); err != nil || rules.AlertsEnabled {
		t.Fatalf("alerts before /start = %+v err=%v", rules, err)
	}
	if err := store.UpsertSubscriber(ctx, db.Subscriber{ChatID: radarAlice}); err != nil {
		t.Fatal(err)
	}
	if _, rules, _ := server.listWatchRules(ctx, alice, listWatchRulesInput{}); !rules.AlertsEnabled {
		t.Fatal("alerts after /start are reported off")
	}

	for _, days := range []int{0, 31} {
		if _, _, err := server.scanHistory(ctx, alice, scanHistoryInput{Days: days}); err == nil {
			t.Fatalf("scan accepted %d days", days)
		}
	}
	if _, result, err := server.scanHistory(ctx, alice, scanHistoryInput{Days: 7}); err != nil || result.Inserted != 1 || accounts[radarAlice].days != 7 {
		t.Fatalf("scan = %+v err=%v", result, err)
	}
	if accounts[radarBob].days != 0 {
		t.Fatal("scan ran on another account")
	}
	if _, _, err := server.listWatchRules(ctx, testRequest(0), listWatchRulesInput{}); err == nil {
		t.Fatal("an unauthenticated call listed rules")
	}
}
