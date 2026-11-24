package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

const sampleDiff = `diff --git a/app/login.go b/app/login.go
index 1..2 100644
--- a/app/login.go
+++ b/app/login.go
@@ -10,4 +10,5 @@ func Login() {
 	user := find()
-	if user == nil {
+	if user != nil {
+		return errNoUser
 	}
 	return nil
diff --git a/go.sum b/go.sum
--- a/go.sum
+++ b/go.sum
@@ -1 +1 @@
-a v1
+a v2
diff --git a/deploy/secret.yaml b/deploy/secret.yaml
--- a/deploy/secret.yaml
+++ b/deploy/secret.yaml
@@ -3,2 +3,2 @@
 key: 1
-pass: x
+pass: y
diff --git a/old.txt b/old.txt
deleted file mode 100644
--- a/old.txt
+++ /dev/null
@@ -1 +0,0 @@
-gone
`

func TestSplitDiffAndAnnotate(t *testing.T) {
	files := splitDiff(sampleDiff)
	if len(files) != 4 {
		t.Fatalf("files = %d", len(files))
	}
	login := files[0]
	if login.path != "app/login.go" || login.firstChanged != 11 {
		t.Errorf("login: %q first %d", login.path, login.firstChanged)
	}
	for _, n := range []int{10, 11, 12, 13, 14} {
		if !login.right[n] {
			t.Errorf("line %d should be in the diff", n)
		}
	}
	if login.right[15] || login.right[9] {
		t.Error("lines outside the hunk counted")
	}
	if files[3].path != "old.txt" || len(files[3].right) != 0 {
		t.Errorf("deleted file: %+v", files[3])
	}
	ann := annotate(login.text)
	for _, want := range []string{"  10| \tuser := find()", "-     | \tif user == nil {", "+ 11| \tif user != nil {", "  14| \treturn nil"} {
		if !strings.Contains(ann, want) {
			t.Errorf("annotated diff lacks %q:\n%s", want, ann)
		}
	}
}

func TestGlobs(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"**/*.lock", "Cargo.lock", true},
		{"**/*.lock", "a/b/yarn.lock", true},
		{"docs/*", "docs/a.md", true},
		{"docs/*", "docs/a/b.md", false},
		{".forgejo/**", ".forgejo/workflows/ci.yml", true},
		{"deploy/{prod,staging}/**", "deploy/prod/x.env", true},
		{"deploy/{prod,staging}/**", "deploy/dev/x.env", false},
		{"file[!0-9].txt", "filea.txt", true},
		{"file[!0-9].txt", "file1.txt", false},
		{"weird{.txt", "weird{.txt", true},
		{"**/*.sops.env", "k8s/app.sops.env", true},
	}
	for _, c := range cases {
		re, err := globRE(c.glob)
		if err != nil {
			t.Fatalf("%s: %v", c.glob, err)
		}
		if got := re.MatchString(c.path); got != c.want {
			t.Errorf("%s ~ %s = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}

func TestSyncPrivate(t *testing.T) {
	got, err := syncPrivate(`# mirror
private = [
  "deploy/**", # infra
  'docs/internal/**',
]
publicize = []
privatize = ["legacy/*"]
`)
	if err != nil || strings.Join(got, ",") != "deploy/**,docs/internal/**,legacy/*" {
		t.Fatalf("private = %v, %v", got, err)
	}
	for _, bad := range []string{`private = "deploy/**"`, `private = [1, 2]`, `private = ["open`, `private = ["a"`} {
		if _, err := syncPrivate(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
	if got, err := syncPrivate(`publicize = []`); err != nil || len(got) != 0 {
		t.Errorf("no private list: %v %v", got, err)
	}
}

func TestReviewRules(t *testing.T) {
	md := "# Agents\nBuild with make.\n## Code Review Rules\n- No panics in handlers.\n### Go\n- Wrap errors.\n## Other\nIgnore me.\n"
	got := reviewRules("AGENTS.md", md)
	if !strings.Contains(got, "No panics") || !strings.Contains(got, "Wrap errors") || strings.Contains(got, "Ignore me") {
		t.Errorf("rules = %q", got)
	}
	if reviewRules("AGENTS.md", "# Agents\nnothing here") != "" {
		t.Error("AGENTS.md without a review section gave rules")
	}
	if reviewRules("REVIEW.md", "Be strict about SQL.") != "Be strict about SQL." {
		t.Error("REVIEW.md counts whole")
	}
}

func TestPlaceFindings(t *testing.T) {
	files := splitDiff(sampleDiff)
	private, _ := newGlobSet([]string{"deploy/**"})
	fs := normalizeFindings([]Finding{
		{File: "login.go", Line: 11, Severity: "HIGH", Claim: "  inverted\n nil check ", Suggestion: "if user == nil {"},
		{File: "app/login.go", Line: 40, Severity: "medium", Claim: "outside the diff"},
		{File: "deploy/secret.yaml", Line: 99, Severity: "low", Claim: "password in plain text"},
		{File: "deploy/nothing.yaml", Line: 1, Severity: "critical", Claim: "private, not in diff"},
		{File: "x.go", Line: 1, Severity: "bogus", Claim: ""},
	}, 8)
	if len(fs) != 4 || fs[0].Severity != "critical" || fs[1].Claim != "inverted nil check" {
		t.Fatalf("normalized: %+v", fs)
	}
	placed := placeFindings(fs, files, private)
	byClaim := map[string]placedFinding{}
	for _, p := range placed {
		byClaim[p.Claim] = p
	}
	if p := byClaim["inverted nil check"]; p.placement != "inline" || p.File != "app/login.go" || p.Suggestion == "" {
		t.Errorf("inline: %+v", p)
	}
	if p := byClaim["outside the diff"]; p.placement != "body" {
		t.Errorf("body: %+v", p)
	}
	if p := byClaim["password in plain text"]; p.placement != "inline" || p.Line != 4 || p.note != " (line 99)" {
		t.Errorf("private pinned to first changed line: %+v", p)
	}
	if p := byClaim["private, not in diff"]; p.placement != "withheld" {
		t.Errorf("private withheld: %+v", p)
	}
	body := reviewBody("abc1234", reviewOutcome{models: []string{"opencode/m"}, placed: placed, private: private, noise: 1, dropped: []string{"deploy/x", "big.go"}})
	for _, want := range []string{"<!-- waypoint-review sha=abc1234 -->", "2 findings inline", "`app/login.go:40`", "1 finding on private files left out", "1 lockfile", "2 files not reviewed", "`big.go`"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "deploy/") {
		t.Errorf("review body names a private path:\n%s", body)
	}
	if fitSuggestion("\tif user != nil {", "\tif user != nil {") != "" || fitSuggestion("  x := 1", "\t\ty := 2") != "\t\tx := 1" {
		t.Error("fitSuggestion")
	}
	if c := inlineComment(byClaim["inverted nil check"], ""); !strings.Contains(c, "**P1** high") || !strings.Contains(c, "```suggestion\n\tif user == nil {\n```") ||
		!strings.Contains(c, "<sub>**Automated review** by a model") || !strings.Contains(c, "<!-- waypoint-finding -->") {
		t.Errorf("inline comment:\n%s", c)
	}
	if c := inlineComment(byClaim["inverted nil check"], "Low-trust bot ({models}); check it."); !strings.Contains(c, "<sub>Low-trust bot (a model); check it.</sub>") {
		t.Errorf("inline comment with a custom note:\n%s", c)
	}
}

func TestReviewMarkers(t *testing.T) {
	posts := []ForgeComment{
		{Author: "waypoint-bot", Body: "x <!-- waypoint-review sha=aaaaaaa rule=3 -->"},
		{Author: "llm-review", Body: "<!-- llm-review:v1 sha=bbbbbbb model=m -->"},
		{Author: "waypoint-bot", Body: "<!-- waypoint-review sha=ccccccc -->"},
		{Author: "eve", Body: "<!-- waypoint-review sha=ddddddd rule=3 --> <!-- llm-review:v1 sha=eeeeeee -->"},
		{Author: "rogue[bot]", Body: "<!-- waypoint-review sha=fffffff rule=3 -->"},
	}
	ours := posters{logins: []string{"waypoint-bot"}}
	r3 := reviewedSHAs(posts, "3", ours)
	if !r3["aaaaaaa"] || !r3["bbbbbbb"] || r3["ccccccc"] || r3["ddddddd"] || r3["eeeeeee"] || r3["fffffff"] {
		t.Errorf("rule 3: %v", r3)
	}
	r4 := reviewedSHAs(posts, "4", ours)
	if r4["aaaaaaa"] || !r4["bbbbbbb"] {
		t.Errorf("rule 4: %v", r4)
	}
	if any := reviewedSHAs(posts, "*", ours); !any["ccccccc"] || any["ddddddd"] {
		t.Errorf("any rule: %v", any)
	}
}

func TestPRHelpers(t *testing.T) {
	if !similarClaim("The nil check is inverted, so login fails", "nil check inverted: every login fails") {
		t.Error("similar claims")
	}
	if similarClaim("SQL injection in search", "nil check inverted in login") {
		t.Error("different claims")
	}
	if effectiveAuthor("mirror", "_Opened on GitHub by **octo** as #4._\n\nbody") != "octo" || effectiveAuthor("robin", "x") != "robin" {
		t.Error("effective author")
	}
	for login, bot := range map[string]bool{"dependabot[bot]": true, "renovate": true, "renovate-bot": true, "robin": false, "robotics": false} {
		if isBotLogin(login) != bot {
			t.Errorf("isBotLogin(%s) = %v", login, !bot)
		}
	}
	open := &PullRequest{State: "open", HeadSHA: "b"}
	for _, c := range []struct {
		old  *PullRequest
		cur  *PullRequest
		want string
	}{
		{nil, open, "opened"},
		{&PullRequest{State: "open", HeadSHA: "a"}, open, "pushed"},
		{&PullRequest{State: "draft", HeadSHA: "b"}, open, "ready"},
		{&PullRequest{State: "closed", HeadSHA: "b"}, open, "reopened"},
		{&PullRequest{State: "open", HeadSHA: "b"}, open, ""},
		{&PullRequest{State: "open", HeadSHA: "a"}, &PullRequest{State: "merged", HeadSHA: "b"}, ""},
	} {
		if got := prChange(c.old, c.cur); got != c.want {
			t.Errorf("prChange(%+v) = %q, want %q", c.old, got, c.want)
		}
	}
	pr := &PullRequest{State: "draft", Author: "robin", HeadRef: "release-please--main", BaseRef: "main", Labels: []string{"dependencies"}}
	if why := reviewFilter.skip(pr, EvPRUpdated); why != "a draft" {
		t.Errorf("draft: %q", why)
	}
	pr.State = "open"
	if why := reviewFilter.skip(pr, EvPRUpdated); why != "labelled dependencies" {
		t.Errorf("label: %q", why)
	}
	pr.Labels = nil
	if why := reviewFilter.skip(pr, EvPRUpdated); !strings.HasPrefix(why, "from ") {
		t.Errorf("head: %q", why)
	}
	pr.HeadRef, pr.Author = "fix", "renovate[bot]"
	if why := reviewFilter.skip(pr, EvPRUpdated); !strings.Contains(why, "bot") {
		t.Errorf("bot: %q", why)
	}
	if why := reviewFilter.skip(pr, EvPRCommand); why != "" {
		t.Errorf("a person asking reviews a bot's pull request: %q", why)
	}
}

// prForge adds pull requests to memForge.
type prForge struct {
	*memForge
	mu        sync.Mutex
	prs       map[string]*ForgePR
	diffs     map[string]string
	files     map[string]string
	reviews   map[string][]ForgeComment
	lines     [][]LineComment
	reactions []string
	refuse    bool
	tokens    []string
	listed    []*ForgePR
	listErr   error
	ownLogin  string
}

func newPRForge() *prForge {
	return &prForge{memForge: newMemForge(), prs: map[string]*ForgePR{}, diffs: map[string]string{}, files: map[string]string{}, reviews: map[string][]ForgeComment{}}
}

func (p *prForge) GetPR(ctx context.Context, f *Forge, repo string, n int) (*ForgePR, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pr := p.prs[mk(f, repo, n)]; pr != nil {
		c := *pr
		return &c, nil
	}
	return nil, &forgeAPIError{method: "GET", path: "/pulls", status: http.StatusNotFound}
}

func (p *prForge) PRDiff(ctx context.Context, f *Forge, repo string, pr *ForgePR) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.diffs[pr.HeadSHA], nil
}

func (p *prForge) DirAt(ctx context.Context, f *Forge, repo, path, ref string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for name := range p.files {
		if rest, ok := strings.CutPrefix(name, path+"/"); ok && !strings.Contains(rest, "/") {
			out = append(out, name)
		}
	}
	return out, nil
}

func (p *prForge) FileAt(ctx context.Context, f *Forge, repo, path, ref string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.files[path]; ok {
		return c, nil
	}
	return "", ErrNotFound
}

func (p *prForge) ReviewBodies(ctx context.Context, f *Forge, repo string, n int) ([]ForgeComment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ForgeComment(nil), p.reviews[mk(f, repo, n)]...), nil
}

func (p *prForge) PostReview(ctx context.Context, f *Forge, repo string, pr *ForgePR, body string, comments []LineComment) (*PostedReview, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refuse && len(comments) > 0 {
		p.refuse = false
		return nil, errors.New("HTTP 422 line not in diff")
	}
	author, _ := p.TokenLogin(ctx, f)
	p.reviews[mk(f, repo, pr.Number)] = append(p.reviews[mk(f, repo, pr.Number)], ForgeComment{Author: author, Body: body})
	p.lines = append(p.lines, comments)
	p.tokens = append(p.tokens, f.token)
	return &PostedReview{ID: "1", URL: pr.URL + "#review"}, nil
}

func (p *prForge) React(ctx context.Context, f *Forge, repo string, n int, commentID, emoji string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	on := cmp(commentID, "pr")
	p.reactions = append(p.reactions, "+"+emoji+"@"+on)
	return emoji + on, nil
}

func (p *prForge) Unreact(ctx context.Context, f *Forge, repo string, n int, commentID, emoji, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reactions = append(p.reactions, "-"+emoji+"@"+cmp(commentID, "pr"))
	return nil
}

func (p *prForge) PostPRComment(ctx context.Context, f *Forge, repo string, n int, body string) (*ForgeComment, error) {
	return p.PostComment(ctx, f, repo, n, body)
}

func (p *prForge) ListPRs(ctx context.Context, f *Forge, repo string) ([]*ForgePR, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listErr != nil {
		return nil, p.listErr
	}
	return p.listed, nil
}

func (p *prForge) setPR(f *Forge, repo string, pr ForgePR, diff string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prs[mk(f, repo, pr.Number)] = &pr
	p.diffs[pr.HeadSHA] = diff
}

func (p *prForge) reviewsOn(f *Forge, repo string, n int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, c := range p.reviews[mk(f, repo, n)] {
		out = append(out, c.Body)
	}
	return out
}

// scriptLLM answers with each model's scripted reply, or fails as told.
type scriptLLM struct {
	mu      sync.Mutex
	answers map[string]string
	calls   []string
	prompts []string
	asks    []gatewayAsk
	exclude [][]string
	windows map[string]int
	reports []feedbackReport
	report  error
}

func (l *scriptLLM) Feedback(ctx context.Context, p *AIProvider, r feedbackReport) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.report != nil {
		return l.report
	}
	l.reports = append(l.reports, r)
	return nil
}

func (l *scriptLLM) Models(ctx context.Context, p *AIProvider) ([]AIModel, error) {
	ms := []AIModel{
		{ID: "opencode/muse-spark-1.2-free", Free: true}, {ID: "opencode/muse-spark-1.3-contributor-free", Free: true},
		{ID: "opencode/nemotron-3-ultra-free", Free: true}, {ID: "opencode/space-bunny-free", Free: true}, {ID: "opencode/mimo-v2.6-flash-free", Free: true},
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range ms {
		ms[i].ContextLength = l.windows[ms[i].ID]
	}
	return ms, nil
}

func (l *scriptLLM) Complete(ctx context.Context, p *AIProvider, model, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, model)
	l.prompts = append(l.prompts, prompt)
	var ask gatewayAsk
	if g := gatewayAskOf(ctx); g != nil {
		ask = *g
	}
	l.asks = append(l.asks, ask)
	l.exclude = append(l.exclude, gatewayExcludeOf(ctx))
	a, ok := l.answers[model]
	switch {
	case !ok:
		return nil, "", &llmError{status: 404, body: "unknown model"}
	case a == "!429":
		return nil, "", &llmError{status: 429, body: "rate limited"}
	case a == "!timeout":
		return nil, "", context.DeadlineExceeded
	}
	return json.RawMessage(a), "", nil
}

type prEnv struct {
	*env
	fg   *prForge
	llm  *scriptLLM
	f    *Forge
	p    *Project
	repo *ProjectRepo
}

func newPREnv(t *testing.T, kind string) *prEnv {
	e := newEnv(t)
	fg := newPRForge()
	e.s.Forges = fg
	llm := &scriptLLM{answers: map[string]string{}}
	e.s.LLM = llm
	e.s.rnd = func() float64 { return 0.99 }
	e.s.beta = func(a, b float64) float64 { return a / (a + b) }
	e.s.trustSeeds = []string{}
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetAITask(e.ctx, e.admin, "verify_review", 0, "", nil); err != nil {
		t.Fatal(err)
	}
	f := e.forge(kind, "https://git.example.com", false)
	p := e.project(e.admin, "App", "")
	r, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "me/app", false)
	if err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	return &prEnv{env: e, fg: fg, llm: llm, f: f, p: p, repo: r}
}

// pullHook sends a pull_request webhook for pr.
func (pe *prEnv) pullHook(action string, pr ForgePR) {
	pe.t.Helper()
	pe.fg.setPR(pe.f, "me/app", pr, sampleDiff)
	headRepo := cmp(pr.HeadRepo, "me/app")
	payload := map[string]any{
		"action": action,
		"pull_request": map[string]any{
			"number": pr.Number, "title": pr.Title, "body": pr.Body, "html_url": "https://git.example.com/me/app/pulls/" + fmt.Sprint(pr.Number),
			"state": map[bool]string{true: "closed", false: "open"}[pr.State == "closed" || pr.State == "merged"], "merged": pr.State == "merged", "draft": pr.State == "draft",
			"user": map[string]string{"login": pr.Author}, "head": map[string]any{"ref": pr.HeadRef, "sha": pr.HeadSHA, "repo": map[string]string{"full_name": headRepo}}, "base": map[string]string{"ref": "main"},
		},
		"repository": map[string]any{"full_name": "me/app"},
	}
	if err := pe.hook(pe.f, "pull_request", payload); err != nil {
		pe.t.Fatal(err)
	}
}

// drain carries out every queued run, the way RunBots does.
func (pe *prEnv) drain() []*BotRun {
	pe.t.Helper()
	var done []*BotRun
	for range 20 {
		r, err := pe.s.claimRun(pe.ctx)
		if err != nil {
			pe.t.Fatal(err)
		}
		if r == nil {
			break
		}
		pe.s.carryOut(pe.ctx, r)
		got, _ := pe.s.botRun(pe.ctx, r.ID)
		done = append(done, got)
	}
	return done
}

func (pe *prEnv) runs() []*BotRun {
	pe.t.Helper()
	rs, err := pe.s.ListBotRuns(pe.ctx, pe.admin, pe.p.Key, 50)
	if err != nil {
		pe.t.Fatal(err)
	}
	return rs
}

const oneFinding = `{"findings":[{"file":"app/login.go","line":11,"severity":"high","claim":"The nil check is inverted, so every login fails.","suggestion":"\tif user == nil {"}]}`

func TestReviewPullRequests(t *testing.T) {
	pe := newPREnv(t, "github")
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = "!429"
	pe.llm.answers["opencode/nemotron-3-ultra-free"] = oneFinding
	pr := ForgePR{Number: 5, Title: "Fix login", Author: "sam", HeadRef: "fix-login", HeadSHA: "1111111aaaa", State: "open"}
	pe.pullHook("opened", pr)
	done := pe.drain()
	if len(done) != 1 || done[0].Status != "done" {
		t.Fatalf("runs = %+v", done)
	}
	run := done[0]
	if len(run.Log) != 2 || run.Log[0].Model != "opencode/muse-spark-1.3-contributor-free" || run.Log[0].Outcome != "rate_limit" || run.Log[1].Outcome != "ok" {
		t.Errorf("log = %+v", run.Log)
	}
	reviews := pe.fg.reviewsOn(pe.f, "me/app", 5)
	if len(reviews) != 1 || !strings.Contains(reviews[0], "sha=1111111aaaa rule=") || !strings.Contains(reviews[0], "1 finding inline") {
		t.Fatalf("reviews = %q", reviews)
	}
	if lc := pe.fg.lines[0]; len(lc) != 1 || lc[0].Path != "app/login.go" || lc[0].Line != 11 || !strings.Contains(lc[0].Body, "```suggestion") {
		t.Errorf("line comments = %+v", lc)
	}
	if got := strings.Join(pe.fg.reactions, " "); got != "+eyes@pr -eyes@pr" {
		t.Errorf("reactions = %s", got)
	}
	if len(run.Findings) != 1 || run.Findings[0].Placement != "inline" || run.ReviewURL == "" {
		t.Errorf("run = %+v", run)
	}
	if p := pe.llm.prompts[0]; !strings.Contains(p, "+ 11| \tif user != nil {") || strings.Contains(p, "a v2") {
		t.Errorf("prompt:\n%s", p)
	}

	pe.pullHook("edited", pr)
	if len(pe.drain()) != 0 {
		t.Error("an edit queued a review")
	}
	pr.HeadSHA = "2222222bbbb"
	pe.pullHook("synchronize", pr)
	done = pe.drain()
	if len(done) != 1 || done[0].Status != "done" || !strings.Contains(done[0].Summary, "1 already raised") {
		t.Fatalf("second review: %+v", done)
	}
	if r := pe.fg.reviewsOn(pe.f, "me/app", 5); len(r) != 1 {
		t.Errorf("a review with nothing to say was posted: %q", r)
	}
	if got := pe.fg.reactions[len(pe.fg.reactions)-1]; got != "+1@pr" && got != "++1@pr" {
		t.Errorf("last reaction = %s", got)
	}
	if done[0].ReviewURL != "" || !strings.HasPrefix(done[0].Summary, "No issues found") {
		t.Errorf("signed-off run = %+v", done[0])
	}
	if err := pe.hook(pe.f, "issue_comment", map[string]any{"action": "created",
		"issue":      map[string]any{"number": 5, "pull_request": map[string]any{}},
		"comment":    map[string]any{"id": 77, "body": "@waypoint review", "user": map[string]string{"login": "sam"}},
		"repository": map[string]any{"full_name": "me/app"}}); err != nil {
		t.Fatal(err)
	}
	if again := pe.drain(); len(again) != 1 || again[0].Status != "skipped" || !strings.Contains(again[0].Summary, "already reviewed at 2222222") {
		t.Fatalf("review at a signed-off head: %+v", again)
	}

	draft := ForgePR{Number: 6, Title: "WIP", Author: "sam", HeadRef: "wip", HeadSHA: "3333333cccc", State: "draft"}
	pe.pullHook("opened", draft)
	if len(pe.drain()) != 0 {
		t.Error("a draft was reviewed")
	}
	draft.State = "open"
	pe.pullHook("ready_for_review", draft)
	if done := pe.drain(); len(done) != 1 || done[0].Reason != "ready" {
		t.Errorf("ready: %+v", done)
	}
	pe.pullHook("opened", ForgePR{Number: 7, Title: "Bump x", Author: "renovate[bot]", HeadRef: "renovate/x", HeadSHA: "4444444dddd", State: "open"})
	if len(pe.drain()) != 0 {
		t.Error("a bot's pull request was reviewed")
	}
	off := false
	if err := pe.s.UpdateProjectRepo(pe.ctx, pe.admin, pe.p.Key, pe.repo.ID, nil, nil, &off); err != nil {
		t.Fatal(err)
	}
	pe.pullHook("opened", ForgePR{Number: 8, Title: "Other", Author: "sam", HeadRef: "o", HeadSHA: "5555555eeee", State: "open"})
	if len(pe.drain()) != 0 {
		t.Error("reviewed with bots off")
	}
}

// A push a non-firing re-read (a merge check, @waypoint check, wp pr wait)
// sees before its webhook still gets one review on the new head (WP-129).
func TestPushSeenFirstByReread(t *testing.T) {
	pe := newPREnv(t, "github")
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = oneFinding
	pr := ForgePR{Number: 5, Title: "Fix login", Author: "sam", HeadRef: "fix-login", HeadSHA: "1111111aaaa", State: "open"}
	pe.pullHook("opened", pr)
	if done := pe.drain(); len(done) != 1 {
		t.Fatalf("first review: %+v", done)
	}
	rec, err := pe.s.prByNumber(pe.ctx, pe.s.db.R, pe.f.ID, "me/app", 5)
	if err != nil || rec == nil {
		t.Fatal(rec, err)
	}
	if _, err := pe.s.db.W.ExecContext(pe.ctx, `UPDATE pull_requests SET armed_at = ? WHERE id = ?`, pe.s.nowMS(), rec.ID); err != nil {
		t.Fatal(err)
	}
	pr.HeadSHA = "2222222bbbb"
	pe.fg.setPR(pe.f, "me/app", pr, sampleDiff)
	if _, err := pe.s.CheckPullRequest(pe.ctx, pe.admin, rec.ID); err != nil {
		t.Fatal(err)
	}
	pe.s.bg.Wait()
	pe.pullHook("synchronize", pr)
	pe.s.bg.Wait()
	done := pe.drain()
	if len(done) != 1 || done[0].HeadSHA != "2222222bbbb" || done[0].Reason != "pushed" {
		t.Fatalf("runs after the push = %+v", done)
	}
	if _, err := pe.s.CheckPullRequest(pe.ctx, pe.admin, rec.ID); err != nil {
		t.Fatal(err)
	}
	pe.s.bg.Wait()
	if again := pe.drain(); len(again) != 0 {
		t.Errorf("a re-read at the same head queued %+v", again)
	}
	stale := pr
	stale.HeadSHA = "1111111aaaa"
	if _, err := pe.s.recordPR(pe.ctx, pe.f, "me/app", &stale, false); err != nil {
		t.Fatal(err)
	}
	if _, err := pe.s.CheckPullRequest(pe.ctx, pe.admin, rec.ID); err != nil {
		t.Fatal(err)
	}
	pe.s.bg.Wait()
	if again := pe.drain(); len(again) != 0 {
		t.Errorf("a stale read queued %+v", again)
	}
}

func TestReviewCommandsAndRetries(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = oneFinding
	pr := ForgePR{Number: 9, Title: "Fix", Author: "sam", HeadRef: "fix", HeadSHA: "aaaaaaa1111", State: "open"}
	pe.pullHook("opened", pr)
	pe.drain()
	comment := func(id int, body string) {
		t.Helper()
		err := pe.hook(pe.f, "issue_comment", map[string]any{"action": "created",
			"issue":      map[string]any{"number": 9, "pull_request": map[string]any{}},
			"comment":    map[string]any{"id": id, "body": body, "user": map[string]string{"login": "sam"}},
			"repository": map[string]any{"full_name": "me/app"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	comment(1, "@waypoint review please")
	done := pe.drain()
	if len(done) != 1 || done[0].Status != "skipped" || !strings.Contains(done[0].Summary, "already reviewed") {
		t.Fatalf("review again: %+v", *done[0])
	}
	if posted := pe.fg.postedTo(pe.f); len(posted) != 1 || !strings.Contains(posted[0], "full review") {
		t.Errorf("a skipped request should say why: %v", posted)
	}
	pe.fg.reactions = nil
	comment(2, "Thanks!\n@waypoint full review")
	done = pe.drain()
	if len(done) != 1 || done[0].Status != "done" {
		t.Fatalf("full review: %+v", done)
	}
	if got := strings.Join(pe.fg.reactions, " "); !strings.Contains(got, "+eyes@2") || !strings.Contains(got, "-eyes@2") || !strings.Contains(got, "++1@2") {
		t.Errorf("comment reactions = %s", got)
	}
	comment(3, "@waypoint pause")
	pr.HeadSHA = "bbbbbbb2222"
	pe.pullHook("synchronize", pr)
	if len(pe.drain()) != 0 {
		t.Error("reviewed while paused")
	}
	comment(4, "@waypoint resume")
	pr.HeadSHA = "ccccccc3333"
	pe.pullHook("synchronize", pr)
	if len(pe.drain()) != 1 {
		t.Error("not reviewed after resume")
	}
	comment(5, "@waypoint help")
	if posted := pe.fg.postedTo(pe.f); len(posted) == 0 || !strings.Contains(posted[len(posted)-1], "@waypoint full review") {
		t.Errorf("help: %v", posted)
	}

	for m := range pe.llm.answers {
		pe.llm.answers[m] = "!timeout"
	}
	pr.HeadSHA = "ddddddd4444"
	pe.pullHook("synchronize", pr)
	done = pe.drain()
	if len(done) != 1 || done[0].Status != "failed" || !strings.Contains(done[0].Error, "unavailable") {
		t.Fatalf("provider errors should end the run: %+v", *done[0])
	}
	pe.setClock(pe.clockNow().Add(botRetryAfter + 1))
	if len(pe.drain()) != 0 {
		t.Error("a failed run was tried again")
	}
	runs := pe.runs()
	if runs[0].Status != "failed" || runs[0].Attempts != 1 {
		t.Errorf("gave up: %+v", runs[0])
	}
	if got := pe.fg.reactions[len(pe.fg.reactions)-1]; got != "+confused@pr" {
		t.Errorf("last reaction = %s", got)
	}
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"findings":[]}`
	again, err := pe.s.RetryBotRun(pe.ctx, pe.admin, runs[0].ID)
	if err != nil || again.Status != "queued" {
		t.Fatalf("retry: %+v %v", again, err)
	}
	if done := pe.drain(); len(done) != 1 || done[0].Status != "done" {
		t.Errorf("retried: %+v", done)
	}

	agent := &Actor{User: pe.admin.User, Agent: "anthropic/claude-opus-5-5"}
	pr.HeadSHA = "eeeeeee5555"
	pe.pullHook("synchronize", pr)
	live := pe.runs()[0]
	if live.Status != "queued" || live.HeadSHA != pr.HeadSHA {
		t.Fatalf("the new head's run: %+v", live)
	}
	if _, err := pe.s.CancelBotRun(pe.ctx, agent, live.ID); err == nil || !strings.Contains(err.Error(), "agents can't cancel the review of a pull request's head") {
		t.Errorf("an agent cancelled the head's review: %v", err)
	}
	if _, err := pe.s.db.W.ExecContext(pe.ctx, `UPDATE bot_runs SET head_sha = 'EEEEEEE' WHERE id = ?`, live.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pe.s.CancelBotRun(pe.ctx, agent, live.ID); err == nil || !strings.Contains(err.Error(), "agents can't cancel the review of a pull request's head") {
		t.Errorf("an agent cancelled the head's review by its short upper-case commit: %v", err)
	}
	if _, err := pe.s.db.W.ExecContext(pe.ctx, `UPDATE pull_requests SET outside = 1 WHERE id = ?`, live.PR.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pe.s.RetryBotRun(pe.ctx, agent, runs[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("an agent retried a run on an outside pull request: %v", err)
	}
	if got, err := pe.s.CancelBotRun(pe.ctx, pe.admin, live.ID); err != nil || got.Status != "cancelled" {
		t.Errorf("a person cancels the head's review: %+v %v", got, err)
	}
	cancels := func() (n int) {
		_ = pe.s.db.R.QueryRowContext(pe.ctx, `SELECT COUNT(*) FROM audit_log WHERE action = 'bot.cancel'`).Scan(&n)
		return n
	}
	before := cancels()
	if got, err := pe.s.cancelRun(pe.ctx, pe.admin, live); !IsInvalid(err) {
		t.Errorf("cancelled a run that had finished: %+v %v", got, err)
	}
	if cancels() != before {
		t.Error("audited a cancel that didn't happen")
	}
}

func TestReviewPrivatePaths(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	pe.fg.files[".forgejo/sync.toml"] = `private = ["deploy/**"]`
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"findings":[
		{"file":"deploy/secret.yaml","line":50,"severity":"critical","claim":"Password committed in plain text.","suggestion":""},
		{"file":"app/login.go","line":99,"severity":"low","claim":"Unused result.","suggestion":""}]}`
	pe.fg.refuse = true
	pe.pullHook("opened", ForgePR{Number: 3, Title: "x", Author: "sam", HeadRef: "x", HeadSHA: "eeeeeee5555", State: "open"})
	done := pe.drain()
	if len(done) != 1 || done[0].Status != "done" {
		t.Fatalf("runs = %+v", done)
	}
	body := pe.fg.reviewsOn(pe.f, "me/app", 3)[0]
	if strings.Contains(body, "deploy/") || strings.Contains(body, "Password") {
		t.Errorf("a private finding reached the review body:\n%s", body)
	}
	if !strings.Contains(body, "`app/login.go:99`") || !strings.Contains(body, "1 finding on private files left out") {
		t.Errorf("body:\n%s", body)
	}
	pe.fg.files[".forgejo/sync.toml"] = `private = "deploy/**"`
	pe.pullHook("opened", ForgePR{Number: 4, Title: "y", Author: "sam", HeadRef: "y", HeadSHA: "fffffff6666", State: "open"})
	done = pe.drain()
	if len(done) != 1 || done[0].Status != "skipped" || !strings.Contains(done[0].Summary, "sync.toml") {
		t.Errorf("unreadable sync.toml: %+v", done)
	}
	if len(pe.fg.reviewsOn(pe.f, "me/app", 4)) != 0 {
		t.Error("posted despite an unreadable sync.toml")
	}
}

// The paths a forge's mirror keeps private are the forge's setting, on any
// kind of forge (WP-459); a forge without one withholds nothing, and a glob
// that doesn't parse isn't saved.
func TestReviewMirrorPrivate(t *testing.T) {
	pe := newPREnv(t, "github")
	if _, err := pe.s.UpdateForge(pe.ctx, pe.admin, pe.f.ID, ForgeInput{MirrorPrivate: &[]string{"deploy/[z-a]"}}); err == nil {
		t.Error("a glob that doesn't parse was saved")
	}
	findings := `{"findings":[
		{"file":"deploy/secret.yaml","line":50,"severity":"critical","claim":"Password committed in plain text.","suggestion":""},
		{"file":"app/login.go","line":99,"severity":"low","claim":"Unused result.","suggestion":""}]}`
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = findings
	pe.fg.refuse = true
	pe.pullHook("opened", ForgePR{Number: 2, Title: "w", Author: "sam", HeadRef: "w", HeadSHA: "ddddddd4444", State: "open"})
	if done := pe.drain(); len(done) != 1 || done[0].Status != "done" {
		t.Fatalf("runs = %+v", done)
	}
	if body := pe.fg.reviewsOn(pe.f, "me/app", 2)[0]; !strings.Contains(body, "Password") {
		t.Errorf("with no mirror a finding was withheld:\n%s", body)
	}

	if f, err := pe.s.UpdateForge(pe.ctx, pe.admin, pe.f.ID, ForgeInput{MirrorPrivate: &[]string{"deploy/**  *.key"}}); err != nil {
		t.Fatal(err)
	} else if strings.Join(f.MirrorPrivate, ",") != "deploy/**,*.key" {
		t.Errorf("saved %q", f.MirrorPrivate)
	}
	pe.fg.refuse = true
	pe.pullHook("opened", ForgePR{Number: 3, Title: "x", Author: "sam", HeadRef: "x", HeadSHA: "eeeeeee5555", State: "open"})
	if done := pe.drain(); len(done) != 1 || done[0].Status != "done" {
		t.Fatalf("runs = %+v", done)
	}
	body := pe.fg.reviewsOn(pe.f, "me/app", 3)[0]
	if strings.Contains(body, "deploy/") || strings.Contains(body, "Password") {
		t.Errorf("a private finding reached the review body:\n%s", body)
	}
	if !strings.Contains(body, "`app/login.go:99`") || !strings.Contains(body, "1 finding on private files left out") {
		t.Errorf("body:\n%s", body)
	}
}

func TestReviewBotToken(t *testing.T) {
	pe := newPREnv(t, "github")
	if _, err := pe.s.UpdateForge(pe.ctx, pe.admin, pe.f.ID, ForgeInput{BotToken: str("bot-tok")}); err != nil {
		t.Fatal(err)
	}
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = oneFinding
	pe.pullHook("opened", ForgePR{Number: 1, Title: "x", Author: "sam", HeadRef: "x", HeadSHA: "abcdef01234", State: "open"})
	pe.drain()
	if len(pe.fg.tokens) != 1 || pe.fg.tokens[0] != "bot-tok" {
		t.Errorf("review posted with %v", pe.fg.tokens)
	}
	forges, _ := pe.s.ListForges(pe.ctx, pe.admin)
	if !forges[0].HasBotToken {
		t.Error("has_bot_token not reported")
	}
}
