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
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
)

// fakeObjects records deletions and can be made to fail.
type fakeObjects struct {
	deleted []string
	failOn  string
}

func (o *fakeObjects) Delete(_ context.Context, key string) error {
	if key == o.failOn {
		return errors.New("gateway unavailable")
	}
	o.deleted = append(o.deleted, key)
	return nil
}

// fakeCacheStore records the accounting side of both cache workers.
type fakeCacheStore struct {
	stale   []sqlcgen.DeleteThumbnailsForStaleContentVersionRow
	byAge   []sqlcgen.ThumbnailEvictionCandidatesByAgeRow
	byQuota []sqlcgen.ThumbnailEvictionCandidatesByQuotaRow
	rows    map[int64]sqlcgen.DeleteThumbnailsRow

	deletedIDs   []int64
	ageCutoff    pgtype.Timestamptz
	quotaMaximum int64
	total        int64
}

func (s *fakeCacheStore) DeleteThumbnailsForStaleContentVersion(
	_ context.Context,
	_ int64,
) ([]sqlcgen.DeleteThumbnailsForStaleContentVersionRow, error) {
	out := s.stale
	// Deleting is what makes the worker idempotent: a retry finds nothing.
	s.stale = nil
	return out, nil
}

func (s *fakeCacheStore) ThumbnailEvictionCandidatesByAge(
	_ context.Context,
	arg sqlcgen.ThumbnailEvictionCandidatesByAgeParams,
) ([]sqlcgen.ThumbnailEvictionCandidatesByAgeRow, error) {
	s.ageCutoff = arg.OlderThan
	out := s.byAge
	s.byAge = nil
	return out, nil
}

func (s *fakeCacheStore) ThumbnailEvictionCandidatesByQuota(
	_ context.Context,
	maxBytes int64,
) ([]sqlcgen.ThumbnailEvictionCandidatesByQuotaRow, error) {
	s.quotaMaximum = maxBytes
	out := s.byQuota
	s.byQuota = nil
	return out, nil
}

func (s *fakeCacheStore) DeleteThumbnails(
	_ context.Context,
	ids []int64,
) ([]sqlcgen.DeleteThumbnailsRow, error) {
	s.deletedIDs = append(s.deletedIDs, ids...)
	out := make([]sqlcgen.DeleteThumbnailsRow, 0, len(ids))
	for _, id := range ids {
		if row, ok := s.rows[id]; ok {
			out = append(out, row)
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

func TestInvalidationDropsTheAccountingRowsAndTheObjectsBehindThem(t *testing.T) {
	store := &fakeCacheStore{
		stale: []sqlcgen.DeleteThumbnailsForStaleContentVersionRow{
			{ID: 1, ObjectKey: "thumb/7/1.webp", SizeBytes: 100},
			{ID: 2, ObjectKey: "thumb/7/2.webp", SizeBytes: 200},
		},
	}
	objects := &fakeObjects{}

	worker := jobs.NewInvalidateThumbnailsWorker(store, objects, discardLogger())
	if err := worker.Work(t.Context(), invalidateJob()); err != nil {
		t.Fatalf("work: %v", err)
	}

	want := []string{"thumb/7/1.webp", "thumb/7/2.webp"}
	if !slices.Equal(objects.deleted, want) {
		t.Errorf("deleted %v, want %v", objects.deleted, want)
	}
}

// A retry after the rows are already gone must not fail: the accounting delete
// is the authoritative step and it is not repeatable.
func TestRepeatingAnInvalidationIsHarmless(t *testing.T) {
	store := &fakeCacheStore{
		stale: []sqlcgen.DeleteThumbnailsForStaleContentVersionRow{
			{ID: 1, ObjectKey: "thumb/7/1.webp", SizeBytes: 100},
		},
	}
	objects := &fakeObjects{}
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
	store := &fakeCacheStore{
		stale: []sqlcgen.DeleteThumbnailsForStaleContentVersionRow{
			{ID: 1, ObjectKey: "thumb/7/1.webp", SizeBytes: 100},
			{ID: 2, ObjectKey: "thumb/7/2.webp", SizeBytes: 200},
		},
	}
	objects := &fakeObjects{failOn: "thumb/7/1.webp"}

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

func evictionJob() *river.Job[jobs.EvictionMaintenanceArgs] {
	return &river.Job[jobs.EvictionMaintenanceArgs]{Args: jobs.EvictionMaintenanceArgs{}}
}

func TestMaintenanceEvictsByAgeAndThenByQuota(t *testing.T) {
	store := &fakeCacheStore{
		byAge: []sqlcgen.ThumbnailEvictionCandidatesByAgeRow{{ID: 1}},
		byQuota: []sqlcgen.ThumbnailEvictionCandidatesByQuotaRow{
			{ID: 2, ObjectKey: "thumb/b.webp", SizeBytes: 500},
		},
		rows: map[int64]sqlcgen.DeleteThumbnailsRow{
			1: {ID: 1, ObjectKey: "thumb/a.webp", SizeBytes: 100},
			2: {ID: 2, ObjectKey: "thumb/b.webp", SizeBytes: 500},
		},
		total: 42,
	}
	objects := &fakeObjects{}

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
	if !slices.Equal(store.deletedIDs, []int64{1, 2}) {
		t.Errorf("deleted rows %v, want [1 2]", store.deletedIDs)
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
		byAge:   []sqlcgen.ThumbnailEvictionCandidatesByAgeRow{{ID: 1}},
		byQuota: []sqlcgen.ThumbnailEvictionCandidatesByQuotaRow{{ID: 2}},
		rows: map[int64]sqlcgen.DeleteThumbnailsRow{
			1: {ID: 1, ObjectKey: "thumb/a.webp"},
			2: {ID: 2, ObjectKey: "thumb/b.webp"},
		},
	}
	objects := &fakeObjects{}

	worker := jobs.NewEvictionMaintenanceWorker(store, objects, 0, 0, discardLogger())
	if err := worker.Work(t.Context(), evictionJob()); err != nil {
		t.Fatalf("work: %v", err)
	}
	if len(objects.deleted) != 0 {
		t.Errorf("deleted %v with both limits disabled", objects.deleted)
	}
}
