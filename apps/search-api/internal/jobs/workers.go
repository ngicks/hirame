package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/ngicks/hirame/apps/search-api/internal/doctype"
	"github.com/ngicks/hirame/apps/search-api/internal/ingest"
	"github.com/ngicks/hirame/apps/search-api/internal/store"
	"github.com/ngicks/hirame/apps/search-api/internal/store/sqlcgen"
	"github.com/ngicks/hirame/apps/search-api/internal/tika"
)

// Extractor is the slice of the Tika client the extraction worker uses.
type Extractor interface {
	Text(ctx context.Context, body io.Reader, contentType string) (string, error)
	Meta(ctx context.Context, body io.Reader, contentType string) (tika.Metadata, error)
	MaxBytes() int64
}

// ObjectDeleter is the slice of the object store the cache workers use. Only
// deletion happens here; the bytes themselves are transferred by presigned URL.
type ObjectDeleter interface {
	Delete(ctx context.Context, key string) error
}

// ExtractStore is the database surface the extraction worker uses. It is an
// interface rather than *sqlcgen.Queries so the worker's failure handling can
// be exercised without a database; *sqlcgen.Queries satisfies it as generated.
type ExtractStore interface {
	FindPathForContentVersion(
		ctx context.Context, contentVersionID *int64,
	) (sqlcgen.FindPathForContentVersionRow, error)
	UpsertExtraction(
		ctx context.Context, arg sqlcgen.UpsertExtractionParams,
	) (sqlcgen.ExtractedContent, error)
	MarkExtractionFailed(ctx context.Context, arg sqlcgen.MarkExtractionFailedParams) error
}

// IngestPathWorker hands a path to the ingest state machine.
type IngestPathWorker struct {
	river.WorkerDefaults[IngestPathArgs]
	processor *ingest.Processor
}

// NewIngestPathWorker builds the worker.
func NewIngestPathWorker(processor *ingest.Processor) *IngestPathWorker {
	return &IngestPathWorker{processor: processor}
}

// Work implements river.Worker.
func (w *IngestPathWorker) Work(ctx context.Context, job *river.Job[IngestPathArgs]) error {
	return w.processor.ProcessPath(ctx, job.Args.MountpointID, job.Args.Path)
}

// ExtractWorker sends one content version's bytes to Tika and stores the
// NFKC-normalized result (D-012).
type ExtractWorker struct {
	river.WorkerDefaults[ExtractArgs]
	queries   ExtractStore
	extractor Extractor
	filter    *doctype.Filter
	opener    ingest.FileOpener
	logger    *slog.Logger
}

// NewExtractWorker builds the worker. logger must not be nil.
func NewExtractWorker(
	queries ExtractStore,
	extractor Extractor,
	filter *doctype.Filter,
	opener ingest.FileOpener,
	logger *slog.Logger,
) *ExtractWorker {
	return &ExtractWorker{
		queries:   queries,
		extractor: extractor,
		filter:    filter,
		opener:    opener,
		logger:    logger,
	}
}

// Timeout implements river.Worker. Extraction of a large scanned document is
// slow, and the client-level default of one minute would cancel it mid-parse.
func (w *ExtractWorker) Timeout(*river.Job[ExtractArgs]) time.Duration { return 30 * time.Minute }

// Work implements river.Worker.
func (w *ExtractWorker) Work(ctx context.Context, job *river.Job[ExtractArgs]) error {
	versionID := job.Args.ContentVersionID

	row, err := w.queries.FindPathForContentVersion(ctx, &versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		// No live document points at this version any more: it was superseded
		// or tombstoned while the job waited. Extracting it would index content
		// that is not current.
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve path for content version %d: %w", versionID, err)
	}

	if max := w.extractor.MaxBytes(); max > 0 && row.Size > max {
		return w.fail(ctx, versionID, fmt.Errorf(
			"%w: %d bytes exceeds the %d byte limit", tika.ErrTooLarge, row.Size, max))
	}

	if err := w.extract(ctx, versionID, row.Path); err != nil {
		return w.fail(ctx, versionID, err)
	}
	return nil
}

func (w *ExtractWorker) extract(ctx context.Context, versionID int64, path string) error {
	head, err := w.readHead(path)
	if err != nil {
		return err
	}
	contentType, ok := w.filter.ContentType(path, head)
	if !ok {
		return fmt.Errorf("%w: %s is not an includable document", errPermanent, path)
	}

	text, err := withFile(ctx, w.opener, path,
		func(ctx context.Context, r io.Reader) (string, error) {
			return w.extractor.Text(ctx, r, contentType)
		},
	)
	if err != nil {
		return err
	}
	// A second pass over the file rather than one /rmeta/text call: the two
	// documented endpoints each answer one question, and reading a local file
	// twice is cheaper than depending on a response shape we cannot exercise
	// against a live server here.
	meta, err := withFile(ctx, w.opener, path,
		func(ctx context.Context, r io.Reader) (tika.Metadata, error) {
			return w.extractor.Meta(ctx, r, contentType)
		},
	)
	if err != nil {
		return err
	}

	metadata := []byte(meta.Raw)
	if len(metadata) == 0 {
		metadata = []byte("{}")
	}
	storedType := contentType
	if meta.ContentType != "" {
		storedType = meta.ContentType
	}

	_, err = w.queries.UpsertExtraction(ctx, sqlcgen.UpsertExtractionParams{
		ContentVersionID: versionID,
		Status:           "succeeded",
		TextNormalized:   store.NormalizeText(text),
		Metadata:         metadata,
		ContentType:      &storedType,
		Error:            nil,
	})
	if err != nil {
		return fmt.Errorf("store extraction %d: %w", versionID, err)
	}

	w.logger.InfoContext(ctx, "content extracted",
		slog.Int64("content_version_id", versionID),
		slog.String("path", path),
		slog.String("content_type", storedType),
		slog.Int("text_bytes", len(text)),
	)
	return nil
}

// fail records why extraction did not work and decides whether another attempt
// could plausibly do better.
//
// The failure is written on every attempt, not only the last: River gives a
// worker no signal that an attempt is final, and a row left at 'pending' after
// the job is discarded would claim work is still coming.
func (w *ExtractWorker) fail(ctx context.Context, versionID int64, cause error) error {
	message := cause.Error()
	if err := w.queries.MarkExtractionFailed(ctx, sqlcgen.MarkExtractionFailedParams{
		ContentVersionID: versionID,
		Error:            &message,
	}); err != nil {
		return fmt.Errorf("record extraction failure %d: %w", versionID, err)
	}
	if isPermanent(cause) {
		w.logger.WarnContext(ctx, "extraction failed permanently",
			slog.Int64("content_version_id", versionID),
			slog.Any("error", cause),
		)
		return river.JobCancel(cause)
	}
	return cause
}

// errPermanent marks a failure no retry can fix.
var errPermanent = errors.New("permanent extraction failure")

func isPermanent(err error) bool {
	if errors.Is(err, errPermanent) ||
		errors.Is(err, tika.ErrTooLarge) ||
		errors.Is(err, fs.ErrNotExist) {
		return true
	}
	var statusErr *tika.StatusError
	return errors.As(err, &statusErr) && statusErr.Permanent()
}

func (w *ExtractWorker) readHead(path string) ([]byte, error) {
	f, err := w.opener.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	head := make([]byte, doctype.SniffLen)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return head[:n], nil
}

// withFile opens path, hands it to fn, and closes it.
func withFile[T any](
	ctx context.Context,
	opener ingest.FileOpener,
	path string,
	fn func(context.Context, io.Reader) (T, error),
) (T, error) {
	var zero T
	f, err := opener.Open(path)
	if err != nil {
		return zero, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return fn(ctx, f)
}
