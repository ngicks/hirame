package service_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"golang.org/x/sync/errgroup"

	hiramev1 "github.com/ngicks/hirame/apps/search-api/internal/gen/hirame/v1"
	"github.com/ngicks/hirame/apps/search-api/internal/service"
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
)

// The GUI's spec, so the fixtures exercise the sizes actually deployed.
var guiSpec = &hiramev1.ThumbnailSpec{
	MaxSize: &hiramev1.ImageSize{Width: 192, Height: 192},
	Format:  hiramev1.ImageFormat_IMAGE_FORMAT_WEBP,
}

// The key the handler derives for guiSpec at page 1: content-hash addressed, so
// a rename cannot orphan it. Spelled out rather than computed so a change to the
// scheme has to be made deliberately — existing objects are stored under it.
const guiObjectKey = "thumbnails/" + liveContentHash + "/p1/192x192.webp"

func newThumbnail(
	store *fakeStore, objects *fakeObjects, renderer service.PageRenderer,
) *service.Thumbnail {
	return service.NewThumbnail(
		store, objects, renderer, service.NewRenderLimit(2), discardLogger(),
	)
}

func getThumbnail(
	t *testing.T,
	handler *service.Thumbnail,
	req *hiramev1.GetThumbnailRequest,
) (*hiramev1.GetThumbnailResponse, error) {
	t.Helper()
	resp, err := handler.GetThumbnail(t.Context(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func guiRequest() *hiramev1.GetThumbnailRequest {
	return &hiramev1.GetThumbnailRequest{
		Ref:        liveRef(),
		PageNumber: 1,
		Spec:       guiSpec,
	}
}

// cachedRow is the accounting row an earlier request would have left behind.
func cachedRow() sqlcgen.ThumbnailCache {
	return sqlcgen.ThumbnailCache{
		ID:               11,
		ContentVersionID: liveVersionID,
		Page:             1,
		Width:            192,
		Height:           192,
		Format:           "webp",
		ObjectKey:        guiObjectKey,
		SizeBytes:        4,
	}
}

func TestGetThumbnailServesACacheHitWithoutRendering(t *testing.T) {
	store := newFakeStore()
	objects := newFakeObjects()
	renderer := newFakeRenderer([]byte("fresh"))

	store.putRow(cachedRow())
	objects.put(guiObjectKey, []byte("kept"))

	resp, err := getThumbnail(t, newThumbnail(store, objects, renderer), guiRequest())
	if err != nil {
		t.Fatalf("get thumbnail: %v", err)
	}

	if string(resp.GetImage()) != "kept" {
		t.Errorf("image = %q, want the cached bytes", resp.GetImage())
	}
	if renderer.callCount() != 0 {
		t.Errorf("the renderer ran %d times on a cache hit", renderer.callCount())
	}
	// Touched only once the bytes were in hand, so the entry keeps its place
	// in the LRU order the quota eviction reads.
	if len(store.touched) != 1 || store.touched[0] != cachedRow().ID {
		t.Errorf("touched = %v, want the hit row touched once", store.touched)
	}
	if resp.GetRef().GetContentVersionId() != liveContentHash {
		t.Errorf("response echoed version %q, want %q",
			resp.GetRef().GetContentVersionId(), liveContentHash)
	}
	if resp.GetFormat() != hiramev1.ImageFormat_IMAGE_FORMAT_WEBP {
		t.Errorf("format = %s, want webp", resp.GetFormat())
	}
}

func TestGetThumbnailRendersAndPublishesOnAMiss(t *testing.T) {
	store := newFakeStore()
	objects := newFakeObjects()
	renderer := newFakeRenderer([]byte("ren"), []byte("dered"))

	resp, err := getThumbnail(t, newThumbnail(store, objects, renderer), guiRequest())
	if err != nil {
		t.Fatalf("get thumbnail: %v", err)
	}

	if string(resp.GetImage()) != "rendered" {
		t.Errorf("image = %q, want the rendered chunks joined", resp.GetImage())
	}
	if !objects.has(guiObjectKey) {
		t.Errorf("nothing was stored under %q; objects present: %v",
			guiObjectKey, objects.puts)
	}

	upsert := store.lastUpsert(t)
	if upsert.ObjectKey != guiObjectKey {
		t.Errorf("accounting row points at %q, want %q", upsert.ObjectKey, guiObjectKey)
	}
	// size_bytes is what the cache-wide quota is enforced against (D-008), so
	// it has to be the stored length rather than an estimate.
	if upsert.SizeBytes != int64(len("rendered")) {
		t.Errorf("size_bytes = %d, want %d", upsert.SizeBytes, len("rendered"))
	}
	if upsert.ContentVersionID != liveVersionID {
		t.Errorf("row keyed by version %d, want %d", upsert.ContentVersionID, liveVersionID)
	}
}

// The invariant: an object is only ever reachable through a live accounting
// row. Invalidation deletes rows first and objects second, so bytes left in the
// bucket must be re-rendered rather than served.
func TestGetThumbnailNeverServesAnObjectWhoseRowIsGone(t *testing.T) {
	store := newFakeStore()
	objects := newFakeObjects()
	renderer := newFakeRenderer([]byte("regenerated"))

	// Exactly the state invalidation leaves behind between its two steps.
	objects.put(guiObjectKey, []byte("stale bytes from the previous content"))
	store.dropRows()

	resp, err := getThumbnail(t, newThumbnail(store, objects, renderer), guiRequest())
	if err != nil {
		t.Fatalf("get thumbnail: %v", err)
	}

	if string(resp.GetImage()) == "stale bytes from the previous content" {
		t.Fatal("served an orphaned object; the accounting row is the only way in")
	}
	if string(resp.GetImage()) != "regenerated" {
		t.Errorf("image = %q, want the re-rendered bytes", resp.GetImage())
	}
	if renderer.callCount() != 1 {
		t.Errorf("the renderer ran %d times, want 1", renderer.callCount())
	}
	if objects.getCount() != 0 {
		t.Errorf("the bucket was read %d times without a row to authorize it",
			objects.getCount())
	}
}

// The reverse gap — a row whose object is missing — is a miss, not a failure.
// Eviction cannot produce it, but a bucket restored from an older backup can.
func TestGetThumbnailRegeneratesWhenTheRowSurvivesButTheObjectDoesNot(t *testing.T) {
	store := newFakeStore()
	objects := newFakeObjects()
	renderer := newFakeRenderer([]byte("regenerated"))

	store.putRow(cachedRow())

	resp, err := getThumbnail(t, newThumbnail(store, objects, renderer), guiRequest())
	if err != nil {
		t.Fatalf("get thumbnail: %v", err)
	}
	if string(resp.GetImage()) != "regenerated" {
		t.Errorf("image = %q, want the re-rendered bytes", resp.GetImage())
	}
	// A lookup that missed in the object store must not extend the entry's
	// life; only the re-render's upsert refreshes it.
	if len(store.touched) != 0 {
		t.Errorf("touched = %v, want an object-store miss not to touch the row",
			store.touched)
	}
	if !objects.has(guiObjectKey) {
		t.Error("the regenerated bytes were not stored")
	}
}

// The object has to be durable before anything advertises it: a row pointing at
// bytes that never arrived is a live 404 on every later read, and it keeps
// counting against the quota.
func TestGetThumbnailWritesNoAccountingRowWhenTheObjectCannotBeStored(t *testing.T) {
	store := newFakeStore()
	objects := newFakeObjects()
	objects.putErr = errors.New("the gateway refused the upload")
	renderer := newFakeRenderer([]byte("rendered"))

	_, err := getThumbnail(t, newThumbnail(store, objects, renderer), guiRequest())
	if err == nil {
		t.Fatal("a failed upload was reported as success")
	}
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("code = %s, want unavailable", connect.CodeOf(err))
	}
	if store.upsertCount() != 0 {
		t.Errorf("wrote %d accounting rows for an object that was never stored",
			store.upsertCount())
	}
}

// An object nothing accounts for is unreachable: it counts against no quota,
// answers no lookup, and the next request renders and stores it all over again.
// Serving through it would hide a cache that has stopped caching.
func TestGetThumbnailFailsWhenTheAccountingRowCannotBeWritten(t *testing.T) {
	store := newFakeStore()
	store.upsertErr = errors.New("the database refused the write")
	objects := newFakeObjects()
	renderer := newFakeRenderer([]byte("rendered"))

	_, err := getThumbnail(t, newThumbnail(store, objects, renderer), guiRequest())
	if err == nil {
		t.Fatal("an unrecorded thumbnail was served as success")
	}
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("code = %s, want unavailable", connect.CodeOf(err))
	}
}

// A search page paints many thumbnails at once, and documents sharing content
// share an entry; without suppression each request would render again.
func TestGetThumbnailCollapsesConcurrentMissesIntoOneRender(t *testing.T) {
	store := newFakeStore()
	objects := newFakeObjects()
	renderer := newFakeRenderer([]byte("once"))
	renderer.block = make(chan struct{})
	renderer.released = make(chan struct{})
	handler := newThumbnail(store, objects, renderer)

	const callers = 8
	var group errgroup.Group
	results := make([][]byte, callers)

	// The first caller is let in and held inside the renderer, so the rest
	// arrive while it is still running.
	group.Go(func() error {
		resp, err := handler.GetThumbnail(t.Context(), connect.NewRequest(guiRequest()))
		if err != nil {
			return err
		}
		results[0] = resp.Msg.GetImage()
		return nil
	})
	<-renderer.released

	for i := 1; i < callers; i++ {
		group.Go(func() error {
			resp, err := handler.GetThumbnail(t.Context(), connect.NewRequest(guiRequest()))
			if err != nil {
				return err
			}
			results[i] = resp.Msg.GetImage()
			return nil
		})
	}
	close(renderer.block)
	if err := group.Wait(); err != nil {
		t.Fatalf("a caller failed: %v", err)
	}

	if renderer.callCount() != 1 {
		t.Errorf("the renderer ran %d times, want 1 for %d concurrent callers",
			renderer.callCount(), callers)
	}
	for i := range callers {
		if string(results[i]) != "once" {
			t.Errorf("caller %d got %q, want the shared render", i, results[i])
		}
	}
}

// The render is detached so a leader's disconnect cannot fail the followers
// still waiting on it. Detached alone would mean a disconnecting client leaves a
// render holding a slot in the limit for the whole of renderLifetime, which a
// handful of them turns into a starved cache. It ends when the last caller
// leaves instead.
func TestGetThumbnailCancelsARenderNobodyIsWaitingForAnyMore(t *testing.T) {
	store := newFakeStore()
	objects := newFakeObjects()
	renderer := newFakeRenderer([]byte("never delivered"))
	// Held open with no release, so only cancellation can end it.
	renderer.block = make(chan struct{})
	renderer.released = make(chan struct{})
	handler := newThumbnail(store, objects, renderer)

	ctx, disconnect := context.WithCancel(t.Context())
	var group errgroup.Group
	group.Go(func() error {
		_, err := handler.GetThumbnail(ctx, connect.NewRequest(guiRequest()))
		if connect.CodeOf(err) != connect.CodeCanceled {
			return fmt.Errorf("code = %s, want canceled", connect.CodeOf(err))
		}
		return nil
	})

	<-renderer.released
	disconnect()
	if err := group.Wait(); err != nil {
		t.Fatalf("the disconnecting caller: %v", err)
	}

	// The render's own context has to be done well inside renderLifetime, which
	// is the whole point: the next caller finds the slot free.
	if err := renderer.awaitContextDone(t); err != nil {
		t.Fatalf("the render outlived every caller waiting for it: %v", err)
	}
}

// A caller that leaves while others are still waiting must not take the render
// down with it: the rest are still holding live requests.
func TestGetThumbnailKeepsRenderingForTheCallersStillWaiting(t *testing.T) {
	store := newFakeStore()
	objects := newFakeObjects()
	renderer := newFakeRenderer([]byte("shared"))
	renderer.block = make(chan struct{})
	renderer.released = make(chan struct{})
	handler := newThumbnail(store, objects, renderer)

	leaving, disconnect := context.WithCancel(t.Context())
	var group errgroup.Group
	group.Go(func() error {
		_, err := handler.GetThumbnail(leaving, connect.NewRequest(guiRequest()))
		if connect.CodeOf(err) != connect.CodeCanceled {
			return fmt.Errorf("code = %s, want canceled", connect.CodeOf(err))
		}
		return nil
	})
	<-renderer.released

	stayed := make(chan []byte, 1)
	group.Go(func() error {
		resp, err := handler.GetThumbnail(t.Context(), connect.NewRequest(guiRequest()))
		if err != nil {
			return err
		}
		stayed <- resp.Msg.GetImage()
		return nil
	})
	// Give the second caller time to join the party before the first departs.
	waitForWaiters(t, handler, 2)

	disconnect()
	// The render is only released once the departure has actually landed, so a
	// cancellation it should not have caused has already had its chance.
	waitForWaiters(t, handler, 1)

	close(renderer.block)
	if err := group.Wait(); err != nil {
		t.Fatalf("a caller failed: %v", err)
	}
	if got := <-stayed; string(got) != "shared" {
		t.Errorf("the remaining caller got %q, want the render it waited for", got)
	}
}

func TestGetThumbnailRejectsARefThatIsNoLongerCurrent(t *testing.T) {
	store := newFakeStore()
	objects := newFakeObjects()
	renderer := newFakeRenderer([]byte("x"))

	// The GUI recovers from exactly this code by re-reading the document and
	// retrying once against whatever version it is on now.
	_, err := getThumbnail(t, newThumbnail(store, objects, renderer),
		&hiramev1.GetThumbnailRequest{
			Ref: &hiramev1.DocumentRef{
				DocumentId:       "7",
				ContentVersionId: "superseded",
			},
			PageNumber: 1,
			Spec:       guiSpec,
		})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("code = %s, want failed_precondition", connect.CodeOf(err))
	}
	if renderer.callCount() != 0 {
		t.Error("a superseded ref reached the renderer")
	}
}

func TestGetThumbnailReportsAnUnknownDocumentAsNotFound(t *testing.T) {
	_, err := getThumbnail(t,
		newThumbnail(newFakeStore(), newFakeObjects(), newFakeRenderer()),
		&hiramev1.GetThumbnailRequest{
			Ref:        &hiramev1.DocumentRef{DocumentId: "999", ContentVersionId: "x"},
			PageNumber: 1,
			Spec:       guiSpec,
		})
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %s, want not_found", connect.CodeOf(err))
	}
}

func TestGetThumbnailRejectsPageZero(t *testing.T) {
	req := guiRequest()
	req.PageNumber = 0

	_, err := getThumbnail(t,
		newThumbnail(newFakeStore(), newFakeObjects(), newFakeRenderer()), req)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want invalid_argument", connect.CodeOf(err))
	}
}

// The spec is half the cache key, so an unquantized size lets any caller mint
// unbounded distinct entries for one page. Quantizing before the key is built
// is what keeps two nearly-identical requests on one object, and the response
// reports the quantized size so both callers agree about what they got.
func TestGetThumbnailQuantizesTheSpecBeforeKeyingTheCache(t *testing.T) {
	for _, tc := range []struct {
		name          string
		size          *hiramev1.ImageSize
		wantW, wantH  uint32
		wantObjectKey string
	}{
		{
			name: "unset selects the server default",
			size: nil, wantW: 192, wantH: 192,
			wantObjectKey: "thumbnails/" + liveContentHash + "/p1/192x192.webp",
		},
		{
			name:  "an odd size rounds up to the quantum",
			size:  &hiramev1.ImageSize{Width: 200, Height: 130},
			wantW: 256, wantH: 192,
			wantObjectKey: "thumbnails/" + liveContentHash + "/p1/256x192.webp",
		},
		{
			name:  "an enormous size is capped",
			size:  &hiramev1.ImageSize{Width: 99999, Height: 99999},
			wantW: 1024, wantH: 1024,
			wantObjectKey: "thumbnails/" + liveContentHash + "/p1/1024x1024.webp",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			objects := newFakeObjects()
			renderer := newFakeRenderer([]byte("x"))

			resp, err := getThumbnail(t, newThumbnail(store, objects, renderer),
				&hiramev1.GetThumbnailRequest{
					Ref:        liveRef(),
					PageNumber: 1,
					Spec: &hiramev1.ThumbnailSpec{
						MaxSize: tc.size,
						Format:  hiramev1.ImageFormat_IMAGE_FORMAT_WEBP,
					},
				})
			if err != nil {
				t.Fatalf("get thumbnail: %v", err)
			}

			if resp.GetSize().GetWidth() != tc.wantW ||
				resp.GetSize().GetHeight() != tc.wantH {
				t.Errorf("reported size = %dx%d, want the quantized %dx%d",
					resp.GetSize().GetWidth(), resp.GetSize().GetHeight(),
					tc.wantW, tc.wantH)
			}
			if got := store.lastUpsert(t).ObjectKey; got != tc.wantObjectKey {
				t.Errorf("object key = %q, want %q", got, tc.wantObjectKey)
			}
			// The renderer must be asked for the quantized shape too, or the
			// stored object would not be what the key claims.
			call := renderer.lastCall(t)
			if call.MaxWidth != tc.wantW || call.MaxHeight != tc.wantH {
				t.Errorf("rendered at %dx%d, want %dx%d",
					call.MaxWidth, call.MaxHeight, tc.wantW, tc.wantH)
			}
		})
	}
}

func TestGetThumbnailResolvesTheServerDefaultFormat(t *testing.T) {
	store := newFakeStore()
	req := guiRequest()
	req.Spec = &hiramev1.ThumbnailSpec{}

	resp, err := getThumbnail(t,
		newThumbnail(store, newFakeObjects(), newFakeRenderer([]byte("x"))), req)
	if err != nil {
		t.Fatalf("get thumbnail: %v", err)
	}
	if resp.GetFormat() != hiramev1.ImageFormat_IMAGE_FORMAT_WEBP {
		t.Errorf("format = %s, want the webp default", resp.GetFormat())
	}
	if got := store.lastUpsert(t).Format; got != "webp" {
		t.Errorf("row format = %q, want webp", got)
	}
}
