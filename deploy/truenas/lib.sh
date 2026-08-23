#!/usr/bin/env bash
# Shared environment and guest-access helpers for the deploy/truenas/*.sh
# scripts, which stand hirame up inside a throwaway TrueNAS SCALE VM built from
# a read-only golden qcow2 image.
#
# The guest is TrueNAS SCALE 25.10 and every middleware/API call the sibling
# scripts make is pinned to it. That API is not stable across releases, so a
# different golden image means re-checking each call site, not just this file.
#
# Sourced, never run:  . "$(cd "$(dirname "$0")" && pwd)/lib.sh"
set -euo pipefail

TRUENAS_GOLDEN=${TRUENAS_GOLDEN:-/var/lib/libvirt/isos/ready/TrueNAS-SCALE-25.10.4-golden.qcow2}
TRUENAS_IMAGE_DIR=${TRUENAS_IMAGE_DIR:-/var/lib/libvirt/images}
TRUENAS_VM_NAME=${TRUENAS_VM_NAME:-truenas-hirame}
TRUENAS_VM_MEM=${TRUENAS_VM_MEM:-8192}
TRUENAS_VM_CPUS=${TRUENAS_VM_CPUS:-4}
TRUENAS_DATA_SIZE=${TRUENAS_DATA_SIZE:-32G}
TRUENAS_PORT_HTTPS=${TRUENAS_PORT_HTTPS:-10443}
TRUENAS_PORT_SSH=${TRUENAS_PORT_SSH:-10022}
TRUENAS_PORT_GUI=${TRUENAS_PORT_GUI:-18080}
TRUENAS_ADMIN_PASSWORD=${TRUENAS_ADMIN_PASSWORD:-truenas-golden-image}
TRUENAS_HOST=${TRUENAS_HOST:-127.0.0.1}

# The deployment host may be a root-only container with no sudo binary at all,
# while a workstation running this as an ordinary user still needs to escalate.
SUDO=""
if [ "$EUID" -ne 0 ]; then SUDO=sudo; fi

# Labelled with the caller's own name, so output from the four scripts in this
# directory stays attributable when they are run back to back.
log() { printf '\e[1m[%s]\e[0m %s\n' "${0##*/}" "$*" >&2; }
die() {
	printf '\e[1;31m[%s] ERROR:\e[0m %s\n' "${0##*/}" "$*" >&2
	exit 1
}

# The name is used twice over: as the libvirt domain name, and as the single
# path component under $TRUENAS_IMAGE_DIR that `01-vm.sh destroy` removes
# wholesale. A value carrying a slash or a `..` would aim that removal at a
# directory outside the image tree, so it is rejected here rather than guarded
# for at each use. A leading dash is out for a second reason: it would reach
# virsh as an option rather than as a domain name.
case "$TRUENAS_VM_NAME" in
.* | -* | *[!A-Za-z0-9._-]*)
	die "TRUENAS_VM_NAME must be letters, digits, dot, dash or underscore and must not start with a dot or a dash: '$TRUENAS_VM_NAME'"
	;;
esac

# Everything one instance owns lives here, which is what makes `01-vm.sh
# destroy` a single subtree removal instead of a list of file names.
TRUENAS_INSTANCE_DIR=$TRUENAS_IMAGE_DIR/$TRUENAS_VM_NAME

# The VM is a throwaway test appliance: its host key changes with every
# destroy/up cycle, so pinning would raise a mismatch on each fresh instance —
# an alarm an operator would learn to click through. There is nothing to
# protect either, the account being truenas_admin with a password baked into
# the golden image and known to everyone who can read this file.
#
# PubkeyAuthentication=no because this login is password-only; without it an
# operator with a populated ssh-agent can exhaust the server's auth attempts on
# keys before ever reaching the password prompt.
TN_SSH_OPTS=(
	-o StrictHostKeyChecking=no
	-o UserKnownHostsFile=/dev/null
	-o LogLevel=ERROR
	-o PubkeyAuthentication=no
	-o ConnectTimeout=5
)

TN_ASKPASS_DIR=""

# Password auth through OpenSSH's own askpass channel (SSH_ASKPASS_REQUIRE
# needs OpenSSH >= 8.4). sshpass is not installed on the deployment host and is
# not worth taking on as a dependency for one fixed appliance password.
#
# The helper reads the password out of the environment it inherits rather than
# embedding it, so the secret never touches the filesystem.
tn_askpass() {
	if [ -n "$TN_ASKPASS_DIR" ]; then return 0; fi
	TN_ASKPASS_DIR=$(mktemp -d) || die "could not create a temporary directory for the ssh askpass helper"
	trap 'rm -rf "$TN_ASKPASS_DIR"' EXIT
	cat >"$TN_ASKPASS_DIR/askpass" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$TRUENAS_ADMIN_PASSWORD"
EOF
	chmod 0700 "$TN_ASKPASS_DIR/askpass"
}

tn_ssh() { # tn_ssh <cmd...>
	tn_askpass
	TRUENAS_ADMIN_PASSWORD="$TRUENAS_ADMIN_PASSWORD" \
		SSH_ASKPASS="$TN_ASKPASS_DIR/askpass" \
		SSH_ASKPASS_REQUIRE=force \
		DISPLAY="${DISPLAY:-:0}" \
		ssh "${TN_SSH_OPTS[@]}" -p "$TRUENAS_PORT_SSH" \
		"truenas_admin@$TRUENAS_HOST" "$@"
}

# A command handed to ssh is re-parsed by a login shell in the guest, so every
# argument is wrapped for that second pass. midclt payloads are JSON documents
# full of braces, quotes and commas, and paths may hold spaces; without this
# they would arrive split into pieces or glob-expanded. Plain single quotes
# rather than bash's ${var@Q}, because truenas_admin's login shell is not
# necessarily bash.
tn_quote() {
	local arg
	for arg in "$@"; do
		printf " '%s'" "${arg//\'/\'\\\'\'}"
	done
}

tn_run() { # tn_run <cmd> [arg...] -- run one command in the guest
	tn_ssh "$(tn_quote "$@")"
}

# tn_scp <src...> <dest>; name the remote side in full, e.g.
# tn_scp ./bundle.tar "truenas_admin@$TRUENAS_HOST:/tmp/".
tn_scp() {
	tn_askpass
	TRUENAS_ADMIN_PASSWORD="$TRUENAS_ADMIN_PASSWORD" \
		SSH_ASKPASS="$TN_ASKPASS_DIR/askpass" \
		SSH_ASKPASS_REQUIRE=force \
		DISPLAY="${DISPLAY:-:0}" \
		scp "${TN_SSH_OPTS[@]}" -P "$TRUENAS_PORT_SSH" "$@"
}

# A cold boot of the appliance reaches sshd in a couple of minutes; the default
# leaves room for a slow first boot growing the overlay.
tn_wait_ssh() { # tn_wait_ssh [timeout_seconds]
	local timeout=${1:-600} deadline
	deadline=$((SECONDS + timeout))
	log "waiting for ssh on $TRUENAS_HOST:$TRUENAS_PORT_SSH (up to ${timeout}s) ..."
	while [ "$SECONDS" -lt "$deadline" ]; do
		if tn_ssh true >/dev/null 2>&1; then
			log "ssh is up"
			return 0
		fi
		sleep 5
	done
	die "no ssh on $TRUENAS_HOST:$TRUENAS_PORT_SSH after ${timeout}s; watch the boot with 'virsh console $TRUENAS_VM_NAME'"
}

tn_port_table() {
	printf '\n  %-22s %-7s %s\n' HOST GUEST SERVICE
	printf '  %-22s %-7s %s\n' "$TRUENAS_HOST:$TRUENAS_PORT_HTTPS" 443 \
		"TrueNAS UI  https://$TRUENAS_HOST:$TRUENAS_PORT_HTTPS"
	printf '  %-22s %-7s %s\n' "$TRUENAS_HOST:$TRUENAS_PORT_SSH" 22 \
		"ssh         ssh -p $TRUENAS_PORT_SSH truenas_admin@$TRUENAS_HOST"
	printf '  %-22s %-7s %s\n' "$TRUENAS_HOST:$TRUENAS_PORT_GUI" 8080 \
		"hirame      http://$TRUENAS_HOST:$TRUENAS_PORT_GUI"
	printf '\n'
}
