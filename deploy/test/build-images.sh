#!/bin/sh
# Build every localhost/ image the Quadlet units name, plus the two host
# binaries the harness runs outside a container. Runs *inside* pnp-enter;
# run-e2e.sh calls it, and it can also be run on its own:
#
#	deploy/test/lib/pnp-enter deploy/test/build-images.sh
#
# Why it cannot run outside: buildah's default oci isolation runs each RUN
# through crun, which sethostname()s, and that syscall is seccomp-blocked here.
# --isolation=chroot skips the container runtime entirely, which is why every
# build below carries PNP_BUILD_FLAGS.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
. "$HERE/lib/pnp-lib.sh"

# The build cache and the intermediate layers are large. TMPDIR defaults into a
# small /run tmpfs here, which shows up as an unexplained ENOSPC halfway
# through the node build.
TMPDIR="${TMPDIR:-$(dirname "$($PODMAN info --format '{{.Store.GraphRoot}}')")/tmp}"
mkdir -p "$TMPDIR"
export TMPDIR

build() { # build <tag> <containerfile> <context>
	_tag=$1
	_file=$2
	_ctx=$3
	log "building $_tag"
	_start=$(date +%s)
	$PODMAN build $PNP_BUILD_FLAGS --tag "$_tag" --file "$_file" "$_ctx" || {
		echo "BUILD FAILED: $_tag" >&2
		return 1
	}
	_end=$(date +%s)
	printf '%s\t%ss\t%s\n' "$_tag" "$((_end - _start))" \
		"$($PODMAN image inspect "$_tag" --format '{{.Size}}')" >>"$BUILD_REPORT"
}

BUILD_REPORT="${BUILD_REPORT:-/run/pnp/build-report.tsv}"
mkdir -p "$(dirname "$BUILD_REPORT")"
: >"$BUILD_REPORT"

# Ordered so the layer cache is reused rather than fought: the three
# golang:1.26-bookworm builds run together, and node last because nothing
# shares its base.
# Context is the repository root; see apps/search-api/Containerfile.
build hirame-search-api:latest "$REPO/apps/search-api/Containerfile" "$REPO"

build gahaku:latest "$REPO/gahaku/Dockerfile" "$REPO/gahaku"
build hirame-web-gui:latest "$REPO/apps/web-gui/Containerfile" "$REPO/apps/web-gui"

# The two overwatch binaries, built for the host rather than into an image:
# the deployment runs the daemon as a native systemd service now (D-009), so
# the harness runs both the real one and its stand-in as plain processes. They
# go under bin/ (gitignored) rather than into the submodule checkout, which is
# read-only, or into /run/pnp, which a later pnp-enter cannot see.
#
# This is the harness's one toolchain requirement beyond podman, gcc and
# python3: the image builds carry their own Go, these do not.
BIN=$HERE/bin
mkdir -p "$BIN"
command -v go >/dev/null 2>&1 ||
	die "go is not on PATH; it builds deploy/test/bin/{overwatch,fakeoverwatch}"

# GOWORK=off for both: go.work deliberately excludes the submodules (D-006),
# and fakeoverwatch is its own module outside the workspace as well.
go_build() { # go_build <output> <module-dir> <package>
	log "building $1"
	_start=$(date +%s)
	(cd "$2" && GOWORK=off go build -trimpath -ldflags '-s -w' -o "$BIN/$1" "$3") || {
		echo "BUILD FAILED: $1" >&2
		return 1
	}
	_end=$(date +%s)
	printf '%s\t%ss\t%s\n' "$1" "$((_end - _start))" \
		"$(wc -c <"$BIN/$1" | tr -d ' ')" >>"$BUILD_REPORT"
}

# The real daemon is built even though the harness expects it to refuse to
# start: e2e.sh runs it once and records the refusal, which is the evidence
# that the stand-in is a necessity rather than a shortcut.
go_build overwatch "$REPO/go-overwatch/overwatch" ./cmd/overwatch
go_build fakeoverwatch "$HERE/fakeoverwatch" .

log "build report"
printf 'ARTIFACT\tBUILD\tBYTES\n'
cat "$BUILD_REPORT"
