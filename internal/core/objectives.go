package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Objectives (WP-927, WP-ADR111). A task can carry an objective: work done
// in rounds until a check says it is met. Three shapes, one mechanism:
//
//   - until a fixed target: "until `go test ./x` runs in under 1s", "until
//     `./coverage` prints at least 80%", "until `make lint` passes". The
//     task closes when a round's last check meets it.
//   - by an amount every round: "2x faster", "-10%". Each round is judged
//     against the commit it started from, so it is never met for good; it
//     asks for more each time, until a person closes the task.
//   - a loop with no check: every round ships what it did and the task opens
//     again, until a person closes it or its rounds run out.
//
// A round ends where the task would close: wp done, or the merge of the
// round's pull request. There the objective decides: met (or out of rounds),
// the close stands; otherwise the task goes back to To Do for the next
// round, keeping what the round shipped. A person's own close always stands.
//
// The check is a shell command agents' machines run (wp objective check, and
// wp ship before it ships). So only a person's own act puts one in effect:
// what an agent sets is a proposal, shown but not run and not looping, until
// a person confirms it. The result is the CLI's report, like test evidence:
// it decides whether the task stays open, nothing else.
//
// Rounds don't crowd out other work. One that moved nothing backs the task
// off (an hour, doubling to a week); `every` sets the least time between
// rounds, a day by default for the shapes that never end; and pace "spare"
// hands the task out only while the person's week's-end plan wants more work
// (WP-ADR106), so it runs on quota that would otherwise go unused.

const (
	ObjectiveProposed = "proposed"
	ObjectiveActive   = "active"
	ObjectiveMet      = "met"
	ObjectiveEnded    = "ended"

	MeasureTime  = "time"
	MeasureValue = "value"
	MeasurePass  = "pass"

	PaceQueue = "queue"
	PaceSpare = "spare"

	RoundMet        = "met"
	RoundProgress   = "progress"
	RoundNone       = "none"
	RoundUnmeasured = "unmeasured"
	RoundLoop       = "loop"

	// objectiveEveryDefault: the least time between rounds of an objective
	// that never ends by itself, unless its person says otherwise.
	objectiveEveryDefault = 24 * time.Hour
	// objectiveStallFirst doubles for every further round in a row that
	// moved nothing, up to objectiveStallMax.
	objectiveStallFirst = time.Hour
	objectiveStallMax   = 7 * 24 * time.Hour
	// objectiveNoise: a change from the base smaller than this isn't called
	// an improvement; nor is a timed one its runs can't tell from noise.
	objectiveNoise = 0.02
	objectiveP     = 0.05

	maxObjectiveCmd    = 2000
	maxObjectiveRounds = 1000
	objectiveRecent    = 5
)

// Objective is a task's objective and where its rounds stand.
type Objective struct {
	IssueID int64  `json:"-"`
	Key     string `json:"key"`
	Title   string `json:"title,omitempty"`
	// Kind: "until" (a fixed target), "each" (an amount every round) or
	// "loop" (no target).
	Kind    string `json:"kind"`
	Check   string `json:"check,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Setup   string `json:"setup,omitempty"`
	Measure string `json:"measure,omitempty"`
	Until   string `json:"until,omitempty"`
	// Op and Target are a fixed target (seconds for time); Factor a relative
	// one: this many times better than the round's base.
	Op     string   `json:"op,omitempty"`
	Target *float64 `json:"target,omitempty"`
	Factor *float64 `json:"factor,omitempty"`
	Better string   `json:"better,omitempty"`
	Unit   string   `json:"unit,omitempty"`
	Pace   string   `json:"pace"`
	// Every is the least seconds from one round's end to the next's start.
	Every     int64      `json:"every"`
	MaxRounds int        `json:"max_rounds,omitempty"`
	Round     int        `json:"round"`
	Stalled   int        `json:"stalled,omitempty"`
	NextAt    *time.Time `json:"next_at,omitempty"`
	State     string     `json:"state"`
	// Summary says the objective in a sentence; Text is the whole of it as
	// wp show prints it.
	Summary string `json:"summary"`
	// Rev changes with the command, folder and target: what a confirm names.
	Rev         string     `json:"rev"`
	Text        string     `json:"text"`
	SetBy       *UserRef   `json:"set_by,omitempty"`
	SetAgent    string     `json:"set_agent,omitempty"`
	ConfirmedBy *UserRef   `json:"confirmed_by,omitempty"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
	// Last is the newest check; Checks the newest few, newest first; Rounds
	// the last finished rounds, oldest first.
	Last   *ObjectiveMeasurement   `json:"last,omitempty"`
	Checks []*ObjectiveMeasurement `json:"checks"`
	Rounds []*ObjectiveRound       `json:"rounds"`

	everyMS                 int64
	roundStartedAt          int64
	setBy, confirmedBy      *int64
	issueClosed, issueDraft bool
	projectID               int64
}

// ObjectiveMeasurement is one run of the check.
type ObjectiveMeasurement struct {
	ID        int64     `json:"id"`
	Round     int       `json:"round"`
	At        time.Time `json:"at"`
	OK        bool      `json:"ok"`
	Value     *float64  `json:"value,omitempty"`
	BaseOK    *bool     `json:"base_ok,omitempty"`
	BaseValue *float64  `json:"base_value,omitempty"`
	BaseSHA   string    `json:"base_sha,omitempty"`
	Tree      string    `json:"tree,omitempty"`
	Commit    string    `json:"commit,omitempty"`
	Runs      int       `json:"runs,omitempty"`
	Low       *float64  `json:"low,omitempty"`
	High      *float64  `json:"high,omitempty"`
	P         *float64  `json:"p,omitempty"`
	Moved     bool      `json:"moved,omitempty"`
	Met       bool      `json:"met"`
	Improved  bool      `json:"improved"`
	Agent     string    `json:"agent,omitempty"`
	// Line says it in words: "1.42s (base 2.1s, 32% better)".
	Line string `json:"line"`
}

// ObjectiveRound is one finished round.
type ObjectiveRound struct {
	Round     int                   `json:"round"`
	StartedAt time.Time             `json:"started_at"`
	EndedAt   time.Time             `json:"ended_at"`
	Outcome   string                `json:"outcome"`
	Check     *ObjectiveMeasurement `json:"check,omitempty"`
	Agent     string                `json:"agent,omitempty"`
	PR        string                `json:"pr_url,omitempty"`
}

// ObjectiveInput sets or changes an objective; nil keeps what is there.
type ObjectiveInput struct {
	Check   *string `json:"check"`
	Dir     *string `json:"dir"`
	Setup   *string `json:"setup"`
	Until   *string `json:"until"`
	Measure *string `json:"measure"`
	Better  *string `json:"better"`
	Pace    *string `json:"pace"`
	// Every is a duration: "0", "90m", "24h", "3d".
	Every     *string `json:"every"`
	MaxRounds *int    `json:"max_rounds"`
}

// ObjectiveCheckInput is what wp objective check measured.
type ObjectiveCheckInput struct {
	Agent     string   `json:"agent"`
	OK        bool     `json:"ok"`
	Value     *float64 `json:"value"`
	BaseOK    *bool    `json:"base_ok"`
	BaseValue *float64 `json:"base_value"`
	BaseSHA   string   `json:"base_sha"`
	Tree      string   `json:"tree"`
	Commit    string   `json:"commit"`
	Runs      int      `json:"runs"`
	Low       *float64 `json:"low"`
	High      *float64 `json:"high"`
	P         *float64 `json:"p"`
	Moved     bool     `json:"moved"`
}

// ObjectiveCheck is the answer to a recorded check.
type ObjectiveCheck struct {
	Objective *Objective            `json:"objective"`
	Check     *ObjectiveMeasurement `json:"check"`
	Met       bool                  `json:"met"`
	Message   string                `json:"message"`
}

// objectiveTarget is a parsed "until".
type objectiveTarget struct {
	measure, until, op, better, unit string
	target, factor                   *float64
}

var (
	objFixedRe = lazyre.New(`^(<=|>=|<|>)\s*([0-9]*\.?[0-9]+)\s*(\S.*)?$`)
	objTimesRe = lazyre.New(`^([0-9]*\.?[0-9]+)\s*[xX×]$`)
	objPctRe   = lazyre.New(`^([+-])\s*([0-9]*\.?[0-9]+)\s*%$`)
)

// timeUnits are the units that make a fixed target a time, in seconds.
var timeUnits = map[string]float64{
	"ns": 1e-9, "us": 1e-6, "µs": 1e-6, "ms": 1e-3, "s": 1, "sec": 1, "secs": 1, "second": 1, "seconds": 1,
	"m": 60, "min": 60, "mins": 60, "minute": 60, "minutes": 60, "h": 3600, "hour": 3600, "hours": 3600,
}

// parseObjectiveTarget reads a target: "" (a loop), "pass", a fixed one
// ("< 1s", ">= 80%", "<= 120 warnings") or a relative one ("2x", "-10%",
// "+5%"). measure and better are what the person said, "" to let the target
// decide: a time unit makes it a time, any other fixed target a value the
// command prints; a relative one is a time unless it asks for more ("+5%").
func parseObjectiveTarget(until, measure, better string) (objectiveTarget, error) {
	until = strings.Join(strings.Fields(until), " ")
	measure, better = strings.ToLower(strings.TrimSpace(measure)), strings.ToLower(strings.TrimSpace(better))
	t := objectiveTarget{measure: measure, better: better}
	if measure != "" && measure != MeasureTime && measure != MeasureValue && measure != MeasurePass {
		return t, invalid("measure is time, value or pass")
	}
	if better != "" && better != "lower" && better != "higher" {
		return t, invalid("better is lower or higher")
	}
	num := func(s string) (float64, error) {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, invalid("%q isn't a number", s)
		}
		return v, nil
	}
	switch {
	case until == "":
		if measure == MeasurePass {
			t.until = "pass"
			return t, nil
		}
		// A loop; with a check and a measure, each round's reading is kept
		// but nothing is judged.
		return t, nil
	case strings.EqualFold(until, "pass") || strings.EqualFold(until, "passes"):
		if measure != "" && measure != MeasurePass {
			return t, invalid("the target \"pass\" goes with the measure pass")
		}
		t.measure, t.until, t.better = MeasurePass, "pass", ""
		return t, nil
	}
	if measure == MeasurePass {
		return t, invalid("the measure pass has no target but \"pass\": the command passes or it doesn't")
	}
	if m := objFixedRe.FindStringSubmatch(until); m != nil {
		v, err := num(m[2])
		if err != nil {
			return t, err
		}
		unit := strings.TrimSpace(m[3])
		if mult, ok := timeUnits[strings.ToLower(unit)]; ok && measure != MeasureValue {
			t.measure, v = MeasureTime, v*mult
			t.until = m[1] + " " + benchSecs(v)
		} else {
			if measure == MeasureTime {
				return t, invalid("a time target needs a time unit: %s %ss, %s %sms", m[1], m[2], m[1], m[2])
			}
			if len(unit) > 24 {
				return t, invalid("the unit %q is too long", unit)
			}
			t.measure, t.unit = MeasureValue, unit
			t.until = m[1] + " " + objectiveNumber(v, unit)
		}
		t.op, t.target = m[1], &v
		want := "lower"
		if strings.HasPrefix(m[1], ">") {
			want = "higher"
		}
		if better != "" && better != want {
			return t, invalid("%s asks for a %s value: leave --better out, or turn the comparison round", m[1], want)
		}
		t.better = want
		return t, nil
	}
	if m := objTimesRe.FindStringSubmatch(until); m != nil {
		f, err := num(m[1])
		if err != nil {
			return t, err
		}
		if f <= 1 {
			return t, invalid("%sx asks for no improvement: give more than 1 (1.2x, 2x)", m[1])
		}
		if t.measure == "" {
			t.measure = MeasureTime
		}
		if t.better == "" {
			t.better = "lower"
		}
		if t.measure == MeasureTime && t.better != "lower" {
			return t, invalid("a time gets better as it gets lower")
		}
		t.factor, t.until = &f, strconv.FormatFloat(f, 'g', 6, 64)+"x"
		return t, nil
	}
	if m := objPctRe.FindStringSubmatch(until); m != nil {
		pct, err := num(m[2])
		if err != nil {
			return t, err
		}
		var f float64
		want := "lower"
		switch {
		case pct <= 0:
			return t, invalid("%s asks for no change", until)
		case m[1] == "-" && pct >= 100:
			return t, invalid("nothing gets 100%% lower: give less (-10%%, -50%%)")
		case m[1] == "-":
			f = 1 / (1 - pct/100)
		default:
			f, want = 1+pct/100, "higher"
		}
		if better != "" && better != want {
			return t, invalid("%s asks for a %s value: leave --better out", until, want)
		}
		t.better = want
		if t.measure == "" {
			t.measure = MeasureTime
			if want == "higher" {
				t.measure = MeasureValue
			}
		}
		if t.measure == MeasureTime && want == "higher" {
			return t, invalid("a time gets better as it gets lower: %s would ask for a slower command", until)
		}
		t.factor, t.until = &f, m[1]+strconv.FormatFloat(pct, 'g', 6, 64)+"%"
		return t, nil
	}
	return t, invalid("the target %q isn't one of: pass; a fixed one (< 1s, <= 250ms, >= 80%%, < 10 warnings); or an amount every round (2x, -10%%, +5%%)", until)
}

// parseEvery reads the least time between rounds: Go durations and days.
func parseEvery(s string) (time.Duration, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "0" || s == "none" {
		return 0, nil
	}
	var d time.Duration
	if n, ok := strings.CutSuffix(s, "d"); ok {
		v, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, invalid("every %q isn't a duration (90m, 24h, 3d)", s)
		}
		d = time.Duration(v * 24 * float64(time.Hour))
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, invalid("every %q isn't a duration (90m, 24h, 3d)", s)
		}
	}
	if d < 0 || d > 366*24*time.Hour {
		return 0, invalid("every is between 0 and a year")
	}
	return d, nil
}

// objectiveNumber prints a value with its unit: "80%", "120 warnings".
func objectiveNumber(v float64, unit string) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if math.Abs(v) >= 1e15 || (v != 0 && math.Abs(v) < 1e-4) {
		s = strconv.FormatFloat(v, 'g', 6, 64)
	} else if strings.Contains(s, ".") && len(s) > 8 {
		s = strings.TrimRight(strings.TrimRight(strconv.FormatFloat(v, 'f', 4, 64), "0"), ".")
	}
	switch unit {
	case "":
		return s
	case "%":
		return s + "%"
	}
	return s + " " + unit
}

// value prints a reading of the objective's measure.
func (o *Objective) value(v float64) string {
	if o.Measure == MeasureTime {
		return benchSecs(v)
	}
	return objectiveNumber(v, o.Unit)
}

// rev names what the objective would run and judge, so a person confirms
// the proposal they read and not one written over it since.
func (o *Objective) rev() string {
	h := sha256.Sum256([]byte(strings.Join([]string{o.Check, o.Setup, o.Dir, o.Measure, o.Until, o.Better}, "\x00")))
	return hex.EncodeToString(h[:6])
}

func (o *Objective) kind() string {
	switch {
	case o.Factor != nil:
		return "each"
	case o.Measure == MeasurePass || o.Target != nil:
		return "until"
	}
	return "loop"
}

// endless: no round ends it; only a person or its rounds running out does.
func (o *Objective) endless() bool { return o.kind() != "until" }

// judge says whether a reading meets the objective, and whether it is
// better than the base it was measured beside.
func (o *Objective) judge(m *ObjectiveMeasurement) (met, improved bool) {
	switch {
	case m.Moved:
		return false, false
	case o.Measure == MeasurePass:
		return m.OK, m.OK && m.BaseOK != nil && !*m.BaseOK
	case o.Measure == "" || !m.OK || m.Value == nil:
		return false, false
	}
	v := *m.Value
	lower := o.Better != "higher"
	var base *float64
	if m.BaseOK != nil && *m.BaseOK && m.BaseValue != nil {
		base = m.BaseValue
	}
	if base != nil {
		if lower {
			improved = v < *base*(1-objectiveNoise)
		} else {
			improved = v > *base*(1+objectiveNoise) || (*base <= 0 && v > *base)
		}
		if m.P != nil && *m.P >= objectiveP {
			improved = false
		}
	}
	switch {
	case o.Target != nil:
		switch o.Op {
		case "<":
			met = v < *o.Target
		case "<=":
			met = v <= *o.Target
		case ">":
			met = v > *o.Target
		case ">=":
			met = v >= *o.Target
		}
	case o.Factor != nil && base != nil && *base > 0:
		if lower {
			met = v <= *base / *o.Factor
		} else {
			met = v >= *base**o.Factor
		}
	}
	return met, improved
}

// line says a reading in words.
func (o *Objective) line(m *ObjectiveMeasurement) string {
	sha := func(s string) string { return s[:min(7, len(s))] }
	var b strings.Builder
	switch {
	case o.Measure == MeasurePass && m.OK:
		b.WriteString("passes")
	case !m.OK:
		b.WriteString("the command failed")
	case m.Value == nil:
		b.WriteString("the command printed no number")
	default:
		b.WriteString(o.value(*m.Value))
		if m.Runs > 1 && m.Low != nil && m.High != nil {
			fmt.Fprintf(&b, " (median of %d, middle half %s to %s)", m.Runs, o.value(*m.Low), o.value(*m.High))
		}
	}
	if m.BaseOK != nil {
		b.WriteString("; base " + sha(m.BaseSHA) + ": ")
		switch {
		case o.Measure == MeasurePass && *m.BaseOK:
			b.WriteString("passes")
		case !*m.BaseOK:
			b.WriteString("fails")
		case m.BaseValue == nil:
			b.WriteString("no number")
		default:
			b.WriteString(o.value(*m.BaseValue))
			if m.OK && m.Value != nil && *m.BaseValue != 0 && o.Measure != MeasurePass {
				pct := (*m.Value - *m.BaseValue) / math.Abs(*m.BaseValue) * 100
				word := "higher"
				if pct < 0 {
					word = "lower"
				}
				fmt.Fprintf(&b, ", %.0f%% %s", math.Abs(pct), word)
				if m.P != nil && *m.P >= objectiveP {
					b.WriteString(", which its runs can't tell from noise")
				}
			}
		}
	}
	if m.Moved {
		b.WriteString("; files changed while it ran, so it doesn't count")
	}
	return b.String()
}

// summary says the objective in a sentence.
func (o *Objective) summary() string {
	cmd := "`" + o.Check + "`"
	if o.Dir != "" {
		cmd += " (in " + o.Dir + ")"
	}
	words := map[string]string{"<": "under", "<=": "at most", ">": "over", ">=": "at least"}
	switch {
	case o.Check == "":
		return "a loop: every round ships what it did, and the task opens again"
	case o.Measure == MeasurePass:
		return "until " + cmd + " passes"
	case o.Target != nil && o.Measure == MeasureTime:
		return fmt.Sprintf("until %s runs in %s %s", cmd, words[o.Op], benchSecs(*o.Target))
	case o.Target != nil:
		return fmt.Sprintf("until %s prints %s %s", cmd, words[o.Op], objectiveNumber(*o.Target, o.Unit))
	case o.Factor != nil && o.Measure == MeasureTime:
		return fmt.Sprintf("every round, %s runs %s faster than at the round's base commit", cmd, o.relWords())
	case o.Factor != nil:
		return fmt.Sprintf("every round, what %s prints gets %s %s than at the round's base commit", cmd, o.relWords(), o.Better)
	}
	return "a loop that reads " + cmd + " every round: nothing is judged, and the task opens again"
}

// relWords: "2x", "10%".
func (o *Objective) relWords() string { return strings.TrimLeft(o.Until, "+-") }

// ObjectiveText is the objective as wp show and wp next print it. forAgent
// adds what a round is and how to end one.
func ObjectiveText(o *Objective, forAgent bool, now time.Time) string {
	if o == nil {
		return ""
	}
	var b strings.Builder
	head := "Objective"
	switch o.State {
	case ObjectiveProposed:
		head = "Proposed objective (an agent's; not in effect until a person confirms it on the task's page or with wp objective confirm " + o.Key + ")"
	case ObjectiveMet:
		head = "Objective met"
	case ObjectiveEnded:
		head = "Objective ended"
	default:
		head += fmt.Sprintf(", round %d", o.Round)
		if o.MaxRounds > 0 {
			head += fmt.Sprintf(" of %d", o.MaxRounds)
		}
	}
	b.WriteString(head + ": " + o.Summary + ".\n")
	var how []string
	switch o.Measure {
	case MeasureTime:
		how = append(how, "median wall time of several runs")
	case MeasureValue:
		how = append(how, "the last number on the command's last line of output")
	}
	if o.Setup != "" {
		how = append(how, "after `"+o.Setup+"` once")
	}
	if o.Pace == PaceSpare {
		how = append(how, "runs only on quota that would go unused")
	}
	if o.Every > 0 {
		how = append(how, "at least "+hoursWords(float64(o.Every)/3600)+" between rounds")
	}
	if len(how) > 0 {
		b.WriteString("  " + upperFirst(strings.Join(how, "; ")) + ".\n")
	}
	for _, r := range o.Rounds {
		line := fmt.Sprintf("  Round %d: ", r.Round)
		switch r.Outcome {
		case RoundMet:
			line += "met"
		case RoundProgress:
			line += "better, not met"
		case RoundNone:
			line += "no better"
		case RoundUnmeasured:
			line += "ended without a check"
		default:
			line += "done"
		}
		if r.Check != nil {
			line += " (" + r.Check.Line + ")"
		}
		b.WriteString(line + ".\n")
	}
	if o.Last != nil && o.Last.Round == o.Round && o.State == ObjectiveActive {
		verdict := "not met"
		if o.Last.Met {
			verdict = "met"
		}
		fmt.Fprintf(&b, "  This round's last check, %s: %s; %s.\n", agoWords(now.Sub(o.Last.At)), o.Last.Line, verdict)
	}
	if o.State == ObjectiveActive && o.NextAt != nil && o.NextAt.After(now) {
		fmt.Fprintf(&b, "  Agents aren't handed round %d before %s", o.Round, o.NextAt.In(time.Local).Format("Jan 2 15:04 MST"))
		if o.Stalled > 0 {
			b.WriteString(": " + plural1(o.Stalled, "round") + " in a row moved nothing")
		}
		b.WriteString(".\n")
	}
	if forAgent && o.State == ObjectiveActive {
		switch o.Check {
		case "":
			b.WriteString("  A round is one useful step. Ship it as usual (wp ship): the task opens again for the next round by itself.\n")
		default:
			b.WriteString("  A round is one real step toward it, not the whole of it. `wp objective check " + o.Key +
				"` measures this tree beside its base commit and records it; wp ship does the same before it ships. Ship what you gained even when it isn't met: the task goes back to To Do for the next round by itself, and closes when a round's last check meets it. If nothing you tried helped, say what you tried in -m so the next round doesn't repeat it.\n")
		}
	}
	return b.String()
}

// agoWords: "just now", "12 min ago", "3 h ago", "2 d ago".
func agoWords(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%.0f min ago", d.Minutes())
	case d < 48*time.Hour:
		return fmt.Sprintf("%.0f h ago", d.Hours())
	}
	return fmt.Sprintf("%.0f d ago", d.Hours()/24)
}

const objectiveCols = `o.issue_id, p.key || '-' || i.number, i.title, i.project_id, i.draft, st.category,
	o.check_cmd, o.dir, o.setup, o.measure, o.until, o.op, o.target, o.factor, o.better, o.unit, o.pace, o.every_ms, o.max_rounds,
	o.round_no, o.round_started_at, o.stalled, o.next_at, o.state, o.set_by, o.set_agent, o.confirmed_by, o.confirmed_at, o.ended_at, o.updated_at`

const objectiveFrom = ` FROM objectives o JOIN issues i ON i.id = o.issue_id JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id`

func scanObjective(sc interface{ Scan(...any) error }) (*Objective, error) {
	o := &Objective{Checks: []*ObjectiveMeasurement{}, Rounds: []*ObjectiveRound{}}
	var target, factor sql.NullFloat64
	var next, setBy, confBy, confAt, ended sql.NullInt64
	var updated int64
	var cat string
	if err := sc.Scan(&o.IssueID, &o.Key, &o.Title, &o.projectID, &o.issueDraft, &cat,
		&o.Check, &o.Dir, &o.Setup, &o.Measure, &o.Until, &o.Op, &target, &factor, &o.Better, &o.Unit, &o.Pace, &o.everyMS, &o.MaxRounds,
		&o.Round, &o.roundStartedAt, &o.Stalled, &next, &o.State, &setBy, &o.SetAgent, &confBy, &confAt, &ended, &updated); err != nil {
		return nil, err
	}
	o.issueClosed = isClosed(cat)
	if target.Valid {
		o.Target = &target.Float64
	}
	if factor.Valid {
		o.Factor = &factor.Float64
	}
	if next.Valid {
		o.NextAt = ptrOf(msTime(next.Int64))
	}
	if setBy.Valid {
		o.setBy = &setBy.Int64
	}
	if confBy.Valid {
		o.confirmedBy = &confBy.Int64
	}
	if confAt.Valid {
		o.ConfirmedAt = ptrOf(msTime(confAt.Int64))
	}
	if ended.Valid {
		o.EndedAt = ptrOf(msTime(ended.Int64))
	}
	o.Every, o.UpdatedAt = o.everyMS/1000, msTime(updated)
	o.Kind, o.Summary, o.Rev = o.kind(), o.summary(), o.rev()
	return o, nil
}

// objectiveRow is an issue's objective without its history; nil for none.
func (s *Service) objectiveRow(ctx context.Context, q queryer, issueID int64) (*Objective, error) {
	o, err := scanObjective(q.QueryRowContext(ctx, `SELECT `+objectiveCols+objectiveFrom+` WHERE o.issue_id = ?`, issueID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return o, err
}

const measurementCols = `id, round_no, at, ok, value, base_ok, base_value, base_sha, tree, commit_sha, runs, low, high, p, moved, met, improved, agent`

func scanMeasurement(o *Objective, sc interface{ Scan(...any) error }) (*ObjectiveMeasurement, error) {
	m := &ObjectiveMeasurement{}
	var at int64
	var value, baseValue, low, high, p sql.NullFloat64
	var baseOK sql.NullBool
	if err := sc.Scan(&m.ID, &m.Round, &at, &m.OK, &value, &baseOK, &baseValue, &m.BaseSHA, &m.Tree, &m.Commit, &m.Runs, &low, &high, &p, &m.Moved, &m.Met, &m.Improved, &m.Agent); err != nil {
		return nil, err
	}
	m.At = msTime(at)
	f := func(v sql.NullFloat64) *float64 {
		if !v.Valid {
			return nil
		}
		return &v.Float64
	}
	m.Value, m.BaseValue, m.Low, m.High, m.P = f(value), f(baseValue), f(low), f(high), f(p)
	if baseOK.Valid {
		m.BaseOK = &baseOK.Bool
	}
	// Judged against the objective as it stands, not as it stood when the
	// check ran: a target changed since doesn't close the task on the old one.
	m.Met, m.Improved = o.judge(m)
	m.Line = o.line(m)
	return m, nil
}

// objectiveOf is an issue's objective with its newest checks and last
// rounds; nil for none.
func (s *Service) objectiveOf(ctx context.Context, q queryer, issueID int64) (*Objective, error) {
	o, err := s.objectiveRow(ctx, q, issueID)
	if err != nil || o == nil {
		return nil, err
	}
	o.SetBy, o.ConfirmedBy = s.userRefPtr(ctx, q, o.setBy), s.userRefPtr(ctx, q, o.confirmedBy)
	rows, err := q.QueryContext(ctx, `SELECT `+measurementCols+` FROM objective_measurements WHERE issue_id = ? ORDER BY id DESC LIMIT ?`, issueID, objectiveRecent)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		m, err := scanMeasurement(o, rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		o.Checks = append(o.Checks, m)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(o.Checks) > 0 {
		o.Last = o.Checks[0]
	}
	rows, err = q.QueryContext(ctx, `SELECT round_no, started_at, ended_at, outcome, coalesce(measurement_id, 0), agent, pr_url FROM objective_rounds
		WHERE issue_id = ? ORDER BY round_no DESC, id DESC LIMIT ?`, issueID, objectiveRecent)
	if err != nil {
		return nil, err
	}
	var checks []int64
	for rows.Next() {
		r := &ObjectiveRound{}
		var started, ended, mid int64
		if err := rows.Scan(&r.Round, &started, &ended, &r.Outcome, &mid, &r.Agent, &r.PR); err != nil {
			_ = rows.Close()
			return nil, err
		}
		r.StartedAt, r.EndedAt = msTime(started), msTime(ended)
		o.Rounds, checks = append([]*ObjectiveRound{r}, o.Rounds...), append([]int64{mid}, checks...)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for n, mid := range checks {
		if mid == 0 {
			continue
		}
		m, err := scanMeasurement(o, q.QueryRowContext(ctx, `SELECT `+measurementCols+` FROM objective_measurements WHERE id = ?`, mid))
		if err == nil {
			o.Rounds[n].Check = m
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	o.Text = ObjectiveText(o, false, s.now())
	return o, nil
}

// GetObjective is an issue's objective; not found when it has none.
func (s *Service) GetObjective(ctx context.Context, actor *Actor, ref string) (*Objective, error) {
	i, _, err := s.getIssueFor(ctx, s.db.R, actor, ref, RoleViewer, false)
	if err != nil {
		return nil, err
	}
	o, err := s.objectiveOf(ctx, s.db.R, i.ID)
	if err == nil && o == nil {
		return nil, notFound("an objective on " + i.Key)
	}
	return o, err
}

// objectiveListMost is how many objectives a list holds, and how many rows
// it reads at a time; a variable for tests.
var objectiveListMost = 500

// ListObjectives lists the objectives the actor can see, in a project or
// everywhere, those in effect or proposed first; all adds met and ended ones.
func (s *Service) ListObjectives(ctx context.Context, actor *Actor, project string, all bool) ([]*Objective, error) {
	q, args := `SELECT o.issue_id, i.project_id`+objectiveFrom+` WHERE 1 = 1`, []any{}
	if project != "" {
		p, err := s.projectByRef(ctx, s.db.R, project)
		if err != nil {
			return nil, err
		}
		q, args = q+` AND i.project_id = ?`, append(args, p.ID)
	}
	if !all {
		q += ` AND o.state IN ('active', 'proposed') AND st.category NOT IN ('done', 'canceled')`
	}
	// Pages are filtered for what the actor can see as they're read, so
	// objectives in projects they can't see don't crowd out ones they can.
	q += ` ORDER BY o.state != 'active', o.updated_at DESC, o.issue_id LIMIT ? OFFSET ?`
	most := objectiveListMost
	out := []*Objective{}
	for offset := 0; len(out) < most; offset += most {
		rows, err := s.db.R.QueryContext(ctx, q, append(slices.Clone(args), most, offset)...)
		if err != nil {
			return nil, err
		}
		var ids [][2]int64
		for rows.Next() {
			var id, pid int64
			if err := rows.Scan(&id, &pid); err != nil {
				_ = rows.Close()
				return nil, err
			}
			ids = append(ids, [2]int64{id, pid})
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		for _, id := range ids {
			if len(out) == most || !s.canSee(ctx, s.db.R, actor, id[0], id[1]) {
				continue
			}
			o, err := s.objectiveOf(ctx, s.db.R, id[0])
			if err != nil {
				return nil, err
			}
			if o != nil {
				out = append(out, o)
			}
		}
		if len(ids) < most {
			break
		}
	}
	return out, nil
}

// cleanObjectiveDir is a folder within the repository, slash-separated.
func cleanObjectiveDir(d string) (string, error) {
	d = strings.Trim(strings.TrimSpace(strings.ReplaceAll(d, "\\", "/")), "/")
	for d == "." || strings.HasPrefix(d, "./") {
		d = strings.TrimLeft(strings.TrimPrefix(d, "."), "/")
	}
	for _, part := range strings.Split(d, "/") {
		if part == ".." || (part == "" && d != "") {
			return "", invalid("dir is a folder within the repository (internal/core), not a path out of it")
		}
	}
	if len(d) > 400 || strings.ContainsAny(d, "\n\r\x00") {
		return "", invalid("dir is too long")
	}
	return d, nil
}

// SetObjective gives an issue an objective, or changes it. A person's own
// act puts it in effect; an agent's is a proposal until a person confirms
// it, and an agent can't change one that is in effect: its check is a
// command other agents' machines run.
func (s *Service) SetObjective(ctx context.Context, actor *Actor, ref string, in ObjectiveInput) (*Objective, error) {
	var issueID int64
	err := s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		issueID = i.ID
		if i.Closed() {
			return invalid("%s is %s: reopen it before giving it an objective", i.Key, i.Status.Name)
		}
		cur, err := s.objectiveRow(ctx, t, i.ID)
		if err != nil {
			return err
		}
		person := actor.actsAsPerson()
		if cur != nil && cur.State == ObjectiveProposed && !person && cur.SetAgent != "" && cur.SetAgent != actor.creditAgent() {
			return forbidden("%s's proposed objective is %s's: another agent can't rewrite it before a person reads it", i.Key, cur.SetAgent)
		}
		if cur != nil && cur.State != ObjectiveProposed && !person {
			return forbidden("%s's objective is in effect, and its check is a command agents' machines run: changing it is for a person, in the web or their own terminal", i.Key)
		}
		o := &Objective{Pace: PaceQueue, Round: 1, everyMS: -1}
		if cur != nil {
			o = cur
		}
		pick := func(dst *string, src *string) {
			if src != nil {
				*dst = strings.TrimSpace(*src)
			}
		}
		measure, better := o.Measure, ""
		if in.Until == nil && in.Measure == nil && in.Better == nil {
			better = o.Better
		}
		// The target as it stands, kept exactly when this change doesn't
		// touch it: the stored text is rounded for reading ("< 1.2345s" reads
		// "< 1.23s"), and a web form sends it back with every save.
		same := func(p *string, was string) bool {
			return p == nil || strings.EqualFold(strings.Join(strings.Fields(*p), " "), was)
		}
		keep := cur != nil && same(in.Until, cur.Until) && same(in.Measure, cur.Measure) && same(in.Better, cur.Better)
		kept := objectiveTarget{measure: o.Measure, until: o.Until, op: o.Op, better: o.Better, unit: o.Unit, target: o.Target, factor: o.Factor}
		wasEndless := cur != nil && cur.endless()
		pick(&o.Check, in.Check)
		pick(&o.Setup, in.Setup)
		pick(&o.Until, in.Until)
		pick(&measure, in.Measure)
		pick(&better, in.Better)
		pick(&o.Pace, in.Pace)
		if in.Until != nil && in.Measure == nil {
			measure = "" // a new target decides its own measure
		}
		if in.Dir != nil {
			if o.Dir, err = cleanObjectiveDir(*in.Dir); err != nil {
				return err
			}
		}
		if len(o.Check) > maxObjectiveCmd || len(o.Setup) > maxObjectiveCmd {
			return invalid("the command is too long (at most %d characters): put it in a script in the repository", maxObjectiveCmd)
		}
		if strings.ContainsRune(o.Check+o.Setup, 0) {
			return invalid("the command has a NUL in it")
		}
		tg := kept
		if !keep {
			if tg, err = parseObjectiveTarget(o.Until, measure, better); err != nil {
				return err
			}
		}
		if o.Check == "" && (tg.until != "" || tg.measure != "" || o.Setup != "" || o.Dir != "") {
			return invalid("a target needs the command that checks it: --check 'go test ./internal/core' (a loop has neither)")
		}
		if o.Check != "" && tg.measure == "" {
			return invalid("say what the check is for: --until pass, a fixed target (\"< 1s\", \">= 80%%\") or an amount every round (2x, -10%%)")
		}
		if o.Setup != "" && tg.measure != MeasureTime {
			return invalid("--setup is for a timed check (it runs once, before the timed runs): put it in the check's own command")
		}
		o.Measure, o.Until, o.Op, o.Target, o.Factor, o.Better, o.Unit = tg.measure, tg.until, tg.op, tg.target, tg.factor, tg.better, tg.unit
		if o.Pace != PaceQueue && o.Pace != PaceSpare {
			return invalid("pace is queue (handed out like any task) or spare (only on quota that would go unused)")
		}
		if in.MaxRounds != nil {
			o.MaxRounds = *in.MaxRounds
		}
		if o.MaxRounds < 0 || o.MaxRounds > maxObjectiveRounds {
			return invalid("rounds is 0 (no limit) to %d", maxObjectiveRounds)
		}
		// One that was met or ended starts again on a new round: the round
		// it ended on is recorded, and its checks aren't the new round's.
		restart := false
		if cur != nil && person && (cur.State == ObjectiveMet || cur.State == ObjectiveEnded) {
			restart = true
			o.Round++
		}
		// A person's change of an agent's proposal puts it in effect now, as
		// confirming it does: its round starts then, not when it was written.
		startNow := restart || (cur != nil && person && cur.State == ObjectiveProposed)
		if o.MaxRounds > 0 && o.MaxRounds < o.Round {
			if restart {
				return invalid("its %s are done: give it more (--rounds %d), or 0 for no limit", plural1(o.MaxRounds, "round"), o.Round)
			}
			return invalid("round %d is under way: rounds can't be fewer", o.Round)
		}
		switch {
		case in.Every != nil:
			d, err := parseEvery(*in.Every)
			if err != nil {
				return err
			}
			o.everyMS = d.Milliseconds()
		case o.endless() && (o.everyMS < 0 || (cur != nil && !wasEndless && o.everyMS == 0)):
			// New, or a fixed target turned into one that never ends by itself.
			o.everyMS = objectiveEveryDefault.Milliseconds()
		case o.everyMS < 0:
			o.everyMS = 0
		}
		now := s.nowMS()
		state, confBy, confAt := ObjectiveProposed, any(nil), any(nil)
		setBy, setAgent := nullUser(actor.id()), actor.creditAgent()
		if person {
			state, confBy, confAt = ObjectiveActive, nullUser(actor.id()), now
		}
		if cur == nil {
			_, err = t.ExecContext(ctx, `INSERT INTO objectives (issue_id, check_cmd, dir, setup, measure, until, op, target, factor, better, unit, pace, every_ms, max_rounds,
				round_no, round_started_at, state, set_by, set_agent, confirmed_by, confirmed_at, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?)`,
				i.ID, o.Check, o.Dir, o.Setup, o.Measure, o.Until, o.Op, nullFloat(o.Target), nullFloat(o.Factor), o.Better, o.Unit, o.Pace, o.everyMS, o.MaxRounds,
				now, state, setBy, setAgent, confBy, confAt, now, now)
		} else {
			// A person's change of one that was met or ended starts it again,
			// its rounds kept; a wait it was under is theirs to end.
			_, err = t.ExecContext(ctx, `UPDATE objectives SET check_cmd = ?, dir = ?, setup = ?, measure = ?, until = ?, op = ?, target = ?, factor = ?, better = ?, unit = ?,
				pace = ?, every_ms = ?, max_rounds = ?, state = ?, set_by = ?, set_agent = ?, confirmed_by = ?, confirmed_at = ?, ended_at = NULL, next_at = NULL, stalled = 0, updated_at = ?,
				round_no = ?, round_started_at = CASE WHEN ? THEN ? ELSE round_started_at END
				WHERE issue_id = ?`,
				o.Check, o.Dir, o.Setup, o.Measure, o.Until, o.Op, nullFloat(o.Target), nullFloat(o.Factor), o.Better, o.Unit,
				o.Pace, o.everyMS, o.MaxRounds, state, setBy, setAgent, confBy, confAt, now, o.Round, startNow, now, i.ID)
		}
		if err != nil {
			return err
		}
		o.Kind = o.kind()
		old := ""
		if cur != nil {
			old = cur.Summary
		}
		t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: "objective", Old: old, New: o.summary()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.objectiveOf(ctx, s.db.R, issueID)
}

// ConfirmObjective puts an agent's proposed objective in effect: a person's
// own act, since its check is a command agents' machines will run.
// rev is the proposal the person read (Objective.Rev): an agent may have
// written another over it since, and that one isn't what they confirm.
func (s *Service) ConfirmObjective(ctx context.Context, actor *Actor, ref, rev string) (*Objective, error) {
	if err := actor.requirePersonDirect("confirming an objective"); err != nil {
		return nil, err
	}
	var issueID int64
	err := s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		issueID = i.ID
		o, err := s.objectiveRow(ctx, t, i.ID)
		switch {
		case err != nil:
			return err
		case o == nil:
			return notFound("an objective on " + i.Key)
		case o.State != ObjectiveProposed:
			return nil // in effect already
		case i.Closed():
			return invalid("%s is %s: reopen it first", i.Key, i.Status.Name)
		case rev == "":
			return invalid("say which proposal you read: {\"rev\": %q} is %s's now (%s)", o.Rev, i.Key, o.Summary)
		case rev != o.Rev:
			return invalid("%s's proposal changed since you read it. It is now: %s. Read it again before confirming", i.Key, o.Summary)
		}
		now := s.nowMS()
		if _, err := t.ExecContext(ctx, `UPDATE objectives SET state = ?, confirmed_by = ?, confirmed_at = ?, round_started_at = ?, updated_at = ? WHERE issue_id = ?`,
			ObjectiveActive, nullUser(actor.id()), now, now, now, i.ID); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: "objective", Old: "proposed", New: "confirmed: " + o.Summary})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.objectiveOf(ctx, s.db.R, issueID)
}

// ClearObjective takes an issue's objective off: it is an ordinary task
// again, and its rounds and checks go with it. An agent can only withdraw a
// proposal.
func (s *Service) ClearObjective(ctx context.Context, actor *Actor, ref string) error {
	return s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		o, err := s.objectiveRow(ctx, t, i.ID)
		switch {
		case err != nil:
			return err
		case o == nil:
			return nil
		case o.State != ObjectiveProposed && !actor.actsAsPerson():
			return forbidden("%s's objective is a person's to take off, in the web or their own terminal; finishing a round is wp ship", i.Key)
		}
		for _, q := range []string{`DELETE FROM objective_rounds WHERE issue_id = ?`, `DELETE FROM objective_measurements WHERE issue_id = ?`, `DELETE FROM objectives WHERE issue_id = ?`} {
			if _, err := t.ExecContext(ctx, q, i.ID); err != nil {
				return err
			}
		}
		t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: "objective", Old: o.Summary, New: ""})
		return nil
	})
}

func finitePtr(vs ...*float64) bool {
	for _, v := range vs {
		if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0)) {
			return false
		}
	}
	return true
}

// RecordObjectiveCheck keeps one run of the check, as the CLI measured it,
// against the round under way, and says whether it meets the objective.
func (s *Service) RecordObjectiveCheck(ctx context.Context, actor *Actor, ref string, in ObjectiveCheckInput) (*ObjectiveCheck, error) {
	if !finitePtr(in.Value, in.BaseValue, in.Low, in.High, in.P) {
		return nil, invalid("a reading isn't a number")
	}
	if in.Runs < 1 {
		in.Runs = 1
	}
	if in.Runs > 1000 || len(in.Tree) > 80 || len(in.Commit) > 80 || len(in.BaseSHA) > 80 {
		return nil, invalid("that isn't a check wp objective check made")
	}
	if !in.OK {
		in.Value = nil
	}
	if in.BaseOK == nil || !*in.BaseOK {
		in.BaseValue = nil
	}
	var issueID, mid int64
	err := s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		issueID = i.ID
		o, err := s.objectiveRow(ctx, t, i.ID)
		switch {
		case err != nil:
			return err
		case o == nil:
			return notFound("an objective on " + i.Key)
		case o.State == ObjectiveProposed:
			return forbidden("%s's objective is an agent's proposal: its check counts once a person confirms it", i.Key)
		case o.State != ObjectiveActive:
			return invalid("%s's objective is %s: there is no round to check", i.Key, o.State)
		case o.Check == "":
			return invalid("%s's objective is a loop: it has no check", i.Key)
		}
		m := &ObjectiveMeasurement{OK: in.OK, Value: in.Value, BaseOK: in.BaseOK, BaseValue: in.BaseValue, P: in.P, Moved: in.Moved}
		m.Met, m.Improved = o.judge(m)
		agent := actor.agentName()
		if agent == "" {
			agent = strings.TrimSpace(in.Agent)
		}
		var session any
		if actor.Session != 0 {
			session = actor.Session
		}
		var baseOK any
		if in.BaseOK != nil {
			baseOK = *in.BaseOK
		}
		res, err := t.ExecContext(ctx, `INSERT INTO objective_measurements (issue_id, round_no, at, ok, value, base_ok, base_value, base_sha, tree, commit_sha, runs, low, high, p, moved, met, improved, agent, user_id, session_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			i.ID, o.Round, s.nowMS(), in.OK, nullFloat(in.Value), baseOK, nullFloat(in.BaseValue), in.BaseSHA, in.Tree, in.Commit, in.Runs,
			nullFloat(in.Low), nullFloat(in.High), nullFloat(in.P), in.Moved, m.Met, m.Improved, agent, nullUser(actor.id()), session)
		if err != nil {
			return err
		}
		mid, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return nil, err
	}
	o, err := s.objectiveOf(ctx, s.db.R, issueID)
	if err != nil {
		return nil, err
	}
	out := &ObjectiveCheck{Objective: o}
	for _, m := range o.Checks {
		if m.ID == mid {
			out.Check = m
		}
	}
	if out.Check == nil {
		return nil, fmt.Errorf("objective check %d: not kept", mid)
	}
	out.Met = out.Check.Met
	verdict := "not met"
	switch {
	case out.Met && o.Kind == "each":
		verdict = "met for this round"
	case out.Met:
		verdict = "met"
	case out.Check.Improved:
		verdict = "better, not met yet"
	}
	out.Message = fmt.Sprintf("%s round %d: %s. Objective (%s): %s.", o.Key, o.Round, out.Check.Line, o.Until, verdict)
	return out, nil
}

// objectiveClose is called where an open issue is about to close. With an
// objective in effect it ends the round and decides: the close stands (nil
// error, st back) when a person closes it themselves, it is being cancelled,
// the round's last check met a fixed target, or its rounds ran out;
// otherwise the issue goes to To Do for the next round (that status back, or
// nil when it is there already).
func (s *Service) objectiveClose(ctx context.Context, t *tx, actor *Actor, i *Issue, st *Status, viaLink string) (*Status, error) {
	o, err := s.objectiveRow(ctx, t, i.ID)
	if err != nil || o == nil || o.State != ObjectiveActive {
		return st, err
	}
	now := s.nowMS()
	end := func(state string) error {
		_, err := t.ExecContext(ctx, `UPDATE objectives SET state = ?, ended_at = ?, next_at = NULL, updated_at = ? WHERE issue_id = ?`, state, now, now, i.ID)
		return err
	}
	note := func(text string) {
		t.emit(&Event{Kind: EvIssueUpdated, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, Field: "objective", Old: fmt.Sprintf("round %d", o.Round), New: text})
	}
	// What an agent wrote on the forge and Waypoint mirrors isn't a
	// person's close.
	byPerson := actor.actsAsPerson() && (!actor.forgeSync || actor.textAgent == "")
	if !byPerson && st.Category == CatCanceled {
		// Not a person's: the cancel stands, and so does the objective (an
		// agent can't end one in effect). It goes on if the task is reopened.
		note("closed as " + st.Name + " by an agent: the objective stays, and goes on if the task is reopened")
		return st, nil
	}
	if !byPerson && viaLink != "" && o.Round > 1 {
		// A pull request from an earlier round merging late ends no round:
		// that round is recorded, and nobody has worked this one.
		var made int64
		err := t.QueryRowContext(ctx, `SELECT created_at FROM external_links WHERE issue_id = ? AND url = ? AND kind = 'pull'`, i.ID, viaLink).Scan(&made)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil && made < o.roundStartedAt {
			return nil, nil
		}
	}
	var last *ObjectiveMeasurement
	if o.Check != "" {
		last, err = scanMeasurement(o, t.QueryRowContext(ctx, `SELECT `+measurementCols+` FROM objective_measurements WHERE issue_id = ? AND round_no = ? ORDER BY id DESC LIMIT 1`, i.ID, o.Round))
		if errors.Is(err, sql.ErrNoRows) {
			last, err = nil, nil
		}
		if err != nil {
			return nil, err
		}
	}
	outcome := RoundNone
	switch {
	case o.Check == "" || o.kind() == "loop":
		outcome = RoundLoop
	case last == nil:
		outcome = RoundUnmeasured
	case last.Met:
		outcome = RoundMet
	case last.Improved:
		outcome = RoundProgress
	}
	var mid any
	if last != nil {
		mid = last.ID
	}
	agent := actor.agentName()
	if agent == "" {
		if agent, err = s.workAgent(ctx, t, i.ID); err != nil {
			return nil, err
		}
	}
	if outcome == RoundUnmeasured && i.Status.Category == CatTodo && !byPerson {
		return nil, nil // nothing was worked: no round to record or count
	}
	var pr string
	if err := t.QueryRowContext(ctx, `SELECT url FROM external_links WHERE issue_id = ? AND kind = 'pull' AND created_at >= ? ORDER BY id DESC LIMIT 1`, i.ID, o.roundStartedAt).Scan(&pr); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if _, err := t.ExecContext(ctx, `INSERT INTO objective_rounds (issue_id, round_no, started_at, ended_at, outcome, measurement_id, agent, pr_url) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		i.ID, o.Round, o.roundStartedAt, now, outcome, mid, agent, pr); err != nil {
		return nil, err
	}
	if byPerson {
		// Their close ends it, and the round it ended on is recorded.
		note("ended: closed as " + st.Name)
		return st, end(ObjectiveEnded)
	}
	said := map[string]string{RoundMet: "met", RoundProgress: "better, not met", RoundNone: "no better", RoundUnmeasured: "ended without a check", RoundLoop: "done"}[outcome]
	if last != nil {
		said += " (" + last.Line + ")"
	}
	switch {
	case outcome == RoundMet && !o.endless():
		note(said)
		return st, end(ObjectiveMet)
	case o.MaxRounds > 0 && o.Round >= o.MaxRounds:
		note(said + "; the last of its " + plural1(o.MaxRounds, "round"))
		return st, end(ObjectiveEnded)
	}
	stalled := 0
	if outcome == RoundNone || outcome == RoundUnmeasured {
		stalled = o.Stalled + 1
	}
	wait := time.Duration(o.everyMS) * time.Millisecond
	if stalled > 0 {
		wait = max(wait, min(objectiveStallFirst<<min(stalled-1, 20), objectiveStallMax))
	}
	var next any
	if wait > 0 {
		next = now + wait.Milliseconds()
	}
	todo, err := s.firstStatusIn(ctx, t, i.ProjectID, CatTodo)
	if err != nil {
		// A workflow without a To Do has nowhere to open the task again:
		// the close stands, and so the objective ends with this round.
		note(said + "; ended: the workflow has no To Do status to open the task again in")
		return st, end(ObjectiveEnded)
	}
	if _, err := t.ExecContext(ctx, `UPDATE objectives SET round_no = round_no + 1, round_started_at = ?, stalled = ?, next_at = ?, updated_at = ? WHERE issue_id = ?`,
		now, stalled, next, now, i.ID); err != nil {
		return nil, err
	}
	note(fmt.Sprintf("%s; open again for round %d", said, o.Round+1))
	if todo.ID == i.Status.ID {
		return nil, nil
	}
	return todo, nil
}

// objectiveReopened: an objective's task stayed open where it would have
// closed. The sentence the agent that finished the round reads, "" when the
// issue has no objective in effect.
func (s *Service) objectiveReopened(ctx context.Context, issueID int64) string {
	o, err := s.objectiveOf(ctx, s.db.R, issueID)
	if err != nil || o == nil || o.State != ObjectiveActive || len(o.Rounds) == 0 {
		return ""
	}
	r := o.Rounds[len(o.Rounds)-1]
	msg := fmt.Sprintf("Round %d of %s's objective is recorded", r.Round, o.Key)
	if r.Check != nil {
		msg += " (" + r.Check.Line + ")"
	}
	msg += fmt.Sprintf("; it isn't finished, so the task is open again for round %d", o.Round)
	if o.NextAt != nil {
		msg += ", not before " + o.NextAt.In(time.Local).Format("Jan 2 15:04 MST")
	}
	return msg + "."
}

// objectiveWait is why an objective's task isn't handed out now.
type objectiveWait struct {
	round int
	until int64 // not before; 0: it waits for spare quota
}

// objectiveWaits are the open tasks whose objective keeps them from being
// handed out now: between rounds, or paced to run only on quota that would
// go unused while the actor's week's-end plan doesn't want more work.
func (s *Service) objectiveWaits(ctx context.Context, actor *Actor) (map[int64]objectiveWait, error) {
	now := s.nowMS()
	rows, err := s.db.R.QueryContext(ctx, `SELECT issue_id, round_no, pace, coalesce(next_at, 0) FROM objectives WHERE state = ? AND (next_at > ? OR pace = ?)`, ObjectiveActive, now, PaceSpare)
	if err != nil {
		return nil, err
	}
	type row struct {
		id, next int64
		round    int
		pace     string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.round, &r.pace, &r.next); err != nil {
			_ = rows.Close()
			return nil, err
		}
		all = append(all, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := map[int64]objectiveWait{}
	spare, asked := false, false
	for _, r := range all {
		if r.next > now {
			out[r.id] = objectiveWait{round: r.round, until: r.next}
			continue
		}
		if !asked {
			asked, spare = true, actor.id() != 0 && s.spendGoesOn(ctx, actor.id())
		}
		if !spare {
			out[r.id] = objectiveWait{round: r.round}
		}
	}
	return out, nil
}

// detail says an objective's wait for the queue.
func (w objectiveWait) detail(key string) string {
	if w.until > 0 {
		return fmt.Sprintf("Round %d of its objective isn't handed out before %s (the time between its rounds, or a back-off after rounds that moved nothing). wp next %s takes it now.",
			w.round, msTime(w.until).In(time.Local).Format("Jan 2 15:04 MST"), key)
	}
	return fmt.Sprintf("Its objective runs on quota that would go unused: autopilot takes it over the week's last hours, while \"Use what's left\" is on and the week's use is short of its target. wp next %s takes it now.", key)
}

func hasWait(m map[int64]objectiveWait, id int64) bool { _, ok := m[id]; return ok }
