-- Mountpoint registration and the go-overwatch reconciliation surface:
-- observations, scan epochs, and the event watermark.

-- name: UpsertMountpoint :one
INSERT INTO mountpoints (root_path, enabled)
VALUES (@root_path, @enabled)
ON CONFLICT (root_path) DO UPDATE SET
    enabled    = EXCLUDED.enabled,
    updated_at = now()
RETURNING *;

-- name: ListMountpoints :many
SELECT * FROM mountpoints ORDER BY root_path;

-- name: GetMountpointByRoot :one
SELECT * FROM mountpoints WHERE root_path = @root_path;

-- name: GetWatermark :one
SELECT watermark FROM reconcile_state WHERE id;

-- LockWatermark is GetWatermark for the apply path, which must decide whether
-- an event was already applied and write the new value without another applier
-- interleaving between the two.
-- name: LockWatermark :one
SELECT watermark FROM reconcile_state WHERE id FOR UPDATE;

-- name: SetWatermark :exec
UPDATE reconcile_state SET watermark = GREATEST(watermark, @watermark) WHERE id;

-- name: NextScanEpoch :one
SELECT nextval('scan_epoch_seq');

-- name: UpsertObservation :exec
INSERT INTO fs_observations (
    mountpoint_id, path, size, mode, mtime, ino, dev, is_dir, seen_epoch, observed_at
) VALUES (
    @mountpoint_id, @path, @size, @mode, @mtime, @ino, @dev, @is_dir, @seen_epoch, now()
)
ON CONFLICT (mountpoint_id, path) DO UPDATE SET
    size        = EXCLUDED.size,
    mode        = EXCLUDED.mode,
    mtime       = EXCLUDED.mtime,
    ino         = EXCLUDED.ino,
    dev         = EXCLUDED.dev,
    is_dir      = EXCLUDED.is_dir,
    seen_epoch  = EXCLUDED.seen_epoch,
    observed_at = now();

-- name: GetObservation :one
SELECT * FROM fs_observations WHERE mountpoint_id = @mountpoint_id AND path = @path;

-- name: DeleteObservation :exec
DELETE FROM fs_observations WHERE mountpoint_id = @mountpoint_id AND path = @path;

-- name: DeleteObservationSubtree :many
DELETE FROM fs_observations
WHERE mountpoint_id = @mountpoint_id AND starts_with(path, @path_prefix)
RETURNING path;

-- MoveObservationSubtree re-paths a renamed directory's contents. It pairs with
-- MoveDocumentSubtree: one Move event stands for every path beneath the
-- directory, so the rows have to be rewritten rather than re-observed.
-- name: MoveObservationSubtree :exec
UPDATE fs_observations
SET path        = @new_prefix || substr(path, length(@old_prefix::text) + 1),
    observed_at = now()
WHERE mountpoint_id = @mountpoint_id AND starts_with(path, @old_prefix);

-- SweepScanEpoch removes everything under a root that the current scan did not
-- observe. That is how deletes missed during a subscribe gap are recovered.
-- name: SweepScanEpoch :many
DELETE FROM fs_observations
WHERE mountpoint_id = @mountpoint_id
  AND (path = @root OR starts_with(path, @root_prefix))
  AND seen_epoch <> @seen_epoch
RETURNING path;
