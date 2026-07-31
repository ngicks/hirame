package ingest_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	overwatchv1 "github.com/ngicks/go-overwatch/overwatch/pkg/api/gen/proto/go/overwatch/v1"

	"github.com/ngicks/hirame/apps/search-api/internal/ingest"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ingestCall records one queued path.
type ingestCall struct {
	MountpointID int64
	Path         string
	Trigger      ingest.Trigger
}

// invalidateCall records one queued thumbnail invalidation.
type invalidateCall struct {
	DocumentID       int64
	SupersededTarget int64
}

// fakeStore is an in-memory [ingest.Store]. It reproduces the parts of the
// schema the state machine depends on — the live-path uniqueness, the epoch
// sweep, and the guard that keeps a copy from being read as a move — so the
// decisions can be tested without a database.
type fakeStore struct {
	tx *fakeTx
}

func newFakeStore() *fakeStore {
	return &fakeStore{tx: &fakeTx{
		observations:  map[string]ingest.Observation{},
		documents:     map[int64]*ingest.Document{},
		tombstoned:    map[int64]bool{},
		versionByHash: map[string]int64{},
		hashByVersion: map[int64]string{},
	}}
}

func (s *fakeStore) InTx(ctx context.Context, fn func(context.Context, ingest.Tx) error) error {
	return fn(ctx, s.tx)
}

type fakeTx struct {
	watermark     int64
	epoch         int64
	observations  map[string]ingest.Observation
	documents     map[int64]*ingest.Document
	tombstoned    map[int64]bool
	versionByHash map[string]int64
	hashByVersion map[int64]string
	nextDocID     int64
	nextVersionID int64

	queuedIngest     []ingestCall
	queuedExtract    []int64
	queuedInvalidate []invalidateCall
	pendingExtracts  []int64

	// onDocumentRead fires once, immediately after a document lookup has taken
	// its snapshot. It is how a test stages the concurrent writer a
	// compare-and-swap exists to notice: the caller goes on holding the value it
	// read while the stored row has already moved on, which is what READ
	// COMMITTED gives a second transaction that commits in between.
	onDocumentRead func()
}

func (t *fakeTx) fireDocumentRead() {
	if t.onDocumentRead == nil {
		return
	}
	hook := t.onDocumentRead
	t.onDocumentRead = nil
	hook()
}

func key(mountpointID int64, path string) string {
	return fmt.Sprintf("%d|%s", mountpointID, path)
}

func (t *fakeTx) Watermark(context.Context) (int64, error) { return t.watermark, nil }

func (t *fakeTx) SetWatermark(_ context.Context, seq int64) error {
	t.watermark = max(t.watermark, seq)
	return nil
}

func (t *fakeTx) NextScanEpoch(context.Context) (int64, error) {
	t.epoch++
	return t.epoch, nil
}

func (t *fakeTx) GetObservation(
	_ context.Context,
	mountpointID int64,
	path string,
) (ingest.Observation, bool, error) {
	obs, ok := t.observations[key(mountpointID, path)]
	return obs, ok, nil
}

func (t *fakeTx) UpsertObservation(_ context.Context, obs ingest.Observation) error {
	t.observations[key(obs.MountpointID, obs.Path)] = obs
	return nil
}

func (t *fakeTx) DeleteObservation(_ context.Context, mountpointID int64, path string) error {
	delete(t.observations, key(mountpointID, path))
	return nil
}

func (t *fakeTx) DeleteObservationSubtree(
	_ context.Context,
	mountpointID int64,
	prefix string,
) error {
	for k, obs := range t.observations {
		if obs.MountpointID == mountpointID && strings.HasPrefix(obs.Path, prefix) {
			delete(t.observations, k)
		}
	}
	return nil
}

func (t *fakeTx) MoveObservationSubtree(
	_ context.Context,
	mountpointID int64,
	oldPrefix, newPrefix string,
) error {
	for k, obs := range t.observations {
		if obs.MountpointID != mountpointID || !strings.HasPrefix(obs.Path, oldPrefix) {
			continue
		}
		delete(t.observations, k)
		obs.Path = newPrefix + strings.TrimPrefix(obs.Path, oldPrefix)
		t.observations[key(mountpointID, obs.Path)] = obs
	}
	return nil
}

func (t *fakeTx) SweepScanEpoch(
	_ context.Context,
	mountpointID int64,
	root string,
	epoch int64,
) ([]string, error) {
	var gone []string
	for k, obs := range t.observations {
		under := obs.Path == root || strings.HasPrefix(obs.Path, strings.TrimSuffix(root, "/")+"/")
		if obs.MountpointID != mountpointID || !under || obs.SeenEpoch == epoch {
			continue
		}
		gone = append(gone, obs.Path)
		delete(t.observations, k)
	}
	return gone, nil
}

func (t *fakeTx) live(id int64) bool { return !t.tombstoned[id] }

func (t *fakeTx) FindLiveDocumentByPath(
	_ context.Context,
	mountpointID int64,
	path string,
) (ingest.Document, bool, error) {
	for _, doc := range t.documents {
		if doc.MountpointID == mountpointID && doc.Path == path && t.live(doc.ID) {
			snapshot := *doc
			t.fireDocumentRead()
			return snapshot, true, nil
		}
	}
	return ingest.Document{}, false, nil
}

func (t *fakeTx) FindLiveDocumentByInode(
	_ context.Context,
	mountpointID, dev, ino int64,
) (ingest.Document, bool, error) {
	for _, doc := range t.documents {
		if doc.MountpointID == mountpointID && doc.Dev == dev && doc.Ino == ino && t.live(doc.ID) {
			return *doc, true, nil
		}
	}
	return ingest.Document{}, false, nil
}

// FindLiveDocumentByContent mirrors the query's NOT EXISTS guard: a document
// whose own path is still observed is a copy's source, not a move's.
func (t *fakeTx) FindLiveDocumentByContent(
	_ context.Context,
	mountpointID int64,
	contentHash string,
) (ingest.Document, bool, error) {
	for _, doc := range t.documents {
		if doc.MountpointID != mountpointID || !t.live(doc.ID) {
			continue
		}
		if t.hashByVersion[doc.CurrentContentVersionID] != contentHash {
			continue
		}
		if _, stillThere := t.observations[key(mountpointID, doc.Path)]; stillThere {
			continue
		}
		return *doc, true, nil
	}
	return ingest.Document{}, false, nil
}

func (t *fakeTx) CreateDocument(
	_ context.Context,
	mountpointID int64,
	path string,
	dev, ino int64,
) (ingest.Document, error) {
	t.nextDocID++
	doc := &ingest.Document{
		ID:           t.nextDocID,
		MountpointID: mountpointID,
		Path:         path,
		Dev:          dev,
		Ino:          ino,
	}
	t.documents[doc.ID] = doc
	return *doc, nil
}

func (t *fakeTx) MoveDocument(_ context.Context, id int64, path string, dev, ino int64) error {
	doc, ok := t.documents[id]
	if !ok {
		return fmt.Errorf("no document %d", id)
	}
	doc.Path, doc.Dev, doc.Ino = path, dev, ino
	return nil
}

func (t *fakeTx) MoveDocumentSubtree(
	_ context.Context,
	mountpointID int64,
	oldPrefix, newPrefix string,
) ([]ingest.Document, error) {
	var moved []ingest.Document
	for _, doc := range t.documents {
		if doc.MountpointID != mountpointID || !t.live(doc.ID) {
			continue
		}
		if !strings.HasPrefix(doc.Path, oldPrefix) {
			continue
		}
		doc.Path = newPrefix + strings.TrimPrefix(doc.Path, oldPrefix)
		moved = append(moved, *doc)
	}
	return moved, nil
}

func (t *fakeTx) TombstoneDocument(_ context.Context, id int64) error {
	t.tombstoned[id] = true
	return nil
}

func (t *fakeTx) TombstoneDocumentByPath(
	ctx context.Context,
	mountpointID int64,
	path string,
) (ingest.Document, bool, error) {
	doc, found, err := t.FindLiveDocumentByPath(ctx, mountpointID, path)
	if err != nil || !found {
		return ingest.Document{}, false, err
	}
	t.tombstoned[doc.ID] = true
	return doc, true, nil
}

func (t *fakeTx) TombstoneDocumentSubtree(
	_ context.Context,
	mountpointID int64,
	prefix string,
) ([]ingest.Document, error) {
	var out []ingest.Document
	for _, doc := range t.documents {
		if doc.MountpointID != mountpointID || !t.live(doc.ID) {
			continue
		}
		if !strings.HasPrefix(doc.Path, prefix) {
			continue
		}
		t.tombstoned[doc.ID] = true
		out = append(out, *doc)
	}
	return out, nil
}

func (t *fakeTx) UpsertContentVersion(
	_ context.Context,
	hash string,
	_ int64,
) (int64, error) {
	if id, ok := t.versionByHash[hash]; ok {
		return id, nil
	}
	t.nextVersionID++
	t.versionByHash[hash] = t.nextVersionID
	t.hashByVersion[t.nextVersionID] = hash
	return t.nextVersionID, nil
}

// setVersion is fixture setup: it puts a document at a version without going
// through the compare-and-swap, which a test arranging a starting state has no
// expectation to compare against.
func (t *fakeTx) setVersion(documentID, contentVersionID int64) {
	t.documents[documentID].CurrentContentVersionID = contentVersionID
}

// SetDocumentCurrentVersion mirrors the query's compare-and-swap: no row is
// touched, and none reported, when the expectation no longer holds.
func (t *fakeTx) SetDocumentCurrentVersion(
	_ context.Context,
	documentID, expected, contentVersionID int64,
) (bool, error) {
	doc, ok := t.documents[documentID]
	if !ok {
		return false, fmt.Errorf("no document %d", documentID)
	}
	if doc.CurrentContentVersionID != expected {
		return false, nil
	}
	doc.CurrentContentVersionID = contentVersionID
	return true, nil
}

func (t *fakeTx) MarkExtractionPending(_ context.Context, contentVersionID int64) error {
	t.pendingExtracts = append(t.pendingExtracts, contentVersionID)
	return nil
}

func (t *fakeTx) EnqueueIngestPath(
	_ context.Context,
	mountpointID int64,
	path string,
	trigger ingest.Trigger,
) error {
	t.queuedIngest = append(t.queuedIngest, ingestCall{mountpointID, path, trigger})
	return nil
}

func (t *fakeTx) EnqueueExtract(_ context.Context, contentVersionID int64) error {
	t.queuedExtract = append(t.queuedExtract, contentVersionID)
	return nil
}

func (t *fakeTx) EnqueueInvalidateThumbnails(
	_ context.Context,
	documentID, supersededContentVersionID int64,
) error {
	t.queuedInvalidate = append(
		t.queuedInvalidate,
		invalidateCall{documentID, supersededContentVersionID},
	)
	return nil
}

var _ ingest.Store = (*fakeStore)(nil)
var _ ingest.Tx = (*fakeTx)(nil)

// fakeWatcher replays a scripted subscription and scan instead of talking to a
// daemon, which needs CAP_SYS_ADMIN and a real filesystem.
type fakeWatcher struct {
	items   []*overwatchv1.SubscribeResponse
	scanned map[string][]*overwatchv1.Observation
	head    uint64
	scans   []string
	// scanErr fails the scan of one root, which is how a bootstrap covering
	// several mountpoints is interrupted part way through.
	scanErr map[string]error
	// roots is what the daemon reports it is watching, which is configured
	// separately from this process's mountpoints and can disagree with them.
	roots []string

	subCalls int
	// onResubscribe stops the loop, which otherwise reconnects forever the way
	// the real one is meant to. It fires once the scripted stream has been
	// consumed and the reconciler comes back for more.
	onResubscribe func()
}

func (w *fakeWatcher) Subscribe(
	_ context.Context,
	_ *overwatchv1.SubscribeRequest,
) (ingest.EventStream, error) {
	w.subCalls++
	if w.subCalls > 1 && w.onResubscribe != nil {
		w.onResubscribe()
	}
	return &sliceStream[overwatchv1.SubscribeResponse]{items: w.items}, nil
}

func (w *fakeWatcher) Scan(
	_ context.Context,
	req *overwatchv1.ScanRequest,
) (ingest.ScanStream, error) {
	w.scans = append(w.scans, req.GetRoot())
	if err, ok := w.scanErr[req.GetRoot()]; ok {
		return nil, err
	}
	obs := w.scanned[req.GetRoot()]
	return &sliceStream[overwatchv1.ScanResponse]{items: []*overwatchv1.ScanResponse{{
		Item: &overwatchv1.ScanResponse_Batch{
			Batch: &overwatchv1.ObservationBatch{Observations: obs},
		},
	}}}, nil
}

func (w *fakeWatcher) Status(
	_ context.Context,
	_ *overwatchv1.StatusRequest,
) (*overwatchv1.StatusResponse, error) {
	roots := make([]*overwatchv1.WatchRootStatus, 0, len(w.roots))
	for _, root := range w.roots {
		roots = append(roots, &overwatchv1.WatchRootStatus{Root: root})
	}
	return &overwatchv1.StatusResponse{RingHeadSeq: w.head, Roots: roots}, nil
}

type sliceStream[T any] struct {
	items []*T
	next  int
}

func (s *sliceStream[T]) Recv() (*T, error) {
	if s.next >= len(s.items) {
		return nil, io.EOF
	}
	item := s.items[s.next]
	s.next++
	return item, nil
}

// event builds a file event for the scripted stream.
func event(
	seq uint64,
	kind overwatchv1.EventKind,
	path string,
	stat *overwatchv1.StatInfo,
) *overwatchv1.SubscribeResponse {
	return &overwatchv1.SubscribeResponse{
		Item: &overwatchv1.SubscribeResponse_Event{
			Event: &overwatchv1.FileEvent{
				Seq:  seq,
				Kind: kind,
				Path: path,
				Stat: stat,
			},
		},
	}
}

func moveEvent(
	seq uint64,
	oldPath, newPath string,
	isDir bool,
	stat *overwatchv1.StatInfo,
) *overwatchv1.SubscribeResponse {
	return &overwatchv1.SubscribeResponse{
		Item: &overwatchv1.SubscribeResponse_Event{
			Event: &overwatchv1.FileEvent{
				Seq:     seq,
				Kind:    overwatchv1.EventKind_EVENT_KIND_MOVE,
				Path:    newPath,
				OldPath: oldPath,
				IsDir:   isDir,
				Stat:    stat,
			},
		},
	}
}

func deleteEvent(seq uint64, path string, isDir bool) *overwatchv1.SubscribeResponse {
	return &overwatchv1.SubscribeResponse{
		Item: &overwatchv1.SubscribeResponse_Event{
			Event: &overwatchv1.FileEvent{
				Seq:   seq,
				Kind:  overwatchv1.EventKind_EVENT_KIND_DELETE,
				Path:  path,
				IsDir: isDir,
			},
		},
	}
}

func gap(seq uint64) *overwatchv1.SubscribeResponse {
	return &overwatchv1.SubscribeResponse{
		Item: &overwatchv1.SubscribeResponse_Gap{
			Gap: &overwatchv1.GapMarker{
				Seq:  seq,
				Kind: overwatchv1.GapKind_GAP_KIND_OVERFLOW,
			},
		},
	}
}

var testMtime = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

func stat(size, ino uint64) *overwatchv1.StatInfo {
	return &overwatchv1.StatInfo{
		Size:  size,
		Ino:   ino,
		Dev:   64,
		Mtime: timestamppb.New(testMtime),
	}
}

func observation(path string, size, ino uint64) *overwatchv1.Observation {
	return &overwatchv1.Observation{Path: path, Stat: stat(size, ino)}
}

// requireIngestQueued fails unless exactly the given paths were queued.
func requireIngestQueued(t *testing.T, got []ingestCall, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("queued %v, want %v", paths(got), want)
	}
	for i, call := range got {
		if call.Path != want[i] {
			t.Fatalf("queued %v, want %v", paths(got), want)
		}
	}
}

func paths(calls []ingestCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Path)
	}
	return out
}
