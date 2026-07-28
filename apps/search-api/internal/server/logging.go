package server

import (
	"context"
	"log/slog"
	"time"

	"connectrpc.com/connect"
)

// NewLoggingInterceptor logs one line per completed RPC.
//
// Both halves are implemented because the render path is server-streaming and
// a unary-only interceptor would leave the slowest calls in the system
// invisible. A streaming call is logged when the stream closes, so its duration
// covers the whole render rather than the handshake.
//
// Request payloads are never logged: a search query is user content, and the
// procedure plus the outcome is what an operator needs.
func NewLoggingInterceptor(logger *slog.Logger) connect.Interceptor {
	return &loggingInterceptor{logger: logger}
}

type loggingInterceptor struct {
	logger *slog.Logger
}

func (i *loggingInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		started := time.Now()
		resp, err := next(ctx, req)
		i.log(ctx, req.Spec().Procedure, started, err)
		return resp, err
	}
}

func (i *loggingInterceptor) WrapStreamingHandler(
	next connect.StreamingHandlerFunc,
) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		started := time.Now()
		err := next(ctx, conn)
		i.log(ctx, conn.Spec().Procedure, started, err)
		return err
	}
}

// WrapStreamingClient is required by the interface and does nothing: this
// process serves these procedures and never calls them.
func (i *loggingInterceptor) WrapStreamingClient(
	next connect.StreamingClientFunc,
) connect.StreamingClientFunc {
	return next
}

func (i *loggingInterceptor) log(
	ctx context.Context,
	procedure string,
	started time.Time,
	err error,
) {
	attrs := []any{
		slog.String("procedure", procedure),
		slog.Duration("duration", time.Since(started)),
		slog.String("code", connect.CodeOf(err).String()),
	}
	if err == nil {
		i.logger.InfoContext(ctx, "rpc served", attrs...)
		return
	}
	// A failure carries its cause; the code alone does not say which
	// precondition or which document was missing.
	i.logger.WarnContext(ctx, "rpc failed", append(attrs, slog.Any("error", err))...)
}
