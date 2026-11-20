package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// issueTypes and issueCategories are what the server accepts (a test
// checks them against it).
var (
	issueTypes      = []string{"epic", "story", "task", "bug", "subtask"}
	issueCategories = []string{"feature", "bugfix", "improvement", "refactor", "docs", "test", "research", "design", "ops", "security", "chore"}
)

var lsCmd = &command{
	name: "ls", group: "Issues", args: "[SCOPE] [QUERY...] [FLAGS]",
	aliases: []string{"list", "search"},
	summary: "list and search issues",
	about: `One line per issue: KEY [Status] priority type "Title" @assignees points due: …

SCOPE: a first word naming a project or repo, when only key:value filters
follow (wp ls harbor is:closed); otherwise every word is searched for.
QUERY: free words and filters, ANDed: status:"In Review" assignee:me
priority:>=high tag:backend due:<friday updated:>=-7d under:APP-3 -tag:wontfix
order:-priority,due. wp help query has them all.
Open issues only, unless --closed, --any-status or is:/status:/resolved:.`,
	examples: []string{
		"wp ls                              # open issues in this repo's project",
		"wp ls harbor                       # open issues in Harbor",
		"wp ls login redirect               # search here for those words",
		"wp ls --mine --priority high",
		`wp ls -A "assignee:me due:<=friday"  # across every project`,
		"wp ls --closed --sort -resolved -n 10",
		`wp ls "under:APP-3"                 # everything in an epic`,
	},
	run: cmdLs,
}

func cmdLs(c *cli, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	mine := fs.Bool("mine", false, "only issues assigned to you")
	assignee := fs.String("assignee", "", "only issues assigned to `USER` (username, me, or none)")
	status := fs.String("status", "", "only issues in this `STATUS` (name like \"In Review\", or todo, in_progress, done…)")
	short(fs, "s", "status")
	typ := fs.String("type", "", "only this `TYPE`: "+strings.Join(issueTypes, ", "))
	short(fs, "t", "type")
	priority := fs.String("priority", "", "only this `PRIORITY` (none, low, medium, high, urgent; >=high works)")
	category := fs.String("category", "", "only this `CATEGORY`: "+strings.Join(issueCategories, ", "))
	agents := fs.String("for", "", "only tasks for `WHO`: anyone, none (humans only), any (any agent), or an agent like claude")
	var tags listFlag
	fs.Var(&tags, "tag", "only issues with this `TAG` (repeat for all of several)")
	closed := fs.Bool("closed", false, "only closed issues (done or canceled)")
	anyStatus := fs.Bool("any-status", false, "open and closed issues")
	sort := fs.String("sort", "", "order by `FIELD`: priority, due, updated, created, resolved, rank, key, title, points (-field: descending)")
	limit := fs.Int("limit", 50, "show at most `N` issues")
	short(fs, "n", "limit")
	offset := fs.Int("offset", 0, "skip the first `N` (for paging)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	var sc *scope
	if *sf.project == "" && !*sf.all && len(pos) > 0 && mayBeScope(pos[0]) && allFilters(pos[1:]) {
		if s, err := c.matchScope(pos[0]); err != nil {
			return err
		} else if !s.all() {
			if err := s.ambiguous(pos[0]); err != nil {
				return err
			}
			s.from = fmt.Sprintf("%q", pos[0])
			sc, pos = s, pos[1:]
		}
	}
	if sc == nil {
		if sc, err = c.workScope(sf); err != nil {
			return err
		}
	}
	userQuery := joinArgs(pos)
	var terms []string
	add := func(key, v string) {
		if v != "" {
			terms = append(terms, key+":"+quoteTerm(v))
		}
	}
	if q := sc.query(); q != "" {
		terms = append(terms, q)
	}
	if *mine {
		add("assignee", "me")
	}
	add("assignee", *assignee)
	add("status", *status)
	add("type", *typ)
	add("category", *category)
	add("agent", *agents)
	for _, t := range tags {
		add("tag", t)
	}
	if *priority != "" {
		terms = append(terms, "priority:"+*priority)
	}
	switch {
	case *closed:
		terms = append(terms, "is:closed")
	case *anyStatus, *status != "", choosesState(userQuery):
	default:
		terms = append(terms, "is:open")
	}
	if *sort != "" {
		terms = append(terms, "order:"+*sort)
	} else if userQuery == "" {
		terms = append(terms, "order:-priority,due,rank")
	}
	if userQuery != "" {
		terms = append(terms, userQuery)
	}
	q := url.Values{"q": {strings.Join(terms, " ")}, "limit": {strconv.Itoa(*limit)}, "offset": {strconv.Itoa(*offset)}}
	if !c.asJSON {
		q.Set("format", "text")
	}
	raw, err := c.call("GET", "/api/v1/issues?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	c.note("Issues in %s.", sc)
	return c.emit(raw, func() string { return strings.Replace(string(raw), "use offset=", "use --offset ", 1) })
}

// allFilters says whether every word is a key:value filter, so that a word
// before them can only be a scope: "wp ls fix login" searches for "fix".
func allFilters(words []string) bool {
	for _, w := range words {
		if !strings.Contains(w, ":") {
			return false
		}
	}
	return true
}

// mayBeScope says whether ls's first word could name a project or repo,
// rather than being a filter or an issue key.
func mayBeScope(w string) bool {
	return w != "" && !strings.ContainsAny(w, ":\"") && !strings.HasPrefix(w, "-") && !keyDashRE.MatchString(strings.ToUpper(w))
}

// choosesState says whether a query picks open/closed itself.
func choosesState(q string) bool {
	for _, f := range strings.Fields(strings.ToLower(q)) {
		f = strings.TrimPrefix(f, "-")
		for _, p := range []string{"is:", "status:", "s:", "resolved:", "key:"} {
			if strings.HasPrefix(f, p) {
				return true
			}
		}
		if keyRE.MatchString(strings.ToUpper(f)) {
			return true
		}
	}
	return false
}

// quoteTerm quotes a filter value with spaces: status:"In Review".
func quoteTerm(v string) string {
	if strings.ContainsAny(v, " \t") && !strings.HasPrefix(v, `"`) {
		return strconv.Quote(v)
	}
	return v
}

var showCmd = &command{
	name: "show", group: "Issues", args: "[CODE...]",
	aliases: []string{"view"},
	summary: "show issues, requests, decisions, pages or pull requests in full",
	about: `An issue: description, comments, then prep pointers, children,
relations and links; attachments are login-gated links (wp files KEY saves
them locally). CODE defaults to your current task. Other codes show what
they name, as their own show command would: R-12 a request, ADR7 a
decision note, PL13 a page, #12 a pull request. A code that names more
than one thing (R1: request R-1 or page R1) lists the commands to pick
from; issue:, request:, design:, page: and pr: say which.`,
	examples: []string{"wp show APP-12 APP-15", "wp show R-12 ADR7 PL13", "wp show   # the task you took"},
	run:      cmdShow,
}

// withPrep puts a task's prep into wp show's text after its comments (which
// follow the description, WP-449) and before the lists that can run long
// (sub-tasks, relations, links), so wp show | head still meets it; after the
// description when there are no comments. A comment's later lines are
// indented, so the first blank line ends the comments.
func withPrep(text, prep string) string {
	at := strings.Index(text, "\nComments:\n  [#")
	if at < 0 {
		at = strings.Index(text, "\nChildren:\n")
	} else if end := strings.Index(text[at+1:], "\n\n"); end >= 0 {
		at += 1 + end + 1
	} else {
		at = len(text)
	}
	for _, list := range []string{"\nRelations:\n", "\nLinks:\n"} {
		if at < 0 {
			at = strings.Index(text, list)
		}
	}
	if at < 0 || at >= len(text) {
		return strings.TrimRight(text, "\n") + "\n\n" + prep
	}
	return text[:at] + "\n" + prep + text[at:]
}

// issueSummary is enough of an issue to confirm what happened.
type issueSummary struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Status struct {
		Name string `json:"name"`
	} `json:"status"`
	Step  string `json:"step"`
	Draft bool   `json:"draft"`
	Hold  *struct {
		Until *time.Time `json:"until"`
		Waits string     `json:"waits"`
		Note  string     `json:"note"`
	} `json:"hold"`
	Assignees []struct {
		Username string `json:"username"`
	} `json:"assignees"`
	FiledFrom *struct {
		Here bool `json:"here"`
	} `json:"filed_from"`
}

func (c *cli) confirmIssue(raw []byte, verb string) error {
	return c.emit(raw, func() string {
		var i issueSummary
		if err := jsonInto(raw, &i); err != nil {
			return verb
		}
		s := fmt.Sprintf("%s %s %s [%s]", verb, i.Key, i.Title, i.Status.Name)
		if i.Step != "" {
			s += " step:" + i.Step
		}
		if i.Draft {
			s += " draft (agents can't see it until you wp publish it)"
		}
		for _, a := range i.Assignees {
			s += " @" + a.Username
		}
		if h := i.Hold; h != nil {
			s += " on hold until "
			switch {
			case h.Until != nil && h.Waits != "":
				s += h.Until.Local().Format("2006-01-02 15:04 MST") + " and " + h.Waits
			case h.Until != nil:
				s += h.Until.Local().Format("2006-01-02 15:04 MST")
			case h.Waits != "":
				s += h.Waits
			default:
				s += "someone lifts it"
			}
			if h.Note != "" {
				s += " (" + h.Note + ")"
			}
			s += ": no one can take it until then"
		}
		if f := i.FiledFrom; f != nil && f.Here && verb == "Created" {
			// WP-378: the filer learns the other side may turn it down.
			s += "\nFiled for another repo: its agents may decline it."
		}
		return s + "\n" + c.server() + "/issue/" + i.Key
	})
}

// fieldFlags are the issue fields new and edit share.
type fieldFlags struct {
	desc, file, typ, agents, category, priority, parent, workflow, due, start, status, points *string
	tags, assign                                                                              *listFlag
}

func addFieldFlags(fs *flag.FlagSet, editing bool) fieldFlags {
	none := ""
	if editing {
		none = ", or none to clear it"
	}
	f := fieldFlags{
		desc:     fs.String("desc", "", "description (`MARKDOWN`)"),
		file:     fs.String("file", "", "read the description from `FILE` (- for stdin)"),
		typ:      fs.String("type", "", "`TYPE`: "+strings.Join(issueTypes, ", ")+map[bool]string{false: " (default task)"}[editing]),
		agents:   fs.String("for", "", "who may take it: anyone, none, any, or an agent (`WHO`)"),
		category: fs.String("category", "", "`CATEGORY`: "+strings.Join(issueCategories, ", ")+none),
		priority: fs.String("priority", "", "`PRIORITY`: none, low, medium, high or urgent"),
		parent:   fs.String("parent", "", "parent issue `KEY` (an epic or story)"+none),
		workflow: fs.String("workflow", "", "`WORKFLOW` name"+none),
		due:      fs.String("due", "", "due `DATE`: 2026-10-20, friday, +3d"+none),
		start:    fs.String("start", "", "start `DATE`, as for --due"+none),
		status:   fs.String("status", "", "`STATUS` name or category"),
		points:   fs.String("points", "", "story `POINTS`"+none),
		tags:     &listFlag{},
		assign:   &listFlag{},
	}
	short(fs, "d", "desc")
	short(fs, "f", "file")
	short(fs, "t", "type")
	alias(fs, "description", "desc")
	alias(fs, "body-file", "file")
	return f
}

func (f fieldFlags) register(fs *flag.FlagSet, editing bool) {
	defer alias(fs, "label", "tag")
	if editing {
		fs.Var(f.tags, "tag", "add a `TAG` (repeat, or comma-separate)")
		fs.Var(f.assign, "assign", "add an assignee: `USER` or me (repeatable)")
		return
	}
	fs.Var(f.tags, "tag", "`TAG` it (repeat, or comma-separate)")
	fs.Var(f.assign, "assign", "assign to `USER` or me (repeatable)")
}

// body is the fields set, as the API names them.
func (f fieldFlags) body(editing bool) (map[string]any, error) {
	b := map[string]any{}
	// agent and category take "none" as it is.
	clearable := map[string]bool{"parent": true, "workflow": true, "due_date": true, "start_date": true}
	for field, v := range map[string]*string{"description": f.desc, "type": f.typ, "agent": f.agents, "category": f.category,
		"priority": f.priority, "parent": f.parent, "workflow": f.workflow, "due_date": f.due, "start_date": f.start, "status": f.status} {
		switch {
		case *v == "":
		case editing && clearable[field] && strings.EqualFold(*v, "none"):
			b[field] = nil
		default:
			b[field] = *v
		}
	}
	if *f.file != "" {
		text, err := readBody(*f.file)
		if err != nil {
			return nil, err
		}
		b["description"] = text
	}
	switch {
	case *f.points == "":
	case editing && strings.EqualFold(*f.points, "none"):
		b["points"] = nil
	default:
		n, err := strconv.ParseFloat(*f.points, 64)
		if err != nil {
			return nil, fmt.Errorf("--points must be a number%s", map[bool]string{true: " or none"}[editing])
		}
		b["points"] = n
	}
	return b, nil
}

var newCmd = &command{
	name: "new", group: "Issues", args: `[TYPE] "TITLE" [FLAGS]`,
	aliases: []string{"create"},
	summary: "create an issue",
	about: `Prints the new key and link. TYPE is one of: ` + strings.Join(issueTypes, ", ") + ` (default task).

--for: anyone (default), none (humans only), any (any agent), or an agent
like claude or "haiku 5.5" (wp next offers it to a match first).
--status backlog: wp next skips it; anyone can still take it by name.
--hold: nobody takes it until a date or lifted (wp help hold).
--draft: hidden from agents until published.
A likely duplicate of an open issue stops the filing; --new files anyway.`,
	examples: []string{
		`wp new bug "Login page crashes on Safari" --priority high --tag ui`,
		`wp new "Write the parser" --for claude --workflow "Agent pipeline"`,
		`wp new "Add tests" --parent APP-3 -f notes.md`,
		`wp new "Rerun the eval" --hold +3d`,
	},
	run: cmdNew,
}

var holdCmd = &command{
	name: "hold", group: "Issues", args: "[KEY] [UNTIL] [-m WHY] [--after KEY,KEY] [--request R-N,R-M] [--reviewed] [--person] | [KEY] --off",
	summary: "hold a task: no one takes it until a date, a condition or lifted",
	about: `Nobody can take it, even by name. UNTIL is a date (2026-11-03, friday, +3d;
start of day, your time zone) or a time (2026-11-03T09:00:00Z); without
one it lasts until --off. --after waits for tasks to close, --request for
requests to be answered, --reviewed for a person to review it, --person
for a person to lift it (for stale work or work against the project's
goals). All of them must be met; it lifts itself when they are. Holding
the task you took ends your claim. Only a person can change or lift a
hold a person placed. List: wp ls is:on-hold.
To park work that may still be taken by name, use wp mv KEY backlog.`,
	examples: []string{
		`wp hold WP-12 +3d -m "after the weekly quota refreshes"`,
		`wp hold WP-12 --after WP-9,APP-4 --request R-146`,
		"wp hold WP-12 --off",
	},
	run: cmdHold,
}

func cmdHold(c *cli, args []string) error {
	fs := c.flags()
	note := fs.String("m", "", "why (`WHY`)")
	alias(fs, "message", "m")
	off := fs.Bool("off", false, "lift the hold")
	waits := holdFlags(fs)
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	key, rest, err := c.currentKey(pos)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return c.usage("expected [KEY] [UNTIL]")
	}
	until := strings.Join(rest, "")
	if *off && (until != "" || *note != "" || waits.any()) {
		return c.usage("--off lifts the hold; it takes no UNTIL, -m or conditions")
	}
	if *off {
		raw, err := c.call("DELETE", "/api/v1/issues/"+esc(key)+"/hold", nil)
		if err != nil {
			return err
		}
		return c.confirmIssue(raw, "Lifted the hold on")
	}
	body := map[string]any{"until": until, "note": *note}
	waits.into(body)
	raw, err := c.call("PUT", "/api/v1/issues/"+esc(key)+"/hold", body)
	if err != nil {
		return err
	}
	c.forgetClaim(key) // a claim on it ended with the hold
	return c.confirmIssue(raw, "Put on hold:")
}

var publishCmd = &command{
	name: "publish", group: "Issues", args: "KEY...",
	summary:  "(people) publish a draft so agents can see it",
	about:    `Also publishes the drafts under it. Only a person can. List: wp ls is:draft.`,
	examples: []string{"wp publish APP-12"},
	run:      cmdPublish,
}

var draftCmd = &command{
	name: "draft", group: "Issues", args: "KEY...",
	summary: "make an issue (and its children) a draft again",
	about: `Hidden from agents until a person publishes it. Refused while an agent
works on it.`,
	examples: []string{"wp draft APP-12"},
	run:      cmdDraft,
}

func cmdDraft(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("expected an issue KEY")
	}
	return c.each(listArgs(pos), false, func(key string) error {
		raw, err := c.call("POST", "/api/v1/issues/"+esc(key)+"/draft", nil)
		if err != nil {
			return err
		}
		return c.confirmIssue(raw, "Made a draft:")
	})
}

func cmdPublish(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("expected an issue KEY")
	}
	return c.each(listArgs(pos), false, func(key string) error {
		raw, err := c.call("POST", "/api/v1/issues/"+esc(key)+"/publish", nil)
		if err != nil {
			return err
		}
		return c.confirmIssue(raw, "Published")
	})
}

func cmdNew(c *cli, args []string) error {
	fs := c.flags()
	project := fs.String("project", "", "create in this `PROJECT`")
	short(fs, "p", "project")
	f := addFieldFlags(fs, false)
	f.register(fs, false)
	draft := fs.Bool("draft", false, "a draft, hidden from agents")
	hold := fs.String("hold", "", "hold until `WHEN` (date, or indefinitely)")
	anyway := fs.Bool("new", false, "file even if a similar issue is open")
	of := addObjectiveFlags(fs, false)
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	obj := of.body(fs)
	if len(obj) > 0 && obj["check"] == nil && obj["judge"] == nil && obj["until"] == nil {
		return c.usage("--keep-passing goes with a --check or a --judge")
	}
	if len(pos) >= 2 && slices.Contains(issueTypes, strings.ToLower(pos[0])) && *f.typ == "" {
		*f.typ, pos = strings.ToLower(pos[0]), pos[1:]
	}
	if len(pos) == 0 {
		return c.usage("give a title")
	}
	body, err := f.body(false)
	if err != nil {
		return c.usage("%s", err.Error())
	}
	body["title"] = joinArgs(pos)
	if *draft {
		body["draft"] = true
	}
	if *hold != "" {
		body["hold"] = map[string]any{"until": *hold}
	}
	if len(*f.tags) > 0 {
		body["labels"] = []string(*f.tags)
	}
	if len(*f.assign) > 0 {
		body["assignees"] = []string(*f.assign)
	}
	// A sub-task's project comes from its parent unless one is named.
	if *project != "" || *f.parent == "" {
		sc, err := c.scopeFor(scopeFlags{project: project})
		if err != nil {
			return err
		}
		if body["project"], err = sc.home(); err != nil {
			return err
		}
	}
	similar := c.similarIssues(body)
	if !*anyway {
		if err := sameWorkHold(similar); err != nil {
			return err
		}
	}
	raw, err := c.call("POST", "/api/v1/issues", body)
	if err != nil {
		return err
	}
	if err := c.confirmIssue(raw, "Created"); err != nil {
		return err
	}
	var made issueSummary
	parsed := jsonInto(raw, &made) == nil
	if len(obj) > 0 {
		if made.Key == "" {
			return fmt.Errorf("the task was filed, but its key wasn't read, so its objective wasn't set: wp objective set KEY …")
		}
		if err := c.setNewObjective(made.Key, obj); err != nil {
			return err
		}
	}
	if c.asJSON || !parsed {
		return nil
	}
	c.print(similarHints(made.Key, similar))
	return nil
}

// similarIssue is an open issue that reads like one about to be filed
// (POST /issues/similar, WP-104).
type similarIssue struct {
	Key        string `json:"key"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	Confidence string `json:"confidence"`
	Same       bool   `json:"same"`
	ClaimedBy  string `json:"claimed_by"`
	Reason     string `json:"reason"`
}

// similarIssues asks which open issues read like the new one; a server
// that can't say (an older one) gets nothing in the way.
func (c *cli) similarIssues(body map[string]any) []similarIssue {
	q := map[string]any{"title": body["title"], "description": body["description"], "project": body["project"], "parent": body["parent"]}
	raw, err := c.call("POST", "/api/v1/issues/similar", q)
	if err != nil {
		return nil
	}
	var out []similarIssue
	if jsonInto(raw, &out) != nil {
		return nil
	}
	return out
}

// sameWorkHold refuses to file what an open issue in the project already
// is, pointing at that one instead: work filed twice leaves the first copy
// in To Do after the second is done.
func sameWorkHold(similar []similarIssue) error {
	for _, m := range similar {
		if !m.Same {
			continue
		}
		take := "Take it with wp next " + m.Key
		if m.ClaimedBy != "" {
			take = m.ClaimedBy + " is working on it (wp show " + m.Key + ")"
		}
		return fmt.Errorf("not filed: %s [%s] %q reads like the same work (%s)\n%s, or file this anyway with --new",
			m.Key, m.Status, m.Title, strings.TrimSuffix(m.Reason, "."), take)
	}
	return nil
}

// similarHints lists the open issues like the one just filed, with how to
// link one.
func similarHints(key string, similar []similarIssue) string {
	if len(similar) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Open issues like it:\n")
	for _, m := range similar {
		fmt.Fprintf(&b, "  %s [%s] %q (%s: %s)\n", m.Key, m.Status, m.Title, m.Confidence, strings.TrimSuffix(m.Reason, "."))
	}
	fmt.Fprintf(&b, "Link one with wp relate %s relates %s\n", key, similar[0].Key)
	return b.String()
}

var editCmd = &command{
	name: "edit", group: "Issues", args: "[KEY...] [FLAGS]",
	aliases: []string{"update"},
	summary: "change an issue's fields",
	about: `Changes only the fields given; none clears one (--due none). Several KEYs
get the same change, except title and description. KEY defaults to your
current task.`,
	examples: []string{
		"wp edit APP-12 --priority high --due friday",
		`wp edit APP-12 --title "New title" --tag safari --untag ui`,
		`wp edit APP-12 --intent "Let people find a task by what it's for"`,
		"wp edit APP-12 APP-13 --for none   # humans only",
		"wp edit APP-12 -p TOOLS           # move, with its sub-tasks",
	},
	run: cmdEdit,
}

func cmdEdit(c *cli, args []string) error {
	fs := c.flags()
	title := fs.String("title", "", "new `TITLE`")
	intent := fs.String("intent", "", "what it's for in a sentence or two (`TEXT`), or none to clear it; an agent's stays a suggestion until a person keeps it")
	f := addFieldFlags(fs, true)
	f.register(fs, true)
	step := fs.String("step", "", "workflow `STEP`")
	sprint := fs.String("sprint", "", "`SPRINT` name, active, next, or none")
	project := fs.String("project", "", "move to this `PROJECT`")
	short(fs, "p", "project")
	msg := fs.String("message", "", "comment to save with the change (`TEXT`)")
	short(fs, "m", "message")
	var untags, unassign listFlag
	fs.Var(&untags, "untag", "remove a `TAG` (repeat)")
	fs.Var(&unassign, "unassign", "remove an assignee `USER` (repeat)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	keys, rest, err := c.currentKeys(pos)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return c.usage("unexpected %q (fields are flags, e.g. --title)", joinArgs(rest))
	}
	if len(keys) > 1 && (*title != "" || *intent != "" || *f.desc != "" || *f.file != "") {
		return c.usage("a title, intent or description is one issue's: give one KEY")
	}
	patch, err := f.body(true)
	if err != nil {
		return c.usage("%s", err.Error())
	}
	for field, v := range map[string]string{"title": *title, "intent": *intent, "step": *step, "sprint": *sprint, "comment": *msg} {
		if v == "" {
			continue
		}
		if field == "intent" && strings.EqualFold(v, "none") {
			patch[field] = ""
		} else if field == "sprint" && strings.EqualFold(v, "none") {
			patch[field] = nil
		} else {
			patch[field] = v
		}
	}
	for field, l := range map[string]listFlag{"add_labels": *f.tags, "remove_labels": untags, "add_assignees": *f.assign, "remove_assignees": unassign} {
		if len(l) > 0 {
			patch[field] = []string(l)
		}
	}
	if len(patch) == 0 && *project == "" {
		return c.usage("nothing to change; see wp edit -h for the fields")
	}
	home := ""
	if *project != "" {
		sc, err := c.findScope(*project, "-p")
		if err != nil {
			return err
		}
		if home, err = sc.home(); err != nil {
			return err
		}
	}
	return c.each(keys, false, func(key string) error {
		var raw []byte
		var err error
		if home != "" {
			if raw, err = c.call("POST", "/api/v1/issues/"+esc(key)+"/move", map[string]string{"project": home}); err != nil {
				return err
			}
			var moved issueSummary
			if jsonInto(raw, &moved) == nil && moved.Key != "" {
				key = moved.Key // a move gives it a new key
			}
		}
		if len(patch) > 0 {
			if raw, err = c.call("PATCH", "/api/v1/issues/"+esc(key), patch); err != nil {
				return err
			}
		}
		return c.confirmIssue(raw, "Updated")
	})
}

var mvCmd = &command{
	name: "mv", group: "Issues", args: "[KEY...] STATUS [-m COMMENT]",
	aliases: []string{"move"},
	summary: "move an issue to a status (In Review, done, …)",
	about: `STATUS is a name ("In Review") or category: backlog, todo, in_progress,
done, canceled. KEY defaults to your current task. wp ship finishes a
task you took.`,
	examples: []string{`wp mv APP-12 "In Review"`, `wp mv APP-12 APP-13 canceled -m "dup"`},
	run:      cmdMv,
}

func cmdMv(c *cli, args []string) error {
	fs := c.flags()
	msg := fs.String("message", "", "comment to save with the move (`TEXT`)")
	short(fs, "m", "message")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	keys, rest, err := c.currentKeys(pos)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return c.usage("which status?")
	}
	return c.each(keys, false, func(key string) error {
		raw, err := c.call("POST", "/api/v1/issues/"+esc(key)+"/transition", map[string]string{"status": joinArgs(rest), "comment": *msg})
		if err != nil {
			return err
		}
		return c.confirmIssue(raw, "Moved")
	})
}

var assignCmd = &command{
	name: "assign", group: "Issues", args: "[KEY] [USER...]",
	summary: "assign an issue to people (you by default)",
	about: `USER is a username; me (default) is you. Assigning someone outside the
project shares the issue with them.`,
	examples: []string{"wp assign APP-12 sam robin"},
	run:      func(c *cli, args []string) error { return assign(c, args, "add_assignees", "Assigned") },
}

var unassignCmd = &command{
	name: "unassign", group: "Issues", args: "[KEY] [USER...]",
	summary:  "remove assignees (you by default)",
	examples: []string{"wp unassign APP-12 sam"},
	run:      func(c *cli, args []string) error { return assign(c, args, "remove_assignees", "Updated") },
}

func assign(c *cli, args []string, field, verb string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	key, users, err := c.currentKey(pos)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		users = []string{"me"}
	}
	raw, err := c.call("PATCH", "/api/v1/issues/"+esc(key), map[string]any{field: users})
	if err != nil {
		return err
	}
	return c.confirmIssue(raw, verb)
}

var commentCmd = &command{
	name: "comment", group: "Issues", args: `[KEY] "TEXT" | [KEY] -f FILE | --edit ID "TEXT" | --delete ID`,
	summary: "comment on an issue (markdown; @username notifies)",
	about: `-f - reads stdin. KEY defaults to your current task, unless the first word
looks like another code (R-28, ADR7, a page): that is refused. Requests
take no comments. --edit and --delete take the comment number printed
here and in wp show; agents can change only agents' comments.`,
	examples: []string{`wp comment APP-12 "Deployed to staging"`, "wp comment --delete 412", `wp comment --edit 412 "Deployed to production"`},
	run:      cmdComment,
}

func cmdComment(c *cli, args []string) error {
	fs := c.flags()
	file := fs.String("file", "", "read the comment from `FILE` (- for stdin)")
	short(fs, "f", "file")
	alias(fs, "body-file", "file")
	edit := fs.String("edit", "", "replace comment `ID`")
	del := fs.String("delete", "", "delete comment `ID`")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if *edit != "" || *del != "" {
		return cmdCommentChange(c, *edit, *del, *file, pos)
	}
	key, rest, err := c.currentKey(pos)
	if err != nil {
		return err
	}
	text := joinArgs(rest)
	if *file != "" {
		if text, err = readBody(*file); err != nil {
			return err
		}
	}
	if strings.TrimSpace(text) == "" {
		return c.usage("the comment is empty")
	}
	raw, err := c.call("POST", "/api/v1/issues/"+esc(key)+"/comments", map[string]string{"body": text})
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var made struct {
			ID int64 `json:"id"`
		}
		if json.Unmarshal(raw, &made) == nil && made.ID > 0 {
			return fmt.Sprintf("Commented on %s (comment %d)", key, made.ID)
		}
		return "Commented on " + key
	})
}

// cmdCommentChange is wp comment --edit / --delete: the comment is named by
// its number, never by the task taken here.
func cmdCommentChange(c *cli, edit, del, file string, pos []string) error {
	if edit != "" && del != "" {
		return c.usage("--edit and --delete contradict each other")
	}
	which := edit
	if del != "" {
		which = del
	}
	id, ok := numRef(which)
	if !ok {
		return c.usage("%q isn't a comment number (wp show lists them)", which)
	}
	if del != "" {
		if len(pos) > 0 || file != "" {
			return c.usage("--delete takes only the comment's number")
		}
		if _, err := c.call("DELETE", fmt.Sprintf("/api/v1/comments/%d", id), nil); err != nil {
			return err
		}
		c.println(fmt.Sprintf("Deleted comment %d.", id))
		return nil
	}
	text := joinArgs(pos)
	if file != "" {
		var err error
		if text, err = readBody(file); err != nil {
			return err
		}
	}
	if strings.TrimSpace(text) == "" {
		return c.usage("the new text is empty")
	}
	if _, err := c.call("PATCH", fmt.Sprintf("/api/v1/comments/%d", id), map[string]string{"body": text}); err != nil {
		return err
	}
	c.println(fmt.Sprintf("Changed comment %d.", id))
	return nil
}

var linkCmd = &command{
	name: "link", group: "Issues", args: "[KEY] URL [--title T]",
	summary: "attach a link (PR, commit, doc) to an issue",
	about: `GitHub, GitLab and Forgejo PRs, issues and commits show their live state.
To relate two issues, use wp relate.`,
	examples: []string{"wp link APP-12 https://github.com/robin/harbor/pull/42"},
	run:      cmdLink,
}

func cmdLink(c *cli, args []string) error {
	fs := c.flags()
	title := fs.String("title", "", "link `TITLE` (forge links name themselves)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	key, rest, err := c.currentKey(pos)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return c.usage("expected one URL")
	}
	raw, err := c.call("POST", "/api/v1/issues/"+esc(key)+"/links", map[string]string{"url": rest[0], "title": *title})
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return "Linked " + rest[0] + " to " + key })
}

var relateCmd = &command{
	name: "relate", group: "Issues", args: "KEY RELATION OTHER... [--as SUBKIND]",
	summary: "relate two issues: blocks, relates, duplicates, follows, supersedes, tension",
	about: `RELATION: blocks (KEY before OTHER), blocked-by, relates, duplicates (KEY
repeats OTHER), duplicated-by, follows (KEY is a follow-up of OTHER: more work
on what OTHER did), followed-by, supersedes (KEY replaces OTHER's plan or work,
so undoing OTHER's work is intended), superseded-by, tension (the two pull
against each other: whoever takes either should weigh both). Blocked tasks
aren't handed out until blockers are done.
--as narrows a follow-up: fix (mends what OTHER got wrong or missed) or
expansion (more of what OTHER did). Relating a pair again with --as changes it.
Waypoint also records follow-ups itself: a task an agent files while working
on another follows it, and so does one whose merged pull request changed
mostly the files a recently finished task's did. "relates" between a task and
one that was done before it was filed records a follow-up. is:followup, is:followed,
is:tension and is:superseded find them.`,
	examples: []string{"wp relate APP-12 blocks APP-15 APP-16", "wp relate APP-20 duplicates APP-4", "wp relate APP-31 follows APP-12 --as fix",
		"wp relate APP-40 tension APP-18", "wp relate APP-44 supersedes APP-9"},
	run: cmdRelate,
}

func cmdRelate(c *cli, args []string) error {
	fs := c.flags()
	as := fs.String("as", "", "`SUBKIND` of the relation: fix or expansion, for follows")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 3 {
		return c.usage("expected KEY RELATION OTHER")
	}
	kind := normalRelation(pos[1])
	if kind == "" {
		return c.usage("RELATION is blocks, blocked-by, relates, duplicates, duplicated-by, follows, followed-by, supersedes, superseded-by or tension")
	}
	sub := strings.ToLower(strings.TrimSpace(*as))
	key := strings.ToUpper(pos[0])
	return c.each(listArgs(pos[2:]), false, func(other string) error {
		other = strings.ToUpper(other)
		raw, err := c.call("POST", "/api/v1/issues/"+esc(key)+"/relations", map[string]string{"kind": kind, "issue": other, "subkind": sub})
		if err != nil {
			return err
		}
		var made struct{ Kind, Found string }
		_ = json.Unmarshal(raw, &made)
		return c.emit(raw, func() string {
			said := strings.ReplaceAll(cmp.Or(made.Kind, kind), "_", " ")
			if sub != "" {
				said += " (" + sub + ")"
			}
			out := fmt.Sprintf("%s %s %s", key, said, other)
			if made.Found == "after" {
				// WP-1003: say why it became a follow-up.
				earlier, later := other, key
				if made.Kind == "followed_by" {
					earlier, later = key, other
				}
				out += fmt.Sprintf(" (a follow-up: %s was done before %s was filed; `wp relate %s follows %s --as fix|expansion` says which)", earlier, later, later, earlier)
			}
			return out
		})
	})
}

// normalRelation turns what people type into the API's relation kinds.
func normalRelation(s string) string {
	k := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(s))
	switch k {
	case "blocks", "block":
		return "blocks"
	case "blocked_by", "blocked":
		return "blocked_by"
	case "relates", "relates_to", "related", "relates_with":
		return "relates"
	case "duplicates", "duplicate", "dupe", "dup":
		return "duplicates"
	case "duplicated_by", "duplicated":
		return "duplicated_by"
	case "follows", "follow_up", "followup", "follows_up", "follow_up_of", "followup_of", "follows_up_on":
		return "follows"
	case "followed_by", "follow_ups", "followups":
		return "followed_by"
	case "supersedes", "supersede", "replaces":
		return "supersedes"
	case "superseded_by", "superseded", "replaced_by":
		return "superseded_by"
	case "tension", "in_tension_with", "tension_with", "conflicts", "conflicts_with", "conflict":
		return "tension"
	}
	return ""
}

var rmCmd = &command{
	name: "rm", group: "Issues", args: "KEY... [--cascade] [--yes]",
	aliases: []string{"delete"},
	summary: "delete an issue for good",
	about: `Deletes the issue and its comments. Sub-tasks need --cascade. Asks at a
terminal; elsewhere --yes is required. To close instead: wp mv KEY done.`,
	examples: []string{"wp rm APP-12", "wp rm APP-3 --cascade --yes"},
	run:      cmdRm,
}

func cmdRm(c *cli, args []string) error {
	fs := c.flags()
	cascade := fs.Bool("cascade", false, "delete sub-tasks too")
	yes := fs.Bool("yes", false, "don't ask")
	short(fs, "y", "yes")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return c.usage("expected an issue KEY")
	}
	keys := listArgs(pos)
	for i, k := range keys {
		keys[i] = strings.ToUpper(k)
	}
	if err := c.confirm(*yes, "Delete "+strings.Join(keys, ", ")+" for good?"); err != nil {
		return err
	}
	return c.each(keys, false, func(key string) error {
		path := "/api/v1/issues/" + esc(key)
		if *cascade {
			path += "?cascade=true"
		}
		if _, err := c.call("DELETE", path, nil); err != nil {
			return err
		}
		return c.emit([]byte(`{}`), func() string { return "Deleted " + key })
	})
}

// stdinIsTerminal says whether someone can answer a question (tests
// replace it).
var stdinIsTerminal = func() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// confirm asks before something can't be undone: at a terminal it asks;
// otherwise --yes must be given.
func (c *cli) confirm(yes bool, question string) error {
	if yes {
		return nil
	}
	if !stdinIsTerminal() {
		return c.usage("this can't be undone: add --yes to go ahead")
	}
	fmt.Fprintf(c.err, "%s [y/N] ", question)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		// No answer (stdin is /dev/null, say): treat it as no terminal.
		_, _ = fmt.Fprintln(c.err)
		return c.usage("this can't be undone: add --yes to go ahead")
	}
	if a := strings.ToLower(strings.TrimSpace(line)); a == "y" || a == "yes" {
		return nil
	}
	return errors.New("not deleted")
}

var openCmd = &command{
	name: "open", group: "Issues", args: "[KEY | PAGE-ID]",
	summary: "open an issue, a page, or Waypoint in the browser",
	about: `No argument: your current task, else Waypoint. Prints the URL without a
browser.`,
	examples: []string{"wp open APP-12", "wp open 7   # page #7"},
	run:      cmdOpen,
}

func cmdOpen(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if c.server() == "" {
		return c.usage("no server set: wp auth login https://your-waypoint")
	}
	u := c.server() + "/"
	switch {
	case len(pos) > 0 && keyRE.MatchString(strings.ToUpper(pos[0])):
		u = c.server() + "/issue/" + esc(strings.ToUpper(pos[0]))
	case len(pos) > 0:
		id, err := pageID(pos[0])
		if err != nil {
			return c.usage("expected an issue KEY or a page number")
		}
		u = fmt.Sprintf("%s/pages/%d", c.server(), id)
	default:
		if key, _, err := c.currentKey(nil); err == nil {
			u = c.server() + "/issue/" + esc(key)
		}
	}
	if err := openBrowser(u); err != nil {
		c.println(u)
	}
	return nil
}
