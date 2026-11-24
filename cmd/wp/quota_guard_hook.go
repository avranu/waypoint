package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// guardPolicyCopy is the server's GuardPolicy.
type guardPolicyCopy struct {
	ChurnUSD   float64           `json:"churn_usd"`
	SessionUSD float64           `json:"session_usd"`
	Hours      float64           `json:"hours"`
	IdleHours  float64           `json:"idle_hours"`
	ModelUSD   float64           `json:"model_usd"`
	Models     []string          `json:"models,omitempty"`
	BurstUSD   float64           `json:"burst_usd"`
	Pace       bool              `json:"pace"`
	From       map[string]string `json:"from,omitempty"`
}

// guardDefaults is the policy before the server has sent one: the server's
// own defaults, with no model rule (it needs the usual models).
var guardDefaults = guardPolicyCopy{ChurnUSD: 20, SessionUSD: 60, Hours: 12, IdleHours: 8, ModelUSD: 3, BurstUSD: 60, Pace: true}

type guardHoldCopy struct {
	Rule   string    `json:"rule"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
	Until  time.Time `json:"until"`
}

// guardCopy is the server's GuardState, kept beside the pause.
type guardCopy struct {
	Off     bool             `json:"off,omitempty"`
	Policy  *guardPolicyCopy `json:"policy,omitempty"`
	Hold    *guardHoldCopy   `json:"hold,omitempty"`
	Checked time.Time        `json:"checked"`
}

// saveGuard stores what the server said; absent or null leaves the copy
// as it is (a server from before the guard, or one that couldn't read it).
func (c *cli) saveGuard(raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var g guardCopy
	if json.Unmarshal(raw, &g) != nil {
		return
	}
	if path := c.pausePath(); path != "" {
		if unlock, err := lockFile(path + ".lock"); err == nil {
			defer unlock()
		}
	}
	cur := c.readPause()
	g.Checked = time.Now()
	cur.Guard = &g
	c.writePause(cur)
}

// guardNow is the policy in force on this machine, nil when the person
// turned the guard off, and the hold when the copy is fresh enough to
// trust one. With no copy yet the defaults stand: the guard is on unless
// the person turned it off.
func (c *cli) guardNow(now time.Time) (*guardPolicyCopy, *guardHoldCopy) {
	g := c.readPause().Guard
	if g == nil {
		p := guardDefaults
		return &p, nil
	}
	if g.Off {
		return nil, nil
	}
	p := guardDefaults
	if g.Policy != nil {
		p = *g.Policy
	}
	var hold *guardHoldCopy
	if g.Hold != nil && g.Hold.Until.After(now) && now.Sub(g.Checked) < pauseFresh {
		hold = g.Hold
	}
	return &p, hold
}

// guardLocal is the guard's counts for one session, in its burn state.
type guardLocal struct {
	// LastCost: the tree's cost at the last look, so the model rule counts
	// only what was spent on an unusual model.
	LastCost float64 `json:"last_cost"`
	// AutoAt: automation gave the session its work (its last pasted prompt,
	// else the guard's first look at a session started by automation).
	AutoAt   time.Time `json:"auto_at,omitzero"`
	AutoCost float64   `json:"auto_cost,omitempty"`
	// ProgCost: the cost at the last change (an edit, a commit, a ship).
	ProgAt   time.Time `json:"prog_at,omitzero"`
	ProgCost float64   `json:"prog_cost,omitempty"`
	// PersonAt: the person's last typed prompt (else the first look).
	PersonAt   time.Time `json:"person_at,omitzero"`
	PersonCost float64   `json:"person_cost,omitempty"`
	ModelCost  float64   `json:"model_cost,omitempty"` // spent on an unusual model since AutoAt
	// Told: rule → the cost when the session was told.
	Told     map[string]float64 `json:"told,omitempty"`
	HoldTold time.Time          `json:"hold_told,omitzero"` // told of the hold that began at HoldSince
	HoldAt   time.Time          `json:"hold_at,omitzero"`
	HoldCost float64            `json:"hold_cost,omitempty"`
	// Stopped: the rule that stopped the session; until a person types, or
	// the guard as it is now no longer calls for it (stopStands).
	Stopped   string `json:"stopped,omitempty"`
	StopModel string `json:"stop_model,omitempty"` // the model a model stop was for
	// Person: a person typed the session's last prompt (into an autopilot
	// session they attached to, say), so it is theirs: only the idle rule
	// applies until automation prompts it again.
	Person bool `json:"person,omitempty"`
}

// guardTrip is one tell or stop, queued for the next pulse.
type guardTrip struct {
	Session string    `json:"session"`
	Rule    string    `json:"rule"`
	Action  string    `json:"action"`
	Cost    float64   `json:"cost"`
	Detail  string    `json:"detail"`
	At      time.Time `json:"at"`
}

// guardProgress: the tool call changed something (a file, a commit, a
// ship), so the session isn't going round in circles.
var reGuardProgress = lazyre.New(`\bgit\s+(?:-C\s+\S+\s+)?(?:commit|push)\b|\bwp\s+(?:ship|done|release|new|comment)\b|\bwp\s+(?:page|design|glossary)\s+(?:new|edit|add)\b|\bwp\s+pr\s+(?:fixed|ack|reply|dismiss)\b`)

func guardProgress(in *hookEventInput) bool {
	switch in.ToolName {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return true
	case "Bash":
		var b struct {
			Command string `json:"command"`
		}
		return json.Unmarshal(in.ToolInput, &b) == nil && reGuardProgress.MatchString(b.Command)
	}
	return false
}

// guardLook is what the burn hook knows at a tool call.
type guardLook struct {
	now      time.Time
	cost     float64
	auto     bool
	model    string
	progress bool
	policy   *guardPolicyCopy
	hold     *guardHoldCopy
	prices   []burnPrice
	task     string
}

// guardRuleText says where a line came from.
func guardFrom(p *guardPolicyCopy, field string) string {
	switch p.From[field] {
	case "set":
		return "the line you set"
	case "usual":
		return "from your usual usage"
	}
	return "the default"
}

// handOff is how the session is told to hand its work back.
func (l guardLook) handOff() string {
	key := cmpStr(l.task, "KEY")
	return fmt.Sprintf("hand the task off now: commit what is worth keeping, note what you tried and what is left (`wp release %s -m \"…\"`), and stop", key)
}

// guardStopTail ends every stop's reason, which the person sees.
const guardStopTail = " A prompt you type carries the session on. `wp usage guard` shows the guard; `wp usage guard set` changes its lines or turns it off."

// decide runs the rules: text to tell the session, stop to stop it now,
// and the trip to report.
func (g *guardLocal) decide(l guardLook) (text, stop string, trip *guardTrip) {
	delta := max(0, l.cost-g.LastCost)
	g.LastCost = l.cost
	if g.Stopped != "" {
		if g.stopStands(l.policy, l.hold, l.now, l.cost) {
			return "", "Quota guard: this session was stopped (" + g.Stopped + ") and stays stopped." + guardStopTail, nil
		}
		g.Stopped, g.StopModel = "", ""
	}
	if !l.auto || g.Person {
		return g.idle(l)
	}
	if g.AutoAt.IsZero() {
		g.AutoAt, g.AutoCost, g.ProgAt, g.ProgCost = l.now, l.cost, l.now, l.cost
	}
	if l.progress {
		g.ProgAt, g.ProgCost = l.now, l.cost
		delete(g.Told, "churn")
	}
	p := l.policy
	type rule struct {
		name       string
		v, line, x float64
		tell, halt string
	}
	var rules []rule
	if d := l.cost - g.ProgCost; p.ChurnUSD > 0 {
		rules = append(rules, rule{"churn", d, p.ChurnUSD, 1.5,
			fmt.Sprintf("Quota guard: this session has spent $%.0f since its last change (an edit, commit or ship), at the $%.0f line (%s) for automation going round in circles. If you are stuck, %s. At $%.0f with no change the guard stops the session.",
				d, p.ChurnUSD, guardFrom(p, "churn_usd"), l.handOff(), 1.5*p.ChurnUSD),
			fmt.Sprintf("Quota guard stopped this session: $%.0f since its last change, 1.5 times the $%.0f line (%s).", d, p.ChurnUSD, guardFrom(p, "churn_usd"))})
	}
	if d := l.cost - g.AutoCost; p.SessionUSD > 0 {
		rules = append(rules, rule{"session", d, p.SessionUSD, 1.5,
			fmt.Sprintf("Quota guard: this session has spent $%.0f on its task, at the $%.0f line (%s) for one automation session. Finish the step you are on, then %s. At $%.0f the guard stops the session.",
				d, p.SessionUSD, guardFrom(p, "session_usd"), l.handOff(), 1.5*p.SessionUSD),
			fmt.Sprintf("Quota guard stopped this session: $%.0f on its task, 1.5 times the $%.0f line (%s).", d, p.SessionUSD, guardFrom(p, "session_usd"))})
	}
	if h := l.now.Sub(g.AutoAt).Hours(); p.Hours > 0 {
		rules = append(rules, rule{"hours", h, p.Hours, 1.5,
			fmt.Sprintf("Quota guard: automation has driven this session for %.0f hours, at the %.0f-hour line (%s). Finish the step you are on, then %s. At %.0f hours the guard stops the session.",
				h, p.Hours, guardFrom(p, "hours"), l.handOff(), 1.5*p.Hours),
			fmt.Sprintf("Quota guard stopped this session: automation drove it for %.0f hours, 1.5 times the %.0f-hour line (%s).", h, p.Hours, guardFrom(p, "hours"))})
	}
	if guardUnusualModel(l.model, p.Models, l.prices) {
		g.ModelCost += delta
	}
	if p.ModelUSD > 0 && g.ModelCost > 0 {
		rules = append(rules, rule{"model", g.ModelCost, p.ModelUSD, 3,
			fmt.Sprintf("Quota guard: this session runs on %s, priced above every model automation usually uses (%s), and has spent $%.0f on it. If nothing asked for it, finish the step you are on and %s. At $%.0f on it the guard stops the session.",
				l.model, strings.Join(p.Models, ", "), g.ModelCost, l.handOff(), 3*p.ModelUSD),
			fmt.Sprintf("Quota guard stopped this session: $%.0f on %s, which automation doesn't usually use (%s), 3 times the $%.0f line.", g.ModelCost, l.model, strings.Join(p.Models, ", "), p.ModelUSD)})
	}
	for _, r := range rules {
		if r.v >= r.x*r.line {
			g.Stopped = r.name
			if r.name == "model" {
				g.StopModel = l.model
			}
			return "", r.halt + guardStopTail, &guardTrip{Rule: r.name, Action: "stopped", Cost: l.cost, Detail: r.halt, At: l.now}
		}
	}
	if h := l.hold; h != nil {
		if g.HoldAt.IsZero() || g.HoldAt.Before(h.Since) {
			g.HoldAt, g.HoldTold, g.HoldCost = h.Since, l.now, l.cost
			msg := "Quota guard: automation is held back: " + strings.TrimSuffix(h.Reason, ".") + ". Put down the step you are on and " + l.handOff() + ". In 10 minutes the guard stops the session."
			return msg, "", &guardTrip{Rule: "hold", Action: "told", Cost: l.cost, Detail: h.Reason, At: l.now}
		}
		if l.now.Sub(g.HoldTold) >= 10*time.Minute || l.cost-g.HoldCost >= 2 {
			g.Stopped = "hold"
			halt := "Quota guard stopped this session: automation is held back (" + strings.TrimSuffix(h.Reason, ".") + ")."
			return "", halt + guardStopTail, &guardTrip{Rule: "hold", Action: "stopped", Cost: l.cost, Detail: halt, At: l.now}
		}
	}
	for _, r := range rules {
		if r.v >= r.line {
			if _, told := g.Told[r.name]; told {
				continue
			}
			if g.Told == nil {
				g.Told = map[string]float64{}
			}
			g.Told[r.name] = l.cost
			return r.tell, "", &guardTrip{Rule: r.name, Action: "told", Cost: l.cost, Detail: r.tell, At: l.now}
		}
	}
	return "", "", nil
}

// stopStands: the guard as it is now still calls for the stop. A person
// who turned the guard off, lifted the hold, or raised or turned off the
// rule's line has let the session go on.
func (g *guardLocal) stopStands(p *guardPolicyCopy, hold *guardHoldCopy, now time.Time, cost float64) bool {
	if p == nil || g.Stopped == "" {
		return false
	}
	switch g.Stopped {
	case "churn":
		return p.ChurnUSD > 0 && cost-g.ProgCost >= 1.5*p.ChurnUSD
	case "session":
		return p.SessionUSD > 0 && cost-g.AutoCost >= 1.5*p.SessionUSD
	case "hours":
		return p.Hours > 0 && now.Sub(g.AutoAt).Hours() >= 1.5*p.Hours
	case "model":
		return p.ModelUSD > 0 && g.ModelCost >= 3*p.ModelUSD && !slices.Contains(p.Models, g.StopModel)
	case "hold":
		return hold != nil
	case "idle":
		return p.IdleHours > 0 && p.SessionUSD > 0 && cost-g.PersonCost >= 1.5*p.SessionUSD
	}
	return true
}

// idle is the person's own session's rule: spend since their last prompt,
// once it is IdleHours old.
func (g *guardLocal) idle(l guardLook) (text, stop string, trip *guardTrip) {
	p := l.policy
	if g.PersonAt.IsZero() {
		g.PersonAt, g.PersonCost = l.now, l.cost
	}
	idle := l.now.Sub(g.PersonAt).Hours()
	d := l.cost - g.PersonCost
	if p.IdleHours <= 0 || p.SessionUSD <= 0 || idle < p.IdleHours || d < p.SessionUSD {
		return "", "", nil
	}
	if d >= 1.5*p.SessionUSD {
		g.Stopped = "idle"
		halt := fmt.Sprintf("Quota guard stopped this session: $%.0f in the %.0f hours since you last typed in it, 1.5 times the $%.0f line (%s).", d, idle, p.SessionUSD, guardFrom(p, "session_usd"))
		return "", halt + guardStopTail, &guardTrip{Rule: "idle", Action: "stopped", Cost: l.cost, Detail: halt, At: l.now}
	}
	if _, told := g.Told["idle"]; told {
		return "", "", nil
	}
	if g.Told == nil {
		g.Told = map[string]float64{}
	}
	g.Told["idle"] = l.cost
	msg := fmt.Sprintf("Quota guard: nobody has typed in this session for %.0f hours and it has spent $%.0f since, at the $%.0f line (%s). Finish the step you are on, note what is left in your final message, and stop; at $%.0f the guard stops the session.",
		idle, d, p.SessionUSD, guardFrom(p, "session_usd"), 1.5*p.SessionUSD)
	return msg, "", &guardTrip{Rule: "idle", Action: "told", Cost: l.cost, Detail: msg, At: l.now}
}

// guardUnusualModel: model isn't one automation usually uses and is
// priced above all of them (a cheaper one is never a problem). No usual
// models known: the rule is off. A model of no known price is costed as the
// dearest but is not called unusual: the rule can stop a session, so it
// needs a price, not an assumption.
func guardUnusualModel(model string, usual []string, prices []burnPrice) bool {
	if model == "" || len(usual) == 0 {
		return false
	}
	top := 0.0
	for _, u := range usual {
		if strings.HasPrefix(model, u) || strings.HasPrefix(u, model) {
			return false
		}
		top = max(top, burnPriceFor(prices, u).Output)
	}
	p, known := burnPriceKnown(prices, model)
	return known && p.Output > top
}

// guardBurn is the guard's part of the burn hook, under the burn state's
// lock: text to tell, stop to stop now. It writes the session's pulse file
// and queues the trip.
func (c *cli) guardBurn(in *hookEventInput, st *burnState, prices []burnPrice) (text, stop string, changed bool) {
	now := burnNow()
	t := st.Tree.totals()
	model := ""
	if a := st.Tree.Agents["main"]; a != nil {
		model = a.Model
	}
	fresh := st.Guard == nil
	if fresh {
		st.Guard = &guardLocal{LastCost: t.Cost}
	}
	auto := c.guardActive(in.SessionID)
	c.writeGuardLive(in.SessionID, guardLive{Cost: t.Cost, Auto: auto && !st.Guard.Person, Model: model, At: now})
	policy, hold := c.guardNow(now)
	if policy == nil {
		moved := st.Guard.LastCost != t.Cost
		st.Guard.LastCost = t.Cost
		return "", "", fresh || moved
	}
	before, _ := json.Marshal(st.Guard)
	defer func() {
		after, _ := json.Marshal(st.Guard)
		changed = string(before) != string(after)
	}()
	keys := splitKeys(os.Getenv("WP_TASK"))
	task := ""
	if len(keys) > 0 {
		task = keys[0]
	}
	text, stop, trip := st.Guard.decide(guardLook{now: now, cost: t.Cost, auto: auto, model: model, progress: guardProgress(in),
		policy: policy, hold: hold, prices: prices, task: task})
	if trip != nil {
		trip.Session = in.SessionID
		c.queueGuardTrip(*trip)
		if trip.Action == "stopped" {
			spawnGuardPulse()
		}
	}
	return text, stop, changed
}

// guardLive is a session's spend for the pulse: <dir>/burn/<id>.live.
type guardLive struct {
	Cost  float64   `json:"cost"`
	Auto  bool      `json:"auto"`
	Model string    `json:"model,omitempty"`
	At    time.Time `json:"at"`
}

func (c *cli) writeGuardLive(session string, l guardLive) {
	path := c.burnPath(session)
	if path == "" {
		return
	}
	_ = writeJSON(strings.TrimSuffix(path, ".json")+".live", l)
}

func (c *cli) guardTripsPath() string {
	if c.dir == "" {
		return ""
	}
	return filepath.Join(c.dir, "guard_trips.json")
}

// queueGuardTrip keeps a trip for the next pulse (at most 50 waiting).
func (c *cli) queueGuardTrip(t guardTrip) {
	path := c.guardTripsPath()
	if path == "" {
		return
	}
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	var q []guardTrip
	_ = readJSON(path, &q)
	q = append(q, t)
	if len(q) > 50 {
		q = q[len(q)-50:]
	}
	_ = writeJSON(path, q)
}

// takeGuardTrips empties the queue; putBack returns them after a failed send.
func (c *cli) takeGuardTrips() (trips []guardTrip, putBack func()) {
	path := c.guardTripsPath()
	if path == "" {
		return nil, func() {}
	}
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return nil, func() {}
	}
	defer unlock()
	_ = readJSON(path, &trips)
	if len(trips) > 0 {
		_ = os.Remove(path)
	}
	return trips, func() {
		for _, t := range trips {
			c.queueGuardTrip(t)
		}
	}
}

// guardSessions is every session that counted spend in the last 3 minutes.
func (c *cli) guardSessions(now time.Time) []map[string]any {
	if c.dir == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(c.dir, "burn", "*.live"))
	var out []map[string]any
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil || now.Sub(info.ModTime()) > 3*time.Minute {
			continue
		}
		var l guardLive
		if readJSON(f, &l) != nil {
			continue
		}
		out = append(out, map[string]any{"session": strings.TrimSuffix(filepath.Base(f), ".live"), "cost": l.Cost, "auto": l.Auto, "model": l.Model})
		if len(out) == 200 {
			break
		}
	}
	return out
}

// pulse sends this machine's sessions' spend and the guard's trips, and
// keeps the pause and the guard the server answers with; false when the
// server doesn't take pulses (the caller pulls the pause instead).
func (c *cli) pulse() bool {
	trips, putBack := c.takeGuardTrips()
	body := map[string]any{"machine": machineName(), "sessions": c.guardSessions(time.Now()), "trips": trips}
	b, err := c.call("POST", "/api/v1/sessions/pulse", body)
	if err != nil {
		putBack()
		return false
	}
	var r struct {
		Pause json.RawMessage `json:"pause"`
		Guard json.RawMessage `json:"guard"`
	}
	if json.Unmarshal(b, &r) != nil {
		return true
	}
	if len(r.Pause) > 0 {
		c.savePause(r.Pause)
	}
	c.saveGuard(r.Guard)
	return true
}

// spawnGuardPulse reports a stop at once, detached: the hook returns first.
func spawnGuardPulse() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "hook", "claude-pause", "--now") // #nosec G204 -- our own binary
	cmd.SysProcAttr = detachedProc()
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}

// guardPrompt notes a prompt for the guard's counts: an autopilot prompt
// gives the session new work (the counts start again), a person's prompt
// takes it back (and lifts a stop). The refusal: a prompt automation
// pasted into a session the guard stopped.
func (c *cli) guardPrompt(session string, byAutomation bool) (refusal string) {
	now := burnNow()
	policy, hold := c.guardNow(now)
	c.withBurnState(session, func(st *burnState) bool {
		cost := st.Tree.totals().Cost
		if st.Guard == nil {
			st.Guard = &guardLocal{LastCost: cost}
		}
		g := st.Guard
		if byAutomation {
			if g.Stopped != "" {
				if g.stopStands(policy, hold, now, cost) {
					refusal = "Quota guard: this session was stopped (" + g.Stopped + "), so it takes no prompts from automation." + guardStopTail
					return false
				}
				g.Stopped, g.StopModel = "", ""
			}
			if g.AutoAt.IsZero() || g.Person {
				g.AutoAt, g.AutoCost, g.ProgAt, g.ProgCost, g.ModelCost, g.Told = now, cost, now, cost, 0, nil
			}
			g.Person = false
			return true
		}
		g.PersonAt, g.PersonCost, g.Stopped, g.StopModel, g.Told, g.Person = now, cost, "", "", nil, true
		g.AutoAt, g.AutoCost, g.ModelCost = time.Time{}, 0, 0
		return true
	})
	return refusal
}

// personCanType: a person can type into this session — their own (a task
// session they started with wp start has WP_TASK, and is still theirs), or
// an autopilot session in tmux (WAYPOINT_SESSION) they may attach to. Into
// a runner's job (claude -p: unattended or a follow-up, no
// WAYPOINT_SESSION) nobody types: its prompts are all automation's.
func personCanType() bool {
	if os.Getenv("WAYPOINT_SESSION") != "" {
		return true
	}
	return os.Getenv("WAYPOINT_FOLLOWUP") == "" && os.Getenv("WAYPOINT_UNATTENDED") != "1"
}

// guardStopped is the rule that stopped the session, if one did and the
// guard as it is now still calls for it.
func (c *cli) guardStopped(session string) string {
	path := c.burnPath(session)
	var st burnState
	if path == "" || readJSON(path, &st) != nil || st.Guard == nil || st.Guard.Stopped == "" {
		return ""
	}
	now := burnNow()
	policy, hold := c.guardNow(now)
	if !st.Guard.stopStands(policy, hold, now, st.Tree.totals().Cost) {
		return ""
	}
	return st.Guard.Stopped
}
