package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func slotHoldBody(t *testing.T, n int) []byte {
	t.Helper()
	var hs []core.SlotHold
	for i := range n {
		hs = append(hs, core.SlotHold{ID: fmt.Sprintf("h%d", i), Name: "tests", Class: "short", QueuedAt: time.Now().UnixMilli() + int64(i), HoldMS: 1000, Slots: 2, LongSlots: 1})
	}
	b, err := json.Marshal(map[string]any{"holds": hs})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSlotHoldsRoundTrip(t *testing.T) {
	a := newTranscriptAPI(t)
	hdr := map[string]string{"Content-Type": "application/json"}
	var res core.SlotHoldsResult
	if code := a.human.json("POST", "/api/v1/slot-holds?machine=box&pending=3&oldest=1&dropped=0", json.RawMessage(slotHoldBody(t, 3)), &res); code != 200 || res.Stored != 3 {
		t.Fatalf("post: %d %+v", code, res)
	}
	if resp, err := a.req("POST", "/api/v1/slot-holds?machine=box&pending=3", slotHoldBody(t, 3), hdr); err != nil || resp.StatusCode != 200 {
		t.Fatalf("resend: %v %v", resp, err)
	}
	var got []core.SlotHold
	if code := a.human.json("GET", "/api/v1/slot-holds?machine=box&limit=2", nil, &got); code != 200 || len(got) != 2 || got[0].ID != "h1" || got[0].Machine != "box" {
		t.Fatalf("get: %d %+v", code, got)
	}
	if code := a.human.json("GET", "/api/v1/slot-holds?since=abc", nil, nil); code != 400 {
		t.Errorf("bad since: %d", code)
	}
}

func TestSlotHoldsOversizeBatchRefusedButMachineRowWritten(t *testing.T) {
	a := newTranscriptAPI(t)
	if code := a.human.json("POST", "/api/v1/slot-holds?machine=box&pending=501&dropped=7", json.RawMessage(slotHoldBody(t, core.SlotHoldsMax+1)), nil); code != 400 {
		t.Fatalf("oversize: %d", code)
	}
	var got []core.SlotHold
	if code := a.human.json("GET", "/api/v1/slot-holds", nil, &got); code != 200 || len(got) != 0 {
		t.Errorf("stored %d (%d)", len(got), code)
	}
	ctx := context.Background()
	a.svc.CheckCollectorHealth(ctx)
	hs, err := a.svc.CollectorHealthList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hs {
		if h.Name == "slot holds" {
			found = true
			if h.Waiting != 1 {
				t.Errorf("slot holds waiting = %d, want 1 (the machine's pending was not recorded)", h.Waiting)
			}
		}
	}
	if !found {
		t.Error("no slot holds row in collector health")
	}
}

// A wp newer than the server sends fields it doesn't know: the batch is still
// stored. A body that can't be read is refused, but what the machine says is
// waiting is kept, so the collector health shows it (WP-1646).
func TestSlotHoldsUnknownFieldsAndUnreadableBody(t *testing.T) {
	a := newTranscriptAPI(t)
	hdr := map[string]string{"Content-Type": "application/json"}
	body := fmt.Sprintf(`{"later":1,"holds":[{"id":"n1","name":"tests","class":"short","queued_at":%d,"new_field":"x"}]}`, time.Now().UnixMilli())
	if resp, err := a.req("POST", "/api/v1/slot-holds?machine=box&pending=1", []byte(body), hdr); err != nil || resp.StatusCode != 200 {
		t.Fatalf("unknown fields: %v %v", resp, err)
	}
	var got []core.SlotHold
	if code := a.human.json("GET", "/api/v1/slot-holds", nil, &got); code != 200 || len(got) != 1 || got[0].ID != "n1" {
		t.Fatalf("stored: %d %+v", code, got)
	}
	if resp, err := a.req("POST", "/api/v1/slot-holds?machine=box&pending=9&oldest=1000", []byte(`{"holds":`), hdr); err != nil || resp.StatusCode != 400 {
		t.Fatalf("unreadable body: %v %v", resp, err)
	}
}

// One hold with a wrongly typed field does not refuse the batch: it is
// counted as rejected and the rest are stored (WP-1646).
func TestSlotHoldsBadHoldRejectedRestStored(t *testing.T) {
	a := newTranscriptAPI(t)
	hdr := map[string]string{"Content-Type": "application/json"}
	body := fmt.Sprintf(`{"holds":[{"id":"ok","name":"tests","class":"short","queued_at":%d},{"id":"bad","name":"tests","class":"short","queued_at":%d,"slot":"x"}]}`,
		time.Now().UnixMilli(), time.Now().UnixMilli())
	resp, err := a.req("POST", "/api/v1/slot-holds?machine=box&pending=2", []byte(body), hdr)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("post: %v %v", resp, err)
	}
	var res core.SlotHoldsResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil || res.Stored != 1 || res.Rejected != 1 {
		t.Errorf("result %+v %v", res, err)
	}
	var got []core.SlotHold
	if code := a.human.json("GET", "/api/v1/slot-holds", nil, &got); code != 200 || len(got) != 1 || got[0].ID != "ok" {
		t.Errorf("stored %d %+v", code, got)
	}
}

func TestSlotHoldsBadQueryAndReadOnlyToken(t *testing.T) {
	a := newTranscriptAPI(t)
	hdr := map[string]string{"Content-Type": "application/json"}
	for _, q := range []string{"pending=-1", "oldest=x", "dropped=x"} {
		resp, err := a.req("POST", "/api/v1/slot-holds?machine=box&"+q, slotHoldBody(t, 1), hdr)
		if err != nil || resp.StatusCode != 400 {
			t.Errorf("%s: %v %v", q, resp, err)
		}
	}
	if code := a.human.json("GET", "/api/v1/slot-holds?until=abc", nil, nil); code != 400 {
		t.Errorf("bad until: %d", code)
	}
	u, err := a.svc.CreateUser(context.Background(), core.SystemActor(), core.CreateUserInput{Username: "ro", Email: "ro@example.com", Password: "correct-horse-battery"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := a.svc.CreateAPIToken(context.Background(), &core.Actor{User: u}, "ro", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	ro := &client{t: t, base: a.ts.URL, http: http.DefaultClient, token: raw}
	if code := ro.json("POST", "/api/v1/slot-holds?machine=box", json.RawMessage(slotHoldBody(t, 1)), nil); code != 403 {
		t.Errorf("read-only post: %d", code)
	}
}

// A runner job's token may send holds through the route, as it sends usage
// (WP-1646): the permission is checked here at the HTTP layer, not only in core.
func TestSlotHoldsRunnerJobTokenPosts(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct horse battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	robin := &core.Actor{User: u}
	r, err := svc.RegisterRunner(ctx, robin, core.RunnerRegistration{Name: "box", Hostname: "box", OS: "linux", Arch: "amd64", Version: "test",
		AllowedDirs: []string{"/work"}, CLIs: []core.RunnerCLI{{Name: "claude"}}})
	if err != nil {
		t.Fatal(err)
	}
	input := `{"cli":"claude","prompt":"hi","repo":"https://x.test/a/b.git","permission":"edit_run"}`
	if _, err := svc.EnqueueRunnerJob(ctx, robin, core.RunnerJobInput{Kind: core.JobAgent, Input: []byte(input)}); err != nil {
		t.Fatal(err)
	}
	zero := 0.0
	w, err := svc.RunnerPoll(ctx, robin, core.RunnerPoll{Runner: r.ID, Wait: &zero})
	if err != nil || w.Job == nil || w.Job.Token == "" {
		t.Fatalf("poll: %v %+v", err, w)
	}
	job := &client{t: t, base: ts.URL, http: http.DefaultClient, token: w.Job.Token}
	body := map[string]any{"holds": []map[string]any{{"id": "j1", "name": "tests", "class": "short", "queued_at": time.Now().UnixMilli()}}}
	code, b := job.do("POST", "/api/v1/slot-holds?machine=box&pending=1", body)
	var res core.SlotHoldsResult
	if code != 200 || json.Unmarshal(b, &res) != nil || res.Stored != 1 {
		t.Fatalf("a job's token could not send a hold: %d %s", code, b)
	}
	got, err := svc.SlotHolds(ctx, robin, core.SlotHoldsFilter{})
	if err != nil || len(got) != 1 || got[0].ID != "j1" {
		t.Errorf("stored %+v %v", got, err)
	}
}
