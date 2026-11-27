package core

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/db"
)

type env struct {
	t       *testing.T
	s       *Service
	ctx     context.Context
	admin   *Actor
	clockMu sync.Mutex
	clock   time.Time
}

// clockNow is the test's time, safe to call from any goroutine.
func (e *env) clockNow() time.Time {
	e.clockMu.Lock()
	defer e.clockMu.Unlock()
	return e.clock
}

// setClock moves the test's time to t.
func (e *env) setClock(t time.Time) {
	e.clockMu.Lock()
	defer e.clockMu.Unlock()
	e.clock = t
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d, err := db.OpenFresh(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	e := envOn(t, d, time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	e.admin = e.user("robin", true)
	return e
}

// envOn is a Service on d with the test's clock at clock, and no admin
// yet: what newEnv and an env started from a snapshot have in common.
func envOn(t *testing.T, d *db.DB, clock time.Time) *env {
	t.Helper()
	t.Cleanup(func() { d.Close() })
	s := New(d, Config{BaseURL: "https://wp.test"}, nil)
	s.StartHub()
	e := &env{t: t, s: s, ctx: context.Background(), clock: clock}
	s.SetClock(e.clockNow)
	t.Cleanup(s.Wait)
	return e
}

func (e *env) user(name string, admin bool) *Actor {
	e.t.Helper()
	u, err := e.s.CreateUser(e.ctx, SystemActor(), CreateUserInput{Username: name, Email: name + "@example.com", Password: "correct-horse-battery", IsAdmin: admin})
	if err != nil {
		e.t.Fatal(err)
	}
	return &Actor{User: u}
}

func (e *env) project(a *Actor, name string, mode string) *Project {
	e.t.Helper()
	p, err := e.s.CreateProject(e.ctx, a, ProjectInput{Name: &name, Settings: &ProjectSettings{Mode: mode}})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) issue(a *Actor, in IssueInput) *IssueDetail {
	e.t.Helper()
	i, err := e.s.CreateIssue(e.ctx, a, in)
	if err != nil {
		e.t.Fatalf("create issue %q: %v", in.Title, err)
	}
	return i
}

func (e *env) get(a *Actor, ref string) *IssueDetail {
	e.t.Helper()
	i, err := e.s.GetIssue(e.ctx, a, ref)
	if err != nil {
		e.t.Fatalf("get %s: %v", ref, err)
	}
	return i
}

func (e *env) search(a *Actor, q string) []string {
	e.t.Helper()
	res, err := e.s.Search(e.ctx, a, SearchParams{Query: q, Limit: 100})
	if err != nil {
		e.t.Fatalf("search %q: %v", q, err)
	}
	var keys []string
	for _, i := range res.Issues {
		keys = append(keys, i.Key)
	}
	return keys
}

func str(s string) *string { return &s }

func TestProjectDefaults(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Waypoint", "")
	if p.Key != "WAYP" {
		t.Errorf("key = %s", p.Key)
	}
	if len(p.Statuses) != 6 || p.Settings.Mode != "kanban" {
		t.Errorf("statuses = %d, mode = %s", len(p.Statuses), p.Settings.Mode)
	}
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "first"})
	if i.Status.Name != "To Do" || i.Key != "WAYP-1" {
		t.Errorf("issue = %s %s", i.Key, i.Status.Name)
	}
	p2 := e.project(e.admin, "Waypoint", "sprints")
	if p2.Key != "WAYP2" {
		t.Errorf("second key = %s", p2.Key)
	}
	i2 := e.issue(e.admin, IssueInput{Project: p2.Key, Title: "x"})
	if i2.Status.Name != "Backlog" {
		t.Errorf("sprint-mode default status = %s", i2.Status.Name)
	}
	autos, _ := e.s.ListAutomations(e.ctx, e.admin, p.Key)
	if len(autos) != 7 {
		t.Errorf("default automations = %d", len(autos))
	}
	if SuggestKey("Home Renovation") != "HR" || SuggestKey("x") != "XPR" {
		t.Errorf("suggest: %s %s", SuggestKey("Home Renovation"), SuggestKey("x"))
	}
}

func TestStatusResolvedAndParentAutoClose(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Home", "")
	epic := e.issue(e.admin, IssueInput{Project: p.Key, Type: "epic", Title: "Kitchen"})
	a := e.issue(e.admin, IssueInput{Parent: epic.Key, Title: "Tiles"})
	b := e.issue(e.admin, IssueInput{Parent: epic.Key, Title: "Paint"})
	if a.Parent == nil || a.Parent.Key != epic.Key {
		t.Fatalf("parent not set: %+v", a.Parent)
	}
	if _, err := e.s.Transition(e.ctx, e.admin, a.Key, "done", ""); err != nil {
		t.Fatal(err)
	}
	if got := e.get(e.admin, a.Key); got.ResolvedAt == nil || got.Status.Category != CatDone {
		t.Errorf("a not resolved: %+v", got.Status)
	}
	if got := e.get(e.admin, epic.Key); got.Closed() {
		t.Fatal("epic closed with an open child")
	}
	if _, err := e.s.Transition(e.ctx, e.admin, b.Key, "Won't Do", ""); err != nil {
		t.Fatal(err)
	}
	got := e.get(e.admin, epic.Key)
	if got.Status.Name != "Done" {
		t.Fatalf("epic should auto-close, status = %s", got.Status.Name)
	}
	if got.ChildCount != 2 || got.ChildDone != 2 {
		t.Errorf("child counts %d/%d", got.ChildDone, got.ChildCount)
	}
	h, _ := e.s.History(e.ctx, e.admin, epic.Key, 10)
	if len(h) == 0 || h[0].ViaAutomation == "" {
		t.Errorf("expected automation in history: %+v", h)
	}
	if _, err := e.s.Transition(e.ctx, e.admin, a.Key, "To Do", ""); err != nil {
		t.Fatal(err)
	}
	if got := e.get(e.admin, epic.Key); got.Status.Category != CatInProgress || got.ResolvedAt != nil {
		t.Errorf("epic should reopen, got %s", got.Status.Name)
	}
}

func TestHierarchyRules(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Tree", "")
	root := e.issue(e.admin, IssueInput{Project: p.Key, Title: "root"})
	child := e.issue(e.admin, IssueInput{Parent: root.Key, Title: "child"})
	grand := e.issue(e.admin, IssueInput{Parent: child.Key, Title: "grand"})
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, root.Key, IssuePatch{Parent: Some(grand.Key)}); !IsInvalid(err) {
		t.Errorf("cycle allowed: %v", err)
	}
	if got := e.search(e.admin, "under:"+root.Key+" order:key"); strings.Join(got, ",") != child.Key+","+grand.Key {
		t.Errorf("under = %v", got)
	}
	sp, err := e.s.CreateSprint(e.ctx, e.admin, p.Key, SprintInput{})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := e.s.BulkUpdate(e.ctx, e.admin, []string{root.Key}, IssuePatch{Sprint: Some(sp.Name)}, true)
	if err != nil || len(keys) != 3 {
		t.Fatalf("bulk = %v %v", keys, err)
	}
	if g := e.get(e.admin, grand.Key); g.SprintID == nil || g.Parent.Key != child.Key {
		t.Errorf("grandchild sprint/parent wrong: %+v", g.Issue)
	}
	if err := e.s.DeleteIssue(e.ctx, e.admin, child.Key, false); err != nil {
		t.Fatal(err)
	}
	if g := e.get(e.admin, grand.Key); g.Parent == nil || g.Parent.Key != root.Key {
		t.Errorf("grandchild should move up: %+v", g.Parent)
	}
	p2 := e.project(e.admin, "Other", "")
	moved, err := e.s.MoveToProject(e.ctx, e.admin, root.Key, p2.Key)
	if err != nil {
		t.Fatal(err)
	}
	if moved.ProjectKey != p2.Key || len(moved.Children) != 1 || !strings.HasPrefix(moved.Children[0].Key, p2.Key+"-") {
		t.Errorf("move: %s children %+v", moved.Key, moved.Children)
	}
	if old := e.get(e.admin, root.Key); old.ID != root.ID {
		t.Errorf("old key should resolve")
	}
}

func TestSharingAndVisibility(t *testing.T) {
	e := newEnv(t)
	partner := e.user("sam", false)
	friend := e.user("alex", false)
	p := e.project(e.admin, "House", "")
	trip := e.issue(e.admin, IssueInput{Project: p.Key, Type: "epic", Title: "Plan trip"})
	flights := e.issue(e.admin, IssueInput{Parent: trip.Key, Title: "Book flights"})
	secret := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Birthday surprise"})

	if _, err := e.s.GetIssue(e.ctx, partner, secret.Key); err == nil {
		t.Fatal("non-member can see issue")
	}
	if err := e.s.ShareIssue(e.ctx, e.admin, trip.Key, "sam", "editor"); err != nil {
		t.Fatal(err)
	}
	if got := e.search(partner, "order:key"); strings.Join(got, ",") != trip.Key+","+flights.Key {
		t.Errorf("partner sees %v", got)
	}
	if _, err := e.s.GetIssue(e.ctx, partner, secret.Key); err == nil {
		t.Error("share leaked a sibling")
	}
	if _, err := e.s.Transition(e.ctx, partner, flights.Key, "In Progress", ""); err != nil {
		t.Errorf("partner edit: %v", err)
	}
	if _, err := e.s.CreateIssue(e.ctx, partner, IssueInput{Parent: trip.Key, Title: "Hotel"}); err != nil {
		t.Errorf("partner subtask: %v", err)
	}
	if _, err := e.s.CreateIssue(e.ctx, partner, IssueInput{Project: p.Key, Title: "nope"}); err == nil {
		t.Error("partner created a top-level issue")
	}
	projects, _ := e.s.ListProjects(e.ctx, partner, false)
	if len(projects) != 1 || projects[0].Role != "guest" {
		t.Errorf("guest projects = %+v", projects)
	}
	if err := e.s.ShareIssue(e.ctx, partner, trip.Key, "alex", "viewer"); err == nil {
		t.Error("guest re-shared")
	}
	if err := e.s.ShareIssue(e.ctx, e.admin, flights.Key, "alex", "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AddComment(e.ctx, friend, flights.Key, "window seat please"); err != nil {
		t.Errorf("viewer comment: %v", err)
	}
	if _, err := e.s.Transition(e.ctx, friend, flights.Key, "done", ""); err == nil {
		t.Error("viewer edited")
	}
}

func TestSearchQueries(t *testing.T) {
	e := newEnv(t)
	sam := e.user("sam", false)
	p := e.project(e.admin, "Search", "")
	e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "editor")
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Login page crashes", Type: "bug", Priority: "urgent", Assignees: []string{"me"}, Labels: []string{"frontend"}})
	b := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Refactor auth", Priority: "low", Assignees: []string{"sam"}, DueDate: "2026-10-01"})
	c := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Write docs", Description: "explain the login flow", Points: ptrF(3)})
	e.s.Transition(e.ctx, e.admin, c.Key, "done", "")

	cases := map[string][]string{
		"assignee:me":                {a.Key},
		"assignee:sam":               {b.Key},
		"assignee:none":              {c.Key},
		"is:open order:key":          {a.Key, b.Key},
		"is:done":                    {c.Key},
		"priority:>=high":            {a.Key},
		"type:bug,task order:key":    {a.Key, b.Key, c.Key},
		"-type:bug order:key":        {b.Key, c.Key},
		"label:frontend":             {a.Key},
		"login order:key":            {a.Key, c.Key},
		"log":                        {a.Key, c.Key},
		"is:overdue":                 {b.Key},
		"has:points":                 {c.Key},
		"points:>2":                  {c.Key},
		a.Key:                        {a.Key},
		"order:-priority is:open":    {a.Key, b.Key},
		`status:"To Do" order:key`:   {a.Key, b.Key},
		"category:done":              {c.Key},
		"created:>=-1d order:key":    {a.Key, b.Key, c.Key},
		"updated:<-1d":               nil,
		"due:<today":                 {b.Key},
		"is:mine":                    {a.Key},
		"project:" + p.Key + " auth": {b.Key},
	}
	for q, want := range cases {
		got := e.search(e.admin, q)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%q = %v, want %v", q, got, want)
		}
	}
	if got := e.search(sam, "is:mine"); len(got) != 1 || got[0] != b.Key {
		t.Errorf("sam is:mine = %v", got)
	}
	if _, err := e.s.Search(e.ctx, e.admin, SearchParams{Query: "bogus:x"}); !IsInvalid(err) {
		t.Errorf("unknown key should be invalid: %v", err)
	}
}

func ptrF(f float64) *float64 { return &f }

func TestSprintsAndBoard(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Sprinty", "sprints")
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "a", Points: ptrF(3)})
	b := e.issue(e.admin, IssueInput{Project: p.Key, Title: "b", Points: ptrF(5)})
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "c"})
	sp, err := e.s.CreateSprint(e.ctx, e.admin, p.Key, SprintInput{})
	if err != nil {
		t.Fatal(err)
	}
	if *sp.StartDate != "2026-10-05" || *sp.EndDate != "2026-10-18" {
		t.Errorf("sprint dates %s..%s", *sp.StartDate, *sp.EndDate)
	}
	e.s.BulkUpdate(e.ctx, e.admin, []string{a.Key, b.Key}, IssuePatch{Sprint: Some("next")}, false)
	if got := e.get(e.admin, a.Key); got.Status.Name != "To Do" {
		t.Errorf("status after sprint add = %s", got.Status.Name)
	}
	bl, err := e.s.GetBacklog(e.ctx, e.admin, p.Key, "")
	if err != nil || len(bl.Sprints) != 1 || len(bl.Sprints[0].Issues) != 2 || len(bl.Backlog) != 1 {
		t.Fatalf("backlog: %+v %v", bl, err)
	}
	board, _ := e.s.GetBoard(e.ctx, e.admin, p.Key, "")
	if board.Sprint != nil {
		t.Error("board shows a sprint before it starts")
	}
	if sp, err = e.s.StartSprint(e.ctx, e.admin, sp.ID); err != nil || *sp.CommittedPoints != 8 {
		t.Fatalf("start: %v %+v", err, sp)
	}
	board, _ = e.s.GetBoard(e.ctx, e.admin, p.Key, "")
	n := 0
	for _, c := range board.Columns {
		n += len(c.Issues)
	}
	if n != 2 || board.Columns[0].Status.Name != "To Do" {
		t.Errorf("board has %d issues, first col %s", n, board.Columns[0].Status.Name)
	}
	e.setClock(e.clockNow().Add(48 * time.Hour))
	e.s.Transition(e.ctx, e.admin, a.Key, "done", "")
	bd, err := e.s.SprintBurndown(e.ctx, e.admin, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bd.Days) != 14 || *bd.Days[0].RemainingPoints != 8 || *bd.Days[2].RemainingPoints != 5 || bd.Days[5].RemainingPoints != nil {
		t.Errorf("burndown: %+v", bd.Days[:6])
	}
	closed, err := e.s.CloseSprint(e.ctx, e.admin, sp.ID, "next")
	if err != nil || *closed.CompletedPoints != 3 {
		t.Fatalf("close: %v %+v", err, closed)
	}
	if got := e.get(e.admin, b.Key); got.SprintID == nil || *got.SprintID == sp.ID {
		t.Errorf("b not carried over: %v", got.SprintID)
	}
	v, _ := e.s.Velocity(e.ctx, e.admin, p.Key, 0)
	if len(v) != 1 || v[0].Committed != 8 || v[0].Completed != 3 {
		t.Errorf("velocity = %+v", v)
	}
}

func TestKanbanBoardHidesOldDone(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Kan", "")
	old := e.issue(e.admin, IssueInput{Project: p.Key, Title: "old"})
	e.s.Transition(e.ctx, e.admin, old.Key, "done", "")
	e.setClock(e.clockNow().AddDate(0, 0, 20))
	recent := e.issue(e.admin, IssueInput{Project: p.Key, Title: "recent"})
	e.s.Transition(e.ctx, e.admin, recent.Key, "done", "")
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "later", Status: "backlog"})
	board, _ := e.s.GetBoard(e.ctx, e.admin, p.Key, "")
	var all []string
	for _, c := range board.Columns {
		for _, i := range c.Issues {
			all = append(all, i.Title)
		}
	}
	if strings.Join(all, ",") != "recent" {
		t.Errorf("board = %v", all)
	}
	bl, _ := e.s.GetBacklog(e.ctx, e.admin, p.Key, "")
	if len(bl.Backlog) != 1 || bl.Backlog[0].Title != "later" {
		t.Errorf("kanban backlog = %+v", bl.Backlog)
	}
}

func TestRanking(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Rank", "")
	b := e.issue(e.admin, IssueInput{Project: p.Key, Title: "b"})
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "a"})
	agent := &Actor{User: e.admin.User, Agent: "claude"}
	c := e.issue(agent, IssueInput{Project: p.Key, Title: "c"})
	if got := e.search(e.admin, "order:rank"); strings.Join(got, ",") != strings.Join([]string{a.Key, b.Key, c.Key}, ",") {
		t.Errorf("as created = %v", got)
	}
	e.s.UpdateIssue(e.ctx, e.admin, c.Key, IssuePatch{Before: a.Key})
	if got := e.search(e.admin, "order:rank"); strings.Join(got, ",") != strings.Join([]string{c.Key, a.Key, b.Key}, ",") {
		t.Errorf("after before = %v", got)
	}
	for n := 0; n < 60; n++ {
		if _, err := e.s.UpdateIssue(e.ctx, e.admin, b.Key, IssuePatch{After: c.Key}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.UpdateIssue(e.ctx, e.admin, a.Key, IssuePatch{After: c.Key}); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.search(e.admin, "order:rank"); strings.Join(got, ",") != strings.Join([]string{c.Key, a.Key, b.Key}, ",") {
		t.Errorf("after many moves = %v", got)
	}
	top := c.Key
	var added []string
	for n := 0; n < 70; n++ {
		d := e.issue(e.admin, IssueInput{Project: p.Key, Title: "new " + strconv.Itoa(n), Before: top})
		added = append(added, d.Key)
	}
	got := e.search(e.admin, "order:rank")
	if len(got) != 73 || got[70] != c.Key {
		t.Fatalf("after many adds = %v", got)
	}
	for n := range added {
		if got[n] != added[n] {
			t.Fatalf("after many adds, #%d = %s, want %s", n, got[n], added[n])
		}
	}
}

func TestTimeTracking(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Time", "")
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "a"})
	b := e.issue(e.admin, IssueInput{Project: p.Key, Title: "b"})
	if _, err := e.s.StartTimer(e.ctx, e.admin, a.Key, ""); err != nil {
		t.Fatal(err)
	}
	e.setClock(e.clockNow().Add(30 * time.Minute))
	if _, err := e.s.StartTimer(e.ctx, e.admin, b.Key, ""); err != nil {
		t.Fatal(err)
	}
	e.setClock(e.clockNow().Add(15 * time.Minute))
	run, _ := e.s.RunningTimer(e.ctx, e.admin)
	if run == nil || run.Issue.Key != b.Key || run.Seconds != 900 {
		t.Fatalf("running = %+v", run)
	}
	stopped, _ := e.s.StopTimer(e.ctx, e.admin)
	if stopped.Running || stopped.Seconds != 900 {
		t.Errorf("stopped = %+v", stopped)
	}
	if _, err := e.s.CreateTimeEntry(e.ctx, e.admin, TimeEntryInput{Issue: &a.Key, Duration: str("1h15m")}); err != nil {
		t.Fatal(err)
	}
	if got := e.get(e.admin, a.Key).TimeSpent; got != 30*60+75*60 {
		t.Errorf("a time = %d", got)
	}
	rep, err := e.s.GetTimeReport(e.ctx, e.admin, e.clockNow().Add(-24*time.Hour), e.clockNow().Add(time.Hour), "")
	if err != nil || rep.TotalSeconds != 30*60+15*60+75*60 {
		t.Errorf("report total = %d %v", rep.TotalSeconds, err)
	}
	for _, d := range []string{"1:30", "90", "1.5h", "1h30m"} {
		if got, _ := ParseDuration(d); got != 90*time.Minute {
			t.Errorf("ParseDuration(%s) = %v", d, got)
		}
	}
}

func TestTimerAutomation(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Auto", "")
	for _, tpl := range AutomationTemplates {
		if strings.Contains(tpl.Name, "timer") {
			r := tpl.Rule
			if _, err := e.s.CreateAutomation(e.ctx, e.admin, p.Key, AutomationInput{Name: &tpl.Name, Rule: &r}); err != nil {
				t.Fatal(err)
			}
		}
	}
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "a"})
	e.s.Transition(e.ctx, e.admin, a.Key, "In Progress", "")
	run, _ := e.s.RunningTimer(e.ctx, e.admin)
	if run == nil || run.Issue.Key != a.Key {
		t.Fatalf("timer not started: %+v", run)
	}
	e.s.Transition(e.ctx, e.admin, a.Key, "In Review", "")
	if run, _ := e.s.RunningTimer(e.ctx, e.admin); run == nil {
		t.Fatal("timer stopped on in-progress to in-progress")
	}
	e.s.Transition(e.ctx, e.admin, a.Key, "done", "")
	if run, _ := e.s.RunningTimer(e.ctx, e.admin); run != nil {
		t.Fatalf("timer still running: %+v", run)
	}
}

func TestAutomationConditionsAndActions(t *testing.T) {
	e := newEnv(t)
	sam := e.user("sam", false)
	p := e.project(e.admin, "Rules", "")
	e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "editor")
	_, err := e.s.CreateAutomation(e.ctx, e.admin, p.Key, AutomationInput{Rule: &Rule{
		Trigger: Trigger{Event: "created"},
		If:      "type:bug",
		Actions: []Action{
			{Type: "add_label", Value: "triage"},
			{Type: "assign", User: "sam"},
			{Type: "comment", Message: "Thanks {{.Actor.DisplayName}}, triaging {{.Issue.Key}}"},
			{Type: "create_subtasks", Titles: []string{"Reproduce", "Fix"}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	bug := e.issue(e.admin, IssueInput{Project: p.Key, Type: "bug", Title: "Broken"})
	got := e.get(e.admin, bug.Key)
	if len(got.Labels) != 1 || got.Labels[0].Name != "triage" || len(got.Assignees) != 1 || got.Assignees[0].Username != "sam" || len(got.Children) != 2 {
		t.Errorf("automation result: labels %+v assignees %+v children %d", got.Labels, got.Assignees, len(got.Children))
	}
	comments, _ := e.s.ListComments(e.ctx, e.admin, bug.Key)
	if len(comments) != 1 || comments[0].Body != "Thanks robin, triaging "+bug.Key {
		t.Errorf("comments = %+v", comments)
	}
	task := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Not a bug"})
	if got := e.get(e.admin, task.Key); len(got.Labels) != 0 {
		t.Error("condition ignored")
	}
	ns, unread, _ := e.s.ListNotifications(e.ctx, sam, false, 10)
	if unread != 2 || ns[1].Kind != NotifyAssigned || ns[0].Kind != NotifyCommented {
		t.Errorf("sam notifications = %+v", ns)
	}
	if _, err := e.s.CreateAutomation(e.ctx, e.admin, p.Key, AutomationInput{Rule: &Rule{Trigger: Trigger{Event: "nope"}, Actions: []Action{{Type: "comment"}}}}); !IsInvalid(err) {
		t.Errorf("bad trigger accepted: %v", err)
	}
}

func TestAutomationLoopIsBounded(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Loop", "")
	mk := func(to, set string) {
		_, err := e.s.CreateAutomation(e.ctx, e.admin, p.Key, AutomationInput{Rule: &Rule{
			Trigger: Trigger{Event: "status_changed", To: to}, Actions: []Action{{Type: "set_status", Status: set}}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	mk("In Progress", "In Review")
	mk("In Review", "In Progress")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "ping pong"})
	done := make(chan struct{})
	go func() {
		e.s.Transition(e.ctx, e.admin, i.Key, "In Progress", "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("automation loop did not terminate")
	}
}

func TestMentionsNotify(t *testing.T) {
	e := newEnv(t)
	sam := e.user("sam", false)
	outsider := e.user("zed", false)
	p := e.project(e.admin, "Mentions", "")
	e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "viewer")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "hello"})
	e.s.AddComment(e.ctx, e.admin, i.Key, "hey @sam and @zed, look")
	ns, _, _ := e.s.ListNotifications(e.ctx, sam, false, 10)
	if len(ns) != 1 || ns[0].Kind != NotifyMentioned {
		t.Errorf("sam: %+v", ns)
	}
	if ns, _, _ := e.s.ListNotifications(e.ctx, outsider, false, 10); len(ns) != 0 {
		t.Errorf("outsider notified about an issue they can't see: %+v", ns)
	}
	e.s.AddComment(e.ctx, sam, i.Key, "seen")
	ns, unread, _ := e.s.ListNotifications(e.ctx, e.admin, true, 10)
	if unread != 1 || ns[0].Kind != NotifyCommented {
		t.Errorf("reporter: %+v", ns)
	}
	e.s.MarkNotificationsRead(e.ctx, e.admin, nil)
	if _, unread, _ := e.s.ListNotifications(e.ctx, e.admin, true, 10); unread != 0 {
		t.Error("mark read failed")
	}
}

func TestInviteLoginReset(t *testing.T) {
	e := newEnv(t)
	sam := e.user("sam", false)
	if _, err := e.s.CreateInvite(e.ctx, sam, "", false, 0); err == nil {
		t.Fatal("non-admin invited")
	}
	link, err := e.s.CreateInvite(e.ctx, e.admin, "riley@example.com", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	tok := link.URL[strings.LastIndex(link.URL, "/")+1:]
	if l, err := e.s.GetLink(e.ctx, tok); err != nil || l.Email != "riley@example.com" {
		t.Fatalf("get link: %+v %v", l, err)
	}
	if _, _, err := e.s.AcceptInvite(e.ctx, tok, AcceptInviteInput{Username: "riley", Password: "short"}, "", ""); !IsInvalid(err) {
		t.Errorf("short password accepted: %v", err)
	}
	sess, u, err := e.s.AcceptInvite(e.ctx, tok, AcceptInviteInput{Username: "riley", Password: "a long enough password"}, "", "")
	if err != nil || u.Email != "riley@example.com" {
		t.Fatalf("accept: %v", err)
	}
	if _, _, err := e.s.AcceptInvite(e.ctx, tok, AcceptInviteInput{Username: "riley2", Password: "a long enough password"}, "", ""); err == nil {
		t.Error("invite reused")
	}
	if got, err := e.s.SessionUser(e.ctx, sess); err != nil || got.ID != u.ID {
		t.Errorf("session: %v", err)
	}
	if _, _, err := e.s.Login(e.ctx, "riley", "wrong password!!", "", ""); err == nil {
		t.Error("bad password accepted")
	}
	reset, err := e.s.CreateResetLink(e.ctx, e.admin, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	rtok := reset.URL[strings.LastIndex(reset.URL, "/")+1:]
	if err := e.s.ResetPassword(e.ctx, rtok, "a brand new password"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SessionUser(e.ctx, sess); err == nil {
		t.Error("reset should end existing sessions")
	}
	if _, _, err := e.s.Login(e.ctx, "riley@example.com", "a brand new password", "", ""); err != nil {
		t.Errorf("login with email after reset: %v", err)
	}
	tr := true
	e.s.UpdateUser(e.ctx, e.admin, u.ID, UserPatch{Disabled: &tr})
	if _, _, err := e.s.Login(e.ctx, "riley", "a brand new password", "", ""); err == nil {
		t.Error("disabled user logged in")
	}
	tok2, _, err := e.s.CreateAPIToken(e.ctx, sam, "agent", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.s.TokenActor(e.ctx, tok2)
	if err != nil || !a.ReadOnly || a.User.ID != sam.User.ID {
		t.Fatalf("token actor: %+v %v", a, err)
	}
	if _, err := e.s.CreateProject(e.ctx, a, ProjectInput{Name: str("x")}); err == nil {
		t.Error("read-only token wrote")
	}
}

type fakeForge struct{ closed map[int]bool }

func (f *fakeForge) GetItem(ctx context.Context, fg *Forge, repo string, n int, pr bool) (*ForgeItem, error) {
	return &ForgeItem{Number: n, Title: "remote title", State: "open", IsPR: pr, Author: "me"}, nil
}
func (f *fakeForge) ListIssues(ctx context.Context, fg *Forge, repo string, since time.Time) ([]ForgeItem, error) {
	return []ForgeItem{{Number: 7, Title: "Imported bug", State: "open", URL: "https://git.example.com/me/app/issues/7", Labels: []string{"bug"}, Author: "me"}}, nil
}
func (f *fakeForge) SetIssueState(ctx context.Context, fg *Forge, repo string, n int, closed bool) error {
	f.closed[n] = closed
	return nil
}

func TestForgeWebhookAndSync(t *testing.T) {
	e := newEnv(t)
	fake := &fakeForge{closed: map[int]bool{}}
	e.s.Forges = fake
	p := e.project(e.admin, "App", "")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: str("forgejo"), BaseURL: str("https://git.example.com"), Token: str("t")})
	if err != nil {
		t.Fatal(err)
	}
	e.forgePeople(f, "me")
	secret, _ := e.s.ForgeSecret(e.ctx, e.admin, f.ID)
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Login"})
	repo, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "me/app", false)
	if err != nil {
		t.Fatal(err)
	}
	other := e.project(e.admin, "Vault", "")
	secretIssue := e.issue(e.admin, IssueInput{Project: other.Key, Title: "Private"})
	send := func(event, body string) error {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(body))
		h := http.Header{}
		h.Set("X-Forgejo-Event", event)
		h.Set("X-Forgejo-Signature", hex.EncodeToString(mac.Sum(nil)))
		return e.s.HandleForgeWebhook(e.ctx, f.ID, h, []byte(body))
	}
	branch := strings.ToLower(i.Key) + "-login"
	pr := `{"action":"opened","pull_request":{"number":3,"title":"Fix login","body":"also ` + secretIssue.Key + `","html_url":"https://git.example.com/me/app/pulls/3","state":"open","merged":false,"head":{"ref":"` + branch + `"}},"repository":{"full_name":"me/app"}}`
	if err := send("pull_request", pr); err != nil {
		t.Fatal(err)
	}
	got := e.get(e.admin, i.Key)
	if len(got.Links) != 1 || got.Links[0].Kind != "pull" || got.Links[0].State != "open" {
		t.Fatalf("links = %+v", got.Links)
	}
	if l := e.get(e.admin, secretIssue.Key).Links; len(l) != 0 {
		t.Errorf("unattached repo linked into another project: %+v", l)
	}
	merged := strings.Replace(strings.Replace(pr, `"merged":false`, `"merged":true`, 1), `"state":"open"`, `"state":"closed"`, 1)
	if err := send("pull_request", merged); err != nil {
		t.Fatal(err)
	}
	if got := e.get(e.admin, i.Key); got.Status.Category != CatDone {
		t.Errorf("PR merge should finish issue, status %s", got.Status.Name)
	}
	h := http.Header{}
	h.Set("X-Forgejo-Event", "ping")
	h.Set("X-Forgejo-Signature", "deadbeef")
	if err := e.s.HandleForgeWebhook(e.ctx, f.ID, h, []byte(`{}`)); err == nil {
		t.Error("bad signature accepted")
	}
	e.s.db.W.Exec(`UPDATE project_repos SET sync_issues = 1 WHERE id = ?`, repo.ID)
	if err := e.s.SyncRepo(e.ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	imported := e.search(e.admin, "type:bug")
	if len(imported) != 1 {
		t.Fatalf("imported = %v", imported)
	}
	e.s.SyncRepo(e.ctx, repo.ID)
	if again := e.search(e.admin, "type:bug"); len(again) != 1 {
		t.Errorf("duplicate import: %v", again)
	}
	e.s.Transition(e.ctx, e.admin, imported[0], "done", "")
	e.s.Wait()
	if !fake.closed[7] {
		t.Error("closing in Waypoint should close the forge issue")
	}
}

func TestForecast(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Fore", "")
	start := e.clockNow()
	e.setClock(start.AddDate(0, 0, -42))
	for w := 0; w < 6; w++ {
		i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "done", Points: ptrF(2)})
		e.setClock(e.clockNow().AddDate(0, 0, 7))
		e.s.Transition(e.ctx, e.admin, i.Key, "done", "")
	}
	e.setClock(start)
	for n := 0; n < 5; n++ {
		e.issue(e.admin, IssueInput{Project: p.Key, Title: "todo", Points: ptrF(2)})
	}
	fc, err := e.s.ForecastWork(e.ctx, e.admin, p.Key, "")
	if err != nil {
		t.Fatal(err)
	}
	if fc.Basis != "points" || fc.Remaining != 10 || fc.P50 == "" {
		t.Fatalf("forecast = %+v", fc)
	}
	p50, _ := time.Parse(dateLayout, fc.P50)
	if weeks := p50.Sub(start).Hours() / 24 / 7; weeks < 4 || weeks > 7 {
		t.Errorf("p50 %s is %.1f weeks out", fc.P50, weeks)
	}
	tp, _ := e.s.Throughput(e.ctx, e.admin, p.Key, 8)
	if tp[len(tp)-1].ScopePoints != 22 {
		t.Errorf("burnup scope = %v", tp[len(tp)-1].ScopePoints)
	}
	cfd, _ := e.s.CumulativeFlow(e.ctx, e.admin, p.Key, 50)
	if last := cfd[len(cfd)-1]; last.Counts[CatDone] != 6 || last.Counts[CatTodo] != 5 {
		t.Errorf("cfd last = %+v", last)
	}
	if first := cfd[0]; len(first.Counts) != 0 {
		t.Errorf("cfd before anything existed = %+v", first)
	}
}

// Weeks before a project had work don't count as weeks nothing got done.
func TestRatesOfAYoungProject(t *testing.T) {
	week := 7 * 24 * time.Hour
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) // a Wednesday
	var all []flowIssue
	for _, ago := range []time.Duration{week, 2 * week} {
		at := now.Add(-ago).UnixMilli()
		all = append(all, flowIssue{created: at, resolved: at}, flowIssue{created: at, resolved: at})
	}
	all = append(all, flowIssue{created: now.UnixMilli()}, flowIssue{created: now.UnixMilli()})
	s := &Service{now: func() time.Time { return now }}
	fc := s.forecast(all, func(flowIssue) bool { return true }, ProjectSettings{VelocityWeeks: 6}, time.UTC)
	// Two weeks old: the two weeks of work and the fewest kept before them.
	if len(fc.WeeklyThroughput) != forecastMinWeeks || fc.AveragePerWeek != 1 {
		t.Errorf("forecast %+v", fc)
	}
	if p := dashPace(all, now); p.Span4 != 2 || p.SpanPrev4 != 0 {
		t.Errorf("spans %v %v, want 2 0", p.Span4, p.SpanPrev4)
	}
}

func TestCalendarFeed(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Cal", "")
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "Pay taxes; file forms, etc", Assignees: []string{"me"}, DueDate: "tomorrow"})
	tok, err := e.s.CalendarToken(e.ctx, e.admin, false)
	if err != nil {
		t.Fatal(err)
	}
	ics, err := e.s.CalendarFeed(e.ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	s := string(ics)
	if !strings.Contains(s, "DTSTART;VALUE=DATE:20261006") || !strings.Contains(s, `Pay taxes\; file forms\, etc`) {
		t.Errorf("ics:\n%s", s)
	}
	if _, err := e.s.CalendarFeed(e.ctx, "nope-nope-nope-nope-nope"); err == nil {
		t.Error("bad token accepted")
	}
}

func TestParseDate(t *testing.T) {
	now := time.Date(2026, 10, 7, 15, 0, 0, 0, time.UTC)
	cases := map[string]string{
		"today": "2026-10-07", "tomorrow": "2026-10-08", "+3d": "2026-10-10", "2w": "2026-10-21",
		"-1d": "2026-10-06", "friday": "2026-10-09", "wed": "2026-10-14", "2026-12-25": "2026-12-25", "eom": "2026-10-31",
	}
	for in, want := range cases {
		got, err := ParseDate(in, now)
		if err != nil || got == nil || *got != want {
			t.Errorf("ParseDate(%q) = %v %v, want %s", in, got, err, want)
		}
	}
}

func TestAutomationBudget(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Loop", "")
	for _, name := range []string{"a", "b"} {
		n := name
		rule := Rule{Trigger: Trigger{Event: "created"}, Actions: []Action{{Type: "create_subtasks", Titles: []string{n + "1", n + "2", n + "3"}}}}
		if _, err := e.s.CreateAutomation(e.ctx, e.admin, p.Key, AutomationInput{Name: &n, Rule: &rule}); err != nil {
			t.Fatal(err)
		}
	}
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "root"})
	total := len(e.search(e.admin, "project:"+p.Key))
	if total > 1+automationBudget {
		t.Fatalf("automations created %d issues", total)
	}
	if total < 2 {
		t.Fatalf("automations didn't run: %d", total)
	}
	t.Logf("one change created %d issues", total)
}

func TestSessionsAndPasswordChange(t *testing.T) {
	e := newEnv(t)
	tok1, _, err := e.s.Login(e.ctx, "robin", "correct-horse-battery", "laptop", "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	tok2, _, _ := e.s.Login(e.ctx, "ROBIN", "correct-horse-battery", "phone", "10.0.0.2")
	tok3, _, _ := e.s.Login(e.ctx, "robin", "correct-horse-battery", "tablet", "10.0.0.3")
	me, err := e.s.SessionActor(e.ctx, tok1)
	if err != nil {
		t.Fatal(err)
	}
	list, err := e.s.ListSessions(e.ctx, me)
	if err != nil || len(list) != 3 {
		t.Fatalf("sessions = %v %v", list, err)
	}
	var phone string
	for _, se := range list {
		if se.Current != (se.UserAgent == "laptop") {
			t.Errorf("current flag wrong: %+v", se)
		}
		if se.UserAgent == "phone" {
			phone = se.ID
		}
	}
	if err := e.s.RevokeSession(e.ctx, me, phone); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SessionUser(e.ctx, tok2); err == nil {
		t.Error("revoked session still works")
	}
	sam := e.user("sam", false)
	if err := e.s.RevokeSession(e.ctx, sam, list[0].ID); err == nil {
		t.Error("revoked someone else's session")
	}
	reset, _ := e.s.CreateResetLink(e.ctx, e.admin, me.User.ID)
	if err := e.s.ChangePassword(e.ctx, me, "correct-horse-battery", "an even better passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SessionUser(e.ctx, tok1); err != nil {
		t.Error("password change ended the current session")
	}
	if _, err := e.s.SessionUser(e.ctx, tok3); err == nil {
		t.Error("password change kept another session")
	}
	rtok := reset.URL[strings.LastIndex(reset.URL, "/")+1:]
	if err := e.s.ResetPassword(e.ctx, rtok, "someone else's password"); err == nil {
		t.Error("reset link survived a password change")
	}
}

func TestReadOnlyActorCannotWrite(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "RO", "")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "x"})
	sam := e.user("sam", false)
	ro := &Actor{User: e.admin.User, ReadOnly: true}
	name, url := "h", "https://example.com/hook"
	checks := map[string]func() error{
		"CreateUser": func() error {
			_, err := e.s.CreateUser(e.ctx, ro, CreateUserInput{Username: "x", Email: "x@example.com", Password: "a long enough password"})
			return err
		},
		"CreateInvite":    func() error { _, err := e.s.CreateInvite(e.ctx, ro, "", false, time.Hour); return err },
		"CreateResetLink": func() error { _, err := e.s.CreateResetLink(e.ctx, ro, sam.User.ID); return err },
		"DeleteUser":      func() error { return e.s.DeleteUser(e.ctx, ro, sam.User.ID) },
		"CreateWebhook": func() error {
			_, err := e.s.CreateWebhook(e.ctx, ro, "", WebhookInput{Name: &name, URL: &url})
			return err
		},
		"CreateForge": func() error {
			_, err := e.s.CreateForge(e.ctx, ro, ForgeInput{Kind: str("forgejo"), BaseURL: str("https://git.example.com")})
			return err
		},
		"Watch":                 func() error { return e.s.Watch(e.ctx, ro, i.Key, false) },
		"MarkNotificationsRead": func() error { return e.s.MarkNotificationsRead(e.ctx, ro, nil) },
		"DeleteView":            func() error { return e.s.DeleteView(e.ctx, ro, 1) },
		"RevokeAPIToken":        func() error { return e.s.RevokeAPIToken(e.ctx, ro, 1) },
		"CreateLabel": func() error {
			_, err := e.s.CreateLabel(e.ctx, ro, p.Key, LabelInput{Name: str("l")})
			return err
		},
		"UpdateIssue": func() error {
			_, err := e.s.UpdateIssue(e.ctx, ro, i.Key, IssuePatch{Title: str("y")})
			return err
		},
		"AddComment":    func() error { _, err := e.s.AddComment(e.ctx, ro, i.Key, "hi"); return err },
		"RevokeSession": func() error { return e.s.RevokeSession(e.ctx, ro, "others") },
	}
	for name, f := range checks {
		if err := f(); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s with a read-only actor: %v", name, err)
		}
	}
}

func TestNoLeaksToOutsiders(t *testing.T) {
	e := newEnv(t)
	sam, kim := e.user("sam", false), e.user("kim", false)
	p := e.project(e.admin, "Private", "")
	vault := e.project(e.admin, "Vault", "")
	secret := e.issue(e.admin, IssueInput{Project: vault.Key, Title: "Vault plans"})
	epic := e.issue(e.admin, IssueInput{Project: p.Key, Type: "epic", Title: "Secret epic"})
	task := e.issue(e.admin, IssueInput{Project: p.Key, Parent: epic.Key, Title: "Book flights"})
	if err := e.s.AddRelation(e.ctx, e.admin, task.Key, "relates", secret.Key); err != nil {
		t.Fatal(err)
	}
	unread := func(a *Actor) int {
		_, n, err := e.s.ListNotifications(e.ctx, a, true, 50)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	if _, err := e.s.UpdateIssue(e.ctx, e.admin, task.Key, IssuePatch{AddAssign: []string{"sam"}}); err != nil {
		t.Fatal(err)
	}
	got := e.get(sam, task.Key)
	if got.Parent != nil || len(got.Ancestors) != 0 {
		t.Errorf("guest sees the parent epic: %+v %+v", got.Parent, got.Ancestors)
	}
	if len(got.Relations) != 0 {
		t.Errorf("guest sees a related issue in another project: %+v", got.Relations)
	}
	proj, err := e.s.GetProject(e.ctx, sam, p.Key)
	if err != nil || len(proj.Members) != 0 || proj.OpenIssues != 1 {
		t.Errorf("guest project view: members %d open %d err %v", len(proj.Members), proj.OpenIssues, err)
	}
	if res := e.search(sam, "project:"+p.Key); len(res) != 1 {
		t.Errorf("guest search = %v", res)
	}
	for _, i := range mustSearch(t, e, sam, "Book") {
		if i.Parent != nil {
			t.Error("search result shows the hidden parent")
		}
	}

	if _, err := e.s.UpdateIssue(e.ctx, sam, task.Key, IssuePatch{AddAssign: []string{"kim"}}); err == nil {
		t.Error("guest shared the issue by assigning it")
	}

	e.s.SetMember(e.ctx, e.admin, p.Key, "kim", "editor")
	c, err := e.s.AddComment(e.ctx, kim, epic.Key, "my idea")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.RemoveMember(e.ctx, e.admin, p.Key, "kim"); err != nil {
		t.Fatal(err)
	}
	before := unread(kim)
	e.s.AddComment(e.ctx, e.admin, epic.Key, "private follow-up")
	if unread(kim) != before {
		t.Error("removed member notified about a watched issue")
	}
	if err := e.s.UpdateComment(e.ctx, kim, c.ID, "rewritten"); err == nil {
		t.Error("removed member edited their old comment")
	}
	if err := e.s.DeleteComment(e.ctx, kim, c.ID); err == nil {
		t.Error("removed member deleted their old comment")
	}
}

func mustSearch(t *testing.T, e *env, a *Actor, q string) []*Issue {
	t.Helper()
	res, err := e.s.Search(e.ctx, a, SearchParams{Query: q})
	if err != nil {
		t.Fatal(err)
	}
	return res.Issues
}

func TestSearchNegationAndLiterals(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Q", "")
	three := 3.0
	eight := 8.0
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "small", Points: &three})
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "big", Points: &eight})
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "100% done_ish"})
	if got := e.search(e.admin, "-points:>5"); len(got) != 2 {
		t.Errorf("-points:>5 should include unestimated: %v", got)
	}
	if got := e.search(e.admin, "points:>5"); len(got) != 1 {
		t.Errorf("points:>5 = %v", got)
	}
	if got := e.search(e.admin, `title:"%"`); len(got) != 1 {
		t.Errorf("title:%% should match literally: %v", got)
	}
	if got := e.search(e.admin, "title:_"); len(got) != 1 {
		t.Errorf("title:_ should match literally: %v", got)
	}
}

func TestGotifyTokenIsWriteOnly(t *testing.T) {
	e := newEnv(t)
	prefs := UserPrefs{GotifyURL: "https://gotify.example.com", GotifyToken: "Asecret"}
	u, err := e.s.UpdateUser(e.ctx, e.admin, e.admin.User.ID, UserPatch{Prefs: &prefs})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(u)
	if strings.Contains(string(b), "Asecret") {
		t.Fatalf("token returned: %s", b)
	}
	var back User
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	back.Prefs.Theme = "dark"
	if _, err := e.s.UpdateUser(e.ctx, e.admin, e.admin.User.ID, UserPatch{Prefs: &back.Prefs}); err != nil {
		t.Fatal(err)
	}
	stored, _ := e.s.userByID(e.ctx, e.s.db.R, e.admin.User.ID)
	if stored.Prefs.GotifyToken != "Asecret" || stored.Prefs.Theme != "dark" {
		t.Errorf("stored prefs = %+v", stored.Prefs)
	}
	bad := UserPrefs{GotifyURL: "file:///etc/passwd"}
	if _, err := e.s.UpdateUser(e.ctx, e.admin, e.admin.User.ID, UserPatch{Prefs: &bad}); err == nil {
		t.Error("non-http Gotify URL accepted")
	}
}

func TestTimedAutomations(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Timed", "")
	add := func(name string, rule Rule) {
		t.Helper()
		if _, err := e.s.CreateAutomation(e.ctx, e.admin, p.Key, AutomationInput{Name: &name, Rule: &rule}); err != nil {
			t.Fatal(err)
		}
	}
	bad := Rule{Trigger: Trigger{Event: "schedule", Every: "week", Weekday: 1, At: "9am"}, Actions: []Action{{Type: "create_issue", Title: "x"}}}
	if _, err := e.s.CreateAutomation(e.ctx, e.admin, p.Key, AutomationInput{Name: str("bad"), Rule: &bad}); err == nil {
		t.Error("bad schedule time accepted")
	}
	add("due", Rule{Trigger: Trigger{Event: "due_soon", Days: 1}, Actions: []Action{{Type: "add_label", Value: "due-soon"}}})
	add("stale", Rule{Trigger: Trigger{Event: "stale", Days: 10}, Actions: []Action{{Type: "add_label", Value: "stale"}}})
	add("weekly", Rule{Trigger: Trigger{Event: "schedule", Every: "week", Weekday: int(time.Monday), At: "09:00"},
		Actions: []Action{{Type: "create_issue", Title: "Plan the week of {{.Date}}", Due: "+2d", Labels: []string{"planning"}}}})

	soon := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Due tomorrow", DueDate: "2026-10-06"})
	later := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Due next week", DueDate: "2026-10-12"})
	old := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Old"})
	e.s.RunTimedAutomations(e.ctx)
	e.s.RunTimedAutomations(e.ctx)
	e.s.Wait()
	labels := func(key string) string {
		var names []string
		for _, l := range e.get(e.admin, key).Labels {
			names = append(names, l.Name)
		}
		return strings.Join(names, ",")
	}
	if l := labels(soon.Key); l != "due-soon" {
		t.Errorf("due soon labels = %q", l)
	}
	if l := labels(later.Key); l != "" {
		t.Errorf("not-yet-due labels = %q", l)
	}
	if got := e.search(e.admin, "label:planning"); len(got) != 1 {
		t.Fatalf("weekly issue created %d times", len(got))
	}
	plan := e.get(e.admin, e.search(e.admin, "label:planning")[0])
	if plan.Title != "Plan the week of 2026-10-05" || plan.DueDate == nil || *plan.DueDate != "2026-10-07" {
		t.Errorf("weekly issue = %q due %v", plan.Title, plan.DueDate)
	}
	e.setClock(e.clockNow().AddDate(0, 0, 11))
	e.s.RunTimedAutomations(e.ctx)
	e.s.Wait()
	if l := labels(old.Key); !strings.Contains(l, "stale") {
		t.Errorf("stale labels = %q", l)
	}
	if got := e.search(e.admin, "label:planning"); len(got) != 1 {
		t.Errorf("weekly issue on a Friday: %d", len(got))
	}
	e.setClock(time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC))
	e.s.RunTimedAutomations(e.ctx)
	if got := e.search(e.admin, "label:planning"); len(got) != 2 {
		t.Errorf("second week: %d planning issues", len(got))
	}
}

func TestIssueTemplates(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Tpl", "")
	three := 3.0
	if _, err := e.s.CreateTemplate(e.ctx, e.admin, p.Key, TemplateInput{Name: str("Bug report"), Type: str("bug"), Title: str("Bug: "),
		Description: str("## Steps\n\n## Expected"), Labels: &[]string{"bug"}, Points: Some(three), Subtasks: &[]string{"Reproduce", "Fix", " ", "Add a test"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateTemplate(e.ctx, e.admin, p.Key, TemplateInput{Name: str("bug REPORT")}); err == nil {
		t.Error("duplicate template name accepted")
	}
	viewer := e.user("vic", false)
	e.s.SetMember(e.ctx, e.admin, p.Key, "vic", "viewer")
	if _, err := e.s.CreateTemplate(e.ctx, viewer, p.Key, TemplateInput{Name: str("x")}); err == nil {
		t.Error("viewer created a template")
	}
	d, err := e.s.CreateIssue(e.ctx, e.admin, IssueInput{Project: p.Key, Template: "bug report", Title: "Bug: login loops"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Type != "bug" || d.Title != "Bug: login loops" || d.Points == nil || *d.Points != 3 || len(d.Labels) != 1 || !strings.Contains(d.Description, "Steps") {
		t.Errorf("from template: %+v", d.Issue)
	}
	if len(d.Children) != 3 || d.Children[0].Title != "Reproduce" {
		t.Errorf("sub-tasks: %d", len(d.Children))
	}
	rule := Rule{Trigger: Trigger{Event: "schedule", Every: "day", At: "00:00"}, Actions: []Action{{Type: "create_issue", Template: "Bug report", Title: "Bug triage {{.Date}}"}}}
	if _, err := e.s.CreateAutomation(e.ctx, e.admin, p.Key, AutomationInput{Name: str("daily"), Rule: &rule}); err != nil {
		t.Fatal(err)
	}
	e.s.RunTimedAutomations(e.ctx)
	got := e.search(e.admin, `title:"Bug triage 2026-10-05"`)
	if len(got) != 1 || len(e.get(e.admin, got[0]).Children) != 3 {
		t.Errorf("scheduled template issue: %v", got)
	}
}

func TestWebPush(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	got := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		got++
		if !strings.HasPrefix(r.Header.Get("Authorization"), "vapid ") || r.Header.Get("Content-Encoding") != "aes128gcm" {
			t.Errorf("push headers %v", r.Header)
		}
		if strings.HasSuffix(r.URL.Path, "/gone") {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	e.s.pushHTTP = srv.Client()
	pub, err := e.s.PushPublicKey(e.ctx)
	if err != nil || pub == "" {
		t.Fatal(err)
	}
	if again, _ := e.s.PushPublicKey(e.ctx); again != pub {
		t.Error("VAPID key changed")
	}
	sam := e.user("sam", false)
	sub := PushSubscription{Endpoint: srv.URL + "/push/ok"}
	sub.Keys.P256dh = "BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQtUbVlUls0VJXg7A8u-Ts1XbjhazAkj7I99e8QcYP7DkM"
	sub.Keys.Auth = "tBHItJI5svbpez7KI4CCXg"
	if err := e.s.SavePushSubscription(e.ctx, sam, sub, "test"); err != nil {
		t.Fatal(err)
	}
	gone := sub
	gone.Endpoint = srv.URL + "/push/gone"
	if err := e.s.SavePushSubscription(e.ctx, sam, gone, "test"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SavePushSubscription(e.ctx, sam, PushSubscription{Endpoint: "http://insecure.example/x"}, ""); err == nil {
		t.Error("http endpoint accepted")
	}
	p := e.project(e.admin, "Push", "")
	e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "editor")
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "For Sam", Assignees: []string{"sam"}})
	e.s.Wait()
	mu.Lock()
	defer mu.Unlock()
	if got != 2 {
		t.Errorf("push requests = %d", got)
	}
	if n, _ := e.s.PushDevices(e.ctx, sam); n != 1 {
		t.Errorf("gone subscription kept: %d devices", n)
	}
}

func TestReviewFixesMisc(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Misc", "")
	long := strings.Repeat("x", 600)
	if _, err := e.s.CreateTemplate(e.ctx, e.admin, p.Key, TemplateInput{Name: str("t"), Subtasks: &[]string{long}}); err == nil {
		t.Error("over-long sub-task title accepted")
	}
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "files"})
	a1, err := e.s.AddAttachment(e.ctx, e.admin, AttachTarget{TargetIssue, i.ID}, AttachmentInput{Name: "image.png", Data: []byte("one")})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteAttachment(e.ctx, e.admin, a1.ID); err != nil {
		t.Fatal(err)
	}
	a2, _ := e.s.AddAttachment(e.ctx, e.admin, AttachTarget{TargetIssue, i.ID}, AttachmentInput{Name: "image.png", Data: []byte("two")})
	if a2.URL == a1.URL {
		t.Errorf("attachment URL reused: %s", a2.URL)
	}
	tok, _, _ := e.s.Login(e.ctx, "robin", "correct-horse-battery", "", "")
	me, _ := e.s.SessionActor(e.ctx, tok)
	sub := PushSubscription{Endpoint: "https://push.example.com/abc"}
	sub.Keys.P256dh, sub.Keys.Auth = "k", "a"
	if err := e.s.SavePushSubscription(e.ctx, me, sub, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ChangePassword(e.ctx, me, "correct-horse-battery", "a new long passphrase"); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.s.PushDevices(e.ctx, me); n != 0 {
		t.Errorf("push subscriptions kept after password change: %d", n)
	}
}

func TestBlocksInLists(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Deps", "")
	vault := e.project(e.admin, "Vault", "")
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Schema"})
	b := e.issue(e.admin, IssueInput{Project: p.Key, Title: "API"})
	hidden := e.issue(e.admin, IssueInput{Project: vault.Key, Title: "Secret"})
	e.s.AddRelation(e.ctx, e.admin, a.Key, "blocks", b.Key)
	e.s.AddRelation(e.ctx, e.admin, b.Key, "blocked_by", a.Key)
	e.s.AddRelation(e.ctx, e.admin, a.Key, "blocks", hidden.Key)
	sam := e.user("sam", false)
	e.s.SetMember(e.ctx, e.admin, p.Key, "sam", "viewer")
	for _, i := range mustSearch(t, e, sam, "project:"+p.Key) {
		if i.Key == a.Key && strings.Join(i.Blocks, ",") != b.Key {
			t.Errorf("%s blocks %v for sam", i.Key, i.Blocks)
		}
	}
	for _, i := range mustSearch(t, e, e.admin, "project:"+p.Key) {
		if i.Key == a.Key && len(i.Blocks) != 2 {
			t.Errorf("admin sees blocks %v", i.Blocks)
		}
	}
}

func TestHiddenHelpPrefs(t *testing.T) {
	e := newEnv(t)
	prefs := UserPrefs{HiddenHelp: []string{"tab:statuses", "tab:workflows"}}
	u, err := e.s.UpdateUser(e.ctx, e.admin, e.admin.User.ID, UserPatch{Prefs: &prefs})
	if err != nil || len(u.Prefs.HiddenHelp) != 2 {
		t.Fatalf("save: %+v %v", u, err)
	}
	if got, _ := e.s.UserByID(e.ctx, e.admin.User.ID); got == nil || len(got.Prefs.HiddenHelp) != 2 {
		t.Errorf("not stored: %+v", got)
	}
	open := UserPrefs{OpenHelp: []string{"agents:queue"}}
	if u, err := e.s.UpdateUser(e.ctx, e.admin, e.admin.User.ID, UserPatch{Prefs: &open}); err != nil || len(u.Prefs.OpenHelp) != 1 {
		t.Fatalf("open save: %+v %v", u, err)
	}
	dw := UserPrefs{DismissedWarnings: []string{"12:names a file (main.go)"}}
	if u, err := e.s.UpdateUser(e.ctx, e.admin, e.admin.User.ID, UserPatch{Prefs: &dw}); err != nil || len(u.Prefs.DismissedWarnings) != 1 {
		t.Fatalf("dismissed warnings: %+v %v", u, err)
	}
	if got, _ := e.s.UserByID(e.ctx, e.admin.User.ID); got == nil || len(got.Prefs.DismissedWarnings) != 1 {
		t.Errorf("dismissed warnings not stored: %+v", got)
	}
	tooMany := UserPrefs{DismissedWarnings: make([]string, maxDismissedWarnings+1)}
	for i := range tooMany.DismissedWarnings {
		tooMany.DismissedWarnings[i] = "w"
	}
	tooMany.DismissedWarnings[0] = "oldest"
	if u, err := e.s.UpdateUser(e.ctx, e.admin, e.admin.User.ID, UserPatch{Prefs: &tooMany}); err != nil ||
		len(u.Prefs.DismissedWarnings) != maxDismissedWarnings || u.Prefs.DismissedWarnings[0] == "oldest" {
		t.Errorf("over-cap dismissed warnings should drop the oldest: %v", err)
	}
	long := UserPrefs{HiddenHelp: []string{strings.Repeat("x", 65)}}
	if _, err := e.s.UpdateUser(e.ctx, e.admin, e.admin.User.ID, UserPatch{Prefs: &long}); err == nil {
		t.Error("an over-long help id was saved")
	}
}

func TestProjectOrderPrefs(t *testing.T) {
	e := newEnv(t)
	me := e.admin.User.ID
	save := func(p UserPrefs) error {
		_, err := e.s.UpdateUser(e.ctx, e.admin, me, UserPatch{Prefs: &p})
		return err
	}
	if err := save(UserPrefs{ProjectSort: "custom", ProjectOrder: []int64{3, 1, 2}}); err != nil {
		t.Fatal(err)
	}
	got, err := e.s.UserByID(e.ctx, me)
	if err != nil || got.Prefs.ProjectSort != "custom" || !slices.Equal(got.Prefs.ProjectOrder, []int64{3, 1, 2}) {
		t.Fatalf("not stored: %+v %v", got, err)
	}
	for name, p := range map[string]UserPrefs{
		"unknown sort": {ProjectSort: "alphabetical"},
		"duplicate id": {ProjectOrder: []int64{1, 2, 1}},
		"bad id":       {ProjectOrder: []int64{0}},
		"too many":     {ProjectOrder: make([]int64, maxProjectOrder+1)},
	} {
		if save(p) == nil {
			t.Errorf("%s was saved", name)
		}
	}
	if got, _ := e.s.UserByID(e.ctx, me); !slices.Equal(got.Prefs.ProjectOrder, []int64{3, 1, 2}) {
		t.Errorf("a rejected save changed the order: %v", got.Prefs.ProjectOrder)
	}
}

// Project lists say when each project last changed, its issues included.
func TestProjectActiveAt(t *testing.T) {
	e := newEnv(t)
	old := e.project(e.admin, "Old", "")
	e.setClock(e.clockNow().Add(time.Hour))
	e.project(e.admin, "New", "")
	e.setClock(e.clockNow().Add(time.Hour))
	e.issue(e.admin, IssueInput{Project: old.Key, Title: "Back at it"})
	list, err := e.s.ListProjects(e.ctx, e.admin, false)
	if err != nil {
		t.Fatal(err)
	}
	at := map[string]time.Time{}
	for _, p := range list {
		if p.ActiveAt == nil {
			t.Fatalf("%s: no active_at", p.Name)
		}
		at[p.Name] = *p.ActiveAt
	}
	if !at["Old"].Equal(e.clockNow()) || !at["Old"].After(at["New"]) {
		t.Errorf("active_at = %v", at)
	}
}

// FindProjects resolves keys, names and repos, best kind of match first,
// with sub-projects following their parent.
func TestFindProjects(t *testing.T) {
	e := newEnv(t)
	org := e.project(e.admin, "City Maps", "")
	child, err := e.s.CreateProject(e.ctx, e.admin, ProjectInput{Name: str("Web"), Parent: Some(org.Key)})
	if err != nil {
		t.Fatal(err)
	}
	other := e.project(e.admin, "Tools", "")
	e.s.Forges = newMemForge()
	f := e.forge("github", "", false)
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, other.Key, f.ID, "robin/harbor", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, child.Key, f.ID, "acme/web", false); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	find := func(ref string) string {
		ps, err := e.s.FindProjects(e.ctx, e.admin, ref, false)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, p := range ps {
			out = append(out, p.Name+":"+p.MatchedBy)
		}
		return strings.Join(out, ",")
	}
	for ref, want := range map[string]string{
		strings.ToLower(org.Key): "City Maps:key,Web:sub-project",
		"city maps":              "City Maps:name,Web:sub-project",
		"robin/harbor":           "Tools:repo",
		"web":                    "Web:key",
		"harbor":                 "Tools:repo_name",
		"CITY-MAPS":              "City Maps:name,Web:sub-project",
		"nothing":                "",
	} {
		if got := find(ref); got != want {
			t.Errorf("%q → %q, want %q", ref, got, want)
		}
	}
	if ps, _ := e.s.FindProjects(e.ctx, e.admin, "city/maps", true); len(ps) != 0 {
		t.Errorf("repo-only matched a name: %v", ps[0].Name)
	}
	if ps, _ := e.s.FindProjects(e.ctx, e.admin, "ROBIN/harbor", true); len(ps) != 1 || ps[0].Name != "Tools" {
		t.Errorf("repo-only: %v", ps)
	}
}
