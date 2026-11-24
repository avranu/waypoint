package core

// Anyone who can see a project the repo is attached to may read its CI evidence.

import (
	stdcmp "cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	ciTestsDefaultDays = 14
	ciTestsLimit       = 15
	ciTestsMaxLimit    = 500
	ciCoverageTrend    = 10
)

// ciEvidenceRepos are the visible repos named by repo (any visible one when
// empty), sorted by name.
func (s *Service) ciEvidenceRepos(ctx context.Context, actor *Actor, repo string) ([]ciRepoRef, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	visible, err := s.ciVisibleRepos(ctx, actor)
	if err != nil {
		return nil, err
	}
	var out []ciRepoRef
	for _, r := range visible {
		if repo == "" || strings.EqualFold(r.repo, repo) {
			out = append(out, r)
		}
	}
	if repo != "" && len(out) == 0 {
		return nil, notFound("repo " + repo + " (in a project you can see)")
	}
	slices.SortFunc(out, func(a, b ciRepoRef) int {
		return stdcmp.Or(stdcmp.Compare(strings.ToLower(a.repo), strings.ToLower(b.repo)), stdcmp.Compare(a.forge.ID, b.forge.ID))
	})
	return out, nil
}

// CITestsInput asks about tests: View flaky, slow or failing ("" for all
// three, shorter), over the last Days days, of Repo (every visible one
// when empty), Package (and the packages under it) when set.
type CITestsInput struct {
	Repo    string
	View    string
	Days    int
	Package string
	Limit   int
}

// CITests is what the kept test results say.
type CITests struct {
	Repo  string     `json:"repo,omitempty"`
	View  string     `json:"view,omitempty"`
	Days  int        `json:"days"`
	Since *time.Time `json:"since,omitempty"` // the oldest kept result of the window
	Jobs  int        `json:"jobs"`            // jobs whose results were kept
	Tests int        `json:"tests"`           // distinct tests
	Runs  int        `json:"runs"`            // results

	Flaky    []CIFlakyTest   `json:"flaky,omitempty"`
	Slow     []CISlowTest    `json:"slow,omitempty"`
	Packages []CISlowPackage `json:"packages,omitempty"`
	Failing  []CIFailingTest `json:"failing,omitempty"`
}

// CIFlakyTest is a test that both passed and failed on one commit in one
// tier. Score is the share of the commits it ran on where it did.
type CIFlakyTest struct {
	Repo      string    `json:"repo"`
	Package   string    `json:"package"`
	Name      string    `json:"name"`
	Commits   int       `json:"commits"`
	Flaky     int       `json:"flaky_commits"`
	Score     float64   `json:"score"`
	Fails     int       `json:"fails"`
	Runs      int       `json:"runs"`
	LastFlake time.Time `json:"last_flake"`
}

// CISlowTest is a test's time in full runs (race detector on), passing.
type CISlowTest struct {
	Repo    string `json:"repo"`
	Package string `json:"package"`
	Name    string `json:"name"`
	Runs    int    `json:"runs"`
	AvgMs   int64  `json:"avg_ms"`
	MaxMs   int64  `json:"max_ms"`
}

// CISlowPackage is a package's tests' average times added up.
type CISlowPackage struct {
	Repo    string `json:"repo"`
	Package string `json:"package"`
	Tests   int    `json:"tests"`
	Ms      int64  `json:"ms"`
}

// CIFailingTest is a test whose newest result on the default branch is a
// failure.
type CIFailingTest struct {
	Repo      string     `json:"repo"`
	Package   string     `json:"package"`
	Name      string     `json:"name"`
	Fails     int        `json:"fails"`      // on the default branch, since it last passed there
	Since     time.Time  `json:"since"`      // the first of those
	LastPass  *time.Time `json:"last_pass"`  // on the default branch, in the window
	LastSHA   string     `json:"last_sha"`   // the newest failing commit
	OtherRefs int        `json:"other_refs"` // failures elsewhere in the window
}

// CITests reads the kept test results.
func (s *Service) CITests(ctx context.Context, actor *Actor, in CITestsInput) (*CITests, error) {
	switch in.View {
	case "", "flaky", "slow", "failing":
	default:
		return nil, invalid("view: flaky, slow or failing")
	}
	days := in.Days
	if days <= 0 {
		days = ciTestsDefaultDays
	}
	if days > int(evidenceKeep/(24*time.Hour)) {
		return nil, invalid("days: at most %d (test results are kept a month)", int(evidenceKeep/(24*time.Hour)))
	}
	limit := in.Limit
	if limit <= 0 {
		limit = ciTestsLimit
		if in.View == "" {
			limit = 5
		}
	}
	limit = min(limit, ciTestsMaxLimit)
	repos, err := s.ciEvidenceRepos(ctx, actor, in.Repo)
	if err != nil {
		return nil, err
	}
	since := s.now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	pkg := strings.Trim(in.Package, "/")
	out := &CITests{Repo: in.Repo, View: in.View, Days: days}
	for _, r := range repos {
		if err := s.ciTestsOf(ctx, r, since, pkg, in.View, out); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(out.Flaky, func(a, b CIFlakyTest) int {
		return stdcmp.Or(stdcmp.Compare(b.Flaky, a.Flaky), stdcmp.Compare(b.Score, a.Score), b.LastFlake.Compare(a.LastFlake), stdcmp.Compare(a.Name, b.Name))
	})
	slices.SortFunc(out.Slow, func(a, b CISlowTest) int {
		return stdcmp.Or(stdcmp.Compare(b.AvgMs, a.AvgMs), stdcmp.Compare(a.Name, b.Name))
	})
	slices.SortFunc(out.Packages, func(a, b CISlowPackage) int {
		return stdcmp.Or(stdcmp.Compare(b.Ms, a.Ms), stdcmp.Compare(a.Package, b.Package))
	})
	slices.SortFunc(out.Failing, func(a, b CIFailingTest) int {
		return stdcmp.Or(a.Since.Compare(b.Since), stdcmp.Compare(a.Name, b.Name))
	})
	out.Flaky = out.Flaky[:min(len(out.Flaky), limit)]
	out.Slow = out.Slow[:min(len(out.Slow), limit)]
	out.Packages = out.Packages[:min(len(out.Packages), limit)]
	out.Failing = out.Failing[:min(len(out.Failing), max(limit, 50))]
	return out, nil
}

// pkgArgs are the arguments of the package condition in the test queries,
// "(? = ” OR t.package = ? OR substr(t.package, 1, ?) = ?)": every package,
// or pkg and those under it.
func pkgArgs(pkg string) []any { return []any{pkg, pkg, len(pkg) + 1, pkg + "/"} }

func (s *Service) ciTestsOf(ctx context.Context, r ciRepoRef, since int64, pkg, view string, out *CITests) error {
	base := append([]any{r.forge.ID, r.repo, since}, pkgArgs(pkg)...)
	var first sql.NullInt64
	var jobs, tests, runs int
	if err := s.db.R.QueryRowContext(ctx, `SELECT min(r.at), count(DISTINCT r.task_id), count(DISTINCT r.test_id), count(*)
		FROM ci_test_runs r JOIN ci_tests t ON t.id = r.test_id WHERE t.forge_id = ? AND t.repo = ? AND r.at >= ? AND (? = '' OR t.package = ? OR substr(t.package, 1, ?) = ?)`, base...).Scan(&first, &jobs, &tests, &runs); err != nil {
		return err
	}
	if first.Valid && (out.Since == nil || msTime(first.Int64).Before(*out.Since)) {
		out.Since = tptr(msTime(first.Int64))
	}
	out.Jobs += jobs
	out.Tests += tests
	out.Runs += runs
	if runs == 0 {
		return nil
	}
	if view == "" || view == "flaky" {
		rows, err := s.db.R.QueryContext(ctx, `WITH per AS (
				SELECT r.test_id, r.sha AS sha, sum(r.outcome = 'pass') AS p, sum(r.outcome = 'fail') AS f, max(r.at) AS at
				FROM ci_test_runs r JOIN ci_tests t ON t.id = r.test_id
				WHERE t.forge_id = ? AND t.repo = ? AND r.at >= ? AND (? = '' OR t.package = ? OR substr(t.package, 1, ?) = ?)
				GROUP BY r.test_id, r.sha, r.tier)
			SELECT t.package, t.name, count(DISTINCT sha), count(DISTINCT CASE WHEN p > 0 AND f > 0 THEN sha END), sum(f), sum(p + f), max(CASE WHEN p > 0 AND f > 0 THEN at END)
			FROM per JOIN ci_tests t ON t.id = per.test_id
			GROUP BY per.test_id HAVING sum(p > 0 AND f > 0) > 0`, base...)
		if err != nil {
			return err
		}
		for rows.Next() {
			x := CIFlakyTest{Repo: r.repo}
			var last int64
			if err := rows.Scan(&x.Package, &x.Name, &x.Commits, &x.Flaky, &x.Fails, &x.Runs, &last); err != nil {
				rows.Close()
				return err
			}
			x.LastFlake = msTime(last)
			x.Score = float64(x.Flaky) / float64(max(x.Commits, 1))
			out.Flaky = append(out.Flaky, x)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	if view == "" || view == "slow" {
		rows, err := s.db.R.QueryContext(ctx, `SELECT t.package, t.name, count(*), CAST(avg(r.ms) AS INTEGER), max(r.ms)
			FROM ci_test_runs r JOIN ci_tests t ON t.id = r.test_id
			WHERE t.forge_id = ? AND t.repo = ? AND r.at >= ? AND (? = '' OR t.package = ? OR substr(t.package, 1, ?) = ?) AND r.tier = '' AND r.outcome = 'pass'
			GROUP BY r.test_id`, base...)
		if err != nil {
			return err
		}
		pkgs := map[string]*CISlowPackage{}
		for rows.Next() {
			x := CISlowTest{Repo: r.repo}
			if err := rows.Scan(&x.Package, &x.Name, &x.Runs, &x.AvgMs, &x.MaxMs); err != nil {
				rows.Close()
				return err
			}
			out.Slow = append(out.Slow, x)
			p := pkgs[x.Package]
			if p == nil {
				p = &CISlowPackage{Repo: r.repo, Package: x.Package}
				pkgs[x.Package] = p
			}
			p.Tests++
			p.Ms += x.AvgMs
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, p := range pkgs {
			out.Packages = append(out.Packages, *p)
		}
	}
	if view == "" || view == "failing" {
		if err := s.ciFailingOf(ctx, r, since, base, out); err != nil {
			return err
		}
	}
	return nil
}

// ciFailingOf adds the tests whose newest default-branch result failed.
func (s *Service) ciFailingOf(ctx context.Context, r ciRepoRef, since int64, base []any, out *CITests) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.test_id, t.package, t.name, r.outcome, r.at, r.sha
		FROM ci_test_runs r JOIN ci_tests t ON t.id = r.test_id
		WHERE t.forge_id = ? AND t.repo = ? AND r.at >= ? AND (? = '' OR t.package = ? OR substr(t.package, 1, ?) = ?) AND r.on_default = 1 AND r.outcome IN ('pass', 'fail')
		ORDER BY r.test_id, r.at DESC`, base...)
	if err != nil {
		return err
	}
	type acc struct {
		x    CIFailingTest
		done bool
	}
	var order []int64
	byTest := map[int64]*acc{}
	for rows.Next() {
		var id, at int64
		var pkgName, name, outcome, sha string
		if err := rows.Scan(&id, &pkgName, &name, &outcome, &at, &sha); err != nil {
			rows.Close()
			return err
		}
		a := byTest[id]
		if a == nil {
			if outcome != "fail" {
				byTest[id] = &acc{done: true}
				continue
			}
			a = &acc{x: CIFailingTest{Repo: r.repo, Package: pkgName, Name: name, LastSHA: sha}}
			byTest[id] = a
			order = append(order, id)
		}
		if a.done {
			continue
		}
		if outcome == "pass" {
			a.x.LastPass = tptr(msTime(at))
			a.done = true
			continue
		}
		a.x.Fails++
		a.x.Since = msTime(at)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range order {
		a := byTest[id]
		if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM ci_test_runs WHERE test_id = ? AND at >= ? AND on_default = 0 AND outcome = 'fail'`,
			id, since).Scan(&a.x.OtherRefs); err != nil {
			return err
		}
		out.Failing = append(out.Failing, a.x)
	}
	return nil
}

// CICoverageInput asks for a commit's coverage: SHA (a prefix will do), or
// the newest of the default branch; of Path (a package directory or a file;
// the whole repo when empty).
type CICoverageInput struct {
	Repo string
	SHA  string
	Path string
}

// CICoverage is a commit's coverage, the shards' merged.
type CICoverage struct {
	Repo      string    `json:"repo"`
	SHA       string    `json:"sha"`
	Ref       string    `json:"ref,omitempty"`
	OnDefault bool      `json:"on_default"`
	At        time.Time `json:"at"`
	Shards    []string  `json:"shards"`
	Complete  bool      `json:"complete"`
	Path      string    `json:"path,omitempty"`
	Stmts     int       `json:"statements"`
	Covered   int       `json:"covered"`
	// Rows: the packages (no Path, or a directory with packages under it),
	// or the files of the Path package.
	Rows []CICoverageRow `json:"rows"`
	// Trend: Path's coverage on the default branch's newest commits.
	Trend []CICoveragePoint `json:"trend"`
	// FilesGone: the per-file detail is older than a month and was dropped.
	FilesGone bool `json:"files_gone,omitempty"`
}

// CICoverageRow is a package's or a file's statements and how many ran.
type CICoverageRow struct {
	Path    string `json:"path"`
	Stmts   int    `json:"statements"`
	Covered int    `json:"covered"`
}

// CICoveragePoint is one commit's coverage of the path.
type CICoveragePoint struct {
	SHA      string    `json:"sha"`
	At       time.Time `json:"at"`
	Stmts    int       `json:"statements"`
	Covered  int       `json:"covered"`
	Complete bool      `json:"complete"`
}

// CICoverage reads a commit's coverage.
func (s *Service) CICoverage(ctx context.Context, actor *Actor, in CICoverageInput) (*CICoverage, error) {
	repos, err := s.ciEvidenceRepos(ctx, actor, in.Repo)
	if err != nil {
		return nil, err
	}
	sha := strings.ToLower(strings.TrimSpace(in.SHA))
	if sha != "" && (len(sha) < 4 || strings.Trim(sha, "0123456789abcdef") != "") {
		return nil, invalid("sha: a commit id or its first 4 or more characters")
	}
	var r *ciRepoRef
	for i := range repos {
		var n int
		if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM ci_coverage WHERE forge_id = ? AND repo = ?`, repos[i].forge.ID, repos[i].repo).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 && in.Repo == "" {
			continue
		}
		if r != nil {
			return nil, invalid("repo: more than one repo has coverage; name one (--repo)")
		}
		r = &repos[i]
	}
	if r == nil {
		return nil, fmt.Errorf("no coverage kept yet (it is read from the backend test jobs' logs as they finish): %w", ErrNotFound)
	}
	q := `SELECT sha, ref, on_default, shards, complete, files, stmts, covered, updated_at FROM ci_coverage WHERE forge_id = ? AND repo = ?`
	args := []any{r.forge.ID, r.repo}
	if sha != "" {
		q += ` AND substr(sha, 1, ?) = ? ORDER BY updated_at DESC LIMIT 2`
		args = append(args, len(sha), sha)
	} else {
		q += ` ORDER BY on_default AND complete DESC, on_default DESC, updated_at DESC LIMIT 1`
	}
	rows, err := s.db.R.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var found []CICoverage
	var blob []byte
	for rows.Next() {
		c := CICoverage{Repo: r.repo, Path: strings.Trim(in.Path, "/")}
		var shards string
		var at int64
		var b []byte
		if err := rows.Scan(&c.SHA, &c.Ref, &c.OnDefault, &shards, &c.Complete, &b, &c.Stmts, &c.Covered, &at); err != nil {
			rows.Close()
			return nil, err
		}
		c.At = msTime(at)
		c.Shards = strings.FieldsFunc(shards, func(r rune) bool { return r == ',' })
		found = append(found, c)
		if blob == nil {
			blob = b
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch {
	case len(found) == 0 && sha != "":
		return nil, notFound("coverage of " + sha + " in " + r.repo)
	case len(found) == 0:
		return nil, notFound("coverage of " + r.repo)
	case len(found) > 1 && !strings.EqualFold(found[0].SHA, found[1].SHA):
		return nil, invalid("sha: %s names more than one commit", sha)
	}
	c := found[0]
	if err := s.coverageRows(ctx, r, &c, blob); err != nil {
		return nil, err
	}
	if err := s.coverageTrend(ctx, r, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// underPath says whether p is path or below it ("" is everything).
func underPath(p, path string) bool {
	return path == "" || p == path || strings.HasPrefix(p, path+"/")
}

// coverageRows fills c's rows (and, for a path, its totals): the packages,
// or a package's files.
func (s *Service) coverageRows(ctx context.Context, r *ciRepoRef, c *CICoverage, blob []byte) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT package, stmts, covered FROM ci_coverage_pkgs WHERE forge_id = ? AND repo = ? AND sha = ? ORDER BY package`,
		r.forge.ID, r.repo, c.SHA)
	if err != nil {
		return err
	}
	var pkgs []CICoverageRow
	for rows.Next() {
		var x CICoverageRow
		if err := rows.Scan(&x.Path, &x.Stmts, &x.Covered); err != nil {
			rows.Close()
			return err
		}
		pkgs = append(pkgs, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	c.Rows = []CICoverageRow{}
	if c.Path == "" {
		c.Rows = pkgs
		return nil
	}
	c.Stmts, c.Covered = 0, 0
	isPkg := false
	for _, p := range pkgs {
		if p.Path == c.Path {
			isPkg = true
		}
		if underPath(p.Path, c.Path) {
			c.Stmts += p.Stmts
			c.Covered += p.Covered
			c.Rows = append(c.Rows, p)
		}
	}
	if !isPkg && len(c.Rows) > 0 {
		return nil
	}
	if blob == nil {
		c.FilesGone = true
		c.Rows = slices.DeleteFunc(c.Rows, func(x CICoverageRow) bool { return x.Path != c.Path })
		c.Stmts, c.Covered = 0, 0
		for _, x := range c.Rows {
			c.Stmts, c.Covered = x.Stmts, x.Covered
		}
		return nil
	}
	files, err := unzipFiles(blob)
	if err != nil {
		return err
	}
	c.Rows = []CICoverageRow{}
	c.Stmts, c.Covered = 0, 0
	for _, f := range files {
		if f.Path == c.Path || (isPkg && pathDir(f.Path) == c.Path) {
			t, cv := f.Statements()
			c.Rows = append(c.Rows, CICoverageRow{Path: f.Path, Stmts: t, Covered: cv})
			c.Stmts += t
			c.Covered += cv
		}
	}
	return nil
}

func pathDir(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return "."
}

// coverageTrend fills c's trend: the path's coverage on the default
// branch's newest commits with coverage, newest first.
func (s *Service) coverageTrend(ctx context.Context, r *ciRepoRef, c *CICoverage) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT sha, complete, updated_at, stmts, covered FROM ci_coverage
		WHERE forge_id = ? AND repo = ? AND on_default = 1 ORDER BY updated_at DESC LIMIT ?`, r.forge.ID, r.repo, ciCoverageTrend)
	if err != nil {
		return err
	}
	var pts []CICoveragePoint
	for rows.Next() {
		var p CICoveragePoint
		var at int64
		if err := rows.Scan(&p.SHA, &p.Complete, &at, &p.Stmts, &p.Covered); err != nil {
			rows.Close()
			return err
		}
		p.At = msTime(at)
		pts = append(pts, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	c.Trend = []CICoveragePoint{}
	for _, p := range pts {
		if c.Path != "" {
			// Must count as coverageRows does: a package alone, or a directory's packages.
			p.Stmts, p.Covered = 0, 0
			var own *CICoveragePoint
			prows, err := s.db.R.QueryContext(ctx, `SELECT package, stmts, covered FROM ci_coverage_pkgs WHERE forge_id = ? AND repo = ? AND sha = ?`,
				r.forge.ID, r.repo, p.SHA)
			if err != nil {
				return err
			}
			for prows.Next() {
				var pk string
				var st, cv int
				if err := prows.Scan(&pk, &st, &cv); err != nil {
					prows.Close()
					return err
				}
				if pk == c.Path {
					own = &CICoveragePoint{Stmts: st, Covered: cv}
				}
				if underPath(pk, c.Path) {
					p.Stmts += st
					p.Covered += cv
				}
			}
			prows.Close()
			if err := prows.Err(); err != nil {
				return err
			}
			if own != nil {
				p.Stmts, p.Covered = own.Stmts, own.Covered
			}
			if p.Stmts == 0 {
				continue
			}
		}
		c.Trend = append(c.Trend, p)
	}
	return nil
}

// CISuppressionsInput asks about a repo's #nosec, //nolint and //wplint:ignore comments on
// its default branch: added or removed in the last Days days; All lists
// every current one.
type CISuppressionsInput struct {
	Repo string
	Days int
	All  bool
}

// CISuppressions is the suppression ledger of one repo.
type CISuppressions struct {
	Repo     string                 `json:"repo"`
	Days     int                    `json:"days"`
	SHA      string                 `json:"sha,omitempty"` // the newest scan
	At       *time.Time             `json:"at,omitempty"`
	Count    int                    `json:"count"`
	NoReason int                    `json:"no_reason"`
	ByRule   []CISuppressionCount   `json:"by_rule"`
	Scans    []CISuppressionScan    `json:"scans"`
	Added    []CISuppressionComment `json:"added"`
	Removed  []CISuppressionComment `json:"removed"`
	All      []CISuppressionComment `json:"all,omitempty"`
}

// CISuppressionCount is how many current suppressions name a rule ("" is
// every rule).
type CISuppressionCount struct {
	Kind  string `json:"kind"`
	Rule  string `json:"rule"`
	Count int    `json:"count"`
}

// CISuppressionScan is one scan's count, newest first.
type CISuppressionScan struct {
	SHA   string    `json:"sha"`
	At    time.Time `json:"at"`
	Count int       `json:"count"`
}

// CISuppressionComment is one suppression.
type CISuppressionComment struct {
	Kind     string     `json:"kind"`
	Rules    []string   `json:"rules"`
	Path     string     `json:"path"`
	Line     int        `json:"line"`
	Code     string     `json:"code"`
	Reason   string     `json:"reason"`
	FirstSHA string     `json:"first_sha"`
	FirstAt  time.Time  `json:"first_at"`
	GoneAt   *time.Time `json:"gone_at,omitempty"`
}

// CISuppressions reads the suppression ledger.
func (s *Service) CISuppressions(ctx context.Context, actor *Actor, in CISuppressionsInput) (*CISuppressions, error) {
	repos, err := s.ciEvidenceRepos(ctx, actor, in.Repo)
	if err != nil {
		return nil, err
	}
	days := in.Days
	if days <= 0 {
		days = 30
	}
	var r *ciRepoRef
	for i := range repos {
		var n int
		if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM ci_suppression_scans WHERE forge_id = ? AND repo = ?`, repos[i].forge.ID, repos[i].repo).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 && in.Repo == "" {
			continue
		}
		if r != nil {
			return nil, invalid("repo: more than one repo has a suppression scan; name one (--repo)")
		}
		r = &repos[i]
	}
	if r == nil {
		return nil, fmt.Errorf("no suppression scan kept yet (one is read from a backend test job of each push to the default branch): %w", ErrNotFound)
	}
	out := &CISuppressions{Repo: r.repo, Days: days, ByRule: []CISuppressionCount{}, Scans: []CISuppressionScan{}, Added: []CISuppressionComment{}, Removed: []CISuppressionComment{}}
	rows, err := s.db.R.QueryContext(ctx, `SELECT sha, at, count FROM ci_suppression_scans WHERE forge_id = ? AND repo = ? ORDER BY at DESC`, r.forge.ID, r.repo)
	if err != nil {
		return nil, err
	}
	var firstScan int64
	for rows.Next() {
		var sc CISuppressionScan
		var at int64
		if err := rows.Scan(&sc.SHA, &at, &sc.Count); err != nil {
			rows.Close()
			return nil, err
		}
		sc.At = msTime(at)
		firstScan = at
		if len(out.Scans) < ciCoverageTrend {
			out.Scans = append(out.Scans, sc)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out.Scans) > 0 {
		out.SHA, out.At = out.Scans[0].SHA, tptr(out.Scans[0].At)
	}
	since := s.now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	rows, err = s.db.R.QueryContext(ctx, `SELECT kind, rules, path, line, code, reason, first_sha, first_at, gone_at FROM ci_suppressions
		WHERE forge_id = ? AND repo = ? AND (gone_at IS NULL OR gone_at >= ?) ORDER BY path, line`, r.forge.ID, r.repo, since)
	if err != nil {
		return nil, err
	}
	counts := map[[2]string]int{}
	for rows.Next() {
		var c CISuppressionComment
		var rules string
		var first int64
		var gone sql.NullInt64
		if err := rows.Scan(&c.Kind, &rules, &c.Path, &c.Line, &c.Code, &c.Reason, &c.FirstSHA, &first, &gone); err != nil {
			rows.Close()
			return nil, err
		}
		c.Rules = strings.FieldsFunc(rules, func(r rune) bool { return r == ',' })
		if c.Rules == nil {
			c.Rules = []string{}
		}
		c.FirstAt = msTime(first)
		if gone.Valid {
			c.GoneAt = tptr(msTime(gone.Int64))
			out.Removed = append(out.Removed, c)
			continue
		}
		out.Count++
		if c.Reason == "" {
			out.NoReason++
		}
		if len(c.Rules) == 0 {
			counts[[2]string{c.Kind, ""}]++
		}
		for _, rule := range c.Rules {
			counts[[2]string{c.Kind, rule}]++
		}
		// Files already present at the first scan are a baseline, not additions.
		if first >= since && first > firstScan {
			out.Added = append(out.Added, c)
		}
		if in.All {
			out.All = append(out.All, c)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for k, n := range counts {
		out.ByRule = append(out.ByRule, CISuppressionCount{Kind: k[0], Rule: k[1], Count: n})
	}
	sort.Slice(out.ByRule, func(i, j int) bool {
		a, b := out.ByRule[i], out.ByRule[j]
		return stdcmp.Or(stdcmp.Compare(b.Count, a.Count), stdcmp.Compare(a.Kind, b.Kind), stdcmp.Compare(a.Rule, b.Rule)) < 0
	})
	slices.SortFunc(out.Added, func(a, b CISuppressionComment) int { return b.FirstAt.Compare(a.FirstAt) })
	slices.SortFunc(out.Removed, func(a, b CISuppressionComment) int { return b.GoneAt.Compare(*a.GoneAt) })
	return out, nil
}
