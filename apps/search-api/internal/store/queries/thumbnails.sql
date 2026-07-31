-- Thumbnail cache accounting (D-008). Rows here describe objects in VersityGW.
--
-- Removal is two-phase, because the row and the bytes live in different stores
-- and no transaction spans both. A row is first marked pending_delete_at, which
-- withdraws it from every read; only once its object is gone is the row purged.
-- The marked rows are therefore the durable list of outstanding object deletes,
-- which is what makes a retried job re-derive exactly the work left over rather
-- than find nothing and report success.

-- UpsertThumbnail clears pending_delete_at: a re-render publishes fresh bytes at
-- the same deterministic key, so the entry is live again and must not be purged
-- by a drain still holding the old id.
-- name: UpsertThumbnail :one
INSERT INTO thumbnail_cache (
    content_version_id, page, width, height, format, object_key, size_bytes
) VALUES (
    @content_version_id, @page, @width, @height, @format, @object_key, @size_bytes
)
ON CONFLICT (content_version_id, page, width, height, format) DO UPDATE SET
    object_key        = EXCLUDED.object_key,
    size_bytes        = EXCLUDED.size_bytes,
    last_access_at    = now(),
    pending_delete_at = NULL
RETURNING *;

-- name: GetThumbnail :one
SELECT * FROM thumbnail_cache
WHERE content_version_id = @content_version_id
  AND page = @page
  AND width = @width
  AND height = @height
  AND format = @format
  AND pending_delete_at IS NULL;

-- TouchThumbnail is the read path's only write. It is separate from
-- GetThumbnail so a lookup that misses in the object store does not extend an
-- entry's life.
-- name: TouchThumbnail :exec
UPDATE thumbnail_cache SET last_access_at = now() WHERE id = @id;

-- name: TotalThumbnailBytes :one
SELECT COALESCE(sum(size_bytes), 0)::bigint FROM thumbnail_cache;

-- MarkThumbnailsPendingDelete withdraws rows the eviction passes chose. The
-- timestamp is preserved on a row already marked so the drain order stays the
-- age of the withdrawal rather than the age of the last pass over it.
-- name: MarkThumbnailsPendingDelete :execrows
UPDATE thumbnail_cache
SET pending_delete_at = COALESCE(pending_delete_at, now())
WHERE id = ANY(@ids::bigint[]);

-- PendingDeleteThumbnails is the drain's work list, and the only place any
-- caller learns which objects are still outstanding.
-- name: PendingDeleteThumbnails :many
SELECT id, object_key, size_bytes FROM thumbnail_cache
WHERE pending_delete_at IS NOT NULL
ORDER BY pending_delete_at, id
LIMIT @result_limit;

-- PurgeThumbnails drops rows whose objects are gone. The pending_delete_at
-- guard is what keeps a concurrent re-render safe: UpsertThumbnail clears the
-- mark, so an entry republished while the drain was running is no longer
-- matched here.
-- name: PurgeThumbnails :execrows
DELETE FROM thumbnail_cache
WHERE id = ANY(@ids::bigint[]) AND pending_delete_at IS NOT NULL;

-- The eviction candidate queries consider live entries only. A withdrawn row is
-- already accounted for by the drain, and offering it again would let one pass
-- choose it twice.
-- name: ThumbnailEvictionCandidatesByAge :many
SELECT id, object_key, size_bytes, created_at, last_access_at
FROM thumbnail_cache
WHERE created_at < @older_than
  AND pending_delete_at IS NULL
ORDER BY created_at
LIMIT @result_limit;

-- ThumbnailEvictionCandidatesByQuota returns the LRU tail that pushes the cache
-- over max_bytes: running_bytes accumulates newest-accessed first, so the rows
-- kept are the most recently used and the rows returned are exactly the
-- overflow, oldest first.
--
-- Written as a correlated subtotal rather than the equivalent
-- `sum(...) OVER (...)` because sqlc cannot resolve a window function's output
-- column when it is referenced from an enclosing WHERE.
-- name: ThumbnailEvictionCandidatesByQuota :many
SELECT
    t.id,
    t.object_key,
    t.size_bytes,
    (
        SELECT COALESCE(sum(t2.size_bytes), 0)
        FROM thumbnail_cache t2
        WHERE (t2.last_access_at, t2.id) >= (t.last_access_at, t.id)
          AND t2.pending_delete_at IS NULL
    )::bigint AS running_bytes
FROM thumbnail_cache t
WHERE t.pending_delete_at IS NULL
  AND (
    SELECT COALESCE(sum(t2.size_bytes), 0)
    FROM thumbnail_cache t2
    WHERE (t2.last_access_at, t2.id) >= (t.last_access_at, t.id)
      AND t2.pending_delete_at IS NULL
) > @max_bytes
ORDER BY t.last_access_at ASC, t.id ASC;

-- MarkStaleThumbnailsPendingDelete withdraws accounting for every content
-- version that is no longer any live document's current version: the
-- modified-document case and the tombstoned-document case are the same query,
-- because both stop the version from being current. It is the periodic backstop
-- behind the targeted form below.
-- name: MarkStaleThumbnailsPendingDelete :execrows
UPDATE thumbnail_cache tc
SET pending_delete_at = COALESCE(tc.pending_delete_at, now())
WHERE tc.pending_delete_at IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM documents d
      WHERE d.current_content_version_id = tc.content_version_id
        AND d.deleted_at IS NULL
  );

-- MarkThumbnailsPendingDeleteForStaleContentVersion is the targeted form, run on
-- the ingestion path with the version a document just stopped pointing at, so a
-- changed document cannot serve a stale thumbnail before the sweep runs. The
-- NOT EXISTS guard keeps it from withdrawing a version another live document
-- still holds, which deduplicated content makes possible.
-- name: MarkThumbnailsPendingDeleteForStaleContentVersion :execrows
UPDATE thumbnail_cache tc
SET pending_delete_at = COALESCE(tc.pending_delete_at, now())
WHERE tc.content_version_id = @content_version_id
  AND tc.pending_delete_at IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM documents d
      WHERE d.current_content_version_id = tc.content_version_id
        AND d.deleted_at IS NULL
  );

-- ReferencedThumbnailObjectKeys answers "which of these keys does accounting
-- still know about", asked one listing page at a time. A key absent from the
-- answer is an object no row can reach, which is what the orphan sweep removes.
-- Rows marked pending_delete_at count as referencing their object: the drain
-- owns those, and removing them here would race it.
-- name: ReferencedThumbnailObjectKeys :many
SELECT object_key FROM thumbnail_cache
WHERE object_key = ANY(@object_keys::text[]);
