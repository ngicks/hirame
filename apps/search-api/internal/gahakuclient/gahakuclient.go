// Package gahakuclient renders document pages through the Gahaku gRPC service.
//
// Gahaku is a sub-repository with committed generated bindings, so this package
// consumes `github.com/ngicks/gahaku/api/gen/proto/go/...` directly rather than
// re-generating its schema here: a second copy of those types would be a second
// thing to keep in step with the pinned submodule revision, and `api/proto/` in
// this repository owns the hirame contract only.
//
// The vocabulary below is deliberately this package's own rather than
// hirame.v1's. Gahaku emits formats no browser displays and takes page ranges
// where hirame takes one page; keeping the translation here means the API
// handlers never hold a Gahaku enum, and this package never has to know what
// hirame's clients are allowed to ask for.
package gahakuclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	gahakuv1 "github.com/ngicks/gahaku/api/gen/proto/go/ngicks/gahaku/v1"
)

// ErrNotConfigured reports that no Gahaku target was configured. As in the tika
// and objstore packages, an absent endpoint reaches the call rather than the
// constructor, so a partially provisioned pod fails on the request that needs
// the missing dependency instead of refusing to start.
var ErrNotConfigured = errors.New("gahakuclient: no target configured")

// uploadChunkSize bounds one RenderRequest carrying input bytes. gRPC's default
// receive limit is 4 MiB and the frame has to fit inside it with room for the
// message overhead.
const uploadChunkSize = 1 << 20

// Format is the encoding of a rendered page, restricted to what this system
// serves a browser.
type Format int

// The formats hirame renders. Zero is invalid rather than a default so a
// caller that forgot to resolve one is caught here instead of silently
// receiving Gahaku's default.
const (
	FormatPNG Format = iota + 1
	FormatJPEG
	FormatWEBP
	FormatAVIF
)

func (f Format) proto() (gahakuv1.ImageFormat, error) {
	switch f {
	case FormatPNG:
		return gahakuv1.ImageFormat_IMAGE_FORMAT_PNG, nil
	case FormatJPEG:
		return gahakuv1.ImageFormat_IMAGE_FORMAT_JPEG, nil
	case FormatWEBP:
		return gahakuv1.ImageFormat_IMAGE_FORMAT_WEBP, nil
	case FormatAVIF:
		return gahakuv1.ImageFormat_IMAGE_FORMAT_AVIF, nil
	default:
		return 0, fmt.Errorf("gahakuclient: unresolved image format %d", int(f))
	}
}

// PageRequest is one page of one document, rendered to one shape.
type PageRequest struct {
	// SourcePath is a file on this process's filesystem. The watched
	// mountpoints are bind-mounted read-only here (D-009), so the bytes are
	// read locally and streamed to Gahaku rather than handed over as a path:
	// Gahaku's local_path source would need the same mount and an allowlisted
	// root on its side, which couples two containers' filesystems for nothing.
	SourcePath string
	// Page is 1-based, matching the hirame API and Gahaku's page range.
	Page   uint32
	Format Format
	// MaxWidth and MaxHeight bound the output, preserving aspect ratio. 0
	// leaves that axis unbounded. Pages are never scaled up.
	MaxWidth  uint32
	MaxHeight uint32
}

// Config is the client's configuration, separate from the process config so a
// test can point it at a stub server.
type Config struct {
	// Target is a gRPC dial target.
	Target string
	// Timeout bounds one render stream end to end.
	Timeout time.Duration
}

// Client renders through one Gahaku server.
type Client struct {
	conn    *grpc.ClientConn
	client  gahakuv1.GahakuServiceClient
	timeout time.Duration
}

// New dials Gahaku. An empty Config.Target is accepted and surfaces as
// ErrNotConfigured on the first call.
//
// The connection is insecure because Gahaku sits on the internal Podman network
// with no published ports (D-010); TLS between two containers on that network
// would add certificate rotation without adding a boundary.
func New(cfg Config) (*Client, error) {
	if cfg.Target == "" {
		return &Client{timeout: cfg.Timeout}, nil
	}
	conn, err := grpc.NewClient(
		cfg.Target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("gahakuclient: dial %q: %w", cfg.Target, err)
	}
	return &Client{
		conn:    conn,
		client:  gahakuv1.NewGahakuServiceClient(conn),
		timeout: cfg.Timeout,
	}, nil
}

// Close releases the connection.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// RenderPage renders one page and hands each slice of the encoded image to
// chunk, in order.
//
// chunk is called on the reading goroutine, so a caller that forwards to a
// response stream applies backpressure to Gahaku for free. An error from chunk
// ends the render.
func (c *Client) RenderPage(
	ctx context.Context,
	req PageRequest,
	chunk func([]byte) error,
) error {
	if c.client == nil {
		return ErrNotConfigured
	}
	format, err := req.Format.proto()
	if err != nil {
		return err
	}

	source, err := os.Open(req.SourcePath)
	if err != nil {
		return fmt.Errorf("gahakuclient: open %q: %w", req.SourcePath, err)
	}
	defer func() { _ = source.Close() }()

	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	stream, err := c.client.Render(ctx)
	if err != nil {
		return fmt.Errorf("gahakuclient: open render stream: %w", err)
	}

	header := &gahakuv1.RenderRequest{
		Payload: &gahakuv1.RenderRequest_Header{
			Header: &gahakuv1.RenderRequestHeader{
				Source: &gahakuv1.Source{
					Kind: &gahakuv1.Source_ByteStream{
						ByteStream: &gahakuv1.ByteStreamSource{},
					},
				},
				Output: &gahakuv1.OutputSpec{
					Kind: &gahakuv1.OutputSpec_ByteStream{
						ByteStream: &gahakuv1.ByteStreamOutput{},
					},
				},
				Options: &gahakuv1.RenderOptions{
					Format: format,
					Resize: &gahakuv1.ResizeSpec{
						MaxWidth:  req.MaxWidth,
						MaxHeight: req.MaxHeight,
					},
					Pages: &gahakuv1.PageRange{
						FirstPage: req.Page,
						LastPage:  req.Page,
					},
				},
			},
		},
	}
	if err := stream.Send(header); err != nil {
		// A Send failure means the server already ended the stream, and its
		// status carries why. Recv surfaces that instead of the useless
		// io.EOF Send reports.
		return recvStatus(stream, err)
	}

	if err := sendFile(stream, source); err != nil {
		return err
	}
	if err := stream.CloseSend(); err != nil {
		return fmt.Errorf("gahakuclient: close send: %w", err)
	}

	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("gahakuclient: render: %w", err)
		}
		// Only one page was requested, so page_index is not consulted: any
		// PageChunk in this stream belongs to it.
		page := resp.GetPageChunk()
		if page == nil {
			continue
		}
		if body := page.GetChunk(); len(body) > 0 {
			if err := chunk(body); err != nil {
				return err
			}
		}
	}
}

func sendFile(
	stream grpc.BidiStreamingClient[gahakuv1.RenderRequest, gahakuv1.RenderResponse],
	source io.Reader,
) error {
	buf := make([]byte, uploadChunkSize)
	for {
		n, err := source.Read(buf)
		if n > 0 {
			msg := &gahakuv1.RenderRequest{
				Payload: &gahakuv1.RenderRequest_Chunk{Chunk: buf[:n]},
			}
			if sendErr := stream.Send(msg); sendErr != nil {
				return recvStatus(stream, sendErr)
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("gahakuclient: read source: %w", err)
		}
	}
}

// recvStatus turns a failed Send into the server's actual status.
//
// gRPC reports every Send on a broken stream as io.EOF and keeps the reason in
// the stream's status, which only Recv returns. Without this the caller would
// see "EOF" for a rejected document, an oversized input, or a page out of
// range — all of which the API has to map to distinct codes.
func recvStatus(
	stream grpc.BidiStreamingClient[gahakuv1.RenderRequest, gahakuv1.RenderResponse],
	sendErr error,
) error {
	if _, err := stream.Recv(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("gahakuclient: render: %w", err)
	}
	return fmt.Errorf("gahakuclient: send: %w", sendErr)
}
