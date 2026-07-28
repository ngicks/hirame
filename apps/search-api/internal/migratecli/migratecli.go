// Package migratecli is the presentation and composition layer of the migrate
// binary: it opens the pool, drives store.Migrator, and renders the result.
//
// It exists so cmd/migrate stays flag parsing only, and so the migration
// runner itself has no opinion about how a Quadlet init container reads its
// output.
package migratecli

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ngicks/hirame/apps/search-api/internal/config"
	"github.com/ngicks/hirame/apps/search-api/internal/store"
)

// Options selects what a run does. The zero value applies pending migrations.
type Options struct {
	// DryRun reports the plan and writes nothing. Combined with the process
	// exit code it is also the readiness check an operator wants before a
	// deploy: nonzero means the schema is not current.
	DryRun bool
	// Status reports the plan and writes nothing, but succeeds whether or not
	// migrations are pending. DryRun takes precedence.
	Status bool
}

// Run opens cfg.Database.URL and applies or reports migrations, writing a
// human-readable plan to out.
func Run(
	ctx context.Context,
	cfg config.Config,
	logger *slog.Logger,
	out io.Writer,
	opts Options,
) error {
	pool, err := openPool(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	migrator, err := store.NewMigrator(pool, logger)
	if err != nil {
		return err
	}

	if opts.DryRun || opts.Status {
		return report(ctx, migrator, out, opts.DryRun)
	}

	applied, err := migrator.Up(ctx)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		_, err = fmt.Fprintln(out, "schema up to date")
		return err
	}
	for _, version := range applied {
		if _, err := fmt.Fprintf(out, "applied %d\n", version); err != nil {
			return err
		}
	}
	return nil
}

func report(ctx context.Context, migrator *store.Migrator, out io.Writer, strict bool) error {
	statuses, riverPending, err := migrator.Status(ctx)
	if err != nil {
		return err
	}

	if riverPending {
		if _, err := fmt.Fprintln(out, "pending  river   (river schema)"); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintln(out, "applied  river   (river schema)"); err != nil {
			return err
		}
	}

	pending := 0
	var drifted []int64
	for _, s := range statuses {
		state := "pending"
		switch {
		case s.Drifted:
			state = "DRIFTED"
			drifted = append(drifted, s.Version)
		case s.Applied:
			state = "applied"
		default:
			pending++
		}
		if _, err := fmt.Fprintf(out, "%-8s %-7d %s\n", state, s.Version, s.Name); err != nil {
			return err
		}
	}

	if len(drifted) > 0 {
		return fmt.Errorf("migrations changed after being applied: %v", drifted)
	}
	if strict && (pending > 0 || riverPending) {
		return fmt.Errorf("%d migration(s) pending", pending+boolToInt(riverPending))
	}
	return nil
}

func openPool(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.Database.URL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if cfg.Database.MaxConns > 0 {
		poolCfg.MaxConns = cfg.Database.MaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	// NewWithConfig is lazy, so without this the first error an init container
	// reports would come from a migration rather than from the connection.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
