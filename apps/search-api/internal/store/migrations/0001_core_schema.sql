-- Core persistence for the ingestion pipeline: what the filesystem currently
-- looks like, which documents that implies, and which content each document
-- currently holds.
--
-- This file is also a sqlc schema input (see apps/search-api/sqlc.yaml), so it
-- must stay declarative DDL that sqlc's PostgreSQL parser accepts.

CREATE TABLE mountpoints (
    id         bigint      PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
    root_path  text        NOT NULL UNIQUE,
    enabled    boolean     NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- fs_observations is the current observed state of every watched path, as
-- reported by go-overwatch. Its shape follows go-overwatch's own reference
-- store (reconciler/repo/pg/schema.sql): the daemon is stateless, so this table
-- plus reconcile_state is the entire durable view of the filesystem.
CREATE TABLE fs_observations (
    mountpoint_id bigint      NOT NULL REFERENCES mountpoints (id) ON DELETE CASCADE,
    path          text        NOT NULL,
    size          bigint      NOT NULL DEFAULT 0,
    mode          bigint      NOT NULL DEFAULT 0,
    mtime         timestamptz,
    ino           bigint      NOT NULL DEFAULT 0,
    dev           bigint      NOT NULL DEFAULT 0,
    is_dir        boolean     NOT NULL DEFAULT false,
    -- seen_epoch tags the most recent scan that observed this path; a scan
    -- sweep deletes rows under a root whose epoch is not the current scan's.
    -- That is how deletes missed during a subscribe gap are recovered.
    seen_epoch    bigint      NOT NULL DEFAULT 0,
    observed_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (mountpoint_id, path)
);

-- text_pattern_ops makes starts_with()/prefix subtree queries index-friendly.
CREATE INDEX fs_observations_path_prefix
    ON fs_observations (mountpoint_id, path text_pattern_ops);

-- Rename detection reads this before falling back to content hash.
CREATE INDEX fs_observations_identity
    ON fs_observations (mountpoint_id, dev, ino)
    WHERE NOT is_dir;

-- Monotonic scan epoch source; each scan draws a fresh value.
CREATE SEQUENCE scan_epoch_seq;

-- Single-row watermark of the highest applied go-overwatch event seq. The seq
-- is global to the daemon's stream, not per mountpoint, so this is a singleton
-- rather than a column on mountpoints.
CREATE TABLE reconcile_state (
    id        boolean PRIMARY KEY DEFAULT true,
    watermark bigint  NOT NULL DEFAULT 0,
    CONSTRAINT reconcile_state_singleton CHECK (id)
);

INSERT INTO reconcile_state (id, watermark) VALUES (true, 0)
ON CONFLICT (id) DO NOTHING;

-- content_versions is content identity, addressed by hash and shared across
-- documents. Two documents holding identical bytes point at one row, so an
-- unchanged file that is copied or moved reuses the existing extraction and
-- thumbnails (D-007).
CREATE TABLE content_versions (
    id           bigint      PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
    -- Lowercase hex SHA-256. text rather than bytea: it crosses the ConnectRPC
    -- boundary and appears in logs and object keys as hex either way.
    content_hash text        NOT NULL UNIQUE,
    size_bytes   bigint      NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- documents is stable identity for a file across rename and move. Rows are
-- never hard-deleted: a removed file is tombstoned so thumbnail accounting and
-- invalidation lineage survive (D-007).
CREATE TABLE documents (
    id                         bigint      PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
    mountpoint_id              bigint      NOT NULL REFERENCES mountpoints (id) ON DELETE CASCADE,
    path                       text        NOT NULL,
    dev                        bigint      NOT NULL DEFAULT 0,
    ino                        bigint      NOT NULL DEFAULT 0,
    current_content_version_id bigint      REFERENCES content_versions (id),
    deleted_at                 timestamptz,
    first_seen_at              timestamptz NOT NULL DEFAULT now(),
    updated_at                 timestamptz NOT NULL DEFAULT now()
);

-- One live document per path; tombstones are excluded so a path can be reused.
CREATE UNIQUE INDEX documents_live_path
    ON documents (mountpoint_id, path)
    WHERE deleted_at IS NULL;

-- Rename detection step 1: same inode, different path.
CREATE INDEX documents_live_identity
    ON documents (mountpoint_id, dev, ino)
    WHERE deleted_at IS NULL;

-- Rename detection step 2 (inode changed, e.g. write-to-temp-then-rename), and
-- the join key of every stale-thumbnail query.
CREATE INDEX documents_current_content_version
    ON documents (current_content_version_id);

-- extracted_contents holds Tika output per content version, not per document,
-- so identical bytes are extracted once. text_normalized is NFKC-normalized at
-- ingestion (D-012) and is the only column the BM25 index reads.
CREATE TABLE extracted_contents (
    content_version_id bigint      PRIMARY KEY
                                   REFERENCES content_versions (id) ON DELETE CASCADE,
    status             text        NOT NULL,
    text_normalized    text        NOT NULL DEFAULT '',
    metadata           jsonb       NOT NULL DEFAULT '{}'::jsonb,
    content_type       text,
    error              text,
    extracted_at       timestamptz,
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT extracted_contents_status_valid
        CHECK (status IN ('pending', 'succeeded', 'failed'))
);

-- thumbnail_cache is accounting only: the bytes live in VersityGW. It is keyed
-- by content version rather than document so a rename cannot orphan an object,
-- and a content change makes every prior row instantly identifiable as stale
-- (D-008).
CREATE TABLE thumbnail_cache (
    id                 bigint      PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
    content_version_id bigint      NOT NULL
                                   REFERENCES content_versions (id) ON DELETE CASCADE,
    page               integer     NOT NULL,
    width              integer     NOT NULL,
    height             integer     NOT NULL,
    format             text        NOT NULL,
    object_key         text        NOT NULL UNIQUE,
    size_bytes         bigint      NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    last_access_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (content_version_id, page, width, height, format)
);

-- Age eviction scans by created_at; quota eviction orders by last_access_at.
CREATE INDEX thumbnail_cache_created_at ON thumbnail_cache (created_at);
CREATE INDEX thumbnail_cache_last_access_at ON thumbnail_cache (last_access_at);
CREATE INDEX thumbnail_cache_content_version ON thumbnail_cache (content_version_id);
