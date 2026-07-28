package service

import (
	"context"
	"path"
	"strconv"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"

	hiramev1 "github.com/ngicks/hirame/apps/search-api/internal/gen/hirame/v1"
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
)

// Document resolves a document's current content version.
type Document struct {
	store DocumentStore
}

// NewDocument builds the handler.
func NewDocument(store DocumentStore) *Document { return &Document{store: store} }

// GetDocument implements hiramev1connect.DocumentServiceHandler.
//
// A tombstoned document answers rather than failing: the proto gives Document a
// `deleted` flag and leaves current_version unset for exactly this case, and
// the GUI's stale-ref recovery calls here first — telling it "this document is
// gone" is what lets it stop, where NOT_FOUND would be indistinguishable from a
// bad id.
func (d *Document) GetDocument(
	ctx context.Context,
	req *connect.Request[hiramev1.GetDocumentRequest],
) (*connect.Response[hiramev1.GetDocumentResponse], error) {
	row, err := loadDocument(ctx, d.store, req.Msg.GetDocumentId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hiramev1.GetDocumentResponse{
		Document: documentMessage(row),
	}), nil
}

func documentMessage(row sqlcgen.GetDocumentRow) *hiramev1.Document {
	doc := &hiramev1.Document{
		DocumentId:   strconv.FormatInt(row.ID, 10),
		MountpointId: strconv.FormatInt(row.MountpointID, 10),
		RelativePath: relativePath(row.RootPath, row.Path),
		FileName:     path.Base(row.Path),
		Deleted:      row.DeletedAt.Valid,
		// updated_at is the closest the schema has to "content last observed to
		// change": it moves on a version flip, and also on a move or tombstone.
		// The alternative, the filesystem's own mtime, describes the file
		// rather than what this system observed.
		DiscoveredTime: timestamp(row.FirstSeenAt),
		ModifiedTime:   timestamp(row.UpdatedAt),
	}
	// A tombstone keeps its row but has nothing to render, so current_version
	// stays unset (D-007) and every render path rejects its refs.
	if !row.DeletedAt.Valid && row.ContentHash != nil {
		doc.CurrentVersion = contentVersionMessage(row)
	}
	return doc
}

func contentVersionMessage(row sqlcgen.GetDocumentRow) *hiramev1.ContentVersion {
	mediaType := mediaTypeOf(row.ContentType)
	version := &hiramev1.ContentVersion{
		ContentVersionId: *row.ContentHash,
		MediaType:        mediaType,
		PageCount:        pageCountOf(row.Metadata, mediaType),
		ExtractionState:  extractionState(row.ExtractionStatus),
		HasExtractedText: row.HasExtractedText,
		FirstSeenTime:    timestamp(row.ContentFirstSeenAt),
	}
	if row.SizeBytes != nil && *row.SizeBytes > 0 {
		version.SizeBytes = uint64(*row.SizeBytes)
	}
	// Only meaningful for a failure, and the proto says so; carrying a stale
	// message from an earlier attempt would contradict the state beside it.
	if version.GetExtractionState() == hiramev1.ExtractionState_EXTRACTION_STATE_FAILED &&
		row.ExtractionError != nil {
		version.ExtractionError = *row.ExtractionError
	}
	return version
}

// extractionState maps extracted_contents.status.
//
// EXTRACTION_STATE_UNSUPPORTED is unreachable by construction: the schema's
// status check admits only pending, succeeded and failed, and a media type this
// system does not extract never becomes a document at all — the ingestion
// filter drops it before a row exists.
func extractionState(status *string) hiramev1.ExtractionState {
	if status == nil {
		return hiramev1.ExtractionState_EXTRACTION_STATE_UNSPECIFIED
	}
	switch *status {
	case "pending":
		return hiramev1.ExtractionState_EXTRACTION_STATE_PENDING
	case "succeeded":
		return hiramev1.ExtractionState_EXTRACTION_STATE_SUCCEEDED
	case "failed":
		return hiramev1.ExtractionState_EXTRACTION_STATE_FAILED
	default:
		return hiramev1.ExtractionState_EXTRACTION_STATE_UNSPECIFIED
	}
}

func timestamp(ts pgtype.Timestamptz) *timestamppb.Timestamp {
	if !ts.Valid {
		return nil
	}
	return timestamppb.New(ts.Time)
}
