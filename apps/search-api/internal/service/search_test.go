package service_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"connectrpc.com/connect"

	hiramev1 "github.com/ngicks/hirame/apps/search-api/internal/gen/hirame/v1"
	"github.com/ngicks/hirame/apps/search-api/internal/service"
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
)

func searchRow(id int64, path, snippet string) sqlcgen.SearchDocumentsRow {
	return sqlcgen.SearchDocumentsRow{
		DocumentID:       id,
		MountpointID:     1,
		RootPath:         liveRoot,
		Path:             path,
		ContentVersionID: id + 100,
		ContentHash:      "hash-" + path,
		SizeBytes:        10,
		ContentType:      new("text/plain; charset=UTF-8"),
		Score:            1.5,
		Snippet:          snippet,
	}
}

func searchRows(n int) []sqlcgen.SearchDocumentsRow {
	rows := make([]sqlcgen.SearchDocumentsRow, 0, n)
	for i := range n {
		rows = append(rows, searchRow(int64(i+1), liveRoot+"/d.txt", "text"))
	}
	return rows
}

func mustSearch(
	t *testing.T, handler *service.Search, req *hiramev1.SearchRequest,
) *hiramev1.SearchResponse {
	t.Helper()
	resp, err := handler.Search(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return resp.Msg
}

func TestSearchNormalizesTheQueryBeforeItReachesTheIndex(t *testing.T) {
	store := newFakeStore()
	store.searchRows = searchRows(1)
	handler := service.NewSearch(store)

	// Halfwidth kana in, NFKC out: the index holds text folded the same way,
	// so an unnormalized query would simply never meet it.
	mustSearch(t, handler, &hiramev1.SearchRequest{Query: "ｱﾊﾟｰﾄ"})

	if got := store.searchCalls[0].Query; got != "アパート" {
		t.Errorf("query reached the index as %q, want the NFKC-folded アパート", got)
	}
}

func TestSearchRejectsAnEmptyQuery(t *testing.T) {
	handler := service.NewSearch(newFakeStore())

	for _, query := range []string{"", "   "} {
		_, err := handler.Search(t.Context(), connect.NewRequest(
			&hiramev1.SearchRequest{Query: query},
		))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("query %q: code = %s, want invalid_argument",
				query, connect.CodeOf(err))
		}
	}
}

func TestSearchPagesWithATokenThatRoundTrips(t *testing.T) {
	store := newFakeStore()
	store.searchRows = searchRows(5)
	store.searchTotal = 5
	handler := service.NewSearch(store)

	first := mustSearch(t, handler, &hiramev1.SearchRequest{Query: "検索", PageSize: 2})
	if len(first.GetHits()) != 2 {
		t.Fatalf("first page has %d hits, want 2", len(first.GetHits()))
	}
	if first.GetNextPageToken() == "" {
		t.Fatal("first page carries no next token though more rows exist")
	}
	if first.GetEstimatedTotalHits() != 5 {
		t.Errorf("estimated_total_hits = %d, want 5", first.GetEstimatedTotalHits())
	}

	second := mustSearch(t, handler, &hiramev1.SearchRequest{
		Query:     "検索",
		PageSize:  2,
		PageToken: first.GetNextPageToken(),
	})
	if got := store.searchCalls[1].ResultOffset; got != 2 {
		t.Errorf("second page read from offset %d, want 2", got)
	}
	if second.GetNextPageToken() == "" {
		t.Error("second page carries no next token though a fifth row exists")
	}

	last := mustSearch(t, handler, &hiramev1.SearchRequest{
		Query:     "検索",
		PageSize:  2,
		PageToken: second.GetNextPageToken(),
	})
	if len(last.GetHits()) != 1 {
		t.Errorf("last page has %d hits, want the 1 remaining", len(last.GetHits()))
	}
	// Empty on the last page, which is what stops the pager offering another.
	if last.GetNextPageToken() != "" {
		t.Errorf("last page carries next token %q, want none", last.GetNextPageToken())
	}
}

// The token encodes its query, so pairing it with another one is refused
// rather than answered with the wrong query's page.
func TestSearchRejectsATokenIssuedForAnotherQuery(t *testing.T) {
	store := newFakeStore()
	store.searchRows = searchRows(5)
	handler := service.NewSearch(store)

	first := mustSearch(t, handler, &hiramev1.SearchRequest{Query: "検索", PageSize: 2})

	_, err := handler.Search(t.Context(), connect.NewRequest(&hiramev1.SearchRequest{
		Query:     "別のクエリ",
		PageSize:  2,
		PageToken: first.GetNextPageToken(),
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want invalid_argument", connect.CodeOf(err))
	}
}

// The offset is narrowed to int32 at the query boundary, so an out-of-range one
// would wrap to a negative OFFSET and surface as an internal error instead of
// the bad request it is.
func TestSearchRejectsATokenWithAnUnreachableOffset(t *testing.T) {
	handler := service.NewSearch(newFakeStore())

	forged := base64.RawURLEncoding.EncodeToString([]byte(
		`{"q":"` + "00000000" + `","o":9223372036854775807}`,
	))
	_, err := handler.Search(t.Context(), connect.NewRequest(&hiramev1.SearchRequest{
		Query:     "検索",
		PageToken: forged,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want invalid_argument", connect.CodeOf(err))
	}
}

func TestSearchRejectsAMalformedToken(t *testing.T) {
	handler := service.NewSearch(newFakeStore())

	for _, token := range []string{"not-base64!!", "aGVsbG8"} {
		_, err := handler.Search(t.Context(), connect.NewRequest(&hiramev1.SearchRequest{
			Query:     "検索",
			PageToken: token,
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("token %q: code = %s, want invalid_argument",
				token, connect.CodeOf(err))
		}
	}
}

// A NFKC-equivalent query is the same query, so its token still resolves —
// the digest is taken after normalization for exactly that reason.
func TestSearchTokenSurvivesAnEquivalentlyNormalizedQuery(t *testing.T) {
	store := newFakeStore()
	store.searchRows = searchRows(5)
	handler := service.NewSearch(store)

	first := mustSearch(t, handler, &hiramev1.SearchRequest{Query: "アパート", PageSize: 2})

	if _, err := handler.Search(t.Context(), connect.NewRequest(&hiramev1.SearchRequest{
		Query:     "ｱﾊﾟｰﾄ",
		PageSize:  2,
		PageToken: first.GetNextPageToken(),
	})); err != nil {
		t.Errorf("halfwidth spelling of the same query was refused: %v", err)
	}
}

func TestSearchClampsThePageSize(t *testing.T) {
	store := newFakeStore()
	handler := service.NewSearch(store)

	for _, tc := range []struct {
		name      string
		requested uint32
		want      int32
	}{
		{"zero selects the default", 0, 21},
		{"a large size is clamped, not rejected", 10_000, 101},
		{"an ordinary size is honoured", 5, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store.searchCalls = nil
			mustSearch(t, handler, &hiramev1.SearchRequest{
				Query:    "検索",
				PageSize: tc.requested,
			})
			// One past the page: that extra row is how the handler knows
			// whether to issue a next token.
			if got := store.searchCalls[0].ResultLimit; got != tc.want {
				t.Errorf("limit = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSearchHitCarriesTheRefAndPresentationFields(t *testing.T) {
	store := newFakeStore()
	store.searchRows = []sqlcgen.SearchDocumentsRow{
		searchRow(9, liveRoot+"/報告/2026.txt", "text"),
	}
	handler := service.NewSearch(store)

	hit := mustSearch(t, handler, &hiramev1.SearchRequest{Query: "検索"}).GetHits()[0]

	if got := hit.GetRef().GetDocumentId(); got != "9" {
		t.Errorf("document_id = %q, want 9", got)
	}
	// The ref carries the version that matched, so a client can render straight
	// from a result without a second round trip.
	if got := hit.GetRef().GetContentVersionId(); got == "" {
		t.Error("hit carries no content version")
	}
	if got := hit.GetRelativePath(); got != "報告/2026.txt" {
		t.Errorf("relative_path = %q, want it relative to the mountpoint root", got)
	}
	if got := hit.GetFileName(); got != "2026.txt" {
		t.Errorf("file_name = %q, want 2026.txt", got)
	}
	// Tika reports parameters; the proto asks for a bare IANA media type.
	if got := hit.GetMediaType(); got != "text/plain" {
		t.Errorf("media_type = %q, want text/plain", got)
	}
}

func TestSearchSplitsSnippetsAtHighlightBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		snippet string
		want    []*hiramev1.SnippetSegment
	}{
		{
			name:    "a highlight in the middle",
			snippet: "全文<b>検索</b>の課題",
			want: []*hiramev1.SnippetSegment{
				{Text: "全文"},
				{Text: "検索", Highlighted: true},
				{Text: "の課題"},
			},
		},
		{
			name:    "two highlights",
			snippet: "<b>検索</b>と<b>評価</b>",
			want: []*hiramev1.SnippetSegment{
				{Text: "検索", Highlighted: true},
				{Text: "と"},
				{Text: "評価", Highlighted: true},
			},
		},
		{
			name:    "an unhighlighted excerpt, which the ngram fallback produces",
			snippet: "本文だけ",
			want:    []*hiramev1.SnippetSegment{{Text: "本文だけ"}},
		},
		{
			name:    "an unclosed marker stays text rather than losing characters",
			snippet: "全文<b>検索",
			want:    []*hiramev1.SnippetSegment{{Text: "全文<b>検索"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.searchRows = []sqlcgen.SearchDocumentsRow{
				searchRow(1, liveRoot+"/a.txt", tc.snippet),
			}
			handler := service.NewSearch(store)

			hits := mustSearch(t, handler, &hiramev1.SearchRequest{Query: "検索"}).GetHits()
			if len(hits[0].GetSnippets()) != 1 {
				t.Fatalf("hit carries %d snippets, want 1", len(hits[0].GetSnippets()))
			}
			got := hits[0].GetSnippets()[0].GetSegments()

			if len(got) != len(tc.want) {
				t.Fatalf("got %d segments, want %d", len(got), len(tc.want))
			}
			var rebuilt strings.Builder
			for i, segment := range got {
				if segment.GetText() != tc.want[i].GetText() {
					t.Errorf("segment %d text = %q, want %q",
						i, segment.GetText(), tc.want[i].GetText())
				}
				if segment.GetHighlighted() != tc.want[i].GetHighlighted() {
					t.Errorf("segment %d highlighted = %v, want %v",
						i, segment.GetHighlighted(), tc.want[i].GetHighlighted())
				}
				rebuilt.WriteString(segment.GetText())
			}
			// The proto's contract: concatenating every segment reproduces the
			// excerpt. Only the markers themselves may go missing.
			want := strings.ReplaceAll(strings.ReplaceAll(tc.snippet, "<b>", ""), "</b>", "")
			if len(got) > 1 && rebuilt.String() != want {
				t.Errorf("segments rebuild %q, want %q", rebuilt.String(), want)
			}
		})
	}
}

func TestSearchHonoursMaxSnippetsAsACeiling(t *testing.T) {
	store := newFakeStore()
	store.searchRows = []sqlcgen.SearchDocumentsRow{
		searchRow(1, liveRoot+"/a.txt", "全文<b>検索</b>"),
	}
	handler := service.NewSearch(store)

	// The query produces one snippet per hit, so a larger ceiling cannot
	// conjure more and a request for none suppresses it.
	for _, tc := range []struct {
		requested uint32
		want      int
	}{{0, 1}, {1, 1}, {5, 1}} {
		resp := mustSearch(t, handler, &hiramev1.SearchRequest{
			Query:       "検索",
			MaxSnippets: tc.requested,
		})
		if got := len(resp.GetHits()[0].GetSnippets()); got != tc.want {
			t.Errorf("max_snippets %d produced %d snippets, want %d",
				tc.requested, got, tc.want)
		}
	}
}
