package service_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hiramev1 "github.com/ngicks/hirame/apps/search-api/internal/gen/hirame/v1"
	"github.com/ngicks/hirame/apps/search-api/internal/gen/hirame/v1/hiramev1connect"
	"github.com/ngicks/hirame/apps/search-api/internal/service"
)

// The render stream is driven through a real Connect client rather than a
// hand-built ServerStream: the frame order and the oneof tags are the contract
// the web GUI reads, and only an actual stream proves them.
func newRenderClient(
	t *testing.T, store *fakeStore, renderer service.PageRenderer,
) hiramev1connect.RenderServiceClient {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(hiramev1connect.NewRenderServiceHandler(
		service.NewRender(store, renderer, service.NewRenderLimit(2)),
	))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return hiramev1connect.NewRenderServiceClient(srv.Client(), srv.URL)
}

func liveRef() *hiramev1.DocumentRef {
	return &hiramev1.DocumentRef{
		DocumentId:       "7",
		ContentVersionId: liveContentHash,
	}
}

// renderedStream is a collected RenderPage response.
type renderedStream struct {
	infos  []*hiramev1.RenderPageInfo
	chunks [][]byte
}

func (r renderedStream) body() []byte { return bytes.Join(r.chunks, nil) }

func collectRender(
	t *testing.T,
	client hiramev1connect.RenderServiceClient,
	req *hiramev1.RenderPageRequest,
) (renderedStream, error) {
	t.Helper()
	stream, err := client.RenderPage(t.Context(), connect.NewRequest(req))
	if err != nil {
		return renderedStream{}, err
	}
	defer func() { _ = stream.Close() }()

	var out renderedStream
	for stream.Receive() {
		msg := stream.Msg()
		switch {
		case msg.GetInfo() != nil:
			// Recorded in arrival order so the test can assert the info frame
			// came before any bytes.
			if len(out.chunks) > 0 {
				t.Error("an info frame arrived after image bytes had started")
			}
			out.infos = append(out.infos, msg.GetInfo())
		default:
			out.chunks = append(out.chunks, msg.GetChunk())
		}
	}
	return out, stream.Err()
}

func TestRenderPageOpensWithOneInfoFrameThenTheImage(t *testing.T) {
	renderer := newFakeRenderer([]byte("head"), []byte("tail"))
	client := newRenderClient(t, newFakeStore(), renderer)

	got, err := collectRender(t, client, &hiramev1.RenderPageRequest{
		Ref:        liveRef(),
		PageNumber: 2,
		Format:     hiramev1.ImageFormat_IMAGE_FORMAT_PNG,
		MaxSize:    &hiramev1.ImageSize{Width: 1600, Height: 1600},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if len(got.infos) != 1 {
		t.Fatalf("stream carried %d info frames, want exactly 1", len(got.infos))
	}
	info := got.infos[0]

	// The GUI drops a stream whose ref is not the one it asked for, so the
	// echo is what keeps a response from a superseded version off the screen.
	if info.GetRef().GetContentVersionId() != liveContentHash {
		t.Errorf("info echoed version %q, want %q",
			info.GetRef().GetContentVersionId(), liveContentHash)
	}
	if info.GetRef().GetDocumentId() != "7" {
		t.Errorf("info echoed document %q, want 7", info.GetRef().GetDocumentId())
	}
	if info.GetPageNumber() != 2 {
		t.Errorf("info page_number = %d, want 2", info.GetPageNumber())
	}
	if info.GetFormat() != hiramev1.ImageFormat_IMAGE_FORMAT_PNG {
		t.Errorf("info format = %s, want png", info.GetFormat())
	}
	if string(got.body()) != "headtail" {
		t.Errorf("image bytes = %q, want the chunks in order", got.body())
	}
	if len(got.chunks) != 2 {
		t.Errorf("got %d chunks, want the renderer's 2 forwarded as they arrived",
			len(got.chunks))
	}
}

func TestRenderPageResolvesTheServerDefaultFormat(t *testing.T) {
	renderer := newFakeRenderer([]byte("x"))
	client := newRenderClient(t, newFakeStore(), renderer)

	got, err := collectRender(t, client, &hiramev1.RenderPageRequest{
		Ref:        liveRef(),
		PageNumber: 1,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// UNSPECIFIED is never echoed back: the GUI picks a blob media type from
	// the format the response reports.
	if got.infos[0].GetFormat() != hiramev1.ImageFormat_IMAGE_FORMAT_PNG {
		t.Errorf("format = %s, want the png default", got.infos[0].GetFormat())
	}
}

func TestRenderPageRejectsARefThatIsNoLongerCurrent(t *testing.T) {
	client := newRenderClient(t, newFakeStore(), newFakeRenderer([]byte("x")))

	_, err := collectRender(t, client, &hiramev1.RenderPageRequest{
		Ref: &hiramev1.DocumentRef{
			DocumentId:       "7",
			ContentVersionId: "a-version-this-document-has-moved-on-from",
		},
		PageNumber: 1,
	})
	// Rejected rather than upgraded: an upgrade would return an image the
	// client never asked for and cannot detect as different.
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("code = %s, want failed_precondition", connect.CodeOf(err))
	}
}

func TestRenderPageRejectsATombstonedDocument(t *testing.T) {
	store := newFakeStore()
	store.documents[liveDocumentID] = tombstonedDocument()
	client := newRenderClient(t, store, newFakeRenderer([]byte("x")))

	_, err := collectRender(t, client, &hiramev1.RenderPageRequest{
		Ref:        liveRef(),
		PageNumber: 1,
	})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("code = %s, want failed_precondition", connect.CodeOf(err))
	}
}

func TestRenderPageReportsAnUnknownDocumentAsNotFound(t *testing.T) {
	client := newRenderClient(t, newFakeStore(), newFakeRenderer([]byte("x")))

	_, err := collectRender(t, client, &hiramev1.RenderPageRequest{
		Ref:        &hiramev1.DocumentRef{DocumentId: "999", ContentVersionId: "x"},
		PageNumber: 1,
	})
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %s, want not_found", connect.CodeOf(err))
	}
}

func TestRenderPageChecksThePageNumber(t *testing.T) {
	client := newRenderClient(t, newFakeStore(), newFakeRenderer([]byte("x")))

	for _, tc := range []struct {
		name string
		page uint32
		// wantErr false means the page is accepted; connect.CodeOf reports
		// CodeUnknown for a nil error, so success cannot be spelled as a code.
		wantErr bool
		want    connect.Code
	}{
		{
			name: "0 is not a page, the numbering is 1-based",
			page: 0, wantErr: true, want: connect.CodeInvalidArgument,
		},
		{
			// The fixture's metadata reports three pages.
			name: "past the document's pages",
			page: 4, wantErr: true, want: connect.CodeOutOfRange,
		},
		{
			name: "the last page is in range",
			page: 3, wantErr: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := collectRender(t, client, &hiramev1.RenderPageRequest{
				Ref:        liveRef(),
				PageNumber: tc.page,
			})
			if !tc.wantErr {
				if err != nil {
					t.Errorf("page %d was refused: %v", tc.page, err)
				}
				return
			}
			if connect.CodeOf(err) != tc.want {
				t.Errorf("code = %s, want %s", connect.CodeOf(err), tc.want)
			}
		})
	}
}

// A page count the metadata does not carry must not become a rejection: the
// check is skipped so a real page is never refused for want of metadata.
func TestRenderPageAllowsAnyPageWhenTheCountIsUnknown(t *testing.T) {
	store := newFakeStore()
	row := liveDocument()
	row.Metadata = []byte(`{}`)
	store.documents[liveDocumentID] = row
	client := newRenderClient(t, store, newFakeRenderer([]byte("x")))

	if _, err := collectRender(t, client, &hiramev1.RenderPageRequest{
		Ref:        liveRef(),
		PageNumber: 99,
	}); err != nil {
		t.Errorf("page 99 was refused though no count is known: %v", err)
	}
}

// The ref checked out, so a file that is gone is a precondition failure rather
// than a bad request: the client re-reads the document and finds the tombstone.
func TestRenderPageReportsAVanishedFileAsAPreconditionFailure(t *testing.T) {
	store := newFakeStore()
	store.sourceMissing = true
	client := newRenderClient(t, store, newFakeRenderer([]byte("x")))

	_, err := collectRender(t, client, &hiramev1.RenderPageRequest{
		Ref:        liveRef(),
		PageNumber: 1,
	})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("code = %s, want failed_precondition", connect.CodeOf(err))
	}
}

func TestRenderPagePassesTheSourceAndShapeToTheRenderer(t *testing.T) {
	renderer := newFakeRenderer([]byte("x"))
	client := newRenderClient(t, newFakeStore(), renderer)

	if _, err := collectRender(t, client, &hiramev1.RenderPageRequest{
		Ref:        liveRef(),
		PageNumber: 2,
		Format:     hiramev1.ImageFormat_IMAGE_FORMAT_JPEG,
		MaxSize:    &hiramev1.ImageSize{Width: 800, Height: 600},
	}); err != nil {
		t.Fatalf("render: %v", err)
	}

	call := renderer.lastCall(t)
	if call.SourcePath != liveAbsolutePath {
		t.Errorf("rendered %q, want the observed path %q", call.SourcePath, liveAbsolutePath)
	}
	if call.Page != 2 {
		t.Errorf("rendered page %d, want 2", call.Page)
	}
	if call.MaxWidth != 800 || call.MaxHeight != 600 {
		t.Errorf("bounds = %dx%d, want 800x600", call.MaxWidth, call.MaxHeight)
	}
}

// Gahaku reads a zero bound as "no limit", so an omitted max_size must not be
// passed through: a request carrying no size at all would otherwise be the one
// way to ask for an unbounded raster.
func TestRenderPageBoundsARequestThatNamesNoSize(t *testing.T) {
	for _, tc := range []struct {
		name          string
		size          *hiramev1.ImageSize
		width, height uint32
	}{
		{"absent", nil, 2048, 2048},
		{"zero", &hiramev1.ImageSize{}, 2048, 2048},
		{"one axis only", &hiramev1.ImageSize{Width: 640}, 640, 2048},
		{"above the cap", &hiramev1.ImageSize{Width: 99999, Height: 99999}, 5000, 5000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			renderer := newFakeRenderer([]byte("x"))
			client := newRenderClient(t, newFakeStore(), renderer)

			resp, err := collectRender(t, client, &hiramev1.RenderPageRequest{
				Ref:        liveRef(),
				PageNumber: 1,
				MaxSize:    tc.size,
			})
			if err != nil {
				t.Fatalf("render: %v", err)
			}

			call := renderer.lastCall(t)
			if call.MaxWidth != tc.width || call.MaxHeight != tc.height {
				t.Errorf("bounds = %dx%d, want %dx%d",
					call.MaxWidth, call.MaxHeight, tc.width, tc.height)
			}
			// The info frame reports the bound the render was held to, so a
			// client is told what it actually got.
			if len(resp.infos) != 1 {
				t.Fatalf("got %d info frames, want 1", len(resp.infos))
			}
			if got := resp.infos[0].GetSize(); got.GetWidth() != tc.width ||
				got.GetHeight() != tc.height {
				t.Errorf("reported size = %dx%d, want %dx%d",
					got.GetWidth(), got.GetHeight(), tc.width, tc.height)
			}
		})
	}
}

func TestRenderPageSurfacesARendererFailure(t *testing.T) {
	renderer := newFakeRenderer()
	renderer.err = errRenderFailed
	client := newRenderClient(t, newFakeStore(), renderer)

	_, err := collectRender(t, client, &hiramev1.RenderPageRequest{
		Ref:        liveRef(),
		PageNumber: 1,
	})
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("code = %s, want internal", connect.CodeOf(err))
	}
}

// checkPage deliberately lets a page through when no count was extracted,
// deferring the range decision to Gahaku. That deferral only completes if
// Gahaku's status survives the trip: flattening it to INTERNAL would leave the
// GUI showing "an unexpected error" where the contract promises "no such page".
// The status is wrapped the way the render client wraps it, since that is what
// the mapping has to see through.
func TestRenderPageTranslatesGahakuStatuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		from codes.Code
		want connect.Code
	}{
		{"a page past what the document holds", codes.OutOfRange, connect.CodeOutOfRange},
		{"bytes of no renderable kind", codes.InvalidArgument, connect.CodeInvalidArgument},
		{"input over the stream limit", codes.ResourceExhausted, connect.CodeResourceExhausted},
		{"the renderer is down", codes.Unavailable, connect.CodeUnavailable},
		// A source root Gahaku's own configuration refuses is a deployment
		// fault; telling the user they lack permission would be a lie.
		{"a source root gahaku will not read", codes.PermissionDenied, connect.CodeInternal},
		{"anything unrecognised", codes.Unknown, connect.CodeInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			renderer := newFakeRenderer()
			renderer.err = fmt.Errorf("gahakuclient: render: %w",
				status.Error(tc.from, "from gahaku"))
			client := newRenderClient(t, newFakeStore(), renderer)

			_, err := collectRender(t, client, &hiramev1.RenderPageRequest{
				Ref:        liveRef(),
				PageNumber: 1,
			})
			if connect.CodeOf(err) != tc.want {
				t.Errorf("gahaku %s became %s, want %s",
					tc.from, connect.CodeOf(err), tc.want)
			}
		})
	}
}
