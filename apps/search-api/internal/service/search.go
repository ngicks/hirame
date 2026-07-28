package service

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	hiramev1 "github.com/ngicks/hirame/apps/search-api/internal/gen/hirame/v1"
	"github.com/ngicks/hirame/apps/search-api/internal/store"
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
)

// Search page sizing. The maximum is a guard against a caller asking for the
// whole corpus in one response, not a tuned value; the GUI asks for 20.
const (
	defaultPageSize = 20
	maxPageSize     = 100
)

// defaultMaxSnippets matches what the query produces. SearchDocuments returns
// one snippet per hit, so this is the ceiling being honoured rather than a
// count to generate up to.
const defaultMaxSnippets = 1

// Highlight markers pg_search wraps a snippet's matching terms in. They are
// pg_search 0.24.3's defaults; the query does not override them, and this is
// the only place that knows what they are.
const (
	highlightOpen  = "<b>"
	highlightClose = "</b>"
)

// SearchStore is the BM25 read surface. *sqlcgen.Queries satisfies it as
// generated.
type SearchStore interface {
	SearchDocuments(
		ctx context.Context, arg sqlcgen.SearchDocumentsParams,
	) ([]sqlcgen.SearchDocumentsRow, error)
	CountSearchDocuments(ctx context.Context, query string) (int64, error)
}

// Search answers queries against the BM25 index.
type Search struct {
	store SearchStore
}

// NewSearch builds the handler.
func NewSearch(store SearchStore) *Search { return &Search{store: store} }

// Search implements hiramev1connect.SearchServiceHandler.
func (s *Search) Search(
	ctx context.Context,
	req *connect.Request[hiramev1.SearchRequest],
) (*connect.Response[hiramev1.SearchResponse], error) {
	msg := req.Msg

	// NFKC here rather than at the caller: the index holds text normalized the
	// same way at ingestion (D-012), and an unnormalized query simply never
	// meets halfwidth kana or fullwidth latin. The proto documents that
	// callers pass the query through as typed for exactly this reason.
	query := store.NormalizeText(msg.GetQuery())
	if strings.TrimSpace(query) == "" {
		return nil, connect.NewError(
			connect.CodeInvalidArgument,
			errors.New("query is required"),
		)
	}

	offset, err := decodePageToken(msg.GetPageToken(), query)
	if err != nil {
		return nil, err
	}
	pageSize := resolvePageSize(msg.GetPageSize())

	// One row past the page decides whether there is a next page. The count
	// below cannot: it is taken separately and may disagree with this page
	// under concurrent ingestion, and paging off the end of a stale count is
	// how a pager ends up offering a page that comes back empty.
	rows, err := s.store.SearchDocuments(ctx, sqlcgen.SearchDocumentsParams{
		Query:        query,
		ResultOffset: int32(offset),
		ResultLimit:  int32(pageSize) + 1,
	})
	if err != nil {
		return nil, connect.NewError(
			connect.CodeInternal,
			fmt.Errorf("search: %w", err),
		)
	}

	hasMore := len(rows) > pageSize
	if hasMore {
		rows = rows[:pageSize]
	}

	total, err := s.store.CountSearchDocuments(ctx, query)
	if err != nil {
		return nil, connect.NewError(
			connect.CodeInternal,
			fmt.Errorf("count search results: %w", err),
		)
	}

	resp := &hiramev1.SearchResponse{
		Hits:               make([]*hiramev1.SearchHit, 0, len(rows)),
		EstimatedTotalHits: uint64(max(total, 0)),
	}
	for _, row := range rows {
		resp.Hits = append(resp.Hits, searchHit(row, msg.GetMaxSnippets()))
	}
	if hasMore {
		next, err := encodePageToken(query, offset+int64(len(rows)))
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		resp.NextPageToken = next
	}
	return connect.NewResponse(resp), nil
}

func resolvePageSize(requested uint32) int {
	if requested == 0 {
		return defaultPageSize
	}
	// Clamped rather than rejected, as the proto documents.
	return min(int(requested), maxPageSize)
}

func searchHit(row sqlcgen.SearchDocumentsRow, maxSnippets uint32) *hiramev1.SearchHit {
	hit := &hiramev1.SearchHit{
		Ref: &hiramev1.DocumentRef{
			DocumentId:       strconv.FormatInt(row.DocumentID, 10),
			ContentVersionId: row.ContentHash,
		},
		FileName:     path.Base(row.Path),
		RelativePath: relativePath(row.RootPath, row.Path),
		MediaType:    mediaTypeOf(row.ContentType),
		Score:        float64(row.Score),
	}
	if snippetBudget(maxSnippets) > 0 && row.Snippet != "" {
		hit.Snippets = []*hiramev1.Snippet{{Segments: snippetSegments(row.Snippet)}}
	}
	return hit
}

func snippetBudget(requested uint32) int {
	if requested == 0 {
		return defaultMaxSnippets
	}
	return min(int(requested), defaultMaxSnippets)
}

// snippetSegments splits a highlighted excerpt at its markers.
//
// Segments rather than offsets because the server counts UTF-8 bytes and the
// browser counts UTF-16 code units, so any offset would have to be translated
// on one side; the proto's contract is that concatenating every segment's text
// reproduces the excerpt, which this preserves whether or not the markers are
// balanced.
func snippetSegments(snippet string) []*hiramev1.SnippetSegment {
	var segments []*hiramev1.SnippetSegment
	appendText := func(text string, highlighted bool) {
		if text == "" {
			return
		}
		segments = append(segments, &hiramev1.SnippetSegment{
			Text:        text,
			Highlighted: highlighted,
		})
	}

	rest := snippet
	for {
		open := strings.Index(rest, highlightOpen)
		if open < 0 {
			break
		}
		closing := strings.Index(rest[open:], highlightClose)
		if closing < 0 {
			// An unclosed marker is not a highlight; leaving it in the text
			// keeps the concatenation identity rather than silently dropping
			// characters the excerpt contained.
			break
		}
		closing += open

		appendText(rest[:open], false)
		appendText(rest[open+len(highlightOpen):closing], true)
		rest = rest[closing+len(highlightClose):]
	}
	appendText(rest, false)

	if segments == nil {
		segments = []*hiramev1.SnippetSegment{{Text: snippet}}
	}
	return segments
}
