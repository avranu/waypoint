package core

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func boolp(b bool) *bool { return &b }

// WP-1265: a repo's set fields win over the person's settings for all
// sessions; what the repo leaves unset stays as All says.
func TestSessionPrefsForRepo(t *testing.T) {
	p := &SessionPrefs{
		All:   SessionSettings{RemoteControl: boolp(true), Model: "opus", Effort: "high"},
		Repos: map[string]SessionSettings{"robin/waypoint": {RemoteControl: boolp(false), CodeMode: boolp(true), Effort: "max"}},
	}
	got := p.For("Robin/Waypoint")
	if got.RemoteControl == nil || *got.RemoteControl || got.CodeMode == nil || !*got.CodeMode || got.Model != "opus" || got.Effort != "max" {
		t.Errorf("in the repo: %+v", got)
	}
	if got := p.For("robin/other"); got.RemoteControl == nil || !*got.RemoteControl || got.CodeMode != nil || got.Effort != "high" {
		t.Errorf("elsewhere: %+v", got)
	}
	if got := (*SessionPrefs)(nil).For("robin/waypoint"); !got.Empty() {
		t.Errorf("none: %+v", got)
	}
}

func TestSessionPrefsAreSavedCheckedAndTidied(t *testing.T) {
	e := newEnv(t)
	save := func(p *SessionPrefs) (*User, error) {
		return e.s.UpdateUser(e.ctx, e.admin, e.admin.id(), UserPatch{Prefs: &UserPrefs{WeekStart: 1, Sessions: p}})
	}
	u, err := save(&SessionPrefs{
		All: SessionSettings{CodeMode: boolp(true), Model: " claude-opus-5-5[1m] ", PermissionMode: "auto"},
		Repos: map[string]SessionSettings{
			"Robin/Waypoint": {RemoteControl: boolp(true)},
			"robin/empty":    {}, // nothing set: dropped
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := u.Prefs.Sessions
	if got == nil || got.All.Model != "claude-opus-5-5[1m]" || len(got.Repos) != 1 || got.Repos["robin/waypoint"].RemoteControl == nil {
		t.Fatalf("saved %+v", got)
	}
	// Nothing set at all is no settings.
	if u, err := save(&SessionPrefs{Repos: map[string]SessionSettings{"a/b": {}}}); err != nil {
		t.Fatalf("empty: %v", err)
	} else if u.Prefs.Sessions != nil {
		t.Fatalf("empty: saved %+v", u.Prefs.Sessions)
	}
	for _, bad := range []struct {
		p    SessionPrefs
		want string
	}{
		{SessionPrefs{All: SessionSettings{PermissionMode: "bypassPermissions"}}, "permission mode"},
		{SessionPrefs{All: SessionSettings{Effort: "huge"}}, "effort"},
		{SessionPrefs{All: SessionSettings{Model: "opus; rm -rf /"}}, "model"},
		{SessionPrefs{Repos: map[string]SessionSettings{"waypoint": {Effort: "low"}}}, "owner/name"},
		{SessionPrefs{Repos: map[string]SessionSettings{"a/b": {Effort: "x"}, "A/B": {Effort: "low"}}}, ""},
	} {
		if _, err := save(&bad.p); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%+v: %v", bad.p, err)
		}
	}
}

// The runner gets its owner's settings with the jobs that start sessions,
// and only with those.
func TestSessionJobsCarryTheOwnersSettings(t *testing.T) {
	e := newEnv(t)
	prefs := &SessionPrefs{All: SessionSettings{RemoteControl: boolp(false), Effort: "low"}}
	if _, err := e.s.UpdateUser(e.ctx, e.admin, e.admin.id(), UserPatch{Prefs: &UserPrefs{WeekStart: 1, Sessions: prefs}}); err != nil {
		t.Fatal(err)
	}
	r := e.runner(e.admin, "m1", func(r *RunnerRegistration) { r.CLIs = []RunnerCLI{{Name: "claude"}} })
	raw, _ := json.Marshal(SessionJob{Dir: "/work", Prompt: "go on"})
	e.enqueue(e.admin, RunnerJobInput{Kind: JobSession, Input: raw})
	w := e.poll(e.admin, r, RunnerPoll{})
	if w.Job == nil || w.Job.Launch == nil || w.Job.Launch.All.Effort != "low" || w.Job.Launch.All.RemoteControl == nil || *w.Job.Launch.All.RemoteControl {
		t.Fatalf("session job: %+v", w.Job)
	}
	e.enqueue(e.admin, RunnerJobInput{Kind: JobCommand, Input: cmdInput("true", "/work")})
	if w := e.poll(e.admin, r, RunnerPoll{}); w.Job == nil || w.Job.Launch != nil {
		t.Fatalf("command job: %+v", w.Job)
	}
}

// Prefs that don't parse send no settings, and say so in the log; prefs
// with nothing set send none either (WP-1265 review 1003.3).
func TestSessionJobsWithUnreadablePrefs(t *testing.T) {
	e := newEnv(t)
	logs := &lockedBuf{}
	e.s.log = slog.New(slog.NewTextHandler(logs, nil))
	r := e.runner(e.admin, "m1", func(r *RunnerRegistration) { r.CLIs = []RunnerCLI{{Name: "claude"}} })
	raw, _ := json.Marshal(SessionJob{Dir: "/work", Prompt: "go on"})
	for _, c := range []struct{ prefs, log string }{
		{`{"sessions":{"all":{}}}`, ""},
		{`{"sessions":`, "session settings"},
	} {
		if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE users SET prefs = ? WHERE id = ?`, c.prefs, e.admin.id()); err != nil {
			t.Fatal(err)
		}
		e.enqueue(e.admin, RunnerJobInput{Kind: JobSession, Input: raw})
		w := e.poll(e.admin, r, RunnerPoll{})
		if w.Job == nil || w.Job.Launch != nil {
			t.Fatalf("%s: %+v", c.prefs, w.Job)
		}
		if c.log != "" && !strings.Contains(logs.String(), c.log) {
			t.Errorf("%s: no log line: %s", c.prefs, logs.String())
		}
	}
}
