#!/usr/bin/env python3
"""loadsum.py RESULTS_DIR: sums up the load test (WP-417, PL7 §6.7).

Reads load-bare.jsonl and load-sandboxed.jsonl (loadwatch.py's samples) and the
load jobs' build times from results.jsonl, and prints tab-separated
"test key value note" lines, which spike.sh records. The pass criteria:
  - no OOM kills during the sandboxed pass;
  - services' pressure and p99 latency no worse than in the bare pass;
  - builds at most 1.3x as long.
"No worse" allows the noise of a busy host: 10% plus 1 point of PSI, and
10% plus 50 ms of latency.
"""
import json
import os
import sys

res = sys.argv[1]
LAT = ("wts_health", "wts_home", "waypoint_healthz", "pg")
PSI = ("system.slice_cpu_psi", "system.slice_mem_psi", "user.slice_cpu_psi")
OOM = ("system.slice_oom_kill", "user.slice_oom_kill", "waypoint.slice_oom_kill")


def pct(xs, p):
    xs = sorted(x for x in xs if x is not None)
    if not xs:
        return None
    return xs[min(len(xs) - 1, int(round(p / 100 * (len(xs) - 1))))]


def mean(xs):
    xs = [x for x in xs if x is not None]
    return sum(xs) / len(xs) if xs else None


def out(test, key, val, note=""):
    if isinstance(val, float):
        val = round(val, 4)
    print(f"{test}\t{key}\t{val}\t{note}")


summary = {}
for p in ("bare", "sandboxed"):
    path = os.path.join(res, f"load-{p}.jsonl")
    if not os.path.exists(path):
        continue
    rows = [json.loads(line) for line in open(path) if line.strip()]
    if not rows:
        continue
    s = {"samples": len(rows)}
    for k in LAT:
        vals = [r[k] for r in rows if k in r]
        s[k + "_p50"] = pct(vals, 50)
        s[k + "_p99"] = pct(vals, 99)
        s[k + "_fail"] = sum(1 for r in rows if k + "_ok" in r and not r[k + "_ok"])
    for k in PSI:
        vals = [r.get(k) for r in rows]
        s[k + "_mean"] = mean(vals)
        s[k + "_p90"] = pct(vals, 90)
    for k in OOM:
        vals = [r.get(k) for r in rows if r.get(k) is not None]
        s[k + "_delta"] = (vals[-1] - vals[0]) if vals else None
    avail = [r["mem_available_kb"] for r in rows if r.get("mem_available_kb") is not None]
    s["mem_available_min_kb"] = min(avail) if avail else None
    s["sandbox_mem_max_kb"] = max((r.get("sandbox_mem_kb") or 0) for r in rows)
    summary[p] = s
    for k, v in s.items():
        out(f"load_{p}", k, v)

# Build times: the load jobs' timed runs, bare against sandboxed.
# Only this run's rows: results.jsonl keeps earlier runs, and each load run
# starts by recording "load limits".
rows = [json.loads(line) for line in open(os.path.join(res, "results.jsonl")) if line.strip()]
starts = [i for i, r in enumerate(rows) if r["test"] == "load" and r["key"] == "limits"]
rows = rows[starts[-1]:] if starts else rows
builds = {"bare": {}, "sandboxed": {}}
failed = []  # load jobs whose build failed: their times compare nothing
for r in rows:
    if r["key"].endswith("_rc") and r["test"].startswith(("load_bare_job", "vm_load-run-")) and str(r["value"]).strip() != "0":
        failed.append(f"{r['test']} {r['key']}={r['value']}")
    if "_timed" not in r["key"] or not r["key"].endswith("_secs"):
        continue  # the timed runs, whole and per make target
    side = "bare" if r["test"].startswith("load_bare_job") else "sandboxed" if r["test"].startswith("vm_load-run-") else None
    if side:
        builds[side].setdefault(r["key"], []).append(float(r["value"]))
for key in sorted(set(builds["bare"]) | set(builds["sandboxed"])):
    b, v = mean(builds["bare"].get(key, [])), mean(builds["sandboxed"].get(key, []))
    out("load_builds", key, f"bare {b} sandboxed {v}", f"ratio {v / b:.2f}" if b and v else "")

if "bare" in summary and "sandboxed" in summary:
    b, v = summary["bare"], summary["sandboxed"]
    fails = []
    ooms = sum(v[k + "_delta"] or 0 for k in OOM)
    if ooms:
        fails.append(f"{ooms} OOM kills")
    for k in ("system.slice_cpu_psi", "system.slice_mem_psi"):
        if b[k + "_mean"] is not None and v[k + "_mean"] is not None and v[k + "_mean"] > b[k + "_mean"] * 1.1 + 1:
            fails.append(f"{k} mean {v[k + '_mean']:.1f} vs bare {b[k + '_mean']:.1f}")
    for k in LAT:
        if b[k + "_p99"] is not None and v[k + "_p99"] is not None and v[k + "_p99"] > b[k + "_p99"] * 1.1 + 0.05:
            fails.append(f"{k} p99 {v[k + '_p99']:.3f}s vs bare {b[k + '_p99']:.3f}s")
    for key in ("bench_waypoint_timed_secs", "bench_node_timed_secs"):
        bb, vv = mean(builds["bare"].get(key, [])), mean(builds["sandboxed"].get(key, []))
        if bb and vv and vv > 1.3 * bb:
            fails.append(f"{key} {vv / bb:.2f}x bare")
    if failed:
        out("load", "verdict", "invalid", "failed builds: " + "; ".join(failed[:6]))
    else:
        out("load", "verdict", "pass" if not fails else "fail", "; ".join(fails))
