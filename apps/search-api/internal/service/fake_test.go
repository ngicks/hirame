package service_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ngicks/hirame/apps/search-api/internal/gahakuclient"
	"github.com/ngicks/hirame/apps/search-api/internal/objstore"
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
)

// The fixture every handler test addresses: one live document, one content
// version, and the file it was observed at.
const (
	liveDocumentID   = int64(7)
	liveVersionID    = int64(42)
	liveContentHash  = "1f0c" // stands in for a sha-256; opacity is the point
	liveRoot         = "/srv/docs"
	liveAbsolutePath = "/srv/docs/報告/2026.pdf"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// liveDocument is a document whose current version is liveContentHash.
func liveDocument() sqlcgen.GetDocumentRow {
	return sqlcgen.GetDocumentRow{
		ID:                      liveDocumentID,
		MountpointID:            1,
		Path:                    liveAbsolutePath,
		RootPath:                liveRoot,
		CurrentContentVersionID: new(liveVersionID),
		ContentHash:             new(liveContentHash),
		SizeBytes:               new(int64(1024)),
		ExtractionStatus:        new("succeeded"),
		ContentType:             new("application/pdf"),
		Metadata:                []byte(`{"xmpTPg:NPages":["3"]}`),
		HasExtractedText:        true,
		FirstSeenAt:             pgtype.Timestamptz{Time: time.Unix(1700000000, 0), Valid: true},
		UpdatedAt:               pgtype.Timestamptz{Time: time.Unix(1700000100, 0), Valid: true},
		ContentFirstSeenAt:      pgtype.Timestamptz{Time: time.Unix(1699999000, 0), Valid: true},
	}
}

// tombstonedDocument keeps its row but has no current version (D-007).
func tombstonedDocument() sqlcgen.GetDocumentRow {
	row := liveDocument()
	row.DeletedAt = pgtype.Timestamptz{Time: time.Unix(1700000200, 0), Valid: true}
	row.CurrentContentVersionID = nil
	row.ContentHash = nil
	return row
}

// fakeStore is the database as far as the handlers can see it.
type fakeStore struct {
	mu sync.Mutex

	documents map[int64]sqlcgen.GetDocumentRow
	// sourceMissing makes FindDocumentSourcePath report no observation, which
	// is a file that disappeared after its ref checked out.
	sourceMissing bool

	searchRows  []sqlcgen.SearchDocumentsRow
	searchTotal int64
	searchCalls []sqlcgen.SearchDocumentsParams

	thumbnails map[string]sqlcgen.ThumbnailCache
	upserts    []sqlcgen.UpsertThumbnailParams
	touched    []int64
	nextID     int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		documents:  map[int64]sqlcgen.GetDocumentRow{liveDocumentID: liveDocument()},
		thumbnails: map[string]sqlcgen.ThumbnailCache{},
		nextID:     1,
	}
}

func (f *fakeStore) GetDocument(_ context.Context, id int64) (sqlcgen.GetDocumentRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.documents[id]
	if !ok {
		return sqlcgen.GetDocumentRow{}, pgx.ErrNoRows
	}
	return row, nil
}

func (f *fakeStore) FindDocumentSourcePath(
	_ context.Context, arg sqlcgen.FindDocumentSourcePathParams,
) (sqlcgen.FindDocumentSourcePathRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sourceMissing {
		return sqlcgen.FindDocumentSourcePathRow{}, pgx.ErrNoRows
	}
	row, ok := f.documents[arg.ID]
	if !ok || row.CurrentContentVersionID == nil || arg.CurrentContentVersionID == nil ||
		*row.CurrentContentVersionID != *arg.CurrentContentVersionID {
		return sqlcgen.FindDocumentSourcePathRow{}, pgx.ErrNoRows
	}
	return sqlcgen.FindDocumentSourcePathRow{Path: row.Path, Size: 1024}, nil
}

func (f *fakeStore) SearchDocuments(
	_ context.Context, arg sqlcgen.SearchDocumentsParams,
) ([]sqlcgen.SearchDocumentsRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchCalls = append(f.searchCalls, arg)

	offset := int(arg.ResultOffset)
	if offset >= len(f.searchRows) {
		return nil, nil
	}
	end := min(offset+int(arg.ResultLimit), len(f.searchRows))
	return f.searchRows[offset:end], nil
}

func (f *fakeStore) CountSearchDocuments(_ context.Context, _ string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.searchTotal, nil
}

func thumbnailKey(arg sqlcgen.GetThumbnailParams) string {
	return objstoreKey(arg.ContentVersionID, arg.Page, arg.Width, arg.Height, arg.Format)
}

func objstoreKey(versionID int64, page, width, height int32, format string) string {
	return fmt.Sprintf("%d/%d/%dx%d.%s", versionID, page, width, height, format)
}

func (f *fakeStore) GetThumbnail(
	_ context.Context, arg sqlcgen.GetThumbnailParams,
) (sqlcgen.ThumbnailCache, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.thumbnails[thumbnailKey(arg)]
	if !ok {
		return sqlcgen.ThumbnailCache{}, pgx.ErrNoRows
	}
	return row, nil
}

func (f *fakeStore) TouchThumbnail(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, id)
	return nil
}

func (f *fakeStore) UpsertThumbnail(
	_ context.Context, arg sqlcgen.UpsertThumbnailParams,
) (sqlcgen.ThumbnailCache, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts = append(f.upserts, arg)
	row := sqlcgen.ThumbnailCache{
		ID:               f.nextID,
		ContentVersionID: arg.ContentVersionID,
		Page:             arg.Page,
		Width:            arg.Width,
		Height:           arg.Height,
		Format:           arg.Format,
		ObjectKey:        arg.ObjectKey,
		SizeBytes:        arg.SizeBytes,
	}
	f.nextID++
	f.thumbnails[objstoreKey(arg.ContentVersionID, arg.Page, arg.Width, arg.Height, arg.Format)] = row
	return row, nil
}

// putRow installs an accounting row directly, standing in for an entry an
// earlier request stored.
func (f *fakeStore) putRow(row sqlcgen.ThumbnailCache) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.thumbnails[objstoreKey(row.ContentVersionID, row.Page, row.Width, row.Height, row.Format)] = row
}

// dropRows removes every accounting row, which is what invalidation does
// before it touches the objects.
func (f *fakeStore) dropRows() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.thumbnails = map[string]sqlcgen.ThumbnailCache{}
}

func (f *fakeStore) upsertCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.upserts)
}

func (f *fakeStore) lastUpsert(t *testing.T) sqlcgen.UpsertThumbnailParams {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.upserts) == 0 {
		t.Fatal("no thumbnail accounting row was written")
	}
	return f.upserts[len(f.upserts)-1]
}

// fakeObjects is the bucket.
type fakeObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
	gets    []string
	puts    []string
	// putErr fails every Put, which must stop an accounting row from appearing.
	putErr error
}

func newFakeObjects() *fakeObjects {
	return &fakeObjects{objects: map[string][]byte{}}
}

func (o *fakeObjects) Get(_ context.Context, key string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.gets = append(o.gets, key)
	body, ok := o.objects[key]
	if !ok {
		return nil, objstore.ErrNoSuchKey
	}
	return body, nil
}

func (o *fakeObjects) Put(_ context.Context, key string, body []byte, _ string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.putErr != nil {
		return o.putErr
	}
	o.puts = append(o.puts, key)
	o.objects[key] = body
	return nil
}

func (o *fakeObjects) put(key string, body []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.objects[key] = body
}

func (o *fakeObjects) has(key string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.objects[key]
	return ok
}

func (o *fakeObjects) getCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.gets)
}

// fakeRenderer stands in for Gahaku.
type fakeRenderer struct {
	mu       sync.Mutex
	chunks   [][]byte
	err      error
	calls    []gahakuclient.PageRequest
	released chan struct{}
	// block holds a render open so a test can observe two callers colliding.
	block chan struct{}
}

func newFakeRenderer(chunks ...[]byte) *fakeRenderer {
	return &fakeRenderer{chunks: chunks}
}

func (r *fakeRenderer) RenderPage(
	ctx context.Context, req gahakuclient.PageRequest, chunk func([]byte) error,
) error {
	r.mu.Lock()
	r.calls = append(r.calls, req)
	block, released, err := r.block, r.released, r.err
	chunks := r.chunks
	r.mu.Unlock()

	if released != nil {
		close(released)
		r.mu.Lock()
		r.released = nil
		r.mu.Unlock()
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	for _, body := range chunks {
		if sendErr := chunk(body); sendErr != nil {
			return sendErr
		}
	}
	return nil
}

func (r *fakeRenderer) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *fakeRenderer) lastCall(t *testing.T) gahakuclient.PageRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		t.Fatal("the renderer was never called")
	}
	return r.calls[len(r.calls)-1]
}

var errRenderFailed = errors.New("renderer said no")
