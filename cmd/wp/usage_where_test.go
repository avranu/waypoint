package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// whLines builds transcript lines for the where tests.
type whLines struct{ b strings.Builder }

func (w *whLines) raw(v any) {
	j, _ := json.Marshal(v)
	w.b.Write(j)
	w.b.WriteByte('\n')
}

func (w *whLines) user(ts, text string) {
	w.raw(map[string]any{"type": "user", "timestamp": ts, "cwd": "/work/proj", "message": map[string]any{"role": "user", "content": text}})
}

func (w *whLines) result(ts, toolID string) {
	w.raw(map[string]any{"type": "user", "timestamp": ts, "cwd": "/work/proj", "message": map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": toolID, "content": "ok"}}}})
}

// call writes one assistant record of the API call (id, req) with usage and tool blocks.
func (w *whLines) call(ts, id, req, model string, out int64, blocks ...map[string]any) {
	content := []any{}
	for _, b := range blocks {
		content = append(content, b)
	}
	w.raw(map[string]any{"type": "assistant", "timestamp": ts, "cwd": "/work/proj", "requestId": req, "message": map[string]any{
		"id": id, "model": model, "content": content,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": out, "cache_read_input_tokens": 1000, "cache_creation_input_tokens": 200,
			"cache_creation": map[string]any{"ephemeral_5m_input_tokens": 100, "ephemeral_1h_input_tokens": 100}}}})
}

// resultErr writes a tool_result that is an error (a failed command).
func (w *whLines) resultErr(ts, toolID string) {
	w.raw(map[string]any{"type": "user", "timestamp": ts, "cwd": "/work/proj", "message": map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": toolID, "is_error": true, "content": "check failed"}}}})
}

func bash(id, cmd string) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": cmd}}
}

func whWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const whWin = "--since=2026-11-24T00:00:00Z"

func whRun(t *testing.T, dir string, args ...string) (*whReport, string) {
	t.Helper()
	var out bytes.Buffer
	c := &cli{out: &out, dir: t.TempDir()}
	if err := cmdUsageWhere(c, append([]string{"--dir", dir, "--json"}, args...)); err != nil {
		t.Fatal(err)
	}
	var r whReport
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	return &r, out.String()
}

func whClass(r *whReport, model, class string) whClassRow {
	for _, a := range r.Activity {
		if a.Model == model {
			for _, k := range a.Classes {
				if k.Class == class {
					return k
				}
			}
		}
	}
	return whClassRow{}
}

func TestWhereDedupeKeepsLargest(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T10:00:00Z", "do it")
	w.call("2026-11-24T10:00:01Z", "m1", "r1", "claude-sonnet-5-5", 5)
	w.call("2026-11-24T10:00:02Z", "m1", "r1", "claude-sonnet-5-5", 500)
	w.call("2026-11-24T10:00:03Z", "m1", "r1", "claude-sonnet-5-5", 50)
	w.call("2026-11-24T10:00:04Z", "m1", "r2", "claude-sonnet-5-5", 7) // same message id, another request: its own call
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	r, _ := whRun(t, dir, whWin)
	if r.Calls != 2 {
		t.Fatalf("calls = %d, want 2", r.Calls)
	}
	want := 500*10.0/1e6 + 7*10.0/1e6 // sonnet output $10/M
	if got := r.Parts.Output; got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("output cost %v, want %v (the largest of the repeats)", got, want)
	}
	if r.Parts.Write5m == 0 || r.Parts.Write1h == 0 || r.Parts.Read == 0 {
		t.Fatalf("cache split missing: %+v", r.Parts)
	}
}

func TestWhereSyntheticCountedNotCosted(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T10:00:00Z", "go")
	w.raw(map[string]any{"type": "assistant", "timestamp": "2026-11-24T10:00:01Z", "message": map[string]any{"id": "s1", "model": "<synthetic>",
		"content": []any{map[string]any{"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{"file_path": "/work/proj/a.go"}}},
		"usage":   map[string]any{"input_tokens": 0, "output_tokens": 0}}})
	w.call("2026-11-24T10:00:02Z", "m1", "r1", "claude-sonnet-5-5", 5)
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	r, _ := whRun(t, dir, whWin)
	if r.Calls != 1 || r.NotKnown.Synthetic != 1 || r.NotKnown.ScriptTools[whReadFile] != 1 {
		t.Fatalf("calls %d synthetic %d scripts %v", r.Calls, r.NotKnown.Synthetic, r.NotKnown.ScriptTools)
	}
}

func TestWhereCompoundShellEditAndTests(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T10:00:00Z", "fix")
	w.call("2026-11-24T10:00:01Z", "m1", "r1", "claude-fable-5", 5, bash("b1", "cd x && sed -i 's/a/b/' f.go && go test ./..."))
	w.result("2026-11-24T10:00:02Z", "b1")
	w.call("2026-11-24T10:00:03Z", "m2", "r2", "claude-fable-5", 5, bash("b2", "cat > /tmp/note.txt <<'EOF'\nhello\nEOF"))
	w.result("2026-11-24T10:00:04Z", "b2")
	w.call("2026-11-24T10:00:05Z", "m3", "r3", "claude-fable-5", 5, bash("b3", "d=$(mktemp -d) && echo hi > $d/x.py && python3 $d/x.py"))
	w.call("2026-11-24T10:00:06Z", "m4", "r4", "claude-fable-5", 5)
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	r, _ := whRun(t, dir, whWin)
	if k := whClass(r, "claude-fable-5", whShellEdit); k.Calls != 1 {
		t.Fatalf("shell edit: %+v in %+v", k, r.Activity)
	}
	if k := whClass(r, "claude-fable-5", whTests); k.Calls != 1 {
		t.Fatalf("tests: %+v", k)
	}
	if k := whClass(r, "claude-fable-5", whScratch); k.Calls != 2 {
		t.Fatalf("scratch writes (/tmp and mktemp): %+v", k)
	}
	if k := whClass(r, "claude-fable-5", whTextOnly); k.Calls != 1 {
		t.Fatalf("text only: %+v", k)
	}
	if k := whClass(r, "claude-fable-5", whToolEdit); k.Calls != 0 {
		t.Fatalf("edit tool counted for shell edits: %+v", k)
	}
	// the first call's cost is split over shell edit and tests
	if a, b := whClass(r, "claude-fable-5", whShellEdit), whClass(r, "claude-fable-5", whTests); a.Cost != b.Cost {
		t.Fatalf("a compound call is split equally: %v vs %v", a.Cost, b.Cost)
	}
	trig := map[string]int{}
	for _, tr := range r.Triggers {
		trig[tr.Trigger] = tr.Calls
	}
	if trig["typed prompt"] != 1 || trig["tool result: Bash"] != 2 || trig["continuation"] != 1 {
		t.Fatalf("triggers %v", trig)
	}
}

func TestWherePhaseAroundShip(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T10:00:00Z", "Handle WP-1 and ship")
	w.call("2026-11-24T10:00:01Z", "m1", "r1", "claude-fable-5", 5, bash("b1", "wp ship -h"))
	w.call("2026-11-24T10:00:02Z", "m2", "r2", "claude-fable-5", 5, bash("b2", "wp ship --review"))
	w.result("2026-11-24T10:00:02Z", "b2")
	w.call("2026-11-24T10:00:03Z", "m3", "r3", "claude-fable-5", 5, bash("b3", "git add -A && wp ship"))
	w.result("2026-11-24T10:00:03Z", "b3")
	w.call("2026-11-24T10:00:04Z", "m4", "r4", "claude-fable-5", 5)
	w.call("2026-11-24T10:00:05Z", "m5", "r5", "claude-fable-5", 5, bash("b5", "wp ship"))
	w.result("2026-11-24T10:00:05Z", "b5")
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	var w2 whLines
	w2.user("2026-11-24T10:00:00Z", "never ships")
	w2.call("2026-11-24T10:00:01Z", "n1", "q1", "claude-fable-5", 5, bash("c1", "wp ship --no-ship"))
	whWrite(t, filepath.Join(dir, "projects", "p", "s2.jsonl"), w2.b.String())
	r, _ := whRun(t, dir, whWin)
	ph := r.Phase
	// --review pushes the branch, so it is a ship; -h and --no-ship are not
	if ph.Shipped != 1 || ph.Ships != 3 || ph.NoShipSession != 1 {
		t.Fatalf("phase %+v", ph)
	}
	one := r.Parts.total() / 6 // six calls, equal cost
	if d := ph.Before - 2*one; d < -1e-9 || d > 1e-9 {
		t.Fatalf("before %v, want two calls (%v each)", ph.Before, one)
	}
	if d := ph.After - 3*one; d < -1e-9 || d > 1e-9 {
		t.Fatalf("after %v, want three calls", ph.After)
	}
	if len(r.Top) == 0 || r.Top[0].Model != "claude-fable-5" || r.Top[0].AvgCtx != 1210 || r.Top[0].Ships != 3 || !strings.HasPrefix(r.Top[0].Prompt, "Handle WP-1") {
		t.Fatalf("top %+v", r.Top)
	}
}

// limitErr writes a limit error record. ri is its rate_limit_info (nil for none).
func (w *whLines) limitErr(ts, id, text string, ri map[string]any) {
	rec := map[string]any{"type": "assistant", "timestamp": ts, "isApiErrorMessage": true, "apiError": "usage_limit_reached", "message": map[string]any{"id": id, "model": "<synthetic>",
		"content": []any{map[string]any{"type": "text", "text": text}}}}
	if ri != nil {
		rec["apiErrorParams"] = map[string]any{"rate_limit_info": ri}
	}
	w.raw(rec)
}

func weeklyOff(resets int64) map[string]any {
	return map[string]any{"status": "rejected", "resetsAt": resets, "rateLimitType": "seven_day", "overageStatus": "rejected", "overageDisabledReason": "org_level_disabled", "isUsingOverage": false}
}

func whLimitRun(t *testing.T, w *whLines, args ...string) (*whReport, string) {
	t.Helper()
	dir := t.TempDir()
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	r, _ := whRun(t, dir, append([]string{whWin}, args...)...)
	var out bytes.Buffer
	c := &cli{out: &out, dir: t.TempDir()}
	if err := cmdUsageWhere(c, append([]string{"--dir", dir, whWin}, args...)); err != nil {
		t.Fatal(err)
	}
	return r, out.String()
}

func resetAt(s string) int64 {
	ts, _ := time.Parse(time.RFC3339, s)
	return ts.Unix()
}

func TestWhereLimitEvent(t *testing.T) {
	var w whLines
	w.user("2026-11-24T10:00:00Z", "go")
	w.call("2026-11-24T10:00:01Z", "m1", "r1", "claude-sonnet-5-5", 5)
	w.limitErr("2026-11-24T10:00:02Z", "e1", "You've hit your weekly limit", weeklyOff(resetAt("2026-11-28T17:00:00Z")))
	w.call("2026-11-24T10:00:03Z", "m2", "r2", "claude-sonnet-5-5", 5)
	w.call("2026-11-24T10:00:04Z", "m3", "r3", "claude-sonnet-5-5", 5)
	r, text := whLimitRun(t, &w)
	if len(r.Limits.Incidents) != 1 {
		t.Fatalf("limits %+v", r.Limits)
	}
	in := r.Limits.Incidents[0]
	if in.Time != "2026-11-24T10:00:02Z" || in.Type != "seven_day" || in.Records != 1 || in.ResetsAt != "2026-11-28T17:00:00Z" || !strings.Contains(in.Text, "weekly limit") {
		t.Fatalf("incident %+v", in)
	}
	if in.ExtraUsage != "extra usage: off (org_level_disabled)" || in.UsingOverage || in.SpendMayBeExtra != nil {
		t.Fatalf("overage %+v", in)
	}
	if r.NotKnown.Synthetic != 0 {
		t.Fatalf("an error message is not a script record: %d", r.NotKnown.Synthetic)
	}
	for _, want := range []string{"seven_day limit, stated reset 2026-11-28T17:00:00Z, 1 records", "extra usage: off (org_level_disabled); calls resumed after 1.0s",
		"calls resumed before the stated reset; the allowance was reset early or the limit was for another bucket"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "spend after") || strings.Contains(text, "spend from the first") {
		t.Errorf("no dollar figure after the message when extra usage is off:\n%s", text)
	}
}

func TestWhereLimitLockout(t *testing.T) {
	var w whLines
	w.user("2026-11-21T08:00:00Z", "go")
	w.call("2026-11-21T08:50:00Z", "m1", "r1", "claude-sonnet-5-5", 5)
	reset := resetAt("2026-11-21T17:15:00Z")
	// Three records of one incident, then a second session's call after the reset.
	w.limitErr("2026-11-21T08:56:00Z", "e1", "You've hit your weekly limit", weeklyOff(reset))
	w.limitErr("2026-11-21T09:10:00Z", "e2", "You've hit your weekly limit", weeklyOff(reset))
	w.limitErr("2026-11-21T09:30:00Z", "e3", "You've hit your weekly limit", weeklyOff(reset))
	w.call("2026-11-21T17:16:00Z", "m2", "r2", "claude-sonnet-5-5", 5)
	r, text := whLimitRun(t, &w, "--since=2026-11-21T00:00:00Z")
	if len(r.Limits.Incidents) != 1 || r.Limits.Incidents[0].Records != 3 {
		t.Fatalf("incidents %+v", r.Limits.Incidents)
	}
	in := r.Limits.Incidents[0]
	if in.LockoutSeconds == nil || int(*in.LockoutSeconds) != 8*3600+20*60 || in.Lockout != "no successful call for 8h20m" || in.ResumedEarly {
		t.Fatalf("lockout %+v", in)
	}
	if !strings.Contains(text, "no successful call for 8h20m") || strings.Contains(text, "resumed before the stated reset") {
		t.Errorf("text:\n%s", text)
	}
}

func TestWhereLimitNoResume(t *testing.T) {
	var w whLines
	w.user("2026-11-24T10:00:00Z", "go")
	w.limitErr("2026-11-24T10:00:02Z", "e1", "You've hit your weekly limit", weeklyOff(resetAt("2026-11-28T17:00:00Z")))
	r, text := whLimitRun(t, &w)
	if in := r.Limits.Incidents[0]; in.LockoutSeconds != nil || in.Lockout != "no successful call before the window ended" {
		t.Fatalf("incident %+v", in)
	}
	if !strings.Contains(text, "no successful call before the window ended") {
		t.Errorf("text:\n%s", text)
	}
}

func TestWhereLimitIncidentsSplit(t *testing.T) {
	var w whLines
	w.user("2026-11-24T10:00:00Z", "go")
	reset := resetAt("2026-11-28T17:00:00Z")
	w.limitErr("2026-11-24T10:00:02Z", "e1", "You've hit your weekly limit", weeklyOff(reset))
	w.limitErr("2026-11-24T10:00:03Z", "e2", "You've hit your weekly limit", weeklyOff(reset))
	w.call("2026-11-24T10:00:04Z", "m1", "r1", "claude-sonnet-5-5", 5)
	// A success in between, so the same type and reset is a new incident.
	w.limitErr("2026-11-24T10:00:05Z", "e3", "You've hit your weekly limit", weeklyOff(reset))
	// A different bucket is its own incident.
	opus := weeklyOff(reset)
	opus["rateLimitType"] = "seven_day_opus"
	w.limitErr("2026-11-24T10:00:06Z", "e4", "You've hit your Opus limit", opus)
	r, _ := whLimitRun(t, &w)
	var got []string
	for _, in := range r.Limits.Incidents {
		got = append(got, fmt.Sprintf("%s/%d", in.Type, in.Records))
	}
	if strings.Join(got, " ") != "seven_day/2 seven_day/1 seven_day_opus/1" {
		t.Fatalf("incidents %v", got)
	}
}

func TestWhereLimitUnclassifiedAndOtherErrors(t *testing.T) {
	var w whLines
	w.user("2026-11-24T10:00:00Z", "go")
	w.limitErr("2026-11-24T10:00:02Z", "e1", "You've hit your weekly limit, try later", nil)
	w.raw(map[string]any{"type": "assistant", "timestamp": "2026-11-24T10:00:03Z", "isApiErrorMessage": true, "apiError": "connection_failed", "message": map[string]any{"id": "e2", "model": "<synthetic>",
		"content": []any{map[string]any{"type": "text", "text": "Connection lost, retrying"}}}})
	w.call("2026-11-24T10:00:04Z", "m1", "r1", "claude-sonnet-5-5", 5)
	r, text := whLimitRun(t, &w)
	if len(r.Limits.Incidents) != 1 || r.Limits.Incidents[0].Type != "unclassified" || r.Limits.Incidents[0].ResetsAt != "" || r.Limits.Incidents[0].ExtraUsage != "extra usage: not stated by the record" {
		t.Fatalf("incidents %+v", r.Limits.Incidents)
	}
	if r.Limits.Incidents[0].ResumedEarly {
		t.Errorf("no stated reset to resume before")
	}
	if r.Limits.OtherErrs["connection_failed"] != 1 || len(r.Limits.OtherErrs) != 1 {
		t.Fatalf("other errors %+v", r.Limits.OtherErrs)
	}
	if !strings.Contains(text, "other API errors, not limits: connection_failed 1") {
		t.Errorf("text:\n%s", text)
	}
}

func TestWhereLimitOverageInUse(t *testing.T) {
	var w whLines
	w.user("2026-11-24T10:00:00Z", "go")
	w.call("2026-11-24T10:00:01Z", "m0", "r0", "claude-sonnet-5-5", 5)
	ri := weeklyOff(resetAt("2026-11-24T10:00:04Z"))
	ri["overageStatus"], ri["isUsingOverage"] = "allowed", true
	delete(ri, "overageDisabledReason")
	w.limitErr("2026-11-24T10:00:02Z", "e1", "You've hit your weekly limit", ri)
	w.call("2026-11-24T10:00:03Z", "m1", "r1", "claude-sonnet-5-5", 5)
	w.call("2026-11-24T10:00:04Z", "m2", "r2", "claude-sonnet-5-5", 5)
	w.call("2026-11-24T10:00:05Z", "m3", "r3", "claude-sonnet-5-5", 5)
	r, text := whLimitRun(t, &w)
	in := r.Limits.Incidents[0]
	if in.ExtraUsage != "extra usage in use" || !in.UsingOverage || in.ResumedEarly || in.SpendMayBeExtra == nil {
		t.Fatalf("incident %+v", in)
	}
	// m1 and m2 are at or before the stated reset; m3 is after it.
	if d := *in.SpendMayBeExtra - r.Cost/4*2; d < -1e-9 || d > 1e-9 {
		t.Fatalf("spend %v of %v", *in.SpendMayBeExtra, r.Cost)
	}
	if !strings.Contains(text, "spend after the message, may be extra usage: $") {
		t.Errorf("text:\n%s", text)
	}
}

func TestWhereLimitJSONShape(t *testing.T) {
	var w whLines
	w.user("2026-11-24T10:00:00Z", "go")
	w.limitErr("2026-11-24T10:00:02Z", "e1", "You've hit your weekly limit", weeklyOff(resetAt("2026-11-28T17:00:00Z")))
	w.call("2026-11-24T10:00:03Z", "m1", "r1", "claude-sonnet-5-5", 5)
	dir := t.TempDir()
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	_, js := whRun(t, dir, whWin)
	var raw struct {
		Limits map[string]json.RawMessage `json:"limit_events"`
	}
	if err := json.Unmarshal([]byte(js), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw.Limits["spend_after_first"]; ok {
		t.Error("the old spend_after_first field is gone")
	}
	for _, k := range []string{"incidents", "other_api_errors"} {
		if _, ok := raw.Limits[k]; !ok {
			t.Errorf("limit_events lacks %q: %v", k, raw.Limits)
		}
	}
	var inc []map[string]any
	if err := json.Unmarshal(raw.Limits["incidents"], &inc); err != nil || len(inc) != 1 {
		t.Fatalf("incidents %s", raw.Limits["incidents"])
	}
	for _, k := range []string{"time", "session", "limit_type", "stated_reset", "records", "extra_usage", "is_using_overage", "lockout_seconds", "lockout", "resumed_before_stated_reset"} {
		if _, ok := inc[0][k]; !ok {
			t.Errorf("incident lacks %q: %v", k, inc[0])
		}
	}
	if _, ok := inc[0]["spend_after_may_be_extra_usage"]; ok {
		t.Error("no spend figure when extra usage is off")
	}
}

func TestWhereSubagentTypeFromMeta(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T10:00:00Z", "go")
	w.call("2026-11-24T10:00:01Z", "m1", "r1", "claude-sonnet-5-5", 5)
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	var a whLines
	a.user("2026-11-24T10:00:02Z", "search the repo")
	a.call("2026-11-24T10:00:03Z", "a1", "r1", "claude-haiku-5-5", 5)
	a.call("2026-11-24T10:00:04Z", "a2", "r2", "claude-haiku-5-5", 5)
	whWrite(t, filepath.Join(dir, "projects", "p", "s1", "subagents", "agent-abc.jsonl"), a.b.String())
	whWrite(t, filepath.Join(dir, "projects", "p", "s1", "subagents", "agent-abc.meta.json"), `{"agentType":"probe"}`)
	r, _ := whRun(t, dir, whWin)
	got := map[string]int{}
	for _, ag := range r.Agents {
		got[ag.Agent] = ag.Calls
	}
	if got["main"] != 1 || got["probe"] != 2 || r.Sessions != 1 {
		t.Fatalf("agents %v sessions %d", got, r.Sessions)
	}
	trig := map[string]int{}
	for _, tr := range r.Triggers {
		trig[tr.Trigger] = tr.Calls
	}
	if trig["subagent task prompt"] != 1 {
		t.Fatalf("triggers %v", trig)
	}
}

func TestWhereWindowEdges(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T09:00:00Z", "go")
	w.call("2026-11-24T09:59:59Z", "m0", "r0", "claude-sonnet-5-5", 5)
	w.call("2026-11-24T10:00:00Z", "m1", "r1", "claude-sonnet-5-5", 5) // from is inclusive
	w.call("2026-11-24T10:59:59Z", "m2", "r2", "claude-sonnet-5-5", 5)
	w.call("2026-11-24T11:00:00Z", "m3", "r3", "claude-sonnet-5-5", 5) // until is exclusive
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	r, _ := whRun(t, dir, "--since", "2026-11-24T10:00:00Z", "--until", "2026-11-24T11:00:00Z")
	if r.Calls != 2 {
		t.Fatalf("calls %d, want 2", r.Calls)
	}
	// a file older than --since is not read
	old := filepath.Join(dir, "projects", "p", "s1.jsonl")
	if err := os.Chtimes(old, fixedTime(2020), fixedTime(2020)); err != nil {
		t.Fatal(err)
	}
	r, _ = whRun(t, dir, "--since", "2026-11-24T10:00:00Z", "--until", "2026-11-24T11:00:00Z")
	if r.Calls != 0 {
		t.Fatalf("an old file was read: %d calls", r.Calls)
	}
	var out bytes.Buffer
	c := &cli{out: &out, dir: t.TempDir()}
	if err := cmdUsageWhere(c, []string{"--dir", dir, "--since", "2026-11-24T11:00:00Z", "--until", "2026-11-24T10:00:00Z"}); err == nil {
		t.Fatal("a window that ends before it starts was accepted")
	}
}

func TestWhereJSONShapeAndCSV(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T10:00:00Z", "hello\nthere")
	w.call("2026-11-24T10:00:01Z", "m1", "r1", "claude-fable-5", 5, map[string]any{"type": "tool_use", "id": "r1", "name": "Read", "input": map[string]any{"file_path": "/work/proj/a.go"}},
		map[string]any{"type": "tool_use", "id": "e1", "name": "Edit", "input": map[string]any{"file_path": "/work/proj/a.go"}})
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	csvPath := filepath.Join(t.TempDir(), "calls.csv")
	r, raw := whRun(t, dir, whWin, "--csv", csvPath)
	var generic map[string]any
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"models", "agents", "projects", "launch_kinds", "premium_main_activity", "triggers", "phase", "top_sessions", "limit_events", "not_counted_or_not_known", "cost_parts"} {
		if _, ok := generic[k]; !ok {
			t.Errorf("json lacks %q", k)
		}
	}
	if len(r.Top) != 1 || r.Top[0].Prompt != "hello there" || r.Models[0].MedianCtx != 1210 {
		t.Fatalf("top %+v models %+v", r.Top, r.Models)
	}
	f, err := os.Open(csvPath) // #nosec G304 -- a test file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("csv %v rows %v", err, rows)
	}
	if strings.Join(rows[0], ",") != "session,file_kind,ts,model,trigger,tool_classes,context_tokens,cost_input,cost_output,cost_cache_write_5m,cost_cache_write_1h,cost_cache_read,cost_total" {
		t.Fatalf("header %v", rows[0])
	}
	row := rows[1]
	if row[0] != "s1" || row[1] != "main" || row[2] != "2026-11-24T10:00:01Z" || row[3] != "claude-fable-5" || row[4] != "typed prompt" || row[5] != "edit tool+read file" || row[6] != "1210" {
		t.Fatalf("row %v", row)
	}
	var sum float64
	for _, s := range row[7:12] {
		var x float64
		_, _ = fmt.Sscan(s, &x)
		sum += x
	}
	var total float64
	_, _ = fmt.Sscan(row[12], &total)
	if d := sum - total; d < -1e-5 || d > 1e-5 {
		t.Fatalf("parts %v do not add up to %v", sum, total)
	}
}

func fixedTime(year int) time.Time { return time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC) }

func TestWhereCostliestCommands(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T10:00:00Z", "look")
	w.call("2026-11-24T10:00:01Z", "m1", "r1", "claude-fable-5", 5, bash("b1", "git log -3 && sed -n 1,5p f.go"))
	w.call("2026-11-24T10:00:02Z", "m2", "r2", "claude-fable-5", 5, map[string]any{"type": "tool_use", "id": "t1", "name": "ToolSearch", "input": map[string]any{"query": "x"}})
	w.call("2026-11-24T10:00:03Z", "m3", "r3", "claude-sonnet-5-5", 5, bash("b3", "grep -r x .")) // not premium
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	r, _ := whRun(t, dir, whWin)
	got := map[string]whCommandRow{}
	for _, c := range r.Commands.Commands {
		got[c.Head] = c
	}
	if got["git log"].Class != whGit || got["sed"].Class != whShellRead || got["git log"].Calls != 1 || got["ToolSearch"].Class != whOther {
		t.Fatalf("commands %+v", r.Commands.Commands)
	}
	if _, ok := got["grep"]; ok {
		t.Fatalf("a sonnet call is listed: %+v", r.Commands.Commands)
	}
	if d := got["git log"].Cost - got["sed"].Cost; d < -1e-9 || d > 1e-9 {
		t.Fatalf("a compound call is split equally: %+v", got)
	}
	if len(r.Commands.OtherTools) != 1 || r.Commands.OtherTools[0].Head != "ToolSearch" {
		t.Fatalf("other tools %+v", r.Commands.OtherTools)
	}
	if !strings.Contains(whereText(r), "Costliest commands") {
		t.Fatal("text lacks the section")
	}
}

// A ship whose result is an error (failed check, conflict, hook refusal) is
// not a ship, and neither is one with no result yet; spend after it is still
// before the first ship (audit 1907.17).
func TestWherePhaseFailedShipIsNotAShip(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T10:00:00Z", "go")
	w.call("2026-11-24T10:00:01Z", "m1", "r1", "claude-fable-5", 5, bash("b1", "wp ship"))
	w.resultErr("2026-11-24T10:00:02Z", "b1")
	w.call("2026-11-24T10:00:03Z", "m2", "r2", "claude-fable-5", 5)
	w.call("2026-11-24T10:00:04Z", "m3", "r3", "claude-fable-5", 5, bash("b3", "wp ship"))
	w.result("2026-11-24T10:00:05Z", "b3")
	w.call("2026-11-24T10:00:06Z", "m4", "r4", "claude-fable-5", 5)
	w.call("2026-11-24T10:00:07Z", "m5", "r5", "claude-fable-5", 5, bash("b5", "wp ship"))
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	r, _ := whRun(t, dir, whWin)
	ph := r.Phase
	if ph.Shipped != 1 || ph.Ships != 1 {
		t.Fatalf("phase %+v: one ship (the second); the failed and the unanswered are not", ph)
	}
	one := r.Parts.total() / 5
	if d := ph.Before - 3*one; d < -1e-9 || d > 1e-9 {
		t.Errorf("before %v, want 3 calls of %v (the failed ship and the call after it count as before)", ph.Before, one)
	}
}

func TestWherePhaseFirstShipBeforeWindow(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T08:00:00Z", "go")
	w.call("2026-11-24T09:00:00Z", "m1", "r1", "claude-fable-5", 5, bash("b1", "wp ship"))
	w.result("2026-11-24T09:00:01Z", "b1")
	w.call("2026-11-24T10:30:00Z", "m2", "r2", "claude-fable-5", 5)
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	var w2 whLines
	w2.user("2026-11-24T10:10:00Z", "go")
	w2.call("2026-11-24T10:20:00Z", "n1", "q1", "claude-fable-5", 5, bash("b1", "wp ship"))
	w2.result("2026-11-24T10:20:01Z", "b1")
	w2.call("2026-11-24T10:40:00Z", "n2", "q2", "claude-fable-5", 5)
	whWrite(t, filepath.Join(dir, "projects", "p", "s2.jsonl"), w2.b.String())
	r, _ := whRun(t, dir, "--since", "2026-11-24T10:00:00Z", "--until", "2026-11-24T12:00:00Z")
	if r.Phase.Shipped != 2 || r.Phase.BeforeWindow != 1 {
		t.Fatalf("phase %+v", r.Phase)
	}
	for _, s := range r.Phase.Sessions {
		if s.FirstShipBeforeWindow != (s.Before == 0) {
			t.Fatalf("row %+v", s)
		}
	}
	txt := whereText(r)
	if !strings.Contains(txt, "1 of them first shipped before the window") || strings.Count(txt, "(first ship before the window)") != 1 {
		t.Fatalf("text:\n%s", txt)
	}
}

func TestWhereMetaMessageKinds(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T10:00:00Z", "go")
	meta := func(ts, text string) {
		w.raw(map[string]any{"type": "user", "timestamp": ts, "isMeta": true, "message": map[string]any{"role": "user", "content": text}})
	}
	meta("2026-11-24T10:00:01Z", "<system-reminder>\nPlan mode is active. You must not edit.\n</system-reminder>")
	w.call("2026-11-24T10:00:02Z", "m1", "r1", "claude-sonnet-5-5", 5)
	meta("2026-11-24T10:00:03Z", "<local-command-caveat>Caveat: x</local-command-caveat>")
	w.call("2026-11-24T10:00:04Z", "m2", "r2", "claude-sonnet-5-5", 5)
	meta("2026-11-24T10:00:05Z", "<system-reminder>\nPlan mode is active. You must not edit.\n</system-reminder>")
	w.call("2026-11-24T10:00:06Z", "m3", "r3", "claude-sonnet-5-5", 5)
	whWrite(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), w.b.String())
	r, _ := whRun(t, dir, whWin)
	for _, tr := range r.Triggers {
		if tr.Trigger != "meta/hook message" {
			continue
		}
		if len(tr.Detail) != 2 || tr.Detail[0].Trigger != "<system-reminder> Plan mode is active. You" || tr.Detail[0].Calls != 2 || tr.Detail[1].Trigger != "<local-command-caveat>" {
			t.Fatalf("detail %+v", tr.Detail)
		}
		if !strings.Contains(whereText(r), "<local-command-caveat>") {
			t.Fatal("text lacks the breakdown")
		}
		return
	}
	t.Fatalf("no meta trigger: %+v", r.Triggers)
}

// A limit record as Claude Code wrote it (2.1.x, November 2026; ids changed):
// rate_limit_info sits under apiErrorParams and resetsAt is Unix seconds, the
// 5pm UTC the record's own text names (audit 1931.30).
func TestLimitRecordAsRecorded(t *testing.T) {
	const line = `{"type":"assistant","timestamp":"2026-11-13T09:12:44.120Z","isApiErrorMessage":true,"message":{"id":"m1","model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"You've hit your weekly limit · resets 5pm (UTC)"}],"context_management":null},"requestId":"req_x","apiError":"usage_limit_reached","apiErrorParams":{"rate_limit_info":{"status":"rejected","resetsAt":1794675600,"rateLimitType":"seven_day","overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false}}}`
	var l rawLine
	if err := json.Unmarshal([]byte(line), &l); err != nil {
		t.Fatal(err)
	}
	if l.APIError != "usage_limit_reached" || l.APIErrorParams == nil || l.APIErrorParams.RateLimitInfo == nil {
		t.Fatalf("record not read: %+v", l)
	}
	ri := l.APIErrorParams.RateLimitInfo
	at := time.Unix(ri.ResetsAt, 0).UTC()
	if ri.RateLimitType != "seven_day" || ri.OverageDisabledReason != "org_level_disabled" || at.Hour() != 17 || at.Year() != 2026 {
		t.Errorf("rate limit %+v, resets %v; want seven_day resetting at 17:00 UTC in 2026", ri, at)
	}
}

// A command is classed by what it does, whichever tool or wrapper runs it.
func TestWhereCommandClasses(t *testing.T) {
	tool := func(name string, input map[string]any) map[string]any {
		return map[string]any{"type": "tool_use", "id": "t", "name": name, "input": input}
	}
	sc := &whScan{}
	cwd := "/work/proj"
	for _, c := range []struct {
		block map[string]any
		want  string
		ships int
	}{
		{bash("b", "make test"), whTests, 0},
		{bash("b", "npm run typecheck"), whTests, 0},
		{bash("b", "cargo test"), whTests, 0},
		{bash("b", "npx vitest run"), whTests, 0},
		{bash("b", "go test ./... 2>&1 | tail -20"), whTests, 0},
		{bash("b", "go run ./cmd/server"), whOther, 0},
		{bash("b", "go mod tidy"), whOther, 0},
		{bash("b", "npm install"), whOther, 0},
		{bash("b", "wp slot run tests -- go test ./..."), whTests, 0},
		{bash("b", "wp slot run tests -- sh -c 'make check'"), whTests, 0},
		{bash("b", "sleep 600"), whWait, 0},
		{bash("b", "wp pr wait 12"), whWait, 0},
		{bash("b", "echo x | grep x"), whOther, 0},
		{bash("b", "cat f.go | grep x"), whShellRead, 0},
		{bash("b", "curl -s -o /dev/null https://example.com"), whOther, 0},
		{bash("b", "curl -sLo docs/spec.md https://example.com"), whShellEdit, 0},
		{bash("b", "curl -o build.log https://example.com"), whOther, 0},
		{bash("b", "curl -o - https://example.com"), whOther, 0},
		{bash("b", "npx prettier@3.3.3 --write src/x.ts"), whShellEdit, 0},
		{bash("b", "git add -A && wp done -m x"), "git+wp", 1},
		{bash("b", "wp ship -m a; wp finish -m b"), whWP, 2},
		{tool("TaskOutput", map[string]any{}), whWait, 0},
		{tool("SendMessage", map[string]any{}), whDelegate, 0},
		{tool("Write", map[string]any{"file_path": "/tmp/patch.py", "content": "open('f.go','w').write('x')\n"}), whScratch, 0},
		{bash("b", "python3 /tmp/patch.py"), whShellEdit, 0},
	} {
		in, _ := json.Marshal(c.block["input"])
		got, ships, _ := sc.whBlockClasses(c.block["name"].(string), in, cwd)
		if strings.Join(got, "+") != c.want || ships != c.ships {
			t.Errorf("%s: %v, %d ships; want %s, %d", in, got, ships, c.want, c.ships)
		}
	}
}

// A main transcript's sidechain records are a subagent's calls, a fork's
// copies are the main thread's, and only the main thread's ships split its
// spend.
func TestWhereMainAndSubagentRecords(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	w.user("2026-11-24T10:00:00Z", "go")
	w.call("2026-11-24T10:00:02Z", "m1", "r1", "claude-fable-5", 5)
	w.raw(map[string]any{"type": "assistant", "timestamp": "2026-11-24T10:00:03Z", "isSidechain": true,
		"message": map[string]any{"model": "claude-fable-5", "usage": map[string]any{"input_tokens": 999999}}})
	main := filepath.Join(dir, "projects", "p", "s1.jsonl")
	whWrite(t, main, w.b.String())
	var f whLines
	f.call("2026-11-24T10:00:02Z", "m1", "r1", "claude-fable-5", 5) // the parent's call, copied
	f.call("2026-11-24T10:00:01Z", "a1", "q1", "claude-fable-5", 5, bash("b1", "wp ship"))
	fork := filepath.Join(dir, "projects", "p", "s1", "subagents", "agent-f.jsonl")
	whWrite(t, fork, f.b.String())
	older := time.Now().Add(-time.Minute)
	if err := os.Chtimes(fork, older, older); err != nil {
		t.Fatal(err)
	}
	r, _ := whRun(t, dir, whWin)
	got := map[string]int{}
	for _, a := range r.Agents {
		got[a.Agent] = a.Calls
	}
	if r.Calls != 2 || got["main"] != 1 {
		t.Fatalf("calls %d agents %v", r.Calls, got)
	}
	if r.Phase.Shipped != 0 || r.Phase.After != 0 || r.Top[0].Ships != 0 {
		t.Fatalf("a subagent's ship split the main thread: %+v", r.Phase)
	}
}

// Only a spent plan limit is a limit event.
func TestWhereOtherErrorsAreNotLimits(t *testing.T) {
	dir := t.TempDir()
	var w whLines
	for i, text := range []string{"Context limit reached · /compact or /clear to continue", "API Error: 429 rate_limit_error"} {
		w.raw(map[string]any{"type": "assistant", "timestamp": fmt.Sprintf("2026-11-24T10:00:0%dZ", i), "isApiErrorMessage": true, "message": map[string]any{"model": "<synthetic>",
			"content": []any{map[string]any{"type": "text", "text": text}}}})
	}
	w.call("2026-11-24T10:00:05Z", "m1", "r1", "acme-large", 5)
	whWrite(t, filepath.Join(dir, "projects", "x-waypoint-runner-work-job1-2", "s1.jsonl"), w.b.String())
	whWrite(t, filepath.Join(dir, "projects", "x-waypoint-runner-work-job1-2", jobSidecarFile), `{"kind":"audit","sessions":["s1"]}`)
	r, _ := whRun(t, dir, whWin)
	if len(r.Limits.Incidents) != 0 {
		t.Fatalf("limits %+v", r.Limits)
	}
	if len(r.NotKnown.AssumedPrices) != 1 || !strings.Contains(whereText(r), "no price is known for acme-large") {
		t.Fatalf("assumed prices %v", r.NotKnown.AssumedPrices)
	}
	if len(r.Launches) != 1 || r.Launches[0].Name != "runner job: audit" {
		t.Fatalf("launches %+v", r.Launches)
	}
}
