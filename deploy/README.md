# Deploying hirame

Rootless Podman with Quadlet in a dedicated service user's **user** systemd
instance, plus one native **system** service for the watcher (D-009). One
internal network, one pod for the application tier, one published port.

Two systemd instances, because one process needs a capability no rootless
container can hold and nothing else does. `systemctl --user` as `hirame` drives
the containers; `sudo systemctl` drives `overwatch.service` alone.

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
│   └─────────────────────────────────────────┘                        │
│         hirame-migrate (oneshot, init — on the network, not in the   │
│         pod: the pod's exit-policy would stop it when the oneshot,   │
│         its only running member on a cold start, exits)              │
└──────────────────────────────────────────────────────────────────────┘
      overwatch — not a container and not rootless: a host binary under the
      system systemd, sharing /run/overwatch/hirame with the indexer by bind
      mount and by group ownership
```

## Units

Everything under `quadlet/` is a **user** unit of the `hirame` account.
Everything under `systemd/` is root's.

| Unit | Generated service | What it is |
| --- | --- | --- |
| `quadlet/hirame.network` | `hirame-network.service` | the internal podman network `hirame` |
| `quadlet/hirame.pod` | `hirame-pod.service` | the application pod; owns the published port |
| `quadlet/hirame-db.volume` | `hirame-db-volume.service` | PostgreSQL's data directory |
| `quadlet/hirame-objectstore.volume` | `hirame-objectstore-volume.service` | VersityGW's posix backend |
| `quadlet/postgres.container` | `postgres.service` | ParadeDB PostgreSQL, healthy when `pg_isready` passes |
| `quadlet/migrations/hirame-migrate.container` | `hirame-migrate.service` | the schema-migration init container (D-005) |
| `quadlet/tika.container` | `tika.service` | Apache Tika extraction |
| `quadlet/versitygw.container` | `versitygw.service` | the S3 gateway holding thumbnails |
| `quadlet/versitygw-bootstrap.container` | `versitygw-bootstrap.service` | creates the thumbnail bucket, once |
| `quadlet/gahaku.container` | `gahaku.service` | the render service, gRPC `:9000` |
| `quadlet/search-api.container` | `search-api.service` | the ConnectRPC API |
| `quadlet/indexer.container` | `indexer.service` | filesystem watching and River workers |
| `quadlet/web-gui.container` | `web-gui.service` | the SPA and the API reverse proxy |
| **`systemd/overwatch.service`** | *(itself)* | the fanotify watcher — a host binary, no container, **system scope** (D-009) |
| **`systemd/hirame-overwatch.tmpfiles.conf`** | *(systemd-tmpfiles)* | pre-creates the socket directory the indexer bind-mounts |

Supporting units carry no `[Install]`; they are reached through the `Requires=`
the generator derives from `Network=`, `Volume=`, and `Pod=`. Everything with an
`[Install]` is `WantedBy=default.target` — the user manager's boot target;
`multi-user.target` does not exist in a user instance.

## Where things go

This deployment has two owners, and every path below belongs to one of them.
`hirame` is a dedicated unprivileged service account with a real home directory.

| What | Path | Owner |
| --- | --- | --- |
| Quadlet units | `~hirame/.config/containers/systemd/` (`migrations/` copied as a subdirectory) | `hirame` |
| Non-secret configuration | `~hirame/.config/hirame/hirame.env`, `~hirame/.config/hirame/postgresql.conf` | `hirame` |
| Credentials | `~hirame/.config/hirame/secrets.env`, mode 0600 | `hirame` |
| Podman volumes and images | `~hirame/.local/share/containers/storage/` | `hirame` |
| The native unit | `/etc/systemd/system/overwatch.service` | root |
| The watcher's configuration | `/etc/hirame/overwatch.json` | root |
| The overwatch binary | `/usr/local/bin/overwatch` | root |
| The overwatch socket | `/run/overwatch/hirame/hirame.sock`, from `RuntimeDirectory=` | `root:hirame` |
| The documents to index | `/srv/documents` — **the same path inside the containers** | any, `hirame`-group-readable |

The units name the user paths as `%h/.config/hirame/...` rather than spelling
out `/home/hirame`. Quadlet passes `%h` through into the generated `ExecStart=`
untouched and systemd expands it when the unit is loaded, so the same files
work for a service account whose home is somewhere else.

`~hirame/.config/containers/systemd/` rather than
`/etc/containers/systemd/users/<uid>/`: both are read by the user manager, but
the home-relative one needs no root to install or update, keeps the units in
the same tree as the store they resolve images against, and does not bake a
numeric uid into a path that then has to survive a rebuild of the account.

`/etc/hirame/` still exists and holds exactly one file, `overwatch.json`. That
is the system half of the configuration, and it belongs where a system service's
configuration belongs; the service user has no business owning it, and the
daemon — running as root — has no business reading configuration out of an
unprivileged user's home. `--config` is first in that daemon's resolution order,
so nothing is left to a default.

**`/srv/documents` is one path in three places and they must stay equal:** the
host directory `overwatch.service` watches, `watch[].root` in
`deploy/config/overwatch.json`, and both sides of the `Volume=` line in
`indexer.container` and `search-api.container`. The daemon is on the host and
reports the paths it observed there; the containers resolve them in their own
mount namespace. Point the deployment at a real archive by editing all of them
together — `Volume=/mnt/archive:/mnt/archive:ro`, not a rewrite to a tidier
container path.

**The archive has to be readable by the `hirame` group**, which is what both
application containers reach it through:

```sh
sudo chgrp -R hirame /srv/documents
sudo chmod -R g+rX   /srv/documents
```

`indexer.container` and `search-api.container` both set `User=10001:0`, and
under rootless podman container gid 0 is the service user's primary group. The
alternative would be a world-readable archive; the section on the socket handoff
below explains why gid 0 lands there and why it is the same mechanism that
opens the socket.

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
| `docker.io/library/golang:1.26-bookworm` + `docker.io/library/debian:bookworm-slim` | build and runtime stages of `hirame-search-api` |

Three images are built here and published nowhere; the units name them
`localhost/...` so a missing build fails loudly instead of pulling something
from docker.io. Build them **as the `hirame` user**, with plain unprivileged
podman — that user's store is the one the units resolve against, and an image
built into any other store is invisible to them:

```sh
podman build --tag hirame-search-api:latest --file apps/search-api/Containerfile .
podman build --tag hirame-web-gui:latest    apps/web-gui
podman build --tag gahaku:latest            gahaku
```

No `sudo`, anywhere: `sudo podman build` writes into root's store, which
nothing in this deployment reads. Getting a shell as the service user needs a
little care, because rootless podman wants a session with `XDG_RUNTIME_DIR`
set and `sudo -u hirame podman ...` does not give it one:

```sh
sudo machinectl shell hirame@.host    # or: sudo -iu hirame, after enable-linger
```

The build context has to be readable by that user too — a repository checkout
under another user's home directory is the usual way this fails.

`gahaku` builds from the submodule's own `Dockerfile` — it is not vendored, and
its image is LibreOffice-sized.

There is deliberately no overwatch image: the daemon is a host binary now, and
`deploy/overwatch/Containerfile` was deleted with the container unit. `just
build` builds the repository's own Go and TypeScript sources; the image builds
above are not justfile recipes because they are a deployment step rather than a
development one.

## Install

Four steps, and the order matters: the service account has to exist before
`overwatch.service` starts, because that unit's `Group=` names it. Nothing here
wants a privileged port.

The procedure is scripted in two halves, split so the target never needs a
toolchain or a checkout. `deploy/build.sh` runs wherever podman and go are
installed — any user, no root, no service account — and writes a
self-contained bundle to `deploy/dist/`: the overwatch binary, the three
images as archives, the units and templates, and a copy of the deploy script.
Copy that one directory to the target and run the script inside it:

```sh
deploy/build.sh
scp -r deploy/dist target:hirame
ssh target sudo ./hirame/deploy.sh
```

`deploy.sh` does everything host-side: it creates the account, installs the
daemon and the units, loads the images into the service user's store, and
starts the stack. Re-running it is the redeploy path — it always refreshes
code (the binary, every unit, the three images) and never overwrites
configuration or credentials it finds already installed.
`sudo deploy/install.sh` runs both halves on one host. The steps below remain
the reference for what they do and why.

### Appliance hosts (TrueNAS SCALE and similar)

Verified end-to-end on TrueNAS SCALE 25.10 in a VM. Appliance systems ship no
podman and mount `/usr` read-only, so podman comes from a static dist (e.g.
podman-static via podman-static-dist) wired into the service user's home, and
deploy.sh's path knobs point at the writable paths:

```sh
sudo env \
  PODMAN=/var/lib/hirame/.local/share/podman-dist/current/usr/local/bin/podman \
  OVERWATCH_BIN=/var/lib/overwatch/overwatch \
  DOCS_DIR=/mnt/documents \
  ./hirame/deploy.sh
```

A non-default `DOCS_DIR` must match the same path edited into
`overwatch.json`, `hirame.env`, and the `Volume=` lines — "Where things go".

Two more knobs cover an account layout that is not this document's default.
`SERVICE_USER` (default `hirame`) names the account the rootless stack runs
as, for a host that already has a service account of its own. The group is
**not** a second knob: deploy.sh derives the account's primary group and
rewrites `Group=` in the `overwatch.service` it installs, and the group column
of the `tmpfiles.d` entry with it, because those two are the socket handoff —
"The socket handoff", below. `QUADLET_DIR` (default
`~SERVICE_USER/.config/containers/systemd`) moves the unit install directory;
a non-default one has to be a directory the generator searches, which is the
`QUADLET_UNIT_DIRS` point below.

Platform constraints found the hard way:

* `/home` is mounted noexec — create the service user with
  `--home-dir /var/lib/hirame` before running deploy.sh, or rootless podman
  cannot exec its helpers from `~/.local`.
* systemd looks for user generators only in `/run/systemd/user-generators`,
  `/etc/systemd/user-generators`, `/usr/local/lib/...` and `/usr/lib/...` —
  never in the user's home. With `/usr` immutable, symlink the dist's quadlet
  binary to `/etc/systemd/user-generators/podman-user-generator`; deploy.sh
  refuses to run until some such generator exists.
* Static podman builds carry no `systemd` build tag, and such a podman never
  schedules healthcheck timers — silently, so every `Notify=healthy` unit
  hangs in activating. deploy.sh installs and enables
  `hirame-healthcheck-driver.timer` in the service user's manager, which
  drives `podman healthcheck run` itself; on a full podman build it is
  redundant but harmless.
* The dist's `QUADLET_UNIT_DIRS` must include
  `~/.config/containers/systemd` (an `environment.d` drop-in on the service
  user), or the generator reads only the dist's own unit directory.

All of that is scripted for one appliance in `deploy/truenas/`, against a
TrueNAS SCALE 25.10 VM built from a golden qcow2 image. `01-vm.sh up` →
`02-kit.sh` → `03-podman.sh` → `04-deploy.sh`, run in that order, allocate the
VM, kit its pool and accounts, install the static podman dist, and deploy the
stack into it; nothing but `TRUENAS_ADMIN_PASSWORD` — or its default — has to
be exported. Each script's header lists what it assumes and the knobs it reads.

### 1. The service user

```sh
sudo useradd --create-home --shell /bin/bash hirame
sudo loginctl enable-linger hirame
grep '^hirame:' /etc/subuid /etc/subgid
```

Four things about that account are load-bearing:

- **A real shell, not `/usr/sbin/nologin`.** This is a service account nobody
  logs into over the network — give it no password and it has no interactive
  route in — but the install below, the image builds, and every later
  `systemctl --user` are all *run as this user* from a root shell, and every way
  of getting there (`machinectl shell`, `su -`, `sudo -i`) launches the shell
  recorded in `/etc/passwd`. With `nologin` they all exit immediately. Keeping
  `nologin` is defensible if you would rather pass the shell explicitly every
  time — `sudo machinectl shell hirame@.host /bin/bash` — but then it has to be
  every time.

- **`--create-home`.** The units resolve `%h`. A `--no-create-home` account —
  which is what `useradd --system` gives you, and the obvious thing to reach
  for — has no home directory, and every `EnvironmentFile=` in the deployment
  then names a path that does not exist.
- **Subordinate id ranges.** Rootless podman cannot start a container without
  them, and `--system` accounts are usually skipped by the automatic
  allocation. If the `grep` prints nothing, allocate a range **that nothing
  else holds** — read `/etc/subuid` and `/etc/subgid` first, because
  `usermod` will happily hand out an overlapping one and two accounts sharing a
  subuid range means neither is isolated from the other:

  ```sh
  cat /etc/subuid /etc/subgid          # 100000-165535 is the usual first user's
  sudo usermod --add-subuids 200000-265535 --add-subgids 200000-265535 hirame
  ```

  The range has to be **wider than 10001**, because `indexer.container` and
  `search-api.container` run as container uid 10001 and a uid outside the
  mapping cannot be resolved. The customary 65536 is ample; a hand-cut range of
  a few thousand is not.
- **Lingering.** `enable-linger` is what starts the user manager at boot and
  keeps it alive with no session open. Without it the whole stack stops when
  the last shell as `hirame` exits, and never comes back after a reboot.

### 2. The overwatch daemon (host binary, system service)

Build it from the submodule checkout, whose revision this repository's index
records (D-006):

```sh
cd go-overwatch/overwatch
GOWORK=off go build -trimpath -ldflags '-s -w' -o /tmp/overwatch ./cmd/overwatch
sudo install -Dm0755 /tmp/overwatch /usr/local/bin/overwatch
```

`GOWORK=off` because `go.work` deliberately excludes the submodules, so a `go`
command run inside one reports "directory prefix . does not contain modules
listed in go.work" without it.

**`go install github.com/ngicks/go-overwatch/overwatch/cmd/overwatch@latest`
does not work**, and not for a version-pinning reason: the module zip contains
`e2e/vm-truenas/con.py`, and `con` is a reserved path component on Windows, so
the module proxy and `sum.golang.org` refuse to serve it at all. The submodule
checkout is the only source. (`deploy/test/fakeoverwatch/go.mod` carries the
same `replace` for the same reason.)

Then the configuration and the unit:

```sh
sudo install -d -m 0755 /etc/hirame
sudo install -Dm0644 deploy/config/overwatch.json    /etc/hirame/overwatch.json
sudo install -Dm0644 deploy/systemd/overwatch.service /etc/systemd/system/overwatch.service
sudo install -Dm0644 deploy/systemd/hirame-overwatch.tmpfiles.conf \
                     /etc/tmpfiles.d/hirame-overwatch.conf
sudo systemd-tmpfiles --create /etc/tmpfiles.d/hirame-overwatch.conf
sudo systemctl daemon-reload
sudo systemctl enable --now overwatch.service
sudo systemctl status overwatch.service
sudo -u hirame /usr/local/bin/overwatch client \
     --socket /run/overwatch/hirame/hirame.sock status
```

The last command is the grant under test, which is why it runs as `hirame`
rather than as root: root reaches that socket whatever the permissions say.

`systemd-tmpfiles --create` is only needed the first time — the entry is applied
at every boot by `systemd-tmpfiles-setup.service`, which is what makes the
directory exist before the `hirame` user manager starts. The readiness section
explains why that matters more than it looks.

The unit's shape follows `go-overwatch/overwatch/doc/ops/systemd.md`; the
section below records where it diverges and why. `Group=hirame` in it is the
socket handoff, and it fails to start if step 1 has not run.

Check the ownership it produced before going further — this is the one thing
that silently degrades rather than failing loudly:

```sh
$ stat -c '%U:%G %a %n' /run/overwatch/hirame /run/overwatch/hirame/hirame.sock
root:hirame 750 /run/overwatch/hirame
root:hirame 660 /run/overwatch/hirame/hirame.sock
```

`overwatch.json` is JSON and cannot carry its own comments, so its three
non-default choices are recorded here. `listen.unix` is written out rather than
derived from `instance`, so the socket path is fixed by configuration instead of
by a naming rule the bind mount would have to track. `events` drops `attrib`
from the daemon's default set: permission and xattr changes produce no work in
this pipeline, and `close_write` is the signal the debounce interval is for.
`limits.syscalls_per_sec` is 200 rather than the default 1000, which is the
budget go-overwatch's README recommends for HDD- or NFS-backed paths; raise it
toward the default on local SSD, where the conservative value only makes the
first full scan slower.

### 3. The Quadlet units, configuration, and credentials

**Everything from here on runs as `hirame`**, with no `sudo` at all:

```sh
sudo machinectl shell hirame@.host
```

The units:

```sh
mkdir -p ~/.config/containers/systemd
cp -r deploy/quadlet/. ~/.config/containers/systemd/
```

The `migrations/` subdirectory is copied as-is; the generator recurses into
subdirectories of a unit directory, and the layout is the one AGENTS.md
prescribes.

Non-secret configuration:

```sh
install -Dm0644 deploy/config/hirame.env.template ~/.config/hirame/hirame.env
install -Dm0644 deploy/config/postgresql.conf     ~/.config/hirame/postgresql.conf
```

`postgresql.conf` stays 0644 on purpose. It is bind-mounted and read by the
container's *mapped* uid, which is a subuid unrelated to `hirame`, so tightening
it to 0600 stops PostgreSQL from reading its own configuration.

Credentials:

```sh
sed -e "s/__DB_PASSWORD__/$(openssl rand -hex 24)/g" \
    -e "s/__S3_ACCESS_KEY__/$(openssl rand -hex 16)/g" \
    -e "s/__S3_SECRET_KEY__/$(openssl rand -hex 32)/g" \
    deploy/config/secrets.env.template |
  install -Dm0600 /dev/stdin ~/.config/hirame/secrets.env
```

0600 owned by `hirame` is enough, and — unlike `postgresql.conf` — the
container's mapped uid does not need to read it: quadlet turns
`EnvironmentFile=` into podman's `--env-file`, which the rootless podman
process opens as `hirame` before the container exists.

The units read `secrets.env` with a plain `EnvironmentFile=` and no `-` prefix:
a service that came up without credentials would be a worse outcome than a unit
that refuses to start. `gahaku/deployment/quadlet/README.md` gives the podman
`Secret=` form for deployments that would rather not keep a file, and the
trade-off that comes with it.

Point the deployment at the documents to index: create `/srv/documents`, give it
the group ownership described under "Where things go", or edit every occurrence
of the path as described there.

### 4. Generate and start

```sh
systemctl --user daemon-reload
systemctl --user start hirame-pod.service
systemctl --user status 'hirame*' 'postgres*' 'tika*' 'versitygw*' 'gahaku*' \
                        'search-api*' 'indexer*' 'web-gui*'
```

`daemon-reload` is what runs the quadlet generator; `systemctl --user cat
search-api.service` shows what it produced, including the `%h` expansions.
Every generated unit with an `[Install]` is `WantedBy=default.target`, so a
`daemon-reload` alone already schedules them for the next boot — which, with
lingering enabled, is a real boot rather than the next login.

`overwatch.service` is not in that glob because it is not this user's: it is
`sudo systemctl status overwatch.service`, the one unit outside the user
instance.

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
re-run migrations. Stop the gate and the applications together, then start the
applications — their `Requires=` pulls a fresh migration run in, ordered.
One stop transaction, one start transaction; two sequential `restart` commands
race, the second cancelling the first's still-running migration job:

```sh
systemctl --user stop hirame-migrate.service search-api.service indexer.service web-gui.service
systemctl --user start search-api.service indexer.service web-gui.service
```

The migration container runs on `hirame.network`, not in the pod, although its
gate protects the pod's applications: the generated pod carries
`--exit-policy stop`, and on a cold start the ordering above makes the oneshot
the only running pod member — as a member, its exit would stop the pod out
from under the applications about to join it.

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
| `overwatch` | **not expressed, and not orderable at all** — see below | nothing |
| `search-api` | `GET /healthz` answers | `web-gui` |
| `web-gui` | the SPA shell is served | — |

`overwatch.service` is the one edge with no systemd relationship whatsoever,
and it lost it twice over. As a Quadlet unit it could be probed with `overwatch
client status` and gated behind `Notify=healthy`; as a native unit it cannot,
because the daemon implements no sd_notify protocol, so `Type=simple` means
"started" is "forked". And now that the container stack runs in a *user*
instance, even the bare ordering is gone: **a user unit cannot express any
dependency on a system unit.** `After=overwatch.service` in `indexer.container`
would name a user unit that does not exist, so it was removed rather than left
to look like a guarantee.

Three things make that acceptable rather than a regression:

- **The daemon's own restart contract.** It emits a `STREAM_START` gap marker
  to every subscriber on start, and clients are required to `Scan` all watched
  roots after reconnecting. `internal/ingest` does. A subscriber that connects
  too early is therefore in the same position as one that reconnects after a
  daemon restart — a case the pipeline has to handle regardless.
- **The dial is lazy and retried.** `OVERWATCH_SOCKET` is opened on first use
  and retried on `WATCH_RECONNECT_BACKOFF`, so an indexer that starts first
  loses time, not events.
- **The bind source is guaranteed by something earlier than either unit.**
  `deploy/systemd/hirame-overwatch.tmpfiles.conf` creates
  `/run/overwatch/hirame` in `systemd-tmpfiles-setup.service`, which runs before
  `basic.target` and therefore before any lingering user manager.

That last one is what the removed ordering used to buy, and it matters more
under rootless than it did before. A container whose bind source is missing is
not a container that mounts an empty directory here: `/run` belongs to root, so
the rootless podman cannot create the missing path at all and the unit fails to
start — and `Restart=always` would exhaust its start limit in about a second and
leave `indexer.service` failed permanently. Pre-creating the directory removes
the race instead of racing it. `RuntimeDirectory=` in `overwatch.service` still
declares the same path (systemd adjusts the existing directory to the unit's
owner and mode rather than replacing it), and `RuntimeDirectoryPreserve=yes`
keeps the inode alive across a daemon restart that a running container has
already mounted.

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

The volumes belong to the `hirame` user's podman, under
`~hirame/.local/share/containers/storage/volumes/`. **Every uid in them is a
subuid, not a host uid.** What PostgreSQL writes as uid 999 inside the container
is `subuid_start + 998` on the host — 100998 with the range suggested above —
and the same is true of every file the gateway writes.

| Volume | Mounted at | Owned by (in-container) | Backup |
| --- | --- | --- | --- |
| `hirame-db` | `/var/lib/postgresql` in `postgres` | uid 999 (`postgres`) | **required** |
| `hirame-objectstore` | `/data` in `versitygw` | the gateway's own uid | not required |

That is what makes a filesystem-level backup the wrong tool here, over and above
the usual reason not to copy a live data directory: a `tar` taken as root
records subuids that mean nothing on a restore host with a different allocation,
and one taken as `hirame` cannot read the files at all. Both correct answers go
through podman as the service user — `podman unshare` enters the mapping,
`podman volume export` writes a namespace-independent archive.

**`hirame-db` is the only irreplaceable one.** It holds the search index, the
River queue, the filesystem observations, the document tombstones (D-007), and
the thumbnail-cache accounting. Back it up with a logical or base backup against
the running server, not by copying the directory out from under it — as
`hirame`, with no `sudo`:

```sh
podman exec postgres pg_dump --format=custom --dbname=hirame \
  > hirame-$(date +%F).dump
```

A logical dump is portable across subuid allocations by construction, which is
the other reason to prefer it here.

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

There is no volume for the overwatch socket any more. `RuntimeDirectory=` puts
it on `/run`, which is a tmpfs the daemon is stateless against: nothing there
survives a reboot, and nothing there needs to.

## overwatch, CAP_SYS_ADMIN, and D-009

fanotify **filesystem** marks require `CAP_SYS_ADMIN` in the **initial** user
namespace. That single fact decided the whole shape of this deployment:

- No rootless container can grant it. `AddCapability=CAP_SYS_ADMIN` grants it
  inside the container's namespace, which is not the initial one, and the daemon
  then refuses to start with a clear error rather than degrading silently.
- A *rootful* container could grant it — and buys nothing. A watcher that must
  see the host's mounts, hold host capabilities, and publish a socket the host
  shares back into another container is a host process wearing an image.

So overwatch is a **native systemd service** running `/usr/local/bin/overwatch`
under the system instance, and it is the *only* thing here that is not rootless.
Everything else obeys the rest of D-009: no rootful podman anywhere.

`deploy/systemd/overwatch.service` follows go-overwatch's own
`doc/ops/systemd.md` with three deliberate divergences:

| Divergence | Why |
| --- | --- |
| `User=root` with `Group=hirame`, rather than the doc's variant A (root, root group) or variant B (a dedicated account) | uid 0 is not negotiable — `CAP_SYS_ADMIN` in the initial namespace is the whole reason this is not a container. The *group* is free, and giving it to the rootless service user is what hands the socket to the indexer. The next section is the argument in full. |
| `CapabilityBoundingSet=CAP_SYS_ADMIN CAP_DAC_READ_SEARCH`, not `CAP_SYS_ADMIN` alone | The doc bounds a non-root account, which never had root's DAC bypass. Bounding a *root* process to `CAP_SYS_ADMIN` alone takes that bypass away, and it then cannot traverse archive directories it does not own. `CAP_DAC_READ_SEARCH` restores exactly the read-traverse half and nothing else. |
| `ProtectHome=read-only`, not `ProtectHome=yes` | The doc's own caution: `yes` hides `/home`, `/root` and `/run/user`, which silently empties a watch root pointed at one. Read-only keeps such a root watchable, and watching never writes. |

## The socket handoff: how a rootless container reaches a root daemon's socket

This is the one seam between the two privilege domains, and it is the part of
the deployment most likely to be broken by a well-meaning edit, so the reasoning
is written out rather than left in the units.

**What is fixed and cannot be configured.** The daemon creates its instance
directory with `os.MkdirAll(dir, 0750)` and then unconditionally
`os.Chmod(socketPath, 0660)` on every start
(`go-overwatch/overwatch/pkg/overwatch/grpcserver/grpc_server.go:224,240`).
`ListenConfig` in `pkg/overwatch/config.go:21-25` carries `unix` and `tcp` and
nothing else — there is **no socket mode or group option in `overwatch.json`**.
So "make the socket 0666 and let the directory be the gate" is not available at
any price: the daemon would chmod it back to 0660 on its next restart.

What is left is ownership, and the socket is owned by whoever runs the daemon.

**The grant, in three steps.**

1. `overwatch.service` sets `Group=hirame` (keeping `User=root`). systemd owns
   the **innermost** `RuntimeDirectory=` by `User=`:`Group=`, so
   `/run/overwatch/hirame` is `root:hirame` at `RuntimeDirectoryMode=0750`. The
   daemon's own `MkdirAll` then finds it already there and changes nothing.

   The parent `/run/overwatch` is not covered by that guarantee — systemd
   applies `Group=` to the last component, not to directories it creates along
   the way — so the traverse bit on it comes from the `tmpfiles.d` entry, which
   pins it to `0755 root root`. Anyone can traverse it; only the instance
   directory below is gated.
2. The daemon creates the socket with that egid and chmods it: `root:hirame`,
   mode `0660`.
3. `indexer.container` and `search-api.container` run `User=10001:0`, and
   podman's **default rootless mapping** maps container gid 0 to the primary
   group of the user running podman. That is `hirame`. Group gets `r-x` on the
   directory and `rw-` on the socket, so `connect(2)` succeeds.

**Why the container's uid is irrelevant, and the trap that follows.** Container
uid 10001 maps to `subuid_start + 10000`, a host uid that owns nothing and is
matched by no ACL. Only the gid does any work. This is the exact parallel of the
rejected rootful arrangement, where container gid 0 *was* host gid 0 and the
socket was `root:root`; the mapping moved, the mechanism did not.

The trap is `--userns=keep-id`. It is the natural reflex when a rootless
container cannot read a host file — and it inverts the mapping this depends on:
keep-id maps the service user's real uid and gid to *themselves* inside the
container and pushes container gid 0 out into the subgid range. `User=10001:0`
would then resolve to a subgid that owns nothing, every watcher RPC would fail
behind the reconnect backoff, and the unit would stay `active` throughout. **Do
not add `--userns=keep-id` to these units.** If the problem being solved is
reading `/srv/documents`, the answer is the `chgrp hirame` under "Where things
go", which works through the same gid 0 and costs nothing.

**Two shapes that were considered and are worse.**

- *An ACL on the directory* (`setfacl -m u:hirame:rwx /run/overwatch/hirame`)
  grants the wrong thing: it reaches the directory but not the socket inode,
  which the daemon deletes and recreates `root:<egid> 0660` on every restart.
  A default ACL would have to survive systemd recreating the directory too.
- *A dedicated `overwatch` group with `hirame` added to it* is one more moving
  part for the same result. The container's gid 0 resolves to the service user's
  *primary* group and to no other, so a supplementary group would need
  `--group-add keep-groups` and a crun-specific annotation to be visible at all.

`Group=` must therefore name the service user's **primary** group. If the
account layout differs — a shared `services` group, say — that one line in
`overwatch.service` is what changes, together with the group column of
`hirame-overwatch.tmpfiles.conf` that has to stay equal to it, and nothing
else. deploy.sh does both edits itself: it derives the group from
`SERVICE_USER` and rewrites the copies it installs, leaving the files in the
checkout with the default baked in.

`go-overwatch/overwatch/doc/ops/containers.md` describes the group-membership
route for consumers that are not podman containers.

**On an SELinux-enforcing host the socket bind mount needs labelling**, and it
is orthogonal to everything above: the DAC grant can be perfect and the
connection still refused. The daemon's runtime directory is created by systemd
and carries `var_run_t`, and a container process (`container_t`) is not allowed
to connect to a socket under that label.
The symptom is the quietest one this deployment has, an indexer whose every
watcher RPC fails behind the reconnect backoff while the unit stays `active`.
`:z`/`:Z` on the `Volume=` is the wrong reflex here — relabelling a
`RuntimeDirectory=` fights systemd, which restores the label on every restart.
`go-overwatch/overwatch/doc/ops/containers.md` covers what to do instead. The
units as shipped are SELinux-clean only where the policy is permissive or
disabled.

## Why not rootful

Rootful system-scope Quadlet was briefly adopted and reverted the same day
(D-009). It is worth recording what it bought and what it cost, because the
question comes back every time the two systemd instances are inconvenient.

What it bought was one management plane and one arithmetic shortcut: container
gid 0 was host gid 0, so a socket left at its default `root:root` was already
reachable and no service account had to exist. That shortcut is the *only* thing
the rootless arrangement has to replace, and `Group=hirame` replaces it in one
line — the same mechanism, one indirection further along.

What it cost:

- **Every container ran as real root on the host.** A container escape in Tika's
  parser registry or a LibreOffice subprocess is a host compromise rather than a
  compromise of one unprivileged account whose subuids own nothing.
- **The image builds needed `sudo`,** so the deployment's images lived in root's
  store and could not be built or inspected by whoever operates it.
- **The published port became an open question.** A rootful publish is host DNAT
  into the bridge, which netavark's `Internal=true` rules may drop; the rootless
  path forwards from inside the network namespace through `rootlessport` and
  never crosses that boundary. See the next section.

The one thing that genuinely does not fit in the rootless model is fanotify, and
that is exactly the one thing left outside it.

Operating two instances is the real cost of this arrangement, and it is worth
stating plainly: `systemctl --user` as `hirame` for the containers,
`sudo systemctl` for `overwatch.service`, and no systemd dependency can be
expressed between them.

## Network exposure (D-010)

`hirame.network` is `Internal=true`: nothing on it reaches the internet and
nothing off the host reaches it. Image pulls are unaffected — they happen on the
host's network before a container joins.

The single published port belongs to `hirame.pod`, and rootless is the
arrangement in which that is least surprising: a rootless publish is
`rootlessport` forwarding from inside the network namespace, which never crosses
the boundary `Internal=true` closes. (A *rootful* publish is host DNAT into the
bridge, which netavark's internal rules can drop — one of the reasons that
arrangement was not kept.)

Still unverified rather than proven: `podman network create --internal` was
observed to report a subnet and a gateway (`10.89.0.0/24`, gateway
`10.89.0.1`), so the bridge stays addressable, but no container could be started
in the environment these files were written in — the podman-in-podman harness
(D-013) runs neither this network nor the pod.

If `:8080` does not answer while every unit is healthy, `hirame.network` is the
first suspect and carries the two fallbacks inline: drop `Internal=true` (the
isolation D-010 asks for is that internal services publish no ports, which every
other unit already satisfies on its own), or add a second, non-internal network
that only `hirame.pod` joins.

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

`gahaku.container` leaves `GAHAKU_INPUT_ALLOW_HTTP` and
`GAHAKU_INPUT_BLOCK_PRIVATE_ADDRESSES` at their defaults, which is to say the
input guards are on. They would have to come off for a presigned-url flow
against a gateway that speaks plain http on a podman network — and nothing uses
that flow: `search-api` and `indexer` stream document bytes to gahaku over gRPC
and never hand it a url, and `objstore`'s `PresignGet`/`PresignPut` have no
caller. Relaxing them bought nothing while leaving gahaku willing to fetch and
upload to any url a caller named, including ones pointing back into
`hirame.network`. Set both if that flow ever gains a caller.

## Verifying a change to these units

The generator can be run without systemd, which catches every syntax and key
error before an install does:

```sh
QUADLET_UNIT_DIRS=$PWD/deploy/quadlet \
  /usr/libexec/podman/quadlet -user -dryrun
```

`-user` because these are user units. It changes exactly one thing in this unit
set — the network ordering the generator synthesises,
`Wants=/After=podman-user-wait-network-online.service` instead of
`network-online.target` — which was measured by diffing the two runs, not
assumed. `QUADLET_UNIT_DIRS` must be absolute: the generator resolves drop-in
directories against it and silently finds none for a relative path.

`%h` in the output is **not** an unexpanded-specifier bug. Quadlet passes it
through verbatim into the generated `ExecStart=`, and systemd expands it when
the unit is loaded; `systemctl --user cat search-api.service` is where the real
path becomes visible. (`deploy/test/`'s supervisor has no systemd behind it, so
it expands `%h` itself — see `lib/quadlet-supervisor.py`.)

(The binary ships with podman; on installations that place it elsewhere, look
for `libexec/podman/quadlet` under the podman prefix.) Read the generated
`[Service]` of `hirame-migrate.service` after any edit to it: it must keep
`Type=oneshot` and its `ExecStart` must not carry `-d`, or the migration gate
above stops enforcing anything.

`systemd-analyze verify /etc/systemd/system/overwatch.service` is the
equivalent check for the native unit.

`deploy/test/` is the podman-in-podman end-to-end harness (D-013).
