//go:build integration

// These tests need a live ParadeDB PostgreSQL. They are behind the
// `integration` build tag and additionally require HIRAME_TEST_DATABASE_URL, so
// `go test ./...` stays green on a machine with no database.
//
//	go test -tags integration ./internal/store/...
package store_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ngicks/hirame/apps/search-api/internal/store"
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
)

const dsnEnv = "HIRAME_TEST_DATABASE_URL"

// testDatabaseLockKey serializes every integration package that shares one
// database. Each of them truncates the whole schema to get a clean fixture, and
// `go test ./...` runs packages in parallel, so without this two suites wipe
// each other's rows and deadlock against each other's TRUNCATE. The value is
// arbitrary but must match in every package that takes it.
const testDatabaseLockKey int64 = 0x6869_7261_6d65_7401 // "hirame t"

func TestMain(m *testing.M) { os.Exit(runLocked(m)) }

// runLocked holds a session-level advisory lock for the whole package run. It
// is a separate function because os.Exit would skip the deferred release.
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

	// The lock lives on this one connection, so it must stay checked out.
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

func newPool(t *testing.T) *pgxpool.Pool {
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
	return pool
}

func migrated(t *testing.T) (*pgxpool.Pool, *sqlcgen.Queries) {
	t.Helper()
	pool := newPool(t)
	migrator, err := store.NewMigrator(pool, discardLogger())
	if err != nil {
		t.Fatalf("new migrator: %v", err)
	}
	if _, err := migrator.Up(t.Context()); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	truncate(t, pool)
	return pool, sqlcgen.New(pool)
}

func truncate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(t.Context(), `
		TRUNCATE thumbnail_cache, extracted_contents, documents,
		         content_versions, fs_observations, mountpoints
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func TestMigratorUpIsIdempotent(t *testing.T) {
	pool := newPool(t)
	migrator, err := store.NewMigrator(pool, discardLogger())
	if err != nil {
		t.Fatalf("new migrator: %v", err)
	}

	if _, err := migrator.Up(t.Context()); err != nil {
		t.Fatalf("first up: %v", err)
	}
	second, err := migrator.Up(t.Context())
	if err != nil {
		t.Fatalf("second up: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second up applied %v, want nothing", second)
	}

	statuses, riverPending, err := migrator.Status(t.Context())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if riverPending {
		t.Error("river schema still pending after up")
	}
	if len(statuses) != len(migrator.Migrations()) {
		t.Fatalf("status reported %d migrations, want %d",
			len(statuses), len(migrator.Migrations()))
	}
	for _, s := range statuses {
		if !s.Applied {
			t.Errorf("migration %d (%s) not applied", s.Version, s.Name)
		}
		if s.Drifted {
			t.Errorf("migration %d (%s) reported as drifted", s.Version, s.Name)
		}
	}
}

func TestRiverTablesExist(t *testing.T) {
	pool, _ := migrated(t)
	var exists bool
	err := pool.QueryRow(t.Context(), `SELECT to_regclass('river_job') IS NOT NULL`).Scan(&exists)
	if err != nil {
		t.Fatalf("probe river_job: %v", err)
	}
	if !exists {
		t.Error("river_job table missing after migration")
	}
}

// seedDocument writes the whole create path: content version, document,
// extraction, and the pointer that makes the version current.
func seedDocument(
	t *testing.T, q *sqlcgen.Queries, mountpointID int64, path, hash, rawText string,
) (doc sqlcgen.Document, versionID int64) {
	t.Helper()
	ctx := t.Context()

	cv, err := q.UpsertContentVersion(ctx, sqlcgen.UpsertContentVersionParams{
		ContentHash: hash,
		SizeBytes:   int64(len(rawText)),
	})
	if err != nil {
		t.Fatalf("upsert content version %s: %v", hash, err)
	}

	doc, err = q.CreateDocument(ctx, sqlcgen.CreateDocumentParams{
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
	return doc, cv.ID
}

func seedCorpus(t *testing.T, q *sqlcgen.Queries) (mountpointID int64, byPath map[string]int64) {
	t.Helper()
	mp, err := q.UpsertMountpoint(t.Context(), sqlcgen.UpsertMountpointParams{
		RootPath: "/srv/docs",
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("upsert mountpoint: %v", err)
	}

	corpus := []struct{ path, hash, text string }{
		{
			"/srv/docs/sekkei.txt", "aaaa",
			"東京都渋谷区の設計仕様書。全文検索エンジンの評価レポート。",
		},
		{
			// Halfwidth kana and fullwidth latin on purpose: only NFKC at
			// ingestion makes this reachable from a normally typed query.
			"/srv/docs/daichou.txt", "bbbb",
			"ｱﾊﾟｰﾄ管理台帳のガイド。ＰＤＦ で配布する半角カナの資料。",
		},
		{
			"/srv/docs/gijiroku.txt", "cccc",
			"大阪府の会議議事録 2026年 検索品質の評価と全文検索の課題",
		},
		{
			"/srv/docs/deleted.txt", "dddd",
			"削除済みドキュメント。検索 検索 検索 全文検索 全文検索。",
		},
	}

	byPath = map[string]int64{}
	for _, c := range corpus {
		doc, _ := seedDocument(t, q, mp.ID, c.path, c.hash, c.text)
		byPath[c.path] = doc.ID
	}
	return mp.ID, byPath
}

func TestSearchJapanese(t *testing.T) {
	_, q := migrated(t)
	ctx := t.Context()
	_, byPath := seedCorpus(t, q)

	// The tombstone must be invisible even though it is by far the strongest
	// BM25 match for both query terms.
	if _, err := q.TombstoneDocument(ctx, byPath["/srv/docs/deleted.txt"]); err != nil {
		t.Fatalf("tombstone: %v", err)
	}

	for _, tc := range []struct {
		name      string
		query     string
		wantPaths []string
	}{
		{"two character term", "検索",
			[]string{"/srv/docs/gijiroku.txt", "/srv/docs/sekkei.txt"}},
		{"compound noun", "全文検索エンジン",
			[]string{"/srv/docs/sekkei.txt", "/srv/docs/gijiroku.txt"}},
		{"halfwidth source, fullwidth query", "アパート",
			[]string{"/srv/docs/daichou.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := q.SearchDocuments(ctx, sqlcgen.SearchDocumentsParams{
				Query:       store.NormalizeText(tc.query),
				ResultLimit: 10,
			})
			if err != nil {
				t.Fatalf("search %q: %v", tc.query, err)
			}
			for i, r := range rows {
				t.Logf("#%d %s score=%f snippet=%s", i+1, r.Path, r.Score, r.Snippet)
			}
			for _, want := range tc.wantPaths {
				if !containsPath(rows, want) {
					t.Errorf("query %q: %s missing from results", tc.query, want)
				}
			}
			for _, r := range rows {
				if r.Path == "/srv/docs/deleted.txt" {
					t.Errorf("query %q returned a tombstoned document", tc.query)
				}
				if r.Score <= 0 {
					t.Errorf("query %q: %s scored %f, want > 0", tc.query, r.Path, r.Score)
				}
			}
		})
	}
}

func TestSearchSnippetHighlightsMatch(t *testing.T) {
	_, q := migrated(t)
	seedCorpus(t, q)

	rows, err := q.SearchDocuments(t.Context(), sqlcgen.SearchDocumentsParams{
		Query:       store.NormalizeText("全文検索"),
		ResultLimit: 1,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no results")
	}
	t.Logf("snippet: %s", rows[0].Snippet)
	if !containsAll(rows[0].Snippet, "<b>", "</b>") {
		t.Errorf("snippet %q carries no highlight markup", rows[0].Snippet)
	}
}

// Each field is queried alone so the D-012 follow-up can see which one supplies
// a hit rather than only that the union found one.
func TestSearchFieldsDiffer(t *testing.T) {
	_, q := migrated(t)
	ctx := t.Context()
	seedCorpus(t, q)

	lindera, err := q.SearchDocumentsLindera(ctx, sqlcgen.SearchDocumentsLinderaParams{
		Query:       store.NormalizeText("渋谷"),
		ResultLimit: 10,
	})
	if err != nil {
		t.Fatalf("lindera search: %v", err)
	}
	ngram, err := q.SearchDocumentsNgram(ctx, sqlcgen.SearchDocumentsNgramParams{
		Query:       store.NormalizeText("渋谷"),
		ResultLimit: 10,
	})
	if err != nil {
		t.Fatalf("ngram search: %v", err)
	}
	for _, r := range lindera {
		t.Logf("lindera %s score=%f", r.Path, r.Score)
	}
	for _, r := range ngram {
		t.Logf("ngram   %s score=%f", r.Path, r.Score)
	}
	if len(lindera) == 0 {
		t.Error("lindera field returned nothing for 渋谷")
	}
	if len(ngram) == 0 {
		t.Error("ngram field returned nothing for 渋谷")
	}
}

func TestSearchPaginationIsDeterministic(t *testing.T) {
	_, q := migrated(t)
	ctx := t.Context()
	seedCorpus(t, q)

	all, err := q.SearchDocuments(ctx, sqlcgen.SearchDocumentsParams{
		Query:       store.NormalizeText("全文検索"),
		ResultLimit: 10,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	total, err := q.CountSearchDocuments(ctx, store.NormalizeText("全文検索"))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if int(total) != len(all) {
		t.Errorf("count %d != page length %d", total, len(all))
	}

	var paged []string
	for offset := int32(0); offset < int32(len(all)); offset++ {
		page, err := q.SearchDocuments(ctx, sqlcgen.SearchDocumentsParams{
			Query:        store.NormalizeText("全文検索"),
			ResultLimit:  1,
			ResultOffset: offset,
		})
		if err != nil {
			t.Fatalf("page %d: %v", offset, err)
		}
		if len(page) != 1 {
			t.Fatalf("page %d returned %d rows", offset, len(page))
		}
		paged = append(paged, page[0].Path)
	}
	for i, r := range all {
		if paged[i] != r.Path {
			t.Errorf("page %d is %s, single query said %s", i, paged[i], r.Path)
		}
	}
}

func TestDocumentIdentityAcrossRename(t *testing.T) {
	_, q := migrated(t)
	ctx := t.Context()
	mountpointID, byPath := seedCorpus(t, q)

	before, err := q.FindLiveDocumentByPath(ctx, sqlcgen.FindLiveDocumentByPathParams{
		MountpointID: mountpointID,
		Path:         "/srv/docs/sekkei.txt",
	})
	if err != nil {
		t.Fatalf("find by path: %v", err)
	}
	if before.ID != byPath["/srv/docs/sekkei.txt"] {
		t.Fatalf("find by path returned %d, want %d", before.ID, byPath["/srv/docs/sekkei.txt"])
	}

	byInode, err := q.FindLiveDocumentByInode(ctx, sqlcgen.FindLiveDocumentByInodeParams{
		MountpointID: mountpointID,
		Dev:          before.Dev,
		Ino:          before.Ino,
	})
	if err != nil {
		t.Fatalf("find by inode: %v", err)
	}
	if byInode.ID != before.ID {
		t.Errorf("inode lookup found %d, want %d", byInode.ID, before.ID)
	}

	moved, err := q.MoveDocument(ctx, sqlcgen.MoveDocumentParams{
		ID:   before.ID,
		Path: "/srv/docs/archive/sekkei.txt",
		Dev:  before.Dev,
		Ino:  before.Ino,
	})
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if moved.ID != before.ID {
		t.Errorf("move changed document id: %d -> %d", before.ID, moved.ID)
	}
	if moved.CurrentContentVersionID == nil ||
		*moved.CurrentContentVersionID != *before.CurrentContentVersionID {
		t.Error("move lost the content version pointer")
	}

	rows, err := q.SearchDocuments(ctx, sqlcgen.SearchDocumentsParams{
		Query:       store.NormalizeText("渋谷"),
		ResultLimit: 10,
	})
	if err != nil {
		t.Fatalf("search after move: %v", err)
	}
	if !containsPath(rows, "/srv/docs/archive/sekkei.txt") {
		t.Error("renamed document is not searchable at its new path")
	}
}

func TestFindLiveDocumentByContentOnlyWhenPathIsGone(t *testing.T) {
	_, q := migrated(t)
	ctx := t.Context()
	mountpointID, _ := seedCorpus(t, q)

	// While the old path is still observed, a same-hash file elsewhere is a
	// copy, not a move.
	if err := q.UpsertObservation(ctx, sqlcgen.UpsertObservationParams{
		MountpointID: mountpointID,
		Path:         "/srv/docs/sekkei.txt",
		Ino:          1001,
		Dev:          64,
	}); err != nil {
		t.Fatalf("upsert observation: %v", err)
	}
	_, err := q.FindLiveDocumentByContent(ctx, sqlcgen.FindLiveDocumentByContentParams{
		MountpointID: mountpointID,
		ContentHash:  "aaaa",
	})
	if err == nil {
		t.Error("content lookup matched a document whose path still exists")
	}

	if err := q.DeleteObservation(ctx, sqlcgen.DeleteObservationParams{
		MountpointID: mountpointID,
		Path:         "/srv/docs/sekkei.txt",
	}); err != nil {
		t.Fatalf("delete observation: %v", err)
	}
	found, err := q.FindLiveDocumentByContent(ctx, sqlcgen.FindLiveDocumentByContentParams{
		MountpointID: mountpointID,
		ContentHash:  "aaaa",
	})
	if err != nil {
		t.Fatalf("content lookup after path removal: %v", err)
	}
	if found.Path != "/srv/docs/sekkei.txt" {
		t.Errorf("content lookup found %s", found.Path)
	}
}

func TestObservationScanSweep(t *testing.T) {
	_, q := migrated(t)
	ctx := t.Context()
	mp, err := q.UpsertMountpoint(ctx, sqlcgen.UpsertMountpointParams{
		RootPath: "/srv/scan",
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("upsert mountpoint: %v", err)
	}

	epoch, err := q.NextScanEpoch(ctx)
	if err != nil {
		t.Fatalf("next epoch: %v", err)
	}
	for _, p := range []string{"/srv/scan/a", "/srv/scan/b"} {
		if err := q.UpsertObservation(ctx, sqlcgen.UpsertObservationParams{
			MountpointID: mp.ID, Path: p, SeenEpoch: epoch,
		}); err != nil {
			t.Fatalf("observe %s: %v", p, err)
		}
	}

	next, err := q.NextScanEpoch(ctx)
	if err != nil {
		t.Fatalf("next epoch: %v", err)
	}
	if next <= epoch {
		t.Fatalf("epoch did not advance: %d -> %d", epoch, next)
	}
	if err := q.UpsertObservation(ctx, sqlcgen.UpsertObservationParams{
		MountpointID: mp.ID, Path: "/srv/scan/a", SeenEpoch: next,
	}); err != nil {
		t.Fatalf("re-observe: %v", err)
	}

	swept, err := q.SweepScanEpoch(ctx, sqlcgen.SweepScanEpochParams{
		MountpointID: mp.ID,
		Root:         "/srv/scan",
		RootPrefix:   "/srv/scan/",
		SeenEpoch:    next,
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(swept) != 1 || swept[0] != "/srv/scan/b" {
		t.Errorf("sweep removed %v, want [/srv/scan/b]", swept)
	}

	if err := q.SetWatermark(ctx, 42); err != nil {
		t.Fatalf("set watermark: %v", err)
	}
	if err := q.SetWatermark(ctx, 7); err != nil {
		t.Fatalf("set watermark: %v", err)
	}
	wm, err := q.GetWatermark(ctx)
	if err != nil {
		t.Fatalf("get watermark: %v", err)
	}
	if wm != 42 {
		t.Errorf("watermark is %d, want 42 (must never go backwards)", wm)
	}
}

func TestThumbnailCacheLifecycle(t *testing.T) {
	_, q := migrated(t)
	ctx := t.Context()
	_, byPath := seedCorpus(t, q)

	docID := byPath["/srv/docs/sekkei.txt"]
	doc, err := q.GetDocument(ctx, docID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	versionID := *doc.CurrentContentVersionID

	spec := sqlcgen.UpsertThumbnailParams{
		ContentVersionID: versionID,
		Page:             1,
		Width:            320,
		Height:           240,
		Format:           "webp",
		ObjectKey:        "thumb/aaaa/1/320x240.webp",
		SizeBytes:        4096,
	}
	first, err := q.UpsertThumbnail(ctx, spec)
	if err != nil {
		t.Fatalf("upsert thumbnail: %v", err)
	}
	spec.SizeBytes = 8192
	second, err := q.UpsertThumbnail(ctx, spec)
	if err != nil {
		t.Fatalf("re-upsert thumbnail: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("re-upsert created a second row: %d then %d", first.ID, second.ID)
	}
	if second.SizeBytes != 8192 {
		t.Errorf("re-upsert kept size %d", second.SizeBytes)
	}

	got, err := q.GetThumbnail(ctx, sqlcgen.GetThumbnailParams{
		ContentVersionID: versionID, Page: 1, Width: 320, Height: 240, Format: "webp",
	})
	if err != nil {
		t.Fatalf("get thumbnail: %v", err)
	}
	if got.ObjectKey != spec.ObjectKey {
		t.Errorf("get returned key %q", got.ObjectKey)
	}
	if err := q.TouchThumbnail(ctx, got.ID); err != nil {
		t.Fatalf("touch: %v", err)
	}

	total, err := q.TotalThumbnailBytes(ctx)
	if err != nil {
		t.Fatalf("total bytes: %v", err)
	}
	if total != 8192 {
		t.Errorf("total bytes %d, want 8192", total)
	}
}

func TestThumbnailEviction(t *testing.T) {
	_, q := migrated(t)
	ctx := t.Context()
	_, byPath := seedCorpus(t, q)

	versions := map[string]int64{}
	for _, p := range []string{
		"/srv/docs/sekkei.txt", "/srv/docs/daichou.txt", "/srv/docs/gijiroku.txt",
	} {
		doc, err := q.GetDocument(ctx, byPath[p])
		if err != nil {
			t.Fatalf("get document %s: %v", p, err)
		}
		versions[p] = *doc.CurrentContentVersionID
	}

	// Inserted oldest-access first, so with a 2500 byte quota over 3x1000 the
	// least recently accessed row is the only one that overflows.
	order := []string{"/srv/docs/sekkei.txt", "/srv/docs/daichou.txt", "/srv/docs/gijiroku.txt"}
	for _, p := range order {
		if _, err := q.UpsertThumbnail(ctx, sqlcgen.UpsertThumbnailParams{
			ContentVersionID: versions[p],
			Page:             1,
			Width:            320,
			Height:           240,
			Format:           "webp",
			ObjectKey:        p + ".webp",
			SizeBytes:        1000,
		}); err != nil {
			t.Fatalf("seed thumbnail %s: %v", p, err)
		}
	}

	overflow, err := q.ThumbnailEvictionCandidatesByQuota(ctx, 2500)
	if err != nil {
		t.Fatalf("quota candidates: %v", err)
	}
	if len(overflow) != 1 {
		t.Fatalf("quota candidates returned %d rows, want 1", len(overflow))
	}
	if overflow[0].ObjectKey != "/srv/docs/sekkei.txt.webp" {
		t.Errorf("quota evicted %q, want the least recently accessed",
			overflow[0].ObjectKey)
	}

	aged, err := q.ThumbnailEvictionCandidatesByAge(ctx,
		sqlcgen.ThumbnailEvictionCandidatesByAgeParams{
			OlderThan:   timestamptz(time.Now().Add(time.Hour)),
			ResultLimit: 10,
		})
	if err != nil {
		t.Fatalf("age candidates: %v", err)
	}
	if len(aged) != 3 {
		t.Errorf("age candidates returned %d rows, want 3", len(aged))
	}

	marked, err := q.MarkThumbnailsPendingDelete(ctx, []int64{overflow[0].ID})
	if err != nil {
		t.Fatalf("withdraw thumbnails: %v", err)
	}
	if marked != 1 {
		t.Errorf("withdrew %d rows, want 1", marked)
	}

	// A withdrawn row is out of every read and out of both candidate queries: it
	// is the drain's now, and offering it again would evict it twice.
	if _, err := q.GetThumbnail(ctx, sqlcgen.GetThumbnailParams{
		ContentVersionID: versions[order[0]],
		Page:             1, Width: 320, Height: 240, Format: "webp",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("get on a withdrawn row = %v, want no rows", err)
	}
	stillAged, err := q.ThumbnailEvictionCandidatesByAge(ctx,
		sqlcgen.ThumbnailEvictionCandidatesByAgeParams{
			OlderThan:   timestamptz(time.Now().Add(time.Hour)),
			ResultLimit: 10,
		})
	if err != nil {
		t.Fatalf("age candidates: %v", err)
	}
	if len(stillAged) != 2 {
		t.Errorf("age candidates returned %d rows, want the 2 not withdrawn", len(stillAged))
	}

	pending, err := q.PendingDeleteThumbnails(ctx, 10)
	if err != nil {
		t.Fatalf("pending deletes: %v", err)
	}
	if len(pending) != 1 || pending[0].ObjectKey != overflow[0].ObjectKey {
		t.Fatalf("pending deletes = %v, want the withdrawn row", pending)
	}

	purged, err := q.PurgeThumbnails(ctx, []int64{pending[0].ID})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if purged != 1 {
		t.Errorf("purged %d rows, want 1", purged)
	}
}

// The regression the pending mark exists for: a job that commits its accounting
// change and then fails to remove one object must find that object again on its
// next attempt, rather than a delete that matches nothing and reports success.
func TestWithdrawnThumbnailsSurviveUntilTheirObjectsAreGone(t *testing.T) {
	_, q := migrated(t)
	ctx := t.Context()
	_, byPath := seedCorpus(t, q)

	doc, err := q.GetDocument(ctx, byPath["/srv/docs/sekkei.txt"])
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	spec := sqlcgen.UpsertThumbnailParams{
		ContentVersionID: *doc.CurrentContentVersionID,
		Page:             1, Width: 320, Height: 240, Format: "webp",
		ObjectKey: "thumb/stuck.webp", SizeBytes: 512,
	}
	row, err := q.UpsertThumbnail(ctx, spec)
	if err != nil {
		t.Fatalf("seed thumbnail: %v", err)
	}
	if _, err := q.MarkThumbnailsPendingDelete(ctx, []int64{row.ID}); err != nil {
		t.Fatalf("withdraw: %v", err)
	}

	// The attempt that could not reach the gateway purges nothing, so the row is
	// still there for the next one to derive its work from.
	again, err := q.PendingDeleteThumbnails(ctx, 10)
	if err != nil {
		t.Fatalf("pending deletes: %v", err)
	}
	if len(again) != 1 || again[0].ObjectKey != "thumb/stuck.webp" {
		t.Fatalf("pending deletes = %v, want the object still outstanding", again)
	}

	// A re-render republishes the entry at the same key. Clearing the mark is
	// what stops a drain still holding the old id from purging a live row.
	republished, err := q.UpsertThumbnail(ctx, spec)
	if err != nil {
		t.Fatalf("republish: %v", err)
	}
	if republished.ID != row.ID {
		t.Fatalf("republish created a second row: %d then %d", row.ID, republished.ID)
	}
	if republished.PendingDeleteAt.Valid {
		t.Error("republishing left the withdrawal mark in place")
	}
	purged, err := q.PurgeThumbnails(ctx, []int64{row.ID})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if purged != 0 {
		t.Errorf("purged %d republished rows, want 0", purged)
	}
	if _, err := q.GetThumbnail(ctx, sqlcgen.GetThumbnailParams{
		ContentVersionID: spec.ContentVersionID,
		Page:             1, Width: 320, Height: 240, Format: "webp",
	}); err != nil {
		t.Errorf("republished entry is not readable again: %v", err)
	}
}

// The orphan sweep's question, asked one listing page at a time: a key the
// answer omits is an object no row can reach. A withdrawn row still counts as a
// reference, because the drain owns that object.
func TestReferencedThumbnailObjectKeysSeparatesOrphansFromOwnedObjects(t *testing.T) {
	_, q := migrated(t)
	ctx := t.Context()
	_, byPath := seedCorpus(t, q)

	doc, err := q.GetDocument(ctx, byPath["/srv/docs/sekkei.txt"])
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	live, err := q.UpsertThumbnail(ctx, sqlcgen.UpsertThumbnailParams{
		ContentVersionID: *doc.CurrentContentVersionID,
		Page:             1, Width: 320, Height: 240, Format: "webp",
		ObjectKey: "thumb/live.webp", SizeBytes: 10,
	})
	if err != nil {
		t.Fatalf("seed live thumbnail: %v", err)
	}
	withdrawn, err := q.UpsertThumbnail(ctx, sqlcgen.UpsertThumbnailParams{
		ContentVersionID: *doc.CurrentContentVersionID,
		Page:             2, Width: 320, Height: 240, Format: "webp",
		ObjectKey: "thumb/withdrawn.webp", SizeBytes: 10,
	})
	if err != nil {
		t.Fatalf("seed withdrawn thumbnail: %v", err)
	}
	if _, err := q.MarkThumbnailsPendingDelete(ctx, []int64{withdrawn.ID}); err != nil {
		t.Fatalf("withdraw: %v", err)
	}

	referenced, err := q.ReferencedThumbnailObjectKeys(ctx, []string{
		live.ObjectKey, withdrawn.ObjectKey, "thumb/orphan.webp",
	})
	if err != nil {
		t.Fatalf("referenced keys: %v", err)
	}
	slices.Sort(referenced)
	want := []string{"thumb/live.webp", "thumb/withdrawn.webp"}
	if !slices.Equal(referenced, want) {
		t.Errorf("referenced = %v, want %v", referenced, want)
	}
}

// The compare-and-swap that keeps two ingest jobs for one path from invalidating
// each other's version. Both read, both hash outside any transaction, and the
// loser must be told rather than allowed to overwrite.
func TestSetDocumentCurrentVersionRefusesAStaleExpectation(t *testing.T) {
	_, q := migrated(t)
	ctx := t.Context()
	_, byPath := seedCorpus(t, q)

	docID := byPath["/srv/docs/sekkei.txt"]
	doc, err := q.GetDocument(ctx, docID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	original := *doc.CurrentContentVersionID

	winner, err := q.UpsertContentVersion(ctx, sqlcgen.UpsertContentVersionParams{
		ContentHash: "cas-winner", SizeBytes: 1,
	})
	if err != nil {
		t.Fatalf("winner version: %v", err)
	}
	if _, err := q.SetDocumentCurrentVersion(ctx, sqlcgen.SetDocumentCurrentVersionParams{
		ID:                       docID,
		CurrentContentVersionID:  &winner.ID,
		ExpectedContentVersionID: &original,
	}); err != nil {
		t.Fatalf("winner swap: %v", err)
	}

	loser, err := q.UpsertContentVersion(ctx, sqlcgen.UpsertContentVersionParams{
		ContentHash: "cas-loser", SizeBytes: 1,
	})
	if err != nil {
		t.Fatalf("loser version: %v", err)
	}
	_, err = q.SetDocumentCurrentVersion(ctx, sqlcgen.SetDocumentCurrentVersionParams{
		ID:                       docID,
		CurrentContentVersionID:  &loser.ID,
		ExpectedContentVersionID: &original,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale swap = %v, want no rows", err)
	}

	after, err := q.GetDocument(ctx, docID)
	if err != nil {
		t.Fatalf("re-read document: %v", err)
	}
	if *after.CurrentContentVersionID != winner.ID {
		t.Errorf("document is at version %d, want the winner's %d",
			*after.CurrentContentVersionID, winner.ID)
	}
}

func TestStaleThumbnailInvalidation(t *testing.T) {
	_, q := migrated(t)
	ctx := t.Context()
	mountpointID, byPath := seedCorpus(t, q)

	docID := byPath["/srv/docs/sekkei.txt"]
	doc, err := q.GetDocument(ctx, docID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	oldVersion := *doc.CurrentContentVersionID

	if _, err := q.UpsertThumbnail(ctx, sqlcgen.UpsertThumbnailParams{
		ContentVersionID: oldVersion,
		Page:             1, Width: 320, Height: 240, Format: "webp",
		ObjectKey: "thumb/old.webp", SizeBytes: 2048,
	}); err != nil {
		t.Fatalf("seed thumbnail: %v", err)
	}

	newCV, err := q.UpsertContentVersion(ctx, sqlcgen.UpsertContentVersionParams{
		ContentHash: "aaaa-v2",
		SizeBytes:   99,
	})
	if err != nil {
		t.Fatalf("new content version: %v", err)
	}
	if _, err := q.SetDocumentCurrentVersion(ctx, sqlcgen.SetDocumentCurrentVersionParams{
		ID:                       docID,
		CurrentContentVersionID:  &newCV.ID,
		ExpectedContentVersionID: &oldVersion,
	}); err != nil {
		t.Fatalf("advance current version: %v", err)
	}

	invalidated, err := q.MarkThumbnailsPendingDeleteForStaleContentVersion(ctx, oldVersion)
	if err != nil {
		t.Fatalf("targeted invalidation: %v", err)
	}
	if invalidated != 1 {
		t.Fatalf("targeted invalidation withdrew %d rows, want 1", invalidated)
	}
	// Withdrawn is enough to stop it being served, which is what a changed
	// document needs before its object has gone anywhere.
	if _, err := q.GetThumbnail(ctx, sqlcgen.GetThumbnailParams{
		ContentVersionID: oldVersion,
		Page:             1, Width: 320, Height: 240, Format: "webp",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("a withdrawn thumbnail is still readable: %v", err)
	}

	// Now the sweep form: a tombstoned document's thumbnails must also go.
	other, err := q.GetDocument(ctx, byPath["/srv/docs/daichou.txt"])
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	if _, err := q.UpsertThumbnail(ctx, sqlcgen.UpsertThumbnailParams{
		ContentVersionID: *other.CurrentContentVersionID,
		Page:             1, Width: 320, Height: 240, Format: "webp",
		ObjectKey: "thumb/other.webp", SizeBytes: 1024,
	}); err != nil {
		t.Fatalf("seed thumbnail: %v", err)
	}
	if _, err := q.TombstoneDocumentByPath(ctx, sqlcgen.TombstoneDocumentByPathParams{
		MountpointID: mountpointID,
		Path:         "/srv/docs/daichou.txt",
	}); err != nil {
		t.Fatalf("tombstone: %v", err)
	}

	// The already-withdrawn row is not counted again: COALESCE keeps its
	// original timestamp and the guard keeps it out of the result.
	swept, err := q.MarkStaleThumbnailsPendingDelete(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 1 {
		t.Fatalf("sweep withdrew %d rows, want only the tombstoned document's 1", swept)
	}

	pending, err := q.PendingDeleteThumbnails(ctx, 10)
	if err != nil {
		t.Fatalf("pending deletes: %v", err)
	}
	keys := make([]string, 0, len(pending))
	for _, row := range pending {
		keys = append(keys, row.ObjectKey)
	}
	slices.Sort(keys)
	if want := []string{"thumb/old.webp", "thumb/other.webp"}; !slices.Equal(keys, want) {
		t.Fatalf("outstanding objects = %v, want %v", keys, want)
	}

	ids := make([]int64, 0, len(pending))
	for _, row := range pending {
		ids = append(ids, row.ID)
	}
	if _, err := q.PurgeThumbnails(ctx, ids); err != nil {
		t.Fatalf("purge: %v", err)
	}
	total, err := q.TotalThumbnailBytes(ctx)
	if err != nil {
		t.Fatalf("total bytes: %v", err)
	}
	if total != 0 {
		t.Errorf("accounting still holds %d bytes", total)
	}
}

func containsPath(rows []sqlcgen.SearchDocumentsRow, path string) bool {
	for _, r := range rows {
		if r.Path == path {
			return true
		}
	}
	return false
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
