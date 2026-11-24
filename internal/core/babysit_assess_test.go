package core

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// finishWith ends a round's job with the agent's own last words.
func (be *bsEnv) finishWith(id int64, pushed bool, answer string) {
	be.t.Helper()
	res, _ := json.Marshal(map[string]any{"pushed": pushed, "commits": []string{"abc"}, "commit": "abc1234", "answer": answer})
	if err := be.s.write(be.ctx, SystemActor(), func(t *tx) error { return t.finishRunnerJob(be.ctx, id, "done", nil, res, "") }); err != nil {
		be.t.Fatal(err)
	}
	be.s.Wait()
	be.s.Wait()
}

func (be *bsEnv) parkRequest(n int) *Request {
	be.t.Helper()
	var id int64
	if err := be.s.db.R.QueryRow(`SELECT id FROM requests WHERE tool_use_id = ?`, parkRequestKey+"1:"+strconv.Itoa(n)).Scan(&id); err != nil {
		be.t.Fatalf("no park question for round %d: %v", n, err)
	}
	return be.request(id)
}

// The assessor reads what the change is for: the linked task, the decision
// written for it and the pull request's description; a person's question it
// ends with parks the pull request and asks its owner, in plain words.
func TestBabysitAssessorQuestionParks(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	task, err := be.s.CreateIssue(be.ctx, be.admin, IssueInput{Project: be.p.Key, Title: "Export rows", Description: "Exports must skip archived rows."})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.db.W.Exec(`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		task.ID, "github", "pull", "https://github.com/me/app/pull/7", "me/app", 7, "open", 1); err != nil {
		t.Fatal(err)
	}
	title, kind, body := "Exports skip archives", "decision", "Archived rows stay out of every export."
	if _, err := be.s.CreateDesign(be.ctx, be.admin, DesignInput{Title: &title, Kind: &kind, Body: &body, Project: &be.p.Key, For: task.Key}); err != nil {
		t.Fatal(err)
	}
	pr := be.open()
	be.start(pr)
	be.ci("7777777aaaa", CheckFailed)
	// A round widened the description after it shipped: the assessor
	// weighs the work against the one it shipped with (WP-440).
	be.fg.mu.Lock()
	be.fg.prs[mk(be.f, "me/app", 7)].Body = "Adds export and rewrites the importer."
	be.fg.mu.Unlock()
	be.finish(be.babysit().JobID, false)
	be.finish(be.babysit().JobID, false)
	b := be.babysit()
	if !b.Rounds[len(b.Rounds)-1].Assessor {
		t.Fatalf("no assessor round: %+v", b)
	}
	in := be.jobInput(b.JobID)
	for _, want := range []string{"Task " + task.Key + ":\n<untrusted>\nExport rows\n", "Exports must skip archived rows.", "Archived rows stay out of every export.",
		"The pull request's description when it shipped:\n<untrusted>\nAdds export."} {
		if !strings.Contains(in.Prompt, want) {
			t.Errorf("the assessor's prompt lacks %q:\n%s", want, in.Prompt)
		}
	}
	if strings.Contains(in.Prompt, "rewrites the importer") {
		t.Errorf("the assessor was given today's widened description:\n%s", in.Prompt)
	}
	if n := strings.Count(in.Prompt, "Exports must skip archived rows."); n != 1 {
		t.Errorf("the task is in the assessor's prompt %d times, want once:\n%s", n, in.Prompt)
	}

	// It holds the front of its merge line while the assessor works.
	if _, err := be.s.db.W.Exec(`UPDATE pull_requests SET armed_at = 1, queued_at = 1 WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	be.finishWith(b.JobID, false, "The test asserts archived rows are exported, which the task forbids.\n\n**Question:** Should exports include archived rows?\n**Because:** it changes what the task asks for.")
	b = be.babysit()
	r := b.Rounds[2]
	if b.State != BabysitNeedsPerson || r.Question != "Should exports include archived rows?" || r.Because != "it changes what the task asks for." || r.Parked == "" || !strings.Contains(b.Wait, "Should exports include archived rows?") {
		t.Fatalf("after the assessor's question: %+v", b)
	}
	q := be.parkRequest(3)
	if q.Kind != RequestQuestion || q.Title != "me/app#7: Should exports include archived rows?" || !strings.Contains(q.Body, "out of the merge line") ||
		!strings.Contains(q.Body, "as the assessor put it:\n\n> it changes what the task asks for.") || len(q.Questions) != 1 || q.Questions[0].Question != "Should exports include archived rows?" {
		t.Fatalf("question = %+v", q)
	}
	// Short: the agents' own account stays on the pull request (WP-930).
	if strings.Contains(q.Body, "archived rows are exported") || len(q.Body) > 600 {
		t.Errorf("the request quotes the agent's account (%d bytes):\n%s", len(q.Body), q.Body)
	}
	if p := be.pr(7); p.QueuedAt != nil {
		t.Error("parked, it keeps its place at the front of the line")
	}
	if why, _ := be.s.babysitParked(be.ctx, be.pr(7)); !strings.Contains(why, "the assessor says a person has to settle it") {
		t.Errorf("readiness hold = %q", why)
	}
	// Waypoint restarting doesn't start it again: the question stands.
	if _, _, ok := be.s.resumable(be.ctx, pr.ID); ok {
		t.Error("a parked babysit is resumable")
	}
	// Steps don't ask twice.
	be.step()
	be.step()
	var asked int
	_ = be.s.db.R.QueryRow(`SELECT count(*) FROM requests WHERE tool_use_id LIKE ?`, parkRequestKey+"%").Scan(&asked)
	if asked != 1 || len(be.jobs()) != 3 {
		t.Fatalf("%d questions, %d jobs", asked, len(be.jobs()))
	}
	// A dismissed question leaves it parked: nothing runs on its own.
	if _, err := be.s.DecideRequest(be.ctx, be.admin, q.Ref, RequestDecision{Decision: "dismiss"}); err != nil {
		t.Fatal(err)
	}
	be.s.Wait()
	be.step()
	if b = be.babysit(); b.State != BabysitNeedsPerson || len(be.jobs()) != 3 {
		t.Fatalf("after a dismissal: %+v", b)
	}
	// A push does.
	be.push("8888888bbbb")
	be.ci("8888888bbbb", CheckFailed)
	if b = be.babysit(); b.State != BabysitWorking || b.Round != 4 || len(be.jobs()) != 4 {
		t.Fatalf("after a push: %+v", b)
	}
}

func TestClosingLines(t *testing.T) {
	cases := map[string][3]string{
		"Fixed it.":                         {},
		"Done.\nQuestion: Keep the flag?":   {"Keep the flag?"},
		"Done.\n- **Question:** Which one?": {"Which one?"},
		"Question: first\nmore\nQuestion:":  {"first more"},
		"Question:   \n":                    {},
		"**Question:** Restart the host?\n**Because:** I have no access to it.": {"Restart the host?", "I have no access to it."},
		"Because: only its owner knows.\nQuestion: Drop the export?":            {"Drop the export?", "only its owner knows."},
		"It isn't the code.\n\nWait: the CodeQL image needs Go 1.27.4":          {"", "", "the CodeQL image needs Go 1.27.4"},
		"I would wait: nothing to do. Because of that, no question: none.":      {},
		// The reason on the lines after its word, and a question over two lines.
		"Question: May I drop it?\nBecause:\n- it can't be undone":                      {"May I drop it?", "it can't be undone"},
		"**Question**\nMay I\nrotate the key?\n\n**Because**\nOnly its owner holds it.": {"May I rotate the key?", "Only its owner holds it."},
		"question: Drop it?\nbecause: it is theirs.\n\nThat is all.":                    {"Drop it?", "it is theirs."},
		// Logs and other people's words aren't how the assessor ended.
		"Pushed a fix for:\n> Wait: lock held by other job\n\nDone.":      {},
		"The log:\n```\nQuestion: retry?\nBecause: flaky\n```\nFixed it.": {},
		"Because the test fails, I changed it.\nWait for it to pass.":     {},
	}
	for in, want := range cases {
		if q, why, w := closingLines(in); q != want[0] || why != want[1] || w != want[2] {
			t.Errorf("closingLines(%q) = %q, %q, %q, want %q", in, q, why, w, want)
		}
	}
}

// Rounds that make progress don't count against the budget; three that
// don't send the next round to the assessor.
func TestPlanRoundBudget(t *testing.T) {
	b := &Babysit{Model: babysitModel, MaxRounds: babysitMaxRounds, MaxRepeats: babysitMaxRepeats}
	add := func(class string) {
		b.Round++
		b.Rounds = append(b.Rounds, BabysitRound{N: b.Round, SHA: "a" + strconv.Itoa(b.Round), Class: class, Pushed: class != RoundNothingPushed})
	}
	for range 8 {
		add(RoundNew)
	}
	if p := planRound(b, "zzz", false, "", 0, "work"); p.rung != rungBase {
		t.Fatalf("8 rounds of new work: %+v", p)
	}
	add(RoundRepeat)
	add(RoundCaused)
	if p := planRound(b, "zzz", false, "", 0, "work"); p.rung != rungBase {
		t.Fatalf("2 stalled: %+v", p)
	}
	add(RoundNothingPushed)
	if p := planRound(b, "zzz", false, "", 0, "work"); p.rung != rungAssessor || !strings.Contains(p.why, "3 rounds made no progress") {
		t.Fatalf("3 stalled: %+v", p)
	}
	// The assessor's round starts the count again.
	b.Round++
	b.Rounds = append(b.Rounds, BabysitRound{N: b.Round, SHA: "zzz", Assessor: true, Model: escalateModel, Class: RoundRepeat, Pushed: true})
	if p := planRound(b, "yyy", false, "", 0, "work"); p.rung != rungBase {
		t.Fatalf("after the assessor: %+v", p)
	}
	// The cap.
	for b.Round < babysitMaxRounds {
		add(RoundNew)
	}
	if p := planRound(b, "yyy", false, "", 0, "work"); p.rung != rungAssessor || !strings.Contains(p.why, "the most a babysit runs") {
		t.Fatalf("at the cap: %+v", p)
	}
}

func TestAssessorBlocked(t *testing.T) {
	b := &Babysit{Model: babysitModel, MaxRounds: babysitMaxRounds, Round: 2, Rounds: []BabysitRound{
		{N: 1, SHA: "aaa", Checks: []string{"ci / test"}},
		{N: 2, SHA: "bbb", Assessor: true, Checks: []string{"ci / test"}, Comments: []string{"c1"}},
	}}
	if why := assessorBlocked(b, "bbb", &BabysitRound{}); !strings.Contains(why, "on this head") {
		t.Errorf("same head: %q", why)
	}
	if why := assessorBlocked(b, "ccc", &BabysitRound{Comments: []string{"c1"}}); !strings.Contains(why, "c1 still open after the assessor's round 2") {
		t.Errorf("same comment: %q", why)
	}
	if why := assessorBlocked(b, "ccc", &BabysitRound{Comments: []string{"c2"}}); why != "" {
		t.Errorf("new work: %q", why)
	}
	// A person's answer starts it again.
	b.Rounds[1].Reply = "Drop the test."
	if why := assessorBlocked(b, "bbb", &BabysitRound{Checks: []string{"ci / test"}}); why != "" {
		t.Errorf("after an answer: %q", why)
	}
}

// A repeat after the stronger model's fix goes to the assessor; after the
// assessor's, to a person.
func TestRaiseForLedger(t *testing.T) {
	b := &Babysit{Model: babysitModel, Rounds: []BabysitRound{
		{N: 1, SHA: "aaa", Pushed: true},
		{N: 2, SHA: "bbb", Pushed: true, Model: escalateModel},
		{N: 3, SHA: "ccc", Pushed: true, Model: escalateModel, Assessor: true},
	}}
	round := func(of int) *BabysitRound {
		return &BabysitRound{Ledger: []LedgerEntry{{ID: "ci / test", Kind: "check", Class: RoundRepeat, Round: of}}}
	}
	if p := raiseForLedger(b, roundPlan{}, round(1)); p.rung != rungStrong {
		t.Errorf("repeat of the base round: %+v", p)
	}
	if p := raiseForLedger(b, roundPlan{}, round(2)); p.rung != rungAssessor {
		t.Errorf("repeat of the stronger round: %+v", p)
	}
	if p := raiseForLedger(b, roundPlan{}, round(3)); p.rung != rungPark {
		t.Errorf("repeat of the assessor's round: %+v", p)
	}
	caused := &BabysitRound{Ledger: []LedgerEntry{{ID: "c9", Kind: "comment", Class: RoundCaused}}}
	if p := raiseForLedger(b, roundPlan{}, caused); p.rung != rungStrong {
		t.Errorf("caused: %+v", p)
	}
}

// A comment a round left for a person is the assessor's next, and its owner
// is asked only the question that remains: quoted, linked, and answered back
// into a round that settles it (WP-759).
func TestAgentLeftCommentGoesToTheAssessorThenItsOwner(t *testing.T) {
	be := newBabysitEnv(t)
	be.sg.threads = humanThread()
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.ci("7777777aaaa", CheckSuccess)
	c := be.comment("501")
	if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{c.Ref}, Verdict: "needs-person", Note: "Whether empty rows count is a product call."}); err != nil {
		t.Fatal(err)
	}
	be.finish(be.babysit().JobID, false)
	b := be.babysit()
	if b.State != BabysitWorking || len(b.Rounds) != 2 || !b.Rounds[1].Assessor {
		t.Fatalf("no assessor round for the held comment: %+v", b)
	}
	in := be.jobInput(b.JobID)
	for _, want := range []string{"Handle empty rows.", "Whether empty rows count is a product call."} {
		if !strings.Contains(in.Prompt, want) {
			t.Errorf("the assessor's prompt lacks %q:\n%s", want, in.Prompt)
		}
	}
	be.finishWith(b.JobID, false, "It depends on the export's contract.\n\nQuestion: Should empty rows be exported?\nBecause: whether empty rows count is the product owner's call.")
	b = be.babysit()
	q := be.parkRequest(2)
	if b.State != BabysitNeedsPerson || q.Kind != RequestQuestion || q.Questions[0].Question != "Should empty rows be exported?" {
		t.Fatalf("parked: %+v, asked %+v", b, q)
	}
	for _, want := range []string{"Handle empty rows.", "app/export.go:4", "](" + c.URL + ")", "Whether empty rows count is a product call."} {
		if !strings.Contains(q.Body, want) {
			t.Errorf("the request lacks %q:\n%s", want, q.Body)
		}
	}
	if strings.Contains(q.Body, c.Ref) {
		t.Errorf("the request names the comment by its id:\n%s", q.Body)
	}
	if _, err := be.s.DecideRequest(be.ctx, be.admin, q.Ref, RequestDecision{Decision: "answer", Text: "No, skip them."}); err != nil {
		t.Fatal(err)
	}
	be.s.Wait()
	b = be.babysit()
	if b.State != BabysitWorking || len(b.Rounds) != 3 || !b.Rounds[2].Assessor || !strings.Contains(be.jobInput(b.JobID).Prompt, "No, skip them.") {
		t.Fatalf("the answer didn't reach a round: %+v", b)
	}
	if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{c.Ref}, Verdict: VerdictRejected, Note: "Skipped already, as the owner wants."}); err != nil {
		t.Fatalf("the assessor couldn't settle it: %v", err)
	}
	be.finish(b.JobID, false)
	be.step()
	if len(be.sg.merged()) != 1 {
		t.Fatalf("not merged: %+v", be.babysit())
	}
}

// An assessor that neither settles the comment nor asks a person asks nobody
// (WP-930): the pull request rests out of the merge line, and the assessor
// gets it again once the rest is up.
func TestAssessorSilentOnHeldCommentRests(t *testing.T) {
	be := newBabysitEnv(t)
	be.sg.threads = humanThread()
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.ci("7777777aaaa", CheckSuccess)
	c := be.comment("501")
	if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{c.Ref}, Verdict: "needs-person", Note: "A product call."}); err != nil {
		t.Fatal(err)
	}
	be.finish(be.babysit().JobID, false)
	be.finish(be.babysit().JobID, false)
	be.step()
	be.step()
	if n := len(be.jobs()); n != 2 {
		t.Fatalf("jobs = %d", n)
	}
	b := be.babysit()
	if n := be.parkQuestions(); n != 0 || b.State != BabysitResting || !strings.HasPrefix(b.Wait, restWait) || !parkedOn(b, be.pr(7).HeadSHA) {
		t.Fatalf("%d questions; %s %q", n, b.State, b.Wait)
	}
	be.setClock(be.clockNow().Add(restFirst))
	be.runnerOnline()
	be.step()
	b = be.babysit()
	if n := len(be.jobs()); n != 3 || !lastRound(b).Assessor || !strings.Contains(be.jobInput(b.JobID).Prompt, "Nobody was asked") {
		t.Fatalf("jobs = %d after the rest: %s %q %+v", n, b.State, b.Wait, lastRound(b))
	}
}

// Settling the comment a park asks about is an answer: the question goes and
// the pull request carries on.
func TestPersonSettlingTheCommentEndsThePark(t *testing.T) {
	be := newBabysitEnv(t)
	be.sg.threads = humanThread()
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.ci("7777777aaaa", CheckSuccess)
	c := be.comment("501")
	if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{c.Ref}, Verdict: "needs-person", Note: "A product call."}); err != nil {
		t.Fatal(err)
	}
	be.finish(be.babysit().JobID, false)
	be.finish(be.babysit().JobID, false)
	be.step()
	be.step()
	if b := be.babysit(); !parkedOn(b, pr.HeadSHA) {
		t.Fatalf("not parked: %+v", b)
	}
	// An agent's held comment is an engineering call: it rests, nobody asked.
	if !resting(lastRound(be.babysit())) {
		t.Fatalf("not resting: %+v", lastRound(be.babysit()))
	}
	var open int
	if err := be.s.db.R.QueryRow(`SELECT count(*) FROM requests`).Scan(&open); err != nil || open != 0 {
		t.Fatalf("a rest asked somebody: requests = %d, %v", open, err)
	}
	if _, err := be.s.DismissReviewComment(be.ctx, be.admin, c.Ref, "Not needed."); err != nil {
		t.Fatal(err)
	}
	be.s.Wait()
	if b := be.babysit(); parkedOn(b, pr.HeadSHA) || b.State == BabysitNeedsPerson {
		t.Fatalf("still parked after the comment was settled: %+v", b)
	}
	var n int
	if err := be.s.db.R.QueryRow(`SELECT count(*) FROM requests`).Scan(&n); err != nil || n != 0 {
		t.Errorf("requests = %d, %v", n, err)
	}
}

// A reply changes no verdict, so it doesn't change whose verdict it is.
func TestReplyKeepsWhoHeldTheComment(t *testing.T) {
	be, ref := restedOnComment(t, func(be *bsEnv) *Actor { return be.admin })
	if _, err := be.s.ReplyToReviewComment(be.ctx, be.agent, ref, "Looked at it."); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{ref}, Verdict: "rejected", Note: "No."}); err == nil {
		t.Fatal("an agent's reply let it overrule a person")
	}
	be2, ref2 := restedOnComment(t, func(be *bsEnv) *Actor { return be.agent })
	if _, err := be2.s.ReplyToReviewComment(be2.ctx, be2.admin, ref2, "Hm, let me think."); err != nil {
		t.Fatal(err)
	}
	if _, err := be2.s.AckReviewComments(be2.ctx, be2.agent, AckInput{IDs: []string{ref2}, Verdict: "rejected", Note: "Wrong."}); err != nil {
		t.Fatalf("a person's reply locked an agent's own hold: %v", err)
	}
}

// parkedAboutChecks parks a babysit over a failing check, with a comment an
// agent held open that the round wasn't given, on a person's question.
func parkedAboutChecks(t *testing.T) (*bsEnv, *PullRequest, string) {
	t.Helper()
	be, ref := restedOnComment(t, func(be *bsEnv) *Actor { return be.agent })
	pr := be.pr(7)
	b := be.babysit()
	b.Rounds = append(b.Rounds, BabysitRound{N: len(b.Rounds) + 1, SHA: pr.HeadSHA, Checks: []string{"ci"}, Outcome: "not fixed", Because: "it is the owner's policy"})
	r := lastRound(b)
	be.s.parkBabysit(be.ctx, b, pr, "the assessor asked a question only a person can answer", "Should the auth check stay?", r.Checks, r.Conflict, r.Comments, r.Findings)
	if b := be.babysit(); !parkedOn(b, pr.HeadSHA) || len(lastRound(b).ParkedOn) != 0 {
		t.Fatalf("park = %+v", lastRound(b))
	}
	return be, pr, ref
}

// Settling a comment doesn't answer a question that wasn't about it.
func TestSettlingAnotherCommentLeavesThePark(t *testing.T) {
	be, pr, ref := parkedAboutChecks(t)
	if _, err := be.s.DismissReviewComment(be.ctx, be.admin, ref, "Not needed."); err != nil {
		t.Fatal(err)
	}
	be.step()
	b := be.babysit()
	if !parkedOn(b, pr.HeadSHA) || lastRound(b).Reply != "" {
		t.Fatalf("the question was taken for answered: %+v", lastRound(b))
	}
}

// Comments that can't be read aren't settled comments.
func TestUnreadableCommentsDontEndThePark(t *testing.T) {
	be := newBabysitEnv(t)
	be.sg.threads = humanThread()
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	ref := be.comment("501").Ref
	b := be.babysit()
	b.Rounds = append(b.Rounds, BabysitRound{N: len(b.Rounds) + 1, SHA: pr.HeadSHA, Comments: []string{ref}, Parked: "why", ParkSHA: pr.HeadSHA, ParkedOn: []string{ref}})
	if _, err := be.s.db.W.Exec(`ALTER TABLE review_comment_events RENAME TO gone_events`); err != nil {
		t.Fatal(err)
	}
	if be.s.settledWhileParked(be.ctx, b, pr) || lastRound(b).Reply != "" {
		t.Fatalf("taken for settled: %+v", lastRound(b))
	}
}

// A round handed to a session doesn't hide a comment an agent held once the
// babysit is resting on that comment.
func TestHeldCommentAfterAHandOverStillGoesToTheAssessor(t *testing.T) {
	be, _ := restedOnComment(t, func(be *bsEnv) *Actor { return be.agent })
	pr := be.pr(7)
	b := be.babysit()
	b.Rounds = append(b.Rounds, BabysitRound{N: len(b.Rounds) + 1, SHA: pr.HeadSHA, To: "session"})
	b.Round, b.RoundSHA = len(b.Rounds), pr.HeadSHA
	raw, _ := json.Marshal(b.Rounds)
	if err := be.s.setBabysit(be.ctx, b, map[string]any{"rounds": string(raw), "round": b.Round, "round_sha": pr.HeadSHA}); err != nil {
		t.Fatal(err)
	}
	be.s.reviveRound(be.ctx, pr.ID)
	be.s.Wait()
	if r := lastRound(be.babysit()); !r.Assessor || r.To == "session" {
		t.Fatalf("no assessor round: %+v", r)
	}
}

// The latest verdict says whose it is: a reopening edit makes it nobody's, a
// person repeating an agent's verdict makes it theirs, and a person's hold
// stays through an edit.
func TestWhoseVerdictItIs(t *testing.T) {
	edit := func(be *bsEnv) {
		be.t.Helper()
		be.sg.threads[0].Comments[0].Body = "Handle empty rows and nil ones."
		if _, err := be.s.syncReviewComments(be.ctx, be.pr(7)); err != nil {
			be.t.Fatal(err)
		}
		be.s.Wait()
	}
	reject := AckInput{Verdict: "rejected", Note: "No."}
	t.Run("a person's other verdict reopened by an edit", func(t *testing.T) {
		be := newBabysitEnv(t)
		be.sg.threads = humanThread()
		pr := be.open()
		be.start(pr)
		ref := be.comment("501").Ref
		if _, err := be.s.AckReviewComments(be.ctx, be.admin, AckInput{IDs: []string{ref}, Verdict: "accept", Note: "Fair."}); err != nil {
			t.Fatal(err)
		}
		edit(be)
		if c := be.comment("501"); c.Verdict != VerdictNew {
			t.Fatalf("verdict = %q", c.Verdict)
		}
		in := reject
		in.IDs = []string{ref}
		if _, err := be.s.AckReviewComments(be.ctx, be.agent, in); err != nil {
			t.Fatalf("an edit left it locked: %v", err)
		}
	})
	t.Run("a person's hold through an edit", func(t *testing.T) {
		be, ref := restedOnComment(t, func(be *bsEnv) *Actor { return be.admin })
		edit(be)
		if c := be.comment("501"); c.Verdict != VerdictNeedsPerson {
			t.Fatalf("verdict = %q", c.Verdict)
		}
		in := reject
		in.IDs = []string{ref}
		if _, err := be.s.AckReviewComments(be.ctx, be.agent, in); err == nil {
			t.Fatal("an edit let an agent overrule a person")
		}
	})
	t.Run("a person repeats an agent's hold", func(t *testing.T) {
		be, ref := restedOnComment(t, func(be *bsEnv) *Actor { return be.agent })
		if _, err := be.s.AckReviewComments(be.ctx, be.admin, AckInput{IDs: []string{ref}, Verdict: "needs-person", Note: "Yes, mine."}); err != nil {
			t.Fatal(err)
		}
		in := reject
		in.IDs = []string{ref}
		if _, err := be.s.AckReviewComments(be.ctx, be.agent, in); err == nil {
			t.Fatal("an agent overruled a person's hold")
		}
	})
}

// assessorAsks runs #7 to the assessor over its failing check and has it end
// with question.
func assessorAsks(t *testing.T, be *bsEnv, question string) *PullRequest {
	t.Helper()
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.ci("7777777aaaa", CheckFailed)
	be.finish(be.babysit().JobID, false)
	be.finish(be.babysit().JobID, false)
	b := be.babysit()
	if !lastRound(b).Assessor {
		t.Fatalf("no assessor round: %+v", b)
	}
	be.finishWith(b.JobID, false, "The check fails on the CI image, not the code.\n\n**Question:** "+question+"\n**Because:** only its owner can say.")
	if b := be.babysit(); !parkedOn(b, pr.HeadSHA) {
		t.Fatalf("not parked: %+v", b)
	}
	return pr
}

func (be *bsEnv) parkQuestions() int {
	be.t.Helper()
	var n int
	if err := be.s.db.R.QueryRow(`SELECT count(*) FROM requests WHERE tool_use_id LIKE ?`, parkRequestKey+"%").Scan(&n); err != nil {
		be.t.Fatal(err)
	}
	return n
}

func (be *bsEnv) openRequest(title string) *Request {
	be.t.Helper()
	req, _, err := be.s.CreateRequest(be.ctx, be.agent, RequestInput{Kind: RequestQuestion, Title: title, Project: be.p.Key, Questions: []Question{{Question: title + "?"}}})
	if err != nil {
		be.t.Fatal(err)
	}
	return req
}

// The failing check a park was over passing on the same head ends the park:
// what failed it was outside the code, and nobody needs to answer
// (WP-914: #368, #369 and #372 waited for answers after CodeQL passed).
func TestParkEndsWhenItsChecksPass(t *testing.T) {
	be := newBabysitEnv(t)
	pr := assessorAsks(t, be, "Will you rebuild the CI image?")
	q := be.parkRequest(3)
	// The re-run's status comes with no webhook: Waypoint reads the head again.
	be.sg.setChecks("7777777aaaa", CommitCheck{Name: "ci / test", State: CheckSuccess, URL: "https://ci.example.com/run/2"})
	be.step()
	if b := be.babysit(); !parkedOn(b, pr.HeadSHA) {
		t.Fatalf("the head was read again before %s: %+v", parkReread, lastRound(b))
	}
	be.setClock(be.clockNow().Add(parkReread))
	be.step()
	b := be.babysit()
	r := lastRound(b)
	if parkedOn(b, pr.HeadSHA) || b.State == BabysitNeedsPerson || !strings.Contains(r.Reply, "The failing check it was parked over (ci / test) passed on the same head") {
		t.Fatalf("still parked after its check passed: %s %q %+v", b.State, b.Wait, r)
	}
	if got := be.request(q.ID); got.State != RequestWithdrawn {
		t.Errorf("question = %s", got.State)
	}
}

// A park over a conflict, review comments or audit findings as well as
// failing checks is not ended by those checks passing: the rest still
// stands. parkBabysit decides it for every caller, the ledger's park
// included, which passed every failing check before (c4656).
func TestParkOverMoreThanChecksOutlastsThem(t *testing.T) {
	for _, tc := range []struct {
		name      string
		conflict  bool
		comments  []string
		findings  []string
		wantLifts bool
	}{
		{name: "checks only", wantLifts: true},
		{name: "conflict", conflict: true},
		{name: "comments", comments: []string{"c1"}},
		{name: "findings", findings: []string{"f1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := newBabysitEnv(t)
			be.runnerOnline()
			pr := be.open()
			be.start(pr)
			b := be.babysit()
			checks := []string{"ci / test"}
			b.Rounds = append(b.Rounds, BabysitRound{N: len(b.Rounds) + 1, SHA: pr.HeadSHA, Checks: checks, Conflict: tc.conflict, Comments: tc.comments, Findings: tc.findings, Outcome: "not fixed"})
			be.s.parkBabysit(be.ctx, b, pr, "its ledger is spent", "", checks, tc.conflict, tc.comments, tc.findings)
			b = be.babysit()
			if !parkedOn(b, pr.HeadSHA) {
				t.Fatalf("not parked: %+v", lastRound(b))
			}
			if got := len(lastRound(b).ParkedChecks) > 0; got != tc.wantLifts {
				t.Fatalf("ParkedChecks = %v, want lifting on checks %v", lastRound(b).ParkedChecks, tc.wantLifts)
			}
			be.sg.setChecks(pr.HeadSHA, CommitCheck{Name: "ci / test", State: CheckSuccess, URL: "https://ci.example.com/run/2"})
			be.setClock(be.clockNow().Add(parkReread))
			be.step()
			b = be.babysit()
			if parked := parkedOn(b, pr.HeadSHA); parked == tc.wantLifts {
				t.Fatalf("parked = %v after its checks passed: %+v", parked, lastRound(b))
			}
		})
	}
}

// A round that names the open request already asking a person waits on it
// instead of asking again (WP-914: R-279 asked what R-277 did), and goes on,
// told how it ended, once it is closed.
func TestParkWaitsOnTheRequestItNames(t *testing.T) {
	be := newBabysitEnv(t)
	img := be.openRequest("Rebuild the CI image")
	pr := assessorAsks(t, be, "Will you rebuild the CI image ("+img.Code+") and re-run the check?")
	b := be.babysit()
	if n := be.parkQuestions(); n != 0 {
		t.Fatalf("asked %d questions while %s asks it", n, img.Ref)
	}
	if r := lastRound(b); len(r.WaitsOn) != 1 || r.WaitsOn[0].ID != img.ID || !strings.Contains(b.Wait, img.Ref) || !strings.Contains(b.Wait, "Rebuild the CI image") {
		t.Fatalf("wait = %q, %+v", b.Wait, r.WaitsOn)
	}
	be.step()
	if b := be.babysit(); !parkedOn(b, pr.HeadSHA) {
		t.Fatalf("went on with the request open: %+v", lastRound(b))
	}
	if _, err := be.s.DecideRequest(be.ctx, be.admin, img.Ref, RequestDecision{Decision: "answer", Text: "Rebuilt with Go 1.27.4."}); err != nil {
		t.Fatal(err)
	}
	be.s.Wait()
	be.step()
	b = be.babysit()
	r := b.Rounds[2]
	if parkedOn(b, pr.HeadSHA) || !strings.Contains(r.Reply, img.Ref+` ("Rebuild the CI image"), which it waited on, was answered by`) || !strings.Contains(r.Reply, "Rebuilt with Go 1.27.4.") {
		t.Fatalf("after the request closed: %+v", r)
	}
	if n := len(be.jobs()); n != 4 {
		t.Errorf("jobs = %d, want the assessor's round told the answer", n)
	}
}

// A pull request parked over the same failing check as another joins the
// request that one waits on, instead of asking its own question.
func TestParkJoinsAnotherPullRequestsWait(t *testing.T) {
	be := newBabysitEnv(t)
	img := be.openRequest("Rebuild the CI image")
	other := bsPR
	other.Number, other.HeadRef, other.HeadSHA = 8, "add-import", "8888888bbbb"
	be.pullHook("opened", other)
	be.s.Wait()
	be.drain()
	pr8 := be.pr(8)
	b8, err := be.s.StartBabysit(be.ctx, be.agent, pr8.ID, BabysitInput{Via: "ship"})
	if err != nil {
		t.Fatal(err)
	}
	be.s.Wait()
	b8.Rounds = []BabysitRound{{N: 1, SHA: pr8.HeadSHA, Checks: []string{"ci / test"}, Parked: "why", ParkSHA: pr8.HeadSHA,
		ParkedChecks: []string{"ci / test"}, WaitsOn: []ParkRequest{{ID: img.ID, Ref: img.Ref, Title: img.Title}}}}
	raw, _ := json.Marshal(b8.Rounds)
	if err := be.s.setBabysit(be.ctx, b8, map[string]any{"rounds": string(raw)}); err != nil {
		t.Fatal(err)
	}
	assessorAsks(t, be, "Is the CI image broken?")
	b := be.babysit()
	if n := be.parkQuestions(); n != 0 || len(lastRound(b).WaitsOn) != 1 || lastRound(b).WaitsOn[0].ID != img.ID {
		t.Fatalf("%d questions, waits on %+v", n, lastRound(b).WaitsOn)
	}
}

// A request a round names that is closed already, or another project's
// that isn't, is no wait: the person is asked.
func TestParkAsksWhenTheNamedRequestIsClosed(t *testing.T) {
	be := newBabysitEnv(t)
	img := be.openRequest("Rebuild the CI image")
	if _, err := be.s.DecideRequest(be.ctx, be.admin, img.Ref, RequestDecision{Decision: "dismiss"}); err != nil {
		t.Fatal(err)
	}
	name := "Elsewhere"
	op, err := be.s.CreateProject(be.ctx, be.admin, ProjectInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	foreign, _, err := be.s.CreateRequest(be.ctx, be.agent, RequestInput{Kind: RequestQuestion, Title: "Elsewhere", Project: op.Key, Questions: []Question{{Question: "Elsewhere?"}}})
	if err != nil {
		t.Fatal(err)
	}
	assessorAsks(t, be, "Will you rebuild the CI image ("+img.Code+") or "+foreign.Code+" or NOPE:R-9?")
	if n := be.parkQuestions(); n != 1 || len(lastRound(be.babysit()).WaitsOn) != 0 {
		t.Fatalf("%d questions, waits on %+v", n, lastRound(be.babysit()).WaitsOn)
	}
}
