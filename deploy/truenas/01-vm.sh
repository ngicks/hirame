#!/usr/bin/env bash
# Lifecycle of the throwaway TrueNAS SCALE VM that hirame gets deployed into:
# a qcow2 overlay on the read-only golden image, three empty data disks for the
# pool the next script builds, and the three forwarded ports the rest of the
# run reaches the guest through.
#
# The guest is TrueNAS SCALE 25.10 and the sibling scripts' middleware/API use
# is pinned to it; a different golden image needs those call sites re-checked.
#
#   01-vm.sh up        create the disks, define and start the domain (default)
#   01-vm.sh down      ask the guest to shut down, wait for it
#   01-vm.sh destroy   undefine and delete this instance's disks (asks first)
#   01-vm.sh destroy --yes   the same, without the confirmation (automation)
#   01-vm.sh status    domain state and the forwarded ports
set -euo pipefail

. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

domain_exists() { $SUDO virsh dominfo "$TRUENAS_VM_NAME" >/dev/null 2>&1; }
domain_state() { $SUDO virsh domstate "$TRUENAS_VM_NAME" 2>/dev/null || true; }

preflight() {
	[ -r "$TRUENAS_GOLDEN" ] ||
		die "golden image not readable: $TRUENAS_GOLDEN (set TRUENAS_GOLDEN)"
	for cmd in qemu-img passt virsh; do
		command -v "$cmd" >/dev/null || die "required command not found: $cmd"
	done

	local err
	if ! err=$($SUDO virsh version 2>&1); then
		printf '%s\n' "$err" >&2
		die "cannot reach a libvirt daemon; 'up' needs one running, built against passt (libvirt >= 9.2) for the port forwards below"
	fi
}

# Rendered on every `up` so the environment knobs stay the single source of
# truth for what the domain looks like.
domain_xml() {
	local emulator="" disks="" i
	local targets=(vdb vdc vdd)

	# Only when virsh runs unescalated: then the daemon shares this shell's
	# environment and libvirt would not find a qemu living in a nix profile or
	# another custom prefix on its own. When virsh needs sudo, the daemon is the
	# system one and must get the system qemu — a user-profile path named here
	# would be unreadable or plain wrong for it — so the element is omitted and
	# libvirt's own emulator lookup applies.
	if [ -z "$SUDO" ] && command -v qemu-system-x86_64 >/dev/null; then
		emulator="    <emulator>$(command -v qemu-system-x86_64)</emulator>"
	fi

	# Serials matter: TrueNAS omits disks that report none from the disk list
	# pool creation draws on, so the pool the next script builds would find
	# nothing. VIRTIO_BLK_ID_BYTES caps these at 20 bytes.
	for i in 0 1 2; do
		disks+="
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='$TRUENAS_INSTANCE_DIR/data-$i.qcow2'/>
      <target dev='${targets[$i]}' bus='virtio'/>
      <serial>hirame-data-$i</serial>
    </disk>"
	done

	# machine='pc' (i440fx), not q35: on q35 the virtio devices sit behind PCIe
	# root ports and are exposed modern-only, and this guest's kernel fails
	# that probe -- it then boots with no disks and no virtio-serial at all,
	# looking like a hang rather than an error. Do not "modernize" this.
	cat <<EOF
<domain type='kvm'>
  <name>$TRUENAS_VM_NAME</name>
  <memory unit='MiB'>$TRUENAS_VM_MEM</memory>
  <currentMemory unit='MiB'>$TRUENAS_VM_MEM</currentMemory>
  <vcpu placement='static'>$TRUENAS_VM_CPUS</vcpu>
  <os>
    <type arch='x86_64' machine='pc'>hvm</type>
    <boot dev='hd'/>
  </os>
  <!-- Without an explicit features block libvirt runs the machine with
       acpi=off, and this guest's kernel hangs early in boot without ACPI. -->
  <features>
    <acpi/>
    <apic/>
  </features>
  <cpu mode='host-passthrough' check='none'/>
  <clock offset='utc'/>
  <on_poweroff>destroy</on_poweroff>
  <on_reboot>restart</on_reboot>
  <on_crash>destroy</on_crash>
  <devices>
$emulator
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='$TRUENAS_INSTANCE_DIR/system.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>$disks
    <interface type='user'>
      <backend type='passt'/>
      <model type='virtio'/>
      <portForward proto='tcp' address='0.0.0.0'>
        <range start='$TRUENAS_PORT_HTTPS' to='443'/>
      </portForward>
      <portForward proto='tcp' address='0.0.0.0'>
        <range start='$TRUENAS_PORT_SSH' to='22'/>
      </portForward>
      <portForward proto='tcp' address='0.0.0.0'>
        <range start='$TRUENAS_PORT_GUI' to='8080'/>
      </portForward>
    </interface>
    <serial type='pty'/>
    <console type='pty'/>
  </devices>
</domain>
EOF
}

cmd_up() {
	preflight

	# Both halves of an instance are refused separately: a leftover of either
	# one means the previous run was not torn down, and reusing it would boot
	# a guest whose disks and definition disagree.
	if domain_exists; then
		die "libvirt domain $TRUENAS_VM_NAME already exists; run '${0##*/} destroy' first"
	fi
	if $SUDO test -e "$TRUENAS_INSTANCE_DIR"; then
		die "$TRUENAS_INSTANCE_DIR already exists; run '${0##*/} destroy' first"
	fi

	log "creating disks under $TRUENAS_INSTANCE_DIR"
	$SUDO mkdir -p "$TRUENAS_INSTANCE_DIR"
	# An overlay, so the golden image is only ever opened read-only and stays
	# reusable for the next instance.
	$SUDO qemu-img create -q -f qcow2 -b "$TRUENAS_GOLDEN" -F qcow2 \
		"$TRUENAS_INSTANCE_DIR/system.qcow2"
	local i
	for i in 0 1 2; do
		$SUDO qemu-img create -q -f qcow2 \
			"$TRUENAS_INSTANCE_DIR/data-$i.qcow2" "$TRUENAS_DATA_SIZE"
	done

	# Kept beside the disks rather than in a temporary file: it is the first
	# thing worth reading when a guest will not boot.
	log "defining domain $TRUENAS_VM_NAME"
	domain_xml | $SUDO tee "$TRUENAS_INSTANCE_DIR/domain.xml" >/dev/null
	$SUDO virsh define "$TRUENAS_INSTANCE_DIR/domain.xml" >/dev/null
	$SUDO virsh start "$TRUENAS_VM_NAME" >/dev/null

	log "started; the appliance boots in a few minutes, and 02-kit.sh switches ssh on and kits it"
	tn_port_table
}

cmd_down() {
	domain_exists || die "no libvirt domain named $TRUENAS_VM_NAME"

	log "asking $TRUENAS_VM_NAME to shut down"
	$SUDO virsh shutdown "$TRUENAS_VM_NAME" >/dev/null || true

	local i
	for ((i = 0; i < 120; i++)); do
		if [ "$(domain_state)" = "shut off" ]; then
			log "shut off"
			return 0
		fi
		sleep 1
	done
	log "still '$(domain_state)' after 120s; '${0##*/} destroy' stops it the hard way"
}

cmd_destroy() {
	local reply
	if [ "${1:-}" != "--yes" ]; then
		# Read the answer from the terminal, not stdin: a pipe or a redirect
		# feeding this script must not be able to confirm on the operator's
		# behalf.
		printf 'Destroy domain %s and delete %s? [y/N] ' \
			"$TRUENAS_VM_NAME" "$TRUENAS_INSTANCE_DIR" >&2
		read -r reply </dev/tty ||
			die "no terminal to confirm on; pass --yes to skip the prompt"
		case "$reply" in
		y | Y | yes | YES) ;;
		*) die "cancelled" ;;
		esac
	fi

	$SUDO virsh destroy "$TRUENAS_VM_NAME" >/dev/null 2>&1 || true
	if domain_exists; then
		$SUDO virsh undefine "$TRUENAS_VM_NAME" --nvram >/dev/null 2>&1 ||
			$SUDO virsh undefine "$TRUENAS_VM_NAME" >/dev/null
	fi

	# lib.sh has already refused a TRUENAS_VM_NAME that could steer this
	# removal out of the image tree; these two re-check the assembled path
	# because TRUENAS_IMAGE_DIR is a knob too, and `rm -rf` as root is worth
	# checking twice. A `..` anywhere is fatal: the kernel resolves it, so a
	# prefix match alone still lets the path climb back out.
	case "$TRUENAS_INSTANCE_DIR" in
	*/../* | */.. | ../* | ..)
		die "refusing to delete $TRUENAS_INSTANCE_DIR: the path contains '..'"
		;;
	esac
	case "$TRUENAS_INSTANCE_DIR" in
	"$TRUENAS_IMAGE_DIR"/?*) ;;
	*) die "refusing to delete $TRUENAS_INSTANCE_DIR: not a subdirectory of $TRUENAS_IMAGE_DIR" ;;
	esac
	$SUDO rm -rf "$TRUENAS_INSTANCE_DIR"

	log "destroyed $TRUENAS_VM_NAME"
}

cmd_status() {
	local state
	if ! state=$($SUDO virsh domstate "$TRUENAS_VM_NAME" 2>&1); then
		printf '%s\n' "$state" >&2
		die "cannot read the state of $TRUENAS_VM_NAME: no libvirt daemon reachable, or the domain is not defined"
	fi
	log "$TRUENAS_VM_NAME: $state"
	tn_port_table
}

case "${1:-up}" in
up) cmd_up ;;
down) cmd_down ;;
destroy)
	shift
	cmd_destroy "$@"
	;;
status) cmd_status ;;
*) die "unknown subcommand: ${1:-} (want: up | down | destroy | status)" ;;
esac
