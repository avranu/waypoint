package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Production diagnostics through the API, not ssh (WP-375). Admins only; a
// permission token also needs server.read.

var serverCmd = &command{
	name: "server", group: "Agent work", args: "[status|problems|job|log|metrics …]",
	summary:  "the Waypoint server's health, what keeps going wrong on it, recent log and metrics, read through the API (admins)",
	examples: []string{"wp server status", "wp server problems", "wp server job 'forge sync'", "wp server log --since 3h --level warn", "wp server metrics --grep waypoint_ai"},
	about: `Diagnose production with wp, not ssh. wp server status is the server's
version, uptime, schema, every scheduled job's last run and whether it works,
the problems that persist, queue depths, the AI key's day and the last hour's
warnings. wp server problems lists what goes wrong: each warning and error
by where it was logged, kept across restarts. wp server job NAME is one job
over time. wp server log reads the server's log (30 days, kept across
restarts; secrets blanked). wp server metrics is the Prometheus text. All
read-only but ack, which is a person's; an admin's token, or a permission
token with server.read.`,
	subs: []*command{
		{name: "status", summary: "version, uptime, schema, scheduled jobs, persistent problems, queues, the AI key's day and recent warnings",
			about: `Each scheduled job shows how often it runs, its runs, its last run and how
that ended: clean, warned (it logged a warning), failed (an error), skipped
(it said it could do nothing), stopped (the server was stopping: neither
clean nor unclean), crashed or interrupted (it was running when the server
died, found out at the next start). STUCK is a run that has gone on too
long, or a job that has only found its last run still going for longer
than a run may take (its worker has hung); SILENT a job that should have
started and hasn't. The counts are
kept in the database, so they go back past the last restart.

Two lines say how the server is. "Before this start" says how the run
before ended: stopped cleanly, or DID NOT SHUT DOWN (it was killed or died),
and how long the server was away. "Job health checked 20s ago" is the check
that tells the admins; NOT CHECKED means the check itself is stuck.`,
			examples: []string{"wp server status", "wp server status --json"},
			run:      cmdServerStatus},
		{name: "problems", args: "[--all] [--source NAME] [--limit N]", summary: "what goes wrong on the server: warnings and errors by where they were logged, the persistent ones first",
			about: `A problem is a warning or an error told apart by its source (a scheduled
job, or requests, events, background, automation runs, bot runs), its
message and what it was about (a repo, a forge, a collector). Each has a
count, the streak it is in, and the last record's attributes, secrets
blanked.

A problem is persistent once a warning or error has kept happening for a
day with three records in it (two runs for a job that runs daily or less
often), or as soon as a job that runs once when the server starts has one,
wherever it was logged. The admins get one
notification for the problems that became persistent, the jobs that are
stuck or silent and what recovered, and a reminder each week for a problem
nobody acknowledged. A problem is over after a day and an hour of quiet, not counting the time
the server was down; nor does the downtime count as time the problem lasted
(3 warnings, a 25-hour outage and 1 more are not a day of failing). With
no admin to tell, problems still clear; what the admins were not told
waits for one.

--all includes the ones that are over (kept 90 days). --source keeps one
source's.`,
			examples: []string{"wp server problems", "wp server problems --source 'forge sync'", "wp server problems --all --json"},
			run:      cmdServerProblems},
		{name: "ack", args: "N -m WHY", summary: "say a server problem is expected and why: it stays listed and is not told again (a person's to say)",
			about: `For a problem that is known and waits on something else. It stays in
wp server problems with who acknowledged it and why, and comes back as new
if it stops for a week and starts again. Only an admin, in their own
terminal: an agent that could acknowledge would silence the alert about
its own failures. An agent that thinks a problem is expected files a
request (wp request todo).`,
			examples: []string{`wp server ack 12 -m "the repo is archived on the forge; detaching it is WP-1310"`},
			run:      cmdServerAck},
		{name: "unack", args: "N", summary: "take an acknowledgement back: the problem is told again",
			examples: []string{"wp server unack 12"},
			run:      cmdServerUnack},
		{name: "job", args: "NAME [--since 7d] [--by hour|day]", summary: "one scheduled job over time: runs, runs that were not clean, warnings, errors and how long it took, with its problems",
			about: `NAME is a job from wp server status, or one of the other sources of log
records: requests, events, background, "automation runs", "bot runs".
--since takes a duration back from now (12h, 30d) or a time (UTC), default
7d; hours are kept 90 days. --by is hour or day (UTC): hour for two days or
less, else day.`,
			examples: []string{"wp server job 'forge sync'", "wp server job 'CI history' --since 24h", "wp server job requests --by day --since 30d"},
			run:      cmdServerJob},
		{name: "log", args: "[--since 1h] [--until T] [--grep RE] [--level warn] [--limit N]", summary: "the server's recent log, oldest first, secrets blanked",
			about: `Reads the server's log: the recent records from memory, older ones from its
log files, which keep 30 days across restarts and deploys. Secrets are blanked
when a record is kept: the server's secret settings and stored tokens, and
anything shaped like a token or a password.

--since and --until take a duration back from now (90m, 3h, 2d) or a time
(2026-11-06T14:00, 14:00 for today, UTC). --since defaults to 1h. --grep
keeps records whose message or attributes match the regular expression
(case-insensitive), --level the records at or above debug, info, warn or
error. --limit N shows the newest N that match (default 500).`,
			examples: []string{"wp server log", "wp server log --since 3h --level warn", "wp server log --grep 'safety screen' --since 1d", "wp server log --since 14:00 --until 14:30"},
			run:      cmdServerLog},
		{name: "metrics", args: "[--grep RE]", summary: "the server's Prometheus metrics, as text",
			about: `Prints what /metrics serves, through the authenticated API. --grep keeps
the samples whose line matches the regular expression, with their HELP and
TYPE lines.`,
			examples: []string{"wp server metrics", "wp server metrics --grep 'waypoint_ai|go_goroutines'"},
			run:      cmdServerMetrics},
	},
}

var aiCmd = &command{
	name: "ai", group: "Agent work", args: "[usage|surplus …]",
	summary:  "what the server's AI requests went on, by task, model, outcome or hour (admins)",
	examples: []string{"wp ai usage", "wp ai usage --since 7d --by day"},
	subs: []*command{
		{name: "usage", args: "[--since 24h] [--until T] [--by task|model|outcome|status|hour|day|provider|priority|bar] [--task NAME]",
			summary: "the server's AI requests in a window, grouped: requests, failures, rate limits, tokens, latency",
			about: `Counts the requests Waypoint sent its AI providers (safety screens, reviews,
replies, automations, Jev's decisions, …), kept 30 days, with the AI key's
day as the gateway counts it. --since and --until take a duration back from
now (3h, 2d) or a time (UTC); --since defaults to 24h. --by groups them
(default task); --task keeps one task's requests.`,
			examples: []string{"wp ai usage", "wp ai usage --by hour --since 12h", "wp ai usage --task task_safety --by outcome"},
			run:      cmdAIUsage},
		{name: "surplus", args: "",
			summary: "optional AI work at the gateway's surplus priority: refusals, prep ahead's payoff, prep picks reported, agentic trials, the prep policy",
			about: `Surplus work (WP-906) is sent only on capacity the gateway would otherwise leave
unused, and refused when there is none. Shows the last day's surplus requests
by task and outcome, how often a prep made ahead of a queue's front was what an
agent got (by position, and the depth that follows), and, by model, how the
file picks of preps and trials matched the files the task's merged work
changed, and how many of those reports the gateway has taken. The models'
ratings are the gateway's, under its prep purpose. Last, agentic prep trials
(WP-ADR104: a gateway agent task reads the repository with tools) against
the chat preps of the same tasks: recall, precision and files a task. Then
the prep policy: the share of tasks whose agent gets no pointers, the chat
prep's or the agentic prep's, each arm's estimated effect on the searches and
reads an agent makes before its first edit (against no pointers, with its 90%
interval, now and over all time), the guard measures that take an arm out,
how the picks score without being shown, and when the shares last moved.`,
			examples: []string{"wp ai surplus", "wp ai surplus --json"},
			run:      cmdAISurplus},
	},
}

func cmdAISurplus(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	return c.getDiag(pos, "/api/v1/ai/surplus", nil)
}

func cmdServerStatus(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	return c.getDiag(pos, "/api/v1/server/status", nil)
}

func cmdServerProblems(c *cli, args []string) error {
	fs := c.flags()
	all := fs.Bool("all", false, "the ones that are over too")
	source := fs.String("source", "", "`NAME`: only this job's or source's")
	limit := fs.Int("limit", 0, "`N`: at most N (default 500)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	q := url.Values{}
	if *all {
		q.Set("all", "1")
	}
	setIf(q, "source", *source)
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	return c.getDiag(pos, "/api/v1/server/problems", q)
}

// serverProblemID reads the N of wp server ack and unack.
func serverProblemID(c *cli, pos []string) (string, error) {
	if len(pos) != 1 {
		return "", c.usage("say which problem: its number from wp server problems")
	}
	id := strings.TrimPrefix(pos[0], "#")
	if n, err := strconv.ParseInt(id, 10, 64); err != nil || n <= 0 {
		return "", c.usage("%q is not a problem's number (wp server problems lists them)", pos[0])
	}
	return id, nil
}

type serverProblem struct {
	ID        int64  `json:"id"`
	Source    string `json:"source"`
	Message   string `json:"message"`
	Target    string `json:"target"`
	AckReason string `json:"ack_reason"`
}

func (p serverProblem) label() string {
	s := fmt.Sprintf("#%d %s: %s", p.ID, p.Source, p.Message)
	if p.Target != "" {
		s += " (" + p.Target + ")"
	}
	return s
}

func cmdServerAck(c *cli, args []string) error {
	fs := c.flags()
	why := fs.String("m", "", "`WHY` it is expected, for whoever reads the list next")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	id, err := serverProblemID(c, pos)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*why) == "" {
		return c.usage("say why it is expected: -m WHY")
	}
	raw, err := c.call("POST", "/api/v1/server/problems/"+id+"/ack", map[string]string{"reason": *why})
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var p serverProblem
		if json.Unmarshal(raw, &p) != nil {
			return "Acknowledged."
		}
		return "Acknowledged " + p.label() + ". It stays in wp server problems and is not told again unless it stops for a week and comes back."
	})
}

func cmdServerUnack(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	id, err := serverProblemID(c, pos)
	if err != nil {
		return err
	}
	raw, err := c.call("DELETE", "/api/v1/server/problems/"+id+"/ack", nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var p serverProblem
		if json.Unmarshal(raw, &p) != nil {
			return "No longer acknowledged."
		}
		return p.label() + " is no longer acknowledged: the admins are told of it while it persists."
	})
}

func cmdServerJob(c *cli, args []string) error {
	fs := c.flags()
	since := fs.String("since", "", "`WHEN`: from this long ago (12h, 30d) or this time (UTC); default 7d")
	by := fs.String("by", "", "`SPAN`: hour or day (default: hour for two days or less)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("say which job: wp server status lists them")
	}
	// A name of several words needs no quotes: wp server job forge sync.
	name := strings.Join(pos, " ")
	q := url.Values{}
	setIf(q, "since", *since)
	setIf(q, "by", *by)
	return c.getDiag(nil, "/api/v1/server/jobs/"+url.PathEscape(name), q)
}

func cmdServerLog(c *cli, args []string) error {
	fs := c.flags()
	since := fs.String("since", "", "`WHEN`: from this long ago (3h, 2d) or this time (UTC); default 1h")
	until := fs.String("until", "", "`WHEN`: up to this long ago or this time")
	grep := fs.String("grep", "", "`RE`: only records that match")
	level := fs.String("level", "", "`LEVEL`: only records at or above debug, info, warn or error")
	limit := fs.Int("limit", 0, "`N`: the newest N that match (default 500)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	q := url.Values{}
	setIf(q, "since", *since)
	setIf(q, "until", *until)
	setIf(q, "grep", *grep)
	setIf(q, "level", *level)
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	return c.getDiag(pos, "/api/v1/server/log", q)
}

func cmdServerMetrics(c *cli, args []string) error {
	fs := c.flags()
	grep := fs.String("grep", "", "`RE`: only samples that match")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	q := url.Values{}
	setIf(q, "grep", *grep)
	raw, err := c.call("GET", "/api/v1/server/metrics?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	c.println(strings.TrimRight(string(raw), "\n"))
	return nil
}

func cmdAIUsage(c *cli, args []string) error {
	fs := c.flags()
	since := fs.String("since", "", "`WHEN`: from this long ago (3h, 2d) or this time (UTC); default 24h")
	until := fs.String("until", "", "`WHEN`: up to this long ago or this time")
	by := fs.String("by", "", "`GROUP`: task (default), model, outcome, status, hour, day, provider, priority or bar (the model the gateway held it to)")
	task := fs.String("task", "", "`NAME`: only this task's requests")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	q := url.Values{}
	setIf(q, "since", *since)
	setIf(q, "until", *until)
	setIf(q, "by", *by)
	setIf(q, "task", *task)
	return c.getDiag(pos, "/api/v1/ai/usage", q)
}

// getDiag GETs a diagnostics report, as text unless --json.
func (c *cli) getDiag(pos []string, path string, q url.Values) error {
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	if q == nil {
		q = url.Values{}
	}
	if !c.asJSON {
		q.Set("format", "text")
	}
	raw, err := c.call("GET", path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return string(raw) })
}

func setIf(q url.Values, k, v string) {
	if v != "" {
		q.Set(k, v)
	}
}
