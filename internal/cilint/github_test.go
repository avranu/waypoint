package cilint

import (
	"strings"
	"testing"
)

const ghGood = `on:
  push:
    branches: [main]
  schedule:
    - cron: "0 3 * * 1"
  workflow_dispatch:
permissions:
  contents: read
jobs:
  a:
    if: github.server_url == 'https://github.com' && github.ref == 'refs/heads/main'
    runs-on: ubuntu-latest
    permissions:
      contents: read
      security-events: write
    steps:
      # secrets and .forgejo/ in a comment are fine
      - run: make
`

func TestGitHubWorkflowProblems(t *testing.T) {
	sub := func(old, repl string) string {
		if !strings.Contains(ghGood, old) {
			t.Fatalf("fixture lacks %q", old)
		}
		return strings.Replace(ghGood, old, repl, 1)
	}
	if got := GitHubWorkflowProblems("w.yml", []byte(ghGood)); len(got) != 0 {
		t.Fatalf("good workflow flagged: %v", got)
	}
	tests := []struct{ name, bad, want string }{
		{"secrets dot", sub("make", "make ${{ secrets.X }}"), "secrets context"},
		{"secrets toJSON", sub("make", "echo ${{ toJSON(secrets) }}"), "secrets context"},
		{"secrets bare", sub("make", "echo ${{ secrets }}"), "secrets context"},
		{"secrets inherit", sub("    steps:", "    secrets: inherit\n    steps:"), "secrets context"},
		{"forgejo path", sub("make", "./.forgejo/x.sh"), ".forgejo/"},
		{"pull_request", sub("  workflow_dispatch:", "  pull_request:"), "runs on pull_request"},
		{"pull_request_target", sub("  workflow_dispatch:", "  pull_request_target:"), "runs on pull_request_target"},
		{"issue_comment", sub("  workflow_dispatch:", "  issue_comment:"), "runs on issue_comment"},
		{"workflow_run", sub("  workflow_dispatch:", "  workflow_run:"), "runs on workflow_run"},
		{"on a string", sub("on:\n  push:\n    branches: [main]\n  schedule:\n    - cron: \"0 3 * * 1\"\n  workflow_dispatch:\n", "on: push\n"), "on: must be a map"},
		{"top permissions write", sub("permissions:\n  contents: read\njobs", "permissions:\n  contents: write\njobs"), "top-level permissions"},
		{"top permissions extra", sub("permissions:\n  contents: read\njobs", "permissions:\n  contents: read\n  actions: read\njobs"), "top-level permissions"},
		{"job write-all", sub("    permissions:\n      contents: read\n      security-events: write\n", "    permissions: write-all\n"), "permissions must be a map"},
		{"job contents write", sub("      contents: read\n      security", "      contents: write\n      security"), "permission contents: write"},
		{"job id-token write", sub("      security-events: write", "      id-token: write"), "permission id-token: write"},
		{"runs-on self-hosted", sub("ubuntu-latest", "self-hosted"), "runs on self-hosted"},
		{"if missing", sub("    if: github.server_url == 'https://github.com' && github.ref == 'refs/heads/main'\n", ""), "its if:"},
		{"if or true", sub("github.ref == 'refs/heads/main'", "github.ref == 'refs/heads/main' || true"), "its if:"},
		{"if or first", sub("github.server_url == 'https://github.com' &&", "true || github.server_url == 'https://github.com' &&"), "its if:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := GitHubWorkflowProblems("w.yml", []byte(tc.bad))
			if !strings.Contains(strings.Join(got, "\n"), tc.want) {
				t.Errorf("want a problem containing %q, got %v", tc.want, got)
			}
		})
	}
	// the bare condition is fine too
	bare := sub(" && github.ref == 'refs/heads/main'", "")
	if got := GitHubWorkflowProblems("w.yml", []byte(bare)); len(got) != 0 {
		t.Errorf("bare github.server_url condition flagged: %v", got)
	}
}

func TestGitHubFileProblemsAction(t *testing.T) {
	if got := GitHubFileProblems("action.yml", []byte("# secrets\nruns:\n  using: composite\n")); len(got) != 0 {
		t.Errorf("good action flagged: %v", got)
	}
	if got := GitHubFileProblems("action.yml", []byte("runs:\n  steps:\n    - run: ${{ toJSON(secrets) }}\n")); len(got) != 1 {
		t.Errorf("want one secrets problem, got %v", got)
	}
}
