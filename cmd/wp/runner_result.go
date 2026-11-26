package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
)

// The size of a job's result (WP-1320). An audit's result carries every
// helper run with its leads, the leads it cut and the second reads'
// evidence; on a big change that passed the old 256 KB, the server refused
// it with a 400 and the runner took that as fatal: the reviewer's findings
// and the whole round's work were lost. The server now takes
// core.RunnerResultMax; the runner sizes its result first and, if it is too
// large, shrinks the helper runs in steps, never the findings, and says
// each step in the job's output.
const (
	// resultMargin is kept free below the server's most, for the rest of
	// the finish body (the state, the error) and the size of a JSON
	// encoding the estimate misses.
	resultMargin = 64 << 10
	// resultEvidenceMax is what a kept lead's evidence is clipped to
	// (characters) in the third step.
	resultEvidenceMax = 300
)

// resultLimit is the most a result may be before the runner shrinks it.
var resultLimit = core.RunnerResultMax - resultMargin

// resultSize is the bytes a result takes as the finish body carries it.
func resultSize(o outcome) int {
	if o.Result == nil {
		return 0
	}
	b, err := json.Marshal(o.Result)
	if err != nil {
		return 0
	}
	return len(b)
}

// helperRunsOf is the helper runs in a result, nil when it has none.
func helperRunsOf(o outcome) (map[string]any, []helperRun) {
	m, ok := o.Result.(map[string]any)
	if !ok {
		return nil, nil
	}
	runs, _ := m["helpers"].([]helperRun)
	return m, runs
}

// withRuns is o with its helper runs replaced by runs (a copy of the
// result's map; the runs the caller made are its own).
func withRuns(o outcome, m map[string]any, runs []helperRun) outcome {
	n := make(map[string]any, len(m))
	for k, v := range m {
		n[k] = v
	}
	n["helpers"] = runs
	o.Result = n
	return o
}

// shrinkSteps are the ways a result is made smaller, in order. Each takes
// a copy of the runs, changes them and says what it did ("" when it found
// nothing to change). None touches the findings.
var shrinkSteps = []func(runs []helperRun) string{
	func(runs []helperRun) string {
		n := 0
		for i := range runs {
			for j, c := range runs[i].Cut {
				if c.Problem == "" && c.Evidence == "" && c.Path == "" && c.ReadProblem == "" && c.ReadTitle == "" {
					continue
				}
				runs[i].Cut[j] = panelLead{ID: c.ID, Title: c.Title, Why: c.Why}
				n++
			}
		}
		if n == 0 {
			return ""
		}
		return fmt.Sprintf("cut leads reduced to their id, title and why (%d)", n)
	},
	func(runs []helperRun) string {
		n := 0
		for i := range runs {
			c := len(runs[i].Cut)
			if c == 0 {
				continue
			}
			n += c
			runs[i].Dropped += c
			note := fmt.Sprintf("%d cut leads were not kept: the result was too large", c)
			runs[i].Error = strings.TrimSpace(runs[i].Error + " " + note)
			runs[i].Cut = nil
		}
		if n == 0 {
			return ""
		}
		return fmt.Sprintf("%d cut leads dropped and counted", n)
	},
	func(runs []helperRun) string {
		n := 0
		for i := range runs {
			for j, l := range runs[i].Leads {
				if len([]rune(l.Evidence)) > resultEvidenceMax {
					runs[i].Leads[j].Evidence = string([]rune(l.Evidence)[:resultEvidenceMax]) + "…"
					n++
				}
			}
		}
		if n == 0 {
			return ""
		}
		return fmt.Sprintf("the evidence of %d kept leads clipped to %d characters", n, resultEvidenceMax)
	},
}

// cloneRuns copies the runs and the slices of leads a step changes.
func cloneRuns(runs []helperRun) []helperRun {
	out := make([]helperRun, len(runs))
	for i, r := range runs {
		out[i] = r
		out[i].Leads = append([]panelLead(nil), r.Leads...)
		out[i].Cut = append([]panelLead(nil), r.Cut...)
	}
	return out
}

// fitResult makes a result fit the server's limit by shrinking the helper
// runs in steps, each said to log (the job's output). Findings are never
// touched. A result still over the limit after every step is returned as it
// is: the server decides, and finish sends the kept leads alone (reduceToKept).
func fitResult(o outcome, limit int, log io.Writer) outcome {
	m, runs := helperRunsOf(o)
	if len(runs) == 0 || resultSize(o) <= limit {
		return o
	}
	size := resultSize(o)
	runs = cloneRuns(runs)
	for i, step := range shrinkSteps {
		what := step(runs)
		if what == "" {
			continue
		}
		o = withRuns(o, m, runs)
		now := resultSize(o)
		fmt.Fprintf(log, "The result was %d KB, over the %d KB the server takes; step %d: %s; now %d KB.\n", size>>10, limit>>10, i+1, what, now>>10)
		size = now
		if size <= limit {
			return o
		}
	}
	fmt.Fprintf(log, "The result is still %d KB after every step; sending it to see whether the server takes it.\n", size>>10)
	return o
}

// reduceToKept is a result with its helper runs held to their kept leads,
// for the one retry after the server said the result was too large (its
// estimate and this one differ): every cut lead dropped and counted, every
// lead's evidence clipped to a sentence. ok is false when there is nothing
// of the kind to reduce.
func reduceToKept(o outcome) (outcome, bool) {
	m, runs := helperRunsOf(o)
	if len(runs) == 0 {
		return o, false
	}
	runs = cloneRuns(runs)
	shrinkSteps[0](runs)
	shrinkSteps[1](runs)
	for i := range runs {
		for j, l := range runs[i].Leads {
			if len([]rune(l.Evidence)) > 80 {
				runs[i].Leads[j].Evidence = string([]rune(l.Evidence)[:80]) + "…"
			}
		}
	}
	return withRuns(o, m, runs), true
}
