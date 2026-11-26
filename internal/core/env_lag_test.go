package core

import (
	"strings"
	"testing"
	"time"
)

// TestEnvLagAlerts: an environment that keeps running an older commit than
// its branch past its lag limit tells the project's owners once per stall,
// again a day later, and not once it catches up (WP-199).
func TestEnvLagAlerts(t *testing.T) {
	x := newEnvT(t)
	res, err := x.s.db.W.Exec(`INSERT INTO forges(kind, name, base_url, created_at) VALUES ('forgejo','Forge','https://git.example.com', 0)`)
	if err != nil {
		t.Fatal(err)
	}
	fid, _ := res.LastInsertId()
	if _, err := x.s.db.W.Exec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?,?, 'acme/app', 0)`, x.p.ID, fid); err != nil {
		t.Fatal(err)
	}
	push := func(after string) {
		t.Helper()
		err := x.s.write(x.ctx, SystemActor(), func(t *tx) error {
			t.emit(&Event{Kind: EvRepoPushed, Data: map[string]any{"repo": "acme/app", "branch": "main", "after": after}})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	pr := 0
	merge := func(sha string) {
		t.Helper()
		pr++
		now := x.clockNow().UnixMilli()
		if _, err := x.s.db.W.Exec(`INSERT INTO pull_requests(forge_id, repo, number, base_ref, state, merged_at, created_at, updated_at)
			VALUES (?, 'Acme/App', ?, 'main', 'merged', ?, ?, ?)`, fid, pr, now, now, now); err != nil {
			t.Fatal(err)
		}
		push(sha)
	}
	deploy := func(sha string) {
		t.Helper()
		if _, err := x.s.RecordDeploymentNamed(x.ctx, x.editor, x.p.Key, "production", DeployInput{Commit: sha, Branch: "main", Version: "v1"}); err != nil {
			t.Fatal(err)
		}
	}
	alerts := func() []string {
		t.Helper()
		x.s.checkEnvLag(x.ctx)
		rows, err := x.s.db.R.Query(`SELECT title || ' | ' || body FROM notifications WHERE kind = ? ORDER BY id`, NotifyDeploy)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			out = append(out, s)
		}
		return out
	}
	advance := func(d time.Duration) { x.setClock(x.clockNow().Add(d)) }

	deploy("aaaaaaa1111")
	prod, _ := x.s.GetEnvironmentNamed(x.ctx, x.viewer, x.p.Key, "production")
	if prod.LagAlertM != envLagDefault {
		t.Fatalf("production lag limit %d", prod.LagAlertM)
	}
	if p := x.create("pr-12", ""); p.LagAlertM != 0 {
		t.Errorf("a preview has a lag limit: %d", p.LagAlertM)
	}
	push("aaaaaaa1111")
	advance(2 * time.Hour)
	if a := alerts(); len(a) != 0 {
		t.Fatalf("at its head: %v", a)
	}

	merge("bbbbbbb2222")
	advance(30 * time.Minute)
	merge("ccccccc3333")
	if a := alerts(); len(a) != 0 {
		t.Fatalf("30 minutes behind: %v", a)
	}
	advance(20 * time.Minute)
	a := alerts()
	if len(a) != 1 || !strings.Contains(a[0], "production is 50m behind main") || !strings.Contains(a[0], "main is at ccccccc") ||
		!strings.Contains(a[0], "still runs aaaaaaa (v1)") {
		t.Fatalf("after 50 minutes: %v", a)
	}
	ev := hasEvent(x.events(), EvEnvLagging)
	if ev == nil || ev.ProjectID != x.p.ID || ev.Data["behind_m"] != 50 || ev.Data["head"] != "ccccccc3333" {
		t.Errorf("event: %+v", ev)
	}
	advance(time.Hour)
	if a := alerts(); len(a) != 1 {
		t.Fatalf("repeated within the day: %v", a)
	}
	advance(envLagRepeat)
	if a = alerts(); len(a) != 2 || !strings.HasPrefix(strings.SplitN(a[1], " | ", 2)[1], "Still behind.") {
		t.Fatalf("a day later: %v", a)
	}

	deploy("ccccccc3333")
	advance(time.Hour)
	if a := alerts(); len(a) != 2 {
		t.Fatalf("after catching up: %v", a)
	}
	merge("ddddddd4444")
	advance(44 * time.Minute)
	if a := alerts(); len(a) != 2 {
		t.Fatalf("44 minutes after a merge: %v", a)
	}
	advance(2 * time.Minute)
	if a := alerts(); len(a) != 3 {
		t.Fatalf("46 minutes after a merge: %v", a)
	}

	deploy("ddddddd4444")
	advance(time.Minute)
	deploy("ccccccc3333")
	advance(40 * time.Minute)
	if a := alerts(); len(a) != 3 {
		t.Fatalf("40 minutes after a rollback: %v", a)
	}
	advance(10 * time.Minute)
	if a := alerts(); len(a) != 4 || !strings.Contains(a[3], "rolled back on purpose") {
		t.Fatalf("50 minutes after a rollback: %v", a)
	}

	deploy("ddddddd4444")
	merge("eeeeeee5555")
	advance(30 * time.Minute)
	deploy("ddddddd4444")
	advance(20 * time.Minute)
	if a := alerts(); len(a) != 5 || !strings.Contains(a[4], "50m behind") {
		t.Fatalf("50 minutes after a merge, redeployed since: %v", a)
	}

	deploy("eeeeeee5555")
	advance(time.Minute)
	pr++
	if _, err := x.s.db.W.Exec(`INSERT INTO pull_requests(forge_id, repo, number, base_ref, state, merged_at, created_at, updated_at)
		VALUES (?, 'acme/app', ?, 'main', 'merged', ?, ?, ?)`, fid, pr, x.clockNow().UnixMilli(), 0, 0); err != nil {
		t.Fatal(err)
	}
	advance(5 * time.Minute)
	deploy("fffffff6666")
	advance(2 * time.Hour)
	if a := alerts(); len(a) != 5 {
		t.Fatalf("after a missed push: %v", a)
	}

	merge("ggggggg7777")
	advance(2 * time.Hour)
	if _, err := x.s.db.W.Exec(`UPDATE projects SET archived = 1 WHERE id = ?`, x.p.ID); err != nil {
		t.Fatal(err)
	}
	if a := alerts(); len(a) != 5 {
		t.Fatalf("archived: %v", a)
	}
	if _, err := x.s.db.W.Exec(`UPDATE projects SET archived = 0 WHERE id = ?`, x.p.ID); err != nil {
		t.Fatal(err)
	}
	if a := alerts(); len(a) != 6 {
		t.Fatalf("unarchived: %v", a)
	}

	dev := x.create("dev", "")
	staging := EnvStaging
	d, err := x.s.UpdateEnvironment(x.ctx, x.editor, dev.ID, EnvironmentInput{Kind: &staging})
	if err != nil || d.LagAlertM != envLagDefault {
		t.Errorf("dev made staging: %v, %+v", err, d)
	}

	zero := 0
	if _, err := x.s.UpdateEnvironment(x.ctx, x.editor, prod.ID, EnvironmentInput{LagAlertM: &zero}); err != nil {
		t.Fatal(err)
	}
	advance(2 * envLagRepeat)
	if a := alerts(); len(a) != 6 {
		t.Fatalf("with the alert off: %v", a)
	}
	bad := envLagAlertMax + 1
	if _, err := x.s.UpdateEnvironment(x.ctx, x.editor, prod.ID, EnvironmentInput{LagAlertM: &bad}); err == nil {
		t.Error("a lag limit over a week was accepted")
	}
}
