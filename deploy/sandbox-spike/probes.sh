#!/usr/bin/env bash
# probes.sh: escape probes for the WP-287 Phase 0 spike (WP-417), a first cut of
# the escape suite in PL7 §13. Runs as the unprivileged sandbox user, inside a
# Contained unit or a VM guest. Every probe should print REFUSED.
#
# Output: "PROBE name REFUSED|ALLOWED detail" lines.
#   SPIKE_HOST_IP  an address of the host on its LAN (optional)
set -u

if [ "$(id -u)" = 0 ]; then
	echo "probes.sh must run as the unprivileged sandbox user, not root" >&2
	exit 2
fi

probe() {
	local name=$1
	shift
	local out
	# Through bash so the helper functions below work under timeout.
	local rc=0 verdict
	out=$(timeout 5 bash -c '"$@"' probe "$@" 2>&1) || rc=$?
	case "$rc" in
	0) verdict=ALLOWED ;;
	124 | 125 | 126 | 127) verdict=INCONCLUSIVE ;;
	*) verdict=REFUSED ;;
	esac
	echo "PROBE $name $verdict $(printf '%s' "$out" | head -c 120 | tr '\n' ' ')"
}

unix_connect() { python3 -c 'import socket,sys; s=socket.socket(socket.AF_UNIX); s.connect(sys.argv[1])' "$1"; }
tcp_connect() { python3 -c 'import socket,sys; socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=3)' "$1" "$2"; }
other_uid_procs() {
	local me n=0 d
	me=$(id -u)
	for d in /proc/[0-9]*; do
		[ "$(stat -c %u "$d" 2>/dev/null)" = "$me" ] || n=$((n + 1))
	done
	echo "$n other-uid pids visible"
	[ "$n" -gt 0 ]
}
host_shm_visible() { [ -e /dev/shm/wpspike-host-marker ]; }
export -f unix_connect tcp_connect other_uid_procs host_shm_visible

probe read_claude_ssh ls /home/claude/.ssh
probe read_shadow cat /etc/shadow
probe list_projects ls /projects
probe read_appdata ls /mnt/AppData
probe list_store ls /mnt/store
probe other_uid_procs other_uid_procs
probe host_dev_shm host_shm_visible
probe write_usr touch /usr/wpspike-probe
probe dbus_socket unix_connect /run/dbus/system_bus_socket
probe snapd_socket unix_connect /run/snapd.socket
probe rpcbind_socket unix_connect /run/rpcbind.sock
probe docker_socket unix_connect /var/run/docker.sock
probe tcp_waypoint tcp_connect 127.0.0.1 28690
probe tcp_k3s_api tcp_connect 127.0.0.1 6443
if [ -n "${SPIKE_HOST_IP:-}" ]; then
	probe tcp_host_lan_ssh tcp_connect "$SPIKE_HOST_IP" 22
fi
probe fetch_example_com python3 -c 'import urllib.request; urllib.request.urlopen("http://example.com", timeout=4)'
probe user_namespace unshare -Ur true
probe mount_tmpfs mount -t tmpfs none /tmp
probe af_vsock python3 -c 'import socket; socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)'
probe af_netlink python3 -c 'import socket; socket.socket(socket.AF_NETLINK, socket.SOCK_RAW, 0)'
