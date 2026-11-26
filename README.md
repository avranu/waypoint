# Waypoint

A project tracker: issues, epics,
kanban boards, sprints, forecasts, time tracking and automations — in one
small, fast, self-hosted binary whose data you own.

Built to stay out of the way: no clutter, no subscriptions, no plugin
hunting, and no setup tax every time you start something new.

## What it does

- **Issues** with types (epic, story, task, bug, subtask), arbitrary nesting,
  priorities, story points, start/due dates, labels, multiple assignees,
  watchers, relations (blocks, relates, duplicates) and markdown with task lists.
- **Boards** (kanban or sprint) with drag and drop on desktop and phone;
  finished work fades off after a configurable number of days.
- **Backlog & sprints**: plan by dragging, start/complete sprints, carry
  unfinished work over automatically.
- **Hierarchy**: move or re-sprint an epic and everything beneath it at once;
  parents close themselves when their last sub-issue is done (and reopen if
  one is reopened).
- **Statuses you define**, each in a category (backlog, todo, in progress,
  done, canceled) so boards, reports and "done" just work.
- **Automations**: when *X* happens and the issue matches a query, do *Y* —
  set fields, assign, comment, notify, start/stop timers, create subtasks,
  call webhooks, trigger Kestra flows, push to Gotify. Sensible ones are on
  by default; more are one click away.
- **Reports**: burndown, burnup, throughput, cumulative flow, velocity, and
  Monte Carlo ETAs (50/85/95% dates) for projects and epics.
- **Time tracking** built in: one-click timers, manual entries,
  weekly timesheet, per-issue totals.
- **Search** with a small query language (`assignee:me is:open priority:>=high`),
  saved views, and ⌘K for everything.
- **Glossary and design notes**: terms per repo, organisation or everywhere
  (people can lock them against agents), and a record of what people intend
  (goals, features, screens, decisions) that agents can add to but not
  rewrite without approval, with every change dated. `[[Wikilinks]]` and
  links tie them to issues, pages, files and PRs; `wp spec` hands an agent
  the intent for what it's about to change, and `wp search` covers it all.
- **Sharing**: add people to a project, or share just one issue (and what's
  under it) with someone.
- **Notifications** in-app and to Gotify; due-date reminders.
- **Git**: link GitHub/Forgejo PRs and issues; PRs that mention a key (even in
  the branch name) link themselves and can move issues along; repo issues can
  be imported and kept in sync both ways.
- **Calendar**: a private iCal feed of your due dates and sprints for Google
  Calendar.
- **Agents**: a REST API built for scripts and LLMs (compact text output,
  human names everywhere) and an MCP server, so Claude can search, create
  and update issues directly.
  The **Agents** page shows what each agent will take next (and lets you
  reorder or pause it), what agents did, the pull requests that came of it,
  how they used the glossary and pages, and an audit log of people's
  decisions.
- **CLI**: `curl -fsSL <your-waypoint>/setup | sh` installs `wp` (Linux, macOS,
  Windows) and sets the computer up (`wp setup`: sign-in, Claude Code hooks, a
  runner; `wp` keeps itself current); then `wp next`, `wp ls`, `wp new`, `wp page …`,
  `wp spec`, `wp define`, `wp search`, scoped to the git repo you're in (`wp help --all` is the
  full reference).
- **Admin**: invite-only accounts, password reset links, avatars, no public
  signup.
- **Ops**: single static binary, SQLite, daily backups, Prometheus metrics,
  JSON logs, optional OpenTelemetry traces, health checks.

## Quick start

```sh
make            # builds web/ and bin/waypoint (needs Go 1.27+ and Node 24+)
WAYPOINT_BASE_URL=http://localhost:8080 ./bin/waypoint
```

On first start Waypoint prints a one-time link (valid 24 hours) to create the
admin account. Invite everyone else from **Admin → Invites**. The database
lands in `data/waypoint.db`; set `WAYPOINT_BACKUP_DIR` to get daily backups
(the Docker image does this for you).

Docker: put `WAYPOINT_PORT` and `WAYPOINT_BASE_URL` in `deploy/.env`, then
`make deploy`. Kubernetes (k3s): `deploy/k8s/waypoint.yaml`. Configuration,
backups, reverse proxies and upgrades are in [docs/DEPLOY.md](docs/DEPLOY.md).

## Development

```sh
make dev     # Go API on :8080 + Vite on http://localhost:5173 with live reload
make test    # go vet, go test, TypeScript typecheck (CI also runs -race)
make check   # test + lint (golangci-lint, gosec) + govulncheck; CI runs the same
make e2e     # Playwright browser checks against a throwaway server (e2e/); E2E_LOOKS=one for a quicker pass
make cli     # cross-compile the wp CLI into bin/cli (served at /cli)
make sast    # gosec (advisory)
make ops-lint  # actionlint, kubeconform, shellcheck, hadolint (via docker)
make help    # everything else
```

Tooling (Python equivalents in brackets): `go vet` + golangci-lint
(`.golangci.yml`) [ruff/mypy], `gofmt` [black], gosec [bandit], govulncheck +
`npm audit` [safety], `tsc --noEmit` [mypy], pre-commit [pre-commit],
Forgejo Actions (`.forgejo/workflows/ci.yml`) + Renovate (`renovate.json`)
[CI + Dependabot]. First run: `make tools` (pinned Go binaries) and
`pip install pre-commit && pre-commit install`, which installs the commit
hooks and a pre-push hook running CI's lint job on what the push changes
(`make prepush`: golangci-lint, actionlint and kubeconform, web typecheck and
prettier) so a lint failure costs seconds here, not a turn in CI's queue.

New dependency versions must be 7 days old, so a hijacked release is likely
caught by others first (WP-844). `go run ./cmd/dep-age` (CI's lint job and
the pre-push hook) checks every version a change adds to `go.sum`, a
`package-lock.json` or a pin `renovate.json` tracks (the Makefile's tools,
pre-commit's prettier, opencode's version) against the npm registry's publish
time and Go's checksum database. A security fix that can't wait goes in
`.dep-age-waivers` with its reason.

Waypoint's own rules run as `make wplint` (part of `make lint`, the pre-push
hook and CI's lint job; WP-707): `cmd/wplint` checks the Go code for what
REVIEW.md and AGENTS.md call defects that an AST can decide (the database
from internal/api, side effects inside `Service.write`, an unused `*Actor`,
SELECT columns against Scan arguments, model schemas missing from the
strict-mode test, `time.Now` where `s.now` is the clock, forge statuses read
without their creator, rename-following protected-path diffs, tests that
inherit the real server's settings), and `wplint repo` refuses an edit,
rename or removal of a merged migration and a new `page.waitForTimeout` in
e2e (an edited or moved one isn't new), both against the merge base with origin/main. Each rule's doc names the
escape that motivated it. A deliberate exception is waived on the line above
or at the end of its line with `//wplint:ignore RULE why` (`// wplint:ignore
e2e-wait why` in e2e, `-- wplint:ignore migrations why` added to a
migration whose edit changes only comments); waivers are counted with the
`#nosec` and `//nolint` ones (`wp report ci suppressions`).

Known gaps (all verified, not guesses):
- **eslint is dormant.** typescript-eslint v8 hard-errors on this repo's
  TypeScript 7 and has no TS >= 7.1 support yet
  ([upstream issue #10940](https://github.com/typescript-eslint/typescript-eslint/issues/10940)).
  `web/eslint.config.mjs` is ready; re-enable `npm run lint` in CI and the
  pre-commit hook (recipe kept as a comment in `.pre-commit-config.yaml`)
  once the parser supports the toolchain. `tsc --noEmit` covers types meanwhile.
- **gosec is built specially.** Upstream gosec v2.29.0 bundles an x/tools
  that can't read Go 1.27 export data, so `make tools` builds it against
  x/tools v0.52.0. It is gated in CI; accepted findings carry
  `#nosec <rule> -- reason`.
- **Renovate doesn't run.** No workflow or bot runs `renovate.json`, so
  dependency updates are made by hand or by agents. Its regex managers still
  tell dep-age which pins to check, and it already asks for 7-day-old
  releases (`minimumReleaseAge`) for the day it runs. Docker base images
  float on their tags until something keeps digest pins current.

Running it: [docs/DEPLOY.md](docs/DEPLOY.md). How each feature works, the
architecture, decisions, plans and the domain glossary are in Waypoint
project WP: feature notes (`wp design ls --kind feature`, `wp design show
N`; Integrations is note 328, Architecture 371), decisions
(`wp design ls --kind decision`), pages (`wp page ls`) and the glossary
(`wp define TERM`).

## License

Waypoint is free software under the GNU Affero General Public License,
version 3 only ([LICENSE](LICENSE)). You may run, change and share it; if you
run a changed version as a service for others, you must offer them its
source.
