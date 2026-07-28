package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"

	"github.com/ngicks/hirame/apps/search-api/internal/doctype"
)

// FileOpener opens a watched file. It is an interface so the state machine can
// be exercised without a filesystem; the indexer passes [OSOpener].
type FileOpener interface {
	Open(path string) (io.ReadCloser, error)
}

// OSOpener reads the real filesystem.
type OSOpener struct{}

// Open implements FileOpener.
func (OSOpener) Open(path string) (io.ReadCloser, error) { return os.Open(path) }

// Processor turns one observed path into content identity: it hashes the file
// and, only when the hash differs from what the document already points at,
// promotes the new version and queues the work that a change implies.
//
// It runs from a job rather than from the event stream because hashing is
// unbounded in time and the stream is not allowed to stall.
type Processor struct {
	store  Store
	filter *doctype.Filter
	opener FileOpener
	logger *slog.Logger
}

// NewProcessor builds a Processor. logger must not be nil.
func NewProcessor(
	store Store,
	filter *doctype.Filter,
	opener FileOpener,
	logger *slog.Logger,
) *Processor {
	return &Processor{store: store, filter: filter, opener: opener, logger: logger}
}

// ProcessPath brings one path's document and content version up to date.
//
// A path that has since been deleted, turned into a directory, or stopped
// matching the include filter is not an error: the event that made it so is
// authoritative and has its own handling.
func (p *Processor) ProcessPath(ctx context.Context, mountpointID int64, path string) error {
	var (
		obs   Observation
		found bool
	)
	if err := p.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		obs, found, err = tx.GetObservation(ctx, mountpointID, path)
		return err
	}); err != nil {
		return err
	}
	if !found || obs.IsDir || !p.filter.AllowPath(path) {
		return nil
	}

	// Hashed outside a transaction: a large file would otherwise pin a pooled
	// connection for the whole read.
	digest, size, head, err := p.hash(ctx, path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, ok := p.filter.ContentType(path, head); !ok {
		return nil
	}

	return p.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		doc, err := p.resolveDocument(ctx, tx, mountpointID, path, obs, digest)
		if err != nil {
			return err
		}
		versionID, err := tx.UpsertContentVersion(ctx, digest, size)
		if err != nil {
			return err
		}
		if doc.CurrentContentVersionID == versionID {
			// Same bytes. A Modify event that changed nothing, a re-observed
			// file, or a duplicate event: all end here, which is what makes the
			// pipeline safe to feed the same event twice.
			return nil
		}

		superseded := doc.CurrentContentVersionID
		if err := tx.SetDocumentCurrentVersion(ctx, doc.ID, versionID); err != nil {
			return err
		}
		if err := tx.MarkExtractionPending(ctx, versionID); err != nil {
			return err
		}
		if err := tx.EnqueueExtract(ctx, versionID); err != nil {
			return err
		}
		if superseded != 0 {
			// Committed with the flip, so the thumbnails of the version this
			// document just stopped pointing at can never outlive it silently.
			if err := tx.EnqueueInvalidateThumbnails(ctx, doc.ID, superseded); err != nil {
				return err
			}
		}

		p.logger.InfoContext(ctx, "content version promoted",
			slog.Int64("document_id", doc.ID),
			slog.String("path", path),
			slog.Int64("content_version_id", versionID),
			slog.Int64("superseded_content_version_id", superseded),
		)
		return nil
	})
}

// resolveDocument finds the document this path belongs to, in the order
// documents.sql documents: path, then inode, then content, then a new row.
func (p *Processor) resolveDocument(
	ctx context.Context,
	tx Tx,
	mountpointID int64,
	path string,
	obs Observation,
	digest string,
) (Document, error) {
	doc, found, err := tx.FindLiveDocumentByPath(ctx, mountpointID, path)
	if err != nil {
		return Document{}, err
	}
	if found {
		if doc.Dev != obs.Dev || doc.Ino != obs.Ino {
			// Same path, different file: the editor replaced it rather than
			// rewriting it in place. The document keeps its identity and picks
			// up the new inode.
			if err := tx.MoveDocument(ctx, doc.ID, path, obs.Dev, obs.Ino); err != nil {
				return Document{}, err
			}
		}
		return doc, nil
	}

	// The inode survives a rename, so it identifies the same file at a new
	// path. A zero inode means the event carried no stat and would match
	// anything.
	if obs.Ino != 0 {
		doc, found, err = tx.FindLiveDocumentByInode(ctx, mountpointID, obs.Dev, obs.Ino)
		if err != nil {
			return Document{}, err
		}
		if found {
			if err := tx.MoveDocument(ctx, doc.ID, path, obs.Dev, obs.Ino); err != nil {
				return Document{}, err
			}
			return doc, nil
		}
	}

	// Identical bytes whose old path no longer exists: a write-to-temp-then-
	// rename, where even the inode changed.
	doc, found, err = tx.FindLiveDocumentByContent(ctx, mountpointID, digest)
	if err != nil {
		return Document{}, err
	}
	if found {
		if err := tx.MoveDocument(ctx, doc.ID, path, obs.Dev, obs.Ino); err != nil {
			return Document{}, err
		}
		return doc, nil
	}

	return tx.CreateDocument(ctx, mountpointID, path, obs.Dev, obs.Ino)
}

// hash reads the file once, returning its SHA-256, its size, and the leading
// bytes the content sniff needs.
func (p *Processor) hash(
	ctx context.Context,
	path string,
) (digest string, size int64, head []byte, err error) {
	f, err := p.opener.Open(path)
	if err != nil {
		return "", 0, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	sum := sha256.New()
	headBuf := make([]byte, 0, doctype.SniffLen)
	buf := make([]byte, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, nil, err
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			sum.Write(buf[:n])
			size += int64(n)
			if len(headBuf) < doctype.SniffLen {
				headBuf = append(headBuf, buf[:min(n, doctype.SniffLen-len(headBuf))]...)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return "", 0, nil, fmt.Errorf("read %s: %w", path, readErr)
		}
	}
	return hex.EncodeToString(sum.Sum(nil)), size, headBuf, nil
}
