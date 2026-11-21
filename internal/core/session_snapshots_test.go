package core

import (
	"encoding/json"
	"fmt"
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
	drops, err := e.s.HookDrops(e.ctx)
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
	drops, err := e.s.HookDrops(e.ctx)
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
	if drops, _ := e.s.HookDrops(e.ctx); len(drops) != 1 || drops[0].Reason != "snapshot_inventory_unusable" {
		t.Errorf("drops = %+v", drops)
	}
}
