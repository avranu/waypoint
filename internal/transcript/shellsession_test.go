package transcript

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// withCwd puts a working directory on a transcript line.
func withCwd(line, cwd string) string {
	var r map[string]any
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		panic(err)
	}
	r["cwd"] = cwd
	return j(r)
}

func TestShellEditNeedsAWorkingDirectoryTheSessionWorkedIn(t *testing.T) {
	for _, c := range []struct {
		name, cmd, cwd string
		seen           []string
		want           bool
	}{
		{"a file in the cwd", "echo x > /r/a.txt", "/r", nil, true},
		{"a relative file", "echo x > a.txt", "/r", nil, true},
		{"a notes file elsewhere", "echo x > /home/u/notes/a.md", "/r", nil, false},
		{"Downloads", "cp /r/a.go /home/u/Downloads/a.go", "/r", nil, false},
		{"another repo never entered", "sed -i s/a/b/ /srv/other/a.go", "/r", nil, false},
		{"another repo entered earlier", "sed -i s/a/b/ /srv/other/a.go", "/r", []string{"/srv/other"}, true},
		{"the main checkout from a worktree", "sed -i s/a/b/ /r/a.go", "/r/.claude/worktrees/w1", nil, true},
		{"its own worktree from the main checkout", "sed -i s/a/b/ /r/.claude/worktrees/w1/a.go", "/r/.claude/worktrees/w1/sub", nil, true},
		{".worktrees too", "sed -i s/a/b/ /r/a.go", "/r/.worktrees/w1", nil, true},
		{"Claude Code's own directory", "echo x > /home/u/.claude/plans/p.md", "/home/u", nil, false},
		{"Claude Code's memory under a cwd of home", "cat > /home/u/.claude/projects/x/memory/m.md <<E\nx\nE", "/home/u/proj", []string{"/home/u"}, false},
		{"a worktree kept under the claude dir", "echo x > /home/u/.claude/worktrees/w/a.go", "/home/u/.claude/worktrees/w", nil, true},
		{"a checkout the runner keeps in a cache", "sed -i s/a/b/ /home/u/.cache/wp/work/j1/a.go", "/home/u/.cache/wp/work/j1", nil, true},
		{"a cache inside that checkout", "echo x > /home/u/.cache/wp/work/j1/.cache/a.go", "/home/u/.cache/wp/work/j1", nil, false},
		{"a cache", "echo x > /home/u/.cache/a.go", "/home/u", nil, false},
		{"scratch", "echo x > /tmp/a.go", "/tmp/x", nil, false},
		{"home shorthand", "echo x > ~/proj/a.go", "/home/u/proj", nil, true},
		{"$HOME", "echo x > $HOME/notes/a.md", "/home/u/proj", nil, false},
		{"cd ~/proj", "cd ~/proj && sed -i s/a/b/ a.go", "/home/u/proj", nil, true},
		{"macOS home", "cd ~/proj && sed -i s/a/b/ a.go", "/Users/u/proj", nil, true},
		{"root home", "echo x > ~/a.go", "/root", nil, true},
		{"cd to a sibling checkout never a cwd", "cd /srv/sibling && python3 - <<'EOF'\nopen('a.go','w').write('x')\nEOF", "/r", nil, true},
		{"cd to a sibling then sed", "cd /srv/sibling; sed -i s/a/b/ internal/a.go", "/r", nil, true},
		{"cd to Claude's memory dir", "cd /home/u/.claude/projects/x/memory && echo x > m.md", "/r", nil, false},
		{"git apply in the cwd", "git apply fix.patch", "/r", nil, true},
		{"git apply in a clone in tmp", "cd /tmp/clone && git apply fix.patch", "/r", nil, false},
		{"git -C a clone in tmp", "git -C /tmp/s/clone apply x.patch", "/r", nil, false},
		{"patch after a cd the line does not spell out", "cd $d && patch -p1 < x.diff", "/r", nil, false},
		{"find xargs in tmp", "find /tmp/s -name '*.go' | xargs sed -i s/a/b/", "/r", nil, false}, // the files are the find's, in scratch (audit 1923.1)
		{"find xargs in the tree", "find internal -name '*.go' | xargs sed -i s/a/b/", "/r", nil, true},
		{"find print0 xargs in tmp", "find /tmp/s -name '*.go' -print0 | xargs -0 gofmt -w", "/r", nil, false},
		{"find -exec in tmp", "find /tmp/s -name '*.go' -exec sed -i s/a/b/ {} +", "/r", nil, false},
		{"find -exec in the tree", "find internal -name '*.go' -exec sed -i s/a/b/ {} +", "/r", nil, true},
		{"find xargs perl reading", "find internal -name '*.go' | xargs perl -MFile::Basename -ne print", "/r", nil, false},
		{"find xargs perl in place", "find internal -name '*.go' | xargs perl -Mstrict -pi -e s/a/b/", "/r", nil, true},
		{"perl reading with a module", "perl -MFile::Basename -ne 'print' a.go", "/r", nil, false}, // audit 1933.4
		{"perl reading with an include dir", "perl -Ilib -ne 'print' a.go", "/r", nil, false},
		{"perl in place with a module", "perl -Mstrict -pi -e s/a/b/ a.go", "/r", nil, true},
		{"perl in place, clustered", "perl -pie s/a/b/ a.go", "/r", nil, true},
		{"gofmt rewrite rule is no file", "gofmt -w -r 'a -> b' /tmp/x.go", "/r", nil, false}, // audit 1933.7
		{"prettier config is no file", "prettier --config .prettierrc.json --write /tmp/x.md", "/r", nil, false},
		{"prettier writes its file", "prettier --config .prettierrc.json --write web/x.md", "/r", nil, true},
		{"prettier option values are no files", "prettier --trailing-comma all --arrow-parens avoid --quote-props consistent --prose-wrap always --write /tmp/x.ts", "/r", nil, false}, // audit 1954.4
		{"prettier switch takes no value", "prettier --editorconfig --write web/x.md", "/r", nil, true},
		{"goimports profile is no file", "goimports -memprofile mem.out -w /tmp/x.go", "/r", nil, false},
		{"find through a filter to xargs in tmp", "find /tmp/s -name '*.go' | grep -v test | xargs sed -i s/a/b/", "/r", nil, false}, // audit 1931.3
		{"find through a filter to xargs in the tree", "find internal -name '*.go' | grep -v test | sort | xargs sed -i s/a/b/", "/r", nil, true},
		{"a find earlier on the line feeds nothing", "find /tmp/s -name x; ls | xargs sed -i s/a/b/", "/r", nil, true},
		{"patch -d into tmp", "patch -d /tmp/s -p1 < fix.diff", "/r", nil, false}, // audit 1931.4
		{"patch -dDIR into tmp", "patch -d/tmp/s -p1 -i fix.diff", "/r", nil, false},
		{"patch --directory into the tree", "patch --directory=web -p1 < fix.diff", "/r", nil, true},
		{"curl -o into the tree", "curl -sS -o internal/data.json https://example.com/d", "/r", nil, true}, // audit 1931.20
		{"curl -o into tmp", "curl -sS -o /tmp/d.json https://example.com/d", "/r", nil, false},
		{"mv into the tree", "mv /tmp/a.go internal/", "/r", nil, true}, // audit 1931.21
		{"mv within tmp", "mv /tmp/a.go /tmp/b.go", "/r", nil, false},
		{"cp into the tree", "cp /tmp/a.go internal/a.go", "/r", nil, true},
		{"cp to a backup beside the file", "cp internal/a.go internal/a.go.bak", "/r", nil, false},
		{"git rm", "git rm internal/a.go", "/r", nil, true},
		{"git mv", "git mv internal/a.go internal/b.go", "/r", nil, true},
		{"touch makes no text", "touch internal/new.go", "/r", nil, false},
		{"for loop over a glob", "for f in *.go; do gofmt -w \"$f\"; done", "/r", nil, true},
		{"for loop over a substitution", "for f in $(git ls-files); do sed -i s/a/b/ $f; done", "/r", nil, true},
		{"for loop in tmp", "cd /tmp/s && for f in *.go; do gofmt -w \"$f\"; done", "/r", nil, false},
		{"for loop over names", "for f in a.go b.go; do sed -i s/a/b/ $f; done", "/r", nil, true},
		{"a variable set by an earlier call is unknown", "sed -i s/a/b/ $REPO/a.go", "/r", nil, false},
		{"a script kept in the same call", "cat > /tmp/e.py <<'EOF'\nopen('a.go','w').write('x')\nEOF\npython3 /tmp/e.py", "/r", nil, true},
		// Audit 1985: perl's own switch rules, patch -o, and scripts that only read.
		{"perl -pie: e is the backup suffix, the next word the program", "perl -pie 's/a/b/' /tmp/x.go", "/r", nil, false},
		{"perl -pi with no -e", "perl -pi -n 's/a/b/' /tmp/x.go", "/r", nil, false},
		{"perl script with its own long options", "perl tools/check.pl --config .claude/settings.json", "/r", nil, false},
		{"perl script with --input", "perl convert.pl --input data.csv", "/r", nil, false},
		{"perl -i with a program file", "perl -i fix.pl a.go", "/r", nil, true},
		{"perl -i after --", "perl -pi -e s/a/b/ -- a.go", "/r", nil, true},
		{"patch -o into tmp", "patch -o /tmp/out.go main.go < fix.diff", "/r", nil, false},
		{"patch --output= into tmp", "patch --output=/tmp/out.go main.go fix.diff", "/r", nil, false},
		{"patch -o into the tree", "patch -o out.go main.go < fix.diff", "/r", nil, true},
		{"patch -r into tmp still patches in place", "patch -r /tmp/x.rej -p1 < fix.diff", "/r", nil, true},
		{"git apply --check --apply applies", "git apply --check --apply fix.patch", "/r", nil, true},
		{"a script that reads a repo file and writes where it cannot be followed", "python3 - <<'EOF'\nimport os, tempfile\nd = open('internal/a.go', 'r').read()\nout = os.path.join(tempfile.gettempdir(), 's.txt')\nopen(out, 'w').write(d)\nEOF", "/r", nil, false},
		{"a script that reads with an encoding", "python3 - <<'EOF'\nimport os, tempfile\nd = open('internal/a.go', encoding='utf-8').read()\nopen(os.path.join(tempfile.gettempdir(), 's.txt'), 'w').write(d)\nEOF", "/r", nil, false},
		{"a scratch script that writes scratch", "cat > /tmp/e.py <<'EOF'\nopen('/tmp/a.go','w').write('x')\nEOF\npython3 /tmp/e.py", "/r", nil, false},
	} {
		s := &ShellSession{}
		for _, d := range c.seen {
			s.Note(d)
		}
		if got, known := s.Edit(c.cmd, c.cwd); !known || got != c.want {
			t.Errorf("%s: Edit(%q) = %v, %v; want %v", c.name, c.cmd, got, known, c.want)
		}
	}
}

// A session that has worked in more directories than it remembers still
// counts a write in the directory the call itself runs in (audit 1985.17).
func TestShellEditCountsTheCwdWhenTheRootsAreFull(t *testing.T) {
	s := &ShellSession{}
	for i := 0; len(s.Roots) < shellMaxRoots; i++ {
		s.Note("/srv/d" + strings.Repeat("x", i))
	}
	if got, known := s.Edit("echo x > a.go", "/r/new"); !got || !known {
		t.Errorf("a relative write in a cwd past the cap: %v, %v", got, known)
	}
	if len(s.Roots) != shellMaxRoots {
		t.Errorf("roots grew to %d", len(s.Roots))
	}
	if got, _ := s.Edit("echo x > /r/new/b.go", "/srv/elsewhere"); got {
		t.Errorf("a directory that could not be remembered is not one the session worked in later")
	}
}

func TestPerlInPlaceFiles(t *testing.T) {
	for _, c := range []struct {
		args string
		want string
	}{
		{"-pi -e s/a/b/ a.go b.go", "a.go b.go"},
		{"-pie s/a/b/ notes.md", "notes.md"},
		{"-i.bak -pe s/a/b/ a.go", "a.go"},
		{"tools/check.pl --config .claude/settings.json", ""},
		{"-i fix.pl a.go", "a.go"},
		{"-pe s/a/b/ a.go -i", ""},
		{"-Mstrict -ne print a.go", ""},
		{"-pi -e s/a/b/ -- -a.go", "-a.go"},
	} {
		if got := strings.Join(PerlInPlaceFiles(strings.Fields(c.args)), " "); got != c.want {
			t.Errorf("perl %s: %q, want %q", c.args, got, c.want)
		}
	}
}

// The guard and the recogniser find the script a command hands an
// interpreter by one rule (audit 1985.20).
func TestInlineScript(t *testing.T) {
	for _, c := range []struct {
		words   []string
		heredoc string
		want    string
	}{
		{[]string{"python3", "-c", "print(1)"}, "", "print(1)"},
		{[]string{"python3.12", "-"}, "x = 1", "x = 1"},
		{[]string{"uv", "run", "python", "-c", "y"}, "", "y"},
		{[]string{"node", "-e", "z"}, "", "z"},
		{[]string{"/usr/bin/ruby", "-e", "r"}, "", "r"},
		{[]string{"cat"}, "not a script", ""},
		{[]string{"python3", "edit.py"}, "", ""},
	} {
		if got := InlineScript(c.words, c.heredoc); got != c.want {
			t.Errorf("%v: %q, want %q", c.words, got, c.want)
		}
	}
}

// The guard reads a script whole: a write past the part the usage report
// reads is still found (audit 1985.2).
func TestScriptWritesReadsTheWholeScript(t *testing.T) {
	long := strings.Repeat("x = 1\n", traceMaxScript/6+10) + "open('.claude/settings.json','w').write('{}')\n"
	if got := ScriptWrites(long); len(got) != 1 || got[0] != ".claude/settings.json" {
		t.Errorf("a write past %d bytes: %v", traceMaxScript, got)
	}
	many := strings.Repeat("open('a.txt','w').write('x')\n", 80) + "open('.claude/settings.json','w').write('{}')\n"
	if got := ScriptWrites(many); len(got) == 0 || got[len(got)-1] != ".claude/settings.json" {
		t.Errorf("the 81st write site: %v", got)
	}
	// More names than the reader follows: a path the script spells is still
	// listed once it stops following (audit 2008.1).
	var b strings.Builder
	for i := 0; i < traceScriptBudget+20; i++ {
		fmt.Fprintf(&b, "n%d = '/tmp/s%d.txt'\nopen(n%d, 'w').write('x')\n", i, i, i)
	}
	b.WriteString("t = '.claude/settings.json'\nopen(t, 'w').write('{}')\n")
	if got := ScriptWrites(b.String()); !slices.Contains(got, ".claude/settings.json") {
		t.Errorf("a write named past the names followed is missing (%d paths)", len(got))
	}
	padded := strings.Repeat("# pad\n", 60<<10) + "a = '/tmp/a.txt'\nb = '/tmp/b.txt'\nc = '/tmp/c.txt'\nt = '.claude/settings.json'\n" +
		"open(a,'w')\nopen(b,'w')\nopen(c,'w')\nopen(t,'w')\n"
	if got := ScriptWrites(padded); !slices.Contains(got, ".claude/settings.json") {
		t.Errorf("a long script's fourth named write is missing: %v", got)
	}
}

func TestShellEditRemembersScriptsAcrossCalls(t *testing.T) {
	s := &ShellSession{}
	if got, _ := s.Edit("cat > /tmp/e.py <<'EOF'\nopen('internal/a.go','w').write('x')\nEOF", "/r"); got {
		t.Fatalf("saving a script is not an edit")
	}
	if got, _ := s.Edit("python3 /tmp/e.py", "/r"); !got {
		t.Errorf("running a script saved by an earlier call writes internal/a.go")
	}
	s.KeepWrite("/tmp/w.py", "/r", "from pathlib import Path\nPath('web/x.ts').write_text('x')\n")
	if got, _ := s.Edit("python3 /tmp/w.py", "/r"); !got {
		t.Errorf("a script saved with the Write tool then run")
	}
	if got, _ := s.Edit("python3 /tmp/other.py", "/r"); got {
		t.Errorf("a script nobody saved")
	}
	for i := 0; i < ShellMaxScripts+10; i++ {
		s.KeepWrite("/tmp/x"+strings.Repeat("a", i)+".py", "/r", "open('a.go','w').write('x')\n")
	}
	if len(s.Scripts) > ShellMaxScripts {
		t.Errorf("remembers %d scripts", len(s.Scripts))
	}
}

func TestParserKeepsScriptsAndRootsPerFile(t *testing.T) {
	r := parse(t, "main",
		withCwd(assistant("m1", 1, m{"output_tokens": 1},
			toolUse("w1", "Write", m{"file_path": "/tmp/e.py", "content": "open('a.go','w').write('x')\n"}),
			toolUse("b0", "Bash", m{"command": "ls"})), "/r/.claude/worktrees/w1"),
		withCwd(assistant("m2", 2, m{"output_tokens": 1},
			toolUse("b1", "Bash", m{"command": "python3 /tmp/e.py"}),
			toolUse("b2", "Bash", m{"command": "sed -i s/a/b/ /r/x.go"}),
			toolUse("b3", "Bash", m{"command": "sed -i s/a/b/ /srv/else/x.go"})), "/r/.claude/worktrees/w1"),
	)
	got := map[string]string{}
	for _, c := range r.ToolCalls {
		got[c.ToolUseID] = "nil"
		if c.ShellWrite != nil {
			got[c.ToolUseID] = map[bool]string{true: "true", false: "false"}[*c.ShellWrite]
		}
	}
	if got["b1"] != "true" || got["b2"] != "true" || got["b3"] != "false" || got["b0"] != "false" || got["w1"] != "nil" {
		t.Errorf("shell writes: %v", got)
	}
}

func TestParserTooLongCommandIsUnknown(t *testing.T) {
	r := parse(t, "main", withCwd(assistant("m1", 1, m{"output_tokens": 1},
		toolUse("b1", "Bash", m{"command": "echo " + strings.Repeat("x", shellMaxCommand) + " > /r/a.go"})), "/r"))
	if r.ToolCalls[0].ShellWrite != nil {
		t.Errorf("a command over the cap is unknown, got %v", *r.ToolCalls[0].ShellWrite)
	}
}

func TestParserRefusedBashDidNotWrite(t *testing.T) {
	cmd := m{"command": "sed -i s/a/b/ a.go && go test"}
	r := parse(t, "main",
		withCwd(assistant("m1", 1, m{"output_tokens": 1}, toolUse("b1", "Bash", cmd), toolUse("b2", "Bash", cmd), toolUse("b3", "Bash", cmd)), "/r"),
		result("b1", 2, true, "Permission for this action has been denied. Reason: nope", nil),
		result("b2", 2, true, "Exit code 1\nFAIL", nil),
		result("b3", 2, true, "The user doesn't want to proceed with this tool use.", nil),
	)
	// A hook's refusal whose text has none of the known phrases is marked by
	// the record's permissionDecision alone (audit 1933.5).
	r2 := parse(t, "main",
		withCwd(assistant("m1", 1, m{"output_tokens": 1}, toolUse("b4", "Bash", cmd)), "/r"),
		strings.Replace(result("b4", 2, true, "PreToolUse:Bash hook error: [wp]: refused", nil), `{"`, `{"permissionDecision":{"decision":"reject","source":"hook"},"`, 1),
	)
	if c := r2.ToolCalls[0]; c.ShellWrite == nil || *c.ShellWrite {
		t.Errorf("a call the record marks rejected never ran: shell write %v", c.ShellWrite)
	}
	got := map[string]bool{}
	for _, c := range r.ToolCalls {
		got[c.ToolUseID] = c.ShellWrite != nil && *c.ShellWrite
	}
	if got["b1"] || !got["b2"] || got["b3"] {
		t.Errorf("shell write by call: %v; a refusal never ran, a failed test run still wrote", got)
	}
}

func TestLooksRefused(t *testing.T) {
	for text, want := range map[string]bool{
		"Permission for this action has been denied": true,
		"Permission to use Bash has been denied":     true,
		"The user doesn't want to proceed":           true,
		"Exit code 1\nPermission for this":           false,
		"Exit code 2":                                false,
	} {
		if LooksRefused(text) != want {
			t.Errorf("LooksRefused(%q) != %v", text, want)
		}
	}
}
