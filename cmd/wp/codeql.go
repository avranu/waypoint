package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

var codeqlCmd = &command{
	name: "codeql", group: "Code and CI", args: "scan [DIR]",
	summary: "run a CodeQL scan here, before pushing",
	about: `Runs CodeQL here, in docker, so a fix for a finding of a pull request's
CodeQL check can be confirmed before pushing. Name the image the check
runs (a CodeQL CLI image) with --image or WP_CODEQL_IMAGE.`,
	examples: []string{"wp codeql scan"},
	subs: []*command{
		{name: "scan", args: "[--lang LANG] [--image IMAGE] [--steps N] [--all] [DIR]", summary: "scan DIR (default .) with CodeQL; exit 1 on a blocking finding",
			about: `Scans DIR (default .) with CodeQL in docker, in IMAGE (--image, else
WP_CODEQL_IMAGE; there is no default): an image with the codeql CLI on its
PATH, and Go for Go code. Languages: those of the findings that failed this
branch's pull request CodeQL check (any check with "codeql" in its name,
when its link is a report listing them), else every language present;
--lang overrides. Config is DIR/.forgejo/codeql/codeql-config.yml or a
default; triaged findings (.forgejo/codeql/triaged-findings.json) don't
block. Blocking: level error, or security-severity 7.0 or more.

One line per finding ("severity rule path:line message"), blocking ones
first, each with the first --steps data-flow steps; then a summary. Only the
first 15 non-blocking are listed (--all lists them). --json prints it all.

Go's dependencies are fetched first, then the scan runs offline with the
image's own Go (GOTOOLCHAIN=local).

Exit 0 nothing blocks, 1 something does, 2 no image is named, docker or the
image is missing, or its Go is older than go.mod asks (a check run in the
same image fails the same way), 3 the scan failed. Go takes about 3
minutes, every language about 7.`,
			examples: []string{"wp codeql scan --lang go ."},
			run:      cmdCodeqlScan},
	},
}

// codeqlMinBytes is how much code a language needs to be scanned.
const codeqlMinBytes = 1000

// codeqlTimeout bounds one docker step.
const codeqlTimeout = 45 * time.Minute

// codeqlListed is how many non-blocking findings are listed without --all.
const codeqlListed = 15

// codeqlExtensions are the file extensions of each language scanned.
var codeqlExtensions = []struct {
	lang string
	exts []string
}{
	{"python", []string{"py", "pyi"}},
	{"javascript-typescript", []string{"js", "jsx", "mjs", "cjs", "ts", "tsx", "mts", "cts", "vue"}},
	{"go", []string{"go"}},
}

// codeqlForeignDirs hold other people's code; their files don't count.
var codeqlForeignDirs = []string{"node_modules", "vendor"}

// codeqlWorkflowDirs are where workflows are looked for; CodeQL's actions
// extractor reads only the first.
var codeqlWorkflowDirs = []string{".github/workflows", ".forgejo/workflows", ".gitea/workflows"}

const (
	codeqlRepoConfig = ".forgejo/codeql/codeql-config.yml"
	codeqlTriageName = "codeql-triaged.json"
)

var codeqlTriageFiles = []string{".forgejo/codeql/triaged-findings.json", ".github/codeql/triaged-findings.json"}

// codeqlDefaultConfig is the config for a repository without its own.
const codeqlDefaultConfig = `# CodeQL config for a repository without its own .forgejo/codeql/codeql-config.yml.
# Queries are each language's default code-scanning suite.
name: "Forgejo default CodeQL config"

paths-ignore:
  - "**/node_modules/**"
  - "**/bower_components/**"
  - "**/vendor/**"
  - "**/third_party/**"
  - "**/dist/**"
  - "**/build/**"
  - "**/target/**"
  - "**/.venv/**"
  - "**/.venv_windows/**"
  - "**/venv/**"
  - "**/site-packages/**"
  - "**/__pycache__/**"
  - "**/.mypy_cache/**"
  - "**/.pytest_cache/**"
  - "**/.hypothesis/**"
  - "**/migrations/**"
  - "**/*.min.js"
  - "**/static/vendor/**"
`

// treeFile is one file of the tree being scanned.
type treeFile struct {
	Path string // slash-separated, relative to the tree
	Size int64
}

// codeqlLanguages is the languages to scan for these files, and why others
// are not ("python: 300 bytes, under 1000"). actions comes last.
func codeqlLanguages(files []treeFile) (langs, skipped []string) {
	sizes := map[string]int64{}
	workflows := false
	for _, f := range files {
		parts := strings.Split(f.Path, "/")
		if slices.ContainsFunc(parts[:len(parts)-1], func(d string) bool { return slices.Contains(codeqlForeignDirs, d) }) {
			continue
		}
		if dir, name := path.Split(f.Path); slices.Contains(codeqlWorkflowDirs, strings.TrimSuffix(dir, "/")) && (strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")) {
			workflows = true
		}
		ext := strings.TrimPrefix(strings.ToLower(path.Ext(f.Path)), ".")
		for _, e := range codeqlExtensions {
			if slices.Contains(e.exts, ext) {
				sizes[e.lang] += f.Size
			}
		}
	}
	for _, e := range codeqlExtensions {
		switch n, ok := sizes[e.lang]; {
		case !ok:
		case n < codeqlMinBytes:
			skipped = append(skipped, fmt.Sprintf("%s: %d bytes, under %d", e.lang, n, codeqlMinBytes))
		default:
			langs = append(langs, e.lang)
		}
	}
	slices.Sort(langs)
	if workflows {
		langs = append(langs, "actions")
	}
	return langs, skipped
}

// codeqlConfig is the config to scan with: the repository's when it has one.
func codeqlConfig(dir string) (text string, repo bool) {
	if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(codeqlRepoConfig))); err == nil { // #nosec G304 -- the tree being scanned
		return string(b), true
	}
	return codeqlDefaultConfig, false
}

// codeqlStep is one step of a finding's data-flow path.
type codeqlStep struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Message string `json:"message,omitempty"`
}

// codeqlFinding is one CodeQL result.
type codeqlFinding struct {
	// Key is stable while the finding stays: rule, file and CodeQL's line hash
	// (the key the triage list is matched on).
	Key      string       `json:"key"`
	Rule     string       `json:"rule"`
	Level    string       `json:"level"`
	Severity string       `json:"severity"` // critical, high, medium, low, or the level
	Score    float64      `json:"security_severity,omitempty"`
	Path     string       `json:"path"`
	Line     int          `json:"line"`
	Message  string       `json:"message"`
	Blocking bool         `json:"blocking"` // level error, or security severity high or critical
	Triaged  bool         `json:"triaged"`
	Flow     []codeqlStep `json:"flow,omitempty"`
}

// blocks is whether the finding fails the check: blocking and not triaged.
func (f codeqlFinding) blocks() bool { return f.Blocking && !f.Triaged }

// codeqlKey is a finding's key: the first 16 hex digits of
// sha256("rule|path|fingerprint").
func codeqlKey(rule, path, fingerprint string) string {
	sum := sha256.Sum256([]byte(rule + "|" + path + "|" + fingerprint))
	return hex.EncodeToString(sum[:])[:16]
}

func codeqlSeverityName(score float64, level string) string {
	switch {
	case score >= 9:
		return "critical"
	case score >= 7:
		return "high"
	case score >= 4:
		return "medium"
	case score > 0:
		return "low"
	}
	return level
}

type sarifRule struct {
	ID                   string `json:"id"`
	DefaultConfiguration struct {
		Level string `json:"level"`
	} `json:"defaultConfiguration"`
	Properties struct {
		SecuritySeverity json.RawMessage `json:"security-severity"`
	} `json:"properties"`
}

type sarifLocation struct {
	PhysicalLocation struct {
		ArtifactLocation struct {
			URI string `json:"uri"`
		} `json:"artifactLocation"`
		Region struct {
			StartLine int `json:"startLine"`
		} `json:"region"`
	} `json:"physicalLocation"`
	Message struct {
		Text string `json:"text"`
	} `json:"message"`
}

type sarifResult struct {
	RuleID string `json:"ruleId"`
	Rule   struct {
		ID string `json:"id"`
	} `json:"rule"`
	Level   string `json:"level"`
	Message struct {
		Text string `json:"text"`
	} `json:"message"`
	Locations           []sarifLocation   `json:"locations"`
	PartialFingerprints map[string]string `json:"partialFingerprints"`
	Suppressions        []json.RawMessage `json:"suppressions"`
	CodeFlows           []struct {
		ThreadFlows []struct {
			Locations []struct {
				Location sarifLocation `json:"location"`
			} `json:"locations"`
		} `json:"threadFlows"`
	} `json:"codeFlows"`
}

// parseSarif reads CodeQL's SARIF into findings:
// suppressed results are skipped, the level falls back to the rule's, and the
// security severity is the rule's property (a number or a string).
func parseSarif(data []byte) ([]codeqlFinding, error) { return parseSarifPaths(data, nil) }

// parseSarifPaths is parseSarif with each result path passed through repoPath
// (nil leaves them), so the key and the triage match see the repository's path.
func parseSarifPaths(data []byte, repoPath func(string) string) ([]codeqlFinding, error) {
	if repoPath == nil {
		repoPath = func(p string) string { return p }
	}
	var doc struct {
		Runs []struct {
			Tool struct {
				Driver     struct{ Rules []sarifRule }   `json:"driver"`
				Extensions []struct{ Rules []sarifRule } `json:"extensions"`
			} `json:"tool"`
			Results []sarifResult `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("SARIF is not JSON: %w", err)
	}
	var out []codeqlFinding
	for _, run := range doc.Runs {
		rules := map[string]sarifRule{}
		lists := [][]sarifRule{run.Tool.Driver.Rules}
		for _, e := range run.Tool.Extensions {
			lists = append(lists, e.Rules)
		}
		for _, l := range lists {
			for _, r := range l {
				if _, ok := rules[r.ID]; !ok && r.ID != "" {
					rules[r.ID] = r
				}
			}
		}
		for _, res := range run.Results {
			if len(res.Suppressions) > 0 {
				continue
			}
			id := codeqlFirst(res.RuleID, res.Rule.ID, "?")
			rule := rules[id]
			level := codeqlFirst(res.Level, rule.DefaultConfiguration.Level, "warning")
			score := 0.0
			if raw := bytes.Trim(rule.Properties.SecuritySeverity, `"`); len(raw) > 0 {
				score, _ = strconv.ParseFloat(string(raw), 64)
			}
			f := codeqlFinding{Rule: id, Level: level, Score: score, Severity: codeqlSeverityName(score, level), Blocking: level == "error" || score >= 7}
			if len(res.Locations) > 0 {
				f.Path = repoPath(res.Locations[0].PhysicalLocation.ArtifactLocation.URI)
				f.Line = res.Locations[0].PhysicalLocation.Region.StartLine
			}
			f.Message, _, _ = strings.Cut(strings.TrimSpace(res.Message.Text), "\n")
			if r := []rune(f.Message); len(r) > 200 {
				f.Message = string(r[:200])
			}
			fp := res.PartialFingerprints["primaryLocationLineHash"]
			if fp == "" {
				fp = f.Message
			}
			f.Key = codeqlKey(id, f.Path, fp)
			if len(res.CodeFlows) > 0 && len(res.CodeFlows[0].ThreadFlows) > 0 {
				for _, l := range res.CodeFlows[0].ThreadFlows[0].Locations {
					msg, _, _ := strings.Cut(strings.TrimSpace(l.Location.Message.Text), "\n")
					f.Flow = append(f.Flow, codeqlStep{Path: repoPath(l.Location.PhysicalLocation.ArtifactLocation.URI), Line: l.Location.PhysicalLocation.Region.StartLine, Message: msg})
				}
			}
			out = append(out, f)
		}
	}
	return out, nil
}

// codeqlTriaged is the keys the triage lists name. An entry without a rule,
// path, fingerprint and reason counts for nothing, and a file that isn't a
// list is skipped.
func codeqlTriaged(texts [][]byte) map[string]bool {
	keys := map[string]bool{}
	for _, text := range texts {
		var entries []map[string]any
		if json.Unmarshal(text, &entries) != nil {
			continue
		}
		for _, e := range entries {
			field := func(name string) string { s, _ := e[name].(string); return strings.TrimSpace(s) }
			if rule, p, fp := field("rule"), field("path"), field("fingerprint"); rule != "" && p != "" && fp != "" && field("reason") != "" {
				keys[codeqlKey(rule, p, fp)] = true
			}
		}
	}
	return keys
}

// isCodeqlTriageFile is whether the tree path is a triage list.
func isCodeqlTriageFile(p string) bool {
	return slices.Contains(codeqlTriageFiles, p) || (path.Base(p) == codeqlTriageName && !slices.Contains(strings.Split(p, "/"), "node_modules"))
}

// markTriaged sets Triaged on the findings the keys name, and orders them:
// those that block first, then by severity, file and line.
func markTriaged(findings []codeqlFinding, triaged map[string]bool) {
	for i := range findings {
		findings[i].Triaged = triaged[findings[i].Key]
	}
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		switch {
		case a.blocks() != b.blocks():
			return a.blocks()
		case a.Blocking != b.Blocking:
			return a.Blocking
		case a.Score != b.Score:
			return a.Score > b.Score
		case a.Path != b.Path:
			return a.Path < b.Path
		}
		return a.Line < b.Line
	})
}

// codeqlUnavailable is docker or the image missing (exit 2).
type codeqlUnavailable struct{ msg string }

func (e codeqlUnavailable) Error() string { return e.msg }

// errCodeqlNoImage is a scan with no image named: there is no default, since
// the image is whatever the repository's CodeQL check runs.
var errCodeqlNoImage = codeqlUnavailable{"no CodeQL image named: pass --image IMAGE or set WP_CODEQL_IMAGE to a docker image with the codeql CLI " +
	"(the one your CodeQL check runs, so the scan matches it)"}

// codeqlDocker is the docker CLI (tests use a fake).
type codeqlDocker interface {
	// HasImage is nil when docker works and has the image, else a
	// codeqlUnavailable saying which is missing.
	HasImage(ctx context.Context, image string) error
	// Run runs `docker args…`, writing its output to log; the exit code.
	Run(ctx context.Context, log io.Writer, args ...string) (int, error)
}

type dockerCLI struct{}

// codeqlDockerClient is the docker the scan uses (tests replace it).
var codeqlDockerClient codeqlDocker = dockerCLI{}

// checkCodeqlImage refuses an image name docker would read as an option (a
// leading dash: --privileged) or that holds a space or control character.
// Image names never start with a dash, so no real image is refused.
func checkCodeqlImage(image string) error {
	bad := strings.HasPrefix(image, "-")
	for _, r := range image {
		bad = bad || unicode.IsControl(r) || unicode.IsSpace(r)
	}
	if bad {
		return codeqlUnavailable{fmt.Sprintf("%q is not a docker image name (no leading dash, spaces or control characters); name the CodeQL image with --image or WP_CODEQL_IMAGE", image)}
	}
	return nil
}

func (dockerCLI) HasImage(ctx context.Context, image string) error {
	if err := checkCodeqlImage(image); err != nil {
		return err
	}
	bin, err := exec.LookPath("docker")
	if err != nil {
		return codeqlUnavailable{"docker is not installed (no docker on PATH)"}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "image", "inspect", "--format", "{{.Id}}", image).CombinedOutput() // #nosec G204 -- docker with fixed arguments; the image is the caller's
	if err == nil {
		return nil
	}
	if strings.Contains(string(out), "No such image") {
		return codeqlUnavailable{fmt.Sprintf("docker has no image %q; pull or build it, or name another with --image or WP_CODEQL_IMAGE", image)}
	}
	return codeqlUnavailable{"docker is not usable: " + cmpStr(lastLine(string(out)), err.Error())}
}

func (dockerCLI) Run(ctx context.Context, log io.Writer, args ...string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, codeqlTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...) // #nosec G204 -- docker run with arguments built here
	cmd.Stdout, cmd.Stderr = log, log
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case ctx.Err() != nil:
		return -1, fmt.Errorf("ran past %s", codeqlTimeout)
	case errors.As(err, &ee):
		return ee.ExitCode(), nil
	}
	return -1, err
}

// codeqlScript is the shell command that makes one language's database and
// analyzes it, inside the container.
func codeqlScript(lang string) string {
	mode := "--build-mode=none"
	if lang == "go" {
		mode = "--build-mode=manual --command='go build ./...'"
	}
	return fmt.Sprintf("codeql database create /work/db/%[1]s --language=%[1]s %[2]s --source-root=/work/src --codescanning-config=/work/cfg.yml --overwrite --threads=0", lang, mode) +
		fmt.Sprintf(" && codeql database analyze /work/db/%[1]s --format=sarif-latest --output=/work/out/%[1]s.sarif --sarif-category=/language:%[1]s --threads=0", lang)
}

// codeqlNoSource is `codeql database create`'s exit code when none of the
// language's code was left after the config's paths.
const codeqlNoSource = 32

// codeqlDockerArgs is the docker command line for one language: work holds
// cfg.yml, db and out; home is the persistent HOME (caches); src is mounted
// read-only. The scan has no network and builds Go with the image's own
// toolchain from the module cache codeqlFetchArgs filled (WP-888): with a
// network, Go downloads a newer toolchain than the image has, and a scan
// passes here that fails in a check that runs offline.
func codeqlDockerArgs(image, work, home, src string, uid, gid int, lang string) []string {
	return codeqlRun(image, work, home, src, uid, gid, true, "GOPROXY=off", codeqlScript(lang))
}

// codeqlFetchArgs is the networked step before a Go scan: the module's
// dependencies into the cache, with the image's own Go.
func codeqlFetchArgs(image, work, home, src string, uid, gid int) []string {
	return codeqlRun(image, work, home, src, uid, gid, false, "GOPROXY=https://proxy.golang.org", "go mod download")
}

func codeqlRun(image, work, home, src string, uid, gid int, offline bool, proxy, script string) []string {
	args := []string{"run", "--rm", "--user", fmt.Sprintf("%d:%d", uid, gid)}
	if offline {
		args = append(args, "--network", "none")
	}
	return append(args,
		"-e", "HOME=/h", "-e", "GOPATH=/h/go", "-e", "GOCACHE=/h/cache", "-e", "GOFLAGS=-modcacherw -mod=mod",
		"-e", "GOTOOLCHAIN=local", "-e", proxy,
		"-v", home+":/h", "-v", work+":/work", "-v", src+":/work/src:ro",
		"-w", "/work/src", "--entrypoint", "bash", image, "-c", script)
}

// codeqlOldGo is Go refusing a module that asks for a newer Go than the
// image has (GOTOOLCHAIN=local): "go.mod requires go >= 1.27.4 (running go
// 1.27.3; GOTOOLCHAIN=local)".
var codeqlOldGo = lazyre.New(`requires go >= (\S+) \(running go (\S+); GOTOOLCHAIN=local\)`)

// codeqlImageGoTooOld says, when the log shows it, that the image's Go is
// older than the module asks for: a check run in the same image fails the
// same way, and no change to the code's findings fixes it (exit 2, like a
// missing image).
func codeqlImageGoTooOld(image, logPath string) error {
	data, err := os.ReadFile(logPath) // #nosec G304 -- in the scan's own work dir
	if err != nil {
		return nil
	}
	m := codeqlOldGo.FindStringSubmatch(string(data))
	if m == nil {
		return nil
	}
	return codeqlUnavailable{fmt.Sprintf("the image %s has go %s, older than the go %s this module asks for: a CodeQL check run in this image fails the same way "+
		"(it builds offline with the image's own Go) until the image has go %s. That is the image, not a finding: rebuilding it is the fix, "+
		"or a go line in go.mod the image has (with a toolchain line for the newer Go)", image, m[2], m[1], m[1])}
}

// codeqlRuleLangs is the language of a rule's prefix (go/… is go).
var codeqlRuleLangs = map[string]string{"go": "go", "js": "javascript-typescript", "py": "python", "actions": "actions"}

var (
	// codeqlNewHeading starts the report's section of new findings, the
	// ones the check fails on ("## 1 new, 1 blocking").
	codeqlNewHeading = lazyre.New(`^## \d+ new, \d+ blocking`)
	// codeqlFailedNote names the languages whose scan failed in CI.
	codeqlFailedNote = lazyre.New(`(?m)^CodeQL failed for (.+); its findings are missing below\.`)
)

// codeqlReportLangs is the languages a CodeQL check's report (the markdown
// its status links to: a "## N new, M blocking" section whose table marks
// blocking rows **!** with the rule third) says it failed on: those of the
// new blocking findings, then those whose scan failed; with the rules, for
// saying why. ok is false when a rule is from a language this doesn't know
// (scan everything then).
func codeqlReportLangs(report string) (langs, rules []string, ok bool) {
	add := func(l string) {
		if !slices.Contains(langs, l) {
			langs = append(langs, l)
		}
	}
	inNew := false
	for _, line := range strings.Split(report, "\n") {
		if strings.HasPrefix(line, "## ") {
			inNew = codeqlNewHeading.MatchString(line)
			continue
		}
		cells := strings.Split(line, "|")
		if !inNew || len(cells) < 4 || strings.TrimSpace(cells[1]) != "**!**" {
			continue
		}
		rule := strings.Trim(strings.TrimSpace(cells[3]), "`")
		prefix, _, _ := strings.Cut(rule, "/")
		lang, known := codeqlRuleLangs[prefix]
		if !known {
			return nil, []string{rule}, false
		}
		add(lang)
		if !slices.Contains(rules, rule) {
			rules = append(rules, rule)
		}
	}
	if m := codeqlFailedNote.FindStringSubmatch(report); m != nil {
		for _, l := range strings.Split(m[1], ",") {
			if l = strings.TrimSpace(l); l != "" {
				add(l)
			}
		}
	}
	return langs, rules, true
}

// codeqlFetchReport reads a CodeQL check's report from where its status
// points (tests replace it).
var codeqlFetchReport = func(ctx context.Context, link string) (string, error) {
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("its link %q isn't a web address", link)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil) // #nosec G704 -- the report link Waypoint has for the check; only read
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req) // #nosec G704 -- as above
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", u.Host, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return string(b), err
}

// codeqlPRLangs is the languages to scan when none are named: when dir is
// the repository checked out here and its branch's pull request failed the
// code-scanning check, the languages of what failed it (WP-259). Otherwise
// nil, to scan every language; why says which and why in either case.
func (c *cli) codeqlPRLangs(dir string) (langs []string, why string) {
	top, err := localGit("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, "every language there"
	}
	real := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	if rel, err := filepath.Rel(real(top), real(dir)); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "every language there"
	}
	d, _, err := c.loadPR(scopeFlags{}, nil, "")
	var ue usageError
	switch {
	case errors.As(err, &ue):
		return nil, "every language here (no open pull request from this branch)"
	case err != nil:
		return nil, fmt.Sprintf("every language here (couldn't look up this branch's pull request: %v)", err)
	}
	var check *prCheck
	if d.Shipping != nil {
		for i, ch := range d.Shipping.Checks {
			if strings.Contains(strings.ToLower(ch.Name), "codeql") && (check == nil || check.State != "failed" && ch.State == "failed") {
				check = &d.Shipping.Checks[i]
			}
		}
	}
	switch {
	case check == nil:
		return nil, fmt.Sprintf("every language here (#%d has no CodeQL check)", d.Number)
	case check.State != "failed":
		return nil, fmt.Sprintf("every language here (#%d's %s is %s, not failed)", d.Number, check.Name, check.State)
	}
	report, err := codeqlFetchReport(context.Background(), check.URL)
	if err != nil {
		return nil, fmt.Sprintf("every language here (couldn't read #%d's %s report: %v)", d.Number, check.Name, err)
	}
	langs, rules, ok := codeqlReportLangs(report)
	switch {
	case !ok:
		return nil, fmt.Sprintf("every language here (#%d's %s failed on %s, a language this doesn't know)", d.Number, check.Name, rules[0])
	case len(langs) == 0:
		return nil, fmt.Sprintf("every language here (#%d's %s report names no blocking finding)", d.Number, check.Name)
	}
	on := strings.Join(rules, ", ")
	if len(rules) > 3 {
		on = strings.Join(rules[:3], ", ") + fmt.Sprintf(" and %d more", len(rules)-3)
	}
	if on == "" {
		on = "a failed scan"
	}
	return langs, fmt.Sprintf("what #%d's %s failed on (%s)", d.Number, check.Name, on)
}

// codeqlOpts is what one scan is told.
type codeqlOpts struct {
	Dir      string // the tree to scan, absolute
	Image    string
	Langs    []string // replaces the languages found in the tree
	Why      string   // why these languages (or every one), said before scanning
	CacheDir string   // waypoint/codeql under the user cache dir
	UID, GID int
	Progress io.Writer
}

// codeqlReport is a scan's result.
type codeqlReport struct {
	Languages []string        `json:"languages"`
	Skipped   []string        `json:"skipped,omitempty"`
	Why       string          `json:"why,omitempty"` // why these languages
	Config    string          `json:"config"`        // "repo" or "default"
	Findings  []codeqlFinding `json:"findings"`
	Blocking  int             `json:"blocking"` // findings that fail the check
	Triaged   int             `json:"triaged"`
	Seconds   int             `json:"seconds"`
}

// codeqlFailed is a scan step that failed (exit 3).
type codeqlFailed struct{ msg string }

func (e codeqlFailed) Error() string { return e.msg }

// runCodeqlScan scans one tree: languages, config, one container per
// language, the findings read from SARIF and checked against the triage lists.
func runCodeqlScan(ctx context.Context, d codeqlDocker, o codeqlOpts) (*codeqlReport, error) {
	started := time.Now()
	if o.Image == "" {
		return nil, errCodeqlNoImage
	}
	if err := checkCodeqlImage(o.Image); err != nil {
		return nil, err
	}
	if err := d.HasImage(ctx, o.Image); err != nil {
		return nil, err
	}
	files, err := codeqlTree(ctx, o.Dir)
	if err != nil {
		return nil, err
	}
	langs, skipped := codeqlLanguages(files)
	if len(o.Langs) > 0 {
		langs, skipped = o.Langs, nil
	}
	rep := &codeqlReport{Languages: langs, Skipped: skipped, Why: o.Why, Config: "default", Findings: []codeqlFinding{}}
	if len(langs) == 0 {
		return rep, nil
	}
	fmt.Fprintf(o.Progress, "codeql: languages %s: %s\n", strings.Join(langs, ", "), cmpStr(o.Why, "every language there"))
	home := filepath.Join(o.CacheDir, "home")
	if err := os.MkdirAll(home, 0o750); err != nil {
		return nil, err
	}
	codeqlSweep(o.CacheDir)
	work, err := os.MkdirTemp(o.CacheDir, "work-")
	if err != nil {
		return nil, err
	}
	cfg, repoCfg := codeqlConfig(o.Dir)
	if repoCfg {
		rep.Config = "repo"
	}
	for _, d := range []string{"db", "out"} {
		if err := os.MkdirAll(filepath.Join(work, d), 0o750); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(work, "cfg.yml"), []byte(cfg), 0o600); err != nil {
		return nil, err
	}
	var all []codeqlFinding
	for _, lang := range langs {
		src := o.Dir
		if lang == "actions" {
			if src, err = codeqlActionsSource(o.Dir, work); err != nil {
				return nil, err
			}
		}
		fmt.Fprintf(o.Progress, "codeql: scanning %s (docker %s)…\n", lang, o.Image)
		logPath := filepath.Join(work, lang+".log")
		lf, err := os.Create(logPath) // #nosec G304 -- in the scan's own work dir
		if err != nil {
			return nil, err
		}
		code := 0
		if lang == "go" {
			code, err = d.Run(ctx, lf, codeqlFetchArgs(o.Image, work, home, src, o.UID, o.GID)...)
			if err == nil && code != 0 {
				_ = lf.Close()
				if old := codeqlImageGoTooOld(o.Image, logPath); old != nil {
					return nil, old
				}
				return nil, codeqlFailed{fmt.Sprintf("go: fetching the module's dependencies exited %d; log %s\n%s", code, logPath, tailLines(logPath, 12))}
			}
		}
		if err == nil {
			code, err = d.Run(ctx, lf, codeqlDockerArgs(o.Image, work, home, src, o.UID, o.GID, lang)...)
		}
		_ = lf.Close()
		switch {
		case err != nil:
			return nil, codeqlFailed{fmt.Sprintf("%s: docker run failed: %v; log %s", lang, err, logPath)}
		case code != 0 && code != codeqlNoSource && codeqlImageGoTooOld(o.Image, logPath) != nil:
			return nil, codeqlImageGoTooOld(o.Image, logPath)
		case code == codeqlNoSource:
			fmt.Fprintf(o.Progress, "codeql: %s: no code left after the config's paths-ignore\n", lang)
			continue
		case code != 0:
			return nil, codeqlFailed{fmt.Sprintf("%s: codeql exited %d; log %s\n%s", lang, code, logPath, tailLines(logPath, 12))}
		}
		data, err := os.ReadFile(filepath.Join(work, "out", lang+".sarif")) // #nosec G304 -- in the scan's own work dir
		if err != nil {
			return nil, codeqlFailed{fmt.Sprintf("%s: no results file; log %s", lang, logPath)}
		}
		var found []codeqlFinding
		if lang == "actions" && src != o.Dir {
			found, err = parseSarifPaths(data, codeqlActionsRepoPath)
		} else {
			found, err = parseSarif(data)
		}
		if err != nil {
			return nil, codeqlFailed{fmt.Sprintf("%s: %v", lang, err)}
		}
		all = append(all, found...)
	}
	var lists [][]byte
	for _, f := range files {
		if isCodeqlTriageFile(f.Path) {
			if b, err := os.ReadFile(filepath.Join(o.Dir, filepath.FromSlash(f.Path))); err == nil { // #nosec G304 -- the tree being scanned
				lists = append(lists, b)
			}
		}
	}
	markTriaged(all, codeqlTriaged(lists))
	for _, f := range all {
		if f.blocks() {
			rep.Blocking++
		}
		if f.Triaged {
			rep.Triaged++
		}
	}
	rep.Findings = append(rep.Findings, all...)
	rep.Seconds = int(time.Since(started).Round(time.Second).Seconds())
	_ = os.RemoveAll(work)
	return rep, nil
}

// codeqlFirst is the first non-empty string.
func codeqlFirst(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// codeqlSweep removes work dirs of failed scans after a day.
func codeqlSweep(cache string) {
	entries, _ := os.ReadDir(cache)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && e.IsDir() && strings.HasPrefix(e.Name(), "work-") && time.Since(info.ModTime()) > 24*time.Hour {
			_ = os.RemoveAll(filepath.Join(cache, e.Name()))
		}
	}
}

// codeqlStagePrefix marks a workflow or action staged from .forgejo or .gitea
// next to the repository's own .github ones, so same-named files don't collide.
// The stage is "<prefix><base>--<name>", e.g. ".github/workflows/wp-stage-forgejo--ci.yml".
const codeqlStagePrefix = "wp-stage-"

// codeqlActionsSource is the tree the actions extractor reads. It looks only
// in .github/workflows, so for a repository whose workflows are in .forgejo or
// .gitea, they are copied there (with their actions). When .github/workflows
// also has YAML, the .github workflows and actions are copied too and the
// others get a codeqlStagePrefix name, so every set is read and
// codeqlActionsRepoPath can name the true path. With only .github/workflows it
// is dir itself.
func codeqlActionsSource(dir, work string) (string, error) {
	hasYAML := func(d string) bool {
		m, _ := filepath.Glob(filepath.Join(dir, filepath.FromSlash(d), "*.y*ml"))
		return len(m) > 0
	}
	github := hasYAML(".github/workflows")
	stage := filepath.Join(work, "actions-src")
	var bases []string
	for _, base := range []string{".forgejo", ".gitea"} {
		if hasYAML(base + "/workflows") {
			bases = append(bases, base)
		}
	}
	switch {
	case len(bases) == 0:
		return dir, nil
	case !github:
		bases = bases[:1]
		if err := copyFiles(filepath.Join(dir, bases[0], "workflows"), filepath.Join(stage, ".github", "workflows")); err != nil {
			return "", err
		}
		if err := copyFiles(filepath.Join(dir, bases[0], "actions"), filepath.Join(stage, ".github", "actions")); err != nil {
			return "", err
		}
		return stage, nil
	}
	for _, kind := range []string{"workflows", "actions"} {
		own, err := os.ReadDir(filepath.Join(dir, ".github", kind))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		for _, e := range own {
			if strings.HasPrefix(e.Name(), codeqlStagePrefix) {
				return "", fmt.Errorf(".github/%s/%s starts with %q, which the scan uses to tell staged .forgejo or .gitea files from this repository's own; rename it", kind, e.Name(), codeqlStagePrefix)
			}
		}
		if err := copyFiles(filepath.Join(dir, ".github", kind), filepath.Join(stage, ".github", kind)); err != nil {
			return "", err
		}
		for _, base := range bases {
			prefix := codeqlStagePrefix + strings.TrimPrefix(base, ".") + "--"
			entries, err := os.ReadDir(filepath.Join(dir, base, kind))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
			for _, e := range entries {
				to := filepath.Join(stage, ".github", kind, prefix+e.Name())
				if e.IsDir() {
					if err := copyFiles(filepath.Join(dir, base, kind, e.Name()), to); err != nil {
						return "", err
					}
				} else if err := copyFiles1(filepath.Join(dir, base, kind, e.Name()), to); err != nil {
					return "", err
				}
			}
		}
	}
	return stage, nil
}

// codeqlActionsRepoPath is the repository path of a path in the actions
// extractor's tree: a staged workflow or action goes back to .forgejo or
// .gitea; any other path is its own.
func codeqlActionsRepoPath(p string) string {
	for _, kind := range []string{"workflows", "actions"} {
		rest, ok := strings.CutPrefix(p, ".github/"+kind+"/"+codeqlStagePrefix)
		if !ok {
			continue
		}
		if base, name, ok := strings.Cut(rest, "--"); ok && base != "" {
			return "." + base + "/" + kind + "/" + name
		}
	}
	return p
}

// copyFiles copies the regular files and directories under from to to;
// symlinks are left behind. A missing from copies nothing; any other error
// is returned.
func copyFiles(from, to string) error {
	entries, err := os.ReadDir(from)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(to, 0o750); err != nil {
		return err
	}
	for _, e := range entries {
		switch {
		case e.IsDir():
			if err := copyFiles(filepath.Join(from, e.Name()), filepath.Join(to, e.Name())); err != nil {
				return err
			}
		case e.Type().IsRegular():
			if err := copyFiles1(filepath.Join(from, e.Name()), filepath.Join(to, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyFiles1 copies one regular file, creating its directory. Anything else
// (a symlink) is left behind without an error, and so is a missing file; any
// other error is returned.
func copyFiles1(from, to string) error {
	info, err := os.Lstat(from)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
		return err
	}
	b, err := os.ReadFile(from) // #nosec G304 -- the tree being scanned
	if err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o600) // #nosec G703 -- a file name read from the tree being scanned, written under the scan's own work dir
}

// tailLines is the last n lines of a file.
func tailLines(p string, n int) string {
	b, err := os.ReadFile(p) // #nosec G304 -- the scan's own log
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// codeqlTree lists the tree's files: what git tracks or would add (so ignored
// build output doesn't count), else everything under dir except .git.
func codeqlTree(ctx context.Context, dir string) ([]treeFile, error) {
	var paths []string
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		cmd := exec.CommandContext(ctx, "git", "-C", dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard") // #nosec G204 -- fixed arguments
		if out, err := cmd.Output(); err == nil {
			for _, p := range strings.Split(string(out), "\x00") {
				if p != "" {
					paths = append(paths, p)
				}
			}
		}
	}
	if paths == nil {
		err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if e.IsDir() {
				if e.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			if rel, err := filepath.Rel(dir, p); err == nil {
				paths = append(paths, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	files := make([]treeFile, 0, len(paths))
	for _, p := range paths {
		if info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p))); err == nil && info.Mode().IsRegular() {
			files = append(files, treeFile{Path: p, Size: info.Size()})
		}
	}
	return files, nil
}

// render is the scan as text: one line per finding, the flow of each blocking
// one, then a summary line.
func (r *codeqlReport) render(steps int, all bool) string {
	var b strings.Builder
	listed, hidden := 0, 0
	for _, f := range r.Findings {
		if !f.blocks() && !f.Triaged && !all {
			if listed >= codeqlListed {
				hidden++
				continue
			}
			listed++
		}
		mark, label := "      ", ""
		if f.blocks() {
			mark = "BLOCK "
		}
		if f.Triaged {
			label = " [triaged]"
		}
		fmt.Fprintf(&b, "%s%s %s %s:%d %s%s\n", mark, f.Severity, f.Rule, f.Path, f.Line, f.Message, label)
		if !f.blocks() {
			continue
		}
		for i, s := range f.Flow {
			if i >= steps {
				last := f.Flow[len(f.Flow)-1]
				fmt.Fprintf(&b, "      … %d more steps, ending at %s:%d\n", len(f.Flow)-steps, last.Path, last.Line)
				break
			}
			fmt.Fprintf(&b, "      %d. %s:%d %s\n", i+1, s.Path, s.Line, s.Message)
		}
	}
	if hidden > 0 {
		fmt.Fprintf(&b, "      … %d more non-blocking findings (--all lists them)\n", hidden)
	}
	verdict := "ok, nothing blocks"
	if r.Blocking > 0 {
		verdict = fmt.Sprintf("FAIL, %d would block the pull request", r.Blocking)
	}
	langs := strings.Join(r.Languages, ", ")
	if langs == "" {
		langs = "no code CodeQL covers"
	}
	fmt.Fprintf(&b, "codeql: %s; %d blocking, %d triaged, %d other (%s; %s config; %ds)", verdict, r.Blocking, r.Triaged, len(r.Findings)-r.Blocking-r.Triaged, langs, r.Config, r.Seconds)
	if len(r.Skipped) > 0 {
		fmt.Fprintf(&b, "; not scanned: %s", strings.Join(r.Skipped, ", "))
	}
	return b.String() + "\n"
}

func cmdCodeqlScan(c *cli, args []string) error {
	fs := c.flags()
	lang := fs.String("lang", "", "scan these `LANGS` (comma-separated)")
	image := fs.String("image", os.Getenv("WP_CODEQL_IMAGE"), "the docker `IMAGE` with the codeql CLI (default $WP_CODEQL_IMAGE)")
	steps := fs.Int("steps", 5, "data-flow steps shown per blocking finding, `N`")
	all := fs.Bool("all", false, "list every non-blocking finding")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return c.usage("one directory at most, got %q", joinArgs(pos))
	}
	dir := "."
	if len(pos) == 1 {
		dir = pos[0]
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return c.usage("%s is not a directory", dir)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	o := codeqlOpts{Dir: dir, Image: *image, CacheDir: filepath.Join(cache, "waypoint", "codeql"), UID: os.Getuid(), GID: os.Getgid(), Progress: c.err}
	switch {
	case o.Image == "":
	case *lang != "":
		for _, l := range strings.Split(*lang, ",") {
			if l = strings.TrimSpace(l); l != "" {
				o.Langs = append(o.Langs, l)
			}
		}
		o.Why = "--lang"
	default:
		o.Langs, o.Why = c.codeqlPRLangs(dir)
	}
	return c.codeqlScan(codeqlDockerClient, o, *steps, *all)
}

// codeqlScan runs the scan and prints it; the exit code says whether it blocks.
func (c *cli) codeqlScan(d codeqlDocker, o codeqlOpts, steps int, all bool) error {
	rep, err := runCodeqlScan(context.Background(), d, o)
	var unavailable codeqlUnavailable
	var failed codeqlFailed
	switch {
	case errors.As(err, &unavailable):
		_, _ = fmt.Fprintln(c.err, "wp codeql scan:", unavailable.msg)
		return exitCode(2)
	case errors.As(err, &failed):
		_, _ = fmt.Fprintln(c.err, "wp codeql scan: failed:", failed.msg)
		return exitCode(3)
	case err != nil:
		return err
	}
	if c.asJSON {
		raw, _ := json.Marshal(rep)
		if err := c.emit(raw, func() string { return "" }); err != nil {
			return err
		}
	} else {
		w := bufio.NewWriter(c.out)
		_, _ = w.WriteString(rep.render(steps, all))
		_ = w.Flush()
	}
	if rep.Blocking > 0 {
		return exitCode(1)
	}
	return nil
}
