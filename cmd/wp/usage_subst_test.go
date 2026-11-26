package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func substCmds(cmd, cwd string) []traceCommand {
	var mem traceScripts
	var cmds []traceCommand
	traceCommands(cmd, cwd, traceVars(cwd), &mem, &cmds)
	return cmds
}

// A command inside $( ) is not what the call did: a commit message read with
// cat is not a read, but a file the substitution writes is still an edit.
func TestClassifyTraceBashSubstitution(t *testing.T) {
	dir := traceRepos(t)
	main := filepath.Join(dir, "main")
	for _, c := range []struct{ name, cmd, want string }{
		{"git commit heredoc", "git commit -m \"$(cat <<'EOF'\nmsg\nEOF\n)\"", trOther},
		{"gh pr create heredoc", "gh pr create --body \"$(cat <<EOF\nbody\nEOF\n)\"", trOther},
		{"sed -i in a substitution", "echo $(sed -i s/a/b/ main.go)", trEdit},
	} {
		got, _, _ := classifyTraceBash(substCmds(c.cmd, main), main)
		if got != c.want {
			t.Errorf("%s: class %q, want %q", c.name, got, c.want)
		}
	}
}

func TestWhereSubstitutionClasses(t *testing.T) {
	dir := traceRepos(t)
	main := filepath.Join(dir, "main")
	sc := &whScan{}
	plain := filepath.Join(dir, "plain") // a directory that is there, in no checkout
	for _, c := range []struct {
		name, cmd string
		want      []string
	}{
		{"git commit heredoc", "git commit -m \"$(cat <<'EOF'\nmsg\nEOF\n)\"", []string{whGit}},
		{"gh pr create heredoc", "gh pr create --body \"$(cat <<EOF\nbody\nEOF\n)\"", []string{whOther}},
		{"sed -i in a substitution", "echo $(sed -i s/a/b/ main.go)", []string{whShellEdit}},
		{"tar -C elsewhere", "tar -xzf a.tgz -C " + plain, nil},
		{"unzip -d elsewhere", "unzip x.zip -d " + plain, nil},
		{"find elsewhere -exec sed", "find " + plain + " -name '*.go' -exec sed -i s/a/b/ {} +", nil},
		{"tar in the checkout", "tar -xzf a.tgz", []string{whShellEdit}},
	} {
		in, _ := json.Marshal(map[string]string{"command": c.cmd})
		classes, _, _ := sc.whBlockClasses("Bash", in, main)
		for _, k := range classes {
			if k == whShellEdit && c.want == nil {
				t.Errorf("%s: classed as shell edit: %v", c.name, classes)
			}
		}
		if c.want != nil && strings.Join(classes, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: classes %v, want %v", c.name, classes, c.want)
		}
	}
}
