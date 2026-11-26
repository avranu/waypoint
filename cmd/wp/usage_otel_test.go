package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageOtelSettingsEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"theme":"dark","env":{"FOO":"1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := otelEnv("https://wp.example/", "tok", false)
	if env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "https://wp.example/api/v1/otlp" || env["OTEL_EXPORTER_OTLP_HEADERS"] != "Authorization=Bearer tok" || env["OTEL_METRICS_EXPORTER"] != "none" {
		t.Fatalf("env %v", env)
	}
	read := func() map[string]any {
		var m map[string]any
		b, _ := os.ReadFile(path)
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if _, err := editClaudeOtelEnv(path, env, false, false); err != nil {
		t.Fatal(err)
	}
	m := read()
	e := m["env"].(map[string]any)
	if m["theme"] != "dark" || e["FOO"] != "1" || e["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" || e["OTEL_EXPORTER_OTLP_PROTOCOL"] != "http/json" {
		t.Errorf("settings %v", m)
	}
	if baks, _ := filepath.Glob(path + ".bak-*"); len(baks) != 1 {
		t.Errorf("backups %v", baks)
	}
	if msg, _ := editClaudeOtelEnv(path, env, false, false); !strings.HasPrefix(msg, "Already") {
		t.Errorf("again: %q", msg)
	}
	e["OTEL_EXPORTER_OTLP_ENDPOINT"] = "http://collector:4318"
	b, _ := json.Marshal(m)
	_ = os.WriteFile(path, b, 0o600)
	if _, err := editClaudeOtelEnv(path, env, false, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("overwrote a foreign endpoint: %v", err)
	}
	if msg, _ := editClaudeOtelEnv(path, env, true, false); !strings.HasPrefix(msg, "No Waypoint") {
		t.Errorf("uninstall of a foreign endpoint: %q", msg)
	}
	if _, err := editClaudeOtelEnv(path, env, false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := editClaudeOtelEnv(path, env, true, false); err != nil {
		t.Fatal(err)
	}
	if e := read()["env"].(map[string]any); len(e) != 1 || e["FOO"] != "1" {
		t.Errorf("after uninstall: %v", e)
	}
	_ = os.WriteFile(path, []byte("{nope"), 0o600)
	if _, err := editClaudeOtelEnv(path, env, false, false); err == nil {
		t.Error("edited invalid JSON")
	}
}

// Without --install the settings are only shown, with a placeholder for the
// token: what is printed lands in transcripts (WP-1337). --install still
// writes the real one.
func TestUsageOtelShowRedactsToken(t *testing.T) {
	const token = "wpt_0123456789abcdefSECRET"
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	t.Setenv("WP_CLAUDE_SETTINGS", settings)
	t.Setenv("WAYPOINT_TOKEN", "")
	t.Setenv("WAYPOINT_SERVER", "")
	var out, errb strings.Builder
	c := &cli{out: &out, err: &errb, cfg: Config{Server: "https://wp.example", Token: token}}
	if err := cmdUsageOtel(c, nil); err != nil {
		t.Fatal(err)
	}
	if got := out.String() + errb.String(); strings.Contains(got, token) || strings.Contains(got, "SECRET") || strings.Contains(got, "wpt_0123") {
		t.Errorf("token printed:\n%s", got)
	} else if !strings.Contains(got, "Authorization=Bearer "+otelTokenPlaceholder) || !strings.Contains(got, "https://wp.example/api/v1/otlp") || !strings.Contains(got, "--install") {
		t.Errorf("output lacks the placeholder, endpoint or the way to install:\n%s", got)
	}
	out.Reset()
	if err := cmdUsageOtel(c, []string{"--install"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), token) {
		t.Errorf("--install printed the token:\n%s", out.String())
	}
	b, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "Authorization=Bearer "+token) {
		t.Errorf("--install did not write the token: %s", b)
	}
}
