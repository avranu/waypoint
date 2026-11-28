package core

import (
	"strings"
	"testing"
	"time"
)

func (e *escapeEnv) prove(t *testing.T, in FixProofInput) {
	t.Helper()
	if in.Machine == "" {
		in.Machine = "m"
	}
	if err := e.s.RecordFixProof(e.ctx, e.admin, in); err != nil {
		t.Fatal(err)
	}
}

func (e *escapeEnv) introConfidence(t *testing.T, pr int64, merge string) float64 {
	t.Helper()
	var c float64
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT confidence FROM fix_trace_introducers WHERE pr_id = ? AND merge_sha = ?`, pr, merge).Scan(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestProofsDueOnlyTracedFixesOnce(t *testing.T) {
	e := newEscapeEnv(t)
	now := e.s.now()
	e.mergedPR(t, 1, now.Add(-48*time.Hour), "m1")
	fix := e.mergedPR(t, 2, now, "m2")
	e.mergedPR(t, 3, now, "m3")
	e.bug(t, "broken", 2)
	e.bug(t, "also broken", 3)

	due, err := e.s.EscapeProofsDue(e.ctx, e.admin, "", "o/r", 0)
	if err != nil || len(due) != 0 {
		t.Fatalf("before any trace: %+v %v", due, err)
	}
	e.record(t, fix, FixIntroducerInput{Merge: "m1", PR: 1, Lines: 2, Share: 0.6, At: now.Add(-48 * time.Hour).UnixMilli()}, "szz")
	due, err = e.s.EscapeProofsDue(e.ctx, e.admin, "", "o/r", 0)
	if err != nil || len(due) != 1 || due[0].PR != fix || due[0].Fix != "placed" || due[0].Intro != "m1" || due[0].Merge != "m2" {
		t.Fatalf("due = %+v %v", due, err)
	}
	e.prove(t, FixProofInput{PR: fix, Commit: "placed", Outcome: "passes", Tests: []string{"calc.TestAdd"}})
	if due, err = e.s.EscapeProofsDue(e.ctx, e.admin, "", "o/r", 0); err != nil || len(due) != 0 {
		t.Fatalf("after the proof: %+v %v", due, err)
	}
	in := FixTraceInput{PR: fix, Machine: "m", Commit: "elsewhere", Method: "none"}
	if err := e.s.RecordFixTrace(e.ctx, e.admin, in); err != nil {
		t.Fatal(err)
	}
	if due, err = e.s.EscapeProofsDue(e.ctx, e.admin, "", "o/r", 0); err != nil || len(due) != 1 || due[0].Fix != "elsewhere" {
		t.Fatalf("after a new trace: %+v %v", due, err)
	}
	err = e.s.RecordFixProof(e.ctx, e.admin, FixProofInput{PR: fix, Machine: "m", Commit: "placed", Outcome: "passes"})
	if err == nil || !strings.Contains(err.Error(), "traced to") {
		t.Fatalf("a proof of the old commit: %v", err)
	}
}

func TestProofConfirmsAndRefutesTheBlame(t *testing.T) {
	e := newEscapeEnv(t)
	now := e.s.now()
	day := 24 * time.Hour
	e.mergedPR(t, 1, now.Add(-30*day), "m1")
	e.mergedPR(t, 2, now.Add(-20*day), "m2")
	fix := e.mergedPR(t, 3, now, "m3")
	task := e.bug(t, "broken", 3)
	in := FixTraceInput{PR: fix, Machine: "m", Commit: "placed", Method: "szz", Introducers: []FixIntroducerInput{
		{Merge: "m1", PR: 1, Lines: 3, Share: 0.6, At: now.Add(-30 * day).UnixMilli()},
		{Merge: "m2", PR: 2, Lines: 2, Share: 0.4, At: now.Add(-20 * day).UnixMilli()},
	}}
	if err := e.s.RecordFixTrace(e.ctx, e.admin, in); err != nil {
		t.Fatal(err)
	}
	rows, err := e.s.EscapeList(e.ctx, e.admin, EscapeQuery{Key: task.Key})
	if err != nil || len(rows) != 1 || rows[0].IntroNumber != 1 {
		t.Fatalf("before: %+v %v", rows, err)
	}

	e.prove(t, FixProofInput{PR: fix, Commit: "placed", Outcome: "proven", Tests: []string{"x.TestA"}, Failed: []string{"x.TestA"}, Intro: "m1", IntroCheck: "passes_at_intro"})
	rows, err = e.s.EscapeList(e.ctx, e.admin, EscapeQuery{Key: task.Key})
	if err != nil || len(rows) != 1 || rows[0].IntroNumber != 2 || rows[0].Confidence != 0.4 || rows[0].Proof != "proven" {
		t.Fatalf("refuted: %+v %v", rows[0], err)
	}

	e.prove(t, FixProofInput{PR: fix, Commit: "placed", Outcome: "proven", Tests: []string{"x.TestA"}, Failed: []string{"x.TestA"}, Intro: "m2", IntroCheck: "confirmed"})
	if c := e.introConfidence(t, fix, "m2"); c != 1 {
		t.Fatalf("confirmed confidence = %v", c)
	}
	if err := e.s.RecordFixTrace(e.ctx, e.admin, in); err != nil {
		t.Fatal(err)
	}
	rows, err = e.s.EscapeList(e.ctx, e.admin, EscapeQuery{Key: task.Key})
	if err != nil || len(rows) != 1 || rows[0].IntroNumber != 2 || rows[0].Confidence != 1 || rows[0].IntroCheck != "confirmed" {
		t.Fatalf("after a re-trace: %+v %v", rows[0], err)
	}
	line, err := fixProofLine(e.ctx, e.s.db.R, fix)
	if err != nil || !strings.HasPrefix(line, "fix proven: x.TestA fails without the fix; the blame on m2 is confirmed") {
		t.Fatalf("pr line = %q %v", line, err)
	}
}

func TestProofStatsCompareRefixes(t *testing.T) {
	e := newEscapeEnv(t)
	now := e.s.now()
	day := 24 * time.Hour
	a := e.mergedPR(t, 1, now.Add(-20*day), "m1")
	b := e.mergedPR(t, 2, now.Add(-10*day), "m2")
	c := e.mergedPR(t, 3, now.Add(-day), "m3")
	e.bug(t, "first", 1)
	e.bug(t, "second", 2)
	e.bug(t, "third", 3)
	e.record(t, a, FixIntroducerInput{}, "none")
	e.record(t, b, FixIntroducerInput{}, "none")
	e.record(t, c, FixIntroducerInput{Merge: "m2", PR: 2, Lines: 1, Share: 1, At: now.Add(-10 * day).UnixMilli()}, "szz")
	e.prove(t, FixProofInput{PR: a, Commit: "placed", Outcome: "proven", Tests: []string{"x.T"}, Failed: []string{"x.T"}})
	e.prove(t, FixProofInput{PR: b, Commit: "placed", Outcome: "no_test"})

	st, err := e.s.EscapeSummary(e.ctx, e.admin, EscapeQuery{Project: e.proj.Key})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][2]int{}
	for _, p := range st.Proofs {
		got[p.Outcome] = [2]int{p.Fixes, p.Refixed}
	}
	want := map[string][2]int{"proven": {1, 0}, "no_test": {1, 1}, "": {1, 0}}
	if len(got) != len(want) || got["proven"] != want["proven"] || got["no_test"] != want["no_test"] || got[""] != want[""] || st.ProofsPending != 1 {
		t.Fatalf("proofs = %+v pending %d", st.Proofs, st.ProofsPending)
	}
}

func TestFixProofInputValidates(t *testing.T) {
	e := newEscapeEnv(t)
	fix := e.mergedPR(t, 1, e.s.now(), "m1")
	e.bug(t, "broken", 1)
	e.record(t, fix, FixIntroducerInput{}, "none")
	bad := []FixProofInput{
		{PR: fix, Outcome: "maybe"},
		{PR: fix, Outcome: "proven", Tests: []string{"x.T"}},
		{PR: fix, Outcome: "passes", Tests: []string{"x.T"}, Failed: []string{"x.U"}},
		{PR: fix, Outcome: "passes", IntroCheck: "confirmed"},
		{PR: fix, Outcome: "passes", Intro: "m0", IntroCheck: "probably"},
	}
	for _, in := range bad {
		if err := e.s.RecordFixProof(e.ctx, e.admin, in); err == nil {
			t.Errorf("accepted %+v", in)
		}
	}
	e2 := newEscapeEnv(t)
	e2.mergedPR(t, 1, e2.s.now().Add(-time.Hour), "m1")
	traced := e2.mergedPR(t, 2, e2.s.now(), "m2")
	e2.bug(t, "broken", 2)
	e2.record(t, traced, FixIntroducerInput{Merge: "m1", PR: 1, Lines: 1, Share: 1, At: e2.s.now().Add(-time.Hour).UnixMilli()}, "szz")
	err := e2.s.RecordFixProof(e2.ctx, e2.admin, FixProofInput{PR: traced, Machine: "m", Commit: "placed", Outcome: "proven", Tests: []string{"x.T"}, Failed: []string{"x.T"}, Intro: "other", IntroCheck: "confirmed"})
	if err == nil || !strings.Contains(err.Error(), "doesn't blame") {
		t.Errorf("a check of an unblamed change: %v", err)
	}
	e2.prove(t, FixProofInput{PR: traced, Commit: "placed", Outcome: "proven", Tests: []string{"x.T"}, Failed: []string{"x.T"}, Intro: "m1", IntroCheck: "confirmed"})
	if err := e.s.RecordFixProof(e.ctx, e.admin, FixProofInput{PR: fix, Outcome: "no_test"}); err == nil {
		t.Error("accepted a proof of an untraced fix")
	}
}

func TestClassifyNoBuildNoteAndSummary(t *testing.T) {
	cases := map[string]string{
		"./merge_line_service_test.go:946:32: undefined: stackRunCeiling":                                              "unexported_symbol",
		"./merge_line_front_test.go:274:25: se.s.lineStanding undefined (type *Service has no field or method)":        "unexported_symbol",
		"./review_pipeline_test.go:279:43: too many arguments in call to failedMidStream have (int64, ...) want (...)": "signature_change",
		"./core_test.go:1436:22: b.ManualOrder undefined (type *Board has no field or method ManualOrder)":             "exported_api",
		"./api_test.go:42:10: undefined: core.NewExportedType":                                                         "exported_api",
		"syntax error: unexpected EOF": "other",
		"":                             "other",
	}
	for note, want := range cases {
		if got := ClassifyNoBuildNote(note); got != want {
			t.Errorf("ClassifyNoBuildNote(%q) = %q, want %q", note, got, want)
		}
	}
	if line := ProofText("no_build", nil, "", "", "./x_test.go:1: undefined: helperFn"); !strings.Contains(line, "tests new unexported symbol") {
		t.Errorf("ProofText(no_build) = %q", line)
	}

	e := newEscapeEnv(t)
	now := e.s.now()
	a := e.mergedPR(t, 1, now.Add(-48*time.Hour), "m1")
	b := e.mergedPR(t, 2, now.Add(-24*time.Hour), "m2")
	e.bug(t, "unexported test", 1)
	e.bug(t, "exported api test", 2)
	e.record(t, a, FixIntroducerInput{}, "none")
	e.record(t, b, FixIntroducerInput{}, "none")
	e.prove(t, FixProofInput{PR: a, Commit: "placed", Outcome: "no_build", Note: "./a_test.go:10: undefined: myHelper"})
	e.prove(t, FixProofInput{PR: b, Commit: "placed", Outcome: "no_build", Note: "./b_test.go:10: x.NewField undefined"})
	st, err := e.s.EscapeSummary(e.ctx, e.admin, EscapeQuery{Project: e.proj.Key})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.NoBuildCauses) != 2 {
		t.Fatalf("NoBuildCauses = %+v, want 2 causes", st.NoBuildCauses)
	}
}
