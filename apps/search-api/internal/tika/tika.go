// Package tika is a client for the Apache Tika server's HTTP interface.
//
// Tika exposes plain HTTP rather than a typed contract, so this package is
// deliberately thin: it owns the two endpoints ingestion needs, the size cap,
// and the timeout, and leaves interpretation of the result to the caller.
package tika

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrTooLarge reports that the document exceeded [Config.MaxBytes]. It is a
// permanent condition: the same bytes will exceed the cap on every retry, so
// callers should record the failure rather than schedule another attempt.
var ErrTooLarge = errors.New("document exceeds the configured size limit")

// ErrNotConfigured reports that no Tika endpoint was configured. Both binaries
// load one whole config and leave the sections they do not use empty, so an
// absent endpoint reaches the call rather than the constructor.
var ErrNotConfigured = errors.New("tika: no base URL configured")

// Config is the client's configuration, kept separate from the process config
// so a test can construct a client against an httptest server.
type Config struct {
	BaseURL string
	Timeout time.Duration
	// MaxBytes caps the document size sent for extraction. 0 disables the cap.
	MaxBytes int64
}

// Client talks to one Tika server.
type Client struct {
	baseURL  string
	maxBytes int64
	http     *http.Client
}

// New builds a Client. An empty Config.BaseURL is accepted and surfaces as
// ErrNotConfigured on the first call.
func New(cfg Config) (*Client, error) {
	base := strings.TrimSuffix(cfg.BaseURL, "/")
	if base != "" {
		if _, err := url.Parse(base); err != nil {
			return nil, fmt.Errorf("tika: parse base URL %q: %w", base, err)
		}
	}
	return &Client{
		baseURL:  base,
		maxBytes: cfg.MaxBytes,
		http:     &http.Client{Timeout: cfg.Timeout},
	}, nil
}

// MaxBytes returns the configured size cap, 0 when disabled. Callers that
// already know a document's size use it to fail without uploading anything.
func (c *Client) MaxBytes() int64 { return c.maxBytes }

// Metadata is Tika's /meta answer: the whole document as stored, plus the one
// field the schema keeps in a column of its own.
type Metadata struct {
	// Raw is the response body verbatim, destined for
	// extracted_contents.metadata. It is kept unparsed because Tika's key set
	// is open-ended and jsonb can store it as-is.
	Raw json.RawMessage
	// ContentType is Tika's detected type, empty when it reported none.
	ContentType string
}

// Text extracts a document's plain text.
//
// contentType may be empty, in which case Tika detects the type itself; when
// known it is passed through, because detection from a stream is weaker than
// the extension and sniff the caller already performed.
func (c *Client) Text(ctx context.Context, body io.Reader, contentType string) (string, error) {
	resp, err := c.put(ctx, "/tika", "text/plain", body, contentType)
	if err != nil {
		return "", err
	}
	return string(resp), nil
}

// Meta extracts a document's metadata.
func (c *Client) Meta(
	ctx context.Context,
	body io.Reader,
	contentType string,
) (Metadata, error) {
	resp, err := c.put(ctx, "/meta", "application/json", body, contentType)
	if err != nil {
		return Metadata{}, err
	}
	if !json.Valid(resp) {
		return Metadata{}, fmt.Errorf("tika: /meta returned invalid JSON (%d bytes)", len(resp))
	}
	return Metadata{Raw: resp, ContentType: contentTypeOf(resp)}, nil
}

func (c *Client) put(
	ctx context.Context,
	path string,
	accept string,
	body io.Reader,
	contentType string,
) ([]byte, error) {
	if c.baseURL == "" {
		return nil, ErrNotConfigured
	}
	if c.maxBytes > 0 {
		body = &cappedReader{r: body, limit: c.maxBytes}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("tika: build %s request: %w", path, err)
	}
	req.Header.Set("Accept", accept)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// The cap is enforced while the body is being streamed, so it surfaces
		// wrapped in a transport error rather than as a status code.
		if errors.Is(err, ErrTooLarge) {
			return nil, ErrTooLarge
		}
		return nil, fmt.Errorf("tika: %s: %w", path, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Path: path, StatusCode: resp.StatusCode}
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("tika: read %s response: %w", path, err)
	}
	return out, nil
}

// StatusError is a non-200 answer from Tika.
type StatusError struct {
	Path       string
	StatusCode int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("tika: %s returned %d %s",
		e.Path, e.StatusCode, http.StatusText(e.StatusCode))
}

// Permanent reports whether retrying could plausibly succeed. Tika answers an
// unsupported or corrupt document with 415 or 422, which no number of retries
// will change; 5xx and 429 are worth another attempt.
func (e *StatusError) Permanent() bool {
	return e.StatusCode == http.StatusUnsupportedMediaType ||
		e.StatusCode == http.StatusUnprocessableEntity
}

// cappedReader fails the request body once more than limit bytes are read.
// Truncating instead would silently index a fraction of a document and report
// success.
type cappedReader struct {
	r     io.Reader
	limit int64
	read  int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.read > c.limit {
		return 0, ErrTooLarge
	}
	// One byte past the limit is enough to prove the document is oversize, and
	// reading no further keeps a document of exactly limit bytes from tripping
	// the guard on its own end-of-file read.
	if room := c.limit + 1 - c.read; int64(len(p)) > room {
		p = p[:room]
	}
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.read > c.limit {
		return 0, ErrTooLarge
	}
	return n, err
}

// contentTypeOf pulls Content-Type out of a Tika metadata document. Tika
// reports repeated keys as an array, so a plain string decode is not enough.
func contentTypeOf(raw json.RawMessage) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ""
	}
	value, ok := fields["Content-Type"]
	if !ok {
		return ""
	}
	var single string
	if err := json.Unmarshal(value, &single); err == nil {
		return single
	}
	var many []string
	if err := json.Unmarshal(value, &many); err == nil && len(many) > 0 {
		return many[0]
	}
	return ""
}
