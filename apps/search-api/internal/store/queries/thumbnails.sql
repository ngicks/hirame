-- Thumbnail cache accounting (D-008). Rows here describe objects in VersityGW;
-- every delete returns the object keys the caller still has to remove there.

-- name: UpsertThumbnail :one
INSERT INTO thumbnail_cache (
    content_version_id, page, width, height, format, object_key, size_bytes
) VALUES (
    @content_version_id, @page, @width, @height, @format, @object_key, @size_bytes
)
ON CONFLICT (content_version_id, page, width, height, format) DO UPDATE SET
    object_key     = EXCLUDED.object_key,
    size_bytes     = EXCLUDED.size_bytes,
    last_access_at = now()
RETURNING *;

-- name: GetThumbnail :one
SELECT * FROM thumbnail_cache
WHERE content_version_id = @content_version_id
  AND page = @page
  AND width = @width
  AND height = @height
  AND format = @format;

-- TouchThumbnail is the read path's only write. It is separate from
-- GetThumbnail so a lookup that misses in the object store does not extend an
-- entry's life.
-- name: TouchThumbnail :exec
UPDATE thumbnail_cache SET last_access_at = now() WHERE id = @id;

-- name: TotalThumbnailBytes :one
SELECT COALESCE(sum(size_bytes), 0)::bigint FROM thumbnail_cache;

-- name: DeleteThumbnails :many
DELETE FROM thumbnail_cache WHERE id = ANY(@ids::bigint[])
RETURNING id, object_key, size_bytes;

-- name: ThumbnailEvictionCandidatesByAge :many
SELECT id, object_key, size_bytes, created_at, last_access_at
FROM thumbnail_cache
WHERE created_at < @older_than
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
    )::bigint AS running_bytes
FROM thumbnail_cache t
WHERE (
    SELECT COALESCE(sum(t2.size_bytes), 0)
    FROM thumbnail_cache t2
    WHERE (t2.last_access_at, t2.id) >= (t.last_access_at, t.id)
) > @max_bytes
ORDER BY t.last_access_at ASC, t.id ASC;

-- DeleteStaleThumbnails drops accounting for every content version that is no
-- longer any live document's current version: the modified-document case and
-- the tombstoned-document case are the same query, because both stop the
-- version from being current.
-- name: DeleteStaleThumbnails :many
DELETE FROM thumbnail_cache tc
WHERE NOT EXISTS (
    SELECT 1 FROM documents d
    WHERE d.current_content_version_id = tc.content_version_id
      AND d.deleted_at IS NULL
)
RETURNING tc.id, tc.object_key, tc.size_bytes;

-- DeleteThumbnailsForStaleContentVersion is the targeted form, run on the
-- ingestion path with the version a document just stopped pointing at, so a
-- changed document cannot serve a stale thumbnail before the sweep runs. The
-- NOT EXISTS guard keeps it from evicting a version another live document still
-- holds, which deduplicated content makes possible.
-- name: DeleteThumbnailsForStaleContentVersion :many
DELETE FROM thumbnail_cache tc
WHERE tc.content_version_id = @content_version_id
  AND NOT EXISTS (
      SELECT 1 FROM documents d
      WHERE d.current_content_version_id = tc.content_version_id
        AND d.deleted_at IS NULL
  )
RETURNING tc.id, tc.object_key, tc.size_bytes;
