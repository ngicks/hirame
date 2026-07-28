package ingest_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	overwatchv1 "github.com/ngicks/go-overwatch/overwatch/pkg/api/gen/proto/go/overwatch/v1"

	"github.com/ngicks/hirame/apps/search-api/internal/doctype"
	"github.com/ngicks/hirame/apps/search-api/internal/ingest"
)

const testRoot = "/srv/docs"

func newReconciler(store *fakeStore, watcher *fakeWatcher) *ingest.Reconciler {
	return ingest.New(
		watcher,
		store,
		doctype.NewFilter(nil),
		[]ingest.Mountpoint{{ID: 1, Root: testRoot}},
		discardLogger(),
		time.Millisecond,
	)
}

// applyAll feeds events straight to the apply path, which is the whole event
// state machine without the reconnect loop around it.
func applyAll(t *testing.T, store *fakeStore, items []*overwatchv1.SubscribeResponse) {
	t.Helper()
	r := newReconciler(store, &fakeWatcher{items: items})
	for _, item := range items {
		ev := item.GetEvent()
		if ev == nil {
			continue
		}
		if err := r.ApplyEvent(t.Context(), ev); err != nil {
			t.Fatalf("apply event seq %d: %v", ev.GetSeq(), err)
		}
	}
}

// runToReconnect drives Run over the scripted stream and stops it when the
// reconciler comes back for a second subscription. Run itself only returns on
// cancellation, because a daemon restart must not end the process.
func runToReconnect(t *testing.T, store *fakeStore, watcher *fakeWatcher) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watcher.onResubscribe = cancel

	if err := newReconciler(store, watcher).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestCreateRecordsObservationAndQueuesIngest(t *testing.T) {
	store := newFakeStore()
	applyAll(t, store, []*overwatchv1.SubscribeResponse{
		event(1, overwatchv1.EventKind_EVENT_KIND_CREATE, testRoot+"/a.pdf", stat(10, 100)),
	})

	if _, ok := store.tx.observations[key(1, testRoot+"/a.pdf")]; !ok {
		t.Fatal("no observation recorded for the created file")
	}
	requireIngestQueued(t, store.tx.queuedIngest, testRoot+"/a.pdf")
	if got := store.tx.queuedIngest[0].Trigger; got != ingest.TriggerDebounced {
		t.Errorf("trigger = %v, want debounced", got)
	}
	if store.tx.watermark != 1 {
		t.Errorf("watermark = %d, want 1", store.tx.watermark)
	}
}

func TestExcludedExtensionIsObservedButNeverQueued(t *testing.T) {
	store := newFakeStore()
	applyAll(t, store, []*overwatchv1.SubscribeResponse{
		event(1, overwatchv1.EventKind_EVENT_KIND_CREATE, testRoot+"/disk.img", stat(10, 100)),
		event(2, overwatchv1.EventKind_EVENT_KIND_CREATE, testRoot+"/.a.pdf.swp", stat(10, 101)),
		event(3, overwatchv1.EventKind_EVENT_KIND_CREATE, testRoot+"/~$memo.docx", stat(10, 102)),
		event(4, overwatchv1.EventKind_EVENT_KIND_CREATE, testRoot+"/memo.docx~", stat(10, 103)),
	})

	if len(store.tx.queuedIngest) != 0 {
		t.Errorf("queued %v, want nothing", paths(store.tx.queuedIngest))
	}
	if len(store.tx.observations) != 4 {
		t.Errorf("recorded %d observations, want 4", len(store.tx.observations))
	}
}

func TestCloseWriteQueuesWithoutDebounce(t *testing.T) {
	store := newFakeStore()
	applyAll(t, store, []*overwatchv1.SubscribeResponse{
		event(1, overwatchv1.EventKind_EVENT_KIND_MODIFY, testRoot+"/a.pdf", stat(10, 100)),
		event(2, overwatchv1.EventKind_EVENT_KIND_MODIFY, testRoot+"/a.pdf", stat(20, 100)),
		event(3, overwatchv1.EventKind_EVENT_KIND_CLOSE_WRITE, testRoot+"/a.pdf", stat(30, 100)),
	})

	triggers := make([]ingest.Trigger, 0, len(store.tx.queuedIngest))
	for _, call := range store.tx.queuedIngest {
		triggers = append(triggers, call.Trigger)
	}
	want := []ingest.Trigger{
		ingest.TriggerDebounced, ingest.TriggerDebounced, ingest.TriggerSettled,
	}
	if len(triggers) != len(want) {
		t.Fatalf("triggers = %v, want %v", triggers, want)
	}
	for i := range want {
		if triggers[i] != want[i] {
			t.Fatalf("triggers = %v, want %v", triggers, want)
		}
	}
}

func TestAttribRefreshesObservationWithoutQueueingWork(t *testing.T) {
	store := newFakeStore()
	applyAll(t, store, []*overwatchv1.SubscribeResponse{
		event(1, overwatchv1.EventKind_EVENT_KIND_ATTRIB, testRoot+"/a.pdf", stat(10, 100)),
	})

	if _, ok := store.tx.observations[key(1, testRoot+"/a.pdf")]; !ok {
		t.Fatal("chmod did not refresh the observation")
	}
	if len(store.tx.queuedIngest) != 0 {
		t.Errorf("queued %v, want nothing: a chmod moves no bytes", paths(store.tx.queuedIngest))
	}
}

func TestDuplicateEventIsDroppedByTheWatermark(t *testing.T) {
	store := newFakeStore()
	create := event(1, overwatchv1.EventKind_EVENT_KIND_CREATE, testRoot+"/a.pdf", stat(10, 100))
	applyAll(t, store, []*overwatchv1.SubscribeResponse{create, create, create})

	requireIngestQueued(t, store.tx.queuedIngest, testRoot+"/a.pdf")
}

func TestDeleteTombstonesTheDocumentAndInvalidatesItsThumbnails(t *testing.T) {
	store := newFakeStore()
	doc, _ := store.tx.CreateDocument(t.Context(), 1, testRoot+"/a.pdf", 64, 100)
	_ = store.tx.SetDocumentCurrentVersion(t.Context(), doc.ID, 7)
	_ = store.tx.UpsertObservation(t.Context(), ingest.Observation{
		MountpointID: 1, Path: testRoot + "/a.pdf",
	})

	applyAll(t, store, []*overwatchv1.SubscribeResponse{
		deleteEvent(1, testRoot+"/a.pdf", false),
	})

	if !store.tx.tombstoned[doc.ID] {
		t.Error("document was not tombstoned")
	}
	if _, ok := store.tx.observations[key(1, testRoot+"/a.pdf")]; ok {
		t.Error("observation survived the delete")
	}
	if len(store.tx.queuedInvalidate) != 1 ||
		store.tx.queuedInvalidate[0].SupersededTarget != 7 {
		t.Errorf("invalidations = %v, want one for version 7", store.tx.queuedInvalidate)
	}
}

func TestDeletingADirectoryTombstonesEverythingBeneathIt(t *testing.T) {
	store := newFakeStore()
	inner, _ := store.tx.CreateDocument(t.Context(), 1, testRoot+"/sub/a.pdf", 64, 100)
	_ = store.tx.SetDocumentCurrentVersion(t.Context(), inner.ID, 9)
	outside, _ := store.tx.CreateDocument(t.Context(), 1, testRoot+"/other.pdf", 64, 101)

	applyAll(t, store, []*overwatchv1.SubscribeResponse{
		deleteEvent(1, testRoot+"/sub", true),
	})

	if !store.tx.tombstoned[inner.ID] {
		t.Error("document inside the deleted directory survived")
	}
	if store.tx.tombstoned[outside.ID] {
		t.Error("document outside the deleted directory was tombstoned")
	}
	if len(store.tx.queuedInvalidate) != 1 {
		t.Errorf("invalidations = %v, want one", store.tx.queuedInvalidate)
	}
}

func TestRenameKeepsDocumentIdentity(t *testing.T) {
	store := newFakeStore()
	doc, _ := store.tx.CreateDocument(t.Context(), 1, testRoot+"/old.pdf", 64, 100)
	_ = store.tx.UpsertObservation(t.Context(), ingest.Observation{
		MountpointID: 1, Path: testRoot + "/old.pdf", Ino: 100, Dev: 64,
	})

	applyAll(t, store, []*overwatchv1.SubscribeResponse{
		moveEvent(1, testRoot+"/old.pdf", testRoot+"/new.pdf", false, stat(10, 100)),
	})

	if store.tx.documents[doc.ID].Path != testRoot+"/new.pdf" {
		t.Errorf("document path = %q, want the new path", store.tx.documents[doc.ID].Path)
	}
	if store.tx.tombstoned[doc.ID] {
		t.Error("a rename tombstoned the document instead of moving it")
	}
	if _, ok := store.tx.observations[key(1, testRoot+"/old.pdf")]; ok {
		t.Error("observation for the source path survived the rename")
	}
}

func TestRenamingToAnExcludedNameTombstonesTheDocument(t *testing.T) {
	store := newFakeStore()
	doc, _ := store.tx.CreateDocument(t.Context(), 1, testRoot+"/report.pdf", 64, 100)
	_ = store.tx.SetDocumentCurrentVersion(t.Context(), doc.ID, 3)

	applyAll(t, store, []*overwatchv1.SubscribeResponse{
		moveEvent(1, testRoot+"/report.pdf", testRoot+"/report.pdf.bak", false, stat(10, 100)),
	})

	if !store.tx.tombstoned[doc.ID] {
		t.Error("renaming out of the include set left the document live")
	}
	if len(store.tx.queuedInvalidate) != 1 {
		t.Errorf("invalidations = %v, want one", store.tx.queuedInvalidate)
	}
	if len(store.tx.queuedIngest) != 0 {
		t.Errorf("queued %v, want nothing", paths(store.tx.queuedIngest))
	}
}

// A rename onto a live path is how editors publish: the destination keeps its
// identity and only its content changes, so the document must survive and be
// re-read rather than be replaced.
func TestWriteToTempThenRenameKeepsTheDestinationDocument(t *testing.T) {
	store := newFakeStore()
	doc, _ := store.tx.CreateDocument(t.Context(), 1, testRoot+"/report.pdf", 64, 100)
	_ = store.tx.SetDocumentCurrentVersion(t.Context(), doc.ID, 3)
	_ = store.tx.UpsertObservation(t.Context(), ingest.Observation{
		MountpointID: 1, Path: testRoot + "/report.pdf", Ino: 100, Dev: 64,
	})

	applyAll(t, store, []*overwatchv1.SubscribeResponse{
		// The temp file is excluded by name, so it never became a document.
		event(1, overwatchv1.EventKind_EVENT_KIND_CREATE,
			testRoot+"/.report.pdf.tmp", stat(10, 200)),
		moveEvent(2, testRoot+"/.report.pdf.tmp", testRoot+"/report.pdf", false, stat(10, 200)),
	})

	if store.tx.tombstoned[doc.ID] {
		t.Error("the published document was tombstoned instead of kept")
	}
	if len(store.tx.documents) != 1 {
		t.Errorf("documents = %d, want the original one only", len(store.tx.documents))
	}
	requireIngestQueued(t, store.tx.queuedIngest, testRoot+"/report.pdf")
	if _, ok := store.tx.observations[key(1, testRoot+"/.report.pdf.tmp")]; ok {
		t.Error("the temp path's observation survived, which would hide a later move")
	}
}

func TestRenamingOverAnExistingDocumentTombstonesTheOverwrittenOne(t *testing.T) {
	store := newFakeStore()
	source, _ := store.tx.CreateDocument(t.Context(), 1, testRoot+"/new.pdf", 64, 100)
	victim, _ := store.tx.CreateDocument(t.Context(), 1, testRoot+"/old.pdf", 64, 101)
	_ = store.tx.SetDocumentCurrentVersion(t.Context(), victim.ID, 5)

	applyAll(t, store, []*overwatchv1.SubscribeResponse{
		moveEvent(1, testRoot+"/new.pdf", testRoot+"/old.pdf", false, stat(10, 100)),
	})

	if !store.tx.tombstoned[victim.ID] {
		t.Error("the overwritten document was not tombstoned")
	}
	if store.tx.tombstoned[source.ID] {
		t.Error("the renamed document was tombstoned")
	}
	if store.tx.documents[source.ID].Path != testRoot+"/old.pdf" {
		t.Errorf("renamed document path = %q", store.tx.documents[source.ID].Path)
	}
}

func TestRenamingADirectoryRepathsItsContentsWithoutReExtraction(t *testing.T) {
	store := newFakeStore()
	doc, _ := store.tx.CreateDocument(t.Context(), 1, testRoot+"/old/a.pdf", 64, 100)
	_ = store.tx.SetDocumentCurrentVersion(t.Context(), doc.ID, 4)
	_ = store.tx.UpsertObservation(t.Context(), ingest.Observation{
		MountpointID: 1, Path: testRoot + "/old/a.pdf",
	})

	applyAll(t, store, []*overwatchv1.SubscribeResponse{
		moveEvent(1, testRoot+"/old", testRoot+"/new", true, stat(0, 50)),
	})

	if got := store.tx.documents[doc.ID].Path; got != testRoot+"/new/a.pdf" {
		t.Errorf("document path = %q, want the re-pathed one", got)
	}
	if store.tx.tombstoned[doc.ID] {
		t.Error("a directory rename tombstoned a document beneath it")
	}
	if _, ok := store.tx.observations[key(1, testRoot+"/new/a.pdf")]; !ok {
		t.Error("observation beneath the renamed directory was not re-pathed")
	}
	if len(store.tx.queuedIngest) != 0 {
		t.Errorf("queued %v, want nothing: the bytes did not move",
			paths(store.tx.queuedIngest))
	}
	if len(store.tx.queuedExtract) != 0 {
		t.Errorf("extractions = %v, want none", store.tx.queuedExtract)
	}
}

func TestMovingOutOfEveryWatchedRootIsADeletion(t *testing.T) {
	store := newFakeStore()
	doc, _ := store.tx.CreateDocument(t.Context(), 1, testRoot+"/a.pdf", 64, 100)
	_ = store.tx.SetDocumentCurrentVersion(t.Context(), doc.ID, 2)

	applyAll(t, store, []*overwatchv1.SubscribeResponse{
		moveEvent(1, testRoot+"/a.pdf", "/elsewhere/a.pdf", false, stat(10, 100)),
	})

	if !store.tx.tombstoned[doc.ID] {
		t.Error("moving outside every watched root left the document live")
	}
}

func TestGapTriggersARescanThatSweepsMissedDeletes(t *testing.T) {
	store := newFakeStore()
	// State from before the gap: two files, one of which is gone by the time
	// the rescan runs.
	survivor, _ := store.tx.CreateDocument(t.Context(), 1, testRoot+"/kept.pdf", 64, 100)
	vanished, _ := store.tx.CreateDocument(t.Context(), 1, testRoot+"/gone.pdf", 64, 101)
	_ = store.tx.SetDocumentCurrentVersion(t.Context(), vanished.ID, 8)
	for _, p := range []string{testRoot + "/kept.pdf", testRoot + "/gone.pdf"} {
		_ = store.tx.UpsertObservation(t.Context(), ingest.Observation{
			MountpointID: 1, Path: p, Size: 10, Mtime: testMtime,
		})
	}

	watcher := &fakeWatcher{
		items: []*overwatchv1.SubscribeResponse{gap(5)},
		head:  9,
		scanned: map[string][]*overwatchv1.Observation{
			testRoot: {observation(testRoot+"/kept.pdf", 10, 100)},
		},
	}
	runToReconnect(t, store, watcher)

	if len(watcher.scans) == 0 {
		t.Fatal("the gap did not trigger a scan")
	}
	if !store.tx.tombstoned[vanished.ID] {
		t.Error("the file that disappeared during the gap was not tombstoned")
	}
	if store.tx.tombstoned[survivor.ID] {
		t.Error("the file that survived the gap was tombstoned")
	}
	if len(store.tx.queuedInvalidate) != 1 {
		t.Errorf("invalidations = %v, want one for the vanished file",
			store.tx.queuedInvalidate)
	}
	if store.tx.watermark != 9 {
		t.Errorf("watermark = %d, want the ring head 9", store.tx.watermark)
	}
}

// A rescan re-reads every file, so it must not re-queue the ones it can already
// account for; only genuinely new or changed files are worth hashing again.
func TestRescanOnlyQueuesFilesItCannotAccountFor(t *testing.T) {
	store := newFakeStore()
	unchanged, _ := store.tx.CreateDocument(t.Context(), 1, testRoot+"/same.pdf", 64, 100)
	_ = store.tx.SetDocumentCurrentVersion(t.Context(), unchanged.ID, 1)
	for _, p := range []string{testRoot + "/same.pdf", testRoot + "/grown.pdf"} {
		_ = store.tx.UpsertObservation(t.Context(), ingest.Observation{
			MountpointID: 1, Path: p, Size: 10, Mtime: testMtime,
		})
	}
	// grown.pdf is observed but has no document: a run interrupted part way
	// through its first scan leaves exactly this state.

	watcher := &fakeWatcher{
		items: []*overwatchv1.SubscribeResponse{gap(1)},
		head:  4,
		scanned: map[string][]*overwatchv1.Observation{
			testRoot: {
				observation(testRoot+"/same.pdf", 10, 100),
				observation(testRoot+"/grown.pdf", 10, 101),
				observation(testRoot+"/fresh.pdf", 99, 102),
			},
		},
	}
	runToReconnect(t, store, watcher)

	queued := map[string]bool{}
	for _, call := range store.tx.queuedIngest {
		queued[call.Path] = true
	}
	if queued[testRoot+"/same.pdf"] {
		t.Error("an unchanged file with a live document was queued again")
	}
	for _, want := range []string{testRoot + "/grown.pdf", testRoot + "/fresh.pdf"} {
		if !queued[want] {
			t.Errorf("%s was not queued", want)
		}
	}
}

func TestBootstrapScanRunsWhenTheStoreHasNeverBeenReconciled(t *testing.T) {
	store := newFakeStore()
	watcher := &fakeWatcher{
		head: 3,
		scanned: map[string][]*overwatchv1.Observation{
			testRoot: {observation(testRoot+"/a.pdf", 10, 100)},
		},
	}
	runToReconnect(t, store, watcher)

	if len(watcher.scans) == 0 {
		t.Fatal("an empty store did not bootstrap with a scan")
	}
	requireIngestQueued(t, store.tx.queuedIngest, testRoot+"/a.pdf")
	if store.tx.watermark != 3 {
		t.Errorf("watermark = %d, want the ring head captured before the scan", store.tx.watermark)
	}
}

// A mountpoint the daemon does not watch produces no events at all, which
// looks exactly like an idle filesystem. Only a log line distinguishes them.
func TestAMountpointNoWatchRootCoversIsReported(t *testing.T) {
	for _, tc := range []struct {
		name     string
		roots    []string
		wantWarn bool
	}{
		{"exact match", []string{testRoot}, false},
		{"daemon watches a parent", []string{"/srv"}, false},
		{"trailing slash", []string{testRoot + "/"}, false},
		{"unrelated root", []string{"/var/spool"}, true},
		{"sibling with a shared prefix", []string{testRoot + "-archive"}, true},
		{"daemon watches nothing", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			r := ingest.New(
				&fakeWatcher{roots: tc.roots},
				newFakeStore(),
				doctype.NewFilter(nil),
				[]ingest.Mountpoint{{ID: 1, Root: testRoot}},
				slog.New(slog.NewTextHandler(&logged, nil)),
				time.Millisecond,
			)

			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_ = r.Run(ctx)

			warned := strings.Contains(logged.String(), "outside every daemon watch root")
			if warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v; log was %s", warned, tc.wantWarn, logged.String())
			}
		})
	}
}

func TestPathsOutsideEveryMountpointAreIgnored(t *testing.T) {
	store := newFakeStore()
	applyAll(t, store, []*overwatchv1.SubscribeResponse{
		event(1, overwatchv1.EventKind_EVENT_KIND_CREATE, "/somewhere/else/a.pdf", stat(10, 100)),
	})

	if len(store.tx.observations) != 0 {
		t.Errorf("recorded %d observations for an unwatched path", len(store.tx.observations))
	}
	if store.tx.watermark != 1 {
		t.Errorf("watermark = %d, want 1: the event was still consumed", store.tx.watermark)
	}
}
