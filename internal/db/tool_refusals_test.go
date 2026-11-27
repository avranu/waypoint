package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// 0162 starts the refused-calls log (WP-232) with the refused calls the
// hooks filed as permission requests: the command from the input (the
// body's fence only when the payload dropped it, as a command can hold a
// fence of its own),
// the tool, input and reason from the payload, ran_at when the person allowed
// it for the session. A permission request an agent asked for itself (no
// tool_use_id) and other kinds stay out; the requests themselves stay.
func TestToolRefusalsFromPermissionRequests(t *testing.T) {
	ctx := context.Background()
	d, err := OpenFresh(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`INSERT INTO users(id, username, email, created_at, updated_at) VALUES (1, 'robin', 'j@example.com', 0, 0)`,
		`INSERT INTO projects(id, key, name, created_at, updated_at) VALUES (1, 'WP', 'Waypoint', 0, 0)`,
		`INSERT INTO requests(id, number, user_id, project_id, agent, kind, title, body, state, payload, decision, decided_at, tool_use_id, dedupe_key, created_at, updated_at) VALUES
			(1, 1, 1, 1, 'anthropic/claude-opus-5-5', 'permission', 'Allow Bash: rm -rf build?', '` + "```" + `
rm -rf build
` + "```" + `

it deletes files
', 'expired', '{"tool":{"name":"Bash","input":{"command":"rm -rf build"},"reason":"it deletes files"}}', 'expire', 50, 'tu-1', 'k1', 10, 50),
			(2, 2, 1, 1, 'anthropic/claude-opus-5-5', 'permission', 'Allow Bash: make deploy?', '` + "```" + `
make deploy &&
  echo done
` + "```" + `
', 'approved', '{"tool":{"name":"Bash"}}', 'session', 70, 'tu-2', 'k2', 20, 70),
			(5, 5, 1, 1, 'anthropic/claude-opus-5-5', 'permission', 'Allow Bash: gh pr create?', '` + "```" + `
gh pr create --body "## Plan
` + "```" + `go
foo
` + "```" + `
end"
` + "```" + `
', 'expired', '{"tool":{"name":"Bash","input":{"command":"gh pr create --body \"## Plan\n` + "```" + `go\nfoo\n` + "```" + `\nend\"\n"}}}', 'expire', 90, 'tu-5', 'k5', 80, 90),
			(6, 6, 1, 1, 'anthropic/claude-opus-5-5', 'permission', 'Allow Write: a.go?', '` + "```" + `
/w/a.go
` + "```" + `
', 'expired', '{"tool":{"name":"Write","input":{"file_path":" /w/a.go ","content":"x"}}}', 'expire', 95, 'tu-6', 'k6', 85, 95),
			(3, 3, 1, 1, 'anthropic/claude-opus-5-5', 'permission', 'Allow Bash?', '', 'open', '{"tool":{"name":"Bash","input":{}}}', '', NULL, '', 'k3', 30, 30),
			(4, 4, 1, 1, 'anthropic/claude-opus-5-5', 'question', 'Which?', '', 'open', '{}', '', NULL, 'tu-4', '', 40, 40)`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	body, err := migrations.ReadFile("migrations/0162_tool_refusals.sql")
	if err != nil {
		t.Fatal(err)
	}
	copyRows := string(body)[strings.Index(string(body), "INSERT INTO tool_refusals"):]
	if _, err := d.W.ExecContext(ctx, copyRows); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		`SELECT group_concat(request_id, ',') FROM tool_refusals`:                                          "1,2,5,6",
		`SELECT command FROM tool_refusals WHERE request_id = 1`:                                           "rm -rf build",
		`SELECT command FROM tool_refusals WHERE request_id = 2`:                                           "make deploy &&\n  echo done",
		`SELECT command FROM tool_refusals WHERE request_id = 5`:                                           "gh pr create --body \"## Plan\n```go\nfoo\n```\nend\"",
		`SELECT command FROM tool_refusals WHERE request_id = 6`:                                           "/w/a.go",
		`SELECT tool || '|' || reason || '|' || input FROM tool_refusals WHERE request_id = 1`:             `Bash|it deletes files|{"command":"rm -rf build"}`,
		`SELECT coalesce(ran_at, 'none') || '|' || created_at FROM tool_refusals ORDER BY id LIMIT 1`:      "none|10",
		`SELECT ran_at || '|' || dedupe_key || '|' || tool_use_id FROM tool_refusals WHERE request_id = 2`: "70|k2|tu-2",
		`SELECT count(*) FROM requests`:                                                                    "6",
	} {
		var got string
		if err := d.W.QueryRowContext(ctx, q).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %q (%v), want %q", q, got, err, want)
		}
	}
}
