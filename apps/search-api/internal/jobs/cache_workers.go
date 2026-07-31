package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/ngicks/hirame/apps/search-api/internal/objstore"
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
)

// evictionBatch bounds one age pass so a cache that has been unattended for a
// long time is drained over several queries rather than one enormous delete.
const evictionBatch = 500

// drainBatch bounds one pass of the object drain.
const drainBatch = 500

// orphanGrace is how long an object must have sat unreferenced before the sweep
// treats it as an orphan.
//
// The render path stores the object and then writes the row (see
// service.Thumbnail.render), so a brand-new object is legitimately unreferenced
// for as long as that gap lasts. The margin is hours rather than seconds because
// the only cost of waiting is bytes, while collecting too early would delete an
// object a row is about to point at.
const orphanGrace = time.Hour

// PendingDeleteStore is the drain's database surface: the half of two-phase
// removal every cache worker shares. *sqlcgen.Queries satisfies it as generated.
type PendingDeleteStore interface {
	PendingDeleteThumbnails(
		ctx context.Context, resultLimit int32,
	) ([]sqlcgen.PendingDeleteThumbnailsRow, error)
	PurgeThumbnails(ctx context.Context, ids []int64) (int64, error)
}

// drainPending removes the objects of every withdrawn accounting row and purges
// the rows whose objects are gone.
//
// Accounting first, objects second, and never the other way round — but the
// first step marks rather than deletes. Marking withdraws the row from every
// read (GetThumbnail excludes it), so nothing can be served through it, while
// leaving behind the one durable record of which objects are still outstanding.
// A job that dies part way through the bucket therefore comes back to exactly
// the work that is left; deleting the rows outright, as this once did, made the
// retry match nothing and report success while the objects it had failed to
// remove stayed forever.
//
// The drain is not scoped to whatever the caller just marked. Every worker here
// drains everything pending, so the leftovers of a job that was discarded after
// its last attempt are picked up by the next one to run.
func drainPending(
	ctx context.Context,
	queries PendingDeleteStore,
	objects ObjectDeleter,
	logger *slog.Logger,
) (int, int64, error) {
	var (
		count int
		bytes int64
	)
	for {
		rows, err := queries.PendingDeleteThumbnails(ctx, drainBatch)
		if err != nil {
			return count, bytes, fmt.Errorf("list withdrawn thumbnails: %w", err)
		}
		if len(rows) == 0 {
			return count, bytes, nil
		}

		var failed error
		purge := make([]int64, 0, len(rows))
		for _, row := range rows {
			if err := objects.Delete(ctx, row.ObjectKey); err != nil {
				logger.ErrorContext(ctx, "delete cached object",
					slog.String("object_key", row.ObjectKey),
					slog.Any("error", err),
				)
				if failed == nil {
					failed = fmt.Errorf("delete object %s: %w", row.ObjectKey, err)
				}
				continue
			}
			purge = append(purge, row.ID)
			count++
			bytes += row.SizeBytes
		}
		if len(purge) > 0 {
			if _, err := queries.PurgeThumbnails(ctx, purge); err != nil {
				return count, bytes, fmt.Errorf("purge thumbnail rows: %w", err)
			}
		}
		if failed != nil {
			return count, bytes, failed
		}
		if len(rows) < drainBatch {
			return count, bytes, nil
		}
	}
}

// InvalidationStore is the database surface the invalidation worker uses.
// *sqlcgen.Queries satisfies it as generated.
type InvalidationStore interface {
	PendingDeleteStore
	MarkThumbnailsPendingDeleteForStaleContentVersion(
		ctx context.Context, contentVersionID int64,
	) (int64, error)
}

// EvictionStore is the database surface the maintenance worker uses.
// *sqlcgen.Queries satisfies it as generated.
type EvictionStore interface {
	PendingDeleteStore
	ThumbnailEvictionCandidatesByAge(
		ctx context.Context, arg sqlcgen.ThumbnailEvictionCandidatesByAgeParams,
	) ([]sqlcgen.ThumbnailEvictionCandidatesByAgeRow, error)
	ThumbnailEvictionCandidatesByQuota(
		ctx context.Context, maxBytes int64,
	) ([]sqlcgen.ThumbnailEvictionCandidatesByQuotaRow, error)
	MarkThumbnailsPendingDelete(ctx context.Context, ids []int64) (int64, error)
	TotalThumbnailBytes(ctx context.Context) (int64, error)
}

// CacheSweepStore is the database surface the periodic sweep uses.
// *sqlcgen.Queries satisfies it as generated.
type CacheSweepStore interface {
	PendingDeleteStore
	MarkStaleThumbnailsPendingDelete(ctx context.Context) (int64, error)
	ReferencedThumbnailObjectKeys(
		ctx context.Context, objectKeys []string,
	) ([]string, error)
}

// ObjectLister enumerates the cache bucket. *objstore.Store satisfies it.
type ObjectLister interface {
	List(
		ctx context.Context,
		prefix string,
		fn func(context.Context, []objstore.Object) error,
	) error
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
	// at — which deduplicated content makes possible — from being withdrawn.
	marked, err := w.queries.MarkThumbnailsPendingDeleteForStaleContentVersion(ctx, versionID)
	if err != nil {
		return fmt.Errorf("withdraw stale thumbnail rows for version %d: %w", versionID, err)
	}

	count, bytes, drainErr := drainPending(ctx, w.queries, w.objects, w.logger)
	w.logger.InfoContext(ctx, "thumbnails invalidated",
		slog.Int64("document_id", job.Args.DocumentID),
		slog.Int64("superseded_content_version_id", versionID),
		slog.Int64("withdrawn", marked),
		slog.Int("objects", count),
		slog.Int64("bytes", bytes),
	)
	return drainErr
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
// working set the quota pass would otherwise evict around. Both only withdraw
// rows; one drain at the end removes the objects behind everything withdrawn.
func (w *EvictionMaintenanceWorker) Work(
	ctx context.Context,
	_ *river.Job[EvictionMaintenanceArgs],
) error {
	aged, agedBytes, err := w.markByAge(ctx)
	if err != nil {
		return err
	}
	over, overBytes, err := w.markByQuota(ctx)
	if err != nil {
		return err
	}

	count, bytes, drainErr := drainPending(ctx, w.queries, w.objects, w.logger)
	total, err := w.queries.TotalThumbnailBytes(ctx)
	if err != nil {
		// drainErr first when both failed: it names the object still in the
		// bucket, which is the actionable half.
		if drainErr != nil {
			return drainErr
		}
		return fmt.Errorf("total thumbnail bytes: %w", err)
	}
	w.logger.InfoContext(ctx, "thumbnail cache maintained",
		slog.Int("evicted_by_age", aged),
		slog.Int64("evicted_by_age_bytes", agedBytes),
		slog.Int("evicted_by_quota", over),
		slog.Int64("evicted_by_quota_bytes", overBytes),
		slog.Int("objects", count),
		slog.Int64("object_bytes", bytes),
		slog.Int64("cache_bytes", total),
	)
	return drainErr
}

func (w *EvictionMaintenanceWorker) markByAge(ctx context.Context) (int, int64, error) {
	if w.maxAge <= 0 {
		return 0, 0, nil
	}
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-w.maxAge), Valid: true}

	var (
		count int
		bytes int64
	)
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
			bytes += c.SizeBytes
		}
		if err := w.mark(ctx, ids); err != nil {
			return count, bytes, err
		}
		count += len(ids)
		if len(candidates) < evictionBatch {
			return count, bytes, nil
		}
	}
}

func (w *EvictionMaintenanceWorker) markByQuota(ctx context.Context) (int, int64, error) {
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
	var bytes int64
	for _, c := range candidates {
		ids = append(ids, c.ID)
		bytes += c.SizeBytes
	}
	if err := w.mark(ctx, ids); err != nil {
		return 0, 0, err
	}
	return len(ids), bytes, nil
}

func (w *EvictionMaintenanceWorker) mark(ctx context.Context, ids []int64) error {
	if _, err := w.queries.MarkThumbnailsPendingDelete(ctx, ids); err != nil {
		return fmt.Errorf("withdraw thumbnail rows: %w", err)
	}
	return nil
}

// CacheSweepWorker is the backstop behind every other path that removes a
// cached thumbnail (D-008).
//
// It exists because the targeted invalidation on the ingestion path can be
// discarded after its last attempt, and because a render that stored its object
// but failed to record the row leaves bytes nothing accounts for. The sweep
// closes both: it withdraws every row no live document's current version claims,
// drains what is outstanding, and then removes objects the accounting side does
// not know about at all.
type CacheSweepWorker struct {
	river.WorkerDefaults[CacheSweepArgs]
	queries CacheSweepStore
	objects CacheObjects
	logger  *slog.Logger
}

// CacheObjects is the object-store surface the sweep needs: it both enumerates
// the bucket and removes from it.
type CacheObjects interface {
	ObjectDeleter
	ObjectLister
}

// NewCacheSweepWorker builds the worker. logger must not be nil.
func NewCacheSweepWorker(
	queries CacheSweepStore,
	objects CacheObjects,
	logger *slog.Logger,
) *CacheSweepWorker {
	return &CacheSweepWorker{queries: queries, objects: objects, logger: logger}
}

// Work implements river.Worker.
func (w *CacheSweepWorker) Work(ctx context.Context, _ *river.Job[CacheSweepArgs]) error {
	stale, err := w.queries.MarkStaleThumbnailsPendingDelete(ctx)
	if err != nil {
		return fmt.Errorf("withdraw stale thumbnail rows: %w", err)
	}

	count, bytes, drainErr := drainPending(ctx, w.queries, w.objects, w.logger)
	if drainErr != nil {
		// The orphan pass is skipped rather than run against a bucket the drain
		// could not reach: every key would look unreferenced for the wrong
		// reason, and List failing is the likelier outcome anyway.
		w.logger.InfoContext(ctx, "thumbnail cache swept",
			slog.Int64("withdrawn", stale),
			slog.Int("objects", count),
			slog.Int64("bytes", bytes),
		)
		return drainErr
	}

	orphans, err := w.sweepOrphans(ctx)
	w.logger.InfoContext(ctx, "thumbnail cache swept",
		slog.Int64("withdrawn", stale),
		slog.Int("objects", count),
		slog.Int64("bytes", bytes),
		slog.Int("orphans", orphans),
	)
	return err
}

// sweepOrphans removes objects no accounting row can reach.
//
// The whole bucket is enumerated rather than one key prefix: it holds the
// thumbnail cache and nothing else (D-008), so an object outside the current key
// scheme is an orphan of an older one rather than something to preserve.
func (w *CacheSweepWorker) sweepOrphans(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-orphanGrace)

	var removed int
	err := w.objects.List(ctx, "", func(ctx context.Context, page []objstore.Object) error {
		candidates := make([]string, 0, len(page))
		for _, obj := range page {
			if obj.LastModified.After(cutoff) {
				continue
			}
			candidates = append(candidates, obj.Key)
		}
		if len(candidates) == 0 {
			return nil
		}

		referenced, err := w.queries.ReferencedThumbnailObjectKeys(ctx, candidates)
		if err != nil {
			return fmt.Errorf("resolve object keys against accounting: %w", err)
		}
		known := make(map[string]struct{}, len(referenced))
		for _, key := range referenced {
			known[key] = struct{}{}
		}

		for _, key := range candidates {
			if _, ok := known[key]; ok {
				continue
			}
			if err := w.objects.Delete(ctx, key); err != nil {
				return fmt.Errorf("delete orphaned object %s: %w", key, err)
			}
			w.logger.WarnContext(ctx, "orphaned cache object removed",
				slog.String("object_key", key))
			removed++
		}
		return nil
	})
	if err != nil {
		return removed, fmt.Errorf("sweep orphaned cache objects: %w", err)
	}
	return removed, nil
}
