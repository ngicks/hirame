-- Document identity (D-007), content versions, and extraction results.
--
-- The indexer resolves identity in this order on a create/modify event:
--   1. FindLiveDocumentByPath   - the ordinary case, path unchanged;
--   2. FindLiveDocumentByInode  - same file, moved (inode survives a rename);
--   3. FindLiveDocumentByContent - inode changed but bytes did not, which is
--      what write-to-temp-then-rename editors produce;
--   4. CreateDocument.

-- name: FindLiveDocumentByPath :one
SELECT * FROM documents
WHERE mountpoint_id = @mountpoint_id AND path = @path AND deleted_at IS NULL;

-- name: FindLiveDocumentByInode :one
SELECT * FROM documents
WHERE mountpoint_id = @mountpoint_id
  AND dev = @dev
  AND ino = @ino
  AND deleted_at IS NULL
ORDER BY id
LIMIT 1;

-- FindLiveDocumentByContent only considers documents whose own path no longer
-- exists as an observation; otherwise a plain copy of a file would be mistaken
-- for a move of it.
-- name: FindLiveDocumentByContent :one
SELECT d.* FROM documents d
JOIN content_versions cv ON cv.id = d.current_content_version_id
WHERE d.mountpoint_id = @mountpoint_id
  AND cv.content_hash = @content_hash
  AND d.deleted_at IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM fs_observations o
      WHERE o.mountpoint_id = d.mountpoint_id AND o.path = d.path
  )
ORDER BY d.id
LIMIT 1;

-- name: CreateDocument :one
INSERT INTO documents (mountpoint_id, path, dev, ino)
VALUES (@mountpoint_id, @path, @dev, @ino)
RETURNING *;

-- name: MoveDocument :one
UPDATE documents
SET path = @path, dev = @dev, ino = @ino, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: TombstoneDocument :one
UPDATE documents
SET deleted_at = COALESCE(deleted_at, now()), updated_at = now()
WHERE id = @id
RETURNING *;

-- name: TombstoneDocumentByPath :one
UPDATE documents
SET deleted_at = COALESCE(deleted_at, now()), updated_at = now()
WHERE mountpoint_id = @mountpoint_id AND path = @path AND deleted_at IS NULL
RETURNING *;

-- The content version comes back with each row because tombstoning is what
-- makes a version stale: the caller turns each one into an invalidation job.
-- name: TombstoneDocumentSubtree :many
UPDATE documents
SET deleted_at = COALESCE(deleted_at, now()), updated_at = now()
WHERE mountpoint_id = @mountpoint_id
  AND starts_with(path, @path_prefix)
  AND deleted_at IS NULL
RETURNING id, path, current_content_version_id;

-- MoveDocumentSubtree re-paths every live document under a renamed directory.
-- go-overwatch reports a directory rename as one Move event and emits nothing
-- for the files inside it, so without this the whole subtree would keep paths
-- that no longer exist and D-007's identity-across-moves guarantee would hold
-- for files but not for the directories containing them.
-- name: MoveDocumentSubtree :many
UPDATE documents
SET path = @new_prefix || substr(path, length(@old_prefix::text) + 1),
    updated_at = now()
WHERE mountpoint_id = @mountpoint_id
  AND starts_with(path, @old_prefix)
  AND deleted_at IS NULL
RETURNING id, path;

-- GetDocument is the API's detail read as well as its version check: the
-- content hash it returns is the content_version_id every render is addressed
-- by, so one row answers both "does this document exist" and "is this ref
-- still current".
--
-- root_path comes along because the API reports a path relative to the
-- mountpoint while documents.path is absolute.
-- name: GetDocument :one
SELECT
    d.*,
    m.root_path,
    cv.content_hash,
    cv.size_bytes,
    cv.created_at      AS content_first_seen_at,
    ec.status          AS extraction_status,
    ec.content_type,
    ec.metadata,
    -- "Stored and searchable", which is stricter than "not empty": a failed
    -- extraction keeps the text of the attempt before it (see
    -- MarkExtractionFailed), and SearchDocuments requires status = 'succeeded',
    -- so text left behind by a failure is present but unfindable. The status
    -- test is what keeps this column agreeing with what search will do.
    --
    -- COALESCE, because the LEFT JOIN yields NULL for a document with no
    -- extraction row at all, which is "no text" rather than an unknown.
    COALESCE(ec.status = 'succeeded' AND ec.text_normalized <> '', false)::boolean
        AS has_extracted_text,
    ec.error           AS extraction_error,
    ec.extracted_at
FROM documents d
JOIN mountpoints m ON m.id = d.mountpoint_id
LEFT JOIN content_versions cv ON cv.id = d.current_content_version_id
LEFT JOIN extracted_contents ec ON ec.content_version_id = cv.id
WHERE d.id = @id;

-- name: GetDocumentText :one
SELECT ec.text_normalized
FROM documents d
JOIN extracted_contents ec ON ec.content_version_id = d.current_content_version_id
WHERE d.id = @id;

-- UpsertContentVersion is hash-addressed, so two documents holding identical
-- bytes converge on one row and reuse its extraction and thumbnails. The
-- no-op UPDATE is there to make RETURNING fire on conflict.
-- name: UpsertContentVersion :one
INSERT INTO content_versions (content_hash, size_bytes)
VALUES (@content_hash, @size_bytes)
ON CONFLICT (content_hash) DO UPDATE SET size_bytes = EXCLUDED.size_bytes
RETURNING *;

-- SetDocumentCurrentVersion is a compare-and-swap rather than a blind UPDATE.
-- The version it supersedes is read in one transaction and written in another —
-- hashing sits between them and must not hold a connection — so two jobs for the
-- same path can interleave. Writing blindly would let the loser overwrite the
-- winner's flip while enqueueing invalidation for a version that is no longer
-- the superseded one, leaving the winner's thumbnails to be dropped and the
-- loser's to survive. No row comes back when the expectation no longer holds,
-- which is the caller's signal to re-read and try again.
--
-- IS NOT DISTINCT FROM, not =, because a document that has never been extracted
-- carries NULL here and that is a legitimate expected value.
-- name: SetDocumentCurrentVersion :one
UPDATE documents
SET current_content_version_id = @current_content_version_id, updated_at = now()
WHERE id = @id
  AND current_content_version_id IS NOT DISTINCT FROM @expected_content_version_id
RETURNING *;

-- name: UpsertExtraction :one
INSERT INTO extracted_contents (
    content_version_id, status, text_normalized, metadata, content_type, error, extracted_at
) VALUES (
    @content_version_id, @status, @text_normalized, @metadata, @content_type, @error, now()
)
ON CONFLICT (content_version_id) DO UPDATE SET
    status          = EXCLUDED.status,
    text_normalized = EXCLUDED.text_normalized,
    metadata        = EXCLUDED.metadata,
    content_type    = EXCLUDED.content_type,
    error           = EXCLUDED.error,
    extracted_at    = EXCLUDED.extracted_at,
    updated_at      = now()
RETURNING *;

-- MarkExtractionPending claims a content version for extraction without
-- disturbing a result already there, so re-enqueueing a job cannot blank out
-- text that is still correct while the retry is in flight.
-- name: MarkExtractionPending :exec
INSERT INTO extracted_contents (content_version_id, status)
VALUES (@content_version_id, 'pending')
ON CONFLICT (content_version_id) DO NOTHING;

-- MarkExtractionFailed records why extraction failed and leaves
-- text_normalized and extracted_at as they were. The row drops out of search
-- either way because SearchDocuments requires status = 'succeeded'; keeping the
-- old text means a later success can be compared against it rather than
-- appearing out of nowhere.
-- name: MarkExtractionFailed :exec
INSERT INTO extracted_contents (content_version_id, status, error)
VALUES (@content_version_id, 'failed', @error)
ON CONFLICT (content_version_id) DO UPDATE SET
    status     = 'failed',
    error      = EXCLUDED.error,
    updated_at = now();

-- name: GetExtraction :one
SELECT * FROM extracted_contents WHERE content_version_id = @content_version_id;

-- FindPathForContentVersion resolves the bytes behind a content version back to
-- a file the extractor can open. It joins fs_observations rather than trusting
-- documents.path alone so a document whose file has since disappeared yields no
-- row instead of a path that fails to open.
-- name: FindPathForContentVersion :one
SELECT d.id AS document_id, d.mountpoint_id, d.path, o.size
FROM documents d
JOIN fs_observations o
  ON o.mountpoint_id = d.mountpoint_id AND o.path = d.path
WHERE d.current_content_version_id = @content_version_id
  AND d.deleted_at IS NULL
ORDER BY d.id
LIMIT 1;

-- FindDocumentSourcePath is the render path's file lookup. It takes both halves
-- of the ref rather than the content version alone: content is deduplicated by
-- hash, so one version is legitimately current for several documents, and
-- resolving by version would stream whichever of them sorted first — a file the
-- caller never named. Requiring the version to still be the document's current
-- one makes a superseded ref yield no row instead of the wrong bytes, and the
-- fs_observations join keeps a document whose file has since disappeared from
-- yielding a path that fails to open.
-- name: FindDocumentSourcePath :one
SELECT d.path, o.size
FROM documents d
JOIN fs_observations o
  ON o.mountpoint_id = d.mountpoint_id AND o.path = d.path
WHERE d.id = @id
  AND d.current_content_version_id = @current_content_version_id
  AND d.deleted_at IS NULL;
