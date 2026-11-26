package transcript

import (
	"strings"
	"testing"
)

// Audit 2027.1: perl takes -I DIR as two words.
func TestPerlInPlaceFilesSplitIncludeDir(t *testing.T) {
	for _, c := range []struct{ args, want string }{
		{"-I lib -pi -e s/a/b/ a.go", "a.go"},
		{"-I lib -pie s/a/b/ a.go", "a.go"},
		{"-Ilib -pi -e s/a/b/ a.go", "a.go"},
		{"-I lib -ne print a.go", ""},
	} {
		if got := strings.Join(PerlInPlaceFiles(strings.Fields(c.args)), " "); got != c.want {
			t.Errorf("perl %s: %q, want %q", c.args, got, c.want)
		}
	}
	if got, _ := (&ShellSession{}).Edit("perl -I lib -pi -e s/a/b/ a.go", "/r"); !got {
		t.Errorf("perl -I lib -pi edits a.go")
	}
}

// Audit 2027.3: a write whose path the script builds is unknown, not no;
// scratch and reads stay known.
func TestShellEditUnfollowedScriptWriteIsUnknown(t *testing.T) {
	for _, c := range []struct {
		name, cmd   string
		wrote, know bool
	}{
		{"rglob", "python3 - <<'E'\nimport pathlib\nfor p in pathlib.Path('internal').rglob('*.go'):\n    p.write_text('x')\nE\n", false, false},
		{"glob open", "python3 - <<'E'\nimport glob\nfor f in glob.glob('**/*.go', recursive=True):\n    open(f, 'w').write('x')\nE\n", false, false},
		{"walk join", "python3 - <<'E'\nimport os\nfor root, d, names in os.walk('.'):\n    for name in names:\n        open(os.path.join(root, name), 'w').write('x')\nE\n", false, false},
		{"node path from the environment", "node -e \"const fs=require('fs'); const d=fs.readFileSync('web/a.ts','utf8'); fs.writeFileSync(process.env.OUT, d)\"", false, false},
		{"reads only", "python3 - <<'E'\nprint(open('a.go').read())\nE\n", false, true},
		{"followed", "python3 - <<'E'\nopen('a.go','w').write('x')\nE\n", true, true},
		{"tempfile", "python3 - <<'E'\nimport tempfile\nwith tempfile.TemporaryDirectory() as d:\n    for n in names:\n        open(d + n, 'w').write('x')\nE\n", false, true},
		{"mkdtemp join", "python3 - <<'E'\nd = tempfile.mkdtemp()\nopen(os.path.join(d, 'x.go'), 'w').write(s)\nE\n", false, true},
		{"tmp literal name", "python3 - <<'E'\nd = '/tmp/build'\nopen(os.path.join(d, 'x.go'), 'w').write(s)\nE\n", false, true},
	} {
		wrote, known := (&ShellSession{}).Edit(c.cmd, "/home/u/proj")
		if wrote != c.wrote || known != c.know {
			t.Errorf("%s: wrote %v known %v, want %v %v", c.name, wrote, known, c.wrote, c.know)
		}
	}
	// A kept script behaves the same when it is run later.
	s := &ShellSession{}
	s.KeepWrite("/tmp/w.py", "/home/u/proj", "import glob\nfor f in glob.glob('*.go'):\n    open(f, 'w').write('x')\n")
	if wrote, known := s.Edit("python3 /tmp/w.py", "/home/u/proj"); wrote || known {
		t.Errorf("kept script with built path: wrote %v known %v, want false false", wrote, known)
	}
	if got := ScriptWrites("import glob\nfor f in glob.glob('*.go'):\n    open(f, 'w').write('x')\n"); len(got) != 0 {
		t.Errorf("the guard's list is unchanged: %v", got)
	}
}

// Audit 2027.4: + modes and Path(X).open(MODE) are writes.
func TestScriptWritesPlusModesAndPathOpen(t *testing.T) {
	for _, c := range []struct {
		script string
		want   string
	}{
		{"open('.claude/settings.json','r+').write('{}')", ".claude/settings.json"},
		{"open('.claude/settings.json','rb+')", ".claude/settings.json"},
		{"open('.claude/settings.json', 'r+b')", ".claude/settings.json"},
		{"open('.claude/settings.json', mode='a+')", ".claude/settings.json"},
		{"Path('.claude/settings.json').open('w').write('x')", ".claude/settings.json"},
		{"pathlib.Path('.claude/settings.json').open('a')", ".claude/settings.json"},
		{"Path('.claude/settings.json').open(mode='r+')", ".claude/settings.json"},
		{"open(p)", ""},
		{"open('a.go')", ""},
		{"open('a.go','r')", ""},
		{"open('a.go','rb')", ""},
		{"Path('a.go').open()", ""},
		{"Path('a.go').open('r')", ""},
		{"webbrowser.open('x')", ""},
	} {
		got := strings.Join(ScriptWrites(c.script), " ")
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.script, got, c.want)
		}
	}
	if wrote, _ := (&ShellSession{}).Edit(`python3 -c "open('a.go','r+').write('x')"`, "/r"); !wrote {
		t.Errorf("r+ is an edit")
	}
	if wrote, _ := (&ShellSession{}).Edit(`python3 -c "Path('a.go').open('w').write('x')"`, "/r"); !wrote {
		t.Errorf("Path.open('w') is an edit")
	}
	if wrote, known := (&ShellSession{}).Edit(`python3 -c "print(open('a.go').read())"`, "/r"); wrote || !known {
		t.Errorf("a read: %v %v", wrote, known)
	}
}

// Audit 2027.8: -c/-e after a script file belong to the script.
func TestInlineScriptStopsAtScriptFile(t *testing.T) {
	for _, c := range []struct {
		words []string
		want  string
	}{
		{[]string{"python3", "build.py", "-e", "X"}, ""},
		{[]string{"node", "tool.js", "-e", "X"}, ""},
		{[]string{"python3", "-u", "-c", "X"}, "X"},
		{[]string{"python3", "-W", "ignore", "-c", "X"}, "X"},
		{[]string{"node", "-r", "ts-node/register", "-e", "X"}, "X"},
		{[]string{"python3", "-m", "pytest", "-c", "conf.ini"}, ""},
		{[]string{"uv", "run", "python", "-u", "-c", "X"}, "X"},
	} {
		if got := InlineScript(c.words, ""); got != c.want {
			t.Errorf("%v: %q, want %q", c.words, got, c.want)
		}
	}
}

// Audit 2027.10: a cd inside ( ) ends with the group.
func TestShellEditSubshellCdDoesNotCarryOver(t *testing.T) {
	for _, c := range []struct {
		name, cmd   string
		wrote, know bool
	}{
		{"subshell", "(cd /tmp/build && make) && sed -i s/a/b/ main.go", true, true},
		{"two groups", "(cd /tmp/a) ; (cd /tmp/b); sed -i s/a/b/ main.go", true, true},
		{"nested", "(cd a && (cd /tmp && true); sed -i s/a/b/ x.go)", true, true},
		{"nested restores outer", "(cd /tmp && (cd /x) ; sed -i s/a/b/ x.go)", false, true},
		{"brace keeps cd", "{ cd /tmp/build; make; } && sed -i s/a/b/ main.go", false, true},
		{"plain cd keeps", "cd /tmp/build && sed -i s/a/b/ main.go", false, true},
		{"command substitution", "echo $(cd /tmp && pwd) && sed -i s/a/b/ main.go", true, true},
		{"cd in group then write in group", "(cd /tmp/build && sed -i s/a/b/ main.go)", false, true},
	} {
		wrote, known := (&ShellSession{}).Edit(c.cmd, "/home/u/proj")
		if wrote != c.wrote || known != c.know {
			t.Errorf("%s: wrote %v known %v, want %v %v", c.name, wrote, known, c.wrote, c.know)
		}
	}
}

// Audit 2027.13: macOS scratch directories.
func TestShellEditMacScratch(t *testing.T) {
	for _, cmd := range []string{
		"cd /private/tmp/x && echo a > a.py",
		"echo a > /private/var/tmp/a.py",
		"echo a > /var/folders/ab/cd/T/a.py",
		"echo a > /private/var/folders/ab/cd/T/a.py",
	} {
		if wrote, _ := (&ShellSession{}).Edit(cmd, "/Users/u/proj"); wrote {
			t.Errorf("%s counted as an edit", cmd)
		}
	}
	if wrote, _ := (&ShellSession{}).Edit("echo a > a.py", "/Users/u/proj"); !wrote {
		t.Errorf("the work itself is not scratch")
	}
}

// A write the reading cannot follow, beside one it can, leaves the call
// unknown unless a followed write is the work; a name's own open(MODE) is a
// write; and a word that is no script file does not end the options.
func TestShellFollowReviewCases(t *testing.T) {
	py := func(body string) string { return "python3 - <<'E'\n" + body + "\nE" }
	for _, c := range []struct {
		name, cmd    string
		wrote, known bool
	}{
		{"scratch write beside an unfollowed one", py("import os\nopen('/var/tmp/log.txt','w').write('x')\nfor r, _, fs in os.walk('.'):\n    for n in fs:\n        open(os.path.join(r, n), 'w').write('y')"), false, false},
		{"work write beside an unfollowed one", py("import os\nopen('main.go','w').write('x')\nopen(os.path.join(r, n), 'w').write('y')"), true, true},
		{"all followed", py("open('/var/tmp/a.txt','w').write('x')\nopen('/var/tmp/b.txt','a').write('y')"), false, true},
		{"a name's open", py("from pathlib import Path\np = Path('main.go')\nwith p.open('w') as f:\n    f.write('x')"), true, true},
	} {
		var s ShellSession
		if wrote, known := s.Edit(c.cmd, "/home/u/proj"); wrote != c.wrote || known != c.known {
			t.Errorf("%s: wrote %v known %v, want %v %v", c.name, wrote, known, c.wrote, c.known)
		}
	}
	if got := ScriptWrites("from pathlib import Path\np = Path('.claude/settings.json')\np.open('w').write('{}')"); len(got) != 1 || got[0] != ".claude/settings.json" {
		t.Errorf("ScriptWrites of a name's open('w') = %v", got)
	}
	if got := InlineScript([]string{"node", "--max-old-space-size", "100", "-e", "X"}, ""); got != "X" {
		t.Errorf("InlineScript past an unknown option's value = %q, want X", got)
	}
}

// Scratch is judged where a write stands, not by a temporary directory used
// anywhere in the script (mark check 2039.1), and a name set from a join
// under the temporary directory is scratch too (2039.2).
func TestShellEditScratchIsJudgedPerWrite(t *testing.T) {
	py := func(body string) string { return "python3 - <<'E'\n" + body + "\nE" }
	for _, c := range []struct {
		name, cmd    string
		wrote, known bool
	}{
		{"temp dir elsewhere, built write", py("import tempfile, pathlib\nlog = tempfile.mktemp()\nfor p in pathlib.Path('internal').rglob('*.go'):\n    p.write_text(p.read_text())"), false, false},
		{"write to a temp name", py("import tempfile\nout = tempfile.mktemp()\nopen(out, 'w').write('x')"), false, true},
		{"built write under a temp dir", py("import os, tempfile\nd = tempfile.mkdtemp()\nfor n in names:\n    open(os.path.join(d, n), 'w').write('x')"), false, true},
		{"loop over a temp dir", py("import pathlib, tempfile\nd = pathlib.Path(tempfile.mkdtemp())\nfor p in d.rglob('*.go'):\n    p.write_text('x')"), false, true},
		{"name set from a join under gettempdir", py("import os, tempfile\nd = os.path.join(tempfile.gettempdir(), 'w')\nopen(os.path.join(d, 'x.go'), 'w').write('s')"), false, true},
	} {
		var s ShellSession
		if wrote, known := s.Edit(c.cmd, "/home/u/proj"); wrote != c.wrote || known != c.known {
			t.Errorf("%s: wrote %v known %v, want %v %v", c.name, wrote, known, c.wrote, c.known)
		}
	}
}
