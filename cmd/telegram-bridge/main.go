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

	"github.com/nextster/telegram-bridge/internal/bot"
	"github.com/nextster/telegram-bridge/internal/codexworker"
	"github.com/nextster/telegram-bridge/internal/config"
	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/media"
	"github.com/nextster/telegram-bridge/internal/monitor"
	"github.com/nextster/telegram-bridge/internal/notify"
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
	if command == "worker" {
		return runWorker(ctx, cfg, flagArgs)
	}
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	cfg.BindFlags(fs)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	switch command {
	case "serve":
		return serve(ctx, cfg)
	case "login":
		return monitor.Login(ctx, cfg, os.Stdin, os.Stdout)
	case "migrate":
		store, err := db.Open(ctx, cfg.DBPath)
		if err != nil {
			return err
		}
		defer store.Close()
		fmt.Fprintf(os.Stdout, "SQLite schema is ready at %s\n", cfg.DBPath)
		return nil
	case "rules-import":
		return importRules(ctx, cfg, os.Stdin, os.Stdout)
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

type projectFlags []string

func (p *projectFlags) String() string { return strings.Join(*p, ",") }
func (p *projectFlags) Set(value string) error {
	*p = append(*p, value)
	return nil
}

func runWorker(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	var projects projectFlags
	var workerID, codexBin string
	var once bool
	fs.Var(&projects, "project", "project mapping slug=/absolute/path (repeatable)")
	fs.StringVar(&workerID, "worker-id", "", "stable worker name")
	fs.StringVar(&codexBin, "codex-bin", "codex", "path to Codex CLI")
	fs.BoolVar(&once, "once", false, "claim at most one job")
	if err := fs.Parse(args); err != nil {
		return err
	}
	parsed, err := codexworker.ParseProjects(projects)
	if err != nil {
		return err
	}
	worker, err := codexworker.New(codexworker.Config{
		BaseURL: cfg.PublicBaseURL, Token: cfg.WorkerToken, WorkerID: workerID,
		Projects: parsed, Poll: 3 * time.Second, CodexBin: codexBin,
	})
	if err != nil {
		return err
	}
	if once {
		claimed, err := worker.RunOnce(ctx)
		if !claimed && err == nil {
			fmt.Fprintln(os.Stdout, "No queued Codex jobs.")
		}
		return err
	}
	return worker.Run(ctx)
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

func importRules(ctx context.Context, cfg config.Config, in io.Reader, out io.Writer) error {
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
	deleted, err := store.ApplyWatchRules(ctx, rules, payload.Delete)
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

	var notifier notify.Notifier = notify.Nop{}
	var systemNotifier notify.SystemNotifier = notify.Nop{}
	var botService *bot.Service
	if cfg.HasBot() {
		botService, err = bot.New(cfg, store)
		if err != nil {
			return err
		}
		notifier = botService
		systemNotifier = botService
	} else {
		log.Print("telegram bot disabled: TELEGRAM_BOT_TOKEN/BOT_TOKEN is not configured")
	}

	var monitorService *monitor.Service
	if cfg.HasTelegramUserAPI() {
		monitorService = monitor.NewService(cfg, store, notifier)
		if botService != nil {
			botService.SetMonitorService(monitorService)
		}
	} else {
		log.Print("telegram user API monitoring disabled: TELEGRAM_API_ID/TELEGRAM_API_HASH are not configured")
	}

	webServer, err := web.New(cfg, store, monitorService, systemNotifier, botService)
	if err != nil {
		return err
	}

	group, ctx := errgroup.WithContext(ctx)
	if monitorService != nil && cfg.HasMCP() {
		mediaService, err := media.New(cfg.Media, store, monitorService)
		if err != nil {
			return err
		}
		defer mediaService.Close()
		webServer.SetMediaService(mediaService)
		group.Go(func() error { return mediaService.Run(ctx) })
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
	if monitorService != nil {
		group.Go(func() error {
			err := monitorService.Run(ctx)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		})
	}

	return group.Wait()
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
  serve    Run web UI, bot polling, and gotd monitor
  login    Authorize Telegram user session for gotd
  migrate  Create or update SQLite schema
  rules-import  Import flexible watch rules from JSON on stdin
  worker   Run the local Codex app-server worker

Environment:
  TELEGRAM_BOT_TOKEN or BOT_TOKEN
  TELEGRAM_API_ID or TG_API_ID
  TELEGRAM_API_HASH or TG_API_HASH
  TELEGRAM_PHONE or TG_PHONE
  TELEGRAM_PASSWORD or TG_PASSWORD
  TELEGRAM_BRIDGE_DB, TELEGRAM_BRIDGE_SESSION, TELEGRAM_BRIDGE_PUBLIC_URL,
  TELEGRAM_BRIDGE_MCP_TOKEN, TELEGRAM_BRIDGE_WORKER_TOKEN,
  TELEGRAM_BRIDGE_NOTIFICATION_TOKEN, TELEGRAM_BRIDGE_NOTIFICATION_CHAT_IDS, PORT`)
}
