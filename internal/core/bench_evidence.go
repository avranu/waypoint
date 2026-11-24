package core

import (
	"fmt"
	"math"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const maxEvidenceBench = 5

// BenchRun is one wp bench result, as the CLI kept it. The request decoder
// refuses a field it doesn't know, so this has every field of the CLI's
// benchRecord. Times are medians, in seconds; the p-values are the
// Mann-Whitney test of this tree against the base.
type BenchRun struct {
	At       int64    `json:"at"` // ms
	Tree     string   `json:"tree"`
	Moved    bool     `json:"moved,omitempty"` // the tree changed during the runs
	Command  string   `json:"command"`
	Dir      string   `json:"dir,omitempty"`   // the folder within the repository
	Setup    string   `json:"setup,omitempty"` // run once before timing
	Runs     int      `json:"runs"`
	Wall     float64  `json:"wall"`
	CPU      float64  `json:"cpu"`
	Load     float64  `json:"load,omitempty"`
	BaseRef  string   `json:"base_ref,omitempty"`
	BaseTree string   `json:"base_tree,omitempty"`
	BaseWall float64  `json:"base_wall,omitempty"`
	BaseCPU  float64  `json:"base_cpu,omitempty"`
	WallP    *float64 `json:"wall_p,omitempty"`
	CPUP     *float64 `json:"cpu_p,omitempty"`
}

func finiteNonNeg(vs ...float64) bool {
	for _, v := range vs {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

func (b *BenchRun) clean() error {
	if b.Runs < 1 || !finiteNonNeg(b.Wall, b.CPU, b.Load, b.BaseWall, b.BaseCPU) {
		return invalid("a bench result needs runs and non-negative times")
	}
	for _, p := range []*float64{b.WallP, b.CPUP} {
		if p != nil && (*p < 0 || *p > 1 || math.IsNaN(*p)) {
			return invalid("a bench p-value is between 0 and 1")
		}
	}
	if len(b.Tree) > maxEvidenceHash || len(b.BaseTree) > maxEvidenceHash {
		return invalid("a bench result's tree is too long")
	}
	b.Command = flatRunes(b.Command, maxEvidenceCommand)
	b.BaseRef = flatRunes(b.BaseRef, 100)
	b.Dir = flatRunes(b.Dir, 300)
	b.Setup = flatRunes(b.Setup, maxEvidenceCommand)
	return nil
}

// benchSignificant: below it a difference is reported as one.
const benchSignificant = 0.05

// benchMinRuns is the fewest runs a side at which a Mann-Whitney p can fall
// under benchSignificant (fully separated samples: 3 a side give 0.08, 4
// give 0.03). Fewer can't tell a change from noise, however large it is, so
// such a result speaks neither for nor against a "faster".
const benchMinRuns = 4

// change is this tree against the base by wall time, else CPU time, when
// either is clear: the percent (negative is faster) and its p. ok is false
// when there is no base or neither measure moved beyond noise.
func (b BenchRun) change() (pct, p float64, measure string, ok bool) {
	if b.BaseRef == "" {
		return 0, 0, "", false
	}
	for _, m := range []struct {
		name     string
		now, was float64
		p        *float64
	}{{"wall", b.Wall, b.BaseWall, b.WallP}, {"cpu", b.CPU, b.BaseCPU, b.CPUP}} {
		if m.p != nil && *m.p < benchSignificant && m.was > 0 {
			return (m.now - m.was) / m.was * 100, *m.p, m.name, true
		}
	}
	return 0, 0, "", false
}

// faster: the base comparison found this tree clearly faster.
func (b BenchRun) faster() bool {
	pct, _, _, ok := b.change()
	return ok && pct < 0
}

func benchSecs(s float64) string {
	switch {
	case s < 0.001:
		return fmt.Sprintf("%.0fµs", s*1e6)
	case s < 1:
		return fmt.Sprintf("%.0fms", s*1e3)
	case s < 100:
		return fmt.Sprintf("%.3gs", s)
	}
	return fmt.Sprintf("%.0fs", s)
}

// benchLine says one result in a line: what was timed, how long it took,
// and what the base comparison found.
func benchLine(b BenchRun) string {
	cmd := flatRunes(b.Command, maxEvidenceShown)
	if b.Dir != "" {
		cmd += " (in " + b.Dir + ")"
	}
	s := fmt.Sprintf("Timed on this tree: %s, wall %s, cpu %s (median of %d)", cmd, benchSecs(b.Wall), benchSecs(b.CPU), b.Runs)
	if b.BaseRef == "" {
		return s
	}
	if pct, p, m, ok := b.change(); ok {
		dir := "slower"
		if pct < 0 {
			dir = "faster"
		}
		return s + fmt.Sprintf("; %.0f%% %s than %s by %s (p=%.3f)", math.Abs(pct), dir, b.BaseRef, m, p)
	}
	pct := 0.0
	if b.BaseWall > 0 {
		pct = (b.Wall - b.BaseWall) / b.BaseWall * 100
	}
	if b.Runs < benchMinRuns {
		return s + fmt.Sprintf("; wall %+.0f%% against %s, too few runs to tell a change from noise (needs %d a side)", pct, b.BaseRef, benchMinRuns)
	}
	return s + fmt.Sprintf("; no clear change from %s (wall %+.0f%%, within noise)", b.BaseRef, pct)
}

func benchLines(runs []BenchRun) []string {
	out := make([]string, 0, len(runs))
	for _, b := range runs {
		out = append(out, benchLine(b))
	}
	return out
}

// stmtFaster is a statement that the change made something faster.
var stmtFaster = lazyre.New(`(?i)\b(?:faster|quicker|sped up|speeds? up|speedup)\b`)

// benchFact is what the timings on a record say against a "faster", or ""
// when they can't speak to it: no result with a base and enough runs to
// show a change, or one that was faster.
func benchFact(sha string, runs []BenchRun) string {
	var compared []BenchRun
	for _, b := range runs {
		if b.BaseRef == "" || b.Runs < benchMinRuns || b.Moved {
			continue
		}
		if b.faster() {
			return ""
		}
		compared = append(compared, b)
	}
	if len(compared) == 0 {
		return ""
	}
	return "wp bench at " + short(sha) + ": " + strings.TrimPrefix(benchLine(compared[0]), "Timed on this tree: ")
}
