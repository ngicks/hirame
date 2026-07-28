// Package indexer is the process that watches mountpoints and runs River
// workers (D-011).
//
// It is composition only: it resolves configuration into the ingest, jobs,
// tika, and objstore collaborators, starts them, and stops them. Every decision
// worth testing lives in one of those packages.
package indexer

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"golang.org/x/sync/errgroup"

	"github.com/ngicks/hirame/apps/search-api/internal/config"
	"github.com/ngicks/hirame/apps/search-api/internal/doctype"
	"github.com/ngicks/hirame/apps/search-api/internal/ingest"
	"github.com/ngicks/hirame/apps/search-api/internal/jobs"
	"github.com/ngicks/hirame/apps/search-api/internal/objstore"
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
	"github.com/ngicks/hirame/apps/search-api/internal/tika"
)

// stopTimeout bounds the wait for in-flight jobs once a signal arrives. It is
// generous because a running extraction holds a document open at Tika, and
// killing it only means the whole parse runs again after the restart.
const stopTimeout = 2 * time.Minute

// Indexer owns the filesystem watcher and the River worker pool.
type Indexer struct {
	cfg    config.Config
	logger *slog.Logger
}

// New builds an Indexer. logger must not be nil.
func New(cfg config.Config, logger *slog.Logger) *Indexer {
	return &Indexer{cfg: cfg, logger: logger}
}

// Run blocks until ctx is cancelled. Cancellation is the normal stop path, so
// it is not reported as an error.
func (ix *Indexer) Run(ctx context.Context) error {
	pool, err := ix.openPool(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	filter := doctype.NewFilter(ix.cfg.Watch.IncludeExtensions)

	tikaClient, err := tika.New(tika.Config{
		BaseURL:  ix.cfg.Tika.BaseURL,
		Timeout:  ix.cfg.Tika.Timeout,
		MaxBytes: ix.cfg.Tika.MaxBytes,
	})
	if err != nil {
		return err
	}
	objects, err := objstore.New(objstore.Config{
		Endpoint:        ix.cfg.Storage.Endpoint,
		Region:          ix.cfg.Storage.Region,
		AccessKeyID:     ix.cfg.Storage.AccessKeyID,
		SecretAccessKey: ix.cfg.Storage.SecretAccessKey,
		Bucket:          ix.cfg.Storage.Bucket,
		UsePathStyle:    ix.cfg.Storage.UsePathStyle,
	})
	if err != nil {
		return err
	}

	enqueuer := jobs.NewEnqueuer(ix.cfg.Watch.DebounceInterval)
	ingestStore := ingest.NewPGStore(pool, enqueuer)
	processor := ingest.NewProcessor(ingestStore, filter, ingest.OSOpener{}, ix.logger)

	riverClient, err := ix.newRiverClient(pool, processor, tikaClient, objects, filter)
	if err != nil {
		return err
	}
	// The enqueuer and the workers are mutually dependent, so the client is
	// attached once both exist and before anything can run.
	enqueuer.Bind(riverClient)

	mountpoints, err := registerMountpoints(ctx, pool, ix.cfg.Watch.Mountpoints)
	if err != nil {
		return err
	}

	watcher, err := ingest.DialDaemon(
		ix.cfg.Watch.OverwatchTarget,
		ix.cfg.Watch.OverwatchSocket,
	)
	if err != nil {
		return err
	}
	defer func() { _ = watcher.Close() }()

	reconciler := ingest.New(
		watcher,
		ingestStore,
		filter,
		mountpoints,
		ix.logger,
		ix.cfg.Watch.ReconnectBackoff,
	)

	ix.logger.InfoContext(ctx, "indexer starting",
		slog.Any("mountpoints", ix.cfg.Watch.Mountpoints),
		slog.Int("river_workers", ix.riverWorkers()),
		slog.Int("extract_concurrency", ix.cfg.Concurrency.Extract),
		slog.Int("render_concurrency", ix.cfg.Concurrency.Render),
		slog.String("overwatch", ix.watchTarget()),
	)

	if err := riverClient.Start(ctx); err != nil {
		return fmt.Errorf("start river client: %w", err)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return reconciler.Run(gctx) })
	g.Go(func() error {
		<-gctx.Done()
		// Detached from gctx, which is already done: Stop needs the drain
		// window rather than returning immediately.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(gctx), stopTimeout)
		defer cancel()
		if err := riverClient.Stop(stopCtx); err != nil {
			return fmt.Errorf("stop river client: %w", err)
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		return err
	}

	ix.logger.InfoContext(ctx, "indexer stopped")
	return nil
}

func (ix *Indexer) openPool(ctx context.Context) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(ix.cfg.Database.URL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if ix.cfg.Database.MaxConns > 0 {
		poolCfg.MaxConns = ix.cfg.Database.MaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}

func (ix *Indexer) newRiverClient(
	pool *pgxpool.Pool,
	processor *ingest.Processor,
	tikaClient *tika.Client,
	objects *objstore.Store,
	filter *doctype.Filter,
) (*river.Client[pgx.Tx], error) {
	queries := sqlcgen.New(pool)
	workers := river.NewWorkers()
	river.AddWorker(workers, jobs.NewIngestPathWorker(processor))
	river.AddWorker(workers, jobs.NewExtractWorker(
		queries, tikaClient, filter, ingest.OSOpener{}, ix.logger,
	))
	river.AddWorker(workers, jobs.NewInvalidateThumbnailsWorker(queries, objects, ix.logger))
	river.AddWorker(workers, jobs.NewEvictionMaintenanceWorker(
		queries,
		objects,
		ix.cfg.Storage.MaxObjectAge,
		ix.cfg.Storage.MaxCacheBytes,
		ix.logger,
	))

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger:  ix.logger,
		Workers: workers,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: ix.riverWorkers()},
			// Extraction has a queue of its own so a backlog of slow documents
			// cannot starve invalidation, which is the job that keeps stale
			// thumbnails from being served.
			jobs.QueueExtract: {MaxWorkers: max(ix.cfg.Concurrency.Extract, 1)},
		},
		PeriodicJobs: []*river.PeriodicJob{
			jobs.EvictionPeriodicJob(ix.cfg.Storage.EvictionInterval),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build river client: %w", err)
	}
	return client, nil
}

func (ix *Indexer) riverWorkers() int {
	if ix.cfg.Concurrency.RiverWorkers > 0 {
		return ix.cfg.Concurrency.RiverWorkers
	}
	return max(runtime.NumCPU(), 1)
}

func (ix *Indexer) watchTarget() string {
	if ix.cfg.Watch.OverwatchTarget != "" {
		return ix.cfg.Watch.OverwatchTarget
	}
	return ix.cfg.Watch.OverwatchSocket
}

// registerMountpoints makes each configured root exist as a row and returns the
// identifiers the reconciler resolves paths against.
//
// The roots are cleaned first because every path decision downstream is a
// prefix comparison against them, and a trailing slash or a "." segment would
// quietly stop matching.
func registerMountpoints(
	ctx context.Context,
	pool *pgxpool.Pool,
	roots []string,
) ([]ingest.Mountpoint, error) {
	queries := sqlcgen.New(pool)
	out := make([]ingest.Mountpoint, 0, len(roots))
	for _, root := range roots {
		cleaned := filepath.Clean(root)
		row, err := queries.UpsertMountpoint(ctx, sqlcgen.UpsertMountpointParams{
			RootPath: cleaned,
			Enabled:  true,
		})
		if err != nil {
			return nil, fmt.Errorf("register mountpoint %s: %w", cleaned, err)
		}
		out = append(out, ingest.Mountpoint{ID: row.ID, Root: row.RootPath})
	}
	return out, nil
}
