# hirame

Self-hosted document indexing and search. Watched filesystem mountpoints are
tracked, documents are extracted and indexed into PostgreSQL, and a web GUI
searches them and views rendered pages.

See `doc/plan/` for the plan and the accepted decisions, and `AGENTS.md` for
the repository rules.

## Layout

```text
.
├── api/proto/          Protobuf contract — the source of truth for the API
│   └── hirame/v1/      search, document detail, render, thumbnail
├── apps/
│   ├── search-api/     Go module: ConnectRPC server + indexer
│   └── web-gui/        Preact + Vite single-page application
├── doc/                Architecture, operations, and planning documents
├── go-overwatch/       Filesystem watcher (git submodule)
├── gahaku/             Document renderer (git submodule)
├── buf.yaml            buf module: api/proto
└── buf.gen.yaml        buf codegen: Go into apps/search-api, TS into apps/web-gui
```

`go-overwatch` and `gahaku` are independently versioned submodules; clone with
`git clone --recurse-submodules`, or run `git submodule update --init` in an
existing checkout.

## Code generation

The protobuf schema is the contract; both the Go server bindings and the
TypeScript browser client are generated from it and committed.

```sh
buf generate     # or: make generate
```

Run it from the repository root — `buf.gen.yaml` writes into two applications,
so its `out` paths are relative to the root:

| target                | output                                    |
| --------------------- | ----------------------------------------- |
| protoc-gen-go         | `apps/search-api/internal/gen`            |
| protoc-gen-connect-go | `apps/search-api/internal/gen`            |
| protoc-gen-es         | `apps/web-gui/src/gen`                    |

Plugins are pinned buf remote plugins, so nothing has to be installed besides
`buf` itself. Both output directories are owned by `buf generate` (`clean:
true` in `buf.gen.yaml`) — never put hand-written code in them.

Check the schema with `buf lint`, and format it with `buf format --write`.

## Applications

### `apps/search-api`

One Go module (`github.com/ngicks/hirame/apps/search-api`, Go 1.26) with two
entrypoints, sharing the schema, job, and configuration packages (D-011):

- `cmd/search-api` — ConnectRPC server. Services are mounted under `/api`,
  with `/healthz` beside it.
- `cmd/indexer` — filesystem watching and River workers.

```sh
cd apps/search-api
go build ./...
go vet ./...
go test ./...
```

Both binaries are configured entirely from the environment
(`internal/config`). `DATABASE_URL` is required; everything else has a
documented default. The variables are `LISTEN_ADDR`, `SHUTDOWN_TIMEOUT`,
`READ_HEADER_TIMEOUT`, `DATABASE_URL`, `DATABASE_MAX_CONNS`, `TIKA_URL`,
`TIKA_TIMEOUT`, `TIKA_MAX_BYTES`, `GAHAKU_URL`, `GAHAKU_TIMEOUT`,
`S3_ENDPOINT`, `S3_REGION`, `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY`,
`S3_BUCKET`, `S3_USE_PATH_STYLE`, `S3_MAX_CACHE_BYTES`, `S3_MAX_OBJECT_AGE`,
`WATCH_MOUNTPOINTS`, `WATCH_DEBOUNCE_INTERVAL`, `RIVER_WORKERS`,
`EXTRACT_CONCURRENCY`, and `RENDER_CONCURRENCY`.

### `apps/web-gui`

Preact + TypeScript + Vite, managed with pnpm.

```sh
cd apps/web-gui
pnpm install
pnpm dev      # proxies /api to http://127.0.0.1:8080
pnpm build
pnpm test
```

The Connect transport uses the relative base URL `/api`, matching
`server.APIPrefix` in `apps/search-api` and the dev proxy in `vite.config.ts`.
In production the GUI and the API share one origin behind the GUI's reverse
proxy (D-010).

Themes are defined once, in `src/styles/app.css`: `hirame-light` and
`hirame-dark`, with daisyUI's built-in themes disabled. Components use
semantic tokens (`bg-base-100`, `text-base-content`, `btn-primary`, …) only.
`src/theme/theme.ts` follows `prefers-color-scheme` until the user chooses a
mode explicitly, which is then persisted to `localStorage`.

## Addressing renders

Document identity is an opaque `document_id` that survives renames and moves.
Content identity is a separate content-hash-derived `content_version_id`.
Renders and thumbnails are addressed by both together (`DocumentRef`), and a
request naming a superseded version is rejected with `FAILED_PRECONDITION`
rather than upgraded — that is what makes a stale thumbnail unservable after
the source changes.

`DocumentService.GetDocument` is the only RPC that resolves "the current
version" of a document; search hits carry the pair too, so a client can go
straight from a result to a render.
