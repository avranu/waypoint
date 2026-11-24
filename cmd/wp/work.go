package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// keyRE matches what can be an issue key: APP-12, or APP12 without its
// dash (the server works out which key a dashless one means). keyDashRE
// is the canonical form the server prints.
var (
	keyRE     = lazyre.New(`^[A-Z][A-Z0-9]{1,9}-?[0-9]+$`)
	keyDashRE = lazyre.New(`^[A-Z][A-Z0-9]*-[0-9]+$`)
)

func jsonInto(raw []byte, v any) error { return json.Unmarshal(raw, v) }

var nextCmd = &command{
	name: "next", group: "Tasks", args: "[KEY] [FLAGS]",
	summary: "take (claim) the next task for you here, or task KEY",
	about: `Claims the most urgent task for you in the scope for 2 hours and moves it
to In Progress: pins, then the automatic order, one score from priority, due
date (more as it nears), what it unblocks, being delegated, bugs, plans, size
and age (wp queue shows the order and why). Humans only tasks
are taken only by KEY; an epic's KEY hands out its first open sub-task while
it has any you can take. Prints the task in full, the checkout's git state and
a short brief; read it whole.

An agent taking a task in the repo's main checkout, or in one holding other
work (uncommitted files, commits not on the default branch) on a branch that
doesn't name the task, gets the task its own worktree under
<repo>/.claude/worktrees/ (as wp worktree KEY makes), its claim moved there,
and is told to cd there, when that folder is inside the session's (else wp
only names the command). wp then removes the finished worktrees there and
their merged branches (wp tidy's rules; none a process or a live session is
in). A task from another project or --repo gets none. --no-worktree or
WP_NO_WORKTREE=1 keeps the task where it is taken.

Asking again returns the task you hold. "Nothing to do." means the queue is
empty.`,
	examples: []string{
		"wp next                      # the next task for this repo",
		"wp next --step review        # only tasks waiting for review",
		"wp take APP-12               # take this one (same as wp next APP-12)",
		"wp next --peek               # look without taking it",
		`wp next -q "tag:backend priority:>=high"`,
	},
	run: func(c *cli, args []string) error { return take(c, "next", args) },
}

var batchCmd = &command{
	name: "batch", group: "Tasks", args: "[KEY] [FLAGS]",
	summary:  "take the next task plus related ones, claimed together",
	about:    "Like wp next, plus up to 10 related open tasks, all claimed for you. Finish each with wp ship KEY.",
	examples: []string{"wp batch"},
	run:      func(c *cli, args []string) error { return take(c, "batch", args) },
}

func take(c *cli, which string, args []string) error {
	fs := c.flags()
	sf := addScopeFlags(fs)
	repo := fs.String("repo", "", "only tasks for `REPO` (owner/name) instead of the scope")
	step := fs.String("step", "", "only tasks at this workflow `STEP` (plan, verify, implement, review, test, …)")
	query := fs.String("query", "", "more filters (`QUERY`: wp help query)")
	short(fs, "q", "query")
	peek := fs.Bool("peek", false, "look at the task without claiming it")
	noBrief := fs.Bool("no-brief", false, "leave out the brief")
	noGit := fs.Bool("no-git", false, "leave out the git state (and make no worktree)")
	noWorktree := fs.Bool("no-worktree", false, "work in this checkout even when it is the main one or holds other work (also WP_NO_WORKTREE=1)")
	delegated := fs.Bool("delegated", false, "only tasks delegated to agents, not Anyone tasks")
	crossProject := fs.String("cross-project", "", "take a task outside this project, for this `REASON` (kept on the task)")
	fs.Bool("here", false, "(hidden) the git repo here: now the default")
	assigned := fs.Bool("assigned", false, "(hidden) autopilot assigned this task: say so in the answer")
	started := fs.Bool("started", false, "(hidden) a person started this session for the task: say so in the answer")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return c.usage("expected at most one task KEY")
	}
	c.noOwnWorktree = *noWorktree
	if len(pos) == 1 && which == "next" && !*peek {
		if *step != "" || *repo != "" || *query != "" {
			return c.usage("a task KEY is taken as it is; --step, --repo and -q only filter wp next without a KEY")
		}
		how := ""
		switch {
		case *assigned:
			how = "1"
		case *started:
			how = "start"
		}
		return c.takeKey(sf, pos[0], *crossProject, !*noBrief, !*noGit, how)
	}
	terms, crossing := []string{}, ""
	if len(pos) == 1 {
		key := strings.ToUpper(pos[0])
		if !keyRE.MatchString(key) {
			return c.usage("%q isn't an issue key like APP-12", pos[0])
		}
		if *peek {
			err = c.checkPeek(sf, *repo, key)
		} else {
			crossing, err = c.crossingForKey(sf, which, key, *crossProject)
		}
		if err != nil {
			return err
		}
		terms = append(terms, "key:"+key)
	} else {
		if !*peek {
			if crossing, err = c.crossingForNext(sf, which, *repo, *crossProject); err != nil {
				return err
			}
		}
		if *repo == "" {
			sc, err := c.workScope(sf)
			if err != nil {
				return err
			}
			if q := sc.query(); q != "" {
				terms = append(terms, q) // The agent's instructions name its repo, which the query lacks, so it is added as a term.
			}
		}
	}
	within := ""
	if !*peek && crossing == "" && (which == "batch" || *repo != "") {
		b, err := c.boundary(sf)
		if err != nil {
			return err
		}
		if b != nil {
			within = b.query()
		}
		if *repo != "" && within != "" {
			terms = append(terms, within)
		}
	}
	if *query != "" {
		terms = append(terms, *query)
	}
	if crossing != "" || *repo != "" && !c.repoHere(*repo) {
		c.noOwnWorktree = true
	}
	body := map[string]any{"step": *step, "query": strings.Join(terms, " "), "repo": *repo, "peek": *peek, "delegated": *delegated}
	if crossing != "" {
		body["cross_project"] = crossing
	}
	if which == "batch" && within != "" {
		body["within"] = within
	}
	if a := c.agentID(); a != "" {
		body["agent"] = a
	}
	path, params := "/api/v1/agent/"+which, url.Values{}
	var payload any = body
	if *peek {
		// GET only looks, which read-only tokens may do too.
		for k, v := range body {
			if v, ok := v.(string); ok && v != "" {
				params.Set(k, v)
			}
		}
		if *delegated {
			params.Set("delegated", "1")
		}
		payload = nil
	}
	if c.asJSON {
		params.Set("format", "json")
	}
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	method := "POST"
	if payload == nil {
		method = "GET"
	}
	var fetched func() error
	if !*peek {
		c.contextTokens = currentContext()
		c.claims = c.heldHere()
		fetched = c.fetchHere(!*noGit)
	}
	raw, err := c.call(method, path, payload)
	if err != nil {
		return err
	}
	var taken string
	if !*peek {
		keys := claimedKeys(raw, c.asJSON)
		if len(keys) > 0 {
			taken = keys[0]
		}
		c.takenKeys = keys
		c.rememberClaim(keys...)
		c.fetchArms(taken, "next")
	}
	return c.printTask(raw, taken, fetched, !*noBrief)
}

// taskClosing opens the paragraph of the server's answer that says what to
// do with the task (internal/api.taskClosing).
const taskClosing = "Assess and complete the task as appropriate."

// splitClosing takes the what-to-do paragraph out of a task's answer, so it
// can come last, after the checkout and the brief (WP-770). A server that
// doesn't send one leaves the answer whole.
func splitClosing(text string) (body, closing string) {
	at := strings.LastIndex(text, "\n\n"+taskClosing)
	if at < 0 {
		return text, ""
	}
	rest := text[at+2:]
	end := strings.IndexByte(rest, '\n')
	if end < 0 {
		return text[:at+1], rest
	}
	return text[:at+1] + rest[end+1:], rest[:end]
}

// printTask shows a task just taken: the answer, the checkout, the brief and
// last what to do with it.
func (c *cli) printTask(raw []byte, key string, fetched func() error, brief bool) error {
	var closing string
	if err := c.emit(raw, func() string {
		var body string
		body, closing = splitClosing(c.absoluteAttachmentLinks(string(raw)))
		return body
	}); err != nil {
		return err
	}
	c.printCheckout(key, fetched)
	if brief {
		c.compactBrief(key)
	}
	if c.delegation != "" && !c.asJSON {
		c.println("\n" + c.delegation)
	}
	if closing != "" {
		c.println("\n" + closing)
	}
	return nil
}

// heldKeys are what wp done without a KEY could mean: the tasks taken in
// this directory and the ones this session took (WP-242).
func (c *cli) heldKeys() []string {
	var keys []string
	if dir, err := os.Getwd(); err == nil {
		if _, cl := c.state.claimFor(dir); cl != nil {
			keys = cl.keys()
		}
	}
	return addKeys(keys, c.state.sessionClaims(c.sessionID())...)
}

// heldHere is heldKeys before a take, for the X-Waypoint-Claims header:
// the server shows the keyless wp done when the task it hands out is the
// only one still held; "none" when there are none.
func (c *cli) heldHere() string {
	if keys := c.heldKeys(); len(keys) > 0 {
		return strings.Join(keys, ",")
	}
	return "none"
}

// rememberClaim notes the tasks taken in this directory, the one handed
// out first. Best effort: it lets `wp done` here omit the key, and the
// Claude Code hook charge this directory's sessions to the task. One taken
// before here and not finished stays, so wp done then asks which.
func (c *cli) rememberClaim(keys ...string) {
	if len(keys) == 0 || keys[0] == "" {
		return
	}
	if dir, err := os.Getwd(); err == nil {
		cl := &claim{Key: keys[0], Agent: c.agentID(), Identity: c.agentHeader(), ClaimedAt: time.Now().UTC(), Checked: true, Top: checkoutOf(dir), Session: c.sessionID()}
		_ = c.updateState(func(s *State) {
			if s.Claims == nil {
				s.Claims = map[string]*claim{}
			}
			also := keys[1:]
			if old := s.Claims[dir]; old != nil {
				also = addKeys(also, old.keys()...)
			}
			cl.Also = slices.DeleteFunc(also, func(k string) bool { return sameKey(k, cl.Key) })
			s.Claims[dir] = cl
		})
	}
}

// issueHome is a task's own key spelling and the project it's in.
func (c *cli) issueHome(key string) (canonical, project string, err error) {
	var i struct {
		Key        string `json:"key"`
		ProjectKey string `json:"project_key"`
	}
	if err := c.do("GET", "/api/v1/issues/"+esc(key), nil, &i); err != nil {
		return "", "", err
	}
	return i.Key, i.ProjectKey, nil
}

// checkPeek holds a look at a named task to the scope, unless -A: looking
// claims nothing, so it needs no reason.
func (c *cli) checkPeek(sf scopeFlags, repo, key string) error {
	if repo != "" {
		return nil
	}
	sc, err := c.workScope(sf)
	if err != nil || sc.all() {
		return err
	}
	_, project, err := c.issueHome(key)
	if err != nil {
		return err
	}
	if !sc.contains(project) {
		return fmt.Errorf("%s is in project %s, outside the scope here (%s); add -A to look at it anyway", key, project, sc)
	}
	return nil
}

// boundary is what a claim has to stay inside to need no reason: the
// project this session works in, else the one -p names. nil when neither.
func (c *cli) boundary(sf scopeFlags) (*scope, error) {
	within, err := c.homeScope()
	if err == nil && within == nil && sf.project != nil && *sf.project != "" {
		within, err = c.scopeFor(sf)
	}
	return within, err
}

// crossingForKey checks taking a named task. Outside the project this
// session works in, that takes a reason, which it returns to be kept on
// the task ("" when nothing is crossed). The session a person or Waypoint
// started for this very task (WP_TASK) has already been told to take it.
func (c *cli) crossingForKey(sf scopeFlags, verb, key, reason string) (string, error) {
	canonical, project, err := c.issueHome(key)
	if err != nil {
		return "", err
	}
	if slices.Contains(splitKeys(os.Getenv("WP_TASK")), canonical) {
		return "", nil
	}
	within, err := c.boundary(sf)
	if err != nil {
		return "", err
	}
	if within != nil && within.contains(project) {
		return "", nil
	}
	if reason = strings.TrimSpace(reason); reason != "" {
		return reason, nil
	}
	retry := fmt.Sprintf("wp %s %s --cross-project \"why\"", verb, canonical)
	if within == nil {
		return "", fmt.Errorf("no project here to take %s in: pass -p %s if this session works on %s.\nFor a one-off from another project: %s", canonical, project, project, retry)
	}
	return "", fmt.Errorf("%s is in project %s, outside the scope here (%s).\nBetter: file what you need there (wp new -p %s \"…\") or start a session on that repo (wp start %s).\nTo take it anyway, say why: %s",
		canonical, project, within, project, canonical, retry)
}

// crossingForNext checks claiming whatever comes next: asking for it
// beyond the project this session works in takes a reason too.
func (c *cli) crossingForNext(sf scopeFlags, verb, repo, reason string) (string, error) {
	home, err := c.homeScope()
	if err != nil {
		return "", err
	}
	var asked *scope
	everywhere := sf.all != nil && *sf.all && repo == ""
	switch {
	case everywhere:
	case repo != "":
		if asked, err = c.repoScope(repo); err != nil {
			return "", err
		}
	case sf.project != nil && *sf.project != "":
		if asked, err = c.scopeFor(sf); err != nil {
			return "", err
		}
	default:
		return "", nil
	}
	if !everywhere && (home == nil || home.covers(asked)) {
		return "", nil
	}
	if reason = strings.TrimSpace(reason); reason != "" {
		return reason, nil
	}
	what := "every project"
	if !everywhere {
		what = asked.String()
		if repo != "" {
			what = repo
		}
	}
	if home == nil {
		return "", fmt.Errorf("claiming the next task from %s is a decision: add --cross-project \"why\" to wp %s, pass -p KEY for one project, or --peek to only look", what, verb)
	}
	return "", fmt.Errorf("claiming the next task from %s goes outside the scope here (%s).\nLook with --peek, or add --cross-project \"why\" to wp %s to take it anyway", what, home, verb)
}

// takeKey takes a named task (POST /agent/take/KEY).
// assigned says who handed the task over, for the answer's first line: ""
// (the agent took it itself), "1" (autopilot) or "start" (a person started
// a session for it, WP-1037).
func (c *cli) takeKey(sf scopeFlags, ref, crossProject string, brief, gitState bool, assigned string) error {
	key := strings.ToUpper(ref)
	if !keyRE.MatchString(key) {
		return c.usage("%q isn't an issue key like APP-12", ref)
	}
	crossing, err := c.crossingForKey(sf, "next", key, crossProject)
	if err != nil {
		return err
	}
	if crossing != "" {
		c.noOwnWorktree = true
	}
	body := map[string]any{}
	if crossing != "" {
		body["cross_project"] = crossing
	}
	if a := c.agentID(); a != "" {
		body["agent"] = a
	}
	c.contextTokens = currentContext()
	c.claims = c.heldHere()
	fetched := c.fetchHere(gitState)
	params := url.Values{}
	if c.asJSON {
		params.Set("format", "json")
	}
	if assigned != "" {
		params.Set("assigned", assigned)
	}
	path := "/api/v1/agent/take/" + esc(key)
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	raw, err := c.call("POST", path, body)
	if err != nil {
		return err
	}
	// The key is the server's spelling (APP-12 for app12), and an epic can hand out a sub-task key.
	if k := claimedKey(raw, c.asJSON); k != "" && assigned == "" {
		key = k
	} else if k := assignedKey(raw, c.asJSON); k != "" && assigned != "" {
		key = k
	}
	c.takenKeys = []string{key}
	c.rememberClaim(key)
	c.fetchArms(key, "take")
	return c.printTask(raw, key, fetched, brief)
}

// claimedKey finds the task key in a next/batch answer.
func claimedKey(raw []byte, isJSON bool) string {
	if keys := claimedKeys(raw, isJSON); len(keys) > 0 {
		return keys[0]
	}
	return ""
}

// assignedKey finds the task key in an assigned take's answer: the task
// key in JSON, else the KEY in its "Autopilot assigned KEY to you" or
// "NAME started this session for KEY:" line (after any "(Passed over …)"
// lines). An older server answers --started as a plain take: KEY "title".
func assignedKey(raw []byte, isJSON bool) string {
	if isJSON {
		return claimedKey(raw, true)
	}
	lines := strings.Split(string(raw), "\n")
	for _, line := range lines {
		k, ok := strings.CutPrefix(line, "Autopilot assigned ")
		if !ok {
			_, k, ok = strings.Cut(line, " started this session for ")
		}
		if ok {
			return leadingKey(k)
		}
	}
	// No such line: an older server's plain answer, KEY "title", after any
	// "(…)" notes.
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "(") {
			return leadingKey(line)
		}
	}
	return ""
}

// leadingKey is the task key s starts with ("KEY:" or "KEY "), or "".
func leadingKey(s string) string {
	if f := strings.Fields(strings.ReplaceAll(s, ":", " ")); len(f) > 0 && keyDashRE.MatchString(f[0]) {
		return f[0]
	}
	return ""
}

// claimedKeys are the task handed out and a batch's others claimed with it.
func claimedKeys(raw []byte, isJSON bool) []string {
	if isJSON {
		type ref struct {
			Key string `json:"key"`
		}
		var v struct {
			Task    *ref  `json:"task"`
			Related []ref `json:"related"`
		}
		if json.Unmarshal(raw, &v) != nil || v.Task == nil {
			return nil
		}
		keys := []string{v.Task.Key}
		for _, r := range v.Related {
			keys = append(keys, r.Key)
		}
		return keys
	}
	var keys []string
	also := false
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		switch {
		case line == "Also claimed for you:":
			also = true
		case also && strings.HasPrefix(line, "  ") && len(f) > 0 && keyDashRE.MatchString(f[0]):
			keys = append(keys, f[0])
		case len(keys) == 0 && len(f) > 0 && keyDashRE.MatchString(f[0]):
			keys = append(keys, f[0])
		default:
			also = also && line != ""
		}
	}
	return keys
}

// codeShapes are the references that name something other than an issue.
var codeShapes = []struct {
	re   *lazyre.Regexp
	what string
}{
	{lazyre.New(`(?i)^(?:[a-z0-9_.-]+:)?r-?\d+$`), "a request"},
	{lazyre.New(`(?i)^(?:[a-z][a-z0-9]{1,9}-?)?adr-?\d+$`), "a decision note"},
	{lazyre.New(`(?i)^(?:[a-z][a-z0-9]{1,9}-)?(?:pl|[ixrnpdfbt])\d+$`), "a page"},
	{lazyre.New(`(?i)^(?:page|design|decision|request|comment|session|file|term|glossary):\S+$`), "something that is not an issue"},
	{lazyre.New(`^[\w.-]+/[\w.-]+#\d+$|^#\d+$`), "a pull request or a number"},
}

// codeShape says what a word that is not an issue KEY names, if it is
// written like a code for something else ("" if it is just a word).
func codeShape(w string) string {
	w = strings.TrimRight(w, ".,;:")
	if keyRE.MatchString(strings.ToUpper(w)) {
		return ""
	}
	for _, sh := range codeShapes {
		if sh.re.MatchString(w) {
			return sh.what
		}
	}
	return ""
}

// currentKey splits off a leading issue KEY argument; without one it's
// the task taken with wp next in this directory (or a parent). A first
// argument that is written like a code for something else (a request, a
// decision, a page) is refused rather than taken as text for that task:
// a command never lands on a different target than the one named.
func (c *cli) currentKey(pos []string) (string, []string, error) {
	if len(pos) > 0 {
		first := strings.ToUpper(strings.TrimRight(pos[0], ".,;:"))
		first = strings.TrimPrefix(first, "ISSUE:")
		if keyRE.MatchString(first) {
			return first, pos[1:], nil
		}
		if what := codeShape(pos[0]); what != "" {
			return "", pos, c.usage("%s looks like a code for %s, but this command works on an issue: give its KEY (like APP-12); it won't use the task taken here instead%s", pos[0], what, codeHint(pos[0]))
		}
	}
	// a keyless done finishes only the one task held, so it never closes a task the agent was not told to finish
	keys := c.heldKeys()
	if len(keys) > 1 {
		keys = c.stillHeld(keys)
	}
	switch len(keys) {
	case 0:
		return "", pos, c.usage("which issue? Give its KEY (like APP-12); nothing was taken with wp next in this directory")
	case 1:
		return keys[0], pos, nil
	}
	return "", pos, c.usage("which issue? Give its KEY: you hold %s", strings.Join(keys, ", "))
}

// stillHeld drops the tasks finished or given back some other way (on the
// board, by a person) from the ones remembered, and forgets them. A task it
// can't look up stays.
func (c *cli) stillHeld(keys []string) []string {
	var held []string
	for _, k := range keys {
		var i struct {
			ClaimedBy string `json:"claimed_by"`
			Status    struct {
				Category string `json:"category"`
			} `json:"status"`
		}
		if err := c.do("GET", "/api/v1/issues/"+esc(k), nil, &i); err == nil && (i.ClaimedBy == "" || i.Status.Category == "done" || i.Status.Category == "canceled") {
			c.forgetClaim(k)
			continue
		}
		held = append(held, k)
	}
	return held
}

// codeHint is what to do instead, for a code with a command of its own.
func codeHint(w string) string {
	w = strings.TrimRight(w, ".,;:")
	if codeShapes[0].re.MatchString(w) {
		return "; requests take no comments (read it with wp request show " + w + "; to change what was asked, wp request withdraw " + w + " and ask again)"
	}
	return ""
}

type usageFlags struct {
	in, out *int64
	cost    *float64
	model   *string
}

func addUsageFlags(fs *flag.FlagSet) usageFlags {
	return usageFlags{
		in:    fs.Int64("tokens-in", 0, "input tokens used"),
		out:   fs.Int64("tokens-out", 0, "output tokens used"),
		cost:  fs.Float64("cost", 0, "cost in `USD`"),
		model: fs.String("model", "", "the `MODEL` that did the work"),
	}
}

func (u usageFlags) set() bool { return *u.in > 0 || *u.out > 0 || *u.cost > 0 }

func (u usageFlags) body() map[string]any {
	b := map[string]any{"input_tokens": *u.in, "output_tokens": *u.out, "cost_usd": *u.cost}
	if *u.model != "" {
		b["model"] = *u.model
	}
	return b
}

// finishDone is wp done's half of finishing (WP-608): nothing to ship, so
// each task moves to its next workflow step, or Done, with msg as the
// comment.
func (c *cli) finishDone(keys []string, msg string, u usageFlags) error {
	err := c.each(keys, false, func(key string) error {
		body := map[string]any{"comment": msg}
		if a := c.agentFor(key); a != "" {
			body["agent"] = a
		}
		if u.set() {
			body["usage"] = u.body()
		}
		path := "/api/v1/agent/done/" + esc(key)
		if c.asJSON {
			path += "?format=json"
		}
		raw, err := c.call("POST", path, body)
		if err != nil {
			return err
		}
		c.forgetClaim(key)
		return c.emit(raw, func() string { return string(raw) })
	})
	if err != nil {
		return err
	}
	c.sessionDone(keys...)
	return nil
}

// holdWaits are the flags that say what a hold or a release waits for (WP-ADR75).
type holdWaits struct {
	after, requests  *string
	reviewed, person *bool
}

func holdFlags(fs *flag.FlagSet) holdWaits {
	return holdWaits{
		after:    fs.String("after", "", "wait for tasks to close (`KEY,KEY`)"),
		requests: fs.String("request", "", "wait for requests to be answered (`R-N,R-M`)"),
		reviewed: fs.Bool("reviewed", false, "wait for a person to review it"),
		person:   fs.Bool("person", false, "wait for a person to lift it"),
	}
}

func (w holdWaits) any() bool {
	return *w.after != "" || *w.requests != "" || *w.reviewed || *w.person
}

func (w holdWaits) into(body map[string]any) {
	if *w.after != "" {
		body["after"] = splitList(*w.after)
	}
	if *w.requests != "" {
		body["requests"] = splitList(*w.requests)
	}
	if *w.reviewed {
		body["reviewed"] = true
	}
	if *w.person {
		body["person"] = true
	}
}

var releaseCmd = &command{
	name: "release", group: "Tasks", args: "[KEY...] [-m WHY] [--until WHEN] [--after KEY,KEY] [--request R-N,R-M] [--reviewed] [--person]",
	summary: "give a task you took back, unfinished",
	about: `Say why in -m (saved as a comment). KEY defaults to the one task you hold.

Say what it waits for and it goes on hold until then: --until a date, --after
tasks close, --request requests are answered, --reviewed a person reviews it,
--person a person lifts it (for stale work, or work against the project's
goals). All of them must be met. With none it isn't handed out for 30 minutes,
doubling each time it is given back again; a person's comment ends that, and
wp next KEY takes it by name. Autopilot goes on to the next task either way.

Commits in the task's checkout that no remote has are pushed to its branch
(a detached head's to wp-rescue/KEY), so the work isn't only on this machine:
never to the default branch or one with an open pull request, never forced,
with the pre-push hooks run. The task says where they went; uncommitted files
stay where they are.`,
	examples: []string{
		`wp release -m "Needs a decision on the API shape" --request R-146`,
		`wp release -m "Stale: the importer it fixes is gone" --person`,
		`wp release --after WP-9,APP-4 --until 2026-11-15`,
	},
	run: cmdRelease,
}

func cmdRelease(c *cli, args []string) error {
	fs := c.flags()
	msg := fs.String("message", "", "why: `TEXT` saved as a comment")
	short(fs, "m", "message")
	until := fs.String("until", "", "not before `WHEN`: 2026-11-15, friday, +3d")
	waits := holdFlags(fs)
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	keys, rest, err := c.currentKeys(pos)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return c.usage("unexpected %q (put the reason in -m)", joinArgs(rest))
	}
	return c.each(keys, false, func(key string) error {
		body := map[string]any{"comment": *msg, "until": *until}
		waits.into(body)
		if a := c.agentFor(key); a != "" {
			body["agent"] = a
		}
		top := c.releaseTop(key)
		raw, err := c.call("POST", "/api/v1/agent/release/"+esc(key), body)
		if err != nil {
			return err
		}
		c.forgetClaim(key)
		c.releasePush(key, top) // push the checkout on release, so its commits do not stay on this machine only
		return c.emit(raw, func() string { return string(raw) })
	})
}

var declineCmd = &command{
	name: "decline", group: "Tasks", args: "[KEY] -m WHY",
	summary: "turn down a task another repo filed here",
	about: `For a task marked "Filed from …" that doesn't belong here. It is canceled
with your -m reason, which the filer hears; say where it belongs if you know.
Don't refile it yourself.`,
	examples: []string{`wp decline APP-88 -m "This repo has no UI; the dashboard lives in Quarry"`},
	run:      cmdDecline,
}

func cmdDecline(c *cli, args []string) error {
	fs := c.flags()
	msg := fs.String("message", "", "why, and where it belongs: `TEXT` the filer reads")
	short(fs, "m", "message")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	keys, rest, err := c.currentKeys(pos)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return c.usage("unexpected %q (put the reason in -m)", joinArgs(rest))
	}
	if strings.TrimSpace(*msg) == "" {
		return c.usage("say why with -m: the agent that filed it reads it")
	}
	return c.each(keys, false, func(key string) error {
		body := map[string]any{"comment": *msg}
		if a := c.agentFor(key); a != "" {
			body["agent"] = a
		}
		raw, err := c.call("POST", "/api/v1/agent/decline/"+esc(key), body)
		if err != nil {
			return err
		}
		c.forgetClaim(key)
		return c.emit(raw, func() string { return string(raw) })
	})
}

var usageCmd = &command{
	name: "usage", group: "Hooks", args: "[KEY] --tokens-in N --tokens-out N [--cost USD]",
	summary:  "report a task's token usage (the hook does it)",
	about:    "With Claude Code, wp hook claude reports usage for you. KEY defaults to the task taken here.",
	examples: []string{"wp usage --tokens-in 12000 --tokens-out 800 --cost 0.21"},
	run:      cmdUsage,
}

func cmdUsage(c *cli, args []string) error {
	fs := c.flags()
	u := addUsageFlags(fs)
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	key, rest, err := c.currentKey(pos)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return c.usage("unexpected %q", joinArgs(rest))
	}
	if !u.set() {
		return c.usage("give --tokens-in, --tokens-out or --cost")
	}
	body := u.body()
	if a := c.agentFor(key); a != "" {
		body["agent"] = a
	}
	raw, err := c.call("POST", "/api/v1/agent/usage/"+esc(key), body)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return string(raw) })
}

// agentFor is the identity to report for a task: the one it was claimed
// as on this machine, else the configured one.
func (c *cli) agentFor(key string) string {
	if c.agent == "" {
		if _, cl := c.state.claimByKey(key); cl != nil && cl.Agent != "" {
			return cl.Agent
		}
	}
	return c.agentID()
}

// forgetClaim drops the task's claim wherever it was taken.
func (c *cli) forgetClaim(key string) {
	if dir, _ := c.state.claimByKey(key); dir == "" {
		return
	}
	_ = c.updateState(func(s *State) {
		for dir, cl := range s.Claims {
			if !cl.has(key) {
				continue
			}
			keys := slices.DeleteFunc(cl.keys(), func(k string) bool { return sameKey(k, key) })
			if len(keys) == 0 {
				delete(s.Claims, dir)
				continue
			}
			cl.Key, cl.Also = keys[0], keys[1:]
		}
	})
}

// repoFromRemote turns https://host/owner/name(.git) or git@host:owner/name.git
// into owner/name (nested groups kept: group/sub/name).
func repoFromRemote(remote string) (string, error) {
	path := remote
	if u, err := url.Parse(remote); err == nil && u.Host != "" {
		path = u.Path
	} else if i := strings.Index(remote, ":"); i >= 0 && !strings.Contains(remote[:i], "/") {
		path = remote[i+1:]
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if strings.Count(path, "/") < 1 || strings.Contains(path, "..") {
		return "", fmt.Errorf("can't tell owner/name from remote %q", remote)
	}
	return path, nil
}

// splitList turns "A,B" into its keys, so each is its own entry.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
