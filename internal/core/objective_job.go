package core

import (
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// The reading runs in fresh checkouts on the person's own runner, so the agent that worked the round has no hand in it.

const (
	// JobObjective reads an objective in fresh checkouts: the check, at the
	// commit the branch stands at and beside the commit the round started
	// from, and the commands that must keep passing.
	JobObjective = "objective"
	// FeatureObjective: the runner's wp can run a JobObjective.
	FeatureObjective = "objective"
	// objectiveSource marks a runner job as an objective's reading; its
	// source id is the task.
	objectiveSource = "objective"
	// objectiveJobTimeout is the longest a reading's job may run, seconds;
	// objectiveJobBudget what the runner plans its runs within, so a slow
	// check is run fewer times and not cut off.
	objectiveJobTimeout = runnerMaxTimeout
	objectiveJobBudget  = runnerMaxTimeout / 2
	// What of a command's output is kept, and how many files a round is
	// said to have touched.
	objectiveOutputMax  = 2000
	objectiveTouchedMax = 40
)

// ObjectiveJob is a JobObjective's input.
type ObjectiveJob struct {
	// Key and Round name what is read, for the job's log.
	Key   string `json:"key"`
	Round int    `json:"round"`
	// Repo is the clone URL; Ref the branch read ("": the default branch).
	Repo string `json:"repo"`
	Ref  string `json:"ref,omitempty"`
	// Base is the commit the round started from: the check is read there
	// too, on the same machine in the same run, when Ref's head is another
	// commit and the reading is a time or a value. "": nothing to compare
	// with (the reading before the first round).
	Base string `json:"base,omitempty"`
	// Check is the command read ("": none, so only the head commit is
	// found and the keep-passing commands run); Setup runs once in each checkout before it;
	// Dir is the folder within the repository they run in.
	Check string `json:"check,omitempty"`
	Setup string `json:"setup,omitempty"`
	Dir   string `json:"dir,omitempty"`
	// Measure is time, value, pass or "" (the command is run and nothing is
	// read from it).
	Measure string `json:"measure,omitempty"`
	// Unit is what a value's target is counted in ("%", "warnings"; "":
	// none): on a line with several numbers, the one followed by it is the
	// value.
	Unit string `json:"unit,omitempty"`
	// KeepPassing are commands that must pass at the head.
	KeepPassing []string `json:"keep_passing,omitempty"`
	// Budget is the seconds the runs may take all told.
	Budget int `json:"budget"`
}

// ObjectiveReading is what a command read in one checkout.
type ObjectiveReading struct {
	// OK: every run of it exited 0.
	OK bool `json:"ok"`
	// Value is the median of its runs: seconds for a time, the last number
	// printed for a value; nil for a pass, a failure, or no number.
	Value *float64 `json:"value,omitempty"`
	// Low and High bound the middle half of the runs.
	Low  *float64 `json:"low,omitempty"`
	High *float64 `json:"high,omitempty"`
	Runs int      `json:"runs"`
	Exit int      `json:"exit"`
	// Output is the end of what the last run printed.
	Output string `json:"output,omitempty"`
}

// ObjectiveKept is how one keep-passing command ran at the head.
type ObjectiveKept struct {
	Command string `json:"command"`
	OK      bool   `json:"ok"`
	Output  string `json:"output,omitempty"`
}

// ObjectiveJobResult is a JobObjective's answer. A check that fails is an
// answer (Reading.OK false) and the job is done; a job fails only when the
// runner couldn't read at all (no clone, no such branch, out of time).
type ObjectiveJobResult struct {
	// Head is the commit read; Base the commit read beside it ("": none).
	Head string `json:"head"`
	Base string `json:"base,omitempty"`
	// Reading is the check's at Head (nil: the job had no check);
	// BaseReading its reading at Base.
	Reading     *ObjectiveReading `json:"reading,omitempty"`
	BaseReading *ObjectiveReading `json:"base_reading,omitempty"`
	// P is how likely the difference between the two sets of runs is noise
	// (a Mann-Whitney U test); nil when there aren't two sets.
	P *float64 `json:"p,omitempty"`
	// Exact: every run in each checkout read the same value, so the two
	// are compared as they are.
	Exact bool            `json:"exact,omitempty"`
	Kept  []ObjectiveKept `json:"kept,omitempty"`
	// Touched are files changed between Base and Head that the job's own
	// commands name.
	Touched []string `json:"touched,omitempty"`
}

var fullCommitRE = lazyre.New(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func (r *ObjectiveReading) clean() bool {
	if r == nil {
		return true
	}
	if !finitePtr(r.Value, r.Low, r.High) {
		return false
	}
	if !r.OK {
		r.Value, r.Low, r.High = nil, nil, nil
	}
	r.Runs = min(max(r.Runs, 1), 1000)
	r.Output = tailText(r.Output, objectiveOutputMax)
	return true
}

// tailText is the end of s, at most n bytes, cut at a rune and with no
// control characters but the newline and tab.
func tailText(s string, n int) string {
	s = strings.ToValidUTF8(strings.TrimSpace(s), "")
	if len(s) > n {
		s = s[len(s)-n:]
		for len(s) > 0 && s[0]&0xC0 == 0x80 {
			s = s[1:]
		}
		s = "…" + s
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// clean checks a runner's answer against the job it answers and bounds what
// is kept of it; why says what is wrong with one that can't be used.
func (r *ObjectiveJobResult) clean(j *ObjectiveJob) (why string) {
	r.Head, r.Base = strings.ToLower(strings.TrimSpace(r.Head)), strings.ToLower(strings.TrimSpace(r.Base))
	switch {
	case !fullCommitRE.MatchString(r.Head):
		return "it doesn't say which commit it read"
	case r.Base != "" && !fullCommitRE.MatchString(r.Base):
		return "its base isn't a commit"
	case !r.Reading.clean() || !r.BaseReading.clean() || !finitePtr(r.P):
		return "a reading in it isn't a number"
	case j.Check != "" && r.Reading == nil:
		return "it has no reading of the check"
	case j.Check == "" && (r.Reading != nil || r.BaseReading != nil):
		return "it has a reading of a check the objective doesn't have"
	case r.BaseReading != nil && (r.Base == "" || r.Base != strings.ToLower(j.Base)):
		return "its base reading is of another commit than the round started from"
	}
	if r.P != nil && (*r.P < 0 || *r.P > 1) {
		return "its p isn't a probability"
	}
	if r.BaseReading == nil {
		r.Base, r.P = "", nil
	}
	kept := make([]ObjectiveKept, 0, len(j.KeepPassing))
	for _, cmd := range j.KeepPassing {
		found := false
		for _, h := range r.Kept {
			if h.Command == cmd {
				h.Output = tailText(h.Output, objectiveOutputMax)
				kept, found = append(kept, h), true
				break
			}
		}
		if !found {
			return "it didn't run `" + excerpt(cmd, 80) + "`, which must keep passing"
		}
	}
	r.Kept = kept
	touched := make([]string, 0, min(len(r.Touched), objectiveTouchedMax))
	for _, f := range r.Touched {
		if f = tailText(f, 300); f != "" && !strings.ContainsAny(f, "\n\t") && len(touched) < objectiveTouchedMax {
			touched = append(touched, f)
		}
	}
	r.Touched = touched
	if j.Base == "" {
		r.Touched = nil
	}
	return ""
}

// broken are the keep-passing commands of an answer that didn't pass.
func (r *ObjectiveJobResult) broken() []string {
	var out []string
	for _, h := range r.Kept {
		if !h.OK {
			out = append(out, h.Command)
		}
	}
	return out
}

// Clean is the check Waypoint makes of a runner's answer to job before it
// counts ("" when it stands, else what is wrong with it), for the runner's
// own tests: an executor whose answers this refuses reads nothing.
func (r *ObjectiveJobResult) Clean(job *ObjectiveJob) string { return r.clean(job) }
