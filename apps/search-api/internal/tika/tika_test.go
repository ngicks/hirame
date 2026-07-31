package tika_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ngicks/hirame/apps/search-api/internal/tika"
)

func newClient(t *testing.T, handler http.HandlerFunc, maxBytes int64) *tika.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client, err := tika.New(tika.Config{
		BaseURL:  srv.URL,
		Timeout:  5 * time.Second,
		MaxBytes: maxBytes,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

func TestTextPutsTheBodyAndAsksForPlainText(t *testing.T) {
	var (
		gotMethod, gotPath, gotAccept, gotContentType string
		gotBody                                       []byte
	)
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAccept = r.Header.Get("Accept")
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte("extracted text"))
	}, 0)

	text, err := client.Text(t.Context(), strings.NewReader("document"), "application/pdf")
	if err != nil {
		t.Fatalf("Text: %v", err)
	}

	for _, tc := range []struct{ name, got, want string }{
		{"method", gotMethod, http.MethodPut},
		{"path", gotPath, "/tika"},
		{"accept", gotAccept, "text/plain"},
		{"content type", gotContentType, "application/pdf"},
		{"body", string(gotBody), "document"},
		{"text", text, "extracted text"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestMetaReturnsTheRawDocumentAndItsContentType(t *testing.T) {
	body := `{"Content-Type":"application/pdf","dc:title":"Report"}`
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/meta" {
			t.Errorf("path = %q, want /meta", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("accept = %q, want application/json", got)
		}
		_, _ = w.Write([]byte(body))
	}, 0)

	meta, err := client.Meta(t.Context(), strings.NewReader("document"), "")
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if string(meta.Raw) != body {
		t.Errorf("Raw = %q, want the response verbatim", meta.Raw)
	}
	if meta.ContentType != "application/pdf" {
		t.Errorf("ContentType = %q", meta.ContentType)
	}
}

// Tika reports a key seen more than once as an array, which a plain string
// decode would silently drop.
func TestMetaReadsAnArrayValuedContentType(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Content-Type":["application/pdf","application/x-pdf"]}`))
	}, 0)

	meta, err := client.Meta(t.Context(), strings.NewReader("d"), "")
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if meta.ContentType != "application/pdf" {
		t.Errorf("ContentType = %q, want the first entry", meta.ContentType)
	}
}

func TestMetaRejectsAResponseThatIsNotJSON(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>gateway error</html>"))
	}, 0)

	if _, err := client.Meta(t.Context(), strings.NewReader("d"), ""); err == nil {
		t.Fatal("Meta accepted a non-JSON body")
	}
}

// Truncating would index a fraction of a document and report success, so the
// cap has to fail instead.
func TestTextFailsRatherThanTruncatingAnOversizeDocument(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("should not matter"))
	}, 8)

	_, err := client.Text(t.Context(), strings.NewReader(strings.Repeat("x", 64)), "")
	if !errors.Is(err, tika.ErrTooLarge) {
		t.Fatalf("Text error = %v, want ErrTooLarge", err)
	}
}

func TestADocumentExactlyAtTheCapIsAccepted(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("ok"))
	}, 8)

	if _, err := client.Text(t.Context(), strings.NewReader("12345678"), ""); err != nil {
		t.Fatalf("Text: %v", err)
	}
}

func newCappedResponseClient(
	t *testing.T,
	handler http.HandlerFunc,
	maxResponseBytes int64,
) *tika.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client, err := tika.New(tika.Config{
		BaseURL:          srv.URL,
		Timeout:          5 * time.Second,
		MaxResponseBytes: maxResponseBytes,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

// An answer this process cannot hold is a failure, not something to buffer:
// nothing in the protocol bounds what a server may send back.
func TestAResponsePastTheCapFailsRatherThanBeingBuffered(t *testing.T) {
	client := newCappedResponseClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(strings.Repeat("x", 64)))
	}, 8)

	_, err := client.Text(t.Context(), strings.NewReader("document"), "")
	if !errors.Is(err, tika.ErrTooLarge) {
		t.Fatalf("Text error = %v, want ErrTooLarge", err)
	}
}

// ErrTooLarge is what the extraction worker reads as permanent, so an oversize
// response is recorded once rather than retried until the attempts run out.
func TestAnOversizeResponseIsPermanent(t *testing.T) {
	client := newCappedResponseClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(strings.Repeat("x", 64)))
	}, 8)

	_, err := client.Meta(t.Context(), strings.NewReader("document"), "")
	if !errors.Is(err, tika.ErrTooLarge) {
		t.Fatalf("Meta error = %v, want ErrTooLarge", err)
	}
}

func TestAResponseExactlyAtTheCapIsAccepted(t *testing.T) {
	client := newCappedResponseClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("12345678"))
	}, 8)

	text, err := client.Text(t.Context(), strings.NewReader("document"), "")
	if err != nil {
		t.Fatalf("Text: %v", err)
	}
	if text != "12345678" {
		t.Errorf("text = %q, want the whole response", text)
	}
}

func TestStatusErrorsSeparatePermanentFromRetryable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		permanent bool
	}{
		{"unsupported media type", http.StatusUnsupportedMediaType, true},
		{"unprocessable document", http.StatusUnprocessableEntity, true},
		{"server error", http.StatusInternalServerError, false},
		{"too many requests", http.StatusTooManyRequests, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}, 0)

			_, err := client.Text(t.Context(), strings.NewReader("d"), "")
			var statusErr *tika.StatusError
			if !errors.As(err, &statusErr) {
				t.Fatalf("error = %v, want a StatusError", err)
			}
			if statusErr.Permanent() != tc.permanent {
				t.Errorf("Permanent() = %v, want %v", statusErr.Permanent(), tc.permanent)
			}
		})
	}
}

func TestAnUnconfiguredEndpointFailsOnUseRatherThanAtConstruction(t *testing.T) {
	client, err := tika.New(tika.Config{})
	if err != nil {
		t.Fatalf("New with no base URL: %v", err)
	}
	if _, err := client.Text(t.Context(), strings.NewReader("d"), ""); !errors.Is(
		err, tika.ErrNotConfigured,
	) {
		t.Errorf("Text error = %v, want ErrNotConfigured", err)
	}
}
