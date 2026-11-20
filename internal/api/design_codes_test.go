package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Decision notes: ADR codes in every place a note is named, and the
// provenance lines agents read (WP-113).
func TestDecisionNotesAPI(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct-horse-battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	me := &core.Actor{User: u}
	tok, _, _ := svc.CreateAPIToken(ctx, me, "robin", false, 0)
	human := &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	agentTok, _, _ := svc.CreateToken(ctx, me, core.TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	agent := &client{t: t, base: ts.URL, http: http.DefaultClient, token: agentTok}
	if code, b := human.do("POST", "/api/v1/projects", map[string]any{"name": "Widgets", "key": "WID"}); code != 200 {
		t.Fatalf("project: %d %s", code, b)
	}

	// An agent proposes a decision: unapproved, with a code.
	var note map[string]any
	if code := agent.json("POST", "/api/v1/design", map[string]any{"title": "ADR-0007: Use SQLite", "kind": "decision", "project": "WID",
		"body": "One file to back up."}, &note); code != 200 || note["code"] != "ADR7" || note["title"] != "Use SQLite" || note["approved"] != false {
		t.Fatalf("agent decision: %d %v", code, note)
	}
	_, b := agent.do("GET", "/api/v1/design/ADR7?project=WID&format=text", nil)
	if s := string(b); !strings.Contains(s, "WID-ADR7 design #") || !strings.Contains(s, "NOT APPROVED by a person") || !strings.Contains(s, "Written by agent anthropic/claude-opus-5-5") {
		t.Errorf("proposed decision text:\n%s", s)
	}

	// A person approves it; the approval is stated.
	if code, b := human.do("POST", "/api/v1/design/adr-0007/approve?project=WID", nil); code != 200 {
		t.Fatalf("approve by code: %d %s", code, b)
	}
	// The agent edits the approved decision and adds to it.
	if code, b := agent.do("PATCH", "/api/v1/design/ADR7?project=WID", map[string]any{"body": "Use Postgres instead."}); code != 200 || !strings.Contains(string(b), "proposed edit") {
		t.Fatalf("agent edit: %d %s", code, b)
	}
	if code, _ := agent.do("POST", "/api/v1/design/WID-ADR7/additions", map[string]any{"body": "Backups: litestream."}); code != 200 {
		t.Fatalf("addition: %d", code)
	}

	for _, path := range []string{"/api/v1/design/ADR7?project=WID&format=text", "/api/v1/design/wid-adr-0007?format=text", "/api/v1/spec?topic=ADR7&project=WID&format=text",
		"/api/v1/spec?topic=WID-ADR-0007&format=text"} {
		code, b := agent.do("GET", path, nil)
		s := string(b)
		if code != 200 {
			t.Errorf("%s: %d %s", path, code, s)
			continue
		}
		if testing.Verbose() && strings.HasPrefix(path, "/api/v1/design/ADR7") {
			t.Log("\n" + s)
		}
		approvedAt := strings.Index(s, "One file to back up.")
		for _, want := range []string{
			"WID-ADR7", "Written by agent anthropic/claude-opus-5-5", "Approved by robin on ",
			"UNAPPROVED agent additions", "NOT APPROVED] Backups: litestream.",
			"UNAPPROVED agent edits (proposals, not applied: the text above is what a person approved)", "Use Postgres instead.",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("%s lacks %q:\n%s", path, want, s)
			}
		}
		// The proposed text comes after, and apart from, the approved text.
		if prop := strings.Index(s, "Use Postgres instead."); approvedAt < 0 || prop < approvedAt || strings.Index(s, "UNAPPROVED agent edits") > prop {
			t.Errorf("%s: proposal not kept apart from the approved text:\n%s", path, s)
		}
	}
	// A person's note says who wrote it, too.
	var plain map[string]any
	human.json("POST", "/api/v1/design", map[string]any{"title": "Board", "kind": "feature", "project": "WID", "body": "Columns."}, &plain)
	_, b = agent.do("GET", "/api/v1/design/"+itoa(int(plain["id"].(float64)))+"?format=text", nil)
	if s := string(b); !strings.Contains(s, "Written by robin on ") || !strings.Contains(s, "Approved by robin on ") || !strings.Contains(s, "History (newest first)") {
		t.Errorf("person's note text:\n%s", s)
	}

	// Listing, search and links by code.
	_, b = agent.do("GET", "/api/v1/design?project=WID&format=text&q=adr7", nil)
	if !strings.Contains(string(b), "WID-ADR7 #") {
		t.Errorf("list by code:\n%s", b)
	}
	_, b = agent.do("GET", "/api/v1/search/all?q=ADR-7&project=WID&kinds=design&format=text", nil)
	if !strings.Contains(string(b), "Use SQLite") {
		t.Errorf("search by code:\n%s", b)
	}
	var it map[string]any
	if code := human.json("GET", "/api/v1/resolve?ref=ADR-0007&project=WID", nil, &it); code != 200 || it["type"] != "design" || it["key"] != "ADR7" {
		t.Errorf("resolve: %d %v", code, it)
	}
	if code, b := human.do("POST", "/api/v1/links", map[string]any{"from": map[string]any{"ref": "design:WID-ADR7"}, "to": map[string]any{"ref": "web/src/db.go"}, "rel": "implements"}); code != 200 {
		t.Errorf("link by code: %d %s", code, b)
	}
	if code, _ := agent.do("GET", "/api/v1/design/ADR99?project=WID", nil); code != 404 {
		t.Errorf("unknown code: %d", code)
	}

	// MCP takes codes where it takes a note id.
	if text := callMCPTool(t, ts.URL, tok, "get_spec", map[string]any{"topic": "ADR7", "project": "WID"}); !strings.Contains(text, "WID-ADR7") || !strings.Contains(text, "NOT APPROVED") {
		t.Errorf("mcp get_spec:\n%s", text)
	}
	if text := callMCPTool(t, ts.URL, tok, "add_to_design", map[string]any{"id": "ADR7", "project": "WID", "body": "via mcp"}); !strings.Contains(text, "Added to design WID-ADR7") {
		t.Errorf("mcp add_to_design: %s", text)
	}
	if text := callMCPTool(t, ts.URL, tok, "save_design", map[string]any{"id": "adr-0007", "project": "WID", "body": "edited via mcp"}); !strings.Contains(text, "Saved design WID-ADR7") {
		t.Errorf("mcp save_design: %s", text)
	}

	// A decision page still works, with a notice (API and MCP).
	var pg map[string]any
	if code := human.json("POST", "/api/v1/pages", map[string]any{"title": "Use Go", "category": "decision", "project": "WID"}, &pg); code != 200 ||
		!strings.Contains(pg["notice"].(string), "wp design new decision") {
		t.Errorf("decision page: %d %v", code, pg)
	}
	var plan map[string]any
	if code := human.json("POST", "/api/v1/pages", map[string]any{"title": "Roadmap", "category": "plan", "project": "WID"}, &plan); code != 200 || plan["notice"] != nil {
		t.Errorf("plan page: %d %v", code, plan)
	}
	if text := callMCPTool(t, ts.URL, tok, "save_page", map[string]any{"title": "Use Rust", "category": "decision", "project": "WID"}); !strings.Contains(text, "save_design") {
		t.Errorf("mcp decision page: %s", text)
	}
}
