package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func ruleList(rs []PermissionRule) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Rule)
	}
	return out
}

func TestBashRules(t *testing.T) {
	cases := []struct {
		cmd  string
		want []string
	}{
		{"wp request show R-12", []string{"Bash(wp request show R-12)", "Bash(wp request show *)"}},
		{"make lint", []string{"Bash(make lint)"}}, // nothing to widen
		{"ls -la /tmp/x", []string{"Bash(ls -la /tmp/x)", "Bash(ls -la *)"}},
		{"cd /repo && wp pr show 12 2>&1", []string{"Bash(wp pr show 12)", "Bash(wp pr show *)"}},
		{`wp comment WP-1 "it's done"`, []string{`Bash(wp comment WP-1 "it's done")`, "Bash(wp comment *)"}},
		// Risky programs: exact only.
		{"rm -rf build", []string{"Bash(rm -rf build)"}},
		{"curl -s https://example.com", []string{"Bash(curl -s https://example.com)"}},
		{"go test ./internal/core", []string{"Bash(go test ./internal/core)"}},
		{"git push origin main", []string{"Bash(git push origin main)"}},
		{"git status --short src/a.go", []string{"Bash(git status --short src/a.go)", "Bash(git status --short *)"}},
		// Audit 940.1: fetch and ls-remote run --upload-pack and ext::
		// commands, grep -O runs a pager command, and log, show, diff,
		// blame, shortlog and rev-list write any file with --output.
		{"git show HEAD~1", []string{"Bash(git show HEAD~1)"}},
		{"git fetch https://example.com/x", []string{"Bash(git fetch https://example.com/x)"}},
		{"git ls-remote origin", []string{"Bash(git ls-remote origin)"}},
		{"git grep -n foo", []string{"Bash(git grep -n foo)"}},
		// Audit 940.2: these run a command named in their arguments.
		{"vim -c '!id' notes.txt", []string{"Bash(vim -c '!id' notes.txt)"}},
		{"expect -c 'spawn id'", []string{"Bash(expect -c 'spawn id')"}},
		{"script -c 'id' out.log", []string{"Bash(script -c 'id' out.log)"}},
		{"man -P 'id' ls", []string{"Bash(man -P 'id' ls)"}},
		{"tar xf a.tar", []string{"Bash(tar xf a.tar)"}},
		{"gdb -batch -ex 'shell id' a.out", []string{"Bash(gdb -batch -ex 'shell id' a.out)"}},
		{"sqlite3 -cmd '.shell id' a.db", []string{"Bash(sqlite3 -cmd '.shell id' a.db)"}},
		{"rg --pre ./pre.sh pattern", []string{"Bash(rg --pre ./pre.sh pattern)"}},
		{"tmux new-session 'id; sh'", []string{"Bash(tmux new-session 'id; sh')"}},
		// Nothing a rule can be read against.
		{"echo $HOME", nil},
		{"wp ls | grep x", nil},
		{"make a && make b", nil},
		{"cat x > y", nil},
		{`echo hi >"out.txt"`, nil},
		{`echo hi >'out.txt'`, nil},
		{`echo hi 2>"err.txt"`, nil},
		{`cd x && echo hi >"out.txt"`, nil},
		{`echo hi "#" '>'`, []string{`Bash(echo hi "#" '>')`, "Bash(echo hi *)"}},
		{`echo hi #"note"`, nil},
		{"sleep 10 &", nil},
		{"(cd x; ls)", nil},
		{"./run.sh", nil},
		{"FOO=1 wp ls", nil},
		{"ls *.go", nil},
		{"echo `id`", nil},
		{"echo hi\nrm x", nil},
		{`echo "a $b"`, nil},
		{"echo 'open", nil},
		{"", nil},
	}
	for _, c := range cases {
		got := ruleList(bashRules(c.cmd))
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%q: %q, want %q", c.cmd, got, c.want)
		}
	}
	// Every rule offered passes the check a person's rule goes through.
	for _, c := range cases {
		for _, r := range bashRules(c.cmd) {
			if err := checkPermissionRule("Bash", r.Rule); err != nil {
				t.Errorf("offered %s fails its own check: %v", r.Rule, err)
			}
		}
	}
}

func TestPermissionRulesOtherTools(t *testing.T) {
	got := permissionRules("WebFetch", json.RawMessage(`{"url":"https://Docs.Example.com/a?b=1","prompt":"x"}`))
	if len(got) != 1 || got[0].Rule != "WebFetch(domain:docs.example.com)" {
		t.Fatalf("webfetch: %+v", got)
	}
	if got := permissionRules("mcp__github__create_issue", json.RawMessage(`{}`)); len(got) != 1 || got[0].Rule != "mcp__github__create_issue" {
		t.Fatalf("mcp: %+v", got)
	}
	for _, tool := range []string{"Write", "Edit", "Agent", "mcp__x"} {
		if got := permissionRules(tool, json.RawMessage(`{"file_path":"/x"}`)); got != nil {
			t.Fatalf("%s: %+v", tool, got)
		}
	}
	if got := permissionRules("WebFetch", json.RawMessage(`{"url":"http://localhost:8080/"}`)); got != nil {
		t.Fatalf("a bare host: %+v", got)
	}
	// Audit 940.3: an address is a machine, not a site: the cloud metadata
	// endpoint or a loopback service.
	for _, u := range []string{"http://169.254.169.254/latest/meta-data/", "http://127.0.0.1:8080/", "http://127.1/", "http://[::1]/"} {
		if got := permissionRules("WebFetch", json.RawMessage(`{"url":"`+u+`"}`)); got != nil {
			t.Errorf("an address %s: %+v", u, got)
		}
	}
}

func TestCheckPermissionRule(t *testing.T) {
	ok := []struct{ tool, rule string }{
		{"Bash", "Bash(wp request show *)"},
		{"Bash", "Bash(wp request ls:*)"},
		{"Bash", "Bash(rm -rf build)"},
		{"Bash", "Bash(git status *)"},
		{"Bash", "Bash(go test ./... 2>&1)"}, // the exact rule bashRules offers keeps the agent's 2>&1
		{"Bash", "Bash(make test#1)"},
		{"WebFetch", "WebFetch(domain:example.com)"},
		{"mcp__github__get_issue", "mcp__github__get_issue"},
	}
	for _, c := range ok {
		if err := checkPermissionRule(c.tool, c.rule); err != nil {
			t.Errorf("%s %s: %v", c.tool, c.rule, err)
		}
	}
	bad := []struct{ tool, rule string }{
		{"Bash", ""},
		{"Bash", "Bash(*)"},
		{"Bash", "Bash(wp *)"},
		{"Bash", "Bash(python3 *)"},
		{"Bash", "Bash(git push *)"},
		{"Bash", "Bash(rm -rf *)"},
		{"Bash", "Bash(wp * show)"},
		// Audit 937.2: what bashRules refuses, a typed rule can't add either.
		{"Bash", "Bash(cat /etc/passwd > /tmp/out *)"},
		{"Bash", "Bash(cat x >> y)"},
		{"Bash", "Bash(make test # *)"},
		{"Bash", "Bash(wp ls*)"}, // matches "wp lsx" too: a wildcard follows a space or :
		{"Bash", "Bash(go test 2>&1 *)"},
		// Audit 940.1-940.3.
		{"Bash", "Bash(git fetch *)"},
		{"Bash", "Bash(git ls-remote *)"},
		{"Bash", "Bash(git log *)"},
		{"Bash", "Bash(git grep *)"},
		{"Bash", "Bash(vim -c *)"},
		{"Bash", "Bash(expect -c *)"},
		{"Bash", "Bash(tar xf *)"},
		{"WebFetch", "WebFetch(domain:169.254.169.254)"},
		{"WebFetch", "WebFetch(domain:127.1)"},
		{"Bash", "Bash(wp ls && rm x)"},
		{"Bash", "Bash(echo $X)"},
		{"Bash", "Bash(/usr/bin/ls)"},
		{"Bash", "WebFetch(domain:example.com)"},
		{"Bash", "Bash(ls)\nBash(rm x)"},
		{"Bash", "Bash(" + strings.Repeat("a", maxRuleLen) + ")"},
		{"WebFetch", "WebFetch(domain:*.example.com)"},
		{"WebFetch", "WebFetch(https://example.com)"},
		{"Write", "Write(/etc/**)"},
		{"mcp__github__get_issue", "mcp__github"},
	}
	for _, c := range bad {
		if err := checkPermissionRule(c.tool, c.rule); !IsInvalid(err) {
			t.Errorf("%s %q passed: %v", c.tool, c.rule, err)
		}
	}
}

func TestAddAllowRule(t *testing.T) {
	const rule = "Bash(wp ls *)"
	cases := []struct{ name, in, want string }{
		{"new file", "", "{\n  \"permissions\": {\n    \"allow\": [\n      \"Bash(wp ls *)\"\n    ]\n  }\n}\n"},
		{"multi-line list", "{\n  \"permissions\": {\n    \"allow\": [\n      \"Bash(make)\"\n    ],\n    \"deny\": []\n  },\n  \"model\": \"x\"\n}\n",
			"{\n  \"permissions\": {\n    \"allow\": [\n      \"Bash(make)\",\n      \"Bash(wp ls *)\"\n    ],\n    \"deny\": []\n  },\n  \"model\": \"x\"\n}\n"},
		{"inline list", `{"permissions":{"allow":["Bash(make)"]}}`, `{"permissions":{"allow":["Bash(make)", "Bash(wp ls *)"]}}`},
		{"empty list", "{\n  \"permissions\": {\n    \"allow\": []\n  }\n}\n", "{\n  \"permissions\": {\n    \"allow\": [\n      \"Bash(wp ls *)\"\n    ]\n  }\n}\n"},
		{"permissions without allow", "{\n  \"permissions\": {\n    \"deny\": [\"Bash(rm *)\"]\n  }\n}\n",
			"{\n  \"permissions\": {\n    \"allow\": [\n      \"Bash(wp ls *)\"\n    ],\n    \"deny\": [\"Bash(rm *)\"]\n  }\n}\n"},
		{"no permissions", "{\n  \"hooks\": {}\n}\n", "{\n  \"permissions\": {\n    \"allow\": [\n      \"Bash(wp ls *)\"\n    ]\n  },\n  \"hooks\": {}\n}\n"},
		{"empty object", "{}\n", "{\n  \"permissions\": {\n    \"allow\": [\n      \"Bash(wp ls *)\"\n    ]\n  }\n}\n"},
		{"tabs", "{\n\t\"model\": \"x\"\n}\n", "{\n\t\"permissions\": {\n\t\t\"allow\": [\n\t\t\t\"Bash(wp ls *)\"\n\t\t]\n\t},\n\t\"model\": \"x\"\n}\n"},
	}
	for _, c := range cases {
		got, err := addAllowRule(c.in, rule)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s:\n%s\nwant:\n%s", c.name, got, c.want)
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(got), &v); err != nil {
			t.Errorf("%s: not JSON: %v", c.name, err)
		}
	}
	if _, err := addAllowRule(`{"permissions":{"allow":["Bash(wp ls *)"]}}`, rule); !errors.Is(err, errRulePresent) {
		t.Fatalf("a rule already there: %v", err)
	}
	for _, in := range []string{"[]", "{", `{"permissions":[]}`, `{"permissions":{"allow":"x"}}`, `{} {}`} {
		if _, err := addAllowRule(in, rule); err == nil {
			t.Errorf("%q took a rule", in)
		}
	}
	// A rule with characters JSON would escape for HTML stays readable.
	if got, _ := addAllowRule("", "Bash(a <b> & c)"); !strings.Contains(got, `"Bash(a <b> & c)"`) {
		t.Fatalf("escaped: %s", got)
	}
}

func TestVersionedRiskyPrograms(t *testing.T) {
	for _, p := range []string{"python3.11", "ruby3.2", "gcc-12", "Python3", "mkfs.ext4", "mkfs.xfs", "x86_64-linux-gnu-gcc",
		"docker-compose", "ts-node", "R", "Rscript", "g++", "clang++", "brew", "aws", "apt-get", "sort", "gzip"} {
		if !isRiskyProgram(p) {
			t.Errorf("%s should be risky", p)
		}
		if err := checkPermissionRule("Bash", "Bash("+p+" -m *)"); err == nil {
			t.Errorf("typed %s rule passed", p)
		}
	}
	for _, p := range []string{"wp", "ls", "golangci-lint", "pre-commit"} {
		if isRiskyProgram(p) {
			t.Errorf("%s is not risky", p)
		}
	}
	// A program that writes a file through an option gets no prefix rule:
	// "sort -r input *" would allow "sort -r input -o ~/.bashrc".
	if got := ruleList(bashRules("sort -r input")); len(got) != 1 {
		t.Errorf("sort: %q", got)
	}
}

// A quoted '>' before & is text, and the & backgrounds what came before:
// the shell runs two commands, so no rule reads it as one.
func TestQuotedRedirectThenBackground(t *testing.T) {
	for _, cmd := range []string{"echo '>'& rm -rf x", `echo ">"& rm -rf x`} {
		if got := bashRules(cmd); got != nil {
			t.Errorf("%s: %q", cmd, ruleList(got))
		}
	}
	if err := checkPermissionRule("Bash", "Bash(echo '>'& rm *)"); !IsInvalid(err) {
		t.Errorf("typed rule with a quoted > then &: %v", err)
	}
	if got := ruleList(bashRules("wp pr show 12 2>&1")); len(got) != 2 {
		t.Errorf("2>&1 is still a redirect: %q", got)
	}
}
