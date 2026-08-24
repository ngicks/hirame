#!/usr/bin/env bash
# Deploy prebuilt artifacts on this host, encoding the procedure of
# deploy/README.md: create the service user, install the overwatch
# daemon as a system service, install the quadlet units and configuration,
# load the images into the service user's store, and start the stack.
#
# Everything installed resolves relative to this script, which build.sh lays
# out identically in its bundle, so both invocations work — from a checkout
# that has run build.sh, and from a copied bundle on a host that has neither
# the checkout nor any toolchain:
#
#	sudo deploy/deploy.sh [artifact-dir]     default: deploy/dist
#	sudo ./deploy.sh                          inside a build.sh bundle
#
# Re-running is safe and is the redeploy path: code is always refreshed (the
# overwatch binary, every unit, every image) and configuration is never
# clobbered (overwatch.json, hirame.env, postgresql.conf, secrets.env are
# installed only if absent) — delete a file to have it regenerated. On a
# re-run with the pod already up, migrations are re-run and the applications
# restarted, per the README's redeploy note.
#
# Every knob below is an env override whose default is what deploy/README.md
# documents; a host whose account layout or writable paths differ sets them on
# the command line — README, "Appliance hosts".
set -euo pipefail

# The account the rootless stack runs as. Its *primary* group is the whole
# socket handoff, so it is derived from the account below rather than asked
# for, and rewritten into the copies of overwatch.service and the tmpfiles
# entry this script installs — the units ship the default baked in.
SERVICE_USER=${SERVICE_USER:-hirame}
SOCKET=/run/overwatch/hirame/hirame.sock
IMAGES="hirame-search-api hirame-web-gui gahaku"

# For hosts whose /usr is immutable (appliance systems — TrueNAS SCALE among
# them) where podman comes from a static dist wired into the service user's
# home and the daemon binary must live on a writable path. The installed
# overwatch.service is rewritten to match a non-default OVERWATCH_BIN.
PODMAN=${PODMAN:-podman}
OVERWATCH_BIN=${OVERWATCH_BIN:-/usr/local/bin/overwatch}
# Only where the directory is created and group-granted. A non-default value
# must match the same path already edited into overwatch.json, hirame.env,
# and the Volume= lines — README, "Where things go".
DOCS_DIR=${DOCS_DIR:-/srv/documents}
# Where the quadlet units are installed; defaults to the service user's
# ~/.config/containers/systemd once its home is known, below. A non-default
# directory only takes effect if the generator searches it — README,
# "Appliance hosts".
QUADLET_DIR=${QUADLET_DIR:-}

# In a bundle the artifacts sit next to this script; in the checkout they are
# under dist/, where build.sh puts them.
HERE=$(cd "$(dirname "$0")" && pwd)
if [ -x "$HERE/overwatch" ]; then
	ART=${1:-$HERE}
else
	ART=${1:-$HERE/dist}
fi

log() { printf '\e[1m[deploy]\e[0m %s\n' "$*"; }
warn() { printf '\e[1;33m[deploy] WARNING:\e[0m %s\n' "$*" >&2; }
die() { printf '\e[1;31m[deploy] ERROR:\e[0m %s\n' "$*" >&2; exit 1; }

# From the service user's home, not the caller's cwd: the bundle often sits
# under /root, and rootless podman re-execs chdir into the inherited cwd as
# the service user.
as_service() {
	(cd "$SERVICE_HOME" &&
		runuser -u "$SERVICE_USER" -- env \
			HOME="$SERVICE_HOME" \
			XDG_RUNTIME_DIR="/run/user/$SERVICE_UID" \
			"$@")
}

# The bundle carries every image the units name, registry ones included, so
# that nothing here reaches a registry — the host may have no outbound network
# at all. The units' own Image= lines say which those are; build.sh derives the
# list and the archive names with these same two expressions, so there is no
# second list on either side to keep in step with the units.
external_images() { # <quadlet dir>
	grep -rh '^Image=' "$1" | sed -e '/^Image=localhost\//d' -e 's/^Image=//' | sort -u
}
image_archive() { # <image ref>
	printf '%s.tar\n' "${1//[\/:]/_}"
}

# --- preflight ---------------------------------------------------------------

[ "$(id -u)" -eq 0 ] || die "run as root: sudo $0"
[ -f "$HERE/quadlet/hirame.pod" ] ||
	die "no quadlet/ next to $0; run from the checkout or from a build.sh bundle"
[ -x "$ART/overwatch" ] || die "no overwatch binary in $ART; run deploy/build.sh first"
for name in $IMAGES; do
	[ -f "$ART/images/$name.tar" ] ||
		die "no $name image archive in $ART/images; run deploy/build.sh first"
done
EXTERNAL_IMAGES=$(external_images "$HERE/quadlet")
for ref in $EXTERNAL_IMAGES; do
	[ -f "$ART/images/$(image_archive "$ref")" ] ||
		die "no archive for $ref in $ART/images; run deploy/build.sh first"
done

for cmd in "$PODMAN" openssl runuser loginctl systemctl usermod awk; do
	command -v "$cmd" >/dev/null || die "required command not found: $cmd"
done
command -v newuidmap >/dev/null ||
	die "newuidmap not found (uidmap/shadow-utils package); rootless podman cannot start without it"
command -v pasta >/dev/null || command -v slirp4netns >/dev/null ||
	warn "neither pasta nor slirp4netns found; rootless networking will likely fail"
# Without the generator the quadlet units never become systemd units and the
# start step fails with "Unit hirame-pod.service not found". systemd searches
# exactly these four directories for user generators — not the user's home.
generator_found=
for d in /run/systemd/user-generators /etc/systemd/user-generators \
	/usr/local/lib/systemd/user-generators /usr/lib/systemd/user-generators; do
	[ -e "$d/podman-user-generator" ] && generator_found=1
done
[ -n "$generator_found" ] ||
	die "no podman-user-generator in any systemd user-generator directory; on hosts
	where /usr is immutable, symlink podman's quadlet binary into
	/etc/systemd/user-generators/podman-user-generator"

if command -v getenforce >/dev/null && [ "$(getenforce)" = Enforcing ]; then
	warn "SELinux is enforcing; the units as shipped are SELinux-clean only where the" \
		"policy is permissive or disabled — see the socket-handoff section of deploy/README.md"
fi

# --- 1. the service user -----------------------------------------------------

if ! id "$SERVICE_USER" >/dev/null 2>&1; then
	log "creating user $SERVICE_USER"
	useradd --create-home --shell /bin/bash "$SERVICE_USER"
else
	log "user $SERVICE_USER exists"
fi
SERVICE_UID=$(id -u "$SERVICE_USER")
SERVICE_HOME=$(getent passwd "$SERVICE_USER" | cut -d: -f6)
[ -d "$SERVICE_HOME" ] || die "$SERVICE_USER has no home directory ($SERVICE_HOME); the units resolve %h"
# The primary group and no other: a container's gid 0 resolves to exactly that
# one under the default rootless mapping, which is what reaches the socket —
# see the socket-handoff section of deploy/README.md.
SERVICE_GROUP=$(id -gn "$SERVICE_USER")
QUADLET_DIR=${QUADLET_DIR:-$SERVICE_HOME/.config/containers/systemd}

# One free range for both files, computed from both files: usermod hands out
# overlapping ranges without complaint, and two accounts sharing one are not
# isolated from each other. 65536 wide — the units run container uid 10001,
# which must fall inside the mapping.
subid_start=$({ cat /etc/subuid /etc/subgid 2>/dev/null || true; } |
	awk -F: 'BEGIN{m=100000} {e=$2+$3; if (e>m) m=e}
		END{print int((m+65535)/65536)*65536}')
ensure_subids() { # <file> <usermod flag>
	grep -q "^${SERVICE_USER}:" "$1" 2>/dev/null && return 0
	log "allocating $1 range ${subid_start}-$((subid_start + 65535))"
	usermod "$2" "${subid_start}-$((subid_start + 65535))" "$SERVICE_USER"
}
ensure_subids /etc/subuid --add-subuids
ensure_subids /etc/subgid --add-subgids

loginctl enable-linger "$SERVICE_USER"
log "waiting for the $SERVICE_USER user manager"
for _ in $(seq 30); do
	[ -d "/run/user/$SERVICE_UID/systemd" ] && break
	sleep 1
done
[ -d "/run/user/$SERVICE_UID/systemd" ] || die "user manager for $SERVICE_USER did not start under lingering"

# --- 2. the documents directory ----------------------------------------------

# The stand-in archive. Pointing the deployment at a real one means editing
# overwatch.json, hirame.env, and the Volume= lines together — README,
# "Where things go". Before the daemon: overwatch probes its first configured
# watch root at startup and fails on a path that does not exist yet.
#
# Permissions are set only on creation. An existing directory is the
# operator's archive — a recursive chgrp/chmod would rewrite it wholesale on
# every redeploy — so the grant under it stays the operator's job.
if [ ! -d "$DOCS_DIR" ]; then
	log "creating $DOCS_DIR"
	install -d -m 0750 -g "$SERVICE_GROUP" "$DOCS_DIR"
elif ! runuser -u "$SERVICE_USER" -- test -r "$DOCS_DIR"; then
	warn "$DOCS_DIR is not readable by $SERVICE_USER; grant group read access" \
		"(chgrp -R $SERVICE_GROUP, chmod -R g+rX) or an equivalent ACL"
fi

# --- 3. the overwatch daemon (host binary, system service) -------------------

install -Dm0755 "$ART/overwatch" "$OVERWATCH_BIN"

if [ -e /etc/hirame/overwatch.json ]; then
	log "keeping existing /etc/hirame/overwatch.json"
else
	install -Dm0644 "$HERE/config/overwatch.json" /etc/hirame/overwatch.json
fi
sed -e "s|/usr/local/bin/overwatch|$OVERWATCH_BIN|g" \
	-e "s|^Group=hirame$|Group=$SERVICE_GROUP|" \
	"$HERE/systemd/overwatch.service" >/etc/systemd/system/overwatch.service
chmod 0644 /etc/systemd/system/overwatch.service
install -d -m 0755 /etc/tmpfiles.d
# The group on the socket directory has to stay equal to the unit's Group= —
# the conf says why — so one derived value rewrites both. The path component
# is the deployment's name, not the account's, and stays as shipped.
sed "s|^\(d /run/overwatch/hirame .*root \)hirame|\1$SERVICE_GROUP|" \
	"$HERE/systemd/hirame-overwatch.tmpfiles.conf" \
	>/etc/tmpfiles.d/hirame-overwatch.conf
chmod 0644 /etc/tmpfiles.d/hirame-overwatch.conf
systemd-tmpfiles --create /etc/tmpfiles.d/hirame-overwatch.conf
systemctl daemon-reload
systemctl enable overwatch.service
systemctl restart overwatch.service

# The grant under test, as the README puts it — run as the service user,
# because root reaches the socket whatever the permissions say. Retried
# briefly: the daemon creates the socket after fork.
log "verifying the socket handoff as $SERVICE_USER"
ok=
for _ in $(seq 10); do
	if as_service "$OVERWATCH_BIN" client --socket "$SOCKET" status >/dev/null 2>&1; then
		ok=1
		break
	fi
	sleep 1
done
[ -n "$ok" ] || {
	systemctl --no-pager status overwatch.service || true
	stat -c '%U:%G %a %n' "$(dirname "$SOCKET")" "$SOCKET" 2>/dev/null || true
	die "$SERVICE_USER cannot reach $SOCKET; see the socket-handoff section of deploy/README.md"
}

# --- 4. images, quadlets, configuration, credentials -------------------------

# Into the service user's store — the only one the units resolve against. The
# redirect is opened by this root shell, so the archives need not be readable
# by the service user.
for name in $IMAGES; do
	log "loading localhost/$name:latest"
	as_service "$PODMAN" load --quiet <"$ART/images/$name.tar"
	as_service "$PODMAN" image exists "localhost/$name:latest" ||
		die "$ART/images/$name.tar did not carry the tag localhost/$name:latest"
done
for ref in $EXTERNAL_IMAGES; do
	log "loading $ref"
	as_service "$PODMAN" load --quiet <"$ART/images/$(image_archive "$ref")"
	as_service "$PODMAN" image exists "$ref" ||
		die "$ART/images/$(image_archive "$ref") did not carry the tag $ref"
done

# Catches unit syntax errors before an install does. Checked where podman
# ships it and next to a non-default $PODMAN (static dists); skipped rather
# than failed where the installation differs.
QUADLET_BIN=/usr/libexec/podman/quadlet
[ -x "$QUADLET_BIN" ] ||
	QUADLET_BIN="$(dirname "$(command -v "$PODMAN")")/../libexec/podman/quadlet"
if [ -x "$QUADLET_BIN" ]; then
	log "validating quadlet units"
	QUADLET_UNIT_DIRS="$HERE/quadlet" "$QUADLET_BIN" -user -dryrun >/dev/null
fi

# Directories as the service user so nothing under its home is root-owned,
# files by root so the checkout need not be readable by that user.
log "installing quadlet units and configuration"
as_service mkdir -p "$QUADLET_DIR" \
	"$SERVICE_HOME/.config/systemd/user" "$SERVICE_HOME/.config/hirame"
as_service chmod 0700 "$SERVICE_HOME/.config/hirame"
cp -r "$HERE/quadlet/." "$QUADLET_DIR/"
chown -R "$SERVICE_USER:$SERVICE_GROUP" "$QUADLET_DIR"

# See the comment in the timer unit: required where podman lacks the systemd
# build tag, redundant but harmless on a full build. The sed mirrors the
# OVERWATCH_BIN treatment of overwatch.service: the unit runs under the user
# manager, whose PATH need not resolve a non-default $PODMAN.
sed "s|PODMAN=podman|PODMAN=$PODMAN|" "$HERE/systemd/hirame-healthcheck-driver.service" \
	>"$SERVICE_HOME/.config/systemd/user/hirame-healthcheck-driver.service"
install -m 0644 -o "$SERVICE_USER" -g "$SERVICE_GROUP" \
	"$HERE/systemd/hirame-healthcheck-driver.timer" \
	"$SERVICE_HOME/.config/systemd/user/"
chown "$SERVICE_USER:$SERVICE_GROUP" "$SERVICE_HOME/.config/systemd/user/hirame-healthcheck-driver.service"
chmod 0644 "$SERVICE_HOME/.config/systemd/user/hirame-healthcheck-driver.service"

# hirame.env and postgresql.conf carry deployment tuning; keep operator
# edits. 0644 on purpose, postgresql.conf especially: it is bind-mounted and
# read by the container's mapped uid — see the README.
install_config() { # <source name> <target name>
	if [ -e "$SERVICE_HOME/.config/hirame/$2" ]; then
		log "keeping existing ~$SERVICE_USER/.config/hirame/$2"
	else
		install -m 0644 -o "$SERVICE_USER" -g "$SERVICE_GROUP" \
			"$HERE/config/$1" "$SERVICE_HOME/.config/hirame/$2"
	fi
}
install_config hirame.env.template hirame.env
install_config postgresql.conf postgresql.conf

if [ -e "$SERVICE_HOME/.config/hirame/secrets.env" ]; then
	log "keeping existing credentials"
else
	log "generating credentials"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	sed -e "s/__DB_PASSWORD__/$(openssl rand -hex 24)/g" \
		-e "s/__S3_ACCESS_KEY__/$(openssl rand -hex 16)/g" \
		-e "s/__S3_SECRET_KEY__/$(openssl rand -hex 32)/g" \
		"$HERE/config/secrets.env.template" >"$tmp/secrets.env"
	install -m 0600 -o "$SERVICE_USER" -g "$SERVICE_GROUP" \
		"$tmp/secrets.env" "$SERVICE_HOME/.config/hirame/secrets.env"
fi

# --- 5. generate and start ---------------------------------------------------

log "starting the stack"
as_service systemctl --user daemon-reload
as_service systemctl --user enable --now hirame-healthcheck-driver.timer
if as_service systemctl --user is-active --quiet hirame-pod.service; then
	# One stop transaction, one start transaction. Sequential restarts race:
	# the second command's transaction cancels the first's still-running start
	# job mid-migration ("Job for hirame-migrate.service canceled"). The stop
	# resets the migrate oneshot, and Requires= pulls it back in, ordered,
	# with the applications.
	as_service systemctl --user stop \
		hirame-migrate.service search-api.service indexer.service web-gui.service
	as_service systemctl --user start search-api.service indexer.service web-gui.service
else
	as_service systemctl --user start hirame-pod.service
fi

# Generous: nothing is pulled any more, but the first start still initialises
# the database, runs every migration, and cold-starts the whole pod before
# anything can become healthy.
ok=
for _ in $(seq 300); do
	if as_service systemctl --user is-active --quiet web-gui.service; then
		ok=1
		break
	fi
	sleep 2
done
as_service systemctl --user --no-pager list-units 'hirame*' 'postgres*' 'tika*' \
	'versitygw*' 'gahaku*' 'search-api*' 'indexer*' 'web-gui*' || true
[ -n "$ok" ] || die "web-gui.service did not come up; inspect with:
	sudo runuser -u $SERVICE_USER -- env XDG_RUNTIME_DIR=/run/user/$SERVICE_UID \\
		journalctl --user -u hirame-migrate -u search-api -u web-gui"

log "done — the GUI is on http://<host>:8080"
log "put documents under $DOCS_DIR ($SERVICE_GROUP-group-readable), or point the"
log "deployment at a real archive per deploy/README.md, 'Where things go'"
log "back up the database: see deploy/README.md, 'Volumes, ownership, and backup'"
