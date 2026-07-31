package jobs_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/ngicks/hirame/apps/search-api/internal/jobs"
	"github.com/ngicks/hirame/apps/search-api/internal/objstore"
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
)

// fakeObjects is the bucket: it records deletions, can be made to fail on one
// key, and enumerates what it still holds.
type fakeObjects struct {
	stored  map[string]time.Time
	deleted []string
	failOn  string
}

func newFakeObjects(keys ...string) *fakeObjects {
	o := &fakeObjects{stored: map[string]time.Time{}}
	for _, key := range keys {
		// Old enough that the orphan grace period never hides a fixture.
		o.stored[key] = time.Now().Add(-24 * time.Hour)
	}
	return o
}

func (o *fakeObjects) Delete(_ context.Context, key string) error {
	if key == o.failOn {
		return errors.New("gateway unavailable")
	}
	delete(o.stored, key)
	o.deleted = append(o.deleted, key)
	return nil
}

func (o *fakeObjects) List(
	ctx context.Context,
	_ string,
	fn func(context.Context, []objstore.Object) error,
) error {
	page := make([]objstore.Object, 0, len(o.stored))
	for key, modified := range o.stored {
		page = append(page, objstore.Object{Key: key, LastModified: modified})
	}
	slices.SortFunc(page, func(a, b objstore.Object) int {
		return len(a.Key) - len(b.Key)
	})
	return fn(ctx, page)
}

// cacheRow is one thumbnail_cache row, including the half of it that two-phase
// removal turns on: pending is the withdrawal mark, stale says no live
// document's current version claims the row.
type cacheRow struct {
	id        int64
	versionID int64
	objectKey string
	size      int64
	pending   bool
	stale     bool
}

// fakeCacheStore models thumbnail_cache closely enough that the mark/drain/purge
// ordering is what the tests actually exercise: a row survives its object's
// failed delete, and a purge only removes rows still carrying the mark.
type fakeCacheStore struct {
	rows []*cacheRow

	// The ids each eviction pass offers, consumed on the first call so a
	// batching loop terminates.
	byAge   []int64
	byQuota []int64

	ageCutoff    pgtype.Timestamptz
	quotaMaximum int64
	total        int64
	purged       []int64
}

func (s *fakeCacheStore) find(id int64) *cacheRow {
	for _, row := range s.rows {
		if row.id == id {
			return row
		}
	}
	return nil
}

func (s *fakeCacheStore) pendingIDs() []int64 {
	var out []int64
	for _, row := range s.rows {
		if row.pending {
			out = append(out, row.id)
		}
	}
	return out
}

func (s *fakeCacheStore) MarkThumbnailsPendingDeleteForStaleContentVersion(
	_ context.Context,
	contentVersionID int64,
) (int64, error) {
	var marked int64
	for _, row := range s.rows {
		if row.versionID == contentVersionID && row.stale && !row.pending {
			row.pending = true
			marked++
		}
	}
	return marked, nil
}

func (s *fakeCacheStore) MarkStaleThumbnailsPendingDelete(context.Context) (int64, error) {
	var marked int64
	for _, row := range s.rows {
		if row.stale && !row.pending {
			row.pending = true
			marked++
		}
	}
	return marked, nil
}

func (s *fakeCacheStore) MarkThumbnailsPendingDelete(
	_ context.Context,
	ids []int64,
) (int64, error) {
	var marked int64
	for _, id := range ids {
		if row := s.find(id); row != nil && !row.pending {
			row.pending = true
			marked++
		}
	}
	return marked, nil
}

func (s *fakeCacheStore) PendingDeleteThumbnails(
	_ context.Context,
	resultLimit int32,
) ([]sqlcgen.PendingDeleteThumbnailsRow, error) {
	var out []sqlcgen.PendingDeleteThumbnailsRow
	for _, row := range s.rows {
		if !row.pending {
			continue
		}
		if len(out) == int(resultLimit) {
			break
		}
		out = append(out, sqlcgen.PendingDeleteThumbnailsRow{
			ID: row.id, ObjectKey: row.objectKey, SizeBytes: row.size,
		})
	}
	return out, nil
}

func (s *fakeCacheStore) PurgeThumbnails(_ context.Context, ids []int64) (int64, error) {
	var purged int64
	s.rows = slices.DeleteFunc(s.rows, func(row *cacheRow) bool {
		if !slices.Contains(ids, row.id) || !row.pending {
			return false
		}
		s.purged = append(s.purged, row.id)
		purged++
		return true
	})
	return purged, nil
}

func (s *fakeCacheStore) ThumbnailEvictionCandidatesByAge(
	_ context.Context,
	arg sqlcgen.ThumbnailEvictionCandidatesByAgeParams,
) ([]sqlcgen.ThumbnailEvictionCandidatesByAgeRow, error) {
	s.ageCutoff = arg.OlderThan
	ids := s.byAge
	s.byAge = nil

	var out []sqlcgen.ThumbnailEvictionCandidatesByAgeRow
	for _, id := range ids {
		if row := s.find(id); row != nil && !row.pending {
			out = append(out, sqlcgen.ThumbnailEvictionCandidatesByAgeRow{
				ID: row.id, ObjectKey: row.objectKey, SizeBytes: row.size,
			})
		}
	}
	return out, nil
}

func (s *fakeCacheStore) ThumbnailEvictionCandidatesByQuota(
	_ context.Context,
	maxBytes int64,
) ([]sqlcgen.ThumbnailEvictionCandidatesByQuotaRow, error) {
	s.quotaMaximum = maxBytes
	ids := s.byQuota
	s.byQuota = nil

	var out []sqlcgen.ThumbnailEvictionCandidatesByQuotaRow
	for _, id := range ids {
		if row := s.find(id); row != nil && !row.pending {
			out = append(out, sqlcgen.ThumbnailEvictionCandidatesByQuotaRow{
				ID: row.id, ObjectKey: row.objectKey, SizeBytes: row.size,
			})
		}
	}
	return out, nil
}

func (s *fakeCacheStore) ReferencedThumbnailObjectKeys(
	_ context.Context,
	objectKeys []string,
) ([]string, error) {
	var out []string
	for _, row := range s.rows {
		if slices.Contains(objectKeys, row.objectKey) {
			out = append(out, row.objectKey)
		}
	}
	return out, nil
}

func (s *fakeCacheStore) TotalThumbnailBytes(context.Context) (int64, error) {
	return s.total, nil
}

func invalidateJob() *river.Job[jobs.InvalidateThumbnailsArgs] {
	return &river.Job[jobs.InvalidateThumbnailsArgs]{
		Args: jobs.InvalidateThumbnailsArgs{
			DocumentID:                 1,
			SupersededContentVersionID: 7,
		},
	}
}

// staleCache is the state the ingestion path leaves behind: two thumbnails of a
// content version the document has just stopped pointing at.
func staleCache() *fakeCacheStore {
	return &fakeCacheStore{
		rows: []*cacheRow{
			{id: 1, versionID: 7, objectKey: "thumb/7/1.webp", size: 100, stale: true},
			{id: 2, versionID: 7, objectKey: "thumb/7/2.webp", size: 200, stale: true},
		},
	}
}

func TestInvalidationDropsTheAccountingRowsAndTheObjectsBehindThem(t *testing.T) {
	store := staleCache()
	objects := newFakeObjects("thumb/7/1.webp", "thumb/7/2.webp")

	worker := jobs.NewInvalidateThumbnailsWorker(store, objects, discardLogger())
	if err := worker.Work(t.Context(), invalidateJob()); err != nil {
		t.Fatalf("work: %v", err)
	}

	want := []string{"thumb/7/1.webp", "thumb/7/2.webp"}
	if !slices.Equal(objects.deleted, want) {
		t.Errorf("deleted %v, want %v", objects.deleted, want)
	}
	if len(store.rows) != 0 {
		t.Errorf("%d accounting rows survived a completed invalidation", len(store.rows))
	}
}

// A retry after everything is already gone must not fail: once the rows are
// purged there is nothing left to re-derive.
func TestRepeatingAnInvalidationIsHarmless(t *testing.T) {
	store := &fakeCacheStore{
		rows: []*cacheRow{
			{id: 1, versionID: 7, objectKey: "thumb/7/1.webp", size: 100, stale: true},
		},
	}
	objects := newFakeObjects("thumb/7/1.webp")
	worker := jobs.NewInvalidateThumbnailsWorker(store, objects, discardLogger())

	if err := worker.Work(t.Context(), invalidateJob()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := worker.Work(t.Context(), invalidateJob()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(objects.deleted) != 1 {
		t.Errorf("deleted %v, want the object removed exactly once", objects.deleted)
	}
}

// An object that could not be removed leaks bytes, so the job has to come back.
func TestAFailedObjectDeleteFailsTheJob(t *testing.T) {
	store := staleCache()
	objects := newFakeObjects("thumb/7/1.webp", "thumb/7/2.webp")
	objects.failOn = "thumb/7/1.webp"

	worker := jobs.NewInvalidateThumbnailsWorker(store, objects, discardLogger())
	err := worker.Work(t.Context(), invalidateJob())
	if err == nil {
		t.Fatal("a failed object delete was swallowed")
	}
	// The rest of the batch is still attempted rather than abandoned at the
	// first failure.
	if !slices.Contains(objects.deleted, "thumb/7/2.webp") {
		t.Error("the batch stopped at the first failure")
	}
}

// The regression this whole two-phase scheme exists for: the first attempt
// commits its accounting change and then fails on one object, and the retry has
// to re-derive that object rather than find an empty result set and report
// success.
func TestARetryReattemptsTheObjectTheFirstAttemptCouldNotDelete(t *testing.T) {
	store := staleCache()
	objects := newFakeObjects("thumb/7/1.webp", "thumb/7/2.webp")
	objects.failOn = "thumb/7/1.webp"

	worker := jobs.NewInvalidateThumbnailsWorker(store, objects, discardLogger())
	if err := worker.Work(t.Context(), invalidateJob()); err == nil {
		t.Fatal("the first attempt reported success despite a failed delete")
	}
	// The row whose object is still there must survive, withdrawn rather than
	// deleted; the one that succeeded must be purged.
	if got := store.pendingIDs(); !slices.Equal(got, []int64{1}) {
		t.Fatalf("outstanding rows = %v, want [1]", got)
	}

	objects.failOn = ""
	if err := worker.Work(t.Context(), invalidateJob()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !slices.Contains(objects.deleted, "thumb/7/1.webp") {
		t.Errorf("deleted %v, want the retry to re-attempt thumb/7/1.webp", objects.deleted)
	}
	if len(store.rows) != 0 {
		t.Errorf("%d accounting rows survived the retry", len(store.rows))
	}
}

// A withdrawn row must not be served while its object is still being removed,
// which is only true if withdrawal is what a read sees. The store's GetThumbnail
// carries that guard; here the invariant checked is the one the drain relies on:
// a row republished by a re-render loses the mark and is no longer purged.
func TestARepublishedEntrySurvivesTheDrain(t *testing.T) {
	store := staleCache()
	objects := newFakeObjects("thumb/7/1.webp", "thumb/7/2.webp")
	objects.failOn = "thumb/7/1.webp"

	worker := jobs.NewInvalidateThumbnailsWorker(store, objects, discardLogger())
	if err := worker.Work(t.Context(), invalidateJob()); err == nil {
		t.Fatal("the first attempt reported success despite a failed delete")
	}

	// What UpsertThumbnail does on conflict: the entry is live again.
	republished := store.find(1)
	if republished == nil {
		t.Fatal("the row was purged while its object was still there")
	}
	republished.pending = false
	republished.stale = false

	objects.failOn = ""
	if err := worker.Work(t.Context(), invalidateJob()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if store.find(1) == nil {
		t.Error("the drain purged an entry a re-render had republished")
	}
}

func evictionJob() *river.Job[jobs.EvictionMaintenanceArgs] {
	return &river.Job[jobs.EvictionMaintenanceArgs]{Args: jobs.EvictionMaintenanceArgs{}}
}

func TestMaintenanceEvictsByAgeAndThenByQuota(t *testing.T) {
	store := &fakeCacheStore{
		rows: []*cacheRow{
			{id: 1, objectKey: "thumb/a.webp", size: 100},
			{id: 2, objectKey: "thumb/b.webp", size: 500},
		},
		byAge:   []int64{1},
		byQuota: []int64{2},
		total:   42,
	}
	objects := newFakeObjects("thumb/a.webp", "thumb/b.webp")

	worker := jobs.NewEvictionMaintenanceWorker(
		store, objects, 24*time.Hour, 1_000, discardLogger(),
	)
	if err := worker.Work(t.Context(), evictionJob()); err != nil {
		t.Fatalf("work: %v", err)
	}

	want := []string{"thumb/a.webp", "thumb/b.webp"}
	if !slices.Equal(objects.deleted, want) {
		t.Errorf("deleted %v, want %v", objects.deleted, want)
	}
	if !slices.Equal(store.purged, []int64{1, 2}) {
		t.Errorf("purged rows %v, want [1 2]", store.purged)
	}
	if store.quotaMaximum != 1_000 {
		t.Errorf("quota = %d, want the configured 1000", store.quotaMaximum)
	}
	if !store.ageCutoff.Valid || !store.ageCutoff.Time.Before(time.Now()) {
		t.Errorf("age cutoff = %v, want a moment in the past", store.ageCutoff)
	}
}

func TestAZeroLimitDisablesThatEvictionPass(t *testing.T) {
	store := &fakeCacheStore{
		rows: []*cacheRow{
			{id: 1, objectKey: "thumb/a.webp"},
			{id: 2, objectKey: "thumb/b.webp"},
		},
		byAge:   []int64{1},
		byQuota: []int64{2},
	}
	objects := newFakeObjects("thumb/a.webp", "thumb/b.webp")

	worker := jobs.NewEvictionMaintenanceWorker(store, objects, 0, 0, discardLogger())
	if err := worker.Work(t.Context(), evictionJob()); err != nil {
		t.Fatalf("work: %v", err)
	}
	if len(objects.deleted) != 0 {
		t.Errorf("deleted %v with both limits disabled", objects.deleted)
	}
}

func sweepJob() *river.Job[jobs.CacheSweepArgs] {
	return &river.Job[jobs.CacheSweepArgs]{Args: jobs.CacheSweepArgs{}}
}

// The backstop behind a discarded invalidation: a row nothing live claims is
// withdrawn and drained without any job naming its content version.
func TestTheSweepDropsRowsNoLiveDocumentClaims(t *testing.T) {
	store := &fakeCacheStore{
		rows: []*cacheRow{
			{id: 1, versionID: 7, objectKey: "thumb/7/1.webp", size: 100, stale: true},
			{id: 2, versionID: 9, objectKey: "thumb/9/1.webp", size: 200},
		},
	}
	objects := newFakeObjects("thumb/7/1.webp", "thumb/9/1.webp")

	worker := jobs.NewCacheSweepWorker(store, objects, discardLogger())
	if err := worker.Work(t.Context(), sweepJob()); err != nil {
		t.Fatalf("work: %v", err)
	}

	if !slices.Equal(objects.deleted, []string{"thumb/7/1.webp"}) {
		t.Errorf("deleted %v, want only the stale version's object", objects.deleted)
	}
	if store.find(2) == nil {
		t.Error("the sweep dropped a row a live document still claims")
	}
}

// The backstop behind a render that stored its object but could not record the
// row: nothing can ever reach those bytes, so the sweep collects them.
func TestTheSweepRemovesObjectsNoAccountingRowReaches(t *testing.T) {
	store := &fakeCacheStore{
		rows: []*cacheRow{
			{id: 1, versionID: 9, objectKey: "thumb/9/1.webp", size: 200},
		},
	}
	objects := newFakeObjects("thumb/9/1.webp", "thumb/orphan.webp")

	worker := jobs.NewCacheSweepWorker(store, objects, discardLogger())
	if err := worker.Work(t.Context(), sweepJob()); err != nil {
		t.Fatalf("work: %v", err)
	}

	if !slices.Equal(objects.deleted, []string{"thumb/orphan.webp"}) {
		t.Errorf("deleted %v, want only the orphan", objects.deleted)
	}
}

// A render publishes its object before it writes the row, so an object younger
// than the grace period is legitimately unreferenced and must be left alone.
func TestTheSweepLeavesARecentlyStoredObjectAlone(t *testing.T) {
	store := &fakeCacheStore{}
	objects := newFakeObjects()
	objects.stored["thumb/just-written.webp"] = time.Now()

	worker := jobs.NewCacheSweepWorker(store, objects, discardLogger())
	if err := worker.Work(t.Context(), sweepJob()); err != nil {
		t.Fatalf("work: %v", err)
	}
	if len(objects.deleted) != 0 {
		t.Errorf("deleted %v, want an object inside the grace period kept", objects.deleted)
	}
}

// An object still owned by the drain is referenced by its withdrawn row, so the
// orphan pass must not race the drain for it.
func TestTheSweepDoesNotCollectAnObjectTheDrainStillOwns(t *testing.T) {
	store := &fakeCacheStore{
		rows: []*cacheRow{
			{id: 1, versionID: 7, objectKey: "thumb/7/1.webp", size: 100, stale: true},
		},
	}
	objects := newFakeObjects("thumb/7/1.webp")
	objects.failOn = "thumb/7/1.webp"

	worker := jobs.NewCacheSweepWorker(store, objects, discardLogger())
	if err := worker.Work(t.Context(), sweepJob()); err == nil {
		t.Fatal("a failed object delete was swallowed")
	}
	if store.find(1) == nil {
		t.Error("the withdrawn row was purged even though its object is still there")
	}
}
