// Package jobs holds the durable work of the ingestion pipeline: the River job
// definitions, their workers, and the enqueuer that inserts them.
//
// Nothing here decides document identity — that is the ingest package's job.
// This package owns only when work runs, how often it may repeat, and how many
// times it may fail.
package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// QueueExtract carries the Tika calls. It is separate from the default queue so
// extraction concurrency can be capped without also throttling the cheap
// bookkeeping jobs, which would let a backlog of large documents stall
// invalidation.
const QueueExtract = "extract"

// stillOutstanding is the uniqueness window every job here uses: a job is a
// duplicate only while an earlier one has yet to finish.
//
// River's default window also counts `completed`, which would be wrong for all
// of them — a completed extraction would stop the same content version from
// ever being re-extracted, and a completed invalidation would stop a later one
// for the same pair. Dropping `completed` is the whole point of spelling the
// window out.
//
// The four states below are the ones River requires any custom window to
// contain, so `running` cannot be dropped even though counting it leaves a
// window: a change landing while a job for the same key is mid-flight is
// swallowed by the job already reading the older bytes. For ingestion that
// window is covered by the two triggers being separate keys — a write emits
// FAN_MODIFY as well as the close, and a modify arriving during a running
// settled job is a different key, so it still enqueues — and by the rescan that
// re-queues anything whose size or mtime moved.
var stillOutstanding = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRetryable,
	rivertype.JobStateRunning,
	rivertype.JobStateScheduled,
}

// The two values [IngestPathArgs.Trigger] takes. They are part of the
// uniqueness key, so they are constants rather than literals: a typo would not
// fail to compile, it would quietly split the debounce into two windows.
const (
	TriggerDebounced = "debounced"
	TriggerSettled   = "settled"
)

// IngestPathArgs asks for one path to be hashed and, if its bytes changed,
// promoted to its document's current content version.
type IngestPathArgs struct {
	MountpointID int64  `json:"mountpoint_id"`
	Path         string `json:"path"`
	// Trigger separates the two ways a path is queued and is deliberately part
	// of the uniqueness key. A burst of writes collapses into one deferred job
	// because every one of them carries "debounced"; the close of the write
	// handle carries "settled" instead, so it is a different key and lands even
	// though the deferred job is still sitting in `scheduled`. Sharing one key
	// would make the debounce swallow the only event that knows the write
	// finished.
	Trigger string `json:"trigger"`
}

// Kind implements river.JobArgs.
func (IngestPathArgs) Kind() string { return "ingest_path" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (IngestPathArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		// A path that cannot be hashed is almost always a permission or
		// disappearance problem that more attempts will not fix.
		MaxAttempts: 5,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: stillOutstanding,
		},
	}
}

// ExtractArgs asks for one content version's text and metadata.
//
// It is keyed by content version rather than by document because extraction is
// a property of the bytes: two documents holding identical content share the
// row, and re-observing the same file must not re-extract it.
type ExtractArgs struct {
	ContentVersionID int64 `json:"content_version_id"`
}

// Kind implements river.JobArgs.
func (ExtractArgs) Kind() string { return "extract" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (ExtractArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: QueueExtract,
		// Tika failures are usually the document, not the weather. A handful of
		// attempts covers a restarting server without spending an hour on a
		// file that will never parse.
		MaxAttempts: 5,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: stillOutstanding,
		},
	}
}

// InvalidateThumbnailsArgs drops every cached thumbnail derived from a content
// version a document no longer points at.
type InvalidateThumbnailsArgs struct {
	DocumentID                 int64 `json:"document_id"`
	SupersededContentVersionID int64 `json:"superseded_content_version_id"`
}

// Kind implements river.JobArgs.
func (InvalidateThumbnailsArgs) Kind() string { return "invalidate_thumbnails" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (InvalidateThumbnailsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		// Retried longer than the others: until this succeeds the cache holds
		// bytes derived from content nothing points at any more.
		MaxAttempts: 10,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: stillOutstanding,
		},
	}
}

// EvictionMaintenanceArgs enforces the thumbnail cache limits (D-008). It
// carries nothing: the limits are process configuration, not job state, so a
// job inserted before a limit changed must not run against the old one.
type EvictionMaintenanceArgs struct{}

// Kind implements river.JobArgs.
func (EvictionMaintenanceArgs) Kind() string { return "eviction_maintenance" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (EvictionMaintenanceArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		MaxAttempts: 3,
		UniqueOpts: river.UniqueOpts{
			ByState: stillOutstanding,
		},
	}
}

// EvictionPeriodicJob schedules the maintenance job at interval.
func EvictionPeriodicJob(interval time.Duration) *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(interval),
		func() (river.JobArgs, *river.InsertOpts) { return EvictionMaintenanceArgs{}, nil },
		// Running at startup means a process that was down past several
		// intervals reclaims space immediately rather than one interval later.
		&river.PeriodicJobOpts{RunOnStart: true},
	)
}

// CacheSweepArgs is the backstop pass over the whole thumbnail cache: rows no
// live document claims, objects still awaiting deletion, and objects no row
// accounts for. Like EvictionMaintenanceArgs it carries nothing, because what it
// sweeps is derived from the database rather than from the job.
type CacheSweepArgs struct{}

// Kind implements river.JobArgs.
func (CacheSweepArgs) Kind() string { return "cache_sweep" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (CacheSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		// Few attempts, because this is the thing that catches what other jobs
		// dropped: the next scheduled run is a better retry than an immediate
		// one against an object store that is evidently unwell.
		MaxAttempts: 3,
		UniqueOpts: river.UniqueOpts{
			ByState: stillOutstanding,
		},
	}
}

// CacheSweepPeriodicJob schedules the backstop sweep at interval.
//
// Deliberately not merged into the eviction job: eviction enforces limits and
// must run often enough to keep the quota honest, while the sweep walks the
// whole bucket and is only ever repairing something that already went wrong.
func CacheSweepPeriodicJob(interval time.Duration) *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(interval),
		func() (river.JobArgs, *river.InsertOpts) { return CacheSweepArgs{}, nil },
		// Not RunOnStart: a restart loop would otherwise walk the whole bucket
		// on every attempt, and nothing it repairs is urgent.
		nil,
	)
}

// ErrNotBound reports that the enqueuer was used before its River client was
// attached.
var ErrNotBound = errors.New("jobs: enqueuer has no River client")

// Enqueuer inserts jobs on a caller's transaction.
//
// The client is attached after construction because the two are mutually
// dependent: the workers need something to enqueue with, and river.NewClient
// needs the workers. Bind is called once, before the client starts.
type Enqueuer struct {
	client   *river.Client[pgx.Tx]
	debounce time.Duration
}

// NewEnqueuer builds an unbound Enqueuer. debounce is how long a deferred
// ingest job waits, which is the window a burst of writes collapses into.
func NewEnqueuer(debounce time.Duration) *Enqueuer {
	return &Enqueuer{debounce: debounce}
}

// Bind attaches the River client.
func (e *Enqueuer) Bind(client *river.Client[pgx.Tx]) { e.client = client }

// IngestPath queues a path for hashing. immediate skips the debounce window,
// which the caller sets when the write is known to be finished.
func (e *Enqueuer) IngestPath(
	ctx context.Context,
	tx pgx.Tx,
	mountpointID int64,
	path string,
	immediate bool,
) error {
	args := IngestPathArgs{
		MountpointID: mountpointID,
		Path:         path,
		Trigger:      TriggerSettled,
	}
	var opts *river.InsertOpts
	if !immediate {
		args.Trigger = TriggerDebounced
		opts = &river.InsertOpts{ScheduledAt: time.Now().Add(e.debounce)}
	}
	return e.insert(ctx, tx, args, opts)
}

// Extract queues a content version for text extraction.
func (e *Enqueuer) Extract(ctx context.Context, tx pgx.Tx, contentVersionID int64) error {
	return e.insert(ctx, tx, ExtractArgs{ContentVersionID: contentVersionID}, nil)
}

// InvalidateThumbnails queues the removal of a superseded version's thumbnails.
func (e *Enqueuer) InvalidateThumbnails(
	ctx context.Context,
	tx pgx.Tx,
	documentID, supersededContentVersionID int64,
) error {
	return e.insert(ctx, tx, InvalidateThumbnailsArgs{
		DocumentID:                 documentID,
		SupersededContentVersionID: supersededContentVersionID,
	}, nil)
}

func (e *Enqueuer) insert(
	ctx context.Context,
	tx pgx.Tx,
	args river.JobArgs,
	opts *river.InsertOpts,
) error {
	if e.client == nil {
		return ErrNotBound
	}
	// InsertTx, never Insert: the job and the row change it describes have to
	// reach the database together or not at all.
	if _, err := e.client.InsertTx(ctx, tx, args, opts); err != nil {
		return err
	}
	return nil
}
