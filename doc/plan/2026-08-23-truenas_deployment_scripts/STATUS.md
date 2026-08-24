# STATUS — TrueNAS deployment scripts

State: **implemented and e2e-verified 2026-08-23/24** — steps 1–7 done.
Commits a91a7fe, bbea344, d262479, 178c5cf (review fixes incl. blocking
quadlet-path bug), 854418e (live-run fixes: ACPI, REST ssh bootstrap,
systemd-run vs log_subcmds, extract+link, docs-path bake, migrate,
path+timer shim escalation — D12–D16, D6 outcome in DECISION.md), and
de12ed1 (post-e2e review: RequiresMountsFor added to the docs-path bake
with a leftover-refusing post-condition; bootstrap hardening).
shellcheck clean; deploy/test harness green (8/8 scenarios); two review
passes (full change set, then the live-run delta).

## Traceability (decision → owning step)

| Decision clause | Owner |
| --- | --- |
| D1 named single instance, `TRUENAS_VM_NAME`, per-instance image dir | step 3 |
| D2 forwards 10443/10022/18080 on 0.0.0.0 | step 3 |
| D3 `SERVICE_USER` knob, derived group, `Group=` + tmpfiles group rewrite, docs grant group | step 2 |
| D3 user/group creation (`podman` g:podman +hirame; `hirame`) | step 4 |
| D4 ssh + `sudo midclt`, password env, idempotent guards | step 4 |
| D5 `QUADLET_DIR` knob in deploy.sh | step 2 |
| D5 committed `75-podman.conf` shipped verbatim; TrueNAS-side dir value | steps 1, 5, 6 |
| D6 prep unit + script content | step 1 |
| D6 install/enable + baked subuid range | step 6 |
| D6 empirical reboot-survival check (escalation recorded) | step 7 |
| D7 dataset `tank/share` + plain `documents` dir, group grant | step 4 |
| D7 `DOCS_DIR` wiring into deploy.sh run | step 6 |

IDEA.md use cases: UC1→step 3, UC2→step 4, UC3→step 5, UC4→steps 1+2+6,
reboot-survival success criterion→step 7. HANDOFF.md entries: H1 user-stated
out-of-scope discovery, H2 out-of-scope discovery — gate passes.

## Checklist

- [x] Idea gate confirmed (IDEA.md `Gate: confirmed by user, 2026-08-23`)
- [x] D1–D7 resolved; open questions drained
- [x] Public surface delta enumerated (PLAN.md)
- [x] Step 1: config assets (`75-podman.conf`, `truenas-prep.{service,sh}`) — commit a91a7fe; template tokens `@SUBUID_LINE@ @SUBGID_LINE@ @QUADLET_BIN@`
- [x] Step 2: parametrize `deploy/deploy.sh` (D3 "gains SERVICE_USER … derives the group", D5 "install dir becomes a knob") — commit bbea344; internals renamed HIRAME_*→SERVICE_*, group operands parametrized throughout
- [x] Step 3: `01-vm.sh` (D1 "named, single at a time"; D2 "10443→443, 10022→22, 18080→8080") — commit a91a7fe; incl. lib.sh (SSH_ASKPASS_REQUIRE instead of sshpass — D8; $SUDO helper — D9); data disks carry virtio serials hirame-data-{0,1,2} so disk.get_unused sees them; machine type pc (q35 broke virtio probe)
- [x] Step 4: `02-kit.sh` (D4 "ssh … sudo midclt call"; D3 users; D7 "dataset tank/share, plain directory documents") — commit d262479; payloads cross-checked against 25.04/26.04 API docs; verified against a mock middleware harness (idempotent re-run, failure paths)
- [x] Step 5: `03-podman.sh` (ships repo `75-podman.conf`; static binary check) — commit d262479; linger enabled before install (link phase needs the user manager); tool kept in ~podman/.local/bin (guest /tmp may be noexec)
- [x] Step 6: `04-deploy.sh` (D6 shim install; D5/D7 knob values; TrueNAS deploy.sh run) — commit d262479; staging at /mnt/tank/hirame-deploy (exec fs); enable+restart instead of enable --now so re-runs re-execute the oneshot
- [x] Step 7: end-to-end + reboot verification (D6 "verify /etc/systemd/system survives") — PASSED on a live VM, 2026-08-23/24:
  - 01 up → 02 → 03 → 04 with only defaults; hirame GUI answers 200 on http://127.0.0.1:18080 from the host
  - `zpool status tank`: raidz1-0, 3 disks, ONLINE; `id podman` = `uid=3001(podman) gid=3001(podman) groups=podman,hirame`
  - three reboots: stack returns unattended (GUI 200) every time; pool imports; `/etc/systemd/system/truenas-prep.service` survives (D6 premise holds), BUT TrueNAS rewrites subuid/subgid again later in boot — escalation applied (path unit + timer sweep, D6 outcome entry); after the final reboot both `podman:` lines persist
  - idempotence: 02 re-run prints "already kitted: nothing to change"; 03 and 04 re-run green
  - golden image byte-identical before/after (11202002944 bytes, mtime unchanged)
  - final confirmation with the finished code, from cold: `destroy --yes` → 01 → 02 → 03 → 04 (SKIP_BUILD=1; bundle inputs unchanged) → reboot, one unattended pass, exit 0 — 02's REST bootstrap mutation path (ssh login, passwordless sudo, ssh service) exercised for real; after reboot: both subid lines, pool ONLINE, `RequiresMountsFor=/mnt/tank/share/documents` in the installed unit, overwatch + path + timer active, GUI 200
  - environment notes: this host needed the libvirt daemons started by hand (container, no systemd) and the bundle built via `deploy/test/lib/pnp-enter deploy/build.sh` + `SKIP_BUILD=1` (this container cannot run podman directly — deploy/test/README.md); on a normal workstation 04 builds directly

## Next action

None — plan complete. The verification VM `truenas-hirame` is left defined
and running for inspection (`deploy/truenas/01-vm.sh status`).
