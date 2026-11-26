package transcript

import (
	"strings"
	"testing"
)

// Audit 2047: writes the recogniser did not reach, and answers that were a
// known "no" where the reading could not tell.
func TestShellEditReach(t *testing.T) {
	many := "python3 - <<'E'\nimport tempfile\nd = tempfile.mkdtemp()\n" + strings.Repeat("open(d + '/a', 'w').write('x')\n", 70) + "for f in names:\n    open(build(f), 'w').write('x')\nE\n"
	for _, c := range []struct {
		name, cmd, cwd string
		wrote, know    bool
	}{
		// .2: a working directory this reading cannot place a relative path in.
		{"windows cwd, relative write", "sed -i s/a/b/ main.go", `C:\Users\x\repo`, true, true},
		{"windows cwd, heredoc", "cat > pkg/x.go <<EOF\nx\nEOF\n", `C:\Users\x\repo`, true, true},
		{"windows cwd, its bash path", "echo x > /c/Users/x/repo/a.go", `C:\Users\x\repo`, true, true},
		{"windows cwd, home", "echo x > ~/repo/a.go", `C:\Users\x\repo`, true, true},
		{"a directory that is no path", "sed -i s/a/b/ main.go", `\\host\share\repo`, false, false},
		{"windows cwd, no write", "go test ./...", `C:\Users\x\repo`, false, true},
		{"windows cwd, scratch", "echo x > /tmp/a.go", `C:\Users\x\repo`, false, true},
		// .7: a write past the sites read.
		{"write past the sites read", many, "/home/u/proj", false, false},
		// .8: commands inside a substitution.
		{"$( ) sed", "echo $(sed -i 's/a/b/' main.go)", "/home/u/proj", true, true},
		{"backtick sed", "echo `sed -i 's/a/b/' main.go`", "/home/u/proj", true, true},
		{"process substitution tee", "go run ./cmd/gen > >(tee docs/CLI.md)", "/home/u/proj", true, true},
		{"$( ) in an assignment", "x=$(echo a > b.go)", "/home/u/proj", true, true},
		{"find piped past a substitution", "find /tmp/x -name '*.go' | xargs sed -i $(echo s/a/b/)", "/home/u/proj", false, true},
		{"$( ) reads only", "echo $(cat main.go)", "/home/u/proj", false, true},
		{"cd in $( ) ends with it", "x=$(cd /tmp && pwd); echo a > b.go", "/home/u/proj", true, true},
		// .9: a home this reading cannot infer.
		{"home with no home to infer", "echo x > ~/a.go", "/srv/r", false, false},
		{"home inferred", "echo x > ~/proj/a.go", "/home/u/proj", true, true},
		// .11: more source endings, and dotfiles.
		{"graphql", "python3 gen_api.py > api/schema.graphql", "/home/u/proj", true, true},
		{"swift", "node scripts/gen.js > src/Api.swift", "/home/u/proj", true, true},
		{"dotfile", "python3 gen.py > .env", "/home/u/proj", true, true},
		{"gitignore", "curl -s https://x/y > .gitignore", "/home/u/proj", true, true},
		{"log from a program", "python3 run.py > run.log", "/home/u/proj", false, true},
		// .13: formatters and archives.
		{"black", "black src/", "/home/u/proj", true, true},
		{"black --check", "black --check src/", "/home/u/proj", false, true},
		{"black --diff", "black --diff src/", "/home/u/proj", false, true},
		{"ruff format", "ruff format .", "/home/u/proj", true, true},
		{"ruff format --check", "ruff format --check .", "/home/u/proj", false, true},
		{"ruff check --fix", "ruff check --fix .", "/home/u/proj", true, true},
		{"ruff check", "ruff check .", "/home/u/proj", false, true},
		{"go fmt", "go fmt ./...", "/home/u/proj", true, true},
		{"go fmt -n", "go fmt -n ./...", "/home/u/proj", false, true},
		{"cargo fmt", "cargo fmt", "/home/u/proj", true, true},
		{"cargo fmt --check", "cargo fmt --check", "/home/u/proj", false, true},
		{"cargo fmt -- --check", "cargo fmt -- --check", "/home/u/proj", false, true},
		{"rustfmt", "rustfmt src/main.rs", "/home/u/proj", true, true},
		{"eslint --fix", "eslint --fix src", "/home/u/proj", true, true},
		{"npx eslint --fix", "npx eslint --fix src", "/home/u/proj", true, true},
		{"eslint", "eslint src", "/home/u/proj", false, true},
		{"formatter elsewhere", "black /tmp/x", "/home/u/proj", false, true},
		{"tar x", "tar -xzf a.tgz", "/home/u/proj", true, true},
		{"tar x bundled", "tar xf a.tar", "/home/u/proj", true, true},
		{"tar --extract", "tar --extract -f a.tar", "/home/u/proj", true, true},
		{"tar x -C scratch", "tar -xzf a.tgz -C /tmp/x", "/home/u/proj", false, true},
		{"tar t", "tar -tzf a.tgz", "/home/u/proj", false, true},
		{"tar c", "tar -czf /tmp/out.tgz src", "/home/u/proj", false, true},
		{"unzip", "unzip -o a.zip", "/home/u/proj", true, true},
		{"unzip -d scratch", "unzip a.zip -d /tmp/x", "/home/u/proj", false, true},
		{"unzip -l", "unzip -l a.zip", "/home/u/proj", false, true},
		// Mark check 2054.2: a generator's output into a data file is an edit; a
		// check's is a report.
		{"go run to json", "go run ./cmd/schema > openapi.json", "/home/u/proj", true, true},
		{"go generate to html", "go run ./cmd/docs > docs/index.html", "/home/u/proj", true, true},
		{"npm script to json", "npm run gen:schema > web/schema.json", "/home/u/proj", true, true},
		{"make target to xml", "make sitemap > public/sitemap.xml", "/home/u/proj", true, true},
		{"go run to tee json", "go run ./cmd/schema | tee openapi.json", "/home/u/proj", true, true},
		{"go test -json", "go test -json ./... > out.json", "/home/u/proj", false, true},
		{"go run to a log", "go run ./cmd/schema > out.log", "/home/u/proj", false, true},
		{"go run to txt", "go run ./cmd/schema > out.txt", "/home/u/proj", false, true},
		{"npm test to json", "npm run test -- --json > results.json", "/home/u/proj", false, true},
		{"npm lint to json", "npm run lint > lint.json", "/home/u/proj", false, true},
		{"make check to xml", "make check > report.xml", "/home/u/proj", false, true},
		{"curl to json", "curl -s https://x/y > page.json", "/home/u/proj", false, true},
		{"go list to json", "go list -json ./... > pkgs.json", "/home/u/proj", false, true},
		// .16: a report kept with tee is a report.
		{"tee a test log", "go test ./... 2>&1 | tee test.log", "/home/u/proj", false, true},
		{"tee a build log", "make build 2>&1 | tee build.log", "/home/u/proj", false, true},
		{"tee -a a log", "go vet ./... | tee -a out.txt", "/home/u/proj", false, true},
		{"tee a generated document", "go run ./cmd/gen | tee docs/CLI.md", "/home/u/proj", true, true},
		{"tee typed text", "printf x | tee a.txt", "/home/u/proj", true, true},
		// .17: a redirect on a group or loop.
		{"group redirect", "{ echo a; echo b; } > config.yaml", "/home/u/proj", true, true},
		{"loop redirect", "for f in a b; do cat $f; done > docs/all.md", "/home/u/proj", true, true},
		{"subshell redirect", "(echo a; echo b) > config.yaml", "/home/u/proj", true, true},
		{"group redirect to a log", "{ go test ./...; go vet ./...; } > out.log", "/home/u/proj", false, true},
		{"group redirect to scratch", "{ echo a; } > /tmp/a.yaml", "/home/u/proj", false, true},
		// .18: a comment after a command.
		{"comment with an apostrophe", "ls # it's here\nsed -i 's/a/b/' main.go", "/home/u/proj", true, true},
		{"comment with an arrow", "go vet ./... # a.go -> b.go", "/home/u/proj", false, true},
		{"hash inside a word", "echo a#b > c.go", "/home/u/proj", true, true},
		{"hash in a parameter", "echo $# ${#x} > c.go", "/home/u/proj", true, true},
		// .19: a reverse apply writes.
		{"git apply -R", "git apply -R fix.patch", "/home/u/proj", true, true},
		{"patch -R", "patch -R -p1 < fix.patch", "/home/u/proj", true, true},
		{"git apply -R --check", "git apply -R --check fix.patch", "/home/u/proj", false, true},
	} {
		wrote, known := (&ShellSession{}).Edit(c.cmd, c.cwd)
		if wrote != c.wrote || known != c.know {
			t.Errorf("%s: wrote %v known %v, want %v %v", c.name, wrote, known, c.wrote, c.know)
		}
	}
}

func TestParseShellComment(t *testing.T) {
	cmds := ParseShell("ls -l # it's here\nsed -i 's/a/b/' main.go")
	if len(cmds) != 2 || strings.Join(cmds[0].Words, " ") != "ls -l" || cmds[1].Words[0] != "sed" {
		t.Fatalf("got %d commands: %+v", len(cmds), cmds)
	}
	if cmds = ParseShell("echo a#b c # d > e"); len(cmds) != 1 || strings.Join(cmds[0].Words, " ") != "echo a#b c" || len(cmds[0].Redirs) != 0 {
		t.Fatalf("a#b is a word and the comment has no redirect: %+v", cmds[0])
	}
}

// Audit 2062: a continuation line, places a write cannot be put, runners,
// and the caps that cut a reading short.
func TestShellEditAudit2062(t *testing.T) {
	for _, c := range []struct {
		name, cmd, cwd string
		wrote, know    bool
	}{
		// .3: a command on a continuation line.
		{"continuation after &&", "cd web && \\\n  sed -i s/a/b/ x.go", "/home/u/proj", true, true},
		{"continuation after a pipe", "ls |\\\n tee out.go", "/home/u/proj", true, true},
		{"continuation before a redirect target", "echo x > \\\n  a.go", "/home/u/proj", true, true},
		// .5: a write that cannot be placed is unknown, not no.
		{"files from a substitution", "sed -i 's/Old/New/' $(grep -rl Old internal/)", "/home/u/proj", false, false},
		{"gofmt on a substitution", "gofmt -w $(git diff --name-only)", "/home/u/proj", false, false},
		{"while read", "git ls-files | while read f; do sed -i s/a/b/ \"$f\"; done", "/home/u/proj", false, false},
		{"cd to a substitution", "cd \"$(git rev-parse --show-toplevel)\" && sed -i s/a/b/ main.go", "/home/u/proj", false, false},
		{"an unset variable", "sed -i s/a/b/ $REPO/a.go", "/home/u/proj", false, false},
		{"mktemp is scratch", "f=$(mktemp); echo x > $f; echo y > \"$f.go\"", "/home/u/proj", false, true},
		{"mktemp -d is scratch", "d=$(mktemp -d) && echo x > $d/a.go", "/home/u/proj", false, true},
		{"TMPDIR is scratch", "echo x > $TMPDIR/a.go", "/home/u/proj", false, true},
		{"a report to a variable stays a report", "go test ./... > $OUT/test.log", "/home/u/proj", false, true},
		// .10: a data file any program writes, as its .json is.
		{"a script's csv", "python3 scripts/export.py > data/rates.csv", "/home/u/proj", true, true},
		{"a binary's tsv", "./bin/gen > out.tsv", "/home/u/proj", true, true},
		{"a reporter's csv stays a report", "go test -json ./... > out.csv", "/home/u/proj", false, true},
		// .7: formatters through a runner, and curl's -o among other letters.
		{"pnpm exec prettier", "pnpm exec prettier --write src", "/home/u/proj", true, true},
		{"pnpm prettier", "pnpm prettier --write .", "/home/u/proj", true, true},
		{"yarn eslint --fix", "yarn eslint --fix src", "/home/u/proj", true, true},
		{"npm exec prettier", "npm exec prettier -- --write .", "/home/u/proj", true, true},
		{"uv run ruff format", "uv run ruff format", "/home/u/proj", true, true},
		{"poetry run black", "poetry run black .", "/home/u/proj", true, true},
		{"python -m black", "python -m black .", "/home/u/proj", true, true},
		{"python3 -m ruff check", "python3 -m ruff check .", "/home/u/proj", false, true},
		{"pnpm prettier --check", "pnpm prettier --check .", "/home/u/proj", false, true},
		{"yarn add prettier", "yarn add prettier", "/home/u/proj", false, true},
		{"curl -fsSLo", "curl -fsSLo vendor/x.js https://x/y", "/home/u/proj", true, true},
		{"curl -ovendor", "curl -s -ovendor/x.js https://x/y", "/home/u/proj", true, true},
		{"curl -fsSL", "curl -fsSL https://x/y", "/home/u/proj", false, true},
		// .10: a generator's csv or tsv.
		{"go run to csv", "go run ./cmd/export > data/rates.csv", "/home/u/proj", true, true},
		{"npm script to tsv", "npm run export > data/rates.tsv", "/home/u/proj", true, true},
		{"go test to csv", "go test ./... > out.csv", "/home/u/proj", false, true},
		// .11: more commands than are read.
		{"a write past the commands read", strings.Repeat("true;", shellMaxCommands+1) + "echo x > a.go", "/home/u/proj", false, false},
		{"under the cap", strings.Repeat("true;", 100) + "echo x > a.go", "/home/u/proj", true, true},
		// nesting deeper than is read.
		{"a write nested past the depth read", "echo $(echo $(echo $(echo $(echo x > a.go))))", "/home/u/proj", false, false},
		{"parens past the parser's depth", "echo " + strings.Repeat("$(echo ", 20) + "x > a.go" + strings.Repeat(")", 20), "/home/u/proj", false, false},
	} {
		wrote, known := (&ShellSession{}).Edit(c.cmd, c.cwd)
		if wrote != c.wrote || known != c.know {
			t.Errorf("%s: wrote %v known %v, want %v %v", c.name, wrote, known, c.wrote, c.know)
		}
	}
}

// .12 and .18: a kept script whose list of writes was cut, and one that was
// forgotten to make room, are unknown when run, not "wrote nothing".
func TestShellScriptCutsAreUnknown(t *testing.T) {
	var s ShellSession
	body := ""
	for i := 0; i < traceMaxScriptPaths; i++ {
		body += "open('/tmp/x/a" + strings.Repeat("b", i) + ".go','w').write('x')\n"
	}
	s.KeepWrite("/tmp/many.py", "/r", body+"open('main.go','w').write('x')\n")
	if wrote, known := s.Edit("python3 /tmp/many.py", "/r"); wrote || known {
		t.Errorf("a script with more writes than are kept: wrote %v known %v, want false false", wrote, known)
	}
	s = ShellSession{}
	s.KeepWrite("/tmp/first.py", "/r", "open('a.go','w').write('x')\n")
	if wrote, known := s.Edit("python3 /tmp/never.py", "/r"); wrote || !known {
		t.Errorf("a script nobody kept, nothing forgotten: wrote %v known %v", wrote, known)
	}
	for i := 0; i < ShellMaxScripts+3; i++ {
		s.KeepWrite("/tmp/x"+strings.Repeat("a", i)+".py", "/r", "open('a.go','w').write('x')\n")
	}
	if len(s.Scripts) > ShellMaxScripts {
		t.Errorf("remembers %d scripts", len(s.Scripts))
	}
	if wrote, known := s.Edit("python3 /tmp/first.py", "/r"); wrote || known {
		t.Errorf("a forgotten script: wrote %v known %v, want false false", wrote, known)
	}
	// The second-oldest went with the oldest when the mark took a place: it
	// is not remembered either, and the one mark covers every such script.
	if wrote, known := s.Edit("python3 /tmp/x.py", "/r"); wrote || known {
		t.Errorf("the second script forgotten: wrote %v known %v, want false false", wrote, known)
	}
	if wrote, known := s.Edit("go test ./...", "/r"); wrote || !known {
		t.Errorf("a command that runs no script stays known: wrote %v known %v", wrote, known)
	}
}

// .2: a name set many times is followed to every value, and .20: a string
// only looked for in text the script read is not a file it writes.
func TestScriptWritesAudit2062(t *testing.T) {
	script := strings.Repeat("p = 'a.txt'\n", 20) + "p = '.claude/settings.json'\nopen(p, 'w').write('x')\n"
	if got := ScriptWrites(script); !strings.Contains(strings.Join(got, " "), ".claude/settings.json") {
		t.Errorf("the 21st value of a name is not followed: %v", got)
	}
	if ps, _, unf := traceScriptWrites(script, false); !strings.Contains(strings.Join(ps, " "), ".claude/settings.json") && !unf {
		t.Errorf("the short reading neither follows the name nor says unknown: %v", ps)
	}
	search := "import glob\nfor f in glob.glob('docs/*.md'):\n    s = open(f).read()\n    if '.claude/settings.json' in s:\n        open(f, 'w').write(s.replace('x', 'y'))\n"
	if got := ScriptWrites(search); strings.Contains(strings.Join(got, " "), "settings.json") {
		t.Errorf("a string looked for in text read is listed as written: %v", got)
	}
	filter := "import glob\nfor f in glob.glob('**/*.json'):\n    if '.claude/settings.json' in f:\n        open(f, 'w').write('{}')\n"
	if got := ScriptWrites(filter); !strings.Contains(strings.Join(got, " "), "settings.json") {
		t.Errorf("a string that picks the file written must stay listed: %v", got)
	}
}

// .24: a write whose word was left unfilled once the line's expansions were
// spent cannot be placed, so the answer is unknown.
func TestShellEditWritePastExpansionBudget(t *testing.T) {
	cmd := "d=/home/u/proj; a=" + strings.Repeat("x", 4000) + "; echo " + strings.Repeat("$a ", 400) + "; echo x > $d/a.go"
	if wrote, known := (&ShellSession{}).Edit(cmd, "/home/u/proj"); wrote || known {
		t.Errorf("wrote %v known %v, want false false (the path was not filled in)", wrote, known)
	}
	if wrote, known := (&ShellSession{}).Edit("d=/home/u/proj; echo x > $d/a.go", "/home/u/proj"); !wrote || !known {
		t.Errorf("under the budget: wrote %v known %v", wrote, known)
	}
}

func TestParseShellContinuation(t *testing.T) {
	cmds := ParseShell("true && \\\n  sed -i s/a/b/ x.go ;\\\n rm y")
	if len(cmds) != 3 || cmds[1].Words[0] != "sed" || cmds[2].Words[0] != "rm" || !cmds[1].AndIn {
		t.Fatalf("got %d commands: %+v", len(cmds), cmds)
	}
	if cmds = ParseShell("echo '' a\\\nb"); len(cmds) != 1 || len(cmds[0].Words) != 3 || cmds[0].Words[1] != "" || cmds[0].Words[2] != "ab" {
		t.Fatalf("an empty quoted word stays, a continuation joins: %+v", cmds[0])
	}
	deep := "echo " + strings.Repeat("$(echo ", 20) + "x" + strings.Repeat(")", 20)
	if cmds = ParseShell(deep); len(cmds) == 0 || !cmds[len(cmds)-1].Cut {
		t.Fatalf("a line nested past the depth read is not marked cut")
	}
	if cmds = ParseShell("echo $(echo x)"); cmds[0].Cut {
		t.Fatalf("a shallow line is marked cut")
	}
}
