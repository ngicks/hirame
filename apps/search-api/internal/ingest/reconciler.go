package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	overwatchv1 "github.com/ngicks/go-overwatch/overwatch/pkg/api/gen/proto/go/overwatch/v1"

	"github.com/ngicks/hirame/apps/search-api/internal/doctype"
)

// Watcher is the slice of the go-overwatch client the reconciler drives.
// Declaring it here rather than taking *client.Client lets the loop be tested
// against a scripted stream instead of a daemon that needs CAP_SYS_ADMIN.
type Watcher interface {
	Subscribe(
		ctx context.Context, req *overwatchv1.SubscribeRequest,
	) (EventStream, error)
	Scan(ctx context.Context, req *overwatchv1.ScanRequest) (ScanStream, error)
	Status(
		ctx context.Context, req *overwatchv1.StatusRequest,
	) (*overwatchv1.StatusResponse, error)
}

// EventStream is a live subscription.
type EventStream interface {
	Recv() (*overwatchv1.SubscribeResponse, error)
}

// ScanStream is one paced walk of a subtree.
type ScanStream interface {
	Recv() (*overwatchv1.ScanResponse, error)
}

// Mountpoint is a watched root and the row that stands for it.
type Mountpoint struct {
	ID   int64
	Root string
}

// Reconciler applies the daemon's stream to the database. Build it with [New]
// and drive it with [Reconciler.Run].
type Reconciler struct {
	watcher     Watcher
	store       Store
	filter      *doctype.Filter
	mountpoints []Mountpoint
	logger      *slog.Logger
	backoff     time.Duration
}

// New builds a Reconciler over the given mountpoints, which must already exist
// in the database. logger must not be nil.
func New(
	watcher Watcher,
	store Store,
	filter *doctype.Filter,
	mountpoints []Mountpoint,
	logger *slog.Logger,
	backoff time.Duration,
) *Reconciler {
	if backoff <= 0 {
		backoff = time.Second
	}
	return &Reconciler{
		watcher:     watcher,
		store:       store,
		filter:      filter,
		mountpoints: mountpoints,
		logger:      logger,
		backoff:     backoff,
	}
}

// Run reconciles until ctx is cancelled, which is the only way it stops.
//
// A dropped stream, a daemon that has not started yet, and a daemon that
// restarted all reach the same retry with a fixed backoff; the stored watermark
// makes each re-subscription resume where the last one left off, and a
// restarted daemon opens its stream with a gap marker that forces the rescan
// needed to recover whatever happened while it was down.
func (r *Reconciler) Run(ctx context.Context) error {
	if len(r.mountpoints) == 0 {
		return errors.New("ingest: no mountpoints configured")
	}
	r.logger.InfoContext(ctx, "ingest starting",
		slog.Any("mountpoints", r.roots()),
		slog.Any("extensions", r.filter.Extensions()),
	)

	for {
		switch err := r.runOnce(ctx); {
		case ctx.Err() != nil:
			r.logger.InfoContext(ctx, "ingest stopped")
			return nil
		case err == nil:
			r.logger.InfoContext(ctx, "watcher stream closed; resubscribing",
				slog.Duration("backoff", r.backoff))
		default:
			r.logger.WarnContext(ctx, "watcher stream ended; retrying",
				slog.Any("error", err),
				slog.Duration("backoff", r.backoff),
			)
		}
		select {
		case <-ctx.Done():
			r.logger.InfoContext(ctx, "ingest stopped")
			return nil
		case <-time.After(r.backoff):
		}
	}
}

// warnOnRootMismatch reports a mountpoint no watch root of the daemon covers.
//
// The daemon is configured separately from this process (D-009 puts it in a
// system unit of its own), so the two lists can disagree — a trailing slash or
// a typo is enough. Nothing else would say so: events for an uncovered path are
// dropped by mountpointFor and the watermark advances regardless, which is
// indistinguishable from a filesystem where nothing is happening.
func (r *Reconciler) warnOnRootMismatch(ctx context.Context) {
	st, err := r.watcher.Status(ctx, &overwatchv1.StatusRequest{})
	if err != nil {
		return // The caller is about to fail on the same daemon and retry.
	}
	watched := make([]string, 0, len(st.GetRoots()))
	for _, root := range st.GetRoots() {
		watched = append(watched, root.GetRoot())
	}
	for _, mp := range r.mountpoints {
		covered := slices.ContainsFunc(watched, func(root string) bool {
			clean := filepath.Clean(root)
			return mp.Root == clean || strings.HasPrefix(mp.Root, subtreePrefix(clean))
		})
		if !covered {
			r.logger.WarnContext(ctx, "mountpoint is outside every daemon watch root; "+
				"no event for it will ever arrive",
				slog.String("mountpoint", mp.Root),
				slog.Any("watch_roots", watched),
			)
		}
	}
}

// runOnce bootstraps if the store has never been reconciled, then drives one
// subscription to completion. It returns nil only on a clean stream EOF.
func (r *Reconciler) runOnce(ctx context.Context) error {
	r.warnOnRootMismatch(ctx)

	var watermark int64
	if err := r.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		watermark, err = tx.Watermark(ctx)
		return err
	}); err != nil {
		return err
	}

	if watermark == 0 {
		// The ring head is read before the scan, not after, so the
		// subscription below replays everything that happened while the scan
		// was running rather than silently starting past it.
		head, err := r.ringHead(ctx)
		if err != nil {
			return err
		}
		if err := r.reconcileAll(ctx, head); err != nil {
			return err
		}
		watermark = head
		r.logger.InfoContext(ctx, "bootstrap scan complete",
			slog.Int64("watermark", watermark))
	}

	stream, err := r.watcher.Subscribe(ctx, &overwatchv1.SubscribeRequest{
		FromSeq: uint64(watermark) + 1,
	})
	if err != nil {
		return fmt.Errorf("subscribe from seq %d: %w", watermark+1, err)
	}
	return r.drive(ctx, stream)
}

// drive reads a subscription, applying events and rescanning on every gap.
func (r *Reconciler) drive(ctx context.Context, stream EventStream) error {
	for {
		resp, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if st, ok := status.FromError(err); ok && st.Code() == codes.Canceled {
				return nil
			}
			return fmt.Errorf("subscribe recv: %w", err)
		}

		switch item := resp.GetItem().(type) {
		case *overwatchv1.SubscribeResponse_Event:
			if err := r.ApplyEvent(ctx, item.Event); err != nil {
				return fmt.Errorf("apply event seq %d: %w", item.Event.GetSeq(), err)
			}
		case *overwatchv1.SubscribeResponse_Gap:
			gap := item.Gap
			r.logger.InfoContext(ctx, "stream gap; rescanning",
				slog.String("kind", gap.GetKind().String()),
				slog.Uint64("seq", gap.GetSeq()),
			)
			head, err := r.ringHead(ctx)
			if err != nil {
				return err
			}
			if err := r.reconcileAll(ctx, head); err != nil {
				return fmt.Errorf("reconcile after gap %s: %w", gap.GetKind(), err)
			}
		}
	}
}

// ApplyEvent durably records one event and advances the watermark. It is
// idempotent: an event at or below the stored watermark is dropped, which is
// what makes a replayed subscription safe.
func (r *Reconciler) ApplyEvent(ctx context.Context, ev *overwatchv1.FileEvent) error {
	return r.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		watermark, err := tx.Watermark(ctx)
		if err != nil {
			return err
		}
		seq := int64(ev.GetSeq())
		if seq <= watermark {
			return nil
		}

		if err := r.applyKind(ctx, tx, ev); err != nil {
			return err
		}
		return tx.SetWatermark(ctx, seq)
	})
}

func (r *Reconciler) applyKind(
	ctx context.Context,
	tx Tx,
	ev *overwatchv1.FileEvent,
) error {
	switch ev.GetKind() {
	case overwatchv1.EventKind_EVENT_KIND_CREATE,
		overwatchv1.EventKind_EVENT_KIND_MODIFY:
		return r.observe(ctx, tx, ev.GetPath(), ev.GetStat(), ev.GetIsDir(), TriggerDebounced)

	case overwatchv1.EventKind_EVENT_KIND_CLOSE_WRITE:
		return r.observe(ctx, tx, ev.GetPath(), ev.GetStat(), ev.GetIsDir(), TriggerSettled)

	case overwatchv1.EventKind_EVENT_KIND_ATTRIB:
		// Permission and timestamp changes move no bytes, so the observation is
		// refreshed but nothing is queued to re-read the file.
		return r.observeOnly(ctx, tx, ev.GetPath(), ev.GetStat(), ev.GetIsDir())

	case overwatchv1.EventKind_EVENT_KIND_DELETE:
		return r.applyDelete(ctx, tx, ev.GetPath(), ev.GetIsDir())

	case overwatchv1.EventKind_EVENT_KIND_MOVE:
		return r.applyMove(ctx, tx, ev)
	}
	return nil
}

// observe records a path and queues it for ingestion when it looks like a
// document.
func (r *Reconciler) observe(
	ctx context.Context,
	tx Tx,
	path string,
	stat *overwatchv1.StatInfo,
	isDir bool,
	trigger Trigger,
) error {
	mp, ok := r.mountpointFor(path)
	if !ok {
		return nil
	}
	if err := r.observeOnly(ctx, tx, path, stat, isDir); err != nil {
		return err
	}
	if isDir || !r.filter.AllowPath(path) {
		return nil
	}
	return tx.EnqueueIngestPath(ctx, mp.ID, path, trigger)
}

func (r *Reconciler) observeOnly(
	ctx context.Context,
	tx Tx,
	path string,
	stat *overwatchv1.StatInfo,
	isDir bool,
) error {
	mp, ok := r.mountpointFor(path)
	if !ok {
		return nil
	}
	return tx.UpsertObservation(ctx, observationOf(mp.ID, path, stat, isDir, 0))
}

// applyDelete drops the observation and tombstones whatever document stood at
// that path, leaving the thumbnails derived from it to be invalidated.
func (r *Reconciler) applyDelete(
	ctx context.Context,
	tx Tx,
	path string,
	isDir bool,
) error {
	mp, ok := r.mountpointFor(path)
	if !ok {
		return nil
	}
	if err := tx.DeleteObservation(ctx, mp.ID, path); err != nil {
		return err
	}
	if isDir {
		if err := tx.DeleteObservationSubtree(ctx, mp.ID, subtreePrefix(path)); err != nil {
			return err
		}
		docs, err := tx.TombstoneDocumentSubtree(ctx, mp.ID, subtreePrefix(path))
		if err != nil {
			return err
		}
		return r.invalidateAll(ctx, tx, docs)
	}

	doc, found, err := tx.TombstoneDocumentByPath(ctx, mp.ID, path)
	if err != nil || !found {
		return err
	}
	return r.invalidate(ctx, tx, doc)
}

// applyMove preserves identity across a rename (D-007). A directory move
// rewrites the paths beneath it, because the daemon reports the directory only
// and emits nothing for its contents.
func (r *Reconciler) applyMove(
	ctx context.Context,
	tx Tx,
	ev *overwatchv1.FileEvent,
) error {
	oldPath, newPath := ev.GetOldPath(), ev.GetPath()
	oldMP, oldOK := r.mountpointFor(oldPath)
	newMP, newOK := r.mountpointFor(newPath)

	// A move out of every watched root is a deletion as far as this system can
	// observe, and a move in is a creation.
	if oldOK && !newOK {
		return r.applyDelete(ctx, tx, oldPath, ev.GetIsDir())
	}
	if !oldOK && newOK {
		return r.observe(ctx, tx, newPath, ev.GetStat(), ev.GetIsDir(), TriggerSettled)
	}
	if !oldOK || oldMP.ID != newMP.ID {
		// Crossing between two watched roots is rare enough that re-deriving
		// both sides is preferable to a partial rewrite across mountpoints.
		if oldOK {
			if err := r.applyDelete(ctx, tx, oldPath, ev.GetIsDir()); err != nil {
				return err
			}
		}
		if newOK {
			return r.observe(ctx, tx, newPath, ev.GetStat(), ev.GetIsDir(), TriggerSettled)
		}
		return nil
	}

	if ev.GetIsDir() {
		return r.moveDirectory(ctx, tx, newMP.ID, oldPath, newPath, ev.GetStat())
	}
	return r.moveFile(ctx, tx, newMP.ID, oldPath, newPath, ev.GetStat())
}

func (r *Reconciler) moveFile(
	ctx context.Context,
	tx Tx,
	mountpointID int64,
	oldPath, newPath string,
	stat *overwatchv1.StatInfo,
) error {
	// The observation for the source has to go before any content-based
	// identity lookup runs: FindLiveDocumentByContent deliberately ignores
	// documents whose own path still exists, so that a copy is not mistaken for
	// a move. Leaving the row in place would make the lookup miss.
	if err := tx.DeleteObservation(ctx, mountpointID, oldPath); err != nil {
		return err
	}
	if err := tx.UpsertObservation(
		ctx, observationOf(mountpointID, newPath, stat, false, 0),
	); err != nil {
		return err
	}

	moved, movedFound, err := tx.FindLiveDocumentByPath(ctx, mountpointID, oldPath)
	if err != nil {
		return err
	}
	if movedFound {
		// Renaming onto an existing document destroys it; tombstone the target
		// first so the live-path unique index still holds after the update.
		existing, existingFound, err := tx.FindLiveDocumentByPath(ctx, mountpointID, newPath)
		if err != nil {
			return err
		}
		if existingFound && existing.ID != moved.ID {
			if err := tx.TombstoneDocument(ctx, existing.ID); err != nil {
				return err
			}
			if err := r.invalidate(ctx, tx, existing); err != nil {
				return err
			}
		}
		if !r.filter.AllowPath(newPath) {
			// Renamed to something this system does not index, e.g. report.pdf
			// to report.pdf.bak.
			if err := tx.TombstoneDocument(ctx, moved.ID); err != nil {
				return err
			}
			return r.invalidate(ctx, tx, moved)
		}
		if err := tx.MoveDocument(
			ctx, moved.ID, newPath, statDev(stat), statIno(stat),
		); err != nil {
			return err
		}
	}

	if !r.filter.AllowPath(newPath) {
		return nil
	}
	// Queued even when the document merely moved: a rename onto an existing
	// path is how editors publish new content, and only a hash can tell that
	// case from a pure relocation.
	return tx.EnqueueIngestPath(ctx, mountpointID, newPath, TriggerSettled)
}

func (r *Reconciler) moveDirectory(
	ctx context.Context,
	tx Tx,
	mountpointID int64,
	oldPath, newPath string,
	stat *overwatchv1.StatInfo,
) error {
	oldPrefix, newPrefix := subtreePrefix(oldPath), subtreePrefix(newPath)

	// Anything already sitting at the destination was overwritten by the
	// rename and is gone.
	if err := tx.DeleteObservation(ctx, mountpointID, newPath); err != nil {
		return err
	}
	if err := tx.DeleteObservationSubtree(ctx, mountpointID, newPrefix); err != nil {
		return err
	}
	replaced, err := tx.TombstoneDocumentSubtree(ctx, mountpointID, newPrefix)
	if err != nil {
		return err
	}
	if err := r.invalidateAll(ctx, tx, replaced); err != nil {
		return err
	}

	if err := tx.MoveObservationSubtree(ctx, mountpointID, oldPrefix, newPrefix); err != nil {
		return err
	}
	if err := tx.DeleteObservation(ctx, mountpointID, oldPath); err != nil {
		return err
	}
	if err := tx.UpsertObservation(
		ctx, observationOf(mountpointID, newPath, stat, true, 0),
	); err != nil {
		return err
	}
	// The bytes did not move, so no re-ingestion is queued: the documents keep
	// their content versions, extractions, and thumbnails.
	_, err = tx.MoveDocumentSubtree(ctx, mountpointID, oldPrefix, newPrefix)
	return err
}

// reconcileAll rescans every mountpoint, anchoring each epoch at the same ring
// head so a failure part way leaves the store at one consistent sequence.
func (r *Reconciler) reconcileAll(ctx context.Context, head int64) error {
	for _, mp := range r.mountpoints {
		if err := r.reconcileRoot(ctx, mp, head); err != nil {
			return err
		}
	}
	return nil
}

// reconcileRoot pulls a full walk of one root and commits it as a single
// mark-and-sweep epoch: every observed path is tagged, then whatever still
// carries an older tag is exactly what was deleted while the stream was blind.
func (r *Reconciler) reconcileRoot(ctx context.Context, mp Mountpoint, head int64) error {
	stream, err := r.watcher.Scan(ctx, &overwatchv1.ScanRequest{Root: mp.Root})
	if err != nil {
		return fmt.Errorf("scan %s: %w", mp.Root, err)
	}

	var observed, queued, swept int
	err = r.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		epoch, err := tx.NextScanEpoch(ctx)
		if err != nil {
			return err
		}
		for {
			resp, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return fmt.Errorf("scan %s recv: %w", mp.Root, err)
			}
			batch := resp.GetBatch()
			if batch == nil {
				// Cursor checkpoints and the done marker carry nothing a
				// single-pass reconcile needs.
				continue
			}
			for _, obs := range batch.GetObservations() {
				enqueued, err := r.observeScanned(ctx, tx, mp, obs, epoch)
				if err != nil {
					return err
				}
				observed++
				if enqueued {
					queued++
				}
			}
		}

		gone, err := tx.SweepScanEpoch(ctx, mp.ID, mp.Root, epoch)
		if err != nil {
			return err
		}
		swept = len(gone)
		for _, path := range gone {
			doc, found, err := tx.TombstoneDocumentByPath(ctx, mp.ID, path)
			if err != nil {
				return err
			}
			if found {
				if err := r.invalidate(ctx, tx, doc); err != nil {
					return err
				}
			}
		}
		return tx.SetWatermark(ctx, head)
	})
	if err != nil {
		return err
	}

	r.logger.InfoContext(ctx, "scan reconciled",
		slog.String("root", mp.Root),
		slog.Int("observed", observed),
		slog.Int("queued", queued),
		slog.Int("swept", swept),
	)
	return nil
}

// observeScanned records one scanned path and reports whether it was queued for
// ingestion.
//
// A rescan re-reads the whole tree, so queueing every file would re-hash it as
// well. Work is queued only when the stored state cannot already account for
// the file: nothing recorded, a size or mtime that moved, or an observation
// with no live document behind it — the last of which is how a run interrupted
// part way through its first scan finishes the job.
func (r *Reconciler) observeScanned(
	ctx context.Context,
	tx Tx,
	mp Mountpoint,
	obs *overwatchv1.Observation,
	epoch int64,
) (bool, error) {
	path := obs.GetPath()
	next := observationOf(mp.ID, path, obs.GetStat(), false, epoch)
	// A scan observation carries no directory flag of its own; the mode does.
	// The daemon converts st_mode to fs.FileMode before it reaches the wire
	// (StatInfoToProto casts an fs.FileMode straight to uint32), so the type
	// bit to test is fs.ModeDir and not S_IFDIR.
	next.IsDir = next.Mode&int64(fs.ModeDir) != 0

	prior, hadPrior, err := tx.GetObservation(ctx, mp.ID, path)
	if err != nil {
		return false, err
	}
	if err := tx.UpsertObservation(ctx, next); err != nil {
		return false, err
	}
	if next.IsDir || !r.filter.AllowPath(path) {
		return false, nil
	}

	changed := !hadPrior || prior.Size != next.Size || !prior.Mtime.Equal(next.Mtime)
	if !changed {
		_, hasDoc, err := tx.FindLiveDocumentByPath(ctx, mp.ID, path)
		if err != nil {
			return false, err
		}
		if hasDoc {
			return false, nil
		}
	}
	return true, tx.EnqueueIngestPath(ctx, mp.ID, path, TriggerSettled)
}

func (r *Reconciler) invalidate(ctx context.Context, tx Tx, doc Document) error {
	if doc.CurrentContentVersionID == 0 {
		return nil
	}
	return tx.EnqueueInvalidateThumbnails(ctx, doc.ID, doc.CurrentContentVersionID)
}

func (r *Reconciler) invalidateAll(ctx context.Context, tx Tx, docs []Document) error {
	for _, doc := range docs {
		if err := r.invalidate(ctx, tx, doc); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) ringHead(ctx context.Context) (int64, error) {
	st, err := r.watcher.Status(ctx, &overwatchv1.StatusRequest{})
	if err != nil {
		return 0, fmt.Errorf("watcher status: %w", err)
	}
	return int64(st.GetRingHeadSeq()), nil
}

// mountpointFor resolves a path to the deepest watched root containing it, so
// nested roots resolve to the more specific one.
func (r *Reconciler) mountpointFor(path string) (Mountpoint, bool) {
	var best Mountpoint
	found := false
	for _, mp := range r.mountpoints {
		if path != mp.Root && !strings.HasPrefix(path, subtreePrefix(mp.Root)) {
			continue
		}
		if !found || len(mp.Root) > len(best.Root) {
			best, found = mp, true
		}
	}
	return best, found
}

func (r *Reconciler) roots() []string {
	out := make([]string, 0, len(r.mountpoints))
	for _, mp := range r.mountpoints {
		out = append(out, mp.Root)
	}
	return out
}

func observationOf(
	mountpointID int64,
	path string,
	stat *overwatchv1.StatInfo,
	isDir bool,
	epoch int64,
) Observation {
	obs := Observation{
		MountpointID: mountpointID,
		Path:         path,
		IsDir:        isDir,
		SeenEpoch:    epoch,
	}
	if stat == nil {
		return obs
	}
	obs.Size = int64(stat.GetSize())
	obs.Mode = int64(stat.GetMode())
	obs.Ino = int64(stat.GetIno())
	obs.Dev = int64(stat.GetDev())
	if ts := stat.GetMtime(); ts != nil {
		obs.Mtime = ts.AsTime()
	}
	return obs
}

func statDev(stat *overwatchv1.StatInfo) int64 { return int64(stat.GetDev()) }
func statIno(stat *overwatchv1.StatInfo) int64 { return int64(stat.GetIno()) }

// subtreePrefix is the string every path strictly beneath dir starts with.
func subtreePrefix(dir string) string {
	return strings.TrimSuffix(dir, "/") + "/"
}
