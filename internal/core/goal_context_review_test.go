package core

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// A title with no word break in its first goalCtxClip runes is shown whole,
// not cut mid-word; a tab or newline breaks a word like a space.
func TestGoalClipBreaks(t *testing.T) {
	solid := strings.Repeat("x", 200)
	if s, ok := goalClip(solid); ok || s != solid {
		t.Errorf("no break: clipped %v to %d runes", ok, utf8.RuneCountInString(s))
	}
	for _, sep := range []string{"\t", "\n"} {
		long := strings.Repeat("a", 100) + sep + strings.Repeat("b", 100)
		s, ok := goalClip(long)
		if !ok || s != strings.Repeat("a", 100)+"…" {
			t.Errorf("break %q: %q %v", sep, s, ok)
		}
	}
}

// Before a task's own draw an agent sees the goal alone, whatever the
// estimate favours; a person sees the favoured arm.
func TestGoalContextReadAgent(t *testing.T) {
	e := newGoalCtxEnv(t)
	now := e.s.now()
	var rows []*goalRow
	for k := range 80 {
		arm, drift := goalArmGoal, 1.0
		if k%2 == 1 {
			arm, drift = goalArmChain, 0
		}
		at := now.Add(-48 * time.Hour).UnixMilli()
		rows = append(rows, &goalRow{issue: int64(k + 1), arm: arm, policy: "thompson", chainLen: 2, textLen: 40 + k%9, assigned: at + int64(k)*1000,
			project: 1, typ: "task", resolved: at + 3600_000, done: true, prs: 1, checked: 1, drift: drift})
	}
	e.s.goalPol.pol = goalPolicyFrom(rows, now)
	i := e.serving("Restyle the help button", e.leaf)
	if g := e.read(e.ag, i.Key); len(g.Chain) != 0 || g.Arm != "" {
		t.Errorf("an agent's read: %+v", g)
	}
	if g := e.read(e.admin, i.Key); g.Arm != goalArmChain {
		t.Errorf("a person's read: %+v", g)
	}
}

// A goal with only an overlap suggestion isn't "unclear" on the dashboard;
// that counts goals worded as implementation.
func TestGoalOverlapNotUnclear(t *testing.T) {
	e := newGoalCtxEnv(t)
	a := e.goal(e.admin, e.p.Key, "Reports export cleanly to spreadsheets")
	b := e.goal(e.admin, e.p.Key, "Finance staff download figures as workbooks")
	e.useJudge(grader(func(j, x, y string) int {
		if j == judgeOverlap && (x == a.Title || y == a.Title) && (x == b.Title || y == b.Title) {
			return 3
		}
		return 0
	}))
	e.meaning()
	o := e.goals(e.admin, e.p.Key)
	if w := e.wording(o, b.ID); w == "" {
		t.Fatalf("no overlap suggestion on #%d", b.ID)
	}
	d := e.dashboard(e.admin, e.p.Key)
	for _, g := range d.Goals.Goals {
		if g.Unclear {
			t.Errorf("goal #%d unclear for an overlap alone", g.ID)
		}
	}
}
