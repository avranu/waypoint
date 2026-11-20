package main

import (
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Goals and traces (WP-382, WP-ADR50): what people want a project to do,
// and how each piece of work, down to a line of code, traces back to it.

var goalsCmd = &command{
	name: "goals", group: "Knowledge", args: "[gaps] [FLAGS]",
	aliases: []string{"intent"},
	summary: "the project's goals, how much work traces to each, and what drifts",
	about: `Lists the goals in scope: the project's own, its parents' and global ones,
in the words people wrote them. Under each: how many tasks, pages and notes
trace to it and how firmly (linked by people, through an agent's link, only
by Waypoint's guess), and wording that names technology or code rather than
what people get.

A goal is a design note of kind goal (wp goal add). Work traces to a goal
through serves links (wp goal link), parents, design notes it implements and
[[links]] in its text. Waypoint guesses links for plainly matching work; a
guess counts for little until a person confirms it.

wp goals gaps lists what needs a person: open tasks that trace to no
approved goal (with the goal each likely serves), links no person confirmed,
suggested goals and notes no person approved, drift a person has yet to
answer, and questions worth asking.
Agents: read it before starting work no person asked for, and pivot freely on
how (libraries, tools, design) as long as the work still serves an approved
goal. wp trace shows one item's route.`,
	examples: []string{"wp goals", "wp goals gaps", "wp goals -p APP", "wp goals --json"},
	run: func(c *cli, args []string) error {
		if len(args) > 0 && args[0] == "gaps" {
			return goalsGaps(c, args[1:])
		}
		return goalsList(c, args, "/api/v1/goals")
	},
}

func goalsGaps(c *cli, args []string) error { return goalsList(c, args, "/api/v1/goals/gaps") }

func goalsList(c *cli, args []string, path string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q (wp goals gaps lists what needs a person)", joinArgs(pos))
	}
	project, err := c.knowledgeScope(sf)
	if err != nil {
		return err
	}
	if project == "" || project == "global" {
		return c.usage("which project? Use -p PROJECT")
	}
	return c.textCall(path, url.Values{"project": {project}})
}

var goalCmd = &command{
	name: "goal", group: "Knowledge", args: "SUBCOMMAND",
	summary: "add a goal, link work to one, confirm or dismiss a link",
	about: `A goal says what people get, in one plain sentence: "People find any task
in a second", not "Add a search index". How to get there (technology,
features, decisions) goes in design notes under it.

A person's goal is approved as it is saved. An agent's is a suggestion until
a person approves it (wp design approve), even one written --for a task a
person filed: a goal is what people want, so only a person makes one.

Links: wp goal link says a task (or page, or note) serves a goal. A person's
link counts fully; an agent's counts as an agent's until a person confirms
it (wp goal confirm LINK). Waypoint's own guesses wait the same way. A
dismissed link is never made again by Waypoint or an agent; only a person
linking the pair again brings it back.`,
	examples: []string{
		`wp goal add "People find any task in a second"`,
		`wp goal add "Owners see the week at a glance" -d "Every Monday, without opening five pages."`,
		"wp goal link WP-12 4",
		"wp goal confirm 311",
		"wp goal dismiss 311",
		"wp goal dismiss WP-12 4",
	},
}

func init() {
	goalCmd.subs = []*command{goalAddCmd, goalLinkCmd, goalConfirmCmd, goalDismissCmd}
	goalCmd.run = func(c *cli, args []string) error {
		if len(args) == 0 {
			return goalsList(c, args, "/api/v1/goals")
		}
		return c.usage("no goal command %q (wp goal add|link|confirm|dismiss; wp goals lists them)", args[0])
	}
}

var goalAddCmd = &command{
	name: "add", args: `"GOAL" [-d MARKDOWN] [--parent GOAL]`, aliases: []string{"new"},
	summary: "write a goal (an agent's is a suggestion until a person approves it)",
	about: `Writes a goal in the scope's project (-p global for a global one). Same flags
as wp design new; the kind is goal.`,
	examples: []string{`wp goal add "People find any task in a second"`, `wp goal add "Exports open in any spreadsheet" --parent 4`},
	run: func(c *cli, args []string) error {
		return designNew(c, append([]string{"--kind", "goal"}, args...))
	},
}

var goalLinkCmd = &command{
	name: "link", args: "ITEM GOAL [--tension|--relates]",
	summary: "say an item serves a goal (or works against it)",
	about: `ITEM is an issue key (APP-12), page:4 or design:12; GOAL is the goal's number.
A person's link counts as theirs; an agent's waits for a person to confirm.
--tension says the item works against the goal (it serves another the two pull
apart), so whoever takes it weighs both; --relates says it touches the goal
without serving it. Neither counts towards the goal trace.`,
	examples: []string{"wp goal link WP-12 4", "wp goal link design:30 4", "wp goal link WP-40 7 --tension"},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		tension := fs.Bool("tension", false, "the item works against the goal")
		relates := fs.Bool("relates", false, "the item touches the goal without serving it")
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 2 {
			return c.usage("give the item and the goal: wp goal link WP-12 4")
		}
		id, err := c.noteID(pos[1])
		if err != nil {
			return err
		}
		rel := "serves"
		switch {
		case *tension && *relates:
			return c.usage("--tension or --relates, not both")
		case *tension:
			rel = "tension"
		case *relates:
			rel = "relates"
		}
		return c.addLinks(map[string]any{"ref": pos[0]}, []string{"design:" + strconv.FormatInt(id, 10)}, rel)
	},
}

var goalConfirmCmd = &command{
	name: "confirm", args: "LINK...", aliases: []string{"yes", "accept"},
	summary:  "make an agent's link or Waypoint's guess your own (people only)",
	about:    `LINK is the link number wp goals gaps prints.`,
	examples: []string{"wp goal confirm 311", "wp goal confirm 311 312"},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) == 0 {
			return c.usage("which link? wp goals gaps lists them")
		}
		for _, p := range pos {
			id, ok := numRef(p)
			if !ok {
				return c.usage("%q isn't a link number", p)
			}
			raw, err := c.call("POST", fmt.Sprintf("/api/v1/goals/links/%d/confirm", id), nil)
			if err != nil {
				return err
			}
			if err := c.emit(raw, func() string { return fmt.Sprintf("Confirmed link #%d: it counts as yours.", id) }); err != nil {
				return err
			}
		}
		return nil
	},
}

var goalDismissCmd = &command{
	name: "dismiss", args: "LINK... | ITEM GOAL", aliases: []string{"no", "reject"},
	summary: "drop a link to a goal, or a suggested match, for good (people only)",
	about: `With link numbers, removes those links; a dismissed guess is never made
again. With an ITEM and a GOAL, tells Waypoint that item doesn't serve that
goal, so it stops suggesting it.`,
	examples: []string{"wp goal dismiss 311", "wp goal dismiss WP-12 4"},
	run: func(c *cli, args []string) error {
		fs := c.flags()
		pos, err := c.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) == 0 {
			return c.usage("which link? Give its number, or an item and a goal")
		}
		if _, ok := numRef(pos[0]); !ok {
			if len(pos) != 2 {
				return c.usage("give an item and a goal: wp goal dismiss WP-12 4")
			}
			goal, err := c.noteID(pos[1])
			if err != nil {
				return err
			}
			if _, err := c.call("POST", "/api/v1/goals/dismiss-match", map[string]any{"item": map[string]any{"ref": pos[0]}, "goal": goal}); err != nil {
				return err
			}
			c.println(fmt.Sprintf("Noted: %s doesn't serve goal #%d; Waypoint won't suggest it again.", pos[0], goal))
			return nil
		}
		for _, p := range pos {
			id, ok := numRef(p)
			if !ok {
				return c.usage("%q isn't a link number", p)
			}
			if _, err := c.call("POST", fmt.Sprintf("/api/v1/goals/links/%d/dismiss", id), nil); err != nil {
				return err
			}
			c.println(fmt.Sprintf("Dismissed link #%d.", id))
		}
		return nil
	},
}

// ---- trace ----

var traceCmd = &command{
	name: "trace", group: "Knowledge", args: "KEY | pr:N | SHA | FILE:LINE",
	aliases: []string{"blame"},
	summary: "which goal something serves, through whom, and how far it is from a person",
	about: `Like git blame for intent: follows an item back to the goals it serves and
prints each step with who made it (a person, an agent, Waypoint's guess, a
[[link]] in the text), whether a person approved the goal, how many steps up
the nearest thing a person wrote or approved is, and how often a person
touched the route.

THING is:
  APP-12       a task
  ADR7, 12     a design note (code or number); PL3 a page
  pr:34, #34   a pull request in this repo's project, through its tasks
  SHA          a commit: the tasks its message names, else its pull request
  FILE:LINE    a line of code: git blame finds the commit, then as SHA

Work that traces to no approved goal says so, with the goal it most likely
serves and the command a person runs to link it. Adversarial reviews give
untraced work extra scrutiny; wp risk counts an agent's untraced task as
medium risk.`,
	examples: []string{"wp trace WP-12", "wp trace pr:34", "wp trace 6c416f5", "wp trace internal/core/goals.go:120", "wp trace WP-12 --json"},
	run:      cmdTrace,
}

// taskKeyRE finds task keys in a commit message: WP-12, APP-340.
var taskKeyRE = lazyre.New(`\b[A-Z][A-Z0-9]{1,9}-[0-9]+\b`)

var hexRE = lazyre.New(`^[0-9a-fA-F]{7,40}$`)

func cmdTrace(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("trace what? Give a task key, pr:N, a commit or FILE:LINE")
	}
	ref := pos[0]
	q := url.Values{}
	project := ""
	if sf.project != nil && *sf.project != "" {
		sc, err := c.scopeFor(sf)
		if err != nil {
			return err
		}
		project, _ = sc.home()
	} else {
		project = c.homeProject()
	}
	if project != "" {
		q.Set("project", project)
	}
	repo, _ := gitRepoHere()
	if repo != "" {
		q.Set("repo", repo)
	}
	switch {
	case strings.HasPrefix(ref, "pr:") || strings.HasPrefix(ref, "#") && len(ref) > 1 && strings.Trim(ref[1:], "0123456789") == "":
		q.Set("pull", strings.TrimPrefix(strings.TrimPrefix(ref, "pr:"), "#"))
	case isFileLine(ref):
		file, line, _ := strings.Cut(ref, ":")
		sha, header, err := blameLine(file, line)
		if err != nil {
			return err
		}
		q.Set("line", header)
		commitQuery(q, sha)
	case hexRE.MatchString(ref) && localCommit(ref) != "":
		commitQuery(q, localCommit(ref))
	default:
		q.Set("ref", ref)
	}
	return c.textCall("/api/v1/trace", q)
}

// isFileLine says whether ref is FILE:LINE for a file in this checkout.
func isFileLine(ref string) bool {
	file, line, ok := strings.Cut(ref, ":")
	if !ok || file == "" {
		return false
	}
	if _, err := strconv.Atoi(line); err != nil {
		return false
	}
	_, err := os.Stat(file)
	return err == nil
}

// blameLine finds the commit that last changed a line, and a header line
// naming it: "internal/x.go:12  6c416f5 Robin Lee  Read diagnostics…".
func blameLine(file, line string) (sha, header string, err error) {
	n, _ := strconv.Atoi(line)
	if n <= 0 {
		return "", "", fmt.Errorf("line %q isn't a line number", line)
	}
	out, err := localGit("blame", "--porcelain", "-L", fmt.Sprintf("%d,%d", n, n), "--", file)
	if err != nil {
		return "", "", fmt.Errorf("git blame %s:%d: %w", file, n, err)
	}
	var author, summary, code string
	for i, l := range strings.Split(out, "\n") {
		switch {
		case i == 0:
			sha, _, _ = strings.Cut(l, " ")
		case strings.HasPrefix(l, "author "):
			author = strings.TrimPrefix(l, "author ")
		case strings.HasPrefix(l, "summary "):
			summary = strings.TrimPrefix(l, "summary ")
		case strings.HasPrefix(l, "\t"):
			code = strings.TrimSpace(l[1:])
		}
	}
	if sha == "" || strings.Trim(sha, "0") == "" {
		return "", "", fmt.Errorf("%s:%d isn't committed yet, so it traces to nothing", file, n)
	}
	if r := []rune(code); len(r) > 80 {
		code = string(r[:77]) + "..."
	}
	header = fmt.Sprintf("%s:%d  %s\n  last changed in %s by %s: %s", file, n, code, sha[:10], author, summary)
	return sha, header, nil
}

// localCommit is the full id of a commit in this checkout, or "".
func localCommit(ref string) string {
	out, err := localGit("rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return ""
	}
	return out
}

// commitQuery fills a commit's trace query from this checkout: the tasks
// its message names and the pull request that merged it.
func commitQuery(q url.Values, sha string) {
	q.Set("commit", sha)
	msg, _ := localGit("log", "-1", "--format=%B", sha)
	var keys []string
	for _, k := range taskKeyRE.FindAllString(msg, -1) {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	if len(keys) > 0 {
		q.Set("keys", strings.Join(keys, ","))
	}
	subject, _, _ := strings.Cut(msg, "\n")
	pr := prNumber(subject)
	if pr == 0 {
		// The merge that brought it in names the pull request.
		if merges, err := localGit("log", "--merges", "--ancestry-path", "--reverse", "--format=%s", sha+"..HEAD"); err == nil {
			for _, s := range strings.Split(merges, "\n") {
				if pr = prNumber(s); pr > 0 {
					break
				}
			}
		}
	}
	if pr > 0 {
		q.Set("pull", strconv.Itoa(pr))
	}
}
