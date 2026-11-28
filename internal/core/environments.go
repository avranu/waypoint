package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Environment kinds and statuses.
const (
	EnvProduction = "production"
	EnvStaging    = "staging"
	EnvPreview    = "preview"
	EnvDev        = "dev"

	EnvUp       = "up"
	EnvDown     = "down"
	EnvDegraded = "degraded"
	EnvUnknown  = "unknown"
)

// EnvKinds are the kinds, in the order they are listed.
var EnvKinds = []string{EnvProduction, EnvStaging, EnvPreview, EnvDev}

// EvEnvUpdated tells live clients an environment changed in a way the other
// env.* events don't announce (settings, a deploy that started, a push to
// its branch). It is live-only.
const EvEnvUpdated = "env.updated"

const (
	envDefaultInterval = 60
	envMinInterval     = 10
	// A certificate expiring within this long warns.
	envTLSWarn = 14 * 24 * time.Hour
	// Stopped ephemeral environments stay listed this long.
	envStoppedKeep = 7 * 24 * time.Hour
	// An ephemeral environment that stays down for this long (its TTL, if
	// it has one) is stopped.
	envDefaultUnreachable = 3 * 24 * time.Hour
)

var envNameRE = lazyre.New(`^[a-z0-9][a-z0-9._-]{0,62}$`)
var envFieldRE = lazyre.New(`^[A-Za-z0-9_.\-]{1,200}$`)

// Environment is a place a project's code runs.
type Environment struct {
	ID          int64  `json:"id"`
	ProjectID   int64  `json:"project_id"`
	Project     string `json:"project"` // key
	ProjectName string `json:"project_name"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	URL         string `json:"url"`
	// HealthURL is checked instead of URL when set; ExpectedStatus is the
	// code that counts as up (0: any 2xx); IntervalS the seconds between
	// checks (0: not checked).
	HealthURL      string `json:"health_url"`
	ExpectedStatus int    `json:"expected_status"`
	IntervalS      int    `json:"interval_s"`
	// VersionURL reports what runs: JSON whose fields (dot paths such as
	// git.commit) hold the commit, branch and version.
	VersionURL   string `json:"version_url"`
	CommitField  string `json:"commit_field"`
	BranchField  string `json:"branch_field"`
	VersionField string `json:"version_field"`
	Ephemeral    bool   `json:"ephemeral"`
	TTLS         int    `json:"ttl_s"`
	// ExpiresAt: the environment is stopped then, unless a deploy or
	// `wp env up` renews it.
	ExpiresAt *time.Time `json:"expires_at"`
	RepoID    *int64     `json:"repo_id"`
	Repo      string     `json:"repo,omitempty"` // owner/name, from the attached repo

	// What it runs.
	Branch     string     `json:"branch"`
	Commit     string     `json:"commit"`
	CommitURL  string     `json:"commit_url,omitempty"`
	Version    string     `json:"version"`
	DeployedAt *time.Time `json:"deployed_at"`
	// Behind: its branch has moved on since this commit (from push events).
	Behind bool   `json:"behind"`
	Head   string `json:"head,omitempty"`
	// LagAlertM: minutes it may run behind its branch before the project's
	// owners are told (0: never; WP-199).
	LagAlertM int `json:"lag_alert_m"`

	// What the last check said.
	Status        string     `json:"status"`
	StatusSince   *time.Time `json:"status_since"`
	LatencyMS     *int       `json:"latency_ms"`
	HTTPCode      *int       `json:"http_code"`
	Error         string     `json:"error"`
	TLSExpiresAt  *time.Time `json:"tls_expires_at"`
	TLSWarning    bool       `json:"tls_warning"`
	Mismatch      bool       `json:"mismatch"` // reports a commit other than the last deployment's
	LastCheckedAt *time.Time `json:"last_checked_at"`

	// Uptime as percentages (nil: no checks yet) and the last 24 hours, one
	// value per hour (percent up; -1: no checks).
	Uptime24h *float64  `json:"uptime_24h"`
	Uptime30d *float64  `json:"uptime_30d"`
	Spark     []float64 `json:"spark"`

	LastDeploy *Deployment `json:"last_deploy"`

	Stopped      bool       `json:"stopped"`
	StoppedAt    *time.Time `json:"stopped_at"`
	Position     int        `json:"position"`
	CreatedBy    *UserRef   `json:"created_by"`
	CreatedAgent string     `json:"created_agent,omitempty"`
	// AddressAgent is who last set the addresses (URL, HealthURL,
	// VersionURL) when it wasn't a person acting directly: an agent's name,
	// "agent-shell", "automation" or "unknown". Only a person's addresses
	// let an unattended agent session read from them (WP-749).
	AddressAgent string    `json:"address_agent,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	repoRef      *envRepo
	failStreak   int
	nextCheckAt  *time.Time
	lastOKAt     *time.Time
	createdByID  *int64
	lagSet       bool
	lagAlertedAt *time.Time
}

// addresses are the environment's addresses, as one comparable value.
func (e *Environment) addresses() string { return e.URL + "\n" + e.HealthURL + "\n" + e.VersionURL }

// addressAgent is who sets an environment's addresses, as address_agent
// keeps it: "" for a person acting directly, else the agent credited.
func addressAgent(a *Actor) string {
	if a == nil || a.User == nil {
		return "unknown"
	}
	return a.CreditAgent()
}

// nextAddressAgent is who an environment's addresses are after actor saves
// some of them (WP-749): an agent's change makes them the agent's, and an
// agent's save of the same ones leaves them as they were; a person's save
// vouches for them (even unchanged) only when it set every address there
// is, so a person changing one doesn't vouch for another an agent set.
func nextAddressAgent(old string, actor *Actor, changed, all bool) string {
	switch by := addressAgent(actor); {
	case by != "" && changed:
		return by
	case by == "" && all:
		return ""
	}
	return old
}

// target is the address that is checked.
func (e *Environment) target() string {
	if e.HealthURL != "" {
		return e.HealthURL
	}
	return e.URL
}

// EnvironmentInput creates or changes an environment; absent fields keep
// their value (or the default, when creating).
type EnvironmentInput struct {
	Name           *string `json:"name"`
	Kind           *string `json:"kind"`
	URL            *string `json:"url"`
	HealthURL      *string `json:"health_url"`
	ExpectedStatus *int    `json:"expected_status"`
	IntervalS      *int    `json:"interval_s"`
	VersionURL     *string `json:"version_url"`
	CommitField    *string `json:"commit_field"`
	BranchField    *string `json:"branch_field"`
	VersionField   *string `json:"version_field"`
	Ephemeral      *bool   `json:"ephemeral"`
	TTLS           *int    `json:"ttl_s"`
	RepoID         *int64  `json:"repo_id"` // 0: none
	Position       *int    `json:"position"`
	LagAlertM      *int    `json:"lag_alert_m"` // minutes; 0: never
}

const envCols = `e.id, e.project_id, p.key, p.name, e.repo_id, e.name, e.kind, e.url, e.health_url, e.expected_status, e.interval_s,
	e.version_url, e.commit_field, e.branch_field, e.version_field, e.ephemeral, e.ttl_s, e.expires_at,
	e.branch, e.commit_sha, e.version, e.deployed_at, e.status, e.status_since, e.latency_ms, e.http_code, e.error,
	e.tls_expires_at, e.fail_streak, e.mismatch, e.last_checked_at, e.next_check_at, e.last_ok_at, e.position,
	e.created_by, e.created_agent, e.address_agent, e.created_at, e.updated_at, e.stopped_at,
	e.lag_alert_m, e.lag_alerted_at`

func scanEnv(sc interface{ Scan(...any) error }) (*Environment, error) {
	var e Environment
	var repo, expires, deployed, since, latency, code, tlsExp, checked, next, lastOK, by, stopped, lagAlerted sql.NullInt64
	var created, updated int64
	if err := sc.Scan(&e.ID, &e.ProjectID, &e.Project, &e.ProjectName, &repo, &e.Name, &e.Kind, &e.URL, &e.HealthURL, &e.ExpectedStatus, &e.IntervalS,
		&e.VersionURL, &e.CommitField, &e.BranchField, &e.VersionField, &e.Ephemeral, &e.TTLS, &expires,
		&e.Branch, &e.Commit, &e.Version, &deployed, &e.Status, &since, &latency, &code, &e.Error,
		&tlsExp, &e.failStreak, &e.Mismatch, &checked, &next, &lastOK, &e.Position,
		&by, &e.CreatedAgent, &e.AddressAgent, &created, &updated, &stopped,
		&e.LagAlertM, &lagAlerted); err != nil {
		return nil, err
	}
	e.RepoID, e.ExpiresAt, e.DeployedAt = ptrInt(repo), ptrTime(expires), ptrTime(deployed)
	e.StatusSince, e.LatencyMS, e.HTTPCode = ptrTime(since), ptrIntN(latency), ptrIntN(code)
	e.TLSExpiresAt, e.LastCheckedAt, e.nextCheckAt, e.lastOKAt = ptrTime(tlsExp), ptrTime(checked), ptrTime(next), ptrTime(lastOK)
	e.createdByID, e.StoppedAt = ptrInt(by), ptrTime(stopped)
	e.Stopped = stopped.Valid
	e.lagAlertedAt = ptrTime(lagAlerted)
	e.CreatedAt, e.UpdatedAt = msTime(created), msTime(updated)
	e.Spark = []float64{}
	return &e, nil
}

// cleanEnvName normalises a name; names are lower case (they end up in URLs
// and CI configs).
func cleanEnvName(name string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	if !envNameRE.MatchString(n) {
		return "", invalid("an environment name is letters, digits, dots, dashes and underscores (up to 63), such as staging or a23x42s")
	}
	return n, nil
}

func validEnvKind(k string) bool {
	for _, v := range EnvKinds {
		if v == k {
			return true
		}
	}
	return false
}

// InferEnvKind guesses a new environment's kind from its name, as CI
// deploying to an environment that doesn't exist yet needs.
func InferEnvKind(name string) string {
	switch strings.ToLower(name) {
	case "production", "prod", "live":
		return EnvProduction
	case "staging", "stage", "stg":
		return EnvStaging
	case "dev", "develop", "development":
		return EnvDev
	}
	return EnvPreview
}

// validEnvURL accepts "" or an http(s) URL with a host.
func validEnvURL(what, raw string) error {
	if raw == "" {
		return nil
	}
	if len(raw) > 2000 {
		return invalid("%s is too long", what)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return invalid("%s must be an http or https address", what)
	}
	return nil
}

// validDevEnvURL checks a project's dev environment pattern.
func validDevEnvURL(pattern string) error {
	if pattern == "" {
		return nil
	}
	if !strings.Contains(pattern, "{name}") {
		return invalid("the dev environment address needs {name} in it, like https://{name}.dev.example.com")
	}
	return validEnvURL("the dev environment address", strings.ReplaceAll(pattern, "{name}", "x"))
}

func devEnvURL(pattern, name string) string {
	if pattern == "" {
		return ""
	}
	return strings.ReplaceAll(pattern, "{name}", name)
}

func validEnvField(what, v string) error {
	if v != "" && !envFieldRE.MatchString(v) {
		return invalid("%s is a path in the JSON like version or git.commit", what)
	}
	return nil
}

func validEnvSettings(e *Environment) error {
	if e.Ephemeral && e.Kind != EnvDev && e.Kind != EnvPreview {
		return invalid("only dev and preview environments can be temporary")
	}
	for _, c := range []struct{ what, v string }{{"the address", e.URL}, {"the health address", e.HealthURL}, {"the version address", e.VersionURL}} {
		if err := validEnvURL(c.what, c.v); err != nil {
			return err
		}
	}
	if e.ExpectedStatus != 0 && (e.ExpectedStatus < 100 || e.ExpectedStatus > 599) {
		return invalid("the expected status is an HTTP code like 200 (0 for any 2xx)")
	}
	if e.IntervalS != 0 && (e.IntervalS < envMinInterval || e.IntervalS > 86400) {
		return invalid("check every %d to 86400 seconds, or 0 to not check", envMinInterval)
	}
	if e.TTLS < 0 || e.TTLS > 365*86400 {
		return invalid("a time to live is up to a year")
	}
	if e.LagAlertM < 0 || e.LagAlertM > envLagAlertMax {
		return invalid("a deploy lag alert is 0 (off) to %d minutes", envLagAlertMax)
	}
	for _, f := range []struct{ what, v string }{{"the commit field", e.CommitField}, {"the branch field", e.BranchField}, {"the version field", e.VersionField}} {
		if err := validEnvField(f.what, f.v); err != nil {
			return err
		}
	}
	if e.VersionURL != "" && e.CommitField == "" && e.VersionField == "" {
		return invalid("name the commit or version field to read from the version address")
	}
	return nil
}

// apply copies the fields set in in onto e.
func (in *EnvironmentInput) apply(e *Environment) error {
	if in.Name != nil {
		n, err := cleanEnvName(*in.Name)
		if err != nil {
			return err
		}
		e.Name = n
	}
	if in.Kind != nil {
		if !validEnvKind(*in.Kind) {
			return invalid("kind is one of %s", strings.Join(EnvKinds, ", "))
		}
		e.Kind = *in.Kind
	}
	str := func(dst *string, v *string) {
		if v != nil {
			*dst = strings.TrimSpace(*v)
		}
	}
	str(&e.URL, in.URL)
	str(&e.HealthURL, in.HealthURL)
	str(&e.VersionURL, in.VersionURL)
	str(&e.CommitField, in.CommitField)
	str(&e.BranchField, in.BranchField)
	str(&e.VersionField, in.VersionField)
	if in.ExpectedStatus != nil {
		e.ExpectedStatus = *in.ExpectedStatus
	}
	if in.IntervalS != nil {
		e.IntervalS = *in.IntervalS
	}
	if in.Ephemeral != nil {
		e.Ephemeral = *in.Ephemeral
	}
	if in.TTLS != nil {
		e.TTLS = *in.TTLS
	}
	if in.Position != nil {
		e.Position = *in.Position
	}
	if in.LagAlertM != nil {
		e.LagAlertM, e.lagSet = *in.LagAlertM, true
	}
	return validEnvSettings(e)
}

// envRow loads one environment without the derived fields, for access
// checks and updates.
func (s *Service) envRow(ctx context.Context, q queryer, id int64) (*Environment, error) {
	e, err := scanEnv(q.QueryRowContext(ctx, `SELECT `+envCols+` FROM environments e JOIN projects p ON p.id = e.project_id WHERE e.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("environment")
	}
	return e, err
}

func (s *Service) envRowByName(ctx context.Context, q queryer, projectID int64, name string) (*Environment, error) {
	e, err := scanEnv(q.QueryRowContext(ctx, `SELECT `+envCols+` FROM environments e JOIN projects p ON p.id = e.project_id WHERE e.project_id = ? AND e.name = ?`, projectID, strings.ToLower(strings.TrimSpace(name))))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("environment")
	}
	return e, err
}

// envFor loads an environment the actor may use at the given role; people
// without access to its project can't tell it exists.
func (s *Service) envFor(ctx context.Context, q queryer, actor *Actor, id int64, min int) (*Environment, error) {
	e, err := s.envRow(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, q, actor, e.ProjectID, min); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, notFound("environment")
		}
		return nil, err
	}
	return e, nil
}

// ListEnvironments lists the environments of one project (ref) or of every
// project the actor can see (ref ""). Stopped ones are left out unless
// asked for.
func (s *Service) ListEnvironments(ctx context.Context, actor *Actor, ref string, stopped bool) ([]*Environment, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	var ids []int64
	if ref != "" {
		p, err := s.projectByRef(ctx, s.db.R, ref)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		ids = []int64{p.ID}
	} else {
		ps, err := s.ListProjects(ctx, actor, false)
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			if p.Role != "guest" {
				ids = append(ids, p.ID)
			}
		}
	}
	if len(ids) == 0 {
		return []*Environment{}, nil
	}
	where := `e.project_id IN (` + placeholders(len(ids)) + `)`
	if !stopped {
		where += ` AND e.stopped_at IS NULL`
	}
	return s.queryEnvs(ctx, where, int64Args(ids)...)
}

// queryEnvs loads environments matching where (over e and p) with their
// derived fields, in listing order.
func (s *Service) queryEnvs(ctx context.Context, where string, args ...any) ([]*Environment, error) {
	// #nosec G202 -- where is built from constants and placeholders; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+envCols+` FROM environments e JOIN projects p ON p.id = e.project_id WHERE `+where+`
		ORDER BY lower(p.name), e.stopped_at IS NOT NULL, CASE e.kind WHEN 'production' THEN 0 WHEN 'staging' THEN 1 WHEN 'preview' THEN 2 ELSE 3 END, e.position, e.name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Environment{}
	for rows.Next() {
		e, err := scanEnv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return out, s.decorateEnvs(ctx, out)
}

type envRepo struct {
	id        int64
	projectID int64
	repo      string
	kind, url string
}

// commitURL links a commit on its forge.
func (r envRepo) commitURL(sha string) string {
	if sha == "" || r.url == "" {
		return ""
	}
	sep := "/commit/"
	if r.kind == "gitlab" {
		sep = "/-/commit/"
	}
	return strings.TrimRight(r.url, "/") + "/" + r.repo + sep + sha
}

// decorateEnvs adds what isn't in the row: the repo and commit link, how far
// behind its branch, uptime, the last deployment and who made it. A few
// queries for all of them, not one each.
func (s *Service) decorateEnvs(ctx context.Context, list []*Environment) error {
	if len(list) == 0 {
		return nil
	}
	now := s.now()
	ids := make([]int64, len(list))
	projects := map[int64]bool{}
	for i, e := range list {
		ids[i] = e.ID
		projects[e.ProjectID] = true
	}
	pids := make([]int64, 0, len(projects))
	for id := range projects {
		pids = append(pids, id)
	}

	repos := map[int64][]envRepo{}
	// #nosec G202 -- marks are placeholders; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.id, r.project_id, r.repo, f.kind, f.base_url FROM project_repos r JOIN forges f ON f.id = r.forge_id
		WHERE r.project_id IN (`+placeholders(len(pids))+`) ORDER BY r.id`, int64Args(pids)...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r envRepo
		if err := rows.Scan(&r.id, &r.projectID, &r.repo, &r.kind, &r.url); err != nil {
			rows.Close()
			return err
		}
		repos[r.projectID] = append(repos[r.projectID], r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	envRepos := map[int64]envRepo{}
	repoNames := map[string]bool{}
	for _, e := range list {
		rs := repos[e.ProjectID]
		var found *envRepo
		for i := range rs {
			if e.RepoID != nil && rs[i].id == *e.RepoID {
				found = &rs[i]
			}
		}
		if found == nil && e.RepoID == nil && len(rs) == 1 {
			found = &rs[0]
		}
		if found != nil {
			envRepos[e.ID] = *found
			e.repoRef = found
			e.Repo = found.repo
			e.CommitURL = found.commitURL(e.Commit)
			repoNames[strings.ToLower(found.repo)] = true
		}
	}

	if len(repoNames) > 0 {
		names := make([]any, 0, len(repoNames))
		for n := range repoNames {
			names = append(names, n)
		}
		// #nosec G202 -- marks are placeholders; values are bound
		rows, err := s.db.R.QueryContext(ctx, `SELECT lower(repo), branch, sha FROM branch_heads WHERE repo IN (`+placeholders(len(names))+`)`, names...)
		if err != nil {
			return err
		}
		heads := map[string]string{}
		for rows.Next() {
			var repo, branch, sha string
			if err := rows.Scan(&repo, &branch, &sha); err != nil {
				rows.Close()
				return err
			}
			heads[repo+"\x00"+branch] = sha
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, e := range list {
			r, ok := envRepos[e.ID]
			if !ok || e.Branch == "" || e.Commit == "" {
				continue
			}
			if head := heads[strings.ToLower(r.repo)+"\x00"+e.Branch]; head != "" && !sameCommit(head, e.Commit) {
				e.Behind, e.Head = true, head
			}
		}
	}

	idx := map[int64]*Environment{}
	for _, e := range list {
		idx[e.ID] = e
		if e.TLSExpiresAt != nil && e.TLSExpiresAt.Sub(now) < envTLSWarn {
			e.TLSWarning = true
		}
	}
	hour := now.Truncate(time.Hour)
	since30 := hour.Add(-30 * 24 * time.Hour).UnixMilli()
	since24 := hour.Add(-23 * time.Hour)
	type tally struct{ checks, ok, c24, ok24 int }
	tallies := map[int64]*tally{}
	// #nosec G202 -- marks are placeholders; values are bound
	rows, err = s.db.R.QueryContext(ctx, `SELECT environment_id, hour, checks, ok_checks FROM environment_hourly
		WHERE environment_id IN (`+placeholders(len(ids))+`) AND hour >= ?`, append(int64Args(ids), since30)...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, h int64
		var checks, ok int
		if err := rows.Scan(&id, &h, &checks, &ok); err != nil {
			rows.Close()
			return err
		}
		t := tallies[id]
		if t == nil {
			t = &tally{}
			tallies[id] = t
		}
		t.checks, t.ok = t.checks+checks, t.ok+ok
		if h >= since24.UnixMilli() {
			t.c24, t.ok24 = t.c24+checks, t.ok24+ok
			e := idx[id]
			if len(e.Spark) == 0 {
				e.Spark = make([]float64, 24)
				for i := range e.Spark {
					e.Spark[i] = -1
				}
			}
			if slot := int((h - since24.UnixMilli()) / int64(time.Hour/time.Millisecond)); slot >= 0 && slot < 24 && checks > 0 {
				e.Spark[slot] = pct(ok, checks)
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for id, t := range tallies {
		e := idx[id]
		if t.checks > 0 {
			u := pct(t.ok, t.checks)
			e.Uptime30d = &u
		}
		if t.c24 > 0 {
			u := pct(t.ok24, t.c24)
			e.Uptime24h = &u
		}
	}

	// #nosec G202 -- marks are placeholders; values are bound
	rows, err = s.db.R.QueryContext(ctx, `SELECT `+deployCols+` FROM environments e
		JOIN deployments d ON d.id = (SELECT x.id FROM deployments x WHERE x.environment_id = e.id ORDER BY x.started_at DESC, x.id DESC LIMIT 1)
		WHERE e.id IN (`+placeholders(len(ids))+`)`, int64Args(ids)...)
	if err != nil {
		return err
	}
	var deploys []*Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			rows.Close()
			return err
		}
		deploys = append(deploys, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	cache := map[int64]*UserRef{}
	for _, d := range deploys {
		s.fillDeployment(ctx, cache, d)
		if e := idx[d.EnvironmentID]; e != nil {
			e.LastDeploy = d
		}
	}
	for _, e := range list {
		e.CreatedBy = s.cachedUser(ctx, cache, nullInt64(e.createdByID))
		if e.LastDeploy != nil {
			e.LastDeploy.CommitURL = envRepos[e.ID].commitURL(e.LastDeploy.Commit)
		}
	}
	return nil
}

func nullInt64(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

func pct(ok, checks int) float64 {
	if checks == 0 {
		return 0
	}
	return float64(int(float64(ok)/float64(checks)*10000+0.5)) / 100
}

// sameCommit says whether two commit ids name the same commit, allowing one
// to be an abbreviation of the other (at least 7 characters).
func sameCommit(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return len(a) >= 7 && strings.HasPrefix(b, a)
}

// EnvironmentDetail is an environment with its history.
type EnvironmentDetail struct {
	*Environment
	Deployments []*Deployment `json:"deployments"`
	// Days is uptime for each of the last 90 days (UTC), oldest first.
	Days []UptimeDay `json:"days"`
	// History is the last 48 hours in 10-minute steps.
	History []HistoryPoint `json:"history"`
}

// UptimeDay is one day's checks.
type UptimeDay struct {
	Date    string   `json:"date"`
	Checks  int      `json:"checks"`
	OK      int      `json:"ok"`
	Uptime  *float64 `json:"uptime"` // percent; nil: no checks
	Latency int      `json:"latency_ms"`
}

// HistoryPoint is a stretch of checks.
type HistoryPoint struct {
	At         time.Time `json:"at"`
	Checks     int       `json:"checks"`
	OK         int       `json:"ok"`
	LatencyMS  int       `json:"latency_ms"`
	MaxLatency int       `json:"max_latency_ms"`
}

// GetEnvironment returns an environment with its checks, uptime by day and
// recent deployments.
func (s *Service) GetEnvironment(ctx context.Context, actor *Actor, id int64) (*EnvironmentDetail, error) {
	e, err := s.envFor(ctx, s.db.R, actor, id, RoleViewer)
	if err != nil {
		return nil, err
	}
	return s.envDetail(ctx, e.ID)
}

// GetEnvironmentNamed is GetEnvironment for a project's environment by name.
func (s *Service) GetEnvironmentNamed(ctx context.Context, actor *Actor, projectRef, name string) (*EnvironmentDetail, error) {
	e, err := s.envByRef(ctx, actor, projectRef, name, RoleViewer)
	if err != nil {
		return nil, err
	}
	return s.envDetail(ctx, e.ID)
}

// envByRef finds a project's environment by name, with access checked.
func (s *Service) envByRef(ctx context.Context, actor *Actor, projectRef, name string, min int) (*Environment, error) {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, min); err != nil {
		return nil, err
	}
	return s.envRowByName(ctx, s.db.R, p.ID, name)
}

func (s *Service) envDetail(ctx context.Context, id int64) (*EnvironmentDetail, error) {
	list, err := s.queryEnvs(ctx, `e.id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, notFound("environment")
	}
	d := &EnvironmentDetail{Environment: list[0]}
	if d.Deployments, err = s.listDeployments(ctx, id, 50); err != nil {
		return nil, err
	}
	if r := d.repoRef; r != nil {
		for _, dep := range d.Deployments {
			dep.CommitURL = r.commitURL(dep.Commit)
		}
	}
	if d.Days, err = s.uptimeDays(ctx, id, 90); err != nil {
		return nil, err
	}
	if d.History, err = s.envHistory(ctx, id, 48*time.Hour); err != nil {
		return nil, err
	}
	return d, nil
}

// uptimeDays sums the hourly counts into UTC days.
func (s *Service) uptimeDays(ctx context.Context, id int64, days int) ([]UptimeDay, error) {
	today := s.now().UTC().Truncate(24 * time.Hour)
	first := today.AddDate(0, 0, -(days - 1))
	out := make([]UptimeDay, days)
	for i := range out {
		out[i].Date = first.AddDate(0, 0, i).Format(dateLayout)
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT hour, checks, ok_checks, latency_sum FROM environment_hourly WHERE environment_id = ? AND hour >= ?`, id, first.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lat := make([]int64, days)
	okSum := make([]int64, days)
	for rows.Next() {
		var h, checks, ok, latSum int64
		if err := rows.Scan(&h, &checks, &ok, &latSum); err != nil {
			return nil, err
		}
		i := int((h - first.UnixMilli()) / (24 * 3600 * 1000))
		if i < 0 || i >= days {
			continue
		}
		out[i].Checks += int(checks)
		out[i].OK += int(ok)
		lat[i] += latSum
		okSum[i] += ok
	}
	for i := range out {
		if out[i].Checks > 0 {
			u := pct(out[i].OK, out[i].Checks)
			out[i].Uptime = &u
		}
		if okSum[i] > 0 {
			out[i].Latency = int(lat[i] / okSum[i])
		}
	}
	return out, rows.Err()
}

// envHistory is the minute samples of the last span, in 10-minute steps.
func (s *Service) envHistory(ctx context.Context, id int64, span time.Duration) ([]HistoryPoint, error) {
	const step = int64(10 * 60 * 1000)
	since := s.now().Add(-span).UnixMilli()
	rows, err := s.db.R.QueryContext(ctx, `SELECT (at / ?) * ?, count(*), sum(ok), COALESCE(avg(CASE WHEN ok = 1 THEN latency_ms END), 0), max(latency_ms)
		FROM environment_checks WHERE environment_id = ? AND at >= ? GROUP BY at / ? ORDER BY 1`, step, step, id, since, step)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoryPoint{}
	for rows.Next() {
		var p HistoryPoint
		var at int64
		var lat float64
		if err := rows.Scan(&at, &p.Checks, &p.OK, &lat, &p.MaxLatency); err != nil {
			return nil, err
		}
		p.At, p.LatencyMS = msTime(at), int(lat)
		out = append(out, p)
	}
	return out, rows.Err()
}

func envEventData(e *Environment, extra map[string]any) map[string]any {
	d := map[string]any{"environment": e.ID, "name": e.Name, "kind": e.Kind, "url": e.URL, "project": e.Project}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

func envEvent(kind string, e *Environment, extra map[string]any) *Event {
	return &Event{Kind: kind, ProjectID: e.ProjectID, Data: envEventData(e, extra)}
}

// checkRepo makes sure repoID is attached to the project.
func (s *Service) checkEnvRepo(ctx context.Context, q queryer, projectID int64, repoID *int64) error {
	if repoID == nil || *repoID == 0 {
		return nil
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM project_repos WHERE id = ? AND project_id = ?`, *repoID, projectID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return invalid("that repo is not attached to this project")
	}
	return nil
}

// insertEnv writes a new environment (the caller validated it).
func (s *Service) insertEnv(t *tx, e *Environment) error {
	now := s.nowMS()
	var pos int
	if err := t.QueryRowContext(context.Background(), `SELECT COALESCE(max(position), 0) + 1 FROM environments WHERE project_id = ?`, e.ProjectID).Scan(&pos); err != nil {
		return err
	}
	if e.Position == 0 {
		e.Position = pos
	}
	var by any
	if t.Actor != nil && t.Actor.User != nil && t.Actor.User.ID != 0 {
		by = t.Actor.User.ID
		e.createdByID = &t.Actor.User.ID
	}
	var expires any
	if e.ExpiresAt != nil {
		expires = e.ExpiresAt.UnixMilli()
	}
	if !e.lagSet {
		e.LagAlertM = envDefaultLag(e)
	}
	res, err := t.ExecContext(context.Background(), `INSERT INTO environments(project_id, repo_id, name, kind, url, health_url, expected_status, interval_s,
		version_url, commit_field, branch_field, version_field, ephemeral, ttl_s, expires_at, branch, commit_sha, version, position, created_by, created_agent, address_agent,
		lag_alert_m, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.ProjectID, nullInt(e.RepoID), e.Name, e.Kind, e.URL, e.HealthURL, e.ExpectedStatus, e.IntervalS,
		e.VersionURL, e.CommitField, e.BranchField, e.VersionField, e.Ephemeral, e.TTLS, expires, e.Branch, e.Commit, e.Version, e.Position,
		by, t.Actor.AgentName(), addressAgent(t.Actor), e.LagAlertM, now, now)
	if err != nil {
		if isUnique(err) {
			return invalid("%s already has an environment called %s", e.Project, e.Name)
		}
		return err
	}
	e.ID, _ = res.LastInsertId()
	e.AddressAgent = addressAgent(t.Actor)
	e.CreatedAt, e.UpdatedAt = msTime(now), msTime(now)
	return nil
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// CreateEnvironment adds an environment to a project. Editors may.
func (s *Service) CreateEnvironment(ctx context.Context, actor *Actor, projectRef string, in EnvironmentInput) (*EnvironmentDetail, error) {
	if err := actor.Need(PermEnvironmentsWrite); err != nil {
		return nil, err
	}
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleEditor); err != nil {
		return nil, err
	}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		return nil, invalid("name the environment")
	}
	e := &Environment{ProjectID: p.ID, Project: p.Key, ProjectName: p.Name, Kind: EnvPreview, IntervalS: envDefaultInterval, Status: EnvUnknown}
	if in.Kind == nil {
		e.Kind = InferEnvKind(*in.Name)
	}
	if err := in.apply(e); err != nil {
		return nil, err
	}
	if err := s.checkEnvRepo(ctx, s.db.R, p.ID, in.RepoID); err != nil {
		return nil, err
	}
	if in.RepoID != nil && *in.RepoID != 0 {
		e.RepoID = in.RepoID
	}
	err = s.write(ctx, actor, func(t *tx) error {
		if err := s.insertEnv(t, e); err != nil {
			return err
		}
		t.emit(envEvent(EvEnvCreated, e, nil))
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.kickEnvChecks()
	return s.envDetail(ctx, e.ID)
}

// UpdateEnvironment changes an environment's settings. Editors may.
func (s *Service) UpdateEnvironment(ctx context.Context, actor *Actor, id int64, in EnvironmentInput) (*EnvironmentDetail, error) {
	if err := actor.Need(PermEnvironmentsWrite); err != nil {
		return nil, err
	}
	e, err := s.envFor(ctx, s.db.R, actor, id, RoleEditor)
	if err != nil {
		return nil, err
	}
	old := *e
	if err := in.apply(e); err != nil {
		return nil, err
	}
	if !e.lagSet && e.Kind != old.Kind && old.LagAlertM == envDefaultLag(&old) {
		e.LagAlertM = envDefaultLag(e)
	}
	if in.URL != nil || in.HealthURL != nil || in.VersionURL != nil {
		all := (in.URL != nil || e.URL == "") && (in.HealthURL != nil || e.HealthURL == "") && (in.VersionURL != nil || e.VersionURL == "")
		e.AddressAgent = nextAddressAgent(old.AddressAgent, actor, e.addresses() != old.addresses(), all)
	}
	if in.RepoID != nil {
		if err := s.checkEnvRepo(ctx, s.db.R, e.ProjectID, in.RepoID); err != nil {
			return nil, err
		}
		e.RepoID = in.RepoID
		if *in.RepoID == 0 {
			e.RepoID = nil
		}
	}
	moved := e.target() != old.target() || e.VersionURL != old.VersionURL || e.ExpectedStatus != old.ExpectedStatus
	err = s.write(ctx, actor, func(t *tx) error {
		var expires any
		if e.ExpiresAt != nil {
			expires = e.ExpiresAt.UnixMilli()
		}
		_, err := t.ExecContext(ctx, `UPDATE environments SET repo_id = ?, name = ?, kind = ?, url = ?, health_url = ?, expected_status = ?, interval_s = ?,
			version_url = ?, commit_field = ?, branch_field = ?, version_field = ?, ephemeral = ?, ttl_s = ?, expires_at = ?, position = ?, address_agent = ?,
			lag_alert_m = ?, updated_at = ?,
			next_check_at = CASE WHEN ? THEN NULL ELSE next_check_at END,
			fail_streak = CASE WHEN ? THEN 0 ELSE fail_streak END
			WHERE id = ?`,
			nullInt(e.RepoID), e.Name, e.Kind, e.URL, e.HealthURL, e.ExpectedStatus, e.IntervalS,
			e.VersionURL, e.CommitField, e.BranchField, e.VersionField, e.Ephemeral, e.TTLS, expires, e.Position, e.AddressAgent,
			e.LagAlertM, s.nowMS(), moved, moved, id)
		if err != nil {
			if isUnique(err) {
				return invalid("%s already has an environment called %s", e.Project, e.Name)
			}
			return err
		}
		t.emit(envEvent(EvEnvUpdated, e, nil))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if moved {
		s.kickEnvChecks()
	}
	return s.envDetail(ctx, id)
}

// StopEnvironment marks an environment stopped: it is no longer checked and
// is listed folded. Dev and preview ones go after a week; others stay until
// deleted. Editors (and their agents) may.
func (s *Service) StopEnvironment(ctx context.Context, actor *Actor, id int64) (*EnvironmentDetail, error) {
	if err := actor.Need(PermEnvironmentsWrite); err != nil {
		return nil, err
	}
	e, err := s.envFor(ctx, s.db.R, actor, id, RoleEditor)
	if err != nil {
		return nil, err
	}
	err = s.write(ctx, actor, func(t *tx) error { return s.stopEnvTx(t, e, "") })
	if err != nil {
		return nil, err
	}
	return s.envDetail(ctx, id)
}

// stopEnvTx stops e (a no-op if it already is); reason says why when the
// system did, "ttl" or "unreachable".
func (s *Service) stopEnvTx(t *tx, e *Environment, reason string) error {
	if e.Stopped {
		return nil
	}
	res, err := t.ExecContext(context.Background(), `UPDATE environments SET stopped_at = ?, expires_at = NULL, next_check_at = NULL, updated_at = ? WHERE id = ? AND stopped_at IS NULL`, s.nowMS(), s.nowMS(), e.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	extra := map[string]any{}
	if reason != "" {
		extra["reason"] = reason
	}
	t.emit(envEvent(EvEnvStopped, e, extra))
	return nil
}

// DeleteEnvironment removes an environment and its history. Editors may,
// people only: agents stop environments, they don't erase them.
func (s *Service) DeleteEnvironment(ctx context.Context, actor *Actor, id int64) error {
	if err := actor.Need(PermEnvironmentsWrite); err != nil {
		return err
	}
	if err := actor.RequireHuman("deleting an environment"); err != nil {
		return err
	}
	e, err := s.envFor(ctx, s.db.R, actor, id, RoleEditor)
	if err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `DELETE FROM environments WHERE id = ?`, id); err != nil {
			return err
		}
		t.audit(AuditEntry{Action: "env.delete", Category: AuditSettings, ProjectID: e.ProjectID, TargetType: "environment",
			Target: e.Project + "/" + e.Name, URL: "/p/" + e.Project + "/environments/" + e.Name, Detail: "deleted the " + e.Kind + " environment " + e.Name})
		t.emit(envEvent(EvEnvUpdated, e, map[string]any{"deleted": true}))
		return nil
	})
}

// EnvUpInput is `wp env up`: make a dev environment (or bring a stopped one
// back), say what it runs, and renew its lease.
type EnvUpInput struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Kind   string `json:"kind"`
	Branch string `json:"branch"`
	Commit string `json:"commit"`
	TTLS   int    `json:"ttl_s"`
}

// UpEnvironment creates an ephemeral environment, or updates and restarts
// the one with that name. The address comes from in.URL or the project's
// dev environment pattern. Editors may; agents too.
func (s *Service) UpEnvironment(ctx context.Context, actor *Actor, projectRef string, in EnvUpInput) (*EnvironmentDetail, error) {
	if err := actor.Need(PermEnvironmentsWrite); err != nil {
		return nil, err
	}
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleEditor); err != nil {
		return nil, err
	}
	name, err := cleanEnvName(in.Name)
	if err != nil {
		return nil, err
	}
	kind := in.Kind
	if kind == "" {
		kind = EnvDev
	}
	addr := strings.TrimSpace(in.URL)
	if addr == "" {
		addr = devEnvURL(p.Settings.DevEnvURL, name)
	}
	var commit string
	if c := strings.ToLower(strings.TrimSpace(in.Commit)); c != "" {
		if !commitRE.MatchString(c) {
			return nil, invalid("a commit is a hex id such as 3fa9c1e")
		}
		commit = c
	}
	branch := cleanLine(in.Branch, 200)
	if in.TTLS < 0 || in.TTLS > 365*86400 {
		return nil, invalid("a time to live is up to a year")
	}
	if err := validEnvURL("the address", addr); err != nil {
		return nil, err
	}
	var id int64
	err = s.write(ctx, actor, func(t *tx) error {
		cur, err := s.envRowByName(ctx, t, p.ID, name)
		created := false
		switch {
		case errors.Is(err, ErrNotFound):
			if kind != EnvDev && kind != EnvPreview {
				return invalid("only dev and preview environments can be brought up; add %s in the project's Environments", name)
			}
			if addr == "" {
				return invalid("give --url, or set the dev environment address in the project's settings (like https://{name}.dev.example.com)")
			}
			cur = &Environment{ProjectID: p.ID, Project: p.Key, ProjectName: p.Name, Name: name, Kind: kind, URL: addr, IntervalS: envDefaultInterval,
				Ephemeral: true, TTLS: in.TTLS, Status: EnvUnknown}
			if in.TTLS > 0 {
				x := s.now().Add(time.Duration(in.TTLS) * time.Second)
				cur.ExpiresAt = &x
			}
			if err := validEnvSettings(cur); err != nil {
				return err
			}
			if err := s.insertEnv(t, cur); err != nil {
				return err
			}
			created = true
		case err != nil:
			return err
		default:
			if addr != "" && addr != cur.URL {
				cur.URL = addr
				cur.AddressAgent = nextAddressAgent(cur.AddressAgent, actor, true, cur.HealthURL == "" && cur.VersionURL == "")
			}
			var expires any
			if in.TTLS > 0 {
				cur.TTLS = in.TTLS
			}
			if cur.TTLS > 0 {
				x := s.now().Add(time.Duration(cur.TTLS) * time.Second)
				expires = x.UnixMilli()
			}
			revived := cur.Stopped
			if _, err := t.ExecContext(ctx, `UPDATE environments SET url = ?, address_agent = ?, ttl_s = ?, expires_at = ?, stopped_at = NULL, updated_at = ?,
				next_check_at = NULL, fail_streak = CASE WHEN ? THEN 0 ELSE fail_streak END,
				status = CASE WHEN ? THEN 'unknown' ELSE status END, status_since = CASE WHEN ? THEN NULL ELSE status_since END WHERE id = ?`,
				cur.URL, cur.AddressAgent, cur.TTLS, expires, s.nowMS(), revived, revived, revived, cur.ID); err != nil {
				return err
			}
			if revived {
				t.emit(envEvent(EvEnvCreated, cur, map[string]any{"restarted": true}))
				cur.Stopped = false
			} else {
				t.emit(envEvent(EvEnvUpdated, cur, nil))
			}
		}
		id = cur.ID
		if created {
			t.emit(envEvent(EvEnvCreated, cur, nil))
		}
		if commit != "" && !sameCommit(commit, cur.Commit) {
			if _, err := s.recordDeployTx(t, cur, DeployInput{Commit: commit, Branch: branch, Status: DeploySucceeded}); err != nil {
				return err
			}
		} else if branch != "" && branch != cur.Branch {
			if _, err := t.ExecContext(ctx, `UPDATE environments SET branch = ? WHERE id = ?`, branch, cur.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.kickEnvChecks()
	return s.envDetail(ctx, id)
}

// DownEnvironment is `wp env down`: stop an environment by name.
func (s *Service) DownEnvironment(ctx context.Context, actor *Actor, projectRef, name string) (*EnvironmentDetail, error) {
	if err := actor.Need(PermEnvironmentsWrite); err != nil {
		return nil, err
	}
	e, err := s.envByRef(ctx, actor, projectRef, name, RoleEditor)
	if err != nil {
		return nil, err
	}
	return s.StopEnvironment(ctx, actor, e.ID)
}

func cleanLine(v string, max int) string {
	v = strings.Join(strings.Fields(v), " ")
	if len(v) > max {
		v = v[:max]
	}
	return v
}

var commitRE = lazyre.New(`^[0-9a-f]{7,64}$`)

// maxEnvCommitLookups is how many commits one EnvironmentsForCommits call
// looks up (up to 58 bound abbreviations each).
const maxEnvCommitLookups = 100

// EnvironmentCounts are how many of a project's environments are in each
// state (stopped ones aren't counted).
type EnvironmentCounts struct {
	Project     string `json:"project"`
	ProjectName string `json:"project_name"`
	ProjectID   int64  `json:"project_id"`
	Total       int    `json:"total"`
	Up          int    `json:"up"`
	Degraded    int    `json:"degraded"`
	Down        int    `json:"down"`
	Unknown     int    `json:"unknown"`
	// Worst names the environments that are down.
	DownNames []string `json:"down_names,omitempty"`
}

// EnvironmentSummary counts environments per project, for the Agents
// overview and the sidebar.
type EnvironmentSummary struct {
	Total    int                  `json:"total"`
	Up       int                  `json:"up"`
	Degraded int                  `json:"degraded"`
	Down     int                  `json:"down"`
	Unknown  int                  `json:"unknown"`
	Projects []*EnvironmentCounts `json:"projects"`
}

// SummarizeEnvironments counts the environments the actor can see.
func (s *Service) SummarizeEnvironments(ctx context.Context, actor *Actor) (*EnvironmentSummary, error) {
	return s.summarizeEnvironments(ctx, actor, "")
}

// summarizeEnvironments counts those of one project (a key; "" for all).
func (s *Service) summarizeEnvironments(ctx context.Context, actor *Actor, project string) (*EnvironmentSummary, error) {
	list, err := s.ListEnvironments(ctx, actor, project, false)
	if err != nil {
		return nil, err
	}
	sum := &EnvironmentSummary{Projects: []*EnvironmentCounts{}}
	by := map[int64]*EnvironmentCounts{}
	for _, e := range list {
		c := by[e.ProjectID]
		if c == nil {
			c = &EnvironmentCounts{Project: e.Project, ProjectName: e.ProjectName, ProjectID: e.ProjectID}
			by[e.ProjectID] = c
			sum.Projects = append(sum.Projects, c)
		}
		c.Total++
		sum.Total++
		switch e.Status {
		case EnvUp:
			c.Up++
			sum.Up++
		case EnvDegraded:
			c.Degraded++
			sum.Degraded++
		case EnvDown:
			c.Down++
			sum.Down++
			c.DownNames = append(c.DownNames, e.Name)
		default:
			c.Unknown++
			sum.Unknown++
		}
	}
	return sum, nil
}

// EnvironmentAt says where a commit runs (or ran).
type EnvironmentAt struct {
	Environment int64     `json:"environment"`
	Project     string    `json:"project"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	Status      string    `json:"status"`
	Commit      string    `json:"commit"`
	At          time.Time `json:"at"`      // when it was deployed there
	Current     bool      `json:"current"` // it still runs there
}

// EnvironmentsForCommits finds where commits of repo (owner/name) run,
// among the environments the actor can see: "in staging since 14:02" on a
// pull request. A commit that was deployed and has since been replaced is
// returned with Current false.
func (s *Service) EnvironmentsForCommits(ctx context.Context, actor *Actor, repo string, shas []string) (map[string][]EnvironmentAt, error) {
	out := map[string][]EnvironmentAt{}
	var want []string
	seen := map[string]bool{}
	for _, c := range shas {
		if c = strings.ToLower(strings.TrimSpace(c)); commitRE.MatchString(c) && !seen[c] && len(want) < maxEnvCommitLookups {
			seen[c] = true
			want = append(want, c)
		}
	}
	if len(want) == 0 || repo == "" {
		return out, nil
	}
	list, err := s.ListEnvironments(ctx, actor, "", true)
	if err != nil {
		return nil, err
	}
	byID := map[int64]*Environment{}
	var ids []int64
	for _, e := range list {
		if strings.EqualFold(e.Repo, repo) {
			byID[e.ID] = e
			ids = append(ids, e.ID)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	cands := map[string]bool{}
	for _, c := range want {
		cands[c] = true
		for n := 7; n < len(c); n++ {
			cands[c[:n]] = true
		}
	}
	args := int64Args(ids)
	for c := range cands {
		args = append(args, c)
	}
	// #nosec G202 -- marks are placeholders; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT environment_id, commit_sha, COALESCE(finished_at, started_at) FROM deployments
		WHERE environment_id IN (`+placeholders(len(ids))+`) AND status IN ('succeeded','detected') AND commit_sha IN (`+placeholders(len(cands))+`)
		ORDER BY started_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct {
		sha string
		env int64
	}
	latest := map[key]EnvironmentAt{}
	for rows.Next() {
		var id, at int64
		var commit string
		if err := rows.Scan(&id, &commit, &at); err != nil {
			return nil, err
		}
		e := byID[id]
		for _, c := range want {
			if !sameCommit(c, commit) {
				continue
			}
			latest[key{c, id}] = EnvironmentAt{Environment: id, Project: e.Project, Name: e.Name, Kind: e.Kind, Status: e.Status, Commit: commit, At: msTime(at),
				Current: !e.Stopped && sameCommit(e.Commit, c)}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for k, v := range latest {
		out[k.sha] = append(out[k.sha], v)
	}
	return out, nil
}

// envEventText words env.* events for webhooks and notifications.
func envEventText(e *Event) string {
	name, _ := e.Data["name"].(string)
	switch e.Kind {
	case EvEnvStatus:
		return fmt.Sprintf("%s is %s", name, e.New)
	case EvEnvDeployed:
		return fmt.Sprintf("Deployed to %s", name)
	case EvEnvDeployFailed:
		return fmt.Sprintf("Deploy to %s failed", name)
	case EvEnvCreated:
		return fmt.Sprintf("Environment %s created", name)
	case EvEnvStopped:
		return fmt.Sprintf("Environment %s stopped", name)
	case EvEnvLagging:
		return fmt.Sprintf("%s runs behind its branch", name)
	}
	return ""
}

// isEnvEvent: webhooks carry environment events (not the live-only update).
func isEnvEvent(kind string) bool {
	switch kind {
	case EvEnvStatus, EvEnvDeployed, EvEnvDeployFailed, EvEnvCreated, EvEnvStopped, EvEnvLagging:
		return true
	}
	return false
}
