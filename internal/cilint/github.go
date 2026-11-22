package cilint

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// These checks guard the files GitHub's hosted runners run on the public
// mirror (WP-1624). They apply to .github/ only, so they are not rules of
// Lint and `wp ci lint` does not run them; TestRepoGitHubWorkflows does.

var (
	// GitHub reads context names in any case, so SECRETS.X is a secret too.
	// This strict check runs on the lines that are not comments: the word
	// "secrets" there is almost surely the context, and a false alarm is cheap.
	reGitHubSecret = regexp.MustCompile(`(?i)\bsecrets\b`)
	// GitHub expands ${{ }} in a run script before the shell sees it, even on
	// a # line, so this check runs on the whole source, comments included.
	reGitHubSecretExpr = regexp.MustCompile(`(?i)\$\{\{[^}]*\bsecrets\b`)
	reForgejoPath      = regexp.MustCompile(`\.forgejo\b`)
	githubEvents       = []string{"push", "schedule", "workflow_dispatch"}
)

const githubOnly = "github.server_url == 'https://github.com'"

// uncommentedLines drops the lines whose first non-space character is #.
func uncommentedLines(src []byte) string {
	var b strings.Builder
	for line := range strings.SplitSeq(string(src), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// GitHubFileProblems checks a workflow or composite action GitHub runs: the
// repository is public, so it holds no secret (any use of the secrets
// context, or the word "secrets" outside a comment line; a ${{ }} that names
// it is caught in comments too), and forge-sync strips .forgejo/ from what
// GitHub gets, so a path under it has nothing to run. Each problem is one line naming name.
func GitHubFileProblems(name string, src []byte) []string {
	var out []string
	code := uncommentedLines(src)
	if reGitHubSecret.MatchString(code) || reGitHubSecretExpr.Match(src) {
		out = append(out, name+" uses the secrets context (or says \"secrets\" outside a comment line): GitHub's workflows get none")
	}
	if reForgejoPath.MatchString(code) {
		out = append(out, name+" uses a path under .forgejo/, which GitHub never gets")
	}
	return out
}

// GitHubWorkflowProblems checks one workflow beyond GitHubFileProblems: it
// runs on main, the schedule and by hand only (never a pull request's code),
// starts read-only, asks for GitHub's own runners, and says each job is
// GitHub's, since Forgejo has no runner with these labels.
func GitHubWorkflowProblems(name string, src []byte) []string {
	out := GitHubFileProblems(name, src)
	var w struct {
		On          any `yaml:"on"`
		Permissions any
		Jobs        map[string]struct {
			If          any
			RunsOn      any `yaml:"runs-on"`
			Permissions any
		}
	}
	if err := yaml.Unmarshal(src, &w); err != nil {
		return append(out, fmt.Sprintf("%s: %v", name, err))
	}
	switch on := w.On.(type) {
	case map[string]any:
		for ev, v := range on {
			if !slices.Contains(githubEvents, ev) {
				out = append(out, fmt.Sprintf("%s runs on %s: GitHub's workflows run on %s only", name, ev, strings.Join(githubEvents, ", ")))
			}
			if ev == "push" && !pushesMainOnly(v) {
				out = append(out, fmt.Sprintf("%s: push must be branches: [main] only, got %v", name, v))
			}
		}
	default:
		out = append(out, name+": on: must be a map of events")
	}
	if p, ok := w.Permissions.(map[string]any); !ok || len(p) != 1 || p["contents"] != "read" {
		out = append(out, fmt.Sprintf("%s: top-level permissions are %v, want only contents: read", name, w.Permissions))
	}
	if len(w.Jobs) == 0 {
		out = append(out, name+": no jobs")
	}
	for id, j := range w.Jobs {
		if label, ok := j.RunsOn.(string); !ok || !strings.HasPrefix(label, "ubuntu-") {
			out = append(out, fmt.Sprintf("%s job %s runs on %v: GitHub's hosted ubuntu runners only", name, id, j.RunsOn))
		}
		if cond, ok := j.If.(string); !ok || !githubOnlyIf(cond) {
			out = append(out, fmt.Sprintf("%s job %s: its if: must be %s, optionally followed by && and more (no ||)", name, id, githubOnly))
		}
		if j.Permissions != nil {
			p, ok := j.Permissions.(map[string]any)
			if !ok {
				out = append(out, fmt.Sprintf("%s job %s: permissions must be a map of read or none, got %v", name, id, j.Permissions))
			}
			for scope, v := range p {
				if v == "read" || v == "none" || (scope == "security-events" && v == "write") {
					continue
				}
				out = append(out, fmt.Sprintf("%s job %s: permission %s: %v, want read or none (only security-events may be write)", name, id, scope, v))
			}
		}
	}
	return out
}

func githubOnlyIf(cond string) bool {
	cond = strings.TrimSpace(cond)
	if strings.Contains(cond, "||") {
		return false
	}
	return cond == githubOnly || strings.HasPrefix(cond, githubOnly+" && ")
}

// pushesMainOnly reports whether a push event's value is branches: [main] and
// nothing else (a tags: filter beside it would run on tag pushes too).
func pushesMainOnly(v any) bool {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return false
	}
	b, ok := m["branches"].([]any)
	return ok && len(b) == 1 && b[0] == "main"
}
