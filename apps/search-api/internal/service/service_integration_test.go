//go:build integration

// These tests drive the real Search handler against a live ParadeDB
// PostgreSQL. They are behind the `integration` build tag and additionally
// require HIRAME_TEST_DATABASE_URL, so `go test ./...` stays green on a machine
// with no database.
//
//	go test -tags integration ./internal/service/...
package service_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	hiramev1 "github.com/ngicks/hirame/apps/search-api/internal/gen/hirame/v1"
	"github.com/ngicks/hirame/apps/search-api/internal/service"
	"github.com/ngicks/hirame/apps/search-api/internal/store"
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
)

const dsnEnv = "HIRAME_TEST_DATABASE_URL"

// testDatabaseLockKey serializes every integration package that shares one
// database; it must match the value in internal/store.
const testDatabaseLockKey int64 = 0x6869_7261_6d65_7401 // "hirame t"

func TestMain(m *testing.M) { os.Exit(runLocked(m)) }

func runLocked(m *testing.M) int {
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		return m.Run()
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect for the suite lock: %v\n", err)
		return 1
	}
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "acquire a connection for the suite lock: %v\n", err)
		return 1
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", testDatabaseLockKey); err != nil {
		fmt.Fprintf(os.Stderr, "take the suite lock: %v\n", err)
		return 1
	}
	return m.Run()
}

func migrated(t *testing.T) *sqlcgen.Queries {
	t.Helper()
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("%s not set", dsnEnv)
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	migrator, err := store.NewMigrator(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new migrator: %v", err)
	}
	if _, err := migrator.Up(t.Context()); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
		TRUNCATE thumbnail_cache, extracted_contents, documents,
		         content_versions, fs_observations, mountpoints
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return sqlcgen.New(pool)
}

// seedDocument writes the whole create path: content version, document,
// extraction, and the pointer that makes the version current.
func seedDocument(
	t *testing.T, q *sqlcgen.Queries, mountpointID int64, path, hash, rawText string,
) int64 {
	t.Helper()
	ctx := t.Context()

	cv, err := q.UpsertContentVersion(ctx, sqlcgen.UpsertContentVersionParams{
		ContentHash: hash,
		SizeBytes:   int64(len(rawText)),
	})
	if err != nil {
		t.Fatalf("upsert content version %s: %v", hash, err)
	}
	doc, err := q.CreateDocument(ctx, sqlcgen.CreateDocumentParams{
		MountpointID: mountpointID,
		Path:         path,
		Dev:          64,
		Ino:          cv.ID + 1000,
	})
	if err != nil {
		t.Fatalf("create document %s: %v", path, err)
	}
	if _, err := q.UpsertExtraction(ctx, sqlcgen.UpsertExtractionParams{
		ContentVersionID: cv.ID,
		Status:           "succeeded",
		TextNormalized:   store.NormalizeText(rawText),
		Metadata:         []byte(`{"Content-Type":"text/plain"}`),
		ContentType:      new("text/plain"),
	}); err != nil {
		t.Fatalf("upsert extraction %s: %v", path, err)
	}
	doc, err = q.SetDocumentCurrentVersion(ctx, sqlcgen.SetDocumentCurrentVersionParams{
		ID:                      doc.ID,
		CurrentContentVersionID: &cv.ID,
	})
	if err != nil {
		t.Fatalf("set current version %s: %v", path, err)
	}
	return doc.ID
}

func seedMountpoint(t *testing.T, q *sqlcgen.Queries) int64 {
	t.Helper()
	mp, err := q.UpsertMountpoint(t.Context(), sqlcgen.UpsertMountpointParams{
		RootPath: "/srv/docs",
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("upsert mountpoint: %v", err)
	}
	return mp.ID
}

func searchOnce(
	t *testing.T, q *sqlcgen.Queries, req *hiramev1.SearchRequest,
) *hiramev1.SearchResponse {
	t.Helper()
	resp, err := service.NewSearch(q).Search(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("search %q: %v", req.GetQuery(), err)
	}
	return resp.Msg
}

func hitPaths(resp *hiramev1.SearchResponse) []string {
	paths := make([]string, 0, len(resp.GetHits()))
	for _, hit := range resp.GetHits() {
		paths = append(paths, hit.GetRelativePath())
	}
	return paths
}

func rankOf(resp *hiramev1.SearchResponse, relativePath string) int {
	for i, hit := range resp.GetHits() {
		if hit.GetRelativePath() == relativePath {
			return i
		}
	}
	return -1
}

// The D-012 validation result: `アパート` reaches a document holding `レポート`
// through the shared `ート` bigram, and unboosted that recall hit can outrank
// the document that actually contains the word. The corpus below is built to
// produce exactly that inversion — BM25 favours the short, term-dense artifact
// over the long true match — so the assertion fails if the Lindera boost in
// search.sql is removed or weakened.
func TestSearchRanksTheLinderaMatchAboveTheNgramArtifact(t *testing.T) {
	q := migrated(t)
	mountpointID := seedMountpoint(t, q)

	filler := strings.Repeat(
		"建物の維持管理に関する詳細な記録と点検の履歴。設備更新の計画と予算の概要。入居者対応の記録。",
		2000,
	)
	// Halfwidth kana on purpose: only NFKC at both ends makes this reachable
	// from a normally typed query.
	seedDocument(t, q, mountpointID, "/srv/docs/apart.txt", "aaaa",
		"ｱﾊﾟｰﾄ管理台帳のガイド。"+filler)
	seedDocument(t, q, mountpointID, "/srv/docs/report.txt", "bbbb", "レポート")
	// Documents carrying neither term, so the ngram field's `ート` is rare
	// enough to score the artifact highly.
	for i := range 30 {
		seedDocument(t, q, mountpointID,
			fmt.Sprintf("/srv/docs/noise%02d.txt", i), fmt.Sprintf("n%02d", i),
			strings.Repeat("会議の議事録と検索品質の評価。", 5)+strconv.Itoa(i))
	}

	resp := searchOnce(t, q, &hiramev1.SearchRequest{Query: "アパート"})
	for i, hit := range resp.GetHits() {
		t.Logf("#%d %s score=%f", i+1, hit.GetRelativePath(), hit.GetScore())
	}

	trueMatch := rankOf(resp, "apart.txt")
	artifact := rankOf(resp, "report.txt")

	if trueMatch < 0 {
		t.Fatalf("the document containing アパート is missing from %v", hitPaths(resp))
	}
	// The ngram field is a recall net, so the artifact is expected to be
	// returned. What must not happen is it ranking first.
	if artifact >= 0 && trueMatch > artifact {
		t.Errorf("the ngram artifact report.txt outranked the true match apart.txt (%v)",
			hitPaths(resp))
	}
	if trueMatch != 0 {
		t.Errorf("apart.txt ranked #%d, want first in %v", trueMatch+1, hitPaths(resp))
	}
}

// PostgreSQL replans a prepared statement generically once it has run a few
// times, and pg_search cannot resolve a bind parameter cast to pdb.boost under
// a generic plan — it fails outright with "the right-hand side of the `|||`
// operator must be a text or text array value". pgx prepares by default, so the
// documented boost spelling breaks search on the sixth query rather than the
// first, which no single-shot test would catch. Ten identical searches over one
// pool is what proves the query survives the switch.
func TestSearchSurvivesThePreparedStatementGenericPlan(t *testing.T) {
	q := migrated(t)
	mountpointID := seedMountpoint(t, q)
	seedDocument(t, q, mountpointID, "/srv/docs/apart.txt", "aaaa", "アパート管理台帳")

	for i := range 10 {
		resp, err := service.NewSearch(q).Search(t.Context(), connect.NewRequest(
			&hiramev1.SearchRequest{Query: "アパート"},
		))
		if err != nil {
			t.Fatalf("search #%d failed: %v", i+1, err)
		}
		if len(resp.Msg.GetHits()) != 1 {
			t.Fatalf("search #%d returned %d hits, want 1",
				i+1, len(resp.Msg.GetHits()))
		}
	}
}

func TestSearchEndToEnd(t *testing.T) {
	q := migrated(t)
	mountpointID := seedMountpoint(t, q)

	corpus := []struct{ path, hash, text string }{
		{"/srv/docs/sekkei.txt", "aaaa",
			"東京都渋谷区の設計仕様書。全文検索エンジンの評価レポート。"},
		{"/srv/docs/daichou.txt", "bbbb",
			"ｱﾊﾟｰﾄ管理台帳のガイド。ＰＤＦ で配布する半角カナの資料。"},
		{"/srv/docs/gijiroku.txt", "cccc",
			"大阪府の会議議事録 2026年 検索品質の評価と全文検索の課題"},
		{"/srv/docs/deleted.txt", "dddd",
			"削除済みドキュメント。検索 検索 検索 全文検索 全文検索。"},
	}
	ids := map[string]int64{}
	for _, c := range corpus {
		ids[c.path] = seedDocument(t, q, mountpointID, c.path, c.hash, c.text)
	}

	// By far the strongest match for both terms, and it must still be invisible.
	if _, err := q.TombstoneDocument(t.Context(), ids["/srv/docs/deleted.txt"]); err != nil {
		t.Fatalf("tombstone: %v", err)
	}

	t.Run("a tombstoned document is excluded", func(t *testing.T) {
		resp := searchOnce(t, q, &hiramev1.SearchRequest{Query: "全文検索"})
		for _, path := range hitPaths(resp) {
			if path == "deleted.txt" {
				t.Error("a tombstoned document was returned")
			}
		}
		if len(resp.GetHits()) == 0 {
			t.Fatal("no results at all")
		}
	})

	t.Run("halfwidth source is reachable from a fullwidth query", func(t *testing.T) {
		resp := searchOnce(t, q, &hiramev1.SearchRequest{Query: "アパート"})
		if rankOf(resp, "daichou.txt") < 0 {
			t.Errorf("daichou.txt missing from %v", hitPaths(resp))
		}
	})

	t.Run("hits carry a ref that renders without a second round trip", func(t *testing.T) {
		resp := searchOnce(t, q, &hiramev1.SearchRequest{Query: "検索"})
		hit := resp.GetHits()[0]
		if hit.GetRef().GetDocumentId() == "" || hit.GetRef().GetContentVersionId() == "" {
			t.Errorf("hit ref = %v, want both halves populated", hit.GetRef())
		}
		if hit.GetMediaType() != "text/plain" {
			t.Errorf("media_type = %q, want text/plain", hit.GetMediaType())
		}
		if hit.GetScore() <= 0 {
			t.Errorf("score = %f, want > 0", hit.GetScore())
		}
	})

	t.Run("a snippet highlights the match", func(t *testing.T) {
		resp := searchOnce(t, q, &hiramev1.SearchRequest{Query: "全文検索"})
		var highlighted bool
		for _, snippet := range resp.GetHits()[0].GetSnippets() {
			for _, segment := range snippet.GetSegments() {
				if segment.GetHighlighted() {
					highlighted = true
				}
			}
		}
		if !highlighted {
			t.Errorf("no segment was highlighted in %v", resp.GetHits()[0].GetSnippets())
		}
	})

	t.Run("paging walks every hit exactly once", func(t *testing.T) {
		seen := map[string]int{}
		var token string
		for range 10 {
			resp := searchOnce(t, q, &hiramev1.SearchRequest{
				Query:     "検索",
				PageSize:  1,
				PageToken: token,
			})
			for _, path := range hitPaths(resp) {
				seen[path]++
			}
			token = resp.GetNextPageToken()
			if token == "" {
				break
			}
		}
		if token != "" {
			t.Error("paging did not terminate within 10 pages")
		}
		if len(seen) == 0 {
			t.Fatal("paging returned nothing")
		}
		for path, count := range seen {
			if count != 1 {
				t.Errorf("%s appeared %d times across pages, want once", path, count)
			}
		}
	})

	t.Run("a token paired with another query is refused", func(t *testing.T) {
		first := searchOnce(t, q, &hiramev1.SearchRequest{Query: "検索", PageSize: 1})
		if first.GetNextPageToken() == "" {
			t.Skip("only one page of results; nothing to mispair")
		}
		_, err := service.NewSearch(q).Search(t.Context(), connect.NewRequest(
			&hiramev1.SearchRequest{
				Query:     "全文検索",
				PageSize:  1,
				PageToken: first.GetNextPageToken(),
			},
		))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("code = %s, want invalid_argument", connect.CodeOf(err))
		}
	})
}

// GetDocument is the only place the current content version is resolved, so the
// value it reports has to be the one every render is addressed by.
func TestGetDocumentEndToEnd(t *testing.T) {
	q := migrated(t)
	mountpointID := seedMountpoint(t, q)
	id := seedDocument(t, q, mountpointID, "/srv/docs/報告/2026.txt", "aaaa", "本文")

	resp, err := service.NewDocument(q).GetDocument(t.Context(), connect.NewRequest(
		&hiramev1.GetDocumentRequest{DocumentId: strconv.FormatInt(id, 10)},
	))
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	doc := resp.Msg.GetDocument()

	if doc.GetRelativePath() != "報告/2026.txt" {
		t.Errorf("relative_path = %q, want it relative to the mountpoint root",
			doc.GetRelativePath())
	}
	// The content hash, which is what a ref carries.
	if got := doc.GetCurrentVersion().GetContentVersionId(); got != "aaaa" {
		t.Errorf("content_version_id = %q, want the content hash aaaa", got)
	}
	if !doc.GetCurrentVersion().GetHasExtractedText() {
		t.Error("has_extracted_text is false though text was stored")
	}

	t.Run("a tombstone answers with no current version", func(t *testing.T) {
		if _, err := q.TombstoneDocument(t.Context(), id); err != nil {
			t.Fatalf("tombstone: %v", err)
		}
		resp, err := service.NewDocument(q).GetDocument(t.Context(), connect.NewRequest(
			&hiramev1.GetDocumentRequest{DocumentId: strconv.FormatInt(id, 10)},
		))
		if err != nil {
			t.Fatalf("a tombstone must answer, not fail: %v", err)
		}
		if !resp.Msg.GetDocument().GetDeleted() {
			t.Error("deleted is false for a tombstoned document")
		}
		if resp.Msg.GetDocument().GetCurrentVersion() != nil {
			t.Error("a tombstone reported a current version")
		}
	})
}
