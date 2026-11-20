package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// wp usage otel: point Claude Code's OpenTelemetry export at Waypoint
// (WP-D20, WP-26).

var usageOtelCmd = &command{
	name: "otel", args: "[--install | --uninstall] [--metrics]",
	summary: "set up Claude Code's OpenTelemetry export to Waypoint",
	about: `Prints the environment variables that make Claude Code send usage events
to Waypoint over OTLP/HTTP with your token: for machines where wp usage sync
can't read transcripts (cloud or remote sessions). --install writes them
into ~/.claude/settings.json (backed up first), --uninstall removes them;
unrelated settings are kept; the OpenTelemetry keys it prints
are replaced, and a different endpoint is kept unless --force. Sessions already
synced from a transcript are not counted twice. Prompts are never stored.

The token lands in settings.json: use one made for this (Settings → API
tokens, then wp token) so it can be revoked on its own.`,
	examples: []string{"wp usage otel --install"},
	run:      cmdUsageOtel,
}

func init() { usageCmd.subs = append(usageCmd.subs, usageOtelCmd) }

// otelEnv is what Claude Code's settings.json env needs.
func otelEnv(server, token string, metrics bool) map[string]string {
	m := "none"
	if metrics {
		m = "otlp"
	}
	return map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY": "1",
		"OTEL_LOGS_EXPORTER":           "otlp",
		"OTEL_METRICS_EXPORTER":        m,
		"OTEL_EXPORTER_OTLP_PROTOCOL":  "http/json",
		"OTEL_EXPORTER_OTLP_ENDPOINT":  strings.TrimRight(server, "/") + "/api/v1/otlp",
		"OTEL_EXPORTER_OTLP_HEADERS":   "Authorization=Bearer " + token,
	}
}

func cmdUsageOtel(c *cli, args []string) error {
	fs := c.flags()
	install := fs.Bool("install", false, "write them into settings.json")
	uninstall := fs.Bool("uninstall", false, "remove them")
	metrics := fs.Bool("metrics", false, "also export metrics (dropped by the server)")
	force := fs.Bool("force", false, "replace OpenTelemetry settings that point elsewhere")
	if pos, err := c.parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	if *install && *uninstall {
		return c.usage("--install and --uninstall together")
	}
	if c.server() == "" || c.token() == "" {
		return errors.New("not signed in: run wp login first")
	}
	env := otelEnv(c.server(), c.token(), *metrics)
	if !*install && !*uninstall {
		c.println(`Put this in the "env" of ~/.claude/settings.json (or run wp usage otel --install):`)
		c.println()
		b, _ := json.MarshalIndent(map[string]any{"env": env}, "", "  ")
		c.println(string(b))
		return nil
	}
	path, err := claudeSettingsPath()
	if err != nil {
		return err
	}
	msg, err := editClaudeOtelEnv(path, env, *uninstall, *force)
	if err != nil {
		return err
	}
	c.println(msg)
	if !*uninstall {
		c.println("Restart Claude Code sessions to start exporting.")
	}
	return nil
}

// editClaudeOtelEnv adds or removes the OTel variables in settings.json's env.
func editClaudeOtelEnv(path string, env map[string]string, remove, force bool) (string, error) {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	settings := map[string]any{}
	raw, err := os.ReadFile(path) // #nosec G304 -- the user's Claude Code settings
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return "", err
	default:
		if err := json.Unmarshal(raw, &settings); err != nil {
			return "", fmt.Errorf("%s isn't valid JSON, so it was left alone: %w", path, err)
		}
	}
	cur, _ := settings["env"].(map[string]any)
	if settings["env"] != nil && cur == nil {
		return "", fmt.Errorf("%s: env isn't an object, so the file was left alone", path)
	}
	ours := func() bool {
		return cur != nil && cur["OTEL_EXPORTER_OTLP_ENDPOINT"] == env["OTEL_EXPORTER_OTLP_ENDPOINT"]
	}
	if remove {
		if cur == nil || !ours() {
			return "No Waypoint OpenTelemetry settings in " + path + ".", nil
		}
		for k := range env {
			delete(cur, k)
		}
		if len(cur) == 0 {
			delete(settings, "env")
		}
	} else {
		if cur == nil {
			cur = map[string]any{}
			settings["env"] = cur
		}
		if ep, ok := cur["OTEL_EXPORTER_OTLP_ENDPOINT"]; ok && !ours() && !force {
			return "", fmt.Errorf("%s already sends OpenTelemetry to %v; use --force to replace it", path, ep)
		}
		changed := false
		for k, v := range env {
			if cur[k] != v {
				cur[k], changed = v, true
			}
		}
		if !changed {
			return "Already set up in " + path + ".", nil
		}
	}
	backup := ""
	if raw != nil {
		backup = path + ".bak-" + time.Now().Format("20060102-150405")
		if err := os.WriteFile(backup, raw, 0o600); err != nil { // #nosec G703 -- next to the user's own settings file
			return "", err
		}
	}
	if err := writeJSON(path, settings); err != nil {
		return "", err
	}
	verb := "Added OpenTelemetry settings to "
	if remove {
		verb = "Removed OpenTelemetry settings from "
	}
	out := verb + path + "."
	if backup != "" {
		out += "\nBacked up the old file to " + backup + "."
	}
	return out, nil
}
