package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/ngicks/hirame/apps/search-api/internal/server"
	"github.com/ngicks/hirame/apps/search-api/internal/service"
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
)

const searchProcedure = "/hirame.v1.SearchService/Search"

// stubSearchStore answers the one procedure these tests drive. Routing is what
// is under test, so the store only has to be reachable, not realistic.
type stubSearchStore struct{}

func (stubSearchStore) SearchDocuments(
	context.Context, sqlcgen.SearchDocumentsParams,
) ([]sqlcgen.SearchDocumentsRow, error) {
	return nil, nil
}

func (stubSearchStore) CountSearchDocuments(context.Context, string) (int64, error) {
	return 0, nil
}

// The handlers other than Search are constructed with nil collaborators: these
// tests never reach them, and giving them fakes would say this file tests more
// than routing.
func testHandlers(logger *slog.Logger) server.Handlers {
	return server.Handlers{
		Search:    service.NewSearch(stubSearchStore{}),
		Document:  service.NewDocument(nil),
		Render:    service.NewRender(nil, nil, service.NewRenderLimit(1)),
		Thumbnail: service.NewThumbnail(nil, nil, nil, service.NewRenderLimit(1), logger),
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(server.NewHandler(testHandlers(discardLogger())))
	t.Cleanup(srv.Close)
	return srv
}

func TestHealthzIsServedOutsideTheAPIPrefix(t *testing.T) {
	srv := newTestServer(t)

	resp, err := srv.Client().Get(srv.URL + server.HealthPath)
	if err != nil {
		t.Fatalf("get %s: %v", server.HealthPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// The prefix is a three-way agreement between this mux, the Vite dev proxy,
// and the web client's transport baseUrl; nothing but a request proves it.
func TestServicesAreReachableOnlyUnderTheAPIPrefix(t *testing.T) {
	srv := newTestServer(t)

	for _, tc := range []struct {
		name string
		path string
		want int
	}{
		{
			name: "prefixed path reaches the handler",
			path: server.APIPrefix + searchProcedure,
			want: http.StatusOK,
		},
		{
			name: "unprefixed path is not routed",
			path: searchProcedure,
			want: http.StatusNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := srv.Client().Post(
				srv.URL+tc.path,
				"application/json",
				strings.NewReader(`{"query":"検索"}`),
			)
			if err != nil {
				t.Fatalf("post %s: %v", tc.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// The interceptor sits in front of every procedure, so a mistake in it is a
// mistake in all of them.
func TestLoggingInterceptorRecordsTheProcedureWithoutDisturbingIt(t *testing.T) {
	var logged strings.Builder
	logger := slog.New(slog.NewTextHandler(&logged, nil))

	srv := httptest.NewServer(server.NewHandler(
		testHandlers(discardLogger()),
		connect.WithInterceptors(server.NewLoggingInterceptor(logger)),
	))
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Post(
		srv.URL+server.APIPrefix+searchProcedure,
		"application/json",
		strings.NewReader(`{"query":"検索"}`),
	)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if !strings.Contains(logged.String(), searchProcedure) {
		t.Errorf("interceptor logged %q, want the procedure named", logged.String())
	}
}
