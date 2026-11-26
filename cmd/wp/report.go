package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// wp report SUBJECT reads and summarises; wp record SUBJECT writes down
// something that happened (WP-ADR164). The names these replaced still run:
// legacyRoutes maps each to its new command before dispatch, so no command is
// defined twice.

var reportCmd = &command{
	name: "report", group: "Reports", args: "SUBJECT [TOPIC] [FLAGS]",
	summary: "reports over a window: AI spend and quota, how agents behaved, the server's AI requests",
	about: `A report reads and summarises over a window or a scope; it never changes
anything. wp report with no subject lists them. Every report takes --since
(a duration back from now, or a time) and --json; --days N means --since Nd.`,
	examples: []string{"wp report usage --since 24h", "wp report behavior risky", "wp report ai"},
	run:      cmdSubjects,
}

var recordCmd = &command{
	name: "record", group: "Reports", args: "SUBJECT [FLAGS]",
	summary: "write down something that happened: a task's tokens, what sessions used",
	about:   "wp record with no subject lists them.",
	examples: []string{
		"wp record usage --tokens-in 12000 --tokens-out 800 --cost 0.21",
		"wp record usage --from-transcripts",
	},
	run: cmdSubjects,
}

var reportUsageCmd = &command{
	name: "usage", aliases: []string{"cost", "spend", "quota"},
	args:    "[--since 24h] [--until T] [--dir CLAUDE_CONFIG_DIR] [--top N] [--csv FILE] [--task KEY | --session [ID]]",
	summary: "where this machine's Claude Code spend went in a window, and what looks wasteful",
	about: `Reads this machine's Claude Code transcripts (main sessions, subagents and
workflow agents) and prints one report in list-price dollars: totals by
model with the cache split; by agent, project and how the session was
started; what the premium-model main threads spent each call on (reading,
searching, editing through the shell or the Edit tool, tests and builds, git,
wp, delegating, waiting, text only), counting the commands of a compound line
(a command that only filters a pipe doesn't count); what
woke each call (typed prompt, notification, tool result, compaction); each
session's spend before and after its first wp ship; the top sessions;
limit messages and the spend after the first; and what the report cannot
see. A call is counted once however many records stream it (the largest
usage wins). --since and --until take a duration ago (24h, 3d) or a time
(2026-11-24T15:00:00Z, 2026-11-24). --csv writes one row per call.

--task KEY shows what one task used, and what its agents had: its tokens,
cost, tool calls, edits, time and merged pull requests, each run's versions
(server, wp, session start, kit, Claude Code, hooks), its experiment arms
(admins), and the tools, skills and commands it used.

--session [ID] shows one session and its subagents: per agent, kind, model,
calls, average and largest context per call and cost, then totals and $/hour.
The session is the ID (or a transcript path), else this shell's, else the
newest for this directory. A hook warns once when the last 10 minutes run
above $100/h (WP_BURN_ALERT_PER_HOUR; 0: off).

Also: wp report usage machines (whether each machine's transcripts reached
the server) and wp report usage guard (the quota guard).`,
	examples: []string{
		"wp report usage", "wp report usage --since 3d --top 20 --csv calls.csv",
		"wp report usage --since 2026-11-24T15:00:00Z --until 2026-11-25T15:00:00Z --json",
		"wp report usage --task WP-192", "wp report usage --session",
	},
	run: cmdReportUsage,
}

var reportBehaviorCmd = &command{
	name: "behavior", aliases: []string{"behaviour"}, args: "[TOPIC] [FLAGS]",
	summary: "how agents worked: wp use, risky commands, stalled jobs, compactions, delegation",
	about: `What agents did, counted from their transcripts and what wp sync uploaded.
wp report behavior with no topic lists the topics and the command for each.`,
	examples: []string{"wp report behavior", "wp report behavior risky --days 7"},
	run:      cmdSubjects,
}

var reportAICmd = &command{
	name: "ai", args: "[--since 24h] [--until T] [--by task|model|outcome|status|hour|day|provider|priority|bar] [--task NAME]",
	summary: "the server's own AI requests in a window: requests, failures, rate limits, tokens, latency (admins)",
	about: `Counts the requests Waypoint sent its AI providers (safety screens, reviews,
replies, automations, Jev's decisions, …), kept 30 days, with the AI key's
day as the gateway counts it. Spend and quota of Claude Code sessions are in
wp report usage. --since and --until take a duration back from now (3h, 2d)
or a time (UTC); --since defaults to 24h. --by groups them (default task);
--task keeps one task's requests.`,
	examples: []string{"wp report ai", "wp report ai --by hour --since 12h", "wp report ai --task task_safety --by outcome", "wp report ai surplus"},
	run:      cmdAIUsage,
}

// quotaGuardCmd is the control for the quota guard; bare, it reads like
// wp report usage guard.
var quotaGuardCmd = &command{
	name: "guard", group: "Tasks", args: usageGuardCmd.args,
	summary: "show the quota guard, or change or lift its hold on automation",
	run:     cmdUsageGuard,
	subs:    []*command{usageGuardSetCmd, usageGuardLiftCmd},
}

// reportSubjects is each report's behavior topics, in the order wp report
// behavior lists them.
func init() {
	reportCmd.subs = []*command{reportUsageCmd, reportBehaviorCmd, reportAICmd}
	recordCmd.subs = []*command{recordUsageCmd}
	reportUsageCmd.subs = []*command{usageMachinesCmd, usageGuardCmd}
	reportAICmd.subs = []*command{aiSurplusCmd}
	reportBehaviorCmd.subs = []*command{usageCLICmd, usageRiskyCmd, usageStalledCmd, usageWarmCmd, usageCompactCmd,
		usageDelegationCmd, usageSubagentsCmd, usageSuggestCmd, usageGoalContextCmd, usageFactsCmd}
	usageTranscriptCmd.group = "Tasks"
	quotaGuardCmd.about = usageGuardCmd.about
	quotaGuardCmd.examples = []string{"wp guard", "wp guard set --churn 40 --session 150", "wp guard lift --for 6h"}
	usageGuardCmd.examples = []string{"wp report usage guard", "wp report usage guard --now --hours", "wp report usage guard --days 30"}
	// The reports that take a window take it as --since or as --days.
	for _, cmd := range slices.Concat([]*command{reportUsageCmd, reportAICmd, usageGuardCmd, quotaGuardCmd}, reportBehaviorCmd.subs) {
		cmd.window = true
	}
}

// usageCmd is where stage 2's reports (meaning, feature) wait for their move
// to wp report; it is out of the help listing, which has no group for it.
var usageCmd = &command{
	name:     "usage",
	args:     "meaning|feature",
	summary:  "reports not yet moved to wp report: meaning, feature",
	examples: []string{"wp usage meaning", "wp usage feature WP-192"},
	about: `The rest of what wp usage did has new names: wp report usage (spend), wp report
behavior TOPIC (how agents worked), wp record usage (write a task's tokens;
--from-transcripts uploads sessions), wp guard, wp transcript, wp setup otel.
The old names still run.`,
}

// legacyRoute is an old name that runs a new command.
type legacyRoute struct {
	from string
	to   string
	// args rewrites the words after the old name; nil keeps them.
	args func(rest []string) []string
}

var behaviorTopics = []string{"cli", "risky", "stalled", "warm", "compact", "delegation", "subagents", "suggest", "goal-context", "facts"}

var legacyRoutes = func() []legacyRoute {
	rs := []legacyRoute{
		{from: "usage where", to: "report usage"},
		{from: "usage task", to: "report usage --task"},
		{from: "usage machines", to: "report usage machines"},
		{from: "usage guard set", to: "guard set"},
		{from: "usage guard lift", to: "guard lift"},
		{from: "usage guard", to: "report usage guard"},
		{from: "usage sync", to: "record usage --from-transcripts"},
		{from: "usage transcript", to: "transcript"},
		{from: "usage otel", to: "setup otel"},
		{from: "burn", to: "report usage --session", args: func(rest []string) []string {
			// wp burn --session ID is wp report usage --session ID: the
			// route already says --session, so its value moves up front.
			for i, a := range rest {
				if !flagIs(a, "session") {
					continue
				}
				if _, v, ok := strings.Cut(a, "="); ok {
					return append([]string{v}, slices.Delete(slices.Clone(rest), i, i+1)...)
				}
				if i+1 < len(rest) {
					return append([]string{rest[i+1]}, slices.Delete(slices.Clone(rest), i, i+2)...)
				}
				return slices.Delete(slices.Clone(rest), i, i+1)
			}
			return rest
		}},
		{from: "ai usage", to: "report ai"},
		{from: "ai surplus", to: "report ai surplus"},
		{from: "ai", to: "report ai"},
		{from: "report requests ai", to: "report ai"},
	}
	for _, t := range behaviorTopics {
		rs = append(rs, legacyRoute{from: "usage " + t, to: "report behavior " + t})
	}
	return rs
}()

// rewriteLegacy turns an old invocation into the new one: the words to run,
// and the route taken (nil when args name no old command). to's own flag
// words (--task, --session) lead the rest; the old name's words are dropped.
func rewriteLegacy(args []string) ([]string, *legacyRoute) {
	var best *legacyRoute
	bestN := 0
	for i := range legacyRoutes {
		r := &legacyRoutes[i]
		f := strings.Fields(r.from)
		if len(f) > bestN && len(args) >= len(f) && slices.Equal(args[:len(f)], f) {
			best, bestN = r, len(f)
		}
	}
	if best == nil {
		// Bare wp usage writes: wp usage KEY --tokens-in N.
		if len(args) > 0 && args[0] == "usage" && (len(args) == 1 || !slices.Contains([]string{"meaning", "feature"}, args[1])) {
			return append([]string{"record", "usage"}, args[1:]...), &legacyRoute{from: "usage", to: "record usage"}
		}
		return args, nil
	}
	rest := args[bestN:]
	if best.args != nil {
		rest = best.args(rest)
	}
	return append(strings.Fields(best.to), rest...), best
}

// legacyNote is the first line of an old name's help.
func legacyNote(r *legacyRoute) string {
	if r.from == "usage" {
		return "wp usage is now wp record usage (reports: wp report usage, wp report behavior):"
	}
	return fmt.Sprintf("wp %s is now wp %s:", r.from, r.to)
}

func isHelpFlag(a string) bool { return a == "-h" || a == "--help" || a == "-help" }

// flagIs reports whether arg is --name, -name or --name=VALUE.
func flagIs(arg, name string) bool {
	a := strings.TrimLeft(arg, "-")
	return len(a) < len(arg) && (a == name || strings.HasPrefix(a, name+"="))
}

// takeFlag removes --name VALUE (or --name=VALUE) from args. With optional,
// the value may be left out (a following word that starts with a dash, or
// none, is not taken for it).
func takeFlag(args []string, name string, optional bool) (rest []string, val string, found bool, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if !flagIs(a, name) {
			continue
		}
		rest = append(slices.Clone(args[:i]), args[i+1:]...)
		if _, v, ok := strings.Cut(a, "="); ok {
			return rest, v, true, nil
		}
		if i+1 < len(args) && !(optional && strings.HasPrefix(args[i+1], "-")) {
			return append(slices.Clone(args[:i]), args[i+2:]...), args[i+1], true, nil
		}
		if !optional {
			return args, "", true, fmt.Errorf("--%s needs a value", name)
		}
		return rest, "", true, nil
	}
	return args, "", false, nil
}

// cmdReportUsage is wp report usage: the spend report, or with --task one
// task's, or with --session one session's (what wp burn showed).
func cmdReportUsage(c *cli, args []string) error {
	if slices.ContainsFunc(args, isHelpFlag) {
		return cmdUsageWhere(c, args)
	}
	rest, sess, hasSess, err := takeFlag(args, "session", true)
	if err != nil {
		return c.usage("%v", err)
	}
	rest, task, hasTask, err := takeFlag(rest, "task", false)
	if err != nil {
		return c.usage("%v", err)
	}
	switch {
	case hasSess && hasTask:
		return c.usage("--session and --task are two reports; give one")
	case hasSess:
		if sess != "" {
			rest = append([]string{"--session", sess}, rest...)
		}
		return cmdBurn(c, rest)
	case hasTask:
		return cmdUsageTask(c, append([]string{task}, rest...))
	}
	return cmdUsageWhere(c, rest)
}

// recordUsageCmd is wp record usage: a task's tokens, or with
// --from-transcripts what Claude Code sessions used.
var recordUsageCmd = &command{
	name: "usage", args: "[KEY] --tokens-in N --tokens-out N [--cost USD] | --from-transcripts [--all] [--since 30d] [--dry-run] [--dir CLAUDE_CONFIG_DIR]",
	summary: "record a task's token usage, or upload what Claude Code sessions used",
	about: `With Claude Code, wp hook claude reports usage for you. KEY defaults to the task taken here.

--from-transcripts counts what each session used from Claude Code's transcripts and uploads it (what wp usage sync did): see wp record usage --from-transcripts -h.`,
	examples: []string{"wp record usage --tokens-in 12000 --tokens-out 800 --cost 0.21", "wp record usage --from-transcripts --all --dry-run"},
	run:      cmdRecordUsage,
}

// transcriptsHelp is wp record usage --from-transcripts -h.
var transcriptsHelp = &command{name: "usage --from-transcripts", args: usageSyncCmd.args, summary: usageSyncCmd.summary, about: usageSyncCmd.about,
	examples: []string{"wp record usage --from-transcripts --all --dry-run"}}

func cmdRecordUsage(c *cli, args []string) error {
	rest, _, from, err := takeFlag(args, "from-transcripts", true)
	if err != nil {
		return c.usage("%v", err)
	}
	if from {
		if slices.ContainsFunc(rest, isHelpFlag) {
			transcriptsHelp.parent = c.cur.parent
			c.cur = transcriptsHelp
		}
		return cmdUsageSync(c, rest)
	}
	return cmdUsage(c, args)
}

// aiSurplusCmd is wp report ai surplus.
var aiSurplusCmd = &command{name: "surplus", args: "",
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
	examples: []string{"wp report ai surplus", "wp report ai surplus --json"},
	run:      cmdAISurplus,
}

// cmdSubjects is wp report, wp record and wp report behavior: with nothing
// after it, the list of what it holds; with a word that is none of them, the
// list again, the closest first.
func cmdSubjects(c *cli, args []string) error {
	cmd := c.cur
	if len(args) > 0 && isHelpFlag(args[0]) {
		return c.usage("help")
	}
	noun := "subject"
	if cmd.name == "behavior" {
		noun = "topic"
	}
	subs := slices.Clone(cmd.subs)
	if len(args) == 0 {
		c.println(strings.TrimSpace(fmt.Sprintf("wp %s %s", cmd.path(), strings.ToUpper(noun))) + ": " + cmd.summary)
		c.println()
		printSubjectList(c.out, subs)
		return nil
	}
	word := args[0]
	slices.SortStableFunc(subs, func(a, b *command) int { return nameDistance(word, a) - nameDistance(word, b) })
	_, _ = fmt.Fprintf(c.err, "wp: unknown %s %s %q; the closest is wp %s %s:\n", cmd.name, noun, word, cmd.path(), subs[0].name)
	printSubjectList(c.err, subs)
	return exitCode(2)
}

func printSubjectList(w io.Writer, subs []*command) {
	width := 0
	for _, s := range subs {
		width = max(width, len(s.path())+3)
	}
	for _, s := range subs {
		_, _ = fmt.Fprintf(w, "  %-*s %s\n", width, "wp "+s.path(), s.summary)
	}
}

// nameDistance is how far a word is from a command's name or aliases: the
// edit distance, less when one starts the other.
func nameDistance(word string, cmd *command) int {
	best := math.MaxInt
	for _, n := range append([]string{cmd.name}, cmd.aliases...) {
		d := editDistance(word, n) * 2
		if strings.HasPrefix(n, word) || strings.HasPrefix(word, n) {
			d--
		}
		best = min(best, d)
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// acceptWindow lets a report that takes --since take --days N (as --since
// Nd), and one that takes --days take --since Nd, so no one has to know
// which a report was written with. A report's own default window stays.
func acceptWindow(fs *flag.FlagSet) {
	switch {
	case fs.Lookup("since") != nil && fs.Lookup("days") == nil:
		fs.Func("days", "(hidden) N days back: --since Nd", func(v string) error {
			if n, err := strconv.ParseFloat(v, 64); err != nil || n < 0 {
				return fmt.Errorf("--days is a number of days, not %q", v)
			}
			return fs.Set("since", v+"d")
		})
	case fs.Lookup("days") != nil && fs.Lookup("since") == nil:
		fs.Func("since", "(hidden) how far back: 7d, 12h, 4w: --days N", func(v string) error {
			d, err := parseSince(v)
			if err != nil {
				return err
			}
			return fs.Set("days", strconv.Itoa(max(1, int(math.Ceil(float64(d)/float64(24*time.Hour))))))
		})
	}
}
