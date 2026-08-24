# HANDOFF — TrueNAS deployment scripts

Out-of-scope items surfaced while planning. A ledger, not a license — each
entry names its follow-up.

## H1 — watcher does not cover datasets added under a watch root

User-stated (Q7 resolution): overwatch's fanotify *filesystem* mark covers
one filesystem; a ZFS dataset later created beneath the watched path (e.g.
under `/mnt/tank/share/documents`) is a separate filesystem the daemon never
re-registers. Mitigated here only by keeping `documents` a plain directory
(D7). Follow-up: the user's planned "heavy improvement" around the watcher
in `go-overwatch` (mount detection / per-dataset re-marking) — its own
future plan, not this one.

## H2 — `podman-static-v5.8.4.tar.zst` committed at the repository root

The artifact's standard location is
`~/.cache/dotfiles/build/podman-static/out/podman-static-v5.8.4.tar.zst`;
the repo-root copy was pushed accidentally (user-stated) but is what this
setup uses, and `deploy/truenas/03-podman.sh` will read it from there.
Follow-up: decide whether to keep shipping it in-repo (30 MiB in history
already) or fetch/build it out-of-tree and drop the file; either way the
script's lookup order (repo root, then the standard cache path) should
match the decision.
