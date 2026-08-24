#!/usr/bin/env bash
# Deploy hirame into the kitted VM: build the deploy/build.sh bundle here, ship
# it to the guest, install the shim that keeps rootless podman's /etc state
# across guest reboots, and run the bundle's own deploy.sh in there.
#
# The guest is TrueNAS SCALE 25.10. Everything below is pinned to what the
# sibling scripts left in that guest — the podman account and its home on the
# pool, the static podman dist under it, the documents directory — and to that
# release's habit of rebuilding /etc from its configuration database at every
# boot, which is the whole reason for the shim.
#
#	04-deploy.sh                 build the bundle, then deploy
#	SKIP_BUILD=1 04-deploy.sh    deploy deploy/dist as it stands
#
# The bundle is rebuilt on every run unless SKIP_BUILD=1: build.sh alone knows
# what its output depends on, and silently deploying a stale dist costs far
# more than the rebuild does.
#
# Re-running is the redeploy path: the bundle is reshipped, the shim
# re-installed with the range the guest already has, and deploy.sh re-run —
# it is idempotent, and so is the shim.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/lib.sh"
REPO=$(cd "$HERE/../.." && pwd)
DIST=$REPO/deploy/dist
IMAGES="hirame-search-api hirame-web-gui gahaku"

# The bundle carries the pinned registry images as well, because the guest is
# not guaranteed a route to docker.io. Which those are is derived from the
# units, exactly as build.sh and deploy.sh derive it, so this check cannot
# describe a bundle different from the one they build and load.
external_images() { # <quadlet dir>
	grep -rh '^Image=' "$1" | sed -e '/^Image=localhost\//d' -e 's/^Image=//' | sort -u
}
image_archive() { # <image ref>
	printf '%s.tar\n' "${1//[\/:]/_}"
}

# The guest layout the earlier scripts created. The home is on the pool
# because /home is mounted noexec there and rootless podman execs its helpers
# out of ~/.local.
GUEST_HOME=/mnt/tank/home/podman
GUEST_DIST=$GUEST_HOME/.local/share/podman-dist/current
GUEST_PODMAN=$GUEST_DIST/usr/local/bin/podman
GUEST_QUADLET=$GUEST_DIST/usr/local/libexec/podman/quadlet
# The dist's QUADLET_UNIT_DIRS entry (from its 50-podman.conf), so the user
# generator finds the units deploy.sh installs there.
GUEST_QUADLET_DIR=$GUEST_HOME/.config/containers-quadlet
GUEST_DOCS=/mnt/tank/share/documents
GUEST_OVERWATCH=/var/lib/overwatch/overwatch
# The bundle is unpacked on the pool: it carries several GiB of image
# archives, and the pool is both the roomy filesystem in that guest and an
# exec one, which running deploy.sh out of the unpacked bundle needs.
STAGE=/mnt/tank/hirame-deploy

# --- 1. the bundle -----------------------------------------------------------

command -v curl >/dev/null ||
	die "required command not found: curl (the final check reaches the GUI with it)"

if [ "${SKIP_BUILD:-}" = 1 ]; then
	log "SKIP_BUILD=1: shipping $DIST as it stands"
	# The same artifacts deploy.sh refuses to start without, checked here so a
	# half-built dist fails before the whole bundle goes over the wire.
	[ -x "$DIST/deploy.sh" ] && [ -x "$DIST/overwatch" ] && [ -f "$DIST/quadlet/hirame.pod" ] ||
		die "$DIST is not a complete bundle; unset SKIP_BUILD to build it"
	for name in $IMAGES; do
		[ -f "$DIST/images/$name.tar" ] ||
			die "no $DIST/images/$name.tar; unset SKIP_BUILD to build it"
	done
	EXTERNAL_IMAGES=$(external_images "$DIST/quadlet")
	for ref in $EXTERNAL_IMAGES; do
		[ -f "$DIST/images/$(image_archive "$ref")" ] ||
			die "no archive for $ref in $DIST/images; the guest has no route to a
	registry and its stack would fail to start — unset SKIP_BUILD to rebuild the bundle"
	done
	# A bundle built before deploy.sh grew the account knobs hardcodes the
	# hirame user, silently ignores the SERVICE_USER passed below, and then
	# dies deep in the guest with a permission error on podman's home. Refuse
	# it here, where the fix (rebuild) is obvious.
	grep -q 'SERVICE_USER' "$DIST/deploy.sh" && grep -q 'QUADLET_DIR' "$DIST/deploy.sh" ||
		die "$DIST/deploy.sh is from before the SERVICE_USER/QUADLET_DIR knobs and would deploy as the hirame user; unset SKIP_BUILD to rebuild the bundle"
else
	# Both are build-host requirements only — the guest needs neither — so they
	# are checked before build.sh gets far enough to leave a partial dist
	# behind. $PODMAN is the knob build.sh itself reads.
	for cmd in "${PODMAN:-podman}" go; do
		command -v "$cmd" >/dev/null ||
			die "required command not found: $cmd; the bundle is built on this host (install it, or
	run with SKIP_BUILD=1 to ship the $DIST of an earlier build)"
	done
	log "building the deployment bundle"
	"$REPO/deploy/build.sh"
fi

# --- 2. ship it --------------------------------------------------------------

tn_wait_ssh

# Everything below escalates in the guest, and a password prompt on a
# tty-less ssh channel would hang rather than say so.
tn_ssh sudo -n true >/dev/null 2>&1 ||
	die "truenas_admin cannot sudo without a password in the guest; run
	deploy/truenas/02-kit.sh, whose preflight grants it"

# No trap: lib.sh owns the EXIT trap for its askpass helper. Only the small
# shim files land here; the directory goes at the end.
WORK=$(mktemp -d) || die "could not create a temporary directory"

# Streamed, never staged: the bundle carries every image the units name and so
# runs to several GiB, more than a tmpfs /tmp holds — and writing it out only
# to read it back doubles the I/O for nothing. Uncompressed because the
# transfer is a port forward into a VM on this same host, where compressing
# that much costs more than it saves.
log "shipping the bundle to $STAGE ($(du -sh "$DIST" | cut -f1))"
tn_ssh "sudo rm -rf $STAGE && sudo install -d -m 0755 -o truenas_admin $STAGE"
tar -C "$DIST" -cf - . | tn_ssh "tar -xf - -C $STAGE"

# deploy.sh's DOCS_DIR knob creates the directory and grants group access, but
# the path itself is baked into overwatch.json, hirame.env and the two
# Volume= lines — an edit deploy/README.md ("Where things go") leaves to the
# operator. This is that edit, applied to the just-unpacked bundle. The same
# host path is kept on both sides of Volume= because the indexer records host
# paths and the search side must resolve the identical strings.
log "pointing the bundle's documents path at $GUEST_DOCS"
tn_ssh "grep -rl /srv/documents \
	$STAGE/config/overwatch.json $STAGE/config/hirame.env.template \
	$STAGE/quadlet/search-api.container $STAGE/quadlet/indexer.container \
	$STAGE/systemd/overwatch.service \
	| xargs -r sed -i 's|/srv/documents|$GUEST_DOCS|g'"
# The list above is easy to leave a new file out of, and the miss shows up far
# from here — a Volume= or a RequiresMountsFor= naming a path that does not
# exist in this guest. So it is asserted rather than trusted. Only these three
# directories: deploy.sh itself carries /srv/documents as its DOCS_DIR default
# and in its comments, which is correct and must stay.
leftover=$(tn_ssh "grep -rl /srv/documents \
	$STAGE/config $STAGE/quadlet $STAGE/systemd; test \$? -eq 1" 2>&1) ||
	die "the shipped bundle still names /srv/documents after the rewrite:
	${leftover:-(grep failed; its message is above)}
	add the file to the rewrite list in ${0##*/}"
# A config already installed by an earlier run is deliberately kept by
# deploy.sh; one still pointing at the shipped default is ours to heal, not
# operator tuning to preserve.
tn_ssh "for f in /etc/hirame/overwatch.json $GUEST_HOME/.config/hirame/hirame.env; do
	if sudo test -e \$f && sudo grep -q /srv/documents \$f; then
		sudo sed -i 's|/srv/documents|$GUEST_DOCS|g' \$f
	fi
done"

# --- 3. the /etc persistence shim --------------------------------------------

# The range is settled once, here, and baked into the shim: the shim then
# writes the same lines every boot. Reusing whatever the guest already has
# matters on a re-run — a second, different range for one account is what
# shadow-utils reads as an overlap, and images already loaded were mapped
# through the first one.
#
# One range for both files. The container uid and gid maps are the same
# width, and keeping them equal is what makes the reuse check above a single
# question instead of two that can disagree.
log "settling the podman subuid/subgid range"
SUBID_LINE=$(tn_ssh sh <<'EOF'
line=$(grep '^podman:' /etc/subuid 2>/dev/null | head -n1 || true)
if [ -n "$line" ]; then
	printf '%s\n' "$line"
	exit 0
fi
# The first 65536-aligned range past every entry in both files, and never
# below 100000 — below that is where distributions put system accounts.
start=$({ cat /etc/subuid /etc/subgid 2>/dev/null || true; } |
	awk -F: 'NF>=3 {e=$2+$3; if (e>m) m=e}
		END{if (m<100000) print 100000; else print int((m+65535)/65536)*65536}')
printf 'podman:%s:65536\n' "$start"
EOF
)
# It is about to be sed-substituted into a script that runs as root at every
# boot, so nothing but a subid entry is allowed through.
[[ $SUBID_LINE =~ ^podman:[0-9]+:[0-9]+$ ]] ||
	die "could not read a subuid range from the guest, got: '$SUBID_LINE'"
log "podman subid range: $SUBID_LINE"

# On a copy — the template in config/ stays a template.
sed -e "s|@SUBUID_LINE@|$SUBID_LINE|" \
	-e "s|@SUBGID_LINE@|$SUBID_LINE|" \
	-e "s|@QUADLET_BIN@|$GUEST_QUADLET|" \
	"$HERE/config/truenas-prep.sh" >"$WORK/truenas-prep.sh"
# Catches a template that grew a token this script does not know about,
# before the half-filled script is installed as a boot-time root job.
! grep -q '@[A-Za-z_]*@' "$WORK/truenas-prep.sh" ||
	die "unsubstituted tokens left in truenas-prep.sh: $(grep -o '@[A-Za-z_]*@' "$WORK/truenas-prep.sh" | sort -u | tr '\n' ' ')"

log "installing the boot-persistence shim"
tn_scp "$WORK/truenas-prep.sh" "$HERE/config/truenas-prep.service" \
	"$HERE/config/truenas-prep.path" "$HERE/config/truenas-prep.timer" \
	"truenas_admin@$TRUENAS_HOST:/tmp/"
# restart, not `enable --now`: restart runs the script just installed
# unconditionally, whatever the unit was doing beforehand — a run still in
# flight from the path unit included. The path unit rides along because
# TrueNAS rewrites /etc/subuid and /etc/subgid again later in the boot
# (observed on 25.10), after the boot-ordered service already ran — the watch
# re-applies the entries whenever that happens.
tn_ssh sudo sh <<'EOF'
set -e
install -d -m 0755 /var/lib/hirame-deploy
install -m 0755 /tmp/truenas-prep.sh /var/lib/hirame-deploy/truenas-prep.sh
install -m 0644 /tmp/truenas-prep.service /etc/systemd/system/truenas-prep.service
install -m 0644 /tmp/truenas-prep.path /etc/systemd/system/truenas-prep.path
install -m 0644 /tmp/truenas-prep.timer /etc/systemd/system/truenas-prep.timer
rm -f /tmp/truenas-prep.sh /tmp/truenas-prep.service /tmp/truenas-prep.path /tmp/truenas-prep.timer
systemctl daemon-reload
systemctl enable truenas-prep.service truenas-prep.path truenas-prep.timer
systemctl restart truenas-prep.service
systemctl restart truenas-prep.path truenas-prep.timer
EOF

# A plain oneshot goes back to inactive on success (it must, for the path
# unit to be able to re-trigger it), so the outcome is read from Result.
state=$(tn_ssh 'systemctl show -p Result --value truenas-prep.service' 2>/dev/null || true)
[ "$state" = success ] || {
	tn_ssh 'systemctl --no-pager status truenas-prep.service' || true
	die "truenas-prep.service result is '$state', not success"
}
# What the shim was installed for, read back rather than assumed: without
# these two deploy.sh's own preflight stops the deployment, and rootless
# podman could not map a container uid anyway.
for f in /etc/subuid /etc/subgid; do
	# Captured, then matched locally: `tn_ssh ... | grep -q` closes the pipe
	# on the first hit and fails the pipeline under pipefail.
	content=$(tn_ssh "cat $f" || true)
	grep -qxF "$SUBID_LINE" <<<"$content" ||
		die "$f does not carry '$SUBID_LINE' after truenas-prep.service ran"
done
# sudo: the symlink target sits under the podman user's 0700 home, which the
# ssh account cannot traverse — an unprivileged test -x would fail on a
# perfectly healthy link.
tn_ssh 'sudo test -x /etc/systemd/user-generators/podman-user-generator' ||
	die "no working podman user generator in the guest; the shim links it to
	$GUEST_QUADLET, which is not there — check the podman dist installation"

rm -rf "$WORK"

# The podman user's first podman run predates the subuid/subgid lines the shim
# just wrote (the previous script starts podman to verify itself), so its user
# namespace still carries the fallback single mapping — under which every
# container fails with "setresgid: Invalid argument". migrate tears the stale
# pause process down; podman then picks the ranges up on next use. Idempotent.
# systemd-run for the same log_subcmds reason as the deploy run below, and the
# dist PATH because podman resolves crun/conmon from it.
log "refreshing the podman user namespace mapping (podman system migrate)"
tn_ssh "sudo systemd-run --wait --pipe --collect --quiet \
	--property=User=podman \
	--property=WorkingDirectory=$GUEST_HOME \
	--setenv=HOME=$GUEST_HOME \
	--setenv=XDG_RUNTIME_DIR=/run/user/\$(id -u podman) \
	--setenv=PATH=$GUEST_DIST/usr/local/bin:/usr/bin:/bin \
	-- $GUEST_PODMAN system migrate"

# --- 4. deploy ---------------------------------------------------------------

# The account layout knobs: SERVICE_USER=podman makes deploy.sh derive the
# podman group and rewrite the overwatch unit's Group= and the tmpfiles entry
# to it, and QUADLET_DIR puts the units where this guest's generator looks.
# The rest are the appliance paths — an immutable /usr has no room for either
# binary.
log "running the bundle's deploy.sh in the guest (this loads the images; it takes a while)"
# systemd-run, not plain sudo: TrueNAS's sudoers carries `Defaults
# log_subcmds`, whose intercept machinery kills podman's /proc/self/exe
# re-exec anywhere below sudo in the process tree, and deploy.sh drives podman
# throughout. systemd-run hands the run to PID 1, outside that tree.
tn_ssh "sudo systemd-run --wait --pipe --collect --quiet \
	--property=WorkingDirectory=$STAGE \
	-- env \
	SERVICE_USER=podman \
	QUADLET_DIR=$GUEST_QUADLET_DIR \
	PODMAN=$GUEST_PODMAN \
	OVERWATCH_BIN=$GUEST_OVERWATCH \
	DOCS_DIR=$GUEST_DOCS \
	$STAGE/deploy.sh"

# --- 5. the GUI, from outside the guest --------------------------------------

# deploy.sh already waited for web-gui.service inside the guest; this is the
# rest of the path — the port forward and the container's own listener — and
# it is still worth a wait of its own, because a unit reports active a little
# before the application answers.
GUI_URL="http://$TRUENAS_HOST:$TRUENAS_PORT_GUI"
log "waiting for the GUI on $GUI_URL"
ok=
for _ in $(seq 60); do
	if curl -fs -o /dev/null --max-time 5 "$GUI_URL"; then
		ok=1
		break
	fi
	sleep 5
done
# Quoted so the paste survives: the uid has to be looked up in the guest, and
# an unquoted \$(...) would be expanded by the operator's own shell instead.
[ -n "$ok" ] || die "no answer from $GUI_URL after 300s; look at the stack with:
	ssh -p $TRUENAS_PORT_SSH truenas_admin@$TRUENAS_HOST \\
		'sudo runuser -u podman -- env XDG_RUNTIME_DIR=/run/user/\$(id -u podman) systemctl --user list-units \"hirame*\"'"

log "hirame is up on $GUI_URL"
tn_port_table
