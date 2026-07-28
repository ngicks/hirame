package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ngicks/hirame/apps/search-api/internal/config"
	"github.com/ngicks/hirame/apps/search-api/internal/migratecli"
)

func main() {
	if err := run(); err != nil {
		slog.Error("migrate failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	var opts migratecli.Options
	flag.BoolVar(&opts.DryRun, "dry-run", false,
		"report the migration plan without applying it; fails if anything is pending")
	flag.BoolVar(&opts.Status, "status", false,
		"report the migration plan without applying it; succeeds regardless")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	return migratecli.Run(ctx, cfg, slog.Default(), os.Stdout, opts)
}
