package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"path"
	"slices"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// TriageVersion names the rules that decided, stored on each row so results
// can be split when the rules change.
const TriageVersion = "rules-1"

// Triage actions.
const (
	triageReview = "review"
	triageSkip   = "skip"
)

// Why a change the rules would skip was reviewed, or why one was skipped.
const (
	triageWhyAsked      = "asked"
	triageWhyAgentSkip  = "agent_skip"
	triageWhyOwnReview  = "own_review"
	triageWhyModeOff    = "mode_off"
	triageWhyModeShadow = "mode_shadow"
	triageWhyShadowRule = "shadow_rule"
	triageWhyHoldout    = "holdout"

	// triageWhySuperseded is a rule's skip of a branch assessed again: the
	// later row says what became of the branch, and reports leave this out.
	triageWhySuperseded = "superseded"
)

// Review skip modes (AutopilotLimits.ReviewSkip).
const (
	ReviewSkipOn     = "on"
	ReviewSkipShadow = "shadow"
	ReviewSkipOff    = "off"
)

// DefaultReviewSkip is Waypoint's default.
const DefaultReviewSkip = ReviewSkipOn

// triageHoldoutPct of the changes active rules would skip are reviewed.
const triageHoldoutPct = 20

// Floors' limits: past these nothing is skipped.
const (
	triageMaxLines = 400
	triageMaxFiles = 20
)

// ChangeFacts is what wp ship's checkout says about the change. The server
// parses it rather than trust a verdict: the rules live in one place, and an
// older wp simply sends nothing, which reviews.
type ChangeFacts struct {
	Numstat string `json:"numstat"`           // git diff --no-renames --numstat base...HEAD
	Diff    string `json:"diff,omitempty"`    // git diff --no-renames --unified=0 base...HEAD
	Commits int    `json:"commits,omitempty"` // commits on the branch
	Clipped bool   `json:"clipped,omitempty"` // the diff was too big to send
}

// TriageFeatures describe a change: every signal the rules use and the ones a
// model might, stored as the row's JSON.
type TriageFeatures struct {
	NoFacts bool `json:"no_facts,omitempty"`
	Clipped bool `json:"clipped,omitempty"`

	Files        int            `json:"files"`
	Added        int            `json:"added"`
	Deleted      int            `json:"deleted"`
	Commits      int            `json:"commits,omitempty"`
	Binary       int            `json:"binary,omitempty"`
	NewFiles     int            `json:"new_files,omitempty"`
	DeletedFiles int            `json:"deleted_files,omitempty"`
	MaxFileLines int            `json:"max_file_lines,omitempty"`
	Exts         map[string]int `json:"exts,omitempty"`

	// Files by class.
	Docs  int `json:"docs,omitempty"`
	Tests int `json:"tests,omitempty"`
	Code  int `json:"code,omitempty"`
	Other int `json:"other,omitempty"`

	// Changed lines outside docs, by what changed.
	CommentLines int `json:"comment_lines,omitempty"`
	BlankLines   int `json:"blank_lines,omitempty"`
	StringLines  int `json:"string_lines,omitempty"` // only the text of a string changed
	CodeLines    int `json:"code_lines,omitempty"`
	SQLStrings   int `json:"sql_strings,omitempty"` // of StringLines, with SQL in the text

	Sensitive []string `json:"sensitive,omitempty"` // first few sensitive paths

	// From the server.
	Provenance   string `json:"provenance,omitempty"`    // the task's provenance risk ("": unknown)
	RiskErr      bool   `json:"risk_err,omitempty"`      // the provenance lookup failed
	Hotspots     int    `json:"hotspots,omitempty"`      // hotspot files, outside docs
	Fixes        int    `json:"fixes,omitempty"`         // most bug fixes on a file, outside docs
	MaxImporters int    `json:"max_importers,omitempty"` // most importers of a file
	Known        int    `json:"known,omitempty"`         // files the code index knows
	RiskyFiles   int    `json:"risky_files,omitempty"`   // files the risky_code floor names
	TaskType     string `json:"task_type,omitempty"`
	Priority     int    `json:"priority,omitempty"`

	paths []string
	class map[string]string
}

// lines is the change's size in lines.
func (f *TriageFeatures) lines() int { return f.Added + f.Deleted }

// File classes.
const (
	fileDoc   = "doc"
	fileTest  = "test"
	fileCode  = "code"
	fileOther = "other"
)

// A doc is a file under a docs directory, or one of docsNames. Prose
// anywhere else may do work, and docsExcludedNames and docsExcludedDirs are
// prose that does, wherever it sits: agent instructions, prompts, the review
// bot's rules (REVIEW.md, read at the pull request's own head), and anything
// embedded in the program. They class as other, so no docs rule applies.
var (
	docsNames         = map[string]bool{"readme": true, "changelog": true, "contributing": true}
	docsExcludedNames = map[string]bool{"agents.md": true, "agent.md": true, "claude.md": true, "gemini.md": true, "codex.md": true, "skill.md": true, "copilot-instructions.md": true, "review.md": true, "requirements.txt": true, "constraints.txt": true}
	docsExcludedDirs  = map[string]bool{".claude": true, ".cursor": true, ".github": true, ".forgejo": true, "agents": true, "skills": true, "prompts": true, "plugins": true, "kit": true,
		"internal": true, "cmd": true, "src": true, "pkg": true, "lib": true, "app": true, "vendor": true, "node_modules": true}
	codeExts = map[string]bool{"go": true, "js": true, "jsx": true, "ts": true, "tsx": true, "mjs": true, "cjs": true, "py": true, "rb": true, "sh": true, "bash": true, "zsh": true,
		"pl": true, "java": true, "c": true, "h": true, "cpp": true, "cc": true, "hpp": true, "rs": true, "swift": true, "kt": true, "cs": true, "scala": true, "php": true,
		"css": true, "scss": true, "html": true, "vue": true, "svelte": true, "sql": true}
)

func fileExt(p string) string {
	return strings.TrimPrefix(strings.ToLower(path.Ext(p)), ".")
}

// classifyFile says what kind of file p is. When unsure it isn't a doc.
func classifyFile(p string) string {
	lower := strings.ToLower(p)
	base := path.Base(lower)
	ext := fileExt(lower)
	segs := strings.Split(lower, "/")
	dirs := segs[:len(segs)-1]
	inDir := func(names ...string) bool {
		for _, d := range dirs {
			for _, n := range names {
				if d == n {
					return true
				}
			}
		}
		return false
	}
	switch ext {
	case "md", "mdx", "rst", "adoc", "txt":
		excluded := docsExcludedNames[base]
		for _, d := range dirs {
			excluded = excluded || docsExcludedDirs[d]
		}
		named := ext != "txt" && docsNames[strings.TrimSuffix(base, "."+ext)]
		if !excluded && (named || inDir("docs", "doc")) {
			return fileDoc
		}
	}
	if strings.HasSuffix(base, "_test.go") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") ||
		(strings.HasPrefix(base, "test_") && ext == "py") || inDir("e2e", "test", "tests") {
		return fileTest
	}
	if codeExts[ext] {
		return fileCode
	}
	return fileOther
}

// sensitivePathRE matches what always gets a review outside docs: access,
// secrets, schema, build and deployment. Generous on purpose: a false match
// costs one review.
var sensitivePathRE = lazyre.New(`auth|login|oauth|token|secret|password|credential|permission|acl|crypto|sandbox|security|migrations/|\.sql$|dockerfile|docker-compose|\.forgejo/|\.github/|makefile|go\.mod$|go\.sum$|package\.json$|package-lock\.json$|yarn\.lock$|pnpm-lock|\.env|nginx|deploy|infra|terraform|k8s|helm|hooks?/|guard`)

// stakesRank orders paths: those that make a change high-stakes first.
func stakesRank(p string) int {
	if stakesPath(p) {
		return 0
	}
	return 1
}

// triageFeatures parses wp ship's facts. Whatever can't be read counts
// against skipping: a changed line the diff doesn't explain is code.
func triageFeatures(c *ChangeFacts) *TriageFeatures {
	f := &TriageFeatures{Exts: map[string]int{}, class: map[string]string{}}
	if c == nil || strings.TrimSpace(c.Numstat) == "" {
		f.NoFacts = true
		return f
	}
	f.Commits = c.Commits
	f.Clipped = c.Clipped
	total := map[string]int{}
	for _, l := range strings.Split(strings.TrimSpace(c.Numstat), "\n") {
		p := strings.SplitN(l, "\t", 3)
		if len(p) != 3 {
			f.Clipped = true
			continue
		}
		name := diffPath(p[2])
		f.Files++
		f.paths = append(f.paths, name)
		cl := classifyFile(name)
		f.class[name] = cl
		switch cl {
		case fileDoc:
			f.Docs++
		case fileTest:
			f.Tests++
		case fileCode:
			f.Code++
		default:
			f.Other++
		}
		if e := fileExt(name); e != "" {
			f.Exts[e]++
		}
		if cl != fileDoc && sensitivePathRE.MatchString(strings.ToLower(name)) {
			f.Sensitive = append(f.Sensitive, name)
		}
		if p[0] == "-" || p[1] == "-" {
			f.Binary++
			continue
		}
		a, errA := strconv.Atoi(p[0])
		d, errD := strconv.Atoi(p[1])
		if errA != nil || errD != nil {
			f.Clipped = true
			continue
		}
		f.Added += a
		f.Deleted += d
		total[name] = a + d
		f.MaxFileLines = max(f.MaxFileLines, a+d)
		if a+d == 0 && cl != fileDoc {
			f.CodeLines++
		}
	}
	slices.SortStableFunc(f.Sensitive, func(a, b string) int { return stakesRank(a) - stakesRank(b) })
	f.Sensitive = f.Sensitive[:min(len(f.Sensitive), 10)]
	seen := parseUnifiedDiff(c.Diff, f)
	for name, n := range total {
		if f.class[name] != fileDoc && seen[name] < n {
			f.CodeLines += n - seen[name]
		}
	}
	return f
}

// diffPath undoes git's quoting of an unusual path, as far as a class needs.
func diffPath(p string) string {
	p = strings.TrimSpace(p)
	if i := strings.IndexByte(p, '\t'); i >= 0 {
		p = p[:i]
	}
	return strings.Trim(p, `"`)
}

var triageHunkRE = lazyre.New(`^@@ -\d+(?:,(\d+))? \+\d+(?:,(\d+))? @@`)

// hunkCount reads a hunk header's line count: absent is 1.
func hunkCount(s string) int {
	if s == "" {
		return 1
	}
	n, _ := strconv.Atoi(s)
	return n
}

// parseUnifiedDiff counts the changed lines of a --unified=0 diff by what
// they are, for files that aren't docs, and returns how many it saw per
// file. Headers are only read between hunks, and hunks are read by their
// own line counts, so a removed line that looks like a header isn't one.
func parseUnifiedDiff(diff string, f *TriageFeatures) map[string]int {
	seen := map[string]int{}
	if diff == "" {
		return seen
	}
	var file string
	var removed, added []string
	var wantDel, wantAdd int
	isDoc := func() bool { return f.class[file] == fileDoc }
	finish := func() {
		if file != "" && !isDoc() {
			classifyHunk(file, removed, added, f)
			seen[file] += len(removed) + len(added)
		}
		removed, added = nil, nil
	}
	for _, l := range strings.Split(diff, "\n") {
		if wantDel+wantAdd > 0 {
			switch {
			case strings.HasPrefix(l, `\`):
			case strings.HasPrefix(l, "-") && wantDel > 0:
				removed = append(removed, l[1:])
				wantDel--
			case strings.HasPrefix(l, "+") && wantAdd > 0:
				added = append(added, l[1:])
				wantAdd--
			default:
				wantDel, wantAdd = 0, 0
			}
			if wantDel+wantAdd == 0 {
				finish()
			}
			continue
		}
		switch {
		case strings.HasPrefix(l, "diff --git "):
			file = ""
		case strings.HasPrefix(l, "--- "):
			name := strings.TrimSpace(l[4:])
			if name == "/dev/null" {
				f.NewFiles++
			} else {
				file = diffPath(strings.TrimPrefix(diffPath(name), "a/"))
			}
		case strings.HasPrefix(l, "+++ "):
			name := strings.TrimSpace(l[4:])
			if name == "/dev/null" {
				f.DeletedFiles++
			} else {
				file = diffPath(strings.TrimPrefix(diffPath(name), "b/"))
			}
		case strings.HasPrefix(l, "@@ "):
			if m := triageHunkRE.FindStringSubmatch(l); m != nil {
				wantDel, wantAdd = hunkCount(m[1]), hunkCount(m[2])
			}
		}
	}
	return seen
}

// classifyHunk counts one hunk's changed lines. Comments and blanks count by
// line; what's left is string-only when the removed and added lines are the
// same once their strings are blanked.
func classifyHunk(file string, removed, added []string, f *TriageFeatures) {
	ext := fileExt(file)
	var oldCode, newCode []string
	blanks, comments := f.BlankLines, f.CommentLines
	byKind := func(lines []string, out *[]string) {
		for _, l := range lines {
			switch {
			case strings.TrimSpace(l) == "":
				f.BlankLines++
			case isComment(ext, l):
				f.CommentLines++
			default:
				*out = append(*out, l)
			}
		}
	}
	byKind(removed, &oldCode)
	byKind(added, &newCode)
	if len(oldCode)+len(newCode) == 0 && f.CommentLines == comments {
		f.CodeLines += f.BlankLines - blanks
		return
	}
	if stringOnly(oldCode, newCode) {
		f.StringLines += len(oldCode) + len(newCode)
		for _, l := range append(append([]string{}, oldCode...), newCode...) {
			if sqlInStringRE.MatchString(l) {
				f.SQLStrings++
			}
		}
		return
	}
	f.CodeLines += len(oldCode) + len(newCode)
}

var sqlInStringRE = lazyre.New(`\b(SELECT|INSERT|UPDATE|DELETE|FROM|WHERE|JOIN|CREATE|ALTER|DROP)\b`)

var (
	slashComment = map[string]bool{"go": true, "js": true, "jsx": true, "ts": true, "tsx": true, "mjs": true, "cjs": true, "java": true, "c": true, "h": true, "cpp": true, "cc": true, "hpp": true,
		"rs": true, "swift": true, "kt": true, "cs": true, "scala": true, "php": true}
	hashComment = map[string]bool{"py": true, "rb": true, "sh": true, "bash": true, "zsh": true, "pl": true, "yaml": true, "yml": true, "toml": true, "tf": true}
)

// commentDirectives are comments that tell a tool something: they count as
// code. (A comment with no space after its marker counts as code too: that
// covers //go:build, //nolint, //export, #!, and commented-out code.)
var commentDirectives = []string{"go:", "+", "@", "eslint", "biome", "prettier", "#", "type:", "pylint", "shellcheck", "mypy", "ruff", "fmt:", "!", "-*-", "lint:",
	"coding", "vim:", "pragma", "frozen_string_literal", "flake8", "isort", "pyright:"}

// commentSuppressions silence a linter or scanner from anywhere in a comment,
// and a change that only silences one is what a review is for. The -disable
// and :ignore forms are most linters' spelling, so a new linter needs no entry.
var commentSuppressions = []string{"nolint", "nosec", "noqa", "nosemgrep", "nosonar", "skipcq", "codeql[", "lgtm[", "-disable", ":disable", "-ignore", ":ignore", "ts-expect-error"}

// isComment is true for a line that is only a comment, in a language whose
// line comments it knows. Block comments aren't recognised: their inner
// lines can't be told from code a line at a time.
func isComment(ext, line string) bool {
	s := strings.TrimSpace(line)
	marker := ""
	switch {
	case slashComment[ext]:
		marker = "//"
	case hashComment[ext]:
		marker = "#"
	default:
		return false
	}
	if !strings.HasPrefix(s, marker) {
		return false
	}
	if strings.HasSuffix(s, `\`) {
		return false
	}
	rest := s[len(marker):]
	if rest == "" {
		return true
	}
	if rest[0] != ' ' && rest[0] != '\t' {
		return false
	}
	rest = strings.ToLower(strings.TrimSpace(rest))
	if strings.Contains(rest, "generated") || strings.Contains(rest, "do not edit") {
		return false
	}
	for _, d := range commentDirectives {
		if strings.HasPrefix(rest, d) {
			return false
		}
	}
	for _, d := range commentSuppressions {
		if strings.Contains(rest, d) {
			return false
		}
	}
	return true
}

// blankStrings replaces each quoted string in a line with a placeholder. ok
// is false when the line has no string, or a quote that doesn't close.
func blankStrings(line string) (string, bool) {
	var b strings.Builder
	found := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c != '"' && c != '\'' && c != '`' {
			b.WriteByte(c)
			continue
		}
		j := i + 1
		for j < len(line) && line[j] != c {
			if line[j] == '\\' && c != '`' {
				j++
			}
			j++
		}
		if j >= len(line) {
			return "", false
		}
		b.WriteString(`""`)
		found = true
		i = j
	}
	return strings.Join(strings.Fields(b.String()), " "), found
}

// stringOnly is true when both sides have lines and they match one for one
// once their strings are blanked.
func stringOnly(oldLines, newLines []string) bool {
	if len(oldLines) == 0 || len(oldLines) != len(newLines) {
		return false
	}
	count := map[string]int{}
	for _, l := range oldLines {
		s, ok := blankStrings(l)
		if !ok {
			return false
		}
		count[s]++
	}
	for _, l := range newLines {
		s, ok := blankStrings(l)
		if !ok || count[s] == 0 {
			return false
		}
		count[s]--
	}
	return true
}

// triageFloor names the floor that keeps the review, "" when none does.
func triageFloor(f *TriageFeatures) string {
	switch {
	case f.NoFacts:
		return "no_facts"
	case f.Clipped:
		return "clipped"
	case f.Files == 0:
		return "no_facts"
	case f.lines() > triageMaxLines || f.Files > triageMaxFiles:
		return "large"
	case f.Binary > 0:
		return "binary"
	case f.RiskErr || f.Provenance == RiskHigh:
		return "high_risk_task"
	case len(f.Sensitive) > 0:
		return "sensitive_path"
	case f.RiskyFiles > 0:
		return "risky_code"
	}
	return ""
}

// triageRule names a kind of low-risk change. Rules are tried in order.
type triageRule struct {
	name   string
	active bool
	says   string
	match  func(f *TriageFeatures) bool
}

func (f *TriageFeatures) nonDocFilesAreCode() bool { return f.Tests == 0 && f.Other == 0 }

// allCommentOrBlank is true when no changed line outside docs does anything.
func (f *TriageFeatures) allCommentOrBlank() bool {
	return f.CodeLines == 0 && f.StringLines == 0
}

// triageRules are the rules, most certain first.
var triageRules = []triageRule{
	{"docs", true, "only docs changed", func(f *TriageFeatures) bool {
		return f.Docs == f.Files && f.lines() <= 300 && f.Files <= 10
	}},
	{"comments", true, "only comments changed", func(f *TriageFeatures) bool {
		return f.nonDocFilesAreCode() && f.Code > 0 && f.CommentLines > 0 && f.allCommentOrBlank() && f.lines() <= 150
	}},
	{"wording", false, "only wording changed", func(f *TriageFeatures) bool {
		return f.CodeLines == 0 && f.SQLStrings == 0 && f.lines() <= 40
	}},
	{"tests", false, "only tests changed", func(f *TriageFeatures) bool {
		return f.Tests > 0 && f.Tests+f.Docs == f.Files && f.lines() <= 200
	}},
	{"tiny_code", false, "a very small change", func(f *TriageFeatures) bool {
		return f.Other == 0 && f.Files <= 2 && f.lines() <= 20
	}},
}

// triageDecision is what triage decided about a change.
type triageDecision struct {
	Rule      string // floor:NAME, a rule's name, or normal
	WouldSkip bool
	Action    string
	Why       string
	Says      string // the rule's words, for the notice and the round's reason
}

// triageNormal is the rule of a change no floor or rule named.
const triageNormal = "normal"

// decideTriage is the decision: pure, so tests can hold it. intent is wp
// ship's, mode is the person's review_skip, roll is the change's place
// (0-99) against the holdout.
func decideTriage(f *TriageFeatures, mode, intent string, roll int) triageDecision {
	d := triageDecision{Rule: triageNormal, Action: triageReview}
	active := false
	if fl := triageFloor(f); fl != "" {
		d.Rule = "floor:" + fl
	} else {
		for _, r := range triageRules {
			if r.match(f) {
				d.Rule, d.WouldSkip, d.Says = r.name, true, r.says
				active = r.active
				break
			}
		}
	}
	switch {
	case intent == ShipReview:
		d.Why = triageWhyAsked
	case intent == ShipSkip:
		d.Action, d.Why = triageSkip, triageWhyAgentSkip
	case !d.WouldSkip:
	case mode == ReviewSkipOff:
		d.Why = triageWhyModeOff
	case mode == ReviewSkipShadow:
		d.Why = triageWhyModeShadow
	case !active:
		d.Why = triageWhyShadowRule
	case roll < triageHoldoutPct:
		d.Why = triageWhyHoldout
	default:
		d.Action = triageSkip
	}
	return d
}

// triageRoll places a change against the holdout: the same task and branch
// always land the same, so shipping twice doesn't change the answer.
func triageRoll(issueID int64, head string) int {
	h := fnv.New32a()
	_, _ = fmt.Fprintf(h, "%d/%s", issueID, head)
	return int(h.Sum32() % 100)
}

// triageRow is one assessment, ready to record with its round.
type triageRow struct {
	issueID, projectID int64
	repo, head, base   string
	decision           triageDecision
	features           *TriageFeatures
	at                 int64
}

// summary is the change in words: "only docs changed (2 files, 14 lines)".
func (r *triageRow) summary() string {
	return fmt.Sprintf("%s (%s, %s)", r.decision.Says, plural1(r.features.Files, "file"), plural1(r.features.lines(), "line"))
}

// roundReason is the skipped round's reason.
func (r *triageRow) roundReason() string {
	return "review triage (" + TriageVersion + "): " + r.summary()
}

// notice tells the agent the review was skipped and how to have it anyway.
func (r *triageRow) notice() string {
	return "Skipped the adversarial review: " + r.summary() + ". To review it anyway: `wp ship --review`."
}

func (r *triageRow) insert(ctx context.Context, t *tx, round int64) error {
	raw, err := json.Marshal(r.features)
	if err != nil {
		return err
	}
	_, err = t.ExecContext(ctx, `INSERT INTO review_triage(round_id, issue_id, project_id, repo, head, base, rule, would_skip, action, why, version, features, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, round, r.issueID, r.projectID, r.repo, r.head, r.base, r.decision.Rule, boolInt(r.decision.WouldSkip), r.decision.Action, r.decision.Why, TriageVersion, string(raw), r.at)
	return err
}

// supersede marks a rule's skip of this branch, on the round before, as
// overtaken by this assessment. Another branch's skip stands: its pull
// request went without a review.
func (r *triageRow) supersede(ctx context.Context, t *tx, round int64) error {
	_, err := t.ExecContext(ctx, `UPDATE review_triage SET why = ? WHERE round_id = ? AND head = ? AND action = ? AND why = ''`, triageWhySuperseded, round, r.head, triageSkip)
	return err
}

// skippedByRule is true when round was skipped by a triage rule rather than
// by the agent.
func (s *Service) skippedByRule(ctx context.Context, round int64) (bool, error) {
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM review_triage WHERE round_id = ? AND action = ? AND why = ''`, round, triageSkip).Scan(&n)
	return n > 0, err
}

// Risky code, on the code index's numbers: a floor even for a comment-only
// change, which on such a file is worth a look.
const (
	triageHotspotFixes = 2
	triageRiskyFixes   = 3
	triageRiskyImports = 20
)

// assessChange runs triage for a ship. It never fails the ship: what it
// can't look up keeps the review.
func (s *Service) assessChange(ctx context.Context, actor *Actor, g *reviewGate, projectID int64, in OpenPRInput, repo, head, base, intent string) *triageRow {
	f := triageFeatures(in.Change)
	s.addTriageFacts(ctx, actor, f, g, projectID, repo)
	return &triageRow{issueID: g.issueID, projectID: g.taskPID, repo: repo, head: head, base: base, features: f, at: s.nowMS(),
		decision: decideTriage(f, cmp(valueOf(g.lim.ReviewSkip), DefaultReviewSkip), intent, triageRoll(g.issueID, head))}
}

// addTriageFacts adds what only the server knows: the task's risk and type,
// and what the code index says about the files.
func (s *Service) addTriageFacts(ctx context.Context, actor *Actor, f *TriageFeatures, g *reviewGate, projectID int64, repo string) {
	if f.NoFacts {
		return
	}
	risk, err := s.issueRiskByID(ctx, s.db.R, actor, g.issueID)
	if err != nil || risk == nil {
		f.RiskErr = true
	} else {
		f.Provenance = risk.Level
	}
	if err := s.db.R.QueryRowContext(ctx, `SELECT type, priority FROM issues WHERE id = ?`, g.issueID).Scan(&f.TaskType, &f.Priority); err != nil {
		f.RiskErr = true
	}
	var forgeID, repoID int64
	err = s.db.R.QueryRowContext(ctx, `SELECT forge_id, forge_repo_id FROM project_repos WHERE project_id = ? AND repo = ? COLLATE NOCASE LIMIT 1`, projectID, repo).Scan(&forgeID, &repoID)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil {
		f.RiskErr = true
		return
	}
	var paths []string
	for _, p := range f.paths {
		if f.class[p] != fileDoc {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return
	}
	files, err := s.codeFiles(ctx, forgeID, repoID, paths, 0)
	if err != nil {
		f.RiskErr = true
		return
	}
	for _, c := range files {
		f.Known++
		f.Fixes = max(f.Fixes, c.Fixes)
		f.MaxImporters = max(f.MaxImporters, c.Importers)
		if c.Hotspot {
			f.Hotspots++
		}
		if (c.Hotspot && c.Fixes >= triageHotspotFixes) || c.Fixes >= triageRiskyFixes || c.Importers >= triageRiskyImports {
			f.RiskyFiles++
		}
	}
}
