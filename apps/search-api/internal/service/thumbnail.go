package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/singleflight"

	"github.com/ngicks/hirame/apps/search-api/internal/gahakuclient"
	hiramev1 "github.com/ngicks/hirame/apps/search-api/internal/gen/hirame/v1"
	"github.com/ngicks/hirame/apps/search-api/internal/objstore"
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
)

// renderLifetime bounds a thumbnail render that outlives the request that asked
// for it. See Thumbnail.load for why one does.
//
// It is deliberately short. The detached render holds a slot in the thumbnail
// render limit for its whole life, and every second past the point where the
// waiters have gone is a slot held for nobody; a thumbnail that has not been
// produced in two minutes is not going to be worth the wait either.
const renderLifetime = 2 * time.Minute

// ObjectStore is the thumbnail bytes' home in VersityGW. *objstore.Store
// satisfies it.
type ObjectStore interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Put(ctx context.Context, key string, body []byte, contentType string) error
}

// ThumbnailStore is the cache-accounting surface. *sqlcgen.Queries satisfies
// it as generated.
type ThumbnailStore interface {
	DocumentStore
	SourceStore
	GetThumbnail(
		ctx context.Context, arg sqlcgen.GetThumbnailParams,
	) (sqlcgen.ThumbnailCache, error)
	TouchThumbnail(ctx context.Context, id int64) error
	UpsertThumbnail(
		ctx context.Context, arg sqlcgen.UpsertThumbnailParams,
	) (sqlcgen.ThumbnailCache, error)
}

// Thumbnail serves cached page previews.
//
// The cache invariant this type exists to hold: an object is only ever served
// through its accounting row. Invalidation and eviction withdraw the row first
// and remove the object second (internal/jobs), and a withdrawn row is one
// GetThumbnail no longer returns — so an entry stops being servable the moment
// it is withdrawn, even while its bytes linger, and an object nothing points at
// is unreachable rather than stale. Reading the bucket directly — by guessing
// the key, or by trusting a key without re-reading its row — would turn that
// deliberate ordering into a stale-image bug.
type Thumbnail struct {
	store    ThumbnailStore
	objects  ObjectStore
	renderer PageRenderer
	// limit is the thumbnail render cap, separate from the full-size one. See
	// [RenderLimit] for why the two are not shared.
	limit *RenderLimit
	// inflight collapses concurrent misses for one entry into one render. The
	// alternative is a thundering herd: a search page paints twenty thumbnails
	// at once and a shared content version would be rendered once per request.
	inflight singleflight.Group
	// parties tracks who is still waiting on each in-flight render, so the
	// render can be cancelled once nobody is. See renderParty.
	mu      sync.Mutex
	parties map[string]*renderParty
	logger  *slog.Logger
}

// renderParty is the set of callers still waiting on one entry's render, and
// the context the render runs on.
//
// The render is detached from every caller's request (see Thumbnail.load), which
// on its own means a client that navigates away leaves a render holding a slot
// in the limit for the rest of renderLifetime. Counting the callers is what
// closes that: the last one to leave cancels the render, so a slot is only ever
// held for work somebody is still waiting for.
type renderParty struct {
	ctx     context.Context
	cancel  context.CancelFunc
	waiting int
}

// NewThumbnail builds the handler. limit is this handler's own render cap, not
// RenderService's; logger must not be nil.
func NewThumbnail(
	store ThumbnailStore,
	objects ObjectStore,
	renderer PageRenderer,
	limit *RenderLimit,
	logger *slog.Logger,
) *Thumbnail {
	return &Thumbnail{
		store:    store,
		objects:  objects,
		renderer: renderer,
		limit:    limit,
		parties:  map[string]*renderParty{},
		logger:   logger,
	}
}

// GetThumbnail implements hiramev1connect.ThumbnailServiceHandler.
func (t *Thumbnail) GetThumbnail(
	ctx context.Context,
	req *connect.Request[hiramev1.GetThumbnailRequest],
) (*connect.Response[hiramev1.GetThumbnailResponse], error) {
	msg := req.Msg

	row, err := resolveRef(ctx, t.store, msg.GetRef())
	if err != nil {
		return nil, err
	}
	page := msg.GetPageNumber()
	if err := checkPage(page, row); err != nil {
		return nil, err
	}
	format, err := resolveFormat(msg.GetSpec().GetFormat(), defaultThumbnailFormat)
	if err != nil {
		return nil, err
	}

	key := entry{
		contentVersionID: *row.CurrentContentVersionID,
		contentHash:      *row.ContentHash,
		page:             page,
		// Quantized before the key is built, so the response reports what the
		// stored object actually is.
		bounds: thumbnailBounds(msg.GetSpec().GetMaxSize()),
		format: format,
	}

	image, err := t.load(ctx, row, key)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&hiramev1.GetThumbnailResponse{
		Ref:        currentRef(row),
		PageNumber: page,
		Format:     format.proto,
		Size:       key.bounds.message(),
		Image:      image,
	}), nil
}

// load resolves one entry, rendering it if the cache cannot answer.
//
// The render runs on a context detached from every caller's request.
// singleflight shares one call's outcome with everyone waiting on it, so a
// leader carrying its own inbound context would fail all of its followers the
// moment its client navigated away — the followers are still waiting and their
// requests are still live. Detached, the render belongs to the party rather than
// to whichever caller happened to start it, and it ends when the party empties
// or renderLifetime expires, whichever comes first.
//
// DoChan rather than Do, because Do blocks its caller past any cancellation:
// a client that disconnects would stay counted as a waiter, which is exactly the
// case the party exists to notice.
func (t *Thumbnail) load(
	ctx context.Context,
	row sqlcgen.GetDocumentRow,
	key entry,
) ([]byte, error) {
	if image, ok, err := t.fromCache(ctx, key); err != nil || ok {
		return image, err
	}

	group := key.singleflightKey()
	renderCtx, leave := t.join(ctx, group)
	defer leave()

	results := t.inflight.DoChan(group, func() (any, error) {
		// Re-checked inside the group: a caller that queued behind the leader
		// would otherwise render again over the entry the leader just stored.
		if image, ok, err := t.fromCache(renderCtx, key); err != nil || ok {
			return image, err
		}
		return t.render(renderCtx, row, key)
	})

	select {
	case <-ctx.Done():
		// This caller is gone. leave drops it from the party, and the render
		// stops once the last one has done the same.
		return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
	case result := <-results:
		if result.Err != nil {
			return nil, result.Err
		}
		image, ok := result.Val.([]byte)
		if !ok {
			return nil, connect.NewError(
				connect.CodeInternal,
				errors.New("thumbnail render produced no image"),
			)
		}
		return image, nil
	}
}

// join adds this caller to the party rendering group, returning the context the
// render runs on and the function that removes the caller again.
//
// The first caller in builds the context; the last one out cancels it. ctx
// contributes its values but not its cancellation, so the render outlives any
// single request while still ending when no request wants it.
func (t *Thumbnail) join(ctx context.Context, group string) (context.Context, func()) {
	t.mu.Lock()
	party, ok := t.parties[group]
	if !ok {
		renderCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), renderLifetime,
		)
		party = &renderParty{ctx: renderCtx, cancel: cancel}
		t.parties[group] = party
	}
	party.waiting++
	t.mu.Unlock()

	return party.ctx, func() {
		t.mu.Lock()
		party.waiting--
		last := party.waiting == 0
		if last && t.parties[group] == party {
			delete(t.parties, group)
		}
		t.mu.Unlock()
		if last {
			party.cancel()
		}
	}
}

// fromCache answers from the cache, reporting whether it could.
//
// Every read starts at the accounting row and only then touches the bucket. An
// object that has gone missing under a live row is reported as a miss rather
// than an error: eviction cannot produce that state, but a bucket restored from
// an older backup can, and re-rendering is both correct and cheaper than
// failing.
func (t *Thumbnail) fromCache(ctx context.Context, key entry) ([]byte, bool, error) {
	row, err := t.store.GetThumbnail(ctx, sqlcgen.GetThumbnailParams{
		ContentVersionID: key.contentVersionID,
		Page:             int32(key.page),
		Width:            int32(key.bounds.width),
		Height:           int32(key.bounds.height),
		Format:           key.format.name,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, connect.NewError(
			connect.CodeInternal,
			fmt.Errorf("look up thumbnail accounting row: %w", err),
		)
	}

	image, err := t.objects.Get(ctx, row.ObjectKey)
	if errors.Is(err, objstore.ErrNoSuchKey) {
		t.logger.WarnContext(ctx, "thumbnail row without an object, re-rendering",
			slog.String("object_key", row.ObjectKey),
			slog.Int64("thumbnail_id", row.ID),
		)
		return nil, false, nil
	}
	if err != nil {
		return nil, false, connect.NewError(
			connect.CodeUnavailable,
			fmt.Errorf("read cached thumbnail: %w", err),
		)
	}

	// Touched only once the bytes are in hand, so a lookup that missed in the
	// object store does not extend the entry's life.
	if err := t.store.TouchThumbnail(ctx, row.ID); err != nil {
		// The entry is served either way; a failed touch only costs it its
		// place in the LRU order.
		t.logger.WarnContext(ctx, "touch thumbnail",
			slog.Int64("thumbnail_id", row.ID),
			slog.Any("error", err),
		)
	}
	return image, true, nil
}

// render produces an entry's bytes and publishes it to the cache.
//
// Object first, accounting row second — the exact reverse of the removal path,
// and for the same reason. The row is what makes an object reachable, so a
// crash between the two leaves bytes nothing can resolve, which the cache sweep
// collects. Writing the row first would advertise an entry whose object may
// never arrive: every later read would resolve the key, fail to fetch it, and
// the row would keep counting against the quota.
func (t *Thumbnail) render(
	ctx context.Context,
	row sqlcgen.GetDocumentRow,
	key entry,
) ([]byte, error) {
	if err := t.limit.acquire(ctx); err != nil {
		return nil, err
	}
	defer t.limit.release()

	source, err := sourcePath(ctx, t.store, row)
	if err != nil {
		return nil, err
	}

	var image bytes.Buffer
	err = t.renderer.RenderPage(ctx, gahakuclient.PageRequest{
		SourcePath: source,
		Page:       key.page,
		Format:     key.format.rendererFmt,
		MaxWidth:   key.bounds.width,
		MaxHeight:  key.bounds.height,
	}, func(body []byte) error {
		image.Write(body)
		return nil
	})
	if err != nil {
		return nil, renderError(err)
	}
	if image.Len() == 0 {
		return nil, connect.NewError(
			connect.CodeInternal,
			fmt.Errorf("renderer produced no bytes for %s", key.objectKey()),
		)
	}

	objectKey := key.objectKey()
	if err := t.objects.Put(ctx, objectKey, image.Bytes(), key.format.mediaType); err != nil {
		return nil, connect.NewError(
			connect.CodeUnavailable,
			fmt.Errorf("store thumbnail: %w", err),
		)
	}

	if _, err := t.store.UpsertThumbnail(ctx, sqlcgen.UpsertThumbnailParams{
		ContentVersionID: key.contentVersionID,
		Page:             int32(key.page),
		Width:            int32(key.bounds.width),
		Height:           int32(key.bounds.height),
		Format:           key.format.name,
		ObjectKey:        objectKey,
		// size_bytes is what the quota is enforced against (D-008), so it is
		// the stored length rather than an estimate.
		SizeBytes: int64(image.Len()),
	}); err != nil {
		// Failed rather than served. Without the row the object is unreachable
		// and unaccounted for: it counts against nothing the quota can see, and
		// the next request would render and store it all over again. Answering
		// here would hide a cache that has quietly stopped caching.
		//
		// The object is left where it is rather than deleted: the key is
		// derived from the content hash, so the next attempt overwrites it, and
		// the sweep collects it if no attempt ever comes.
		t.logger.ErrorContext(ctx, "record thumbnail accounting row",
			slog.String("object_key", objectKey),
			slog.Any("error", err),
		)
		return nil, connect.NewError(
			connect.CodeUnavailable,
			fmt.Errorf("record thumbnail accounting row: %w", err),
		)
	}
	return image.Bytes(), nil
}
