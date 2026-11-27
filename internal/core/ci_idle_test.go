package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// actionCalls counts the requests made for a repo's Actions.
func (a *apiFake) actionCalls(repo string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, c := range a.got {
		if strings.HasPrefix(c.path, "/repos/"+repo+"/actions/") {
			n++
		}
	}
	return n
}

// TestCIBoardIdleRepos: a repo with no CI activity is not asked again until
// something marks it active or ciIdleReread has passed (WP-1980).
func TestCIBoardIdleRepos(t *testing.T) {
	e := newEnv(t)
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	e.setClock(t0)
	p := e.project(e.admin, "Alpha", "kanban")
	a, _, cl := newAPIFake(t, "forgejo")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), Name: ptr("Forge"), BaseURL: ptr(a.srv.URL), Token: ptr("tok")})
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"acme/idle", "acme/off", "acme/broken", "acme/busy"} {
		if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, ?, 0)`, p.ID, f.ID, repo); err != nil {
			t.Fatal(err)
		}
	}
	e.s.Forges = cl
	none := map[string]any{"workflow_runs": []map[string]any{}}
	for _, repo := range []string{"acme/idle", "acme/busy"} {
		a.reply["GET /repos/"+repo+"/actions/tasks?page=1&limit=50&status=running"] = none
		a.reply["GET /repos/"+repo+"/actions/tasks?page=1&limit=50"] = none
		a.reply["GET /repos/"+repo+"/actions/runs?page=1&limit=50"] = none
	}
	const waiting = "/actions/runs?page=1&limit=50&status=waiting&status=running&status=blocked"
	a.reply["GET /repos/acme/idle"+waiting] = none
	a.reply["GET /repos/acme/off"+waiting] = 404
	a.reply["GET /repos/acme/broken"+waiting] = 500
	a.reply["GET /repos/acme/busy"+waiting] = map[string]any{"workflow_runs": []map[string]any{
		{"id": 300, "index_in_repo": 3, "workflow_id": "ci.yml", "status": "running", "created": "2026-10-28T11:50:00Z", "started": "2026-10-28T11:55:00Z",
			"stopped": "1970-01-01T00:00:00Z", "commit_sha": "abc", "prettyref": "main", "trigger_event": "push", "title": "Busy"}}}
	a.reply["GET /repos/acme/busy/actions/runs/300/jobs"] = []map[string]any{}

	sweep := func(at time.Time) map[string]int {
		t.Helper()
		e.setClock(at)
		before := map[string]int{}
		for _, repo := range []string{"acme/idle", "acme/off", "acme/broken", "acme/busy"} {
			before[repo] = a.actionCalls(repo)
		}
		if _, err := e.s.readCI(e.ctx); err != nil {
			t.Fatal(err)
		}
		got := map[string]int{}
		for repo, n := range before {
			got[repo] = a.actionCalls(repo) - n
		}
		return got
	}
	// check compares which repos the sweep asked with want; a repo not in want isn't checked.
	check := func(step string, got map[string]int, want map[string]bool) {
		t.Helper()
		for repo, w := range want {
			if (got[repo] > 0) != w {
				t.Errorf("%s: %s made %d requests, asked = %v wanted", step, repo, got[repo], w)
			}
		}
	}
	all := func(v bool) map[string]bool {
		return map[string]bool{"acme/idle": v, "acme/off": v, "acme/broken": v, "acme/busy": v}
	}
	min := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

	check("first sweep reads every repo", sweep(t0), all(true))
	// Only the failed repo and the one with a run are asked again.
	check("second sweep", sweep(min(4)), map[string]bool{"acme/idle": false, "acme/off": false, "acme/broken": true, "acme/busy": true})
	e.s.noteCIActive(f.ID, "Acme/Idle")
	check("a webhook mark", sweep(min(5)), map[string]bool{"acme/idle": true, "acme/off": false})
	check("the mark's tail", sweep(min(14)), map[string]bool{"acme/idle": true})
	check("the mark is 15 minutes old", sweep(min(21)), map[string]bool{"acme/idle": false})

	// ciIdleReread after their last reads, every idle repo is read in full.
	check("full read", sweep(min(21).Add(ciIdleReread)), all(true))
	check("then idle again", sweep(min(25).Add(ciIdleReread)), map[string]bool{"acme/idle": false, "acme/off": false, "acme/broken": true, "acme/busy": true})

	// The busy repo ends its run: read through ciActiveFor, then left alone.
	a.reply["GET /repos/acme/busy"+waiting] = none
	check("busy repo's last read", sweep(min(29).Add(ciIdleReread)), map[string]bool{"acme/busy": true})
	check("busy repo's tail", sweep(min(38).Add(ciIdleReread)), map[string]bool{"acme/busy": true})
	check("busy repo idle", sweep(min(46).Add(ciIdleReread)), map[string]bool{"acme/busy": false, "acme/broken": true})
}

// TestCIGateIdle covers the gate's decision on its own.
func TestCIGateIdle(t *testing.T) {
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		g     *ciGate
		now   time.Duration
		board bool
		idle  bool
	}{
		{"never read", nil, time.Minute, true, false},
		{"read, quiet", &ciGate{readAt: t0, data: &ciRepoData{}}, time.Minute + ciActiveFor, true, true},
		{"read, active recently", &ciGate{readAt: t0, activeAt: t0, data: &ciRepoData{}}, time.Minute, true, false},
		{"read long ago", &ciGate{readAt: t0, data: &ciRepoData{}}, ciIdleReread + ciActiveFor, true, false},
		{"read failed", &ciGate{readAt: t0, data: &ciRepoData{err: errors.New("boom")}}, ciActiveFor, true, false},
		{"read found work, no sweep since", &ciGate{readAt: t0, activeAt: t0, data: &ciRepoData{active: []ActionRun{{}}}}, time.Minute + ciActiveFor, true, false},
		{"history read, quiet", &ciGate{histAt: t0}, ciActiveFor, false, true},
		{"history never read", &ciGate{readAt: t0, data: &ciRepoData{}}, ciActiveFor, false, false},
		{"activity since the read", &ciGate{readAt: t0, activeAt: t0.Add(time.Second), data: &ciRepoData{}}, time.Minute + ciActiveFor, true, false},
		{"activity since the history read", &ciGate{histAt: t0, activeAt: t0.Add(time.Second)}, time.Minute + ciActiveFor, false, false},
	} {
		c := &ciBoardCache{}
		if tc.g != nil {
			c.gate = map[string]*ciGate{"1|a/b": tc.g}
		}
		if got := c.idle("1|a/b", t0.Add(tc.now), tc.board); got != tc.idle {
			t.Errorf("%s: idle = %v, want %v", tc.name, got, tc.idle)
		}
	}
}

// TestCIHistoryIdleRepos: the history pass leaves a quiet repo alone until a
// webhook marks it, a failed read is tried again, and the board doesn't show
// a quiet repo's failed history read as no jobs (WP-1980).
func TestCIHistoryIdleRepos(t *testing.T) {
	e := newEnv(t)
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	e.setClock(t0)
	p := e.project(e.admin, "Alpha", "kanban")
	a, _, cl := newAPIFake(t, "forgejo")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), Name: ptr("Forge"), BaseURL: ptr(a.srv.URL), Token: ptr("tok")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', 0)`, p.ID, f.ID); err != nil {
		t.Fatal(err)
	}
	e.s.Forges = cl
	none := map[string]any{"workflow_runs": []map[string]any{}}
	const tasks = "GET /repos/acme/app/actions/tasks?page=1&limit=50"
	a.reply["GET /repos/acme/app/actions/runs?page=1&limit=50"] = none
	a.reply["GET /repos/acme/app/actions/tasks?page=1&limit=50&status=running"] = none
	a.reply["GET /repos/acme/app/actions/runs?page=1&limit=50&status=waiting&status=running&status=blocked"] = none
	pass := func(minute int, asked bool) {
		t.Helper()
		e.setClock(t0.Add(time.Duration(minute) * time.Minute))
		before := a.actionCalls("acme/app")
		e.s.RecordCIHistory(e.ctx)
		e.s.Wait()
		if n := a.actionCalls("acme/app") - before; (n > 0) != asked {
			t.Errorf("minute %d: %d requests, asked = %v wanted", minute, n, asked)
		}
	}

	a.reply[tasks] = 500
	pass(0, true)
	pass(1, true) // the failed read isn't one to idle on
	a.reply[tasks] = none
	pass(2, true)
	pass(5, false)

	secret, err := e.s.ForgeSecret(e.ctx, e.admin, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"ref":"refs/heads/main","repository":{"full_name":"acme/app"}}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	h := http.Header{}
	h.Set("X-Forgejo-Event", "push")
	h.Set("X-Forgejo-Signature", hex.EncodeToString(mac.Sum(nil)))
	e.setClock(t0.Add(6 * time.Minute))
	if err := e.s.HandleForgeWebhook(e.ctx, f.ID, h, body); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	pass(7, true)
	pass(23, false)

	// The history read fails while the repo is quiet for the board.
	if _, err := e.s.readCI(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.s.ciBoard.mu.Lock()
	e.s.ciBoard.history[ciGateKey(f.ID, "acme/app")] = &ciHistory{at: e.s.now(), err: errors.New("boom")}
	e.s.ciBoard.mu.Unlock()
	e.setClock(t0.Add(28 * time.Minute))
	before := a.actionCalls("acme/app")
	if _, err := e.s.readCI(e.ctx); err != nil {
		t.Fatal(err)
	}
	if a.actionCalls("acme/app") == before {
		t.Error("the board didn't read a repo whose history read had failed")
	}
}
