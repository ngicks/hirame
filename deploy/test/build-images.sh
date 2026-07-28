#!/bin/sh
# Build every localhost/ image the Quadlet units name. Runs *inside* pnp-enter;
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

# hirame-overwatch is built even though the harness expects it to refuse to
# start: run-e2e.sh runs it once and records the refusal, which is the evidence
# that the fake below is a necessity rather than a shortcut.
build hirame-overwatch:latest "$REPO/deploy/overwatch/Containerfile" "$REPO/go-overwatch"

# The fake daemon's go.mod replaces go-overwatch with the submodule checkout,
# so its context has to hold both trees at the depth the replace names. Built
# here rather than committed as a repo-root .containerignore, which would have
# to exclude everything and re-include two subtrees.
FAKE_CTX=$TMPDIR/hirame-fakeoverwatch-ctx
rm -rf "$FAKE_CTX"
mkdir -p "$FAKE_CTX/deploy/test/fakeoverwatch" "$FAKE_CTX/go-overwatch/overwatch"
cp -a "$HERE/fakeoverwatch/." "$FAKE_CTX/deploy/test/fakeoverwatch/"
cp -a "$REPO/go-overwatch/overwatch/." "$FAKE_CTX/go-overwatch/overwatch/"
build hirame-fake-overwatch:latest \
	"$FAKE_CTX/deploy/test/fakeoverwatch/Containerfile" "$FAKE_CTX"
rm -rf "$FAKE_CTX"

build gahaku:latest "$REPO/gahaku/Dockerfile" "$REPO/gahaku"
build hirame-web-gui:latest "$REPO/apps/web-gui/Containerfile" "$REPO/apps/web-gui"

log "image build report"
printf 'IMAGE\tBUILD\tBYTES\n'
cat "$BUILD_REPORT"
