package core

import (
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// queueKeys lists keys of queue items, in order.
func queueKeys(items []*QueueItem) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Issue.Key)
	}
	return out
}

func TestMergePins(t *testing.T) {
	auto := []int64{1, 2, 3, 4, 5}
	for _, c := range []struct {
		name string
		pins map[int64]pin
		want []int64
	}{
		{"no pins", nil, []int64{1, 2, 3, 4, 5}},
		{"one pin after one automatic task", map[int64]pin{4: {1, 1}}, []int64{1, 4, 2, 3, 5}},
		{"a pin at its own place changes nothing", map[int64]pin{3: {2, 1}}, []int64{1, 2, 3, 4, 5}},
		{"pins keep their places around the rest", map[int64]pin{5: {0, 1}, 2: {2, 4}}, []int64{5, 1, 3, 2, 4}},
		{"past the end is the end", map[int64]pin{1: {99, 1}}, []int64{2, 3, 4, 5, 1}},
		{"pins with the same number go by rank", map[int64]pin{2: {0, 3}, 4: {0, 2}}, []int64{4, 2, 1, 3, 5}},
		{"then by id", map[int64]pin{2: {0, 1}, 4: {0, 1}}, []int64{2, 4, 1, 3, 5}},
		{"pins at the top stay there", map[int64]pin{4: {0, 2}, 5: {0, 3}}, []int64{4, 5, 1, 2, 3}},
	} {
		if got := mergePins(auto, c.pins); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPlacePin(t *testing.T) {
	auto := []int64{1, 2, 3, 4, 5}
	got := placePin(auto, nil, 4, 2)
	if !maps.Equal(got, map[int64]pin{4: {1, 2}}) {
		t.Errorf("one drag pinned %v", got)
	}
	got = placePin(auto, map[int64]pin{4: {1, 2}}, 3, 1)
	if !maps.Equal(got, map[int64]pin{3: {0, 1}, 4: {1, 3}}) {
		t.Errorf("pass: %v", got)
	}
	got = placePin(auto, map[int64]pin{5: {3, 4}}, 1, 1)
	if want := map[int64]pin{1: {0, 1}, 5: {2, 4}}; !maps.Equal(got, want) {
		t.Errorf("ahead: %v, want %v", got, want)
	}
	if order := mergePins(auto, got); !slices.Equal(order, []int64{1, 2, 3, 5, 4}) {
		t.Errorf("ahead order: %v", order)
	}
	got = placePin(auto, map[int64]pin{4: {1, 2}}, 4, 4)
	if len(got) != 0 {
		t.Errorf("back in place: %v", got)
	}
	if got = placePin(auto, nil, 1, 99); !maps.Equal(got, map[int64]pin{1: {4, 5}}) {
		t.Errorf("to the end: %v", got)
	}
	if got = placePin(auto, map[int64]pin{5: {0, 1}}, 3, 0); !maps.Equal(got, map[int64]pin{5: {0, 1}, 3: {2, 4}}) {
		t.Errorf("pin here: %v", got)
	}
}

func TestPinRanks(t *testing.T) {
	all := []float64{1024, 2048, 3072, 4096}
	got, ok := pinRanks([]int64{2, 1}, 0, []float64{4096, 2048}, all)
	if !ok || !maps.Equal(got, map[int64]float64{2: 2048, 1: 4096}) {
		t.Errorf("swap: %v", got)
	}
	all = []float64{1024, 2048, 3072, 4096}
	if got, _ = pinRanks([]int64{1, 9, 2}, 9, []float64{2048, 4096}, all); got[9] != 2560 || got[1] != 2048 || got[2] != 4096 {
		t.Errorf("after: %v", got)
	}
	if got, _ = pinRanks([]int64{9, 1}, 9, []float64{2048}, all); got[9] != 1536 {
		t.Errorf("first: %v", got)
	}
	if got, _ = pinRanks([]int64{9}, 9, nil, all); got[9] != 0 {
		t.Errorf("alone: %v", got)
	}
	if got, _ = pinRanks([]int64{9}, 9, nil, nil); got[9] != 1024 {
		t.Errorf("only: %v", got)
	}
	if got, _ = pinRanks([]int64{1}, 9, []float64{2048}, all); !maps.Equal(got, map[int64]float64{1: 2048}) {
		t.Errorf("let go: %v", got)
	}
	if _, ok = pinRanks([]int64{1, 9}, 9, []float64{1}, []float64{1, 1 + 1e-9}); ok {
		t.Error("found room where there is none")
	}
}

// Pins set in different projects' queues keep each project's order in
// the queue of every project (WP-494): their ranks don't count 1, 2, 3
// in each project, colliding where the projects' queues meet.
func TestQueuePinsAcrossProjects(t *testing.T) {
	e := newEnv(t)
	a := e.project(e.admin, "Alpha", "")
	b := e.project(e.admin, "Beta", "")
	mk := func(p *Project, title string) *IssueDetail {
		return e.issue(e.admin, IssueInput{Project: p.Key, Title: title, Agent: "claude"})
	}
	var as, bs []*IssueDetail
	for n := range 4 {
		as = append(as, mk(a, "a"+strconv.Itoa(n)))
		bs = append(bs, mk(b, "b"+strconv.Itoa(n)))
	}
	move := func(p *Project, d *IssueDetail, pos int) {
		t.Helper()
		if err := e.s.MoveInQueue(e.ctx, e.admin, QueueMove{Key: d.Key, Position: pos, Project: p.Key}); err != nil {
			t.Fatal(err)
		}
	}
	pinnedIn := func(project string) []string {
		t.Helper()
		q, err := e.s.AgentQueueFor(e.ctx, e.admin, "", project)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, it := range q.Next {
			if it.Pinned {
				got = append(got, it.Issue.Key)
			}
		}
		return got
	}
	for n, d := range []*IssueDetail{as[3], as[1], as[2]} {
		move(a, d, n+1)
	}
	for n, d := range []*IssueDetail{bs[2], bs[0], bs[3]} {
		move(b, d, n+1)
	}
	move(a, as[2], 1)
	wantA := []string{as[2].Key, as[3].Key, as[1].Key}
	wantB := []string{bs[2].Key, bs[0].Key, bs[3].Key}
	if got := pinnedIn(a.Key); !slices.Equal(got, wantA) {
		t.Errorf("alpha = %v, want %v", got, wantA)
	}
	if got := pinnedIn(b.Key); !slices.Equal(got, wantB) {
		t.Errorf("beta = %v, want %v", got, wantB)
	}
	want := append(slices.Clone(wantB), wantA...)
	if got := pinnedIn(""); !slices.Equal(got, want) {
		t.Errorf("every project = %v, want %v", got, want)
	}
}

func TestAutoOrder(t *testing.T) {
	const today = "2026-10-05"
	day, _ := time.Parse(dateLayout, today)
	str := func(s string) *string { return &s }
	pts := func(f float64) *float64 { return &f }
	mk := func(id int64, f func(*queuedIssue)) *queuedIssue {
		qi := &queuedIssue{issue: &Issue{ID: id, Agent: "claude", Type: "task", Rank: float64(id)}}
		f(qi)
		return qi
	}
	items := []*queuedIssue{
		mk(1, func(qi *queuedIssue) {}),
		mk(2, func(qi *queuedIssue) { qi.issue.Points = pts(1) }),
		mk(3, func(qi *queuedIssue) { qi.issue.Points = pts(8) }),
		mk(4, func(qi *queuedIssue) { qi.issue.Type = "bug" }),
		mk(5, func(qi *queuedIssue) { qi.issue.Category = "bugfix" }),
		mk(6, func(qi *queuedIssue) { qi.issue.DueDate = str("2026-10-20") }),
		mk(7, func(qi *queuedIssue) { qi.issue.DueDate = str("2026-10-08") }),
		mk(8, func(qi *queuedIssue) { qi.issue.Priority = 3 }),
		mk(9, func(qi *queuedIssue) { qi.issue.Priority = 4 }),
		mk(10, func(qi *queuedIssue) { qi.issue.DueDate = str("2026-10-01") }),
		mk(11, func(qi *queuedIssue) { qi.issue.Agent = ""; qi.issue.Priority = 4 }),
		mk(12, func(qi *queuedIssue) { qi.planned = true }),
		mk(13, func(qi *queuedIssue) { qi.issue.Priority = 1; qi.unblocks, qi.blockedUrgency = 1, urgency(4, nil, day) }),
		mk(14, func(qi *queuedIssue) { qi.issue.Type = "bug"; qi.issue.TextAgent, qi.issue.Unreviewed = "claude", true }),
		mk(15, func(qi *queuedIssue) { qi.issue.CreatedAt = day.AddDate(0, 0, -30) }),
	}
	for _, qi := range items {
		qi.score = queueScore(qi, day, day)
	}
	slices.SortStableFunc(items, func(a, b *queuedIssue) int { return autoOrder(a, b, today, false) })
	got := []int64{}
	for _, qi := range items {
		got = append(got, qi.issue.ID)
	}
	if want := []int64{13, 9, 10, 11, 8, 7, 4, 5, 15, 12, 2, 6, 1, 3, 14}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	slices.SortStableFunc(items, func(a, b *queuedIssue) int { return autoOrder(a, b, today, true) })
	if items[0].issue.ID != 10 {
		t.Errorf("custom order starts with %d, want the overdue 10", items[0].issue.ID)
	}
}

// TestUrgency: a due date counts more as it nears, and more again once
// it has passed.
func TestUrgency(t *testing.T) {
	day, _ := time.Parse(dateLayout, "2026-10-05")
	due := func(d string) float64 { return urgency(0, &d, day) }
	if !(due("2026-10-30") < due("2026-10-12") && due("2026-10-12") < due("2026-10-06") && due("2026-10-06") < due("2026-10-05") &&
		due("2026-10-05") < due("2026-10-04") && due("2026-10-04") < due("2026-09-20")) {
		t.Error("urgency doesn't grow as the due date nears")
	}
	if due("2026-01-01") != due("2025-01-01") {
		t.Error("overdue urgency has no ceiling")
	}
	urgent := urgency(4, nil, day)
	if hi := "2026-10-09"; urgency(3, &hi, day) >= urgent {
		t.Error("high priority due in four days passed urgent")
	}
	if hi := "2026-10-06"; urgency(3, &hi, day) <= urgent {
		t.Error("high priority due tomorrow stayed behind urgent")
	}
}

func TestAgentQueueOrderAndPins(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Queue", "")
	mk := func(title, agent, prio, due string) *IssueDetail {
		return e.issue(e.admin, IssueInput{Project: p.Key, Title: title, Agent: agent, Priority: prio, DueDate: due})
	}
	low := mk("Low", "claude", "low", "")
	urgent := mk("Urgent", "claude", "urgent", "")
	late := mk("Late", "claude", "low", "2026-10-01")
	high := mk("High, due soon", "claude", "high", "2026-10-09")
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	opus, _ := e.s.TokenActor(e.ctx, tok)
	const view = "anthropic/claude-opus-5-5"
	queue := func() *AgentQueue {
		t.Helper()
		q, err := e.s.AgentQueueFor(e.ctx, e.admin, view, "")
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	order := func(want ...string) *AgentQueue {
		t.Helper()
		q := queue()
		if got := queueKeys(q.Next); !slices.Equal(got, want) {
			t.Fatalf("order = %v, want %v", got, want)
		}
		return q
	}
	pinned := func(keys ...string) {
		t.Helper()
		got := []string{}
		for _, it := range queue().Next {
			if it.Pinned {
				got = append(got, it.Issue.Key)
			}
		}
		slices.Sort(got)
		slices.Sort(keys)
		if !slices.Equal(got, keys) {
			t.Fatalf("pinned = %v, want %v", got, keys)
		}
	}
	move := func(m QueueMove) {
		t.Helper()
		m.Agent = view
		if err := e.s.MoveInQueue(e.ctx, e.admin, m); err != nil {
			t.Fatal(err)
		}
	}

	q := order(late.Key, urgent.Key, high.Key, low.Key)
	if !q.Exact || q.Next[0].Why != "Overdue (due Oct 1) · Low priority" || q.Next[2].Why != "High priority · due Oct 9" || q.Next[0].Position != 1 {
		t.Errorf("why: %q, %q (exact %v)", q.Next[0].Why, q.Next[2].Why, q.Exact)
	}

	move(QueueMove{Key: low.Key, Top: true})
	q = order(low.Key, late.Key, urgent.Key, high.Key)
	if !q.Next[0].Pinned || q.Next[0].Why != "Pinned at #1 by robin" || q.Next[1].Why != "Overdue (due Oct 1) · Low priority" {
		t.Errorf("pins: %+v / %+v", q.Next[0], q.Next[1])
	}
	pinned(low.Key)
	move(QueueMove{Key: high.Key, Position: 3})
	order(low.Key, late.Key, high.Key, urgent.Key)
	pinned(low.Key, high.Key)

	newer := mk("Newer", "claude", "urgent", "2026-10-02")
	order(low.Key, newer.Key, high.Key, late.Key, urgent.Key)
	pinned(low.Key, high.Key)

	move(QueueMove{Key: low.Key, Position: 4})
	order(newer.Key, high.Key, late.Key, low.Key, urgent.Key)
	if d := e.get(e.admin, high.Key); d.AgentOrder == nil || d.AgentAhead != 1 {
		t.Errorf("high after %d tasks (%v), want 1", d.AgentAhead, d.AgentOrder)
	}
	move(QueueMove{Key: low.Key, Position: 5})
	order(newer.Key, high.Key, late.Key, urgent.Key, low.Key)
	pinned(high.Key)
	move(QueueMove{Key: urgent.Key})
	pinned(high.Key, urgent.Key)
	if d := e.get(e.admin, urgent.Key); d.AgentOrder == nil || d.AgentAhead != 2 {
		t.Errorf("urgent pinned after %d tasks (%v), want 2", d.AgentAhead, d.AgentOrder)
	}
	move(QueueMove{Key: urgent.Key, Unpin: true})
	move(QueueMove{Key: high.Key, Unpin: true})
	order(newer.Key, late.Key, urgent.Key, high.Key, low.Key)
	pinned()

	move(QueueMove{Key: high.Key, Position: 2})
	move(QueueMove{Key: late.Key, Top: true})
	task, err := e.s.AgentNext(e.ctx, opus, AgentQuery{})
	if err != nil || task == nil || task.Issue.Key != late.Key || task.Why != "Pinned at #1 by robin" {
		t.Fatalf("next = %+v, %v", task, err)
	}
	if _, err := e.s.AgentDone(e.ctx, opus, late.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	if d := e.get(e.admin, late.Key); d.AgentOrder != nil {
		t.Error("a closed issue kept its pin")
	}
	order(newer.Key, high.Key, urgent.Key, low.Key)
	none := "none"
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, high.Key, IssuePatch{Agent: &none}); err != nil {
		t.Fatal(err)
	}
	if d := e.get(e.admin, high.Key); d.AgentOrder != nil {
		t.Error("an issue taken off agents kept its pin")
	}
	if err := e.s.MoveInQueue(e.ctx, e.admin, QueueMove{Key: high.Key, Top: true}); err == nil {
		t.Error("pinned an issue that isn't delegated")
	}
	if err := e.s.MoveInQueue(e.ctx, e.admin, QueueMove{Key: low.Key, Position: -1}); err == nil {
		t.Error("placed an issue at a negative place")
	}
	if err := e.s.MoveInQueue(e.ctx, opus, QueueMove{Key: low.Key, Top: true}); err == nil {
		t.Error("an agent reordered the queue")
	}
}

func TestAgentQueueWaitingAndScope(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Waits", "")
	last := ""
	mk := func(in IssueInput) *IssueDetail {
		in.Project, in.After = p.Key, last
		d := e.issue(e.admin, in)
		last = d.Key
		return d
	}
	ready := mk(IssueInput{Title: "Ready", Agent: "claude"})
	backlog := mk(IssueInput{Title: "Someday", Agent: "claude", Status: "backlog"})
	blocked := mk(IssueInput{Title: "Blocked", Agent: "any"})
	blocker := mk(IssueInput{Title: "Blocker", Agent: "none"})
	if err := e.s.AddRelation(e.ctx, e.admin, blocker.Key, "blocks", blocked.Key); err != nil {
		t.Fatal(err)
	}
	haikuOnly := mk(IssueInput{Title: "For haiku", Agent: "haiku"})
	mk(IssueInput{Title: "Humans only", Agent: "none"})
	tokO, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	tokH, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "haiku", Agent: "anthropic/claude-haiku-5-5"})
	opus, _ := e.s.TokenActor(e.ctx, tokO)
	haiku, _ := e.s.TokenActor(e.ctx, tokH)

	q, err := e.s.AgentQueueFor(e.ctx, e.admin, "anthropic/claude-opus-5-5", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := queueKeys(q.Next); !slices.Equal(got, []string{ready.Key}) {
		t.Errorf("opus next = %v", got)
	}
	reasons := map[string]string{}
	for _, it := range q.Waiting {
		reasons[it.Issue.Key] = it.Reason
	}
	if reasons[backlog.Key] != WaitBacklog || reasons[blocked.Key] != WaitBlocked || len(reasons) != 2 {
		t.Errorf("waiting = %v", reasons)
	}
	for _, it := range q.Waiting {
		if it.Reason == WaitBlocked && (len(it.Blockers) != 1 || it.Blockers[0] != blocker.Key) {
			t.Errorf("blockers = %v", it.Blockers)
		}
	}
	if q.Next[0].DelegatedBy == nil || q.Next[0].DelegatedBy.Username != "robin" {
		t.Errorf("delegated by = %+v", q.Next[0].DelegatedBy)
	}

	fam, _ := e.s.AgentQueueFor(e.ctx, e.admin, "claude", "")
	if fam.Exact || len(fam.Covers) != 2 {
		t.Fatalf("family covers %v (exact %v)", fam.Covers, fam.Exact)
	}
	if got := queueKeys(fam.Next); !slices.Equal(got, []string{ready.Key, haikuOnly.Key}) {
		t.Errorf("family next = %v", got)
	}
	for _, it := range fam.Next {
		if it.Issue.Key == haikuOnly.Key && !slices.Equal(it.Takers, []string{"anthropic/claude-haiku-5-5"}) {
			t.Errorf("takers = %v", it.Takers)
		}
	}
	all, _ := e.s.AgentQueueFor(e.ctx, e.admin, "", "")
	if len(all.Next) != 2 || len(all.Waiting) != 2 {
		t.Errorf("all = %v / %v", queueKeys(all.Next), queueKeys(all.Waiting))
	}

	if task, _ := e.s.AgentNext(e.ctx, haiku, AgentQuery{}); task == nil || task.Issue.Key != ready.Key {
		t.Fatalf("haiku next = %+v", task)
	}
	q, _ = e.s.AgentQueueFor(e.ctx, e.admin, "anthropic/claude-opus-5-5", "")
	found := false
	for _, it := range q.Waiting {
		if it.Issue.Key == ready.Key && it.Reason == WaitClaimed && strings.Contains(it.Detail, "claude-haiku") {
			found = true
		}
	}
	if !found {
		t.Errorf("claimed by another agent not waiting: %+v", queueKeys(q.Waiting))
	}
	hq, _ := e.s.AgentQueueFor(e.ctx, e.admin, "anthropic/claude-haiku-5-5", "")
	if got := queueKeys(hq.Working); !slices.Equal(got, []string{ready.Key}) {
		t.Errorf("haiku working = %v", got)
	}

	if err := e.s.ReleaseClaim(e.ctx, haiku, ready.Key); err == nil {
		t.Error("an agent used the person's release")
	}
	if err := e.s.ReleaseClaim(e.ctx, e.admin, ready.Key); err != nil {
		t.Fatal(err)
	}
	d := e.get(e.admin, ready.Key)
	if d.ClaimedBy != "" || len(d.AgentRuns) != 1 || d.AgentRuns[0].Outcome != "released" {
		t.Errorf("after release: %q %+v", d.ClaimedBy, d.AgentRuns)
	}

	yes := true
	if _, err := e.s.UpdateProject(e.ctx, e.admin, p.Key, ProjectInput{Settings: &ProjectSettings{AgentsPaused: yes}}); err != nil {
		t.Fatal(err)
	}
	if task, err := e.s.AgentNext(e.ctx, opus, AgentQuery{}); err != nil || task != nil {
		t.Errorf("next in a paused project = %+v, %v", task, err)
	}
	q, _ = e.s.AgentQueueFor(e.ctx, e.admin, "anthropic/claude-opus-5-5", "")
	if len(q.Next) != 0 || len(q.PausedProjects) != 1 || q.Waiting[0].Reason != WaitProjectPaused {
		t.Errorf("paused project queue: next %v, paused %v", queueKeys(q.Next), q.PausedProjects)
	}
}

func TestAgentPauses(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Pauses", "")
	first := e.issue(e.admin, IssueInput{Project: p.Key, Title: "First", Agent: "any", Priority: "high"})
	second := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Second", Agent: "any"})
	tokO, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	tokG, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "gpt", Agent: "openai/gpt-6"})
	opus, _ := e.s.TokenActor(e.ctx, tokO)
	gpt, _ := e.s.TokenActor(e.ctx, tokG)

	if task, _ := e.s.AgentNext(e.ctx, opus, AgentQuery{}); task == nil || task.Issue.Key != first.Key {
		t.Fatalf("next = %+v", task)
	}
	if _, err := e.s.PauseAgents(e.ctx, opus, "claude", "", false); err == nil {
		t.Error("an agent paused agents")
	}
	if _, err := e.s.PauseAgents(e.ctx, e.admin, "claude", "reviewing what it did", false); err != nil {
		t.Fatal(err)
	}
	if task, err := e.s.AgentNext(e.ctx, opus, AgentQuery{}); err != nil || task == nil || task.Issue.Key != first.Key {
		t.Errorf("held task while paused = %+v, %v", task, err)
	}
	if _, err := e.s.AgentDone(e.ctx, opus, first.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	_, err := e.s.AgentNext(e.ctx, opus, AgentQuery{})
	var paused *ErrAgentsPaused
	if !errors.As(err, &paused) || !strings.Contains(err.Error(), "reviewing what it did") {
		t.Fatalf("paused next = %v", err)
	}
	if task, _ := e.s.AgentNext(e.ctx, gpt, AgentQuery{}); task == nil || task.Issue.Key != second.Key {
		t.Errorf("gpt next = %+v", task)
	}
	q, _ := e.s.AgentQueueFor(e.ctx, e.admin, "anthropic/claude-opus-5-5", "")
	if len(q.Pauses) != 1 || q.Pauses[0].Spec != "claude" || q.Pauses[0].By == nil {
		t.Errorf("queue pauses = %+v", q.Pauses)
	}
	if n, err := e.s.ResumeAgents(e.ctx, e.admin, "claude"); err != nil || n != 1 {
		t.Fatalf("resume = %d, %v", n, err)
	}
	if _, err := e.s.ResumeAgents(e.ctx, e.admin, "claude"); err == nil {
		t.Error("resumed a pause that isn't there")
	}
	if _, err := e.s.AgentNext(e.ctx, opus, AgentQuery{}); err != nil {
		t.Errorf("next after resume: %v", err)
	}
	var n int
	e.s.db.R.QueryRow(`SELECT count(*) FROM audit_log WHERE action IN ('agent.pause','agent.resume')`).Scan(&n)
	if n != 2 {
		t.Errorf("audit entries = %d", n)
	}
}

// A parent with open sub-issues handed to agents waits for them: they are
// the same work, and listing both put it in the queue twice (WP-494). A
// template's checklist sub-tasks (Anyone's) are the parent's own steps.
func TestQueueParentWaitsForSubIssues(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Family", "")
	epic := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Epic", Type: "epic", Agent: "claude"})
	child := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Slice", Parent: epic.Key, Agent: "claude"})
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "Done slice", Parent: epic.Key, Agent: "claude", Status: "done"})
	story := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Story", Agent: "claude"})
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "Checklist step", Parent: story.Key})

	q, err := e.s.AgentQueueFor(e.ctx, e.admin, "claude", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := queueKeys(q.Next); !slices.Contains(got, child.Key) || !slices.Contains(got, story.Key) || slices.Contains(got, epic.Key) {
		t.Errorf("next = %v", got)
	}
	var wait *QueueItem
	for _, it := range q.Waiting {
		if it.Issue.Key == epic.Key {
			wait = it
		}
	}
	if wait == nil || wait.Reason != WaitSubtasks || !strings.Contains(wait.Detail, "1 of 2 open") {
		t.Fatalf("epic waits: %+v", wait)
	}

	none := AgentHumans
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, child.Key, IssuePatch{Agent: &none}); err != nil {
		t.Fatal(err)
	}
	q, _ = e.s.AgentQueueFor(e.ctx, e.admin, "claude", "")
	if got := queueKeys(q.Next); !slices.Contains(got, epic.Key) {
		t.Errorf("after its sub-issues: next = %v", got)
	}
}
