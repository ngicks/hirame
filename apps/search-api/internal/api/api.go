// Package api is the process that serves the ConnectRPC API (D-011).
//
// It is composition only, mirroring the indexer: it resolves configuration into
// the store, objstore, gahakuclient and service collaborators, mounts them, and
// serves until the context is done. Every decision worth testing lives in one
// of those packages.
package api

import (
	"context"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ngicks/hirame/apps/search-api/internal/config"
	"github.com/ngicks/hirame/apps/search-api/internal/gahakuclient"
	"github.com/ngicks/hirame/apps/search-api/internal/objstore"
	"github.com/ngicks/hirame/apps/search-api/internal/server"
	"github.com/ngicks/hirame/apps/search-api/internal/service"
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
)

// API owns the HTTP server and the collaborators its handlers hold.
type API struct {
	cfg    config.Config
	logger *slog.Logger
}

// New builds an API. logger must not be nil.
func New(cfg config.Config, logger *slog.Logger) *API {
	return &API{cfg: cfg, logger: logger}
}

// Run serves until ctx is done, then drains. Cancellation is the normal stop
// path, so it is not reported as an error.
func (a *API) Run(ctx context.Context) error {
	pool, err := a.openPool(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	objects, err := objstore.New(objstore.Config{
		Endpoint:        a.cfg.Storage.Endpoint,
		Region:          a.cfg.Storage.Region,
		AccessKeyID:     a.cfg.Storage.AccessKeyID,
		SecretAccessKey: a.cfg.Storage.SecretAccessKey,
		Bucket:          a.cfg.Storage.Bucket,
		UsePathStyle:    a.cfg.Storage.UsePathStyle,
	})
	if err != nil {
		return err
	}

	renderer, err := gahakuclient.New(gahakuclient.Config{
		Target:  a.cfg.Gahaku.Target,
		Timeout: a.cfg.Gahaku.Timeout,
	})
	if err != nil {
		return err
	}
	defer func() { _ = renderer.Close() }()

	queries := sqlcgen.New(pool)
	// One limit for both render paths: the knob describes this process's hold
	// on Gahaku, not one service's.
	renderLimit := service.NewRenderLimit(a.cfg.Concurrency.Render)

	handler := server.NewHandler(
		server.Handlers{
			Search:   service.NewSearch(queries),
			Document: service.NewDocument(queries),
			Render:   service.NewRender(queries, renderer, renderLimit),
			Thumbnail: service.NewThumbnail(
				queries, objects, renderer, renderLimit, a.logger,
			),
		},
		connect.WithInterceptors(server.NewLoggingInterceptor(a.logger)),
	)

	a.logger.InfoContext(ctx, "search-api listening",
		slog.String("addr", a.cfg.Server.ListenAddr),
		slog.String("api_prefix", server.APIPrefix),
		slog.Int("render_concurrency", max(a.cfg.Concurrency.Render, 1)),
		slog.String("gahaku", a.cfg.Gahaku.Target),
		slog.String("s3_endpoint", a.cfg.Storage.Endpoint),
	)

	if err := server.Run(ctx, server.Config{
		ListenAddr:        a.cfg.Server.ListenAddr,
		ShutdownTimeout:   a.cfg.Server.ShutdownTimeout,
		ReadHeaderTimeout: a.cfg.Server.ReadHeaderTimeout,
	}, handler); err != nil {
		return err
	}

	a.logger.InfoContext(ctx, "search-api stopped")
	return nil
}

func (a *API) openPool(ctx context.Context) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(a.cfg.Database.URL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if a.cfg.Database.MaxConns > 0 {
		poolCfg.MaxConns = a.cfg.Database.MaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}
