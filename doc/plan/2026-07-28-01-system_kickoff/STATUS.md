# System kickoff status

## Current state

Implemented and verified. All seven implementation steps are complete. The
final review gate (five-focus fleet review) surfaced 13 confirmed findings —
thumbnail-accounting retry leaks, a version-flip lost-update race, nested-
mountpoint and bootstrap-watermark ingest bugs, a tombstone-resurrection race,
and four reachable resource-exhaustion gaps — all fixed with discriminating
tests. All suites pass afterward (unit, `-race`, integration against live
ParadeDB, GUI 55 tests) and the podman-in-podman e2e re-run is green 8/8.

## Checklist

- [x] Establish source and contract layout.
- [x] Define persistence and migrations.
- [x] Implement ingestion and lifecycle processing.
- [x] Implement the ConnectRPC API.
- [x] Implement the web GUI.
- [x] Build deployment composition.
- [x] Verify the system end to end (podman-in-podman per D-013, replacing the
      KVM guest; 8/8 scenarios pass).

## Done

- Resolved open questions 1–6: D-006..D-011 accepted with the plan's tentative
  defaults (D-009 amended: the overwatch daemon needs initial-namespace
  `CAP_SYS_ADMIN` for fanotify and runs as the one system-level unit; the rest
  of the stack stays rootless). D-013 records the user-directed switch to
  podman-in-podman e2e testing.
- `go-overwatch/` and `gahaku/` attached as pinned Git submodules.
- `api/proto/` (buf, `hirame.v1`): search/document/render/thumbnail services;
  all renders addressed by `DocumentRef{document_id, content_version_id}` so
  stale thumbnails are structurally unservable. Go (connect-go) and TS
  (connect-es) bindings generated.
- `apps/search-api/`: migrations + `cmd/migrate` (River schema + versioned SQL,
  advisory-locked, checksum drift detection), sqlc queries incl. ParadeDB
  `pg_search` BM25 search (Lindera Japanese field boosted 16x over the paired
  ngram recall field — factor measured against a corpus reproducing the D-012
  inversion), ingest reconciler (overwatch Subscribe/Scan, epoch sweep,
  content-hash versioning, D-007 tombstone/rename semantics), River jobs
  (ingest, Tika extract + NFKC, thumbnail invalidation accounting-row-first,
  age/quota eviction), ConnectRPC handlers, `cmd/indexer` and `cmd/search-api`.
- `apps/web-gui/`: Preact + Signals + preact-iso + TanStack Preact Query +
  Ark UI + daisyUI; semantic `hirame-light`/`hirame-dark` themes with system
  default and persisted override; search/detail/thumbnail/render UI; 54 tests.
- `deploy/`: 15 Quadlet units (generator-validated), Containerfiles, config
  templates, ops README; migration init unit gates dependents (D-005) via
  `Type=oneshot` + `Requires=`/`After=`, verified on generated systemd output.
- `deploy/test/`: podman-in-podman e2e harness (two-extent userns `pnp-enter`,
  quadlet-supervisor executing real units in `After=` order, fake overwatch
  gRPC daemon for the fanotify-less nested environment). 8/8 scenarios pass:
  clean install, migration-failure gate, Japanese ingest→search (11 s),
  render+thumbnail cache (2351 ms cold → 24 ms cached), invalidation after
  modification (`failed_precondition` for superseded versions), restart with
  persistent data, web entrypoint, eviction.
- D-012 Japanese validation done against live pg_search 0.24.3 (compound nouns,
  kana, half-width NFKC all rank sensibly; PGroonga fallback not warranted).
  Found and fixed: prepared-statement generic-plan failure of the `|||` boost
  spelling (now `pdb.match_disjunction(...)::pdb.boost`).

## In progress

- Nothing.

## Blocked

- Nothing.

## Follow-ups (deferred, with reasons)

- Upstream go-overwatch: `overwatch/e2e/vm-truenas/con.py` makes the module
  unfetchable from any Go proxy (Windows reserved filename breaks module zip);
  consumed via `replace` until renamed upstream.
- Authentication (D-010 explicitly defers it; trusted-network deployment).
- E2e does not exercise: live fanotify events (kernel-capability-bound; Scan
  path covered via fake daemon), systemd `Restart=`/sd_notify semantics (no
  systemd possible in this environment — proven cgroup threaded-domain block),
  rootless-vs-rootful publish path, Tika `/rmeta` single-pass optimization,
  orphan-object sweep in VersityGW (bounded leak until API-phase writes).

## Next action

Commit the work (submodule pins, apps, api, deploy, doc updates) once reviewed
by the user.
