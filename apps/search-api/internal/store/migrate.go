// Package store owns the PostgreSQL schema of the search system: the embedded
// migrations, the runner that applies them, and the sqlc-generated queries in
// ./sqlcgen.
package store

import (
	"cmp"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationLockKey guards the whole run so two init containers racing at pod
// start cannot apply the same version twice. The value is arbitrary but must
// stay fixed; it is this schema's namespace within pg_advisory_lock.
const migrationLockKey int64 = 0x6869_7261_6d65 // "hirame"

// versionTable is created by the runner rather than by a migration: the runner
// has to read it before it can decide whether migration 1 has run.
const versionTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    bigint      PRIMARY KEY,
    name       text        NOT NULL,
    checksum   text        NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
)`

// Migration is one versioned SQL file embedded in the binary.
type Migration struct {
	Version  int64
	Name     string
	Checksum string
	SQL      string
}

// Status describes one migration's state in a database.
type Status struct {
	Migration
	Applied bool
	// Drifted is true when the file's checksum differs from the one recorded
	// at apply time, i.e. an already-applied migration was edited.
	Drifted bool
}

// Migrator applies River's schema and this package's migrations to one
// database. It is safe to run repeatedly: applied versions are skipped.
type Migrator struct {
	pool       *pgxpool.Pool
	logger     *slog.Logger
	migrations []Migration
}

// NewMigrator builds a Migrator over an existing pool. The caller owns the
// pool. logger must not be nil.
func NewMigrator(pool *pgxpool.Pool, logger *slog.Logger) (*Migrator, error) {
	migrations, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	return &Migrator{pool: pool, logger: logger, migrations: migrations}, nil
}

// Migrations returns the embedded migrations in version order.
func (m *Migrator) Migrations() []Migration { return m.migrations }

// Up applies River's schema and then every pending migration, each in its own
// transaction. It returns the versions it applied.
//
// River runs first and outside our advisory lock: rivermigrate takes its own
// transaction and locking path, and nesting the two would only widen the window
// in which a crashed init container holds a lock.
func (m *Migrator) Up(ctx context.Context) ([]int64, error) {
	if err := m.migrateRiver(ctx, false); err != nil {
		return nil, err
	}

	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(
			context.WithoutCancel(ctx),
			"SELECT pg_advisory_unlock($1)",
			migrationLockKey,
		); err != nil {
			m.logger.ErrorContext(ctx, "release migration lock", slog.Any("error", err))
		}
	}()

	if _, err := conn.Exec(ctx, versionTable); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return nil, err
	}

	var run []int64
	for _, mig := range m.migrations {
		prior, ok := applied[mig.Version]
		if ok {
			if prior != mig.Checksum {
				return run, fmt.Errorf(
					"migration %d (%s) changed after it was applied: recorded %s, embedded %s",
					mig.Version, mig.Name, prior, mig.Checksum,
				)
			}
			continue
		}
		if err := m.apply(ctx, conn, mig); err != nil {
			return run, err
		}
		m.logger.InfoContext(ctx, "migration applied",
			slog.Int64("version", mig.Version),
			slog.String("name", mig.Name),
		)
		run = append(run, mig.Version)
	}
	return run, nil
}

// Status reports every embedded migration and whether it is applied, plus
// whether River still has pending migrations. It writes nothing.
func (m *Migrator) Status(ctx context.Context) (migrations []Status, riverPending bool, err error) {
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	applied := map[int64]string{}
	// A database with no schema_migrations table is simply fully pending, so a
	// missing table is not an error here the way it would be during Up.
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('schema_migrations') IS NOT NULL").
		Scan(&exists); err != nil {
		return nil, false, fmt.Errorf("probe schema_migrations: %w", err)
	}
	if exists {
		if applied, err = appliedVersions(ctx, conn); err != nil {
			return nil, false, err
		}
	}

	statuses := make([]Status, 0, len(m.migrations))
	for _, mig := range m.migrations {
		prior, ok := applied[mig.Version]
		statuses = append(statuses, Status{
			Migration: mig,
			Applied:   ok,
			Drifted:   ok && prior != mig.Checksum,
		})
	}

	riverMigrator, err := newRiverMigrator(m.pool, m.logger)
	if err != nil {
		return nil, false, err
	}
	res, err := riverMigrator.Validate(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("validate river schema: %w", err)
	}
	return statuses, !res.OK, nil
}

// DryRun reports what Up would do without writing, in the same order Up would
// apply it.
func (m *Migrator) DryRun(ctx context.Context) ([]Status, bool, error) {
	return m.Status(ctx)
}

func (m *Migrator) apply(ctx context.Context, conn *pgxpool.Conn, mig Migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", mig.Version, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, mig.SQL); err != nil {
		return fmt.Errorf("apply migration %d (%s): %w", mig.Version, mig.Name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
		mig.Version, mig.Name, mig.Checksum,
	); err != nil {
		return fmt.Errorf("record migration %d: %w", mig.Version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %d: %w", mig.Version, err)
	}
	return nil
}

func (m *Migrator) migrateRiver(ctx context.Context, dryRun bool) error {
	riverMigrator, err := newRiverMigrator(m.pool, m.logger)
	if err != nil {
		return err
	}
	res, err := riverMigrator.Migrate(ctx, rivermigrate.DirectionUp, &rivermigrate.MigrateOpts{
		DryRun: dryRun,
	})
	if err != nil {
		return fmt.Errorf("migrate river schema: %w", err)
	}
	for _, v := range res.Versions {
		m.logger.InfoContext(ctx, "river migration applied", slog.Int("version", v.Version))
	}
	return nil
}

func newRiverMigrator(pool *pgxpool.Pool, logger *slog.Logger) (
	*rivermigrate.Migrator[pgx.Tx], error,
) {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("build river migrator: %w", err)
	}
	return migrator, nil
}

func appliedVersions(ctx context.Context, c *pgxpool.Conn) (map[int64]string, error) {
	rows, err := c.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := map[int64]string{}
	for rows.Next() {
		var (
			version  int64
			checksum string
		)
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	return applied, nil
}

// loadMigrations reads the embedded migrations/NNNN_name.sql files in version
// order.
func loadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	migrations := make([]Migration, 0, len(entries))
	seen := map[int64]string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(entry.Name())
		if err != nil {
			return nil, err
		}
		if prior, dup := seen[version]; dup {
			return nil, fmt.Errorf(
				"duplicate migration version %d: %s and %s", version, prior, entry.Name(),
			)
		}
		seen[version] = entry.Name()

		body, err := fs.ReadFile(migrationsFS, path.Join("migrations", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		sum := sha256.Sum256(body)
		migrations = append(migrations, Migration{
			Version:  version,
			Name:     name,
			Checksum: hex.EncodeToString(sum[:]),
			SQL:      string(body),
		})
	}

	// fs.ReadDir sorts lexically, which matches numeric order only while the
	// prefixes are the same width. Sorting on the parsed version instead keeps
	// that from becoming a silent trap at version 10000.
	slices.SortFunc(migrations, func(a, b Migration) int {
		return cmp.Compare(a.Version, b.Version)
	})
	return migrations, nil
}

func parseMigrationName(filename string) (version int64, name string, err error) {
	base := strings.TrimSuffix(filename, ".sql")
	prefix, rest, ok := strings.Cut(base, "_")
	if !ok {
		return 0, "", fmt.Errorf("migration %q: want NNNN_name.sql", filename)
	}
	version, err = strconv.ParseInt(prefix, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("migration %q: bad version prefix: %w", filename, err)
	}
	if version <= 0 {
		return 0, "", fmt.Errorf("migration %q: version must be positive", filename)
	}
	return version, rest, nil
}
