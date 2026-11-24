package core

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// EvAgentSetupUpdated: a repo's agent files were read, the shared
// instructions saved, a pull request opened on them, or a machine reported.
const EvAgentSetupUpdated = "agent_setup.updated"

// agentScanMu keeps two reads of the same repos from racing each other.
var agentScanMu sync.Mutex

// agentRefreshing is set while a forced read of every repo is running:
// pressing Refresh again meanwhile starts nothing more.
var agentRefreshing bool

// agentRefreshAgain: a Refresh came in during the read, so another follows.
var agentRefreshAgain bool

// agentRefreshMu guards both, so the worker's last check and going idle are
// one step with respect to a request.
var agentRefreshMu sync.Mutex

// setupRepo is an attached repo as Agent setup sees it: one row per forge
// repo however many projects link it.
type setupRepo struct {
	forgeID  int64
	repo     string
	projects []string
	// Opted out if any of its projects turned repo syncing off.
	optedOut bool
}

// setupRepos lists the repos attached to live projects.
func (s *Service) setupRepos(ctx context.Context) ([]setupRepo, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.forge_id, r.repo, p.key, coalesce(json_extract(p.settings, '$.no_shared_setup'), 0)
		FROM project_repos r JOIN projects p ON p.id = r.project_id
		WHERE p.archived = 0 ORDER BY r.forge_id, lower(r.repo), p.key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []setupRepo
	for rows.Next() {
		var r setupRepo
		var key string
		var off bool
		if err := rows.Scan(&r.forgeID, &r.repo, &key, &off); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].forgeID == r.forgeID && strings.EqualFold(out[n-1].repo, r.repo) {
			if !slices.Contains(out[n-1].projects, key) {
				out[n-1].projects = append(out[n-1].projects, key)
			}
			out[n-1].optedOut = out[n-1].optedOut || off
			continue
		}
		r.projects, r.optedOut = []string{key}, off
		out = append(out, r)
	}
	return out, rows.Err()
}

// RefreshAgentSetup reads every attached repo whose default branch moved
// since its last read, then opens the shared instructions' pull requests
// the repos are owed (hourly).
func (s *Service) RefreshAgentSetup(ctx context.Context) {
	if err := s.scanAgentRepos(ctx, false); err != nil {
		s.log.WarnContext(ctx, "read agent files", "err", err)
	}
	if err := s.autoPublishShared(ctx); err != nil {
		s.log.WarnContext(ctx, "publish shared instructions", "err", err)
	}
}

// RefreshAgentSetupNow reads every attached repo again, moved or not (the
// page's Refresh), in the background.
func (s *Service) RefreshAgentSetupNow(ctx context.Context, actor *Actor) error {
	if err := actor.canWrite(); err != nil {
		return err
	}
	agentRefreshMu.Lock()
	if agentRefreshing {
		// A read is under way and may have passed some repos already: one
		// more follows it.
		agentRefreshAgain = true
		agentRefreshMu.Unlock()
		return nil
	}
	agentRefreshing = true
	agentRefreshMu.Unlock()
	bg := context.WithoutCancel(ctx)
	s.goBG(func() {
		for {
			agentRefreshMu.Lock()
			agentRefreshAgain = false
			agentRefreshMu.Unlock()
			if err := s.scanAgentRepos(bg, true); err != nil {
				s.log.WarnContext(ctx, "read agent files", "err", err)
			}
			agentRefreshMu.Lock()
			if !agentRefreshAgain || bg.Err() != nil {
				agentRefreshing = false
				agentRefreshMu.Unlock()
				return
			}
			agentRefreshMu.Unlock()
		}
	})
	return nil
}

// scanAgentRepos reads every attached repo's agent files (all of them when
// force, else those that moved) and drops the files of repos no longer attached.
func (s *Service) scanAgentRepos(ctx context.Context, force bool) error {
	repos, err := s.setupRepos(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range repos {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.scanAgentRepo(ctx, r.forgeID, r.repo, force); err != nil {
			errs = append(errs, err)
		}
	}
	if _, err := s.db.W.ExecContext(ctx, `DELETE FROM agent_repo_files WHERE NOT EXISTS (SELECT 1 FROM project_repos r
		WHERE r.forge_id = agent_repo_files.forge_id AND r.repo = agent_repo_files.repo COLLATE NOCASE)`); err != nil {
		errs = append(errs, err)
	}
	if _, err := s.db.W.ExecContext(ctx, `DELETE FROM agent_repo_scans WHERE NOT EXISTS (SELECT 1 FROM project_repos r
		WHERE r.forge_id = agent_repo_scans.forge_id AND r.repo = agent_repo_scans.repo COLLATE NOCASE)`); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// onAgentFilesPush reads a repo again when a push to its default branch
// changed an agent file.
func (s *Service) onAgentFilesPush(ctx context.Context, f *Forge, ev *forgeEvent, branch string) {
	if !onDefaultBranch(branch, ev.defaultBranch) {
		return
	}
	touched := false
	for _, c := range ev.commits {
		for _, p := range c.paths {
			if agentFileKind(p) != "" {
				touched = true
			}
		}
	}
	if !touched {
		return
	}
	bg := context.WithoutCancel(ctx)
	s.goBG(func() {
		if err := s.scanAgentRepo(bg, f.ID, ev.repo, false); err != nil {
			s.log.WarnContext(ctx, "read agent files", "repo", ev.repo, "err", err)
		}
	})
}

// scanAgentRepo reads one repo's agent files at its default branch's head,
// unless that head was read already (force reads anyway). A failure is
// recorded on the repo's row and returned.
func (s *Service) scanAgentRepo(ctx context.Context, forgeID int64, repo string, force bool) error {
	agentScanMu.Lock()
	defer agentScanMu.Unlock()
	cl, ok := s.Forges.(ForgeKitReader)
	if !ok {
		return errors.New("this forge client can't read files")
	}
	f, err := s.forgeByID(ctx, forgeID)
	if err != nil {
		return err
	}
	var lastHead, lastErr string
	err = s.db.R.QueryRowContext(ctx, `SELECT head, error FROM agent_repo_scans WHERE forge_id = ? AND repo = ? COLLATE NOCASE`, forgeID, repo).Scan(&lastHead, &lastErr)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	fail := func(what string, err error) error {
		msg := what + ": " + excerpt(err.Error(), 300)
		s.recordAgentScan(ctx, forgeID, repo, "", "", nil, msg)
		return errors.New(repo + ": " + msg)
	}
	branch, err := cl.DefaultBranch(ctx, f, repo)
	if err != nil {
		return fail("couldn't ask the forge for the default branch", err)
	}
	head, err := cl.BranchHead(ctx, f, repo, branch)
	if err != nil {
		return fail("couldn't read "+branch, err)
	}
	if !force && head == lastHead && lastErr == "" {
		_, err := s.db.W.ExecContext(ctx, `UPDATE agent_repo_scans SET scanned_at = ? WHERE forge_id = ? AND repo = ? COLLATE NOCASE`, s.nowMS(), forgeID, repo)
		return err
	}
	tree, err := cl.RepoTree(ctx, f, repo, head)
	if err != nil {
		return fail("couldn't list the files", err)
	}
	paths := pickAgentFiles(tree)
	files := make([]storedAgentFile, 0, len(paths))
	for _, p := range paths {
		body, err := cl.FileAt(ctx, f, repo, p, head)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return fail("couldn't read "+p, err)
		}
		files = append(files, newStoredFile(p, agentFileKind(p), body))
	}
	s.recordAgentScan(ctx, forgeID, repo, branch, head, files, "")
	return nil
}

// pickAgentFiles keeps a tree's agent files, at most maxAgentFiles: root
// files first, then the shallowest.
func pickAgentFiles(tree []string) []string {
	var out []string
	for _, p := range tree {
		if agentFileKind(p) != "" {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		di, dj := agentPathDepth(out[i]), agentPathDepth(out[j])
		if di != dj {
			return di < dj
		}
		return out[i] < out[j]
	})
	if len(out) > maxAgentFiles {
		out = out[:maxAgentFiles]
	}
	sort.Strings(out)
	return out
}

// agentPathDepth counts a path's folders, the .claude folder not counting.
func agentPathDepth(p string) int {
	if strings.HasPrefix(p, ".claude/") || p == ".mcp.json" {
		return 0
	}
	return strings.Count(p, "/")
}

// storedAgentFile is an agent file as kept.
type storedAgentFile struct {
	path, kind, content string
	size                int
	truncated           bool
}

func newStoredFile(p, kind, body string) storedAgentFile {
	f := storedAgentFile{path: p, kind: kind, size: len(body)}
	if !utf8.ValidString(body) || strings.ContainsRune(body, 0) {
		f.content, f.truncated = "", true
		return f
	}
	if len(body) > maxAgentFileBytes {
		body, f.truncated = truncateBytes(body, maxAgentFileBytes), true
	}
	f.content = body
	return f
}

// recordAgentScan stores a read: its files replace the last ones; a failed
// one (msg set) keeps them and says what went wrong.
func (s *Service) recordAgentScan(ctx context.Context, forgeID int64, repo, branch, head string, files []storedAgentFile, msg string) {
	now := s.nowMS()
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		if msg != "" {
			if _, err := t.ExecContext(ctx, `INSERT INTO agent_repo_scans(forge_id, repo, scanned_at, error) VALUES (?,?,?,?)
				ON CONFLICT(forge_id, repo) DO UPDATE SET scanned_at = excluded.scanned_at, error = excluded.error`, forgeID, repo, now, msg); err != nil {
				return err
			}
			t.emit(&Event{Kind: EvAgentSetupUpdated, Data: map[string]any{"repo": repo}})
			return nil
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO agent_repo_scans(forge_id, repo, branch, head, scanned_at, error) VALUES (?,?,?,?,?,'')
			ON CONFLICT(forge_id, repo) DO UPDATE SET branch = excluded.branch, head = excluded.head, scanned_at = excluded.scanned_at, error = ''`,
			forgeID, repo, branch, head, now); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM agent_repo_files WHERE forge_id = ? AND repo = ? COLLATE NOCASE`, forgeID, repo); err != nil {
			return err
		}
		for _, fl := range files {
			if _, err := t.ExecContext(ctx, `INSERT INTO agent_repo_files(forge_id, repo, path, kind, content, size, truncated) VALUES (?,?,?,?,?,?,?)`,
				forgeID, repo, fl.path, fl.kind, fl.content, fl.size, boolInt(fl.truncated)); err != nil {
				return err
			}
		}
		t.emit(&Event{Kind: EvAgentSetupUpdated, Data: map[string]any{"repo": repo}})
		return nil
	})
	if err != nil {
		s.log.WarnContext(ctx, "record agent files", "repo", repo, "err", err)
	}
}

// repoAgentFiles are a repo's stored files, path → file.
func (s *Service) repoAgentFiles(ctx context.Context, q queryer, forgeID int64, repo string, withContent bool) (map[string]storedAgentFile, error) {
	col := "''"
	if withContent {
		col = "content"
	}
	// #nosec G202 -- col is one of two fixed strings
	rows, err := q.QueryContext(ctx, `SELECT path, kind, `+col+`, size, truncated FROM agent_repo_files WHERE forge_id = ? AND repo = ? COLLATE NOCASE`, forgeID, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]storedAgentFile{}
	for rows.Next() {
		var f storedAgentFile
		if err := rows.Scan(&f.path, &f.kind, &f.content, &f.size, &f.truncated); err != nil {
			return nil, err
		}
		out[f.path] = f
	}
	return out, rows.Err()
}
