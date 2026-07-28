#!/bin/sh
# The end-to-end deployment test (D-013). One command, one namespace:
#
#	deploy/test/run-e2e.sh              build the images, then run every scenario
#	deploy/test/run-e2e.sh --no-build   reuse the images already in the store
#
# On an ordinary host with systemd, this harness is not the way to test the
# deployment: install deploy/quadlet/ under ~/.config/containers/systemd/, run
# `systemctl --user daemon-reload`, and let systemd do what
# lib/quadlet-supervisor.py stands in for here. It exists for the CI shape D-013
# chose -- a container that cannot run podman directly -- and it stays the CI
# path because it needs nothing of the host but a kernel with user namespaces.
#
# See deploy/test/README.md for what is substituted and what is real.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)

BUILD=1
for arg in "$@"; do
	case "$arg" in
	--no-build) BUILD=0 ;;
	*)
		echo "usage: $0 [--no-build]" >&2
		exit 64
		;;
	esac
done

# Everything downstream of here runs inside ONE user namespace. Namespaces are
# not re-enterable in this environment -- there is no nsenter, and a second
# pnp-enter creates a *different* user namespace -- so the builds and the run
# cannot be split across invocations if they are to share a mount namespace.
# They do not need to: images live in the shared graphroot, so the build is its
# own invocation and only the run has to be atomic.
if [ "$BUILD" -eq 1 ]; then
	"$HERE/lib/pnp-enter" "$HERE/build-images.sh"
fi

exec "$HERE/lib/pnp-enter" "$HERE/e2e.sh"
