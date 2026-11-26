package core

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestAttachmentsOnRequestsAndSessions(t *testing.T) {
	r := newReqEnv(t)
	q := r.ask(RequestInput{Kind: RequestQuestion, Title: "Which one?", Questions: choiceQ()})
	stranger := r.user("stranger", false)
	req := AttachTarget{Type: TargetRequest, ID: q.ID}
	sess := AttachTarget{Type: TargetSession, ID: r.session}

	if _, err := r.attach(r.ana, req, "shot.png", pngBytes); err != nil {
		t.Fatalf("ana on her request: %v", err)
	}
	if _, err := r.attach(r.agent, req, "trace.log", []byte("12:00 started\n")); err != nil {
		t.Fatalf("her agent on the request: %v", err)
	}
	if _, err := r.attach(r.admin, req, "admin.png", pngBytes); err != nil {
		t.Fatalf("admin on the request: %v", err)
	}
	if _, err := r.attach(r.bob, req, "x.png", pngBytes); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob on the request: %v", err)
	}
	if _, err := r.s.ListAttachments(r.ctx, stranger, req); !errors.Is(err, ErrNotFound) {
		t.Errorf("stranger listing the request's files: %v", err)
	}
	if list, err := r.s.ListAttachments(r.ctx, r.ana, req); err != nil || len(list) != 3 || list[0].TargetType != TargetRequest {
		t.Errorf("request files: %v %v", list, err)
	}
	ro := &Actor{User: r.ana.User, ReadOnly: true}
	if _, err := r.attach(ro, req, "y.png", pngBytes); !errors.Is(err, ErrForbidden) {
		t.Errorf("read-only: %v", err)
	}

	shot, err := r.attach(r.agent, sess, "screen.png", pngBytes)
	if err != nil {
		t.Fatalf("agent on its own session: %v", err)
	}
	if _, err := r.attach(r.bob, sess, "x.png", pngBytes); !errors.Is(err, ErrForbidden) {
		t.Errorf("a project member added to someone's session: %v", err)
	}
	if list, err := r.s.ListAttachments(r.ctx, r.bob, sess); err != nil || len(list) != 1 {
		t.Errorf("bob reading the session's files: %v %v", list, err)
	}
	if _, data, _, err := r.s.AttachmentData(r.ctx, r.bob, shot.ID); err != nil || len(data) != len(pngBytes) {
		t.Errorf("bob downloading: %v", err)
	}
	if _, err := r.s.ListAttachments(r.ctx, stranger, sess); !errors.Is(err, ErrNotFound) {
		t.Errorf("stranger on the session: %v", err)
	}
	if _, _, _, err := r.s.AttachmentData(r.ctx, stranger, shot.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("stranger downloading: %v", err)
	}
	if err := r.s.DeleteAttachment(r.ctx, r.bob, shot.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("bob deleting: %v", err)
	}
	if err := r.s.DeleteAttachment(r.ctx, r.ana, shot.ID); err != nil {
		t.Errorf("ana deleting: %v", err)
	}
}

func TestResolveRequestAndSessionTargets(t *testing.T) {
	r := newReqEnv(t)
	q := r.ask(RequestInput{Kind: RequestQuestion, Title: "Which one?", Questions: choiceQ()})

	for _, spec := range []string{"request:" + q.Ref, "request:" + q.Code, fmt.Sprintf("request:%d", q.ID)} {
		if tg, err := r.s.ResolveAttachTarget(r.ctx, r.ana, spec, ""); err != nil || tg.Type != TargetRequest || tg.ID != q.ID {
			t.Errorf("%s: %+v %v", spec, tg, err)
		}
	}
	if _, err := r.s.ResolveAttachTarget(r.ctx, r.bob, "request:"+q.Ref, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob resolving the request: %v", err)
	}
	if _, err := r.s.ResolveAttachTarget(r.ctx, r.ana, "request:R-99", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("a request that isn't: %v", err)
	}
	for name, c := range map[string]struct {
		actor *Actor
		spec  string
	}{
		"in the session":   {r.agent, "session:cli-1"},
		"without a header": {r.ana, "session:cli-1"},
		"by number":        {r.ana, fmt.Sprintf("session:%d", r.session)},
	} {
		if tg, err := r.s.ResolveAttachTarget(r.ctx, c.actor, c.spec, ""); err != nil || tg.Type != TargetSession || tg.ID != r.session {
			t.Errorf("%s: %+v %v", name, tg, err)
		}
	}
	if _, err := r.s.ResolveAttachTarget(r.ctx, r.bob, "session:cli-1", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob resolving ana's CLI id: %v", err)
	}
	if _, err := r.s.ResolveAttachTarget(r.ctx, r.ana, "session:nope", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session: %v", err)
	}
}

func TestSessionUsageJoinsTheUsageTables(t *testing.T) {
	r := newReqEnv(t)
	hour := r.clockNow().Add(-time.Hour).Truncate(time.Hour).UnixMilli()
	chunk := UsageChunk{ID: "c1",
		Messages: []UsageMessageRow{
			msg(hour, "cli-1", "acme/app", "claude-opus-5-5", "main", 3, 1000, 400, 200, 0, 9000),
			msg(hour, "cli-1", "acme/app", "claude-haiku-5-5", "Explore", 1, 100, 50, 0, 0, 500),
			msg(hour, "cli-other", "acme/app", "claude-opus-5-5", "main", 1, 7777, 7777, 0, 0, 0),
		},
		Tools: []UsageToolRow{
			{Hour: hour, Session: "cli-1", Kind: "builtin", Tool: "Bash", Calls: 9, Errors: 1},
			{Hour: hour, Session: "cli-1", Kind: "bash", Tool: "Bash", Detail: "go test", Calls: 4},
			{Hour: hour, Session: "cli-other", Kind: "builtin", Tool: "Read", Calls: 50},
		},
		Refusals: []UsageRefusal{{At: hour + 5, Session: "cli-1", Tool: "Bash", Detail: "rm -rf", Reason: "Production Deploy", Source: "classifier"}},
	}
	if _, err := r.s.IngestUsage(r.ctx, r.ana, UsageIngest{Machine: "box", Chunks: []UsageChunk{chunk}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.IngestUsage(r.ctx, r.ana, UsageIngest{Machine: "lap", Chunks: []UsageChunk{{ID: "c2", Messages: []UsageMessageRow{
		msg(hour, "cli-1", "acme/app", "claude-opus-5-5", "main", 1, 5555, 0, 0, 0, 0)}}}}); err != nil {
		t.Fatal(err)
	}

	u, err := r.s.GetSessionUsage(r.ctx, r.ana, r.session)
	if err != nil {
		t.Fatal(err)
	}
	if !u.HasData || u.Totals.Messages != 4 || u.Totals.Input != 1100 || u.Totals.Output != 450 || u.Totals.CacheWrite != 200 || u.Totals.CacheRead != 9500 {
		t.Errorf("totals: %+v", u.Totals)
	}
	if len(u.Models) != 2 || len(u.Kinds) != 2 {
		t.Errorf("models %d, kinds %d", len(u.Models), len(u.Kinds))
	}
	var bash, builtin bool
	for _, tl := range u.Tools {
		builtin = builtin || (tl.Name == "Bash" && tl.Calls == 9 && tl.Errors == 1)
		if tl.Name == "Read" {
			t.Error("another session's tool")
		}
	}
	for _, b := range u.Bash {
		bash = bash || (b.Name == "go test" && b.Calls == 4)
	}
	if !builtin || !bash || u.Refusals.Total != 1 || u.Refusals.Recent[0].Reason != "Production Deploy" {
		t.Errorf("tools %+v, bash %+v, refusals %+v", u.Tools, u.Bash, u.Refusals)
	}
	if bobs, err := r.s.GetSessionUsage(r.ctx, r.bob, r.session); err != nil || bobs.HasData || !bobs.Private {
		t.Errorf("bob: %+v %v", bobs, err)
	}
	if adm, err := r.s.GetSessionUsage(r.ctx, r.admin, r.session); err != nil || !adm.HasData || adm.Totals.Messages != 4 {
		t.Errorf("admin: %+v %v", adm, err)
	}
	if own, err := r.s.GetSessionUsage(r.ctx, r.agent, r.session); err != nil || !own.HasData || own.Private {
		t.Errorf("the session's agent: %+v %v", own, err)
	}
	adminsAgent := agentActor(r.admin, "anthropic/claude-code")
	if other, err := r.s.GetSessionUsage(r.ctx, adminsAgent, r.session); err != nil || other.HasData || !other.Private {
		t.Errorf("an admin's agent on someone else's session: %+v %v", other, err)
	}
	if _, err := r.s.GetSessionUsage(r.ctx, r.user("stranger", false), r.session); !errors.Is(err, ErrNotFound) {
		t.Errorf("stranger: %v", err)
	}
	res, err := r.s.SessionHook(r.ctx, agentActor(r.ana, "anthropic/claude-code"), []HookEvent{hook("box", "cli-new", "SessionStart", nil)})
	if err != nil {
		t.Fatal(err)
	}
	if none, err := r.s.GetSessionUsage(r.ctx, r.ana, res.Session); err != nil || none.HasData {
		t.Errorf("empty: %+v %v", none, err)
	}

	rows, err := r.s.UsageBreakdowns(r.ctx, r.ana, UsageQuery{Days: 30}, "session", 50)
	if err != nil {
		t.Fatal(err)
	}
	linked := map[string]int64{}
	for _, row := range rows {
		linked[row.Machine+"/"+row.Key] = row.SessionID
	}
	if linked["box/cli-1"] != r.session || linked["lap/cli-1"] != 0 || linked["box/cli-other"] != 0 || len(rows) != 3 {
		t.Errorf("session rows: %v", linked)
	}
}
