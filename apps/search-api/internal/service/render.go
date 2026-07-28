package service

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ngicks/hirame/apps/search-api/internal/gahakuclient"
	hiramev1 "github.com/ngicks/hirame/apps/search-api/internal/gen/hirame/v1"
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
)

// PageRenderer turns one page of a document into an encoded image.
//
// The bytes come back through a callback rather than as a slice so a full-size
// render can be forwarded frame by frame without ever being held whole, which
// is what makes D-008's "stream it, never cache it" affordable.
// *gahakuclient.Client satisfies it.
type PageRenderer interface {
	RenderPage(
		ctx context.Context, req gahakuclient.PageRequest, chunk func([]byte) error,
	) error
}

// SourceStore resolves the file on disk behind a document's current version.
//
// It takes both halves of the ref rather than the content version alone
// because content is deduplicated by hash (D-007): one version is legitimately
// current for several documents, and only a document names a path.
// *sqlcgen.Queries satisfies it as generated.
type SourceStore interface {
	FindDocumentSourcePath(
		ctx context.Context, arg sqlcgen.FindDocumentSourcePathParams,
	) (sqlcgen.FindDocumentSourcePathRow, error)
}

// RenderStore is the render path's database surface. *sqlcgen.Queries
// satisfies it as generated.
type RenderStore interface {
	DocumentStore
	SourceStore
}

// sourcePath resolves the file a resolved row's current version lives in.
//
// A missing row means the file went away after the ref checked out, which is
// FAILED_PRECONDITION rather than NOT_FOUND: the client's next step is to
// re-read the document, where it learns the document is now a tombstone.
func sourcePath(
	ctx context.Context,
	store SourceStore,
	row sqlcgen.GetDocumentRow,
) (string, error) {
	source, err := store.FindDocumentSourcePath(ctx, sqlcgen.FindDocumentSourcePathParams{
		ID:                      row.ID,
		CurrentContentVersionID: row.CurrentContentVersionID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", connect.NewError(
			connect.CodeFailedPrecondition,
			fmt.Errorf("no readable file for document %d at its current version", row.ID),
		)
	}
	if err != nil {
		return "", connect.NewError(
			connect.CodeInternal,
			fmt.Errorf("resolve source path for document %d: %w", row.ID, err),
		)
	}
	return source.Path, nil
}

// RenderLimit bounds how many Gahaku streams a process holds at once.
//
// It is a shared value rather than a field each handler builds for itself,
// because what is being limited is not either service but the renderer behind
// both of them: Gahaku's own queue is the bottleneck, and
// config.Concurrency.Render describes the process, not the RPC. Two private
// semaphores of size n would let the process open 2n streams while the
// configured knob still read n.
type RenderLimit struct {
	streams *semaphore.Weighted
}

// NewRenderLimit builds the shared cap. n below 1 is raised to 1.
func NewRenderLimit(n int) *RenderLimit {
	return &RenderLimit{streams: semaphore.NewWeighted(int64(max(n, 1)))}
}

func (l *RenderLimit) acquire(ctx context.Context) error {
	if err := l.streams.Acquire(ctx, 1); err != nil {
		return connect.NewError(connect.CodeCanceled, err)
	}
	return nil
}

func (l *RenderLimit) release() { l.streams.Release(1) }

// Render streams full-size page images produced on demand.
type Render struct {
	store    RenderStore
	renderer PageRenderer
	limit    *RenderLimit
}

// NewRender builds the handler.
func NewRender(store RenderStore, renderer PageRenderer, limit *RenderLimit) *Render {
	return &Render{store: store, renderer: renderer, limit: limit}
}

// RenderPage implements hiramev1connect.RenderServiceHandler.
//
// Nothing is cached (D-008) and nothing is buffered: the info frame goes out as
// soon as the ref checks out, then each slice Gahaku produces is forwarded as
// it arrives. Sending on the response stream inside the renderer's callback is
// what applies backpressure — a slow client slows the read from Gahaku instead
// of growing a buffer here.
func (r *Render) RenderPage(
	ctx context.Context,
	req *connect.Request[hiramev1.RenderPageRequest],
	stream *connect.ServerStream[hiramev1.RenderPageResponse],
) error {
	msg := req.Msg

	row, err := resolveRef(ctx, r.store, msg.GetRef())
	if err != nil {
		return err
	}
	page := msg.GetPageNumber()
	if err := checkPage(page, row); err != nil {
		return err
	}
	format, err := resolveFormat(msg.GetFormat(), defaultRenderFormat)
	if err != nil {
		return err
	}
	bounds := renderBounds(msg.GetMaxSize())

	source, err := sourcePath(ctx, r.store, row)
	if err != nil {
		return err
	}

	// The info frame echoes the ref so a client that kept a request in flight
	// across a navigation can drop a stream belonging to another version
	// instead of painting it. size reports the bound the render was held to,
	// not the pixels that come back: the encoded size is not known until the
	// image is decoded, and buffering the head of the stream to find out would
	// trade the streaming this whole path exists for against a number the
	// contract already lets the client treat as an upper bound.
	// total_bytes stays 0, which the proto documents as "streaming without
	// knowing the length up front".
	if err := stream.Send(&hiramev1.RenderPageResponse{
		Payload: &hiramev1.RenderPageResponse_Info{
			Info: &hiramev1.RenderPageInfo{
				Ref:        currentRef(row),
				PageNumber: page,
				Format:     format.proto,
				Size:       bounds.message(),
			},
		},
	}); err != nil {
		return err
	}

	if err := r.limit.acquire(ctx); err != nil {
		return err
	}
	defer r.limit.release()

	err = r.renderer.RenderPage(ctx, gahakuclient.PageRequest{
		SourcePath: source,
		Page:       page,
		Format:     format.rendererFmt,
		MaxWidth:   bounds.width,
		MaxHeight:  bounds.height,
	}, func(body []byte) error {
		return stream.Send(&hiramev1.RenderPageResponse{
			Payload: &hiramev1.RenderPageResponse_Chunk{Chunk: body},
		})
	})
	if err != nil {
		return renderError(err)
	}
	return nil
}

// checkPage enforces the 1-based page contract as far as the database can.
//
// A page above a known count is OUT_OF_RANGE here rather than wherever Gahaku
// would land: the count is only known when extraction recorded one, and when it
// is not, the check is deliberately skipped so a real page is never rejected
// for lack of metadata.
func checkPage(page uint32, row sqlcgen.GetDocumentRow) error {
	if page == 0 {
		return connect.NewError(
			connect.CodeInvalidArgument,
			errors.New("page_number is 1-based; 0 is not a page"),
		)
	}
	count := pageCountOf(row.Metadata, mediaTypeOf(row.ContentType))
	if count > 0 && page > count {
		return connect.NewError(
			connect.CodeOutOfRange,
			fmt.Errorf("page %d is past the document's %d pages", page, count),
		)
	}
	return nil
}

// renderError maps a renderer failure onto the API's codes.
//
// Gahaku's gRPC status is carried through rather than flattened, because this
// is where the page checks that could not be made here are actually answered.
// checkPage skips the range test when no page count was extracted, deferring to
// the renderer that has the document open; if its OUT_OF_RANGE arrived as
// INTERNAL, that deferral would never complete and the GUI would show "an
// unexpected error" where the contract promises "no such page".
//
// A Connect error raised by the response stream itself passes through
// untouched: it means the client went away, and reporting it as a render
// failure would blame Gahaku for a disconnect.
func renderError(err error) error {
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		return connectErr
	}
	if errors.Is(err, gahakuclient.ErrNotConfigured) {
		return connect.NewError(connect.CodeUnavailable, err)
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	// FromError reports ok only for an error that really carries a status; it
	// unwraps through the fmt.Errorf chain the render client wraps with.
	if st, ok := status.FromError(err); ok {
		return connect.NewError(renderCode(st.Code()), fmt.Errorf("render: %w", err))
	}
	return connect.NewError(connect.CodeInternal, fmt.Errorf("render: %w", err))
}

// renderCode translates the statuses Gahaku documents into this API's.
//
// PERMISSION_DENIED is deliberately not passed through: Gahaku raises it for a
// source root its own configuration does not allow, which is a deployment fault
// the caller can neither see nor fix, and forwarding it would tell the user
// they lack permission to read their own document.
func renderCode(code codes.Code) connect.Code {
	switch code {
	case codes.OutOfRange:
		return connect.CodeOutOfRange
	case codes.InvalidArgument:
		return connect.CodeInvalidArgument
	case codes.ResourceExhausted:
		return connect.CodeResourceExhausted
	case codes.Unavailable:
		return connect.CodeUnavailable
	case codes.DeadlineExceeded:
		return connect.CodeDeadlineExceeded
	case codes.Canceled:
		return connect.CodeCanceled
	default:
		return connect.CodeInternal
	}
}
