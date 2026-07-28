// Package server mounts the generated ConnectRPC handlers and the operational
// endpoints on one HTTP server.
package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sync/errgroup"

	"github.com/ngicks/hirame/apps/search-api/internal/gen/hirame/v1/hiramev1connect"
)

// APIPrefix is where every generated service is mounted.
//
// It has to agree with the web GUI's Connect transport baseUrl and with the
// reverse proxy in front of both (D-010): Connect derives a request path of
// <baseUrl>/<package>.<Service>/<Method>, so a mismatch here is a 404 that no
// type checker catches.
const APIPrefix = "/api"

// HealthPath is unauthenticated, cheap, and deliberately outside APIPrefix so
// a readiness probe does not depend on the RPC stack.
const HealthPath = "/healthz"

// Config is the HTTP-level configuration; it is deliberately not the process
// config so the server stays constructible from a test.
type Config struct {
	ListenAddr        string
	ShutdownTimeout   time.Duration
	ReadHeaderTimeout time.Duration
}

// Handlers is the set of service implementations to mount.
type Handlers struct {
	Search    hiramev1connect.SearchServiceHandler
	Document  hiramev1connect.DocumentServiceHandler
	Render    hiramev1connect.RenderServiceHandler
	Thumbnail hiramev1connect.ThumbnailServiceHandler
}

// NewHandler builds the root handler: services under APIPrefix, health beside
// it.
func NewHandler(h Handlers, opts ...connect.HandlerOption) http.Handler {
	api := http.NewServeMux()
	api.Handle(hiramev1connect.NewSearchServiceHandler(h.Search, opts...))
	api.Handle(hiramev1connect.NewDocumentServiceHandler(h.Document, opts...))
	api.Handle(hiramev1connect.NewRenderServiceHandler(h.Render, opts...))
	api.Handle(hiramev1connect.NewThumbnailServiceHandler(h.Thumbnail, opts...))

	root := http.NewServeMux()
	root.Handle(APIPrefix+"/", http.StripPrefix(APIPrefix, api))
	root.HandleFunc("GET "+HealthPath, handleHealth)
	return root
}

// Run serves until ctx is done, then drains within Config.ShutdownTimeout.
func Run(ctx context.Context, cfg Config, handler http.Handler) error {
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("listen %q: %w", cfg.ListenAddr, err)
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		// Detached from gctx: it is already done here, and Shutdown must be
		// given the drain window rather than returning immediately.
		shutdownCtx, cancel := context.WithTimeout(
			context.WithoutCancel(gctx),
			cfg.ShutdownTimeout,
		)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	})
	return g.Wait()
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
