package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/singleflight"

	"github.com/ngicks/hirame/apps/search-api/internal/gahakuclient"
	hiramev1 "github.com/ngicks/hirame/apps/search-api/internal/gen/hirame/v1"
	"github.com/ngicks/hirame/apps/search-api/internal/objstore"
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
)

// renderLifetime bounds a thumbnail render that outlives the request that
// asked for it. See Thumbnail.load for why one does.
const renderLifetime = 5 * time.Minute

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
// through its accounting row. Invalidation and eviction delete the row first
// and the object second (internal/jobs), so a row that is gone means the entry
// is gone even while its bytes linger, and an object nothing points at is
// unreachable rather than stale. Reading the bucket directly — by guessing the
// key, or by trusting a key without re-reading its row — would turn that
// deliberate ordering into a stale-image bug.
type Thumbnail struct {
	store    ThumbnailStore
	objects  ObjectStore
	renderer PageRenderer
	limit    *RenderLimit
	// inflight collapses concurrent misses for one entry into one render. The
	// alternative is a thundering herd: a search page paints twenty thumbnails
	// at once and a shared content version would be rendered once per request.
	inflight singleflight.Group
	logger   *slog.Logger
}

// NewThumbnail builds the handler. limit is shared with RenderService; logger
// must not be nil.
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
// The render runs on a context detached from this request. singleflight shares
// one call's outcome with every caller waiting on it, so a leader carrying the
// inbound context would fail all of its followers the moment its own client
// navigated away — the followers are still waiting and their requests are still
// live. The detached context keeps its own deadline so nothing runs unbounded.
func (t *Thumbnail) load(
	ctx context.Context,
	row sqlcgen.GetDocumentRow,
	key entry,
) ([]byte, error) {
	if image, ok, err := t.fromCache(ctx, key); err != nil || ok {
		return image, err
	}

	rendered, err, _ := t.inflight.Do(key.singleflightKey(), func() (any, error) {
		renderCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), renderLifetime,
		)
		defer cancel()

		// Re-checked inside the group: a caller that queued behind the leader
		// would otherwise render again over the entry the leader just stored.
		if image, ok, err := t.fromCache(renderCtx, key); err != nil || ok {
			return image, err
		}
		return t.render(renderCtx, row, key)
	})
	if err != nil {
		return nil, err
	}
	image, ok := rendered.([]byte)
	if !ok {
		return nil, connect.NewError(
			connect.CodeInternal,
			errors.New("thumbnail render produced no image"),
		)
	}
	return image, nil
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
// Object first, accounting row second — the exact reverse of the delete path,
// and for the same reason. The row is what makes an object reachable, so a
// crash between the two leaves bytes nothing can resolve, which the eviction
// sweep collects. Writing the row first would advertise an entry whose object
// may never arrive: every later read would resolve the key, fail to fetch it,
// and the row would keep counting against the quota.
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
		// The bytes are already stored and correct, so the caller is served.
		// The row is what the eviction job accounts against, and its absence
		// leaves the object unreachable — collected by the stale sweep rather
		// than served to anyone.
		t.logger.ErrorContext(ctx, "record thumbnail accounting row",
			slog.String("object_key", objectKey),
			slog.Any("error", err),
		)
	}
	return image.Bytes(), nil
}
