package core

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"git.jmann.me/claude/waypoint/internal/sandbox"
)

var sandboxKeyMu sync.Mutex

// readSandboxKey returns the server's spec key, or nil before it's made.
// It only reads, so it's safe inside a write.
func readSandboxKey(ctx context.Context, q queryer) (ed25519.PrivateKey, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = 'sandbox_spec_key'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(v)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("the sandbox spec key in server_settings is damaged")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// sandboxKey returns the server's spec key, making it the first time. Not
// inside a write: it may write itself.
func (s *Service) sandboxKey(ctx context.Context) (ed25519.PrivateKey, error) {
	sandboxKeyMu.Lock()
	defer sandboxKeyMu.Unlock()
	if priv, err := readSandboxKey(ctx, s.db.R); err != nil || priv != nil {
		return priv, err
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	if _, err := s.db.W.ExecContext(ctx, `INSERT OR IGNORE INTO server_settings(key, value) VALUES ('sandbox_spec_key', ?)`, base64.StdEncoding.EncodeToString(seed)); err != nil {
		return nil, err
	}
	return readSandboxKey(ctx, s.db.R)
}

// SandboxKey is the public half of the server's spec key.
type SandboxKey struct {
	// Key is what the helper pins (/etc/waypoint/sandboxd.pub).
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
}

// SandboxKey returns the key a machine's helper pins; anyone who may see
// runners may read it.
func (s *Service) SandboxKey(ctx context.Context, actor *Actor) (*SandboxKey, error) {
	if err := actor.Need(PermRunnersRead); err != nil {
		return nil, err
	}
	priv, err := s.sandboxKey(ctx)
	if err != nil {
		return nil, err
	}
	pub, _ := priv.Public().(ed25519.PublicKey)
	return &SandboxKey{Key: sandbox.FormatKey(pub), Fingerprint: sandbox.Fingerprint(pub)}, nil
}

// SandboxHelper is what a machine's helper last reported.
type SandboxHelper struct {
	sandbox.Report
	At time.Time `json:"at"`
	// Ready: Waypoint hands this machine's jobs signed specs. Problem says
	// why not.
	Ready   bool   `json:"ready"`
	Problem string `json:"problem,omitempty"`
}

// Limits on a report, which a runner (not the helper) sends.
const (
	maxReportUsers = 2000
	maxReportDrift = 50
	maxReportText  = 500
)

// cleanSandboxReport bounds what a runner can make Waypoint store.
func cleanSandboxReport(r *sandbox.Report) {
	r.Error = clip(r.Error, maxReportText)
	st := r.Status
	if st == nil {
		return
	}
	if !r.Installed {
		r.Status = nil
		return
	}
	st.Version, st.Key = clip(st.Version, maxReportText), clip(st.Key, maxReportText)
	st.Store.Path, st.Store.Problem, st.Store.FSType = clip(st.Store.Path, maxReportText), clip(st.Store.Problem, maxReportText), clip(st.Store.FSType, maxReportText)
	if len(st.Users) > maxReportUsers {
		st.Users = st.Users[:maxReportUsers]
	}
	for i := range st.Users {
		u := &st.Users[i]
		u.Name, u.Kind, u.Key, u.Slug = clip(u.Name, maxReportText), clip(u.Kind, maxReportText), clip(u.Key, maxReportText), clip(u.Slug, maxReportText)
	}
	if len(st.Removed) > maxReportDrift {
		st.Removed = st.Removed[:maxReportDrift]
	}
	for i := range st.Removed {
		u := &st.Removed[i]
		u.Name, u.Kind, u.Key, u.Slug, u.Archive = clip(u.Name, maxReportText), clip(u.Kind, maxReportText), clip(u.Key, maxReportText), clip(u.Slug, maxReportText), clip(u.Archive, maxReportText)
	}
	if len(st.Drift) > maxReportDrift {
		st.Drift = st.Drift[:maxReportDrift]
	}
	for i := range st.Drift {
		st.Drift[i] = clip(st.Drift[i], maxReportText)
	}
}

// helperReadiness says whether specs signed with fingerprint fp can work
// on a machine whose helper reported rep.
func helperReadiness(rep sandbox.Report, fp string) (bool, string) {
	switch {
	case !rep.Installed:
		return false, "the sandbox helper isn't installed (wp sandbox install)"
	case rep.Error != "":
		return false, rep.Error
	case rep.Status == nil:
		return false, "the helper hasn't reported"
	case fp == "":
		return false, "this server has no sandbox signing key yet"
	case rep.Status.Key != fp:
		return false, fmt.Sprintf("the helper pins another key (%s); this server signs with %s: run wp sandbox install again", rep.Status.Key, fp)
	case !rep.Status.Store.OK:
		return false, "the sandbox store: " + rep.Status.Store.Problem
	}
	return true, ""
}

// sandboxHelper reads a runner's last report; nil when it never sent one.
func (s *Service) sandboxHelper(ctx context.Context, q queryer, runnerID int64) (*SandboxHelper, error) {
	var text string
	var at int64
	err := q.QueryRowContext(ctx, `SELECT report, reported_at FROM sandbox_helpers WHERE runner_id = ?`, runnerID).Scan(&text, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	h := &SandboxHelper{At: msTime(at)}
	if err := json.Unmarshal([]byte(text), &h.Report); err != nil {
		return nil, err
	}
	priv, err := readSandboxKey(ctx, q)
	if err != nil {
		return nil, err
	}
	fp := ""
	if priv != nil {
		pub, _ := priv.Public().(ed25519.PublicKey)
		fp = sandbox.Fingerprint(pub)
	}
	h.Ready, h.Problem = helperReadiness(h.Report, fp)
	return h, nil
}

// saveSandboxReport keeps a runner's report of its helper.
func (t *tx) saveSandboxReport(ctx context.Context, runnerID int64, rep sandbox.Report) error {
	cleanSandboxReport(&rep)
	b, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	_, err = t.ExecContext(ctx, `INSERT INTO sandbox_helpers(runner_id, report, reported_at) VALUES (?, ?, ?)
		ON CONFLICT(runner_id) DO UPDATE SET report = excluded.report, reported_at = excluded.reported_at`, runnerID, string(b), t.s.nowMS())
	return err
}

// specSigner signs a runner's job specs; nil when its machine shouldn't
// get any (no ready helper, or paused).
type specSigner struct {
	priv   ed25519.PrivateKey
	runner int64
}

// sandboxSigner decides, before a claim, whether this runner's jobs get
// specs. Any error means no spec: Phase 1 never stops a job for it.
// sandboxPaused reads only the pause flag: the claim path has no use for
// the machine's users and ops. A machine with no row isn't paused.
func (s *Service) sandboxPaused(ctx context.Context, q queryer, runnerID int64) (bool, error) {
	var paused bool
	err := q.QueryRowContext(ctx, `SELECT paused FROM sandbox_machines WHERE runner_id = ?`, runnerID).Scan(&paused)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return paused, err
}

func (s *Service) sandboxSigner(ctx context.Context, r *Runner) *specSigner {
	h, err := s.sandboxHelper(ctx, s.db.R, r.ID)
	if err != nil {
		s.log.WarnContext(ctx, "sandbox helper report", "runner", r.ID, "err", err)
		return nil
	}
	if h == nil || !h.Ready {
		return nil
	}
	paused, err := s.sandboxPaused(ctx, s.db.R, r.ID)
	if err != nil || paused {
		return nil
	}
	priv, err := s.sandboxKey(ctx)
	if err != nil || priv == nil {
		s.log.WarnContext(ctx, "sandbox spec key", "runner", r.ID, "err", err)
		return nil
	}
	return &specSigner{priv: priv, runner: r.ID}
}

// sandboxRepos are the forge repositories a project's jobs may work on,
// keyed by forge id (WP-166): its linked repos whose forge id Waypoint
// knows, the one the job names first (WP-448). The helper makes a repo
// user for each and an org user for each owner.
func sandboxRepos(ctx context.Context, q queryer, projectID int64, hint string) ([]sandbox.Repo, error) {
	rows, err := q.QueryContext(ctx, `SELECT pr.forge_id, pr.forge_repo_id, pr.repo, f.base_url
		FROM project_repos pr JOIN forges f ON f.id = pr.forge_id
		WHERE pr.project_id = ? AND pr.forge_repo_id > 0 ORDER BY pr.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sandbox.Repo
	for rows.Next() {
		var forgeID, repoID int64
		var slug, baseURL string
		if err := rows.Scan(&forgeID, &repoID, &slug, &baseURL); err != nil {
			return nil, err
		}
		owner, _, ok := strings.Cut(slug, "/")
		if !ok || owner == "" {
			continue
		}
		r := sandbox.Repo{
			ID:    fmt.Sprintf("%d/%d", forgeID, repoID),
			Slug:  slug,
			Owner: fmt.Sprintf("%d/%s", forgeID, strings.ToLower(owner)),
		}
		if hint != "" && repoURLIs(hint, slug, baseURL) {
			out = append([]sandbox.Repo{r}, out...)
		} else {
			out = append(out, r)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > sandbox.MaxSpecRepos {
		out = out[:sandbox.MaxSpecRepos]
	}
	return out, nil
}

// repoURLIs says whether a clone URL (https or ssh) is of owner/name on the
// forge at baseURL: the same slug on another forge is another repository.
func repoURLIs(url, slug, baseURL string) bool {
	u := strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(url), "/"), ".git"))
	sl := strings.ToLower(slug)
	if !strings.HasSuffix(u, "/"+sl) && !strings.HasSuffix(u, ":"+sl) {
		return false
	}
	host := strings.ToLower(baseURL)
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	host = strings.TrimSuffix(host, "/")
	if i := strings.IndexAny(host, "/"); i >= 0 {
		host = host[:i]
	}
	if host == "" {
		return true
	}
	rest := strings.TrimSuffix(strings.TrimSuffix(u, sl), "/")
	rest = strings.TrimSuffix(rest, ":")
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	hostOnly := host
	if i := strings.LastIndex(hostOnly, ":"); i >= 0 {
		hostOnly = hostOnly[:i]
	}
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		rest = rest[:i]
	}
	return rest == host || rest == hostOnly
}

// jobRepoURL is the repository an agent job clones, if it names one.
func jobRepoURL(kind string, input json.RawMessage) string {
	if kind != JobAgent {
		return ""
	}
	var in AgentJob
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	return in.Repo
}

// sandboxSpecLife: the runner hands a spec to its helper as soon as it
// starts the job.
const sandboxSpecLife = 10 * time.Minute

// sign makes the ensure_users spec for a job of a project, and records
// the job as its users' last; nil when the project has no repository
// Waypoint can name.
func (sg *specSigner) sign(ctx context.Context, t *tx, now time.Time, c *RunnerClaim, projectID int64) (*sandbox.Signed, error) {
	repos, err := sandboxRepos(ctx, t, projectID, jobRepoURL(c.Kind, c.Input))
	if err != nil || len(repos) == 0 {
		return nil, err
	}
	for _, r := range repos {
		for kind, key := range map[string]string{sandbox.KindRepo: r.ID, sandbox.KindOrg: r.Owner} {
			if _, err := t.ExecContext(ctx, `INSERT INTO sandbox_user_jobs(runner_id, user_key, kind, job_id, at) VALUES (?,?,?,?,?)
				ON CONFLICT(runner_id, kind, user_key) DO UPDATE SET job_id = excluded.job_id, at = excluded.at`,
				sg.runner, key, kind, c.ID, now.UnixMilli()); err != nil {
				return nil, err
			}
		}
	}
	return sandbox.Sign(sg.priv, &sandbox.Spec{
		Op: sandbox.OpEnsureUsers, Nonce: sandbox.NewNonce(),
		Issued: now.Unix(), Expiry: now.Add(sandboxSpecLife).Unix(),
		Runner: sg.runner, Job: c.ID, Repos: repos,
	})
}
