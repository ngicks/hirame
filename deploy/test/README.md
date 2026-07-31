# Deployment end-to-end test

One command brings the whole deployment up from the units in
`deploy/quadlet/`, drives eight scenarios against it, and prints a summary
table:

```sh
deploy/test/run-e2e.sh              # build the images, then run
deploy/test/run-e2e.sh --no-build   # reuse the images already in the store
```

The units under test are the deployed ones, byte for byte. `e2e.sh` copies
`deploy/quadlet/` into a scratch directory, drops the files from `dropins/`
beside them, and refuses to start if any copied unit differs from its source —
so a change to the deployment cannot be papered over here.

## Why this shape

D-013 chose podman-in-podman over a KVM guest, and this execution environment
turns out not to allow even an ordinary rootless arrangement. **On a normal host
with systemd none of this is needed**: install `deploy/quadlet/` under the
service user's `~/.config/containers/systemd/` and
`deploy/systemd/overwatch.service` under `/etc/systemd/system/`, run `systemctl
--user daemon-reload` and `sudo systemctl daemon-reload`, and systemd generates
and supervises the same services. This harness exists for the CI shape, and it
stays the CI path because it needs nothing of the host but a kernel with
unprivileged user namespaces.

Four environment constraints shape everything below. Each was measured, not
assumed.

| Constraint | Consequence |
| --- | --- |
| The surrounding user namespace maps one uid and holds no `CAP_SYS_ADMIN` | `lib/pnp-enter` creates a nested user namespace mapping 0..65536, which is what crun and the multi-uid images need |
| `iptables` and `nft` are both absent | netavark cannot program a bridge at any depth, so every container runs `--network=host` and `env/` addresses services as `127.0.0.1` |
| `/sys/fs/cgroup` is a read-only, threaded cgroup2 tree | systemd cannot run (not even as PID 1), every container needs `--cgroups=disabled`, and `podman pod create` fails outright |
| `sethostname(2)` is seccomp-blocked | every container needs `--uts=host`, and `podman build` needs `--isolation=chroot` |

Prerequisites on the host running the harness: podman (vendored under
`PODMAN_DIST`), `gcc` for `lib/*.c`, `python3` for the supervisor, and **`go`**,
which `build-images.sh` needs for the two overwatch binaries — those are host
binaries rather than images now, so they cannot carry their own toolchain in a
build stage.

The harness writes to the paths the deployment names, and reproduces its split
between the two privilege domains (D-009): `$HOME/.config/hirame/` for the user
units' configuration — which is what their `%h/.config/hirame/...` expands to —
plus `/etc/hirame/overwatch.json` for the system daemon, `/srv/documents` for
the watched tree, and `/run/overwatch/hirame/` for the socket. Inside
`pnp-enter` those are the nested namespace's own and `$HOME` is `/root`, so the
supervisor's `%h` expansion is what makes the user paths land; the watched tree
is a tmpfs of its own.

**One `pnp-enter` per run.** There is no `nsenter` here and a second
`pnp-enter` is a *different* user namespace, so a stack started in one
invocation does not exist in the next. `run-e2e.sh` therefore hands the entire
scenario sequence to a single invocation. Image builds are a separate
invocation only because images live in the shared graphroot and survive it.

## Layout

| Path | What it is |
| --- | --- |
| `run-e2e.sh` | the entrypoint; builds, then runs `e2e.sh` inside `pnp-enter` |
| `e2e.sh` | setup, the fixtures, the assertion helpers, and the summary table |
| `scenarios/s*.sh` | one function per scenario, sourced by `e2e.sh` |
| `build-images.sh` | builds the three `localhost/` images, plus `bin/overwatch` and `bin/fakeoverwatch` |
| `lib/pnp-enter`, `lib/userns-exec.c`, `lib/pnp-mount.c` | the nested user namespace and the mounts inside it |
| `lib/pnp-lib.sh` | `pnp_scratch`, `pnp_volume_tmpfs`, `pnp_require_port`, `wait_for` |
| `lib/quadlet-supervisor.py` | runs podman's Quadlet generator and executes what it emits |
| `dropins/` | the test profile, as Quadlet drop-ins |
| `dropins-migration-failure/` | S2's injected failure, applied on top of `dropins/` |
| `dropins-eviction-quota/` | S8's one-byte cache quota, applied on top of `dropins/` |
| `env/` | concrete `hirame.env` / `secrets.env` for the test profile |
| `fakeoverwatch/` | the stand-in overwatch daemon (its own Go module; build it with `GOWORK=off`) |
| `bin/` | the two compiled daemons, gitignored |

## What is substituted, and what stays real

`lib/quadlet-supervisor.py` runs the real Quadlet generator — with `-user`,
because the deployed units are user units — and executes the `ExecStart` lines
it emits, in `After=` order. It substitutes three things systemd would otherwise
provide:

- **Specifier expansion.** Quadlet passes `%h` through into the generated
  `ExecStart=` for systemd to expand at load time, and there is no systemd here,
  so `expand()` does it. This is what puts the units' `%h/.config/hirame/...`
  onto the same files `e2e.sh` installed. An unexpanded `%h` would reach podman
  as a relative path that it creates as an empty directory, which is why the
  validation below greps the supervisor's `print` output for a leftover `%`.

- **`Requires=` / `After=`.** Reimplemented, because D-005's guarantee *is*
  that a failed migration stops its dependents. A unit whose `Requires=` names
  a failed or skipped unit is recorded `skipped` and its `ExecStart` never
  runs. S2 asserts on that state *and* on `podman container exists`, never on
  log output.
- **`Notify=healthy`.** There is no `NOTIFY_SOCKET`, so the generated
  `--sdnotify=healthy` becomes `--sdnotify=ignore` and readiness is established
  by running the unit's own `HealthCmd=` through `podman healthcheck run` until
  it passes. The probe is the deployed one; only the thing that waits on it
  changed.

`e2e.sh` substitutes one thing more, outside the supervisor:
`deploy/systemd/overwatch.service`, whose `RuntimeDirectory=` and `ExecStart=`
it performs by hand around the stand-in binary. That unit is not a Quadlet unit,
so the generator never sees it.

A `Requires=` target the supervisor has never been asked to start counts as
satisfied — only `failed` and `skipped` block a dependent. S2 is unaffected
because it starts `hirame-migrate` explicitly, but a scenario that simply
omitted a required unit would pass vacuously; pass the whole set.

Not substituted, and therefore **not covered**: `Restart=`, `RestartSec=`, and
socket activation. Nothing here restarts a crashed container.

Everything else is the real deployment: ParadeDB with `pg_search`, Apache Tika,
Gahaku with LibreOffice, VersityGW, River, the migrations embedded in the
`hirame-search-api` image, and the ConnectRPC API behind the deployed Caddyfile.

### The test profile (`dropins/`)

Additive drop-ins only. Quadlet reads `*.container.d/*.conf` and honours
systemd's empty-value list reset, which is how `Network=` and `Pod=` are
cleared without touching a unit.

| Drop-in | Change | Why |
| --- | --- | --- |
| every container | `Network=host`, `PodmanArgs=--uts=host --cgroups=disabled` | no iptables, seccomp, read-only cgroups (table above) |
| `search-api`, `indexer`, `web-gui`, `hirame-migrate` | `Pod=` cleared | `podman pod create` always makes a pod cgroup, and the cgroup tree is read-only |

There is no drop-in for overwatch any more: it is not a Quadlet unit, so there
is no `Image=` to override, and `indexer.container` no longer names it in
`After=` either — a user unit cannot order against a system unit, so that edge
does not exist in the deployment to reproduce. What is exercised exactly as
deployed is the mount, `Volume=/run/overwatch/hirame:/run/overwatch/hirame:ro`,
against a directory `e2e.sh` creates before the container starts, which is what
the deployment gets from `RuntimeDirectory=` and its `tmpfiles.d` entry.

Dropping the pod costs nothing the pod was carrying here: host networking gives
search-api on `127.0.0.1:8081` reachable from the Caddy container over
loopback, and gives the published `:8080` on the test's own loopback. The
**deployed `Caddyfile` and the deployed `LISTEN_ADDR` are exercised unchanged**
— which is also why the harness uses the deployed port numbers rather than an
offset block, and fails fast if any of the six is already in use.

The volumes need no drop-in at all. `pnp_volume_tmpfs` mounts a tmpfs on the
podman volume's own mountpoint before anything uses it, so
`postgres.container` and `versitygw.container` keep their `Volume=` lines and
their images' `chown` to uid 999 succeeds — a tmpfs mounted inside this user
namespace accepts it, where podman's volume directory (owned by an ancestor
namespace) does not. The mount outlives every container, which is what S6
restarts against.

### The stand-in overwatch daemon (`fakeoverwatch/`)

The deployment runs overwatch as a native systemd service (D-009), so the
harness runs its stand-in the same way: a plain background process inside the
`pnp-enter` namespace, started by `e2e.sh`'s `start_fake_overwatch` after it has
created `/run/overwatch/hirame` — which is what
`deploy/systemd/overwatch.service` gets from `RuntimeDirectory=`, and it matters
in the same way, because podman creates a missing bind source as an empty
directory.

`build-images.sh` compiles the **real** daemon from the submodule as well, and
`e2e.sh` runs it once first, recording verbatim what it says. It cannot work:
fanotify filesystem marks require `CAP_SYS_ADMIN` in the **initial** user
namespace, which no nesting depth can supply. The probe runs before the stand-in
starts, because both bind the same socket and an "address already in use" would
hide the error worth recording.

The stand-in answers the same `OverwatchService` on the same unix socket, reads
the same `deploy/config/overwatch.json`, and accepts the same two argv shapes
(`server serve --config …` and `client --socket … status`), so nothing about the
deployment has to be overridden for it — the indexer bind-mounts the same
directory and dials the same path it would in production.

- **`Scan` really walks the filesystem.** Every path, size, mode and mtime the
  indexer records came from the kernel.
- **`Subscribe` emits no file events.** It reports a `GAP_KIND_STREAM_START`
  marker on a timer instead, which drives the indexer down its `Scan`
  reconciliation path — the fallback D-009's amendment names for environments
  without fanotify.

**Caveats.** Change detection is therefore as slow as the gap interval
(`FAKE_OVERWATCH_GAP_INTERVAL`, default 10s) rather than milliseconds, and the
event-driven half of `internal/ingest` — `ApplyEvent`, rename and delete
handling, the debounce — is **not** exercised here. That half is covered by
`internal/ingest`'s own tests against a scripted stream, and by a deployment on
a host that can hold a fanotify mark.

## Scenarios

| ID | What it proves | Requirement |
| --- | --- | --- |
| S1 | Fresh volume → migrate runs → apps start after it; schema, BM25 index, River tables and `shared_preload_libraries` all present | AGENTS.md "first-time migration", D-005, D-012 |
| S2 | Migrate fails → `search-api` and `indexer` are **not started**, and no container exists for either | AGENTS.md "failed migration", D-005 |
| S3 | A seeded file becomes searchable by a Japanese compound-noun query with highlighted snippets, and NFKC width folding matches | D-012 |
| S4 | A thumbnail renders through Gahaku, stores one object in VersityGW, and a second identical request adds neither a row nor an object | D-008 |
| S5 | Modifying the file re-ingests it, the old thumbnail row and object are gone, the old ref answers `failed_precondition`, and the new text is searchable | AGENTS.md lifecycle, D-007 |
| S6 | A restart over the same data applies no migration, and the documents stay searchable without being extracted again | AGENTS.md "restart with an up-to-date schema" |
| S7 | Caddy serves the SPA shell, falls back to it for a client-side route, and proxies `/api` to the API | D-010 |
| S8 | Both cache limits fire: a back-dated row is evicted by age, and an over-quota one by the byte quota — row *and* object in each case | D-008 |

Each scenario prints `PASS`/`FAIL` per assertion and contributes one row to the
summary table; a failing scenario does not abort the run.

## Known gaps

- **Eviction is driven by restarting the indexer**, not by waiting out
  `S3_EVICTION_INTERVAL`: the River periodic job carries `RunOnStart`, so a
  restart runs one pass immediately. The scheduling half — that the interval
  itself fires — is therefore not covered.
- **Deletion and tombstoning (D-007)** are exercised only indirectly, through
  the scan sweep. Removing a file and asserting the row survives while search
  excludes it would be a small addition to S5.
- **`Internal=true` on `hirame.network` together with the pod's
  `PublishPort=`** is the assumption this harness is least able to settle: it
  runs neither the network nor the pod, because neither can be created here.
  `hirame.network` carries the two fallbacks inline; they still need a host
  with a working network backend to check.
- **The privilege model, and with it the socket handoff.** Podman runs rootful
  *inside* the nested namespace, as gahaku's harness does, so there is no
  subuid mapping between the container and the stand-in daemon and no service
  account: the indexer's `User=10001:0` reaches the socket here because gid 0
  is gid 0 in this namespace, which is not the reason it works in the
  deployment. The whole argument in `deploy/README.md`'s socket-handoff section
  — `Group=hirame`, container gid 0 mapping to the service user's primary
  group — is therefore **unexercised**, and so is the `--userns=keep-id` trap
  it warns about. A host with a real service account is the only place to
  settle it; the `stat` check in that section is the cheapest form.
- **`loginctl enable-linger`, `%h` as a real home, and `subuid`/`subgid`
  allocation** are all supplied by the namespace here rather than tested.
- **`Restart=` and crash recovery** are outside what the supervisor models.
- **Duplicate-event idempotency** is covered by `internal/ingest`'s tests, not
  here: the stand-in daemon emits no events to duplicate.
