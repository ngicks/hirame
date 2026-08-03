#!/usr/bin/env bash
# Build and deploy in one run, for the host that is both build machine and
# deployment target — deploy/build.sh followed by deploy/deploy.sh:
#
#	sudo deploy/install.sh [artifact-dir]     default: deploy/dist
#
# Run the halves separately to build elsewhere and copy the artifacts over.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ART=${1:-$HERE/dist}

log() { printf '\e[1m[install]\e[0m %s\n' "$*"; }
die() { printf '\e[1;31m[install] ERROR:\e[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root: sudo $0"

# Build as the invoking user where there is one: root's image store gains
# nothing but the build cache, and this is usually their checkout anyway.
# Built as root, the images still deploy — they travel by archive, not store.
if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ] &&
	[ -d "/run/user/$(id -u "$SUDO_USER")" ]; then
	log "building as $SUDO_USER"
	runuser -u "$SUDO_USER" -- env \
		HOME="$(getent passwd "$SUDO_USER" | cut -d: -f6)" \
		XDG_RUNTIME_DIR="/run/user/$(id -u "$SUDO_USER")" \
		"$HERE/build.sh" "$ART"
else
	log "building as root"
	"$HERE/build.sh" "$ART"
fi

exec "$HERE/deploy.sh" "$ART"
