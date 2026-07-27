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

- **Status:** Open
- **Decision needed:** Git submodules or externally managed nested Git
  checkouts.
- **Tentative choice:** Git submodules.

## D-007: Filesystem removal and identity semantics

- **Status:** Open
- **Decision needed:** Deletion, rename, move, tombstone, and identity behavior.
- **Tentative choice:** Tombstone deletions and preserve identity for detectable
  renames or moves.

## D-008: Render-cache scope and limits

- **Status:** Open
- **Decision needed:** Stored render types and the meaning of the size cap.
- **Tentative choice:** Cache thumbnails only, bounded by a cache-wide byte quota
  and per-object maximum age.

## D-009: Quadlet privilege mode

- **Status:** Open
- **Decision needed:** Rootless or system/rootful services.
- **Tentative choice:** Rootless unless watched-mount access requires otherwise.

## D-010: Authentication and network exposure

- **Status:** Open
- **Decision needed:** User authentication and externally reachable services.
- **Tentative choice:** Expose the GUI and same-origin ConnectRPC endpoint only;
  keep supporting services private.

## D-011: Ingestion process boundary

- **Status:** Open
- **Decision needed:** Run watching and workers with the API or as a separate
  indexer process.
- **Tentative choice:** A separate indexer process sharing internal Go packages.
