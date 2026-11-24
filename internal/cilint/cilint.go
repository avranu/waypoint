// Package cilint is a deterministic, advisory linter for GitHub/Forgejo
// Actions workflow files. It looks for the shapes of waste that burn slots on
// the shared runner pool: runs nobody cancels, setup nobody caches, gates that
// take a runner turn to echo a value. One rule is about correctness instead:
// a condition on an event field Forgejo never sends is silently wrong. No
// model, no network: a YAML parse and a few rules. A rule that cannot be
// decided reliably is not here.
package cilint

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Finding is one problem: where, which rule, what, and the one-line fix.
type Finding struct {
	Rule string `json:"rule"`
	File string `json:"file"`
	Line int    `json:"line"`
	Job  string `json:"job,omitempty"`
	Msg  string `json:"message"`
	Fix  string `json:"fix"`
}

// Key identifies a finding across edits (line numbers move).
func (f Finding) Key() string { return f.File + "|" + f.Rule + "|" + f.Job + "|" + f.Msg }

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d %s: %s Fix: %s", f.File, f.Line, f.Rule, f.Msg, f.Fix)
}

// Rule ids.
const (
	RuleNoCancel   = "ci-no-cancel-superseded"
	RuleNoCache    = "ci-setup-no-cache"
	RuleToolCache  = "ci-download-uncached"
	RuleSlowOnPR   = "ci-slow-mode-on-pr"
	RuleEmptyJob   = "ci-echo-only-job"
	RuleSmallJobs  = "ci-many-small-jobs"
	RuleHeavyScan  = "ci-heavy-scan-everywhere"
	RuleDockerCach = "ci-docker-no-cache"
	RuleNeedsChain = "ci-needs-chain"
	RuleNoTimeout  = "ci-no-timeout"
	RuleFgEvent    = "ci-forgejo-event-name"
	RuleFgPerms    = "ci-forgejo-permissions"
)

// Rules lists the ids, for the docs and the tests.
var Rules = []string{RuleNoCancel, RuleNoCache, RuleToolCache, RuleSlowOnPR, RuleEmptyJob, RuleSmallJobs, RuleHeavyScan, RuleDockerCach, RuleNeedsChain, RuleNoTimeout, RuleFgEvent, RuleFgPerms}

// IsWorkflowPath says whether a path is a workflow file.
func IsWorkflowPath(p string) bool {
	p = filepath.ToSlash(p)
	ext := filepath.Ext(p)
	if ext != ".yml" && ext != ".yaml" {
		return false
	}
	return strings.Contains(p, ".forgejo/workflows/") || strings.Contains(p, ".github/workflows/")
}

type step struct {
	node *yaml.Node
	uses string
	run  string
	with map[string]string
	cond string
}

type job struct {
	id      string
	node    *yaml.Node
	steps   []step
	needs   []string
	cond    string
	timeout bool
	reuse   bool
	env     bool
}

type workflow struct {
	file     string
	root     *yaml.Node
	events   map[string]*yaml.Node
	onNode   *yaml.Node
	jobs     []*job
	cancelIn bool
}

// Lint checks one workflow. name is only used in the findings. A file that
// does not parse gives no findings (actionlint owns syntax).
func Lint(name string, src []byte) []Finding {
	var doc yaml.Node
	if yaml.Unmarshal(src, &doc) != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	w := parse(name, doc.Content[0])
	var out []Finding
	for _, rule := range []func(*workflow) []Finding{
		ruleNoCancel, ruleNoCache, ruleToolCache, ruleSlowOnPR, ruleEmptyJob, ruleSmallJobs, ruleHeavyScan, ruleDockerCache, ruleNeedsChain, ruleNoTimeout, ruleForgejoEvent, ruleForgejoPerms,
	} {
		out = append(out, rule(w)...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

func mapGet(n *yaml.Node, key string) (k, v *yaml.Node) {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i], n.Content[i+1]
		}
	}
	return nil, nil
}

func scalar(n *yaml.Node) string {
	if n != nil && n.Kind == yaml.ScalarNode {
		return n.Value
	}
	return ""
}

func parse(file string, root *yaml.Node) *workflow {
	w := &workflow{file: file, root: root, events: map[string]*yaml.Node{}}
	if k, v := mapGet(root, "on"); k != nil {
		w.onNode = k
		switch v.Kind {
		case yaml.ScalarNode:
			w.events[v.Value] = nil
		case yaml.SequenceNode:
			for _, e := range v.Content {
				w.events[e.Value] = nil
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(v.Content); i += 2 {
				w.events[v.Content[i].Value] = v.Content[i+1]
			}
		}
	}
	if _, v := mapGet(root, "concurrency"); hasCancel(v) {
		w.cancelIn = true
	}
	if _, jobs := mapGet(root, "jobs"); jobs != nil && jobs.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(jobs.Content); i += 2 {
			w.jobs = append(w.jobs, parseJob(w, jobs.Content[i].Value, jobs.Content[i+1]))
		}
	}
	return w
}

func parseJob(w *workflow, id string, jn *yaml.Node) *job {
	j := &job{id: id, node: jn}
	if _, c := mapGet(jn, "concurrency"); hasCancel(c) {
		w.cancelIn = true
	}
	_, cond := mapGet(jn, "if")
	j.cond = scalar(cond)
	j.timeout = has(jn, "timeout-minutes")
	j.reuse = has(jn, "uses")
	j.env = has(jn, "environment")
	if _, nd := mapGet(jn, "needs"); nd != nil {
		if nd.Kind == yaml.ScalarNode {
			j.needs = []string{nd.Value}
		} else {
			for _, e := range nd.Content {
				j.needs = append(j.needs, e.Value)
			}
		}
	}
	if _, st := mapGet(jn, "steps"); st != nil && st.Kind == yaml.SequenceNode {
		for _, sn := range st.Content {
			s := step{node: sn, with: map[string]string{}}
			_, u := mapGet(sn, "uses")
			s.uses = scalar(u)
			_, r := mapGet(sn, "run")
			s.run = scalar(r)
			_, ic := mapGet(sn, "if")
			s.cond = scalar(ic)
			if _, wn := mapGet(sn, "with"); wn != nil && wn.Kind == yaml.MappingNode {
				for x := 0; x+1 < len(wn.Content); x += 2 {
					s.with[wn.Content[x].Value] = wn.Content[x+1].Value
				}
			}
			j.steps = append(j.steps, s)
		}
	}
	return j
}

func has(n *yaml.Node, key string) bool { k, _ := mapGet(n, key); return k != nil }

func hasCancel(n *yaml.Node) bool {
	k, v := mapGet(n, "cancel-in-progress")
	return k != nil && v != nil && v.Value != "false"
}

// action is the action's name without its version: "actions/setup-go".
func action(uses string) string {
	a, _, _ := strings.Cut(uses, "@")
	return strings.ToLower(a)
}

func (w *workflow) onPR() bool {
	_, a := w.events["pull_request"]
	_, b := w.events["pull_request_target"]
	return a || b
}

// pushesBeyondMain says whether push runs on more than main.
func (w *workflow) pushesBeyondMain() bool {
	cfg, ok := w.events["push"]
	if !ok {
		return false
	}
	_, b := mapGet(cfg, "branches")
	if b == nil {
		return true
	}
	for _, e := range b.Content {
		if e.Value != "main" && e.Value != "master" {
			return true
		}
	}
	return false
}

// everyRun: the workflow runs on pull requests or on pushes to more than main.
func (w *workflow) everyRun() bool { return w.onPR() || w.pushesBeyondMain() }

var reNarrow = lazyre.New(`refs/heads/(main|master)|github\.ref_name\s*==|github\.event_name\s*[!=]=|github\.ref\s*==|schedule`)

// narrowed: an if: that already ties the job or step to main, an event or a schedule.
func narrowed(conds ...string) bool {
	for _, c := range conds {
		if reNarrow.MatchString(c) {
			return true
		}
	}
	return false
}

func (w *workflow) at(n *yaml.Node, rule, jobID, msg, fix string) Finding {
	line := 1
	if n != nil {
		line = n.Line
	}
	return Finding{Rule: rule, File: w.file, Line: line, Job: jobID, Msg: msg, Fix: fix}
}

func (w *workflow) jobKey(id string) *yaml.Node {
	_, jobs := mapGet(w.root, "jobs")
	k, _ := mapGet(jobs, id)
	return k
}

func (j *job) hasAction(names ...string) bool {
	for _, s := range j.steps {
		if slices.Contains(names, action(s.uses)) {
			return true
		}
	}
	return false
}

// hasLocalGoCache says whether the job uses a local action named for a Go
// cache (uses: ./.forgejo/actions/go-cache): one file can't see inside it,
// and a repository that keys its own Go caches turns setup-go's off on
// purpose. It says nothing about npm or pip caches.
func (j *job) hasLocalGoCache() bool {
	for _, s := range j.steps {
		base := path.Base(s.uses)
		if strings.HasPrefix(s.uses, "./") && strings.Contains(base, "cache") && strings.Contains(base, "go") {
			return true
		}
	}
	return false
}

func ruleNoCancel(w *workflow) []Finding {
	if !w.onPR() || w.cancelIn {
		return nil
	}
	return []Finding{w.at(w.onNode, RuleNoCancel, "", "runs on pull requests but nothing cancels a superseded run, so every push leaves the old run burning a slot.",
		"add `concurrency: {group: ci-${{ github.ref }}, cancel-in-progress: ${{ github.event_name == 'pull_request' }}}`.")}
}

func ruleNoCache(w *workflow) []Finding {
	fixes := map[string]string{"actions/setup-go": "true", "actions/setup-node": "npm", "actions/setup-python": "pip"}
	var out []Finding
	for _, j := range w.jobs {
		hasCacheStep := j.hasAction("actions/cache")
		goCache := hasCacheStep || j.hasLocalGoCache()
		for _, s := range j.steps {
			a := action(s.uses)
			var bad bool
			switch a {
			case "actions/setup-go": // caches by default since v4; only an explicit off is waste
				bad = s.with["cache"] == "false"
			case "actions/setup-node", "actions/setup-python":
				bad = s.with["cache"] == ""
			default:
				continue
			}
			if bad && !hasCacheStep && (a != "actions/setup-go" || !goCache) {
				out = append(out, w.at(s.node, RuleNoCache, j.id, a+" without a dependency cache re-downloads every module on every run.",
					"set `with: {cache: "+fixes[a]+"}`."))
			}
		}
	}
	return out
}

var reToolDownload = lazyre.New(`\bplaywright\s+install\b|\bcypress\s+install\b|\bpuppeteer\s+browsers\s+install\b`)

func ruleToolCache(w *workflow) []Finding {
	var out []Finding
	for _, j := range w.jobs {
		if j.hasAction("actions/cache") {
			continue
		}
		for _, s := range j.steps {
			if reToolDownload.MatchString(s.run) {
				out = append(out, w.at(s.node, RuleToolCache, j.id, "downloads a browser or tool on every run and the job has no actions/cache step for it.",
					"cache ~/.cache/ms-playwright (key on the lockfile) with actions/cache, or bake it into the job image."))
				break
			}
		}
	}
	return out
}

var reRace = lazyre.New(`\bgo\s+test\b[^\n]*\s-(race|fuzz)\b`)

func ruleSlowOnPR(w *workflow) []Finding {
	if !w.onPR() {
		return nil
	}
	var out []Finding
	for _, j := range w.jobs {
		for _, s := range j.steps {
			if reRace.MatchString(s.run) && !narrowed(j.cond, s.cond) && !strings.Contains(s.run, "GITHUB_EVENT_NAME") {
				out = append(out, w.at(s.node, RuleSlowOnPR, j.id, "runs `go test -race` (or -fuzz) on pull requests too; it multiplies test time.",
					"gate the step with `if: github.event_name != 'pull_request'` so it runs on main only."))
			}
		}
	}
	return out
}

var reEchoLine = lazyre.New(`^(echo|printf|true|:|exit 0)\b`)

func echoOnly(run string) bool {
	n := 0
	for _, l := range strings.Split(run, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if !reEchoLine.MatchString(l) {
			return false
		}
		n++
	}
	return n > 0
}

func ruleEmptyJob(w *workflow) []Finding {
	var out []Finding
	for _, j := range w.jobs {
		if j.cond != "" || j.reuse || len(j.steps) == 0 {
			continue
		}
		only := true
		for _, s := range j.steps {
			if s.uses != "" || !echoOnly(s.run) {
				only = false
				break
			}
		}
		if only {
			out = append(out, w.at(w.jobKey(j.id), RuleEmptyJob, j.id, "only echoes or sets outputs and has no job-level `if`, so every event pays a runner turn for nothing.",
				"fold the value into the job that needs it, or add a job-level `if:` (for example `github.event_name == 'push'`)."))
		}
	}
	return out
}

func ruleSmallJobs(w *workflow) []Finding {
	byTool := map[string][]*job{}
	var order []string
	for _, j := range w.jobs {
		if j.reuse || len(j.steps) > 4 || reShip.MatchString(j.id) || !j.hasAction("actions/checkout") || has(j.node, "strategy") {
			continue
		}
		for _, s := range j.steps {
			if a := action(s.uses); strings.HasPrefix(a, "actions/setup-") {
				key := a + "@" + s.with["go-version"] + s.with["node-version"] + s.with["python-version"]
				if _, seen := byTool[key]; !seen {
					order = append(order, key)
				}
				byTool[key] = append(byTool[key], j)
				break
			}
		}
	}
	var out []Finding
	for _, key := range order {
		js := byTool[key]
		if len(js) < 3 {
			continue
		}
		var ids []string
		for _, j := range js {
			ids = append(ids, j.id)
		}
		tool, _, _ := strings.Cut(key, "@")
		out = append(out, w.at(w.jobKey(js[0].id), RuleSmallJobs, js[0].id,
			fmt.Sprintf("%d small jobs (%s) each repeat checkout + %s; every one pays setup and a queue turn.", len(js), strings.Join(ids, ", "), tool),
			"merge them into one job with several steps."))
	}
	return out
}

var reHeavy = lazyre.New(`(?i)\bnuclei\b|\bsqlmap\b|zaproxy|zap-baseline|zap-full|\btrivy\s+image\b|aquasecurity/trivy-action|github/codeql-action/(init|analyze)|\bcodeql\s+(database|analyze)`)
var reFuzz = lazyre.New(`-fuzztime[= ]\d+(m|h)\b`)

func ruleHeavyScan(w *workflow) []Finding {
	if !w.everyRun() {
		return nil
	}
	var out []Finding
	for _, j := range w.jobs {
		if narrowed(j.cond) {
			continue
		}
		for _, s := range j.steps {
			hay := s.uses + "\n" + s.run
			if !narrowed(s.cond) && (reHeavy.MatchString(hay) || reFuzz.MatchString(hay)) {
				out = append(out, w.at(s.node, RuleHeavyScan, j.id, "a heavy scanner (codeql, nuclei, sqlmap, zap, trivy image or a long fuzz) runs on every push or pull request.",
					"move it to a `schedule:` workflow, or add `if: github.ref == 'refs/heads/main'`."))
				break
			}
		}
	}
	return out
}

var reDockerBuild = lazyre.New(`\bdocker\s+(buildx\s+)?build\b`)

func ruleDockerCache(w *workflow) []Finding {
	var out []Finding
	for _, j := range w.jobs {
		for _, s := range j.steps {
			switch {
			case action(s.uses) == "docker/build-push-action":
				if s.with["cache-from"] == "" && s.with["cache-to"] == "" {
					out = append(out, w.at(s.node, RuleDockerCach, j.id, "docker/build-push-action without cache-from/cache-to rebuilds every layer each run.",
						"add `cache-from: type=gha` and `cache-to: type=gha,mode=max` (or a registry cache)."))
				}
			case reDockerBuild.MatchString(s.run):
				if !strings.Contains(s.run, "--cache-from") && !strings.Contains(s.run, "--cache-to") {
					out = append(out, w.at(s.node, RuleDockerCach, j.id, "docker build without --cache-from/--cache-to rebuilds every layer each run.",
						"add `--cache-from type=registry,ref=…` and `--cache-to` (buildx), or reuse a pulled image."))
				}
			}
		}
	}
	return out
}

var reShip = lazyre.New(`(?i)release|deploy|publish|tag|promote`)

func ruleNeedsChain(w *workflow) []Finding {
	var out []Finding
	byID := map[string]*job{}
	for _, j := range w.jobs {
		byID[j.id] = j
	}
	for _, j := range w.jobs {
		if len(j.needs) != 1 || j.env || reShip.MatchString(j.id) || j.hasAction("actions/download-artifact") {
			continue
		}
		dep := j.needs[0]
		if b, ok := byID[dep]; !ok || reShip.MatchString(b.id) {
			continue
		}
		text := nodeText(j.node)
		if strings.Contains(text, "needs."+dep) || strings.Contains(text, "needs['"+dep+"']") || strings.Contains(text, `needs["`+dep+`"]`) {
			continue
		}
		out = append(out, w.at(w.jobKey(j.id), RuleNeedsChain, j.id, fmt.Sprintf("needs %q but uses none of its outputs or artifacts, so it waits for a separate queue turn behind it.", dep),
			"drop the `needs:` so both run at once (keep it only when this job must not start after a failure)."))
	}
	return out
}

// nodeText flattens every scalar in a node, so a needs.X reference anywhere counts.
func nodeText(n *yaml.Node) string {
	var b strings.Builder
	walkScalars(n, func(x *yaml.Node) {
		b.WriteString(x.Value)
		b.WriteByte('\n')
	})
	return b.String()
}

// walkScalars calls fn on every scalar under n, in document order.
func walkScalars(n *yaml.Node, fn func(*yaml.Node)) {
	if n.Kind == yaml.ScalarNode {
		fn(n)
	}
	for _, c := range n.Content {
		walkScalars(c, fn)
	}
}

func ruleNoTimeout(w *workflow) []Finding {
	var out []Finding
	for _, j := range w.jobs {
		if j.timeout || j.reuse || len(j.steps) < 3 {
			continue
		}
		out = append(out, w.at(w.jobKey(j.id), RuleNoTimeout, j.id, "no timeout-minutes; a hung job holds a runner slot until the runner's 3h limit.",
			"add `timeout-minutes:` at about twice the job's normal duration."))
	}
	return out
}

// fgAction says what Forgejo's Actions payload has in event.action for the
// activity GitHub calls by the key. `on.<event>.types` takes GitHub's names
// (Forgejo translates them when matching), but the payload keeps its own, so
// an == with the GitHub name never holds and a != always does
// (modules/actions/workflows.go matchPullRequestEvent; seen on run 6044,
// WP-536).
var fgAction = map[string]string{"labeled": "label_updated", "unlabeled": "label_cleared", "synchronize": "synchronized"}

const (
	reFgContext = `(?:github|gitea|forge(?:jo)?)\.event\.`
	reEventCtx  = reFgContext + `action`
)

var (
	reFgAction = lazyre.New(reEventCtx + `\s*([!=]=)\s*['"](labeled|unlabeled|synchronize)['"]|['"](labeled|unlabeled|synchronize)['"]\s*([!=]=)\s*` + reEventCtx)
	// Forgejo's Actions payload for an edit carries no changes (WP-174), though
	// the webhook for the same edit does.
	reFgChanges = lazyre.New(reFgContext + `changes\.title\b`)
)

// ruleForgejoEvent finds conditions on event fields Forgejo doesn't send. Each
// name and operator is reported once per file, at its first use: one fix
// covers them all. A file under .github/workflows may run on GitHub, where
// the names are right.
func ruleForgejoEvent(w *workflow) []Finding {
	if strings.Contains(w.file, ".github/workflows/") {
		return nil
	}
	var out []Finding
	seen := map[string]bool{}
	report := func(n *yaml.Node, key, msg, fix string) {
		if !seen[key] {
			seen[key] = true
			out = append(out, w.at(n, RuleFgEvent, "", msg, fix))
		}
	}
	walkScalars(w.root, func(n *yaml.Node) {
		for _, m := range reFgAction.FindAllStringSubmatch(n.Value, -1) {
			op, name := m[1]+m[4], m[2]+m[3]
			holds := "never holds"
			if op == "!=" {
				holds = "always holds"
			}
			report(n, name+op, "compares event.action with '"+name+"', which Forgejo never sends there: it says '"+fgAction[name]+"', so the "+op+" "+holds+".",
				"compare with '"+fgAction[name]+"'.")
		}
		if reFgChanges.MatchString(n.Value) {
			report(n, "changes", "reads event.changes.title, which Forgejo's Actions payload for an edit doesn't have, so it is always empty.",
				"a run can't see the old title; go by the current one, `github.event.pull_request.title` (`github.event.issue.title` on an issue event).")
		}
	})
	return out
}

// ruleForgejoPerms: Forgejo ignores `permissions:` at the top and on a job,
// and warns on every run of every job that has one (WP-289). It grants and
// limits nothing, so a block there only reads as if the token were scoped.
func ruleForgejoPerms(w *workflow) []Finding {
	if strings.Contains(w.file, ".github/workflows/") {
		return nil
	}
	const msg = "sets permissions:, which Forgejo ignores and warns about on every run; it does not scope the job token."
	const fix = "remove the block; on Forgejo a job that needs more than its token gives uses a token in a secret."
	var out []Finding
	if k, _ := mapGet(w.root, "permissions"); k != nil {
		out = append(out, w.at(k, RuleFgPerms, "", "the workflow "+msg, fix))
	}
	for _, j := range w.jobs {
		if k, _ := mapGet(j.node, "permissions"); k != nil {
			out = append(out, w.at(k, RuleFgPerms, j.id, "job "+j.id+" "+msg, fix))
		}
	}
	return out
}
