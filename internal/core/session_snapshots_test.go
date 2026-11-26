package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestSnapshotCapsKeepFirstAndNewestAndCountWhatGoes(t *testing.T) {
	e := newEnv(t)
	model := "claude-opus-5"
	for i := 0; i < snapshotsPerSession+7; i++ {
		build := int64(i)
		if _, err := e.s.SessionHook(e.ctx, e.admin, []HookEvent{hook("box", "cap-1", "ConfigChange", func(ev *HookEvent) {
			ev.Source, ev.At = "user_settings", e.s.nowMS()+int64(i)
			ev.Snapshot = &HookSnapshot{Model: &model, BuildMS: &build}
		})}); err != nil {
			t.Fatal(err)
		}
	}
	x := sessionsFor(t, e, e.admin, AgentSessionQuery{})[0]
	snaps, err := e.s.SessionSnapshots(e.ctx, e.admin, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != snapshotsPerSession {
		t.Fatalf("kept %d snapshots, want %d", len(snaps), snapshotsPerSession)
	}
	if *snaps[0].BuildMS != 0 || *snaps[len(snaps)-1].BuildMS != int64(snapshotsPerSession+6) {
		t.Errorf("first/last build_ms = %d/%d: the first and newest must stay", *snaps[0].BuildMS, *snaps[len(snaps)-1].BuildMS)
	}
	drops, err := e.s.HookDrops(e.ctx, e.admin)
	if err != nil || len(drops) != 1 || drops[0].Reason != "server_cap" || drops[0].Event != "snapshot" || drops[0].Dropped != 7 {
		t.Errorf("drops = %+v, %v", drops, err)
	}
}

func TestObservedValuesRecordOnlyChangesAndCapInstructionFiles(t *testing.T) {
	e := newEnv(t)
	send := func(event string, mod func(*HookEvent)) {
		t.Helper()
		if _, err := e.s.SessionHook(e.ctx, e.admin, []HookEvent{hook("box", "obs-1", event, mod)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"default", "default", "plan", "default"} {
		send("UserPromptSubmit", func(ev *HookEvent) { ev.Prompt, ev.PermissionMode = "p", mode })
	}
	for i := 0; i < observedPerSession+3; i++ {
		send("InstructionsLoaded", func(ev *HookEvent) {
			ev.File = fmt.Sprintf("/work/app/%d/AGENTS.md", i)
			ev.LoadReason = "nested_traversal"
		})
	}
	x := sessionsFor(t, e, e.admin, AgentSessionQuery{})[0]
	obs, err := e.s.SessionObserved(e.ctx, e.admin, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	var modes, files []string
	for _, o := range obs {
		switch o.Kind {
		case "permission_mode":
			modes = append(modes, o.Value+"<-"+fmt.Sprint(prevOrNil(o.Prev)))
		case "instruction_file":
			files = append(files, o.Value)
		}
	}
	if got := strings.Join(modes, " "); got != "default<-<nil> plan<-default default<-plan" {
		t.Errorf("permission modes = %q", got)
	}
	if len(files) != observedPerSession {
		t.Errorf("kept %d instruction files, want %d", len(files), observedPerSession)
	}
	drops, err := e.s.HookDrops(e.ctx, e.admin)
	if err != nil || len(drops) != 1 || drops[0].Reason != "server_cap" || drops[0].Event != "instruction_file" || drops[0].Dropped != 3 {
		t.Errorf("drops = %+v, %v", drops, err)
	}
}

func prevOrNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestOversizedInventoryIsDroppedCountedAndSaidOnTheRow(t *testing.T) {
	e := newEnv(t)
	big, _ := json.Marshal(map[string]string{"x": strings.Repeat("a", snapshotMaxBytes)})
	model := "m"
	if _, err := e.s.SessionHook(e.ctx, e.admin, []HookEvent{hook("box", "big-1", "SessionStart", func(ev *HookEvent) {
		ev.Snapshot = &HookSnapshot{Model: &model, Inventory: big}
	})}); err != nil {
		t.Fatal(err)
	}
	x := sessionsFor(t, e, e.admin, AgentSessionQuery{})[0]
	snaps, err := e.s.SessionSnapshots(e.ctx, e.admin, x.ID)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots: %d, %v", len(snaps), err)
	}
	if string(snaps[0].Inventory) != "{}" || snaps[0].Missing["inventory"] == "" || *snaps[0].Model != "m" {
		t.Errorf("row = %+v", snaps[0])
	}
	if drops, _ := e.s.HookDrops(e.ctx, e.admin); len(drops) != 1 || drops[0].Reason != "snapshot_inventory_unusable" {
		t.Errorf("drops = %+v", drops)
	}
}

func dropsByKey(t *testing.T, e *env, a *Actor) map[string]int64 {
	t.Helper()
	drops, err := e.s.HookDrops(e.ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]int64{}
	for _, d := range drops {
		by[d.Reason+"/"+d.Event] += d.Dropped
	}
	return by
}

// A drop report sent twice (the client lost the reply) is counted once (WP-1468).
func TestDropsIDMakesAReportIdempotent(t *testing.T) {
	e := newEnv(t)
	send := func(id string) {
		t.Helper()
		if _, err := e.s.SessionHook(e.ctx, e.admin, []HookEvent{hook("box", "did-1", "Stop", func(ev *HookEvent) {
			ev.Drops, ev.DropsID = map[string]int64{"overflow/Stop": 2}, id
		})}); err != nil {
			t.Fatal(err)
		}
	}
	send("a")
	send("a")
	if got := dropsByKey(t, e, e.admin)["overflow/Stop"]; got != 2 {
		t.Errorf("same id twice counted %d, want 2", got)
	}
	send("b")
	if got := dropsByKey(t, e, e.admin)["overflow/Stop"]; got != 4 {
		t.Errorf("different ids counted %d, want 4", got)
	}
	send("")
	send("")
	if got := dropsByKey(t, e, e.admin)["overflow/Stop"]; got != 8 {
		t.Errorf("no id counted %d, want 8 (each time)", got)
	}
	long := strings.Repeat("x", 65)
	send(long)
	send(long)
	if got := dropsByKey(t, e, e.admin)["overflow/Stop"]; got != 12 {
		t.Errorf("an over-long id counted %d, want 12 (treated as no id)", got)
	}
}

func TestDropsReportsOlderThan30DaysArePruned(t *testing.T) {
	e := newEnv(t)
	old := e.s.nowMS() - 31*24*3600*1000
	if _, err := e.s.db.W.Exec(`INSERT INTO hook_drop_reports(user_id, report_id, at) VALUES (?,?,?)`, e.admin.ID(), "old", old); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SessionHook(e.ctx, e.admin, []HookEvent{hook("box", "pr-1", "Stop", func(ev *HookEvent) {
		ev.Drops, ev.DropsID = map[string]int64{"overflow/Stop": 1}, "new"
	})}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM hook_drop_reports`).Scan(&n); err != nil || n != 1 {
		t.Errorf("reports kept = %d, %v; want only the new one", n, err)
	}
}

func TestHookDropsSplitByUserAndAreForAdmins(t *testing.T) {
	e := newEnv(t)
	ana, bob := e.user("ana", false), e.user("bob", false)
	for _, a := range []*Actor{ana, bob} {
		if _, err := e.s.SessionHook(e.ctx, a, []HookEvent{hook("box", "u-1", "Stop", func(ev *HookEvent) {
			ev.Drops = map[string]int64{"overflow/Stop": 1}
		})}); err != nil {
			t.Fatal(err)
		}
	}
	drops, err := e.s.HookDrops(e.ctx, e.admin)
	if err != nil || len(drops) != 2 {
		t.Fatalf("drops = %+v, %v; want one row per user", drops, err)
	}
	names := []string{drops[0].User.Username, drops[1].User.Username}
	sort.Strings(names)
	if names[0] != "ana" || names[1] != "bob" {
		t.Errorf("users = %v", names)
	}
	if _, err := e.s.HookDrops(e.ctx, ana); err == nil {
		t.Error("a non-admin read the drops")
	}
}

// Snapshots and the observed timeline are the owner's and admins' (WP-1468).
func TestSessionSnapshotsAndObservedAreTheOwnersAndAdmins(t *testing.T) {
	e := newEnv(t)
	ana, bob, cy := e.user("ana", false), e.user("bob", false), e.user("cy", false)
	model := "m"
	if _, err := e.s.SessionHook(e.ctx, ana, []HookEvent{hook("box", "own-1", "SessionStart", func(ev *HookEvent) {
		ev.Model, ev.Snapshot = model, &HookSnapshot{Model: &model}
	})}); err != nil {
		t.Fatal(err)
	}
	p := e.project(e.admin, "Shared", "kanban")
	if err := e.s.SetMember(e.ctx, e.admin, p.Key, "ana", "editor"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetMember(e.ctx, e.admin, p.Key, "bob", "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.Exec(`UPDATE agent_sessions SET project_id = ?`, p.ID); err != nil {
		t.Fatal(err)
	}
	id := sessionsFor(t, e, ana, AgentSessionQuery{})[0].ID
	for _, a := range []*Actor{ana, e.admin} {
		if sn, err := e.s.SessionSnapshots(e.ctx, a, id); err != nil || len(sn) != 1 {
			t.Errorf("snapshots for %s: %d, %v", a.User.Username, len(sn), err)
		}
		if ob, err := e.s.SessionObserved(e.ctx, a, id); err != nil || len(ob) != 1 {
			t.Errorf("observed for %s: %d, %v", a.User.Username, len(ob), err)
		}
	}
	var fe *forbiddenError
	if _, err := e.s.SessionSnapshots(e.ctx, bob, id); !errors.As(err, &fe) {
		t.Errorf("a colleague's snapshots err = %v, want forbidden", err)
	}
	if _, err := e.s.SessionObserved(e.ctx, bob, id); !errors.As(err, &fe) {
		t.Errorf("a colleague's observed err = %v, want forbidden", err)
	}
	if _, err := e.s.SessionSnapshots(e.ctx, cy, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("no role: snapshots err = %v, want not found", err)
	}
	if _, err := e.s.SessionObserved(e.ctx, cy, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("no role: observed err = %v, want not found", err)
	}
}

// An event that arrives late is placed in the timeline, not taken for the
// newest change (WP-1468).
func TestObservedLateEventsKeepTheTimelineStraight(t *testing.T) {
	e := newEnv(t)
	t1, t2, t3 := e.s.nowMS()-3000, e.s.nowMS()-2000, e.s.nowMS()-1000
	send := func(cli string, at int64, mode string) {
		t.Helper()
		if _, err := e.s.SessionHook(e.ctx, e.admin, []HookEvent{hook("box", cli, "UserPromptSubmit", func(ev *HookEvent) {
			ev.Prompt, ev.PermissionMode, ev.At = "p", mode, at
		})}); err != nil {
			t.Fatal(err)
		}
	}
	line := func(cli string) string {
		t.Helper()
		var x *AgentSession
		for _, c := range sessionsFor(t, e, e.admin, AgentSessionQuery{}) {
			if c.CLIID == cli {
				x = c
			}
		}
		obs, err := e.s.SessionObserved(e.ctx, e.admin, x.ID)
		if err != nil {
			t.Fatal(err)
		}
		var parts []string
		for _, o := range obs {
			parts = append(parts, fmt.Sprintf("%s<-%v@%d", o.Value, prevOrNil(o.Prev), o.At.UnixMilli()-t1))
		}
		return strings.Join(parts, " ")
	}
	send("late-1", t2, "plan")
	send("late-1", t1, "default")
	if got, want := line("late-1"), fmt.Sprintf("default<-<nil>@0 plan<-default@%d", t2-t1); got != want {
		t.Errorf("late predecessor: %q, want %q", got, want)
	}
	send("late-2", t3, "plan")
	send("late-2", t1, "default")
	send("late-2", t2, "plan")
	if got, want := line("late-2"), fmt.Sprintf("default<-<nil>@0 plan<-default@%d", t2-t1); got != want {
		t.Errorf("late duplicate: %q, want %q", got, want)
	}
}

func TestPostModelSwitchStoresFromAndRequestedModel(t *testing.T) {
	e := newEnv(t)
	if _, err := e.s.SessionHook(e.ctx, e.admin, []HookEvent{hook("box", "sw-1", "PostModelSwitch", func(ev *HookEvent) {
		ev.Model, ev.FromModel, ev.RequestedModel = "opus", "sonnet", strings.Repeat("r", 3000)
	})}); err != nil {
		t.Fatal(err)
	}
	x := sessionsFor(t, e, e.admin, AgentSessionQuery{})[0]
	obs, err := e.s.SessionObserved(e.ctx, e.admin, x.ID)
	if err != nil || len(obs) != 1 {
		t.Fatalf("observed %d, %v", len(obs), err)
	}
	var d map[string]string
	if err := json.Unmarshal(obs[0].Detail, &d); err != nil {
		t.Fatal(err)
	}
	if d["from_model"] != "sonnet" || d["requested_model"] == "" || len([]rune(d["requested_model"])) > 1001 {
		t.Errorf("detail = %v", d)
	}
	if _, err := e.s.SessionHook(e.ctx, e.admin, []HookEvent{hook("box", "sw-1", "PostModelSwitch", func(ev *HookEvent) {
		ev.Model = "haiku"
	})}); err != nil {
		t.Fatal(err)
	}
	obs, _ = e.s.SessionObserved(e.ctx, e.admin, x.ID)
	if string(obs[1].Detail) != "{}" {
		t.Errorf("detail = %s, want {}", obs[1].Detail)
	}
}

// Observational data must not refuse the batch (WP-1468): a failure to record
// is counted as server_error and the event goes on.
func TestRecordObservedFailureDoesNotRefuseTheBatch(t *testing.T) {
	e := newEnv(t)
	if _, err := e.s.db.W.Exec(`DROP TABLE session_observed`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SessionHook(e.ctx, e.admin, []HookEvent{hook("box", "fail-1", "UserPromptSubmit", func(ev *HookEvent) {
		ev.Prompt, ev.PermissionMode = "hello", "plan"
	})}); err != nil {
		t.Fatalf("the batch was refused: %v", err)
	}
	x := sessionsFor(t, e, e.admin, AgentSessionQuery{})[0]
	if x.LastPrompt != "hello" {
		t.Errorf("the prompt was lost: %q", x.LastPrompt)
	}
	if got := dropsByKey(t, e, e.admin)["server_error/UserPromptSubmit"]; got != 1 {
		t.Errorf("server_error counted %d, want 1", got)
	}
}

func TestMalformedDropKeysAreCounted(t *testing.T) {
	e := newEnv(t)
	if _, err := e.s.SessionHook(e.ctx, e.admin, []HookEvent{hook("box", "mk-1", "Stop", func(ev *HookEvent) {
		ev.Drops = map[string]int64{"nodash": 2, strings.Repeat("k", dropKeyMax) + "/Stop": 3, "ok/Stop": 1, "zero/Stop": 0, "neg/Stop": -4}
	})}); err != nil {
		t.Fatal(err)
	}
	by := dropsByKey(t, e, e.admin)
	if by["malformed_key/?"] != 5 || by["ok/Stop"] != 1 || len(by) != 2 {
		t.Errorf("drops = %v", by)
	}
}

func TestSnapshotInventoryNotJSONIsDroppedAndCounted(t *testing.T) {
	e := newEnv(t)
	model := "m"
	if _, err := e.s.SessionHook(e.ctx, e.admin, []HookEvent{hook("box", "bad-1", "SessionStart", func(ev *HookEvent) {
		ev.Snapshot = &HookSnapshot{Model: &model, Inventory: json.RawMessage(`{"x": `), Missing: map[string]string{"effort": "not in payload"}}
	})}); err != nil {
		t.Fatal(err)
	}
	x := sessionsFor(t, e, e.admin, AgentSessionQuery{})[0]
	snaps, err := e.s.SessionSnapshots(e.ctx, e.admin, x.ID)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots: %d, %v", len(snaps), err)
	}
	if string(snaps[0].Inventory) != "{}" || snaps[0].Missing["inventory"] == "" || snaps[0].Missing["effort"] == "" {
		t.Errorf("row = %+v", snaps[0])
	}
	if by := dropsByKey(t, e, e.admin); by["snapshot_inventory_unusable/SessionStart"] != 1 {
		t.Errorf("drops = %v", by)
	}
}
