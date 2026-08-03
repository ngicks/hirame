#!/usr/bin/env bash
# Build a self-contained deployment bundle: the overwatch host binary, the
# three localhost/ images saved as docker-archives, the quadlet units,
# configuration and templates, and a copy of deploy.sh. Run as any user with
# podman and go — no root, and no service account. The target needs none of
# that and no checkout either, only podman and systemd:
#
#	deploy/build.sh [output-dir]     default: deploy/dist
#	scp -r deploy/dist target:hirame
#	ssh target sudo ./hirame/deploy.sh
#
# The archives are uncompressed and gahaku's is LibreOffice-sized; compress in
# transit if the copy is over a slow link.
set -euo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
OUT=${1:-$REPO/deploy/dist}

# Same knobs as deploy/test/build-images.sh, for build hosts where plain
# podman cannot run a container runtime (sandboxes, CI): e.g.
# PODMAN_BUILD_FLAGS='--isolation=chroot --network=host'.
PODMAN=${PODMAN:-podman}
read -ra BUILD_FLAGS <<<"${PODMAN_BUILD_FLAGS:-}"

log() { printf '\e[1m[build]\e[0m %s\n' "$*"; }
die() { printf '\e[1;31m[build] ERROR:\e[0m %s\n' "$*" >&2; exit 1; }

[ -f "$REPO/gahaku/Dockerfile" ] && [ -f "$REPO/go-overwatch/overwatch/go.mod" ] ||
	die "submodules are not checked out; run: git submodule update --init"
for cmd in "$PODMAN" go; do
	command -v "$cmd" >/dev/null || die "required command not found: $cmd"
done

# Absolute before the subshell cd below, or a relative output-dir argument
# would resolve against the submodule checkout.
mkdir -p "$OUT/images"
OUT=$(cd "$OUT" && pwd)

# GOWORK=off: go.work deliberately excludes the submodules (D-006). The module
# proxy cannot serve this module at all — the checkout is the only source.
# CGO_ENABLED=0 because the bundle deploys on hosts whose libc (and dynamic
# loader path) need not match the build host's.
log "building overwatch from the submodule checkout"
(cd "$REPO/go-overwatch/overwatch" &&
	GOWORK=off CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o "$OUT/overwatch" ./cmd/overwatch)

# The archive keeps the localhost/ name, which is what the units resolve; the
# rm is because the docker-archive transport refuses to overwrite.
build_and_save() { # <name> <containerfile> <context>
	log "building localhost/$1:latest"
	"$PODMAN" build "${BUILD_FLAGS[@]}" --tag "localhost/$1:latest" --file "$2" "$3"
	rm -f "$OUT/images/$1.tar"
	"$PODMAN" save --output "$OUT/images/$1.tar" "localhost/$1:latest"
}
build_and_save hirame-search-api "$REPO/apps/search-api/Containerfile" "$REPO"
build_and_save hirame-web-gui "$REPO/apps/web-gui/Containerfile" "$REPO/apps/web-gui"
build_and_save gahaku "$REPO/gahaku/Dockerfile" "$REPO/gahaku"

# What deploy.sh installs besides the artifacts, laid out as its siblings —
# the same relative layout it sees when run from the checkout.
log "staging deployment files"
rm -rf "$OUT/quadlet" "$OUT/config" "$OUT/systemd"
cp -r "$REPO/deploy/quadlet" "$REPO/deploy/config" "$REPO/deploy/systemd" "$OUT/"
install -m 0755 "$REPO/deploy/deploy.sh" "$OUT/deploy.sh"

log "bundle in $OUT"
du -sh "$OUT/overwatch" "$OUT/images"/*.tar
