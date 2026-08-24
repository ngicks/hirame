# DECISION — TrueNAS deployment scripts

Stubs seeded from PLAN.md open questions; each becomes a full entry as it
resolves.

## D1 — instance naming / multiplicity (2026-08-23)

**Chosen:** named, single at a time. `TRUENAS_VM_NAME` (default
`truenas-hirame`) names the libvirt domain and the image subdirectory
`/var/lib/libvirt/images/<name>/`. Ports overridable, no auto-offsetting.
**Rejected:** fixed single name (no knob); full multi-instance with port
offsetting (complexity nobody asked for).

## D2 — forwarded ports (2026-08-23)

**Chosen (user-specified):** passt forwards on `0.0.0.0`: host 10443 →
guest 443 (TrueNAS UI), host 10022 → guest 22 (ssh), host 18080 → guest
8080 (hirame web-gui). One port suffices for hirame: the pod publishes only
8080 and web-gui serves the SPA and proxies `/api` same-origin
(deploy/README.md, "Network exposure").
**Rejected:** 8443/2222/8080 defaults; adding guest 80.

## D3 — runner-user model (2026-08-23, revised same day)

**Chosen:** service user `podman` (home `/mnt/tank/home/podman`) with its
**own primary group `podman`**, supplementary member of `hirame`.
`deploy/deploy.sh` gains a `SERVICE_USER` knob (default `hirame`); the
service group is **derived** (`id -gn "$SERVICE_USER"`), not a second knob.
It rewrites `Group=hirame` in `overwatch.service` to that group — the
adaptation deploy/README.md's socket-handoff section itself prescribes:
"`Group=` must therefore name the service user's **primary** group. If the
account layout differs … that one line in `overwatch.service` is what
changes, and nothing else." One amendment to that quoted claim, found while
planning: `deploy/systemd/hirame-overwatch.tmpfiles.conf:29` also bakes the
group (its comment requires it equal to `Group=`), so deploy.sh rewrites
that line too. The documents-dir group grant is parametrized the same way
(group `podman` on the verification VM).
**Rationale:** the user requires clear privilege separation between the two
accounts, so `podman` must not carry `hirame` as primary group. Container
gid 0 maps to the runner's *primary* group, so the socket and docs grants
follow that group; the supplementary `hirame` membership remains as
requested but its effect is host-side file sharing with the `hirame` user
only — it is not visible inside containers.
**Rejected:** primary group `hirame` (blurs the accounts — first proposal,
overruled at the idea gate); keep `hirame` as runner (drops the requested
separation entirely); `--group-add keep-groups` to pass supplementary
groups into containers (crun-specific annotation, the README's documented
anti-pattern here).
## D4 — kitting transport (2026-08-23)

**Chosen:** ssh (forwarded port 10022) as `truenas_admin` with
`TRUENAS_ADMIN_PASSWORD` (default `truenas-golden-image`), running
`sudo midclt call` for every middleware operation, so all kitted state lands
in the TrueNAS config DB and survives reboots. Later steps need ssh anyway.
**Rejected:** REST/WebSocket API via 10443 (second transport for no gain).

## D5 — quadlet unit directory (2026-08-23)

**Chosen:** parametrize `deploy/deploy.sh`'s unit install dir as
`QUADLET_DIR` (default unchanged: `~SERVICE_USER/.config/containers/systemd`);
on TrueNAS pass `~podman/.config/containers-quadlet-additional`, which the
committed `deploy/truenas/config/75-podman.conf` already lists in
`QUADLET_UNIT_DIRS`. The repo ships that conf verbatim (user decision at the
idea gate: fold the workstation file into the repo).
**Rejected:** symlinking `containers-quadlet-additional` →
`containers/systemd` (indirection); editing 75-podman.conf to append
`containers/systemd` (diverges from the dist convention).

## D6 — /etc persistence shim (2026-08-23)

**Chosen:** a root systemd oneshot, `truenas-prep.service` in
`/etc/systemd/system` (enabled, `Before=user@.service`), executing
`/var/lib/hirame-deploy/truenas-prep.sh`, which each boot re-applies the
`podman:` subuid/subgid entries and re-links
`/etc/systemd/user-generators/podman-user-generator` to the dist's quadlet
binary — because TrueNAS regenerates managed /etc files from its DB at boot
and post-init-script changes were observed reverted. Premise to verify
empirically (plan step 7): `/etc/systemd/system` itself survives reboot; if
not, escalate to the belt-and-braces variant (a TrueNAS postinit entry that
re-installs the unit).
**Rejected as primary:** TrueNAS init/shutdown script alone (observed
reverted); both-at-once up front (complexity before evidence).

## D8 — password ssh without sshpass [automatic] (2026-08-23)

**Chosen:** `lib.sh` feeds `TRUENAS_ADMIN_PASSWORD` to ssh/scp via a
generated `SSH_ASKPASS` helper with `SSH_ASKPASS_REQUIRE=force` (supported
since OpenSSH 8.4; host has 10.5). No sshpass dependency.
**Rationale:** sshpass is absent from the implementation host and the
askpass mechanism is built into OpenSSH itself.
**Rejected:** requiring sshpass (extra dependency); expect/pexpect.

## D9 — privilege helper instead of hardcoded sudo [automatic] (2026-08-23)

**Chosen:** `lib.sh` sets `SUDO=""` when EUID is 0, else `SUDO=sudo`; all
host-side privileged calls (`virsh`, image dir creation) go through it.
**Rationale:** the implementation host runs as root in a container with no
`sudo` binary; workstations keep the sudo path.

## D10 — commit cadence [automatic] (2026-08-23)

**Chosen:** commit per implementation step (ngcommit convention), never
sweeping in the pre-existing dirty `apm.yml` / `apm.lock.yaml` changes,
which predate this run.

## D11 — step 7 environment [automatic] (2026-08-23)

**Chosen:** libvirt daemons are not running on this host (container, no
systemd); start them by hand for the end-to-end run per the
kvm-in-container procedure. shellcheck is fetched via nix for the
verification gate. If the VM run proves infeasible in this environment,
record steps 1–6 as done + verified and step 7 as environment-blocked in
STATUS.md rather than looping.

## D12 — ssh bootstrap over the middleware REST API [automatic] (2026-08-23)

**Found empirically:** the golden image boots with the ssh service disabled,
`ssh_password_enabled: false` for truenas_admin, and sudo password-gated —
all three D4 assumptions false. **Chosen:** 02-kit's preflight bootstraps
them through the middleware REST API on the forwarded UI port (basic auth
with `TRUENAS_ADMIN_PASSWORD`), idempotently: enable password ssh login,
grant `sudo_commands_nopasswd: ["ALL"]`, enable+start the ssh service. The
UI port is the only door initially open, and the grants persist in the
config DB. **Rejected:** rebuilding the golden image (out of scope);
failing with a message (leaves the chain unusable as shipped).

## D13 — systemd-run instead of sudo-descended process trees [automatic] (2026-08-23)

**Found empirically:** TrueNAS's /etc/sudoers carries `Defaults
log_subcmds`, whose intercept machinery kills podman's `/proc/self/exe`
re-exec anywhere below sudo in the process tree
(`sudo: pathname mismatch, expected "/proc/self/exe"`), and /etc/sudoers
has no includedir, so a sudoers.d drop-in is inert. **Chosen:** guest
commands that reach podman (03's as_podman, 04's deploy.sh run and
migrate) go through `sudo systemd-run --wait --pipe`, handing the payload
to PID 1, outside the intercepted tree. **Rejected:** editing
/etc/sudoers (regenerated at boot; policy mutation).

## D14 — extract + link --skip-systemd in 03 [automatic] (2026-08-23)

`podman-static-dist install`'s final step wires the user generator into
/etc with sudo, which the podman account does not have and must not have;
on TrueNAS that symlink is owned by the boot shim anyway. 03 therefore
runs `extract --tar` + `link --skip-systemd --tag <stamp>`; home-side
links (config, environment.d, current) are still made.

## D15 — documents path baked into the bundle by 04 [automatic] (2026-08-23)

deploy.sh's `DOCS_DIR` knob only creates/grants the directory; the path
itself is baked into overwatch.json, hirame.env.template and the two
`Volume=` lines as an operator edit per deploy/README.md. 04 automates
that edit on the staged bundle (and heals already-installed configs that
still point at the shipped default). Same host path on both sides of
`Volume=` because the indexer records host paths.

## D16 — podman system migrate after the shim [automatic] (2026-08-23)

03 starts podman (its own verify) before 04 writes the subid ranges, so
the user namespace carries the fallback single mapping and every container
fails with `setresgid: Invalid argument`. 04 runs `podman system migrate`
right after the shim, tearing the stale pause process down. Idempotent.

## D6 — outcome of the empirical check (2026-08-23, appended)

`/etc/systemd/system` **survives** reboot and the unit runs — but TrueNAS
rewrites /etc/subuid and /etc/subgid again *later in the same boot*
(observed ~4s after the boot-ordered run), wiping the entries. Escalation
applied: `truenas-prep.path` re-triggers the shim when either file is
rewritten, and `truenas-prep.timer` (30s after boot, then every 2min)
sweeps the observed race where the second file is rewritten while the shim
is already running for the first (that edge is lost by the path unit).
The service drops `RemainAfterExit` so it can be re-triggered. Verified:
removing the podman line via rename-replace is healed within seconds.

## D7 — documents location (2026-08-23)

**Chosen (user-specified):** `/mnt/tank/share/documents` — dataset
`tank/share`, plain directory `documents` inside it, passed to deploy.sh as
`DOCS_DIR` and edited into overwatch.json / hirame.env / `Volume=` lines per
deploy/README.md "Where things go". A plain directory keeps the watch root
on one ZFS filesystem, which a fanotify filesystem mark fully covers.
**Known limitation (user-stated, out of scope):** the watcher does not
re-register when a dataset is added under the watched dataset — nested
datasets are separate filesystems outside the mark. Recorded in HANDOFF.md;
a "heavy improvement" around the watcher is planned separately.
**Rejected:** `/srv/documents` default (documents on the small system
disk); making `documents` its own nested dataset (invites exactly the
nested-fs blind spot above).
