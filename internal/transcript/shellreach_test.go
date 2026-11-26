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
