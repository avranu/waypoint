#!/usr/bin/env bash
# Lines starting SPIKE_ on the serial console are the protocol that spike.sh on the host parses.
set -u

tty=/dev/ttyS0
out() { printf '%s\n' "$*" >"$tty"; }
param() { tr ' ' '\n' </proc/cmdline | sed -n "s/^spike\.$1=//p"; }
mark() { # Pause so the host can sample QEMU memory before the guest goes on.
	out "SPIKE_MARK $1"
	sleep 8
}
as_agent() { runuser -u agent -- env HOME=/home/agent PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin "$@"; }
drop_caches() {
	sync
	echo 3 >/proc/sys/vm/drop_caches
	sleep 20
}
wait_net() {
	local _
	for _ in $(seq 1 60); do
		if getent hosts archive.ubuntu.com >/dev/null 2>&1; then return 0; fi
		sleep 1
	done
	out "SPIKE_RESULT guest_network_ok 0"
	return 1
}

job=$(param job)
out "SPIKE_READY $(cut -d' ' -f1 /proc/uptime)"
out "SPIKE_RESULT guest_kernel $(uname -r)"
out "SPIKE_RESULT guest_mem_total_kb $(awk '/MemTotal/{print $2}' /proc/meminfo)"
modprobe vmw_vsock_virtio_transport 2>/dev/null || true

case "$job" in
idle)
	sleep 60
	out "SPIKE_RESULT guest_boot $(systemd-analyze time 2>&1 | head -1 | tr -s ' ')"
	out "SPIKE_RESULT guest_blame $(systemd-analyze blame 2>/dev/null | head -6 | tr -s ' ' | paste -sd ';')"
	out "SPIKE_RESULT guest_first_kmsg_to_init $(dmesg 2>/dev/null | grep -m1 'Run /sbin/init' | cut -d']' -f1 | tr -d '[ ')"
	out "SPIKE_RESULT guest_kernel_gaps $(dmesg 2>/dev/null | awk '{
		t = $0; sub(/^\[ */, "", t); sub(/\].*/, "", t); t += 0
		m = $0; sub(/^\[[^]]*\] /, "", m)
		if (NR > 1 && t - p >= 0.2) printf "%.2fs>%s; ", t - p, substr(m, 1, 70)
		p = t
	}' | head -c 900)"
	# Needs initcall_debug on the kernel command line.
	out "SPIKE_RESULT guest_slow_initcalls $(dmesg 2>/dev/null | awk '/initcall .* returned .* after [0-9]+ usecs/ {
		for (i = 1; i <= NF; i++) if ($i == "after") u = $(i + 1)
		for (i = 1; i <= NF; i++) if ($i == "initcall") f = $(i + 1)
		if (u + 0 >= 100000) printf "%s %.2fs; ", f, u / 1e6
	}' | head -c 600)"
	out "SPIKE_RESULT guest_critical_chain $(systemd-analyze critical-chain spike-guest.service --no-pager 2>/dev/null | tail -n +3 | tr -s ' ' | paste -sd ';' | head -c 600)"
	out "SPIKE_RESULT guest_balloon driver=$(ls -d /sys/bus/virtio/drivers/virtio_balloon/virtio* >/dev/null 2>&1 && echo bound || echo none) free_page_reporting=$(cat /sys/module/page_reporting/parameters/page_reporting_order 2>/dev/null || echo n/a)"
	out "SPIKE_RESULT guest_mem_used_kb $(awk '/MemTotal/{t=$2} /MemAvailable/{a=$2} END{print t-a}' /proc/meminfo)"
	mark idle_60s
	drop_caches
	mark idle_dropped
	as_agent bash /usr/local/bin/spike-probes | sed 's/^PROBE \([^ ]*\) /SPIKE_RESULT guest_probe_\1 /' >"$tty"
	;;

hold)
	# Drops its cache as the guest agent will after 5 idle minutes, so the idle state matches the real one.
	sleep 60
	drop_caches
	mark held
	sleep infinity
	;;

bench)
	if wait_net; then
		start=$(date +%s)
		DEBIAN_FRONTEND=noninteractive apt-get -qq update >/dev/null 2>&1
		DEBIAN_FRONTEND=noninteractive apt-get -qq install -y --no-install-recommends \
			build-essential make git ca-certificates >/dev/null 2>&1
		out "SPIKE_RESULT guest_setup_rc $?"
		out "SPIKE_RESULT guest_setup_secs $(($(date +%s) - start))"
		mark before_build
		as_agent bash /usr/local/bin/spike-bench waypoint warmup >"$tty"
		as_agent bash /usr/local/bin/spike-bench waypoint timed >"$tty"
		mark after_waypoint
		as_agent bash /usr/local/bin/spike-bench node warmup >"$tty"
		as_agent bash /usr/local/bin/spike-bench node timed >"$tty"
		mark after_node
		drop_caches
		mark after_build_dropped
	fi
	;;

vsock)
	if as_agent python3 -c '
import socket
s = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
s.settimeout(5)
s.connect((2, 5000))
s.sendall(b"from-guest-user\n")
' 2>/dev/null; then
		out "SPIKE_RESULT vsock_guest_user_to_host connected"
	else
		out "SPIKE_RESULT vsock_guest_user_to_host refused"
	fi
	python3 -c '
import socket
s = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
s.bind((socket.VMADDR_CID_ANY, 5001))
s.listen(1)
print("SPIKE_LISTENING", flush=True)
s.settimeout(40)
try:
    c, peer = s.accept()
    print("SPIKE_RESULT vsock_host_to_guest accepted_from_cid_%d" % peer[0])
except OSError:
    print("SPIKE_RESULT vsock_host_to_guest none")
' >"$tty"
	;;

virtiofs)
	mkdir -p /mnt/share
	modprobe virtiofs 2>/dev/null || true
	if mount -t virtiofs share /mnt/share; then
		out "SPIKE_RESULT virtiofs_mount ok"
		for where in /mnt/share/waypoint /home/agent/waypoint; do
			label=$([ "$where" = /home/agent/waypoint ] && echo disk || echo virtiofs)
			start=$(date +%s.%N)
			# Each build gets its own empty GOCACHE, so only where the sources live differs between builds.
			as_agent bash -c "rm -rf ~/.cache/go-build-$label && cd $where && find . -type f | wc -l >/dev/null && GOCACHE=~/.cache/go-build-$label GOTOOLCHAIN=local GOFLAGS=-mod=readonly go build -o /dev/null ./cmd/... ./internal/..." >/dev/null 2>&1
			rc=$?
			out "SPIKE_RESULT virtiofs_build_${label}_rc $rc"
			out "SPIKE_RESULT virtiofs_build_${label}_secs $(awk -v a="$start" -v b="$(date +%s.%N)" 'BEGIN{printf "%.1f", b-a}')"
		done
	else
		out "SPIKE_RESULT virtiofs_mount failed"
	fi
	;;

*)
	out "SPIKE_RESULT guest_unknown_job $job"
	;;
esac

out "SPIKE_DONE"
systemctl poweroff --no-block || poweroff -f
