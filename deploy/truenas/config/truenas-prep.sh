#!/bin/sh
# Rootless podman needs three pieces of state under /etc that TrueNAS does not
# keep: it regenerates much of /etc from its configuration database at every
# boot, so anything written there by hand is gone by the next one. Running this
# before the user managers start makes each boot look like a machine where the
# state had survived.
#
# Idempotent by design — it is the same work every boot, and running it by hand
# between boots must change nothing.
#
# The subuid/subgid entries and the quadlet binary path below are filled in
# when this script is installed onto the machine.
set -eu

# Exact line already there: nothing to do. A stale entry for the same user is
# replaced rather than appended to, since two ranges for one user are what
# shadow-utils reads as an overlap.
ensure_subid() { # <file> <entry>
	entry=$2
	user=${entry%%:*}
	[ -e "$1" ] || : >"$1"
	if grep -q "^${user}:" "$1"; then
		grep -qxF -- "$entry" "$1" || sed -i "s|^${user}:.*|${entry}|" "$1"
	else
		printf '%s\n' "$entry" >>"$1"
	fi
}

ensure_subid /etc/subuid '@SUBUID_LINE@'
ensure_subid /etc/subgid '@SUBGID_LINE@'

# The generator lives in the podman user's home because the static distribution
# is unpacked there; systemd only looks for it under /etc, so this symlink is
# the whole of how user-session quadlet units get generated.
mkdir -p /etc/systemd/user-generators
ln -sfn '@QUADLET_BIN@' /etc/systemd/user-generators/podman-user-generator
