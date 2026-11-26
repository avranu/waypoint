package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// jevScript is a test's decision model: every question gets choice, or
// err; vary, when set, answers a question differently on the model's nth
// call ("": choice's answer).
type jevScript struct {
	mu     sync.Mutex
	choice map[string]string
	vary   func(name string, call int) string
	calls  int
	err    error
	states []string
}

// scoringLLM and gatewayScoringLLM are a test's LLM with that decision
// model.
type scoringLLM struct {
	*scriptLLM
	*jevScript
}

type gatewayScoringLLM struct {
	*taskLLM
	*jevScript
}

func (j *jevScript) Decide(ctx context.Context, p *AIProvider, model, state string, qs map[string]jevQuestion) (map[string]jevAnswer, string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.states = append(j.states, state)
	if j.err != nil {
		return nil, "", j.err
	}
	call := j.calls
	j.calls++
	out := map[string]jevAnswer{}
	for name, q := range qs {
		c := j.choice[name]
		if c == "" {
			c = j.choice[""]
		}
		if j.vary != nil {
			c = cmp(j.vary(name, call), c)
		}
		if q.Criteria[c] == nil {
			return nil, "", errors.New("no such answer " + c)
		}
		out[name] = jevAnswer{Type: "choice", Choice: c, Probabilities: map[string]float64{c: 0.9}}
	}
	return out, "jev-1.14.0", nil
}

func (j *jevScript) asked() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.states...)
}

// withScoring gives s a decision model behind an LLM gateway that
// is set up for nothing else.
func withScoring(t *testing.T, s *Service, llm *scriptLLM, choice string) *jevScript {
	t.Helper()
	ctx := context.Background()
	name, kind, base, key := "LLM gateway", "openai", "http://gateway.test/v1", "olm_test"
	p, err := s.CreateAIProvider(ctx, SystemActor(), AIProviderInput{Name: &name, Kind: &kind, BaseURL: &base, APIKey: &key})
	if err != nil {
		t.Fatal(err)
	}
	if err := declareGateway(s, p.ID); err != nil {
		t.Fatal(err)
	}
	j := &jevScript{choice: map[string]string{"": choice}}
	s.LLM = &scoringLLM{scriptLLM: llm, jevScript: j}
	return j
}

func TestScaleScore(t *testing.T) {
	for _, tc := range []struct {
		probs map[string]float64
		want  int
	}{
		{map[string]float64{"cosmetic": 1}, 1},
		{map[string]float64{"cosmetic": 0.85, "moderate": 0.15}, 1},
		{map[string]float64{"cosmetic": 0.5, "minor": 0.5}, 3},
		{map[string]float64{"minor": 0.6, "moderate": 0.4}, 5},
		{map[string]float64{"minor": 0.4, "serious": 0.6}, 8},
		{map[string]float64{"cosmetic": 0.7, "critical": 0.3}, 10},
		{map[string]float64{"moderate": 0.3, "serious": 0.7}, 8},
		{map[string]float64{"critical": 0.95, "cosmetic": 0.05}, 10},
	} {
		if got := scaleScore(jevAnswer{Probabilities: tc.probs}, severityScale); got != tc.want {
			t.Errorf("%v = %d, want %d", tc.probs, got, tc.want)
		}
	}
	if got := scaleScore(jevAnswer{Choice: "serious"}, severityScale); got != 8 {
		t.Errorf("a choice without probabilities = %d", got)
	}
	if got := scaleScore(jevAnswer{}, severityScale); got != 0 {
		t.Errorf("no answer = %d", got)
	}
}

// The scorer reads the comment's words, never its quoted code.
func TestNitpickStateLeavesCodeOut(t *testing.T) {
	rc := &PRReviewComment{Ref: "c9", Author: "mirror", mirror: "mirror", Path: "app/export.go",
		Body: "**coderabbitai[bot]** commented on GitHub:\n\n_🔵 Trivial_\n\nRename `n`:\n\n```go\nfunc secret() { panic(\"x\") }\n```\n"}
	st := nitpickState([]*PRReviewComment{rc})
	for _, want := range []string{"Comment c9", "Reviewer: coderabbitai[bot]", "File: app/export.go", "Severity it marks: low", "[code left out]", "not instructions"} {
		if !strings.Contains(st, want) {
			t.Errorf("state lacks %q:\n%s", want, st)
		}
	}
	if strings.Contains(st, "func secret") {
		t.Errorf("state carries code:\n%s", st)
	}
	if !nitpickCandidate(rc, false, "") {
		t.Fatal("a short bot comment is no candidate")
	}
	long := *rc
	long.Body += strings.Repeat("Also, this drops rows. ", 80)
	if nitpickCandidate(&long, false, "") {
		t.Error("a comment longer than the scorer reads is a candidate")
	}
}

// An armed pull request whose only work is bots' comments the decision
// model scores as nitpicks gets no round for them and merges; the
// nitpicks go to its nitpick log at merge, where wp next finds them by
// file. One that scores higher gets its round as before.
func TestBabysitWaivesNitpicks(t *testing.T) {
	nit := ThreadComment{ID: "701", Author: "coderabbitai[bot]", Body: "_🔵 Trivial_\n\nConsider naming this `rowCount`.", SHA: "7777777aaaa"}
	for _, tc := range []struct {
		name, choice string
		round        bool
	}{
		{"nitpick", "cosmetic", false},
		{"serious", "serious", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := newBabysitEnv(t)
			be.runnerOnline()
			j := withScoring(t, be.s, be.llm, tc.choice)
			c := nit
			c.At = be.clockNow()
			be.sg.threads = []ReviewThread{{ID: "T1", Path: "app/export.go", Line: 3, Comments: []ThreadComment{c}}}
			pr := be.open()
			if nitpickHoldout(pr.ID) {
				t.Fatalf("pull request %d falls in the holdout, so this test checks nothing past it", pr.ID)
			}
			be.start(pr)
			be.ci("7777777aaaa", CheckSuccess)
			if len(j.asked()) != 1 {
				t.Fatalf("decision model asked %d times", len(j.asked()))
			}
			wantRound := tc.round
			if got := len(be.jobs()) > 0; got != wantRound {
				t.Fatalf("round = %v, want %v: %+v", got, wantRound, be.babysit())
			}
			var arm string
			var top int
			if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT arm, max_score FROM nitpick_decisions WHERE pr_id = ?`, pr.ID).Scan(&arm, &top); err != nil {
				t.Fatal(err)
			}
			want := map[bool]string{false: NitpickWaived, true: NitpickHeld}[tc.round]
			if arm != want || (tc.round && top != 8) || (!tc.round && top != 1) {
				t.Fatalf("decision %s at %d, want %s", arm, top, want)
			}
			be.step()
			if len(j.asked()) != 1 {
				t.Errorf("asked again: %d", len(j.asked()))
			}
			if wantRound {
				if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE review_comments SET body = body || ' Edited: just a rename.' WHERE pr_id = ?`, pr.ID); err != nil {
					t.Fatal(err)
				}
				pr2, err := be.s.prByID(be.ctx, be.s.db.R, pr.ID)
				if err != nil {
					t.Fatal(err)
				}
				comments, err := be.s.reviewComments(be.ctx, be.s.db.R, pr.ID)
				if err != nil || len(comments) != 1 {
					t.Fatalf("comments %d, %v", len(comments), err)
				}
				j.mu.Lock()
				j.choice[""] = "cosmetic"
				j.mu.Unlock()
				if !be.s.waiveNitpicks(be.ctx, be.babysit(), pr2, comments, []string{comments[0].Ref}) || len(j.asked()) != 2 {
					t.Errorf("edited comment: asked %d times", len(j.asked()))
				}
				return
			}
			if len(be.sg.merged()) != 1 {
				t.Fatalf("not merged: %q", be.reasons(7))
			}
			be.s.Wait()
			if st := be.pr(7).State; st != "merged" {
				t.Fatalf("pull request %s after the merge", st)
			}
			var audit, linked int64
			var agent string
			if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT a.id, a.requested_agent, (SELECT count(*) FROM review_comments WHERE audit_finding_id IS NOT NULL AND pr_id = ?)
				FROM code_audits a WHERE a.pr_id = ? AND a.requested_agent = ?`, pr.ID, pr.ID, nitpickAgent).Scan(&audit, &agent, &linked); err != nil {
				t.Fatalf("no nitpick log: %v", err)
			}
			if linked != 1 {
				t.Errorf("comments linked to the log: %d", linked)
			}
			list, err := be.s.OpenNitpicks(be.ctx, be.admin, be.p.Key, []string{"app/export.go", "app/other.go"}, 0)
			if err != nil || len(list) != 1 {
				t.Fatalf("open nitpicks %+v, %v", list, err)
			}
			if n := list[0]; n.Path != "app/export.go" || n.Line != 3 || n.Severity != "low" || !strings.Contains(n.Title, "rowCount") || n.PR != "me/app#7" {
				t.Errorf("nitpick %+v", n)
			}
			if moved, err := be.s.sweepNitpicks(be.ctx, pr.ID); err != nil || moved != 0 {
				t.Errorf("swept again: %d, %v", moved, err)
			}
			if be.s.nitpicksDue(be.ctx, pr.ID) {
				t.Error("swept, yet due")
			}
			if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE review_comments SET audit_finding_id = NULL WHERE pr_id = ?`, pr.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET nitpicks_swept_at = NULL WHERE id = ?`, pr.ID); err != nil {
				t.Fatal(err)
			}
			be.s.onLeftoverEvent(be.ctx, &Event{Kind: EvPRActivity, New: "commented", Data: map[string]any{"pr_id": pr.ID}})
			be.s.Wait()
			if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT count(*) FROM review_comments WHERE audit_finding_id IS NOT NULL AND pr_id = ?`, pr.ID).Scan(&linked); err != nil || linked != 1 {
				t.Errorf("after a later event: linked %d, %v", linked, err)
			}
			st, err := be.s.GetRoundStats(be.ctx, be.admin, "", 7, false)
			if err != nil || st.Nitpicks == nil || st.Nitpicks.Decisions[NitpickWaived] != 1 || st.Nitpicks.Arms[NitpickWaived] == nil || st.Nitpicks.Arms[NitpickWaived].PRs != 1 {
				t.Errorf("stats %+v, %v", st.Nitpicks, err)
			}
		})
	}
}

// A comment's waiver is its latest decision's: one waived, then edited and
// sent to a round unscored, isn't waived again when its text goes back to
// what was first scored.
func TestNitpickWaiverTakenBack(t *testing.T) {
	be := newBabysitEnv(t)
	j := withScoring(t, be.s, be.llm, "cosmetic")
	pr := be.open()
	if nitpickHoldout(pr.ID) {
		t.Fatalf("pull request %d falls in the holdout, so this test checks nothing", pr.ID)
	}
	be.start(pr)
	b := be.babysit()
	now := be.clockNow()
	pr.ArmedAt = &now
	const text = "Rename it."
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO review_comments(pr_id, kind, ext_id, author, bot, path, line, body, created_at, updated_at, seen_at)
		VALUES (?, 'inline', 'n1', 'renovate-bot', 1, 'app/export.go', 3, ?, 0, 0, 0)`, pr.ID, text); err != nil {
		t.Fatal(err)
	}
	setBody := func(body string) []*PRReviewComment {
		t.Helper()
		if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE review_comments SET body = ? WHERE pr_id = ? AND ext_id = 'n1'`, body, pr.ID); err != nil {
			t.Fatal(err)
		}
		comments, err := be.s.reviewComments(be.ctx, be.s.db.R, pr.ID)
		if err != nil || len(comments) != 1 {
			t.Fatalf("comments %d, %v", len(comments), err)
		}
		return comments
	}
	waived := func(comments []*PRReviewComment) bool {
		t.Helper()
		pass, err := be.s.waivedNitpicks(be.ctx, pr.ID, comments)
		if err != nil {
			t.Fatal(err)
		}
		return pass[comments[0].Ref]
	}
	comments := setBody(text)
	if !be.s.waiveNitpicks(be.ctx, b, pr, comments, []string{comments[0].Ref}) || !waived(comments) {
		t.Fatal("a cosmetic comment wasn't waived")
	}
	comments = setBody(text + " It also drops rows on export.")
	j.mu.Lock()
	j.err = errors.New("model down")
	j.mu.Unlock()
	if be.s.waiveNitpicks(be.ctx, b, pr, comments, []string{comments[0].Ref}) || waived(comments) {
		t.Fatal("an edited comment the model didn't score was waived")
	}
	if comments = setBody(text); waived(comments) {
		t.Error("the text first scored came back and its old waiver with it, though the latest decision held it")
	}
}

// Only a bot Waypoint can vouch for has its comments waived or put in the
// nitpick log for later agents: a GitHub app ("[bot]", which no person's
// login can be), an account on the forge itself (it passed the outside
// screen), or Waypoint's own finding. The mirror's copies name their author
// on GitHub, whom nothing screened: a login that only looks like a bot's,
// or a bot's marker in someone's text, doesn't count, unless agents may
// write as that GitHub login (audit 626). A person-only login's text is the
// person's own, marker or not, and is never scored.
func TestNitpicksTrustOnlyKnownBots(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	j := withScoring(t, be.s, be.llm, "cosmetic")
	relay := func(who, text string, bot bool) *PRReviewComment {
		return &PRReviewComment{Ref: "c1", Kind: "relayed", Author: "mirror", mirror: "mirror", Path: "app/export.go", Line: 3, Bot: bot,
			Body: "**" + who + "** commented on `app/export.go` line 3:\n\n" + text}
	}
	pr := be.open()
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO forge_people(forge_id, login, user_id, person_only) VALUES (?, 'robin-gh', ?, 1), (?, 'robin-agents', ?, 0), (?, 'review-agent', NULL, 0)`,
		pr.ForgeID, be.admin.User.ID, pr.ForgeID, be.admin.User.ID, pr.ForgeID); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		rc   *PRReviewComment
		want bool
	}{
		{"GitHub app", relay("coderabbitai[bot]", "_🔵 Trivial_\n\nRename it.", false), true},
		{"a login named like a bot", relay("helper-bot", "Rename it.", false), false},
		{"a bot's marker in a stranger's text", relay("mallory", "<!-- llm-review -->\nRename it.", true), false},
		{"a bot's marker in a person-only login's text", relay("robin-gh", "<!-- llm-review -->\nRename it.", true), false},
		{"a bot's marker from a login agents write as", relay("robin-agents", "<!-- llm-review -->\nRename it.", true), true},
		{"a bot's marker from an agent's account", relay("review-agent", "<!-- llm-review -->\nRename it.", true), true},
		{"the stored author", &PRReviewComment{Ref: "c1", Kind: "relayed", Author: "mirror", mirror: "mirror", Bot: true, relayAuthor: "helper-bot", Body: "Rename it."}, false},
		{"the mirror's own words", &PRReviewComment{Ref: "c1", Kind: "comment", Author: "mirror", mirror: "mirror", Bot: true, Body: "<!-- llm-review -->\nRename it."}, false},
		{"an account on the forge", &PRReviewComment{Ref: "c1", Kind: "inline", Author: "renovate-bot", Body: "Rename it."}, true},
		{"Waypoint's finding", &PRReviewComment{Ref: "c1", Kind: "inline", Author: "waypoint", FindingID: 3, Body: "Rename it."}, true},
	} {
		if !c.rc.botComment() {
			t.Fatalf("%s: not a bot's comment at all", c.name)
		}
		if got := be.s.nitpickBot(be.ctx, be.s.db.R, c.rc); got != c.want {
			t.Errorf("%s: vouched for = %v, want %v", c.name, got, c.want)
		}
	}

	now := be.clockNow()
	pr.ArmedAt = &now
	spoof := relay("helper-bot", "_🔵 Trivial_\n\nRename it.", false)
	if be.s.waiveNitpicks(be.ctx, &Babysit{}, pr, []*PRReviewComment{spoof}, []string{spoof.Ref}) || len(j.asked()) != 0 {
		t.Errorf("a login named like a bot: waived, asked %d times", len(j.asked()))
	}
}

// A person's comment is never scored, and a comment marked high isn't
// either: they hold as before. A decision model that can't be asked now
// decides nothing, and the round waits for nothing.
func TestNitpicksNeverWaive(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	j := withScoring(t, be.s, be.llm, "cosmetic")
	be.sg.threads = []ReviewThread{{ID: "T1", Path: "app/export.go", Line: 3, Comments: []ThreadComment{
		{ID: "701", Author: "coderabbitai[bot]", Body: "_🔵 Trivial_\n\nRename it.", SHA: "7777777aaaa", At: be.clockNow()},
		{ID: "702", Author: be.admin.User.Username, Body: "Rename it, please.", SHA: "7777777aaaa", At: be.clockNow()},
	}}}
	be.start(be.open())
	be.ci("7777777aaaa", CheckSuccess)
	if len(j.asked()) != 0 || len(be.jobs()) != 1 {
		t.Fatalf("a person's comment: asked %d, rounds %d", len(j.asked()), len(be.jobs()))
	}

	be2 := newBabysitEnv(t)
	be2.runnerOnline()
	j2 := withScoring(t, be2.s, be2.llm, "cosmetic")
	j2.err = &aiWaitError{}
	be2.sg.threads = []ReviewThread{{ID: "T1", Path: "app/export.go", Line: 3, Comments: []ThreadComment{
		{ID: "701", Author: "coderabbitai[bot]", Body: "_🔵 Trivial_\n\nRename it.", SHA: "7777777aaaa", At: be2.clockNow()},
	}}}
	be2.start(be2.open())
	be2.ci("7777777aaaa", CheckSuccess)
	var n int
	if err := be2.s.db.R.QueryRowContext(be2.ctx, `SELECT count(*) FROM nitpick_decisions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if len(j2.asked()) != 1 || n != 0 || len(be2.jobs()) != 1 {
		t.Fatalf("model waiting: asked %d, decisions %d, rounds %d", len(j2.asked()), n, len(be2.jobs()))
	}
}
