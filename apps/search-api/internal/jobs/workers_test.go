package jobs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/ngicks/hirame/apps/search-api/internal/doctype"
	"github.com/ngicks/hirame/apps/search-api/internal/jobs"
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
	"github.com/ngicks/hirame/apps/search-api/internal/tika"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeExtractStore stands in for the generated queries.
type fakeExtractStore struct {
	row     sqlcgen.FindPathForContentVersionRow
	findErr error

	stored   *sqlcgen.UpsertExtractionParams
	failures []sqlcgen.MarkExtractionFailedParams
}

func (s *fakeExtractStore) FindPathForContentVersion(
	_ context.Context,
	_ *int64,
) (sqlcgen.FindPathForContentVersionRow, error) {
	return s.row, s.findErr
}

func (s *fakeExtractStore) UpsertExtraction(
	_ context.Context,
	arg sqlcgen.UpsertExtractionParams,
) (sqlcgen.ExtractedContent, error) {
	s.stored = &arg
	return sqlcgen.ExtractedContent{}, nil
}

func (s *fakeExtractStore) MarkExtractionFailed(
	_ context.Context,
	arg sqlcgen.MarkExtractionFailedParams,
) error {
	s.failures = append(s.failures, arg)
	return nil
}

// fakeTika answers with canned text and metadata.
type fakeTika struct {
	text     string
	meta     tika.Metadata
	textErr  error
	metaErr  error
	maxBytes int64
	seenType string
}

func (f *fakeTika) Text(_ context.Context, body io.Reader, contentType string) (string, error) {
	f.seenType = contentType
	_, _ = io.Copy(io.Discard, body)
	return f.text, f.textErr
}

func (f *fakeTika) Meta(
	_ context.Context,
	body io.Reader,
	_ string,
) (tika.Metadata, error) {
	_, _ = io.Copy(io.Discard, body)
	return f.meta, f.metaErr
}

func (f *fakeTika) MaxBytes() int64 { return f.maxBytes }

type fakeOpener struct{ files map[string]string }

func (o fakeOpener) Open(path string) (io.ReadCloser, error) {
	body, ok := o.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader([]byte(body))), nil
}

const testPath = "/srv/docs/a.pdf"

func extractJob() *river.Job[jobs.ExtractArgs] {
	return &river.Job[jobs.ExtractArgs]{Args: jobs.ExtractArgs{ContentVersionID: 7}}
}

func newExtractWorker(store *fakeExtractStore, client *fakeTika) *jobs.ExtractWorker {
	return jobs.NewExtractWorker(
		store,
		client,
		doctype.NewFilter(nil),
		fakeOpener{files: map[string]string{testPath: "%PDF-1.4 body"}},
		discardLogger(),
	)
}

// The BM25 index only ever reads NFKC-normalized text (D-012), so the worker
// has to fold what Tika returns before storing it.
func TestExtractionStoresNormalizedTextAndTheDetectedType(t *testing.T) {
	store := &fakeExtractStore{
		row: sqlcgen.FindPathForContentVersionRow{DocumentID: 1, Path: testPath, Size: 12},
	}
	client := &fakeTika{
		// Halfwidth katakana and a fullwidth latin letter, which NFKC folds.
		text: "ﾚﾎﾟｰﾄ Ａ",
		meta: tika.Metadata{
			Raw:         []byte(`{"Content-Type":"application/pdf"}`),
			ContentType: "application/pdf",
		},
	}

	if err := newExtractWorker(store, client).Work(t.Context(), extractJob()); err != nil {
		t.Fatalf("work: %v", err)
	}
	if store.stored == nil {
		t.Fatal("nothing was stored")
	}
	if store.stored.Status != "succeeded" {
		t.Errorf("status = %q, want succeeded", store.stored.Status)
	}
	if store.stored.TextNormalized != "レポート A" {
		t.Errorf("text = %q, want the NFKC-folded form", store.stored.TextNormalized)
	}
	if got := client.seenType; got != "application/pdf" {
		t.Errorf("content type sent to Tika = %q", got)
	}
	if len(store.failures) != 0 {
		t.Errorf("failures = %v, want none", store.failures)
	}
}

// By the time the job runs the document may already point somewhere else.
// Extracting anyway would index content that is not current.
func TestExtractionSkipsAVersionNoLiveDocumentPointsAt(t *testing.T) {
	store := &fakeExtractStore{findErr: pgx.ErrNoRows}

	if err := newExtractWorker(store, &fakeTika{}).Work(t.Context(), extractJob()); err != nil {
		t.Fatalf("work: %v", err)
	}
	if store.stored != nil {
		t.Error("a superseded version was extracted anyway")
	}
	if len(store.failures) != 0 {
		t.Error("a superseded version was recorded as a failure")
	}
}

func TestAnOversizeDocumentFailsPermanentlyWithoutReachingTika(t *testing.T) {
	store := &fakeExtractStore{
		row: sqlcgen.FindPathForContentVersionRow{Path: testPath, Size: 5_000},
	}
	client := &fakeTika{maxBytes: 1_000}

	err := newExtractWorker(store, client).Work(t.Context(), extractJob())
	if !errors.Is(err, &river.JobCancelError{}) {
		t.Fatalf("error = %v, want a cancelled job", err)
	}
	if len(store.failures) != 1 {
		t.Fatalf("failures = %v, want one", store.failures)
	}
	if client.seenType != "" {
		t.Error("an oversize document was still uploaded to Tika")
	}
}

func TestATikaRejectionIsRecordedAndNotRetried(t *testing.T) {
	store := &fakeExtractStore{
		row: sqlcgen.FindPathForContentVersionRow{Path: testPath, Size: 12},
	}
	client := &fakeTika{textErr: &tika.StatusError{
		Path:       "/tika",
		StatusCode: http.StatusUnsupportedMediaType,
	}}

	err := newExtractWorker(store, client).Work(t.Context(), extractJob())
	if !errors.Is(err, &river.JobCancelError{}) {
		t.Fatalf("error = %v, want a cancelled job", err)
	}
	if len(store.failures) != 1 {
		t.Fatalf("failures = %v, want one", store.failures)
	}
	if store.failures[0].Error == nil || *store.failures[0].Error == "" {
		t.Error("the failure was recorded without a reason")
	}
}

// A failure is written on every attempt, not only the last: River gives the
// worker no signal that an attempt is final, so a row left at 'pending' after
// the job is discarded would claim work is still coming.
func TestATikaOutageIsRecordedAndLeftRetryable(t *testing.T) {
	store := &fakeExtractStore{
		row: sqlcgen.FindPathForContentVersionRow{Path: testPath, Size: 12},
	}
	client := &fakeTika{textErr: &tika.StatusError{
		Path:       "/tika",
		StatusCode: http.StatusInternalServerError,
	}}

	err := newExtractWorker(store, client).Work(t.Context(), extractJob())
	if err == nil {
		t.Fatal("a server error was swallowed instead of being retried")
	}
	if errors.Is(err, &river.JobCancelError{}) {
		t.Fatal("a server error cancelled the job instead of retrying it")
	}
	if len(store.failures) != 1 {
		t.Errorf("failures = %v, want one", store.failures)
	}
}

func TestExtractionBoundsItsRetriesAndRunsOnItsOwnQueue(t *testing.T) {
	opts := jobs.ExtractArgs{}.InsertOpts()
	if opts.MaxAttempts != 5 {
		t.Errorf("MaxAttempts = %d, want a small bound", opts.MaxAttempts)
	}
	if opts.Queue != jobs.QueueExtract {
		t.Errorf("Queue = %q, want %q", opts.Queue, jobs.QueueExtract)
	}
}

// River's default uniqueness window includes `completed`, which would stop a
// content version from ever being processed twice — wrong for every job here.
// The four states River insists any custom window contain are asserted too,
// because leaving one out is rejected at insert time rather than at compile
// time.
func TestUniquenessNeverCountsCompletedJobs(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts river.InsertOpts
	}{
		{"ingest path", jobs.IngestPathArgs{}.InsertOpts()},
		{"extract", jobs.ExtractArgs{}.InsertOpts()},
		{"invalidate thumbnails", jobs.InvalidateThumbnailsArgs{}.InsertOpts()},
		{"eviction maintenance", jobs.EvictionMaintenanceArgs{}.InsertOpts()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			states := tc.opts.UniqueOpts.ByState
			if len(states) == 0 {
				t.Fatal("ByState is unset, so River's default window applies")
			}
			if slices.Contains(states, rivertype.JobStateCompleted) {
				t.Error("ByState counts completed jobs, which blocks every later run")
			}
			for _, required := range []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
			} {
				if !slices.Contains(states, required) {
					t.Errorf("ByState omits %q, which River rejects at insert", required)
				}
			}
		})
	}
}

// The debounce and the close-of-write must be different unique keys, or the
// deferred job sitting in `scheduled` would swallow the only event that knows
// the write finished.
func TestTheSettledTriggerIsADistinctUniqueKeyFromTheDebouncedOne(t *testing.T) {
	if !(jobs.IngestPathArgs{}).InsertOpts().UniqueOpts.ByArgs {
		t.Fatal("uniqueness ignores the args, so the trigger cannot separate them")
	}

	// ByArgs hashes the encoded args, so two triggers for one path are only
	// separate jobs if they encode differently.
	encode := func(trigger string) []byte {
		t.Helper()
		out, err := json.Marshal(jobs.IngestPathArgs{
			MountpointID: 1,
			Path:         testPath,
			Trigger:      trigger,
		})
		if err != nil {
			t.Fatalf("marshal args: %v", err)
		}
		return out
	}
	if bytes.Equal(encode(jobs.TriggerDebounced), encode(jobs.TriggerSettled)) {
		t.Fatal("both triggers encode identically, so the debounce would swallow the close")
	}
}
