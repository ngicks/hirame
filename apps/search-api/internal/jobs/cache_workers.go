package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
)

// evictionBatch bounds one age pass so a cache that has been unattended for a
// long time is drained over several queries rather than one enormous delete.
const evictionBatch = 500

// eviction is the shared shape of a row both cache workers delete.
type eviction struct {
	ID        int64
	ObjectKey string
	SizeBytes int64
}

// dropEvictions removes objects whose accounting rows are already gone.
//
// Accounting first, objects second, and never the other way round. Once the row
// is committed away nothing can resolve the key: every read path reaches an
// object through thumbnail_cache, so an object still sitting in the bucket is
// unreachable rather than stale, and a crash here costs bytes instead of
// correctness. Deleting the object first would leave the reverse — a row that
// still advertises a cache entry whose bytes are gone, which is a live 404 on
// the read path and keeps counting against the quota.
//
// It reports the keys it could not remove so the caller can fail the job and
// have River bring it back.
func dropEvictions(
	ctx context.Context,
	objects ObjectDeleter,
	rows []eviction,
	logger *slog.Logger,
) error {
	var failed error
	for _, row := range rows {
		if err := objects.Delete(ctx, row.ObjectKey); err != nil {
			logger.ErrorContext(ctx, "delete cached object",
				slog.String("object_key", row.ObjectKey),
				slog.Any("error", err),
			)
			if failed == nil {
				failed = fmt.Errorf("delete object %s: %w", row.ObjectKey, err)
			}
		}
	}
	return failed
}

// InvalidationStore is the database surface the invalidation worker uses.
// *sqlcgen.Queries satisfies it as generated.
type InvalidationStore interface {
	DeleteThumbnailsForStaleContentVersion(
		ctx context.Context, contentVersionID int64,
	) ([]sqlcgen.DeleteThumbnailsForStaleContentVersionRow, error)
}

// EvictionStore is the database surface the maintenance worker uses.
// *sqlcgen.Queries satisfies it as generated.
type EvictionStore interface {
	ThumbnailEvictionCandidatesByAge(
		ctx context.Context, arg sqlcgen.ThumbnailEvictionCandidatesByAgeParams,
	) ([]sqlcgen.ThumbnailEvictionCandidatesByAgeRow, error)
	ThumbnailEvictionCandidatesByQuota(
		ctx context.Context, maxBytes int64,
	) ([]sqlcgen.ThumbnailEvictionCandidatesByQuotaRow, error)
	DeleteThumbnails(ctx context.Context, ids []int64) ([]sqlcgen.DeleteThumbnailsRow, error)
	TotalThumbnailBytes(ctx context.Context) (int64, error)
}

// InvalidateThumbnailsWorker removes the thumbnails of a content version a
// document no longer points at.
type InvalidateThumbnailsWorker struct {
	river.WorkerDefaults[InvalidateThumbnailsArgs]
	queries InvalidationStore
	objects ObjectDeleter
	logger  *slog.Logger
}

// NewInvalidateThumbnailsWorker builds the worker. logger must not be nil.
func NewInvalidateThumbnailsWorker(
	queries InvalidationStore,
	objects ObjectDeleter,
	logger *slog.Logger,
) *InvalidateThumbnailsWorker {
	return &InvalidateThumbnailsWorker{
		queries: queries,
		objects: objects,
		logger:  logger,
	}
}

// Work implements river.Worker.
func (w *InvalidateThumbnailsWorker) Work(
	ctx context.Context,
	job *river.Job[InvalidateThumbnailsArgs],
) error {
	versionID := job.Args.SupersededContentVersionID

	// The query's own guard keeps a version another live document still points
	// at — which deduplicated content makes possible — from being evicted.
	deleted, err := w.queries.DeleteThumbnailsForStaleContentVersion(ctx, versionID)
	if err != nil {
		return fmt.Errorf("delete stale thumbnail rows for version %d: %w", versionID, err)
	}
	if len(deleted) == 0 {
		return nil
	}

	rows := make([]eviction, 0, len(deleted))
	var bytes int64
	for _, row := range deleted {
		rows = append(
			rows,
			eviction{ID: row.ID, ObjectKey: row.ObjectKey, SizeBytes: row.SizeBytes},
		)
		bytes += row.SizeBytes
	}

	w.logger.InfoContext(ctx, "thumbnails invalidated",
		slog.Int64("document_id", job.Args.DocumentID),
		slog.Int64("superseded_content_version_id", versionID),
		slog.Int("objects", len(rows)),
		slog.Int64("bytes", bytes),
	)
	return dropEvictions(ctx, w.objects, rows, w.logger)
}

// EvictionMaintenanceWorker enforces the cache-wide byte quota and the
// per-object maximum age (D-008).
type EvictionMaintenanceWorker struct {
	river.WorkerDefaults[EvictionMaintenanceArgs]
	queries  EvictionStore
	objects  ObjectDeleter
	maxAge   time.Duration
	maxBytes int64
	logger   *slog.Logger
}

// NewEvictionMaintenanceWorker builds the worker. A zero maxAge or maxBytes
// disables that limit. logger must not be nil.
func NewEvictionMaintenanceWorker(
	queries EvictionStore,
	objects ObjectDeleter,
	maxAge time.Duration,
	maxBytes int64,
	logger *slog.Logger,
) *EvictionMaintenanceWorker {
	return &EvictionMaintenanceWorker{
		queries:  queries,
		objects:  objects,
		maxAge:   maxAge,
		maxBytes: maxBytes,
		logger:   logger,
	}
}

// Work implements river.Worker.
//
// Age runs before quota so that expired entries are not counted as the live
// working set the quota pass would otherwise evict around.
func (w *EvictionMaintenanceWorker) Work(
	ctx context.Context,
	_ *river.Job[EvictionMaintenanceArgs],
) error {
	aged, agedBytes, err := w.evictByAge(ctx)
	if err != nil {
		return err
	}
	over, overBytes, err := w.evictByQuota(ctx)
	if err != nil {
		return err
	}

	total, err := w.queries.TotalThumbnailBytes(ctx)
	if err != nil {
		return fmt.Errorf("total thumbnail bytes: %w", err)
	}
	w.logger.InfoContext(ctx, "thumbnail cache maintained",
		slog.Int("evicted_by_age", aged),
		slog.Int64("evicted_by_age_bytes", agedBytes),
		slog.Int("evicted_by_quota", over),
		slog.Int64("evicted_by_quota_bytes", overBytes),
		slog.Int64("cache_bytes", total),
	)
	return nil
}

func (w *EvictionMaintenanceWorker) evictByAge(ctx context.Context) (int, int64, error) {
	if w.maxAge <= 0 {
		return 0, 0, nil
	}
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-w.maxAge), Valid: true}

	var count int
	var bytes int64
	for {
		candidates, err := w.queries.ThumbnailEvictionCandidatesByAge(
			ctx,
			sqlcgen.ThumbnailEvictionCandidatesByAgeParams{
				OlderThan:   cutoff,
				ResultLimit: evictionBatch,
			},
		)
		if err != nil {
			return count, bytes, fmt.Errorf("age eviction candidates: %w", err)
		}
		if len(candidates) == 0 {
			return count, bytes, nil
		}
		ids := make([]int64, 0, len(candidates))
		for _, c := range candidates {
			ids = append(ids, c.ID)
		}
		n, b, err := w.deleteByID(ctx, ids)
		count += n
		bytes += b
		if err != nil {
			return count, bytes, err
		}
		if len(candidates) < evictionBatch {
			return count, bytes, nil
		}
	}
}

func (w *EvictionMaintenanceWorker) evictByQuota(ctx context.Context) (int, int64, error) {
	if w.maxBytes <= 0 {
		return 0, 0, nil
	}
	candidates, err := w.queries.ThumbnailEvictionCandidatesByQuota(ctx, w.maxBytes)
	if err != nil {
		return 0, 0, fmt.Errorf("quota eviction candidates: %w", err)
	}
	if len(candidates) == 0 {
		return 0, 0, nil
	}
	ids := make([]int64, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.ID)
	}
	return w.deleteByID(ctx, ids)
}

func (w *EvictionMaintenanceWorker) deleteByID(
	ctx context.Context,
	ids []int64,
) (int, int64, error) {
	deleted, err := w.queries.DeleteThumbnails(ctx, ids)
	if err != nil {
		return 0, 0, fmt.Errorf("delete thumbnail rows: %w", err)
	}
	rows := make([]eviction, 0, len(deleted))
	var bytes int64
	for _, row := range deleted {
		rows = append(
			rows,
			eviction{ID: row.ID, ObjectKey: row.ObjectKey, SizeBytes: row.SizeBytes},
		)
		bytes += row.SizeBytes
	}
	return len(rows), bytes, dropEvictions(ctx, w.objects, rows, w.logger)
}
