# Sandbox Phase 0 spike

These scripts take the measurements that PL7 §14 phase 0 needs before any sandbox code is built (WP-287, WP-417, decisions WP-ADR28). They are measurement only. Nothing in Waypoint calls them, and they change nothing Waypoint, its runner or any service uses.

| File | Runs | Does |
|---|---|---|
| `spike.sh` | on the host, as root | prepares an image and a throwaway user, runs each test, records results |
| `guest.sh` | inside a VM at boot, as root | the guest side of each VM test, reporting over the serial console |
| `bench.sh` | bare on the host and inside the VM | the same build workload in both places: `make check` here, `npm ci && npm test` in a Node project |
| `probes.sh` | as the sandbox user | the first cut of the escape suite (PL7 §13); every probe should print `REFUSED` |
| `loadwatch.py` | on the host, as root, during `load` | samples the services' response times, pressure, memory and OOM kills every 5 s, and stops the test if a service stays slow |
| `loadsum.py` | after `load` | sums up both passes and gives the verdict against PL7 §6.7's pass criteria |

## Running it

A person runs it, at a quiet hour, because it installs packages (`qemu-system-x86 qemu-utils virtiofsd cloud-guest-utils`), creates a user and uses KVM:

```sh
sudo deploy/sandbox-spike/spike.sh start        # runs "all" in the background unit waypoint-spike-run
journalctl -fu waypoint-spike-run               # follow it: 1-3 hours, mostly the bench
deploy/sandbox-spike/spike.sh report            # the results as a Markdown table
sudo deploy/sandbox-spike/spike.sh cleanup      # remove the user, units and images
```

`sudo deploy/sandbox-spike/spike.sh all` does the same in the foreground. Use `start` from a Waypoint request, because a request's command is stopped after 10 minutes. Each full run moves the previous `results.jsonl` aside to `results-<time>.jsonl`. Running it again reuses the packages, user, clones and Go staging. The VM image is rebuilt only when `guest.sh`, `bench.sh`, `probes.sh` or the recipe change.

You can also run single steps after `prepare`, in this order: `contained`, `vm-start`, `vm-tuned`, `vm-q35`, `vsock`, `virtiofs`, `bench` and `load` (`all` runs every one but `load`, which needs `contained`'s files). `sudo spike.sh start prepare load` runs a step in the background the same way. A step that fails records the failure and the run carries on. A failure is a finding, not something to fix by hand.

Settings come from the environment: `SPIKE_ROOT` (default `/srv/waypoint-spike`), `VM_CPUS` (4), `VM_MEM` (4096 MB), `WAYPOINT_SRC` (this checkout), `NODE_SRC` (a Node project, required) and `SPIKE_HOST_IP` (a LAN address of the host, for one extra probe). `BENCH_ROUNDS` (2) alternates bare and VM as bare, VM, VM, bare. `TUNED_MACHINE` and `TUNED_APPEND` set `vm-tuned`'s machine and kernel options. `LOAD_JOBS` (3), `LOAD_IDLE_CT` (16), `LOAD_IDLE_VMS` (4) and `LOAD_IDLE_MEM` (2048 MB) size the load test.

## What it creates

- User and group `wpspike`, uid 39990. That is outside the plan's repo-user range (40000–49999). The account is nologin and locked, and is in no groups.
- Harness files live under `$SPIKE_ROOT`: the Ubuntu 24.04 minimal cloud image (checked against `SHA256SUMS`), staged clones of both repos at `origin/main`, a Go module cache, the images and the workspaces. Outside it are `/var/lib/waypoint-spike` (the rename test's pin) and `/dev/shm/wpspike-host-marker`; `cleanup` removes both.
- Transient units `wpspike-*.service` in `waypoint-spike.slice`.

The workload clones are fetched as the repos' owner (`$SUDO_USER`), so root never runs git in a gitdir it doesn't own.

`cleanup` removes all of this except `$SPIKE_ROOT/results`. The packages stay installed.

## What each test answers

| Step | Question (PL7) | Target |
|---|---|---|
| `contained` | Does the allowlisted unit (§6.3) start, how fast, and do the escape probes all fail? Can an offline build and git work inside it? Does a store bind mount survive the source directory being renamed, replaced or swapped for a symlink (§5.2)? | every probe refused; mount pinned |
| `vm-start` | Cold start of a QEMU microvm as an unprivileged user in a hardened unit (no NIC, `-sandbox on`), and its memory after 60 s idle and after dropping caches | ≤10 s |
| `vm-tuned` | The microvm again with an RTC (`-M microvm,rtc=on`), the guest's AppArmor, snap, apport and pollinate units masked, and `initcall_debug`, so the boot breakdown names what's left | ≤10 s |
| `vm-q35` | The same on a minimal q35 guest, for comparison | |
| `vsock` | Can a guest reach a vsock listener of an unrelated host user? Can any host user reach a guest port? (This is why the guest agent needs a per-boot secret, §6.2.) | recorded |
| `virtiofs` | Does unprivileged `virtiofsd --sandbox none` run under Ubuntu's AppArmor userns restriction? How does a build on virtiofs compare with the guest's own disk? | recorded |
| `bench` | `make check` (timed per target: test, lint, sast, vuln) and a Node project's `npm ci && npm test`, bare (as `wpspike`, capped to the VM's CPUs and memory, with its own /tmp) against the same in a microvm, alternating, plus RAM after the builds. Each bare round starts from a fresh home (the staged sources and module cache, no build caches), as each VM starts from a fresh disk: a kept home's warm Go build cache once made the VM look twice as slow | ≤1.3× bare |
| `load` | PL7 §6.7's load test: 3 bench jobs bare, then 3 runner VMs with 16 idle Contained sessions and 4 idle 2 GB VMs, under the plan's slice limits (set at runtime and undone after). The services are watched throughout | no OOM kills; services' pressure and p99 no worse than bare; builds ≤1.3×. A pass whose builds failed is reported `invalid`, since its times compare nothing |

Host load (loadavg, CPU and memory PSI, MemAvailable) is recorded around the bench, because a dev host is rarely idle. Compare the bench numbers in that light.

## Caveats

- The bench VM uses QEMU user-mode networking so apt, npm and govulncheck work. From that VM the host's loopback is reachable at 10.0.2.2. It runs only these scripts, and it is not how a sandbox will reach the network (that is the broker, §7). Every other VM has no NIC.
- The guest boots the host's kernel with no initrd. The VM level will ship its own kernel and image (Phase 3). This spike measures the hypervisor and the walls, not the image.
- The load test stops itself (every load unit, then the limits are undone) if the other web app on the host, Waypoint or postgres stays over 5 s for three samples, or MemAvailable falls under 3 GB. The reason goes in the results.
- Each guest reports where its boot time goes: `guest_kernel_gaps` (pauses of 0.2 s or more between kernel messages), `guest_slow_initcalls` (with `initcall_debug`) and `guest_critical_chain`.

## Results

Results go in `$SPIKE_ROOT/results/results.jsonl`, one JSON object per line (`test`, `key`, `value`, `note`), plus `spike.log`. They are owned by the person who ran sudo. The analysis goes on the WP-417 experiment page.
