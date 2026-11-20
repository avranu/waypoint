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
	"unicode/utf8"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Objectives (WP-927, WP-ADR111, WP-ADR142). A task can carry an objective: work done
// in rounds until something other than the agent doing the work says it is
// finished. Four shapes, one mechanism:
//
//   - until a fixed target: "until `go test ./x` runs in under 1s", "until
//     `./coverage` prints at least 80%", "until `make lint` passes". The
//     task closes when a round's reading meets it.
//   - by an amount every round: "2x faster", "-10%". Each round is judged
//     against the commit it started from, so it is never met for good; it
//     asks for more each time, until a person closes the task.
//   - judged: "until a reviewer finds nothing of medium severity or worse
//     against: <what done means>". After every round a model that didn't do
//     the work reads the repository against those words (an audit); what it
//     finds is the next round's brief.
//   - a loop with no check: every round ships what it did and the task opens
//     again, until a person closes it or its rounds run out.
//
// A round's work ends where the task would close: wp done, or the merge of
// the round's pull request. The close doesn't stand: the task goes to To Do
// and waits while Waypoint takes the round's reading itself (objective_verify.go):
// the check runs in fresh checkouts on the runner of the person who put the
// objective in effect, or the judge's audit runs. Only that reading decides
// (objective_rounds.go): met, the task closes; otherwise it opens for the
// next round. What the working agent measures with wp objective check is
// kept and shown, and decides nothing: the agent that did the work never
// says whether the work is enough, and it can't end the rounds by closing,
// cancelling or deleting the task. Only a person's own act does that.
//
// The check and the keep-passing commands are shell commands a person's machine runs. So
// only a person's own act puts an objective in effect: what an agent sets
// is a proposal, shown but not run and not looping, until a person confirms
// the exact proposal they read (rev).
//
// Rounds don't crowd out other work. One that moved nothing backs the task
// off (an hour, doubling to a week, never less than `every`); `every` sets
// the least time between rounds, a day by default for the shapes that never
// end; and pace "spare" hands the task out only while the person's
// week's-end plan wants more work (WP-ADR106).
//
// Nothing about an objective is silent (objective_health.go): its person is
// told when it is met, ended, needs them, or has stood where nothing can
// move it on.

const (
	ObjectiveProposed = "proposed"
	ObjectiveActive   = "active"
	ObjectiveMet      = "met"
	ObjectiveEnded    = "ended"

	// Phases of an objective in effect. Agents are handed its task only in
	// PhaseWork.
	PhaseWork     = ""         // a round is worked, or waited for
	PhaseBaseline = "baseline" // its starting point is being read, before the first round
	PhaseVerdict  = "verdict"  // a round's work ended; its reading is being taken
	PhaseBlocked  = "blocked"  // it needs a person: PhaseWhy, PhaseNote

	// Why an objective is blocked.
	BlockedRepo    = "repo"    // no repository to read it in
	BlockedPerson  = "person"  // nobody whose runner may run it
	BlockedCheck   = "check"   // the check gives no reading at the start
	BlockedTouched = "touched" // the round changed what the check runs
	BlockedReading = "reading" // the reading couldn't be taken, after retries
	BlockedRounds  = "rounds"  // out of rounds, not met

	MeasureTime  = "time"
	MeasureValue = "value"
	MeasurePass  = "pass"
	// MeasureJudge is a judged objective's: the count of the judge's
	// findings at or above its bar, met at none.
	MeasureJudge = "judge"

	PaceQueue = "queue"
	PaceSpare = "spare"

	RoundReading    = "reading" // its work ended; its reading isn't in yet
	RoundMet        = "met"
	RoundProgress   = "progress"
	RoundWorked     = "worked" // work landed; a pass or a judge can't say how much nearer it is
	RoundNone       = "none"
	RoundEmpty      = "empty" // nothing landed: the round is still owed
	RoundUnmeasured = "unmeasured"
	RoundLoop       = "loop"

	// Who took a reading. Only Waypoint's own decide a round.
	SourceAgent    = "agent"    // wp objective check, by the agent working the round
	SourceRunner   = "runner"   // Waypoint's job on the person's runner
	SourceJudge    = "judge"    // an audit by another model
	SourceBaseline = "baseline" // Waypoint's reading before the first round

	// objectiveEveryDefault: the least time between rounds of an objective
	// that never ends by itself, unless its person says otherwise.
	objectiveEveryDefault = 24 * time.Hour
	// objectiveStallFirst doubles for every further round in a row that
	// moved nothing, up to objectiveStallMax. The wait is never less than
	// `every`, so with the default day between rounds the first five
	// stalled rounds all wait that day.
	objectiveStallFirst = time.Hour
	objectiveStallMax   = 7 * 24 * time.Hour
	// objectiveNoise: a change from the base smaller than this isn't called
	// an improvement when the readings vary from run to run; nor is one
	// its runs can't tell from noise. A reading that is the same every run
	// (a count, a coverage figure) is compared exactly.
	objectiveNoise = 0.02
	objectiveP     = 0.05
	// objectiveAgentReadings is how many of a worker's own readings a task
	// keeps (Waypoint's readings are all kept).
	objectiveAgentReadings = 200

	maxObjectiveCmd    = 2000
	maxObjectiveRounds = 1000
	maxObjectiveKeep   = 8
	// maxObjectiveJudge: what a judge reads against goes into an audit as its
	// instructions (auditInstructionsMax).
	maxObjectiveJudge = auditInstructionsMax
	objectiveRecent   = 5
)

// Objective is a task's objective and where its rounds stand.
type Objective struct {
	IssueID int64  `json:"-"`
	Key     string `json:"key"`
	Title   string `json:"title,omitempty"`
	// Kind: "until" (a fixed target), "each" (an amount every round),
	// "judged" (until a judge finds nothing) or "loop" (no target).
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
	// Repo is the repository (owner/name) the check runs in and the judge
	// reads; Branch the branch read ("": its default).
	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch,omitempty"`
	// KeepPassing are commands that must keep passing: a round that breaks one
	// hasn't met the objective, whatever the check reads.
	KeepPassing []string `json:"keep_passing"`
	// Judge is what "done" means, in words, for a model that didn't do the
	// work; JudgeBar the lowest severity of finding that keeps the task
	// open; JudgeModels the models asked, in order (none: the audit default).
	Judge       string   `json:"judge,omitempty"`
	JudgeBar    string   `json:"judge_bar,omitempty"`
	JudgeModels []string `json:"judge_models"`
	Pace        string   `json:"pace"`
	// Every is the least seconds from one round's end to the next's start.
	Every     int64      `json:"every"`
	MaxRounds int        `json:"max_rounds,omitempty"`
	Round     int        `json:"round"`
	Stalled   int        `json:"stalled,omitempty"`
	NextAt    *time.Time `json:"next_at,omitempty"`
	State     string     `json:"state"`
	// Phase is where one in effect stands (PhaseWork …); PhaseWhy and
	// PhaseNote say why it is blocked, key and words.
	Phase     string     `json:"phase,omitempty"`
	PhaseAt   *time.Time `json:"phase_at,omitempty"`
	PhaseWhy  string     `json:"phase_why,omitempty"`
	PhaseNote string     `json:"phase_note,omitempty"`
	// Health says where it stands in a word, for lists (proposed, working,
	// ready, waiting, reading, blocked, stuck, met, ended); HealthNote in a
	// sentence.
	Health     string `json:"health"`
	HealthNote string `json:"health_note,omitempty"`
	// StuckSince: nothing can move it on since then (StuckWhy), as the
	// watchdog found.
	StuckSince *time.Time `json:"stuck_since,omitempty"`
	StuckWhy   string     `json:"stuck_why,omitempty"`
	// BaseSHA is the commit the round under way started from: the last one
	// Waypoint read.
	BaseSHA string `json:"base_sha,omitempty"`
	// Summary says the objective in a sentence; Text is the whole of it as
	// wp show prints it.
	Summary string `json:"summary"`
	// Rev changes with anything a person confirms: the commands, where they
	// run, the target, the judge and the pacing. A confirm names it.
	Rev         string     `json:"rev"`
	Text        string     `json:"text"`
	SetBy       *UserRef   `json:"set_by,omitempty"`
	SetAgent    string     `json:"set_agent,omitempty"`
	ConfirmedBy *UserRef   `json:"confirmed_by,omitempty"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	EndedWhy    string     `json:"ended_why,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
	// Last is the newest reading of any source; Verdict the newest that
	// decides (Waypoint's own); Checks the newest few, newest first; Rounds
	// the last rounds, oldest first; Findings what the judge last found at
	// or above the bar.
	Last     *ObjectiveMeasurement   `json:"last,omitempty"`
	Verdict  *ObjectiveMeasurement   `json:"verdict,omitempty"`
	Checks   []*ObjectiveMeasurement `json:"checks"`
	Rounds   []*ObjectiveRound       `json:"rounds"`
	Findings []ObjectiveFinding      `json:"findings,omitempty"`

	everyMS                 int64
	roundStartedAt          int64
	setBy, confirmedBy      *int64
	issueClosed, issueDraft bool
	issueCategory           string
	projectID               int64
	blockedFrom             string
	closeStatus             *int64
	verifyTry               int
	verifyJob, verifyAudit  *int64
	verifyAt                int64
	// verifyReading: nil while the commands of the reading under way
	// haven't run; then the reading they gave (0: there is no check, so no
	// reading of one); verifyHead the commit they ran at.
	verifyReading      *int64
	verifyHead         string
	pendingMeasurement *int64
	baseOK             *bool
	baseValue          *float64
	stalledTold        int
	stuckToldAt        int64
}

// ObjectiveFinding is one thing a judge found.
type ObjectiveFinding struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Problem  string `json:"problem,omitempty"`
}

// ObjectiveMeasurement is one reading.
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
	// Source is who took it (SourceAgent …); Decides whether it is one of
	// Waypoint's own, of the objective as it stands.
	Source  string `json:"source"`
	Decides bool   `json:"decides"`
	// Exact: every run read the same, so it is compared exactly.
	Exact bool `json:"exact,omitempty"`
	// Touched are files the round changed that the check's own commands
	// name; Broken the keep-passing commands that no longer pass.
	Touched []string `json:"touched,omitempty"`
	Broken  []string `json:"broken,omitempty"`
	// Note is what the command last printed when it gave no reading, or
	// the judge's summary.
	Note    string `json:"note,omitempty"`
	JobID   int64  `json:"job_id,omitempty"`
	AuditID int64  `json:"audit_id,omitempty"`
	// Line says it in words: "1.42s (base 2.1s, 32% better)".
	Line string `json:"line"`

	rev string
}

// ObjectiveRound is one round.
type ObjectiveRound struct {
	Round     int                   `json:"round"`
	StartedAt time.Time             `json:"started_at"`
	EndedAt   time.Time             `json:"ended_at"`
	Outcome   string                `json:"outcome"`
	Check     *ObjectiveMeasurement `json:"check,omitempty"`
	Agent     string                `json:"agent,omitempty"`
	PR        string                `json:"pr_url,omitempty"`
	BaseSHA   string                `json:"base_sha,omitempty"`
	HeadSHA   string                `json:"head_sha,omitempty"`
	Note      string                `json:"note,omitempty"`
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
	Repo      *string `json:"repo"`
	Branch    *string `json:"branch"`
	// KeepPassing replaces the commands that must keep passing.
	KeepPassing *[]string `json:"keep_passing"`
	// Judge is the criteria of a judged objective ("" takes the judge off);
	// JudgeBar critical, high, medium (the default) or low; JudgeModels the
	// models to ask.
	Judge       *string   `json:"judge"`
	JudgeBar    *string   `json:"judge_bar"`
	JudgeModels *[]string `json:"judge_models"`
	// Rev is the agent's proposal the person read, when their change builds
	// on it: without it a person's change starts from nothing, so no
	// command an agent wrote runs unread.
	Rev string `json:"rev"`
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
	Exact     bool     `json:"exact"`
	// Rev is the objective the CLI measured (Objective.Rev): a reading of
	// one changed since isn't kept.
	Rev string `json:"rev"`
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
	objFixedRe = lazyre.New(`^(<=|>=|<|>)\s*([0-9]*\.?[0-9]+(?:[eE][+-]?[0-9]+)?)\s*(\S.*)?$`)
	objTimesRe = lazyre.New(`^([0-9]*\.?[0-9]+)\s*[xX×]$`)
	objPctRe   = lazyre.New(`^([+-])\s*([0-9]*\.?[0-9]+)\s*%$`)
	// A unit is words in letters: "s", "%", "warnings", "ops/s", "open
	// findings". No digits, so "< 1h30m" isn't read as 1 of a unit "h30m".
	objUnitRe = lazyre.New(`^[\p{L}%/°$€£][\p{L}%/°_-]*( [\p{L}%/°][\p{L}%/°_-]*){0,3}$`)
)

// timeUnits are the units that make a fixed target a time, in seconds.
var timeUnits = map[string]float64{
	"ns": 1e-9, "us": 1e-6, "µs": 1e-6, "ms": 1e-3, "s": 1, "sec": 1, "secs": 1, "second": 1, "seconds": 1,
	"m": 60, "min": 60, "mins": 60, "minute": 60, "minutes": 60, "h": 3600, "hour": 3600, "hours": 3600,
}

// judgeBars are the severities a judge's bar can be, worst first.
var judgeBars = []string{"critical", "high", "medium", "low"}

// parseObjectiveTarget reads a target: "" (a loop), "pass", a fixed one
// ("< 1s", ">= 80%", "<= 120 warnings") or a relative one ("2x", "-10%",
// "+5%"). measure and better are what the person said, "" to let the target
// decide: a time unit makes it a time, any other fixed target a value the
// command prints; a relative one is a time unless it asks for more ("+5%").
// The text it keeps (until) is the person's own number and unit, so reading
// it again gives the same target.
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
		if unit != "" && !objUnitRe.MatchString(unit) {
			return t, invalid("the target %q: after the number comes a unit in letters (< 1.5s, >= 80%%, < 10 warnings); a time is one number and one unit (< 90m, not < 1h30m)", until)
		}
		if len(unit) > 24 {
			return t, invalid("the unit %q is too long", unit)
		}
		if mult, ok := timeUnits[strings.ToLower(unit)]; ok && measure != MeasureValue {
			unit = strings.ToLower(unit)
			if v <= 0 && strings.HasPrefix(m[1], "<") {
				return t, invalid("nothing runs in %s %s%s: give a time above 0", map[string]string{"<": "under", "<=": "at most"}[m[1]], m[2], unit)
			}
			t.measure, v = MeasureTime, v*mult
			if math.IsInf(v, 0) {
				return t, invalid("%q isn't a time", until)
			}
			t.until = m[1] + " " + m[2] + unit
		} else {
			if measure == MeasureTime {
				return t, invalid("a time target needs a time unit: %s %ss, %s %sms", m[1], m[2], m[1], m[2])
			}
			t.measure, t.unit = MeasureValue, unit
			t.until = m[1] + " " + m[2]
			switch unit {
			case "":
			case "%":
				t.until += "%"
			default:
				t.until += " " + unit
			}
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
		t.factor, t.until = &f, m[1]+"x"
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
		t.factor, t.until = &f, m[1]+m[2]+"%"
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

// measured names what a reading is a reading of: the commands, where they
// run, the target and the judge. A reading taken under another one never
// decides a round.
func (o *Objective) measured() []string {
	num := func(p *float64) string {
		if p == nil {
			return ""
		}
		return strconv.FormatFloat(*p, 'g', -1, 64)
	}
	return []string{"2", o.Check, o.Setup, o.Dir, o.Measure, o.Op, num(o.Target), num(o.Factor), o.Better, o.Unit, o.Repo, o.Branch,
		strings.Join(o.KeepPassing, "\x01"), o.Judge, o.JudgeBar, strings.Join(o.JudgeModels, "\x01")}
}

func objectiveHash(parts []string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:6])
}

func (o *Objective) measuredRev() string { return objectiveHash(o.measured()) }

// rev names everything a person confirms: what is measured, and how the
// rounds are paced and bounded. A person confirms the proposal they read
// and not one written over it since.
func (o *Objective) rev() string {
	return objectiveHash(append(o.measured(), o.Pace, strconv.FormatInt(o.everyMS, 10), strconv.Itoa(o.MaxRounds)))
}

func (o *Objective) kind() string {
	switch {
	case o.Factor != nil:
		return "each"
	case o.Measure == MeasurePass || o.Target != nil:
		return "until"
	case o.Judge != "":
		return "judged"
	}
	return "loop"
}

// endless: no round ends it; only a person or its rounds running out does.
func (o *Objective) endless() bool { k := o.kind(); return k == "each" || k == "loop" }

// verified: its rounds are read by Waypoint (a check, a judge); a loop's
// aren't read at all.
func (o *Objective) verified() bool { return o.Check != "" || o.Judge != "" }

// judge says whether a reading meets the objective, and whether it is
// better than the base it was measured beside.
func (o *Objective) judge(m *ObjectiveMeasurement) (met, improved bool) {
	if m.Moved || len(m.Broken) > 0 {
		return false, false
	}
	var base *float64
	if m.BaseOK != nil && *m.BaseOK && m.BaseValue != nil {
		base = m.BaseValue
	}
	switch {
	case m.Source == SourceJudge:
		// The count of findings at or above the bar: met at none.
		if !m.OK || m.Value == nil {
			return false, false
		}
		return *m.Value == 0, base != nil && *m.Value < *base
	case o.Measure == MeasurePass:
		return m.OK, m.OK && m.BaseOK != nil && !*m.BaseOK
	case o.Measure == "" || o.Measure == MeasureJudge || !m.OK || m.Value == nil:
		return false, false
	}
	v := *m.Value
	lower := o.Better != "higher"
	switch {
	case base != nil:
		// A reading that varies from run to run has to clear the noise; one
		// that is the same every run is compared as it is.
		gap := 0.0
		if !m.Exact {
			gap = math.Abs(*base) * objectiveNoise
		}
		if lower {
			improved = v < *base-gap
		} else {
			improved = v > *base+gap
		}
		if !m.Exact && m.P != nil && *m.P >= objectiveP {
			improved = false
		}
	case m.BaseOK != nil:
		improved = true // it reads where the base gave nothing
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
		met = met && improved
	}
	return met, improved
}

// barWords: "medium severity or worse".
func barWords(bar string) string {
	if bar == "critical" {
		return "critical severity"
	}
	return bar + " severity or worse"
}

// line says a reading in words.
func (o *Objective) line(m *ObjectiveMeasurement) string {
	sha := func(s string) string { return s[:min(7, len(s))] }
	var b strings.Builder
	if m.Source == SourceJudge {
		switch {
		case !m.OK || m.Value == nil:
			b.WriteString("the judge gave no verdict")
		case *m.Value == 0:
			b.WriteString("the judge found nothing of " + barWords(o.JudgeBar))
		default:
			b.WriteString("the judge found " + plural1(int(*m.Value), "thing") + " of " + barWords(o.JudgeBar))
		}
		if m.BaseValue != nil && m.OK && m.Value != nil && *m.Value > 0 {
			fmt.Fprintf(&b, " (%s the round before)", objectiveNumber(*m.BaseValue, ""))
		}
		return b.String()
	}
	if m.Runs == 0 && m.Source != SourceAgent {
		// No check ran: Waypoint's reading is of the keep-passing commands alone.
		if len(m.Broken) == 0 {
			return "what must keep passing does"
		}
		return "`" + strings.Join(m.Broken, "`, `") + "` no longer passes"
	}
	switch {
	case o.Measure == MeasurePass && m.OK:
		b.WriteString("passes")
	case !m.OK:
		b.WriteString("the command failed")
	case m.Value == nil:
		b.WriteString("the command printed no number")
	default:
		b.WriteString(o.value(*m.Value))
		if m.Runs > 1 && m.Low != nil && m.High != nil && !m.Exact {
			fmt.Fprintf(&b, " (median of %d, middle half %s to %s)", m.Runs, o.value(*m.Low), o.value(*m.High))
		}
	}
	if m.BaseOK != nil {
		b.WriteString("; base")
		if m.BaseSHA != "" {
			b.WriteString(" " + sha(m.BaseSHA))
		}
		b.WriteString(": ")
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
				switch {
				case *m.Value == *m.BaseValue:
					b.WriteString(", the same")
				case math.Abs(pct) < 0.5:
					fmt.Fprintf(&b, ", %.2g%% %s", math.Abs(pct), word)
				default:
					fmt.Fprintf(&b, ", %.0f%% %s", math.Abs(pct), word)
				}
				if !m.Exact && m.P != nil && *m.P >= objectiveP {
					b.WriteString(", which its runs can't tell from noise")
				}
			}
			if o.Factor != nil && *m.BaseValue <= 0 {
				b.WriteString("; an amount every round can't be judged from a base of " + o.value(*m.BaseValue))
			}
		}
	}
	if m.Moved {
		b.WriteString("; files changed while it ran, so it doesn't count")
	}
	for _, h := range m.Broken {
		b.WriteString("; `" + h + "` no longer passes")
	}
	if len(m.Touched) > 0 {
		b.WriteString("; the round changed " + strings.Join(m.Touched, ", ") + ", which the check runs")
	}
	return b.String()
}

// judgeSummary: "a judge finds nothing of medium severity or worse against: …".
func (o *Objective) judgeSummary() string {
	return "a judge finds nothing of " + barWords(o.JudgeBar) + " against: " + excerpt(strings.Join(strings.Fields(o.Judge), " "), 160)
}

// summary says the objective in a sentence.
func (o *Objective) summary() string {
	cmd := "`" + o.Check + "`"
	if o.Dir != "" {
		cmd += " (in " + o.Dir + ")"
	}
	words := map[string]string{"<": "under", "<=": "at most", ">": "over", ">=": "at least"}
	var s string
	switch {
	case o.Check == "" && o.Judge != "":
		s = "until " + o.judgeSummary()
	case o.Check == "":
		return "a loop: every round ships what it did, and the task opens again"
	case o.Measure == MeasurePass:
		s = "until " + cmd + " passes"
	case o.Target != nil && o.Measure == MeasureTime:
		s = fmt.Sprintf("until %s runs in %s %s", cmd, words[o.Op], benchSecs(*o.Target))
	case o.Target != nil:
		s = fmt.Sprintf("until %s prints %s %s", cmd, words[o.Op], objectiveNumber(*o.Target, o.Unit))
	case o.Factor != nil && o.Measure == MeasureTime:
		s = fmt.Sprintf("every round, %s runs %s faster than at the round's base commit", cmd, o.relWords())
	case o.Factor != nil:
		s = fmt.Sprintf("every round, what %s prints gets %s %s than at the round's base commit", cmd, o.relWords(), o.Better)
	default:
		return "a loop that reads " + cmd + " every round: nothing is judged, and the task opens again"
	}
	if o.Judge != "" && o.Check != "" {
		s += ", and " + o.judgeSummary()
	}
	if n := len(o.KeepPassing); n > 0 {
		s += fmt.Sprintf(", while %s", map[bool]string{true: "`" + o.KeepPassing[0] + "` keeps passing", false: plural1(n, "command") + " keep passing"}[n == 1])
	}
	return s
}

// relWords: "2x", "10%".
func (o *Objective) relWords() string { return strings.TrimLeft(o.Until, "+-") }

// roundWords says a round's outcome.
func roundWords(outcome string) string {
	switch outcome {
	case RoundReading:
		return "its work ended; Waypoint is reading it"
	case RoundMet:
		return "met"
	case RoundProgress:
		return "better, not met"
	case RoundWorked:
		return "work landed, not met"
	case RoundNone:
		return "no better"
	case RoundEmpty:
		return "nothing landed, so the round is still owed"
	case RoundUnmeasured:
		return "ended without a reading"
	}
	return "done"
}

// objectiveClock prints a time for people and agents alike: UTC, so it
// reads the same on the server, in the web and in a terminal elsewhere.
func objectiveClock(t time.Time) string { return t.UTC().Format("Jan 2 15:04 UTC") }

// ObjectiveText is the objective as wp show and wp next print it. forAgent
// adds what a round is and how it ends.
func ObjectiveText(o *Objective, forAgent bool, now time.Time) string {
	if o == nil {
		return ""
	}
	var b strings.Builder
	head := "Objective"
	switch o.State {
	case ObjectiveProposed:
		head = "Proposed objective (an agent's; not in effect, and none of its commands run, until a person confirms it on the task's page or with wp objective confirm " + o.Key + ")"
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
	if o.EndedWhy != "" && (o.State == ObjectiveMet || o.State == ObjectiveEnded) {
		b.WriteString("  " + upperFirst(o.EndedWhy) + ".\n")
	}
	var how []string
	if o.Check != "" {
		where := "`" + o.Check + "`"
		if o.Dir != "" {
			where += " in " + o.Dir
		}
		if o.Repo != "" {
			where += " of " + o.Repo
			if o.Branch != "" {
				where += " (" + o.Branch + ")"
			}
		}
		how = append(how, "reads "+where)
	}
	switch o.Measure {
	case MeasureTime:
		how = append(how, "median wall time of several runs")
	case MeasureValue:
		if o.Unit != "" {
			// What readValue in cmd/wp/objective_measure.go does with a unit.
			how = append(how, "the last number printed directly before \""+o.Unit+"\" (any case; \"81.3% of 1204\" reads 81.3 for %), on the last line that has one")
		} else {
			how = append(how, "the last number on the command's last non-empty line of output (a dash between numbers, as in 10-20, is a range, not a sign)")
		}
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
	for _, h := range o.KeepPassing {
		b.WriteString("  Keeps passing: `" + h + "`.\n")
	}
	if o.Judge != "" {
		b.WriteString("  Judged (anything of " + barWords(o.JudgeBar) + " keeps it open) against: " + strings.TrimSpace(o.Judge) + "\n")
	}
	for _, r := range o.Rounds {
		if r.Outcome == RoundReading {
			continue // said below, with its phase
		}
		line := fmt.Sprintf("  Round %d: %s", r.Round, roundWords(r.Outcome))
		if r.Check != nil {
			line += " (" + r.Check.Line + ")"
		}
		if r.Note != "" {
			line += ": " + r.Note
		}
		b.WriteString(line + ".\n")
	}
	if o.State != ObjectiveActive {
		return b.String()
	}
	switch o.Phase {
	case PhaseBaseline:
		b.WriteString("  Waypoint is reading where it starts from; round 1 is handed out once it has.\n")
	case PhaseVerdict:
		fmt.Fprintf(&b, "  Round %d's work ended; Waypoint is reading it, and the task waits for that.\n", o.Round)
	case PhaseBlocked:
		b.WriteString("  Needs a person: " + o.PhaseNote + "\n")
	}
	if o.StuckSince != nil && o.Phase != PhaseBlocked {
		fmt.Fprintf(&b, "  Stuck since %s: %s\n", objectiveClock(*o.StuckSince), o.StuckWhy)
	}
	if o.Last != nil && o.Last.Source == SourceAgent && o.Last.Round == o.Round && o.Phase == PhaseWork {
		fmt.Fprintf(&b, "  This round's last working reading, %s: %s. It is the working agent's own and decides nothing.\n", agoWords(now.Sub(o.Last.At)), o.Last.Line)
	}
	if o.Phase == PhaseWork && o.NextAt != nil && o.NextAt.After(now) {
		fmt.Fprintf(&b, "  Agents aren't handed round %d before %s", o.Round, objectiveClock(*o.NextAt))
		if o.Stalled > 0 {
			b.WriteString(": " + plural1(o.Stalled, "round") + " in a row moved nothing")
		}
		b.WriteString(".\n")
	}
	if len(o.Findings) > 0 {
		b.WriteString("  What the judge last found, for this round to settle:\n")
		for _, f := range o.Findings {
			at := ""
			if f.Path != "" {
				at = " (" + f.Path
				if f.Line > 0 {
					at += ":" + strconv.Itoa(f.Line)
				}
				at += ")"
			}
			fmt.Fprintf(&b, "    - [%s] %s%s", f.Severity, f.Title, at)
			if forAgent && f.Problem != "" {
				b.WriteString(": " + excerpt(strings.Join(strings.Fields(f.Problem), " "), 600))
			}
			b.WriteString("\n")
		}
	}
	if !forAgent || o.Phase != PhaseWork {
		return b.String()
	}
	switch {
	case !o.verified():
		b.WriteString("  A round is one useful step. Ship it as usual (wp ship): the task opens again for the next round by itself.\n")
	default:
		b.WriteString("  A round is one real step toward it, not the whole of it. Ship what you gained (wp ship) even when it isn't met. When the round's work lands, Waypoint reads the result itself")
		if o.Check != "" {
			b.WriteString(", in a fresh checkout")
		}
		if o.Judge != "" {
			b.WriteString(", and a model that didn't do the work judges it against the words above")
		}
		b.WriteString("; that reading alone decides whether the task closes or opens for the next round. Nothing you report decides it, and closing or cancelling the task doesn't end it: if the objective itself is wrong or can't be reached, say so with wp request ask.\n")
		if o.Check != "" {
			b.WriteString("  `wp objective check " + o.Key + "` reads this tree beside its base commit, for you to work by. Don't change what the check runs (its scripts, its inputs) to move the reading: a round that does waits for a person to look at it.\n")
		}
		if o.Judge != "" && len(o.Findings) > 0 {
			b.WriteString("  Settle each finding in the code. Where one is wrong, say why with the evidence in your closing note (wp ship -m): the next judge reads it.\n")
		}
		if o.Stalled >= 2 {
			fmt.Fprintf(&b, "  The last %d rounds moved nothing. Before another attempt, find out why: read what they tried (this task's comments and pull requests), measure where the problem actually is, and write what you learn in your closing note so the next round builds on it.\n", o.Stalled)
		} else {
			b.WriteString("  If nothing you tried helped, say what you tried in your closing note so the next round doesn't repeat it.\n")
		}
	}
	return b.String()
}

const objectiveCols = `o.issue_id, p.key || '-' || i.number, i.title, i.project_id, i.draft, st.category,
	o.check_cmd, o.dir, o.setup, o.measure, o.until, o.op, o.target, o.factor, o.better, o.unit, o.pace, o.every_ms, o.max_rounds,
	o.round_no, o.round_started_at, o.stalled, o.next_at, o.state, o.set_by, o.set_agent, o.confirmed_by, o.confirmed_at, o.ended_at, o.updated_at,
	o.repo, o.branch, o.keep_passing, o.judge, o.judge_bar, o.judge_models, o.phase, o.phase_at, o.phase_why, o.phase_note, o.blocked_from, o.close_status_id,
	o.verify_try, o.verify_job, o.verify_audit, coalesce(o.verify_at, 0), o.pending_measurement, o.base_sha, o.base_ok, o.base_value, o.ended_why,
	o.stuck_since, o.stuck_why, coalesce(o.stuck_told_at, 0), o.stalled_told, o.verify_reading, o.verify_head`

const objectiveFrom = ` FROM objectives o JOIN issues i ON i.id = o.issue_id JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id`

// stringList reads a JSON array of strings a column holds; never nil, so
// it is [] in what the API returns.
func stringList(raw string) []string {
	if out := jsonStrings(raw); out != nil {
		return out
	}
	return []string{}
}

func scanObjective(sc interface{ Scan(...any) error }) (*Objective, error) {
	o := &Objective{Checks: []*ObjectiveMeasurement{}, Rounds: []*ObjectiveRound{}}
	var target, factor, baseValue sql.NullFloat64
	var next, setBy, confBy, confAt, ended, phaseAt, closeStatus, job, audit, pending, stuck, reading sql.NullInt64
	var baseOK sql.NullBool
	var updated int64
	var keep, models string
	if err := sc.Scan(&o.IssueID, &o.Key, &o.Title, &o.projectID, &o.issueDraft, &o.issueCategory,
		&o.Check, &o.Dir, &o.Setup, &o.Measure, &o.Until, &o.Op, &target, &factor, &o.Better, &o.Unit, &o.Pace, &o.everyMS, &o.MaxRounds,
		&o.Round, &o.roundStartedAt, &o.Stalled, &next, &o.State, &setBy, &o.SetAgent, &confBy, &confAt, &ended, &updated,
		&o.Repo, &o.Branch, &keep, &o.Judge, &o.JudgeBar, &models, &o.Phase, &phaseAt, &o.PhaseWhy, &o.PhaseNote, &o.blockedFrom, &closeStatus,
		&o.verifyTry, &job, &audit, &o.verifyAt, &pending, &o.BaseSHA, &baseOK, &baseValue, &o.EndedWhy,
		&stuck, &o.StuckWhy, &o.stuckToldAt, &o.stalledTold, &reading, &o.verifyHead); err != nil {
		return nil, err
	}
	o.issueClosed = isClosed(o.issueCategory)
	o.KeepPassing, o.JudgeModels = stringList(keep), stringList(models)
	if target.Valid {
		o.Target = &target.Float64
	}
	if factor.Valid {
		o.Factor = &factor.Float64
	}
	if baseValue.Valid {
		o.baseValue = &baseValue.Float64
	}
	if baseOK.Valid {
		o.baseOK = &baseOK.Bool
	}
	ms := func(v sql.NullInt64) *time.Time {
		if !v.Valid {
			return nil
		}
		return ptrOf(msTime(v.Int64))
	}
	id := func(v sql.NullInt64) *int64 {
		if !v.Valid {
			return nil
		}
		return &v.Int64
	}
	o.NextAt, o.ConfirmedAt, o.EndedAt, o.PhaseAt, o.StuckSince = ms(next), ms(confAt), ms(ended), ms(phaseAt), ms(stuck)
	o.setBy, o.confirmedBy, o.closeStatus, o.verifyJob, o.verifyAudit, o.pendingMeasurement = id(setBy), id(confBy), id(closeStatus), id(job), id(audit), id(pending)
	o.verifyReading = id(reading)
	// Seconds, rounded up: a wait under a second isn't shown as none.
	o.Every, o.UpdatedAt = (o.everyMS+999)/1000, msTime(updated)
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

const measurementCols = `id, round_no, at, ok, value, base_ok, base_value, base_sha, tree, commit_sha, runs, low, high, p, moved, met, improved, agent,
	source, rev, coalesce(job_id, 0), coalesce(audit_id, 0), touched, broken, note, exact`

// decidingSources are the readings that are Waypoint's own.
var decidingSources = []string{SourceRunner, SourceJudge, SourceBaseline}

func scanMeasurement(o *Objective, sc interface{ Scan(...any) error }) (*ObjectiveMeasurement, error) {
	m := &ObjectiveMeasurement{}
	var at int64
	var value, baseValue, low, high, p sql.NullFloat64
	var baseOK sql.NullBool
	var touched, broken string
	if err := sc.Scan(&m.ID, &m.Round, &at, &m.OK, &value, &baseOK, &baseValue, &m.BaseSHA, &m.Tree, &m.Commit, &m.Runs, &low, &high, &p, &m.Moved, &m.Met, &m.Improved, &m.Agent,
		&m.Source, &m.rev, &m.JobID, &m.AuditID, &touched, &broken, &m.Note, &m.Exact); err != nil {
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
	if l := stringList(touched); len(l) > 0 {
		m.Touched = l
	}
	if l := stringList(broken); len(l) > 0 {
		m.Broken = l
	}
	// Judged against the objective as it stands, not as it stood when the
	// reading was taken; and one taken of another command or target says
	// nothing about this one.
	m.Decides = slices.Contains(decidingSources, m.Source) && m.rev == o.measuredRev()
	if m.rev == "" || m.rev == o.measuredRev() {
		m.Met, m.Improved = o.judge(m)
	} else {
		m.Met, m.Improved = false, false
	}
	m.Line = o.line(m)
	if m.rev != "" && m.rev != o.measuredRev() {
		m.Line += "; taken before the objective changed, so it doesn't count"
	}
	return m, nil
}

// measurements reads readings, checking the rows' own error.
func (s *Service) measurements(ctx context.Context, q queryer, o *Objective, query string, args ...any) ([]*ObjectiveMeasurement, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ObjectiveMeasurement
	for rows.Next() {
		m, err := scanMeasurement(o, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// measurementByID is one reading; nil when it is gone.
func (s *Service) measurementByID(ctx context.Context, q queryer, o *Objective, id int64) (*ObjectiveMeasurement, error) {
	m, err := scanMeasurement(o, q.QueryRowContext(ctx, `SELECT `+measurementCols+` FROM objective_measurements WHERE id = ? AND issue_id = ?`, id, o.IssueID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// objectiveOf is an issue's objective with its newest readings and last
// rounds; nil for none.
func (s *Service) objectiveOf(ctx context.Context, q queryer, issueID int64) (*Objective, error) {
	o, err := s.objectiveRow(ctx, q, issueID)
	if err != nil || o == nil {
		return nil, err
	}
	o.SetBy, o.ConfirmedBy = s.userRefPtr(ctx, q, o.setBy), s.userRefPtr(ctx, q, o.confirmedBy)
	if o.Checks, err = s.measurements(ctx, q, o, `SELECT `+measurementCols+` FROM objective_measurements WHERE issue_id = ? ORDER BY id DESC LIMIT ?`, issueID, objectiveRecent); err != nil {
		return nil, err
	}
	if o.Checks == nil {
		o.Checks = []*ObjectiveMeasurement{}
	}
	if len(o.Checks) > 0 {
		o.Last = o.Checks[0]
	}
	verdicts, err := s.measurements(ctx, q, o, `SELECT `+measurementCols+` FROM objective_measurements WHERE issue_id = ? AND source != ? AND rev = ? ORDER BY id DESC LIMIT 1`,
		issueID, SourceAgent, o.measuredRev())
	if err != nil {
		return nil, err
	}
	if len(verdicts) > 0 {
		o.Verdict = verdicts[0]
	}
	rows, err := q.QueryContext(ctx, `SELECT round_no, started_at, ended_at, outcome, coalesce(measurement_id, 0), agent, pr_url, base_sha, head_sha, note FROM objective_rounds
		WHERE issue_id = ? ORDER BY id DESC LIMIT ?`, issueID, objectiveRecent)
	if err != nil {
		return nil, err
	}
	var checks []int64
	for rows.Next() {
		r := &ObjectiveRound{}
		var started, ended, mid int64
		if err := rows.Scan(&r.Round, &started, &ended, &r.Outcome, &mid, &r.Agent, &r.PR, &r.BaseSHA, &r.HeadSHA, &r.Note); err != nil {
			_ = rows.Close()
			return nil, err
		}
		r.StartedAt, r.EndedAt = msTime(started), msTime(ended)
		o.Rounds, checks = append([]*ObjectiveRound{r}, o.Rounds...), append([]int64{mid}, checks...)
	}
	if err := cmpErr(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for n, mid := range checks {
		if mid == 0 {
			continue
		}
		if o.Rounds[n].Check, err = s.measurementByID(ctx, q, o, mid); err != nil {
			return nil, err
		}
	}
	if o.State == ObjectiveActive && o.Judge != "" {
		if o.Findings, err = s.objectiveFindings(ctx, q, o); err != nil {
			return nil, err
		}
	}
	now := s.now()
	o.Health, o.HealthNote = o.health(now)
	o.HealthNote = upperFirst(o.HealthNote)
	o.Text = ObjectiveText(o, false, now)
	return o, nil
}

// health says where an objective stands in a word and a sentence.
func (o *Objective) health(now time.Time) (string, string) {
	switch {
	case o.State == ObjectiveProposed:
		return "proposed", "An agent's proposal: nothing runs until a person confirms it."
	case o.State != ObjectiveActive:
		return o.State, upperFirst(o.EndedWhy)
	case o.Phase == PhaseBlocked:
		return "blocked", o.PhaseNote
	case o.StuckSince != nil:
		return "stuck", o.StuckWhy
	case o.Phase == PhaseBaseline:
		return "reading", strings.TrimSpace("Waypoint is reading where it starts from. " + o.PhaseNote)
	case o.Phase == PhaseVerdict:
		return "reading", strings.TrimSpace(fmt.Sprintf("Waypoint is reading round %d. ", o.Round) + o.PhaseNote)
	case o.NextAt != nil && o.NextAt.After(now):
		return "waiting", fmt.Sprintf("Round %d isn't handed out before %s.", o.Round, objectiveClock(*o.NextAt))
	case o.issueCategory == CatInProgress:
		return "working", fmt.Sprintf("Round %d is being worked.", o.Round)
	}
	return "ready", fmt.Sprintf("Round %d waits to be handed out.", o.Round)
}

// agoWords: "just now", "12 minutes ago", "3 hours ago", "2 days ago".
func agoWords(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural1(int(d/time.Minute), "minute") + " ago"
	case d < 48*time.Hour:
		return plural1(int(d/time.Hour), "hour") + " ago"
	}
	return plural1(int(d/(24*time.Hour)), "day") + " ago"
}

// objectivePerson: the act is a person's own. Waypoint itself isn't one
// here (the system actor passes for a person elsewhere): what it does by
// itself never puts a command in effect, and never ends an objective the
// way a person's close does.
func objectivePerson(a *Actor) bool {
	return a.actsAsPerson() && a.User != nil && a.User.ID != 0
}

// objectiveEvent is a change of an objective, in the task's history. what
// names it for the handlers that act on it (objective_health.go).
func objectiveEvent(o *Objective, what, old, text string) *Event {
	return &Event{Kind: EvIssueUpdated, ProjectID: o.projectID, IssueID: o.IssueID, IssueKey: o.Key, Field: "objective", Old: old, New: text,
		Data: map[string]any{"objective": what}}
}

// objectiveColumns are the columns objSet may write, by name. The name that
// reaches the query is the one written here, never the caller's string.
var objectiveColumns = func() map[string]string {
	m := map[string]string{}
	for _, c := range []string{
		"base_ok", "base_sha", "base_value", "better", "blocked_from", "branch",
		"check_cmd", "close_status_id", "confirmed_at", "confirmed_by", "dir", "ended_at",
		"ended_why", "every_ms", "factor", "judge", "judge_bar", "judge_models",
		"keep_passing", "max_rounds", "measure", "next_at", "op", "pace",
		"pending_measurement", "phase", "phase_at", "phase_note", "phase_why", "repo",
		"round_no", "round_started_at", "set_agent", "set_by", "setup", "stalled",
		"stalled_told", "state", "stuck_since", "stuck_told_at", "stuck_why", "target",
		"unit", "until", "verify_at", "verify_audit", "verify_head", "verify_job",
		"verify_reading", "verify_try",
	} {
		m[c] = c
	}
	return m
}()

// objSet writes columns of an objective: pairs of a column and its value.
// A name that isn't one of objectiveColumns is a bug here, and an error.
func (s *Service) objSet(ctx context.Context, t *tx, issueID int64, kv ...any) error {
	if len(kv)%2 != 0 {
		return fmt.Errorf("objSet: %d arguments is not pairs of a column and a value", len(kv))
	}
	sets, args := []string{"updated_at = ?"}, []any{s.nowMS()}
	for n := 1; n < len(kv); n += 2 {
		name, _ := kv[n-1].(string)
		col, ok := objectiveColumns[name]
		if !ok {
			return fmt.Errorf("objSet: %v is not a column of an objective", kv[n-1])
		}
		sets, args = append(sets, col+" = ?"), append(args, kv[n])
	}
	// #nosec G202 -- the columns are objectiveColumns' own names; values are bound
	_, err := t.ExecContext(ctx, `UPDATE objectives SET `+strings.Join(sets, ", ")+` WHERE issue_id = ?`, append(args, issueID)...)
	return err
}

// verifyReset forgets a reading under way: the next one starts from nothing.
func verifyReset() []any {
	return []any{"verify_try", 0, "verify_job", nil, "verify_audit", nil, "verify_at", nil, "verify_reading", nil, "verify_head", "", "pending_measurement", nil}
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

// objectiveListMost is how many objectives a list holds; a variable for tests.
var objectiveListMost = 500

// ListObjectives lists the objectives the actor can see, in a project or
// everywhere: those that need a person first (proposals, blocked and stuck
// ones), then the rest in effect, whatever their task's status; all adds
// met and ended ones.
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
		// One in effect is listed even on a closed task: that is a fault to
		// see. A proposal on a closed task is nothing to act on.
		q += ` AND (o.state = 'active' OR (o.state = 'proposed' AND st.category NOT IN ('done', 'canceled')))`
	}
	q += ` ORDER BY CASE WHEN o.state = 'proposed' THEN 0 WHEN o.state = 'active' AND (o.phase = 'blocked' OR o.stuck_since IS NOT NULL) THEN 0
		WHEN o.state = 'active' THEN 1 ELSE 2 END, o.updated_at DESC, o.issue_id`
	rows, err := s.db.R.QueryContext(ctx, q, args...)
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
	if err := cmpErr(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	out := []*Objective{}
	for _, id := range ids {
		if len(out) == objectiveListMost {
			break
		}
		if !s.canSee(ctx, s.db.R, actor, id[0], id[1]) {
			continue
		}
		o, err := s.objectiveOf(ctx, s.db.R, id[0])
		if err != nil {
			return nil, err
		}
		if o != nil { // nil: taken off since the ids were read
			out = append(out, o)
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

// cleanKeepPassing trims the commands that must keep passing and drops repeats.
func cleanKeepPassing(in []string) ([]string, error) {
	out := []string{}
	for _, h := range in {
		h = strings.TrimSpace(h)
		if h == "" || slices.Contains(out, h) {
			continue
		}
		out = append(out, h)
	}
	if len(out) > maxObjectiveKeep {
		return nil, invalid("at most %d commands that must keep passing: put the rest in one script in the repository", maxObjectiveKeep)
	}
	return out, nil
}

// SetObjective gives an issue an objective, or changes it. A person's own
// act puts it in effect; an agent's is a proposal until a person confirms
// it, and an agent can't change one that is in effect: its commands run on
// a person's machine, and its target is what the agent's own work is
// measured by.
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
		if todo, err := s.objectiveTodo(ctx, t, i.ProjectID); err != nil {
			return err
		} else if todo == nil {
			return invalid("%s's workflow has no To Do status, and that is where a task waits for its next round: add one to the project's workflow first", i.Key)
		}
		cur, err := s.objectiveRow(ctx, t, i.ID)
		if err != nil {
			return err
		}
		person := objectivePerson(actor)
		if cur != nil && cur.State == ObjectiveProposed && !person && cur.SetAgent != "" && cur.SetAgent != actor.creditAgent() {
			return forbidden("%s's proposed objective is %s's: another agent can't rewrite it before a person reads it", i.Key, cur.SetAgent)
		}
		if cur != nil && cur.State != ObjectiveProposed && !person {
			return forbidden("%s's objective is in effect: changing what its rounds are measured by is for a person, in the web or their own terminal. If it is wrong or can't be reached, say so with wp request ask", i.Key)
		}
		// What the change builds on. A person's change of an agent's
		// proposal builds on it only when they name the proposal they read;
		// otherwise it starts from nothing, so no command an agent wrote
		// goes into effect unread beside the person's own.
		fresh := cur == nil
		if cur != nil && cur.State == ObjectiveProposed && person {
			switch {
			case in.Rev != "" && in.Rev != cur.Rev:
				return invalid("%s's proposal changed since you read it. It is now: %s. Read it again (rev %s)", i.Key, cur.Summary, cur.Rev)
			case in.Rev != "":
			case in.Check != nil || in.Judge != nil:
				fresh = true
			default:
				return invalid("%s has an agent's proposal (%s). Confirm it as it is (wp objective confirm %s), change it by naming the proposal you read (rev %s), or write your own check or judge",
					i.Key, cur.Summary, i.Key, cur.Rev)
			}
		}
		o := &Objective{Pace: PaceQueue, Round: 1, everyMS: -1, KeepPassing: []string{}, JudgeModels: []string{}}
		if !fresh {
			c := *cur
			o = &c
		}
		o.IssueID, o.Key, o.projectID = i.ID, i.Key, i.ProjectID
		pick := func(dst *string, src *string) {
			if src != nil {
				*dst = strings.TrimSpace(*src)
			}
		}
		measure, better := o.Measure, ""
		if in.Until == nil && in.Measure == nil && in.Better == nil {
			better = o.Better
		}
		// The target as it stands is kept, not read again from its text,
		// when this change doesn't touch it: a form sends it back with
		// every save.
		same := func(p *string, was string) bool {
			return p == nil || strings.EqualFold(strings.Join(strings.Fields(*p), " "), was)
		}
		keep := !fresh && same(in.Until, o.Until) && same(in.Measure, o.Measure) && same(in.Better, o.Better)
		kept := objectiveTarget{measure: o.Measure, until: o.Until, op: o.Op, better: o.Better, unit: o.Unit, target: o.Target, factor: o.Factor}
		wasEndless := !fresh && cur.endless()
		pick(&o.Check, in.Check)
		pick(&o.Setup, in.Setup)
		pick(&o.Until, in.Until)
		pick(&measure, in.Measure)
		pick(&better, in.Better)
		pick(&o.Pace, in.Pace)
		pick(&o.Judge, in.Judge)
		pick(&o.JudgeBar, in.JudgeBar)
		pick(&o.Repo, in.Repo)
		pick(&o.Branch, in.Branch)
		o.JudgeBar = strings.ToLower(o.JudgeBar)
		if in.Until != nil && in.Measure == nil {
			measure = "" // a new target decides its own measure
		}
		if in.Dir != nil {
			if o.Dir, err = cleanObjectiveDir(*in.Dir); err != nil {
				return err
			}
		}
		if in.KeepPassing != nil {
			if o.KeepPassing, err = cleanKeepPassing(*in.KeepPassing); err != nil {
				return err
			}
		}
		for _, c := range append([]string{o.Check, o.Setup}, o.KeepPassing...) {
			if len(c) > maxObjectiveCmd {
				return invalid("the command is too long (at most %d characters): put it in a script in the repository", maxObjectiveCmd)
			}
			if strings.ContainsRune(c, 0) {
				return invalid("the command has a NUL in it")
			}
		}
		if utf8.RuneCountInString(o.Judge) > maxObjectiveJudge {
			return invalid("what the judge reads against is too long (at most %d characters): say what done means, and keep the detail in the task", maxObjectiveJudge)
		}
		tg := kept
		if !keep {
			if tg, err = parseObjectiveTarget(o.Until, measure, better); err != nil {
				return err
			}
		}
		switch {
		case o.Check == "" && (tg.until != "" || tg.measure != ""):
			return invalid("a target needs the command that checks it: --check 'go test ./internal/core' (a judged objective and a loop have neither)")
		case o.Check == "" && len(o.KeepPassing) == 0 && (o.Setup != "" || o.Dir != ""):
			return invalid("--setup and --dir say how a check or a --keep-passing command runs: give one")
		case o.Check != "" && tg.measure == "":
			return invalid("say what the check is for: --until pass, a fixed target (\"< 1s\", \">= 80%%\") or an amount every round (2x, -10%%)")
		case o.Judge != "" && tg.factor != nil:
			return invalid("an amount every round never ends, and a judged objective ends when the judge finds nothing: give the check a fixed target or pass, or leave the judge out")
		case o.Judge == "" && o.Check == "" && len(o.KeepPassing) > 0:
			return invalid("--keep-passing names a command that must keep passing while a check or a judge decides the rounds: give one of those (a loop has no rounds to read)")
		}
		o.Measure, o.Until, o.Op, o.Target, o.Factor, o.Better, o.Unit = tg.measure, tg.until, tg.op, tg.target, tg.factor, tg.better, tg.unit
		if in.JudgeModels != nil {
			o.JudgeModels = []string{}
			for _, v := range *in.JudgeModels {
				if v = strings.TrimSpace(v); v == "" {
					continue
				}
				m, err := parseAuditModel(v)
				if err != nil {
					return err
				}
				if name := auditModelName(m); !slices.Contains(o.JudgeModels, name) {
					o.JudgeModels = append(o.JudgeModels, name)
				}
			}
			if len(o.JudgeModels) > auditMaxModels {
				return invalid("at most %d judge models", auditMaxModels)
			}
		}
		if o.Judge == "" {
			if (in.JudgeBar != nil && o.JudgeBar != "") || (in.JudgeModels != nil && len(o.JudgeModels) > 0) {
				return invalid("the bar and the models are a judge's: say what it judges against (--judge 'what done means')")
			}
			o.JudgeBar, o.JudgeModels = "", []string{}
		} else {
			if o.JudgeBar == "" {
				o.JudgeBar = "medium"
			}
			if !slices.Contains(judgeBars, o.JudgeBar) {
				return invalid("the judge's bar is the lowest severity that keeps the task open: %s", strings.Join(judgeBars, ", "))
			}
		}
		// Where it is read: a repository of the task's project.
		if !o.verified() {
			if (in.Repo != nil && o.Repo != "") || (in.Branch != nil && o.Branch != "") {
				return invalid("a loop reads nothing, so it has no repository or branch")
			}
			o.Repo, o.Branch = "", ""
		} else {
			repos, err := s.projectRepoList(ctx, i.ProjectID)
			if err != nil {
				return err
			}
			var names []string
			found := ""
			for _, r := range repos {
				names = append(names, r.Repo)
				if strings.EqualFold(r.Repo, o.Repo) {
					found = r.Repo
				}
			}
			switch {
			case len(repos) == 0:
				return invalid("%s's project has no repository for Waypoint to read the objective in: attach one in the project's settings", i.Key)
			case o.Repo != "" && found == "":
				return invalid("%s isn't a repository of %s's project (%s)", o.Repo, i.Key, strings.Join(names, ", "))
			case o.Repo == "" && len(repos) > 1:
				return invalid("%s's project has several repositories: say which one the objective is read in (--repo %s)", i.Key, strings.Join(names, " | "))
			case o.Repo == "":
				found = repos[0].Repo
			}
			o.Repo = found
			if o.Branch != "" && !validBranch(o.Branch) {
				return invalid("%q isn't a branch name", o.Branch)
			}
		}
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
		// it ended on is recorded, and its readings aren't the new round's.
		restart := !fresh && person && (cur.State == ObjectiveMet || cur.State == ObjectiveEnded)
		outOfRounds := !fresh && cur.State == ObjectiveActive && cur.Phase == PhaseBlocked && cur.PhaseWhy == BlockedRounds
		if restart {
			o.Round++
		}
		switch {
		case o.MaxRounds == 0 || o.MaxRounds > o.Round || (o.MaxRounds == o.Round && !outOfRounds):
		case restart:
			return invalid("its %s are done: give it more (--rounds %d), or 0 for no limit", plural1(o.MaxRounds, "round"), o.Round)
		case outOfRounds:
			// Still out of rounds: the change is kept, and it stays blocked.
		default:
			return invalid("round %d is under way: rounds can't be fewer", o.Round)
		}
		switch {
		case in.Every != nil:
			d, err := parseEvery(*in.Every)
			if err != nil {
				return err
			}
			o.everyMS = d.Milliseconds()
		case o.endless() && (o.everyMS < 0 || (!fresh && !wasEndless && o.everyMS == 0)):
			// New, or a fixed target turned into one that never ends by itself.
			o.everyMS = objectiveEveryDefault.Milliseconds()
		case o.everyMS < 0:
			o.everyMS = 0
		}
		now := s.nowMS()
		if cur == nil {
			if _, err := t.ExecContext(ctx, `INSERT INTO objectives (issue_id, round_started_at, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
				i.ID, now, ObjectiveProposed, now, now); err != nil {
				return err
			}
		}
		if err := s.objSet(ctx, t, i.ID, "check_cmd", o.Check, "dir", o.Dir, "setup", o.Setup, "measure", o.Measure, "until", o.Until, "op", o.Op,
			"target", nullFloat(o.Target), "factor", nullFloat(o.Factor), "better", o.Better, "unit", o.Unit, "pace", o.Pace, "every_ms", o.everyMS,
			"max_rounds", o.MaxRounds, "repo", o.Repo, "branch", o.Branch, "keep_passing", jsonList(o.KeepPassing), "judge", o.Judge, "judge_bar", o.JudgeBar,
			"judge_models", jsonList(o.JudgeModels), "set_by", nullUser(actor.id()), "set_agent", actor.creditAgent()); err != nil {
			return err
		}
		o.Kind = o.kind()
		old := ""
		if cur != nil {
			old = cur.Summary
		}
		switch {
		case !person:
			// An agent's: a proposal, nothing runs.
			if err := s.objSet(ctx, t, i.ID, "state", ObjectiveProposed, "confirmed_by", nil, "confirmed_at", nil); err != nil {
				return err
			}
			t.emit(objectiveEvent(o, "proposed", old, "proposed: "+o.summary()))
		case fresh || cur.State != ObjectiveActive:
			if err := s.objectiveStart(ctx, t, o, actor.id(), o.Round); err != nil {
				return err
			}
			t.emit(objectiveEvent(o, "active", old, o.summary()))
		default:
			if err := s.objectiveEdited(ctx, t, cur, o, actor); err != nil {
				return err
			}
			t.emit(objectiveEvent(o, "changed", old, o.summary()))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.objectiveOf(ctx, s.db.R, issueID)
}

// objectiveStart puts an objective in effect on a round, from its start:
// by is the person whose act it is, and whose runner reads it. One Waypoint
// reads has its starting point read first.
func (s *Service) objectiveStart(ctx context.Context, t *tx, o *Objective, by int64, round int) error {
	now := s.nowMS()
	phase := PhaseWork
	if o.verified() {
		phase = PhaseBaseline
	}
	return s.objSet(ctx, t, o.IssueID, append([]any{"state", ObjectiveActive, "confirmed_by", nullUser(by), "confirmed_at", now, "round_no", round, "round_started_at", now,
		"stalled", 0, "stalled_told", 0, "next_at", nil, "ended_at", nil, "ended_why", "", "phase", phase, "phase_at", now, "phase_why", "", "phase_note", "",
		"blocked_from", "", "close_status_id", nil, "base_sha", "", "base_ok", nil, "base_value", nil, "stuck_since", nil, "stuck_why", "", "stuck_told_at", nil},
		verifyReset()...)...)
}

// objectiveEdited carries a person's change of an objective in effect into
// where its rounds stand. A change of what is measured makes every earlier
// reading one of something else: the reading under way starts again, and
// the back-off that followed rounds measured the old way is dropped. A
// change of pacing alone moves nothing. A judge at work on the old one is
// cancelled by the sweep the change's event sets off.
func (s *Service) objectiveEdited(ctx context.Context, t *tx, cur, o *Objective, actor *Actor) error {
	now := s.nowMS()
	changed := cur.measuredRev() != o.measuredRev()
	outOfRounds := cur.Phase == PhaseBlocked && cur.PhaseWhy == BlockedRounds
	from := cur.Phase
	if from == PhaseBlocked {
		from = cur.blockedFrom
	}
	// Whoever changes it answers for its commands from here on: they run
	// on that person's runner.
	if changed {
		if err := s.objSet(ctx, t, o.IssueID, "confirmed_by", nullUser(actor.id()), "confirmed_at", now); err != nil {
			return err
		}
	}
	switch {
	case outOfRounds && (o.MaxRounds == 0 || o.MaxRounds > cur.Round):
		// More rounds: the next one starts.
		if changed {
			o.BaseSHA, o.baseOK, o.baseValue = "", nil, nil
		}
		return s.objectiveOpenRound(ctx, t, o, cur.Round+1, 0, 0, changed && o.verified())
	case !changed || outOfRounds:
		return nil
	}
	if err := s.objectiveDropJob(ctx, t, cur, "the objective changed"); err != nil {
		return err
	}
	sets := append([]any{"stalled", 0, "stalled_told", 0, "next_at", nil, "phase_at", now, "phase_why", "", "phase_note", "", "blocked_from", ""}, verifyReset()...)
	if cur.Repo != o.Repo || cur.Branch != o.Branch {
		sets = append(sets, "base_sha", "", "base_ok", nil, "base_value", nil)
	} else {
		sets = append(sets, "base_ok", nil, "base_value", nil)
	}
	switch {
	case from == PhaseVerdict && !o.verified():
		// A round's work waits to be read, and nothing reads a loop: the
		// round is done.
		if err := s.objSet(ctx, t, o.IssueID, sets...); err != nil {
			return err
		}
		return s.objectiveSettle(ctx, t, o, roundEnd{outcome: RoundLoop, note: "the objective became a loop before it was read"})
	case from == PhaseVerdict:
		sets = append(sets, "phase", PhaseVerdict, "verify_at", now)
	case o.verified():
		sets = append(sets, "phase", PhaseBaseline)
	default:
		sets = append(sets, "phase", PhaseWork)
	}
	return s.objSet(ctx, t, o.IssueID, sets...)
}

// ConfirmObjective puts an agent's proposed objective in effect: a person's
// own act, since its commands run on that person's runner. rev is the
// proposal the person read (Objective.Rev): an agent may have written
// another over it since, and that one isn't what they confirm.
func (s *Service) ConfirmObjective(ctx context.Context, actor *Actor, ref, rev string) (*Objective, error) {
	if err := actor.requirePersonDirect("confirming an objective"); err != nil {
		return nil, err
	}
	if !objectivePerson(actor) {
		return nil, forbidden("confirming an objective is for a person")
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
		if o.verified() {
			// Its repository may have been detached since it was proposed.
			repos, err := s.projectRepoList(ctx, i.ProjectID)
			if err != nil {
				return err
			}
			if !slices.ContainsFunc(repos, func(r stepRepo) bool { return r.Repo == o.Repo }) {
				return invalid("%s's objective is read in %s, which isn't a repository of its project any more: set it again", i.Key, o.Repo)
			}
		}
		if err := s.objectiveStart(ctx, t, o, actor.id(), o.Round); err != nil {
			return err
		}
		t.emit(objectiveEvent(o, "active", "proposed", "confirmed: "+o.Summary))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.objectiveOf(ctx, s.db.R, issueID)
}

// ClearObjective takes an issue's objective off: it is an ordinary task
// again. Its rounds and readings go with it, so what they came to is
// written into the task's history first. An agent can only withdraw its
// own proposal; ending one in effect and keeping its record is ObjectiveGo's
// "end".
func (s *Service) ClearObjective(ctx context.Context, actor *Actor, ref string) error {
	return s.write(ctx, actor, func(t *tx) error {
		i, _, err := s.getIssueFor(ctx, t, actor, ref, RoleEditor, false)
		if err != nil {
			return err
		}
		o, err := s.objectiveOf(ctx, t, i.ID)
		person := objectivePerson(actor)
		switch {
		case err != nil:
			return err
		case o == nil:
			return nil
		case o.State != ObjectiveProposed && !person:
			return forbidden("%s's objective is a person's to take off, in the web or their own terminal. Finishing a round is wp ship; if the objective is wrong or can't be reached, say so with wp request ask", i.Key)
		case o.State == ObjectiveProposed && !person && o.SetAgent != "" && o.SetAgent != actor.creditAgent():
			return forbidden("%s's proposed objective is %s's: it is theirs or a person's to withdraw", i.Key, o.SetAgent)
		}
		if err := s.objectiveDropJob(ctx, t, o, "the objective was taken off"); err != nil {
			return err
		}
		var rounds int
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM objective_rounds WHERE issue_id = ? AND outcome != ?`, i.ID, RoundReading).Scan(&rounds); err != nil {
			return err
		}
		said := "taken off"
		if o.State != ObjectiveProposed {
			said += fmt.Sprintf(" after %s", plural1(rounds, "round"))
			if o.Verdict != nil {
				said += "; Waypoint's last reading: " + o.Verdict.Line
			}
		}
		for _, q := range []string{`DELETE FROM objective_rounds WHERE issue_id = ?`, `DELETE FROM objective_merges WHERE issue_id = ?`, `DELETE FROM objective_measurements WHERE issue_id = ?`, `DELETE FROM objectives WHERE issue_id = ?`} {
			if _, err := t.ExecContext(ctx, q, i.ID); err != nil {
				return err
			}
		}
		t.emit(objectiveEvent(o, "cleared", o.Summary, said))
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

// RecordObjectiveCheck keeps a reading the working agent took with wp
// objective check, for the agent to work by and for people to see. It
// decides nothing: only a reading Waypoint takes itself ends a round or
// meets the objective.
func (s *Service) RecordObjectiveCheck(ctx context.Context, actor *Actor, ref string, in ObjectiveCheckInput) (*ObjectiveCheck, error) {
	if !finitePtr(in.Value, in.BaseValue, in.Low, in.High, in.P) {
		return nil, invalid("a reading isn't a number")
	}
	if in.Runs < 1 {
		in.Runs = 1
	}
	if in.Runs > 1000 || len(in.Tree) > 80 || len(in.Commit) > 80 || len(in.BaseSHA) > 80 || len(in.Agent) > 200 {
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
			return forbidden("%s's objective is an agent's proposal: nothing is read until a person confirms it", i.Key)
		case o.State != ObjectiveActive:
			return invalid("%s's objective is %s: there is no round to check", i.Key, o.State)
		case o.Check == "":
			return invalid("%s's objective has no check command to read", i.Key)
		case in.Rev != "" && in.Rev != o.Rev:
			return invalid("%s's objective changed while the check ran: read it again (wp objective show %s) and run the check again", i.Key, i.Key)
		}
		m := &ObjectiveMeasurement{OK: in.OK, Value: in.Value, BaseOK: in.BaseOK, BaseValue: in.BaseValue, P: in.P, Moved: in.Moved, Exact: in.Exact, Source: SourceAgent}
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
		res, err := t.ExecContext(ctx, `INSERT INTO objective_measurements (issue_id, round_no, at, ok, value, base_ok, base_value, base_sha, tree, commit_sha, runs, low, high, p, moved, met, improved,
			agent, user_id, session_id, source, rev, exact)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			i.ID, o.Round, s.nowMS(), in.OK, nullFloat(in.Value), baseOK, nullFloat(in.BaseValue), in.BaseSHA, in.Tree, in.Commit, in.Runs,
			nullFloat(in.Low), nullFloat(in.High), nullFloat(in.P), in.Moved, m.Met, m.Improved, agent, nullUser(actor.id()), session, SourceAgent, o.measuredRev(), in.Exact)
		if err != nil {
			return err
		}
		if mid, err = res.LastInsertId(); err != nil {
			return err
		}
		// A worker's own readings are working notes: the newest are kept.
		_, err = t.ExecContext(ctx, `DELETE FROM objective_measurements WHERE issue_id = ? AND source = ? AND id NOT IN
			(SELECT id FROM objective_measurements WHERE issue_id = ? AND source = ? ORDER BY id DESC LIMIT ?)`, i.ID, SourceAgent, i.ID, SourceAgent, objectiveAgentReadings)
		return err
	})
	if err != nil {
		return nil, err
	}
	o, err := s.objectiveOf(ctx, s.db.R, issueID)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, notFound("an objective on " + ref)
	}
	out := &ObjectiveCheck{Objective: o}
	if out.Check, err = s.measurementByID(ctx, s.db.R, o, mid); err != nil {
		return nil, err
	}
	if out.Check == nil {
		return nil, notFound("the reading just kept")
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
	out.Message = fmt.Sprintf("%s round %d, a working reading: %s. By this reading: %s. It decides nothing: when the round's work lands, Waypoint reads it in a fresh checkout, and that reading decides.",
		o.Key, o.Round, out.Check.Line, verdict)
	return out, nil
}

// objectiveFindings is what the judge last found at or above the bar, for
// the round under way to settle: the findings of the newest judge's reading
// of the objective as it stands.
func (s *Service) objectiveFindings(ctx context.Context, q queryer, o *Objective) ([]ObjectiveFinding, error) {
	var audit int64
	err := q.QueryRowContext(ctx, `SELECT coalesce(audit_id, 0) FROM objective_measurements WHERE issue_id = ? AND source = ? AND rev = ? ORDER BY id DESC LIMIT 1`,
		o.IssueID, SourceJudge, o.measuredRev()).Scan(&audit)
	if errors.Is(err, sql.ErrNoRows) || audit == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a, err := s.auditRow(ctx, audit)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil // the audit was deleted; its count is kept
		}
		return nil, err
	}
	if err := s.loadFindings(ctx, a); err != nil {
		return nil, err
	}
	var out []ObjectiveFinding
	for _, f := range a.Findings {
		if !judgeCounts(f, o.JudgeBar) {
			continue
		}
		out = append(out, ObjectiveFinding{ID: f.ID, Severity: f.Severity, Title: f.Title, Path: f.Path, Line: f.Line, Problem: f.Problem})
	}
	return out, nil
}

// objectiveWait is why an objective's task isn't handed out now.
type objectiveWait struct {
	round int
	phase string // one of the phases agents aren't handed it in, "" for a wait between rounds
	note  string
	until int64 // not before; 0: it waits for spare quota
}

// objectiveWaits are the open tasks whose objective keeps them from being
// handed out now: while Waypoint reads a round or it needs a person,
// between rounds, or paced to run only on quota that would go unused while
// the actor's week's-end plan doesn't want more work.
func (s *Service) objectiveWaits(ctx context.Context, actor *Actor) (map[int64]objectiveWait, error) {
	now := s.nowMS()
	rows, err := s.db.R.QueryContext(ctx, `SELECT issue_id, round_no, pace, coalesce(next_at, 0), phase, phase_note FROM objectives
		WHERE state = ? AND (phase != '' OR next_at > ? OR pace = ?)`, ObjectiveActive, now, PaceSpare)
	if err != nil {
		return nil, err
	}
	type row struct {
		id, next          int64
		round             int
		pace, phase, note string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.round, &r.pace, &r.next, &r.phase, &r.note); err != nil {
			_ = rows.Close()
			return nil, err
		}
		all = append(all, r)
	}
	if err := cmpErr(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	out := map[int64]objectiveWait{}
	spare, asked := false, false
	for _, r := range all {
		switch {
		case r.phase != PhaseWork:
			out[r.id] = objectiveWait{round: r.round, phase: r.phase, note: r.note}
			continue
		case r.next > now:
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
func (w objectiveWait) detail() string {
	switch {
	case w.phase == PhaseBaseline:
		return fmt.Sprintf("Waypoint is reading where its objective starts from; round %d is handed out once it has.", w.round)
	case w.phase == PhaseVerdict:
		return fmt.Sprintf("Round %d of its objective ended; Waypoint is reading it. The task closes if it is met, and opens for the next round if not.", w.round)
	case w.phase == PhaseBlocked:
		return "Its objective needs a person: " + w.note
	case w.until > 0:
		return fmt.Sprintf("Round %d of its objective isn't handed out before %s (the time between its rounds, or a back-off after rounds that moved nothing).", w.round, objectiveClock(msTime(w.until)))
	}
	return "Its objective runs on quota that would go unused: autopilot takes it over the week's last hours, while \"Use what's left\" is on and the week's use is short of its target."
}

func hasWait(m map[int64]objectiveWait, id int64) bool { _, ok := m[id]; return ok }

// objectiveTakeRefusal is why an objective's task can't be taken now. While
// Waypoint reads a round or the objective needs a person, nobody takes it:
// work done then would be read as part of a round it isn't. Between rounds
// and on spare pace, a person naming the task in a session they drive may
// take it early; a session nobody watches may not (WP-ADR111).
func (s *Service) objectiveTakeRefusal(ctx context.Context, t *tx, actor *Actor, i *Issue, explicit bool) error {
	o, err := s.objectiveRow(ctx, t, i.ID)
	if err != nil || o == nil || o.State != ObjectiveActive {
		return err
	}
	now := s.nowMS()
	w := objectiveWait{round: o.Round, phase: o.Phase, note: o.PhaseNote}
	switch {
	case o.Phase != PhaseWork:
	case o.NextAt != nil && o.NextAt.UnixMilli() > now:
		w.until = o.NextAt.UnixMilli()
		if explicit && !s.unattended(ctx, actor) {
			return nil
		}
	case o.Pace == PaceSpare:
		if (explicit && !s.unattended(ctx, actor)) || (actor.id() != 0 && s.spendGoesOn(ctx, actor.id())) {
			return nil
		}
	default:
		return nil
	}
	if !explicit {
		return errAlreadyClaimed
	}
	return invalid("%s can't be taken now. %s", i.Key, w.detail())
}
