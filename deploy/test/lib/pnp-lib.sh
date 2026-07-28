# Shared helpers for the end-to-end harness. Sourced from *inside* pnp-enter.
#
# Everything here encodes a constraint of the nested environment, not of the
# deployment. See deploy/test/README.md for which constraint each one is.

PNP_ROOT="${PNP_ROOT:-$(cd "$(dirname "$0")" && pwd)}"
export PATH="$PNP_ROOT:$PATH"

PODMAN_DIST="${PODMAN_DIST:-/root/.local/share/podman-dist/current}"
PODMAN="${PNP_PODMAN:-$PODMAN_DIST/usr/local/bin/podman}"
QUADLET="${PNP_QUADLET:-$PODMAN_DIST/usr/local/libexec/podman/quadlet}"
export PNP_PODMAN="$PODMAN" PNP_QUADLET="$QUADLET"

# Every container in this environment needs the same three flags:
#
#   --uts=host		sethostname(2) is blocked by the surrounding seccomp
#			filter (permitted only with CAP_SYS_ADMIN in the
#			*initial* user namespace, which we do not have)
#   --network=host	netavark shells out to iptables, which is not installed,
#			and there is no nft either; bridge networking therefore
#			cannot come up at any depth, private netns or not
#   --cgroups=disabled	/sys/fs/cgroup is a read-only, threaded cgroup2 tree
#
# The first and last are added by the test drop-ins; the middle one is the
# reason deploy/test/env/ addresses every service as 127.0.0.1 instead of by
# container name.
PNP_FLAGS="--uts=host --network=host --cgroups=disabled"
export PNP_FLAGS

# podman build cannot use the default oci isolation: buildah runs each RUN
# through crun, which sethostname()s. chroot isolation skips that entirely.
PNP_BUILD_FLAGS="--isolation=chroot --network=host"
export PNP_BUILD_FLAGS

log() { printf '\n== %s\n' "$*" >&2; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
die() {
	printf 'ERROR: %s\n' "$*" >&2
	exit 1
}

# A tmpfs mounted *inside* this user namespace: its s_user_ns is our own, so
# chown(2) to any of uid 0..65536 succeeds on it. Bind-mounted outer paths and
# podman's own volume directory live on a filesystem owned by an ancestor user
# namespace and reject chown, which breaks every image that su-execs to a
# service account -- postgres (999) and versitygw among them.
pnp_scratch() { # pnp_scratch [size]
	# /run is a tmpfs *shared* with the surrounding container (a new mount
	# namespace copies the mount tree, not the superblock), so `mkdir
	# /run/pnp` is visible outside and "does the directory exist" is not a
	# usable mount check. Test the mount table instead, or a second run
	# silently writes into a 64 MiB /run and dies with ENOSPC.
	if ! grep -q " /run/pnp " /proc/self/mountinfo; then
		mkdir -p /run/pnp
		pnp-mount tmpfs pnp-scratch /run/pnp "size=${1:-4g}"
	fi
}

# Back a podman named volume with a tmpfs of our own.
#
# This is what lets postgres.container and versitygw.container run *unmodified*:
# the units keep their Volume=hirame-db.volume:... lines and podman keeps
# managing the volume, but the directory it hands to the container is a
# filesystem this user namespace owns, so the images' chown succeeds. The mount
# outlives every container, which is what scenario S6 restarts against.
pnp_volume_tmpfs() { # pnp_volume_tmpfs <volume> <size>
	_mp=$("$PODMAN" volume inspect "$1" --format '{{.Mountpoint}}') ||
		die "volume $1 does not exist yet; start its .volume unit first"
	if grep -q " $_mp " /proc/self/mountinfo; then return 0; fi
	pnp-mount tmpfs "pnp-$1" "$_mp" "size=$2" ||
		die "could not mount tmpfs on $_mp"
}

# --network=host means every service shares one network namespace, so ports are
# a global resource: a leftover process from an earlier run silently steals the
# port and clients then talk to the WRONG server. Refuse to start on one.
pnp_port_free() { # pnp_port_free <port>
	_hex=$(printf '%04X' "$1")
	! awk -v h=":$_hex" '$4=="0A" && index($2,h)' \
		/proc/net/tcp /proc/net/tcp6 2>/dev/null | grep -q .
}

pnp_require_port() { # pnp_require_port <port> <what>
	pnp_port_free "$1" ||
		die "port $1 (${2:-unknown service}) is already in use in this network namespace"
}

wait_for() { # wait_for <seconds> <cmd...>
	_n=$1
	shift
	while [ "$_n" -gt 0 ]; do
		if "$@" >/dev/null 2>&1; then return 0; fi
		_n=$((_n - 1))
		sleep 1
	done
	return 1
}
