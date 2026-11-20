#!/usr/bin/env python3
"""loadwatch.py: the load test's watcher (WP-417, PL7 §6.7 "What Phase 0 must show").

    loadwatch.py OUT.jsonl ABORT_FILE

Every 5 s it appends one JSON line to OUT.jsonl with:
  - response times of the services: another web app on the host (its health API, and its home
    page every third sample, which reads the database), Waypoint's /healthz,
    and a connection handshake with the host postgres (an SSLRequest, so no
    credentials are involved);
  - CPU and memory pressure (some, avg10) of system.slice and user.slice;
  - MemAvailable, and the sandbox slice's memory;
  - the oom_kill counters of system.slice, user.slice and waypoint.slice.

It stops when killed. If a service stays slow (over 5 s for three samples in a
row) or MemAvailable falls under 3 GB, it writes ABORT_FILE once, with the
reason; the driver (spike.sh load) then stops every load unit.
"""
import json
import socket
import sys
import time
import urllib.request

OUT, ABORT = sys.argv[1], sys.argv[2]
CG = "/sys/fs/cgroup"
SANDBOX_SLICE = CG + "/waypoint.slice/waypoint-spike.slice"
HTTP = {
    "wts_health": "http://127.0.0.1:21330/api/health",
    "waypoint_healthz": "http://127.0.0.1:21200/healthz",
}
WTS_HOME = "http://127.0.0.1:21330/"
SLOW_SECS, SLOW_RUN, MIN_AVAIL_KB = 5.0, 3, 3 * 1024 * 1024


def http_secs(url, timeout=15):
    t0 = time.monotonic()
    try:
        with urllib.request.urlopen(url, timeout=timeout) as r:
            r.read()
            ok = 200 <= r.status < 400
    except Exception:
        ok = False
    return round(time.monotonic() - t0, 4), ok


def pg_secs(timeout=10):
    t0 = time.monotonic()
    try:
        with socket.create_connection(("127.0.0.1", 5432), timeout=timeout) as s:
            s.sendall(b"\x00\x00\x00\x08\x04\xd2\x16\x2f")  # SSLRequest
            ok = s.recv(1) in (b"S", b"N")
    except Exception:
        ok = False
    return round(time.monotonic() - t0, 4), ok


def psi(path):
    try:
        for line in open(path):
            if line.startswith("some"):
                return float(line.split()[1].split("=")[1])
    except OSError:
        return None


def read_int(path):
    try:
        return int(open(path).read().strip())
    except (OSError, ValueError):
        return None


def oom_kills(cg):
    try:
        for line in open(cg + "/memory.events"):
            parts = line.split()
            if len(parts) == 2 and parts[0] == "oom_kill":
                return int(parts[1])
    except (OSError, ValueError):
        return None


def mem_available_kb():
    for line in open("/proc/meminfo"):
        if line.startswith("MemAvailable:"):
            return int(line.split()[1])


slow = {k: 0 for k in list(HTTP) + ["pg"]}
aborted = False
n = 0
with open(OUT, "a", buffering=1) as out:
    while True:
        t = time.time()
        row = {"t": round(t, 1)}
        for k, url in HTTP.items():
            row[k], row[k + "_ok"] = http_secs(url)
        if n % 3 == 0:
            row["wts_home"], row["wts_home_ok"] = http_secs(WTS_HOME, timeout=30)
        row["pg"], row["pg_ok"] = pg_secs()
        for s in ("system.slice", "user.slice"):
            row[s + "_cpu_psi"] = psi(f"{CG}/{s}/cpu.pressure")
            row[s + "_mem_psi"] = psi(f"{CG}/{s}/memory.pressure")
        row["mem_available_kb"] = mem_available_kb()
        cur = read_int(SANDBOX_SLICE + "/memory.current")
        row["sandbox_mem_kb"] = cur // 1024 if cur is not None else None
        for s in ("system.slice", "user.slice", "waypoint.slice"):
            row[s + "_oom_kill"] = oom_kills(f"{CG}/{s}")
        out.write(json.dumps(row) + "\n")

        reason = None
        for k in slow:
            slow[k] = slow[k] + 1 if (row[k] > SLOW_SECS or not row[k + "_ok"]) else 0
            if slow[k] >= SLOW_RUN:
                reason = f"{k} slow or failing for {SLOW_RUN} samples (last {row[k]} s)"
        if row["mem_available_kb"] is not None and row["mem_available_kb"] < MIN_AVAIL_KB:
            reason = f"MemAvailable {row['mem_available_kb']} kB"
        if reason and not aborted:
            with open(ABORT, "w") as f:
                f.write(reason + "\n")
            aborted = True
        n += 1
        time.sleep(max(0.0, 5 - (time.time() - t)))
