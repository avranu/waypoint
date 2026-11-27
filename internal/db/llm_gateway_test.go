package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// 0164 carries a site that ran OpenLLMs over with nothing for a person to
// do (WP-1187): its provider is the LLM gateway's, with every extension
// declared, and the once-only upgrade marks are gone.
func TestLLMGatewayMigrationSettings(t *testing.T) {
	ctx := context.Background()
	d, err := OpenFresh(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`INSERT INTO server_settings(key, value) VALUES ('openllms_provider', '7'), ('openllms_models', '1'), ('openllms_only', '1'), ('openllms_prep_purpose', '1')`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	body, err := migrations.ReadFile("migrations/0164_llm_gateway.sql")
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if _, err := d.W.ExecContext(ctx, s[strings.Index(s, "UPDATE server_settings"):strings.Index(s, "UPDATE code_audits")]); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	rows, err := d.W.QueryContext(ctx, `SELECT key, value FROM server_settings WHERE key LIKE 'openllms%' OR key LIKE 'llm_gateway%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		got[k] = v
	}
	want := map[string]string{"llm_gateway_provider": "7", "llm_gateway_extensions": "routes,fallbacks,routing,exclude,key,tasks,feedback,decide"}
	if len(got) != len(want) || got["llm_gateway_provider"] != want["llm_gateway_provider"] || got["llm_gateway_extensions"] != want["llm_gateway_extensions"] {
		t.Errorf("settings = %v, want %v", got, want)
	}
}

// 0164 renames the gateway's records (WP-1187): model names that start with
// openllms/ and the openllms CLI, never the word elsewhere in a rule (a
// repository's URL, a prompt).
func TestLLMGatewayMigration(t *testing.T) {
	ctx := context.Background()
	d, err := OpenFresh(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rule := `{"actions":[{"type":"audit","models":[{"cli":"openllms","model":"grok"},"openllms/glm"],"prompt":"see https://git.example/openllms/repo and openllms/x"}]}`
	if _, err := d.W.ExecContext(ctx, `INSERT INTO automations(id, project_id, name, rule, created_at, updated_at) VALUES (1, NULL, 'a', ?, 0, 0)`, rule); err != nil {
		t.Fatal(err)
	}
	body, err := migrations.ReadFile("migrations/0164_llm_gateway.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, string(body)[strings.Index(string(body), "UPDATE automations"):]); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := d.W.QueryRowContext(ctx, `SELECT rule FROM automations WHERE id = 1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := `{"actions":[{"type":"audit","models":[{"cli":"gateway","model":"grok"},"gateway/glm"],"prompt":"see https://git.example/openllms/repo and openllms/x"}]}`
	if got != want {
		t.Errorf("rule = %s\nwant   %s", got, want)
	}
}
