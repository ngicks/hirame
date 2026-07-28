package service_test

import (
	"testing"

	"connectrpc.com/connect"

	hiramev1 "github.com/ngicks/hirame/apps/search-api/internal/gen/hirame/v1"
	"github.com/ngicks/hirame/apps/search-api/internal/service"
)

func getDocument(
	t *testing.T, store *fakeStore, documentID string,
) (*hiramev1.Document, error) {
	t.Helper()
	resp, err := service.NewDocument(store).GetDocument(
		t.Context(),
		connect.NewRequest(&hiramev1.GetDocumentRequest{DocumentId: documentID}),
	)
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetDocument(), nil
}

func TestGetDocumentReportsTheCurrentVersion(t *testing.T) {
	doc, err := getDocument(t, newFakeStore(), "7")
	if err != nil {
		t.Fatalf("get document: %v", err)
	}

	if doc.GetDeleted() {
		t.Error("a live document reported itself deleted")
	}
	if got := doc.GetRelativePath(); got != "報告/2026.pdf" {
		t.Errorf("relative_path = %q, want it relative to the mountpoint root", got)
	}
	if got := doc.GetFileName(); got != "2026.pdf" {
		t.Errorf("file_name = %q, want 2026.pdf", got)
	}

	version := doc.GetCurrentVersion()
	if version == nil {
		t.Fatal("a live document has no current version")
	}
	if got := version.GetContentVersionId(); got != liveContentHash {
		t.Errorf("content_version_id = %q, want the content hash %q", got, liveContentHash)
	}
	if got := version.GetMediaType(); got != "application/pdf" {
		t.Errorf("media_type = %q, want application/pdf", got)
	}
	// Tika reported three pages as a one-element array of strings, which is
	// the shape that breaks a naive map[string]string decode.
	if got := version.GetPageCount(); got != 3 {
		t.Errorf("page_count = %d, want the 3 Tika reported", got)
	}
	if got := version.GetSizeBytes(); got != 1024 {
		t.Errorf("size_bytes = %d, want 1024", got)
	}
	got := version.GetExtractionState()
	if got != hiramev1.ExtractionState_EXTRACTION_STATE_SUCCEEDED {
		t.Errorf("extraction_state = %s, want succeeded", got)
	}
	if !version.GetHasExtractedText() {
		t.Error("has_extracted_text is false though the extraction stored text")
	}
	if doc.GetDiscoveredTime() == nil || doc.GetModifiedTime() == nil {
		t.Error("the document carries no timestamps")
	}
	if version.GetFirstSeenTime() == nil {
		t.Error("the content version carries no first-seen time")
	}
}

// A tombstone answers rather than failing: the row survives deletion so
// invalidation lineage stays consistent (D-007), and the GUI's stale-ref
// recovery reads this to learn it should stop retrying.
func TestGetDocumentAnswersForATombstone(t *testing.T) {
	store := newFakeStore()
	store.documents[liveDocumentID] = tombstonedDocument()

	doc, err := getDocument(t, store, "7")
	if err != nil {
		t.Fatalf("a tombstone must answer, not fail: %v", err)
	}
	if !doc.GetDeleted() {
		t.Error("deleted is false for a tombstoned document")
	}
	if doc.GetCurrentVersion() != nil {
		t.Error("a tombstone reported a current version; there is nothing to render")
	}
}

func TestGetDocumentReportsAnUnknownIdAsNotFound(t *testing.T) {
	for _, tc := range []struct{ name, documentID string }{
		{"an id no document has", "999"},
		// Opaque to the client, so it cannot tell a malformed id from a
		// deleted one; NOT_FOUND is the answer it can act on.
		{"an id that is not a number at all", "not-an-id"},
		{"an empty id", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := getDocument(t, newFakeStore(), tc.documentID)
			if connect.CodeOf(err) != connect.CodeNotFound {
				t.Errorf("code = %s, want not_found", connect.CodeOf(err))
			}
		})
	}
}

func TestGetDocumentReportsAFailedExtraction(t *testing.T) {
	store := newFakeStore()
	row := liveDocument()
	row.ExtractionStatus = new("failed")
	row.ExtractionError = new("tika refused the document")
	row.HasExtractedText = false
	store.documents[liveDocumentID] = row

	doc, err := getDocument(t, store, "7")
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	version := doc.GetCurrentVersion()
	if got := version.GetExtractionState(); got != hiramev1.ExtractionState_EXTRACTION_STATE_FAILED {
		t.Errorf("extraction_state = %s, want failed", got)
	}
	if version.GetExtractionError() == "" {
		t.Error("a failed extraction carries no error message")
	}
}

// The message is only meaningful for a failure, so a stale one from an earlier
// attempt must not travel beside a state that contradicts it.
func TestGetDocumentSuppressesAStaleExtractionErrorOnSuccess(t *testing.T) {
	store := newFakeStore()
	row := liveDocument()
	row.ExtractionError = new("a message from the attempt before this one")
	store.documents[liveDocumentID] = row

	doc, err := getDocument(t, store, "7")
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	if got := doc.GetCurrentVersion().GetExtractionError(); got != "" {
		t.Errorf("extraction_error = %q on a succeeded extraction, want empty", got)
	}
}

func TestGetDocumentReadsThePageCountTikaActuallyEmits(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata string
		media    string
		want     uint32
	}{
		{"an array of strings", `{"xmpTPg:NPages":["12"]}`, "application/pdf", 12},
		{"a bare string", `{"xmpTPg:NPages":"12"}`, "application/pdf", 12},
		{"a number", `{"xmpTPg:NPages":12}`, "application/pdf", 12},
		{"the office spelling", `{"meta:page-count":["4"]}`, "application/pdf", 4},
		{"no count at all", `{"Content-Type":"application/pdf"}`, "application/pdf", 0},
		{"metadata that is not an object", `["nope"]`, "application/pdf", 0},
		// An image is one page whether or not anything counted, which keeps
		// hirame's OUT_OF_RANGE in front of Gahaku's INVALID_ARGUMENT.
		{"an image", `{}`, "image/png", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			row := liveDocument()
			row.Metadata = []byte(tc.metadata)
			row.ContentType = &tc.media
			store.documents[liveDocumentID] = row

			doc, err := getDocument(t, store, "7")
			if err != nil {
				t.Fatalf("get document: %v", err)
			}
			if got := doc.GetCurrentVersion().GetPageCount(); got != tc.want {
				t.Errorf("page_count = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestGetDocumentMapsExtractionState(t *testing.T) {
	for _, tc := range []struct {
		status *string
		want   hiramev1.ExtractionState
	}{
		{new("pending"), hiramev1.ExtractionState_EXTRACTION_STATE_PENDING},
		{new("succeeded"), hiramev1.ExtractionState_EXTRACTION_STATE_SUCCEEDED},
		{new("failed"), hiramev1.ExtractionState_EXTRACTION_STATE_FAILED},
		// No extraction row yet: the version exists but nothing has run.
		{nil, hiramev1.ExtractionState_EXTRACTION_STATE_UNSPECIFIED},
	} {
		store := newFakeStore()
		row := liveDocument()
		row.ExtractionStatus = tc.status
		store.documents[liveDocumentID] = row

		doc, err := getDocument(t, store, "7")
		if err != nil {
			t.Fatalf("get document: %v", err)
		}
		if got := doc.GetCurrentVersion().GetExtractionState(); got != tc.want {
			t.Errorf("status %v mapped to %s, want %s", tc.status, got, tc.want)
		}
	}
}

// A document recorded under a root it no longer sits beneath keeps a usable
// path rather than collapsing to an empty string.
func TestGetDocumentFallsBackWhenThePathLeftItsRoot(t *testing.T) {
	store := newFakeStore()
	row := liveDocument()
	row.Path = "/elsewhere/a.pdf"
	store.documents[liveDocumentID] = row

	doc, err := getDocument(t, store, "7")
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	if got := doc.GetRelativePath(); got != "/elsewhere/a.pdf" {
		t.Errorf("relative_path = %q, want the whole path", got)
	}
}
