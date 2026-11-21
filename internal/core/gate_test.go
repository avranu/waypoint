package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// shipForge adds review threads, commit statuses, CI and merging to the fake
// pull request forge.
type shipForge struct {
	*prForge
	sm            sync.Mutex
	threads       []ReviewThread
	nextID        int
	replies       []string // "thread-id: text"
	resolved      []string
	noResolv      bool                          // Forgejo: no resolve API
	statuses      map[string][]CommitStatusPost // sha → posted, in order
	stamps        map[string][]int64            // sha → the forge's id of each posted status
	seq           int64                         // the last status id given out
	checks        map[string][]CommitCheck      // sha → what the forge lists
	logs          map[string]string             // check name → log tail
	merges        []MergeOptions
	mergeErr      error
	behindErr     error                 // BehindBase fails with it
	createErr     error                 // CreatePR fails with it
	listHides     int                   // the first N ListPRs answer nothing (the forge not showing a pull request yet)
	defaultBranch string                // what DefaultBranch says: "" main, "-" an error
	reacts        map[string][]Reaction // comment id → reactions
	fileAt        map[string]string     // sha:path → content
	reads         int                   // thread reads
	replyErr      error                 // ReplyToThread fails with it
	reactErr      map[string]error      // comment id → CommentReactions fails with it

	tips   map[string]string // branch → what BranchHead says; absent: it can't say
	tipErr error             // BranchHead fails with it
	// tipHang: BranchHead doesn't answer until its context ends.
	tipHang bool

	created    []NewPR           // pull requests opened through CreatePR
	heads      map[string]string // head branch → the sha a pull request from it starts at
	labelOps   []string          // "+name" / "-name" on pull requests
	reviewers  [][]string
	retries    []string // shas whose failed checks were run again
	noRetry    bool     // a forge that can't re-run checks (Forgejo before 17)
	retryNone  bool     // a forge that finds nothing to re-run
	notes      []PRNote // review bodies and conversation comments (babysit_test.go)
	reactedTo  []string // "kind:id:emoji" put on comments
	titleEdits []string // titles set through EditPRTitle (draft_first_test.go)
	bodyEdits  []string
	// afterTitleEdit runs once a title edit is made, as the forge's webhook
	// for it may arrive before the editor's own write.
	afterTitleEdit func(title string)
	// createAuthor opens pull requests (wp-bot when empty); afterCreate runs
	// before CreatePR answers, as the forge's webhook usually does, and what
	// it returns is CreatePR's error.
	createAuthor string
	afterCreate  func(fp ForgePR) error

	behind    map[string]bool // head sha → lacks the base's tip (forge_uptodate_test.go)
	baseTip   string
	updates   []int // pull requests whose branch was updated
	updateErr error
	// updateErrs: per pull request number, over updateErr.
	updateErrs map[int]error
	// onMerge runs inside MergePR, with sm held.
	onMerge func()
	hidden  map[string]bool   // commits a read of the base doesn't show yet (WP-316)
	trees   map[string]string // commit sha → tree sha (merge_queue_test.go)

	// merge_stack_test.go: mergeSHAs gives pull request n's merge commit
	// (else 4e7ced0ffee), onMergeN runs inside MergePR with sm held,
	// commitFn makes CommitFiles work, parents lists commits' parents.
	mergeSHAs map[int]string
	onMergeN  func(n int)
	commitFn  func(in CommitInput) (*ForgeCommit, error)
	parents   map[string][]string
	deleted   []string // branches deleted
	// forge_union_test.go: a Forgejo that merges union files only once the
	// branch holds the base's version of them.
	union     unionFake
	cancelled []string // commits whose runs were cancelled
}

func newShipForge() *shipForge {
	return &shipForge{prForge: newPRForge(), statuses: map[string][]CommitStatusPost{}, checks: map[string][]CommitCheck{},
		logs: map[string]string{}, reacts: map[string][]Reaction{}, fileAt: map[string]string{}, heads: map[string]string{}}
}

func (g *shipForge) id() string {
	g.nextID++
	return strconv.Itoa(1000 + g.nextID)
}

// PostReview also makes a thread of each line comment, as a forge does.
func (g *shipForge) PostReview(ctx context.Context, f *Forge, repo string, pr *ForgePR, body string, comments []LineComment) (*PostedReview, error) {
	res, err := g.prForge.PostReview(ctx, f, repo, pr, body, comments)
	if err != nil {
		return res, err
	}
	g.sm.Lock()
	defer g.sm.Unlock()
	for _, c := range comments {
		cid := g.id()
		g.threads = append(g.threads, ReviewThread{ID: "T" + cid, Path: c.Path, Line: c.Line,
			Comments: []ThreadComment{{ID: cid, Author: "waypoint-bot", Body: c.Body, SHA: pr.HeadSHA, At: time.Now()}}})
	}
	return res, nil
}

func (g *shipForge) FileAt(ctx context.Context, f *Forge, repo, path, ref string) (string, error) {
	g.sm.Lock()
	c, ok := g.fileAt[ref+":"+path]
	g.sm.Unlock()
	if ok {
		return c, nil
	}
	return g.prForge.FileAt(ctx, f, repo, path, ref)
}

func (g *shipForge) ReviewThreads(ctx context.Context, f *Forge, repo string, n int) ([]ReviewThread, error) {
	g.sm.Lock()
	defer g.sm.Unlock()
	g.reads++
	out := make([]ReviewThread, len(g.threads))
	for i, t := range g.threads {
		t.Comments = slices.Clone(t.Comments)
		out[i] = t
	}
	return out, nil
}

func (g *shipForge) ReplyToThread(ctx context.Context, f *Forge, repo string, pr *ForgePR, th ReviewThread, body string) (string, error) {
	g.sm.Lock()
	defer g.sm.Unlock()
	if g.replyErr != nil {
		return "", g.replyErr
	}
	for i := range g.threads {
		if g.threads[i].ID == th.ID {
			cid := g.id()
			g.threads[i].Comments = append(g.threads[i].Comments, ThreadComment{ID: cid, Author: "waypoint-bot", Body: body})
			g.replies = append(g.replies, th.ID+": "+body)
			return cid, nil
		}
	}
	return "", errors.New("no such thread")
}

func (g *shipForge) UnresolveThread(ctx context.Context, f *Forge, repo string, pr *ForgePR, th ReviewThread) error {
	g.sm.Lock()
	defer g.sm.Unlock()
	if g.noResolv {
		return ErrUnsupported
	}
	for i := range g.threads {
		if g.threads[i].ID == th.ID {
			g.threads[i].Resolved, g.threads[i].ResolvedBy = false, ""
		}
	}
	return nil
}

func (g *shipForge) ResolveThread(ctx context.Context, f *Forge, repo string, pr *ForgePR, th ReviewThread) error {
	g.sm.Lock()
	defer g.sm.Unlock()
	if g.noResolv {
		return ErrUnsupported
	}
	for i := range g.threads {
		if g.threads[i].ID == th.ID {
			g.threads[i].Resolved, g.threads[i].ResolvedBy = true, "waypoint-bot"
			g.resolved = append(g.resolved, th.ID)
		}
	}
	return nil
}

func (g *shipForge) CommentReactions(ctx context.Context, f *Forge, repo string, n int, id string) ([]Reaction, error) {
	g.sm.Lock()
	defer g.sm.Unlock()
	if err := g.reactErr[id]; err != nil {
		return nil, err
	}
	return slices.Clone(g.reacts[id]), nil
}

// person acts on thread i as a person does on the forge.
func (g *shipForge) resolve(i int, by string) {
	g.sm.Lock()
	defer g.sm.Unlock()
	g.threads[i].Resolved, g.threads[i].ResolvedBy = true, by
}

func (g *shipForge) reply(i int, who, text string) string {
	g.sm.Lock()
	defer g.sm.Unlock()
	cid := g.id()
	g.threads[i].Comments = append(g.threads[i].Comments, ThreadComment{ID: cid, Author: who, Body: text, At: time.Now()})
	return cid
}

func (g *shipForge) thread(i int) ReviewThread {
	g.sm.Lock()
	defer g.sm.Unlock()
	t := g.threads[i]
	t.Comments = slices.Clone(t.Comments)
	return t
}

func (g *shipForge) SetCommitStatus(ctx context.Context, f *Forge, repo, sha string, st CommitStatusPost) error {
	g.sm.Lock()
	defer g.sm.Unlock()
	g.statuses[sha] = append(g.statuses[sha], st)
	if g.stamps == nil {
		g.stamps = map[string][]int64{}
	}
	g.seq++
	g.stamps[sha] = append(g.stamps[sha], 1_000_000+g.seq)
	return nil
}

// forgeStatus is a status another account posts on sha, newer than every
// status before it.
func (g *shipForge) forgeStatus(sha string, c CommitCheck) {
	g.sm.Lock()
	defer g.sm.Unlock()
	g.seq++
	c.ID, c.Source = 1_000_000+g.seq, "status"
	g.checks[sha] = append(g.checks[sha], c)
}

// postedOn is how many statuses Waypoint posted on sha.
func (g *shipForge) postedOn(sha string) int {
	g.sm.Lock()
	defer g.sm.Unlock()
	return len(g.statuses[sha])
}

func (g *shipForge) lastStatus(sha string) CommitStatusPost {
	g.sm.Lock()
	defer g.sm.Unlock()
	list := g.statuses[sha]
	if len(list) == 0 {
		return CommitStatusPost{}
	}
	return list[len(list)-1]
}

func (g *shipForge) CommitChecks(ctx context.Context, f *Forge, repo, sha string) ([]CommitCheck, error) {
	g.sm.Lock()
	defer g.sm.Unlock()
	out := slices.Clone(g.checks[sha])
	// A test may store a forge's raw state ("skipped", "queued"); map it
	// as the real client does. The Check* constants map to themselves.
	for i := range out {
		out[i].State = checkState(out[i].State)
	}
	// Waypoint's own status is among them, as on a real forge, posted by
	// the account of its token (prForge.TokenLogin).
	for i, st := range g.statuses[sha] {
		out = append(out, CommitCheck{Name: st.Context, State: checkState(st.State), Source: "status", Creator: "waypoint-bot", ID: g.stamps[sha][i]})
	}
	return out, nil
}

func (g *shipForge) setChecks(sha string, checks ...CommitCheck) {
	g.sm.Lock()
	defer g.sm.Unlock()
	g.checks[sha] = checks
}

func (g *shipForge) FailedLogs(ctx context.Context, f *Forge, repo, sha string, failed []CommitCheck) (map[string]string, error) {
	g.sm.Lock()
	defer g.sm.Unlock()
	out := map[string]string{}
	for _, c := range failed {
		if l, ok := g.logs[c.Name]; ok {
			out[c.Name] = l
		}
	}
	return out, nil
}

func (g *shipForge) MergePR(ctx context.Context, f *Forge, repo string, pr *ForgePR, o MergeOptions) error {
	g.sm.Lock()
	defer g.sm.Unlock()
	if g.mergeErr != nil {
		return g.mergeErr
	}
	g.merges = append(g.merges, o)
	g.prs[mk(f, repo, pr.Number)].State = "merged"
	g.prs[mk(f, repo, pr.Number)].MergeSHA = cmp(g.mergeSHAs[pr.Number], "4e7ced0ffee")
	if g.onMerge != nil {
		g.onMerge() // the base moves: set fields directly, sm is held
	}
	if g.onMergeN != nil {
		g.onMergeN(pr.Number)
	}
	return nil
}

// JobLog is the whole log: what FailedLogs keeps the tail of.
func (g *shipForge) JobLog(ctx context.Context, f *Forge, repo, sha string, c CommitCheck) (string, error) {
	g.sm.Lock()
	defer g.sm.Unlock()
	if l, ok := g.logs[sha+" "+c.Name]; ok {
		return "the start of the log\n" + l, nil
	}
	if l, ok := g.logs[c.Name]; ok {
		return "the start of the log\n" + l, nil
	}
	return "", errNoJobLog
}

func (g *shipForge) merged() []MergeOptions {
	g.sm.Lock()
	defer g.sm.Unlock()
	return slices.Clone(g.merges)
}

// shipEnv is a pull request environment on the shipping fake.
type shipEnv struct {
	*prEnv
	sg *shipForge
}

func newShipEnv(t *testing.T, kind string) *shipEnv {
	pe := newPREnv(t, kind)
	sg := newShipForge()
	pe.s.Forges = sg
	pe.fg = sg.prForge
	// Same models as the review tests.
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = oneFinding
	return &shipEnv{prEnv: pe, sg: sg}
}

// gate turns the review gate on at severity sev.
func (se *shipEnv) gate(sev string) {
	se.t.Helper()
	on := true
	if err := se.s.SetRepoGate(se.ctx, se.admin, se.p.Key, se.repo.ID, &on, &sev); err != nil {
		se.t.Fatal(err)
	}
	se.s.Wait()
}

func (se *shipEnv) pr(n int) *PullRequest {
	se.t.Helper()
	se.s.Wait()
	pr, err := se.s.prByNumber(se.ctx, se.s.db.R, se.f.ID, "me/app", n)
	if err != nil || pr == nil {
		se.t.Fatalf("pull request %d: %v", n, err)
	}
	return pr
}

func (se *shipEnv) findings(n int) []*findingRow {
	se.t.Helper()
	fs, err := se.s.prFindings(se.ctx, se.s.db.R, se.pr(n).ID)
	if err != nil {
		se.t.Fatal(err)
	}
	return fs
}

// events collects the events of kinds from the hub.
type eventLog struct {
	mu   sync.Mutex
	list []*Event
}

func (se *shipEnv) watch(kinds ...string) *eventLog {
	sub := se.s.Hub.Subscribe(se.admin.User.ID)
	l := &eventLog{}
	done := make(chan struct{})
	se.t.Cleanup(func() { se.s.Hub.Unsubscribe(sub); close(done) })
	go func() {
		for {
			select {
			case e := <-sub.C:
				if len(kinds) == 0 || slices.Contains(kinds, e.Kind) {
					l.mu.Lock()
					l.list = append(l.list, e)
					l.mu.Unlock()
				}
			case <-done:
				return
			}
		}
	}()
	return l
}

func (l *eventLog) all() []*Event {
	time.Sleep(20 * time.Millisecond) // the collector goroutine
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.list)
}

var testPR = ForgePR{Number: 5, Title: "Fix login", Author: "sam", HeadRef: "fix-login", HeadSHA: "1111111aaaa", State: "open"}

func TestReviewGateLifecycle(t *testing.T) {
	se := newShipEnv(t, "github")
	se.gate("P2")
	gateEvents := se.watch(EvPRReviewGate)

	se.pullHook("opened", testPR)
	se.s.Wait()
	// Pending while the review waits.
	if st := se.sg.lastStatus("1111111aaaa"); st.Context != "waypoint/review" || st.State != "pending" || st.Description != "Review in progress" ||
		!strings.Contains(st.TargetURL, "/agents?tab=pulls&pr=") {
		t.Fatalf("first status = %+v", st)
	}
	if done := se.drain(); len(done) != 1 || done[0].Status != "done" {
		t.Fatalf("review: %+v", done)
	}
	// A high finding is above P2: the gate fails, naming it.
	st := se.sg.lastStatus("1111111aaaa")
	if st.State != "failure" || !strings.HasPrefix(st.Description, "1 open finding: P1 The nil check is inverted") {
		t.Fatalf("status after review = %+v", st)
	}
	pr := se.pr(5)
	if pr.GateState != "failure" || pr.gateOpen != 1 || pr.gateSHA != "1111111aaaa" {
		t.Errorf("pull request = %+v", pr)
	}
	// The finding is linked to its comment and thread.
	fs := se.findings(5)
	if len(fs) != 1 || fs[0].threadID != "T1001" || fs[0].commentID != "1001" || fs[0].State != "open" || fs[0].headSHA != "1111111aaaa" {
		t.Fatalf("findings = %+v", fs)
	}

	// A person resolves the thread on the forge; Forgejo sends no event, so
	// the periodic check finds out.
	se.sg.resolve(0, "sam")
	se.setClock(se.clockNow().Add(3 * time.Minute))
	se.s.CheckGates(se.ctx)
	if st := se.sg.lastStatus("1111111aaaa"); st.State != "success" || st.Description != "All 1 finding resolved" {
		t.Fatalf("status after resolving = %+v", st)
	}
	fs = se.findings(5)
	if fs[0].State != FindingResolved {
		t.Fatalf("state = %s", fs[0].State)
	}
	hist, _ := se.s.findingHistory(se.ctx, se.pr(5).ID)
	if h := hist[fs[0].ID]; len(h) != 1 || h[0].Kind != "forge" || h[0].By != "sam" || h[0].State != FindingResolved {
		t.Errorf("history = %+v", h)
	}
	// Reopening the thread makes it block again.
	se.sg.sm.Lock()
	se.sg.threads[0].Resolved = false
	se.sg.sm.Unlock()
	se.s.checkPR(se.ctx, se.pr(5).ID, "test")
	if st := se.sg.lastStatus("1111111aaaa"); st.State != "failure" {
		t.Fatalf("reopened thread: %+v", st)
	}
	// Dismissing in Waypoint stops it blocking. An agent may, saying why
	// (WP-128).
	agent := agentActorFor(se.admin, "anthropic/claude-opus-5-5")
	var ie *InvalidError
	if _, err := se.s.DismissFinding(se.ctx, agent, fs[0].ID, " "); !errors.As(err, &ie) {
		t.Errorf("an agent dismissed a finding without a note: %v", err)
	}
	got, err := se.s.DismissFinding(se.ctx, agent, fs[0].ID, "the client already reconnects")
	if err != nil || got.State != FindingDismissed {
		t.Fatalf("agent dismiss: %+v %v", got, err)
	}
	se.s.Wait()
	if st := se.sg.lastStatus("1111111aaaa"); st.State != "success" {
		t.Fatalf("after an agent dismissed it: %+v", st)
	}
	hist, _ = se.s.findingHistory(se.ctx, se.pr(5).ID)
	if h := hist[fs[0].ID]; h[len(h)-1].Kind != "agent" || !strings.Contains(h[len(h)-1].By, "anthropic/claude-opus-5-5") || h[len(h)-1].Note != "the client already reconnects" {
		t.Errorf("agent dismissal history = %+v", h[len(h)-1])
	}
	if got, err := se.s.ReopenFinding(se.ctx, se.admin, fs[0].ID); err != nil || got.State != FindingOpen {
		t.Fatalf("reopen: %+v %v", got, err)
	}
	se.s.checkPR(se.ctx, se.pr(5).ID, "test")
	got, err = se.s.DismissFinding(se.ctx, se.admin, fs[0].ID, "")
	if err != nil || got.State != FindingDismissed {
		t.Fatalf("dismiss: %+v %v", got, err)
	}
	se.s.Wait()
	if st := se.sg.lastStatus("1111111aaaa"); st.State != "success" {
		t.Fatalf("after dismissing: %+v", st)
	}
	if got, err := se.s.ReopenFinding(se.ctx, se.admin, fs[0].ID); err != nil || got.State != FindingOpen {
		t.Fatalf("reopen: %+v %v", got, err)
	}
	// The gate's events: pending, failure, success, failure, success, failure.
	var states []string
	for _, e := range gateEvents.all() {
		states = append(states, e.Data["state"].(string))
	}
	if got := strings.Join(states, " "); !strings.HasPrefix(got, "pending failure success failure") {
		t.Errorf("gate events = %s", got)
	}
}

func mustAtoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		panic(err)
	}
	return n
}

// agentActorFor is a person's actor acting as an agent.
func agentActorFor(a *Actor, name string) *Actor {
	c := *a
	c.Agent = name
	return &c
}

func TestReviewGateSeverityAndPrivateFindings(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.gate("P1") // only critical and high
	se.sg.files[".forgejo/sync.toml"] = `private = ["deploy/**"]`
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"findings":[
		{"file":"app/login.go","line":11,"severity":"medium","claim":"Unchecked error from find.","suggestion":""},
		{"file":"deploy/secret.yaml","line":4,"severity":"critical","claim":"Password committed in plain text.","suggestion":""}]}`
	se.pullHook("opened", testPR)
	se.drain()
	st := se.sg.lastStatus("1111111aaaa")
	// The critical one blocks, but its text stays off a status GitHub may get.
	if st.State != "failure" || st.Description != "1 open finding: P0 (a private file)" {
		t.Fatalf("status = %+v", st)
	}
	fs := se.findings(5)
	if len(fs) != 2 {
		t.Fatalf("findings = %d", len(fs))
	}
	// Dismiss the critical one: the medium is below the gate.
	for _, f := range fs {
		if f.Severity == "critical" {
			if _, err := se.s.DismissFinding(se.ctx, se.admin, f.ID, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	se.s.Wait()
	if st := se.sg.lastStatus("1111111aaaa"); st.State != "success" || st.Description != "No blocking findings (1 below P1)" {
		t.Fatalf("status = %+v", st)
	}
	// Severity can be read as P or as a word.
	for _, in := range []string{"P0", "critical", " Medium "} {
		if _, ok := parseSeverity(in); !ok {
			t.Errorf("parseSeverity(%q)", in)
		}
	}
	if _, ok := parseSeverity("P5"); ok {
		t.Error("P5 is not a severity")
	}
	bad := "urgent"
	if err := se.s.SetRepoGate(se.ctx, se.admin, se.p.Key, se.repo.ID, nil, &bad); !IsInvalid(err) {
		t.Errorf("bad severity: %v", err)
	}
}

// A review with nothing to say posts no review; the gate's status is the
// head's sign-off (WP-234).
func TestReviewGateSignOff(t *testing.T) {
	se := newShipEnv(t, "github")
	se.gate("P2")
	for m := range se.llm.answers {
		se.llm.answers[m] = `{"findings":[]}`
	}
	se.pullHook("opened", testPR)
	if done := se.drain(); len(done) != 1 || done[0].Status != "done" || done[0].ReviewURL != "" {
		t.Fatalf("review: %+v", done)
	}
	se.s.Wait()
	if st := se.sg.lastStatus("1111111aaaa"); st.State != "success" || st.Description != "Reviewed: no issues found" {
		t.Fatalf("status = %+v", st)
	}
}

func TestReviewGateFailedAndOff(t *testing.T) {
	se := newShipEnv(t, "github")
	se.gate("P2")
	for m := range se.llm.answers {
		se.llm.answers[m] = "!timeout"
	}
	se.pullHook("opened", testPR)
	se.drain()
	for range botMaxAttempts {
		se.setClock(se.clockNow().Add(botRetryAfter + 1))
		se.drain()
	}
	se.s.Wait()
	// A review that never ran isn't a pass.
	if st := se.sg.lastStatus("1111111aaaa"); st.State != "failure" || !strings.Contains(st.Description, "didn't run") {
		t.Fatalf("status = %+v", st)
	}
	// Switching the gate off takes a failing status down.
	off := false
	if err := se.s.SetRepoGate(se.ctx, se.admin, se.p.Key, se.repo.ID, &off, nil); err != nil {
		t.Fatal(err)
	}
	se.s.Wait()
	if st := se.sg.lastStatus("1111111aaaa"); st.State != "success" || !strings.Contains(st.Description, "off") {
		t.Fatalf("status after switching off = %+v", st)
	}
	if pr := se.pr(5); pr.GateState != "" {
		t.Errorf("gate state = %q", pr.GateState)
	}
	repos, _ := se.s.ListProjectRepos(se.ctx, se.admin, se.p.Key)
	if repos[0].ReviewGate || repos[0].GateSeverity != "medium" {
		t.Errorf("repo = %+v", repos[0])
	}
	// Only a project owner changes it.
	viewer := se.user("vera", false)
	on := true
	if err := se.s.SetRepoGate(se.ctx, viewer, se.p.Key, se.repo.ID, &on, nil); err == nil {
		t.Error("a non-member changed the gate")
	}
}

func TestVerifyFixedFindings(t *testing.T) {
	se := newShipEnv(t, "github")
	se.gate("P2")
	se.sg.fileAt["1111111aaaa:app/login.go"] = "line1\nif user != nil {\nline3\n"
	se.sg.fileAt["2222222bbbb:app/login.go"] = "line1\nif user == nil {\nline3\n"
	se.pullHook("opened", testPR)
	se.drain()
	if st := se.sg.lastStatus("1111111aaaa"); st.State != "failure" {
		t.Fatalf("first status = %+v", st)
	}
	// A push changes the file; the model says the finding is fixed.
	// One answer for every call: no new findings, and the old one fixed.
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"findings":[],"verdict":"fixed","reason":"The comparison is now ==."}`
	pr := testPR
	pr.HeadSHA = "2222222bbbb"
	se.pullHook("synchronize", pr)
	done := se.drain()
	if len(done) != 1 || done[0].Status != "skipped" && done[0].Status != "done" {
		t.Fatalf("second review: %+v", done)
	}
	fs := se.findings(5)
	if fs[0].State != FindingFixed || fs[0].checkedSHA != "2222222bbbb" {
		t.Fatalf("finding = %+v", fs[0])
	}
	if len(se.sg.replies) != 1 || !strings.HasPrefix(se.sg.replies[0], "T1001: Fixed in `2222222`.") {
		t.Errorf("replies = %v", se.sg.replies)
	}
	if !slices.Contains(se.sg.resolved, "T1001") {
		t.Errorf("thread not resolved: %v", se.sg.resolved)
	}
	hist, _ := se.s.findingHistory(se.ctx, se.pr(5).ID)
	if h := hist[fs[0].ID]; len(h) != 1 || h[0].Kind != "verify" || h[0].State != FindingFixed || h[0].SHA != "2222222bbbb" || h[0].Note != "The comparison is now ==." {
		t.Errorf("history = %+v", h)
	}
	se.s.Wait()
	if st := se.sg.lastStatus("2222222bbbb"); st.State != "success" {
		t.Fatalf("status on the new head = %+v", st)
	}
	// The fixed finding no longer hides a look-alike if the problem comes back.
	found, repeated, err := se.s.dropRepeated(se.ctx, se.pr(5).ID, []Finding{{File: "app/login.go", Line: 11, Severity: "high", Claim: "The nil check is inverted, so every login fails."}}, nil)
	if err != nil || repeated != 0 || len(found) != 1 {
		t.Errorf("a fixed finding still suppresses: %v %d %v", found, repeated, err)
	}
}

// Failed model calls count toward the cap on one push's model calls.
func TestVerifyBoundsFailedModelCalls(t *testing.T) {
	se := newShipEnv(t, "github")
	for m := range se.llm.answers {
		se.llm.answers[m] = "!timeout"
	}
	se.sg.fileAt["1111111aaaa:a.go"] = "before\n"
	se.sg.fileAt["2222222bbbb:a.go"] = "after\n"
	var rows []*findingRow
	for i := 1; i <= 3*verifyMax; i++ {
		rows = append(rows, &findingRow{BotFinding: BotFinding{ID: int64(i), Path: "a.go", State: FindingOpen, Severity: "high", Claim: "c"}, headSHA: "1111111aaaa"})
	}
	chain, err := se.s.taskChain(se.ctx, "review")
	if err != nil || len(chain) == 0 {
		t.Fatalf("chain: %v %v", chain, err)
	}
	before := len(se.llm.calls)
	run := &BotRun{PR: &PullRequest{Repo: "me/app", ForgeID: se.f.ID}}
	out := se.s.verifyFindings(se.ctx, run, se.f, se.sg, &ForgePR{HeadSHA: "2222222bbbb"}, &syncResult{findings: rows}, chain)
	if calls := len(se.llm.calls) - before; calls == 0 || calls > verifyMax*len(chain) {
		t.Errorf("%d model calls for one push (cap %d findings x %d models)", calls, verifyMax, len(chain))
	}
	if out.checked != 0 {
		t.Errorf("failed calls counted as checked: %+v", out)
	}
}

func TestVerifyUnchangedAndForgejo(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.sg.noResolv = true // no resolve API
	se.gate("P2")
	se.sg.fileAt["1111111aaaa:app/login.go"] = "same\n"
	se.sg.fileAt["2222222bbbb:app/login.go"] = "same\n"
	se.pullHook("opened", testPR)
	se.drain()
	before := len(se.llm.calls)
	pr := testPR
	pr.HeadSHA = "2222222bbbb"
	se.pullHook("synchronize", pr)
	se.drain()
	// The file didn't change: no model call to check it, the finding stays.
	calls := se.llm.calls[before:]
	for _, c := range calls {
		_ = c
	}
	if fs := se.findings(5); fs[0].State != FindingOpen || fs[0].checkedSHA != "2222222bbbb" {
		t.Fatalf("finding = %+v", fs[0])
	}
	for _, p := range se.llm.prompts[before:] {
		if strings.Contains(p, "The file when the finding was raised") {
			t.Errorf("an unchanged file was put to a model:\n%s", p)
		}
	}
	// Changed, but the model can't tell: stays open; Forgejo can't resolve.
	se.sg.fileAt["3333333cccc:app/login.go"] = "different\n"
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"verdict":"unclear","reason":"Can't see the caller."}`
	pr.HeadSHA = "3333333cccc"
	se.pullHook("synchronize", pr)
	se.drain()
	if fs := se.findings(5); fs[0].State != FindingOpen || fs[0].checkedSHA != "3333333cccc" {
		t.Fatalf("finding = %+v", fs[0])
	}
	// Fixed on Forgejo: a reply, but the thread stays unresolved there.
	se.sg.fileAt["4444444dddd:app/login.go"] = "fixed\n"
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"verdict":"fixed","reason":"Gone."}`
	pr.HeadSHA = "4444444dddd"
	se.pullHook("synchronize", pr)
	se.drain()
	if fs := se.findings(5); fs[0].State != FindingFixed {
		t.Fatalf("finding = %+v", fs[0])
	}
	if len(se.sg.replies) != 1 || len(se.sg.resolved) != 0 {
		t.Errorf("replies %v resolved %v", se.sg.replies, se.sg.resolved)
	}
}

func TestReviewSeesTheConversation(t *testing.T) {
	se := newShipEnv(t, "github")
	se.pullHook("opened", testPR)
	se.drain()
	// A person answers the finding, and another reviewer leaves a comment.
	se.sg.reply(0, "sam", "This is deliberate: the caller checks.")
	se.sg.sm.Lock()
	se.sg.threads = append(se.sg.threads, ReviewThread{ID: "TX", Path: "app/login.go", Line: 12, Comments: []ThreadComment{{ID: "9", Author: "copilot-reviewer", Body: "Consider logging the user id here."}}})
	se.sg.sm.Unlock()
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"findings":[
		{"file":"app/login.go","line":12,"severity":"low","claim":"Consider logging the user id.","suggestion":""}]}`
	pr := testPR
	pr.HeadSHA = "2222222bbbb"
	se.pullHook("synchronize", pr)
	n := len(se.llm.prompts)
	// Sam's reply also queues an answer (which this test's model can't give).
	var done []*BotRun
	for _, r := range se.drain() {
		if r.Kind == "review" {
			done = append(done, r)
		}
	}
	if len(done) != 1 {
		t.Fatalf("runs: %+v", done)
	}
	var prompt string
	for _, p := range se.llm.prompts[n:] {
		if strings.Contains(p, "The diff:") {
			prompt = p
		}
	}
	for _, want := range []string{"Waypoint's earlier findings", "[open] P1 app/login.go:11", "sam said: This is deliberate", "Other reviewers' open comments",
		"copilot-reviewer on app/login.go:12", "Report only problems not already raised"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	// A new finding that restates another reviewer's open comment isn't posted.
	if done[0].Summary == "" || !strings.Contains(done[0].Summary, "1 already raised") {
		t.Errorf("summary = %q", done[0].Summary)
	}
}

func TestAnswerReplies(t *testing.T) {
	se := newShipEnv(t, "github")
	se.gate("P2")
	se.pullHook("opened", testPR)
	se.drain()
	// A person replies in the bot's thread: a webhook says a comment came.
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"action":"stand_by","reply":"find() returns nil when the user is unknown: see store.go:40."}`
	cid := se.sg.reply(0, "sam", "No, find() never returns nil here.")
	replyHook := func() {
		t.Helper()
		err := se.hook(se.f, "pull_request_review_comment", map[string]any{"action": "created",
			"pull_request": map[string]any{"number": 5, "title": "Fix login", "html_url": "https://git.example.com/me/app/pulls/5", "state": "open", "user": map[string]string{"login": "sam"},
				"head": map[string]string{"ref": "fix-login", "sha": "1111111aaaa"}, "base": map[string]string{"ref": "main"}},
			"comment":    map[string]any{"id": mustAtoi(cid), "body": "reply", "user": map[string]string{"login": "sam"}},
			"repository": map[string]any{"full_name": "me/app"}})
		if err != nil {
			t.Fatal(err)
		}
		se.s.Wait()
	}
	replyHook()
	runs := se.drain()
	if len(runs) != 1 || runs[0].Kind != "reply" || runs[0].Status != "done" {
		t.Fatalf("runs = %+v", runs)
	}
	// The model stood by it with evidence.
	if runs[0].Summary == "" {
		t.Error("no summary")
	}
	// Triggering again without a new comment queues nothing.
	replyHook()
	if len(se.drain()) != 0 {
		t.Error("answered the same comment twice")
	}
	// The thread now ends with the bot's answer.
	th := se.sg.thread(0)
	if last := th.Comments[len(th.Comments)-1]; !strings.Contains(last.Body, "<!-- waypoint-reply -->") {
		t.Errorf("last comment = %q", last.Body)
	}
}

func TestAnswerRepliesWithdrawAndResolved(t *testing.T) {
	se := newShipEnv(t, "github")
	se.gate("P2")
	se.pullHook("opened", testPR)
	se.drain()
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"action":"withdraw","reply":"You're right, the caller checks. Withdrawn."}`
	se.sg.reply(0, "sam", "find() is checked by the caller.")
	se.s.checkPR(se.ctx, se.pr(5).ID, "test")
	se.s.Wait()
	runs := se.drain()
	if len(runs) != 1 || runs[0].Kind != "reply" || runs[0].Status != "done" || !strings.Contains(runs[0].Summary, "Withdrew") {
		t.Fatalf("runs = %+v", runs)
	}
	fs := se.findings(5)
	if fs[0].State != FindingWithdrawn {
		t.Fatalf("finding = %+v", fs[0])
	}
	if !slices.Contains(se.sg.resolved, "T1001") {
		t.Errorf("a withdrawn finding's thread should be resolved: %v", se.sg.resolved)
	}
	se.s.Wait()
	if st := se.sg.lastStatus("1111111aaaa"); st.State != "success" {
		t.Errorf("status = %+v", st)
	}

	// A thread a person resolved is never argued again, even if they reply.
	se2 := newShipEnv(t, "github")
	se2.pullHook("opened", testPR)
	se2.drain()
	se2.sg.reply(0, "sam", "Still wrong, I think.")
	se2.sg.resolve(0, "sam")
	se2.s.checkPR(se2.ctx, se2.pr(5).ID, "test")
	se2.s.Wait()
	if runs := se2.drain(); len(runs) != 0 {
		t.Errorf("argued a resolved thread: %+v", runs)
	}
}

// A thread the forge reports with no comments (its only notes were system
// notes) is skipped, not indexed.
func TestAnswerRepliesToEmptyThread(t *testing.T) {
	se := newShipEnv(t, "github")
	se.gate("P2")
	se.pullHook("opened", testPR)
	se.drain()
	fs := se.findings(5)
	se.sg.sm.Lock()
	se.sg.threads[0].Comments = nil
	se.sg.sm.Unlock()
	run := &BotRun{PR: se.pr(5), Kind: "reply", options: fmt.Sprintf(`{"finding_id":%d}`, fs[0].ID)}
	_, err := se.s.replyRun(se.ctx, run)
	var skip *errSkip
	if !errors.As(err, &skip) {
		t.Errorf("replyRun on an empty thread = %v", err)
	}
}

func TestThumbsDownDismisses(t *testing.T) {
	se := newShipEnv(t, "github")
	se.gate("P2")
	se.pullHook("opened", testPR)
	se.drain()
	// The bot's own 👍 doesn't count; a person's 👎 does.
	se.sg.sm.Lock()
	se.sg.reacts["1001"] = []Reaction{{Emoji: "+1", User: "waypoint-bot"}, {Emoji: "-1", User: "sam"}}
	se.sg.sm.Unlock()
	se.s.checkPR(se.ctx, se.pr(5).ID, "test")
	fs := se.findings(5)
	if fs[0].State != FindingDismissed {
		t.Fatalf("finding = %+v", fs[0])
	}
	hist, _ := se.s.findingHistory(se.ctx, se.pr(5).ID)
	if h := hist[fs[0].ID]; len(h) != 1 || h[0].By != "sam" || h[0].Kind != "forge" {
		t.Errorf("history = %+v", h)
	}
	if st := se.sg.lastStatus("1111111aaaa"); st.State != "success" {
		t.Errorf("status = %+v", st)
	}
}

// One comment whose reactions can't be read (deleted, say) doesn't hide the
// 👎 on the findings after it.
func TestThumbsDownSurvivesOneFailedRead(t *testing.T) {
	se := newShipEnv(t, "github")
	se.gate("P2")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"findings":[
		{"file":"app/login.go","line":11,"severity":"high","claim":"Unchecked error from find.","suggestion":""},
		{"file":"app/login.go","line":12,"severity":"high","claim":"The token is logged in plain text.","suggestion":""}]}`
	se.pullHook("opened", testPR)
	se.drain()
	if len(se.sg.threads) != 2 {
		t.Fatalf("threads = %d", len(se.sg.threads))
	}
	se.sg.sm.Lock()
	se.sg.reactErr = map[string]error{se.sg.threads[0].Comments[0].ID: errors.New("404")}
	se.sg.reacts[se.sg.threads[1].Comments[0].ID] = []Reaction{{Emoji: "-1", User: "sam"}}
	se.sg.sm.Unlock()
	se.s.checkPR(se.ctx, se.pr(5).ID, "test")
	dismissed := 0
	for _, f := range se.findings(5) {
		if f.State == FindingDismissed {
			dismissed++
		}
	}
	if dismissed != 1 {
		t.Errorf("%d findings dismissed, want the second one", dismissed)
	}
}

// A reply that fails to post adds no comment, so the next person's reply is
// still new.
func TestFailedThreadReplyKeepsRepliesSeen(t *testing.T) {
	se := newShipEnv(t, "github")
	se.gate("P2")
	se.pullHook("opened", testPR)
	se.drain()
	pr := se.pr(5)
	fnd := se.findings(5)[0]
	se.sg.replyErr = errors.New("forge down")
	se.s.closeFindingThread(se.ctx, pr, fnd, "Fixed.", false)
	if got := se.findings(5)[0].repliesSeen; got != fnd.repliesSeen {
		t.Errorf("replies_seen %d -> %d after a failed reply", fnd.repliesSeen, got)
	}
	se.sg.replyErr = nil
	se.s.closeFindingThread(se.ctx, pr, fnd, "Fixed.", false)
	if got := se.findings(5)[0].repliesSeen; got != fnd.repliesSeen+1 {
		t.Errorf("replies_seen %d -> %d after a posted reply", fnd.repliesSeen, got)
	}
}

func TestAtWaypointCheck(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.gate("P2")
	se.pullHook("opened", testPR)
	se.drain()
	se.sg.resolve(0, "sam")
	err := se.hook(se.f, "issue_comment", map[string]any{"action": "created",
		"issue":      map[string]any{"number": 5, "pull_request": map[string]any{}},
		"comment":    map[string]any{"id": 77, "body": "@waypoint check", "user": map[string]string{"login": "sam"}},
		"repository": map[string]any{"full_name": "me/app"}})
	if err != nil {
		t.Fatal(err)
	}
	se.s.Wait()
	if st := se.sg.lastStatus("1111111aaaa"); st.State != "success" {
		t.Fatalf("status after @waypoint check = %+v", st)
	}
	if got := strings.Join(se.fg.reactions, " "); !strings.Contains(got, "+eyes@77") || !strings.Contains(got, "++1@77") {
		t.Errorf("reactions = %s", got)
	}
}

func TestSeverityHelpers(t *testing.T) {
	if !blocksGate("high", "medium") || blocksGate("low", "medium") || !blocksGate("medium", "medium") {
		t.Error("blocksGate")
	}
	if priority("critical") != "P0" || priority("low") != "P3" {
		t.Error("priority")
	}
	if got := shortClaim("The nil check is inverted. Every login fails afterwards.", 40); got != "The nil check is inverted" {
		t.Errorf("shortClaim = %q", got)
	}
	if got := clipRunes(strings.Repeat("é", 200), 10); len([]rune(got)) != 10 {
		t.Errorf("clipRunes = %q", got)
	}
	if got := fmt.Sprintf("%s %s %s", agentFamily("anthropic/claude-opus-5-5"), agentFamily("claude-code"), agentFamily("openai/codex")); got != "claude claude codex" {
		t.Errorf("agentFamily = %q", got)
	}
}

// BranchHead is the forge's word for where a branch is: tips, or tipErr.
// A branch with no tip set is one the forge can't say.
func (g *shipForge) BranchHead(ctx context.Context, f *Forge, repo, branch string) (string, error) {
	if g.tipHang {
		<-ctx.Done()
		return "", ctx.Err()
	}
	g.sm.Lock()
	defer g.sm.Unlock()
	if g.tipErr != nil {
		return "", g.tipErr
	}
	if sha, ok := g.tips[branch]; ok {
		return sha, nil
	}
	return "", ErrNotFound
}

func (g *shipForge) RepoTree(ctx context.Context, f *Forge, repo, sha string) ([]string, error) {
	return nil, ErrNotFound
}
