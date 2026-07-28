package ingest_test

import (
	"bytes"
	"io"
	"io/fs"
	"testing"

	"github.com/ngicks/hirame/apps/search-api/internal/doctype"
	"github.com/ngicks/hirame/apps/search-api/internal/ingest"
)

// fakeOpener serves file contents from memory.
type fakeOpener struct {
	files map[string]string
}

func (o fakeOpener) Open(path string) (io.ReadCloser, error) {
	body, ok := o.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader([]byte(body))), nil
}

func newProcessor(store *fakeStore, files map[string]string) *ingest.Processor {
	return ingest.NewProcessor(
		store,
		doctype.NewFilter(nil),
		fakeOpener{files: files},
		discardLogger(),
	)
}

// seedObservation records what the event loop would have recorded before the
// job runs.
func seedObservation(t *testing.T, store *fakeStore, path string, ino int64) {
	t.Helper()
	err := store.tx.UpsertObservation(t.Context(), ingest.Observation{
		MountpointID: 1,
		Path:         path,
		Ino:          ino,
		Dev:          64,
		Mtime:        testMtime,
	})
	if err != nil {
		t.Fatalf("seed observation: %v", err)
	}
}

func TestProcessingANewFileCreatesADocumentAndQueuesExtraction(t *testing.T) {
	store := newFakeStore()
	path := testRoot + "/a.pdf"
	seedObservation(t, store, path, 100)

	p := newProcessor(store, map[string]string{path: "%PDF-1.4 hello"})
	if err := p.ProcessPath(t.Context(), 1, path); err != nil {
		t.Fatalf("process: %v", err)
	}

	if len(store.tx.documents) != 1 {
		t.Fatalf("documents = %d, want 1", len(store.tx.documents))
	}
	for _, doc := range store.tx.documents {
		if doc.CurrentContentVersionID == 0 {
			t.Error("document has no current content version")
		}
	}
	if len(store.tx.queuedExtract) != 1 {
		t.Errorf("extractions = %v, want one", store.tx.queuedExtract)
	}
	if len(store.tx.pendingExtracts) != 1 {
		t.Errorf("pending marks = %v, want one", store.tx.pendingExtracts)
	}
	if len(store.tx.queuedInvalidate) != 0 {
		t.Errorf("invalidations = %v, want none: nothing was superseded",
			store.tx.queuedInvalidate)
	}
}

// Filesystem events arrive duplicated, and a rescan re-reads everything. Both
// must stop here, at the hash, rather than re-extracting identical bytes.
func TestReprocessingUnchangedBytesChangesNothing(t *testing.T) {
	store := newFakeStore()
	path := testRoot + "/a.pdf"
	seedObservation(t, store, path, 100)
	p := newProcessor(store, map[string]string{path: "%PDF-1.4 hello"})

	if err := p.ProcessPath(t.Context(), 1, path); err != nil {
		t.Fatalf("first process: %v", err)
	}
	if err := p.ProcessPath(t.Context(), 1, path); err != nil {
		t.Fatalf("second process: %v", err)
	}

	if len(store.tx.documents) != 1 {
		t.Errorf("documents = %d, want 1", len(store.tx.documents))
	}
	if len(store.tx.queuedExtract) != 1 {
		t.Errorf("extractions = %v, want exactly one", store.tx.queuedExtract)
	}
	if len(store.tx.queuedInvalidate) != 0 {
		t.Errorf("invalidations = %v, want none", store.tx.queuedInvalidate)
	}
}

func TestChangedBytesSupersedeTheVersionAndInvalidateItsThumbnails(t *testing.T) {
	store := newFakeStore()
	path := testRoot + "/a.pdf"
	seedObservation(t, store, path, 100)

	files := map[string]string{path: "%PDF-1.4 first"}
	p := newProcessor(store, files)
	if err := p.ProcessPath(t.Context(), 1, path); err != nil {
		t.Fatalf("first process: %v", err)
	}
	var docID, firstVersion int64
	for _, doc := range store.tx.documents {
		docID, firstVersion = doc.ID, doc.CurrentContentVersionID
	}

	files[path] = "%PDF-1.4 second, quite different"
	if err := p.ProcessPath(t.Context(), 1, path); err != nil {
		t.Fatalf("second process: %v", err)
	}

	doc := store.tx.documents[docID]
	if doc.CurrentContentVersionID == firstVersion {
		t.Fatal("the document still points at the old content version")
	}
	if len(store.tx.queuedExtract) != 2 {
		t.Errorf("extractions = %v, want two", store.tx.queuedExtract)
	}
	if len(store.tx.queuedInvalidate) != 1 {
		t.Fatalf("invalidations = %v, want one", store.tx.queuedInvalidate)
	}
	got := store.tx.queuedInvalidate[0]
	if got.DocumentID != docID || got.SupersededTarget != firstVersion {
		t.Errorf("invalidation = %+v, want document %d superseding version %d",
			got, docID, firstVersion)
	}
}

// The inode survives a rename, which is the cheapest identity signal there is.
func TestARenamedFileKeepsItsDocumentViaTheInode(t *testing.T) {
	store := newFakeStore()
	oldPath, newPath := testRoot+"/old.pdf", testRoot+"/new.pdf"
	body := "%PDF-1.4 hello"

	seedObservation(t, store, oldPath, 100)
	p := newProcessor(store, map[string]string{oldPath: body})
	if err := p.ProcessPath(t.Context(), 1, oldPath); err != nil {
		t.Fatalf("initial process: %v", err)
	}

	// What the move handler leaves behind: source observation gone, same inode
	// at the new path.
	_ = store.tx.DeleteObservation(t.Context(), 1, oldPath)
	seedObservation(t, store, newPath, 100)

	p2 := newProcessor(store, map[string]string{newPath: body})
	if err := p2.ProcessPath(t.Context(), 1, newPath); err != nil {
		t.Fatalf("process after rename: %v", err)
	}

	if len(store.tx.documents) != 1 {
		t.Fatalf("documents = %d, want the original one reused", len(store.tx.documents))
	}
	for _, doc := range store.tx.documents {
		if doc.Path != newPath {
			t.Errorf("document path = %q, want %q", doc.Path, newPath)
		}
	}
	if len(store.tx.queuedExtract) != 1 {
		t.Errorf("extractions = %v, want one: the bytes never changed",
			store.tx.queuedExtract)
	}
}

// Write-to-temp-then-rename changes the inode too, so only the content hash can
// still recognise the file — and only once the old path has stopped being
// observed, which is what tells a move apart from a copy.
func TestIdenticalBytesAtANewPathReuseTheDocumentOnlyAfterTheOldPathIsGone(t *testing.T) {
	store := newFakeStore()
	oldPath, newPath := testRoot+"/old.pdf", testRoot+"/new.pdf"
	body := "%PDF-1.4 hello"

	seedObservation(t, store, oldPath, 100)
	p := newProcessor(store, map[string]string{oldPath: body})
	if err := p.ProcessPath(t.Context(), 1, oldPath); err != nil {
		t.Fatalf("initial process: %v", err)
	}

	// A copy: both paths exist, and the inode differs.
	seedObservation(t, store, newPath, 200)
	if err := newProcessor(store, map[string]string{newPath: body}).
		ProcessPath(t.Context(), 1, newPath); err != nil {
		t.Fatalf("process the copy: %v", err)
	}
	if len(store.tx.documents) != 2 {
		t.Fatalf("documents = %d, want 2: a copy is its own document",
			len(store.tx.documents))
	}

	// A move: the source path stops being observed, and a third path with the
	// same bytes and yet another inode appears.
	movedPath := testRoot + "/moved.pdf"
	_ = store.tx.DeleteObservation(t.Context(), 1, oldPath)
	seedObservation(t, store, movedPath, 300)
	if err := newProcessor(store, map[string]string{movedPath: body}).
		ProcessPath(t.Context(), 1, movedPath); err != nil {
		t.Fatalf("process the move: %v", err)
	}
	if len(store.tx.documents) != 2 {
		t.Errorf("documents = %d, want 2: the move reused the orphaned document",
			len(store.tx.documents))
	}
}

func TestAFileThatDisappearedBeforeTheJobRanIsNotAnError(t *testing.T) {
	store := newFakeStore()
	path := testRoot + "/a.pdf"
	seedObservation(t, store, path, 100)

	// Observed, but the bytes are already gone.
	if err := newProcessor(store, nil).ProcessPath(t.Context(), 1, path); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(store.tx.documents) != 0 {
		t.Errorf("documents = %d, want none", len(store.tx.documents))
	}
}

func TestAnUnobservedPathIsNotProcessed(t *testing.T) {
	store := newFakeStore()
	path := testRoot + "/a.pdf"

	err := newProcessor(store, map[string]string{path: "%PDF-1.4"}).
		ProcessPath(t.Context(), 1, path)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(store.tx.documents) != 0 {
		t.Errorf("documents = %d, want none", len(store.tx.documents))
	}
}

func TestBinaryContentUnderATextExtensionIsRejected(t *testing.T) {
	store := newFakeStore()
	path := testRoot + "/notes.txt"
	seedObservation(t, store, path, 100)

	err := newProcessor(store, map[string]string{path: "\x00\x01\x02\x03binary"}).
		ProcessPath(t.Context(), 1, path)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(store.tx.documents) != 0 {
		t.Errorf("documents = %d, want none", len(store.tx.documents))
	}
}
