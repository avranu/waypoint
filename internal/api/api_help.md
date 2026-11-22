# Waypoint API (v1)

Base: `/api/v1`. JSON in and out. Authenticate with a personal access token
(create one under Settings → API tokens): `Authorization: Bearer wpt_…`.
Read-only tokens can't change anything. A token limited to chosen permissions
(Settings → API tokens → Custom) may use only the routes they cover; others
answer 403 naming the missing permission. `GET /me` lists them under `token`.

Issues are addressed by key (`WAY-12`). Wherever a request names something,
use its human name: project keys, status names (or a category such as
`done`), usernames (or `me`), sprint names (or `active` / `next`), label
names. Unknown JSON fields are rejected so typos surface. Errors are
`{"error": "…"}` with a 4xx status; a 502 says something Waypoint calls
(the forge, an assistant) didn't answer, and is worth trying again.

AI agents can also use the MCP endpoint at `/mcp` (streamable HTTP,
stateless, same bearer token).

## Finding issues

    GET /issues?q=<query>&project=KEY&limit=50&offset=0
        &format=text            one compact line per issue
        &fields=key,title,status   only these fields
        &description=1          include descriptions
        &near=KEY               rank words matched in this project first (orders, never filters)
    GET /issues/{key}           full detail (?format=text for a readable summary)
                                written: what was live when it was written (WP-460) {environment, version,
                                commit, live_version, live_commit, releases_since}; absent when nothing was deployed
    GET /help/query             the query language

## Changing issues

    POST  /issues                 {"project","title","type","description","status","priority",
                                   "parent","sprint","assignees":[],"labels":[],"points",
                                   "start_date","due_date","template"}
    PATCH /issues/{key}           any of the above, plus "add_assignees", "remove_assignees",
                                  "add_labels", "remove_labels", "after"/"before" (rank next to
                                  another key), "comment" (saved with the change),
                                  "append_description" (Markdown added to the end in the same
                                  transaction, so concurrent edits aren't lost), "intent" (what
                                  it's for in a sentence or two, apart from the description;
                                  an agent's wording shows as a suggestion until a person keeps
                                  it; an agent can't change a person's). Use null to
                                  clear parent, sprint, points, dates. Assigning someone outside
                                  the project shares the issue with them (project editors only).
    POST  /issues/{key}/transition {"status": "In Review", "comment": "optional, same transaction"}
    POST  /issues/{key}/publish   publish a draft (people only; see Drafts below)
    POST  /issues/similar         {"project","parent","title","description"} before filing: open issues it
                                  reads like, best first [{key,title,status,project,score,confidence,
                                  same_project,sibling,same,claimed_by,reason}]; "same" = likely this one
                                  again: same project, not a sibling, not named in the text, and the
                                  same title or nearly the same words. Nothing is filed
    POST  /issues/bulk            {"issues": ["WAY-1","WAY-2"], "patch": {…}, "descendants": true}
    POST  /issues/{key}/move      {"project": "OTHER"}   moves the whole subtree
    DELETE /issues/{key}?cascade=true
    POST  /issues/{key}/comments  {"body": "markdown, @mentions notify"}
    POST  /issues/{key}/links     {"url": "https://github.com/o/r/pull/3"}
    POST  /issues/{key}/relations {"kind": "blocks|blocked_by|relates|duplicates|duplicated_by|follows|followed_by|supersedes|superseded_by|tension", "issue": "WAY-9", "subkind": "fix|expansion"}
          subkind narrows follows (fix: mends what the other got wrong or missed; expansion: more of it); again with
          another subkind changes it. Relations carry basis (person|agent|automation|inferred; absent: made before
          Waypoint recorded it), created_by, created_agent and score (how sure Waypoint was, where it measured it).
          Returns {kind, found}: relates between a task and one done before it was filed is recorded as
          follows/followed_by, found "after"; a sub-task of the done one stays relates
    GET   /issues/{key}/relations/suggested  open issues to link {suggestions:[{key,title,kind,confidence,reason}]}:
          dependencies the text states, the same title, much the same words; nothing is linked. What a person is
          shown is recorded, and relating the pair later accepts it; a ranker fitted daily on those decisions orders
          them once it beats the fixed blend on the latest month
    POST  /issues/{key}/relations/suggested/dismiss {"issue": "WAY-9"[, "undo": true]}  a person says a suggestion
          isn't related: it isn't suggested between the two again, from either end; undo takes that back (people
          only, not agents)
    POST  /issues/{key}/rewritten {"repo": "owner/name", "prs": [{number, lines, share, at}]}  other tasks whose
          recent work the change for {key} replaces, from the pull requests that last touched those lines (wp ship
          sends it and prints `wp relate` commands for the result): [{key,title,pr,...}]; leaves out the task itself,
          its parent and subtasks, tasks already related to it, tasks you can't see, and small overlaps; nothing is
          linked
    PATCH|DELETE /comments/{id}    your own comments
    DELETE /issues/{key}/relations/{id} | /links/{id}
    POST  /issues/{key}/links/refresh  re-read PR/issue state from the forge
    GET   /issues/{key}/forge-mentions[?all=1]  PRs, commits and forge issues naming it without being its work
          (verb: follows, part_of, relates, ...; places: body, comment, ...); all=1 adds linked and edited-away ones
    PUT|DELETE /issues/{key}/shares/{username} {"role": "editor|viewer"}
    PUT|DELETE /issues/{key}/watch
    GET   /issues/{key}/history | /comments | /time
    GET   /issues/{key}/worklog   its work log (WP-285): {summary, entries}; entries are adversarial review
                                  rounds (state, found/fixed/rejected, items, pushed; audit: the audit it
                                  used, audit_alt: why another model did it instead of Grok, audit_note: why it
                                  fell back to its own prompt), notes, agent runs, pull
                                  requests, review findings and review-comment verdicts, oldest first
    POST  /issues/{key}/worklog {"body", "tag": "found|fixed|rejected|"}   add a note (who may comment)
    POST  /issues/{key}/review-report {"report": JSON text | "none": true, "summary"}   from the task's own agent
                                  session: what the adversarial review it ran itself found, as the findings of its
                                  review round before the pull request (WP-1320). report is {"summary", "findings":
                                  [{title, path, line, severity: critical|high|medium|low, problem, outcome:
                                  fixed|rejected|open, note}]}; none with a summary says it raised nothing. Returns
                                  {audit, round, replaced, dropped, next}: the audit holds the findings (mark them
                                  with POST /audits/findings), and the round is then checked and scored at wp ship
    GET   /worklog/stats?project=KEY&days=30   what adversarial reviews found, per project and in total
    GET   /worklog/triage?project=KEY&days=30&rows=1   review triage (WP-692): per rule, what it decided and how it
                                  turned out; what skipping avoided; rows=1 adds each assessment with its
                                  features and labels
    GET   /worklog/stakes?project=KEY&days=14&rows=1   review rounds by stakes tier (WP-1231): barred and
                                  failed audits, free re-asks, slow hand-overs, Sonnet reviews and their
                                  cost, findings, useful PR findings, escapes; per model; rows=1 adds rounds
    POST  /issues/{key}/attachments   multipart "file", or a raw body with ?name= (25 MB max)
    POST  /attachments?target=page:12 same on anything: target is WP-12, issue:WP-12, page:12|PL3 (?project= for
                                      a bare code), design:12 or comment:40; multipart "file" (+ "caption"), or a
                                      raw body with ?name=&caption= → {id, url, kind, markdown, …}; ?review=1 marks
                                      a screenshot taken for whoever reviews the work (wp shot; Catch up shows it first)
    GET   /attachments?target=…       the files on a target (no bytes)
    GET   /attachments/{id}/{name}    images and video in place (Range works), text as text/plain, the rest downloads
                                      (?download forces it); sandboxed, never sniffed
    PATCH /attachments/{id}           {"caption"}        DELETE /attachments/{id}   whoever can edit the target
    POST  /issues/{key}/ai/estimate   suggested points {points, confidence, reason, model}
    POST  /issues/{key}/ai/subtasks   suggested sub-tasks {subtasks: [{title, points}], model}
    GET   /issues/{key}/prep          the task's prep (WP-212): {state, model, notes: {files, symbols, tests,
                                      related, terms, questions}, dropped, held_out, stale}; ?shown=1 from an agent
                                      taking it records that it was shown; a held-out one has no notes for agents
    POST  /issues/{key}/prep          a code bundle from a checkout {repo, commit, machine, files: [{path, lines, why,
                                      symbols: [{name, kind, start, end}]}], bodies, tests, docs} (editors); the
                                      prep model reads it in the background
    GET   /prep/due?machine=NAME      tasks your runner on NAME should prepare: [{key, project, dir}] (people)

Dates accept `2026-10-20`, `today`, `tomorrow`, `+3d`, `2w`, `friday`.

## Agents (see GET /api/v1/agent)

Agents identify themselves with an agent token or `X-Waypoint-Agent: provider/model`.
An issue's `agent` field says who may pick it up (WP-93): empty or `anyone` (the default for new
issues) is Anyone: people, and agents through `next`, after the work delegated to them; `none` is
Humans only: `next` never hands it out; `any`, `claude`, `haiku 5.5`, … delegate it to those agents
(`PATCH /issues/{key} {"agent": "…"}`, `POST /issues {"agent": "…"}`; an agent's own new issues are
for itself unless it says `anyone`). Search: `is:anyone`, `is:human`, `is:agent` (delegated),
`agent:anyone|none|any|claude`.

    GET  /agent                       short index; GET /agent/help/{topic} for detail
    POST /agent/next  {"project","repo","step","query","peek","delegated"}   claim the most urgent task, with its comments (text; ?format=json);
                                      "delegated": only work delegated to agents, never Anyone's (for tools that start work unasked)
    POST /agent/batch (same)          next + related tasks, claimed together
    POST /agent/done/{key} {"comment","usage":{"input_tokens","output_tokens","cost_usd","model"}}
                                      a task never taken (not delegated) is taken first, as take does, then finished
    POST /agent/release/{key} {"comment"}   POST /agent/usage/{key} {"input_tokens",…}
    POST /agent/decline/{key} {"comment"}   cancel a task filed from another repo (filed_from); the filer hears why
    POST /agent/take/{key} {"cross_project"}   take a named task (even if humans only; an epic with open sub-tasks for agents hands out the first one the agent can take): assigns it to the agent, In Progress, clock starts; cross_project is kept on its history as why it was taken from outside the session's project
    POST /agent/pause/{key}           idle: stop the clock on a held task, keeping it   POST /agent/resume/{key}
    GET  /agents                      agent identities seen at work on what you can see, and your agent tokens
    GET  /agents/activity?agent=SPEC&project=KEY&days=30   what agents did: totals, per agent, per day,
                                      review bots' models, latest runs and their PRs (?format=text: a summary)
    GET|POST /workflows (?project=KEY; {"project","name","steps":[{"name","agent","instructions"}]})
    PATCH|DELETE /workflows/{id}

The CI board (WP-225): Forgejo Actions on the attached repos, with ETAs:

    GET  /ci                          what runs, waits and finished in the last day, and when it will all
                                      be done (?format=text: wp ci status; &repo=OWNER/NAME lists one repo's)
    PUT  /ci/pools/{forge} {"runners":[{"name","labels":[…],"capacity"}]}   the runner pool the ETAs use
                                      (admins; no runners: guess it again)
    GET  /ci/history                  how CI fared over time, from every finished run and job kept (WP-490):
                                      per day or week jobs, failures, runner time, runner wait, full-run
                                      time, merges and time per merge; the last 7 days against the 7 before;
                                      each full-tier job's p50 now and before (?repo=OWNER/NAME, ?days=N
                                      up to 730, default 30; ?bucket=day|week; ?format=text: wp ci history)
    GET  /ci/tests                    from the backend test jobs' evidence (WP-704): flaky tests (passed and
                                      failed on one commit, with a score), the slowest tests and packages,
                                      tests failing on the default branch (?repo, ?view=flaky|slow|failing,
                                      ?days=N up to 30, ?package=P, ?limit=N; ?format=text: wp ci tests)
    GET  /ci/coverage                 a commit's coverage, shards merged: per package, or ?path= a directory,
                                      package or file; the default branch's trend (?repo, ?sha=PREFIX;
                                      default the default branch's newest; ?format=text: wp ci coverage)
    GET  /ci/suppressions             the default branch's #nosec, //nolint and //wplint:ignore comments:
                                      count by rule, added and removed (?repo, ?days=N default 30, ?all=1
                                      lists them; ?format=text: wp ci suppressions)

The escape ledger (WP-705, WP-ADR68): bug fixes traced to the change that introduced them.

    GET  /escapes/stats               escapes per week, median lifetime, stage found and detector split, reopens, proofs,
                                      fixes of fixes (?project=KEY, ?repo=OWNER/NAME, ?days=N; ?format=text: wp escapes)
    GET  /escapes                     the rows, newest first (same filters, ?key=KEY-N, ?limit=N; ?format=text)
    GET  /escapes/files?repo=O/N      per-file counts that wp code risk scores with, and the repo's total
    GET  /escapes/due                 merged bug fixes not traced yet (?machine=NAME, ?repo=O/N, ?limit=N; runners)
    POST /escapes/traces {pr_id, machine, tried_merge, commit, landed_at, method: szz|context|none, lines, dropped, note, introducers, files}
                                      a runner's trace of one fix
    GET  /escapes/proofs/due          traced fixes not proved yet, with the blamed change (WP-709; same params, runners)
    POST /escapes/proofs {pr_id, machine, commit, base, outcome: proven|passes|no_build|head_fails|no_test|unsupported|none,
                          tests, failed, intro, intro_check: confirmed|passes_at_intro|fails_before|no_build, note, seconds}
                                      whether the fix's tests fail without it; a checked blame sets its confidence to 1 or 0

The queue and its controls (WP-D15; people only, except reading):

    GET  /agents                      agents seen at work on what you can see, and your agent tokens
    GET  /agents/queue?agent=SPEC&project=KEY   what next hands out, in order and why ("next"),
                                      what agents hold ("working") and what waits, with a reason
                                      (step, claimed, project_paused, backlog, hold, blocked, subtasks,
                                      review, safety); ?format=text
    POST /agents/queue/order {"key","position"|"top"|"unpin","agent","project"}   pin a task at a
                                      place (1 = next) of the queue as agent/project filter it; only that
                                      task is pinned, the rest keep the automatic order around it; pins
                                      keep one order in every queue that shows them.
                                      No position: pinned where it is. A pin dropped where the automatic
                                      order would put it, or "unpin": back to the automatic order
    POST /agents/release/{key}        take a task back from the agent holding it
    GET|POST /agents/pauses {"spec","note","sessions"}   your pauses: agents matching spec ("any" = all
                                      yours) take no new work; "sessions": true also winds down running
                                      sessions (no agents, finish the step, hand off, stop; Waypoint
                                      doesn't wake them, WP-146). DELETE /agents/pauses?spec=SPEC (none: all)
    GET  /agents/session-places       (people) per project, where your runners can start a task
                                      session: {"project_key","runner_id","runner","online","dir"},
                                      the checkout you last worked in on that runner's machine
    POST /agents/start/{key}          (people) a fresh task session for it there, as wp start KEY
                                      --bg would: {"key","job","place"} (the job is a session job)
A project's owners stop agents taking new work there with settings.agents_paused.
A project's settings.audit_prs (off by default, WP-355) has Waypoint audit its members' pull requests when they open and when
they are marked ready at a new head; see "Audits" below and WP design #353.

Autopilot (WP-ADR9, WP-ADR72; people only): in a project with autopilot on, your runner starts
sessions of autopilot's own; once one has finished its task, Waypoint restarts it in its tmux pane
with the next task as its prompt (or clears it and pastes the task's wp next answer); limits pause
it. Your own sessions (ones you started or typed in) are never moved on (WP-1319), except by
POST /autopilot/sessions/{id}/start.

    GET  /autopilot                   your defaults ("defaults" as set, "effective", "waypoint"), each
                                      project you can work in {"enabled","limits","effective","steps"},
                                      the readings the limits use ("meter") and "unreviewed" summaries
    PUT  /autopilot {"limits","merge","reset","quota_off_until"}   your defaults; limits: max_hourly_usd, week_ahead,
                                      week_max, week_end_hours, five_hour_max (0 turns one off), merge (auto, always,
                                      never), grace_seconds, review (true: an adversarial review round in the
                                      session before it moves on, after /compact), review_prompt (one line;
                                      "" for the default), review_skip (on: low-risk changes skip the review,
                                      shadow: record only, off: never skip), review_text (the reviewer's brief for
                                      every adversarial review round, up to 12000 characters; "" for the default),
                                      review_model (who reviews a round: auto, session, or a model as cli/model,
                                      e.g. gateway/MODEL, claude/sonnet); "merge": true keeps the limits not given;
                                      quota_off_until (RFC 3339, up to a week ahead) turns the week and five-hour
                                      limits off until then, a past time back on
    GET  /autopilot/review-models     what review_model can be: {modes, gateway, runners} as {value, label,
                                      note}, and the default brief (default_text, max_text) (WP-1320)
    PUT  /autopilot/projects/{key} {"enabled","limits","merge","reset"}   one project's (limits it
                                      doesn't set inherit your defaults)
    POST /autopilot/steps/{id}/stop|now   stop what it was about to do with a session, or do it now
                                      (skips the grace and a merge wait, never a limit)
    GET  /autopilot/steps/{id}/wanted?job=N   (runners) {"wanted"}: the step still waits for that job
    GET  /autopilot/summaries?project=KEY&all=1&before=ID&limit=N   what agents said after finishing
                                      a task (every session), unreviewed only unless all=1; "asks":
                                      "waiting on you" lines no request covered (filed as "request")
    POST /autopilot/summaries/reviewed {"ids"|"all","project","unread"}   mark them read (or unread)
    GET  /autopilot/text              its wording for every project: {prompt, done} as admins set
                                      them (else the default), default_prompt, default_done,
                                      custom_prompt, custom_done (WP-308)
    GET  /projects/{key}/autopilot-text   as that project uses it: its own settings.autopilot_prompt
                                      and settings.autopilot_done (PATCH /projects/{key}) win;
                                      inherited_prompt, inherited_done are what applies without them
    GET  /stalled-jobs/policy         when sessions are told of stalled background jobs, for every
                                      project (WP-1045): {policy, default, custom}; policy is {off,
                                      min_age, quiet, skip_timeout, early, cold_wake, fixed}, in minutes
    GET  /projects/{key}/stalled-jobs   as that project uses it: its own settings.stalled_jobs
                                      (PATCH /projects/{key}; null inherits) wins; inherited is what
                                      applies without it
    PUT  /admin/stalled-jobs {"policy"}   (admins) set it for every project; null restores the default
    PUT  /admin/autopilot-text {"prompt","done"}   (admins) word it for every project; "" restores
                                      the default. The prompt fills in {key} {title} {done} {next}
                                      (wp next's answer) and must name {key} or {next}; the note at
                                      the Stop that finished a task fills in {key} and {status}
    GET  /projects/{key}/sandbox      its sandbox levels: level (runner jobs), interactive_level, can_edit
    GET  /sandbox/machines            your runners' (an admin: all) default_level, paused (kill switch) and
                                      helper: what its sandbox helper last reported, ready, problem
    GET  /sandbox/key                 the public key sandbox helpers pin: key, fingerprint
    GET  /sandbox/log?project=KEY&runner=ID&limit=N   sandbox changes, newest first; "current": revertable
    Sandbox writes are the web app's only (no token): PATCH /projects/{key}/sandbox, PATCH
    /sandbox/machines/{id}, POST /sandbox/log/{id}/revert, POST /me/reauth {"password"}
Issue fields: "agent", "category" (feature, bugfix, improvement, refactor, docs, test, research,
design, ops, security, chore), "workflow" (name or id), "step"; read-only "reporter_agent",
"claimed_by", "claimed_until", "claimed_at", "agent_order" (set when pinned: its rank among pins), "agent_ahead" (pinned: how many automatically ordered tasks go before it), "claim_worked" (seconds) and "claim_working_since" (null
while paused), and "agent_runs" on GET /issues/{key}. An agent's clock (a run, or its own timer) stops
by itself after 24 hours.
Requests count as an agent's when they carry an agent identity or come over /mcp; agents can't lock
glossary terms, approve or delete design notes, change approved ones directly, review, or edit and
delete people's comments.

## Agent sessions (WP-D16)

One run of a coding agent on one machine, kept by the CLI's hooks (`wp hook claude`), never by the
model. Keyed by (person, machine, the CLI's session id), so a resumed session is the same one.
States: working, waiting (for you), idle, ended; a session not heard from in 30 minutes reads as
idle, in 24 hours as ended. Prompts are only shown to their owner (and admins).

    GET  /sessions?user=NAME&machine=&repo=owner/name&branch=&project=KEY&state=working|waiting|idle|ended|active
                 &agent=SPEC&limit=50&offset=0   {sessions:[…], total, next}  (next = the next offset)
    GET  /sessions/{id}              the summary: doing (title, task, last prompt, last update), done (issues
                                     closed and created, comments, pages, pull requests, commits), deferred
                                     (issues filed into backlog or To Do, snoozed requests), waiting (open
                                     requests), requests, runs on issues, timeline
    POST /sessions/hook {"events":[{"event":"Stop","session_id","machine","cwd","repo","branch",…,"at":ms}]}
                 what the hooks send: one call for a batch (queued events first); events are SessionStart,
                 UserPromptSubmit, Notification, PermissionDenied, PreToolUse and PostToolUse (AskUserQuestion),
                 Stop, SubagentStart, SubagentStop, SessionEnd, PostCompact, ConfigChange, InstructionsLoaded
                 and PostModelSwitch (WP-1468: what the session runs with; see snapshots below). An event may
                 carry "drops":{"reason/event":N} (what the CLI's outbox dropped) with "drops_id" (a random id,
                 at most 64 characters): a report sent again with the same id is counted once. Answers {session, deliver:[requests], text}:
                 at Stop and SessionStart, the decisions made since the agent asked (marked delivered),
                 and messages queued for it (messages:[{id, body, from, agent, automation_id, …}]; agent:
                 the agent that wrote it, itself or as the automation's rule, WP-560). A Stop carries "pending":{tasks:[{type,status,
                 description}],crons:[{schedule,recurring,prompt}]} (Claude Code's background_tasks and
                 session_crons): with both empty, nothing delivered and no open request, the agent has
                 finished (session.finished). A Stop that set off a follow-up answers await: N (seconds).
                 A Stop also carries "cache_ttl" ("1h" or "5m", the prompt-cache tier its last calls wrote);
                 a session shows cache:{ttl, decision: warm|cool|compacting|compacted|failed, note, pings, at},
                 what Waypoint did with its cache at its latest stop (WP-192). A prompt starting "Waypoint
                 keep-warm" is a keep-warm ping: its prompt and stop record nothing of a turn.
    GET  /sessions/{id}/snapshots    what the session ran with: one row per SessionStart, PostCompact and
                                     ConfigChange (model, effort, permission mode, git, memory, inventory by
                                     name), oldest first; a null column is one the hook did not carry, and
                                     "missing" says why. Owner and admins only (a colleague gets 403). Model,
                                     effort and permission-mode changes between snapshots are in /observed;
                                     a session keeps its first snapshot and its newest 99
    GET  /sessions/{id}/observed     the changes the hook saw (model, effort, permission_mode, config_change,
                                     instruction_file), oldest first, each with prev and source. Owner and
                                     admins only
                 Admins see what was dropped and counted in `GET /server/status` as hook_drops:[{user,
                 machine, reason, event, dropped, first, last}] (wp server status).
    POST /sessions/hook/await {"session_id","machine","wait":8}   a Stop hook waiting for a follow-up's
                 message: {session, messages, text} (text empty: none came)
    GET  /sessions/pause         {pause}: your pause with running sessions included that covers this
                 agent, or null (WP-146). Every /sessions/hook answer carries the same "pause"; a Stop
                 while paused takes no decisions or messages and answers a note instead. Both, and
                 /sessions/pulse, carry "guard": the live quota guard's policy and hold (WP-932).
    POST /sessions/pulse {"machine","sessions":[{session,cost,auto,model}],"trips":[{session,rule,action,
                 cost,detail,at}]}   the hooks' spend per session (list price, the tree's total) and the
                 quota guard's trips (rule churn|session|hours|model|idle|hold, action told|stopped); a
                 stopped session is left alone and its tasks wait as after a release. {pause, guard}

Follow-ups (WP-D22): steps queued on a session that run once, when the agent truly finishes (no
background task, cron or wakeup left, no open request). Only the session's person, as a person.

    GET    /sessions/{id}/follow-up       {session, automation, state: none|armed|running|succeeded|failed|
                                          cancelled, steps:[…], held (why it hasn't run), run, runners}
    PUT    /sessions/{id}/follow-up {"steps":[{"type":"start_session","model":"opus","prompt":"…"}]}
                                          queue (replacing) and arm; runs now if the agent already finished
    POST   /sessions/{id}/follow-up/run   run it now, whatever the agent is doing
    DELETE /sessions/{id}/follow-up       remove it, cancelling its run
    GET    /sessions/current?cli_id=&machine=   one of your own sessions by the CLI's session id (and
                                          machine); without cli_id, the one X-Waypoint-Session names.
                                          404 when none is known. What `wp followup` acts on by default.

Agents (an agent identity, or MCP) are refused all four follow-up calls, reads included: there are
no MCP tools for follow-ups. `wp followup show|set|run|clear` is the CLI.

Send `X-Waypoint-Session: <the CLI session id>` (and `X-Waypoint-Machine`) with any call and what it
does (issues created or closed, comments, pages, requests, runs) is recorded on that session.

    GET  /sessions/{id}/usage        what that session used, joined from the usage tables by person, machine
                                     and CLI session id: {has_data, private, totals, models, kinds (the session
                                     and its subagents), tools, bash (command heads), refusals}. Usage is the
                                     owner's (and admins'); others get {private:true}; agents are refused

## Agents overview (WP-D16)

The first tab of the Agents page in one round trip, built from what each part of the app already has.
Everything is limited to what you may see; `agent` is a spec (as for the queue), `project` a key.

    GET  /agents/overview?agent=SPEC&project=KEY&days=7
                 {waiting:{requests, design, review, total}, sessions:[live sessions, newest first],
                  sessions_total, next:[{agent, items:[{key,title,why,position}], more}] (the head of each agent's
                  queue), claims:[issues agents hold], pulls:{open:[with ci, gate, armed], total, totals},
                  branches:[{repo, branch, pr, ci, sessions:[…]}] (pushed lately, with a pull request, or with
                  a live session), changes:{changes, next_before}, done:[issues agents closed, pull requests
                  they merged, requests resolved], environments, usage (this week; null for agents),
                  summary:{doing, done, deferred, waiting} each {count, parts:[{kind,label,count}], items:[top 5,
                  with url]}}
                 doing: sessions working or waiting for you, and issues held; done, in the last `days`: issues
                 closed by agents, pull requests merged, requests resolved; deferred: issues agents filed into
                 backlog or To Do, snoozed requests, pull requests waiting; waiting: open requests, design notes
                 to approve, agent-written text to review
    GET  /changes?kind=issue|page|glossary|design&project=KEY&agent=SPEC&before=MS&limit=20
                 {changes:[{kind, verb, ref, title, url, project, by, agent, at}], next_before}  newest first;
                 pass next_before as before for the next page

## Requests (WP-D16)

Something an agent asks a person while it keeps working. Kinds: question (free text or choices),
change (file edits a hook or rule stopped the agent making), command (cells to run), permission (a
refused tool call; the hooks filed these until WP-232, which logs refusals instead: GET /refusals), action
(a step only a person can do). Numbered per project
(R-12; outside a project they are personal: me:R-3). Reference one as `R-12` (your session's, your own,
or the only one), `KEY:R-12`, `me:R-3` or its numeric id. States: open, then answered, approved,
denied, done or dismissed; approved changes go applying, then applied, failed or conflict (the files
changed since the agent made the change: nothing was written); withdrawn (the agent took it back).
A person decides: agents and read-only tokens can't answer, approve, run or snooze; an agent's done or dismiss
withdraws its own request, its note kept as the reason. Project requests
are decided by the person they are for and the project's editors; the person gets an in-app and web
push notification.

    GET  /requests?state=open|answered|decided|snoozed|all|STATE&mine=1&session=ID&project=KEY|me
                 &kind=&agent=&limit=50&offset=0   {requests:[…], total, next}; answered = decided, agent not told yet
    POST /requests {"kind","title","body","project","issue","questions":[{"question","choices":[{"label"}],"multi","default"}],
                 "files":[{"path","base_sha","base","content","delete"}],"message","cells":[{"command","dir"}],
                 "tool","tool_input","reason","tool_use_id"}   → the request + "new" (false: the same permission request
                 within 10 minutes). Change files carry the old text (base) and its git blob (base_sha, "" for a new file); the
                 diff is computed here from them.
    GET  /requests/{ref}?wait=SECONDS   one, with diffs and every cell run; holds an open one up to 25 s until decided.
                 An agent reading its own decided request counts as being told; ?peek=1 only looks. For a person,
                 refused: what the classifier refused in its session in the hour before it was filed (at most 5).
    POST /requests/{ref}/decide {"decision":"answer|approve|deny|done|dismiss","answers":[{"choices":[],"text"}],"text","note",
                 "apply_path":"runner|pr","rule"}   answer questions; approve/deny changes and permissions; done a command or action.
                 rule: approving a permission request also adds this allow rule (one of its rules, or another that
                 passes the same checks) to rule_repo's .claude/settings.json by pull request (WP-233).
                 Approving a change applies it: apply_path runner (the person's runner on the session's machine, in its
                 worktree, committing there) or pr (a new branch, one commit, a pull request through the forge; needs
                 editor rights); by default the runner if one is online, else a PR.
    POST /requests/{ref}/snooze {"until"|"minutes"}   hide an open request until then (none: bring it back)
    POST /requests/{ref}/withdraw {"note"}   the asking agent (or person) takes back an open request; note: why, for the person
    POST /requests/{ref}/cells/{n}/run {"command"}   run a command cell on the person's runner on the session's machine
                 (the command may be edited first); output (256 KB), exit code and time are saved, runs are kept
    GET  /refusals?project=KEY&agent=SPEC&days=30&session=ID&before=ID&limit=50   the refused-calls log (WP-232,
                 ADR115): each tool call the auto-mode classifier refused (the PermissionDenied hook), newest first:
                 {refusals:[{id, at, last_at, repeats, tool, command, reason, agent, project, session, issue, ran_at,
                 request, rules:[{rule, covers}], rule_repo, rule, rule_pr, rule_reason}], counts:{refusals, calls,
                 sessions, ran, tools}, rules:[{rule, covers, tool, repo, refusal, refusals, calls, sessions, ran, added,
                 pr}] (first page), more}. repeats: the same call refused again in the session within 10 minutes; ran_at:
                 it later ran there. Visible as requests are. A row's rules: WP-233's narrow allow rules for it (none
                 once one was picked); the page's rules: those covering the period's calls refused twice or more, or
                 allowed by hand, by sessions then calls (at most 5).
    POST /refusals/{id}/rule {"rule"}   a person adds an allow rule for a refused call to its session repo's
                 .claude/settings.json by an automerge pull request (WP-233, ADR114); the rule passes the same checks
                 as a suggestion; needs editor rights on the repo's project. → the row: rule + rule_pr, or rule_reason.
    GET  /waiting                    what waits on you: {requests:[open for you], design, review, pulls, total};
                                      closes: agents' closes a person may review (not in total)

## Provenance and review (WP-D11)

Read-only fields say which agent wrote what ("" = a person) and whether a person has reviewed it:
issues "text_agent", "unreviewed", "resolved_agent", "close_unreviewed" (+ "text_by", "reviewed_by",
"resolved_by", "close_reviewed_by" on GET /issues/{key}); comments "agent", "unreviewed", "reviewed_by"; pages "created_agent",
"text_agent", "text_by", "unreviewed", "reviewed_by"; glossary "updated_agent", "unreviewed",
"reviewed_by"; time entries "agent". ?format=text output spells it out for agents.

    GET  /review?project=KEY&format=text        agent-written content, and agents' closes of issues
                                      (type "close"), no person has reviewed
    POST /review/{issue|comment|close|page|glossary}/{ref}   mark reviewed (people only): an issue key
                                      (its text, agent comments and an agent's close); for close, an issue key
                                      (the close only); for comment, a comment id; for page, a page id or
                                      code; for glossary, an entry id
    POST /review/closes {"issues":[keys]}   mark agents' closes of these issues reviewed (people only)
    GET  /catch-up?project=KEY        Catch up (WP-245): agents' unreviewed closes, oldest first, each with its
                                      summary (the agent's last comment), pull request, live stage, screenshots
                                      (review ones first) and try links (a path gets the production, else staging,
                                      environment URL) → {items, total}
    POST /issues/{key}/try-links {"url"}   where to try the change: an http(s) URL or a /path (editors)
    POST /issues/{key}/not-finished {"note"}   reopen a closed task with the note as a comment (people only)
    POST /issues/{key}/problems {"note"}   file a bug related to the task; the note's first line is its title,
                                      else "Problem with KEY: title" (people only)
    GET  /risk/{issue|comment|page|design|glossary|pull|prompt|automation}/{ref}?project=KEY   provenance
                                      risk (WP-554): {level: low|medium|high, reasons: [{factor, level, text,
                                      ref}]}, worked out from who wrote it and who checked it, no model; ref
                                      as for /review, a design note's number or code, a pull request's id, a
                                      queued session message's id (prompt; needs sessions.read), an
                                      automation's id (needs automations.read). An issue's and an
                                      automation's JSON carry it as "risk"; WQL risk:high, risk:>=medium

## Audit log (WP-D15)

What people decided: reviews and approvals (also of pull requests, on forges), delegating work to
agents or taking it back, pausing and ordering agents, settings (automations, workflows, AI models,
forges, repos) and access (members, tokens, CLI sign-ins, invites, accounts). Newest first; you see
entries in your projects and your own, admins see all.

    GET /audit?project=KEY&user=NAME&category=approval|delegation|control|settings|access
              &agent=SPEC&days=30&limit=50&before=ID   {entries:[…], next_before} (pass it as before)
    GET /audit.csv?…same filters…    the same as a spreadsheet (up to 10,000 entries)

## Pages

    GET  /pages?project=KEY|none&parent=ID|0&category=plan&q=text|CODE&issue=KEY&format=text
    POST /pages {"title","body","category","project","parent","issues":[keys],"code":"PL13"}
    GET|PATCH|DELETE /pages/{id|code}?project=KEY   PUT|DELETE /pages/{id}/issues/{key}
    DELETE /pages/{id}/codes/{code}   stop an old code leading here (it stays reserved)
    PATCH  /pages/{id} {"append_body": "…"}   add Markdown to the end, read and written in one transaction
    PATCH  /pages/{id} {"story": "…"}   a person writes or keeps the user story ("" clears; agents' is dropped, WP-190)
    POST   /pages/{id}/story   a person has the story model write a suggested story now (not over a person's; one at a time)
    GET|PUT /projects/{key}/page-codes   next free code per prefix; PUT {"PL":15} moves them up (owners)
A page carries "story", "story_agent" (waypoint/story until a person keeps it), "story_model", "story_by", "story_at",
"story_stale" (the body changed since), "reviewed_on_story" (the review approved it on its story), "session_id" and "prompt" {text, at, session_id}: what its person typed last
in the agent session that created it (only for that person and admins). An agent's long plan (1,500+ characters)
gets its story from the story model; approving the page (POST /review/page/{id}) keeps the story as the person's.
Categories (code prefix): plan PL, idea I, experiment X, reference R, note N, decision D, problem P,
feature F, bug B. A page's "code" (PL13) is numbered per project; APP-PL13 names another project's.
Codes are never reused, and "other_codes" (old category or project, retired prefixes like T7) still
resolve. A new page titled "PL13: …" takes that code. Pages outside a project are private to their author.
Decisions (ADRs) are design notes, not pages: a decision-category page is created but the response's
"notice" says so. Decision notes ("kind":"decision") have a "code" (ADR7, numbered per project; APP-ADR7
from elsewhere; ADR-0007 and any case work), accepted wherever a note is named (the path of /design/{id},
/spec?topic=, links, [[ADR7]], search). A title "ADR-0007: …" takes that code; codes are never reused.

## Drafts

POST /issues or /pages with "draft": true files a draft (WP-42): "draft": true marks it for people,
and agents can't see it anywhere (lists, search, show, next, links, relations, children, MCP, live
events) until a person publishes it. Sub-issues and sub-pages of a draft are drafts too. A draft
sets nothing off: no notifications, webhooks or automations. Agents are tokens with an agent
identity, the X-Waypoint-Agent header, MCP, an agent session (X-Waypoint-Session) or the CLI's
X-Waypoint-Agent-Env header (sent from a coding agent's shell even with WAYPOINT_AGENT=none).
    POST /issues/{key}/publish   POST /pages/{id}/publish   the draft and the drafts under it
    POST /issues/{key}/draft     make an issue (and what is under it) a draft again; not while claimed

## Text from outside

Forge issues and comments written by someone who isn't a Waypoint person (WP-279, ADR11) reach
no agent, automation or model until a person approves them. A forge login is a person's when an
admin maps it (PUT /forges/{id}/people; the forge token's own login is mapped to whoever
connected the forge). An issue from outside is a draft with "outside": true, "outside_by" (the
login) and "outside_forge"; publishing it approves it. A comment from outside, or a new text for
an approved comment or issue, waits apart until approved (approving writes it as by its author)
or discarded (the same text again stays out). People only, in the web or their own terminal:
    GET  /issues/{key}/outside    [{id, kind (comment, comment_edit, issue_edit), author, via, forge,
                                  url, title, body, was, created_at, updated_at}]
    POST /outside/{id}/approve    POST /outside/{id}/discard
WQL is:outside finds issues holding it (for people). A mapped login's text counts as unreviewed
agent text, text_agent or agent "forge/LOGIN", unless the login is person_only: agents may hold
a person's forge credentials (WP-282).
Pull requests too (WP-281, ADR14): one whose author isn't a person has "outside": true, no title
until approved, and is hidden from agents, bots and automations; reviews and comments from outside
wait apart. Approving a pull request covers its title, description and code at that head. People only:
    GET  /pulls/{id}/outside      [{id, kind (pull, inline, review, comment), author, url, path, line,
                                  title, body, head_sha, created_at, updated_at}]
    POST /outside-pr/{id}/approve    POST /outside-pr/{id}/discard (comments only)

## Agent instructions

Text Waypoint adds to a session's context by what the session is, at its start, at a sub-agent's
start and after a compaction (ADR146). An instruction belongs to a project or to none (every
project; admins). Who it is for: "scope" main, subagent or any; "models" (names matched inside the
model id; a session whose model isn't known gets only instructions naming none); "agent_types"
(globs on the sub-agent's type, "!" leaves one out); "moment" start (and again after each
compaction) or compact (main thread, only after one). A person's change stands as written; an
agent's is a pending revision until a person approves it, and only the approved text is sent.

    GET  /instructions?project=KEY               the project's and every project's, each with "pending"
    GET  /instructions/preview?project=KEY&scope=main|subagent&model=M&agent_type=T&compact=1
                                                 {text, chars, items: [{id, title, outcome, chars}]}: what such
                                                 a session would get now; records nothing
    POST /instructions {"project","title","body","scope","models":[],"agent_types":[],"moment"}
    GET  /instructions/{id}                      + "revisions" and "sent" {sent, cut, unknown_model, sessions,
                                                 subagents, compactions, last}
    PATCH /instructions/{id}                     any of the fields above; "enabled", "position" (people only)
    POST /instructions/{id}/review {"approve":true|false,"rev":N}   people only; rev 0 or absent: the newest pending
    DELETE /instructions/{id}                    people only
The hook answer (POST /sessions/hook) carries "instructions" at a SessionStart and SubagentStart.
Outcomes recorded per hook: sent, cut (over the size budget: 6,000 characters a session start, 9,000 a
sub-agent start; sent whole or not at all), unknown_model. A project turns delivery off with its
setting "no_agent_instructions".

## Glossary

Terms per project or global (no project). A project's full glossary is its own terms, its
ancestors' (an organisation is a parent project) and the global ones; nearer ones override.

    GET  /glossary?project=KEY&own=1&q=text      full (or own) glossary, or search it; ?format=text
    GET  /glossary/lookup?term=T&project=KEY     exact term/alias, nearest first; suggestions otherwise
    POST /glossary {"term","definition","aliases":[],"project","locked","links":[refs]}   project "" = global (admins)
    GET|PATCH|DELETE /glossary/{id}              entry with links and history; PATCH {"locked":true} (people only)
Locked entries can't be edited, deleted or linked from by agents. [[Wikilinks]] in definitions become links.

## Design notes and specs

What people intend: kinds goal, principle, feature, screen, decision, requirement, note; status
active, superseded or retired. A person's notes are approved as written; an agent's are proposed
until a person approves, unless written for a task a person filed ("for": "WP-177"): then it counts on
their request (`requested`; `approved_by` stays empty, as nobody approved it) and the agent's later
edits and additions apply directly. On other approved notes agents only add
(pending additions) or propose edits.

    GET  /design?project=KEY&kind=&status=active|archived|all&q=&parent=ID|0&pending=1&own=1
    POST /design {"title","kind","body","project","parent","status","code":"ADR12","links":[refs],"for":"WP-177"}
    GET|PATCH|DELETE /design/{id|code}?project=KEY   note with additions, proposals, history, links (DELETE: people)
    POST /design/{id|code}/additions {"body"}  append (pending for agents)
    POST /design/{id|code}/approve {"version"}      a person approves an agent's note; version = the updated_at they saw (409 if it changed)
    POST /design/revisions/{rid}/approve|reject|merge {"note"}   a person reviews an addition or edit
    GET  /design/review?project=KEY            what waits for a person
    GET  /spec?topic=T&project=KEY             a note's spec: intent, additions, parts, related, terms,
                                               tasks, some PRs/commits; no topic = goals/principles + index; ?format=text

## Goals and the trace to them (WP-382, WP-ADR50)

Goals are design notes of kind goal: what people should be able to do, in plain words. Work traces to
a goal through links (serves, implements, subtask of, under, …); each link says who stands behind it
(basis: person, agent, inferred = Waypoint's guess, text = a [[wikilink]]). An agent's link counts
as a person's once a person confirms it.

    GET  /goals?project=KEY                    goals with coverage, links waiting for a person, untraced open work
                                               (grounding, best candidate), drift, goals no person approved; ?format=text
    GET  /goals/gaps?project=KEY               the same, as what to look at; ?format=text
    GET  /trace?ref=WP-12|page:4|design:12|URL&project=KEY   routes from an item to its goals: hops, who made
                                               each, grounding, nearest person, human influences; ?format=text
    GET  /trace?pull=N&repo=OWNER/NAME | ?commit=SHA&repo= | ?keys=WP-1,WP-2&line=TEXT   a pull request, a
                                               commit or the tasks a line's commit names (wp trace FILE:LINE)
    POST /goals/links/{id}/confirm             a person stands behind a link (people only)
    POST /goals/links/{id}/dismiss             it doesn't serve that goal: a guess or an agent's link goes; agents
                                               can't link that pair again (people only)
    POST /goals/links/acknowledge {"ids":[…]}  stop listing these as waiting; not a confirm: each keeps counting
                                               as an agent's or a guess (people only)
    POST /goals/drift/{id}/answer {"answer":"intent"|"stop"}   a person answers a drift flag (the overview's
                                               drift_flags): "intent" records the work as wanted and ends a "stop" hold;
                                               "stop" holds its tasks and disarms the merge (people only)
    POST /goals/dismiss-match {"item":{"type","id"|"key"},"goal":ID}   stop suggesting this goal for the item
    POST /goals/suggest?project=KEY            a model proposes goals from people's untraced tasks and clearer
                                               words for flagged goals; each waits for a person (people only)
    GET  /goals/{id}/work?project=KEY&q=TEXT   open tasks not linked to the goal, likeliest first by shared words
                                               (no model); without q only plausible ones; with q any matching task
    POST /issues/{key}/goal-suggest            a model names the approved goal a task serves, or one new goal in
                                               plain words; nothing is saved until a person links it (people only)
    POST /links {"from":…,"to":{"ref":"design:ID"},"rel":"serves"}   link work to a goal ("tension": works against it;
          "relates": touches it without serving it; neither counts towards the goal trace)

## Links and search everything

    GET  /links?type=issue|page|glossary|design&id=ID (issues: key=KEY)   links both ways
    POST /links {"from":{"type","id"|"key"},"to":{"type","id"}|{"ref":"APP-12|page:4|glossary:T|design:12|file:path|URL"},"rel","note"}
    DELETE /links/{id}                         rel: relates, implements, depends_on, supersedes, tension, serves (mentions = from [[wikilinks]])
    GET  /resolve?ref=T&project=KEY&soft=1     what a [[wikilink]] points at (soft: null instead of 404)
    GET  /search/all?q=&kinds=issues,requests,design,glossary,pages,comments|logs&project=K1,K2&archived=1&limit=10
         a code in q (R-28, R28, API:R-28, ADR7, WP-PL3, APP12, page #60) finds what it names ("exact", listed first), then
         text that writes it (match: mention), then the words (match: text)
                                               grouped hits with snippets; closed issues and retired notes need archived=1
    POST /refs {text, project}                 what a text names: [{match, kind, id, key, title, state, project_key, url, external}];
         issues, requests, decisions, pages, pull requests, glossary terms; bare codes resolve in project first; codes
         naming nothing visible are left out
    GET  /graph?ref=CODE&project=KEY&hops=3&limit=40&guesses=0&types=issue,page,design,glossary,file,pull,session&open=1
         the relation graph (WP-385) around a thing, nearest first: every relation, parent, origin, link,
         page, decision, goal, pull request, file a pull request changed and agent session, read as one
         graph; distance sums each step's length (by kind; agent ×1.2, guess ×1.5) plus 0.5·log2(degree)
         for each thing passed through, so busy hubs count less; guesses (on unless guesses=0) add
         issues alike in words; each hit carries its path. format=text for lines
    GET  /graph/path?from=CODE&to=CODE&project=KEY&guesses=0   the nearest route between two (6 steps at most);
         found=false when nothing joins them

## Knowledge use (WP-D15)

Reading a glossary entry, page or design note with a token (CLI, MCP, an agent; not the browser)
is a lookup, once per 10 minutes per reader. Text naming one ([[link]], a page code like WP-D11,
a term or alias) is a mention, once per issue, comment, page, note or pull request; a glossary
entry's word to avoid written where its term belongs is recorded against the entry. Glossary
lists carry "uses" (90 days) and "last_used_at".

    GET  /knowledge/usage?type=glossary|page|design&id=ID   counts and the latest 50 uses you can see
    GET  /agents/knowledge?agent=SPEC&project=KEY&days=30  most used, words to avoid written,
                                               what agents read, items unused in the period

## Usage analytics (WP-D20)

`wp usage sync` (from the Claude Code hooks, a runner, or by hand) uploads what Claude Code sessions
used, read from their transcripts; `wp statusline` uploads the subscription quota. One person's
data: they and admins read it back, never agents or project members. Uploads are idempotent by
chunk id (one per transcript file range), so repeating one counts it once; negative counts are
differences (a message written again with more tokens). Cost is API-equivalent, from a price table.

    POST /usage/ingest {"machine","version","chunks":[{"id","messages":[{h,s,repo,model,kind,n,in,out,cw5,cw1,cr}],
                        "tools":[{h,s,repo,model,kind,tool,detail,calls,errors,refusals,prompts,bytes}],
                        "sessions":[{s,cwd,repo,branch,version,title,first,last,compactions}],
                        "refusals":[{t,s,repo,model,tool,detail,reason,source,excerpt}]}],
                        "inventory":[{kind,name,scope}], "parsed","skipped","unknown":{type:count}}
                                    {accepted, duplicate}; h is an hour start in ms; messages counts model
                                    responses (API messages), not turns, and prompts in tools counts permission
                                    prompts (WP-1470); at most 2000 chunks and
                                    60000 rows; unknown fields are ignored
    POST /usage/quota {"machine","session","model","context_pct","five_hour":{percent,resets_at},"seven_day":{…}}
                                    resets_at in ms; a reading the same as the last is not stored again
    GET  /usage/overview?days=30&agent=SPEC&project=KEY&machine=&session=ID&user=NAME|all
                                    the whole Usage tab: totals, daily by model, breakdowns (repos, machines,
                                    sessions, agent kinds, models), tools, Bash command heads, unused
                                    inventory, refusals, quota with pace, suggestions with their evidence
                                    (user is for admins)
    GET  /usage/trends?days=30&…    the Trends tab (WP-196), same filters: per day sums (cost, tokens,
                                    cache_read, messages, sessions, tool_calls, tool_errors, subagents, tasks)
                                    and averages (avg_context, over_200k, hit_rate, cost_per_session), the top
                                    repos' series, marks (instruction file changes, new Claude Code versions),
                                    merged pull requests, per-task usage and what goes with a task's cost
    GET  /usage/compare?at=TIME&window=7&repo=OWNER/NAME&…   the days before a moment against the days after:
                                    per session, task and day, medians with a Mann–Whitney p-value
    GET  /usage/feature?ref=KEY|VERSION&window=30&repo=OWNER/NAME&…   tasks whose runs had a release against those
                                    whose runs didn't, from what each run recorded (WP-256): had, before, partial, unknown
    GET  /usage/task/KEY            one task's usage, each run's versions, the tools it used and (admins) its experiment arms
    GET  /usage/summary             this week's tokens, cache hit rate and quota, for the overview page
    GET  /usage/breakdown?by=repo|machine|session|kind|model&limit=100   more rows than the overview shows
    GET  /usage/refusals?before=ID&limit=25     older refused calls, with their excerpts
    GET  /usage/risky?level=high|medium&kind=K&outcome=ran|refused&before=ID&limit=50   risky shell commands
                                    agents ran (WP-647): deletes, force pushes, production, disks; each with its
                                    redacted command, outcome (ran, error, refused, unknown) and the task; the
                                    first page counts the period's by level and kind
    POST /usage/transcript?machine=&session=&file=main|PATH&kind=&fp=&from=&to=&redacted=1&version=   the transcript
                                    archive (WP-1466): the body is gzip of whole JSONL lines of one file, from..to
                                    its byte offsets; answers {status: stored|duplicate|behind|gap, have, gen}
                                    (behind and gap say where to resume). Needs the sessions:report permission and
                                    a person's token, not a runner job's. The redacted flag is the client's
                                    statement that it removed secrets; the server does not check it. Optional
                                    redactions=N&redaction_rules={"secret_key":n,"known_value":n,"pattern":n}: how
                                    many values redaction blanked in the piece (counts only; absent is stored as
                                    unknown). 507 "the transcript archive is full" when the server's size cap
                                    (WAYPOINT_TRANSCRIPT_MAX_MB) would be passed: the client holds the file and
                                    retries later; nothing is stored. The cap is checked against what the index
                                    records, so deleting or moving files frees nothing: raise the cap. Also 507 "the server's disk is
                                    nearly full" when the piece would leave less free space than the admin's
                                    free-disk limit. Both carry "code":"transcript_full"; a runner job's token's
                                    403 carries "code":"transcript_job".
    POST /usage/transcript/status {"machine","pending_bytes","pending_files","dropped":{reason:count},"pass_ms",
                                    "pass_max_ms","stop","cut","off"}   a machine's upload health: the last and
                                    longest hook pass (ms; a manual pass sends neither), why the last stopped early ("" ran to the end), passes cut
                                    short so far, and off (WP_USAGE_ARCHIVE=off or a --no-archive pass; an off report changes nothing else).
                                    Fields left out keep what was reported before (unknown until reported)
    GET  /usage/machines?user=       the machines that report for you (admins: anyone) with each one's archive record:
                                    pending, dropped by reason (refused counts refused upload attempts, not
                                    files; deleted_unsent counts transcripts deleted with unsent bytes), off, pass_ms, pass_max_ms, stop, cut, last_piece_at
    GET  /usage/transcripts?session=ID&user=&machine=&gen=   the files held for a session: pieces, byte range,
                                    complete (no hole), sizes, whether every piece was redacted, and redactions (values blanked,
                                    with redaction_rules by rule; null when any piece predates the counts)
    GET  /usage/transcript?session=ID&file=main|PATH&machine=&user=&gen=   one file's JSONL, whole lines in order;
                                    a person only (an agent's shell is refused). A stored piece that fails its
                                    check after bytes went out breaks the connection instead of ending the body
    GET  /usage/warm?days=28&user=   keep-warm decisions per class of wait (WP-1000): pings, compactions,
                                    cools, what became of the sessions (used within the hour, used again,
                                    cleared, ended, open) and the shares a ping and a compaction need to pay
    GET  /usage/goal-context        admins: how much of a task's goal chain agents are shown (WP-1035): per arm
                                    (goal, chain, pruned) the share of new tasks, p_best, drawn and measured
                                    tasks, effect on drift flags, guards, and figures (done, PRs, flags, findings)
    GET  /usage/stalled?days=28&user=   quiet background jobs (WP-1045) per kind of command: spells, how
                                    their sessions were told (woke, at_stop, passive, keep_warm, cold_wake),
                                    how they ended (acted, self, withdrawn, unknown, open), the median quiet
                                    time of those that ended by themselves, and the quiet time each kind gets now
    GET  /usage/guard?days=14&now=1&user=   the daily quota guard (WP-932): per check, the points of the
                                    week the readings rose, automation's share by phase of the week (under,
                                    ahead, end, over, off, unknown), its biggest sessions and jobs, early
                                    use-what's-left starts, hours, and breach reasons; now=1 adds a check
                                    of the last 24 hours, not stored
    GET  /usage/guard/live?days=7&user=   the live quota guard: settings, the lines in force (and where
                                    each came from: set, usual, default), the baseline, a hold, automation's
                                    spend in the last hour and the trips (told, stopped, held, lifted)
    PUT  /usage/guard/settings {settings:{off,churn_usd,session_usd,hours,idle_hours,model_usd,models,
                                    burst_usd,pace}, merge, on, clear:[field|all]}   POST /usage/guard/lift
                                    {for:"2h"}   people only: the web, their terminal, or a session they
                                    typed in last (not a task session, an agent token or an automation)
    GET  /usage/prices              PUT /usage/prices {model, input, output, cache_write_5m, cache_write_1h,
                                    cache_read}   DELETE /usage/prices?model=   USD per million tokens, matched by
                                    model-id prefix (admins change them)

## Experiments (WP-254, WP-ADR53)

A/B tests of how agents work. The experiments (arms, control first; what they measure) are in code;
a run switches one on with fixed weights until its end date, its target of finished tasks per arm,
a guardrail, or a stop. Each task an agent takes gets an arm, at random and kept; no live run is the
control. Reading needs ai.manage; starting and stopping, a person who is an admin.

    GET  /experiments               every experiment: arms, default weights, params, live run with its
                                    counts (assigned, exposed by arm), past runs, and the general caveats
    POST /experiments/{key}/start {"weights":{arm:n},"params":{k:v},"projects":[KEY],"days":14,
                                   "target":40,"note"}   409 when a run is live; days 1 to 90
    POST /experiments/{key}/stop {"reason"}
    GET  /experiments/{key}/results?run=ID&scope=done|all   per arm: assigned, exposed, measured, done
                                    rate; per metric the median, mean, ratio to the control with a
                                    bootstrap 95% interval, Mann-Whitney p and Holm-adjusted p; the
                                    assignment check (SRM), detectable change, verdict, notes, caveats
    POST /issues/{key}/arms {"experiments":[key],"via":"next|take"}   (an agent's wp, after taking a
                                    task) {key, arms:{experiment:{arm,run,params}}}: the arms of the
                                    experiments wp carries out; empty for a person or a read-only token

## Objectives (WP-927, WP-ADR111, WP-ADR142)

A task with an objective is worked in rounds. A round's work ends where the task would close (wp
done, or the merge of its pull request). Waypoint then reads the round itself: it runs the check
and the commands that must keep passing in fresh checkouts on a runner of the person who confirmed the objective, and has a
model that didn't do the work judge it when the objective has a judge. That reading alone decides:
the task closes when it met the target, and otherwise opens for the next round (out of rounds
unmet, it waits for a person). What the worker's own wp objective check reports is kept as a working reading and decides
nothing. A person closing the task themselves always closes it. Four shapes: until a fixed target
("< 1s", ">= 80%", "pass"); until a judge finds nothing at or above a severity; an amount every
round against the commit the round started from ("2x", "-10%", "+5%"), never met for good; a loop
with no check. The commands run on people's machines, so an agent's objective is a proposal (state
proposed: shown, not run, not looping) until a person confirms it, and only a person changes,
steers or clears one in effect.

    GET    /objectives?project=KEY&all=1   those in effect or proposed (all: met and ended too)
    GET    /issues/{key}/objective  {kind until|each|judged|loop, check, dir, setup, measure
                                    time|value|pass, until, repo, branch, keep_passing[], judge, judge_bar
                                    critical|high|medium|low, judge_models[], pace queue|spare, every
                                    (seconds), max_rounds, diagnose_after, round, round_kind ""|diagnose, no_better, stalled,
                                    diagnosis {round,at,by,text}, next_at, state
                                    proposed|active|met|ended, phase ""|baseline|verdict|blocked,
                                    phase_why, phase_note, health proposed|working|ready|waiting|
                                    reading|blocked|stuck|met|ended, health_note, stuck_since,
                                    stuck_why, base_sha, ended_why, summary, rev, text, last (newest
                                    reading of any source), verdict (newest of Waypoint's own),
                                    checks[] (each with source agent|runner|judge|baseline),
                                    rounds[] (outcome reading|met|progress|worked|none|empty|
                                    unmeasured|loop|diagnosed, and a diagnosed one's diagnosis,
                                    diagnosis_by), findings[]}; also "objective" on GET /issues/{key}
    PUT    /issues/{key}/objective {"check","dir","setup","until","measure","better":"lower|higher",
                                    "repo":"owner/name","branch","keep_passing":[cmd],"judge","judge_bar",
                                    "judge_models":[model],"pace","every":"24h","max_rounds":0,"diagnose_after":2,"rev"}
                                    set or change; a field left out keeps its value. keep_passing:
                                    commands a round mustn't break. judge: what done means, in words. pace
                                    spare: handed out only while the week's-end plan wants more work
                                    (WP-ADR106). every: least time between rounds (default 24h for
                                    each and loop, 0 for until and judged). rev: the agent's proposal
                                    a person's change builds on.
                                    diagnose_after: rounds in a row that come no nearer before the next
                                    is a diagnose round that ships no code (0: never; 2 by default, and for
                                    objectives that existed before it; a loop has none to set)
    POST   /issues/{key}/objective/confirm {"rev"}   a person puts an agent's proposal in effect; rev
                                    is the one they read, refused if it was rewritten since
    POST   /issues/{key}/objective/go {"action":"retry|next|accept|end"}   a person steers one in
                                    effect: retry a reading that couldn't be taken; next: hand the
                                    round out now, or leave an unread round behind; accept a reading
                                    that waits because the round changed what the check runs; end it (the
                                    task stays, an ordinary task)
    POST   /issues/{key}/objective/diagnose {"text","ask","agent"}   the agent working a diagnose
                                    round (or a person who may edit the task) records why the rounds
                                    before it came no nearer, and what to try: the round ends with no
                                    reading, the task goes back to To Do, and the same round opens as
                                    a work round. ask: a person must act first, so it waits for them
    DELETE /issues/{key}/objective  takes it off, rounds and readings with it; the task's history says
                                    what it had reached
    POST   /issues/{key}/objective/checks {"ok","value","base_ok","base_value","base_sha","tree",
                                    "commit","runs","low","high","p","moved"}   what wp objective
                                    check measured on the worker's machine (time in seconds): a
                                    working reading → {check, met, message}

## CLI

    GET /cli                          install script (curl -fsSL <server>/cli | sh)
    GET /cli/{wp-<os>-<arch>|SHA256SUMS}
    POST /cli/login {"name","agent"}  device login: {device_code, user_code, verification_url, interval}
    POST /cli/login/poll {"device_code"}   {"status":"pending"} or {"status":"approved","token"}
    GET /cli/login/{user_code}        (signed in) what's asking; POST …/approve to grant a token

## Projects

    GET  /projects?match=REF         projects a key, name, repo or repo name names (+ sub-projects; matched_by says how)
    GET  /projects?repo=owner/name   projects that repo is attached to (+ sub-projects)
    GET  /projects                   POST /projects {"name","key","color","icon","settings":{"mode":"kanban|sprints"}}
    GET  /projects/{key}             statuses, labels, members, open sprints
    GET  /projects/{key}/board?q=    columns of issues
    GET  /projects/{key}/backlog?q=  sprints + backlog + epics
    GET  /projects/{key}/activity
    PATCH|DELETE /projects/{key}     (owner)
    POST /projects/{key}/look/regenerate  "Pick another": Waypoint picks a different icon, colour and background, no AI (owner)
    POST /projects/{key}/look/reset       back to the default look, which Waypoint then leaves alone (owner)
                                     the project's "look" says who chose: auto, generated, custom or plain
    PUT|DELETE /projects/{key}/members/{username} {"role": "owner|editor|viewer"}
    POST /projects/{key}/statuses    PATCH|DELETE /statuses/{id}
    POST /projects/{key}/labels      PATCH|DELETE /labels/{id}
    GET  /projects/{key}/sprints     POST /projects/{key}/sprints
    GET|PATCH|DELETE /sprints/{id}
    POST /sprints/{id}/start         POST /sprints/{id}/close {"move_to": "next|backlog|<sprint>"}
    GET  /projects/{key}/automations POST /projects/{key}/automations
    PATCH|DELETE /automations/{id}   GET  /automation-templates
        Triggers include time-based ones: {"event":"due_soon","days":1},
        {"event":"stale","days":30}, {"event":"schedule","every":"day|weekday|week|month",
        "at":"09:00","weekday":1,"month_day":1} or {"event":"schedule","every":"cron",
        "cron":"0 9 * * 1-5"} (the rule's time_zone, else the project owner's; with "if", per
        matching issue). Other triggers: manual, webhook, count_crossed {"query","op":">=|<=","n"},
        automation_finished, timer_started, sprint_created, glossary_changed, request_waiting
        {"after":"15m"}, and the event triggers of sessions, requests, pull requests, CI,
        environments and repos (see the catalog); "by":"person|agent" narrows issue triggers. Action create_issue {"template","title","body","issue_type","user","labels","due","issue_priority","agent","written_by"} (written_by: the agent whose answer the text carries, so the issue is its unreviewed text)
        makes recurring issues.
        Pull requests (WP-D14) of attached repos with bots on: {"event":"pr_updated"} (opened,
        pushed to, ready, reopened), {"event":"pr_command","command":"review"} ("@waypoint
        review" in a comment), {"event":"pr_labeled","label":"needs-review"} (empty: any label)
        and {"event":"review_requested","who":"bot|user|any","reviewer":"ann"} (the forge's bot
        account, a named person or anyone asked to review), narrowed by "pr": {"drafts","bots",
        "bases":[],"authors":["ann","*[bot]"] (only these; * is any run of characters; naming
        authors lets bots in),"skip_authors":[],"labels":[] (all of them),"skip_labels":[],
        "skip_heads":[],"paths":["web/**"] (touches one),"skip_paths":["docs/**"] (every file
        matches one: left out),"min_changed_lines","max_changed_lines" (additions + deletions),
        "repos":["owner/*"]}. Actions: review {"models":[{"provider_id","model"}],"focus",
        "max_findings" (how many are posted, worst first, 1-100, default 8; the rest are kept,
        counted in the review and hold the merge gate like the posted ones),"min_severity":"P0|P1|P2|P3","skip_files":[globs],"force"}; when steps
        follow a review the run waits for it and the step leaves Findings, Posted, Worst
        ("P0".."P3", "" for none), P0..P3 (counts), Reviewed, Status, Summary, URL, RunID, e.g.
        "only_if":"{{or .Steps.review.P0 .Steps.review.P1}}". pr_comment {"message"}, react
        {"value": "eyes|+1|-1|rocket|heart|confused"}.
    Steps (WP-D18): "actions" is a list of steps. Each may have "id" (so later steps read its
        outputs as {{.Steps.id.Name}}), "only_if" (a template that must render true),
        "on_error": "stop|continue|retry" (+ "retries" 1-5, "retry_delay": "30s"); a
        {"type":"repeat","max":3,"until":"{{...}}","steps":[...]} step repeats a group.
        Rules also take "inputs":[{"name","type":"text|number|bool|select","default","options",
        "required"}] (asked for by manual runs, {{.Inputs.name}}), "concurrency":
        "parallel|skip|queue" (schedules default to skip), "time_zone" (IANA) and "allow_agents"
        (agents may run a manual automation only when true). Templates see .Trigger (.Data, .By),
        .Steps, .Previous, .Inputs, .Issue, .PR, .Project, .Actor; functions json, untrusted,
        truncate, lower, upper, trim, join, default, contains, hasPrefix. Rules saved before
        steps existed load and run unchanged.
        New actions: ask_model {"prompt","models","schema","system"} (outputs Text, Data),
        delay {"duration":"15m"}, wait_for {"event","key":"pr:42","where":"state=passed",
        "timeout","on_timeout":"fail|continue"} (outputs Event, TimedOut), search_issues
        {"query","limit"}. A rule with long steps (ask_model, delay, wait_for, retries) is
        durable: a worker carries each run on, across restarts.
        Agent and pull request steps (phase 2): agent_task {"agents":[{"cli":"claude|codex|
        opencode|cursor","model"}],"permission":"read|edit|edit_run","prompt","branch","push",
        "base","timeout"} and run_command {"command","dir"} run on a runner of the person who last
        saved the automation (see saved_by, runners below); fix_ci {"agents"}, open_pr {"head",
        "title","body","base","draft"}, merge_pr {"when_ready","style","timeout","on_timeout"},
        wait_for {"what":"ci|gate|merged"} (or event/key/where), pr_label {"add","remove"},
        request_reviewers {"users"}, set_commit_status {"context","state"}, retry_ci, ask_person
        and create_request {"user","title","kind","choices"}, agent_activity {"days"}. Pull
        request steps take "pr" (a number, owner/repo#n; default the trigger's or the last open_pr).
        An automation shows "saved_by" (the person whose runners it uses) and, when a step runs
        on a runner, "runners": {"owner","total","online"}.
    GET  /automation-catalog         every trigger and action: name, label, group, phrase, typed
                                     params (type, required, options, default, when), outputs, what
                                     triggers provide; presets for "only if"; template functions.
                                     The editor draws its fields from this
    GET  /automations/{id}           one automation (owners also get hook_url)
    GET  /automations/{id}/runs?limit=30&before=ID   runs, newest first (editors and up)
    GET  /automation-runs/{id}       a run: status, trigger summary, event, inputs, steps (inputs
                                     after templating, outputs, log, error, durations), the rule it ran
    POST /automations/{id}/run {"inputs":{"name":"value"}}   run a manual automation; agents only
                                     if the rule allows it. Returns the run (long ones finish later)
    POST /automations/{id}/test {"inputs":{},"data":{}}   dry run against a recent matching event
                                     (or sample data): what each step would do; nothing changes
    POST /automation-runs/{id}/cancel   POST /automation-runs/{id}/rerun
    POST /automations/{id}/webhook-address   new secret address for an incoming-webhook automation
    GET  /automation-cron?expr=0+9+*+*+1-5&tz=Europe/London&n=5   next run times of a cron expression
    POST /hooks/automations/{token}  the incoming-webhook trigger (no login: the address is the
                                     secret; a JSON body up to 256 KB is .Trigger.Data; with a
                                     "secret" set, send X-Hub-Signature-256: sha256=HMAC-SHA256(body);
                                     60 requests a minute) -> 202 {"run","status"}
    GET|POST /admin/automations      admin automations (no project; they see every project's events
                                     and the system's; system triggers system_resources,
                                     runner_offline, backup_failed). Edit them with PATCH|DELETE /automations/{id}
    GET  /admin/system               free disk, database size, free memory, the transcript archive (size, pieces, cap, refusals) and the limits
    PUT  /admin/system-limits {"disk_free_mb","database_mb","memory_free_mb","transcripts_mb"}   0 turns a check off
    GET  /projects/{key}/bot-runs?limit=50   bot runs, newest first: pr, reason, status, log
                                     (each model tried: model, outcome, ms), findings, review_url
    POST /bot-runs/{id}/cancel       POST /bot-runs/{id}/retry   (again at the current head)
    GET  /reviews/stats?project=KEY&days=30   how review findings held up (WP-371): per reviewer
                                     model × strategy × prompt version and per verifier model, useful,
                                     false and unjudged with precision (90% interval), evidence drops,
                                     tokens per useful finding; verifier on against off (no project:
                                     admins only; days up to 730; ?format=text: wp pr review-stats)
    GET  /mistakes?project=KEY       recurring mistakes filed as check tasks (WP-710): key, label, issue,
                                     state (open, landed, declined), instances, recurred since the
                                     check landed (?format=text: wp mistakes)
    GET  /projects/{key}/templates   POST /projects/{key}/templates {"name","type","title",
                                     "description","labels":[],"points","subtasks":[]}
    PATCH|DELETE /projects/{key}/templates/{id}
    GET  /projects/{key}/webhooks    POST /projects/{key}/webhooks
    PATCH|DELETE /webhooks/{id}      POST /webhooks/{id}/test
    GET  /webhooks/{id}/deliveries   last 50 (14 days): event, status, error, duration_ms, response
    POST /webhooks/{id}/deliveries/{did}/replay   send that body again (new delivery id)
    GET  /projects/{key}/repos       POST /projects/{key}/repos
    PATCH /projects/{key}/repos/{id} {"sync_issues","post_comments","bots"}
    DELETE /projects/{key}/repos/{id}  POST /projects/{key}/repos/{id}/sync
    POST /projects/{key}/repos/{id}/hook   add the forge webhook again (hook_state shows how it went)
    POST /projects/{key}/repos/{id}/move {"repo": "owner/name"}   follow a repo renamed or transferred on its
      forge: the attachment, links and pull requests take the new name, replacing an attachment the move made
      under it. Waypoint follows moves itself once it knows the repo's forge id (from webhooks, discovery, sync)

## Pull requests (WP-D15)

    GET /pulls?project=KEY&repo=owner/name&agent=SPEC&state=open|merged|closed&days=30&q=text
        pull requests of repos attached to projects you can see: open ones, and ones opened,
        merged, closed or updated in the period, most recently active first; each with its
        agents, state, dates, merged_by, hours_open, pushes, comments and reviews
        {people, bots}, approvals, findings by severity, linked issues, and the agent time,
        tokens and cost on those issues; plus totals {opened, merged, closed, open_now,
        median_hours_to_merge, bot_reviews, findings, human_approvals}. agent matches as
        delegation does ("claude" matches anthropic/claude-code), or a run's model
    GET /pulls/{id}                  one with its timeline (forge history, bot reviews, agent
                                     runs, linked issues' moves), agent_time and bot_runs

## Environments and deployments (WP-D19)

    GET /environments?project=KEY&stopped=1   environments you can see: status (up, degraded,
                                     down, unknown), status_since, latency_ms, http_code, error,
                                     tls_expires_at/tls_warning, branch, commit (commit_url),
                                     version, behind, uptime_24h/30d, spark (24 hourly %), last_deploy
    GET /environments/summary        counts {total, up, degraded, down, unknown, projects:
                                     [{project, up, down, ..., down_names}]}
    GET /environments/at?repo=owner/name&sha=a,b   where commits run or ran [{name, kind, at, current}]
    GET|PATCH|DELETE /environments/{id}   with deployments (50), days (90, uptime % per UTC day)
                                     and history (48 h in 10-minute steps). PATCH any of name, kind,
                                     url, health_url, expected_status (0: any 2xx), interval_s
                                     (10-86400; 0: no checks), version_url + commit_field/branch_field/
                                     version_field (dot paths like git.commit), ephemeral, ttl_s, repo_id,
                                     lag_alert_m (minutes behind its branch before owners are told;
                                     0: never; production and staging start at 45)
    POST /environments/{id}/stop     stop checking it (dev environments are forgotten after a week)
    POST /environments/{id}/check    check now
    GET|POST /environments/{id}/deployments
    GET|POST /projects/{key}/environments   POST {name, kind?, url, ...}; kind is inferred from the name
    GET /projects/{key}/environments/{name}
    POST /projects/{key}/environments/{name}/deployments
        {commit, branch, version, status: started|succeeded|failed, url (the CI run), notes}
        what CI calls, with a token that can edit the project. started creates a deployment; a
        later call with the same commit finishes it. An unknown name creates the environment
        (production, staging, dev by name, else a preview).
    POST /projects/{key}/environments/{name}/up    {url?, branch?, commit?, ttl_s?, kind?}
        a dev environment is ready (the project's dev address fills url); renews its lease
    POST /projects/{key}/environments/{name}/down
    POST /projects/{key}/environments/{name}/vouch    {url?, health_url?, version_url?, reason?, issue?}
        let unattended agents read from its addresses (WP-ADR90): a person's call records them
        as theirs (creating it if missing); an agent's files one question for a person, whose
        "Allow reads" records them → {vouched, hosts, environment?, request?, new?}
    Events: env.status (+status, previous), env.deployed, env.deploy_failed, env.created,
    env.stopped, env.lagging (+commit, head, branch, behind_m, limit_m: still behind its branch
    past lag_alert_m minutes); env.updated is live-only. Webhooks can subscribe to env.*.
## Review gate, CI and merging (WP-D21)

    POST /projects/{key}/pulls {repo, head, base, title, body, draft, ready, issue, for, urgent, review, reason, force, head_time}
                                     open a pull request from a pushed branch (wp pr open, wp ship). review is
                                     what to do about the task's adversarial review when the caller's autopilot
                                     reviews first (WP-688): auto, review (schedule it; force: again), pr, or
                                     skip (reason says why); head_time is the head's commit time (unix s). A
                                     held pull request answers {held: what to tell the agent}; notice says what
                                     was done instead of the intent. Without review, held is a 400 error.
    GET /projects/{key}/pulls/{number}?repo=owner/name   a pull request by number: the detail above plus
                                     "shipping": {ci, checks[{name,state,url,description,has_log}],
                                     gate{enabled,severity,state,note,open,below}, findings[{id,path,line,
                                     severity,claim,state,history[{old,state,kind,by,sha,note,at}]}],
                                     mergeable (null while the forge is still checking), readiness{ready,reasons[],
                                     only_waiting,checking}, armed{at,by,agent,style,
                                     label,setting,note} (setting: armed by the repo's Merge automatically),
                                     can_merge}. States: findings open|fixed|withdrawn|dismissed|resolved;
                                     checks pending|success|failed; ci pending|passed|failed|none
    POST /pulls/{id}/check           read its threads and checks from the forge now, re-evaluate the gate;
                                     returns the detail (editors; what "@waypoint check" does)
    GET /pulls/{id}/readiness        {ready, reasons[], only_waiting, checking}: open, not a draft, mergeable, every
                                     check passed, the repo's required checks passed, gate green (if on), no
                                     person asked for changes; when armed, also reviews settled and quiet
                                     (WP-133). only_waiting: only those waits are left (a person may merge now).
                                     checking: the forge said "no" to mergeable but is still working it out after a
                                     push or a base move (WP-138); not a conflict until a read 90 s or more after
                                     the change repeats it (or 15 min pass)
    POST /pulls/{id}/merge {style, delete_branch, title, message, when_ready, head}
                                     merge a ready pull request (style merge|squash|rebase, default the
                                     repo's; delete_branch defaults to true; the head it checked is passed to
                                     the forge; with head, only if that is still its head). With when_ready it
                                     is armed instead and merged the moment it is ready. → {merged, armed,
                                     reasons[], sha}. Editors; agents only for pull requests they wrote. Audited
    DELETE /pulls/{id}/merge         take merge-when-ready off
    POST|DELETE /pulls/{id}/after/{number}?repo=owner/name
                                     merge only after pull request {number} (its own repo by default, same
                                     forge) has merged, or drop that (WP-912; wp pr after) → the detail, with
                                     merge_after[{pr_id,repo,number,url,state,why,set,mutual}]: set ones, and
                                     open pull requests of tasks blocking its tasks (no setting needed). While
                                     one is open its readiness says "merges after #N, ..." and it holds no
                                     place in the merge queue; a pair waiting both ways holds neither (mutual)
    GET /pulls/{id}/failed-checks    [{name,url,log}]: failed checks on its head with the tail (64 KB) of the log
    GET /pulls/{id}/logs?check=NAME&full=1
                                     each failed check's job log read from the forge now (Forgejo Actions,
                                     GitHub Actions, GitLab jobs), cut to the failing part: Go test failures,
                                     panics, data races, tracebacks, pytest, tsc/eslint/vet errors, Playwright,
                                     make, with the lines around them, at most 200 lines. check: the checks
                                     named that or with it in their name (passed ones too); full: the whole
                                     log. Colour codes and timestamps removed. → {pr_id, repo, number,
                                     head_sha, checks[{name, state, url, excerpt, lines, shown, cut, found,
                                     log, from: forge|kept, error}]}
    GET /pulls/{id}/wait?timeout=SECONDS&head=SHA
                                     long poll (at most 300 s; ask again): returns when its checks settle, a
                                     conflict appears, it merges or closes, it becomes ready, or the time is
                                     up. head: count only that head's checks. → {verdict: ready|merged|closed|
                                     failed|conflict|blocked|waiting, why, id, repo, number, url, state,
                                     base_ref, head_sha, merged_sha, ci, checks[], readiness{ready,reasons[],checking},
                                     armed, armed_note}. waiting: the time ran out, or only the forge is still
                                     checking whether it merges (WP-138); blocked: checks passed but
                                     review stops it (findings, changes requested, a draft). An armed pull
                                     request that is ready waits for its merge
    GET /pulls/{id}/babysit           its babysitting (WP-134): {babysit: {state: watching|working|waiting_review|ready|
                                     needs_person|resting|merged|stopped|gave_up, active, wait, round, max_rounds, model, owner, rounds[{n,
                                     sha, to: runner|session, job, why, outcome}], end_reason} or null, comments[{id: c12, kind:
                                     inline|review|comment|finding, author, path, line, sha, body, url, verdict: new|seen|accepted|
                                     rejected|partial|needs_person|fixing|fixed|verified|dismissed, note, reactions[], dealt}],
                                     can_edit}. Agents: don't poll it; the session that started it is told
    POST /pulls/{id}/babysit {model, via}
                                     start babysitting it (editors, agents too; wp ship does): Waypoint watches it until it merges
                                     and hands each round of failed checks, conflicts and review comments to a fresh Claude Code
                                     session on the starter's runner (model default sonnet). Starting one that is going returns it
    DELETE /pulls/{id}/babysit       stop it (and a round's job in progress); clears one that gave up
    GET /pulls/{id}/todo             the work packet: {checks[] (failed, with excerpts), conflict, rebase, comments[] (not dealt
                                     with, each with code: ±4 lines at the head), text}. Reads the review comments from the forge
    POST /review-comments/ack {ids, verdict, note}
                                     a verdict on review comments: accepted 👍, rejected 👎, partial 😕, needs_person 😕 (note
                                     required but for accepted; it is posted as a reply). Agents can't change a person's verdict
                                     or one that needs a person. → the comments
    POST /review-comments/fixed {ids, note, sha}
                                     they are fixed: 🎉, thread resolved, a reply only with a note (always on a relayed or body
                                     finding, or a comment copied from GitHub). With sha not yet the head:
                                     fixing 🚀 until the next push
    POST /review-comments/{id}/dismiss {note}
                                     nothing to do about it (agents must give a note)
    POST /findings/{id}/dismiss {note}   a finding stops blocking the gate (agents must give a note; 👎 on the forge does too)
    POST /findings/{id}/reopen       undo a dismissal, withdrawal or resolution
    GET /branches?project=KEY        branches pushed in the last 30 days that no merged or closed pull request
                                     has finished: {repo, branch, head_sha, pushed_at, pusher, ci, pr{id,number,
                                     state,gate,armed}}
    PATCH /projects/{key}/repos/{id} {review_gate: bool, gate_severity: "P0".."P3"|critical..low} (owners)
    PATCH /projects/{key}/repos/{id} {auto_merge: bool, required_checks: [..], review_wait_minutes: 0..1440,
                                     quiet_minutes: 0..1440, external_reviewers: [login|context]} (owners;
                                     WP-133). auto_merge arms every open non-draft pull request (not forks,
                                     release PRs, do-not-merge or wip); off takes off only its own arming.
                                     The repo list returns the same fields
## Audits (WP-348)

A model reads a repository, branch, commit, range or pull request from your runner's checkout and
reports findings: by default the one the LLM gateway picks (gateway/audit), or Grok through
Cursor on a Waypoint with no LLM gateway. It never changes anything; findings are claims until
someone checks them. Not the audit log above.

    POST /audits {target: repo|branch|commit|range|pr, repo, project, ref, base, pr, pr_id,
                 instructions, models: ["cursor/grok-4.7-high", …], comment, fix, blocking,
                 max_findings}
                                     ask for one (editors with audits.run): target empty means pr when pr or
                                     pr_id is set, range when ref has "..", else repo. branch: ref against
                                     base (default branch); commit: ref against its parent; range: "A..B";
                                     repo: the whole tree at ref. models are tried in order when one is busy
                                     or out of quota (default: the chain from GET /audits/defaults); a chain
                                     that is only busy is retried up to 3 times. comment (default yes) posts
                                     a pull request's findings on it; fix makes a task for Claude to check
                                     and fix them. max_findings (0: 15; 1-100) is how many findings are shown
                                     at a time (wp audit show, the audit's page, the pull request's comment: the
                                     rest are counted and one step away, and open ones hold a pull request's merge gate like the shown
                                     ones); it is not a limit. The model is told no
                                     number and every finding it reports is kept (the guard is 100: past it the
                                     summary says how many were cut). GET returns all of them.
                                     The asking session is told when it ends
    GET /audits?project=KEY&pr_id=N&status=queued|running|done|failed|limited|cancelled|active&limit=50
                                     newest first, each with counts {total, open, critical, high, medium, low}
    GET /audits/defaults             {instructions, models, targets, severities, states, clis}
    GET /audits/nitpicks?project=KEY&path=P&path=…&limit=10
                                     findings still open on those files from pull requests' nitpick logs
                                     (bots' comments that didn't hold a merge, WP-695) and from leftover
                                     audits with no fix task: [{ref: "41.2", path, line, severity, title, pr}]
    GET /audits/{id}                 one with its attempts [{model, outcome, error}], summary, findings
                                     [{id: "12.3", title, path, line, url, severity, problem, confirm, state,
                                     note, issue, by}], comment_url, fix_issue. limited: every model was out
                                     of quota
    POST /audits/{id}/rerun          the same audit again as a new one (a pull request's at its head now)
    POST /audits/{id}/cancel         stop one that hasn't ended
    POST /audits/{id}/fix            the task for Claude to check and fix a finished audit's open findings
    POST /audits/findings {ids: ["12.3"], state: open|confirmed|fixed|rejected, note, issue}
                                     your verdict on findings

## Runners (WP-D17)

`wp runner` on a machine registers, takes jobs and reports back, signed in as its owner (never
an agent). Only the owner's work runs on a runner. Jobs: `command`, `apply`, `agent`, `review`.

    POST /runner/register        {name, hostname, os, arch, version, labels[], allowed_dirs[],
                                  clis[{name, version, signed_in, detail}], max_jobs}
        idempotent per owner and name; returns the runner (id, online, last_seen_at, …)
        and server_version
    POST /runner/poll            {runner, running[ids], free, clis?, wait?}
                                  → {job, cancel[ids], server_version}
        held up to 30 s for a job this runner may take (claimed atomically); also the
        heartbeat: polled in the last 90 s = online. job = {id, kind, title, input, timeout,
        token?}: token is the job's own agent token (autopilot, and agent jobs that may run
        commands), deleted when the job ends; never hand the runner's token to a job;
        cancel lists running jobs a person stopped, and jobs in running that the server
        ended itself more than 45 s ago (stop them). Jobs the server thinks the runner has
        that it doesn't list in running are failed as lost. server_version is the server's
        version: a runner on a release build that differs updates itself between jobs
        (/cli/wp-OS-ARCH, checked against /cli/SHA256SUMS)
    POST /runner/jobs/{id}/log   {offset, text} → {size, cancel, truncated}
        append output; offset (bytes) makes a resent chunk harmless; 1 MB kept per job;
        cancel true = stop the job
    POST /runner/jobs/{id}/finish  {state: done|failed|cancelled, exit_code, result, error}
    POST /runner/jobs/{id}/gateway  multipart/form-data: prompt (text), repo (the checkout as
        .tar.gz, 20 MiB at most) → the task. An audit job whose model is gateway/…: Waypoint
        starts a gateway agent task over the checkout with its own key (WP-617)
    GET /runner/jobs/{id}/gateway   → {id, status, model, text, json, error_kind, error,
        retry_after, input_tokens, output_tokens, cost_usd}: the job's task now; model is the
        provider/model that ran it. status is the gateway's
        (queued, running, succeeded, failed, cancelled), or refused (it took no task;
        retry_after seconds when sending again may work), or unknown (ask again)
    POST /runner/jobs/{id}/gateway/cancel  stop the job's task at the gateway
    POST /runner/jobs/{id}/cancel  (people) same as POST /runner-jobs/{id}/cancel
    POST /runner/stalled-jobs    {runner, v, jobs[{session, task, pid, group, command, started_at,
        quiet_minutes, timeout_minutes, evidence[], running[]}], settled[{session, task, how}]}
        → {jobs[{task, session, outcome, retry, again_in}], policy}: Claude Code background jobs
        on the runner's machine that made no progress (WP-605). With v 2 (policy 2 in the answer):
        every job quiet 5 minutes, again in again_in seconds while it stays quiet, and in settled
        once it moved again (how "progress") or ended; Waypoint applies the project's stall
        policy (GET /stalled-jobs/policy) and tells each quiet spell's session once (WP-1045)

    GET /runners[?all=1]         yours (admins: everyone's with all=1): name, hostname, os, arch,
                                 version, labels, allowed_dirs, clis, online, last_seen_at, running
    GET /runners/{id}            DELETE /runners/{id}   (waiting jobs are cancelled, running ones fail)
    GET /runner-jobs?runner=ID&state=queued,running&source=bot_run&source_id=N&limit=50&before=ID
        jobs on your runners or started by you, newest first (all=1 for admins: they see that
        a job exists, not what it does)
    GET /runner-jobs/{id}[?log=1|after=BYTE]   the job with input and result; log=1 adds the
        output, after=N only what follows byte N (log_end is where to read next); a queued job
        says what it is waiting for
    POST /runner-jobs            {kind, runner_id?, label?, timeout?, input}   (people only; run by hand)
        command input {command, dir, env?, keep_api_keys?}; apply {dir, files[{path, base_sha,
        content, delete}], commit?, push?}; agent/review {cli: claude|codex|opencode|cursor,
        model?, prompt, repo (clone URL), base?, branch?, push?, read_only?, json_schema?,
        commit_message?}
    POST /runner-jobs/{id}/cancel   the job's creator or its runner's owner; a running job's
        runner kills its process group at its next poll or log upload

Live events (the stream): `runner.status` (runner, name, online), `runner.job_updated` (job,
state, size), `runner.job_finished` (job, kind, state, source, source_id).

## Agent kit (WP-ADR2)

A repo attached to a project, laid out as a Claude Code plugin marketplace, with `kit.toml`
saying which plugins and notes go to which repos (WP design #368). Waypoint reads it at its
default branch's head on a push and hourly; a broken kit.toml is reported and the last good kit
stays. Anyone signed in reads it; admins pick the repo and roll out.

    GET  /kit                   {configured, repo_id, repo, project, repo_url, branch, commit,
                                 kit{marketplace, git_url, owner, plugins[{name, description,
                                 version, source, targeted, target{repos, when, machines},
                                 skills[], agents[], commands[], hooks[], mcp[], bin[]
                                 ({name, description})}], notes[{repos, when, machines, text}]},
                                 good_at, read_at, errors[], read_sha, reach{plugin: [repos]},
                                 can_manage}
    GET  /kit/resolve?repo=OWNER/NAME
                                → {repo, attached, commit, marketplace, git_url,
                                   plugins[{name, when[], machines[]}], notes[{text, when[],
                                   machines[]}], all[]}  what one repo gets (wp kit sync and
                                   SessionStart check when/machines in the checkout); a repo
                                   attached to no project gets nothing; no kit: commit ""
    POST /kit/reports           {machine, path, repo, kit_commit, plugins[], drift[{plugin, what}],
                                 wp_version, error} → 204   a checkout's state after a sync
    GET  /kit/checkouts         latest report per machine and path (yours; admins: everyone's),
                                 failed, behind or drifted first: {user, machine, path, repo,
                                 kit_commit, behind, plugins, drift, wp_version, error, reported_at}
    POST /kit/rollout           (admins, people) queue `wp kit sync --all` as a command job on
                                 each of your online runners → [{runner_id, runner, job_id?,
                                 skipped?}]
    POST /kit/add               {kind: skill|agent|command|hook, plugin, name, files[{path,
                                 content}], hooks? (JSON for hooks.json), message?, description?}
                                 → {branch, commit, pr_url, pr_number, files[], new_plugin}
                                 opens a PR on the kit repo (text files, 1 MB)
    POST /kit/read              (admins) read the kit again now, even at the same commit
    GET  /admin/kit             PUT /admin/kit {repo_id}   (admins) the kit repo (0: none) and the
                                 attached repos it could be

Live events: `kit.updated` (the kit was read), `kit.source_set`, `kit.reported`.

## Agent setup (WP-ADR44)

The files agents read in every attached repo (every CLAUDE.md and AGENTS.md, and at the root
`.claude/` settings, agents, commands, skills, hooks and `.mcp.json`, read from the forge at the
default branch's head on a push that changes one, hourly and on refresh) and on every machine
(`wp kit sync` and session start report them, settings without secrets); the shared
instructions Waypoint keeps between `<!-- waypoint:shared … -->` markers at the end of each
repo's root instruction file. Repos: those of your projects (admins: all). Machines: yours
(admins: all). Agents read; people edit; admins edit and publish the shared text.

    GET  /agent-setup           {shared{current, versions[{version, text, saved_by, saved_at}],
                                 counts{state: n}}, repos[{forge_id, repo, url, projects,
                                 opted_out, can_edit, branch, head, scanned_at, error, root_file,
                                 shared: current|missing|behind|edited|unread|none,
                                 shared_version, shared_pr, shared_error, files[{path, kind,
                                 size, lines, truncated}], components[{kind, name, detail, path,
                                 only_here}], missing[{kind, name}], kit_plugins[], checks[{key,
                                 label, state: ok|warn|fail, note}], prs[], status}],
                                 repo_common[{kind, name, count, common}], machines[{user,
                                 machine, os, wp_version, reported_at, files, components,
                                 missing, checks, status}], machine_common, can_manage, can_edit}
    GET  /agent-setup/file?forge=ID&repo=OWNER/NAME&path=P
                                → {path, kind, size, lines, truncated, content, repo, sha, url,
                                   editable, shared}   one repo file as last read
    PUT  /agent-setup/file      {forge_id, repo, path, content, base_sha, message?} → {branch,
                                 number, url}   (people, editors) a PR labelled automerge with the
                                 change; 409 if the file moved since base_sha
    GET  /agent-setup/machine-file?user=ID&machine=M&path=P   one machine file (owner, admins)
    PUT  /agent-setup/shared    {text} → {version, text, saved_by, saved_at}   (admins)
                                 saves a version; repos missing it get a PR within the hour
    POST /agent-setup/publish   (admins) → {version, repos[], skipped[]}   a PR with the
                                 current text on every repo whose block differs, in the background
    POST /agent-setup/refresh   → 202   read every repo again now
    POST /agent-setup/machines  {machine, os, wp_version, files[{path, content}], checks[{key,
                                 label, state, note}]} → 204   a machine's report (paths relative
                                 to the home folder; settings are cleaned again on arrival)

Live event: `agent_setup.updated`.

## Assistants (WP-ADR1)

A person's own Hermes agent behind its gate. Every route is for its owner signed in in a
browser; API tokens are refused, whatever their permissions. Admins connect them.

    GET    /assistants                  yours (admins: all), with state {reachable, frozen,
                                        signal, pending[approvals], error, checked_at}
    POST   /assistants                  admins: {name, owner_id?, url, ca_pem, key, username?,
                                        password, token_id?}; secrets are never returned
    PATCH  /assistants/{id}             admins: the same fields; empty secrets keep the old ones
    DELETE /assistants/{id}             admins
    POST   /assistants/{id}/check       read the gate's status now
    POST   /assistants/{id}/test        [{name: gate|hermes, ok, error}]
    POST   /assistants/{id}/freeze      cut the agent off at its gate (only Signal undoes it)
    GET    /assistants/{id}/live        server-sent events: hello {state, open[questions]},
                                        then each frame of Hermes's chat gateway, numbered
                                        (Last-Event-ID or ?after= catches up)
    POST   /assistants/{id}/rpc         {method, params}: one gateway call, if its method is
                                        on the allowlist (assistantMethods in
                                        internal/core/assistant_chat.go: chat, sessions,
                                        attachments, slash commands, …); others are 400
    POST   /assistants/{id}/answer      {id, result}: answer the agent's approval or clarify
    *      /assistants/{id}/api/{path}  Hermes's REST API at /api/{path} (an allowlist; no
                                        logins, updates, installs or secrets)
    GET    /assistants/{id}/gate/approvals|audit|logs/{name}|connections
    PUT    /assistants/{id}/gate/connections/{name}   {kind, url, headers, policy, description}:
                                        200 saved, or 202 {status: pending, code} until Signal
    DELETE /assistants/{id}/gate/connections/{name}
    GET    /assistants/{id}/links?session=      conversations linked to issues
    POST   /assistants/{id}/links       {session_id, issue, title}
    DELETE /assistants/{id}/links/{link}
    GET    /issues/{key}/assistant-links

## Reports

    GET  /dashboard                      every project at a glance: open work by column, pace, share on a goal,
                                         what may be wrong (counted, no model)
    GET  /projects/{key}/dashboard       one project: the same plus its goals, suggested goals, drift; each
                                         problem has a WQL query, and fix "agent" or "suggest_goals" when one
                                         click can act on it
    POST /projects/{key}/dashboard/fix {"signal":"stuck"}   file a task for an agent with the tasks behind a
                                         problem, or return the open one already filed; {issue, created}

    GET /projects/{key}/reports/forecast?q=under:WAY-3   ETA (p50/p85/p95) from recent throughput
    GET /projects/{key}/reports/roadmap                  open epics with progress and ETAs
    GET /projects/{key}/reports/throughput?weeks=12      weekly flow + burnup
    GET /projects/{key}/reports/flow?days=30             cumulative flow
    GET /projects/{key}/reports/velocity                 closed sprints
    GET /sprints/{id}/burndown
    GET /reports/time?from=2026-10-01&to=2026-10-08&project=KEY

## Time tracking

    GET  /timer                     the running timer (an agent's own, beside its person's)
    POST /timer/start {"issue": "WAY-12", "note": ""}
    POST /timer/stop
    POST /time {"issue": "WAY-12", "duration": "1h30m", "note": ""}
    GET  /time?from=…&to=…          PATCH|DELETE /time/{id}

## You

    GET|PATCH /me                   profile and preferences
    POST /me/password {"current","new"}   also signs out your other sessions
    GET /me/sessions                DELETE /me/sessions/{id|others}
    GET /me/push                    {public_key, devices}   web push (VAPID)
    POST /me/push {endpoint, keys:{p256dh, auth}}   DELETE /me/push {endpoint}
    PUT|DELETE /me/avatar           image body or multipart field "file"
    GET|POST /me/tokens {"name","read_only","expires_in_days","agent","permissions"}   DELETE /me/tokens/{id}
    PATCH /me/tokens/{id} {"name","read_only","permissions": [...] or null}   (people only)
    DELETE /me/tokens/{id}/refusals   GET /me/token-permissions (the catalog and presets)
    GET /me/calendar                POST /me/calendar/rotate   private iCal feed URL
    POST /me/gotify/test

## Admin (admins only)

    GET|POST /admin/users           PATCH|DELETE /admin/users/{id}
    PUT /admin/users/{id}/avatar    POST /admin/users/{id}/reset-link
    GET|POST /admin/invites         DELETE /admin/invites/{id}
    GET|POST /admin/webhooks        global webhooks
    GET|POST /forges                PATCH|DELETE /forges/{id}   GET /forges/{id}/secret
                                    "bot_token": a bot account for reviews and comments
    GET  /forges/{id}/discover      repos discovery would attach: [{repo, namespace, new_namespace, joins}]
    POST /forges/{id}/discover      attach every repo the token sees (kind: github, gitlab, forgejo);
                                    body {repos: [...], skip: [...]} attaches only those, skipping the rest for good
    GET /forges/{id}/orgs           organisations (GitLab: owned groups) the token sees
    GET|POST /forges/{id}/org-hooks {"org"}   organisation webhook (POST again to retry)
    PUT /forges/{id}/people {"people": [{login, user, person_only}]}   its logins that are Waypoint
                                    people (whose text isn't from outside); in the web or your own
                                    terminal. Without person_only, a login's text counts as
                                    unreviewed agent text ("forge/LOGIN"): agents may hold its
                                    credentials (WP-282)
    DELETE /forges/{id}/org-hooks/{id}        its repos get their own webhooks back
    GET|POST /ai/providers          PATCH|DELETE /ai/providers/{id}   GET /ai/providers/{id}/models
    GET|PUT /admin/review-note {"note"}   the line bot reviews start with and sign line comments
                                    with; {models} names the models ("" restores the default)
    GET|PUT /admin/ship-reply {"reply"}   the template wp ship gives an agent for its last
                                    message; {task} {pr} {url} ("" restores the default)
    GET /admin/pr-bodies?days=30&show=N   PR descriptions both ways: the template's Summary
                                    and a free model's, counts, unsupported names, the last N
                                    pairs (WP-55)
    PUT /admin/pr-bodies {"mode":"shadow|model|template"}   shadow keeps the model's
                                    unpublished; model publishes it when it passes the check;
                                    template asks no model (people only)
    GET|PUT /admin/email {"host","port","security":"starttls|tls|none","username","password",
                                    "clear_password","from"}   SMTP for the email automation
                                    action (offered only once set); "" host turns email off;
                                    the password is never returned (has_password)
    POST /admin/email/test {"to"}   send a test ("" sends it to you)
    PUT /ai/tasks/{task} {"provider_id", "model", "fallbacks": [{"provider_id","model"}]}
                                    (provider_id 0 turns the task off; fallbacks are tried in
                                    order when a model fails; "opencode/muse-spark-*" is the
                                    newest model of that name)
    GET|POST /ai/safety-drill       the weekly safety drill (WP-278): {configured, running, last:
                                    {passed, failed, errors, cases:[{name, label, want, verdict,
                                    flags, model, pass}]}, models:[{model, passed, total}], next_at};
                                    POST runs one now in the background

## Other

    GET /notifications?unread=1     POST /notifications/read {"ids": [], "upto": N}  (empty: all, up to id N when given)
    GET /inbox                      the Inbox sorted: {summary, live, stale, earlier}, each a list of
                                    groups (one per issue or thing, with its newest notifications);
                                    stale = unread but no longer relevant (issue closed, request
                                    decided, alert days old). GET /inbox/counts is its summary alone
                                    (the badge). POST /inbox/read {"section":"live|stale", "group":
                                    key?, "upto": summary.as_of} marks one group or a section read,
                                    only notifications up to that id (what was on screen)
    GET|POST /views                 PATCH|DELETE /views/{id}   saved queries
    GET /stream                     server-sent events of changes
    GET /users                      everyone you can assign
    GET /search                     alias of GET /issues
    GET /ai/tasks                   AI tasks and whether a model is set up
    GET /tags                       tag names used across your projects, most used first
    POST /projects/{key}/ai/estimate  estimate open unestimated issues in the background
    GET /help                       this page

Outside `/api/v1`: `POST /api/v1/hooks/forge/{id}` (signed forge webhooks),
`/mcp`, `/cal/{token}.ics`, `/healthz`, `/readyz`, `/metrics`.
