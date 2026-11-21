package main

// Burn meter (WP-144, X6): what a Claude Code session tree has spent, from
// the transcripts on disk and Waypoint's price table. `wp burn` prints it;
// the PostToolUse hook (hook claude-burn) keeps the same numbers
// incrementally and puts one line into the agent's context only when the
// last 10 minutes burn irregularly fast. Local only: no network in the hook.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// burnPrice is one model's price in USD per million tokens.
type burnPrice struct {
	Model        string  `json:"model"` // an id prefix: claude-opus-5-5
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheWrite5m float64 `json:"cache_write_5m"`
	CacheWrite1h float64 `json:"cache_write_1h"`
	CacheRead    float64 `json:"cache_read"`
}

// burnFallbackPrices are used when the server's table was never fetched.
var burnFallbackPrices = []burnPrice{
	{"claude-opus-5-5", 4, 20, 5, 8, 0.2},
	{"claude-sonnet-5-5", 2, 10, 2.5, 4, 0.2},
	{"claude-haiku-5-5", 0.1, 0.5, 0.125, 0.2, 0.01},
	{"claude-fable-5", 10, 50, 12.5, 20, 1}, // as seeded (0026_usage.sql); 5.2's own rows are WP-1417
	{"claude-mythos-5", 10, 50, 12.5, 20, 1},
}

// burnFallbackFamily is the fallback row a model name falls to by family.
// Ordered, dearest first, so a name with two families prices the same every time.
var burnFallbackFamily = []struct{ Family, ID string }{
	{"fable", "claude-fable-5"}, {"mythos", "claude-mythos-5"},
	{"opus", "claude-opus-5-5"}, {"sonnet", "claude-sonnet-5-5"}, {"haiku", "claude-haiku-5-5"},
}

// burnPriceFor finds the price of a model: the longest id prefix in the
// table, else the fallback row by family name, else the dearest (Fable's).
func burnPriceFor(table []burnPrice, model string) burnPrice {
	p, _ := burnPriceKnown(table, model)
	return p
}

// burnPriceKnown is burnPriceFor, and whether the price was found rather
// than assumed.
func burnPriceKnown(table []burnPrice, model string) (burnPrice, bool) {
	best, n := -1, 0
	for i, p := range table {
		if strings.HasPrefix(model, p.Model) && len(p.Model) > n {
			best, n = i, len(p.Model)
		}
	}
	if best >= 0 {
		return table[best], true
	}
	for _, f := range burnFallbackFamily {
		if strings.Contains(model, f.Family) {
			return burnFallbackByID(f.ID), true
		}
	}
	return burnFallbackByID("claude-fable-5"), false
}

// burnFallbackByID is the fallback row with that id prefix.
func burnFallbackByID(id string) burnPrice {
	for _, p := range burnFallbackPrices {
		if p.Model == id {
			return p
		}
	}
	return burnFallbackPrices[0]
}

// burnPricesFile caches the server's price table.
type burnPricesFile struct {
	Fetched time.Time   `json:"fetched"`
	Prices  []burnPrice `json:"prices"`
}

const burnPricesTTL = 24 * time.Hour

// loadBurnPrices reads the cached price table (fallbacks when absent). With
// fetch, it asks the server when the cache is older than a day; a failure
// keeps what there is.
func (c *cli) loadBurnPrices(fetch bool) []burnPrice {
	var f burnPricesFile
	path := filepath.Join(c.dir, "prices.json")
	if c.dir != "" {
		_ = readJSON(path, &f)
	}
	if fetch && c.dir != "" && c.server() != "" && c.token() != "" && time.Since(f.Fetched) > burnPricesTTL {
		c.hookTimeout = hookHTTPTimeout
		var got []burnPrice
		if err := c.do("GET", "/api/v1/usage/prices", nil, &got); err == nil && len(got) > 0 {
			f = burnPricesFile{Fetched: time.Now(), Prices: got}
			_ = writeJSON(path, f)
		}
		c.hookTimeout = 0
	}
	return append(append([]burnPrice{}, f.Prices...), burnFallbackPrices...)
}

// ---- the tree ----

// burnAgent is one transcript of the tree: the main session or a subagent.
type burnAgent struct {
	Kind    string  `json:"kind"` // main, an agent type, or fork
	Desc    string  `json:"desc,omitempty"`
	Depth   int     `json:"depth,omitempty"`
	Model   string  `json:"model,omitempty"`
	Calls   int     `json:"calls"`
	CtxSum  int64   `json:"ctx_sum"`
	CtxMax  int64   `json:"ctx_max"`
	Cost    float64 `json:"cost"`
	CostCR  float64 `json:"cost_cr"` // of Cost: cache reads
	First   int64   `json:"first,omitempty"`
	Last    int64   `json:"last,omitempty"`
	metaSet bool
}

func (a *burnAgent) avgCtx() int64 {
	if a.Calls == 0 {
		return 0
	}
	return a.CtxSum / int64(a.Calls)
}

// burnTree is the spend of a session and its subagents. Message ids are
// counted once across all files, because a fork's transcript starts with
// copies of its parent's messages.
type burnTree struct {
	Seen    map[string]bool       `json:"seen"`
	Offsets map[string]int64      `json:"offsets"` // by file name: bytes already read
	Agents  map[string]*burnAgent `json:"agents"`  // by agent id; "main" for the session itself
	Minutes map[int64]float64     `json:"minutes"` // cost by minute (unix/60), for the recent rate

	bounded bool // the hook limits each update to budget bytes
	budget  int64
}

func newBurnTree() *burnTree {
	return &burnTree{Seen: map[string]bool{}, Offsets: map[string]int64{}, Agents: map[string]*burnAgent{}, Minutes: map[int64]float64{}}
}

// burnMeta is what Claude Code writes beside a subagent's transcript.
type burnMeta struct {
	AgentType     string `json:"agentType"`
	IsFork        bool   `json:"isFork"`
	SpawnDepth    int    `json:"spawnDepth"`
	ParentAgentID string `json:"parentAgentId"`
	Description   string `json:"description"`
}

// burnFile is one transcript to read.
type burnFile struct {
	path, id string // id: "main" or the agent id
}

// burnFiles lists the transcripts of a session: the main one first, then
// subagents/agent-*.jsonl in name order.
func burnFiles(main string) []burnFile {
	fs := []burnFile{{path: main, id: "main"}}
	dir := strings.TrimSuffix(main, ".jsonl") + string(filepath.Separator) + "subagents"
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, "agent-") && strings.HasSuffix(n, ".jsonl") {
			fs = append(fs, burnFile{path: filepath.Join(dir, n), id: strings.TrimSuffix(strings.TrimPrefix(n, "agent-"), ".jsonl")})
		}
	}
	return fs
}

// update reads what is new in the files since the last time. It returns
// whether anything changed.
func (t *burnTree) update(files []burnFile, prices []burnPrice) bool {
	changed := false
	for _, f := range files {
		if t.bounded && t.budget < 0 {
			break
		}
		fi, err := os.Stat(f.path)
		if err != nil || fi.Size() == t.Offsets[f.id] {
			continue
		}
		if fi.Size() < t.Offsets[f.id] {
			t.Offsets[f.id] = 0 // rewritten: its ids are known already
		}
		if t.read(f, prices) {
			changed = true
		}
	}
	return changed
}

func (t *burnTree) agent(f burnFile) *burnAgent {
	a := t.Agents[f.id]
	if a == nil {
		a = &burnAgent{Kind: "main"}
		t.Agents[f.id] = a
	}
	if !a.metaSet && f.id != "main" {
		a.metaSet = true
		var m burnMeta
		if b, err := os.ReadFile(strings.TrimSuffix(f.path, ".jsonl") + ".meta.json"); err == nil && json.Unmarshal(b, &m) == nil { // #nosec G304 -- beside the transcript Claude Code gave us
			a.Kind, a.Desc, a.Depth = m.AgentType, m.Description, m.SpawnDepth
			if m.IsFork {
				a.Kind = "fork"
			}
		} else {
			a.Kind = "agent"
		}
	}
	return a
}

var burnUsageMark = []byte(`"usage"`)

// read parses one file from its offset; the first line of a message id wins.
func (t *burnTree) read(f burnFile, prices []burnPrice) bool {
	fh, err := os.Open(f.path) // #nosec G304 -- a Claude Code transcript
	if err != nil {
		return false
	}
	defer func() { _ = fh.Close() }()
	off := t.Offsets[f.id]
	if _, err := fh.Seek(off, 0); err != nil {
		return false
	}
	a := t.agent(f)
	br := bufio.NewReaderSize(fh, 256<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			off += int64(len(line))
			t.budget -= int64(len(line))
			t.add(a, line, prices)
			if t.bounded && t.budget < 0 {
				break // out of budget: the rest next time
			}
		}
		if err != nil {
			break // a partial last line is read next time
		}
	}
	t.Offsets[f.id] = off
	return true
}

// add counts one transcript line (the parser's rawLine: usage_parse.go).
func (t *burnTree) add(a *burnAgent, line []byte, prices []burnPrice) {
	if !bytes.Contains(line, burnUsageMark) {
		return
	}
	var l rawLine
	if json.Unmarshal(line, &l) != nil || l.Type != "assistant" || l.Message == nil || l.Message.Usage == nil || l.Message.ID == "" {
		return
	}
	m, u := l.Message, l.Message.Usage
	model := cleanModel(m.Model)
	if model == "" || strings.HasPrefix(model, "<") || t.Seen[m.ID] {
		return
	}
	t.Seen[m.ID] = true
	cw5, cw1 := u.CacheCreation, int64(0)
	if cc := u.CC; cc != nil && cc.E5m+cc.E1h > 0 {
		cw5, cw1 = cc.E5m, cc.E1h
		if rest := u.CacheCreation - cc.E5m - cc.E1h; rest > 0 {
			cw5 += rest
		}
	}
	p := burnPriceFor(prices, model)
	cr := float64(u.CacheRead) * p.CacheRead / 1e6
	cost := float64(u.Input)*p.Input/1e6 + float64(u.Output)*p.Output/1e6 +
		float64(cw5)*p.CacheWrite5m/1e6 + float64(cw1)*p.CacheWrite1h/1e6 + cr
	ctx := u.Input + u.CacheRead + u.CacheCreation
	a.Calls++
	a.CtxSum += ctx
	a.CtxMax = max(a.CtxMax, ctx)
	a.Cost += cost
	a.CostCR += cr
	a.Model = model
	if ts := parseTS(l.Timestamp); ts > 0 {
		if a.First == 0 || ts < a.First {
			a.First = ts
		}
		a.Last = max(a.Last, ts)
		t.Minutes[ts/60000] += cost
	}
}

// burnTotals are the sums over the tree.
type burnTotals struct {
	Agents, Calls int // Agents: without the main session
	Cost, CostCR  float64
	CtxSum        int64
	First, Last   int64
}

func (t *burnTree) totals() burnTotals {
	var s burnTotals
	for id, a := range t.Agents {
		if id != "main" {
			s.Agents++
		}
		s.Calls += a.Calls
		s.Cost += a.Cost
		s.CostCR += a.CostCR
		s.CtxSum += a.CtxSum
		if a.First > 0 && (s.First == 0 || a.First < s.First) {
			s.First = a.First
		}
		s.Last = max(s.Last, a.Last)
	}
	return s
}

func (s burnTotals) avgCtx() int64 {
	if s.Calls == 0 {
		return 0
	}
	return s.CtxSum / int64(s.Calls)
}

func (s burnTotals) minutes() float64 { return float64(s.Last-s.First) / 60000 }

// perHour is the average rate over the whole run (0 when it is too short).
func (s burnTotals) perHour() float64 {
	if m := s.minutes(); m >= 1 {
		return s.Cost / m * 60
	}
	return 0
}

// recentRate is $/h over the 10 minutes up to now, from the per-minute costs.
func (t *burnTree) recentRate(now time.Time) float64 {
	cur := now.Unix() / 60
	var sum float64
	for m, c := range t.Minutes {
		if m > cur-10 && m <= cur {
			sum += c
		}
	}
	return sum * 6
}

// ---- finding the session ----

var reNonAlnum = lazyre.New(`[^A-Za-z0-9]`)

// transcriptOfSession finds <config>/projects/*/<id>.jsonl.
func transcriptOfSession(id string) string {
	m, _ := filepath.Glob(filepath.Join(claudeConfigDir(), "projects", "*", id+".jsonl"))
	if len(m) == 0 {
		return ""
	}
	return m[0]
}

// currentTranscript is the transcript burn reports on: --session (an id or
// a path), the session this shell belongs to, the one the hooks recorded for
// this directory, else the newest transcript for it.
func (c *cli) currentTranscript(session, cwd string) (string, error) {
	pick := func(s string) string {
		if strings.HasSuffix(s, ".jsonl") {
			if _, err := os.Stat(s); err == nil { // #nosec G703 -- the user named the transcript
				return s
			}
			return ""
		}
		return transcriptOfSession(s)
	}
	if session != "" {
		if p := pick(session); p != "" {
			return p, nil
		}
		return "", fmt.Errorf("no transcript for session %s under %s", session, claudeConfigDir())
	}
	if p := pick(os.Getenv("WAYPOINT_TRANSCRIPT")); os.Getenv("WAYPOINT_TRANSCRIPT") != "" && p != "" {
		return p, nil
	}
	if s := os.Getenv("WAYPOINT_SESSION"); s != "" {
		if p := transcriptOfSession(s); p != "" {
			return p, nil
		}
	}
	for d := filepath.Clean(cwd); ; d = filepath.Dir(d) {
		for _, cs := range c.state.sessionsAt(d) {
			if p := transcriptOfSession(cs.Session); p != "" {
				return p, nil
			}
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	dir := filepath.Join(claudeConfigDir(), "projects", reNonAlnum.ReplaceAllString(cwd, "-"))
	ents, _ := os.ReadDir(dir)
	var best string
	var bt time.Time
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && strings.HasSuffix(e.Name(), ".jsonl") && fi.ModTime().After(bt) {
			best, bt = filepath.Join(dir, e.Name()), fi.ModTime()
		}
	}
	if best == "" {
		return "", errors.New("no Claude Code transcript for this directory; pass --session ID")
	}
	return best, nil
}

// ---- wp burn ----

var burnCmd = &command{
	name: "burn", group: "Code and CI", args: "[--session ID]",
	summary: "what this session and its subagents have spent",
	about: `Per agent: kind, model, calls, average and largest context per call and
cost, then totals and $/hour. The session is --session (an id or transcript
path), else this shell's, else the newest for this directory. Most of a long
session's cost is the context re-read on every call. A hook warns you once
when the last 10 minutes run above $100/h (WP_BURN_ALERT_PER_HOUR; 0: off).`,
	examples: []string{"wp burn"},
	run:      cmdBurn,
}

type burnRow struct {
	ID    string  `json:"id"`
	Kind  string  `json:"kind"`
	Desc  string  `json:"description,omitempty"`
	Depth int     `json:"depth"`
	Model string  `json:"model"`
	Calls int     `json:"calls"`
	Avg   int64   `json:"avg_context"`
	Max   int64   `json:"max_context"`
	Cost  float64 `json:"cost"`
}

type burnReport struct {
	Transcript string    `json:"transcript"`
	Agents     []burnRow `json:"agents"`
	Calls      int       `json:"calls"`
	AvgContext int64     `json:"avg_context"`
	Cost       float64   `json:"cost"`
	Minutes    float64   `json:"minutes"`
	PerHour    float64   `json:"per_hour"`
}

func cmdBurn(c *cli, args []string) error {
	fs := c.flags()
	session := fs.String("session", "", "the `ID` of the session (or the path of its transcript)")
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	path, err := c.currentTranscript(*session, cwd)
	if err != nil {
		return err
	}
	tree := newBurnTree()
	tree.update(burnFiles(path), c.loadBurnPrices(true))
	rep := tree.report(path)
	if c.asJSON {
		b, _ := json.Marshal(rep)
		return c.emit(b, func() string { return "" })
	}
	c.println(rep.text())
	return nil
}

func (t *burnTree) report(path string) burnReport {
	r := burnReport{Transcript: path}
	for id, a := range t.Agents {
		r.Agents = append(r.Agents, burnRow{ID: id, Kind: a.Kind, Desc: a.Desc, Depth: a.Depth, Model: a.Model, Calls: a.Calls, Avg: a.avgCtx(), Max: a.CtxMax, Cost: a.Cost})
	}
	sort.Slice(r.Agents, func(i, j int) bool {
		if r.Agents[i].Cost != r.Agents[j].Cost {
			return r.Agents[i].Cost > r.Agents[j].Cost
		}
		return r.Agents[i].ID < r.Agents[j].ID
	})
	s := t.totals()
	r.Calls, r.AvgContext, r.Cost, r.Minutes, r.PerHour = s.Calls, s.avgCtx(), s.Cost, s.minutes(), s.perHour()
	return r
}

func (r burnReport) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-26s %-14s %6s %8s %8s %9s\n", "AGENT", "MODEL", "CALLS", "AVG CTX", "MAX CTX", "$")
	for _, a := range r.Agents {
		kind := a.Kind
		if a.Depth > 0 {
			kind += fmt.Sprintf("@d%d", a.Depth)
		}
		fmt.Fprintf(&b, "%-26s %-14s %6d %8s %8s %9.2f\n", truncRunes(kind, 26), truncRunes(strings.TrimPrefix(a.Model, "claude-"), 14), a.Calls, kTokens(a.Avg), kTokens(a.Max), a.Cost)
	}
	rate := "-"
	if r.PerHour > 0 {
		rate = fmt.Sprintf("$%.0f/h", r.PerHour)
	}
	fmt.Fprintf(&b, "TOTAL: %d agents, %d calls, avg context %s, $%.2f in %.0f min (%s)", max(len(r.Agents)-1, 0), r.Calls, kTokens(r.AvgContext), r.Cost, r.Minutes, rate)
	return b.String()
}

// kTokens writes a token count as 283k.
func kTokens(n int64) string {
	if n < 1000 {
		return fmt.Sprint(n)
	}
	return fmt.Sprintf("%dk", (n+500)/1000)
}

// ---- the PostToolUse hook ----

// burnHookBudget is how many bytes of transcript one hook call may read, so
// a session with 100 MB behind it catches up over a few calls and never
// runs into the hook's timeout.
var burnHookBudget int64 = 8 << 20

// behind says whether some file has more to read than the tree has.
func (t *burnTree) behind(files []burnFile) bool {
	for _, f := range files {
		if fi, err := os.Stat(f.path); err == nil && fi.Size() > t.Offsets[f.id] {
			return true
		}
	}
	return false
}

const (
	burnAlertDefault = 100.0 // $/h over the last 10 minutes; a normal Opus session is $10-25/h
	burnAlertRepeat  = 30 * time.Minute
)

// burnAlertLimit is the $/h line (WP_BURN_ALERT_PER_HOUR); 0 turns the alert off.
func burnAlertLimit() float64 {
	if v := strings.TrimSpace(os.Getenv("WP_BURN_ALERT_PER_HOUR")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return burnAlertDefault
}

// burnNow is the clock of the hook (a test swaps it).
var burnNow = time.Now

// burnState is what the hook keeps per session, in <config>/burn/<id>.json.
type burnState struct {
	Tree      *burnTree `json:"tree"`
	AlertRate float64   `json:"alert_rate,omitempty"` // $/h at the last alert
	AlertAt   time.Time `json:"alert_at,omitzero"`
	// A fan-out package's session (WP-148): told to hand off at its budget,
	// stopped at 1.5 times it.
	BudgetSaid    bool `json:"budget_said,omitempty"`
	BudgetStopped bool `json:"budget_stopped,omitempty"`
	// Guard: the live quota guard's counts (WP-932).
	Guard *guardLocal `json:"guard,omitempty"`
}

var reSafeID = lazyre.New(`^[A-Za-z0-9._-]{1,128}$`)

func (c *cli) burnPath(session string) string {
	if c.dir == "" || !reSafeID.MatchString(session) {
		return ""
	}
	return filepath.Join(c.dir, "burn", session+".json")
}

// withBurnState runs change on the session's burn state under a lock, and
// saves it when change says so.
func (c *cli) withBurnState(session string, change func(*burnState) bool) {
	path := c.burnPath(session)
	if path == "" {
		return
	}
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	st := &burnState{}
	_ = readJSON(path, st)
	if st.Tree == nil {
		st.Tree = newBurnTree()
	}
	if change(st) {
		b, err := json.Marshal(st)
		if err != nil {
			return
		}
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		tmp := path + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			_ = os.Rename(tmp, path)
		}
	}
}

// hookClaudeBurn is the PostToolUse hook: local only, never blocks.
func hookClaudeBurn(c *cli, stdin io.Reader, stdout io.Writer) {
	var in hookEventInput
	if json.NewDecoder(io.LimitReader(stdin, 4<<20)).Decode(&in) != nil {
		return
	}
	// Edit-time CI feedback rides on this hook: no new hook row (WP-161).
	burn, stop := c.burnText(&in)
	if stop != "" {
		// Past 1.5 times a fan-out package's budget, or a quota guard line
		// (WP-932): stop it, and let the next package start (WP-148).
		_, _ = stdout.Write(promptStop(stop))
		endBackgroundSession(in.SessionID)
		fanoutEnded("budget", in.TranscriptPath, c.sessionCost(in.SessionID))
		return
	}
	text := strings.TrimSpace(c.hookCILint(&in) + "\n" + burn)
	if in.AgentID == "" { // a subagent's tool result never reaches the agent the note is for
		text = strings.TrimSpace(text + "\n\n" + c.takeNotes(in.SessionID, in.TranscriptPath)) // WP-562
	}
	if in.AgentID == "" && in.ToolName == "Bash" && isShipCall(bashCommand(in.ToolInput)) {
		// wp ship with --summary may end the turn (WP-1093): not with a
		// note for the agent to read, and later for one Claude Code moved
		// to the background.
		if id := bashBackground(in.ToolResponse); id != "" {
			c.noteShipBackground(in.SessionID, id)
		} else if text == "" {
			stop, more := c.shipStop(in, "")
			if stop != nil {
				_, _ = stdout.Write(stop)
				return
			}
			text = more
		}
	} else if in.AgentID == "" {
		c.dropUnusedShipEnd(in.SessionID) // the turn went on past the ship
	}
	if text != "" {
		_, _ = stdout.Write(hookContext("PostToolUse", text))
	}
}

// burnText is the burn meter's part of the hook: "" unless the tree burns
// fast, or a fan-out package's session reached its budget; stop, when it
// must stop now.
func (c *cli) burnText(in *hookEventInput) (text, stop string) {
	quiet := os.Getenv("WAYPOINT_NO_BURN") != ""
	if in.SessionID == "" || !filepath.IsAbs(in.TranscriptPath) {
		return "", ""
	}
	prices := c.loadBurnPrices(false)
	c.withBurnState(in.SessionID, func(st *burnState) bool {
		files := burnFiles(in.TranscriptPath)
		st.Tree.bounded, st.Tree.budget = true, burnHookBudget
		changed := st.Tree.update(files, prices)
		st.Tree.bounded = false
		if st.Tree.behind(files) {
			return changed // still catching up: silent until the totals are whole
		}
		var mutated bool
		if !quiet {
			text, mutated = st.decide(burnNow())
		}
		said, halt, budgetChanged := fanoutBudgetLine(st, st.Tree.totals().Cost)
		if said != "" {
			text = strings.TrimSpace(text + "\n" + said)
		}
		stop = halt
		// The live quota guard (WP-932); WAYPOINT_NO_BURN quiets only the
		// rate alert above.
		guardText, guardStop, guardChanged := c.guardBurn(in, st, prices)
		if stop == "" {
			stop = guardStop
		} else if guardStop != "" {
			guardText = strings.TrimSpace(guardText + "\n" + guardStop)
		}
		text = strings.TrimSpace(text + "\n" + guardText)
		return changed || mutated || budgetChanged || guardChanged
	})
	return text, stop
}

// sessionCost is what the burn hook last counted for a session.
func (c *cli) sessionCost(session string) float64 {
	path := c.burnPath(session)
	var st burnState
	if path == "" || readJSON(path, &st) != nil || st.Tree == nil {
		return 0
	}
	return st.Tree.totals().Cost
}

// decide says what the hook adds to the context: nothing, unless the last 10
// minutes run above the alert line. It speaks once, then again only when the
// rate has doubled since the last alert or 30 minutes later while still above
// the line. The bool: whether the state changed.
func (st *burnState) decide(now time.Time) (string, bool) {
	limit := burnAlertLimit()
	rate := st.Tree.recentRate(now)
	if limit <= 0 || rate <= limit {
		if st.AlertRate != 0 && rate < limit/2 {
			st.AlertRate, st.AlertAt = 0, time.Time{}
			return "", true
		}
		return "", false
	}
	if st.AlertRate != 0 && rate < 2*st.AlertRate && now.Sub(st.AlertAt) < burnAlertRepeat {
		return "", false
	}
	st.AlertRate, st.AlertAt = rate, now
	s := st.Tree.totals()
	who := "this session is"
	if s.Agents > 0 {
		who = fmt.Sprintf("this session and %d agents are", s.Agents)
	}
	return fmt.Sprintf("Unusual spend: %s running at $%.0f/h (avg context %s). If that's not intended, stop or narrow the agents; `wp burn` shows which.", who, rate, kTokens(s.avgCtx())), true
}
