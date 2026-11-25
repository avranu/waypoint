package core

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

// WP-255: follow-ups are a relation people and agents set, and one Waypoint
// records for what an agent files while working on a task.
func TestFollowUps(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Follow", "")
	first := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Speed up the board", Agent: "claude"})
	later := e.issue(e.admin, IssueInput{Project: p.Key, Title: "The board is slow again on big projects"})
	rel := func(key string) []Relation {
		t.Helper()
		return e.get(e.admin, key).Relations
	}
	has := func(rs []Relation, kind, key, found string) bool {
		return slices.ContainsFunc(rs, func(r Relation) bool { return r.Kind == kind && r.Issue.Key == key && r.Found == found })
	}

	if err := e.s.AddRelation(e.ctx, e.admin, later.Key, "follows", first.Key); err != nil {
		t.Fatal(err)
	}
	if !has(rel(later.Key), "follows", first.Key, "") || !has(rel(first.Key), "followed_by", later.Key, "") {
		t.Errorf("follows: %+v / %+v", rel(later.Key), rel(first.Key))
	}
	other := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Another pass on board speed"})
	if err := e.s.AddRelation(e.ctx, e.admin, first.Key, "followed_by", other.Key); err != nil {
		t.Fatal(err)
	}
	if !has(rel(other.Key), "follows", first.Key, "") {
		t.Errorf("followed_by stored the wrong way: %+v", rel(other.Key))
	}
	search := func(q string) []string {
		t.Helper()
		res, err := e.s.Search(e.ctx, e.admin, SearchParams{Query: q, Project: p.Key})
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, i := range res.Issues {
			keys = append(keys, i.Key)
		}
		slices.Sort(keys)
		return keys
	}
	want := []string{later.Key, other.Key}
	slices.Sort(want)
	if got := search("is:followup"); !slices.Equal(got, want) {
		t.Errorf("is:followup = %v, want %v", got, want)
	}
	if got := search("is:followed"); !slices.Equal(got, []string{first.Key}) {
		t.Errorf("is:followed = %v, want %v", got, []string{first.Key})
	}

	a := agentActor(e.admin, "anthropic/claude-opus-5-5")
	sess, err := e.s.SessionHook(e.ctx, a, []HookEvent{hook("box", "follow-a", "SessionStart", nil)})
	if err != nil {
		t.Fatal(err)
	}
	a.Session = sess.Session
	if _, err := e.s.AgentTake(e.ctx, a, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	filed := e.issue(a, IssueInput{Project: p.Key, Title: "Index the board query", Agent: "claude"})
	slice := e.issue(a, IssueInput{Project: p.Key, Title: "Measure the board query", Agent: "claude", Parent: first.Key})
	if !has(rel(filed.Key), "follows", first.Key, "session") {
		t.Errorf("an agent's filed task: %+v", rel(filed.Key))
	}
	if rs := rel(slice.Key); slices.ContainsFunc(rs, func(r Relation) bool { return r.Kind == "follows" }) {
		t.Errorf("a sub-task follows its parent: %+v", rs)
	}
	if err := e.s.AddRelation(e.ctx, e.admin, first.Key, "follows_up", later.Key); err == nil {
		t.Error("took an unknown relation")
	}
}

func TestBestFollowed(t *testing.T) {
	set := func(files ...string) fileSet { return fileSet{files: files} }
	cases := []struct {
		name  string
		mine  []string
		cands []fileSet
		want  int
	}{
		{"most of mine", []string{"a.go", "b.go", "c.go"}, []fileSet{set("x.go"), set("a.go", "b.go", "z.go")}, 1},
		{"one shared file is not enough", []string{"a.go"}, []fileSet{set("a.go")}, -1},
		{"under half of mine", []string{"a.go", "b.go", "c.go", "d.go", "e.go"}, []fileSet{set("a.go", "b.go")}, -1},
		{"a tie goes to the newer (first)", []string{"a.go", "b.go"}, []fileSet{set("a.go", "b.go"), set("a.go", "b.go", "c.go")}, 0},
		{"more shared wins", []string{"a.go", "b.go", "c.go"}, []fileSet{set("a.go", "b.go"), set("a.go", "b.go", "c.go")}, 1},
		{"nothing", []string{"a.go"}, nil, -1},
	}
	for _, c := range cases {
		if got := bestFollowed(c.mine, c.cands); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	var many []fileSet
	for range 8 {
		many = append(many, set("CHANGELOG.md", "help.md", "other.go"))
	}
	many = append(many, set("CHANGELOG.md", "help.md", "a.go"))
	if got := bestFollowed([]string{"CHANGELOG.md", "help.md", "a.go", "b.go", "c.go"}, many); got != -1 {
		t.Errorf("common files made a follow-up: %d", got)
	}
	if got := bestFollowed([]string{"CHANGELOG.md", "help.md", "a.go", "b.go"}, append(many, set("a.go", "b.go"))); got != 9 {
		t.Errorf("own files after the common ones: %d, want 9", got)
	}
}

// WP-255 (audit 808.1, 808.5): a task whose merged pull request changed
// mostly the files a done task's did follows it, found "files"; only in
// its own project, though another project shares the repo, since Waypoint
// acts for no person here (as autoRelate does).
func TestFollowUpsByFiles(t *testing.T) {
	e := newEscapeEnv(t)
	other := e.project(e.admin, "Beta", "kanban")
	now := e.clockNow()
	pr := func(n int, at time.Time, files ...string) {
		t.Helper()
		b, _ := json.Marshal(prFiles{Paths: files})
		if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO pull_requests(forge_id, repo, number, url, state, head_sha, files, files_sha, merged_at, created_at, updated_at)
			VALUES (?, 'o/r', ?, ?, 'merged', 'h', ?, 'h', ?, ?, ?)`, e.forge, n, "http://f.test/o/r/pulls/"+itoa(n), string(b), at.UnixMilli(), at.UnixMilli(), at.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	done := func(project, title string, n int) *IssueDetail {
		t.Helper()
		i := e.issue(e.admin, IssueInput{Project: project, Title: title, Status: "done"})
		e.link(t, i.ID, n)
		return i
	}
	pr(1, now.Add(-3*time.Hour), "a.go", "b.go", "c.go")
	pr(2, now.Add(-2*time.Hour), "a.go", "b.go")
	pr(3, now.Add(-time.Hour), "a.go", "b.go", "d.go")
	mine := done(e.proj.Key, "Board speed", 1)
	theirs := done(other.Key, "Beta's board work", 2)
	later := e.issue(e.admin, IssueInput{Project: e.proj.Key, Title: "Board speed again"})
	e.link(t, later.ID, 3)
	if err := e.s.noteFollowUpsByFiles(e.ctx, later.ID, "http://f.test/o/r/pulls/3"); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range e.get(e.admin, later.Key).Relations {
		got = append(got, r.Kind+" "+r.Issue.Key+" "+r.Found)
	}
	if want := []string{"follows " + mine.Key + " files"}; !slices.Equal(got, want) {
		t.Errorf("relations = %v, want %v (not %s)", got, want, theirs.Key)
	}
}
