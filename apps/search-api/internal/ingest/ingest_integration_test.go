//go:build integration

// These tests need a live ParadeDB PostgreSQL. They are behind the
// `integration` build tag and additionally require HIRAME_TEST_DATABASE_URL, so
// `go test ./...` stays green on a machine with no database.
//
//	go test -tags integration ./internal/ingest/...
package ingest_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	overwatchv1 "github.com/ngicks/go-overwatch/overwatch/pkg/api/gen/proto/go/overwatch/v1"

	"github.com/ngicks/hirame/apps/search-api/internal/doctype"
	"github.com/ngicks/hirame/apps/search-api/internal/ingest"
	"github.com/ngicks/hirame/apps/search-api/internal/jobs"
	"github.com/ngicks/hirame/apps/search-api/internal/store"
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
)

const dsnEnv = "HIRAME_TEST_DATABASE_URL"

// testDatabaseLockKey serializes every integration package that shares one
// database. Each of them truncates the whole schema to get a clean fixture, and
// `go test ./...` runs packages in parallel, so without this two suites wipe
// each other's rows and deadlock against each other's TRUNCATE. The value is
// arbitrary but must match in every package that takes it; internal/store takes
// the same one.
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

// pipeline is a real ingestion pipeline over a real database: the only fake is
// the daemon, which needs CAP_SYS_ADMIN and a filesystem this test cannot mark.
type pipeline struct {
	pool       *pgxpool.Pool
	queries    *sqlcgen.Queries
	store      *ingest.PGStore
	processor  *ingest.Processor
	reconciler *ingest.Reconciler
	mountpoint ingest.Mountpoint
	root       string
}

func newPipeline(t *testing.T) *pipeline {
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

	migrator, err := store.NewMigrator(pool, discardLogger())
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
	if _, err := pool.Exec(t.Context(), `DELETE FROM river_job`); err != nil {
		t.Fatalf("clear river_job: %v", err)
	}
	if _, err := pool.Exec(
		t.Context(), `UPDATE reconcile_state SET watermark = 0 WHERE id`,
	); err != nil {
		t.Fatalf("reset watermark: %v", err)
	}

	// An insert-only client: this test drives the workers directly, so nothing
	// should race it by picking a job up.
	riverClient, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("new river client: %v", err)
	}
	enqueuer := jobs.NewEnqueuer(time.Second)
	enqueuer.Bind(riverClient)

	queries := sqlcgen.New(pool)
	root := t.TempDir()
	row, err := queries.UpsertMountpoint(t.Context(), sqlcgen.UpsertMountpointParams{
		RootPath: root,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("register mountpoint: %v", err)
	}
	mountpoint := ingest.Mountpoint{ID: row.ID, Root: row.RootPath}

	pgStore := ingest.NewPGStore(pool, enqueuer)
	filter := doctype.NewFilter(nil)
	return &pipeline{
		pool:      pool,
		queries:   queries,
		store:     pgStore,
		processor: ingest.NewProcessor(pgStore, filter, ingest.OSOpener{}, discardLogger()),
		reconciler: ingest.New(
			&fakeWatcher{}, pgStore, filter,
			[]ingest.Mountpoint{mountpoint}, discardLogger(), time.Millisecond,
		),
		mountpoint: mountpoint,
		root:       root,
	}
}

// jobArgs returns the encoded args of every queued job of a kind, newest last.
func (p *pipeline) jobArgs(t *testing.T, kind string) []string {
	t.Helper()
	rows, err := p.pool.Query(t.Context(),
		`SELECT args::text FROM river_job WHERE kind = $1 ORDER BY id`, kind)
	if err != nil {
		t.Fatalf("query river_job: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var args string
		if err := rows.Scan(&args); err != nil {
			t.Fatalf("scan river_job: %v", err)
		}
		out = append(out, args)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read river_job: %v", err)
	}
	return out
}

func (p *pipeline) write(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(p.root, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestEventBecomesADocumentAndQueuedWork walks the whole path the task
// describes: a filesystem event reaches the database, the ingest job it queued
// produces a document row, and the version flip queues extraction in the same
// transaction.
func TestEventBecomesADocumentAndQueuedWork(t *testing.T) {
	p := newPipeline(t)
	path := p.write(t, "report.pdf", "%PDF-1.4 first revision")

	err := p.reconciler.ApplyEvent(t.Context(), &overwatchv1.FileEvent{
		Seq:  1,
		Kind: overwatchv1.EventKind_EVENT_KIND_CLOSE_WRITE,
		Path: path,
		Stat: stat(23, 4242),
	})
	if err != nil {
		t.Fatalf("apply event: %v", err)
	}

	obs, err := p.queries.GetObservation(t.Context(), sqlcgen.GetObservationParams{
		MountpointID: p.mountpoint.ID,
		Path:         path,
	})
	if err != nil {
		t.Fatalf("observation was not recorded: %v", err)
	}
	if obs.Ino != 4242 {
		t.Errorf("observation ino = %d, want 4242", obs.Ino)
	}
	if got := p.jobArgs(t, "ingest_path"); len(got) != 1 {
		t.Fatalf("ingest_path jobs = %v, want one", got)
	}
	if wm, err := p.queries.GetWatermark(t.Context()); err != nil || wm != 1 {
		t.Errorf("watermark = %d (err %v), want 1", wm, err)
	}

	// The job the event queued.
	if err := p.processor.ProcessPath(t.Context(), p.mountpoint.ID, path); err != nil {
		t.Fatalf("process path: %v", err)
	}

	doc, err := p.queries.FindLiveDocumentByPath(
		t.Context(),
		sqlcgen.FindLiveDocumentByPathParams{MountpointID: p.mountpoint.ID, Path: path},
	)
	if err != nil {
		t.Fatalf("document row was not created: %v", err)
	}
	if doc.CurrentContentVersionID == nil {
		t.Fatal("document has no current content version")
	}
	firstVersion := *doc.CurrentContentVersionID

	extraction, err := p.queries.GetExtraction(t.Context(), firstVersion)
	if err != nil {
		t.Fatalf("extraction row was not claimed: %v", err)
	}
	if extraction.Status != "pending" {
		t.Errorf("extraction status = %q, want pending", extraction.Status)
	}
	if got := p.jobArgs(t, "extract"); len(got) != 1 {
		t.Fatalf("extract jobs = %v, want one", got)
	}
	if got := p.jobArgs(t, "invalidate_thumbnails"); len(got) != 0 {
		t.Fatalf("invalidate jobs = %v, want none: nothing was superseded", got)
	}

	// Re-running the same job must not produce a second version: duplicated
	// events and rescans both land here.
	if err := p.processor.ProcessPath(t.Context(), p.mountpoint.ID, path); err != nil {
		t.Fatalf("reprocess path: %v", err)
	}
	if got := p.jobArgs(t, "extract"); len(got) != 1 {
		t.Fatalf("extract jobs after a duplicate = %v, want still one", got)
	}

	// New bytes supersede the version and must invalidate its thumbnails.
	if err := os.WriteFile(path, []byte("%PDF-1.4 second revision, longer"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := p.processor.ProcessPath(t.Context(), p.mountpoint.ID, path); err != nil {
		t.Fatalf("process changed file: %v", err)
	}

	doc, err = p.queries.FindLiveDocumentByPath(
		t.Context(),
		sqlcgen.FindLiveDocumentByPathParams{MountpointID: p.mountpoint.ID, Path: path},
	)
	if err != nil {
		t.Fatalf("document disappeared: %v", err)
	}
	if *doc.CurrentContentVersionID == firstVersion {
		t.Fatal("the document still points at the superseded version")
	}
	if got := p.jobArgs(t, "extract"); len(got) != 2 {
		t.Errorf("extract jobs = %v, want two", got)
	}
	if got := p.jobArgs(t, "invalidate_thumbnails"); len(got) != 1 {
		t.Errorf("invalidate jobs = %v, want one for the superseded version", got)
	}
}

// Two distinct events for one path collapse into a single job, which is what
// keeps a burst of writes from hashing the same file over and over. This
// exercises River's own uniqueness rather than this package's reading of it.
func TestABurstOfEventsForOnePathQueuesOneJob(t *testing.T) {
	p := newPipeline(t)
	path := p.write(t, "busy.pdf", "%PDF-1.4 being written")

	for seq := uint64(1); seq <= 4; seq++ {
		err := p.reconciler.ApplyEvent(t.Context(), &overwatchv1.FileEvent{
			Seq:  seq,
			Kind: overwatchv1.EventKind_EVENT_KIND_MODIFY,
			Path: path,
			Stat: stat(uint64(seq)*10, 77),
		})
		if err != nil {
			t.Fatalf("apply event %d: %v", seq, err)
		}
	}
	if got := p.jobArgs(t, "ingest_path"); len(got) != 1 {
		t.Fatalf("ingest_path jobs = %v, want one for the whole burst", got)
	}

	// The close of the write handle is a different key on purpose, so it lands
	// even though the debounced job is still waiting to run.
	err := p.reconciler.ApplyEvent(t.Context(), &overwatchv1.FileEvent{
		Seq:  5,
		Kind: overwatchv1.EventKind_EVENT_KIND_CLOSE_WRITE,
		Path: path,
		Stat: stat(50, 77),
	})
	if err != nil {
		t.Fatalf("apply close_write: %v", err)
	}
	got := p.jobArgs(t, "ingest_path")
	if len(got) != 2 {
		t.Fatalf("ingest_path jobs = %v, want the close to have landed too", got)
	}
	if !strings.Contains(got[0], jobs.TriggerDebounced) ||
		!strings.Contains(got[1], jobs.TriggerSettled) {
		t.Errorf("jobs = %v, want one debounced and one settled", got)
	}
}

// Directory names in this system are routinely Japanese, and the subtree
// re-path is written with length()/substr(), which count characters while
// starts_with() matches the raw prefix. A disagreement between them would
// corrupt every path beneath a renamed directory.
func TestRenamingADirectoryWithAMultibyteNameRepathsItsContents(t *testing.T) {
	p := newPipeline(t)
	oldDir := filepath.Join(p.root, "文書")
	newDir := filepath.Join(p.root, "資料")
	if err := os.Mkdir(oldDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	child := filepath.Join(oldDir, "報告書.pdf")
	if err := os.WriteFile(child, []byte("%PDF-1.4 日本語"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := p.reconciler.ApplyEvent(t.Context(), &overwatchv1.FileEvent{
		Seq: 1, Kind: overwatchv1.EventKind_EVENT_KIND_CREATE, Path: child, Stat: stat(20, 11),
	}); err != nil {
		t.Fatalf("apply create: %v", err)
	}
	if err := p.processor.ProcessPath(t.Context(), p.mountpoint.ID, child); err != nil {
		t.Fatalf("process: %v", err)
	}
	before, err := p.queries.FindLiveDocumentByPath(
		t.Context(),
		sqlcgen.FindLiveDocumentByPathParams{MountpointID: p.mountpoint.ID, Path: child},
	)
	if err != nil {
		t.Fatalf("document was not created: %v", err)
	}

	if err := os.Rename(oldDir, newDir); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := p.reconciler.ApplyEvent(t.Context(), &overwatchv1.FileEvent{
		Seq:     2,
		Kind:    overwatchv1.EventKind_EVENT_KIND_MOVE,
		OldPath: oldDir,
		Path:    newDir,
		IsDir:   true,
		Stat:    stat(0, 12),
	}); err != nil {
		t.Fatalf("apply move: %v", err)
	}

	wantPath := filepath.Join(newDir, "報告書.pdf")
	after, err := p.queries.FindLiveDocumentByPath(
		t.Context(),
		sqlcgen.FindLiveDocumentByPathParams{MountpointID: p.mountpoint.ID, Path: wantPath},
	)
	if err != nil {
		var actual string
		_ = p.pool.QueryRow(t.Context(),
			`SELECT path FROM documents WHERE id = $1`, before.ID).Scan(&actual)
		t.Fatalf("no document at %q after the rename; it is at %q", wantPath, actual)
	}
	if after.ID != before.ID {
		t.Errorf("document id = %d, want the original %d", after.ID, before.ID)
	}

	obs, err := p.queries.GetObservation(t.Context(), sqlcgen.GetObservationParams{
		MountpointID: p.mountpoint.ID,
		Path:         wantPath,
	})
	if err != nil {
		t.Fatalf("observation was not re-pathed: %v", err)
	}
	if obs.Path != wantPath {
		t.Errorf("observation path = %q, want %q", obs.Path, wantPath)
	}
}

// A duplicate event must not be applied twice, which is what lets a dropped
// subscription resume by replaying from the stored watermark.
func TestReplayedEventsAreDroppedByTheWatermark(t *testing.T) {
	p := newPipeline(t)
	path := p.write(t, "memo.docx", "PK\x03\x04 body")
	ev := &overwatchv1.FileEvent{
		Seq:  7,
		Kind: overwatchv1.EventKind_EVENT_KIND_CREATE,
		Path: path,
		Stat: stat(11, 99),
	}

	for range 3 {
		if err := p.reconciler.ApplyEvent(t.Context(), ev); err != nil {
			t.Fatalf("apply event: %v", err)
		}
	}
	if got := p.jobArgs(t, "ingest_path"); len(got) != 1 {
		t.Errorf("ingest_path jobs = %v, want one", got)
	}
	if wm, _ := p.queries.GetWatermark(t.Context()); wm != 7 {
		t.Errorf("watermark = %d, want 7", wm)
	}
}

// Deleting a file must tombstone rather than remove it, so the invalidation
// lineage survives (D-007), and must queue the thumbnail cleanup.
func TestDeletingAFileTombstonesItAndQueuesInvalidation(t *testing.T) {
	p := newPipeline(t)
	path := p.write(t, "gone.pdf", "%PDF-1.4 doomed")

	if err := p.reconciler.ApplyEvent(t.Context(), &overwatchv1.FileEvent{
		Seq: 1, Kind: overwatchv1.EventKind_EVENT_KIND_CREATE, Path: path, Stat: stat(15, 5),
	}); err != nil {
		t.Fatalf("apply create: %v", err)
	}
	if err := p.processor.ProcessPath(t.Context(), p.mountpoint.ID, path); err != nil {
		t.Fatalf("process: %v", err)
	}

	if err := p.reconciler.ApplyEvent(t.Context(), &overwatchv1.FileEvent{
		Seq: 2, Kind: overwatchv1.EventKind_EVENT_KIND_DELETE, Path: path,
	}); err != nil {
		t.Fatalf("apply delete: %v", err)
	}

	_, err := p.queries.FindLiveDocumentByPath(
		t.Context(),
		sqlcgen.FindLiveDocumentByPathParams{MountpointID: p.mountpoint.ID, Path: path},
	)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("the document is still live after a delete (err %v)", err)
	}

	var tombstoned int
	if err := p.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM documents WHERE path = $1 AND deleted_at IS NOT NULL`, path,
	).Scan(&tombstoned); err != nil {
		t.Fatalf("count tombstones: %v", err)
	}
	if tombstoned != 1 {
		t.Errorf("tombstoned rows = %d, want 1: the row must survive the delete", tombstoned)
	}
	if got := p.jobArgs(t, "invalidate_thumbnails"); len(got) != 1 {
		t.Errorf("invalidate jobs = %v, want one", got)
	}
}
