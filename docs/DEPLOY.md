# Deploying and running Waypoint

## Configuration

All configuration is environment variables.

| Variable | Default | Meaning |
|---|---|---|
| `WAYPOINT_ADDR` | `:8080` | listen address |
| `WAYPOINT_DB` | `data/waypoint.db` | SQLite file (directory is created) |
| `WAYPOINT_BASE_URL` | `http://localhost:8080` | public URL, no trailing slash; used in links (invites, notifications, calendar, webhooks). `https://` also makes cookies Secure |
| `WAYPOINT_TRUST_PROXY` | off | which reverse proxies to believe: `1` trusts loopback and private addresses (10/8, 172.16/12, 192.168/16, fc00::/7), or give a comma list of addresses and CIDRs (`10.0.0.1, 2001:db8::/64`). Only a request arriving *from* a trusted proxy has its `X-Forwarded-For` read, from the right, skipping trusted hops (anything further left is client-supplied), and its `X-Forwarded-Proto: https` marks cookies Secure. Anyone else's headers are ignored, so the port being reachable directly from the internet can't be used to forge an address and dodge the login rate limit |
| `WAYPOINT_BACKUP_DIR` | `backups/` next to the database (Docker image: `/data/backups`) | a consistent copy of the DB is written there daily (mode 0600); `off` disables backups |
| `WAYPOINT_BACKUP_KEEP` | `14` | how many daily backups to keep |
| `WAYPOINT_MAX_ATTACHMENT_MB` | `25` | the size limit of one attached file (issues, pages, design notes, comments, requests, sessions); 1 to 1024. A reverse proxy needs a body limit at least this large (nginx: `client_max_body_size`) |
| `WAYPOINT_METRICS_TOKEN` | none | if set, `/metrics` requires `Authorization: Bearer <token>`. Without it, `/metrics` only answers direct (not proxied) requests from loopback or private addresses |
| `WAYPOINT_EGRESS_BLOCK_PRIVATE` | off | webhooks, Gotify and automation calls never reach loopback or link-local (cloud metadata) addresses and never follow redirects; `1` also blocks private networks (LAN, Tailscale). Off by default because self-hosted Gotify and Kestra usually live on the LAN |
| `WAYPOINT_AI_OPENCODE_URL` | none (compose: `http://opencode:4096`) | an `opencode serve` URL to set up as the default AI provider at startup, if none is configured yet |
| `WAYPOINT_AI_OPENCODE_PASSWORD` | none | that server's `OPENCODE_SERVER_PASSWORD` |
| `WAYPOINT_AI_MEANING_URL` | none (compose: `http://meaning:8080`) | a meaning server (the compose `meaning` service, or any server with the same API: `/v1/embeddings` and `/v1/nli`) that judges how goals and tasks relate in meaning. Empty turns it off |
| `LLM_GATEWAY_URL` | none | an LLM gateway's OpenAI-compatible API (LiteLLM, OpenRouter, a self-hosted proxy, OpenLLMs, …); its root also works (`/v1` is added). With `LLM_GATEWAY_API_KEY`, startup adds it as the *LLM gateway* provider, first for every AI task, and keeps its URL, key and extensions in step on later starts (a start with only the key keeps the URL it has) |
| `LLM_GATEWAY_API_KEY` | none | Waypoint's key for the gateway. Without it, no gateway is set up |
| `LLM_GATEWAY_MODEL` | none | the model every AI task asks the gateway for when it has no `routes` extension: needed by a first setup without it, and by a start that drops it if none was named before; unset keeps the one named before |
| `LLM_GATEWAY_EXTENSIONS` | none | what the gateway takes beyond the standard OpenAI API, comma-separated, or `all` or `none` alone: `routes` (it answers the routes `review`, `review-verify`, `prep` and `free`, which then lead each task's chain), `fallbacks` (a request's `fallbacks` list), `routing` (`routing.paid`/`priority`/`weakest` and `X-Request-ID`; with `routes`, surplus work and prep trials), `exclude` (models to leave out), `key` (`GET /v1/key`, the daily limit), `tasks` (`/v1/tasks` agent tasks: audits and agentic prep), `feedback` (`POST /v1/feedback` on how answers fared), `decide` (a decision model that screens tasks first). `none` declares none; unset keeps what an earlier start declared (none on the first). Without an extension, Waypoint sends only standard requests and skips the work that needs it. When a later start declares or drops `routes`, or names another model, each task whose chain still starts with what setup gave it asks for the routes or the model instead, the rest of its chain kept; a chain changed in Admin → AI stays as it is. OpenLLMs implements them all |
| `OPENLLMS_BASE_URL`, `OPENLLMS_API_KEY` | none | the names of `LLM_GATEWAY_URL` and `LLM_GATEWAY_API_KEY` before WP-1187, read for one release with a warning when the new name isn't set; a gateway set up through them keeps every extension (a first setup through them declares them all) unless `LLM_GATEWAY_EXTENSIONS` says otherwise |
| `WAYPOINT_ASSISTANT_KEY_HEADER` | `X-Gate-Key` | the header an assistant's gate reads Waypoint's key from (Admin → Assistants); each assistant can set its own |
| `WAYPOINT_ASSISTANT_AGENT_URL` | none | an assistant gate's address as the assistant's own agent reaches it (`http://host:port`), where the gate serves each connection as an MCP server at `/conn/NAME/mcp`. Without it (or an assistant's own), the assistant's Tools page lists connections but can't add them to the agent for you |
| `WAYPOINT_CLI_DIR` | `cli/` next to the binary (Docker image: `/cli`) | where the `wp` binaries served at `/cli` live (the installers `/cli` and `/setup`, and `wp upgrade`, which runners and hooks run when the server's version changes, download them) |
| `WAYPOINT_AI_MODEL` | `opencode/big-pickle` | the model the default provider gets for every AI task (a free one) |
| `WAYPOINT_LOG_LEVEL` | `info` | `debug` logs every API request |
| `WAYPOINT_LOG_FORMAT` | `json` | `text` for humans |
| `WAYPOINT_LOG_DIR` | `logs/` next to the database (Docker image: `/data/logs`) | the server log's day files, read by `wp server log` across restarts and deploys: one JSON line a record, secrets blanked, mode 0600, gzipped once the day is over, at most 512 MB in all; `off` keeps the log in memory only |
| `WAYPOINT_LOG_DAYS` | `30` | how many days of log files to keep |
| `WAYPOINT_TRANSCRIPT_DIR` | `transcripts/` next to the database (Docker image: `/data/transcripts`) | the transcript archive `wp usage sync` uploads (WP-1466): one gzipped file per piece, about a third of the raw size of the sessions on the machines that sync (one busy machine on 2026-11-21, by `find ~/.claude/projects -name '*.jsonl' -mtime -30`, sub-agent files included: 3,314 files were 6.6 GB raw and 2.0 GB through `gzip -6`, and the last 7 days alone were 3.0 GB raw; that is before redaction and gzipped as one stream, so take it as a rough size, not a forecast); the daily DB backup does not include it; `off` keeps no archive: an upload and a read are answered `501` (`this server keeps no transcript archive ...`), which is a setting and not logged as a failed request, and `wp usage sync` takes it to mean this server keeps no archive (nothing is held or counted as failed) |
| `WAYPOINT_TRANSCRIPT_MAX_MB` | none (0 or unset: no cap) | the most the transcript archive may hold, in MB. When storing a piece would pass it, the server refuses with `507` (`the transcript archive is full: ...`) and counts the refusal (`refused` in `wp usage machines` counts refused upload attempts, not files); `wp usage sync` treats that as held (the file stays pending, the pass stops quietly with that reason, nothing is dropped) and tries again next time (a machine's hook passes wait an hour after a `507` before trying again; a manual `wp usage sync` always tries). The cap refuses, it never deletes: see "Backups and restore" |
| `WAYPOINT_DEV_PROXY` | none | proxy the web app to a Vite dev server |
| `OTEL_EXPORTER_OTLP_ENDPOINT` (or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`) | none | turns on OpenTelemetry traces, sent over OTLP/HTTP (e.g. `http://tempo:4318`). The other standard variables apply: `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_SERVICE_NAME` (default `waypoint`), `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_TRACES_SAMPLER`/`_ARG` (default `parentbased_always_on`); `OTEL_SDK_DISABLED=true` turns them off again. See Observability |

Development only: `WAYPOINT_BACKEND` (read by `web/vite.config.ts`, default
`http://localhost:8080`) is where Vite proxies `/api`, `/mcp` and `/cal`.

## First run

Start the server; with no users it prints a one-time link (valid 24h) to
create the admin account. Lost it? Run `waypoint invite --admin` (in Docker:
`docker exec waypoint-waypoint-1 /waypoint invite --admin`).

## Commands

```sh
waypoint                          # serve
waypoint invite [--admin] [email] # print a signup link (7 days); flags before the email
waypoint reset-password <user>    # print a password reset link (48h)
waypoint backup <file>            # consistent copy of the database (VACUUM INTO)
waypoint healthcheck              # exit 0 if the local server answers /healthz
waypoint version
```

Commands use the same `WAYPOINT_DB` and are safe to run while the server is
up: SQLite in WAL mode allows a second process, and `backup` uses
`VACUUM INTO`, which writes a transactionally consistent copy.

## Docker (compose)

`deploy/compose.yaml` reads per-host settings from `deploy/.env`
(gitignored):

```sh
cat > deploy/.env <<'EOF'
WAYPOINT_PORT=21200
WAYPOINT_BASE_URL=https://waypoint.example.com
WAYPOINT_METRICS_TOKEN=<openssl rand -hex 24>
OPENCODE_SERVER_PASSWORD=<openssl rand -hex 24>
EOF
make deploy        # = docker compose -p waypoint -f deploy/compose.yaml up -d --build
docker compose -p waypoint -f deploy/compose.yaml logs waypoint | grep -i invite
```

Other knobs: `WAYPOINT_BIND` (default `0.0.0.0`; set `127.0.0.1` if the
proxy runs on the same host), `WAYPOINT_TRUST_PROXY` (default `1` in compose:
private addresses, where a LAN or same-host proxy connects from),
`WAYPOINT_LOG_LEVEL`.

Compose also runs `opencode` (built from `deploy/opencode/Dockerfile`), the
default AI provider until an LLM gateway is set up (then the gateway goes first in
each chain, and the service is only used for anything you point at it): it's only reachable on the compose network and is
protected by `OPENCODE_SERVER_PASSWORD`, which Waypoint is given too. It brings
opencode's free models; to add others (Anthropic, OpenAI, …) sign in inside
the container: `docker compose -p waypoint -f deploy/compose.yaml exec
opencode opencode auth login`. Don't want AI? Set `WAYPOINT_AI_OPENCODE_URL=`
(empty) in `deploy/.env` and remove the service. See WP design
note 346, "AI estimates and sub-tasks".

Compose also runs `meaning` (built from `deploy/meaning/Dockerfile`), a small
CPU model service Waypoint asks how goals and tasks relate in meaning: e5
embeddings (`intfloat/e5-base-v2`, for topical overlap) and a DeBERTa NLI
cross-encoder (`cross-encoder/nli-deberta-v3-base`, for restating and
contradiction). Both models are baked into the image at pinned revisions; it
needs no key, is only reachable on the compose network, and sends nothing
outside the host (`HF_HUB_OFFLINE=1`). The image is about 3 GB on disk (CPU-only torch and both models; `docker
images` reports 4.2 GB) and needs about 0.6 GB of memory once both models are
loaded; a batch of 32 texts takes one to three seconds on 4 CPU threads, and the
first build takes several minutes to download them.
The API is JSON over HTTP on port 8080: `GET /health`, `GET /v1/models`,
`POST /v1/embeddings` (OpenAI format) and `POST /v1/nli`; at most 256 inputs
per request, each clipped to 2000 characters. `MEANING_TOKEN` (in `.env`) makes it require
`Authorization: Bearer <token>`, and compose hands the same token to Waypoint
as `WAYPOINT_AI_MEANING_TOKEN`; `MEANING_THREADS` (default 4) sets the torch
threads. `/health` needs no token; it runs both models on a word at most
every 10 seconds and never waits behind a request (it reports them busy),
so a process that answers HTTP while its models are wedged turns
unhealthy. Docker's unhealthy mark restarts nothing, so the service ends
itself and compose restarts it: a watchdog thread exits when a batch has
run longer than `MEANING_STUCK_S` (default 180 seconds) or when its own
`/health` has failed six times in a row, 10 seconds apart. A deadlock that
stops every Python thread stops the watchdog too; that one takes `make
restart-remote` (below). Waypoint stops calling a
service that left three calls in a row unanswered (no connection, a timeout,
a 5xx): calls fail at once for 5 minutes, then one goes through as a try,
and each failed try doubles the wait, up to an hour; the goal judgements
schedule a run for when the next try may go, so it comes back without
waiting for new work. It logs `meaning
service: not answering` when it stops and after each failed try (one
problem in `wp server problems`, with when it began), `answering again`
when it comes back, and `wp server status` shows its state. To use another server with the same API, set
`WAYPOINT_AI_MEANING_URL` to it; set it empty to turn the feature off and
remove the service.

The image is distroless, runs as uid 65532, and keeps everything under
`/data` (database plus daily backups in `/data/backups`) on the
`waypoint_waypoint-data` volume. Its `HEALTHCHECK` runs `waypoint
healthcheck`. Run **one** container — SQLite has one writer.

### The production host

Production runs with compose, in `~/waypoint` on its host. `DEPLOY_HOST` is
that host's ssh name for the `make` targets below: set it in the
environment or in `deploy/site.mk` (gitignored; `DEPLOY_HOST = prod`).
`deploy/.env` there sets `WAYPOINT_PORT` and `WAYPOINT_BASE_URL` (plus the
opencode password, metrics token and `LLM_GATEWAY_API_KEY`); a reverse proxy
terminates TLS and forwards to that port (it sends `X-Forwarded-For`,
`X-Real-IP` and `X-Forwarded-Proto`, and does not buffer server-sent
events). A deployer there pulls each release from the forge and deploys it
([Continuous delivery](#continuous-delivery)); `make deploy-remote` ships a
`git archive` of a checkout by hand instead (only committed changes are
shipped), through the same program, `deploy/remote-deploy.sh`. `make
deploy-status` shows what runs there and what the deployer would do next,
and `make rollback-remote` goes back one deploy.

### Continuous delivery

WP-D21, WP-ADR45. Every change to main arrives by pull request; once it
merges, CI releases it and production pulls the release and deploys it,
without anyone running a command. CI holds no key to production (WP-406):
every job on the runner pool can reach its host's docker socket, so a
deploy key in a CI job is readable by whatever runs beside it.

**The pipeline** (`.forgejo/workflows/ci.yml`, on push to main only):

1. `backend`, `lint` (Go, workflow and web checks), `e2e` run as on any
   change. A pull request titled `WIP: …` (a draft, how agents open them,
   WP-158) gets the fast tier: no race detector, no e2e, no gosec,
   govulncheck, npm audit or web build. Dropping `WIP: ` runs the full suite
   once. A pull request that changes only docs skips the tests. A merge whose
   tree is exactly its pull request's head, where that head passed the full
   suite, skips the checks on main and goes straight to `release` (its
   `current` job says so in a notice, and says why when it can't tell, WP-159,
   WP-315; `wp ci log SHA --job current --full` reads that log, and
   WP design note 357 "Skipping main's rerun" has how it decides). Main's runs queue
   one behind another rather than cancelling each other (a merge used to
   cancel the run still testing the one before, so a busy afternoon of merges
   released nothing, WP-62). A queued run whose commit is no longer main's tip
   when it starts skips everything (its `current` job says so in a notice):
   the newer push's own run releases both changes. On a ready pull request
   `current` is skipped and the three checks queue at once (WP-156). On a push to a
   draft ("WIP: ") it runs once, light, and skips the checks when the push only merged
   the base in over a head whose checks passed (its notice says why not otherwise, WP-1808);
   it reads the head's parents from the API first (a push that is not a two-parent merge
   costs one request), counts only real, non-skipped results, walks back over at most 10
   merge-only commits to the first with real passed ones, and bounds itself with timeouts;
   that is a skipped draft run, never a full one: dropping "WIP: " still runs the full suite.
   `backend` and `e2e` are the statuses branch protection and Waypoint read,
   but each is a small job that waits for parallel shard jobs (`backend shard
   1`..`4`, `e2e shard 1`..`4`, WP-313, WP-314) and fails unless every shard
   passed. The backend tests (`.forgejo/scripts/gotest-shard.sh`) deal
   `internal/core`'s and `cmd/wp`'s tests out by name, heaviest first by the
   seconds in `.forgejo/scripts/gotest-weights.txt` (unlisted tests count
   1s), so a new test lands in a shard by itself, and give every other
   package whole to the shard with the least package time; a shard runs its
   part of each split package and its whole packages side by side. The browser suites (`e2e/shard.mjs`)
   are dealt out by the seconds in `e2e/weights.txt`; a suite missing from it
   counts as the median one, so refresh the file from a run's
   `e2e/out/timings.txt` now and then. Each e2e shard runs its suites in two
   lanes side by side (`E2E_LANES`, each with its own server and database),
   because the suites mostly wait on the browser. `SHARDS` in each shard job and its
   `matrix.shard` list must agree; `internal/cilint/repo_test.go` checks.
   A pull request or main run does each browser suite at one viewport and
   theme (`E2E_LOOKS=one`); a weekly schedule (Sundays 03:17 UTC, or "Run
   workflow" with looks `all`) runs the e2e jobs alone at all four looks (plus `advisories`, below). Its
   failure is the red `ci / e2e (schedule)` on main's tip commit and in the
   repository's Actions list; nothing else waits for it.

   **Advisories (WP-1660).** `npm audit` (high and above) and govulncheck
   (called vulnerabilities) depend on advisory databases, not on the change
   under test, so a new advisory used to fail every branch's full run. What
   changes is where it fails; no advisory is ignored, except the npm ones on
   `web/scripts/audit.mjs`'s waiver list (each reviewed by a person, with an
   expiry, and checked again on every run).
   `deploy/advisories.sh` runs the check on the tree under test
   (`lint` for npm, the last `backend shard` for Go); with no findings that is
   all. The npm check is skipped on a pull request that leaves `web/package.json`
   and `web/package-lock.json` as they were where it left main: it can't add an
   advisory. The Go check always runs, since a Go change alone can call
   vulnerable code. With findings it checks origin/main's tip the same way and compares
   by advisory id: a pull request or merge queue stack fails only for an id
   it adds (and prints it), and when every finding is also on main it
   passes with a `::warning title=Advisory on main::` naming them. An id
   main's tip lacks isn't added by a branch that had it when it left main (main
   fixed it since): the script checks that commit too (the pull request's
   merge base, `ADVISORIES_BASE_SHA` in CI) and passes with
   `::warning title=Advisory fixed on main::`. An npm advisory the change does
   add passes when `npm run audit -- --only IDS` passes on its tree, with just
   the ids the change adds, so another advisory main has too can't fail them.
   That is how a pull request adds a dependency together with its reviewed
   waiver. Main's own
   findings fail the `advisories` job (its npm step is `npm run audit`, which
   honours the waiver list and expiry rules in `web/scripts/audit.mjs`), which runs on every push to main
   and, as a copy of the job in `.forgejo/workflows/advisories.yml`, every day at
   04:43 and on a manual run (the weekly schedule above is the e2e looks'
   only; a failed daily run shows as `advisories / advisories (schedule)`),
   without
   `current`'s skip, because a merge tested on its branch says nothing about
   advisories published since. No other job needs `advisories`, so `release`
   and `deploy` are not held back by it: an advisory in a build tool must not
   stop the release of an unrelated fix, or of the fix for its own
   dependency. A red `advisories` makes main's push run red; when it is the only
   job that failed, the CI doctor says so as a `main_advisories` warning ("still
   released and deployed") instead of `main_red`, and the base-fix-first and
   runner-fault re-run logic ignore the job. Because nothing waits for the
   job, Waypoint files one urgent task for an agent when it fails ("Fix the
   advisories on REPO main", in the repo's project) and no other while that
   one is open.
   `make vuln` (needs network and `jq`) runs the same script in its pull request mode (it lives in
   `deploy/` because the public mirror has no `.forgejo/`), so locally and on
   the mirror a branch fails govulncheck only for what it adds over
   origin/main. `npm run audit` is unchanged: whole tree, with the waiver list.
2. `release` (needs all three to pass, or `current` to say the merge was already tested) runs `deploy/release.sh --publish`. It picks the
   next version from the commits since the last `v*` tag: the first release is
   `v0.1.0`; a breaking change (`type!:` subject or a `BREAKING CHANGE:` line)
   is a minor bump while 0.x (major from 1.0); a `feat` commit or a new file in
   `internal/db/migrations/` is a minor bump; anything else is a patch.
   It tags the commit, publishes a Forgejo release whose notes are the commit
   subjects (a merged PR shows as its title; grouped Features / Fixes / Other
   when conventional prefixes are used) and attaches the `wp` binaries from
   `make cli`. Preview it any time with `make release-dry-run`.
3. **The deployer** on the production host (`deploy/pull-deploy.sh`, installed as
   `~/bin/waypoint-deployer`, run two minutes after its last run by the
   systemd user timer `waypoint-deployer.timer`) fetches main and the `v*`
   tags with a read-only forge token and deploys the newest
   `vMAJOR.MINOR.PATCH` tag only when its commit is on main (branch
   protection makes main the only reviewed and tested history, so a stolen
   release token can make a tag but can't get anything else deployed), it
   descends from the commit running now (nothing goes backwards; a live
   commit main doesn't have yet, such as an incident deploy of a branch,
   holds every release until it is merged), and its
   release is published with the `wp` binaries (`release.sh` attaches them
   last). Each release is tried once: after a rollback or a failed deploy it
   waits for the next release instead of putting the same one back. It hands
   `git archive` of that commit to `~/bin/waypoint-deploy`, records the
   deployment (started, then succeeded or failed) in Waypoint's *production*
   environment when it has a Waypoint token (without one Waypoint still sees
   the new version on `/healthz` and records it as detected), and after a
   successful deploy updates its three programs in `~/bin` from the release.
   A release usually goes live a few minutes after `release` finishes.
   `make deploy-status` runs `waypoint-deployer status`: the newest release,
   what is live and what the next run would do, or why not (`REFUSED: …` for
   a tag that isn't on main or would go backwards, or a live commit not yet
   on main). Its log: `ssh $DEPLOY_HOST
   journalctl --user -u waypoint-deployer`.

   Until `deploy/setup-cd.sh` retires it, CI's old `deploy` job still deploys
   over ssh when the `DEPLOY_SSH_KEY` secret exists, to the repository
   variables `DEPLOY_USER_HOST` (`user@host`) and `SITE_URL` (it fails, saying
   so, when the key is there and they aren't); with the secret gone it skips
   itself with a notice, and WP-569 removes it.

**On the host** `~/bin/waypoint-deploy` is a copy of
`deploy/remote-deploy.sh`. It accepts only `deploy VERSION` (a tag or sha,
tarball on stdin), `rollback` and `status`. A deploy unpacks the tree,
keeps the host's `deploy/.env`, copies the database inside the data volume
(`backups/pre-deploy-VERSION.db`, so a bad migration can be undone), moves the
live tree to `~/waypoint.prev` (one is kept), runs `docker compose -p waypoint
up -d --build` and waits up to 3 minutes for `/healthz` to report VERSION. It
never touches another compose project on the host. Exit
status: 0 done; 1 refused or failed before changing anything; 2 the new
version never became healthy and the previous one is running again (the bad
tree stays in `~/waypoint.failed`); 3 failed and the previous one could not
be restored: Waypoint may be down, look at `docker compose -p waypoint logs`.
Waypoint doesn't prune the `pre-deploy-*.db` copies; delete old ones from the
volume now and then.

**Pausing**: `ssh $DEPLOY_HOST bin/waypoint-deployer pause WHY` stops deploys
(an incident, a by-hand deploy you want to keep); `… resume` starts them
again, and the next run deploys the newest release it hasn't tried.

**One-time setup**, by a person, from a checkout: `deploy/setup-cd.sh` (add
`--dry-run` to read what it will do). It walks through eight numbered steps,
each explained and confirmed: the deployer's read-only forge token (scope
`read:repository`, stored on the production host in
`~/.config/waypoint-deployer/forge-token`, mode 0600); the optional Waypoint
API token (Settings → API tokens, write on project WP) it records deployments
with (`~/.config/waypoint-deployer/waypoint-token`); installing the deployer,
`waypoint-deploy`, `waypoint-record-deployment` and the systemd units;
retiring CI's deploy key (deleting the secrets `DEPLOY_SSH_KEY`,
`DEPLOY_KNOWN_HOSTS` and `WAYPOINT_DEPLOY_TOKEN` with a Forgejo admin token you
paste, used once and never stored, and the key's line in the host's
`authorized_keys`); starting the timer; the optional `RELEASE_TOKEN`; branch
protection on main; and "delete branch after merge" by default. It can be
re-run safely. The deployer's user needs lingering on (`loginctl
enable-linger`) so its timer runs with nobody logged in. The deployer reads
`FORGE_URL`, `REPO` and `WAYPOINT_URL` from `~/.config/waypoint-deployer/settings`,
which setup writes; an install from before that file falls back to its
mirror's origin and the live tree's `WAYPOINT_BASE_URL`.

Secrets the workflow reads: `RELEASE_TOKEN` (optional), and until the old
push path is retired `DEPLOY_SSH_KEY`, `DEPLOY_KNOWN_HOSTS` and
`WAYPOINT_DEPLOY_TOKEN`, with the variables `DEPLOY_USER_HOST` and `SITE_URL`.
The `release` job uses the job's own token (Forgejo ignores a workflow's
`permissions:` block, so the workflow has none); if the forge refuses it (the job log says "refused (403)"), set `RELEASE_TOKEN`
to a token with `write:repository` and it is used instead.

**The urgent lane** (WP-528): a pull request labelled `ci-urgent` has every
job ask for `forgejo-ci-urgent`, a runner label the pool keeps for it, so it
starts ahead of everything waiting (the runner pool's configuration gives
it its own slots). Almost never use
it: every urgent job slows CI for every repo on that machine. It is for the
rare pull request that must skip the line, like a fix to CI while CI is
stuck, and an issue's priority is not a reason. `wp ship --urgent` or `wp pr open --urgent` opens the pull request with the label, so
its first run is already on the lane; `wp pr urgent N` labels an open one,
which starts its run again on the lane and cancels the queued one, and `wp
pr urgent N --off` takes it off. A task tagged `ci-urgent` passes the label to
its pull request when that opens, by `wp ship` or on the forge; tagging a task
later changes nothing already open, and a push or another ship doesn't bring
back a label taken off. Waypoint
makes the label in a repo that lacks it. Any other label added to a pull request runs nothing and cancels
nothing (its checks skip as "label added, nothing to run"). Pushes, to main
and the merge line's candidates, carry no labels and keep their usual runners.
`wp pr show` and `wp pr ls` say when a pull request is urgent. The label also
puts it ahead in its merge line and its babysit rounds ahead of the line
(WP design note 357, "An urgent pull request cuts the line", WP-686).

**The priority lane** (WP-1991, WP-ADR32): `current` and the `backend` and
`e2e` aggregates, the jobs of seconds that every run waits on, ask for
`forgejo-ci-priority`. Forgejo gives a free slot to the job that became ready
first, so on a full pool each of them waited a whole turn, three turns a run.
The runner pool's configuration keeps two slots that take only this label, so
these jobs start in seconds; every other runner offers the label too, so
while those two slots are down the jobs wait their turn as before and never
wait forever. An urgent pull request's go to the urgent lane with its other
jobs. Only jobs of seconds belong on it: `release` and `deploy` run for
minutes and ask for `forgejo-ci`. The two slots are a runner of their own in
the pool's configuration; until it is started the label reserves nothing
(`wp ci status -A --json` gives each host's `labels` and `capacity`).

A merge line candidate whose heads don't merge fails in `current`, which
builds the merges first, and its other jobs skip: the line reads the conflict
from that one light job instead of waiting for a heavy job to get a slot.

**The CI job image** (WP-402): the heavy jobs (backend shards, lint, e2e
shards) don't run in the pool's image but in Waypoint's own,
the `waypoint-ci` package `.forgejo/ci-image/build.sh` pushes (its `REPO`), built from `.forgejo/ci-image/Dockerfile`.
It has Go at go.mod's version and Node in the tool cache setup-go and
setup-node read, Playwright's Chromium and its system packages, and the
Makefile's Go QA tools, so none of them installs per run. Its tag is a hash of
those inputs; ci.yml names it by tag and digest (`internal/cilint/repo_test.go`
checks all three jobs agree). When a bump changes an input (Renovate's Go,
Playwright or tool pins), CI still passes on the old image, installing the
difference every run, and the lint job warns "CI image is stale": run `make
ci-image` on a host logged in to the registry as `claude` (`docker login`), which
builds and pushes the new tag and rewrites ci.yml, and commit that. Pulling
needs no login: the `claude` account's packages are public, and the image holds
only public toolchains. `.forgejo/ci-image/build.sh --local` builds it without
pushing, to try a change.

**CI on GitHub** (WP-1624): the public mirror, github.com/avranu/waypoint, runs
`.github/workflows/` on GitHub's standard hosted runners, which GitHub does not
bill a public repository for
([About billing for GitHub Actions](https://docs.github.com/en/billing/managing-billing-for-your-products/managing-billing-for-github-actions/about-billing-for-github-actions);
its limits on concurrent jobs and runner size have not been checked against our
load: WP-1629). Two settings on the mirror must hold for them to run: Actions
enabled (Settings, Actions), and code scanning left on advanced setup, not
GitHub's default CodeQL setup, which refuses `github-codeql`'s upload. No run there gates a merge, a
release or a deploy; those go by Forgejo's statuses alone, so a GitHub outage
or a halted mirror stops nothing. (Forgejo's lint and backend jobs do check the
files themselves, with actionlint and `internal/cilint`.) Nothing in Waypoint
watches these runs either: a failure shows in GitHub's Actions and Security
tabs only, until WP-1629 decides whether they earn an alarm.
`github-ci` runs on each push forge-sync makes to main: lint, the backend tests
under the race detector (three jobs: `internal/core`, `cmd/wp`, the rest) and
the four e2e shards; on Sundays it runs the e2e shards at all four looks.
`github-codeql` scans main with CodeQL into GitHub's Security tab (pull requests
are scanned on Forgejo by forge-checks; a finding triaged in `.forgejo/codeql/`
is dismissed by hand on GitHub, which never sees that file).

The rules, which `internal/cilint/repo_test.go` checks: no secrets (GitHub has
none of ours), no `pull_request` or `pull_request_target` trigger, GitHub's
hosted runners only (never attach one of ours to the public repository),
read-only `permissions:` (but `security-events: write` for the job that uploads
CodeQL's findings), and no path under `.forgejo/`, which forge-sync
strips: the workflows can't use Forgejo's scripts, actions or job image, so they
install Go, Node, the QA tools and Chromium themselves, from GitHub's cache.
forge-sync also rewrites every commit (the tree without `.forgejo/`), so a
GitHub SHA is never Forgejo's; the commit's `Sync-Origin-Commit:` trailer names
it (read off the mirror's commits in November 2026: forge-sync is another
repository, so nothing here checks it). A run starts only after forge-sync has
published the push, and not at all while it holds main (a gitleaks finding).

What a GitHub job leaves out: the race jobs run without `-cover`, and
`TestNoSiteNames` skips there (its list is the `SITE_NAMES` secret), as do
`internal/cilint`'s checks of Forgejo's workflow.

To compare GitHub's runners with ours, take the last 7 days' push runs only (a
Sunday run does four looks, and a manual one may deal eight shards), and for
each job its wait for a runner, its time when green (the median, as `wp ci
history` gives ours), and how often it failed:

```sh
gh run list -R avranu/waypoint -w github-ci -e push -s completed --created ">=$(date -u -d '7 days ago' +%F)" -L 500 --json databaseId -q '.[].databaseId' |
  while read -r id; do
    gh run view -R avranu/waypoint "$id" --json createdAt,jobs \
      -q '.createdAt as $c | .jobs[] | select(.conclusion != "skipped")
          | if .conclusion == "success" or .conclusion == "failure"
            then {name, conclusion, wait: ((.startedAt | fromdate) - ($c | fromdate)), time: ((.completedAt | fromdate) - (.startedAt | fromdate))}
            else {name, conclusion} end'
  done | jq -rs 'def p50: if length == 0 then "-" else sort[(length - 1) / 2 | floor] / 6 | round / 10 end;
      group_by(.name)[] | map(select(.time)) as $done
      | "\(.[0].name): \($done | length) finished, \($done | map(select(.conclusion == "failure")) | length) failed, \(length - ($done | length)) cancelled, p50 wait \($done | map(.wait) | p50) min, p50 time when green \($done | map(select(.conclusion == "success").time) | p50) min"'
wp ci history --days 7   # ours, under "Full-tier jobs": lint, e2e shard N, backend shard N
```

Read the two with these in mind:

- A cancelled job (the next push superseded its run) has no time, and the
  longer a job runs the likelier it is cancelled, so GitHub's p50 reads low
  when "cancelled" is large beside "finished". A handful of finished runs
  settles nothing either: how many are enough, and how much faster counts, is
  WP-1629's to set before it reads the numbers.
- Ours counts the full tier's jobs on pull requests as well as on main: the
  same jobs running the same commands on the same hosts, where GitHub has main
  only. (Our Sunday four-look run is in there too, one run in a week's median.)
- Lint and the e2e shards compare job with job, with two cautions. GitHub's e2e
  job installs Chromium each time and ours has it in the job image, so compare
  the "browser checks" step (`gh run view --job ID --json steps`) or allow for
  the install. And GitHub's lint job builds the QA tools (`make tools`, some
  minutes) on the first run after a Go version, a pin or the `tools` recipe
  changes, where ours has them in the job image: leave that run out.
- The race jobs do not compare job with job: ours are four shards dealt by test
  name and GitHub's are three groups by package. For runner time compare the
  sum of the three with the sum of our four backend shards; for the wall clock
  compare the longest of each, which on GitHub is `race (core)`, all of
  `internal/core` in one process (26 minutes on our hosts against 8 for a
  shard).

`gh workflow run github-ci -R avranu/waypoint -f e2e_shards=8` deals the browser
checks over eight jobs instead of four (manual runs never cancel each other, so
a 4-shard and an 8-shard run can go side by side; read their jobs with `gh run
view`, since the recipe above takes push runs). Moving a check that gates to GitHub
needs more than a faster runner: a relay that posts GitHub's result on the
Forgejo commit the trailer names, pull request runs (and so a rule for the
mirror's twin pull requests), and Waypoint reading `wp-evidence` lines from
GitHub's logs. WP-1629 holds the measurement and that decision.

**Go's caches** are kept per kind of job (`.forgejo/actions/go-cache`, with
setup-go's own cache off): `backend-race`, `backend` (the draft tier),
`lint` (with golangci-lint's), `e2e` and `release`, each keyed by Go's
version, go.sum and the day. The first passing job of a kind each day saves
it; the rest restore it and save nothing. setup-go's cache had one key for
every job, and restored it as `setup-go-linux-…` but saved it as
`setup-go-Linux-…`, so every heavy job uploaded 350 to 480 MB again (45 to 90
s) and none of them found a cache of its own kind.

**Rolling back**: `make rollback-remote` (or `ssh $DEPLOY_HOST 'bash
waypoint/deploy/remote-deploy.sh rollback'`) swaps the previous tree back in
and waits for it to be healthy; run it again to go forward. The deployer
doesn't undo it: it never deploys a release twice by itself. Migrations only
move forward, so if the version you leave added one, the script warns, and a
rollback that misbehaves needs the `pre-deploy-*.db` copy restored (Backups
and restore, below). After a rollback, fix forward: the next merge to main
deploys again, so revert or fix the bad change by pull request.

**When something fails**:

- A red `release` job: nothing was tagged, or the tag exists without
  binaries. Re-run the job; it completes what is missing.
- A release that didn't go live: `make deploy-status` says what the deployer
  sees and why it waits; `ssh $DEPLOY_HOST journalctl --user -u
  waypoint-deployer` has each run's output, including the last 40 container
  log lines of a failed deploy (`systemctl --user --failed` lists the failed
  run). If the new version never became healthy the script already rolled
  production back (exit status 2), and the next release deploys as usual.
- A deploy that worked but wasn't recorded: recording never fails a deploy;
  Waypoint records the version it sees as detected.
- To deploy by hand, `make deploy-remote` from a checkout of the tag (pause
  the deployer first if it shouldn't follow up with a newer release).

**How agents ship**: branch → commit → open a pull request into main → arm
"merge when ready" (`wp pr merge N --when-ready`, or label it `automerge`;
with the repo's *Merge automatically* on, Waypoint arms it by itself).
Waypoint merges it when the checks and the review gate are green and reviews
have settled; CI then releases and production deploys the release. Nobody pushes to main (branch protection forbids it,
admins included) and nobody runs `make deploy-remote` for a normal change.
An agent ends at `wp ship`; when it must block on the result it runs `wp pr
wait N` once (a verdict line, the failing log parts, an exit code), and
reads a failed job with `wp pr log N`, not by polling CI.

## Kubernetes (k3s)

`deploy/k8s/waypoint.yaml` is a one-replica StatefulSet (a rolling update
stops the old pod before starting the new one, so there is never a second
writer) with a PVC, startup/readiness/liveness probes, a read-only root
filesystem and an Ingress with TLS. Edit the `CHANGE-ME` lines, push the
image to a registry the cluster can pull from, then
`kubectl apply -f deploy/k8s/waypoint.yaml`.

## Backups and restore

Daily backups land in `WAYPOINT_BACKUP_DIR` as `waypoint-YYYY-MM-DD.db`
(the first one is written at startup). They contain password hashes and
integration tokens, so keep the directory private and copy it off the machine
(restic, rclone, …).

The transcript archive (`WAYPOINT_TRANSCRIPT_DIR`) is not in these backups: the
database holds only an index of it. Copy that directory off the machine too,
and treat it as private (it holds what was said in every session, redacted of
credentials of known shapes only). Restoring a database without it leaves the
index pointing at files that are not there; reading such a piece is an error.
The server notices: at start and then daily it checks (by file name only, no
file is read) that each indexed piece has its file, and the "transcript
archive" line of `wp server status` says "N stored transcript pieces are
missing on disk" while any are. It reports only. It does not change the index,
and the machines do not send those bytes again by themselves: their offsets
are already past them, and a machine with nothing new in a file never asks the
server where its copy ends. The remedy is to put the directory back from your
copy; once the files are there the next daily check clears the line. Pieces
with no copy anywhere stay missing.

Nothing in the transcript archive is deleted by age or by size, and removing a
session from the app leaves its transcripts. The one exception: deleting a
user's account removes that account's archived transcripts from the disk (the
`user.delete` audit entry says so, and `user.transcripts.remove_failed` says
when the files could not be removed). `WAYPOINT_TRANSCRIPT_MAX_MB` only makes
the server refuse new pieces once the archive is that large (the uploading
machines keep the files pending and say so in `wp usage machines`); it does not
prune. Whatever the cap, the server also refuses (`507`, "the server's disk is
nearly full") a piece that would leave less free space on the archive's disk
than the free-disk limit of the admin Automations page (1024 MB unless changed;
0 turns the floor off); nothing is deleted then either, and the machines hold
their files like they do for a full archive. The refusals carry a `code`
(`transcript_full` for both, `transcript_job` for a runner job's token) that
the client goes by. The "Transcript archive" resource turns low, and the
admin automations for it fire, when the archive holds 90% of the cap (and when
it passes the admin's own limit), so you hear before uploads are refused. Pruning old pieces and backing the archive up are yours to do: the
directory needs its own off-machine copy. The cap is checked against the
size the index records, which changes only when pieces are added to the index:
deleting or moving files in the directory frees no room (and reading such a
piece is an error); the remedy for a full archive is to raise the cap. The archive's size and piece count are in the admin Automations page
(System limits, "Transcript archive larger than (MB)" alerts like disk,
database and memory do) and in `GET /admin/system`; a machine's
opt-out (`WP_USAGE_ARCHIVE=off`), drops and slow or cut-short passes are in
`wp usage machines` and the "transcript archive" line of `wp server status`.
The drop reasons `wp usage machines` lists are `upload_failed`, `refused` (a
file the server refuses for good; the machine retries it daily),
`line_too_large` and `redaction_failed` (a line replaced by a stub),
`deleted_unsent` (a transcript deleted from the machine with bytes the server
never got), `unreadable_file`, `out_of_sync` and `state_reset`. The "dropped"
total of `wp server status` and the collector notice count the ones that need
someone to act: `line_too_large`, `redaction_failed`, `refused` and
`deleted_unsent`; the others the next pass settles. The pass times are the
machine's hook passes only (a manual `wp usage sync` backfill is not timed
there), and the last one can be up to 12 hours old. A hook pass is bounded:
the hour counts' uploads together get 10 seconds from its start and its status
post ends inside the hook's 30. A manual `wp usage sync` that is not `--all`
says how many transcripts older than its 30-day window were never uploaded;
`wp usage sync --all` archives them.

Restore (compose):

```sh
docker compose -p waypoint -f deploy/compose.yaml stop
docker run --rm -v waypoint_waypoint-data:/data alpine sh -c '
  cp /data/waypoint.db /data/waypoint.db.before-restore &&
  cp /data/backups/waypoint-2026-10-12.db /data/waypoint.db &&
  rm -f /data/waypoint.db-wal /data/waypoint.db-shm &&
  chown 65532:65532 /data/waypoint.db'
docker compose -p waypoint -f deploy/compose.yaml start
```

Check a backup without restoring it:
`sqlite3 waypoint-2026-10-12.db 'PRAGMA integrity_check'`.

## Upgrades

Migrations run automatically at startup and only move forward. An older
binary starts against a newer schema without complaint but may misbehave, so
**rolling back means restoring the backup taken before the upgrade**. Take one first
(`waypoint backup`, or rely on the daily one), then swap the binary or image
(`make deploy`; production deploys itself, see Continuous delivery). `waypoint version` prints the running build.

A server built with a version names it on every answer (`X-Waypoint-Version`;
one built without sends no header, and nothing follows it), and `wp` and
runners built from a release follow it:

- **`wp`**: any command that sees the server on another release starts
  `wp upgrade --auto` in the background when it ends, once per server
  version, and tries again after 6 hours if it didn't take. The SessionStart
  hook also looks once a day. One download runs at a time: the others wait
  for its claim (15 minutes at most).
- **Runner**: the download starts as soon as the server's version differs,
  even with jobs running, and once it succeeds the binary is replaced, so
  `wp` calls on that machine use it at once; until then (or for 6 hours
  after a failed download) jobs run the old one. The runner process restarts
  on the new binary when no job is running, whichever `wp` put it there, as
  long as the file is still the one it checked. If it has waited 15 minutes
  (counted from the first release it was to restart on, even when a newer
  one has replaced it since), it takes no new jobs until the running ones
  finish, then restarts. A server back on the runner's own release has the
  binary put back, with no restart; a put-back that fails is tried again
  after 15 minutes, not 6 hours.

This applies only to release builds (`vX.Y.Z`) on both sides; a runner started
with `--once` never updates. A runner started on a `wp` from before this
change keeps the old rule (it updates only when no job is running), so run
`wp upgrade` and restart the runner once on each machine.

`WAYPOINT_NO_AUTO_UPDATE=1` stops all of this, and `wp runner --no-auto-update`
stops it for one runner. A runner that doesn't update (those two, or `--once`)
passes `WAYPOINT_NO_AUTO_UPDATE=1` on to everything it starts (jobs, sessions,
autopilot), so the `wp` they run stays as it is too.

## Observability

- `GET /healthz`, `/readyz` — database reachability (both; there is no
  separate warm-up phase beyond migrations, which finish before the listener
  starts). The JSON includes the build `version`, which CD polls to see a deploy land.
- `GET /metrics` — Prometheus: request counts/latency by route, live clients,
  automation runs, webhook deliveries, Go runtime. Scrape with
  `authorization: { credentials: <WAYPOINT_METRICS_TOKEN> }`.
- Logs are JSON on stderr: one line per failed request, automation failures,
  webhook failures, sync problems. Docker keeps them with its default log
  driver; set compose `logging` options if you need rotation limits.
- Traces (optional, WP-D8): set `OTEL_EXPORTER_OTLP_ENDPOINT` to an
  OTLP/HTTP collector (Tempo, Jaeger, the OpenTelemetry Collector). Each
  request is a span named by its route (`GET /api/v1/issues/{key}`), with
  child spans for write transactions (`db.write CreateIssue`, with the time
  spent queueing for SQLite's single writer as `db.wait_ms`), the work after
  a change (`dispatch`: notifications, webhooks, automations) and outbound
  calls (method, host and status only). Scheduled jobs (`job forge sync`,
  `job hourly`, …) are traces of their own. Log lines written inside a
  span carry `trace_id` and `span_id`. Spans never hold paths, query
  strings, issue keys or text, and a `traceparent` header is only
  believed from a `WAYPOINT_TRUST_PROXY` peer. Off, tracing costs nothing.

## Diagnosing production

Read production through `wp`, not ssh (WP-375). Each report is read-only,
for an admin's token or a permission token with `server.read` (runner job
tokens never qualify):

- `wp server status`: version, uptime, schema level and database size; the
  problems that persist; every scheduled job with how often it runs, its
  runs, last run, time taken and longest, and how the last run ended (clean,
  warned, failed, skipped, stopped (the server was stopping: neither clean
  nor unclean), crashed, or interrupted (it was running when the server
  died, found out at the next start; unclean); `STUCK` for a run that has gone on too
  long, or for a job whose every run for longer than a run may take found the
  last one still going (a hung worker), `SILENT` for a job that should have started and hasn't), kept in the
  database, so across restarts; how the run before this one ended
  (`Before this start: stopped cleanly 12:01, away 40s.`, or `DID NOT SHUT
  DOWN (last alive 12:01)` for a server that was killed or died, which also
  logs an error and marks the jobs it was running interrupted); when the job
  health check last finished (`Job health checked 20s ago.`, or `NOT CHECKED
  for 14m` when the check itself is stuck, which a watchdog job also logs and
  tells the admins of once a day; the watchdog runs on a timer of its own, not
  on the scheduler's loop); queue depths (safety screens: the
  unclaimed To Do tasks with unreviewed agent text that no screen has
  cleared since they last changed, which the sweep screens, and the screens
  queued or running in the server; bot runs, runner jobs, automation runs);
  the AI key's day as the gateway counts it (`/v1/key`) and whether requests
  are parked; the meaning service's state (`answering, last answered …`, or
  `DOWN since …` with the calls left unanswered, the next try and the last
  error; its counts are `waypoint_meaning_requests_total` and
  `waypoint_meaning_up` in `wp server metrics`); the last hour's warnings
  and errors, counted by message.
- Request failures are listed per route in `wp server problems`, one record
  for each request. A caller that hangs up is status 499 and is no problem,
  unless it had waited 10s or more on a route that does not hold connections
  open (a long poll or a stream does).
- `wp server log [--since 1h] [--until T] [--grep RE] [--level warn]`: the
  server's own log. Recent records come from memory (48h or 50,000 since its
  last start), older ones from its day files (`WAYPOINT_LOG_DIR`, 30 days),
  so a restart or a deploy loses nothing. The merge queue logs each hold as
  it opens and clears (`--grep 'merge hold.*#377'`), and `wp pr show N`
  lists a pull request's holds. Secrets are blanked as each record is kept: the values of
  secret-sounding environment variables, the stored AI keys, forge tokens and
  webhook secrets (reloaded every minute), and anything shaped like a token,
  bearer header, `token=` parameter or URL password.
- `wp server problems [--all] [--source NAME] [--limit N]`: what goes wrong, kept across
  restarts (WP-ADR139). Every warning and error is counted where it was
  logged: on the scheduled job whose run logged it, or under `requests`,
  `events` (what a write sets off), `automation runs`, `bot runs` or
  `background`. A problem is one source, message and subject (a repo, a
  forge, a collector) with its count, its current streak and the last
  record's attributes. It is persistent once a warning or error has kept
  happening for a day with three records in it (two runs for a job that runs
  daily or less often), and at once for a job that runs only when the
  server starts, wherever it was logged. Admins get one "Server health" notification for
  what became persistent, for stuck and silent jobs and for what recovered,
  and a weekly reminder until someone acknowledges it. A problem is over
  after 25 hours of quiet, not counting the time the server was down: a long
  stop is not a recovery, and not time the problem lasted either (three
  warnings, a 25-hour outage and one more are not a day of failing). With no
  admin to tell, problems still clear and the digest waits for the first
  admin. A secret learned after a problem was recorded is blanked in what is
  stored within two minutes (the `log secrets` job).
- `wp server ack N -m WHY` / `wp server unack N`: an admin, in their own
  terminal, says a problem is expected. It stays listed with who
  and why, is not told again, and counts as new if it stops for a week and
  returns. Agents cannot: they file a request.
- `wp server job NAME [--since 7d] [--by hour|day]`: one job, or one of the
  other sources, over time: runs, runs that were not clean, skips, warnings,
  errors, noted (failures logged below a warning, and failures of work that
  was called off: a server stopping, a caller hanging up), average and longest time (hours
  are kept 90 days), with its problems.
- `wp server metrics [--grep RE]`: the `/metrics` text, without the metrics
  token.
- `wp ai usage [--since 24h] [--by task|model|outcome|status|hour|day|provider]
  [--task NAME]`: every request Waypoint sent an AI provider (table
  `ai_calls`, kept 30 days): requests, failures, rate limits, tokens and
  latency per group.

These are named reports; there is no endpoint for arbitrary SQL. Ssh to the
host stays for incidents a person runs: an older log than the ring holds
(`docker logs`), `make rollback-remote`, `make deploy-status`.

One exception: when `wp server status` says the meaning service is down
(or `make deploy-status` shows it unhealthy) and it has not come back by
itself, an agent restarts it with `make restart-remote SERVICE=meaning`
(`SERVICE=opencode` for the opencode service). It runs one fixed command
through the deploy script (`remote-deploy.sh restart SERVICE`, which takes
only those two names), restarts that container alone and prints its state;
Waypoint's breaker tries it again on its own timer (5 minutes after it
stopped answering, doubling to at most an hour) and finds it answering.

## Reverse proxy

Any proxy works. Requirements:

- Pass `Host`, append `X-Forwarded-For`, set `X-Forwarded-Proto`, and set
  `WAYPOINT_TRUST_PROXY=1` (or the proxy's own address, if it isn't on a
  private network).
- Don't buffer `/api/v1/stream` (server-sent events). Waypoint sends
  `X-Accel-Buffering: no`, which nginx honours; keep read timeouts above the
  25-second keep-alive ping the stream sends. `wp pr wait` holds a request up
  to five minutes; with a shorter read timeout it still works (it asks for
  55 seconds at a time after a 502 or 504), but a timeout of five minutes or
  more saves the retries.
- Allow request bodies up to 5 MB (forge webhooks; avatars are 3 MB, JSON
  2 MB). If runners reach Waypoint through the proxy and audits may run
  through an LLM gateway (`gateway/audit`, WP-ADR84), allow the gateway's archive limit plus about 2 MB: the runner uploads
  its packed checkout, up to what the gateway takes (20 MiB today, 64 MiB
  once the gateway raises it; Waypoint reads the figure from the gateway),
  and it must arrive within Waypoint's two-minute read timeout. A
  proxy that refuses the upload only fails that try (the audit goes on to
  its next model).
- The transcript archive (`POST /api/v1/usage/transcript`, WP-1466) sends
  pieces of up to 8 MiB raw, and a piece may be up to 24 MiB compressed (the
  server's cap), so give that route its own larger limit (below). When a
  proxy answers 413 to a piece, `wp usage sync` halves its piece size and
  goes on, keeps the smaller size for a day, and so still archives through a
  smaller limit, in more requests. Only a single line too large for the
  proxy holds its file (counted as a refused upload, tried again once a day,
  and `wp usage machines` names the likely cause).
- Whether any of this needs a change depends on the proxy: nginx refuses
  bodies over 1 MB unless `client_max_body_size` says otherwise, while Caddy
  has no limit unless the site sets `request_body max_size`. A response's
  `Server` or `Via` header says which one answers (`curl -sI URL/healthz`).

nginx example:

```nginx
location / {
    proxy_pass http://prod-host:21200;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_http_version 1.1;
    proxy_read_timeout 1h;   # long-lived event stream
    client_max_body_size 5m;
}

location = /api/v1/usage/transcript {
    proxy_pass http://prod-host:21200;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_http_version 1.1;
    proxy_request_buffering off;
    client_max_body_size 25m;
}
```
