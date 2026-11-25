package core

import (
	"reflect"
	"testing"
)

func (e *uaEnv) trace(key, session string, seq int, started int64, calls int64, ended string) UsageTrace {
	ce := int64(60000)
	return UsageTrace{Key: key, Session: session, Seq: seq, Started: started, Last: started + 1000*calls, Ended: ended, Model: "claude-opus-5-5",
		Calls: calls, Reads: calls, PreCalls: calls, CtxStart: 40000, CtxFirstEdit: &ce,
		Files: []TraceFile{{Path: "internal/a.go", Repo: "acme/app", Nav: 2, Read: true, Edited: true, Seen: true}}}
}

func (e *uaEnv) ingestTraces(a *Actor, id, machine string, tr ...UsageTrace) *UsageIngestResult {
	e.t.Helper()
	return e.ingest(a, id, machine, UsageChunk{Traces: tr})
}

func TestTaskTracesIngest(t *testing.T) {
	e := newAnalyticsEnv(t)
	p := e.project(e.admin, "Web", "")
	if err := e.s.SetMember(e.ctx, e.admin, p.Key, "pat", "editor"); err != nil {
		t.Fatal(err)
	}
	secret := e.project(e.admin, "Secret", "")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "first"})
	hidden := e.issue(e.admin, IssueInput{Project: secret.Key, Title: "hidden"})
	var landed [][]int64
	e.s.tracesLanded = func(ids []int64) { landed = append(landed, ids) }
	h := e.hour(-3) + 500

	r := e.ingestTraces(e.me, "c1", "m1", e.trace(i.Key, "s1", 1, h, 3, ""))
	if r.Traces != 1 || r.TracesDropped != 0 {
		t.Fatalf("result = %+v", r)
	}
	if want := [][]int64{{i.ID}}; !reflect.DeepEqual(landed, want) {
		t.Fatalf("landed = %v, want %v", landed, want)
	}
	r = e.ingestTraces(e.me, "c2", "m1", e.trace(i.Key, "s1", 1, h, 5, "ship"))
	if r.Traces != 1 || len(landed) != 2 {
		t.Fatalf("grown: %+v landed %v", r, landed)
	}
	r = e.ingestTraces(e.me, "c3", "m1", e.trace(i.Key, "s1", 1, h, 4, ""))
	if r.Traces != 0 || len(landed) != 2 {
		t.Fatalf("older copy: %+v landed %v", r, landed)
	}
	r = e.ingestTraces(e.me, "c4", "m1", e.trace(i.Key, "s1", 1, h, 5, "ship"))
	if r.Traces != 0 || len(landed) != 2 {
		t.Fatalf("identical copy: %+v landed %v", r, landed)
	}
	got, err := e.s.taskTraces(e.ctx, i.ID)
	if err != nil || len(got) != 1 {
		t.Fatalf("taskTraces = %v, %v", got, err)
	}
	if g := got[0]; g.Calls != 5 || g.Ended != "ship" || g.Machine != "m1" || len(g.Files) != 1 || !g.Files[0].Edited || g.Files[0].Repo != "acme/app" || g.CtxFirstEdit == nil {
		t.Errorf("stored = %+v", g)
	}
	e.ingestTraces(e.me, "c5", "m2", e.trace(i.Key, "s1", 1, h-5000, 2, ""))
	got, _ = e.s.taskTraces(e.ctx, i.ID)
	if len(got) != 2 || got[0].Machine != "m2" {
		t.Fatalf("two machines, oldest first: %+v", got)
	}

	bad := e.trace(i.Key, "s2", 1, h, 1, "")
	bad.Files = []TraceFile{{Path: "/home/pat/x.go"}, {Path: "../x.go"}, {Path: "ok.go", Repo: "not a repository"}}
	badEnd := e.trace(i.Key, "s3", 1, h, 1, "bogus")
	before := len(landed)
	r = e.ingestTraces(e.me, "c6", "m1", e.trace("WEB-999", "s2", 1, h, 1, ""), e.trace(hidden.Key, "s2", 2, h, 1, ""), badEnd, bad)
	if r.Traces != 1 || r.TracesDropped != 3 {
		t.Fatalf("drops: %+v", r)
	}
	if len(landed) != before+1 {
		t.Errorf("landed %v", landed)
	}
	if n, _ := e.s.taskTraces(e.ctx, hidden.ID); len(n) != 0 {
		t.Errorf("a trace of an unreadable task was stored: %+v", n)
	}
	all, _ := e.s.taskTraces(e.ctx, i.ID)
	for _, g := range all {
		if g.Session == "s2" && (len(g.Files) != 1 || g.Files[0].Path != "ok.go" || g.Files[0].Repo != "") {
			t.Errorf("absolute and outside paths kept: %+v", g.Files)
		}
	}

	again := e.trace(i.Key, "s1", 1, h, 5, "ship")
	again.PreCalls, again.Edits = 2, 1
	if r = e.ingestTraces(e.me, "c8", "m1", again); r.Traces != 1 {
		t.Errorf("read again: %+v", r)
	}
	huge := e.trace(i.Key, "s4", 1, h, 1, "")
	huge.Calls, huge.PreCalls, huge.Reads, huge.CtxStart = 1<<62, 1<<62, 1<<62, 1<<62
	huge.Files[0].Nav = 1 << 62
	early := e.trace(i.Key, "s5", 1, h, 1, "")
	fe := h - 1
	early.FirstEditAt = &fe
	if r = e.ingestTraces(e.me, "c9", "m1", huge, early); r.Traces != 1 || r.TracesDropped != 1 {
		t.Errorf("bounds: %+v", r)
	}
	all, _ = e.s.taskTraces(e.ctx, i.ID)
	for _, g := range all {
		if g.Session == "s4" && (g.Calls != traceMaxCount || g.PreCalls != traceMaxCount || g.CtxStart != traceMaxCtx || g.Files[0].Nav != traceMaxCount) {
			t.Errorf("unbounded counts kept: %+v", g)
		}
	}

	r = e.ingest(e.me, "c7", "m1", UsageChunk{Messages: []UsageMessageRow{msg(h, "s9", "a/b", "claude-opus-5-5", "main", 1, 1, 1, 0, 0, 0)}})
	if r.Traces != 0 || len(landed) != before+3 {
		t.Errorf("no traces: %+v", r)
	}
}
