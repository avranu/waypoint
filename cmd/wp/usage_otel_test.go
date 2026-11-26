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
