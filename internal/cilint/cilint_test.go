package cilint

import (
	"slices"
	"strings"
	"testing"
)

func rules(src string) []string {
	var ids []string
	for _, f := range Lint("w.yml", []byte(src)) {
		ids = append(ids, f.Rule)
	}
	return ids
}

// hdr is a clean header; each case alters one thing below it.
const hdr = "on:\n  pull_request:\nconcurrency:\n  group: g\n  cancel-in-progress: true\n"

func wfJob(extra string) string {
	return hdr + "jobs:\n  a:\n    runs-on: x\n    timeout-minutes: 10\n" + extra
}

func TestRules(t *testing.T) {
	steps3 := "    steps:\n      - uses: actions/checkout@v4\n      - run: make\n      - run: make test\n"
	small := func(n int, extra string) string {
		s := hdr + "jobs:\n"
		for _, id := range []string{"j1", "j2", "j3", "j4"}[:n] {
			s += "  " + id + ":\n    runs-on: x\n    timeout-minutes: 1\n    steps:\n      - uses: actions/checkout@v4\n      - uses: actions/setup-go@v5\n" + extra + "      - run: make\n"
		}
		return s
	}
	tests := []struct {
		name, rule, bad, good string
	}{
		{"no cancel", RuleNoCancel,
			"on:\n  pull_request:\njobs:\n  a:\n    runs-on: x\n    timeout-minutes: 1\n    steps:\n      - run: a\n",
			"on:\n  pull_request:\nconcurrency:\n  group: g\n  cancel-in-progress: ${{ github.event_name == 'pull_request' }}\njobs:\n  a:\n    runs-on: x\n    timeout-minutes: 1\n    steps:\n      - run: a\n"},
		{"setup-node no cache", RuleNoCache,
			wfJob("    steps:\n      - uses: actions/setup-node@v4\n        with:\n          node-version: 20\n      - run: a\n      - run: b\n"),
			wfJob("    steps:\n      - uses: actions/setup-node@v4\n        with:\n          cache: npm\n      - run: a\n      - run: b\n")},
		{"setup-go cache off", RuleNoCache,
			wfJob("    steps:\n      - uses: actions/setup-go@v5\n        with:\n          cache: false\n      - run: a\n      - run: b\n"),
			wfJob("    steps:\n      - uses: actions/setup-go@v5\n      - run: a\n      - run: b\n")},
		{"setup-go cache off for a local cache action", RuleNoCache,
			wfJob("    steps:\n      - uses: actions/setup-go@v5\n        with:\n          cache: false\n      - uses: ./.forgejo/actions/go-mod\n      - run: b\n"),
			wfJob("    steps:\n      - uses: actions/setup-go@v5\n        with:\n          cache: false\n      - uses: ./.forgejo/actions/go-cache\n      - run: b\n")},
		{"setup-node no cache beside a local go cache", RuleNoCache,
			wfJob("    steps:\n      - uses: ./.forgejo/actions/go-cache\n      - uses: actions/setup-node@v4\n      - run: b\n"),
			wfJob("    steps:\n      - uses: ./.forgejo/actions/go-cache\n      - uses: actions/setup-node@v4\n        with:\n          cache: npm\n      - run: b\n")},
		{"playwright", RuleToolCache,
			wfJob("    steps:\n      - run: npx playwright install --with-deps\n      - run: a\n      - run: b\n"),
			wfJob("    steps:\n      - uses: actions/cache@v4\n      - run: npx playwright install --with-deps\n      - run: b\n")},
		{"race on pr", RuleSlowOnPR,
			wfJob("    steps:\n      - run: go test -race ./...\n      - run: a\n      - run: b\n"),
			wfJob("    steps:\n      - run: go test -race ./...\n        if: github.event_name != 'pull_request'\n      - run: a\n      - run: b\n")},
		{"echo-only job", RuleEmptyJob,
			wfJob("    steps:\n      - id: x\n        run: echo \"run=1\" >> $GITHUB_OUTPUT\n"),
			hdr + "jobs:\n  a:\n    if: github.event_name == 'push'\n    runs-on: x\n    steps:\n      - run: echo hi\n"},
		{"many small jobs", RuleSmallJobs, small(3, ""), small(2, "")},
		{"heavy scan", RuleHeavyScan,
			wfJob("    steps:\n      - uses: github/codeql-action/analyze@v3\n      - run: a\n      - run: b\n"),
			wfJob("    if: github.ref == 'refs/heads/main'\n    steps:\n      - uses: github/codeql-action/analyze@v3\n      - run: a\n      - run: b\n")},
		{"long fuzz", RuleHeavyScan,
			wfJob("    steps:\n      - run: go test -fuzz=F -fuzztime=30m ./...\n      - run: a\n      - run: b\n"),
			wfJob("    steps:\n      - run: go test -fuzz=F -fuzztime=10s ./...\n      - run: a\n      - run: b\n")},
		{"docker build", RuleDockerCach,
			wfJob("    steps:\n      - run: docker build -t x .\n      - run: a\n      - run: b\n"),
			wfJob("    steps:\n      - run: docker buildx build --cache-from type=gha -t x .\n      - run: a\n      - run: b\n")},
		{"build-push-action", RuleDockerCach,
			wfJob("    steps:\n      - uses: docker/build-push-action@v6\n      - run: a\n      - run: b\n"),
			wfJob("    steps:\n      - uses: docker/build-push-action@v6\n        with:\n          cache-from: type=gha\n      - run: a\n      - run: b\n")},
		{"needs chain", RuleNeedsChain,
			hdr + "jobs:\n  a:\n    runs-on: x\n    timeout-minutes: 1\n    steps:\n      - run: a\n  b:\n    needs: a\n    runs-on: x\n    timeout-minutes: 1\n    steps:\n      - run: b\n",
			hdr + "jobs:\n  a:\n    runs-on: x\n    timeout-minutes: 1\n    outputs:\n      v: ${{ steps.s.outputs.v }}\n    steps:\n      - run: a\n  b:\n    needs: a\n    runs-on: x\n    timeout-minutes: 1\n    steps:\n      - run: echo ${{ needs.a.outputs.v }}\n"},
		{"no timeout", RuleNoTimeout,
			hdr + "jobs:\n  a:\n    runs-on: x\n" + steps3,
			hdr + "jobs:\n  a:\n    runs-on: x\n    timeout-minutes: 30\n" + steps3},
		{"forgejo event name", RuleFgEvent,
			wfJob("    if: github.event.action == 'labeled'\n    steps:\n      - run: a\n"),
			wfJob("    if: github.event.label && github.event.action == 'edited'\n    steps:\n      - run: a\n")},
		{"forgejo permissions", RuleFgPerms,
			wfJob("    permissions:\n      contents: write\n    steps:\n      - run: a\n"),
			wfJob("    steps:\n      - run: a\n")},
	}
	covered := map[string]bool{}
	for _, tc := range tests {
		covered[tc.rule] = true
		t.Run(tc.name, func(t *testing.T) {
			if got := rules(tc.bad); !slices.Contains(got, tc.rule) {
				t.Errorf("bad snippet: want %s, got %v", tc.rule, got)
			}
			if got := rules(tc.good); slices.Contains(got, tc.rule) {
				t.Errorf("good snippet: %s raised, got %v", tc.rule, got)
			}
		})
	}
	for _, r := range Rules {
		if !covered[r] {
			t.Errorf("rule %s has no table case", r)
		}
	}
}

func TestForgejoEventNames(t *testing.T) {
	step := func(cond string) string {
		return "on:\n  pull_request:\n    types: [labeled, synchronize]\njobs:\n  a:\n    if: " + cond + "\n    runs-on: x\n"
	}
	for _, cond := range []string{
		"github.event.action == 'labeled'",
		"github.event.action != 'unlabeled'",
		"${{ !(github.event.action == 'synchronize') }}",
		`forge.event.action == "labeled"`,
		`forgejo.event.action == "labeled"`,
		"${{ 'labeled' == github.event.action }}",
		"github.event.changes.title.from != ''",
		"forgejo.event.changes.title.from != ''",
	} {
		if got := rules(step(cond)); !slices.Contains(got, RuleFgEvent) {
			t.Errorf("%s: not reported, got %v", cond, got)
		}
	}
	for _, cond := range []string{
		"github.event.action == 'label_updated'",
		"github.event.action == 'synchronized'",
		"github.event.action == 'edited'",
		"github.event.changes.ref.from != ''",
	} {
		if got := rules(step(cond)); slices.Contains(got, RuleFgEvent) {
			t.Errorf("%s: reported, got %v", cond, got)
		}
	}
	if got := Lint(".github/workflows/ci.yml", []byte(step("github.event.action == 'labeled'"))); slices.ContainsFunc(got, func(f Finding) bool { return f.Rule == RuleFgEvent }) {
		t.Errorf("a GitHub workflow is right to say labeled: %v", got)
	}
	twice := step("github.event.action == 'labeled'") + "  b:\n    if: github.event.action == 'labeled'\n    runs-on: x\n"
	n := 0
	for _, r := range rules(twice) {
		if r == RuleFgEvent {
			n++
		}
	}
	if n != 1 {
		t.Errorf("want one finding per name per file, got %d", n)
	}
	both := step("github.event.action == 'labeled'") + "  b:\n    if: github.event.action != 'labeled'\n    runs-on: x\n"
	var msgs []string
	for _, f := range Lint(".forgejo/workflows/ci.yml", []byte(both)) {
		if f.Rule == RuleFgEvent {
			msgs = append(msgs, f.Msg)
		}
	}
	if len(msgs) != 2 || !strings.Contains(msgs[0], "== never holds") || !strings.Contains(msgs[1], "!= always holds") {
		t.Errorf("want the == and the != told apart, got %q", msgs)
	}
}

func TestForgejoPermissions(t *testing.T) {
	src := "permissions:\n  contents: read\n" + wfJob("    permissions:\n      contents: write\n    steps:\n      - run: a\n")
	var jobs []string
	for _, f := range Lint(".forgejo/workflows/ci.yml", []byte(src)) {
		if f.Rule == RuleFgPerms {
			jobs = append(jobs, f.Job)
		}
	}
	if !slices.Equal(jobs, []string{"", "a"}) {
		t.Errorf("want the workflow's and job a's blocks, got %q", jobs)
	}
	for _, f := range Lint(".github/workflows/ci.yml", []byte(src)) {
		if f.Rule == RuleFgPerms {
			t.Errorf("GitHub honours permissions:, got %v", f)
		}
	}
}

func TestReleaseAfterTestsIsNotAChain(t *testing.T) {
	src := hdr + "jobs:\n  a:\n    runs-on: x\n    timeout-minutes: 1\n    steps:\n      - run: a\n  release:\n    needs: a\n    runs-on: x\n    timeout-minutes: 1\n    steps:\n      - run: r\n"
	if got := rules(src); slices.Contains(got, RuleNeedsChain) {
		t.Errorf("release after tests is a gate, got %v", got)
	}
}

func TestFindingsCarryLinesAndFixes(t *testing.T) {
	fs := Lint(".forgejo/workflows/ci.yml", []byte("on:\n  pull_request:\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: a\n"))
	if len(fs) == 0 || fs[0].Line != 1 || fs[0].Fix == "" || !strings.HasPrefix(fs[0].String(), ".forgejo/workflows/ci.yml:1 ci-no-cancel-superseded") {
		t.Fatalf("unexpected findings: %+v", fs)
	}
}

func TestGarbageIsSilent(t *testing.T) {
	for _, s := range []string{"", "::: not yaml", "- a\n- b\n", "on: push\n"} {
		if got := Lint("w.yml", []byte(s)); len(got) != 0 {
			t.Errorf("%q: %v", s, got)
		}
	}
}

func TestIsWorkflowPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/r/.forgejo/workflows/ci.yml": true, ".github/workflows/x.yaml": true,
		"/r/.github/workflows/readme.md": false, "/r/ci.yml": false,
	} {
		if IsWorkflowPath(p) != want {
			t.Errorf("%s: want %v", p, want)
		}
	}
}
