package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/sync/errgroup"

	"github.com/nextster/tg-radar/internal/bot"
	"github.com/nextster/tg-radar/internal/config"
	"github.com/nextster/tg-radar/internal/db"
	"github.com/nextster/tg-radar/internal/monitor"
	"github.com/nextster/tg-radar/internal/notify"
	"github.com/nextster/tg-radar/internal/web"
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
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	cfg.BindFlags(fs)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", command)
	}
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

	webServer, err := web.New(cfg, store, monitorService, systemNotifier)
	if err != nil {
		return err
	}

	group, ctx := errgroup.WithContext(ctx)
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
	fmt.Fprintln(os.Stdout, `tg-radar

Commands:
  serve    Run web UI, bot polling, and gotd monitor
  login    Authorize Telegram user session for gotd
  migrate  Create or update SQLite schema

Environment:
  TELEGRAM_BOT_TOKEN or BOT_TOKEN
  TELEGRAM_API_ID or TG_API_ID
  TELEGRAM_API_HASH or TG_API_HASH
  TELEGRAM_PHONE or TG_PHONE
  TELEGRAM_PASSWORD or TG_PASSWORD
  TG_RADAR_DB, TG_RADAR_SESSION, TG_RADAR_PUBLIC_URL, PORT`)
}
