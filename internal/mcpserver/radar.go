package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/monitor"
)

// RadarStore holds watch rules, monitored sources, and matches. Every method
// is scoped to one owner, and the tools always pass the authenticated caller.
type RadarStore interface {
	ListKeywords(ctx context.Context, ownerUserID int64) ([]db.Keyword, error)
	UpsertWatchRule(ctx context.Context, ownerUserID int64, rule db.Keyword) (db.Keyword, error)
	DeleteKeyword(ctx context.Context, ownerUserID int64, value string) (int64, error)
	ListMonitorPeers(ctx context.Context, ownerUserID int64, enabledOnly bool) ([]db.MonitorPeer, error)
	GetMonitorPeer(ctx context.Context, ownerUserID int64, peerType string, peerID int64) (db.MonitorPeer, bool, error)
	SetMonitorPeerEnabled(ctx context.Context, ownerUserID int64, peerType string, peerID int64, enabled bool) error
	ListEvents(ctx context.Context, ownerUserID int64, limit int) ([]db.Event, error)
	IsSubscribed(ctx context.Context, userID int64) (bool, error)
}

// radarAccount is the part of a connected account that refreshes sources and
// scans history.
type radarAccount interface {
	SyncDialogs(ctx context.Context) (int, error)
	Backfill(ctx context.Context, days int) (monitor.BackfillResult, error)
}

const (
	maxScanDays      = 30
	maxMatchTextRune = 1000
)

type watchRule struct {
	ID                  int64      `json:"id"`
	Name                string     `json:"name"`
	Any                 []string   `json:"any,omitempty"`
	All                 []string   `json:"all,omitempty"`
	RequiredAny         [][]string `json:"required_any,omitempty"`
	Prefer              []string   `json:"prefer,omitempty"`
	Exclude             []string   `json:"exclude,omitempty"`
	Note                string     `json:"note,omitempty"`
	ExcludeCompleteBike bool       `json:"exclude_complete_bike,omitempty"`
	Sources             []string   `json:"sources,omitempty" jsonschema:"Chat keys the rule is limited to; empty means every monitored source"`
}

type saveWatchRuleInput struct {
	Name                string     `json:"name" jsonschema:"Rule name; saving a rule with an existing name replaces it"`
	Any                 []string   `json:"any,omitempty" jsonschema:"At least one of these terms must match"`
	All                 []string   `json:"all,omitempty" jsonschema:"Every one of these terms must match"`
	RequiredAny         [][]string `json:"required_any,omitempty" jsonschema:"One term from every group must match; a group may use num>=1000:lm|lumen for a numeric minimum with a unit"`
	Prefer              []string   `json:"prefer,omitempty" jsonschema:"Terms that raise the score without being required"`
	Exclude             []string   `json:"exclude,omitempty" jsonschema:"Any one of these terms suppresses the match"`
	Note                string     `json:"note,omitempty" jsonschema:"Note copied into alerts"`
	ExcludeCompleteBike bool       `json:"exclude_complete_bike,omitempty" jsonschema:"Ignore listings of complete bikes"`
	Sources             []string   `json:"sources,omitempty" jsonschema:"Chat keys such as channel:123 to limit the rule to; empty means every monitored source"`
}

type listWatchRulesInput struct{}

type listWatchRulesOutput struct {
	Rules         []watchRule `json:"rules"`
	AlertsEnabled bool        `json:"alerts_enabled" jsonschema:"Whether the bot sends alerts; the user turns them on with /start in the bot"`
}

type deleteWatchRuleInput struct {
	Rule string `json:"rule" jsonschema:"Rule ID or name"`
}

type deleteWatchRuleOutput struct {
	Deleted bool `json:"deleted"`
}

type listSourcesInput struct {
	Refresh     bool   `json:"refresh,omitempty" jsonschema:"Import the 100 most recent chats from Telegram before listing"`
	EnabledOnly bool   `json:"enabled_only,omitempty" jsonschema:"List only monitored sources"`
	Query       string `json:"query,omitempty" jsonschema:"Optional case-insensitive filter on title, username, or chat key"`
}

type radarSource struct {
	Chat       string `json:"chat"`
	Title      string `json:"title,omitempty"`
	Username   string `json:"username,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Monitored  bool   `json:"monitored"`
	LastScanAt string `json:"last_scan_at,omitempty"`
}

type listSourcesOutput struct {
	Sources []radarSource `json:"sources"`
}

type setSourceMonitoringInput struct {
	Chat    string `json:"chat" jsonschema:"Chat key such as channel:123, from telegram_list_sources or telegram_list_dialogs"`
	Enabled bool   `json:"enabled" jsonschema:"true to monitor the chat for watch rules, false to stop"`
}

type setSourceMonitoringOutput struct {
	Source radarSource `json:"source"`
}

type listMatchesInput struct {
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum number of matches, from 1 to 100; default 20"`
	Rule  string `json:"rule,omitempty" jsonschema:"Optional rule name to filter by"`
}

type radarMatch struct {
	ID          int64  `json:"id"`
	Rule        string `json:"rule"`
	Chat        string `json:"chat"`
	MessageID   int    `json:"message_id"`
	MessageDate string `json:"message_date,omitempty"`
	FoundAt     string `json:"found_at"`
	Reason      string `json:"reason,omitempty"`
	Score       int    `json:"score,omitempty"`
	Text        string `json:"text"`
}

type listMatchesOutput struct {
	Matches []radarMatch `json:"matches"`
}

type scanHistoryInput struct {
	Days int `json:"days" jsonschema:"How many days back to scan monitored sources, from 1 to 30"`
}

type scanHistoryOutput struct {
	Sources  int    `json:"sources"`
	Scanned  int    `json:"scanned"`
	Matched  int    `json:"matched"`
	Inserted int    `json:"inserted"`
	Since    string `json:"since"`
}

func (s *Server) addRadarTools(server *mcp.Server) {
	no := false
	yes := true
	mcp.AddTool(server, &mcp.Tool{
		Name:        "telegram_list_watch_rules",
		Description: "List the keyword radar rules of the logged-in account and whether the bot sends match alerts. Read-only.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.listWatchRules)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "telegram_save_watch_rule",
		Description: "Create or replace a keyword radar rule by name. New messages in monitored sources that match it are recorded and alerted in the bot. Sources listed on the rule are turned on for monitoring. Never sends Telegram messages.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &no, IdempotentHint: true},
	}, s.saveWatchRule)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "telegram_delete_watch_rule",
		Description: "Delete one of the logged-in account's radar rules by ID or name.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes, IdempotentHint: true},
	}, s.deleteWatchRule)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "telegram_list_sources",
		Description: "List chats known to the radar and whether each is monitored. refresh=true first imports the 100 most recent chats from Telegram.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &no, IdempotentHint: true},
	}, s.listSources)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "telegram_set_source_monitoring",
		Description: "Turn radar monitoring of one chat on or off. Unknown chats are imported from the 100 most recent Telegram chats first.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &no, IdempotentHint: true},
	}, s.setSourceMonitoring)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "telegram_list_matches",
		Description: "List recent radar matches of the logged-in account, newest first. Read-only.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.listMatches)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "telegram_scan_history",
		Description: "Scan the last days of monitored sources with the current radar rules and record matches without sending alerts. May take a while.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &no, IdempotentHint: true},
	}, s.scanHistory)
}

func (s *Server) radarOwner(req *mcp.CallToolRequest) (int64, error) {
	if s.radar == nil {
		return 0, errors.New("the radar is not available on this server")
	}
	return principal(req)
}

func (s *Server) radarAccount(req *mcp.CallToolRequest) (radarAccount, error) {
	account, err := s.account(req)
	if err != nil {
		return nil, err
	}
	radar, ok := account.(radarAccount)
	if !ok {
		return nil, errors.New("this Telegram account cannot refresh sources")
	}
	return radar, nil
}

func (s *Server) listWatchRules(ctx context.Context, req *mcp.CallToolRequest, _ listWatchRulesInput) (*mcp.CallToolResult, listWatchRulesOutput, error) {
	owner, err := s.radarOwner(req)
	if err != nil {
		return nil, listWatchRulesOutput{}, err
	}
	keywords, err := s.radar.ListKeywords(ctx, owner)
	if err != nil {
		return nil, listWatchRulesOutput{}, err
	}
	subscribed, err := s.radar.IsSubscribed(ctx, owner)
	if err != nil {
		return nil, listWatchRulesOutput{}, err
	}
	out := listWatchRulesOutput{Rules: make([]watchRule, 0, len(keywords)), AlertsEnabled: subscribed}
	for _, keyword := range keywords {
		out.Rules = append(out.Rules, ruleFromKeyword(keyword))
	}
	return nil, out, nil
}

func (s *Server) saveWatchRule(ctx context.Context, req *mcp.CallToolRequest, input saveWatchRuleInput) (*mcp.CallToolResult, watchRule, error) {
	owner, err := s.radarOwner(req)
	if err != nil {
		return nil, watchRule{}, err
	}
	sources := make([]db.RuleSource, 0, len(input.Sources))
	for _, key := range input.Sources {
		peer, err := s.ownSource(ctx, req, owner, key)
		if err != nil {
			return nil, watchRule{}, err
		}
		sources = append(sources, db.RuleSource{PeerType: peer.PeerType, PeerID: peer.PeerID})
	}
	saved, err := s.radar.UpsertWatchRule(ctx, owner, db.Keyword{
		Phrase:              input.Name,
		AnyTerms:            input.Any,
		AllTerms:            input.All,
		RequiredAnyGroups:   input.RequiredAny,
		PreferredTerms:      input.Prefer,
		ExcludeTerms:        input.Exclude,
		Note:                input.Note,
		ExcludeCompleteBike: input.ExcludeCompleteBike,
		Sources:             sources,
		Enabled:             true,
	})
	if err != nil {
		return nil, watchRule{}, err
	}
	// A rule scoped to a chat only fires while that chat is monitored.
	for _, source := range sources {
		if err := s.radar.SetMonitorPeerEnabled(ctx, owner, source.PeerType, source.PeerID, true); err != nil {
			return nil, watchRule{}, err
		}
	}
	return nil, ruleFromKeyword(saved), nil
}

func (s *Server) deleteWatchRule(ctx context.Context, req *mcp.CallToolRequest, input deleteWatchRuleInput) (*mcp.CallToolResult, deleteWatchRuleOutput, error) {
	owner, err := s.radarOwner(req)
	if err != nil {
		return nil, deleteWatchRuleOutput{}, err
	}
	rows, err := s.radar.DeleteKeyword(ctx, owner, input.Rule)
	if err != nil {
		return nil, deleteWatchRuleOutput{}, err
	}
	return nil, deleteWatchRuleOutput{Deleted: rows > 0}, nil
}

func (s *Server) listSources(ctx context.Context, req *mcp.CallToolRequest, input listSourcesInput) (*mcp.CallToolResult, listSourcesOutput, error) {
	owner, err := s.radarOwner(req)
	if err != nil {
		return nil, listSourcesOutput{}, err
	}
	if input.Refresh {
		account, err := s.radarAccount(req)
		if err != nil {
			return nil, listSourcesOutput{}, err
		}
		if _, err := account.SyncDialogs(ctx); err != nil {
			return nil, listSourcesOutput{}, err
		}
	}
	peers, err := s.radar.ListMonitorPeers(ctx, owner, input.EnabledOnly)
	if err != nil {
		return nil, listSourcesOutput{}, err
	}
	query := strings.ToLower(strings.TrimSpace(input.Query))
	out := listSourcesOutput{Sources: []radarSource{}}
	for _, peer := range peers {
		source := sourceFromPeer(peer)
		if query != "" && !strings.Contains(strings.ToLower(source.Chat+" "+source.Title+" "+source.Username), query) {
			continue
		}
		out.Sources = append(out.Sources, source)
	}
	return nil, out, nil
}

func (s *Server) setSourceMonitoring(ctx context.Context, req *mcp.CallToolRequest, input setSourceMonitoringInput) (*mcp.CallToolResult, setSourceMonitoringOutput, error) {
	owner, err := s.radarOwner(req)
	if err != nil {
		return nil, setSourceMonitoringOutput{}, err
	}
	peer, err := s.ownSource(ctx, req, owner, input.Chat)
	if err != nil {
		return nil, setSourceMonitoringOutput{}, err
	}
	if err := s.radar.SetMonitorPeerEnabled(ctx, owner, peer.PeerType, peer.PeerID, input.Enabled); err != nil {
		return nil, setSourceMonitoringOutput{}, err
	}
	peer.Enabled = input.Enabled
	return nil, setSourceMonitoringOutput{Source: sourceFromPeer(peer)}, nil
}

func (s *Server) listMatches(ctx context.Context, req *mcp.CallToolRequest, input listMatchesInput) (*mcp.CallToolResult, listMatchesOutput, error) {
	owner, err := s.radarOwner(req)
	if err != nil {
		return nil, listMatchesOutput{}, err
	}
	limit := input.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	fetch := limit
	rule := strings.TrimSpace(input.Rule)
	if rule != "" {
		fetch = 500
	}
	events, err := s.radar.ListEvents(ctx, owner, fetch)
	if err != nil {
		return nil, listMatchesOutput{}, err
	}
	out := listMatchesOutput{Matches: []radarMatch{}}
	for _, event := range events {
		if rule != "" && !strings.EqualFold(event.Keyword, rule) {
			continue
		}
		match := radarMatch{
			ID:        event.ID,
			Rule:      event.Keyword,
			Chat:      event.SourcePeerType + ":" + strconv.FormatInt(event.SourcePeerID, 10),
			MessageID: event.MessageID,
			FoundAt:   event.CreatedAt.UTC().Format(time.RFC3339),
			Reason:    event.MatchReason,
			Score:     event.MatchScore,
			Text:      truncateRunes(event.Text, maxMatchTextRune),
		}
		if !event.MessageDate.IsZero() {
			match.MessageDate = event.MessageDate.UTC().Format(time.RFC3339)
		}
		out.Matches = append(out.Matches, match)
		if len(out.Matches) == limit {
			break
		}
	}
	return nil, out, nil
}

func (s *Server) scanHistory(ctx context.Context, req *mcp.CallToolRequest, input scanHistoryInput) (*mcp.CallToolResult, scanHistoryOutput, error) {
	if _, err := s.radarOwner(req); err != nil {
		return nil, scanHistoryOutput{}, err
	}
	if input.Days < 1 || input.Days > maxScanDays {
		return nil, scanHistoryOutput{}, fmt.Errorf("days must be from 1 to %d", maxScanDays)
	}
	account, err := s.radarAccount(req)
	if err != nil {
		return nil, scanHistoryOutput{}, err
	}
	result, err := account.Backfill(ctx, input.Days)
	if err != nil {
		return nil, scanHistoryOutput{}, err
	}
	return nil, scanHistoryOutput{
		Sources:  result.Peers,
		Scanned:  result.Scanned,
		Matched:  result.Matched,
		Inserted: result.Inserted,
		Since:    result.Since.UTC().Format(time.RFC3339),
	}, nil
}

// ownSource returns one of the owner's radar sources by chat key, importing
// recent chats from Telegram once if it is not known yet.
func (s *Server) ownSource(ctx context.Context, req *mcp.CallToolRequest, owner int64, key string) (db.MonitorPeer, error) {
	peerType, rawID, ok := strings.Cut(strings.TrimSpace(key), ":")
	peerID, err := strconv.ParseInt(rawID, 10, 64)
	if !ok || peerType == "" || err != nil || peerID <= 0 {
		return db.MonitorPeer{}, fmt.Errorf("invalid chat key %q; use keys such as channel:123 from telegram_list_sources", key)
	}
	peer, found, err := s.radar.GetMonitorPeer(ctx, owner, peerType, peerID)
	if err != nil || found {
		return peer, err
	}
	account, err := s.radarAccount(req)
	if err != nil {
		return db.MonitorPeer{}, err
	}
	if _, err := account.SyncDialogs(ctx); err != nil {
		return db.MonitorPeer{}, err
	}
	peer, found, err = s.radar.GetMonitorPeer(ctx, owner, peerType, peerID)
	if err != nil {
		return db.MonitorPeer{}, err
	}
	if !found {
		return db.MonitorPeer{}, fmt.Errorf("chat %s is not among your 100 most recent Telegram chats; open it in Telegram and try again", key)
	}
	return peer, nil
}

func ruleFromKeyword(keyword db.Keyword) watchRule {
	rule := watchRule{
		ID:                  keyword.ID,
		Name:                keyword.Phrase,
		Any:                 keyword.AnyTerms,
		All:                 keyword.AllTerms,
		RequiredAny:         keyword.RequiredAnyGroups,
		Prefer:              keyword.PreferredTerms,
		Exclude:             keyword.ExcludeTerms,
		Note:                keyword.Note,
		ExcludeCompleteBike: keyword.ExcludeCompleteBike,
	}
	for _, source := range keyword.Sources {
		rule.Sources = append(rule.Sources, source.PeerType+":"+strconv.FormatInt(source.PeerID, 10))
	}
	return rule
}

func sourceFromPeer(peer db.MonitorPeer) radarSource {
	source := radarSource{
		Chat:      peer.PeerType + ":" + strconv.FormatInt(peer.PeerID, 10),
		Title:     peer.Title,
		Username:  peer.Username,
		Kind:      peer.Kind,
		Monitored: peer.Enabled,
	}
	if !peer.LastBackfillAt.IsZero() {
		source.LastScanAt = peer.LastBackfillAt.UTC().Format(time.RFC3339)
	}
	return source
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}
