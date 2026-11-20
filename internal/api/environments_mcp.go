package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// MCP tools for environments and deployments (WP-D19).
var environmentTools = []mcpTool{
	{Name: "list_environments", Title: "List environments", Annotations: readOnly,
		Description: "Where a project's code runs (production, staging, dev environments): whether each is up and since when, the branch, commit and version it runs, and its last deployment. Without a project: every environment you can see.",
		InputSchema: obj(nil, map[string]any{"project": str("Project key"), "stopped": map[string]any{"type": "boolean", "description": "Include stopped dev environments"}})},
	{Name: "record_deployment", Title: "Record a deployment",
		Description: "Say that a commit was deployed to an environment (created if it doesn't exist: production, staging and dev by name, else a preview). status is started, succeeded (default) or failed; a later call for the same commit finishes a started deployment.",
		InputSchema: obj([]string{"project", "environment"}, map[string]any{"project": str("Project key"), "environment": str("Environment name, e.g. staging"),
			"commit": str("Commit id"), "branch": str("Branch"), "version": str("Version, e.g. 1.4.0"), "status": str("started, succeeded or failed"),
			"url": str("Link to the CI run"), "notes": str("Anything worth knowing")})},
}

func init() { mcpTools = append(mcpTools, environmentTools...) }

func (s *Server) mcpEnvironments(ctx context.Context, actor *core.Actor, name string, args mcpArgs) (string, bool, error) {
	switch name {
	case "list_environments":
		stopped, _ := args["stopped"].(bool)
		list, err := s.svc.ListEnvironments(ctx, actor, args.str("project"), stopped)
		if err != nil {
			return "", true, err
		}
		return environmentsText(list), true, nil
	case "record_deployment":
		d, err := s.svc.RecordDeploymentNamed(ctx, actor, args.str("project"), args.str("environment"), core.DeployInput{
			Commit: args.str("commit"), Branch: args.str("branch"), Version: args.str("version"), Status: args.str("status"),
			URL: args.str("url"), Notes: args.str("notes")})
		if err != nil {
			return "", true, err
		}
		what := shortSHA(d.Commit)
		if what == "" {
			what = d.Version
		}
		return fmt.Sprintf("Recorded %s deployment of %s to %s.", d.Status, what, args.str("environment")), true, nil
	}
	return "", false, nil
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// environmentsText lists environments for agents: one line each.
func environmentsText(list []*core.Environment) string {
	if len(list) == 0 {
		return "No environments."
	}
	var b strings.Builder
	for _, e := range list {
		fmt.Fprintf(&b, "%s/%s (%s) %s", e.Project, e.Name, e.Kind, e.Status)
		if e.Stopped {
			b.WriteString(" (stopped)")
		} else if e.StatusSince != nil {
			b.WriteString(" since " + e.StatusSince.UTC().Format(time.RFC3339))
		}
		if e.URL != "" {
			b.WriteString("  " + e.URL)
		}
		if e.Commit != "" || e.Version != "" {
			b.WriteString("  runs")
			if e.Branch != "" {
				b.WriteString(" " + e.Branch)
			}
			if e.Commit != "" {
				b.WriteString("@" + shortSHA(e.Commit))
			}
			if e.Version != "" {
				b.WriteString(" v" + strings.TrimPrefix(e.Version, "v"))
			}
		}
		if e.Behind {
			b.WriteString(", behind its branch")
		}
		if e.Error != "" && e.Status != core.EnvUp {
			b.WriteString("  [" + e.Error + "]")
		}
		b.WriteString("\n")
	}
	return b.String()
}
