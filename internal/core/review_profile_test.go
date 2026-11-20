package core

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// The reviewer's brief and model as saved (WP-1320): the default brief is
// kept as "", so a later default reaches it; modes are lower-cased; a
// brief too long, with control characters, or a model that isn't one is
// refused.
func TestReviewProfileLimits(t *testing.T) {
	l := AutopilotLimits{ReviewText: ptrOf("  " + strings.ReplaceAll(DefaultReviewText, "\n", "\r\n") + "\n"), ReviewModel: ptrOf(" Session ")}
	l.normalizeReviewProfile()
	if *l.ReviewText != "" || *l.ReviewModel != ReviewModelSession {
		t.Fatalf("normalized: %q %q", *l.ReviewText, *l.ReviewModel)
	}
	if l.reviewText() != DefaultReviewText || l.reviewModel() != ReviewModelSession {
		t.Fatalf("read back: %q", l.reviewModel())
	}
	if _, ok := l.namedReviewModel(); ok {
		t.Fatal("session names no model")
	}
	l = AutopilotLimits{ReviewModel: ptrOf("")}
	l.normalizeReviewProfile()
	if *l.ReviewModel != ReviewModelAuto {
		t.Fatalf("empty model = %q", *l.ReviewModel)
	}
	for _, m := range []string{"auto", "session", "claude/sonnet", "cursor/grok-4.7-high", "gateway/some-model"} {
		if err := (AutopilotLimits{ReviewModel: ptrOf(m)}).validateReviewProfile(); err != nil {
			t.Errorf("%s: %v", m, err)
		}
	}
	named, ok := AutopilotLimits{ReviewModel: ptrOf("claude/sonnet")}.namedReviewModel()
	if !ok || auditModelName(named) != "claude/sonnet" {
		t.Fatalf("named = %+v %v", named, ok)
	}
	for _, bad := range []AutopilotLimits{
		{ReviewModel: ptrOf("nobody/x")},
		{ReviewModel: ptrOf("Some model")},
		{ReviewText: ptrOf(strings.Repeat("a", maxReviewText+1))},
		{ReviewText: ptrOf("ok\x00then")},
	} {
		if err := bad.validateReviewProfile(); err == nil {
			t.Errorf("accepted %q %q", valueOf(bad.ReviewModel), clipText(valueOf(bad.ReviewText), 20))
		}
	}
	if err := (AutopilotLimits{ReviewText: ptrOf("Line one\n\tindented")}).validateReviewProfile(); err != nil {
		t.Fatal(err)
	}
	// A person's own brief goes into the session's prompt; with none, the
	// default does, as it does for every other reviewer.
	if p := ownReviewBrief("Review it.", AutopilotLimits{}); !strings.HasPrefix(p, "Review it.") || !strings.Contains(p, "<brief>\n"+briefText(promptText(DefaultReviewText))+"\n</brief>") {
		t.Fatalf("no brief: %q", p)
	}
	if p := ownReviewBrief("Review it.", AutopilotLimits{ReviewText: ptrOf("  ")}); !strings.Contains(p, "Your job is to break it") {
		t.Fatalf("a blank brief: %q", p)
	}
	if p := ownReviewBrief("Review it.", AutopilotLimits{ReviewText: ptrOf("Check the locks.")}); !strings.Contains(p, "<brief>\nCheck the locks.\n</brief>") {
		t.Fatalf("brief: %q", p)
	}
	// A brief may hold tabs (a code sample); the session's prompt can't, or
	// its runner job is refused.
	if p := ownReviewBrief("Review it.", AutopilotLimits{ReviewText: ptrOf("Check:\n\tthe locks")}); strings.ContainsRune(p, '\t') || !strings.Contains(p, "\n the locks") {
		t.Fatalf("a brief with a tab: %q", p)
	}
}

// Saved through the settings, the brief and the model are kept per project
// over the person's defaults, and the options list the modes, the
// runners' CLIs and the default brief.
func TestReviewProfileSettings(t *testing.T) {
	a, _ := auditAPEnv(t)
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, "", AutopilotSettingInput{Merge: true,
		Limits: AutopilotLimits{ReviewText: ptrOf("My brief."), ReviewModel: ptrOf("claude/sonnet")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Merge: true,
		Limits: AutopilotLimits{ReviewModel: ptrOf("SESSION")}}); err != nil {
		t.Fatal(err)
	}
	_, lim, err := a.s.autopilotFor(a.ctx, a.s.db.R, a.admin.User.ID, a.p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lim.reviewText() != "My brief." || lim.reviewModel() != ReviewModelSession {
		t.Fatalf("effective: %q %q", lim.reviewText(), lim.reviewModel())
	}
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Merge: true,
		Limits: AutopilotLimits{ReviewModel: ptrOf("nobody/x")}}); err == nil {
		t.Fatal("a model that isn't one was saved")
	}
	o, err := a.s.ReviewModelOptions(a.ctx, a.admin)
	if err != nil {
		t.Fatal(err)
	}
	values := func(opts []ReviewModelOption) (out []string) {
		for _, x := range opts {
			out = append(out, x.Value)
		}
		return
	}
	if !slices.Equal(values(o.Modes), []string{ReviewModelAuto, ReviewModelSession}) || o.DefaultText != DefaultReviewText || o.MaxText != maxReviewText {
		t.Fatalf("modes %v", values(o.Modes))
	}
	if r := values(o.Runners); !slices.Contains(r, "claude/sonnet") || !slices.Contains(r, "cursor/grok-4.7-high") {
		t.Fatalf("runners %v", r)
	}
	if len(o.Gateway) == 0 || o.Gateway[0].Value != GatewayCLI+"/"+gatewayAuditModel {
		t.Fatalf("gateway %+v", o.Gateway)
	}
	if _, err := a.s.ReviewModelOptions(a.ctx, a.ag); err != nil {
		t.Fatalf("an agent reads the options: %v", err)
	}
}

// review_model "session": the round asks no other model; the session
// reviews its own change, by the person's brief.
func TestReviewModelSession(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	if _, err := e.s.SetAutopilot(e.ctx, e.admin, e.p.Key, AutopilotSettingInput{Merge: true,
		Limits: AutopilotLimits{ReviewModel: ptrOf("session"), ReviewText: ptrOf("Check every lock twice.")}}); err != nil {
		t.Fatal(err)
	}
	if res, err := e.ship(e.task.Key, ShipAuto); err != nil || res.Held == "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	e.stopWith("Pushed; the review comes first.")
	round := e.step().reviewID.Int64
	e.tick(61 * time.Second)
	e.tick(time.Second)
	if len(e.jobs) == 0 {
		t.Fatal("no review job")
	}
	var in AutopilotJob
	_ = json.Unmarshal(e.jobs[len(e.jobs)-1].Input, &in)
	if !strings.Contains(in.Review, "<brief>\nCheck every lock twice.\n</brief>") {
		t.Fatalf("the session's prompt lacks the brief: %q", in.Review)
	}
	ra, au := e.roundAuditRow(round)
	if au != nil || ra.note != reviewBySession {
		t.Fatalf("round audit %+v note %q", au, ra.note)
	}
	var n int
	if err := e.s.db.R.QueryRow(`SELECT COUNT(*) FROM code_audits`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("audits asked: %d %v", n, err)
	}
}

// A named review_model reviews the round's first audit, by the brief, at
// no stakes bar; when it fails the round goes on as an automatic one.
func TestReviewModelNamed(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	if _, err := e.s.SetAutopilot(e.ctx, e.admin, e.p.Key, AutopilotSettingInput{Merge: true,
		Limits: AutopilotLimits{ReviewModel: ptrOf("cursor/grok-4.7-high"), ReviewText: ptrOf("Check every lock twice.")}}); err != nil {
		t.Fatal(err)
	}
	if res, err := e.ship(e.task.Key, ShipAuto); err != nil || res.Held == "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	e.stopWith("Pushed; the review comes first.")
	round := e.step().reviewID.Int64
	e.tick(61 * time.Second)
	_, au := e.roundAuditRow(round)
	if au == nil || !slices.Equal(au.Models, []string{"cursor/grok-4.7-high"}) || au.Weakest != "" {
		t.Fatalf("round audit %+v", au)
	}
	var prompt string
	if err := e.s.db.R.QueryRow(`SELECT prompt FROM code_audits WHERE id = ?`, au.ID).Scan(&prompt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "<brief>\nCheck every lock twice.\n</brief>") || strings.Contains(prompt, "Burden of proof") || !strings.Contains(prompt, "Keep the merge from losing updates") {
		t.Fatalf("prompt:\n%s", prompt)
	}
	e.endAudit(e.box, "failed")
	e.tick(time.Second)
	ra, next := e.roundAuditRow(round)
	if next == nil || next.ID == au.ID || ra.alt == "" || slices.Equal(next.Models, au.Models) {
		t.Fatalf("after the named model failed: %+v %+v", ra, next)
	}
	if want := auditModelNames(e.s.defaultAuditChain(e.ctx)); !slices.Equal(next.Models, want) {
		t.Fatalf("fallback models %v, want %v", next.Models, want)
	}
}
