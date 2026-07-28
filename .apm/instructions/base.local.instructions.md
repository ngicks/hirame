---
description: "Project purpose, architecture, repository layout, and delivery workflow"
applyTo: "*"
---

## Project purpose

This repository is the top-level workspace for a self-hosted document indexing and
search system. It contains:

- deployment definitions and configuration in ordinary directories; and
- the source repositories for project-owned components as sub-repositories.

The system watches configured filesystem mountpoints, records filesystem state,
extracts and indexes document text, and provides a web interface for finding and
viewing documents.

## Structure overview

The intended workspace layout is:

```text
.
├── .apm/                  Agent instructions and project automation
├── deploy/                Deployment files owned by this repository
│   ├── quadlet/           Quadlet containers, volumes, networks, and pods
│   │   └── migrations/    Schema-migration init containers and pod wiring
│   ├── config/            Non-secret service configuration and templates
│   └── test/              Deployment and integration-test assets
├── doc/                   Architecture, operations, and planning documents
├── apps/                  System-specific applications owned by this repository
│   ├── search-api/        Full-text search API
│   └── web-gui/           Search and document-viewer web application
├── go-overwatch/          Filesystem watcher source sub-repository
└── gahaku/                Document renderer source sub-repository
```

The search API and web GUI are tied to this system and are versioned directly in
this repository. `go-overwatch` and `gahaku` remain independently versioned
sub-repositories. Apache Tika, VersityGW, and the database are deployment
dependencies; do not vendor their source into this workspace unless a later
decision explicitly requires it.

## Technology stack

- Use Go for most application and service code.
- PostgreSQL is the mandatory system database and full-text search backend. Its
  deployment is part of this repository.
- Provide full-text search with [ParadeDB](https://www.paradedb.com/)
  `pg_search` BM25 indexes; the deployed PostgreSQL image is ParadeDB-based.
  Japanese content is the primary search target: NFKC-normalize extracted text
  at ingestion and index it with the Lindera Japanese tokenizer paired with an
  ngram field for recall.
- Keep SQL explicit and use [`sqlc`](https://sqlc.dev/) to generate type-safe Go
  code from queries. Do not introduce an ORM around generated query code.
- Use [River](https://riverqueue.com/) with PostgreSQL for durable task queuing,
  retries, and scheduled jobs.
- Define the API exposed to the web GUI with
  [ConnectRPC](https://connectrpc.com/). Protobuf schemas are the source of truth;
  generate the Go server bindings and TypeScript web client from them.
- Use Apache Tika for document text and metadata extraction.
- Use Podman and Quadlet for service deployment.

### Web GUI

- Build the web GUI with Preact.
- Use Preact Signals (`@preact/signals`) for local reactive state and
  `preact-iso` for routing and application structure.
- Use `@tanstack/preact-query` for remote/server state, request caching, and
  mutation lifecycle handling.
- Use `@ark-ui/react` for accessible headless UI behavior through Preact's React
  compatibility layer.
- Use daisyUI for styled components.
- Access backend functionality through the generated ConnectRPC client. Do not
  create a parallel ad hoc REST contract for the web GUI.
- Define project-owned daisyUI light and dark themes in one centralized theme
  configuration. Components must use semantic theme tokens rather than
  hard-coded theme colors so the visual theme can be replaced without rewriting
  components.
- Support light and dark display modes. With no saved user preference, select
  the mode from the system `prefers-color-scheme` setting and react to subsequent
  system changes. Allow the user to override the mode and persist that explicit
  preference.

## System flow

1. Watch configured mountpoints and track filesystem state with
   [`github.com/ngicks/go-overwatch`](https://github.com/ngicks/go-overwatch).
2. When a document is added or changed, send it to
   [Apache Tika](https://tika.apache.org/) for text and metadata extraction.
3. Submit durable extraction, indexing, cleanup, and maintenance work through
   River where asynchronous processing is appropriate.
4. Store the extracted data and its full-text search index in PostgreSQL.
5. Expose a ConnectRPC full-text search API in front of the database. Other
   clients must use this API rather than access the search tables directly.
6. Provide a web GUI that searches through the API and displays matching
   documents.
7. Use [`github.com/ngicks/gahaku`](https://github.com/ngicks/gahaku) to render
   documents. Create thumbnails and full-size views on demand through the Gahaku
   API.
8. Store generated thumbnails in
   [Versity S3 Gateway](https://github.com/versity/versitygw).

## Required lifecycle behavior

- A newly discovered document must be recorded, extracted, stored, and indexed.
- A changed document must be re-extracted and re-indexed.
- A changed document must invalidate every cached thumbnail derived from the
  previous content before a new result is served.
- The thumbnail cache must have a configurable total size limit and maximum
  object age, with predictable eviction behavior when either limit is reached.
- Filesystem state, indexed data, source documents, and generated assets must
  retain enough identity and version information to prevent stale renders from
  being returned after a change.
- Processing should be retryable and idempotent because filesystem events and
  service calls may be duplicated or interrupted.

## Repository conventions

- Treat `go-overwatch`, `gahaku`, and any future explicitly designated nested
  source repository as independently versioned sub-repositories. Do not flatten
  their histories into this repository.
- Keep the system-specific search API and web GUI in this repository so their
  database, API, and deployment changes can be delivered atomically.
- If a required feature is missing or defective in a project-owned
  sub-repository, make and test the fix in place, then push it to that
  repository.
- Keep deployment composition, Quadlet units, environment templates, and
  integration-test assets in ordinary directories owned by this repository.
- Do not edit third-party source merely to work around deployment configuration.
- Before changing a sub-repository, inspect and follow its own `AGENTS.md`,
  instructions, build system, and contribution conventions.
- Keep credentials and machine-specific secrets out of version control. Commit
  documented templates instead.

## Deployment and verification

- Deploy services as Podman containers managed by Quadlet.
- Include PostgreSQL in the Quadlet deployment; it is not an optional external
  dependency.
- Define a Quadlet pod with init containers that apply all pending database
  schema migrations before dependent application containers start. A migration
  failure must prevent those applications from starting against a partially
  migrated schema.
- Express service dependencies and readiness explicitly; container start order
  alone is not proof that a service is ready.
- Give persistent database, object-store, index, and application data explicit
  volumes and documented ownership and backup expectations.
- Exercise deployment changes in a VM using the `kvm-in-container` skill.
- Integration tests should cover the end-to-end path from a filesystem event to
  searchable text, document rendering, thumbnail storage, expiration, and
  invalidation after modification.
- Web GUI tests must cover light mode, dark mode, system-default selection, and
  persistence of an explicit user override.
- Deployment tests must verify first-time migration, restart with an up-to-date
  schema, and failure behavior for an unsuccessful migration.
- Add focused tests in any sub-repository changed to implement or repair
  behavior.
