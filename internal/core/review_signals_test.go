package core

import (
	"fmt"
	"slices"
	"testing"
)

// The label-only signals of the review-again decision (WP-1320): which
// findings call for another round on their own, and how a check's finding
// about one of them counts.

func TestAgainFloorAndLabel(t *testing.T) {
	for _, tc := range []struct {
		tier string
		want float64
	}{
		{StakesHigh, 5}, {StakesElevated, 5}, {StakesUnknown, 5}, {StakesNormal, 8},
		{"", 5}, {"made-up", 5}, // a tier not known is held as unknown
	} {
		if got := againFloor(tc.tier); got != tc.want {
			t.Errorf("againFloor(%q) = %v, want %v", tc.tier, got, tc.want)
		}
	}
	for _, tc := range []struct {
		sev  string
		want float64
	}{
		{"critical", 10}, {"high", 8}, {"medium", 5}, {"low", 3},
		{"", 5}, {"major", 5}, {"HIGH", 5}, // not a label it gave: medium
	} {
		if got := findingLabel(tc.sev); got != tc.want {
			t.Errorf("findingLabel(%q) = %v, want %v", tc.sev, got, tc.want)
		}
	}
}

func TestAgainFindingSignal(t *testing.T) {
	base := func(mod func(f *againFinding)) *againFinding {
		f := &againFinding{severity: "medium", hold: 1}
		if mod != nil {
			mod(f)
		}
		return f
	}
	for _, tc := range []struct {
		name  string
		f     *againFinding
		floor float64
		want  bool
	}{
		{"fixed medium at a medium floor", base(nil), 5, true},
		{"high at a normal floor", base(func(f *againFinding) { f.severity = "high" }), 8, true},
		{"medium under a normal floor", base(nil), 8, false},
		{"low under a medium floor", base(func(f *againFinding) { f.severity = "low" }), 5, false},
		{"unknown label is medium", base(func(f *againFinding) { f.severity = "major" }), 5, true},
		{"unknown label is medium, under a high floor", base(func(f *againFinding) { f.severity = "major" }), 8, false},
		{"open (hold half)", base(func(f *againFinding) { f.hold = 0.5 }), 5, false},
		{"rejected (hold 0)", base(func(f *againFinding) { f.hold = 0 }), 5, false},
		{"repeat of an earlier finding", base(func(f *againFinding) { f.repeatOf = 7 }), 5, false},
		{"refuted by the check against the code", base(func(f *againFinding) { f.verdict = VerdictRefuted }), 5, false},
		{"unsupported still counts", base(func(f *againFinding) { f.verdict = VerdictUnsupported }), 5, true},
		{"confirmed counts", base(func(f *againFinding) { f.verdict = VerdictConfirmed }), 5, true},
	} {
		if got := tc.f.signal(tc.floor); got != tc.want {
			t.Errorf("%s: signal(%v) = %v, want %v", tc.name, tc.floor, got, tc.want)
		}
	}
}

func TestCountSignals(t *testing.T) {
	const round, check = 10, 20
	// f builds a finding of audit a, ordinal n, standing (hold 1) at medium.
	f := func(a int64, n int, mod func(*againFinding)) againFinding {
		x := againFinding{audit: a, ordinal: n, title: fmt.Sprintf("finding %d.%d", a, n), severity: "medium", hold: 1}
		if mod != nil {
			mod(&x)
		}
		return x
	}
	sev := func(s string) func(*againFinding) { return func(x *againFinding) { x.severity = s } }
	hold := func(h float64) func(*againFinding) { return func(x *againFinding) { x.hold = h } }
	refuted := func(x *againFinding) { x.verdict = VerdictRefuted }
	repeat := func(x *againFinding) { x.repeatOf = 3 }
	refs := func(ref string, mod ...func(*againFinding)) func(*againFinding) {
		return func(x *againFinding) {
			x.title = ref + " the same problem, said again"
			for _, m := range mod {
				m(x)
			}
		}
	}
	for _, tc := range []struct {
		name            string
		floor           float64
		findings        []againFinding
		signals, repeat int
	}{
		{"none", 5, nil, 0, 0},
		{"one standing medium", 5, []againFinding{f(round, 1, nil)}, 1, 0},
		{"two standing", 5, []againFinding{f(round, 1, nil), f(round, 2, sev("high"))}, 2, 0},
		{"below the floor", 8, []againFinding{f(round, 1, nil), f(round, 2, sev("low"))}, 0, 0},
		{"high meets a normal floor", 8, []againFinding{f(round, 1, sev("high")), f(round, 2, sev("critical"))}, 2, 0},
		{"open and rejected do not count", 5, []againFinding{f(round, 1, hold(0.5)), f(round, 2, hold(0))}, 0, 0},
		{"refuted does not count, nor repeat", 5, []againFinding{f(round, 1, refuted), f(round, 2, func(x *againFinding) { refuted(x); repeat(x) })}, 0, 0},
		{"a repeat is a repeat, not a signal", 5, []againFinding{f(round, 1, repeat)}, 0, 1},
		{"a repeat under the floor is neither", 8, []againFinding{f(round, 1, repeat)}, 0, 0},
		{"a repeat nobody held is neither", 5, []againFinding{f(round, 1, func(x *againFinding) { repeat(x); x.hold = 0.5 })}, 0, 0},
		{"a refuted repeat is neither", 5, []againFinding{f(round, 1, func(x *againFinding) { repeat(x); refuted(x) })}, 0, 0},
		{"a signal and a repeat", 5, []againFinding{f(round, 1, nil), f(round, 2, repeat)}, 1, 1},
		{"unsupported counts", 5, []againFinding{f(round, 1, func(x *againFinding) { x.verdict = VerdictUnsupported })}, 1, 0},
		// A check's finding that names a round finding which is a signal
		// is the same problem: counted once.
		{"check finding naming a standing finding counts once", 5,
			[]againFinding{f(round, 1, nil), f(check, 1, refs("[10.1]"))}, 1, 0},
		{"check first in the list, same", 5,
			[]againFinding{f(check, 1, refs("[10.1]")), f(round, 1, nil)}, 1, 0},
		{"check naming a finding that isn't a signal counts on its own", 5,
			[]againFinding{f(round, 1, refuted), f(check, 1, refs("[10.1]"))}, 1, 0},
		{"check naming an open finding counts on its own", 5,
			[]againFinding{f(round, 1, hold(0.5)), f(check, 1, refs("[10.1]"))}, 1, 0},
		{"check naming another finding adds one", 5,
			[]againFinding{f(round, 1, nil), f(check, 1, refs("[10.2]"))}, 2, 0},
		{"check with no reference adds one", 5,
			[]againFinding{f(round, 1, nil), f(check, 1, nil)}, 2, 0},
		{"two checks naming the same finding count once", 5,
			[]againFinding{f(check, 1, refs("[10.1]")), f(check, 2, refs("[10.1]"))}, 1, 0},
		{"two checks, each its own finding", 5,
			[]againFinding{f(check, 1, refs("[10.1]")), f(check, 2, refs("[10.2]"))}, 2, 0},
		{"reference with a letter is not one", 5,
			[]againFinding{f(round, 1, nil), f(check, 1, refs("[a.1]"))}, 2, 0},
		{"reference not at the start is not one", 5,
			[]againFinding{f(round, 1, nil), f(check, 1, func(x *againFinding) { x.title = "see [10.1]" })}, 2, 0},
		{"check finding below the floor is nothing", 8,
			[]againFinding{f(check, 1, refs("[10.1]", sev("medium")))}, 0, 0},
		{"a refuted check finding naming a standing one leaves it counted", 5,
			[]againFinding{f(round, 1, nil), f(check, 1, refs("[10.1]", refuted))}, 1, 0},
		{"the round's own title that looks like a reference keeps its own key", 5,
			[]againFinding{f(round, 1, nil), f(round, 2, refs("[10.1]"))}, 2, 0},
	} {
		d := &againDecision{audit: round, signals: 99, signalRepeats: 99, floor: -1}
		d.countSignals(tc.findings, tc.floor)
		if d.signals != tc.signals || d.signalRepeats != tc.repeat || d.floor != tc.floor {
			t.Errorf("%s: signals %d, repeats %d, floor %v; want %d, %d, %v", tc.name, d.signals, d.signalRepeats, d.floor, tc.signals, tc.repeat, tc.floor)
		}
	}
}

// A round whose check of its marks was passed over counts the rejections
// that would have been signals: new, not refuted, at or over the floor,
// the round's own.
func TestCountUnchecked(t *testing.T) {
	const round, check = 10, 20
	f := func(a int64, state, sev string, mod func(*againFinding)) againFinding {
		x := againFinding{audit: a, state: state, severity: sev, hold: findingHold(state)}
		if mod != nil {
			mod(&x)
		}
		return x
	}
	refuted := func(x *againFinding) { x.verdict = VerdictRefuted }
	repeat := func(x *againFinding) { x.repeatOf = 3 }
	for _, tc := range []struct {
		name     string
		floor    float64
		findings []againFinding
		want     int
	}{
		{"none", 5, nil, 0},
		{"a rejected medium", 5, []againFinding{f(round, "rejected", "medium", nil)}, 1},
		{"two rejected, one under the floor", 8, []againFinding{f(round, "rejected", "high", nil), f(round, "rejected", "medium", nil)}, 1},
		{"a rejected low", 5, []againFinding{f(round, "rejected", "low", nil)}, 0},
		{"refuted by the check against the code", 5, []againFinding{f(round, "rejected", "critical", refuted)}, 0},
		{"unsupported still counts", 5, []againFinding{f(round, "rejected", "high", func(x *againFinding) { x.verdict = VerdictUnsupported })}, 1},
		{"a repeat of an earlier round's", 5, []againFinding{f(round, "rejected", "high", repeat)}, 0},
		{"fixed and open are not rejections", 5, []againFinding{f(round, "fixed", "high", nil), f(round, "open", "high", nil)}, 0},
		{"another audit's rejection", 5, []againFinding{f(check, "rejected", "high", nil)}, 0},
		{"an unlabelled rejection is medium", 5, []againFinding{f(round, "rejected", "", nil)}, 1},
	} {
		d := &againDecision{audit: round, unchecked: 99}
		d.countUnchecked(tc.findings, tc.floor)
		if d.unchecked != tc.want {
			t.Errorf("%s: unchecked %d, want %d", tc.name, d.unchecked, tc.want)
		}
	}
}

func TestLabelOnly(t *testing.T) {
	findings := []againFinding{
		{severity: "high", state: "fixed", modelScore: 9, repeatOf: 4, probs: map[string]float64{"x": 1}},
		{severity: "critical", state: "rejected"}, // held 0: out of the worst and novel
		{severity: "medium", state: "open"},       // half held
		{severity: "low", state: "confirmed"},     // held, but low
		{severity: "weird", state: "fixed"},       // medium
	}
	d := &againDecision{worst: 99, novel: 99, reworked: 99, repeats: 99, fixesMean: 99, fixes: 99, probs: "x"}
	d.labelOnly(findings)
	// worst: high (8*1); the rejected critical stands for 0.
	if d.worst != 8 || d.worstAll != 8 || d.issues != 8 {
		t.Errorf("worst %v, worstAll %v, issues %d; want 8", d.worst, d.worstAll, d.issues)
	}
	if d.novel != 4 || d.reworked != 4 {
		t.Errorf("novel = %d, reworked = %d, want 4 (all but the rejected)", d.novel, d.reworked)
	}
	if d.repeats != 0 || d.fixesMean != 0 || d.fixes != 0 || d.probs != "{}" {
		t.Errorf("repeats %d, fixesMean %v, fixes %d, probs %q: want all reset", d.repeats, d.fixesMean, d.fixes, d.probs)
	}
	for i, f := range findings {
		if f.ref != fmt.Sprintf("f%d", i+1) {
			t.Errorf("finding %d ref = %q", i, f.ref)
		}
		if f.repeatOf != 0 || f.modelScore != 0 || f.probs != nil {
			t.Errorf("finding %d keeps a model's read: %+v", i, f)
		}
	}
	if findings[1].standing() != 0 || findings[2].standing() != 2.5 || findings[4].sev != 5 {
		t.Errorf("standing: rejected %v, open %v, unknown-label sev %v", findings[1].standing(), findings[2].standing(), findings[4].sev)
	}

	// Only a rejected finding: nothing stands, nothing novel.
	d = &againDecision{}
	d.labelOnly([]againFinding{{severity: "critical", state: "rejected"}})
	if d.worst != 0 || d.novel != 0 {
		t.Errorf("all rejected: worst %v, novel %d", d.worst, d.novel)
	}
	// No findings at all.
	d = &againDecision{worst: 3}
	d.labelOnly(nil)
	if d.worst != 0 || d.issues != 0 {
		t.Errorf("no findings: worst %v, issues %d", d.worst, d.issues)
	}
}

func TestAgainPick(t *testing.T) {
	bar := againBar{worst: 6, fixes: 6, risky: 7}
	for _, tc := range []struct {
		name string
		d    againDecision
		want string
	}{
		{"worst at the bar calls for another", againDecision{worst: 6, index: 1}, ""},
		{"worst under, nothing else: enough", againDecision{worst: 5.9, index: 1}, AgainEnough},
		{"fixes at the bar in round 1", againDecision{fixesMean: 6, index: 1}, ""},
		{"fixes at the bar later, with nothing new: not by fixes", againDecision{fixesMean: 6, index: 2, reworked: 0}, AgainEnough},
		{"fixes at the bar later, something new reworked", againDecision{fixesMean: 6, index: 2, reworked: 1}, ""},
		{"would-be signals repeat earlier rounds'", againDecision{worst: 2, signalRepeats: 1, index: 2}, AgainRepeat},
		{"repeat signals don't beat a real worst", againDecision{worst: 7, signalRepeats: 2, index: 2}, ""},
		{"repeats whose worst was at the bar", againDecision{worst: 2, worstAll: 6, repeats: 1, index: 2}, AgainRepeat},
		{"repeats whose worst was under the bar", againDecision{worst: 2, worstAll: 4, repeats: 1, index: 2}, AgainEnough},
		{"repeats, fixes at the bar from round 2", againDecision{fixesMean: 6, repeats: 1, index: 2}, AgainRepeat},
		{"repeats, fixes at the bar in round 1 call for another", againDecision{fixesMean: 6, repeats: 1, index: 1}, ""},
		{"nothing at all", againDecision{index: 3}, AgainEnough},
	} {
		if got := tc.d.pick(bar); got != tc.want {
			t.Errorf("%s: pick = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// checkClaims reads the session's rejections and fixes of every severity,
// worst first, and no more than checkClaimsMax of them.
func TestCheckClaims(t *testing.T) {
	fx := newHelperFixture(t)
	audit := fx.audit("review-round:1")
	insert := func(n int, sev, state string) {
		t.Helper()
		fx.finding(audit, n, sev, state, "")
	}
	// Excluded: open and confirmed.
	insert(1, "critical", "open")
	insert(2, "critical", "confirmed")
	// Included, of every severity.
	insert(3, "low", "fixed")
	insert(4, "low", "rejected")
	insert(5, "medium", "fixed")
	insert(6, "high", "fixed")
	insert(7, "critical", "fixed")
	insert(8, "high", "rejected")
	text, ids, err := fx.s.checkClaims(fx.ctx, audit)
	if err != nil {
		t.Fatal(err)
	}
	a := fmt.Sprint(audit)
	// By severity, rejected before fixed within one, then by ordinal.
	want := []string{a + ".7", a + ".8", a + ".6", a + ".5", a + ".4", a + ".3"}
	if !slices.Equal(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	if text == "" || !hasText(text, "[%s.4] low", a) || !hasText(text, "Rejected because") || !hasText(text, "Marked fixed") {
		t.Errorf("text misses a low finding or the marks:\n%s", text)
	}
	if hasText(text, "[%s.1]", a) || hasText(text, "[%s.2]", a) {
		t.Errorf("an open or confirmed finding is in the claims:\n%s", text)
	}

	// An audit with nothing marked has no claims.
	empty := fx.audit("review-round:2")
	fx.finding(empty, 1, "high", "open", "")
	if text, ids, err := fx.s.checkClaims(fx.ctx, empty); err != nil || text != "" || len(ids) != 0 {
		t.Errorf("nothing marked: %q, %v, %v", text, ids, err)
	}

	// Past the cap: the worst checkClaimsMax, all rejected lows go last.
	big := fx.audit("review-round:3")
	for n := 1; n <= 40; n++ {
		sev := "low"
		if n > 35 {
			sev = "critical"
		}
		fx.finding(big, n, sev, "rejected", "")
	}
	_, ids, err = fx.s.checkClaims(fx.ctx, big)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != checkClaimsMax {
		t.Fatalf("%d claims, want the cap %d", len(ids), checkClaimsMax)
	}
	b := fmt.Sprint(big)
	for n := 36; n <= 40; n++ {
		if !slices.Contains(ids, fmt.Sprintf("%s.%d", b, n)) {
			t.Errorf("critical finding %d fell off the cap: %v", n, ids)
		}
	}
	for _, n := range []int{1, 25} {
		if !slices.Contains(ids, fmt.Sprintf("%s.%d", b, n)) {
			t.Errorf("low finding %d (an early ordinal) fell off the cap: %v", n, ids)
		}
	}
	if slices.Contains(ids, b+".26") {
		t.Errorf("finding 26 is past the 30 the cap keeps: %v", ids)
	}
}

// againUsefulSQL counts a next round as useful only for a new, held finding
// of medium or worse the check against the code didn't refute.
func TestAgainUsefulSQL(t *testing.T) {
	for _, tc := range []struct {
		name            string
		sev, state, ver string
		scored          bool // a score row exists
		repeat          bool // ...that says it repeats an earlier finding
		want            bool
	}{
		{"fixed medium, scored new", "medium", "fixed", "", true, false, true},
		{"confirmed high", "high", "confirmed", "", true, false, true},
		{"confirmed by the check", "medium", "fixed", VerdictConfirmed, true, false, true},
		{"unsupported by the check still counts", "medium", "fixed", VerdictUnsupported, true, false, true},
		{"refuted by the check", "medium", "fixed", VerdictRefuted, true, false, false},
		{"low", "low", "fixed", "", true, false, false},
		{"open", "high", "open", "", true, false, false},
		{"rejected", "high", "rejected", "", true, false, false},
		{"a repeat of an earlier finding", "high", "fixed", "", true, true, false},
		{"never scored", "high", "fixed", "", false, false, false},
	} {
		fx := newHelperFixture(t)
		audit := fx.audit("review-round:1")
		round := fx.round(0)
		fx.exec(`UPDATE work_log SET audit_id = ?, audit_note = '' WHERE id = ?`, audit, round)
		id := fx.finding(audit, 1, tc.sev, tc.state, tc.ver)
		if tc.scored {
			var rep any
			if tc.repeat {
				rep = id
			}
			fx.exec(`INSERT INTO review_finding_scores(finding_id, round_id, repeat_of, created_at) VALUES (?, ?, ?, 1)`, id, round, rep)
		}
		var got bool
		if err := fx.s.db.R.QueryRowContext(fx.ctx, `SELECT `+againUsefulSQL+` FROM work_log w WHERE w.id = ?`, round).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("%s: useful = %v, want %v", tc.name, got, tc.want)
		}
	}
}
