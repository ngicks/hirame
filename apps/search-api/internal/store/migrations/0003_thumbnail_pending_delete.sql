-- Two-phase thumbnail removal (D-008).
--
-- Deleting the accounting row and the object it describes cannot be one atomic
-- step: the row lives here and the bytes live in VersityGW. Committing the row
-- away first and then removing the object left a retried job with nothing to
-- re-derive — the second attempt matched zero rows and reported success while
-- the object it had failed to remove stayed in the bucket forever.
--
-- pending_delete_at is the durable record of "this entry is withdrawn, its
-- object has yet to go". Marked rows are excluded from every read, so nothing
-- can be served through one, and they stay until their object is gone, so a
-- retry re-derives exactly the work that is outstanding.
ALTER TABLE thumbnail_cache
    ADD COLUMN pending_delete_at timestamptz;

-- Partial: the drain pass reads only marked rows, which are a vanishing
-- fraction of the table outside a failure.
CREATE INDEX thumbnail_cache_pending_delete
    ON thumbnail_cache (pending_delete_at)
    WHERE pending_delete_at IS NOT NULL;
