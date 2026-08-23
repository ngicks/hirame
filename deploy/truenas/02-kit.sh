#!/usr/bin/env bash
# Kits the freshly booted TrueNAS VM: the storage pool, the datasets under it,
# the two accounts hirame runs as, and the directory the watcher indexes.
#
# Nothing here reaches for zpool, groupadd or useradd. Every object is created
# through the middleware (`midclt call`) so that it is recorded in the TrueNAS
# configuration database: the appliance re-imports pools and regenerates the
# account files in /etc from that database on every boot, and anything written
# behind its back is gone by the next one.
#
# The guest is TrueNAS SCALE 25.10 and every method name and payload below is
# pinned to that release's middleware API. A different golden image means
# re-checking each call, not just the constants.
#
# Safe to re-run: every create is guarded by a query and skipped when the object
# is already there. The grants are the exception — ownership and permissions on
# the homes and on the documents directory are re-asserted on every run, because
# those are what the rest of the deployment depends on and a wrong one fails far
# away from here.
set -euo pipefail

. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

[ $# -eq 0 ] || die "takes no arguments"

POOL=tank
HOME_DATASET=$POOL/home
SHARE_DATASET=$POOL/share
DOCUMENTS_DIR=/mnt/$SHARE_DATASET/documents
HIRAME_HOME=/mnt/$HOME_DATASET/hirame
PODMAN_HOME=/mnt/$HOME_DATASET/podman

# The serials 01-vm.sh stamps on the three data disks, and the only way to tell
# them apart from any other disk the appliance happens not to be using yet.
# Choosing disks by position or by size would just as happily swallow one that
# is not ours.
DATA_SERIALS=(hirame-data-0 hirame-data-1 hirame-data-2)

# Set as soon as anything is actually created, so the closing line can tell a
# first run apart from a re-run over an already kitted VM.
CHANGED=0

# ---------------------------------------------------------------------------
# guest access
# ---------------------------------------------------------------------------

mid() { # mid <method> [json_arg...] -- middleware call, reply on stdout
	tn_run sudo midclt call "$@"
}

# pool.create is a middleware *job*: the plain call returns a job id the moment
# the work is queued, which would leave the datasets below asking for a pool
# that is still being built. -j waits for the job to finish and fails with it.
mid_job() { # mid_job <method> [json_arg...]
	tn_run sudo midclt call -j "$@"
}

# The middleware's replies are parsed here rather than in the guest: nothing in
# the golden image promises the appliance carries jq.
mid_exists() { # mid_exists <query method> <filters json>
	local count
	count=$(mid "$1" "$2" | jq 'length') ||
		die "$1 failed in the guest (its error is above)"
	[ "$count" -gt 0 ]
}

# ---------------------------------------------------------------------------
# pool and datasets
# ---------------------------------------------------------------------------

ensure_pool() {
	if mid_exists pool.query "$(jq -nc --arg n "$POOL" '[["name","=",$n]]')"; then
		log "pool $POOL: already kitted"
		return 0
	fi

	local unused disks
	unused=$(mid disk.get_unused) ||
		die "disk.get_unused failed in the guest (its error is above)"
	# Selected in the order the serials are listed, so the vdev is assembled
	# the same way on every host regardless of how the guest enumerated them.
	disks=$(printf '%s' "$unused" | jq -c \
		--argjson want "$(jq -nc '$ARGS.positional' --args "${DATA_SERIALS[@]}")" \
		'[ $want[] as $s | (map(select(.serial == $s)) | .[0].name // empty) ]')

	if [ "$(printf '%s' "$disks" | jq 'length')" -ne "${#DATA_SERIALS[@]}" ]; then
		local seen
		seen=$(printf '%s' "$unused" | jq -r '[.[] | "\(.name) (\(.serial))"] | join(", ")')
		die "pool $POOL does not exist and the data disks are not all free; expected serials ${DATA_SERIALS[*]}, but the guest reports these unused disks: ${seen:-none}. Was the VM created by 01-vm.sh?"
	fi

	# One RAIDZ1 data vdev and nothing else. A separate log or cache vdev would
	# have to come out of the same three disks -- which are three files on one
	# host filesystem here -- so it would cost capacity and buy no locality.
	log "creating pool $POOL over $(printf '%s' "$disks" | jq -r 'join(" ")') (raidz1); this takes a moment"
	mid_job pool.create "$(jq -nc --arg name "$POOL" --argjson disks "$disks" \
		'{name: $name, topology: {data: [{type: "RAIDZ1", disks: $disks}]}}')" >/dev/null
	CHANGED=1
}

ensure_dataset() { # ensure_dataset <pool/name>
	local name=$1
	# A dataset's `id` is its pool-qualified path, which is the field that
	# names it unambiguously.
	if mid_exists pool.dataset.query "$(jq -nc --arg n "$name" '[["id","=",$n]]')"; then
		log "dataset $name: already kitted"
		return 0
	fi
	log "creating dataset $name"
	mid pool.dataset.create "$(jq -nc --arg n "$name" '{name: $n}')" >/dev/null
	CHANGED=1
}

# ---------------------------------------------------------------------------
# accounts
# ---------------------------------------------------------------------------

group_api_id() { # group_api_id <name> -- the group's API id, empty if absent
	# The whole list is fetched and matched here instead of filtering in the
	# guest because the key holding a group's name differs between middleware
	# releases (`group` in the older schema, `name` in the newer one); reading
	# both leaves the guard working either way, and the list is short.
	mid group.query | jq -r --arg n "$1" \
		'map(select((.name // .group) == $n)) | .[0].id // empty'
}

# The result is left in GROUP_API_ID instead of being printed: a caller reading
# it back through $(...) would run this in a subshell, and the CHANGED flag set
# in there would be discarded with it.
GROUP_API_ID=""
ensure_group() { # ensure_group <name>
	local name=$1
	GROUP_API_ID=$(group_api_id "$name")
	if [ -n "$GROUP_API_ID" ]; then
		log "group $name: already kitted"
		return 0
	fi

	log "creating group $name"
	# smb:false: these groups exist to own files on the host and to back the
	# container runtime's gid 0, never to appear in an SMB share ACL, so there
	# is no reason to map them to an NT group and hand them a sid.
	mid group.create "$(jq -nc --arg n "$name" '{name: $n, smb: false}')" >/dev/null
	CHANGED=1

	GROUP_API_ID=$(group_api_id "$name")
	[ -n "$GROUP_API_ID" ] ||
		die "group $name was created but does not come back from group.query"
}

ensure_user() { # ensure_user <username> <primary group name> <home> <payload>
	local username=$1 group=$2 home=$3 payload=$4

	# The middleware stores `home` as given and rejects a path that is not
	# there yet, so the directory is made first. Its own home_create is left
	# alone deliberately: it appends the username to the path it is handed
	# unless that path already ends in one, which makes the resulting home
	# depend on how the argument happened to be spelled.
	tn_run sudo mkdir -p "$home"

	if mid_exists user.query "$(jq -nc --arg n "$username" '[["username","=",$n]]')"; then
		log "user $username: already kitted"
	else
		log "creating user $username"
		mid user.create "$payload" >/dev/null
		CHANGED=1
	fi

	# Re-asserted on every run rather than only right after creation: this home
	# is where the container runtime keeps all of its state, and one left owned
	# by the wrong account is exactly the silent breakage this script exists to
	# rule out.
	tn_run sudo chown "$username:$group" "$home"
	tn_run sudo chmod 700 "$home"
}

ensure_accounts() {
	local hirame_group podman_group

	ensure_group hirame
	hirame_group=$GROUP_API_ID
	ensure_group podman
	podman_group=$GROUP_API_ID

	# password_disabled pairs with smb:false because the middleware refuses to
	# disable password authentication for an account that may reach SMB shares.
	# Both of these are service accounts that are only ever entered through
	# sudo/runuser from the appliance itself.
	ensure_user hirame hirame "$HIRAME_HOME" \
		"$(jq -nc --arg u hirame --argjson g "$hirame_group" --arg h "$HIRAME_HOME" '{
			username: $u,
			full_name: "hirame service account",
			group: $g,
			home: $h,
			password_disabled: true,
			smb: false
		}')"

	# podman runs the rootless container stack, and it gets a primary group of
	# its own for privilege separation: a rootless container's gid 0 maps to
	# the runner's *primary* group, so anything that group can read, every
	# container can read. Making hirame that group would hand the containers
	# the hirame account's files wholesale. The supplementary hirame membership
	# is only host-side file sharing between the two accounts -- supplementary
	# groups do not follow the runner into a container.
	#
	# `group` and `groups` take middleware entry ids, not Unix gids.
	ensure_user podman podman "$PODMAN_HOME" \
		"$(jq -nc --arg u podman --argjson g "$podman_group" \
			--argjson aux "$hirame_group" --arg h "$PODMAN_HOME" '{
			username: $u,
			full_name: "hirame rootless container runner",
			group: $g,
			groups: [$aux],
			home: $h,
			shell: "/usr/bin/bash",
			password_disabled: true,
			smb: false
		}')"
}

# ---------------------------------------------------------------------------
# documents
# ---------------------------------------------------------------------------

# documents is a plain directory inside the tank/share dataset and must never
# become a dataset of its own. The watcher registers a fanotify mark covering a
# whole filesystem, and one mark covers exactly one filesystem: a nested
# dataset is a separate filesystem, so every file under it would drop out of
# the watch with nothing raised to notice it by.
ensure_documents() {
	if tn_run sudo test -d "$DOCUMENTS_DIR"; then
		log "documents directory $DOCUMENTS_DIR: already kitted"
	else
		log "creating documents directory $DOCUMENTS_DIR"
		tn_run sudo mkdir -p "$DOCUMENTS_DIR"
		CHANGED=1
	fi

	# Re-asserted on every run: this grant is the entire path by which the
	# container stack reads the documents, and a directory that already exists
	# carrying some other group would fail far away from here.
	tn_run sudo chgrp podman "$DOCUMENTS_DIR"
	tn_run sudo chmod g+rX "$DOCUMENTS_DIR"
}

# ---------------------------------------------------------------------------

preflight() {
	command -v jq >/dev/null ||
		die "required command not found: jq (middleware replies are parsed here, not in the guest)"

	tn_wait_ssh

	# One call that exercises the whole chain at once -- ssh, the password, and
	# sudo without a prompt -- so a golden image missing any of them is named
	# here instead of failing halfway through the kitting below. -n makes sudo
	# fail rather than sit waiting for a password on a session with no tty.
	local err
	if ! err=$(tn_ssh sudo -n midclt call system.info 2>&1 >/dev/null); then
		printf '%s\n' "$err" >&2
		die "cannot reach the middleware as truenas_admin; the golden image is expected to have ssh enabled, password authentication for truenas_admin (TRUENAS_ADMIN_PASSWORD) and passwordless sudo for that account"
	fi
}

preflight
ensure_pool
ensure_dataset "$HOME_DATASET"
ensure_dataset "$SHARE_DATASET"
ensure_accounts
ensure_documents

if [ "$CHANGED" -eq 1 ]; then
	log "kitted: pool $POOL, datasets $HOME_DATASET and $SHARE_DATASET, users hirame and podman, documents at $DOCUMENTS_DIR"
else
	log "already kitted: nothing to change"
fi
