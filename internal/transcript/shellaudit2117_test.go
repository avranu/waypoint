package transcript

import (
	"reflect"
	"strings"
	"testing"
)

// Audit 2117.1: $'…' ends only at an unescaped quote, so what follows it is
// read as commands and redirects.
func TestParseShellAnsiQuote(t *testing.T) {
	cmds := ParseShell(`echo $'\'' ; sudo id`)
	if len(cmds) != 2 || cmds[0].Words[1] != "'" || cmds[1].Words[0] != "sudo" {
		t.Fatalf("got %d commands: %+v", len(cmds), cmds)
	}
	cmds = ParseShell(`echo $'a\'b\n' > /r/x.go`)
	if len(cmds) != 1 || cmds[0].Words[1] != "a'b\n" || len(cmds[0].Redirs) != 1 || cmds[0].Redirs[0] != "/r/x.go" {
		t.Fatalf("got %+v", cmds)
	}
	if wrote, known := (&ShellSession{}).Edit(`echo $'a\'b' > /r/x.go`, "/r"); !wrote || !known {
		t.Errorf("the redirect after $'a\\'b' is a write: %v %v", wrote, known)
	}
}

// Audit 2117.2: bash -c behind options that take a value.
func TestShellDashCPastValueOptions(t *testing.T) {
	for _, c := range []struct {
		args   string
		script string
		ok     bool
	}{
		{"-c S", "S", true},
		{"-lc S", "S", true},
		{"-ce S", "S", true},
		{"-o pipefail -c S", "S", true},
		{"-eo pipefail -c S", "S", true},
		{"-euo pipefail -c S", "S", true},
		{"+O extglob -c S", "S", true},
		{"--norc --rcfile x -c S", "S", true},
		{"-e script.sh", "", false},
		{"-o pipefail script.sh", "", false},
	} {
		if s, ok := ShellDashC(strings.Fields(c.args)); s != c.script || ok != c.ok {
			t.Errorf("ShellDashC(%s) = %q %v, want %q %v", c.args, s, ok, c.script, c.ok)
		}
	}
	if wrote, known := (&ShellSession{}).Edit(`bash -euo pipefail -c 'echo x > a.go'`, "/r"); !wrote || !known {
		t.Errorf("the write in bash -euo pipefail -c is read: %v %v", wrote, known)
	}
}

// Audit 2117.5 and the same thing elsewhere: an option's value (or a
// directory an option moves the write to) is not a file written in the
// command's directory.
func TestShellEditOptionValuesAreNoFiles(t *testing.T) {
	for _, c := range []struct {
		cmd          string
		wrote, known bool
	}{
		// sed: BSD -i '' and -i .bak take a word of their own.
		{`sed -i '' 's/a/b/' /srv/other/a.go`, false, true},
		{`sed -i .bak 's/a/b/' /srv/other/a.go`, false, true},
		{`sed -i '' 's/a/b/' a.go`, true, true},
		{`sed -i 's/a/b/' a.go`, true, true},
		{`sed --line-length 80 -i 's/a/b/' /tmp/a.go`, false, true},
		// cp and mv: -t DIR is where every operand goes; -S's suffix is no file.
		{`cp -t /tmp/s a.go b.go`, false, true},
		{`cp --target-directory=/tmp/s a.go`, false, true},
		{`cp -t pkg /tmp/s/a.go`, true, true},
		{`mv -S .orig /tmp/a /tmp/b`, false, true},
		// formatters: a flag's value is no file.
		{`black -l 100 /tmp/x.py`, false, true},
		{`black --line-length 100 x.py`, true, true},
		{`black -c 'x=1'`, false, true},
		{`rustfmt --edition 2021 /tmp/x.rs`, false, true},
		{`rustfmt --emit stdout x.rs`, false, true},
		{`ruff format --line-length 100 /tmp/x.py`, false, true},
		{`eslint --fix --ext .ts /tmp/s`, false, true},
		{`eslint --fix -c cfg.json src`, true, true},
		// find: its own options before the root.
		{`find -L /tmp/s -name '*.go' -exec sed -i s/a/b/ {} +`, false, true},
		{`find -L /tmp/s -name '*.go' | xargs sed -i s/a/b/`, false, true},
		{`find -name '*.go' -exec sed -i s/a/b/ {} +`, true, true},
		// curl --output-dir places the file.
		{`curl -o x.js --output-dir /tmp/s https://x/y`, false, true},
		{`curl -o x.js https://x/y`, true, true},
		// git rm --cached leaves the file alone.
		{`git rm --cached a.go`, false, true},
		{`git rm a.go`, true, true},
		// patch -o - prints.
		{`patch -o - a.go < fix.diff`, false, true},
		// A runner or go given a directory runs the tool there.
		{`pnpm -C /tmp/s exec prettier --write .`, false, true},
		{`yarn --cwd /tmp/s eslint --fix src`, false, true},
		{`uv run --directory /tmp/s ruff format`, false, true},
		{`pnpm -C web exec prettier --write .`, true, true},
		{`go -C /tmp/s fmt ./...`, false, true},
		{`go -C sub fmt ./...`, true, true},
		{`cargo fmt --manifest-path /tmp/s/Cargo.toml`, false, true},
		{`cargo fmt`, true, true},
	} {
		if wrote, known := (&ShellSession{}).Edit(c.cmd, "/r"); wrote != c.wrote || known != c.known {
			t.Errorf("Edit(%q) = %v, %v, want %v, %v", c.cmd, wrote, known, c.wrote, c.known)
		}
	}
}

// EditFiles names the files of the work a command wrote, once each.
func TestShellEditFiles(t *testing.T) {
	files, known := (&ShellSession{}).EditFiles(`sed -i s/a/b/ a.go b.go && echo x > /r/a.go && echo y > /tmp/c && patch -d sub -p1 < fix.diff`, "/r")
	if want := []string{"/r/a.go", "/r/b.go", "/r/sub/*"}; !known || !reflect.DeepEqual(files, want) {
		t.Errorf("EditFiles = %q %v, want %q", files, known, want)
	}
	if files, known := (&ShellSession{}).EditFiles(`ls`, "/r"); files != nil || !known {
		t.Errorf("ls: %q %v", files, known)
	}
}
