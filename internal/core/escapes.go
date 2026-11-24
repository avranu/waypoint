package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Runners trace the fixes because the server has no git; only links from bug tasks count, not fix-worded commit subjects.

const (
	escapesDueMax    = 60
	escapesDuePerRep = 15
	escapesDueSlow   = 5 * time.Second
	escapeLateMax    = 200
	escapeRowsMax    = 5000
	escapeDaysMax    = 3650
	escapeConfident  = 0.5
	contextWeight    = 0.4
)

// Where a bug was found.
const (
	foundReview  = "review"
	foundPRCI    = "pr_ci"
	foundMainCI  = "main_ci"
	foundProdLog = "production_log"
	foundPerson  = "person"
	foundAudit   = "audit"
	foundUse     = "use"
)

const issueIsBugSQL = `(i.type = 'bug' OR i.category = 'bugfix')`

// pullLinkSQL joins a pull request p to a link el. Only for queries whose other
// predicates pick the rows first; the two forms below are for ones that start
// from p or from el.
const pullLinkSQL = `el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number))`

// pullLinkFromPullSQL is pullLinkSQL for queries that start from the pull
// request p (CROSS JOIN external_links el ON ...): SQLite 3.53 scanned every
// link per pull request with the OR form (WP-999).
const pullLinkFromPullSQL = `el.id IN (SELECT id FROM external_links WHERE kind = 'pull' AND url = p.url
	UNION SELECT id FROM external_links WHERE kind = 'pull' AND repo = p.repo COLLATE NOCASE AND number = p.number)`

// pullLinkFromLinkSQL is pullLinkSQL for queries that start from the link el
// (CROSS JOIN pull_requests p ON ...). The OR form made SQLite 3.53 scan every
// pull request per link; this one looks p up by primary key from two index
// searches (WP-999).
const pullLinkFromLinkSQL = `el.kind = 'pull' AND p.id IN (SELECT id FROM pull_requests WHERE url = el.url
	UNION SELECT id FROM pull_requests WHERE repo = el.repo COLLATE NOCASE AND number = el.number)`

// fixUntracedSQL says pull request p still needs a trace: it has none, or the
// last one couldn't place the fix and the merge commit it looked for has
// since changed (a forge can report it after the runner's first look).
const fixUntracedSQL = `NOT EXISTS (SELECT 1 FROM fix_traces ft WHERE ft.pr_id = p.id AND (ft.fix_sha != '' OR ft.tried_merge = p.merge_sha))`

// EscapeDue is a merged fix waiting for its trace.
type EscapeDue struct {
	PR      int64  `json:"pr_id"`
	Repo    string `json:"repo"`
	Number  int    `json:"number"`
	Merge   string `json:"merge"`
	Head    string `json:"head"`
	Base    string `json:"base"`
	Commits int    `json:"commits"`
	FiledAt int64  `json:"filed_at"` // ms: the latest of its bug tasks' creation; each task's own row applies its own
	Dir     string `json:"dir,omitempty"`
}

type escapePlace struct {
	project int64
	dir     string
}

func (s *Service) escapeDuePlaces(ctx context.Context, a *Actor, machine string) ([]escapePlace, error) {
	var out []escapePlace
	if machine == "" {
		projects, err := s.ListProjects(ctx, a, false)
		if err != nil {
			return nil, err
		}
		for _, p := range projects {
			out = append(out, escapePlace{project: p.ID})
		}
		return out, nil
	}
	places, err := s.sessionPlaces(ctx, a.User.ID, jobNeeds{})
	if err != nil {
		return nil, err
	}
	for _, pl := range places {
		if !strings.EqualFold(pl.Runner, machine) {
			continue
		}
		if p, err := s.projectByKey(ctx, pl.ProjectKey); err == nil {
			out = append(out, escapePlace{project: p.ID, dir: pl.Dir})
		}
	}
	return out, nil
}

// EscapesDue lists fixes for a runner to trace, on a machine (each with the
// checkout to use) or for one repo (a person running it in a checkout). Each
// repo gets its own slots: a project attaching a repo no runner has must not
// use up the round.
func (s *Service) EscapesDue(ctx context.Context, a *Actor, machine, repo string, limit int) ([]*EscapeDue, error) {
	out := []*EscapeDue{}
	if err := a.requireHuman("tracing fixes on a machine"); err != nil {
		return nil, err
	}
	if a.User == nil || a.User.ID == 0 || (machine == "" && repo == "") {
		return out, nil
	}
	if limit <= 0 || limit > escapesDueMax {
		limit = escapesDueMax
	}
	// Runs here because the runner's visit is the only regular trigger, and
	// a link can be added long after its pull request was traced.
	began := time.Now()
	var tLate, tPlaces, tDue, tTrace time.Duration
	var nPlaces, nDue int
	defer func() {
		if total := time.Since(began); total > escapesDueSlow {
			s.log.WarnContext(ctx, "escapes due slow", "total", total.Round(time.Millisecond), "late", tLate.Round(time.Millisecond),
				"places", tPlaces.Round(time.Millisecond), "due_queries", tDue.Round(time.Millisecond), "may_trace", tTrace.Round(time.Millisecond),
				"place_rows", nPlaces, "due_rows", nDue, "out_rows", len(out), "machine", machine, "repo", repo)
		}
	}()
	if err := s.fillLateEscapes(ctx, a); err != nil {
		return nil, err
	}
	tLate = time.Since(began)
	mark := time.Now()
	places, err := s.escapeDuePlaces(ctx, a, machine)
	if err != nil {
		return nil, err
	}
	nPlaces = len(places)
	tPlaces = time.Since(mark)
	seen := map[int64]bool{}
	for _, pl := range places {
		if s.requireProjectRole(ctx, s.db.R, a, pl.project, RoleEditor) != nil {
			continue
		}
		mark = time.Now()
		due, err := s.escapesDueIn(ctx, pl.project, repo)
		tDue += time.Since(mark)
		if err != nil {
			return nil, err
		}
		nDue += len(due)
		for _, d := range due {
			if len(out) >= limit || seen[d.PR] {
				continue
			}
			seen[d.PR] = true
			mark = time.Now()
			ok, err := s.mayTraceFix(ctx, a, d.PR)
			tTrace += time.Since(mark)
			if err != nil {
				return nil, err
			}
			if ok {
				d.Dir = pl.dir
				out = append(out, d)
			}
		}
	}
	return out, nil
}

const mayTraceFixSQL = `SELECT DISTINCT i.project_id FROM pull_requests p
	CROSS JOIN external_links el ON ` + pullLinkFromPullSQL + ` JOIN issues i ON i.id = el.issue_id
	WHERE p.id = ? AND ` + issueIsBugSQL

// mayTraceFix says whether a may record this fix's trace. Its result becomes
// the ledger of every project with a bug task on the fix, so a must be able
// to edit them all.
func (s *Service) mayTraceFix(ctx context.Context, a *Actor, pr int64) (bool, error) {
	rows, err := s.db.R.QueryContext(ctx, mayTraceFixSQL, pr)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	var projects []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return false, err
		}
		projects = append(projects, id)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, id := range projects {
		if s.requireProjectRole(ctx, s.db.R, a, id, RoleEditor) != nil {
			return false, nil
		}
	}
	return len(projects) > 0, nil
}

// escapeRepoSQL limits the due and pending queries to one repo. The unary
// plus keeps the planner from driving the join with the repo index: it then
// walked every pull request of the repo once per link, links x pull requests
// (WP-999). It is fixed text.
const escapeRepoSQL = ` AND +p.repo = ? COLLATE NOCASE`

// escapesDueSQL lists a project's merged, untraced fixes of bug tasks, at most
// escapesDuePerRep per repo. It starts from the project's issues: from the
// pull requests it scanned them all, once per project (WP-999). The CROSS
// JOINs pin that order. repoSQL is fixed text.
func escapesDueSQL(repoSQL string) string {
	// The ranking must sit outside the grouping: a window over the join makes the planner scan pull_requests in updated order.
	return `SELECT id, repo, number, merge_sha, head_sha, base_ref, commits, filed FROM (
		SELECT id, repo, number, merge_sha, head_sha, base_ref, commits, filed,
			ROW_NUMBER() OVER (PARTITION BY lower(repo) ORDER BY at DESC, id DESC) AS rn FROM (
		SELECT p.id, p.repo, p.number, p.merge_sha, p.head_sha, p.base_ref, COALESCE(p.commits, 0) AS commits, MAX(i.created_at) AS filed,
			COALESCE(p.merged_at, p.landed_at, p.updated_at) AS at
		FROM issues i CROSS JOIN external_links el ON el.issue_id = i.id
		CROSS JOIN pull_requests p ON ` + pullLinkFromLinkSQL + `
		WHERE i.project_id = ? AND ` + issueIsBugSQL + ` AND (p.state = 'merged' OR p.landed_at IS NOT NULL)` + repoSQL + `
			AND ` + fixUntracedSQL + `
		GROUP BY p.id)) WHERE rn <= ` + itoa(escapesDuePerRep) + ` ORDER BY rn, id DESC`
}

func (s *Service) escapesDueIn(ctx context.Context, project int64, repo string) ([]*EscapeDue, error) {
	args := []any{project}
	repoSQL := ""
	if repo != "" {
		repoSQL = escapeRepoSQL
		args = append(args, repo)
	}
	// #nosec G202 -- repoSQL is fixed text and the cap a constant; values are bound
	rows, err := s.db.R.QueryContext(ctx, escapesDueSQL(repoSQL), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*EscapeDue
	for rows.Next() {
		d := &EscapeDue{}
		if err := rows.Scan(&d.PR, &d.Repo, &d.Number, &d.Merge, &d.Head, &d.Base, &d.Commits, &d.FiledAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// FixIntroducerInput is one candidate cause in an uploaded trace.
type FixIntroducerInput struct {
	Merge  string  `json:"merge"`
	Commit string  `json:"commit"`
	PR     int     `json:"pr"`
	Lines  int     `json:"lines"`
	Share  float64 `json:"share"`
	At     int64   `json:"at"`
}

// FixFileInput is a code file an uploaded trace says the fix changed.
type FixFileInput struct {
	Path  string `json:"path"`
	Lines int    `json:"lines"`
}

// FixTraceInput is what a runner uploads for one fix.
type FixTraceInput struct {
	PR          int64                `json:"pr_id"`
	Machine     string               `json:"machine"`
	TriedMerge  string               `json:"tried_merge"` // the merge commit the runner was told to look for
	Commit      string               `json:"commit"`
	LandedAt    int64                `json:"landed_at"`
	Method      string               `json:"method"`
	Note        string               `json:"note"`
	Lines       int                  `json:"lines"`
	Dropped     int                  `json:"dropped"`
	Introducers []FixIntroducerInput `json:"introducers"`
	Files       []FixFileInput       `json:"files"`
}

func (in *FixTraceInput) validate() error {
	switch in.Method {
	case "szz", "context", "none":
	default:
		return invalid("method must be szz, context or none")
	}
	if len(in.Introducers) > 50 || len(in.Files) > 1000 || len(in.Note) > 2000 || len(in.Machine) > 200 || len(in.Commit) > 64 || len(in.TriedMerge) > 64 {
		return invalid("trace is too large")
	}
	if in.Lines < 0 || in.Dropped < 0 {
		return invalid("lines can't be negative")
	}
	for _, it := range in.Introducers {
		if it.Merge == "" || len(it.Merge) > 64 || len(it.Commit) > 64 || it.Lines < 0 || it.Share < 0 || it.Share > 1 || it.At < 0 {
			return invalid("introducers has an invalid entry")
		}
	}
	for _, f := range in.Files {
		if f.Path == "" || len(f.Path) > 1024 || f.Lines < 0 {
			return invalid("files has an invalid entry")
		}
	}
	return nil
}

// RecordFixTrace stores a runner's trace and the escapes it completes. A
// later upload replaces an earlier one and the escapes made from it.
func (s *Service) RecordFixTrace(ctx context.Context, a *Actor, in FixTraceInput) error {
	if err := a.requireHuman("recording a fix's trace"); err != nil {
		return err
	}
	if err := in.validate(); err != nil {
		return err
	}
	allowed, err := s.mayTraceFix(ctx, a, in.PR)
	if err != nil {
		return err
	}
	if !allowed {
		return notFound("pull request")
	}
	tags, err := json.Marshal(escapeTags(in.Files))
	if err != nil {
		return err
	}
	return s.write(ctx, a, func(t *tx) error {
		var forge int64
		var repo string
		if err := t.QueryRowContext(ctx, `SELECT forge_id, repo FROM pull_requests WHERE id = ?`, in.PR).Scan(&forge, &repo); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM fix_traces WHERE pr_id = ?`, in.PR); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM escapes WHERE pr_id = ?`, in.PR); err != nil {
			return err
		}
		// Proofs of other commits are stale; this commit's own proof is applied again below.
		if _, err := t.ExecContext(ctx, `DELETE FROM fix_proofs WHERE pr_id = ? AND fix_sha != ?`, in.PR, in.Commit); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO fix_traces(pr_id, fix_sha, tried_merge, landed_at, method, note, lines, dropped, tags, machine, traced_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`, in.PR, in.Commit, in.TriedMerge, in.LandedAt, in.Method, in.Note, in.Lines, in.Dropped, string(tags), in.Machine, s.nowMS()); err != nil {
			return err
		}
		for _, it := range in.Introducers {
			intro, err := findIntroPR(ctx, t, in.PR, forge, repo, it)
			if err != nil {
				return err
			}
			conf := it.Share
			if in.Method == "context" {
				conf *= contextWeight
			}
			if _, err := t.ExecContext(ctx, `INSERT INTO fix_trace_introducers(pr_id, merge_sha, commit_sha, intro_pr_id, intro_number, lines, share, confidence, at)
				VALUES (?,?,?,?,?,?,?,?,?)`, in.PR, it.Merge, it.Commit, intro, it.PR, it.Lines, it.Share, conf, it.At); err != nil {
				return err
			}
		}
		for _, f := range in.Files {
			if _, err := t.ExecContext(ctx, `INSERT INTO fix_trace_files(pr_id, path, lines) VALUES (?,?,?)`, in.PR, f.Path, f.Lines); err != nil {
				return err
			}
		}
		if err := applyProof(ctx, t, in.PR); err != nil {
			return err
		}
		return fillEscapes(ctx, t, s.nowMS(), in.PR)
	})
}

// findIntroPR finds the pull request behind a candidate: by what it merged
// as, else by the number in the commit's subject.
func findIntroPR(ctx context.Context, t *tx, fix, forge int64, repo string, it FixIntroducerInput) (any, error) {
	var id int64
	err := t.QueryRowContext(ctx, `SELECT id FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND merge_sha = ? LIMIT 1`, forge, repo, it.Merge).Scan(&id)
	if err == sql.ErrNoRows && it.PR > 0 {
		err = t.QueryRowContext(ctx, `SELECT id FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND number = ? LIMIT 1`, forge, repo, it.PR).Scan(&id)
	}
	if err == sql.ErrNoRows || (err == nil && id == fix) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return id, nil
}

type missingEscape struct {
	issue, project, pr       int64
	textAgent, reporterAgent string
	outside                  bool
	title, description       string
	fixedAt, filedAt         int64
}

type rowQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func missingEscapesSQL(prSQL string) string {
	return `SELECT DISTINCT i.id, i.project_id, p.id, i.text_agent, i.reporter_agent, i.outside, i.title, i.description,
			COALESCE(p.merged_at, p.landed_at, NULLIF(ft.landed_at, 0), ft.traced_at), i.created_at
		FROM fix_traces ft CROSS JOIN pull_requests p ON p.id = ft.pr_id
		CROSS JOIN external_links el ON ` + pullLinkFromPullSQL + ` JOIN issues i ON i.id = el.issue_id
		WHERE ft.fix_sha != '' AND ` + issueIsBugSQL + prSQL + `
			AND NOT EXISTS (SELECT 1 FROM escapes e WHERE e.issue_id = i.id AND e.pr_id = p.id)
		LIMIT ` + itoa(escapeLateMax)
}

// missingEscapes are bug-task links to traced fixes with no escape yet. pr 0
// means any fix.
func missingEscapes(ctx context.Context, q rowQueryer, pr int64) ([]*missingEscape, error) {
	args := []any{}
	prSQL := ""
	if pr != 0 {
		prSQL = ` AND ft.pr_id = ?`
		args = append(args, pr)
	}
	// #nosec G202 -- prSQL is fixed text; values are bound
	rows, err := q.QueryContext(ctx, missingEscapesSQL(prSQL), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*missingEscape
	for rows.Next() {
		m := &missingEscape{}
		if err := rows.Scan(&m.issue, &m.project, &m.pr, &m.textAgent, &m.reporterAgent, &m.outside, &m.title, &m.description, &m.fixedAt, &m.filedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Service) fillLateEscapes(ctx context.Context, a *Actor) error {
	late, err := missingEscapes(ctx, s.db.R, 0)
	if err != nil || len(late) == 0 {
		return err
	}
	// Rows land in every project with a bug task on the fix.
	var prs []int64
	seen := map[int64]bool{}
	for _, m := range late {
		if seen[m.pr] {
			continue
		}
		seen[m.pr] = true
		ok, err := s.mayTraceFix(ctx, a, m.pr)
		if err != nil {
			return err
		}
		if ok {
			prs = append(prs, m.pr)
		}
	}
	if len(prs) == 0 {
		return nil
	}
	return s.write(ctx, a, func(t *tx) error {
		for _, pr := range prs {
			if err := fillEscapes(ctx, t, s.nowMS(), pr); err != nil {
				return err
			}
		}
		return nil
	})
}

// fillEscapes adds the escapes a trace (or a late link to one) completes.
func fillEscapes(ctx context.Context, t *tx, now, pr int64) error {
	todo, err := missingEscapes(ctx, t, pr)
	if err != nil {
		return err
	}
	for _, m := range todo {
		facts, err := findingFactsOf(ctx, t, m)
		if err != nil {
			return err
		}
		stage, detector := classifyFinding(facts)
		var merge string
		var introPR, introAt any
		var conf float64
		var days any
		var at int64
		var intro sql.NullInt64
		// One trace serves bugs filed at different times; only introducers that landed before the bug was filed can have caused it.
		err = t.QueryRowContext(ctx, `SELECT merge_sha, intro_pr_id, confidence, at FROM fix_trace_introducers
			WHERE pr_id = ? AND at <= ? ORDER BY confidence DESC, at DESC, merge_sha LIMIT 1`, m.pr, m.filedAt).Scan(&merge, &intro, &conf, &at)
		switch {
		case err == nil:
			introAt = at
			if intro.Valid {
				introPR = intro.Int64
			}
			days = max(float64(m.fixedAt-at)/float64(24*time.Hour/time.Millisecond), 0)
		case err != sql.ErrNoRows:
			return err
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO escapes(issue_id, pr_id, project_id, stage, detector, intro_merge, intro_pr_id, intro_at, confidence, days_lived, fixed_at, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT (issue_id, pr_id) DO NOTHING`,
			m.issue, m.pr, m.project, stage, detector, merge, introPR, introAt, conf, days, m.fixedAt, now); err != nil {
			return err
		}
	}
	return nil
}

func findingFactsOf(ctx context.Context, t *tx, m *missingEscape) (findingFacts, error) {
	f := findingFacts{textAgent: m.textAgent, reporterAgent: m.reporterAgent, outside: m.outside, title: m.title, description: m.description}
	rows, err := t.QueryContext(ctx, `SELECT l.name FROM issue_labels il JOIN labels l ON l.id = il.label_id WHERE il.issue_id = ?`, m.issue)
	if err != nil {
		return f, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			_ = rows.Close()
			return f, err
		}
		f.labels = append(f.labels, strings.ToLower(n))
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return f, err
	}
	var name sql.NullString
	err = t.QueryRowContext(ctx, `SELECT a.name FROM events e JOIN automations a ON a.id = e.via_automation
		WHERE e.issue_id = ? AND e.kind = ? ORDER BY e.id LIMIT 1`, m.issue, EvIssueCreated).Scan(&name)
	if err != nil && err != sql.ErrNoRows {
		return f, err
	}
	f.automation = name.String
	return f, nil
}

// findingFacts is what is known about how a bug task came to be filed.
type findingFacts struct {
	labels                   []string
	textAgent, reporterAgent string
	outside                  bool
	automation               string
	title, description       string
}

var (
	prodLogWords = lazyre.New(`(?i)\b(server logs?|production logs?|prod logs?|wp server log)\b`)
	warnWord     = lazyre.New(`\bWARN\b`)
	reviewWords  = lazyre.New(`(?i)\b(coderabbit|grok|adversarial review|post-merge review|code review|review bot|review finding)`)
	mainCIWords  = lazyre.New(`(?i)\b(ci on main|main'?s (ci|build)|main (has )?fail|fail\w* on main|broke main|red on main)\b`)
)

// classifyFinding reads where a bug was found and what found it. The labels
// and filers Waypoint sets itself are facts. For text an agent wrote, its
// words are only a hint, so nothing is called pr_ci on words alone and what
// says nothing counts as found in use.
func classifyFinding(f findingFacts) (stage, detector string) {
	switch {
	case slices.Contains(f.labels, "audit"):
		return foundAudit, "audit"
	case slices.Contains(f.labels, "flaky"):
		return foundPRCI, "ci_flake"
	case slices.Contains(f.labels, "ci") && f.textAgent == "waypoint":
		return foundPRCI, "ci_fault"
	}
	if f.automation != "" {
		return stageFromText(f), "automation:" + f.automation
	}
	if f.outside || f.textAgent == "forge" {
		return foundPerson, "forge"
	}
	if f.textAgent == "" && f.reporterAgent == "" {
		return foundPerson, "person"
	}
	if f.textAgent == "waypoint" {
		return stageFromText(f), "waypoint"
	}
	return stageFromText(f), "agent"
}

func stageFromText(f findingFacts) string {
	text := f.title + "\n" + f.description
	switch {
	case prodLogWords.MatchString(text), warnWord.MatchString(text):
		return foundProdLog
	case reviewWords.MatchString(text):
		return foundReview
	case mainCIWords.MatchString(text):
		return foundMainCI
	}
	return foundUse
}

// escapeTags names the classes of a fix from the files it changed.
func escapeTags(files []FixFileInput) []string {
	has := map[string]bool{}
	for _, f := range files {
		p := f.Path
		switch {
		case strings.HasPrefix(p, "internal/core/forge"), strings.HasPrefix(p, "internal/forge"):
			has["forge"] = true
		case strings.HasPrefix(p, "internal/db/migrations"):
			has["migration"] = true
		case strings.HasPrefix(p, ".forgejo/workflows"), strings.HasPrefix(p, ".forgejo/scripts"):
			has["ci"] = true
		case strings.HasPrefix(p, "web/"):
			has["web"] = true
		case strings.HasPrefix(p, "cmd/wp"):
			has["cli"] = true
		}
	}
	out := make([]string, 0, len(has))
	for t := range has {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// EscapeIntroducer is one candidate cause of an escape.
type EscapeIntroducer struct {
	Merge      string    `json:"merge"`
	Commit     string    `json:"commit"`
	Number     int       `json:"number,omitempty"`
	Lines      int       `json:"lines"`
	Share      float64   `json:"share"`
	Confidence float64   `json:"confidence"`
	At         time.Time `json:"at"`
}

// Escape is one bug task's fix in the ledger.
type Escape struct {
	Key         string             `json:"key"`
	Title       string             `json:"title"`
	Repo        string             `json:"repo"`
	Number      int                `json:"number"`
	Stage       string             `json:"stage"`
	Detector    string             `json:"detector"`
	Method      string             `json:"method"`
	Note        string             `json:"note,omitempty"`
	Tags        []string           `json:"tags"`
	IntroMerge  string             `json:"intro_merge,omitempty"`
	IntroNumber int                `json:"intro_number,omitempty"`
	Confidence  float64            `json:"confidence"`
	DaysLived   *float64           `json:"days_lived,omitempty"`
	FixedAt     time.Time          `json:"fixed_at"`
	Introducers []EscapeIntroducer `json:"introducers,omitempty"`
	// Proof: what running the fix's tests without it found (WP-709); ""
	// until a runner proves it.
	Proof       string   `json:"proof,omitempty"`
	ProofFailed []string `json:"proof_failed,omitempty"`
	ProofIntro  string   `json:"proof_intro,omitempty"`
	IntroCheck  string   `json:"intro_check,omitempty"`
	ProofNote   string   `json:"proof_note,omitempty"`

	issue, pr int64
	introPR   int64
	fixedMS   int64
}

func (e *Escape) confident() bool { return e.Confidence >= escapeConfident && e.DaysLived != nil }

// EscapeQuery narrows the ledger.
type EscapeQuery struct {
	Project string
	Repo    string
	Key     string
	Days    int
	Limit   int
}

func (s *Service) escapeProjects(ctx context.Context, a *Actor, key string) ([]int64, error) {
	if key != "" {
		p, err := s.projectByKey(ctx, key)
		if err != nil {
			return nil, notFound("project")
		}
		if err := s.requireProjectRole(ctx, s.db.R, a, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		return []int64{p.ID}, nil
	}
	projects, err := s.ListProjects(ctx, a, true)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, p := range projects {
		ids = append(ids, p.ID)
	}
	return ids, nil
}

// escapeRows loads the ledger, newest first. A task since retyped away from
// bug drops out, and each project is the reader's to see.
func (s *Service) escapeRows(ctx context.Context, a *Actor, q EscapeQuery) ([]*Escape, error) {
	ids, err := s.escapeProjects(ctx, a, q.Project)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	days := q.Days
	if days <= 0 {
		days = 90
	}
	if days > escapeDaysMax {
		days = escapeDaysMax
	}
	limit := q.Limit
	if limit <= 0 || limit > escapeRowsMax {
		limit = escapeRowsMax
	}
	args := append(int64Args(ids), s.now().AddDate(0, 0, -days).UnixMilli())
	where := ""
	if q.Repo != "" {
		where += ` AND p.repo = ? COLLATE NOCASE`
		args = append(args, q.Repo)
	}
	if q.Key != "" {
		cut := strings.LastIndex(q.Key, "-")
		if cut < 1 {
			return nil, invalid("key must look like WP-12")
		}
		where += ` AND pj.key = ? AND i.number = ?`
		args = append(args, q.Key[:cut], q.Key[cut+1:])
	}
	args = append(args, limit)
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT e.issue_id, e.pr_id, pj.key || '-' || i.number, i.title, p.repo, p.number, e.stage, e.detector,
			COALESCE(ft.method, ''), COALESCE(ft.note, ''), COALESCE(ft.tags, '[]'), e.intro_merge, COALESCE(e.intro_pr_id, 0),
			COALESCE(ip.number, (SELECT fi.intro_number FROM fix_trace_introducers fi WHERE fi.pr_id = e.pr_id AND fi.merge_sha = e.intro_merge), 0),
			e.confidence, e.days_lived, e.fixed_at, COALESCE(fp.outcome, ''), COALESCE(fp.failed, '[]'),
			COALESCE(fp.intro_merge, ''), COALESCE(fp.intro_check, ''), COALESCE(fp.note, '')
		FROM escapes e JOIN issues i ON i.id = e.issue_id JOIN projects pj ON pj.id = e.project_id
		JOIN pull_requests p ON p.id = e.pr_id LEFT JOIN fix_traces ft ON ft.pr_id = e.pr_id
		LEFT JOIN pull_requests ip ON ip.id = e.intro_pr_id LEFT JOIN fix_proofs fp ON fp.pr_id = e.pr_id
		WHERE e.project_id IN (`+placeholders(len(ids))+`) AND `+issueIsBugSQL+` AND e.fixed_at >= ?`+where+`
		ORDER BY e.fixed_at DESC, e.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Escape{}
	for rows.Next() {
		e := &Escape{}
		var tags, failed string
		var days sql.NullFloat64
		if err := rows.Scan(&e.issue, &e.pr, &e.Key, &e.Title, &e.Repo, &e.Number, &e.Stage, &e.Detector, &e.Method, &e.Note, &tags,
			&e.IntroMerge, &e.introPR, &e.IntroNumber, &e.Confidence, &days, &e.fixedMS, &e.Proof, &failed, &e.ProofIntro, &e.IntroCheck, &e.ProofNote); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(failed), &e.ProofFailed); err != nil {
			e.ProofFailed = nil
		}
		e.FixedAt = time.UnixMilli(e.fixedMS).UTC()
		if err := json.Unmarshal([]byte(tags), &e.Tags); err != nil {
			e.Tags = nil
		}
		if days.Valid {
			d := days.Float64
			e.DaysLived = &d
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EscapeList is the ledger's rows, with every candidate cause when asked for
// one task.
func (s *Service) EscapeList(ctx context.Context, a *Actor, q EscapeQuery) ([]*Escape, error) {
	if q.Key != "" && q.Days <= 0 {
		q.Days = escapeDaysMax
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	list, err := s.escapeRows(ctx, a, q)
	if err != nil || q.Key == "" {
		return list, err
	}
	for _, e := range list {
		rows, err := s.db.R.QueryContext(ctx, `SELECT merge_sha, commit_sha, COALESCE((SELECT p.number FROM pull_requests p WHERE p.id = fi.intro_pr_id), fi.intro_number),
			lines, share, confidence, at FROM fix_trace_introducers fi WHERE fi.pr_id = ? ORDER BY confidence DESC, at DESC`, e.pr)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var it EscapeIntroducer
			var at int64
			if err := rows.Scan(&it.Merge, &it.Commit, &it.Number, &it.Lines, &it.Share, &it.Confidence, &at); err != nil {
				_ = rows.Close()
				return nil, err
			}
			it.At = time.UnixMilli(at).UTC()
			e.Introducers = append(e.Introducers, it)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return list, nil
}

// EscapeWeek is one week's fixes, by the Monday it starts (UTC).
type EscapeWeek struct {
	Start  string   `json:"start"`
	Count  int      `json:"count"`
	Median *float64 `json:"median_days"`
}

// EscapeGroup is the split by one value of stage, detector or class.
type EscapeGroup struct {
	Name   string   `json:"name"`
	Count  int      `json:"count"`
	Median *float64 `json:"median_days"`
}

// EscapeStats answers "how many, how long, found by what".
type EscapeStats struct {
	Days          int           `json:"days"`
	Total         int           `json:"total"`
	Traced        int           `json:"traced"`
	Confident     int           `json:"confident"`
	MedianDays    *float64      `json:"median_days"`
	MedianAllDays *float64      `json:"median_all_days"`
	Weeks         []EscapeWeek  `json:"weeks"`
	Stages        []EscapeGroup `json:"stages"`
	Detectors     []EscapeGroup `json:"detectors"`
	Tags          []EscapeGroup `json:"tags"`
	Reopens       int           `json:"reopens"`
	FixOfFix      int           `json:"fix_of_fix"`
	Chains        [][]string    `json:"chains"`
	Pending       int           `json:"pending"`
	// Proofs: the fixes by proof outcome, with how many were fixed again
	// (WP-709); ProofsPending of them have no proof yet.
	Proofs        []EscapeProof `json:"proofs"`
	ProofsPending int           `json:"proofs_pending"`
}

func medianDays(v []float64) *float64 {
	if len(v) == 0 {
		return nil
	}
	s := slices.Clone(v)
	slices.Sort(s)
	m := s[len(s)/2]
	if len(s)%2 == 0 {
		m = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return &m
}

func escapeGroups(rows []*Escape, names func(*Escape) []string) []EscapeGroup {
	count := map[string]int{}
	lives := map[string][]float64{}
	for _, e := range rows {
		for _, n := range names(e) {
			count[n]++
			if e.confident() {
				lives[n] = append(lives[n], *e.DaysLived)
			}
		}
	}
	out := make([]EscapeGroup, 0, len(count))
	for n, c := range count {
		out = append(out, EscapeGroup{Name: n, Count: c, Median: medianDays(lives[n])})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// escapeChains follows confident causes that were themselves fixes: a bug
// fixed by a change that had to be fixed in turn. Chains read oldest first.
func escapeChains(rows []*Escape) (int, [][]string) {
	byPR := map[int64]*Escape{}
	for _, e := range rows {
		if byPR[e.pr] == nil {
			byPR[e.pr] = e
		}
	}
	next := map[int64]int64{}
	blamed := map[int64]bool{}
	for _, e := range rows {
		if e.introPR == 0 || !e.confident() || byPR[e.introPR] == nil {
			continue
		}
		next[e.pr] = e.introPR
		blamed[e.introPR] = true
	}
	var chains [][]string
	for pr := range next {
		if blamed[pr] {
			continue
		}
		seen := map[int64]bool{}
		var keys []string
		for cur, ok := pr, true; ok && !seen[cur]; cur, ok = next[cur] {
			seen[cur] = true
			keys = append(keys, byPR[cur].Key)
		}
		slices.Reverse(keys)
		chains = append(chains, keys)
	}
	sort.Slice(chains, func(i, j int) bool {
		if len(chains[i]) != len(chains[j]) {
			return len(chains[i]) > len(chains[j])
		}
		return strings.Join(chains[i], " ") < strings.Join(chains[j], " ")
	})
	if len(chains) > 10 {
		chains = chains[:10]
	}
	return len(next), chains
}

// EscapeSummary is the ledger's numbers over the last q.Days days.
func (s *Service) EscapeSummary(ctx context.Context, a *Actor, q EscapeQuery) (*EscapeStats, error) {
	q.Key, q.Limit = "", escapeRowsMax
	if q.Days <= 0 {
		q.Days = 90
	}
	rows, err := s.escapeRows(ctx, a, q)
	if err != nil {
		return nil, err
	}
	st := &EscapeStats{Days: q.Days, Total: len(rows), Weeks: []EscapeWeek{}, Chains: [][]string{}, Proofs: []EscapeProof{}}
	var confident, all []float64
	type week struct {
		count int
		lives []float64
	}
	weeks := map[string]*week{}
	for _, e := range rows {
		if e.IntroMerge != "" {
			st.Traced++
		}
		if e.DaysLived != nil {
			all = append(all, *e.DaysLived)
		}
		monday := e.FixedAt.AddDate(0, 0, -((int(e.FixedAt.Weekday()) + 6) % 7)).Format("2006-01-02")
		w := weeks[monday]
		if w == nil {
			w = &week{}
			weeks[monday] = w
		}
		w.count++
		if e.confident() {
			st.Confident++
			confident = append(confident, *e.DaysLived)
			w.lives = append(w.lives, *e.DaysLived)
		}
	}
	st.MedianDays, st.MedianAllDays = medianDays(confident), medianDays(all)
	for start, w := range weeks {
		st.Weeks = append(st.Weeks, EscapeWeek{Start: start, Count: w.count, Median: medianDays(w.lives)})
	}
	sort.Slice(st.Weeks, func(i, j int) bool { return st.Weeks[i].Start < st.Weeks[j].Start })
	st.Stages = escapeGroups(rows, func(e *Escape) []string { return []string{e.Stage} })
	st.Detectors = escapeGroups(rows, func(e *Escape) []string { return []string{e.Detector} })
	st.Tags = escapeGroups(rows, func(e *Escape) []string { return e.Tags })
	st.FixOfFix, st.Chains = escapeChains(rows)
	if st.Chains == nil {
		st.Chains = [][]string{}
	}
	st.Proofs, st.ProofsPending = escapeProofs(rows)
	if st.Reopens, err = s.escapeReopens(ctx, rows); err != nil {
		return nil, err
	}
	if st.Pending, err = s.escapesPending(ctx, a, q); err != nil {
		return nil, err
	}
	return st, nil
}

// escapeReopens counts tasks moved back open after their fix landed.
func (s *Service) escapeReopens(ctx context.Context, rows []*Escape) (int, error) {
	first := map[int64]int64{}
	for _, e := range rows {
		if at, ok := first[e.issue]; !ok || e.fixedMS < at {
			first[e.issue] = e.fixedMS
		}
	}
	if len(first) == 0 {
		return 0, nil
	}
	ids := make([]int64, 0, len(first))
	for id := range first {
		ids = append(ids, id)
	}
	args := append([]any{EvIssueStatus, CatDone, CatCanceled, CatDone, CatCanceled}, int64Args(ids)...)
	// #nosec G202 -- only placeholders are added
	res, err := s.db.R.QueryContext(ctx, `SELECT issue_id, created_at FROM events WHERE kind = ?
		AND json_extract(data, '$.old_category') IN (?, ?) AND json_extract(data, '$.new_category') NOT IN (?, ?)
		AND issue_id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Close() }()
	n := 0
	for res.Next() {
		var id, at int64
		if err := res.Scan(&id, &at); err != nil {
			return 0, err
		}
		if at > first[id] {
			n++
		}
	}
	return n, res.Err()
}

// escapesPendingSQL counts the fixes escapesDueSQL would list, in n projects.
func escapesPendingSQL(n int, repoSQL string) string {
	return `SELECT COUNT(DISTINCT p.id) FROM issues i CROSS JOIN external_links el ON el.issue_id = i.id
		CROSS JOIN pull_requests p ON ` + pullLinkFromLinkSQL + `
		WHERE i.project_id IN (` + placeholders(n) + `) AND ` + issueIsBugSQL + ` AND (p.state = 'merged' OR p.landed_at IS NOT NULL)` + repoSQL + `
			AND ` + fixUntracedSQL
}

func (s *Service) escapesPending(ctx context.Context, a *Actor, q EscapeQuery) (int, error) {
	ids, err := s.escapeProjects(ctx, a, q.Project)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	args := int64Args(ids)
	repo := ""
	if q.Repo != "" {
		repo = escapeRepoSQL
		args = append(args, q.Repo)
	}
	n := 0
	// #nosec G202 -- repo is fixed text; values are bound
	err = s.db.R.QueryRowContext(ctx, escapesPendingSQL(len(ids), repo), args...).Scan(&n)
	return n, err
}

// EscapeFile is one file's history in the ledger.
type EscapeFile struct {
	Path    string `json:"path"`
	Escapes int    `json:"escapes"`
	LastAt  int64  `json:"last_at"` // ms
}

// EscapeFiles is what wp code risk reads: per file, the bug fixes that
// touched it, and how many fixes the repo has in the ledger at all.
type EscapeFiles struct {
	Total int          `json:"total"`
	Files []EscapeFile `json:"files"`
}

// EscapeFilesFor reports the ledger's per-file counts for one repo.
func (s *Service) EscapeFilesFor(ctx context.Context, a *Actor, repo string) (*EscapeFiles, error) {
	if repo == "" {
		return nil, invalid("repo is required")
	}
	out := &EscapeFiles{Files: []EscapeFile{}}
	ids, err := s.escapeProjects(ctx, a, "")
	if err != nil || len(ids) == 0 {
		return out, err
	}
	base := `FROM escapes e JOIN issues i ON i.id = e.issue_id JOIN pull_requests p ON p.id = e.pr_id
		WHERE e.project_id IN (` + placeholders(len(ids)) + `) AND ` + issueIsBugSQL + ` AND p.repo = ? COLLATE NOCASE`
	args := append(int64Args(ids), repo)
	// Total gates when the ledger replaces the keyword count; a fix counts only if it has trace files.
	if err := s.db.R.QueryRowContext(ctx, `SELECT COUNT(DISTINCT e.pr_id) `+base+`
		AND EXISTS (SELECT 1 FROM fix_trace_files tf WHERE tf.pr_id = e.pr_id)`, args...).Scan(&out.Total); err != nil {
		return nil, err
	}
	// #nosec G202 -- only placeholders and the fixed bug condition are added; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT f.path, COUNT(DISTINCT e.pr_id), MAX(e.fixed_at) FROM fix_trace_files f
		JOIN (SELECT e.pr_id, e.fixed_at `+base+`) e ON e.pr_id = f.pr_id GROUP BY f.path`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var f EscapeFile
		if err := rows.Scan(&f.Path, &f.Escapes, &f.LastAt); err != nil {
			return nil, err
		}
		out.Files = append(out.Files, f)
	}
	return out, rows.Err()
}

// EscapeShortDays renders a lifetime for text output.
func EscapeShortDays(d *float64) string {
	switch {
	case d == nil:
		return "-"
	case *d < 1:
		return "<1d"
	}
	return fmt.Sprintf("%.0fd", *d)
}
