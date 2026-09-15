package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/nextster/telegram-bridge/internal/apitoken"
	"github.com/nextster/telegram-bridge/internal/bot"
	"github.com/nextster/telegram-bridge/internal/config"
	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/media"
	"github.com/nextster/telegram-bridge/internal/monitor"
	"github.com/nextster/telegram-bridge/internal/notify"
	"github.com/nextster/telegram-bridge/internal/oauth"
	"github.com/nextster/telegram-bridge/internal/web"
)

func main() {
	if err := run(os.Args); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	command, flagArgs := splitCommand(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	cfg.BindFlags(fs)
	owner := fs.Int64("owner", 0, "Telegram user ID that owns imported rules (rules-import)")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	switch command {
	case "serve":
		return serve(ctx, cfg)
	case "migrate":
		store, err := db.Open(ctx, cfg.DBPath)
		if err != nil {
			return err
		}
		defer store.Close()
		fmt.Fprintf(os.Stdout, "SQLite schema is ready at %s\n", cfg.DBPath)
		return nil
	case "rules-import":
		return importRules(ctx, cfg, *owner, os.Stdin, os.Stdout)
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

type ruleImport struct {
	Delete                     []string           `json:"delete"`
	DefaultExclude             []string           `json:"default_exclude"`
	DefaultSources             []ruleImportSource `json:"default_sources"`
	DefaultExcludeCompleteBike bool               `json:"default_exclude_complete_bike"`
	Rules                      []ruleImportItem   `json:"rules"`
}

type ruleImportItem struct {
	Name                string             `json:"name"`
	Any                 []string           `json:"any"`
	All                 []string           `json:"all"`
	RequiredAny         [][]string         `json:"required_any"`
	Prefer              []string           `json:"prefer"`
	Exclude             []string           `json:"exclude"`
	Note                string             `json:"note"`
	ExcludeCompleteBike *bool              `json:"exclude_complete_bike"`
	Sources             []ruleImportSource `json:"sources"`
}

type ruleImportSource struct {
	PeerType string `json:"peer_type"`
	PeerID   int64  `json:"peer_id"`
}

func importRules(ctx context.Context, cfg config.Config, owner int64, in io.Reader, out io.Writer) error {
	if owner <= 0 {
		return errors.New("rules-import needs -owner with the Telegram user ID that owns the rules")
	}
	var payload ruleImport
	decoder := json.NewDecoder(in)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return fmt.Errorf("decode rules import: %w", err)
	}
	store, err := db.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()
	rules := make([]db.Keyword, 0, len(payload.Rules))
	for _, item := range payload.Rules {
		importSources := item.Sources
		if item.Sources == nil {
			importSources = payload.DefaultSources
		}
		sources := make([]db.RuleSource, 0, len(importSources))
		for _, source := range importSources {
			sources = append(sources, db.RuleSource{PeerType: source.PeerType, PeerID: source.PeerID})
		}
		excludeCompleteBike := payload.DefaultExcludeCompleteBike
		if item.ExcludeCompleteBike != nil {
			excludeCompleteBike = *item.ExcludeCompleteBike
		}
		rules = append(rules, db.Keyword{
			Phrase:              item.Name,
			AnyTerms:            item.Any,
			AllTerms:            item.All,
			RequiredAnyGroups:   item.RequiredAny,
			PreferredTerms:      item.Prefer,
			ExcludeTerms:        append(append([]string(nil), payload.DefaultExclude...), item.Exclude...),
			Note:                item.Note,
			ExcludeCompleteBike: excludeCompleteBike,
			Sources:             sources,
			Enabled:             true,
		})
	}
	deleted, err := store.ApplyWatchRules(ctx, owner, rules, payload.Delete)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Imported %d watch rules; deleted %d old rules\n", len(payload.Rules), deleted)
	return nil
}

func serve(ctx context.Context, cfg config.Config) error {
	store, err := db.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()

	var botService *bot.Service
	if cfg.HasBot() {
		botService, err = bot.New(cfg, store)
		if err != nil {
			return err
		}
	} else {
		log.Print("telegram bot disabled: TELEGRAM_BOT_TOKEN/BOT_TOKEN is not configured")
	}

	var manager *monitor.Manager
	if cfg.HasTelegramUserAPI() {
		if err := cfg.ValidateSessionKey(); err != nil {
			return err
		}
		vault, err := monitor.NewSessionVault(store, cfg.SessionKey)
		if err != nil {
			return err
		}
		var notifier notify.Notifier = notify.Nop{}
		if botService != nil {
			notifier = botService
		}
		manager = monitor.NewManager(cfg, store, notifier, vault)
		if botService != nil {
			botService.SetAccounts(manager)
		}
	} else {
		log.Print("telegram user API disabled: TELEGRAM_API_ID/TELEGRAM_API_HASH are not configured")
	}

	// Interfaces are assigned only from non-nil values.
	var accounts web.Accounts
	if manager != nil {
		accounts = manager
	}
	var userNotifier web.UserNotifier
	if botService != nil {
		userNotifier = botService
	}
	webServer, err := web.New(cfg, store, accounts, userNotifier)
	if err != nil {
		return err
	}
	if cfg.OAuthMode == "on" && !cfg.HasOAuth() {
		log.Print("MCP OAuth disabled: it needs TELEGRAM_BOT_TOKEN, TELEGRAM_BRIDGE_PUBLIC_URL and TELEGRAM_LOGIN_CLIENT_SECRET")
	}
	if cfg.HasOAuth() && botService != nil && manager != nil {
		oauthServer, err := oauth.New(cfg.PublicBaseURL, store, botService, oauth.Options{
			ExtraRedirectURIs: cfg.OAuthExtraRedirectURIs,
			TelegramLogin:     oauth.TelegramLogin{ClientID: cfg.TelegramLoginClientID(), ClientSecret: cfg.TelegramLoginSecret},
		})
		if err != nil {
			log.Printf("MCP OAuth disabled: %v", err)
		} else {
			webServer.SetOAuth(oauthServer)
		}
	}

	group, ctx := errgroup.WithContext(ctx)
	if manager != nil {
		mediaService, err := media.New(cfg.Media, store, manager)
		if err != nil {
			return err
		}
		defer mediaService.Close()
		webServer.SetMediaService(mediaService)
		group.Go(func() error { return mediaService.Run(ctx) })
		group.Go(func() error {
			// Importing checks the old session with Telegram, so it runs here
			// rather than delaying the web server and health checks.
			importCtx, cancel := context.WithTimeout(ctx, time.Minute)
			if err := importLegacyAccount(importCtx, cfg, store, manager); err != nil {
				log.Printf("legacy account import failed: %v", err)
			}
			cancel()
			err := manager.Run(ctx)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		})
	}
	group.Go(func() error {
		return webServer.Run(ctx)
	})
	if botService != nil {
		group.Go(func() error {
			err := botService.Run(ctx)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		})
	}

	return group.Wait()
}

// importLegacyAccount moves a single-account deployment into the per-user
// model. The static tokens and group allowlist are given only to the account
// whose session file is imported in this same run, so they can never be
// attached to anybody else later.
func importLegacyAccount(ctx context.Context, cfg config.Config, store *db.Store, manager *monitor.Manager) error {
	owner, err := manager.ImportLegacySession(ctx)
	if owner <= 0 {
		return err
	}
	now := time.Now()
	for scope, token := range map[string]string{db.APITokenScopeMCP: cfg.MCPToken, db.APITokenScopeNotify: cfg.NotificationToken} {
		if token == "" {
			continue
		}
		if _, err := store.CreateAPIToken(ctx, owner, scope, "imported from environment", apitoken.Hash(token), now); err != nil {
			log.Printf("import %s token for account %d failed: %v", scope, owner, err)
		}
	}
	for _, chatID := range cfg.NotificationChatIDs {
		if err := store.AddNotificationChat(ctx, owner, chatID, "", now); err != nil {
			log.Printf("import notification group for account %d failed: %v", owner, err)
		}
	}
	if cfg.MCPToken != "" || cfg.NotificationToken != "" || len(cfg.NotificationChatIDs) > 0 {
		log.Printf("imported legacy tokens and groups for account %d; remove them from the environment", owner)
	}
	return err
}

func splitCommand(args []string) (string, []string) {
	if len(args) < 2 {
		return "serve", nil
	}
	first := args[1]
	if strings.HasPrefix(first, "-") {
		return "serve", args[1:]
	}
	return first, args[2:]
}

func printUsage() {
	fmt.Fprintln(os.Stdout, `telegram-bridge

Commands:
  serve         Run the web UI, bot, MCP server, and Telegram accounts
  migrate       Create or update the SQLite schema
  rules-import  Import watch rules from JSON on stdin: rules-import -owner <telegram user id>

Users connect their own Telegram account with /login in the bot.

Environment:
  TELEGRAM_BOT_TOKEN or BOT_TOKEN
  TELEGRAM_API_ID or TG_API_ID
  TELEGRAM_API_HASH or TG_API_HASH
  TELEGRAM_BRIDGE_SESSION_KEY (32 bytes, openssl rand -base64 32)
  TELEGRAM_BRIDGE_DB, TELEGRAM_BRIDGE_PUBLIC_URL, TELEGRAM_BRIDGE_OAUTH, PORT`)
}
