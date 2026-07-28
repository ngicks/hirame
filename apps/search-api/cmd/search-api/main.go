package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ngicks/hirame/apps/search-api/internal/api"
	"github.com/ngicks/hirame/apps/search-api/internal/config"
)

func main() {
	if err := run(); err != nil {
		slog.Error("search-api exited", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	return api.New(cfg, slog.Default()).Run(ctx)
}
