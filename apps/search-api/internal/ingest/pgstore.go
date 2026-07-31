package ingest

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
)

// Enqueuer inserts jobs on an open transaction. It is satisfied by the jobs
// package; declaring it here keeps ingest free of any River dependency, so the
// state machine can be tested without a queue.
type Enqueuer interface {
	IngestPath(
		ctx context.Context, tx pgx.Tx, mountpointID int64, path string, immediate bool,
	) error
	Extract(ctx context.Context, tx pgx.Tx, contentVersionID int64) error
	InvalidateThumbnails(
		ctx context.Context, tx pgx.Tx, documentID, supersededContentVersionID int64,
	) error
}

// PGStore is the PostgreSQL-backed [Store].
type PGStore struct {
	pool     *pgxpool.Pool
	enqueuer Enqueuer
}

// NewPGStore wraps an existing pool. The caller owns the pool's lifecycle.
func NewPGStore(pool *pgxpool.Pool, enqueuer Enqueuer) *PGStore {
	return &PGStore{pool: pool, enqueuer: enqueuer}
}

// InTx runs fn in one transaction, committing when it returns nil.
func (s *PGStore) InTx(ctx context.Context, fn func(context.Context, Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	// Rolling back on a context that is already cancelled would leave the
	// transaction open until the connection is reaped, so abort detached.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := fn(ctx, &pgTx{tx: tx, q: sqlcgen.New(tx), enqueuer: s.enqueuer}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

type pgTx struct {
	tx       pgx.Tx
	q        *sqlcgen.Queries
	enqueuer Enqueuer
}

func (t *pgTx) Watermark(ctx context.Context) (int64, error) {
	wm, err := t.q.LockWatermark(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("lock watermark: %w", err)
	}
	return wm, nil
}

func (t *pgTx) SetWatermark(ctx context.Context, seq int64) error {
	if err := t.q.SetWatermark(ctx, seq); err != nil {
		return fmt.Errorf("set watermark %d: %w", seq, err)
	}
	return nil
}

func (t *pgTx) NextScanEpoch(ctx context.Context) (int64, error) {
	epoch, err := t.q.NextScanEpoch(ctx)
	if err != nil {
		return 0, fmt.Errorf("next scan epoch: %w", err)
	}
	return epoch, nil
}

func (t *pgTx) GetObservation(
	ctx context.Context,
	mountpointID int64,
	path string,
) (Observation, bool, error) {
	row, err := t.q.GetObservation(ctx, sqlcgen.GetObservationParams{
		MountpointID: mountpointID,
		Path:         path,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Observation{}, false, nil
	}
	if err != nil {
		return Observation{}, false, fmt.Errorf("get observation %s: %w", path, err)
	}
	return Observation{
		MountpointID: row.MountpointID,
		Path:         row.Path,
		Size:         row.Size,
		Mode:         row.Mode,
		Mtime:        row.Mtime.Time,
		Ino:          row.Ino,
		Dev:          row.Dev,
		IsDir:        row.IsDir,
		SeenEpoch:    row.SeenEpoch,
	}, true, nil
}

func (t *pgTx) UpsertObservation(ctx context.Context, obs Observation) error {
	var mtime pgtype.Timestamptz
	if !obs.Mtime.IsZero() {
		mtime = pgtype.Timestamptz{Time: obs.Mtime, Valid: true}
	}
	err := t.q.UpsertObservation(ctx, sqlcgen.UpsertObservationParams{
		MountpointID: obs.MountpointID,
		Path:         obs.Path,
		Size:         obs.Size,
		Mode:         obs.Mode,
		Mtime:        mtime,
		Ino:          obs.Ino,
		Dev:          obs.Dev,
		IsDir:        obs.IsDir,
		SeenEpoch:    obs.SeenEpoch,
	})
	if err != nil {
		return fmt.Errorf("upsert observation %s: %w", obs.Path, err)
	}
	return nil
}

func (t *pgTx) DeleteObservation(ctx context.Context, mountpointID int64, path string) error {
	err := t.q.DeleteObservation(ctx, sqlcgen.DeleteObservationParams{
		MountpointID: mountpointID,
		Path:         path,
	})
	if err != nil {
		return fmt.Errorf("delete observation %s: %w", path, err)
	}
	return nil
}

func (t *pgTx) DeleteObservationSubtree(
	ctx context.Context,
	mountpointID int64,
	prefix string,
) error {
	_, err := t.q.DeleteObservationSubtree(ctx, sqlcgen.DeleteObservationSubtreeParams{
		MountpointID: mountpointID,
		PathPrefix:   prefix,
	})
	if err != nil {
		return fmt.Errorf("delete observation subtree %s: %w", prefix, err)
	}
	return nil
}

func (t *pgTx) MoveObservationSubtree(
	ctx context.Context,
	mountpointID int64,
	oldPrefix, newPrefix string,
) error {
	err := t.q.MoveObservationSubtree(ctx, sqlcgen.MoveObservationSubtreeParams{
		MountpointID: mountpointID,
		OldPrefix:    oldPrefix,
		NewPrefix:    newPrefix,
	})
	if err != nil {
		return fmt.Errorf("move observation subtree %s: %w", oldPrefix, err)
	}
	return nil
}

func (t *pgTx) SweepScanEpoch(
	ctx context.Context,
	mountpointID int64,
	root string,
	epoch int64,
) ([]string, error) {
	paths, err := t.q.SweepScanEpoch(ctx, sqlcgen.SweepScanEpochParams{
		MountpointID: mountpointID,
		Root:         root,
		RootPrefix:   subtreePrefix(root),
		SeenEpoch:    epoch,
	})
	if err != nil {
		return nil, fmt.Errorf("sweep scan epoch %s: %w", root, err)
	}
	return paths, nil
}

func (t *pgTx) FindLiveDocumentByPath(
	ctx context.Context,
	mountpointID int64,
	path string,
) (Document, bool, error) {
	row, err := t.q.FindLiveDocumentByPath(ctx, sqlcgen.FindLiveDocumentByPathParams{
		MountpointID: mountpointID,
		Path:         path,
	})
	return documentOrMissing(row, err, "find document by path "+path)
}

func (t *pgTx) FindLiveDocumentByInode(
	ctx context.Context,
	mountpointID, dev, ino int64,
) (Document, bool, error) {
	row, err := t.q.FindLiveDocumentByInode(ctx, sqlcgen.FindLiveDocumentByInodeParams{
		MountpointID: mountpointID,
		Dev:          dev,
		Ino:          ino,
	})
	return documentOrMissing(row, err, "find document by inode")
}

func (t *pgTx) FindLiveDocumentByContent(
	ctx context.Context,
	mountpointID int64,
	contentHash string,
) (Document, bool, error) {
	row, err := t.q.FindLiveDocumentByContent(ctx, sqlcgen.FindLiveDocumentByContentParams{
		MountpointID: mountpointID,
		ContentHash:  contentHash,
	})
	return documentOrMissing(row, err, "find document by content")
}

func (t *pgTx) CreateDocument(
	ctx context.Context,
	mountpointID int64,
	path string,
	dev, ino int64,
) (Document, error) {
	row, err := t.q.CreateDocument(ctx, sqlcgen.CreateDocumentParams{
		MountpointID: mountpointID,
		Path:         path,
		Dev:          dev,
		Ino:          ino,
	})
	if err != nil {
		return Document{}, fmt.Errorf("create document %s: %w", path, err)
	}
	return toDocument(row), nil
}

func (t *pgTx) MoveDocument(ctx context.Context, id int64, path string, dev, ino int64) error {
	_, err := t.q.MoveDocument(ctx, sqlcgen.MoveDocumentParams{
		ID:   id,
		Path: path,
		Dev:  dev,
		Ino:  ino,
	})
	if err != nil {
		return fmt.Errorf("move document %d to %s: %w", id, path, err)
	}
	return nil
}

func (t *pgTx) MoveDocumentSubtree(
	ctx context.Context,
	mountpointID int64,
	oldPrefix, newPrefix string,
) ([]Document, error) {
	rows, err := t.q.MoveDocumentSubtree(ctx, sqlcgen.MoveDocumentSubtreeParams{
		MountpointID: mountpointID,
		OldPrefix:    oldPrefix,
		NewPrefix:    newPrefix,
	})
	if err != nil {
		return nil, fmt.Errorf("move document subtree %s: %w", oldPrefix, err)
	}
	out := make([]Document, 0, len(rows))
	for _, row := range rows {
		out = append(out, Document{ID: row.ID, MountpointID: mountpointID, Path: row.Path})
	}
	return out, nil
}

func (t *pgTx) TombstoneDocument(ctx context.Context, id int64) error {
	if _, err := t.q.TombstoneDocument(ctx, id); err != nil {
		return fmt.Errorf("tombstone document %d: %w", id, err)
	}
	return nil
}

func (t *pgTx) TombstoneDocumentByPath(
	ctx context.Context,
	mountpointID int64,
	path string,
) (Document, bool, error) {
	row, err := t.q.TombstoneDocumentByPath(ctx, sqlcgen.TombstoneDocumentByPathParams{
		MountpointID: mountpointID,
		Path:         path,
	})
	return documentOrMissing(row, err, "tombstone document "+path)
}

func (t *pgTx) TombstoneDocumentSubtree(
	ctx context.Context,
	mountpointID int64,
	prefix string,
) ([]Document, error) {
	rows, err := t.q.TombstoneDocumentSubtree(ctx, sqlcgen.TombstoneDocumentSubtreeParams{
		MountpointID: mountpointID,
		PathPrefix:   prefix,
	})
	if err != nil {
		return nil, fmt.Errorf("tombstone document subtree %s: %w", prefix, err)
	}
	out := make([]Document, 0, len(rows))
	for _, row := range rows {
		out = append(out, Document{
			ID:                      row.ID,
			MountpointID:            mountpointID,
			Path:                    row.Path,
			CurrentContentVersionID: derefID(row.CurrentContentVersionID),
		})
	}
	return out, nil
}

func (t *pgTx) UpsertContentVersion(
	ctx context.Context,
	hash string,
	size int64,
) (int64, error) {
	row, err := t.q.UpsertContentVersion(ctx, sqlcgen.UpsertContentVersionParams{
		ContentHash: hash,
		SizeBytes:   size,
	})
	if err != nil {
		return 0, fmt.Errorf("upsert content version %s: %w", hash, err)
	}
	return row.ID, nil
}

func (t *pgTx) SetDocumentCurrentVersion(
	ctx context.Context,
	documentID, expected, contentVersionID int64,
) (bool, error) {
	// 0 is the package's "no version yet" sentinel and NULL is the column's, so
	// the CAS argument has to be the pointer form rather than a zero.
	var expectedID *int64
	if expected != 0 {
		expectedID = &expected
	}
	_, err := t.q.SetDocumentCurrentVersion(ctx, sqlcgen.SetDocumentCurrentVersionParams{
		ID:                       documentID,
		CurrentContentVersionID:  &contentVersionID,
		ExpectedContentVersionID: expectedID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// No row matched: another job moved the document between the read and
		// this write. That is the caller's signal, not a failure.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf(
			"set document %d version %d: %w", documentID, contentVersionID, err)
	}
	return true, nil
}

func (t *pgTx) MarkExtractionPending(ctx context.Context, contentVersionID int64) error {
	if err := t.q.MarkExtractionPending(ctx, contentVersionID); err != nil {
		return fmt.Errorf("mark extraction pending %d: %w", contentVersionID, err)
	}
	return nil
}

func (t *pgTx) EnqueueIngestPath(
	ctx context.Context,
	mountpointID int64,
	path string,
	trigger Trigger,
) error {
	return t.enqueuer.IngestPath(ctx, t.tx, mountpointID, path, trigger == TriggerSettled)
}

func (t *pgTx) EnqueueExtract(ctx context.Context, contentVersionID int64) error {
	return t.enqueuer.Extract(ctx, t.tx, contentVersionID)
}

func (t *pgTx) EnqueueInvalidateThumbnails(
	ctx context.Context,
	documentID, supersededContentVersionID int64,
) error {
	return t.enqueuer.InvalidateThumbnails(ctx, t.tx, documentID, supersededContentVersionID)
}

func documentOrMissing(
	row sqlcgen.Document,
	err error,
	what string,
) (Document, bool, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, false, nil
	}
	if err != nil {
		return Document{}, false, fmt.Errorf("%s: %w", what, err)
	}
	return toDocument(row), true, nil
}

func toDocument(row sqlcgen.Document) Document {
	return Document{
		ID:                      row.ID,
		MountpointID:            row.MountpointID,
		Path:                    row.Path,
		Dev:                     row.Dev,
		Ino:                     row.Ino,
		CurrentContentVersionID: derefID(row.CurrentContentVersionID),
	}
}

func derefID(id *int64) int64 {
	if id == nil {
		return 0
	}
	return *id
}

var _ Store = (*PGStore)(nil)
