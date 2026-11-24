package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A claude that edits the CI config as well as the file it was asked for,
// and records how it was run.
const fakeClaudeSneaky = `#!/bin/sh
echo "$@" > "$FAKE_OUT/args"
cat > "$FAKE_OUT/stdin"
echo "fixed" > fix.txt
mkdir -p .github/workflows
echo "on: push" > .github/workflows/ci.yml
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done"}'
`

func TestProtectedFiles(t *testing.T) {
	globs := []string{".github/**", "**/package-lock.json", "go.sum", ".env*", "*.pem"}
	files := []string{"main.go", ".github/workflows/ci.yml", "web/package-lock.json", "go.sum", "sub/go.sum", ".env.local", "key.pem", "deep/key.pem", "docs/ci.md"}
	got := protectedFiles(globs, files)
	want := []string{".github/workflows/ci.yml", "web/package-lock.json", "go.sum", ".env.local", "key.pem"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("protected = %v, want %v", got, want)
	}
	if protectedFiles(nil, files) != nil {
		t.Error("no globs protect something")
	}
	// A "!" pattern exempts a file from broader rules wherever it sits in the list.
	globs = []string{"!**/.env.example", "**/.env", "**/.env.*", "!**/.env.sample"}
	files = []string{"deploy/.env.example", ".env.sample", "deploy/.env", ".env.production", "deploy/.env.example.bak", "main.go"}
	got = protectedFiles(globs, files)
	want = []string{"deploy/.env", ".env.production", "deploy/.env.example.bak"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("with exemptions protected = %v, want %v", got, want)
	}
}

// A "!" glob opens a directory rule only when written for that directory
// (WP-750): the same list the server sends, in the same order.
func TestProtectedFilesDirectoryExemptions(t *testing.T) {
	globs := []string{".github/**", "**/.env.*", "!**/.env.example", "**/secrets/**", "!**/secrets/**/*.go"}
	files := []string{"gate/internal/secrets/secrets.go", "deploy/.env.example", ".github/.env.example", ".github/secrets/x.go", "secrets/prod.yaml", "secrets/.env.example"}
	got := protectedFiles(globs, files)
	want := []string{".github/.env.example", ".github/secrets/x.go", "secrets/prod.yaml", "secrets/.env.example"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("protected = %v, want %v", got, want)
	}
}

// A round on a pull request into a release branch merges that branch: CI
// files it brings are the base's, not the agent's. An edit of its own on top
// is still refused (WP-750).
func TestAgentJobProtectedComparesTheTarget(t *testing.T) {
	merge := `git fetch -q origin release && git -c user.name=a -c user.email=a@example.com merge -q --no-edit FETCH_HEAD` + "\n"
	done := `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done"}'` + "\n"
	setup := func(t *testing.T) string {
		t.Helper()
		remote := newGitRepo(t)
		gitOut(t, remote, "config", "receive.denyCurrentBranch", "ignore")
		main := strings.TrimSpace(gitOut(t, remote, "symbolic-ref", "--short", "HEAD"))
		gitOut(t, remote, "checkout", "-q", "-b", "fix/x")
		if err := os.WriteFile(filepath.Join(remote, "work.txt"), []byte("work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitOut(t, remote, "add", "-A")
		gitOut(t, remote, "commit", "-q", "-m", "Work")
		gitOut(t, remote, "checkout", "-q", "-b", "release", main)
		if err := os.MkdirAll(filepath.Join(remote, ".github", "workflows"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(remote, ".github", "workflows", "ci.yml"), []byte("on: push\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitOut(t, remote, "add", "-A")
		gitOut(t, remote, "commit", "-q", "-m", "CI on the release branch")
		gitOut(t, remote, "checkout", "-q", main)
		return remote
	}
	for _, tc := range []struct {
		name, script, target string
		pushed, held         bool
	}{
		{"merged the target", merge, "release", true, false},
		{"no target known", merge, "", false, true},
		{"edited it too", merge + "echo edited >> .github/workflows/ci.yml\ngit -c user.name=a -c user.email=a@example.com commit -qam edit\n", "release", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeCLI(t, "claude", "#!/bin/sh\ncat > /dev/null\n"+tc.script+done)
			remote := setup(t)
			p := newTestProc(t)
			job := agentJob{CLI: "claude", Prompt: "fix", Repo: remote, Base: "fix/x", Branch: "fix/x", Push: true, Permission: "edit_run",
				CommitMessage: "Fix", ProtectPaths: []string{".github/**"}, Target: tc.target}
			o := p.execAgent(context.Background(), newRun(time.Minute), "agent", job)
			if o.State != "done" {
				t.Fatalf("the round failed: %+v", o)
			}
			if pushed, _ := resultOf(o)["pushed"].(bool); pushed != tc.pushed {
				t.Fatalf("pushed = %v: %+v", pushed, o)
			}
			if _, held := resultOf(o)["held_patch"]; held != tc.held {
				t.Fatalf("held = %v: %+v", held, o.Result)
			}
			if tc.held && tc.pushed {
				if got := gitOut(t, remote, "show", "fix/x:.github/workflows/ci.yml"); strings.Contains(got, "edited") {
					t.Errorf("the held edit was pushed:\n%s", got)
				}
				if !strings.Contains(resultOf(o)["held_patch"].(string), "+edited") {
					t.Errorf("held patch %q", resultOf(o)["held_patch"])
				}
			}
		})
	}
}

// A conflict round on a pull request that changes the CI config itself
// merges its target, whose CI config changed too: where git merged that
// file on its own, the merge is no edit of the agent's and is pushed; where
// the agent settled a conflict in it, or edited it further, it is refused
// (WP-597).
func TestAgentJobProtectedTakesACleanMerge(t *testing.T) {
	merge := `git fetch -q origin main && git -c user.name=a -c user.email=a@example.com merge -q --no-edit FETCH_HEAD || true` + "\n"
	commit := `git add -A && git -c user.name=a -c user.email=a@example.com commit -qm resolve` + "\n"
	done := `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done"}'` + "\n"
	ci := func(t *testing.T, dir, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, ".github", "workflows", "ci.yml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setup := func(t *testing.T, branchCI string) string {
		t.Helper()
		remote := newGitRepo(t)
		gitOut(t, remote, "config", "receive.denyCurrentBranch", "ignore")
		main := strings.TrimSpace(gitOut(t, remote, "symbolic-ref", "--short", "HEAD"))
		if main != "main" {
			gitOut(t, remote, "branch", "-m", main, "main")
		}
		if err := os.MkdirAll(filepath.Join(remote, ".github", "workflows"), 0o755); err != nil {
			t.Fatal(err)
		}
		ci(t, remote, "on: push\njobs:\n  a: one\n  b: two\n  c: three\n  d: four\n")
		gitOut(t, remote, "add", "-A")
		gitOut(t, remote, "commit", "-q", "-m", "CI")
		gitOut(t, remote, "checkout", "-q", "-b", "fix/x")
		ci(t, remote, branchCI)
		if err := os.WriteFile(filepath.Join(remote, "work.txt"), []byte("mine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitOut(t, remote, "add", "-A")
		gitOut(t, remote, "commit", "-q", "-m", "Work")
		gitOut(t, remote, "checkout", "-q", "main")
		ci(t, remote, "on: push\njobs:\n  a: one\n  b: two\n  c: three\n  d: FOUR\n")
		if err := os.WriteFile(filepath.Join(remote, "work.txt"), []byte("theirs\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitOut(t, remote, "add", "-A")
		gitOut(t, remote, "commit", "-q", "-m", "Main moves")
		return remote
	}
	apart := "on: push\njobs:\n  a: ONE\n  b: two\n  c: three\n  d: four\n"
	same := "on: push\njobs:\n  a: one\n  b: two\n  c: three\n  d: 4\n"
	head := "on: push\njobs:\n  a: one\n  b: two\n  c: three\n"
	agentCI := map[string]string{
		"and edited too":                             "on: push\njobs:\n  a: ONE\n  b: two\n  c: three\n  d: FOUR\n  e: five\n",
		"ci.yml conflicted, a new line":              head + "  d: FOUR 4\n",
		"ci.yml conflicted, a shared line deleted":   "on: push\njobs:\n  a: one\n  b: two\n  d: 4\n  d: FOUR\n",
		"ci.yml conflicted, deleted":                 "",
		"ci.yml conflicted, ours taken":              same,
		"ci.yml merged by git, main's change undone": apart,
	}
	conflictCI := func(last string) string {
		return "echo both > work.txt\nprintf 'on: push\\njobs:\\n  a: one\\n  b: two\\n  c: three\\n  d: " + last + "\\n' > .github/workflows/ci.yml\n"
	}
	for _, tc := range []struct {
		name, branchCI, script string
		pushed, held, heldAll  bool
	}{
		{"ci.yml merged by git", apart, merge + "echo both > work.txt\n" + commit, true, false, false},
		{"and edited too", apart, merge + "echo both > work.txt\necho '  e: five' >> .github/workflows/ci.yml\n" + commit, true, true, false},
		// An agent's own line on a shared conflicted line can't be split from the merge, so the whole round is held.
		{"ci.yml conflicted, a new line", same, merge + conflictCI("FOUR 4") + commit, false, true, true},
		{"ci.yml conflicted, both kept", same, merge + conflictCI(`4\n  d: FOUR`) + commit, true, false, false},
		{"ci.yml conflicted, both kept, theirs first", same, merge + conflictCI(`FOUR\n  d: 4`) + commit, true, false, false},
		{"ci.yml conflicted, theirs taken", same, merge + conflictCI("FOUR") + commit, true, false, false},
		// A branch file taken whole, outside the round's diff, can still drop the target's change to it, so it is held like any settling that drops a side's line.
		{"ci.yml conflicted, ours taken", same, merge + conflictCI("4") + commit, false, true, true},
		{"ci.yml merged by git, main's change undone", apart, merge + "echo both > work.txt\nprintf 'on: push\\njobs:\\n  a: ONE\\n  b: two\\n  c: three\\n  d: four\\n' > .github/workflows/ci.yml\n" + commit, true, true, false},
		{"ci.yml conflicted, a shared line deleted", same, merge + `echo both > work.txt
printf 'on: push\njobs:\n  a: one\n  b: two\n  d: 4\n  d: FOUR\n' > .github/workflows/ci.yml
` + commit, false, true, true},
		{"ci.yml conflicted, deleted", same, merge + "echo both > work.txt\ngit rm -q .github/workflows/ci.yml\n" + commit, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeCLI(t, "claude", "#!/bin/sh\ncat > /dev/null\n"+tc.script+done)
			remote := setup(t, tc.branchCI)
			p := newTestProc(t)
			job := agentJob{CLI: "claude", Prompt: "fix", Repo: remote, Base: "fix/x", Branch: "fix/x", Push: true, Permission: "edit_run",
				CommitMessage: "Fix", ProtectPaths: []string{".github/**"}, Target: "main"}
			o := p.execAgent(context.Background(), newRun(time.Minute), "agent", job)
			if o.State != "done" {
				t.Fatalf("the round failed: %+v", o)
			}
			if pushed, _ := resultOf(o)["pushed"].(bool); pushed != tc.pushed {
				t.Fatalf("pushed = %v: %+v", pushed, o.Result)
			}
			if _, held := resultOf(o)["held_patch"]; held != tc.held {
				t.Fatalf("held = %v: %+v", held, o.Result)
			}
			if all, _ := resultOf(o)["held_all"].(bool); all != tc.heldAll {
				t.Fatalf("held_all = %v: %+v", all, o.Result)
			}
			ci := gitOut(t, remote, "show", "fix/x:.github/workflows/ci.yml")
			if tc.held && strings.Contains(ci, "five") || tc.held && strings.Contains(ci, "FOUR 4") {
				t.Errorf("the held change was pushed:\n%s", ci)
			}
			if tc.pushed && strings.Contains(gitOut(t, remote, "show", "fix/x:work.txt"), "mine") {
				t.Errorf("work.txt wasn't pushed")
			}
			if !tc.held {
				return
			}
			if merged, _ := resultOf(o)["held_merge"].(string); tc.name == "ci.yml conflicted, ours taken" && !strings.Contains(merged, "-  d: FOUR") {
				t.Errorf("held_merge %q", merged)
			}
			patch, _ := resultOf(o)["held_patch"].(string)
			po := p.execAgent(context.Background(), newRun(time.Minute), "agent", agentJob{Repo: remote, Base: "fix/x", Branch: "fix/x", Push: true, Patch: patch, CommitMessage: "Held"})
			if po.State != "done" {
				t.Fatalf("the held patch didn't apply: %+v\n%s", po, patch)
			}
			want, ok := agentCI[tc.name]
			if !ok {
				t.Fatalf("no file the agent made for %q", tc.name)
			}
			files := gitOut(t, remote, "ls-tree", "-r", "--name-only", "fix/x")
			if want == "" {
				if strings.Contains(files, "ci.yml") {
					t.Errorf("ci.yml is still there: %s", files)
				}
			} else if got := gitOut(t, remote, "show", "fix/x:.github/workflows/ci.yml"); got != strings.TrimSpace(want) {
				t.Errorf("after the patch ci.yml is\n%s\nwant\n%s", got, want)
			}
			if got := gitOut(t, remote, "show", "fix/x:work.txt"); got != "both" {
				t.Errorf("work.txt %q", got)
			}
		})
	}
}

func TestAgentJobPermissionArguments(t *testing.T) {
	remote := newGitRepo(t)
	p := newTestProc(t)
	args := func(perm string) string {
		t.Helper()
		rec := fakeCLI(t, "claude", fakeClaudeSneaky)
		o := p.execAgent(context.Background(), newRun(time.Minute), "agent", agentJob{CLI: "claude", Prompt: "x", Repo: remote, Permission: perm})
		if o.State != "done" {
			t.Fatalf("%s: %+v", perm, o)
		}
		b, _ := os.ReadFile(filepath.Join(rec, "args"))
		return string(b)
	}
	if a := args("read"); !strings.Contains(a, "--tools Read,Grep,Glob --permission-mode dontAsk --allowedTools Read(//") || strings.Contains(a, "acceptEdits") || strings.Contains(a, "Bash") {
		t.Errorf("read: %s", a)
	}
	if a := args("edit"); !strings.Contains(a, "--permission-mode acceptEdits") || strings.Contains(a, "Bash") {
		t.Errorf("edit: %s", a)
	}
	if a := args("edit_run"); !strings.Contains(a, "acceptEdits") || !strings.Contains(a, "Read,Grep,Glob,Edit,Write,Bash") {
		t.Errorf("edit_run: %s", a)
	}

	codex := func(job agentJob) string {
		rec := fakeCLI(t, "codex", `#!/bin/sh
cat > "$FAKE_OUT/stdin"
echo "$@" > "$FAKE_OUT/args"
echo '{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`)
		job.CLI, job.Prompt, job.Repo = "codex", "x", remote
		if o := newTestProc(t).execAgent(context.Background(), newRun(time.Minute), "agent", job); o.State != "done" {
			t.Fatalf("codex: %+v", o)
		}
		b, _ := os.ReadFile(filepath.Join(rec, "args"))
		return string(b)
	}
	if a := codex(agentJob{Permission: "read"}); !strings.Contains(a, "--sandbox read-only") || strings.Contains(a, "network_access") {
		t.Errorf("codex read: %s", a)
	}
	if a := codex(agentJob{Permission: "edit"}); !strings.Contains(a, "--sandbox workspace-write") || strings.Contains(a, "network_access") {
		t.Errorf("codex edit: %s", a)
	}
	if a := codex(agentJob{Permission: "edit_run"}); !strings.Contains(a, "sandbox_workspace_write.network_access=true") {
		t.Errorf("codex edit_run: %s", a)
	}

	opencode := func(perm string) string {
		rec := fakeCLI(t, "opencode", `#!/bin/sh
case "$*" in *"--help"*) echo "  --format  format: default or json"; exit 0;; esac
cat > "$FAKE_OUT/stdin"
echo "$OPENCODE_PERMISSION" > "$FAKE_OUT/perm"
echo '{"type":"text","part":{"text":"ok"}}'
`)
		if o := newTestProc(t).execAgent(context.Background(), newRun(time.Minute), "agent", agentJob{CLI: "opencode", Prompt: "x", Repo: remote, Permission: perm}); o.State != "done" {
			t.Fatalf("opencode: %+v", o)
		}
		b, _ := os.ReadFile(filepath.Join(rec, "perm"))
		return strings.TrimSpace(string(b))
	}
	if got := opencode("edit"); !strings.Contains(got, `"bash":"deny"`) {
		t.Errorf("opencode edit: %q", got)
	}
	if got := opencode("edit_run"); !strings.Contains(got, `"bash":"allow"`) {
		t.Errorf("opencode edit_run: %q", got)
	}
	if got := opencode("read"); !strings.Contains(got, `"external_directory":"deny"`) || !strings.Contains(got, `"bash":"deny"`) || !strings.Contains(got, `"edit":"deny"`) {
		t.Errorf("opencode read permissions: %q", got)
	}
}

// A claude that merges main into the branch (to clear a conflict) and fixes
// one file: the CI files that arrive with the merge are main's, not its own.
const fakeClaudeMerger = `#!/bin/sh
cat > /dev/null
GIT_AUTHOR_NAME=a GIT_AUTHOR_EMAIL=a@example.com GIT_COMMITTER_NAME=a GIT_COMMITTER_EMAIL=a@example.com git merge --no-edit refs/remotes/origin/main >/dev/null
echo "fixed" > fix.txt
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done"}'
`

func TestAgentJobIgnoresProtectedFilesMainChanged(t *testing.T) {
	fakeCLI(t, "claude", fakeClaudeMerger)
	remote := newGitRepo(t)
	gitOut(t, remote, "config", "receive.denyCurrentBranch", "ignore")
	gitOut(t, remote, "branch", "fix/ci")
	if err := os.MkdirAll(filepath.Join(remote, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, ".github", "workflows", "ci.yml"), []byte("on: push\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, remote, "add", "-A")
	gitOut(t, remote, "-c", "user.name=m", "-c", "user.email=m@example.com", "commit", "-q", "-m", "CI change on main")
	p := newTestProc(t)
	job := agentJob{CLI: "claude", Prompt: "resolve the conflict", Repo: remote, Base: "fix/ci", Branch: "fix/ci", Push: true, Permission: "edit_run",
		CommitMessage: "Fix", ProtectPaths: []string{".github/**"}}
	o := p.execAgent(context.Background(), newRun(time.Minute), "agent", job)
	if o.State != "done" {
		t.Fatalf("a merge of main was refused: %+v", o)
	}
	if res, _ := o.Result.(map[string]any); res["pushed"] != true {
		t.Errorf("not pushed: %v", res)
	}
}

func TestAgentJobRefusesToPushProtectedFiles(t *testing.T) {
	fakeCLI(t, "claude", fakeClaudeSneaky)
	remote := newGitRepo(t)
	gitOut(t, remote, "config", "receive.denyCurrentBranch", "ignore")
	p := newTestProc(t)
	job := agentJob{CLI: "claude", Prompt: "fix the test", Repo: remote, Branch: "fix/ci", Push: true, Permission: "edit_run", CommitMessage: "Fix"}
	job.ProtectPaths = []string{".github/**", "**/package-lock.json"}
	o := p.execAgent(context.Background(), newRun(time.Minute), "agent", job)
	res, _ := o.Result.(map[string]any)
	if o.State != "done" || res["pushed"] != true {
		t.Fatalf("outcome = %+v", o)
	}
	if held, _ := res["held_files"].([]string); len(held) != 1 || held[0] != ".github/workflows/ci.yml" {
		t.Errorf("held files = %v", res["held_files"])
	}
	if files, _ := res["files"].([]string); len(files) != 1 || files[0] != "fix.txt" {
		t.Errorf("files = %v", res["files"])
	}
	if got := gitOut(t, remote, "ls-tree", "-r", "--name-only", "fix/ci"); strings.Contains(got, ".github") || !strings.Contains(got, "fix.txt") {
		t.Errorf("pushed tree: %q", got)
	}
	remote = newGitRepo(t)
	gitOut(t, remote, "config", "receive.denyCurrentBranch", "ignore")
	job.Repo = remote
	job.ProtectPaths = nil
	o = p.execAgent(context.Background(), newRun(time.Minute), "agent", job)
	if o.State != "done" {
		t.Fatalf("unprotected = %+v", o)
	}
	if files := o.Result.(map[string]any)["files"].([]string); len(files) != 2 || files[0] != ".github/workflows/ci.yml" {
		t.Errorf("files = %v", files)
	}
}

// An agent can't get a protected file past the check by renaming it away,
// or by giving a new one a name git would quote or split (WP-415).
func TestAgentJobProtectedRenamesAndOddNames(t *testing.T) {
	for _, tc := range []struct {
		name, script, glob, want string
	}{
		{"renamed out", `git mv .github/workflows/ci.yml old-ci.txt`, ".github/**", ".github/workflows/ci.yml"},
		{"renamed in", `git mv a.txt .github/workflows/a.yml`, ".github/**", ".github/workflows/a.yml"},
		{"a space", `echo x > ".github/workflows/x y.yml"`, ".github/workflows/*.yml", ".github/workflows/x y.yml"},
		{"non-ASCII", `echo x > ".github/workflows/é.yml"`, ".github/workflows/*.yml", ".github/workflows/é.yml"},
		{"a leading blank", `echo x > " key.pem"`, "*.pem", " key.pem"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeCLI(t, "claude", "#!/bin/sh\ncat > /dev/null\n"+tc.script+"\n"+
				`printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done"}'`+"\n")
			remote := newGitRepo(t)
			gitOut(t, remote, "config", "receive.denyCurrentBranch", "ignore")
			if err := os.MkdirAll(filepath.Join(remote, ".github", "workflows"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(remote, ".github", "workflows", "ci.yml"), []byte("on: push\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitOut(t, remote, "add", "-A")
			gitOut(t, remote, "commit", "-q", "-m", "CI")
			p := newTestProc(t)
			job := agentJob{CLI: "claude", Prompt: "fix", Repo: remote, Branch: "fix/x", Push: true, Permission: "edit_run",
				CommitMessage: "Fix", ProtectPaths: []string{tc.glob}}
			o := p.execAgent(context.Background(), newRun(time.Minute), "agent", job)
			files, _ := resultOf(o)["held_files"].([]string)
			if o.State != "done" || !slices.Contains(files, tc.want) {
				t.Fatalf("outcome = %+v", o)
			}
			pushed := gitOut(t, remote, "branch", "--list", "fix/x") != ""
			if pushed != (tc.name == "renamed out" || tc.name == "renamed in") {
				t.Errorf("pushed = %v", pushed)
			}
			if pushed && strings.Contains(gitOut(t, remote, "ls-tree", "-r", "--name-only", "fix/x"), "a.yml") {
				t.Errorf("the held file was pushed")
			}
		})
	}
}

// resultOf is an outcome's result map.
func resultOf(o outcome) map[string]any {
	m, _ := o.Result.(map[string]any)
	return m
}

// resolutionIsTheMerge passes a resolution that keeps every line outside the
// conflicts and settles each with both sides' lines whole; a line dropped,
// moved or new is the agent's (WP-ADR138).
func TestResolutionIsTheMerge(t *testing.T) {
	merged := "a\nb\n<<<<<<< ours\nmine 1\nmine 2\n=======\ntheirs\n>>>>>>> theirs\nc\nd\n<<<<<<< ours\nx\n=======\ny\n>>>>>>> theirs\ne\n"
	for _, tc := range []struct {
		name, resolved string
		ok             bool
	}{
		{"both kept, ours first", "a\nb\nmine 1\nmine 2\ntheirs\nc\nd\nx\ny\ne\n", true},
		{"both kept, theirs first", "a\nb\ntheirs\nmine 1\nmine 2\nc\nd\ny\nx\ne\n", true},
		{"blank lines and CRLF", "a\r\n\nb\nmine 1\nmine 2\n  \ntheirs\nc\nd\nx\ny\ne", true},
		{"ours taken", "a\nb\nmine 1\nmine 2\nc\nd\nx\ne\n", false},
		{"theirs taken", "a\nb\ntheirs\nc\nd\ny\ne\n", false},
		{"a line outside the conflicts deleted", "a\nb\nmine 1\nmine 2\ntheirs\nd\nx\ny\ne\n", false},
		{"a side's lines interleaved", "a\nb\nmine 1\ntheirs\nmine 2\nc\nd\nx\ny\ne\n", false},
		{"lines reordered", "b\na\nmine 1\nmine 2\ntheirs\nc\nd\nx\ny\ne\n", false},
		{"a new line", "a\nb\nmine 1\nmine 2\ntheirs\nnew\nc\nd\nx\ny\ne\n", false},
		{"emptied", "", false},
		{"the markers left in", merged, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolutionIsTheMerge(tc.resolved, merged); got != tc.ok {
				t.Errorf("= %v", got)
			}
		})
	}
	diff3 := "a\n<<<<<<< ours\nmine\n||||||| base\nold\n=======\ntheirs\n>>>>>>> theirs\nb\n"
	if !resolutionIsTheMerge("a\nmine\ntheirs\nb\n", diff3) || resolutionIsTheMerge("a\nold\nb\n", diff3) {
		t.Error("diff3 style")
	}
	// A conflict with no markers (modify/delete) counts as the merge only if the file matches what git left.
	if !resolutionIsTheMerge("kept\n", "kept\n") || resolutionIsTheMerge("", "kept\n") {
		t.Error("modify/delete")
	}
	for _, bad := range []string{"<<<<<<< a\nx\n", "=======\n", ">>>>>>> b\n", "<<<<<<< a\n<<<<<<< a\n=======\n>>>>>>> b\n"} {
		if resolutionIsTheMerge("x\n", bad) {
			t.Errorf("odd markers %q", bad)
		}
	}
}

// A held patch is applied to the branch and pushed with no agent; a patch the
// branch no longer takes fails and pushes nothing (WP-ADR138).
func TestExecPatch(t *testing.T) {
	const add = "diff --git a/.github/ci.yml b/.github/ci.yml\nnew file mode 100644\n--- /dev/null\n+++ b/.github/ci.yml\n@@ -0,0 +1 @@\n+on: push\n"
	const bad = "diff --git a/work.txt b/work.txt\n--- a/work.txt\n+++ b/work.txt\n@@ -1 +1 @@\n-nothing like it\n+x\n"
	for _, tc := range []struct {
		name, patch string
		ok          bool
	}{{"applies", add, true}, {"doesn't apply", bad, false}} {
		t.Run(tc.name, func(t *testing.T) {
			remote := newGitRepo(t)
			gitOut(t, remote, "config", "receive.denyCurrentBranch", "ignore")
			gitOut(t, remote, "checkout", "-q", "-b", "fix/x")
			if err := os.WriteFile(filepath.Join(remote, "work.txt"), []byte("work\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitOut(t, remote, "add", "-A")
			gitOut(t, remote, "commit", "-q", "-m", "Work")
			before := strings.TrimSpace(gitOut(t, remote, "rev-parse", "fix/x"))
			gitOut(t, remote, "checkout", "-q", "-")
			p := newTestProc(t)
			o := p.execAgent(context.Background(), newRun(time.Minute), "agent", agentJob{Repo: remote, Base: "fix/x", Branch: "fix/x", Push: true, Patch: tc.patch, CommitMessage: "Held"})
			after := strings.TrimSpace(gitOut(t, remote, "rev-parse", "fix/x"))
			if tc.ok {
				if o.State != "done" || after == before {
					t.Fatalf("outcome %+v, head %s -> %s", o, before, after)
				}
				if got := gitOut(t, remote, "show", "fix/x:.github/ci.yml"); got != "on: push" {
					t.Errorf("pushed file %q", got)
				}
				if got := strings.TrimSpace(gitOut(t, remote, "log", "-1", "--format=%s", "fix/x")); got != "Held" {
					t.Errorf("message %q", got)
				}
				return
			}
			if o.State == "done" || after != before || !strings.Contains(o.Error, "doesn't apply") {
				t.Errorf("outcome %+v, head %s -> %s", o, before, after)
			}
		})
	}
	// Only the branch guard stops a held patch pushing elsewhere; this test remote would accept that push.
	remote := newGitRepo(t)
	gitOut(t, remote, "config", "receive.denyCurrentBranch", "ignore")
	gitOut(t, remote, "branch", "fix/x")
	base := strings.TrimSpace(gitOut(t, remote, "rev-parse", "HEAD"))
	before := strings.TrimSpace(gitOut(t, remote, "rev-parse", "fix/x"))
	o := newTestProc(t).execAgent(context.Background(), newRun(time.Minute), "agent", agentJob{Repo: remote, Base: base, Branch: "fix/x", Push: true, Patch: add, CommitMessage: "Held"})
	if o.State == "done" || !strings.Contains(o.Error, "branch it starts from") {
		t.Errorf("a patch to another branch than its base: %+v", o)
	}
	if after := strings.TrimSpace(gitOut(t, remote, "rev-parse", "fix/x")); after != before {
		t.Errorf("fix/x moved: %s -> %s", before, after)
	}
}
