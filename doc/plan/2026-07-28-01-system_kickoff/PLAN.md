# System kickoff

Build the first deployable version of the filesystem-backed document ingestion,
full-text search, and rendering system described by the repository instructions.

## Goal and success criteria

- A configured mountpoint is watched and its filesystem state is recorded.
- Adding or changing a supported document schedules durable processing, extracts
  its text with Apache Tika, and makes it searchable through ConnectRPC.
- The Preact GUI can search, open results, request Gahaku renders, and display
  on-demand thumbnails.
- Thumbnail storage in VersityGW enforces the agreed size and age policy and
  invalidates stale thumbnails after source modification.
- PostgreSQL schema migrations complete before dependent services start.
- The complete Quadlet deployment passes end-to-end tests in a VM.

## Scope

- Bootstrap the system-owned applications in `apps/search-api/` and
  `apps/web-gui/`.
- Integrate the `go-overwatch/` and `gahaku/` sub-repositories.
- Define protobuf contracts, PostgreSQL schema and queries, River jobs, and
  document lifecycle behavior.
- Add PostgreSQL, Apache Tika, VersityGW, application services, persistent
  volumes, and schema migration init containers under `deploy/`.
- Add automated unit, integration, and VM deployment tests.

## Non-goals

- Generalizing the search API or GUI for unrelated systems.
- Forking or vendoring Apache Tika, PostgreSQL, or VersityGW.
- Supporting a database or task queue other than PostgreSQL and River.
- Adding a second REST contract beside ConnectRPC.

## Context

- Durable repository rules live in
  `.apm/instructions/base.local.instructions.md` and compile into `AGENTS.md`.
- The repository is at its initial bootstrap: the application, deployment, and
  sub-repository directories described above have not been added yet.
- The only committed project history is the initial `LICENSE` commit; the APM
  configuration and this plan are current workspace additions.

## Approach

Use a Go-first service architecture backed by PostgreSQL. Keep explicit SQL and
generate Go bindings with sqlc. Use River for durable extraction, indexing,
invalidation, and scheduled maintenance. Provide full-text search with ParadeDB
`pg_search` BM25 indexes — a Lindera Japanese-tokenized field paired with an
ngram field for recall, over NFKC-normalized extracted text — so the deployed
PostgreSQL image is ParadeDB-based (D-012). Define the browser-facing contract in
protobuf and generate ConnectRPC bindings for Go and TypeScript. Keep the search
API and Preact GUI in this repository while retaining `go-overwatch` and
`gahaku` as independently versioned sub-repositories.

Rejected alternatives:

- Separate repositories for the GUI and search API: they share a deployment and
  contract lifecycle and benefit from atomic changes.
- An ORM or non-PostgreSQL queue: these conflict with the selected sqlc and River
  stack.
- PGroonga, pg_bigm, or an external search engine (Elasticsearch/OpenSearch):
  see D-012 — BM25 ranking inside PostgreSQL was chosen over more mature
  Japanese tokenization or a second stateful store.
- Direct browser access to database or object storage internals: all
  system-facing behavior belongs behind the generated API.

## Implementation steps

1. Establish source and contract layout.
   - Add `apps/search-api/`, `apps/web-gui/`, and `api/proto/`.
   - Add `go-overwatch/` and `gahaku/` using the selected sub-repository
     mechanism.
   - Verify code generation and baseline builds.
2. Define persistence and migrations.
   - Add versioned migrations and sqlc configuration under `apps/search-api/`.
   - Model mountpoints, filesystem observations, documents, extracted content,
     content versions, and render-cache identity.
   - Install River's required PostgreSQL schema.
3. Implement ingestion and lifecycle processing.
   - Connect go-overwatch events to idempotent River jobs.
   - Implement Tika extraction, NFKC text normalization, database updates,
     ParadeDB BM25 indexing (Lindera Japanese plus ngram fields), retries, and
     stale-thumbnail invalidation.
   - Implement the agreed delete, rename, and move semantics.
4. Implement the ConnectRPC API.
   - Define search, document detail, render, and thumbnail operations in
     `api/proto/`.
   - Generate and implement Go handlers in `apps/search-api/`.
   - Add authorization and exposure controls selected below.
5. Implement the web GUI.
   - Bootstrap Preact in `apps/web-gui/` with Signals, preact-iso, TanStack
     Preact Query, Ark UI, and daisyUI.
   - Use the generated ConnectRPC client.
   - Add centralized semantic light/dark themes, system-default selection, and
     a persistent user override.
6. Build deployment composition.
   - Add Quadlet units, networks, volumes, health checks, and configuration
     templates under `deploy/`.
   - Include PostgreSQL (ParadeDB-based image providing `pg_search`), Tika,
     VersityGW, application containers, and a pod with schema-migration init
     containers.
   - Select and document rootless or system/rootful operation.
7. Verify the system.
   - Add focused package and UI tests.
   - Add integration coverage for event-to-search and rendering/cache
     lifecycles.
   - Use the `kvm-in-container` skill to test clean installation, migration,
     restart, failure recovery, persistence, expiration, and invalidation.

## Testing and verification

- Run Go unit tests, static analysis, protobuf checks, sqlc generation checks,
  and frontend tests in their owning directories.
- Exercise duplicated events and interrupted River jobs to prove idempotency.
- Verify search results update after modification and disappear or transition as
  specified after deletion.
- Verify Japanese search quality with real Tika-extracted documents: full/half
  width and kana variants, compound nouns, and short 1–2 character terms match
  and rank sensibly (D-012 follow-up; PGroonga is the fallback).
- Verify stale thumbnails cannot be served after document content changes.
- Verify theme behavior for light, dark, system default, and persisted override.
- Run the full Quadlet deployment in a KVM guest from a clean state and after a
  restart with persistent data.

## Risks

- Filesystem event coalescing and rename behavior can create stale database
  records unless identity rules are explicit.
- Mountpoint permissions may force system/rootful deployment even if most
  containers could otherwise run rootless.
- VersityGW may not directly implement the required cache-wide eviction policy,
  requiring a River maintenance job and database accounting.
- Tika and Gahaku processing cost requires bounded concurrency and size limits
  to avoid resource exhaustion.
- Browser access, local-file rendering, and unauthenticated deployment can
  expose sensitive document content.
- ParadeDB is a young extension: PostgreSQL major upgrades are coupled to its
  image releases, and its CJK path is less battle-tested than PGroonga's —
  Japanese recall depends on ingest-time NFKC normalization and the dual
  Lindera/ngram field setup working as expected.

## Open questions

1. How are `go-overwatch/` and `gahaku/` attached: Git submodules, or independent
   nested checkouts managed outside the parent Git index? Tentative default: Git
   submodules so revisions are reproducible.
2. What are the required semantics for source deletion, rename, and move?
   Tentative default: tombstone deleted documents and remove them from search;
   preserve identity across detectable renames and moves.
3. Does VersityGW store only thumbnails or full-size rendered output too, and
   does “size limit” mean a cache-wide byte quota? Tentative default:
   thumbnails only, with a cache-wide quota and per-object maximum age.
4. Should Quadlet run rootless or system/rootful? Tentative default: rootless
   unless access to watched mountpoints requires system services.
5. What authentication and network exposure are required for the GUI,
   ConnectRPC API, Gahaku, and VersityGW? Tentative default: expose only the GUI
   and same-origin ConnectRPC endpoint, with internal services on a private
   network.
6. Should filesystem watching and River workers run inside `apps/search-api/` or
   in a separate `apps/indexer/` service? Tentative default: a separate indexer
   process with shared Go packages and independently configurable concurrency.
