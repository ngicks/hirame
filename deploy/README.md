# Deploying hirame

Rootless Podman with Quadlet (D-009). One internal network, one pod for the
application tier, one published port.

```
                       host :8080
                           │
┌──────────────────────────┼───────────────────────────────────────────┐
│ hirame.network (internal, no gateway)                                │
│                          │                                           │
│   ┌──────────────────────┴──────────────────┐                        │
│   │ hirame.pod                              │   postgres  :5432      │
│   │  web-gui   :8080  (caddy, SPA + /api)   │   tika      :9998      │
│   │  search-api 127.0.0.1:8081              │   gahaku    :9000      │
│   │  indexer                                │   versitygw :7070      │
│   │  hirame-migrate (oneshot, init)         │                        │
│   └─────────────────────────────────────────┘                        │
└──────────────────────────────────────────────────────────────────────┘
      overwatch (no network at all; unix socket in a shared volume)
```

## Units

| Unit | Generated service | What it is |
| --- | --- | --- |
| `quadlet/hirame.network` | `hirame-network.service` | the internal podman network `hirame` |
| `quadlet/hirame.pod` | `hirame-pod.service` | the application pod; owns the published port |
| `quadlet/hirame-db.volume` | `hirame-db-volume.service` | PostgreSQL's data directory |
| `quadlet/hirame-objectstore.volume` | `hirame-objectstore-volume.service` | VersityGW's posix backend |
| `quadlet/hirame-overwatch-run.volume` | `hirame-overwatch-run-volume.service` | the overwatch socket directory |
| `quadlet/postgres.container` | `postgres.service` | ParadeDB PostgreSQL, healthy when `pg_isready` passes |
| `quadlet/migrations/hirame-migrate.container` | `hirame-migrate.service` | the schema-migration init container (D-005) |
| `quadlet/tika.container` | `tika.service` | Apache Tika extraction |
| `quadlet/versitygw.container` | `versitygw.service` | the S3 gateway holding thumbnails |
| `quadlet/versitygw-bootstrap.container` | `versitygw-bootstrap.service` | creates the thumbnail bucket, once |
| `quadlet/gahaku.container` | `gahaku.service` | the render service, gRPC `:9000` |
| `quadlet/overwatch.container` | `overwatch.service` | the fanotify watcher |
| `quadlet/search-api.container` | `search-api.service` | the ConnectRPC API |
| `quadlet/indexer.container` | `indexer.service` | filesystem watching and River workers |
| `quadlet/web-gui.container` | `web-gui.service` | the SPA and the API reverse proxy |

Supporting units carry no `[Install]`; they are reached through the `Requires=`
the generator derives from `Network=`, `Volume=`, and `Pod=`.

## Images

Everything third-party is pinned to an exact tag. A floating tag would let a
restart move the database, the extractor, or the gateway underneath a
deployment whose data was produced by the previous one.

| Image | Why this tag |
| --- | --- |
| `docker.io/paradedb/paradedb:0.24.3-pg18` | the exact pair `internal/store/migrations/0002_search_index.sql` was verified against (D-012) |
| `docker.io/apache/tika:3.3.1.0-full` | `-full` carries the OCR/image toolchain scanned documents need |
| `docker.io/versity/versitygw:v1.7.0` | what gahaku's own deployment tests ran against |
| `docker.io/minio/mc:RELEASE.2025-08-13T08-35-41Z` | bucket bootstrap only |
| `docker.io/library/caddy:2.10-alpine` | runtime stage of `hirame-web-gui` |
| `docker.io/library/node:24-bookworm-slim` + `pnpm@10.34.5` | build stage of `hirame-web-gui` |
| `docker.io/library/golang:1.26-bookworm` + `docker.io/library/debian:bookworm-slim` | build and runtime stages of `hirame-search-api` and `hirame-overwatch` |

Four images are built here and published nowhere; the units name them
`localhost/...` so a missing build fails loudly instead of pulling something
from docker.io:

```sh
podman build --tag hirame-search-api:latest --file apps/search-api/Containerfile .
podman build --tag hirame-web-gui:latest    apps/web-gui
podman build --tag gahaku:latest            gahaku
podman build -f deploy/overwatch/Containerfile --tag hirame-overwatch:latest go-overwatch
```

`gahaku` builds from the submodule's own `Dockerfile` — it is not vendored, and
its image is LibreOffice-sized. `hirame-overwatch` builds from the submodule
path rather than from a module proxy because go-overwatch is pre-release and
publishes no tag this deployment could pin; the submodule revision recorded in
this repository's index is the reference (D-006).

## Install

Rootless, under the service user's own systemd. Nothing here wants a privileged
port — except `overwatch.service`, which is the subject of its own section
below.

```sh
mkdir -p ~/.config/containers/systemd
cp -r deploy/quadlet/. ~/.config/containers/systemd/
```

The `migrations/` subdirectory is copied as-is; the generator recurses into
subdirectories of a unit directory, and the layout is the one AGENTS.md
prescribes.

Configuration, none of which is secret:

```sh
install -Dm0644 deploy/config/hirame.env.template   ~/.config/hirame/hirame.env
install -Dm0644 deploy/config/postgresql.conf       ~/.config/hirame/postgresql.conf
install -Dm0644 deploy/config/overwatch.json        ~/.config/hirame/overwatch.json
```

Credentials, which are:

```sh
mkdir -p ~/.config/hirame
umask 077
sed -e "s/__DB_PASSWORD__/$(openssl rand -hex 24)/g" \
    -e "s/__S3_ACCESS_KEY__/$(openssl rand -hex 16)/g" \
    -e "s/__S3_SECRET_KEY__/$(openssl rand -hex 32)/g" \
    deploy/config/secrets.env.template > ~/.config/hirame/secrets.env
```

The units read `secrets.env` with a plain `EnvironmentFile=` and no `-` prefix:
a service that came up without credentials would be a worse outcome than a unit
that refuses to start. `gahaku/deployment/quadlet/README.md` gives the podman
`Secret=` form for deployments that would rather not keep a file, and the
trade-off that comes with it.

Point the deployment at the documents to index. Both `overwatch.container` and
`indexer.container` bind-mount `%h/hirame/documents` read-only at
`/srv/documents`; edit the host side of both `Volume=` lines to the real
mountpoint, and keep `WATCH_MOUNTPOINTS` and `deploy/config/overwatch.json`
naming the same container path — the indexer resolves the paths the daemon
reports in its own mount namespace.

`overwatch.json` is JSON and cannot carry its own comments, so its three
non-default choices are recorded here. `listen.unix` is written out rather than
derived from `instance`, so the socket path is fixed by configuration instead of
by a naming rule the volume mount would have to track. `events` drops `attrib`
from the daemon's default set: permission and xattr changes produce no work in
this pipeline, and `close_write` is the signal the debounce interval is for.
`limits.syscalls_per_sec` is 200 rather than the default 1000, which is the
budget go-overwatch's README recommends for HDD- or NFS-backed paths; raise it
toward the default on local SSD, where the conservative value only makes the
first full scan slower.

Then generate and start:

```sh
systemctl --user daemon-reload
systemctl --user start hirame-pod.service
systemctl --user status 'hirame*' 'postgres*' 'tika*' 'versitygw*' 'gahaku*' \
                        'overwatch*' 'search-api*' 'indexer*' 'web-gui*'
```

`daemon-reload` is what runs the quadlet generator; `systemctl --user cat
search-api.service` shows what it produced. Every unit with an `[Install]` is
`WantedBy=default.target`, so a `daemon-reload` alone already schedules them for
the next login — `loginctl enable-linger $USER` keeps them running when nobody
is logged in.

The GUI is then on `http://<host>:8080`.

## How migrations are enforced (D-005)

`hirame-migrate.service` runs `apps/search-api`'s `migrate` binary from the same
image as `search-api` and `indexer`, so the migrations applied and the code that
reads the resulting schema can never be different builds. Three systemd
properties do the actual enforcing:

1. **`Type=oneshot` + a foreground `podman run`.** For a `Type=oneshot` source
   unit the generator emits an `ExecStart` with no `-d`, so the container runs
   in the foreground and its exit status becomes the unit's. A migration that
   fails is a unit that failed, not a container that logged an error.
2. **`Requires=` *and* `After=` on the dependents.** `search-api.container` and
   `indexer.container` carry both. systemd's rule for that pair is what the
   whole decision rests on: when a `Requires=` dependency fails to activate and
   an `After=` orders against it, the dependent unit is not started at all.
   `Requires=` on its own would only pull the migration in, concurrently, and
   the applications would race it.
3. **`Requires=`/`After=postgres.service`, whose `Notify=healthy`** makes
   "started" mean "`pg_isready` over TCP passes". Ordering against a container
   that merely exists would race `initdb` on a first boot — and the image's own
   initialisation runs a temporary socket-only server, which is why the health
   command forces `--host=127.0.0.1`.

`RemainAfterExit=yes` keeps a successful run active so the `Requires=` of both
applications is satisfied by one run rather than triggering one per dependent.
The cost is that after a redeploy, restarting an application alone does not
re-run migrations:

```sh
systemctl --user restart hirame-migrate.service
systemctl --user restart search-api.service indexer.service
```

To see what a run would do without doing it — the check to run before a deploy —
the binary takes `-status` (reports, always succeeds) and `-dry-run` (reports,
fails if anything is pending).

## How readiness is expressed

Container start order proves nothing, so every service whose consumers must
wait carries a healthcheck *and* `Notify=healthy`, which makes podman withhold
the systemd readiness notification until that healthcheck passes:

| Service | Healthy when | Consumers ordered after it |
| --- | --- | --- |
| `postgres` | `pg_isready` answers over TCP | `hirame-migrate` |
| `versitygw` | `GET /health` answers | `versitygw-bootstrap` |
| `tika` | `GET /tika` answers | `indexer` (`Wants=`) |
| `overwatch` | `overwatch client status` answers on the socket | `indexer` (`Wants=`) |
| `search-api` | `GET /healthz` answers | `web-gui` |
| `web-gui` | the SPA shell is served | — |

`gahaku.service` and `indexer.service` have no healthcheck: gahaku's readiness
is a gRPC port with no unauthenticated probe endpoint, and the indexer is a
worker loop with no listener. Both are `Wants=` dependencies rather than
`Requires=` ones, because River retries — a job that cannot reach Tika or the
renderer fails and is retried, which is a better failure than a worker pool that
refuses to start and leaves the queue unattended.

This is a deliberate divergence from `gahaku/deployment/quadlet/versitygw.container`,
which omits `Notify=healthy` so that an unhealthy gateway is visible in
`podman ps` without a failed timer being able to fail the boot. Here the
ordering has to be real: a bucket created against a gateway that is not
answering, or a migration run against a database that is not, is the failure
mode D-005 exists to prevent.

## Volumes, ownership, and backup

| Volume | Mounted at | Owned by | Backup |
| --- | --- | --- | --- |
| `hirame-db` | `/var/lib/postgresql` in `postgres` | uid 999 (`postgres`) inside the container's user namespace | **required** |
| `hirame-objectstore` | `/data` in `versitygw` | the gateway's own uid | not required |
| `hirame-overwatch-run` | `/run/overwatch/hirame` in `overwatch` and `indexer` | root inside the container; socket mode `0660` | never |

**`hirame-db` is the only irreplaceable one.** It holds the search index, the
River queue, the filesystem observations, the document tombstones (D-007), and
the thumbnail-cache accounting. Back it up with a logical or base backup against
the running server, not by copying the directory out from under it:

```sh
podman exec postgres pg_dump --format=custom --dbname=hirame > hirame-$(date +%F).dump
```

**`hirame-objectstore` is a cache, not a system of record.** It holds only
thumbnails (D-008) and every one of them can be re-rendered from its source
document, so it does not need backing up. It does have to survive a restart: the
accounting that decides what to evict lives in PostgreSQL and would otherwise
describe objects that are gone. If the volume is lost, the honest recovery is to
clear the cache accounting rows so the next request re-renders.

VersityGW's posix backend keeps bucket and object metadata in `user.*` extended
attributes. A filesystem that drops them surfaces as metadata errors from
ordinary S3 calls that mention nothing about xattrs — worth knowing before
moving this volume onto exotic storage.

**`hirame-overwatch-run` is runtime state.** The daemon is stateless by design
and removes its socket on exit.

## overwatch, CAP_SYS_ADMIN, and D-009

`overwatch.container` is the one unit in this deployment that rootless podman
cannot fully satisfy. fanotify *filesystem* marks require `CAP_SYS_ADMIN` in the
**initial** user namespace. `AddCapability=CAP_SYS_ADMIN` grants it inside the
container's namespace, which is what a rootful podman needs and is not what a
rootless one can give: under rootless the daemon starts and then refuses with a
clear error, which is its documented behaviour rather than a silent degradation.

D-009 anticipates exactly this — "escalate to system units only if a real
mountpoint's permissions demand it". Two supported ways out, both of which leave
every other unit unchanged:

1. **Install the whole unit set as system units.** Copy `deploy/quadlet/` to
   `/etc/containers/systemd/` instead, drive it with `systemctl` rather than
   `systemctl --user`, and put the configuration where `%h` then resolves
   (`/root/.config/hirame/`). The shared volume between `overwatch` and
   `indexer` keeps working because the whole stack moved together. This is the
   smaller change and the one to prefer.
2. **Run the daemon on the host** per
   `go-overwatch/overwatch/doc/ops/systemd.md`, which uses
   `AmbientCapabilities=CAP_SYS_ADMIN` to run it as a non-root service account,
   and leave the rest of the stack rootless. Then drop `overwatch.container` and
   `hirame-overwatch-run.volume`, and replace the socket volume in
   `indexer.container` with a bind mount of the host runtime directory:

   ```ini
   Volume=/run/overwatch/hirame:/run/overwatch/hirame:ro
   ```

   The socket is mode `0660` owned by the daemon's host user, so this remedy
   also has to change `User=10001:0` in `indexer.container`: group 0 is the
   container-root group the co-located daemon's socket carries, and a host
   daemon's socket carries the host group instead. Put the rootless service user
   in that group and set the unit's gid to its container-side id.
   `go-overwatch/overwatch/doc/ops/containers.md` covers the group membership
   and SELinux labelling this needs.

This is a decision that outlives the deployment files, and it is not one this
directory can settle on its own.

`indexer.container` runs as `User=10001:0` for the same reason: the socket is
`0660` owned by whoever runs the daemon, and joining group 0 is what lets the
image's unprivileged `hirame` user connect without also being uid 0.

## Network exposure (D-010)

`hirame.network` is `Internal=true`: nothing on it reaches the internet and
nothing off the host reaches it. Image pulls are unaffected — they happen on the
host's network before a container joins.

The single published port belongs to `hirame.pod`. **Whether that survives
`Internal=true` is the deployment's one unverified assumption** — no container
could be started in the environment these files were written in. The reasoning
is that a rootless publish is made by `rootlessport` from inside the rootless
network namespace to the pod's address, so it never crosses the boundary
`Internal=true` closes; `podman network inspect` confirms the bridge still gets
a gateway address with `--internal`. A *rootful* publish is host DNAT instead,
which netavark's internal rules may drop — which matters for both escalation
option 1 below and for the podman-in-podman harness. If `:8080` does not answer
while every unit is healthy, `hirame.network` is the first suspect and carries
the two fallbacks inline.

Inside the pod, `search-api` binds `127.0.0.1:8081` and Caddy reaches it over
loopback, which is what makes the same-origin arrangement real rather than
conventional. The proxy uses `handle /api/*` and **not** `handle_path`: the
server mounts every generated service under `/api` itself and strips the prefix
in its own handler, so a proxy that also stripped it would turn every RPC into a
404. `apps/web-gui/Caddyfile`, `internal/server/server.go`'s `APIPrefix`, and
`src/api/transport.ts`'s `API_BASE_URL` all have to say `/api`.

There is no user authentication in this version. The deployment is for trusted
networks; put an authenticating reverse proxy in front of `:8080` if that is not
true of yours, and narrow the pod's `PublishPort=` to `127.0.0.1:8080:8080` when
you do.

`gahaku.container` sets `GAHAKU_INPUT_ALLOW_HTTP=true` and
`GAHAKU_INPUT_BLOCK_PRIVATE_ADDRESSES=false`. Both are guards the binary turns
on by default and both have to come off for a gateway that speaks plain http on
a podman network. The cost is that gahaku will fetch and upload to any url a
caller names, including ones pointing back into `hirame.network`; that is
acceptable here only because the only callers are `search-api` and `indexer`,
which sign every url themselves.

## Verifying a change to these units

The generator can be run without systemd, which catches every syntax and key
error before an install does:

```sh
QUADLET_UNIT_DIRS=deploy/quadlet \
  /usr/libexec/podman/quadlet -dryrun -user
```

(The binary ships with podman; on installations that place it elsewhere, look
for `libexec/podman/quadlet` under the podman prefix.) Read the generated
`[Service]` of `hirame-migrate.service` after any edit to it: it must keep
`Type=oneshot` and its `ExecStart` must not carry `-d`, or the migration gate
above stops enforcing anything.

`deploy/test/` is where the podman-in-podman end-to-end harness goes (D-013).
