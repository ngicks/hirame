// Package service holds the ConnectRPC handler implementations.
//
// Handlers live here rather than beside the generated code because internal/gen
// is deleted and rewritten by `buf generate`.
//
// The identifiers crossing this boundary are strings the client treats as
// opaque, and they are not the database's own. A document_id is its row id; a
// content_version_id is the content hash, which is what makes the pair
// meaningful: content is deduplicated by hash (D-007), so the hash names the
// exact bytes a render was addressed to no matter which document is holding
// them. Resolving a ref is therefore one GetDocument, and a superseded ref is
// visible as a hash that is no longer the document's current one.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	hiramev1 "github.com/ngicks/hirame/apps/search-api/internal/gen/hirame/v1"
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
)

// DocumentStore is the row lookup every handler starts from: it answers both
// "does this document exist" and "is this ref still current".
// *sqlcgen.Queries satisfies it as generated.
type DocumentStore interface {
	GetDocument(ctx context.Context, id int64) (sqlcgen.GetDocumentRow, error)
}

// resolveRef finds the document a ref names and rejects the ref if it no longer
// addresses that document's current content.
//
// This is the single chokepoint the render and thumbnail contracts rest on: an
// unknown document is NOT_FOUND, a superseded or tombstoned one is
// FAILED_PRECONDITION, and nothing is ever transparently upgraded to the
// current version — the client re-reads the document and retries, which is what
// lets it tell a stale image from a fresh one.
func resolveRef(
	ctx context.Context,
	store DocumentStore,
	ref *hiramev1.DocumentRef,
) (sqlcgen.GetDocumentRow, error) {
	if ref.GetDocumentId() == "" || ref.GetContentVersionId() == "" {
		return sqlcgen.GetDocumentRow{}, connect.NewError(
			connect.CodeInvalidArgument,
			errors.New("ref requires both document_id and content_version_id"),
		)
	}

	row, err := loadDocument(ctx, store, ref.GetDocumentId())
	if err != nil {
		return sqlcgen.GetDocumentRow{}, err
	}

	// A tombstone has no current version at all, so it fails the same check a
	// superseded ref does rather than needing a case of its own.
	if row.DeletedAt.Valid || row.ContentHash == nil {
		return sqlcgen.GetDocumentRow{}, connect.NewError(
			connect.CodeFailedPrecondition,
			fmt.Errorf("document %s has no current content version", ref.GetDocumentId()),
		)
	}
	if *row.ContentHash != ref.GetContentVersionId() {
		return sqlcgen.GetDocumentRow{}, connect.NewError(
			connect.CodeFailedPrecondition,
			fmt.Errorf(
				"content version %s is superseded for document %s",
				ref.GetContentVersionId(), ref.GetDocumentId(),
			),
		)
	}
	return row, nil
}

// loadDocument resolves a document_id with no version check.
//
// An id that is not a number is NOT_FOUND rather than INVALID_ARGUMENT: the
// value is opaque to the client, which cannot tell a malformed one from a
// deleted one, and the GUI's stale-ref recovery calls GetDocument before it
// gives up. Reporting "malformed" would send it down a path it has no way to
// act on.
func loadDocument(
	ctx context.Context,
	store DocumentStore,
	documentID string,
) (sqlcgen.GetDocumentRow, error) {
	id, err := strconv.ParseInt(documentID, 10, 64)
	if err != nil {
		return sqlcgen.GetDocumentRow{}, notFound(documentID)
	}
	row, err := store.GetDocument(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.GetDocumentRow{}, notFound(documentID)
	}
	if err != nil {
		return sqlcgen.GetDocumentRow{}, connect.NewError(
			connect.CodeInternal,
			fmt.Errorf("load document %s: %w", documentID, err),
		)
	}
	return row, nil
}

func notFound(documentID string) error {
	return connect.NewError(
		connect.CodeNotFound,
		fmt.Errorf("document %s not found", documentID),
	)
}

// currentRef rebuilds the ref a resolved row is addressed by, so responses echo
// exactly what the client sent rather than a value assembled twice.
func currentRef(row sqlcgen.GetDocumentRow) *hiramev1.DocumentRef {
	ref := &hiramev1.DocumentRef{DocumentId: strconv.FormatInt(row.ID, 10)}
	if row.ContentHash != nil {
		ref.ContentVersionId = *row.ContentHash
	}
	return ref
}

// relativePath turns the absolute path stored for a document into the
// mountpoint-relative one the API reports. Identity lives in document_id
// (D-007), so this is presentation only.
func relativePath(root, absolute string) string {
	switch {
	case absolute == root:
		return path.Base(absolute)
	case strings.HasPrefix(absolute, root+"/"):
		return absolute[len(root)+1:]
	default:
		// A path that does not sit under the root it was recorded with means
		// the mountpoint was reconfigured; the whole path is still more useful
		// to a reader than an empty string.
		return absolute
	}
}

// pageCountOf reads a renderable page count out of Tika's metadata, 0 when the
// document does not say.
//
// 0 is a first-class answer the proto allows, and it is what disables the
// server-side OUT_OF_RANGE check: guessing a count would reject pages that
// exist, while reporting none defers the decision to Gahaku, which has the
// document open.
func pageCountOf(metadata []byte, mediaType string) uint32 {
	// Images are one page whether or not Tika counted: it is the one media
	// type where the count is a property of the kind rather than the file, and
	// knowing it keeps hirame's own OUT_OF_RANGE in front of Gahaku, which
	// rejects an over-range image page as INVALID_ARGUMENT instead.
	if strings.HasPrefix(mediaType, "image/") {
		return 1
	}
	if len(metadata) == 0 {
		return 0
	}
	// Tika's key set is open-ended and a value is either a scalar or an array
	// of them, so the document is decoded one level deep and each candidate
	// key interpreted on its own. Decoding into map[string]string would fail
	// outright on any multi-valued key and lose the count for every document.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &fields); err != nil {
		return 0
	}
	for _, key := range []string{"xmpTPg:NPages", "meta:page-count", "Page-Count"} {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		if n := firstNumber(raw); n > 0 {
			return n
		}
	}
	return 0
}

// firstNumber reads a count out of a Tika value, which may be a number, a
// string holding one, or an array whose first element is either.
func firstNumber(raw json.RawMessage) uint32 {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0
	}
	if list, ok := value.([]any); ok {
		if len(list) == 0 {
			return 0
		}
		value = list[0]
	}
	switch v := value.(type) {
	case float64:
		if v < 1 {
			return 0
		}
		return uint32(v)
	case string:
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 32)
		if err != nil {
			return 0
		}
		return uint32(n)
	default:
		return 0
	}
}

// mediaTypeOf reports the detected media type, empty when detection failed.
func mediaTypeOf(contentType *string) string {
	if contentType == nil {
		return ""
	}
	// Tika reports parameters such as "; charset=UTF-8"; the proto asks for an
	// IANA media type, and every consumer here compares against a bare one.
	if i := strings.IndexByte(*contentType, ';'); i >= 0 {
		return strings.TrimSpace((*contentType)[:i])
	}
	return strings.TrimSpace(*contentType)
}
