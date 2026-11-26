package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	EvKitUpdated   = "kit.updated"
	EvKitSourceSet = "kit.source_set"
	EvKitReported  = "kit.reported"
)

// kitRefreshMu keeps two reads of the kit from racing each other.
var kitRefreshMu sync.Mutex

// kitRow is the kit's stored state and where it lives.
type kitRow struct {
	id, repoID, forgeID int64
	repo, project       string
	branch, commit      string
	kit                 *Kit
	goodAt, readAt      sql.NullInt64
	readSHA             string
	errors              []string
}

const kitRowCols = `k.id, k.repo_id, r.forge_id, r.repo, p.key, k.branch, k.commit_sha, k.kit, k.good_at, k.read_at, k.read_sha, k.errors`

func scanKitRow(sc interface{ Scan(...any) error }) (*kitRow, error) {
	var k kitRow
	var raw, errs string
	if err := sc.Scan(&k.id, &k.repoID, &k.forgeID, &k.repo, &k.project, &k.branch, &k.commit, &raw, &k.goodAt, &k.readAt, &k.readSHA, &errs); err != nil {
		return nil, err
	}
	if raw != "" {
		k.kit = &Kit{}
		if err := json.Unmarshal([]byte(raw), k.kit); err != nil {
			k.kit = nil
		}
	}
	_ = json.Unmarshal([]byte(errs), &k.errors)
	return &k, nil
}

// theKit is the instance's kit; nil when no repo is the kit. One per
// instance for now: the oldest row.
func (s *Service) theKit(ctx context.Context, q queryer) (*kitRow, error) {
	k, err := scanKitRow(q.QueryRowContext(ctx, `SELECT `+kitRowCols+` FROM agent_kits k
		JOIN project_repos r ON r.id = k.repo_id JOIN projects p ON p.id = r.project_id ORDER BY k.id LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return k, err
}

// RefreshKit reads the kit if its default branch moved since the last read
// (scheduled hourly, and after a push to it).
func (s *Service) RefreshKit(ctx context.Context) {
	if err := s.refreshKit(ctx, false); err != nil {
		s.log.WarnContext(ctx, "read the agent kit", "err", err)
	}
}

// refreshKit reads kit.toml, the marketplace and each plugin's contents at
// the head of the kit repo's default branch. A kit with problems is
// recorded as such and never replaces the last good one; force reads even
// when the head is the one read last.
func (s *Service) refreshKit(ctx context.Context, force bool) error {
	kitRefreshMu.Lock()
	defer kitRefreshMu.Unlock()
	k, err := s.theKit(ctx, s.db.R)
	if err == nil && k == nil {
		jobSkip(ctx, "no agent kit is attached to a repo")
	}
	if err != nil || k == nil {
		return err
	}
	cl, ok := s.Forges.(ForgeKitReader)
	if !ok {
		return errors.New("this forge client can't read files")
	}
	f, err := s.forgeByID(ctx, k.forgeID)
	if err != nil {
		return err
	}
	fail := func(problems ...string) error {
		return s.recordKitRead(ctx, k, "", "", nil, problems)
	}
	branch, err := cl.DefaultBranch(ctx, f, k.repo)
	if err != nil {
		return fail("couldn't ask the forge for " + k.repo + "'s default branch: " + excerpt(err.Error(), 200))
	}
	head, err := cl.BranchHead(ctx, f, k.repo, branch)
	if err != nil {
		return fail("couldn't read " + branch + " of " + k.repo + ": " + excerpt(err.Error(), 200))
	}
	if !force && ((head == k.commit && len(k.errors) == 0 && k.kit != nil) || (head == k.readSHA && len(k.errors) > 0)) {
		_, err := s.db.W.ExecContext(ctx, `UPDATE agent_kits SET read_at = ? WHERE id = ?`, s.nowMS(), k.id)
		return err
	}
	read := func(p string) (string, error) {
		body, err := cl.FileAt(ctx, f, k.repo, p, head)
		if errors.Is(err, ErrNotFound) {
			return "", nil
		}
		return body, err
	}
	tomlText, err := read(kitTOML)
	if err != nil {
		return s.recordKitRead(ctx, k, branch, head, nil, []string{"couldn't read " + kitTOML + ": " + excerpt(err.Error(), 200)})
	}
	mkt, err := read(kitMarketplace)
	if err != nil {
		return s.recordKitRead(ctx, k, branch, head, nil, []string{"couldn't read " + kitMarketplace + ": " + excerpt(err.Error(), 200)})
	}
	kit, problems := parseKitFiles(tomlText, mkt)
	if len(problems) > 0 {
		return s.recordKitRead(ctx, k, branch, head, nil, problems)
	}
	paths, err := cl.RepoTree(ctx, f, k.repo, head)
	if err != nil {
		return s.recordKitRead(ctx, k, branch, head, nil, []string{"couldn't list the kit's files: " + excerpt(err.Error(), 200)})
	}
	tree := &kitTree{paths: map[string]bool{}, read: func(p string) (string, error) { return read(p) }}
	for _, p := range paths {
		tree.paths[p] = true
	}
	readKitComponents(kit, tree)
	if kit.GitURL == "" {
		kit.GitURL = cloneURL(f, k.repo)
	}
	return s.recordKitRead(ctx, k, branch, head, kit, nil)
}

// cloneURL is the https clone URL of a forge's repo.
func cloneURL(f *Forge, repo string) string {
	return strings.TrimRight(f.BaseURL, "/") + "/" + repo + ".git"
}

// recordKitRead stores a read: a good kit replaces the last one; problems
// are recorded beside it. head is "" when the forge couldn't be asked.
func (s *Service) recordKitRead(ctx context.Context, k *kitRow, branch, head string, kit *Kit, problems []string) error {
	now := s.nowMS()
	if problems == nil {
		problems = []string{}
	}
	errs, _ := json.Marshal(problems)
	return s.write(ctx, SystemActor(), func(t *tx) error {
		var err error
		if kit != nil {
			b, _ := json.Marshal(kit)
			_, err = t.ExecContext(ctx, `UPDATE agent_kits SET branch = ?, commit_sha = ?, kit = ?, good_at = ?, read_at = ?, read_sha = ?, errors = '[]' WHERE id = ?`,
				branch, head, string(b), now, now, head, k.id)
		} else {
			_, err = t.ExecContext(ctx, `UPDATE agent_kits SET read_at = ?, read_sha = CASE WHEN ? = '' THEN read_sha ELSE ? END, errors = ? WHERE id = ?`,
				now, head, head, string(errs), k.id)
		}
		if err != nil {
			return err
		}
		e := &Event{Kind: EvKitUpdated, New: shortSHA(cmp(head, k.commit))}
		if kit != nil && head != k.commit {
			e.Data = map[string]any{"commit": head}
		}
		t.emit(e)
		return nil
	})
}

// onKitPush reads the kit again when its default branch was pushed to.
func (s *Service) onKitPush(ctx context.Context, e *Event) {
	repo, _ := e.Data["repo"].(string)
	branch, _ := e.Data["branch"].(string)
	k, err := s.theKit(ctx, s.db.R)
	if err != nil || k == nil || !strings.EqualFold(k.repo, repo) || (k.branch != "" && branch != k.branch) {
		return
	}
	s.goBG(func() { s.RefreshKit(ctx) })
}

// KitInfo is the kit with where it comes from and how its last read went.
type KitInfo struct {
	Configured bool   `json:"configured"`
	RepoID     int64  `json:"repo_id,omitempty"`
	Repo       string `json:"repo,omitempty"`
	Project    string `json:"project,omitempty"`
	RepoURL    string `json:"repo_url,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Commit     string `json:"commit,omitempty"`
	// Kit is the last good kit (nil until one was read).
	Kit    *Kit       `json:"kit"`
	GoodAt *time.Time `json:"good_at"`
	ReadAt *time.Time `json:"read_at"`
	// Errors are what was wrong at the last read (of ReadSHA): the kit
	// above is still the one in use.
	Errors  []string `json:"errors"`
	ReadSHA string   `json:"read_sha,omitempty"`
	// Reach is, per plugin, the attached repos it goes to (when and machines
	// conditions aside).
	Reach map[string][]string `json:"reach"`
	// CanManage: the viewer may pick the kit repo, read it now and roll out.
	CanManage bool `json:"can_manage"`
}

// GetKit shows the kit to anyone signed in.
func (s *Service) GetKit(ctx context.Context, actor *Actor) (*KitInfo, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	out := &KitInfo{Errors: []string{}, Reach: map[string][]string{}, CanManage: actor.isAdmin()}
	k, err := s.theKit(ctx, s.db.R)
	if err != nil || k == nil {
		return out, err
	}
	out.Configured, out.RepoID, out.Repo, out.Project = true, k.repoID, k.repo, k.project
	out.Branch, out.Commit, out.Kit, out.ReadSHA = k.branch, k.commit, k.kit, k.readSHA
	out.GoodAt, out.ReadAt, out.Errors = ptrTime(k.goodAt), ptrTime(k.readAt), k.errors
	if out.Errors == nil {
		out.Errors = []string{}
	}
	if f, err := s.forgeByID(ctx, k.forgeID); err == nil {
		out.RepoURL = strings.TrimRight(f.BaseURL, "/") + "/" + k.repo
	}
	if k.kit != nil {
		repos, err := s.attachedRepos(ctx)
		if err != nil {
			return nil, err
		}
		for _, p := range k.kit.Plugins {
			out.Reach[p.Name] = []string{}
			if !p.Targeted {
				continue
			}
			for _, r := range repos {
				if p.Target.matchesRepo(r.repo, r.projects) {
					out.Reach[p.Name] = append(out.Reach[p.Name], r.repo)
				}
			}
		}
	}
	return out, nil
}

type attachedRepo struct {
	repo     string
	projects []string
}

// attachedRepos are the repos attached to projects, each with its
// projects' keys.
func (s *Service) attachedRepos(ctx context.Context) ([]attachedRepo, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.repo, p.key FROM project_repos r JOIN projects p ON p.id = r.project_id ORDER BY lower(r.repo), p.key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []attachedRepo
	for rows.Next() {
		var repo, key string
		if err := rows.Scan(&repo, &key); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && strings.EqualFold(out[n-1].repo, repo) {
			out[n-1].projects = append(out[n-1].projects, key)
			continue
		}
		out = append(out, attachedRepo{repo: repo, projects: []string{key}})
	}
	return out, rows.Err()
}

// ResolveKit is what one repo (OWNER/NAME) gets from the kit, for `wp kit
// sync` and SessionStart. Nil kit (none configured or never read): an
// empty answer, not an error, so a checkout simply gets nothing.
func (s *Service) ResolveKit(ctx context.Context, actor *Actor, repo string) (*KitResolved, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if repo == "" || len(repo) > 255 || !strings.Contains(repo, "/") {
		return nil, invalid("repo must be OWNER/NAME")
	}
	k, err := s.theKit(ctx, s.db.R)
	if err != nil {
		return nil, err
	}
	if k == nil || k.kit == nil {
		return &KitResolved{Repo: repo, Plugins: []KitResolvedItem{}, Notes: []KitResolvedItem{}, All: []string{}}, nil
	}
	var projects []string
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT p.key FROM project_repos r JOIN projects p ON p.id = r.project_id WHERE r.repo = ? COLLATE NOCASE ORDER BY p.key`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		projects = append(projects, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := k.kit.resolve(repo, projects)
	out.Commit = k.commit
	return out, nil
}

// KitReport is what a checkout reports after `wp kit sync`.
type KitReport struct {
	Machine   string     `json:"machine"`
	Path      string     `json:"path"`
	Repo      string     `json:"repo"`
	KitCommit string     `json:"kit_commit"` // the kit commit it last applied
	Plugins   []string   `json:"plugins"`    // kit plugins enabled there
	Drift     []KitDrift `json:"drift"`
	WPVersion string     `json:"wp_version"`
	Error     string     `json:"error"` // what failed, when the sync did
}

// KitDrift is something sync left alone because a person changed it.
type KitDrift struct {
	Plugin string `json:"plugin,omitempty"`
	What   string `json:"what"`
}

// ReportKitCheckout records a checkout's state (anyone signed in; reports
// are their own).
func (s *Service) ReportKitCheckout(ctx context.Context, actor *Actor, in KitReport) error {
	if err := actor.canWrite(); err != nil {
		return err
	}
	clean := func(v string, n int) string {
		return strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, excerpt(v, n)))
	}
	in.Machine, in.Path, in.Repo = clean(in.Machine, 100), clean(in.Path, 1000), clean(in.Repo, 255)
	in.KitCommit, in.WPVersion, in.Error = clean(in.KitCommit, 64), clean(in.WPVersion, 64), clean(in.Error, 1000)
	if in.Machine == "" || in.Path == "" {
		return invalid("a report needs the machine and the checkout's path")
	}
	if len(in.Plugins) > 100 || len(in.Drift) > 100 {
		return invalid("at most 100 plugins and 100 drift items")
	}
	plugins := []string{}
	for _, p := range in.Plugins {
		if p = clean(p, 100); p != "" {
			plugins = append(plugins, p)
		}
	}
	drift := []KitDrift{}
	for _, d := range in.Drift {
		if d.What = clean(d.What, 300); d.What != "" {
			drift = append(drift, KitDrift{Plugin: clean(d.Plugin, 100), What: d.What})
		}
	}
	pb, _ := json.Marshal(plugins)
	db, _ := json.Marshal(drift)
	return s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `INSERT INTO kit_reports(user_id, machine, path, repo, kit_commit, plugins, drift, wp_version, error, reported_at) VALUES (?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(user_id, machine, path) DO UPDATE SET repo = excluded.repo, kit_commit = excluded.kit_commit, plugins = excluded.plugins,
				drift = excluded.drift, wp_version = excluded.wp_version, error = excluded.error, reported_at = excluded.reported_at`,
			actor.id(), in.Machine, in.Path, in.Repo, in.KitCommit, string(pb), string(db), in.WPVersion, in.Error, s.nowMS()); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvKitReported})
		return nil
	})
}

// KitCheckout is a checkout's last report, with how it compares to the kit.
type KitCheckout struct {
	User       UserRef    `json:"user"`
	Machine    string     `json:"machine"`
	Path       string     `json:"path"`
	Repo       string     `json:"repo"`
	KitCommit  string     `json:"kit_commit"`
	Behind     bool       `json:"behind"` // it applied another commit than the kit's
	Plugins    []string   `json:"plugins"`
	Drift      []KitDrift `json:"drift"`
	WPVersion  string     `json:"wp_version"`
	Error      string     `json:"error"`
	ReportedAt time.Time  `json:"reported_at"`
}

// ListKitCheckouts lists the viewer's checkouts (admins: everyone's),
// behind or drifted ones first.
func (s *Service) ListKitCheckouts(ctx context.Context, actor *Actor) ([]KitCheckout, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	k, err := s.theKit(ctx, s.db.R)
	if err != nil {
		return nil, err
	}
	current := ""
	if k != nil {
		current = k.commit
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT u.id, u.username, u.display_name, u.avatar_version, k.machine, k.path, k.repo, k.kit_commit, k.plugins, k.drift, k.wp_version, k.error, k.reported_at
		FROM kit_reports k JOIN users u ON u.id = k.user_id WHERE ? OR k.user_id = ? ORDER BY k.reported_at DESC LIMIT 500`, actor.isAdmin(), actor.id())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KitCheckout{}
	for rows.Next() {
		var c KitCheckout
		var plugins, drift string
		var at, av int64
		var dname string
		if err := rows.Scan(&c.User.ID, &c.User.Username, &dname, &av, &c.Machine, &c.Path, &c.Repo, &c.KitCommit, &plugins, &drift, &c.WPVersion, &c.Error, &at); err != nil {
			return nil, err
		}
		c.User.DisplayName, c.User.AvatarURL = cmp(dname, c.User.Username), avatarURL(c.User.ID, av)
		_ = json.Unmarshal([]byte(plugins), &c.Plugins)
		_ = json.Unmarshal([]byte(drift), &c.Drift)
		if c.Plugins == nil {
			c.Plugins = []string{}
		}
		if c.Drift == nil {
			c.Drift = []KitDrift{}
		}
		c.ReportedAt = time.UnixMilli(at).UTC()
		c.Behind = current != "" && c.KitCommit != current
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rank := func(c KitCheckout) int {
		switch {
		case c.Error != "":
			return 0
		case c.Behind || len(c.Drift) > 0:
			return 1
		}
		return 2
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	return out, nil
}

// KitRollout is one runner's part of a roll-out.
type KitRollout struct {
	RunnerID int64  `json:"runner_id"`
	Runner   string `json:"runner"`
	JobID    int64  `json:"job_id,omitempty"`
	Skipped  string `json:"skipped,omitempty"` // why no job was queued
}

// kitSyncCommand is what a runner runs to roll the kit out: its own wp
// (the runner says where it is), over every checkout it may touch.
func kitSyncCommand(os string) string {
	if os == "windows" {
		return "wp kit sync --all --quiet"
	}
	return `"${WAYPOINT_WP:-wp}" kit sync --all --quiet`
}

// RolloutKit queues `wp kit sync --all` on each of the caller's online
// runners (admins: the people who look after the kit). A runner runs only
// its owner's jobs, so other people's machines catch up at their next
// session start or sync.
func (s *Service) RolloutKit(ctx context.Context, actor *Actor) ([]KitRollout, error) {
	if !actor.isAdmin() {
		return nil, forbidden("only admins can roll the kit out")
	}
	if err := actor.requireHuman("rolling the kit out"); err != nil {
		return nil, err
	}
	k, err := s.theKit(ctx, s.db.R)
	if err != nil {
		return nil, err
	}
	if k == nil || k.kit == nil {
		return nil, invalid("there is no kit to roll out yet: pick the kit repo in Admin → Forges")
	}
	runners, err := s.ownerRunners(ctx, actor.id())
	if err != nil {
		return nil, err
	}
	out := []KitRollout{}
	for _, r := range runners {
		x := KitRollout{RunnerID: r.ID, Runner: r.Name}
		switch {
		case !r.Online:
			x.Skipped = "offline"
		case len(r.AllowedDirs) == 0:
			x.Skipped = "it allows no directories (wp runner --allow DIR)"
		default:
			in, _ := json.Marshal(CommandJob{Command: kitSyncCommand(r.OS), Dir: r.AllowedDirs[0]})
			job, err := s.EnqueueRunnerJob(ctx, actor, RunnerJobInput{Kind: JobCommand, RunnerID: r.ID, Input: in, Source: "kit_rollout", Timeout: 1200})
			if err != nil {
				x.Skipped = excerpt(err.Error(), 200)
			} else {
				x.JobID = job.ID
			}
		}
		out = append(out, x)
	}
	if len(out) == 0 {
		return nil, invalid("you have no runners: start one with wp runner install --allow DIR")
	}
	return out, nil
}

// KitSource is the admin's view of the kit's repo, with the repos it could be.
type KitSource struct {
	RepoID     int64           `json:"repo_id"` // 0: none
	Candidates []KitRepoOption `json:"candidates"`
}

// KitRepoOption is an attached repo that could be the kit.
type KitRepoOption struct {
	ID      int64  `json:"id"`
	Repo    string `json:"repo"`
	Project string `json:"project"`
	Forge   string `json:"forge"`
}

// GetKitSource shows admins the kit repo and the repos to pick from.
func (s *Service) GetKitSource(ctx context.Context, actor *Actor) (*KitSource, error) {
	if !actor.isAdmin() {
		return nil, forbidden("only admins can pick the kit repo")
	}
	out := &KitSource{Candidates: []KitRepoOption{}}
	if k, err := s.theKit(ctx, s.db.R); err != nil {
		return nil, err
	} else if k != nil {
		out.RepoID = k.repoID
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.id, r.repo, p.key, f.name FROM project_repos r JOIN projects p ON p.id = r.project_id
		JOIN forges f ON f.id = r.forge_id ORDER BY lower(r.repo), p.key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var o KitRepoOption
		if err := rows.Scan(&o.ID, &o.Repo, &o.Project, &o.Forge); err != nil {
			return nil, err
		}
		out.Candidates = append(out.Candidates, o)
	}
	return out, rows.Err()
}

// SetKitSource makes an attached repo the kit (0: no kit). The kit is read
// from it right after (EvKitSourceSet).
func (s *Service) SetKitSource(ctx context.Context, actor *Actor, repoID int64) (*KitSource, error) {
	if !actor.isAdmin() {
		return nil, forbidden("only admins can pick the kit repo")
	}
	if err := actor.requireHuman("picking the kit repo"); err != nil {
		return nil, err
	}
	err := s.write(ctx, actor, func(t *tx) error {
		cur, err := s.theKit(ctx, t)
		if err != nil {
			return err
		}
		if cur != nil && cur.repoID == repoID {
			return nil
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM agent_kits`); err != nil {
			return err
		}
		detail, target := "no repo is the agent kit any more", "Agent kit"
		if repoID != 0 {
			var repo string
			if err := t.QueryRowContext(ctx, `SELECT repo FROM project_repos WHERE id = ?`, repoID).Scan(&repo); errors.Is(err, sql.ErrNoRows) {
				return notFound("repository")
			} else if err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `INSERT INTO agent_kits(repo_id, created_at) VALUES (?, ?)`, repoID, s.nowMS()); err != nil {
				return err
			}
			detail, target = "made "+repo+" the agent kit", "Agent kit: "+repo
		}
		t.audit(AuditEntry{Action: "settings.kit", Category: AuditSettings, TargetType: "setting", Target: target, URL: "/agents?tab=kit", Detail: detail})
		t.emit(&Event{Kind: EvKitSourceSet})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetKitSource(ctx, actor)
}

// ReadKitNow reads the kit again now, even at the same commit (admins).
func (s *Service) ReadKitNow(ctx context.Context, actor *Actor) (*KitInfo, error) {
	if !actor.isAdmin() {
		return nil, forbidden("only admins can read the kit again")
	}
	if err := s.refreshKit(ctx, true); err != nil {
		return nil, err
	}
	return s.GetKit(ctx, actor)
}

// plugin is one plugin of the stored kit, nil if there is none by that name.
func (k *kitRow) plugin(name string) *KitPlugin {
	if k.kit == nil {
		return nil
	}
	i := slices.IndexFunc(k.kit.Plugins, func(p KitPlugin) bool { return p.Name == name })
	if i < 0 {
		return nil
	}
	return &k.kit.Plugins[i]
}
