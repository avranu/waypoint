package core

import (
	"strings"
	"testing"
)

func (e *uaEnv) otlp(a *Actor, evs ...OTLPEvent) *OTLPResult {
	e.t.Helper()
	r, err := e.s.IngestOTLP(e.ctx, a, evs)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func apiReq(at int64, session string, in, out string) OTLPEvent {
	return OTLPEvent{At: at, Attrs: map[string]string{"event.name": "api_request", "session.id": session, "model": "claude-opus-5-5",
		"input_tokens": in, "output_tokens": out, "cache_read_tokens": "500", "cache_creation_tokens": "20", "event.sequence": in,
		"workspace.host_paths": "/home/pat/src/waypoint"}}
}

func TestOTLPMapsEvents(t *testing.T) {
	e := newAnalyticsEnv(t)
	at := e.hour(-1) + 5000
	tool := func(name, extra string, kv ...string) OTLPEvent {
		a := map[string]string{"event.name": name, "session.id": "s1", "tool_name": "Bash", "event.sequence": extra}
		for i := 0; i+1 < len(kv); i += 2 {
			a[kv[i]] = kv[i+1]
		}
		return OTLPEvent{At: at, Attrs: a}
	}
	r := e.otlp(e.me,
		apiReq(at, "s1", "100", "40"),
		tool("tool_result", "1", "success", "true", "tool_result_size_bytes", "2048", "decision_source", "user_temporary"),
		tool("tool_result", "2", "success", "false", "tool_name", "mcp__github__create_issue"),
		tool("tool_decision", "3", "decision", "reject", "source", "hook", "tool_parameters", `{"bash_command":"rm -rf /tmp/x","full_command":"rm -rf /tmp/x && echo hi"}`),
		tool("tool_decision", "4", "decision", "accept", "source", "config"),
		OTLPEvent{At: at, Attrs: map[string]string{"event.name": "user_prompt", "session.id": "s1", "prompt": "TOPSECRET"}},
		OTLPEvent{At: at, Attrs: map[string]string{"event.name": "api_request", "model": "m"}},                    // no session
		OTLPEvent{At: 5, Attrs: map[string]string{"event.name": "api_request", "session.id": "s1", "model": "m"}}, // implausible time
	)
	if r.Rejected != 2 || r.Ignored != 2 {
		t.Errorf("result %+v", r)
	}
	o := e.overview(e.me, UsageQuery{})
	if tt := o.Totals; tt.Messages != 1 || tt.Input != 100 || tt.Output != 40 || tt.CacheRead != 500 || tt.CacheWrite != 20 {
		t.Errorf("totals %+v", tt)
	}
	var repo string
	var calls, errs, prompts, refusals, bytes int64
	q := func(where string) {
		t.Helper()
		if err := e.s.db.R.QueryRow(`SELECT coalesce(sum(calls),0), coalesce(sum(errors),0), coalesce(sum(prompts),0), coalesce(sum(refusals),0), coalesce(sum(result_bytes),0) FROM tool_hours WHERE via = 'otlp' AND `+where).
			Scan(&calls, &errs, &prompts, &refusals, &bytes); err != nil {
			t.Fatal(err)
		}
	}
	q(`kind = 'builtin' AND tool = 'Bash'`)
	if calls != 1 || prompts != 1 || bytes != 2048 || refusals != 1 || errs != 0 {
		t.Errorf("Bash: calls %d prompts %d bytes %d refusals %d errs %d", calls, prompts, bytes, refusals, errs)
	}
	q(`kind = 'mcp' AND tool = 'mcp__github__create_issue' AND detail = 'github'`)
	if calls != 1 || errs != 1 {
		t.Errorf("mcp: calls %d errs %d", calls, errs)
	}
	var reason, src, excerpt, detail string
	if err := e.s.db.R.QueryRow(`SELECT reason, source, excerpt, repo FROM permission_events WHERE via = 'otlp'`).Scan(&reason, &src, &excerpt, &repo); err != nil {
		t.Fatal(err)
	}
	_ = detail
	if src != "hook" || reason == "" || excerpt != "rm -rf /tmp/x && echo hi" || repo != "" {
		t.Errorf("refusal: %q %q %q repo %q", reason, src, excerpt, repo)
	}
	var m string
	_ = e.s.db.R.QueryRow(`SELECT machine || '/' || repo FROM usage_hours WHERE via = 'otlp'`).Scan(&m)
	if m != "otel/waypoint" {
		t.Errorf("machine/repo = %q", m)
	}
}

func TestOTLPIsIdempotent(t *testing.T) {
	e := newAnalyticsEnv(t)
	ev := apiReq(e.hour(-1)+1, "s1", "10", "5")
	e.otlp(e.me, ev)
	r := e.otlp(e.me, ev, ev)
	if r.Ignored != 2 {
		t.Errorf("retry result %+v", r)
	}
	// a different event in the same hour still counts
	e.otlp(e.me, apiReq(e.hour(-1)+2, "s1", "10", "5"))
	if tt := e.overview(e.me, UsageQuery{}).Totals; tt.Messages != 2 || tt.Input != 20 {
		t.Errorf("totals %+v", tt)
	}
}

func TestOTLPNeverDoubleCountsATranscriptSession(t *testing.T) {
	e := newAnalyticsEnv(t)
	h := e.hour(-1)
	// OTLP first; the transcript arrives later and replaces it
	e.otlp(e.me, apiReq(h+1, "s1", "10", "5"), apiReq(h+2, "s2", "7", "3"))
	e.ingest(e.me, "c1", "devhost", UsageChunk{Messages: []UsageMessageRow{msg(h+1, "s1", "a/b", "claude-opus-5-5", "main", 1, 10, 5, 20, 0, 500)}})
	if tt := e.overview(e.me, UsageQuery{}).Totals; tt.Messages != 2 || tt.Input != 17 {
		t.Errorf("transcript replaces OTLP for s1 only: %+v", tt)
	}
	// transcript first; later OTLP events for that session are ignored
	e.ingest(e.me, "c2", "devhost", UsageChunk{Messages: []UsageMessageRow{msg(h+1, "s3", "a/b", "claude-opus-5-5", "main", 1, 100, 5, 0, 0, 0)}})
	r := e.otlp(e.me, apiReq(h+9, "s3", "100", "5"), apiReq(h+10, "s3", "50", "5"), apiReq(h+3, "s2", "1", "1"))
	if r.Ignored != 2 {
		t.Errorf("result %+v", r)
	}
	if tt := e.overview(e.me, UsageQuery{}).Totals; tt.Messages != 4 || tt.Input != 118 {
		t.Errorf("totals %+v", tt)
	}
	var n int
	_ = e.s.db.R.QueryRow(`SELECT count(*) FROM usage_hours WHERE via = 'otlp' AND session IN ('s1','s3')`).Scan(&n)
	if n != 0 {
		t.Errorf("%d OTLP rows left for transcript sessions", n)
	}
}

func TestOTLPStoresNoPromptText(t *testing.T) {
	e := newAnalyticsEnv(t)
	at := e.hour(-1) + 1
	secret := "TOPSECRET-do-not-store"
	e.otlp(e.me,
		OTLPEvent{At: at, Attrs: map[string]string{"event.name": "user_prompt", "session.id": "s1", "prompt": secret, "prompt_text": secret}},
		OTLPEvent{At: at, Attrs: map[string]string{"event.name": "api_request", "session.id": "s1", "model": "claude-opus-5-5", "prompt": secret, "response": secret, "user.email": "x@y.z", "input_tokens": "1"}},
		OTLPEvent{At: at, Attrs: map[string]string{"event.name": "tool_result", "session.id": "s1", "tool_name": "Read", "tool_input": secret, "error": secret, "tool_parameters": `{"file_path":"` + secret + `"}`}},
		OTLPEvent{At: at, Attrs: map[string]string{"event.name": "tool_decision", "session.id": "s1", "tool_name": "Write", "decision": "reject", "source": "user_reject", "tool_input": secret}},
	)
	for _, tbl := range []string{"usage_hours", "tool_hours", "permission_events", "usage_sessions", "usage_machines", "otlp_seen"} {
		rows, err := e.s.db.R.Query(`SELECT * FROM ` + tbl)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for _, v := range vals {
				if s, ok := v.(string); ok && (strings.Contains(s, "TOPSECRET") || strings.Contains(s, "x@y.z")) {
					t.Errorf("%s stores %q", tbl, s)
				}
			}
		}
		rows.Close()
	}
}

func TestOTLPNeedsAPersonWhoMayWrite(t *testing.T) {
	e := newAnalyticsEnv(t)
	if _, err := e.s.IngestOTLP(e.ctx, SystemActor(), []OTLPEvent{apiReq(e.hour(-1), "s", "1", "1")}); err == nil {
		t.Error("system actor accepted")
	}
	if _, err := e.s.IngestOTLP(e.ctx, nil, nil); err == nil {
		t.Error("anonymous accepted")
	}
}
