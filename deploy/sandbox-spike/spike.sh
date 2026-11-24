#!/usr/bin/env bash
# Every VM except the bench VM runs with no NIC; the bench VM has user-mode networking and runs only this harness's scripts.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=${SPIKE_ROOT:-/srv/waypoint-spike}
RES=$ROOT/results
SU=wpspike
SUID=39990
OWNER=${SPIKE_OWNER:-${SUDO_USER:-claude}}
WAYPOINT_SRC=${WAYPOINT_SRC:-$(cd "$HERE/../.." && pwd)}
NODE_SRC=${NODE_SRC:-}
VM_CPUS=${VM_CPUS:-4}
VM_MEM=${VM_MEM:-4096}
IMG_URL=${IMG_URL:-https://cloud-images.ubuntu.com/minimal/releases/noble/release}
IMG_FILE=ubuntu-24.04-minimal-cloudimg-amd64.img
KVER=$(uname -r)
SLICE=waypoint-spike.slice
IMAGE_REV=2

log() { echo "[$(date +%T)] $*" | tee -a "$RES/spike.log" >&2; }
die() {
	log "ERROR: $*"
	exit 1
}
need_root() { [ "$(id -u)" = 0 ] || die "run as root (sudo $0 $*)"; }

rec() {
	python3 -c '
import json, sys, time
a = sys.argv[1:] + [""]
print(json.dumps({"t": time.strftime("%Y-%m-%dT%H:%M:%S"), "test": a[0], "key": a[1], "value": a[2], "note": a[3]}))
' "$@" >>"$RES/results.jsonl"
}
now() { date +%s.%N; }
since() { awk -v a="$1" -v b="$(now)" 'BEGIN{printf "%.2f", b-a}'; }
as_owner() { # The unit has no HOME, so HOME is set to the owner's to read the owner's login profile.
	runuser -u "$OWNER" -- env HOME="$(getent passwd "$OWNER" | cut -d: -f6)" "$@"
}
owner_go() { as_owner bash -lc "go env $1"; }

record_load() {
	rec load "$1_loadavg" "$(cut -d' ' -f1-3 /proc/loadavg)"
	rec load "$1_cpu_psi_some_avg60" "$(awk '/some/{sub("avg60=","",$3); print $3}' /proc/pressure/cpu)"
	rec load "$1_mem_psi_some_avg60" "$(awk '/some/{sub("avg60=","",$3); print $3}' /proc/pressure/memory)"
	if [ -r /sys/fs/cgroup/system.slice/cpu.pressure ]; then
		rec load "$1_system_slice_cpu_psi_avg60" "$(awk '/some/{sub("avg60=","",$3); print $3}' /sys/fs/cgroup/system.slice/cpu.pressure)"
	fi
	rec load "$1_mem_available_kb" "$(awk '/MemAvailable/{print $2}' /proc/meminfo)"
}

finish() {
	if [ -d "$RES" ]; then chown -R "$OWNER" "$RES" 2>/dev/null || true; fi
}

prepare() {
	need_root prepare
	[ -c /dev/kvm ] || die "no /dev/kvm"
	command -v node >/dev/null && command -v npm >/dev/null \
		|| die "node and npm must be installed on the host: the guest image is built from the host copy"
	install -d -m 0755 -o root -g root "$ROOT"
	install -d -m 0755 -o "$OWNER" "$RES"
	log "prepare: packages"
	DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
		qemu-system-x86 qemu-utils virtiofsd cloud-guest-utils >/dev/null
	rec prepare qemu_version "$(qemu-system-x86_64 --version | head -1)"
	rec prepare virtiofsd_version "$(/usr/libexec/virtiofsd --version 2>&1 | head -1)"
	rec prepare host_kernel "$KVER"
	rec prepare systemd_version "$(systemctl --version | head -1)"

	if ! id "$SU" >/dev/null 2>&1; then
		groupadd -g "$SUID" "$SU"
		useradd -u "$SUID" -g "$SUID" -M -d /nonexistent -s /usr/sbin/nologin -c "waypoint-sandbox spike (WP-417)" "$SU"
		passwd -l "$SU" >/dev/null
	fi
	rec prepare spike_user "$(id "$SU")"

	install -d -m 0700 -o "$SU" -g "$SU" "$ROOT/vm" "$ROOT/bare"
	install -d -m 0755 -o "$OWNER" "$ROOT/stage"
	install -d -m 0700 -o root -g root "$ROOT/dl" "$ROOT/mnt"

	stage_sources
	stage_bare
	stage_image
	log "prepare: done"
}

# Workload sources are staged as the repos' owner, so root never runs git in a
# gitdir it doesn't own (PL7 §8.1).
stage_sources() {
	local name src sha
	for name in waypoint node; do
		src=$WAYPOINT_SRC
		[ "$name" = node ] && src=$NODE_SRC
		[ -d "$ROOT/stage/$name/.git" ] && continue
		[ -n "$src" ] || die "set NODE_SRC to a Node project's checkout (its npm ci && npm test is the second workload)"
		sha=$(as_owner git -C "$src" rev-parse --verify -q refs/remotes/origin/main || as_owner git -C "$src" rev-parse HEAD)
		log "stage: $name at $sha"
		as_owner git init -q "$ROOT/stage/$name"
		as_owner git -C "$ROOT/stage/$name" fetch -q --depth 1 "file://$src" "$sha"
		as_owner git -C "$ROOT/stage/$name" checkout -q --detach FETCH_HEAD
		rec prepare "${name}_commit" "$sha"
	done
	stage_go
}

# go.mod may ask for a newer Go than the owner's installed one, so the toolchain is resolved with GOTOOLCHAIN=auto.
stage_go() {
	local goroot
	if [ ! -x "$ROOT/stage/go/bin/go" ]; then
		rm -rf "$ROOT/stage/go" "$ROOT/stage/go.tmp" "$ROOT/stage/gobin" "$ROOT/stage/gobin.tmp" "$ROOT/stage/gomod"
		goroot=$(as_owner bash -lc "cd '$ROOT/stage/waypoint' && GOTOOLCHAIN=auto go env GOROOT") ||
			die "could not resolve the Go toolchain for waypoint's go.mod"
		log "stage: Go toolchain from $goroot"
		cp -a "$goroot" "$ROOT/stage/go.tmp"
		chmod -R u+w "$ROOT/stage/go.tmp"
		install -d "$ROOT/stage/gobin.tmp"
		cp -a "$(owner_go GOPATH)/bin/." "$ROOT/stage/gobin.tmp/"
		# Move go last: the -x guard above tests only that binary, so an interrupted copy must not leave it in place.
		mv "$ROOT/stage/gobin.tmp" "$ROOT/stage/gobin"
		mv "$ROOT/stage/go.tmp" "$ROOT/stage/go"
	fi
	rec prepare go_version "$(as_owner env GOTOOLCHAIN=local "$ROOT/stage/go/bin/go" version)"
	if [ ! -d "$ROOT/stage/gomod" ]; then
		log "stage: Go module cache"
		rm -rf "$ROOT/stage/gomod.tmp"
		as_owner bash -lc "export GOMODCACHE='$ROOT/stage/gomod.tmp' GOTOOLCHAIN=local PATH='$ROOT/stage/go/bin':\$PATH && cd '$ROOT/stage/waypoint' && go mod download" ||
			die "go mod download failed"
		mv "$ROOT/stage/gomod.tmp" "$ROOT/stage/gomod"
	fi
}

stage_bare() {
	install -d -o "$SU" -g "$SU" -m 0700 "$ROOT/bare"
	install -m 0755 "$HERE/bench.sh" "$ROOT/bare/bench.sh"
	if [ ! -x "$ROOT/bare/go/bin/go" ]; then
		cp -a "$ROOT/stage/go" "$ROOT/bare/go"
		chown -R "$SU:$SU" "$ROOT/bare/go"
	fi
}

# Each bare run gets a fresh home: a kept Go build cache would make bare runs faster than a VM's fresh disk.
fresh_home() {
	local h=$1
	rm -rf "$h"
	install -d "$h/go/pkg"
	cp -a --reflink=auto "$ROOT/stage/waypoint" "$ROOT/stage/node" "$h/"
	cp -a --reflink=auto "$ROOT/stage/gomod" "$h/go/pkg/mod"
	cp -a --reflink=auto "$ROOT/stage/gobin" "$h/go/bin"
	chown -R "$SU:$SU" "$h"
}

stage_image() {
	local raw=$ROOT/vm/base.raw dl=$ROOT/dl/$IMG_FILE loop mnt=$ROOT/mnt stamp
	stamp="r$IMAGE_REV-$KVER-$(cat "$HERE/guest.sh" "$HERE/bench.sh" "$HERE/probes.sh" | sha256sum | cut -c1-16)"
	[ -f "$raw" ] && [ "$(cat "$raw.stamp" 2>/dev/null)" = "$stamp" ] && return 0
	rm -f "$raw" "$raw.stamp" "$raw.tmp"
	if ! (cd "$ROOT/dl" && [ -f "$IMG_FILE" ] && grep " \*\?$IMG_FILE\$" SHA256SUMS 2>/dev/null | sha256sum -c --status); then
		log "image: download $IMG_FILE"
		curl -fsSL -o "$dl" "$IMG_URL/$IMG_FILE"
		curl -fsSL -o "$ROOT/dl/SHA256SUMS" "$IMG_URL/SHA256SUMS"
		(cd "$ROOT/dl" && grep " \*\?$IMG_FILE\$" SHA256SUMS | sha256sum -c --status) || die "image checksum mismatch"
	fi
	log "image: build ($stamp)"
	qemu-img convert -O raw "$dl" "$raw.tmp"
	qemu-img resize -q -f raw "$raw.tmp" 40G

	loop=$(losetup -f --show -P "$raw.tmp")
	# shellcheck disable=SC2064 # expand now: the trap must see this loop device
	trap "umount -R '$mnt' 2>/dev/null; losetup -d '$loop' 2>/dev/null; finish" EXIT
	growpart "$loop" 1 >/dev/null || true
	e2fsck -fy "${loop}p1" >/dev/null 2>&1 || true
	resize2fs -f "${loop}p1" >/dev/null 2>&1
	mount "${loop}p1" "$mnt"

	touch "$mnt/etc/cloud/cloud-init.disabled"
	install -d "$mnt/lib/modules"
	cp -a "/lib/modules/$KVER" "$mnt/lib/modules/"
	# The guest user has the spike user's uid so virtiofs ownership lines up.
	chroot "$mnt" groupadd -g "$SUID" agent
	chroot "$mnt" useradd -m -u "$SUID" -g "$SUID" -s /bin/bash agent
	# The guest boots the host kernel with no initrd from vda1 alone; the image's
	# /boot and /boot/efi partitions aren't needed and mustn't stall the boot.
	sed -i -E '/[[:space:]]\/boot(\/efi)?[[:space:]]/s/^/#/' "$mnt/etc/fstab"
	install -d "$mnt/etc/systemd/network"
	printf '[Match]\nName=e*\n\n[Network]\nDHCP=yes\n' >"$mnt/etc/systemd/network/10-spike.network"
	chroot "$mnt" systemctl enable -q systemd-networkd 2>/dev/null || true
	chroot "$mnt" systemctl mask -q systemd-networkd-wait-online.service apt-daily.timer apt-daily-upgrade.timer \
		unattended-upgrades.service motd-news.timer serial-getty@ttyS0.service \
		snapd.service snapd.socket snapd.seeded.service 2>/dev/null || true

	cp -a "$ROOT/stage/go" "$mnt/usr/local/go"
	install -m 0755 "$(command -v node)" "$mnt/usr/local/bin/node"
	install -d "$mnt/usr/local/lib/node_modules"
	cp -a "$(dirname "$(readlink -f "$(command -v npm)")")/.." "$mnt/usr/local/lib/node_modules/npm"
	ln -sf ../lib/node_modules/npm/bin/npm-cli.js "$mnt/usr/local/bin/npm"
	ln -sf ../lib/node_modules/npm/bin/npx-cli.js "$mnt/usr/local/bin/npx"
	cp -a "$ROOT/stage/waypoint" "$ROOT/stage/node" "$mnt/home/agent/"
	install -d "$mnt/home/agent/go/pkg"
	cp -a "$ROOT/stage/gomod" "$mnt/home/agent/go/pkg/mod"
	cp -a "$ROOT/stage/gobin" "$mnt/home/agent/go/bin"
	chroot "$mnt" chown -R agent:agent /home/agent

	install -m 0755 "$HERE/guest.sh" "$mnt/usr/local/sbin/spike-guest"
	install -m 0755 "$HERE/bench.sh" "$mnt/usr/local/bin/spike-bench"
	install -m 0755 "$HERE/probes.sh" "$mnt/usr/local/bin/spike-probes"
	cat >"$mnt/etc/systemd/system/spike-guest.service" <<'EOF'
[Unit]
Description=WP-417 spike job
After=local-fs.target systemd-networkd.service

[Service]
# exec, not oneshot, so the boot finishes and systemd-analyze can report it.
Type=exec
ExecStart=/usr/local/sbin/spike-guest
StandardOutput=journal

[Install]
WantedBy=multi-user.target
EOF
	chroot "$mnt" systemctl enable -q spike-guest.service

	umount -R "$mnt"
	e2fsck -fy "${loop}p1" >/dev/null 2>&1 || true
	losetup -d "$loop"
	trap finish EXIT
	install -m 0600 -o "$SU" -g "$SU" "/boot/vmlinuz-$KVER" "$ROOT/vm/vmlinuz"
	chown "$SU:$SU" "$raw.tmp"
	mv "$raw.tmp" "$raw"
	echo "$stamp" >"$raw.stamp"
	rec prepare image "$IMG_FILE $(du -h --apparent-size "$raw" | cut -f1) apparent, $(du -h "$raw" | cut -f1) used"
}

contained_props() {
	local base=$ROOT/contained
	printf '%s\n' --uid="$SU" --gid="$SU" --collect --wait --pipe --quiet \
		-p TemporaryFileSystem=/:ro -p MountAPIVFS=yes \
		-p BindReadOnlyPaths=/usr -p BindReadOnlyPaths=/usr/bin:/bin -p BindReadOnlyPaths=/usr/sbin:/sbin \
		-p BindReadOnlyPaths=/usr/lib:/lib -p BindReadOnlyPaths=/usr/lib64:/lib64 \
		-p BindReadOnlyPaths=/etc/ssl -p BindReadOnlyPaths=/etc/ca-certificates \
		-p BindReadOnlyPaths=/etc/ld.so.cache -p BindReadOnlyPaths=/etc/alternatives \
		-p "BindReadOnlyPaths=$base/etc/passwd:/etc/passwd" -p "BindReadOnlyPaths=$base/etc/group:/etc/group" \
		-p "BindReadOnlyPaths=$base/etc/hosts:/etc/hosts" -p "BindReadOnlyPaths=$base/etc/nsswitch.conf:/etc/nsswitch.conf" \
		-p "BindReadOnlyPaths=$ROOT/bare/go:/opt/go" -p "BindReadOnlyPaths=$HERE/probes.sh:/opt/spike/probes.sh" \
		-p "BindPaths=$1:/work" -p "BindPaths=$base/home:/home/sandbox" \
		-p "TemporaryFileSystem=/tmp:mode=1777,size=2G" \
		-p PrivateDevices=yes -p "TemporaryFileSystem=/dev/shm:mode=1777,size=256M" -p PrivateIPC=yes -p PrivateNetwork=yes \
		-p ProtectProc=invisible -p ProcSubset=pid \
		-p ProtectKernelTunables=yes -p ProtectKernelModules=yes -p ProtectKernelLogs=yes \
		-p ProtectControlGroups=yes -p ProtectClock=yes -p ProtectHostname=yes \
		-p RestrictNamespaces=yes -p RestrictSUIDSGID=yes -p NoNewPrivileges=yes -p LockPersonality=yes \
		-p CapabilityBoundingSet= -p "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6" \
		-p SystemCallFilter=@system-service -p SystemCallErrorNumber=EPERM -p UMask=0002 \
		-p "Slice=$SLICE" -p MemoryMax=4G -p CPUQuota=400% -p TasksMax=4096 \
		-E HOME=/home/sandbox -E PATH=/opt/go/bin:/home/sandbox/go/bin:/usr/local/bin:/usr/bin:/bin \
		-E GOTOOLCHAIN=local -E GOFLAGS=-mod=readonly -E "SPIKE_HOST_IP=${SPIKE_HOST_IP:-}"
}

t_contained() {
	need_root contained
	local base=$ROOT/contained props=() i t0 out
	log "contained: setup"
	install -d -m 0700 -o "$SU" -g "$SU" "$base" "$base/home"
	install -d -m 0755 "$base/etc"
	printf 'root:x:0:0::/nonexistent:/usr/sbin/nologin\n%s:x:%s:%s::/home/sandbox:/bin/bash\nnobody:x:65534:65534::/nonexistent:/usr/sbin/nologin\n' "$SU" "$SUID" "$SUID" >"$base/etc/passwd"
	printf 'root:x:0:\n%s:x:%s:\nnogroup:x:65534:\n' "$SU" "$SUID" >"$base/etc/group"
	printf '127.0.0.1 localhost\n::1 localhost\n' >"$base/etc/hosts"
	printf 'passwd: files\ngroup: files\nhosts: files\n' >"$base/etc/nsswitch.conf"
	if [ ! -d "$base/ws" ]; then
		cp -a "$ROOT/stage/waypoint" "$base/ws"
		install -d "$base/home/go/pkg"
		cp -a "$ROOT/stage/gomod" "$base/home/go/pkg/mod"
		cp -a "$ROOT/stage/gobin" "$base/home/go/bin"
		chown -R "$SU:$SU" "$base/ws" "$base/home"
	fi
	touch /dev/shm/wpspike-host-marker
	mapfile -t props < <(contained_props "$base/ws")

	for i in 1 2 3 4 5; do
		t0=$(now)
		if systemd-run "${props[@]}" -- /bin/true; then
			rec contained start_secs "$(since "$t0")" "run $i"
		else
			rec contained start_failed "rc=$?" "the allowlisted property set did not start"
			rm -f /dev/shm/wpspike-host-marker
			return 0
		fi
	done

	log "contained: probes"
	while read -r tag name verdict detail; do
		[ "$tag" = PROBE ] && rec contained "probe_$name" "$verdict" "$detail"
	done < <(systemd-run "${props[@]}" -- /bin/bash /opt/spike/probes.sh 2>&1 || true)
	rm -f /dev/shm/wpspike-host-marker

	log "contained: build and test inside the unit"
	t0=$(now)
	if out=$(systemd-run "${props[@]}" -- /bin/bash -c 'cd /work && go build -o /dev/null ./cmd/... ./internal/... && go test -count=1 ./internal/wql/... >/dev/null' 2>&1); then
		rec contained build_test_secs "$(since "$t0")" "offline: go build cmd+internal, go test internal/wql"
	else
		rec contained build_test_failed "$(printf '%s' "$out" | tail -c 300)"
	fi
	if out=$(systemd-run "${props[@]}" -- /bin/bash -c 'cd /work && git status --porcelain | wc -l && git log -1 --format=%h' 2>&1); then
		rec contained git_works "$(printf '%s' "$out" | tr '\n' ' ')"
	else
		rec contained git_failed "$(printf '%s' "$out" | tail -c 300)"
	fi
	if out=$(systemd-run "${props[@]}" -- /bin/bash -c 'mv /work /work2' 2>&1); then
		rec contained rename_workspace_mount ALLOWED "$out"
	else
		rec contained rename_workspace_mount REFUSED "$(printf '%s' "$out" | tail -c 200)"
	fi

	t_rename_ancestor
}

# A bind-mounted store pins its directory: renaming or replacing an ancestor of
# the store source afterwards must not change what a new unit sees (PL7 §5.2).
t_rename_ancestor() {
	local src=$ROOT/rename/host-store pin=/var/lib/waypoint-spike/store got props=()
	log "contained: rename-of-ancestor"
	umount "$pin" 2>/dev/null || true
	rm -rf "$ROOT/rename"
	install -d "$src/ws" "$pin"
	echo original >"$src/ws/marker"
	mount --bind "$src" "$pin"
	mapfile -t props < <(contained_props "$pin/ws")
	got=$(systemd-run "${props[@]}" -- /bin/cat /work/marker 2>&1 || true)
	rec rename_ancestor before "$got"
	mv "$src" "$ROOT/rename/host-store.moved"
	install -d "$src/ws"
	echo replaced >"$src/ws/marker"
	got=$(systemd-run "${props[@]}" -- /bin/cat /work/marker 2>&1 || true)
	rec rename_ancestor after_rename_and_replace "$got" "want original"
	rm -rf "$src"
	ln -s /etc "$src"
	got=$(systemd-run "${props[@]}" -- /bin/cat /work/marker 2>&1 || true)
	rec rename_ancestor after_symlink_swap "$got" "want original"
	umount "$pin"
	rm -f "$src"
	rm -rf "$ROOT/rename" /var/lib/waypoint-spike
}

CID=100
firmware_args() {
	if [ ! -e /usr/share/qemu/qboot.rom ] && [ -e /usr/share/seabios/bios-microvm.bin ]; then
		echo "-bios /usr/share/seabios/bios-microvm.bin"
	fi
}

vm_start() {
	local name=$1 mach=$2 job=$3 net=$4 d t0 qemu=() props=() dev
	shift 4
	d=$ROOT/vm/run-$name
	rm -rf "$d"
	install -d -m 0700 -o "$SU" -g "$SU" "$d"
	t0=$(now)
	cp --reflink=auto "$ROOT/vm/base.raw" "$d/disk.raw"
	rec "vm_$name" reflink_copy_secs "$(since "$t0")"
	chown "$SU:$SU" "$d/disk.raw"
	CID=$((CID + 1))

	dev=device
	[ "$mach" = q35 ] && dev=pci
	# firmware_args output is word-split unquoted, so it must print plain words with no spaces.
	# shellcheck disable=SC2207,SC2054
	qemu=(qemu-system-x86_64 -M "$mach" $(firmware_args) -enable-kvm -cpu host -smp "$VM_CPUS" -m "$VM_MEM"
		-nodefaults -no-user-config -display none -no-reboot
		-kernel "$ROOT/vm/vmlinuz"
		-append "console=ttyS0 root=/dev/vda1 rw quiet systemd.show_status=0 panic=-1 ${VM_APPEND:-}spike.job=$job"
		-serial "file:$d/serial.log"
		-sandbox on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny
		-drive "id=root,file=$d/disk.raw,format=raw,if=none,cache=none,aio=native"
		-device "virtio-blk-$dev,drive=root"
		-device "vhost-vsock-$dev,guest-cid=$CID"
		-device "virtio-balloon-$dev,free-page-reporting=on"
		-device "virtio-rng-$dev")
	if [ "$net" = user ]; then
		# shellcheck disable=SC2054 # QEMU option syntax
		qemu+=(-netdev user,id=n0 -device "virtio-net-$dev,netdev=n0")
	fi
	qemu+=("$@")

	props=(--unit="wpspike-$name" --collect --quiet --uid="$SU" --gid="$SU"
		-p SupplementaryGroups=kvm -p DevicePolicy=closed
		-p "DeviceAllow=/dev/kvm rw" -p "DeviceAllow=/dev/vhost-vsock rw"
		-p ProtectSystem=strict -p ProtectHome=yes -p "ReadWritePaths=$d" -p PrivateTmp=yes
		-p ProtectProc=invisible -p NoNewPrivileges=yes -p ProtectKernelTunables=yes
		-p ProtectKernelModules=yes -p ProtectControlGroups=yes -p RestrictNamespaces=yes
		-p LockPersonality=yes -p CapabilityBoundingSet= -p "Slice=$SLICE")
	if [ "$net" = user ]; then
		props+=(-p "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6")
	else
		props+=(-p PrivateNetwork=yes -p RestrictAddressFamilies=AF_UNIX)
	fi
	[ -n "${VM_EXTRA_PROPS:-}" ] && read -ra extra <<<"$VM_EXTRA_PROPS" && props+=("${extra[@]}")

	rec "vm_$name" qemu_argv "${qemu[*]}"
	t0=$(now)
	systemd-run "${props[@]}" -- "${qemu[@]}" || {
		rec "vm_$name" start_failed "systemd-run rc=$?"
		return 1
	}
	while ! grep -aq SPIKE_READY "$d/serial.log" 2>/dev/null; do
		if ! systemctl is-active -q "wpspike-$name"; then
			rec "vm_$name" start_failed "unit exited: $(journalctl -q -u "wpspike-$name" -n 5 --no-pager | tr '\n' ' ' | tail -c 400)"
			return 1
		fi
		if [ "$(awk -v a="$t0" -v b="$(now)" 'BEGIN{print (b-a>120)}')" = 1 ]; then
			rec "vm_$name" start_failed "no SPIKE_READY after 120 s; serial tail: $(tail -c 400 "$d/serial.log" 2>/dev/null | tr -d '\r' | tr '\n' ' ')"
			systemctl stop "wpspike-$name" || true
			return 1
		fi
		sleep 0.1
	done
	rec "vm_$name" cold_start_secs "$(since "$t0")" "systemd-run to SPIKE_READY"
	rec "vm_$name" guest_uptime_at_ready "$(grep -ao 'SPIKE_READY [0-9.]*' "$d/serial.log" | head -1 | cut -d' ' -f2)"
}

mem_sample() {
	local unit=wpspike-$1 pid cg
	pid=$(systemctl show -p MainPID --value "$unit" 2>/dev/null || echo 0)
	[ "${pid:-0}" != 0 ] || return 0
	cg=/sys/fs/cgroup$(systemctl show -p ControlGroup --value "$unit")
	rec "vm_$1" "mem_$2_qemu_pss_kb" "$(awk '/^Pss:/{print $2}' "/proc/$pid/smaps_rollup")"
	rec "vm_$1" "mem_$2_qemu_rss_kb" "$(awk '/^Rss:/{print $2}' "/proc/$pid/smaps_rollup")"
	[ -r "$cg/memory.current" ] && rec "vm_$1" "mem_$2_cgroup_kb" "$(($(cat "$cg/memory.current") / 1024))"
	return 0
}

SEEN=0
vm_relay() {
	local name=$1 line key val i lines=()
	mapfile -t lines < <(grep -a 'SPIKE_' "$ROOT/vm/run-$name/serial.log" 2>/dev/null)
	for ((i = SEEN; i < ${#lines[@]}; i++)); do
		line=${lines[i]//$'\r'/}
		line=${line#*SPIKE_}
		case "$line" in
		MARK\ *) mem_sample "$name" "${line#MARK }" ;;
		RESULT\ *)
			line=${line#RESULT }
			key=${line%% *}
			val=${line#"$key"}
			rec "vm_$name" "$key" "${val# }"
			;;
		LOG\ *) rec "vm_$name" log "${line#LOG }" ;;
		esac
	done
	SEEN=${#lines[@]}
}

vm_follow() {
	local name=$1 limit=$2 t0 peak=0 pss pid
	t0=$(now)
	SEEN=0
	while :; do
		vm_relay "$name"
		pid=$(systemctl show -p MainPID --value "wpspike-$name" 2>/dev/null) || pid=0
		if [ "${pid:-0}" != 0 ]; then
			pss=$(awk '/^Pss:/{print $2}' "/proc/$pid/smaps_rollup" 2>/dev/null) || pss=0
			[ "${pss:-0}" -gt "$peak" ] && peak=$pss
		fi
		systemctl is-active -q "wpspike-$name" || break
		if [ "$(awk -v a="$t0" -v b="$(now)" -v l="$limit" 'BEGIN{print (b-a>l)}')" = 1 ]; then
			rec "vm_$name" timeout "$limit s"
			systemctl stop "wpspike-$name" || true
			break
		fi
		sleep 2
	done
	vm_relay "$name" # lines written just before poweroff
	grep -aq SPIKE_DONE "$ROOT/vm/run-$name/serial.log" || rec "vm_$name" incomplete "no SPIKE_DONE"
	rec "vm_$name" mem_peak_qemu_pss_kb "$peak"
	rec "vm_$name" total_secs "$(since "$t0")"
}

t_vm_start() {
	need_root vm-start
	local i
	log "vm-start: microvm cold starts and idle memory (no NIC, hardened unit)"
	for i in 1 2 3; do
		vm_start "microvm-idle-$i" microvm idle none && vm_follow "microvm-idle-$i" 300
	done
}

# Without an RTC the guest kernel spends about 5 s failing to read the time, so rtc=on.
TUNED_MACHINE=${TUNED_MACHINE:-microvm,rtc=on}
TUNED_APPEND=${TUNED_APPEND:-initcall_debug systemd.mask=apparmor.service systemd.mask=snapd.apparmor.service systemd.mask=lxd-installer.socket systemd.mask=apport.service systemd.mask=pollinate.service }
t_vm_tuned() {
	need_root vm-tuned
	local i
	log "vm-tuned: microvm cold starts as $TUNED_MACHINE with: $TUNED_APPEND"
	rec vm_tuned settings "$TUNED_MACHINE" "$TUNED_APPEND"
	for i in 1 2 3; do
		VM_APPEND=$TUNED_APPEND vm_start "tuned-idle-$i" "$TUNED_MACHINE" idle none && vm_follow "tuned-idle-$i" 300
	done
}

t_vm_q35() {
	need_root vm-q35
	local i
	log "vm-q35: the same idle job on a minimal q35 guest"
	for i in 1 2 3; do
		vm_start "q35-idle-$i" q35 idle none && vm_follow "q35-idle-$i" 300
	done
	rec vm_q35 note "q35 with a shared DAX base image (virtio-pmem) is not measured by this harness"
}

t_vsock() {
	need_root vsock
	local lfile=$ROOT/vm/vsock-listener.out hpid t0
	log "vsock: identity across users"
	install -d -m 0700 -o "$SU" -g "$SU" "$ROOT/vm"
	: >"$lfile"
	setpriv --reuid=nobody --regid=nogroup --clear-groups python3 -c '
import socket, sys
s = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
s.bind((socket.VMADDR_CID_ANY, 5000)); s.listen(1); s.settimeout(90)
try:
    c, peer = s.accept(); print("accepted_from_cid_%d %s" % (peer[0], c.recv(64).decode().strip()))
except OSError as e:
    print("none %s" % e)
' >"$lfile" 2>&1 &
	hpid=$!
	vm_start vsock microvm vsock none || {
		kill "$hpid" 2>/dev/null
		return 0
	}
	t0=$(now)
	until grep -aq SPIKE_LISTENING "$ROOT/vm/run-vsock/serial.log" 2>/dev/null; do
		if [ "$(awk -v a="$t0" -v b="$(now)" 'BEGIN{print (b-a>60)}')" = 1 ]; then break; fi
		sleep 0.5
	done
	if setpriv --reuid=nobody --regid=nogroup --clear-groups python3 -c '
import socket, sys
s = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM); s.settimeout(5); s.connect((int(sys.argv[1]), 5001))
' "$CID" 2>/dev/null; then
		rec vsock host_nobody_to_guest connected "any host user can reach a guest port: the guest agent needs the per-boot secret (PL7 §6.2)"
	else
		rec vsock host_nobody_to_guest refused
	fi
	vm_follow vsock 120
	wait "$hpid" 2>/dev/null || true
	rec vsock host_listener_as_nobody "$(cat "$lfile")" "a guest reached a listener of an unrelated host user?"
}

t_virtiofs() {
	need_root virtiofs
	local d=$ROOT/vm/vfs share=$ROOT/vm/vfs/share
	log "virtiofs: virtiofsd --sandbox none as the spike user under AppArmor"
	rm -rf "$d"
	install -d -m 0700 -o "$SU" -g "$SU" "$d" "$share"
	cp -a "$ROOT/stage/waypoint" "$share/waypoint"
	chown -R "$SU:$SU" "$share"
	if ! systemd-run --unit=wpspike-virtiofsd --collect --quiet --uid="$SU" --gid="$SU" \
		-p ProtectSystem=strict -p ProtectHome=yes -p "ReadWritePaths=$d" -p NoNewPrivileges=yes \
		-p PrivateNetwork=yes -p RestrictAddressFamilies=AF_UNIX -p "Slice=$SLICE" \
		-- /usr/libexec/virtiofsd --socket-path="$d/vfs.sock" --shared-dir="$share" --sandbox none --cache auto; then
		rec virtiofs virtiofsd_start failed
		return 0
	fi
	sleep 2
	if ! systemctl is-active -q wpspike-virtiofsd; then
		rec virtiofs virtiofsd_start "exited: $(journalctl -q -u wpspike-virtiofsd -n 5 --no-pager | tr '\n' ' ' | tail -c 400)"
		return 0
	fi
	rec virtiofs virtiofsd_start ok "--sandbox none, unprivileged, AppArmor userns restriction on"
	VM_EXTRA_PROPS="-p ReadWritePaths=$d" vm_start virtiofs "microvm,memory-backend=mem" virtiofs none \
		-object "memory-backend-memfd,id=mem,size=${VM_MEM}M,share=on" \
		-chardev "socket,id=vfs,path=$d/vfs.sock" -device vhost-user-fs-device,chardev=vfs,tag=share &&
		vm_follow virtiofs 900
	systemctl stop wpspike-virtiofsd 2>/dev/null || true
}

# Each job gets its own /tmp: golangci-lint locks /tmp/golangci-lint.lock, and parallel jobs sharing one /tmp fail.
bare_job() {
	systemd-run --unit="wpspike-$1" --uid="$SU" --gid="$SU" --collect --wait --pipe --quiet \
		-p "Slice=$SLICE" -p "CPUQuota=$((VM_CPUS * 100))%" -p "MemoryMax=${VM_MEM}M" -p PrivateTmp=yes \
		-E "HOME=$2" -E "PATH=$ROOT/bare/go/bin:/usr/local/bin:/usr/bin:/bin" \
		-- /bin/bash -c "for w in waypoint node; do bash $ROOT/bare/bench.sh \$w warmup; bash $ROOT/bare/bench.sh \$w timed; done" 2>&1 || echo "SPIKE_RESULT bench_runner_rc $?"
}

# Alternating bare and VM rounds spreads any drift in host load across both sides.
BENCH_ROUNDS=${BENCH_ROUNDS:-2}
t_bench() {
	need_root bench
	local order=(bare:1 vm:1) side round
	[ "$BENCH_ROUNDS" -ge 2 ] && order=(bare:1 vm:1 vm:2 bare:2)
	record_load bench_start
	for side in "${order[@]}"; do
		round=${side#*:}
		case "$side" in
		bare:*)
			log "bench: bare round $round (as $SU, ${VM_CPUS} CPUs / ${VM_MEM} MB like the VM)"
			fresh_home "$ROOT/bare/home-$round"
			while read -r tag key val; do
				[ "$tag" = SPIKE_RESULT ] && rec "bare-$round" "$key" "$val"
			done < <(bare_job "bench-bare-$round" "$ROOT/bare/home-$round")
			;;
		vm:*)
			log "bench: microvm round $round"
			vm_start "bench-$round" microvm bench user && vm_follow "bench-$round" 7200
			;;
		esac
		record_load "bench_${side/:/}_done"
	done
}

LOAD_JOBS=${LOAD_JOBS:-3}
LOAD_IDLE_CT=${LOAD_IDLE_CT:-16}
LOAD_IDLE_VMS=${LOAD_IDLE_VMS:-4}
LOAD_IDLE_MEM=${LOAD_IDLE_MEM:-2048}

load_limits() {
	if [ "$1" = on ]; then
		systemctl start "$SLICE" # an implicit slice must be loaded to take properties
		systemctl set-property --runtime "$SLICE" MemoryHigh=20G MemoryMax=22G CPUWeight=50 CPUQuota=600% TasksMax=16384
		systemctl set-property --runtime system.slice MemoryLow=20G
	else
		systemctl set-property --runtime "$SLICE" MemoryHigh=infinity MemoryMax=infinity CPUWeight=100 CPUQuota= TasksMax=infinity || true
		systemctl set-property --runtime system.slice MemoryLow=0 || true
	fi
}

load_stop_units() {
	local u
	for u in $(systemctl list-units --plain --no-legend 'wpspike-load-*' | awk '{print $1}'); do
		systemctl stop "$u" || true
	done
}

load_watch() {
	local pass=$1 abort=$ROOT/load/abort
	while systemctl list-units --plain --no-legend --state=active 'wpspike-load-run-*' 'wpspike-load-bare-*' | grep -q .; do
		if [ -s "$abort" ]; then
			rec "load_$pass" aborted "$(cat "$abort")"
			log "load: ABORT ($(cat "$abort")); stopping the load units"
			load_stop_units
			return 1
		fi
		sleep 5
	done
}

load_pass() {
	local pass=$1 i h t0 pids=() ok=0
	log "load: $pass pass"
	: >"$ROOT/load/abort"
	python3 -I "$HERE/loadwatch.py" "$RES/load-$pass.jsonl" "$ROOT/load/abort" 2>>"$RES/spike.log" &
	local watch=$!
	sleep 30
	t0=$(now)
	rec "load_$pass" started "$(date -u +%FT%TZ)"
	if [ "$pass" = bare ]; then
		for i in $(seq 1 "$LOAD_JOBS"); do
			h=$ROOT/load/bare-$i
			(while read -r tag key val; do
				[ "$tag" = SPIKE_RESULT ] && rec "load_bare_job$i" "$key" "$val"
			done < <(bare_job "load-bare-$i" "$h")) &
			pids+=($!)
		done
		sleep 10
	else
		for i in $(seq 1 "$LOAD_IDLE_CT"); do
			install -d -m 0700 -o "$SU" -g "$SU" "$ROOT/load/ct-$i"
			mapfile -t props < <(contained_props "$ROOT/load/ct-$i" | grep -v -x -e --wait -e --pipe)
			systemd-run --unit="wpspike-load-ct-$i" "${props[@]}" -- /bin/sleep infinity ||
				rec load_sandboxed "ct_${i}_failed" "rc=$?"
		done
		for i in $(seq 1 "$LOAD_IDLE_VMS"); do
			VM_MEM=$LOAD_IDLE_MEM vm_start "load-idle-$i" microvm hold none || rec load_sandboxed "idle_vm_${i}_failed" ""
		done
		for i in $(seq 1 "$LOAD_JOBS"); do
			if vm_start "load-run-$i" microvm bench user; then
				vm_follow "load-run-$i" 7200 &
				pids+=($!)
			fi
		done
	fi
	load_watch "$pass" || ok=1
	wait "${pids[@]}" 2>/dev/null || true
	rec "load_$pass" wall_secs "$(since "$t0")"
	for i in $(seq 1 "$LOAD_IDLE_VMS"); do mem_sample "load-idle-$i" end; done
	load_stop_units
	sleep 30 # Let the load watch run 30 seconds past the work to record the recovery tail.
	kill "$watch" 2>/dev/null || true
	wait "$watch" 2>/dev/null || true
	return "$ok"
}

t_load() {
	need_root load
	local i
	[ -f "$ROOT/contained/etc/passwd" ] || die "load needs the contained step's files: run contained first"
	rm -rf "$ROOT/load"
	# 0711: the bare jobs run as the spike user and must reach their homes.
	install -d -m 0711 "$ROOT/load"
	for i in $(seq 1 "$LOAD_JOBS"); do fresh_home "$ROOT/load/bare-$i"; done
	rm -f "$RES/load-bare.jsonl" "$RES/load-sandboxed.jsonl"
	load_limits on
	# shellcheck disable=SC2064 # expand now
	trap "load_stop_units; load_limits off; finish" EXIT
	rec load limits "$(systemctl show "$SLICE" -p MemoryHigh -p MemoryMax -p CPUWeight -p CPUQuotaPerSecUSec | paste -sd ' ')" \
		"system.slice $(systemctl show system.slice -p MemoryLow)"
	if load_pass bare; then
		sleep 120
		load_pass sandboxed || true
	fi
	load_limits off
	trap finish EXIT
	python3 -I "$HERE/loadsum.py" "$RES" | while IFS=$'\t' read -r test key val note; do
		rec "$test" "$key" "$val" "$note"
	done
}

report() {
	python3 - "$RES/results.jsonl" <<'EOF'
import json, sys
rows = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
print("| test | key | value | note |\n|---|---|---|---|")
for r in rows:
    if r["key"] == "qemu_argv":
        continue
    print("| %s | %s | %s | %s |" % (r["test"], r["key"], str(r["value"]).replace("|", "/").replace("\n", " "), r["note"].replace("|", "/").replace("\n", " ")))
EOF
}

cleanup() {
	need_root cleanup
	local u
	log "cleanup"
	for u in $(systemctl list-units --plain --no-legend 'wpspike-*' | awk '{print $1}'); do
		systemctl stop "$u" || true
	done
	umount -R "$ROOT/mnt" 2>/dev/null || true
	umount /var/lib/waypoint-spike/store 2>/dev/null || true
	losetup -j "$ROOT/vm/base.raw.tmp" 2>/dev/null | cut -d: -f1 | xargs -r -n1 losetup -d
	rm -rf "${ROOT:?}/vm" "${ROOT:?}/bare" "${ROOT:?}/contained" "${ROOT:?}/stage" "${ROOT:?}/dl" "${ROOT:?}/mnt" "${ROOT:?}/rename" "${ROOT:?}/load" /var/lib/waypoint-spike
	rm -f /dev/shm/wpspike-host-marker
	if id "$SU" >/dev/null 2>&1; then
		pkill -u "$SU" || true
		userdel "$SU" || true
		getent group "$SU" >/dev/null && groupdel "$SU" || true
	fi
	log "cleanup: done (results kept in $RES; packages kept)"
}

# Runs as a transient unit so it outlives the shell: a Waypoint request's command is stopped after 10 minutes.
start() {
	need_root start
	[ $# -gt 0 ] || set -- all
	if systemctl is-active -q waypoint-spike-run; then
		die "already running: journalctl -fu waypoint-spike-run"
	fi
	install -d -m 0755 -o "$OWNER" "$RES"
	systemd-run --unit=waypoint-spike-run --collect --quiet \
		-E "SPIKE_OWNER=$OWNER" -E "SPIKE_ROOT=$ROOT" -E "VM_CPUS=$VM_CPUS" -E "VM_MEM=$VM_MEM" \
		-E "WAYPOINT_SRC=$WAYPOINT_SRC" -E "NODE_SRC=$NODE_SRC" -E "SPIKE_HOST_IP=${SPIKE_HOST_IP:-}" \
		-E "TUNED_MACHINE=$TUNED_MACHINE" -E "TUNED_APPEND=$TUNED_APPEND" -E "BENCH_ROUNDS=$BENCH_ROUNDS" \
		-E "LOAD_JOBS=$LOAD_JOBS" -E "LOAD_IDLE_CT=$LOAD_IDLE_CT" -E "LOAD_IDLE_VMS=$LOAD_IDLE_VMS" -E "LOAD_IDLE_MEM=$LOAD_IDLE_MEM" \
		-- "$HERE/spike.sh" "$@"
	echo "started waypoint-spike-run ($*): journalctl -fu waypoint-spike-run, log in $RES/spike.log"
}

main() {
	[ $# -gt 0 ] || {
		sed -n '2,25p' "$0"
		exit 2
	}
	if [ "$1" = report ]; then
		report
		return
	fi
	if [ "$1" = start ]; then
		start "${@:2}"
		return
	fi
	need_root "$@"
	install -d -m 0755 "$ROOT"
	install -d -m 0755 -o "$OWNER" "$RES"
	trap finish EXIT
	exec 9>"$ROOT/.lock"
	flock -n 9 || die "another spike.sh is running"
	local cmd
	if [ "$1" = all ]; then
		set -- prepare contained vm-start vm-tuned vm-q35 vsock virtiofs bench
		if [ -s "$RES/results.jsonl" ]; then
			mv "$RES/results.jsonl" "$RES/results-$(date +%Y%m%d-%H%M%S).jsonl"
		fi
	fi
	for cmd in "$@"; do
		case "$cmd" in
		prepare) prepare ;;
		contained) t_contained || log "step $cmd failed; continuing" ;;
		vm-start) t_vm_start || log "step $cmd failed; continuing" ;;
		vm-tuned) t_vm_tuned || log "step $cmd failed; continuing" ;;
		vm-q35) t_vm_q35 || log "step $cmd failed; continuing" ;;
		vsock) t_vsock || log "step $cmd failed; continuing" ;;
		virtiofs) t_virtiofs || log "step $cmd failed; continuing" ;;
		bench) t_bench || log "step $cmd failed; continuing" ;;
		load) t_load || log "step $cmd failed; continuing" ;;
		cleanup) cleanup ;;
		*) die "unknown step $cmd" ;;
		esac
	done
	log "finished: $*; results in $RES/results.jsonl (deploy/sandbox-spike/spike.sh report)"
}

main "$@"
