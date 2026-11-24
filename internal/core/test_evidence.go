package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Test evidence outcomes.
const (
	EvidencePass  = "pass"
	EvidenceFail  = "fail"
	EvidenceEmpty = "empty"
	EvidenceNone  = "none"
)

const (
	// WorkTested and WorkMismatch are work log kinds: a commit's test
	// evidence (read from test_evidence), and something said that disagrees
	// with Waypoint's records (stored).
	WorkTested   = "tested"
	WorkMismatch = "mismatch"

	maxEvidenceRuns     = 10
	maxEvidencePackages = 500
	maxEvidenceFailures = 20
	maxEvidenceCommand  = 4000
	maxEvidenceShown    = 80
	maxEvidenceHash     = 64
	maxStatementText    = 300
	// testStatementFresh: how long after a ship an agent's "tests pass" is still
	// about that shipped tree; later it may be about newer work.
	testStatementFresh = 30 * time.Minute
)

// TestRun is one wp testsum run, as the CLI kept it. The request decoder
// refuses a field it doesn't know, so this has every field of the CLI's
// testRun (WP-802).
type TestRun struct {
	At       int64    `json:"at"` // ms
	Tree     string   `json:"tree,omitempty"`
	Moved    bool     `json:"moved,omitempty"` // the tree changed during the run
	Command  string   `json:"command"`
	Outcome  string   `json:"outcome"` // pass, fail, empty
	Passed   int      `json:"passed"`
	Failed   int      `json:"failed"`
	Skipped  int      `json:"skipped"`
	Units    int      `json:"units"`
	UnitName string   `json:"unit_name,omitempty"`
	Packages []string `json:"packages,omitempty"`
	Failures []string `json:"failures,omitempty"`
	Seconds  float64  `json:"seconds"`
	Duration float64  `json:"duration"`
	Exit     int      `json:"exit,omitempty"`
}

// TestRunNear is the newest run that wasn't on the shipped tree.
type TestRunNear struct {
	TestRun
	// FilesAway is how many files its tree differs by; -1 when git can't
	// tell (its tree was pruned).
	FilesAway int `json:"files_away"`
}

// TestEvidenceInput is what wp ship sends: the runs matching its head.
type TestEvidenceInput struct {
	Repo    string       `json:"repo"`
	SHA     string       `json:"sha"`
	Tree    string       `json:"tree"`
	Issue   string       `json:"issue"`
	Summary string       `json:"summary"` // the ship's -m, checked against the records
	Runs    []TestRun    `json:"runs"`
	Nearest *TestRunNear `json:"nearest"`
	Bench   []BenchRun   `json:"bench,omitempty"` // wp bench results on the tree (WP-935)
}

// TestEvidence is a commit's record.
type TestEvidence struct {
	ID      int64      `json:"id"`
	Repo    string     `json:"repo"`
	SHA     string     `json:"sha"`
	Tree    string     `json:"tree"`
	Outcome string     `json:"outcome"`
	Line    string     `json:"line"` // what it says, in one line
	Runs    []TestRun  `json:"runs,omitempty"`
	Bench   []BenchRun `json:"bench,omitempty"`
	// BenchLines say each timing in a line, as the work log shows it.
	BenchLines []string  `json:"bench_lines,omitempty"`
	At         time.Time `json:"at"`
	// Notices are what the summary said that the records contradict.
	Notices []string `json:"notices,omitempty"`
	// CITests: the repo's CI ran tests in the last month (WP-1080), so wp
	// ship leaves out the line saying none ran here. Set only on recording.
	CITests bool `json:"ci_tests,omitempty"`
}

// flatRunes puts s on a single line of at most n runes.
func flatRunes(s string, n int) string { return clipRunes(strings.Join(strings.Fields(s), " "), n) }

func (r *TestRun) clean() error {
	switch r.Outcome {
	case EvidencePass, EvidenceFail, EvidenceEmpty:
	default:
		return invalid("a run's outcome must be pass, fail or empty")
	}
	if r.Passed < 0 || r.Failed < 0 || r.Skipped < 0 || r.Units < 0 {
		return invalid("a run's counts can't be negative")
	}
	if len(r.Tree) > maxEvidenceHash {
		return invalid("a run's tree is too long")
	}
	r.Command = flatRunes(r.Command, maxEvidenceCommand)
	r.UnitName = clipRunes(r.UnitName, 20)
	if len(r.Packages) > maxEvidencePackages {
		r.Packages = r.Packages[:maxEvidencePackages]
	}
	if len(r.Failures) > maxEvidenceFailures {
		r.Failures = r.Failures[:maxEvidenceFailures]
	}
	for i := range r.Failures {
		r.Failures[i] = flatRunes(r.Failures[i], 200)
	}
	return nil
}

// runCount is "1 file", "1,664 files".
func runCount(n int, one string) string { return groupThousands(n) + " " + one + plural(n) }

func groupThousands(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func unitWords(r TestRun) string {
	if r.Units == 0 {
		return ""
	}
	name := strings.TrimSuffix(cmp(r.UnitName, "packages"), "s")
	return runCount(r.Units, name)
}

// runSize orders runs: the one that ran the most says the most.
func runSize(r TestRun) int { return r.Passed + r.Failed + r.Skipped + r.Units }

// summarizeEvidence is the record's outcome and line: the newest run of each
// command counts, a failure among them first, else the widest.
func summarizeEvidence(runs []TestRun, near *TestRunNear, now int64) (string, string) {
	latest := map[string]TestRun{}
	for _, r := range runs {
		if old, ok := latest[r.Command]; !ok || r.At >= old.At {
			latest[r.Command] = r
		}
	}
	if len(latest) == 0 {
		return EvidenceNone, noRunLine(near, now)
	}
	picked := make([]TestRun, 0, len(latest))
	var rerun *TestRun
	for _, r := range latest {
		if r.Outcome == EvidenceFail && passedLater(r, latest) {
			if rerun == nil || r.At > rerun.At {
				rerun = &r
			}
			continue
		}
		picked = append(picked, r)
	}
	slices.SortFunc(picked, func(a, b TestRun) int {
		if c := runSize(b) - runSize(a); c != 0 {
			return c
		}
		return strings.Compare(a.Command, b.Command)
	})
	head := picked[0]
	outcome := EvidenceEmpty
	for _, r := range picked {
		if r.Outcome == EvidencePass {
			outcome = EvidencePass
		}
	}
	for _, r := range picked {
		if r.Outcome == EvidenceFail {
			head, outcome = r, EvidenceFail
			break
		}
	}
	if outcome == EvidencePass && head.Outcome != EvidencePass {
		for _, r := range picked {
			if r.Outcome == EvidencePass {
				head = r
				break
			}
		}
	}
	more := ""
	if n := len(picked) - 1; n > 0 {
		more = "; " + runCount(n, "other run")
	}
	if rerun != nil {
		more += "; an earlier run failed"
		if len(rerun.Failures) > 0 {
			more += " (" + rerun.Failures[0] + ")"
		}
		more += " and a later one passed"
	}
	return outcome, evidenceLine(outcome, head) + more
}

// reTimedOut is how wp testsum names a package go test stopped at its
// -timeout (cmd/wp/testsum.go goTimeoutFailure).
var reTimedOut = lazyre.New(` \(timed out after \S+\)$`)

// passedLater: f failed only by running out of time, and a later run of
// the tree passed having run at least what f did, its packages and as many
// tests: a package that hit go test's time limit on a busy machine and
// passed with a longer -timeout (WP-883). A narrower later run (one -run,
// one package) doesn't clear it, nor does anything clear a failing test or
// a command that failed, which a later run of other commands can't speak
// for.
func passedLater(f TestRun, latest map[string]TestRun) bool {
	if len(f.Failures) == 0 {
		return false
	}
	for _, name := range f.Failures {
		if !reTimedOut.MatchString(name) {
			return false
		}
	}
	if len(f.Packages) == 0 {
		return false
	}
	for _, r := range latest {
		if r.Outcome != EvidencePass || r.At <= f.At || r.Passed+r.Skipped < f.Passed+f.Failed+f.Skipped || r.Units < f.Units {
			continue
		}
		has := map[string]bool{}
		for _, p := range r.Packages {
			has[p] = true
		}
		covers := true
		for _, p := range f.Packages {
			if !has[p] {
				covers = false
				break
			}
		}
		if covers {
			return true
		}
	}
	return false
}

func runCounts(r TestRun) string {
	var bits []string
	if r.Failed > 0 {
		bits = append(bits, groupThousands(r.Failed)+" failed")
	}
	if r.Passed > 0 {
		bits = append(bits, groupThousands(r.Passed)+" passed")
	}
	if r.Skipped > 0 {
		bits = append(bits, groupThousands(r.Skipped)+" skipped")
	}
	return strings.Join(bits, ", ")
}

func evidenceLine(outcome string, r TestRun) string {
	how := ""
	if r.Command != "" {
		how = " (" + clipRunes(r.Command, maxEvidenceShown) + ")"
	}
	counts, units := runCounts(r), unitWords(r)
	switch outcome {
	case EvidenceEmpty:
		return "No test ran on this tree" + how + ": nothing matched"
	case EvidenceFail:
		s := "Tests on this tree: " + cmp(counts, "the command failed")
		if units != "" && counts != "" {
			s += " in " + units
		}
		s += how
		if len(r.Failures) > 0 {
			s += ": " + strings.Join(r.Failures[:min(3, len(r.Failures))], ", ")
		}
		return s
	}
	if counts == "" {
		return "Tests on this tree: " + cmp(units+" passed", "passed") + how
	}
	if units != "" {
		counts += " in " + units
	}
	return "Tests on this tree: " + counts + how
}

func noRunLine(near *TestRunNear, now int64) string {
	if near == nil {
		return "No test run recorded on this tree"
	}
	away := "the last ran on another tree"
	switch {
	case near.Moved:
		away = "the last ran while its files were changing"
	case near.FilesAway > 0:
		away = "the last was before " + runCount(near.FilesAway, "file") + " changed"
	}
	state := runCounts(near.TestRun)
	if near.Outcome == EvidenceEmpty {
		state = "no test ran"
	}
	if state == "" {
		state = cmp(unitWords(near.TestRun), "ran")
	}
	if near.At > 0 && now > near.At {
		state += ", " + shortAgo(time.Duration(now-near.At)*time.Millisecond) + " ago"
	}
	return fmt.Sprintf("No test run on this tree; %s (%s)", away, state)
}

func shortAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return runCount(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		return runCount(int(d.Hours()), "hour")
	}
	return runCount(int(d.Hours()/24), "day")
}

// RecordTestEvidence keeps a commit's test evidence, replacing what an
// earlier ship of the same commit said, and checks what the ship's summary says.
func (s *Service) RecordTestEvidence(ctx context.Context, actor *Actor, projectRef string, in TestEvidenceInput) (*TestEvidence, error) {
	in.Repo, in.SHA, in.Tree = strings.TrimSpace(in.Repo), strings.TrimSpace(in.SHA), strings.TrimSpace(in.Tree)
	if in.Repo == "" || in.SHA == "" || in.Tree == "" || len(in.SHA) > maxEvidenceHash || len(in.Tree) > maxEvidenceHash || len(in.Repo) > 300 {
		return nil, invalid("repo, sha and tree are required")
	}
	capped := int64(0)
	if len(in.Runs) > maxEvidenceRuns {
		capped += int64(len(in.Runs) - maxEvidenceRuns)
		in.Runs = in.Runs[:maxEvidenceRuns]
	}
	for i := range in.Runs {
		if err := in.Runs[i].clean(); err != nil {
			return nil, err
		}
	}
	if in.Nearest != nil {
		if err := in.Nearest.clean(); err != nil {
			return nil, err
		}
	}
	if len(in.Bench) > maxEvidenceBench {
		capped += int64(len(in.Bench) - maxEvidenceBench)
		in.Bench = in.Bench[:maxEvidenceBench]
	}
	for i := range in.Bench {
		if err := in.Bench[i].clean(); err != nil {
			return nil, err
		}
	}
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	p, err := s.GetProject(ctx, actor, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleEditor); err != nil {
		return nil, err
	}
	var out *TestEvidence
	err = s.write(ctx, actor, func(t *tx) error {
		now := s.nowMS()
		outcome, line := summarizeEvidence(in.Runs, in.Nearest, now)
		var issue *Issue
		if key := strings.TrimSpace(in.Issue); key != "" {
			if i, _, err := s.getIssueFor(ctx, t, actor, key, RoleEditor, false); err == nil {
				issue = i
			}
		}
		runs, err := json.Marshal(in.Runs)
		if err != nil {
			return err
		}
		bench, err := json.Marshal(in.Bench)
		if err != nil {
			return err
		}
		if in.Bench == nil {
			bench = []byte("[]")
		}
		var near []byte
		if in.Nearest != nil {
			if near, err = json.Marshal(in.Nearest); err != nil {
				return err
			}
		}
		var issueID, sess, uid any
		if issue != nil {
			issueID = issue.ID
		}
		if actor.Session != 0 {
			sess = actor.Session
		}
		if actor.id() != 0 {
			uid = actor.id()
		}
		var id int64
		if err := t.QueryRowContext(ctx, `INSERT INTO test_evidence(project_id, issue_id, repo, sha, tree, outcome, line, runs, nearest, bench, session_id, user_id, agent, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(project_id, repo, sha) DO UPDATE SET issue_id = coalesce(excluded.issue_id, issue_id), tree = excluded.tree, outcome = excluded.outcome, line = excluded.line,
				runs = excluded.runs, nearest = excluded.nearest, bench = excluded.bench, session_id = excluded.session_id, user_id = excluded.user_id, agent = excluded.agent, updated_at = excluded.updated_at
			RETURNING id`,
			p.ID, issueID, in.Repo, in.SHA, in.Tree, outcome, line, string(runs), string(near), string(bench), sess, uid, actor.agentName(), now, now).Scan(&id); err != nil {
			return err
		}
		if err := addDropRow(ctx, t, actor.id(), "server", DropEvidenceRunsCap, "test_evidence", capped, now); err != nil {
			return err
		}
		out = &TestEvidence{ID: id, Repo: in.Repo, SHA: in.SHA, Tree: in.Tree, Outcome: outcome, Line: line, Runs: in.Runs, Bench: in.Bench, BenchLines: benchLines(in.Bench), At: msTime(now)}
		if issue == nil {
			return nil
		}
		t.emit(&Event{Kind: EvIssueWorkLog, ProjectID: issue.ProjectID, IssueID: issue.ID, IssueKey: issue.Key, New: WorkTested, Data: map[string]any{"evidence": id, "state": WorkTested}})
		found, err := s.checkStatements(ctx, t, statementScope{issue: issue.ID, project: issue.ProjectID, key: issue.Key, session: actor.Session, agent: actor.agentName(), source: "ship", testsFresh: true}, in.Summary, now)
		if err != nil {
			return err
		}
		for _, c := range found {
			out.Notices = append(out.Notices, c.notice())
		}
		return nil
	})
	if err == nil && out != nil && out.Outcome == EvidenceNone {
		out.CITests = s.repoCIRunsTests(ctx, out.ID, in.Repo)
	}
	return out, err
}

// ciTestsLookback is how far back repoCIRunsTests looks.
const ciTestsLookback = 30 * 24 * time.Hour

// testWords is SQL true when column col names tests: a word starting with
// "test" (test, tests, testing) or a runner's name. It isn't fooled by
// "latest", "attestation", "contest" or "majestic" (audit 1000.4).
func testWords(col string) string {
	n := "(' ' || replace(replace(replace(lower(" + col + "), '-', ' '), '_', ' '), '.', ' ') || ' ')"
	return "(" + n + " LIKE '% test%' OR " + n + " LIKE '% pytest%' OR " + n + " LIKE '% unittest%' OR " + n + " LIKE '% jest%' OR " + n + " LIKE '% vitest%')"
}

// repoCIRunsTests says whether repo's CI ran tests in the last month: test
// results its jobs printed (WP-704), go test packages read from its logs,
// or a finished job or workflow named for tests. A repo Waypoint has no CI
// history for doesn't.
func (s *Service) repoCIRunsTests(ctx context.Context, evidenceID int64, repo string) bool {
	since := s.now().Add(-ciTestsLookback).UnixMilli()
	const forges = `(SELECT pr.forge_id FROM project_repos pr WHERE pr.repo = ? COLLATE NOCASE
		AND pr.project_id = (SELECT project_id FROM test_evidence WHERE id = ?))`
	var one int
	err := s.db.R.QueryRowContext(ctx, `SELECT 1 WHERE
		EXISTS (SELECT 1 FROM ci_jobs WHERE started_at >= ? AND repo = ? COLLATE NOCASE AND forge_id IN `+forges+` AND status IN ('success', 'failure')
			AND (`+testWords("name")+` OR `+testWords("workflow")+`))
		OR EXISTS (SELECT 1 FROM ci_job_packages WHERE repo = ? COLLATE NOCASE AND forge_id IN `+forges+` AND at >= ?)
		OR EXISTS (SELECT 1 FROM ci_tests t JOIN ci_test_runs r ON r.test_id = t.id WHERE t.repo = ? COLLATE NOCASE AND t.forge_id IN `+forges+` AND r.at >= ?)`,
		since, repo, repo, evidenceID, repo, repo, evidenceID, since, repo, repo, evidenceID, since).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read repo ciruns tests", "evidenceID", evidenceID, "repo", repo, "err", err)
		}
		return false
	}
	return true
}

// IssueEvidence is the task's newest test evidence, for wp show and the
// issue page; nil when there is none.
func (s *Service) issueEvidence(ctx context.Context, q queryer, issueID int64) (*TestEvidence, error) {
	return scanEvidence(q.QueryRowContext(ctx, evidenceSelect+` WHERE issue_id = ? ORDER BY updated_at DESC, id DESC LIMIT 1`, issueID))
}

const evidenceSelect = `SELECT id, repo, sha, tree, outcome, line, runs, bench, updated_at FROM test_evidence`

func scanEvidence(row *sql.Row) (*TestEvidence, error) {
	e := &TestEvidence{}
	var runs, bench string
	var at int64
	if err := row.Scan(&e.ID, &e.Repo, &e.SHA, &e.Tree, &e.Outcome, &e.Line, &runs, &bench, &at); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	e.At = msTime(at)
	_ = json.Unmarshal([]byte(runs), &e.Runs)
	_ = json.Unmarshal([]byte(bench), &e.Bench)
	e.BenchLines = benchLines(e.Bench)
	return e, nil
}

// prEvidenceScope keeps a pull request to the records of the projects its
// repo belongs to: the same name on another forge is another repo.
const prEvidenceScope = `project_id IN (SELECT r.project_id FROM project_repos r JOIN pull_requests p ON r.forge_id = p.forge_id AND r.repo = p.repo COLLATE NOCASE WHERE p.id = ?) AND repo = ? COLLATE NOCASE`

// prEvidenceLine says what is known about a pull request's head: its own
// record, else the newest for one of its tasks, marked as an older commit.
func (s *Service) prEvidenceLine(ctx context.Context, q queryer, repo, head string, id int64) (string, error) {
	var e *TestEvidence
	var err error
	if head != "" {
		e, err = scanEvidence(q.QueryRowContext(ctx, evidenceSelect+` WHERE `+prEvidenceScope+` AND sha = ? ORDER BY updated_at DESC LIMIT 1`, id, repo, head))
		if err != nil || e != nil {
			if e == nil {
				return "", err
			}
			return strings.Join(append([]string{e.Line}, e.BenchLines...), "\n"), nil
		}
	}
	e, err = scanEvidence(q.QueryRowContext(ctx, evidenceSelect+` WHERE `+prEvidenceScope+` AND issue_id IN (
		SELECT el.issue_id FROM external_links el JOIN pull_requests p ON el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number)) WHERE p.id = ?)
		ORDER BY updated_at DESC LIMIT 1`, id, repo, id))
	if err != nil || e == nil {
		return "", err
	}
	return fmt.Sprintf("No test record for head %s; at %s: %s", short(head), short(e.SHA), e.Line), nil
}

// testedWorkLog adds the task's test evidence to its work log.
func (s *Service) testedWorkLog(ctx context.Context, wl *WorkLog, issueID int64) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT sha, outcome, line, runs, bench, updated_at FROM test_evidence WHERE issue_id = ? ORDER BY updated_at, id`, issueID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sha, outcome, line, raw, rawBench string
		var at int64
		if err := rows.Scan(&sha, &outcome, &line, &raw, &rawBench, &at); err != nil {
			return err
		}
		var runs []TestRun
		_ = json.Unmarshal([]byte(raw), &runs)
		var cmds []string
		for _, r := range runs {
			if r.Command != "" {
				cmds = append(cmds, r.Command)
			}
		}
		var bench []BenchRun
		_ = json.Unmarshal([]byte(rawBench), &bench)
		cmds = append(cmds, benchLines(bench)...)
		wl.Entries = append(wl.Entries, &WorkLogEntry{Kind: WorkTested, At: msTime(at), State: outcome, Title: line + " · " + short(sha), Body: strings.Join(cmds, "\n"), By: "waypoint"})
	}
	return rows.Err()
}

type statementScope struct {
	issue, project int64
	key            string
	session        int64
	agent          string
	source         string
	testsFresh     bool
}

// StatementMismatch is something said that the records contradict.
type StatementMismatch struct {
	Kind string
	Said string // the sentence
	Fact string // what the records say
}

func (c StatementMismatch) notice() string {
	return fmt.Sprintf("said %q, but %s", c.Said, c.Fact)
}

var (
	stmtCodeBlock = lazyre.New("(?s)```.*?```")
	stmtInline    = lazyre.New("`[^`\n]*`")
	// stmtHedge: a sentence about what will, may or didn't happen says
	// nothing about what is.
	stmtHedge = lazyre.New(`(?i)\b(?:not|never|no|won't|will|would|once|after|before|until|when|while|if|unless|pending|should|must|needs?|can|could|may|might|yet|expected?|ready to|about to|to be|wait(?:ing|s)?|hasn't|isn't|wasn't|haven't|didn't|doesn't|don't|couldn't|shouldn't)\b|\?`)
	stmtCI    = lazyre.New(`(?i)\bCI\b|\bpipeline\b`)
	stmtKey   = lazyre.New(`\b[A-Z][A-Z0-9]{1,9}-\d+\b|\bR-\d+\b`)

	stmtTests      = lazyre.New(`(?i)\b(?:(?:all|the|unit|go|backend|full)\s+)*tests?(?:\s+suite)?\s+(?:(?:all|now|still)\s+)*(?:pass(?:ed|es|ing)?\b|(?:are|is|were|was)\s+(?:all\s+)?(?:green|passing)\b|green\b)|\bmake check\s+(?:pass(?:ed|es)\b|is green\b|green\b)`)
	stmtPRNamed    = lazyre.New(`(?i)\b(?:PR|pull request)\s*#?(\d+)\s+(?:(?:is|was|has been|got|been)\s+)?merged\b|\bmerged\s+(?:(?:PR|pull request)\s*)?#(\d+)\b`)
	stmtItMerged   = lazyre.New(`(?i)\b(?:it|this|the (?:PR|pull request|change|task|fix))\s+(?:is|was|has been|got)\s+merged\b`)
	stmtDeployed   = lazyre.New(`(?i)\b(?:is|are|was|were|been|now|got|already)\s+(?:now\s+)?(?:deployed\b|live (?:in|on) prod(?:uction)?\b)|\bdeployed to prod(?:uction)?\b`)
	stmtApplied    = lazyre.New(`(?i)\bapplied\s+(?:[A-Z0-9]+:)?R-(\d+)\b|\bR-(\d+)\s+(?:(?:is|was|has been|got)\s+)?applied\b`)
	stmtNotApplied = lazyre.New(`(?i)\bR-(\d+)\s+(?:is\s*n.t|was\s*n.t|has\s*n.t\s+been|is\s+not|was\s+not|not)\s+(?:yet\s+)?(?:been\s+)?applied\b`)
)

// stmtRequestNotApplied: states of a request "applied R-N" can't be true of.
// Approved isn't one: only a change is applied by Waypoint and it goes
// straight to applying, so an approved request is one the agent may carry out.
var stmtRequestNotApplied = []string{RequestOpen, RequestAnswered, RequestDenied, RequestDismissed, RequestWithdrawn, RequestApplying, RequestFailed, RequestConflict, RequestExpired}

type foundStatement struct {
	kind, text, ref string
	negative        bool
}

// findStatements picks the sentences of text that state a fact we can check.
// Quoted lines and code are the words of others; hedged sentences state nothing.
func findStatements(text string) []foundStatement {
	text = stmtCodeBlock.ReplaceAllString(text, " ")
	text = stmtInline.ReplaceAllString(text, "")
	var out []foundStatement
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), ">") {
			continue
		}
		for _, sent := range splitSentences(l) {
			if m := stmtNotApplied.FindStringSubmatch(sent); m != nil {
				out = append(out, foundStatement{kind: "applied", text: sent, ref: m[1], negative: true})
				continue
			}
			if stmtHedge.MatchString(sent) {
				continue
			}
			if m := stmtApplied.FindStringSubmatch(sent); m != nil {
				out = append(out, foundStatement{kind: "applied", text: sent, ref: cmp(m[1], m[2])})
			}
			if m := stmtPRNamed.FindStringSubmatch(sent); m != nil {
				out = append(out, foundStatement{kind: "merged", text: sent, ref: cmp(m[1], m[2])})
			} else if stmtItMerged.MatchString(sent) {
				out = append(out, foundStatement{kind: "merged", text: sent})
			}
			if stmtDeployed.MatchString(sent) {
				out = append(out, foundStatement{kind: "deployed", text: sent})
			}
			if stmtTests.MatchString(sent) && !stmtCI.MatchString(sent) {
				out = append(out, foundStatement{kind: "tests", text: sent})
			}
			if stmtFaster.MatchString(sent) && !stmtCI.MatchString(sent) {
				out = append(out, foundStatement{kind: "faster", text: sent})
			}
		}
	}
	return out
}

type stmtPR struct {
	number int
	state  string
}

func stmtPRs(ctx context.Context, q queryer, issueID int64) ([]stmtPR, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT p.number, p.state FROM pull_requests p
		JOIN external_links el ON el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number))
		WHERE el.issue_id = ? ORDER BY p.number`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stmtPR
	for rows.Next() {
		var p stmtPR
		if err := rows.Scan(&p.number, &p.state); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func unmergedFact(prs []stmtPR) string {
	var names []string
	for _, p := range prs {
		names = append(names, fmt.Sprintf("#%d is %s", p.number, p.state))
	}
	return "pull request " + strings.Join(names, ", ")
}

// checkStatements compares what text says with the records, and keeps each
// new mismatch in the task's work log. What the records can't speak to is
// left alone: this is a fact check, not a judge.
func (s *Service) checkStatements(ctx context.Context, t *tx, sc statementScope, text string, now int64) ([]StatementMismatch, error) {
	said := findStatements(text)
	if len(said) == 0 {
		return nil, nil
	}
	prs, err := stmtPRs(ctx, t, sc.issue)
	if err != nil {
		return nil, err
	}
	merged := slices.ContainsFunc(prs, func(p stmtPR) bool { return p.state == "merged" })
	var out []StatementMismatch
	add := func(c foundStatement, fact string) {
		out = append(out, StatementMismatch{Kind: c.kind, Said: excerpt(c.text, maxStatementText), Fact: fact})
	}
	for _, c := range said {
		switch c.kind {
		case "merged":
			if c.ref == "" {
				if len(prs) > 0 && !merged {
					add(c, unmergedFact(prs))
				}
				continue
			}
			for _, p := range prs {
				if fmt.Sprint(p.number) == c.ref && p.state != "merged" {
					add(c, unmergedFact([]stmtPR{p}))
				}
			}
		case "deployed":
			if keys := stmtKey.FindAllString(c.text, -1); len(prs) > 0 && !merged && !slices.ContainsFunc(keys, func(k string) bool { return k != sc.key }) {
				add(c, unmergedFact(prs)+", so it can't be deployed")
			}
		case "applied":
			var state, result string
			err := t.QueryRowContext(ctx, `SELECT state, result FROM requests WHERE project_id = ? AND number = ?`, sc.project, c.ref).Scan(&state, &result)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			} else if err != nil {
				return nil, err
			}
			var res RequestResult
			_ = json.Unmarshal([]byte(result), &res)
			switch {
			case state == RequestApplied && res.NoChange:
				if !c.negative {
					add(c, fmt.Sprintf("R-%s was approved but changed nothing", c.ref))
				}
			case c.negative && (state == RequestApplied || state == RequestDone):
				add(c, fmt.Sprintf("R-%s is %s", c.ref, state))
			case !c.negative && slices.Contains(stmtRequestNotApplied, state):
				add(c, fmt.Sprintf("R-%s is %s", c.ref, state))
			}
		case "tests":
			e, err := s.issueEvidence(ctx, t, sc.issue)
			if err != nil {
				return nil, err
			}
			if e == nil || e.Outcome == EvidencePass || e.Outcome == EvidenceNone || (!sc.testsFresh && now-e.At.UnixMilli() > testStatementFresh.Milliseconds()) {
				continue
			}
			add(c, "Waypoint's record at "+short(e.SHA)+": "+e.Line)
		case "faster":
			e, err := s.issueEvidence(ctx, t, sc.issue)
			if err != nil {
				return nil, err
			}
			if e == nil || (!sc.testsFresh && now-e.At.UnixMilli() > testStatementFresh.Milliseconds()) {
				continue
			}
			if fact := benchFact(e.SHA, e.Bench); fact != "" {
				add(c, fact)
			}
		}
	}
	kept := out[:0]
	for _, m := range out {
		var seen int
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM work_log WHERE issue_id = ? AND kind = ? AND body = ? AND reason = ?`, sc.issue, WorkMismatch, m.Said, m.Fact).Scan(&seen); err != nil {
			return nil, err
		}
		if seen > 0 {
			continue
		}
		var sess any
		if sc.session != 0 {
			sess = sc.session
		}
		res, err := t.ExecContext(ctx, `INSERT INTO work_log(issue_id, kind, tag, session_id, agent, body, reason, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sc.issue, WorkMismatch, m.Kind, sess, sc.agent, m.Said, m.Fact, now, now)
		if err != nil {
			return nil, err
		}
		entry, _ := res.LastInsertId()
		t.emit(&Event{Kind: EvIssueWorkLog, ProjectID: sc.project, IssueID: sc.issue, IssueKey: sc.key, New: WorkMismatch, Data: map[string]any{"entry": entry, "state": WorkMismatch, "source": sc.source}})
		kept = append(kept, m)
	}
	return kept, nil
}

// statementsAtStop checks a session's last message against its task's records.
func (s *Service) statementsAtStop(ctx context.Context, t *tx, x *AgentSession, msg string, at int64) error {
	if len(findStatements(msg)) == 0 {
		return nil
	}
	var issue, project int64
	var key string
	err := t.QueryRowContext(ctx, `SELECT i.id, i.project_id, p.key || '-' || i.number FROM agent_runs r JOIN issues i ON i.id = r.issue_id JOIN projects p ON p.id = i.project_id
		WHERE r.session_id = ? ORDER BY r.id DESC LIMIT 1`, x.ID).Scan(&issue, &project, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	_, err = s.checkStatements(ctx, t, statementScope{issue: issue, project: project, key: key, session: x.ID, agent: x.Agent, source: "stop"}, msg, at)
	return err
}

// EvidenceWeek counts a week's ships (Monday, UTC) for wp worklog stats.
type EvidenceWeek struct {
	Week string `json:"week"`
	// Ships: commits with a record; OnTree: of them, with a run on the
	// shipped tree. Mismatches: statements the records contradict.
	Ships      int `json:"ships"`
	OnTree     int `json:"on_tree"`
	Mismatches int `json:"mismatches"`
}

func weekOf(ms int64) string {
	d := time.UnixMilli(ms).UTC()
	return d.AddDate(0, 0, -(int(d.Weekday())+6)%7).Format("2006-01-02")
}

// evidenceWeeks sums ships and mismatches per week, newest last.
func (s *Service) evidenceWeeks(ctx context.Context, projects []int64, since int64) ([]EvidenceWeek, error) {
	byWeek := map[string]*EvidenceWeek{}
	week := func(ms int64) *EvidenceWeek {
		k := weekOf(ms)
		if byWeek[k] == nil {
			byWeek[k] = &EvidenceWeek{Week: k}
		}
		return byWeek[k]
	}
	for _, p := range projects {
		rows, err := s.db.R.QueryContext(ctx, `SELECT outcome, created_at FROM test_evidence WHERE project_id = ? AND created_at >= ?`, p, since)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var outcome string
			var at int64
			if err := rows.Scan(&outcome, &at); err != nil {
				_ = rows.Close()
				return nil, err
			}
			w := week(at)
			w.Ships++
			if outcome != EvidenceNone {
				w.OnTree++
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		rows, err = s.db.R.QueryContext(ctx, `SELECT w.created_at FROM work_log w JOIN issues i ON i.id = w.issue_id WHERE i.project_id = ? AND w.kind = ? AND w.created_at >= ?`, p, WorkMismatch, since)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var at int64
			if err := rows.Scan(&at); err != nil {
				_ = rows.Close()
				return nil, err
			}
			week(at).Mismatches++
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	out := make([]EvidenceWeek, 0, len(byWeek))
	for _, w := range byWeek {
		out = append(out, *w)
	}
	slices.SortFunc(out, func(a, b EvidenceWeek) int { return strings.Compare(a.Week, b.Week) })
	return out, nil
}
