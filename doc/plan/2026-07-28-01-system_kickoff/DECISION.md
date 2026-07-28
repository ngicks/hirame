# System kickoff decisions

## D-001: Keep system-specific applications in this repository

- **Status:** Accepted
- **Choice:** Version `apps/search-api/` and `apps/web-gui/` directly in this
  repository.
- **Rationale:** Their database, API, GUI, and deployment contracts evolve
  together and benefit from atomic changes.
- **Rejected:** Separate repositories for the search API and GUI.

## D-002: Use a Go and PostgreSQL application stack

- **Status:** Accepted
- **Choice:** Use Go for most services, PostgreSQL for persistence and full-text
  search, sqlc for query bindings, and River for durable jobs and scheduling.
- **Rationale:** This is the selected project stack and keeps queues and indexed
  state transactional in PostgreSQL.
- **Rejected:** An ORM, a separate queue broker, or another search database.

## D-003: Use ConnectRPC for the browser-facing API

- **Status:** Accepted
- **Choice:** Treat protobuf schemas as the source of truth and generate Go
  server and TypeScript browser bindings.
- **Rationale:** One typed contract serves the Go backend and Preact GUI.
- **Rejected:** A parallel ad hoc REST API.

## D-004: Use the selected Preact stack and swappable themes

- **Status:** Accepted
- **Choice:** Use Preact, Preact Signals, preact-iso, TanStack Preact Query, Ark
  UI through Preact compatibility, and daisyUI. Provide centralized semantic
  light and dark themes, defaulting to the system setting with a persistent user
  override.
- **Rationale:** This is the selected UI stack and preserves theme portability.
- **Rejected:** Hard-coded component colors or a fixed default color mode.

## D-005: Apply schema migrations before application startup

- **Status:** Accepted
- **Choice:** Deploy PostgreSQL and a Quadlet pod with migration init containers;
  failed migrations prevent dependent applications from starting.
- **Rationale:** Applications must never run against a partially migrated schema.
- **Rejected:** Opportunistic in-process migration after serving begins.

## D-006: Sub-repository attachment mechanism

- **Status:** Accepted
- **Choice:** Attach `go-overwatch/` and `gahaku/` as Git submodules.
- **Rationale:** Submodules pin exact revisions in the parent index, keeping the
  workspace reproducible without flattening sub-repository history.
- **Rejected:** Externally managed nested checkouts (revisions would not be
  recorded in this repository).

## D-007: Filesystem removal and identity semantics

- **Status:** Accepted
- **Choice:** Tombstone deleted documents (retain the row, mark deleted, drop
  from search results). Preserve document identity across detectable renames and
  moves; content identity is tracked by content hash so an unchanged file moved
  elsewhere keeps its extraction and render lineage.
- **Rationale:** Tombstones keep render/cache accounting and audit history
  consistent while search reflects only live files.
- **Rejected:** Hard row deletion (loses invalidation lineage) and path-only
  identity (breaks on rename).

## D-008: Render-cache scope and limits

- **Status:** Accepted
- **Choice:** VersityGW stores thumbnails only; full-size renders are produced
  on demand and streamed without caching. The cache is bounded by a cache-wide
  byte quota and a per-object maximum age, enforced by a scheduled River
  maintenance job using database-side accounting.
- **Rationale:** Thumbnails dominate request volume; database accounting makes
  eviction deterministic where VersityGW has no native policy.
- **Rejected:** Caching full renders (unbounded size for marginal benefit).

## D-009: Quadlet privilege mode

- **Status:** Accepted
- **Choice:** Rootless Quadlet under a dedicated service user, with the watched
  mountpoint bind-mounted read-only into the indexer container. Escalate to
  system units only if a real mountpoint's permissions demand it.
- **Rationale:** Rootless is the safer default and sufficient when the service
  user is granted read access to watched paths.
- **Rejected:** Rootful-by-default operation.
- **Amendment (2026-07-28):** go-overwatch's fanotify filesystem marks require
  `CAP_SYS_ADMIN` in the initial user namespace, which no rootless container can
  provide. The escalation clause applies to the watcher alone: run the overwatch
  daemon as a system-level unit (rootful container or host daemon per
  go-overwatch's systemd ops doc) and share its Unix socket with the otherwise
  rootless stack via a bind mount. In nested/e2e environments without fanotify,
  ingestion falls back to the indexer's Scan reconciliation path.

## D-010: Authentication and network exposure

- **Status:** Accepted
- **Choice:** Expose only the web GUI origin; the ConnectRPC API is served
  same-origin behind the GUI's reverse proxy. PostgreSQL, Tika, Gahaku, and
  VersityGW live on an internal Podman network with no published ports. No user
  authentication in this first version; the deployment is for trusted networks
  and auth is a documented follow-up.
- **Rationale:** Minimal exposed surface; internal services are unreachable from
  the browser.
- **Rejected:** Publishing internal service ports; building auth before the
  core pipeline exists.

## D-011: Ingestion process boundary

- **Status:** Accepted
- **Choice:** A separate `indexer` binary in `apps/search-api/` (shared Go
  module, `cmd/indexer` and `cmd/search-api` entrypoints) runs filesystem
  watching and River workers with independently configurable concurrency.
- **Rationale:** Shared packages keep schema and job types atomic while process
  separation isolates crash and resource behavior.
- **Rejected:** In-API workers (couples API latency to extraction load) and a
  separate Go module (splits the shared schema code).

## D-012: Use ParadeDB pg_search for full-text search

- **Status:** Accepted
- **Choice:** Base the deployed PostgreSQL image on ParadeDB's distribution and
  index extracted document text with `pg_search` BM25 indexes. Pair the Lindera
  Japanese tokenizer with an ngram-tokenized field for recall, and apply NFKC
  normalization (full/half width, kana variants) to extracted text at ingestion.
- **Rationale:** Stays inside PostgreSQL per D-002, so indexing remains
  transactional with River jobs and filesystem state, while providing true BM25
  relevance ranking plus snippets, highlighting, and facets. Queries remain
  plain SQL, compatible with sqlc.
- **Rejected:**
  - PGroonga: the most mature Japanese tokenization and normalization, but its
    default scoring is term-frequency based; relevance ranking was prioritized.
  - pg_bigm and native `tsvector`: no relevance ranking, and no maintained
    Japanese segmentation respectively.
  - Elasticsearch/OpenSearch: best-in-class Japanese analysis (kuromoji/Sudachi)
    but a second stateful store — dual-write consistency, reindex tooling, and
    JVM operational burden are disproportionate at this system's scale.
- **Follow-up:** Validate Japanese query quality early with real Tika-extracted
  documents (width and kana variants, compound nouns, short 1–2 character
  terms). PGroonga is the designated fallback if quality disappoints.
- **Validation result (2026-07-28):** Verified against live pg_search 0.24.3:
  compound nouns, kana, and half-width→NFKC cases all match and rank sensibly;
  tombstoned documents stay excluded. One precision cost observed: the ngram
  field admits low-score false positives (e.g. `アパート` matching `レポート`
  via the `ート` bigram) and can slightly outrank Lindera's correct
  segmentation. Mitigation: boost the Lindera field over the ngram field in the
  search query. PGroonga fallback is not warranted on current evidence.

## D-013: End-to-end testing via podman-in-podman

- **Status:** Accepted (user decision, 2026-07-28)
- **Choice:** Run deployment end-to-end tests with podman-in-podman: an outer
  privileged-enough Podman container runs the Quadlet deployment with an inner
  Podman, instead of a KVM guest VM.
- **Rationale:** User directive; the execution environment lacks reliable
  `/dev/kvm`, and podman-in-podman exercises the same Quadlet units with far
  lighter setup.
- **Rejected:** `kvm-in-container` KVM guest testing (retained as a future
  option for kernel-level fidelity, e.g. real systemd boot ordering).
