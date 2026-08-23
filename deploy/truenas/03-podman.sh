#!/usr/bin/env bash
# Installs a static podman distribution into the podman user's home inside the
# TrueNAS SCALE VM and makes the rootless stack runnable without a login.
#
# The appliance gives podman nowhere ordinary to live: /usr is immutable, so
# nothing installs system-wide, and /home is mounted noexec, which is why the
# previous script placed the podman user's home on the pool instead. A static
# distribution unpacked under that home is the only shape that survives both
# constraints — it is self-contained, and the pool is the one path that is
# writable and executable at once.
#
# The guest is TrueNAS SCALE 25.10; the guest-side assumptions here (sudo for
# truenas_admin, systemd-run, loginctl, a systemd user manager) are pinned to it
# and want re-checking against a different golden image.
#
# Re-running is safe and is the upgrade path: the artifact re-extracts over its
# own tag, link relinks idempotently, and linger is already on.
#
#   03-podman.sh
#
# PODMAN_STATIC_TAR   the podman-static .tar.zst to install; overrides the two
#                     default locations searched below.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/lib.sh"

GUEST_USER=podman
# Where podman-static-dist puts the tree and the `current` symlink it maintains,
# with XDG_DATA_HOME left unset below so its default applies.
DIST_SUBDIR=.local/share/podman-dist

# Both failure modes below leave the operator in the same place, so they get the
# same instructions.
DIST_TOOL_HINT='podman-static-dist is the Go tool github.com/ngicks/dotfiles/tool/podman-static-dist;
    install a static build of it with

      CGO_ENABLED=0 go install github.com/ngicks/dotfiles/tool/podman-static-dist/cmd/podman-static-dist@latest'

# The guest-side twin of deploy/deploy.sh's as_service: same two variables, same
# reason. systemd-run rather than runuser because TrueNAS's sudoers carries
# `Defaults log_subcmds`, whose intercept machinery kills podman's
# /proc/self/exe re-exec anywhere below sudo in the process tree — systemd-run
# hands the payload to PID 1, outside that tree. The working directory matters:
# the ssh session lands in truenas_admin's home, which podman cannot enter, and
# rootless podman re-execs itself into whatever working directory it inherits.
as_podman() { # as_podman <cmd> [arg...] -- run one command as the podman user
	tn_run sudo systemd-run --wait --pipe --collect --quiet \
		--property=User="$GUEST_USER" \
		--property=WorkingDirectory="$GUEST_HOME" \
		--setenv=HOME="$GUEST_HOME" \
		--setenv=XDG_RUNTIME_DIR="/run/user/$GUEST_UID" \
		-- "$@"
}

# --- what gets shipped -------------------------------------------------------

preflight_local() {
	DIST_TOOL=$(command -v podman-static-dist) ||
		die "podman-static-dist not found on PATH.
    $DIST_TOOL_HINT"

	# This copy of the tool is handed to an appliance whose glibc and library
	# paths are not this workstation's, so a dynamically linked build would not
	# start there. ldd exits non-zero on a static Go binary: that is the good
	# case, not an error.
	if ! command -v ldd >/dev/null; then
		log "note: no ldd here, so $DIST_TOOL is shipped unchecked; it must be static"
	else
		local ldd_out ldd_rc=0
		ldd_out=$(ldd "$DIST_TOOL" 2>&1) || ldd_rc=$?
		case "$ldd_out" in
		*"not a dynamic executable"* | *"statically linked"*) ;;
		*)
			[ "$ldd_rc" -eq 0 ] ||
				die "could not tell whether $DIST_TOOL is statically linked: $ldd_out"
			die "$DIST_TOOL is dynamically linked and would not run on the appliance.
    $DIST_TOOL_HINT"
			;;
		esac
	fi

	local cache_tar=$HOME/.cache/dotfiles/build/podman-static/out/podman-static-v5.8.4.tar.zst
	local repo_tar
	repo_tar=$(cd "$HERE/../.." && pwd)/podman-static-v5.8.4.tar.zst
	if [ -n "${PODMAN_STATIC_TAR:-}" ]; then
		# Set but wrong is a mistake worth naming, not a reason to quietly
		# install some other artifact.
		[ -f "$PODMAN_STATIC_TAR" ] ||
			die "PODMAN_STATIC_TAR names no file: $PODMAN_STATIC_TAR"
		TAR=$PODMAN_STATIC_TAR
	elif [ -f "$cache_tar" ]; then
		TAR=$cache_tar
	elif [ -f "$repo_tar" ]; then
		TAR=$repo_tar
	else
		die "no podman-static artifact found; looked at
      \$PODMAN_STATIC_TAR   (unset)
      $cache_tar
      $repo_tar
    build one with 'podman-static-dist build', or point PODMAN_STATIC_TAR at it"
	fi

	log "tool     $DIST_TOOL"
	log "artifact $TAR"
}

# --- the guest ---------------------------------------------------------------

resolve_guest() {
	GUEST_HOME=$(tn_run getent passwd "$GUEST_USER" | cut -d: -f6) ||
		die "no user '$GUEST_USER' in the guest; run 02-kit.sh first"
	[ -n "$GUEST_HOME" ] || die "user '$GUEST_USER' has no home directory in the guest"
	GUEST_UID=$(tn_run id -u "$GUEST_USER")
	GUEST_GROUP=$(tn_run id -gn "$GUEST_USER")
	case "$GUEST_UID" in
	'' | *[!0-9]*) die "unexpected uid for '$GUEST_USER' in the guest: '$GUEST_UID'" ;;
	esac
	log "$GUEST_USER is uid $GUEST_UID with home $GUEST_HOME"
}

# Before anything else touches the user manager: podman-static-dist wires
# systemd units as part of installing, and `systemctl --user` needs a manager
# and a /run/user/<uid> that only linger keeps alive between logins.
start_user_manager() {
	log "enabling linger for $GUEST_USER"
	tn_run sudo loginctl enable-linger "$GUEST_USER"

	local i
	for ((i = 0; i < 30; i++)); do
		if as_podman systemctl --user show-environment >/dev/null 2>&1; then
			log "the $GUEST_USER systemd user manager is up"
			return 0
		fi
		sleep 2
	done
	die "the systemd user manager for $GUEST_USER never came up; check 'loginctl user-status $GUEST_USER' in the guest"
}

install_dist() {
	log "copying the tool and the artifact to the guest"
	tn_scp "$DIST_TOOL" "$TAR" "truenas_admin@$TRUENAS_HOST:/tmp/"

	local staged_tool=/tmp/${DIST_TOOL##*/}
	local staged_tar=/tmp/${TAR##*/}
	local tool_dst=$GUEST_HOME/.local/bin/podman-static-dist

	# /tmp is fine to read the artifact out of, but not to run the tool from:
	# the appliance may well mount it noexec, and the pool home is the one path
	# already known to be executable. Keeping the tool there also leaves the
	# appliance able to re-install without this workstation.
	as_podman mkdir -p "$GUEST_HOME/.local/bin"
	tn_run sudo install -o "$GUEST_USER" -g "$GUEST_GROUP" -m 0755 "$staged_tool" "$tool_dst"
	# scp carries this workstation's mode across and the artifact is read back
	# as podman, not as the account that uploaded it.
	tn_run sudo chmod 0644 "$staged_tar"

	# extract + link rather than the tool's one-shot install: install's final
	# step wires the quadlet generator into /etc with sudo, which the podman
	# account does not have — and on this appliance must not have. On TrueNAS
	# the generator symlink is owned by the boot-persistence shim anyway, since
	# anything written into /etc by hand is regenerated away at the next boot.
	# --skip-systemd leaves exactly that step out; the home-side links (config,
	# environment.d, user units, the `current` symlink) are still made.
	# No --tag on extract: the artifact stamps its own, so the tree always
	# names the version actually installed. That stamp is the artifact's
	# version, so the same string is read off the file name here and link is
	# pointed at it -- naming the tag beats picking the newest directory, which
	# aims `current` at whichever tree was touched last once two of them
	# coexist. Re-running extracts over the existing tree and relinks, which is
	# why a re-run is safe.
	local tag=${TAR##*/}
	tag=${tag#podman-static-}
	tag=${tag%.tar.zst}
	log "installing the distribution under $GUEST_HOME/$DIST_SUBDIR"
	as_podman "$tool_dst" --log=text extract --tar "$staged_tar"
	if ! as_podman test -d "$GUEST_HOME/$DIST_SUBDIR/$tag" >/dev/null 2>&1; then
		local listing
		listing=$(as_podman ls -1 "$GUEST_HOME/$DIST_SUBDIR" 2>&1 | tr '\n' ' ') || true
		die "extract left no tree at $GUEST_HOME/$DIST_SUBDIR/$tag; the tag is
    read off the artifact name (${TAR##*/}) and has to match the one the
    artifact stamps on itself. That directory holds: ${listing:-nothing}"
	fi
	as_podman "$tool_dst" --log=text link --skip-systemd --tag "$tag"

	# /tmp is RAM on the appliance and the artifact is not small.
	tn_run rm -f "$staged_tool" "$staged_tar"

	# environment.d is turned into manager environment by a systemd environment
	# generator, and generators re-run on reload — so the file just written
	# takes effect without restarting the manager out from under anything it is
	# already running.
	as_podman systemctl --user daemon-reload
}

# --- verification ------------------------------------------------------------

verify() {
	local rc=0 out quadlet quadlet_bin

	log "verifying"
	if out=$(as_podman "$GUEST_HOME/$DIST_SUBDIR/current/usr/local/bin/podman" info 2>&1); then
		log "  ok    podman info"
	else
		printf '%s\n' "$out" >&2
		log "  FAIL  podman info"
		rc=1
	fi

	# The next script links this binary into /etc/systemd/user-generators and
	# bakes its path into the boot shim, so a distribution that keeps it
	# somewhere else has to be caught here: past this point the failure surfaces
	# as units that are simply never generated.
	quadlet_bin=$GUEST_HOME/$DIST_SUBDIR/current/usr/local/libexec/podman/quadlet
	if as_podman test -x "$quadlet_bin" >/dev/null 2>&1; then
		log "  ok    $quadlet_bin"
	else
		log "  FAIL  no executable quadlet binary at $quadlet_bin"
		rc=1
	fi

	# QUADLET_UNIT_DIRS comes from the distribution's 50-podman.conf, which
	# link wires into ~/.config/environment.d — so its value in the manager
	# environment is the evidence that the environment.d linking took effect.
	if ! out=$(as_podman systemctl --user show-environment 2>&1); then
		printf '%s\n' "$out" >&2
		log "  FAIL  systemctl --user show-environment"
		rc=1
	elif quadlet=$(printf '%s\n' "$out" | grep '^QUADLET_UNIT_DIRS='); then
		case "$quadlet" in
		*containers-quadlet*) log "  ok    $quadlet" ;;
		*)
			log "  FAIL  $quadlet does not include the containers-quadlet directory"
			rc=1
			;;
		esac
	else
		log "  FAIL  no QUADLET_UNIT_DIRS in the $GUEST_USER manager environment"
		rc=1
	fi

	[ "$rc" -eq 0 ] || die "verification failed; the stack will not start in this state"
	log "podman is installed and the $GUEST_USER user manager is wired for quadlet"
}

preflight_local
tn_wait_ssh
resolve_guest
start_user_manager
install_dist
verify
