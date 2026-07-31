// Package ingest turns go-overwatch's event stream into document and content
// identity (D-007).
//
// It is split in two halves that never block each other. The [Reconciler] owns
// the subscription: it applies each event to fs_observations, advances the
// watermark, and enqueues work. The [Processor] owns everything expensive —
// hashing a file, deciding whether its bytes actually changed, and flipping a
// document's current version — and runs from a job, not from the stream. A
// subscriber that stalls is not a slow subscriber but a lost one: the daemon
// answers a full buffer with a gap marker that costs a whole-tree rescan.
package ingest

import (
	"context"
	"time"
)

// Observation is one path's observed filesystem state.
type Observation struct {
	MountpointID int64
	Path         string
	Size         int64
	Mode         int64
	Mtime        time.Time
	Ino          int64
	Dev          int64
	IsDir        bool
	SeenEpoch    int64
}

// Document is the subset of a documents row the state machine reasons about.
//
// CurrentContentVersionID is 0 rather than a pointer when the document has no
// content version yet; content_versions.id is GENERATED ALWAYS AS IDENTITY, so
// it never takes that value and the sentinel cannot collide with a real one.
type Document struct {
	ID                      int64
	MountpointID            int64
	Path                    string
	Dev                     int64
	Ino                     int64
	CurrentContentVersionID int64
}

// Trigger says how soon a path's ingest job should run.
type Trigger int

const (
	// TriggerDebounced defers the job, letting a burst of writes to one path
	// collapse into a single hash. Create and Modify use it.
	TriggerDebounced Trigger = iota
	// TriggerSettled runs as soon as a worker is free, because the write is
	// known to be over: a closed write handle or a completed rename.
	TriggerSettled
)

func (t Trigger) String() string {
	if t == TriggerSettled {
		return "settled"
	}
	return "debounced"
}

// Tx is everything one atomic unit of ingestion touches, including the job
// enqueues.
//
// Enqueueing belongs here rather than beside it because the two must commit
// together: a version flip whose extraction job was inserted outside the
// transaction leaves a document that is current but never indexed if the
// process dies in between, and an insert that commits while the flip rolls
// back runs extraction against content nothing points at.
//
// It is one interface rather than several because every method is reachable
// from a single event, and splitting it would only move the union to the
// implementations.
type Tx interface {
	Watermark(ctx context.Context) (int64, error)
	SetWatermark(ctx context.Context, seq int64) error
	NextScanEpoch(ctx context.Context) (int64, error)

	GetObservation(ctx context.Context, mountpointID int64, path string) (Observation, bool, error)
	UpsertObservation(ctx context.Context, obs Observation) error
	DeleteObservation(ctx context.Context, mountpointID int64, path string) error
	DeleteObservationSubtree(ctx context.Context, mountpointID int64, prefix string) error
	MoveObservationSubtree(
		ctx context.Context,
		mountpointID int64,
		oldPrefix, newPrefix string,
	) error
	// SweepScanEpoch drops every row under root that the epoch did not observe
	// and returns their paths, which are exactly the deletes the stream missed.
	SweepScanEpoch(
		ctx context.Context, mountpointID int64, root string, epoch int64,
	) ([]string, error)

	FindLiveDocumentByPath(
		ctx context.Context, mountpointID int64, path string,
	) (Document, bool, error)
	FindLiveDocumentByInode(
		ctx context.Context, mountpointID, dev, ino int64,
	) (Document, bool, error)
	FindLiveDocumentByContent(
		ctx context.Context, mountpointID int64, contentHash string,
	) (Document, bool, error)
	CreateDocument(
		ctx context.Context, mountpointID int64, path string, dev, ino int64,
	) (Document, error)
	MoveDocument(ctx context.Context, id int64, path string, dev, ino int64) error
	MoveDocumentSubtree(
		ctx context.Context, mountpointID int64, oldPrefix, newPrefix string,
	) ([]Document, error)
	TombstoneDocument(ctx context.Context, id int64) error
	TombstoneDocumentByPath(
		ctx context.Context, mountpointID int64, path string,
	) (Document, bool, error)
	TombstoneDocumentSubtree(
		ctx context.Context, mountpointID int64, prefix string,
	) ([]Document, error)

	UpsertContentVersion(ctx context.Context, hash string, size int64) (int64, error)
	// SetDocumentCurrentVersion swaps the document's current version, reporting
	// false when expected is no longer what the row holds. It is a
	// compare-and-swap rather than a plain write because the expectation is read
	// in an earlier transaction — hashing sits between the two — so a second job
	// for the same path can have moved the document in the meantime.
	SetDocumentCurrentVersion(
		ctx context.Context, documentID, expected, contentVersionID int64,
	) (bool, error)
	MarkExtractionPending(ctx context.Context, contentVersionID int64) error

	EnqueueIngestPath(
		ctx context.Context, mountpointID int64, path string, trigger Trigger,
	) error
	EnqueueExtract(ctx context.Context, contentVersionID int64) error
	EnqueueInvalidateThumbnails(
		ctx context.Context, documentID, supersededContentVersionID int64,
	) error
}

// Store opens the transactions the state machine runs in.
type Store interface {
	InTx(ctx context.Context, fn func(context.Context, Tx) error) error
}
