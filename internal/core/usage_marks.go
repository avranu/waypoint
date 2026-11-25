package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// instructionPath says whether a changed file is one agents read as
// instructions.
func instructionPath(p string) bool {
	p = strings.TrimPrefix(p, "./")
	switch path.Base(p) {
	case "CLAUDE.md", "AGENTS.md":
		return true
	}
	return strings.HasPrefix(p, ".claude/") || strings.Contains(p, "/.claude/")
}

// onDefaultBranch says whether a push went to the repo's main line: the
// default branch the payload names, or main or master when it names none.
func onDefaultBranch(branch, def string) bool {
	if def != "" {
		return branch == def
	}
	return branch == "main" || branch == "master"
}

func markTitle(s string) string {
	return truncateBytes(strings.TrimSpace(firstLine(s)), 200)
}

// recordInstructionMarks keeps the commits of a push that changed
// instruction files. Best effort: a failure is logged, not returned.
func (s *Service) recordInstructionMarks(ctx context.Context, f *Forge, ev *forgeEvent, branch string) {
	if !onDefaultBranch(branch, ev.defaultBranch) {
		return
	}
	now := s.nowMS()
	for _, c := range ev.commits {
		var paths []string
		for _, p := range c.paths {
			if instructionPath(p) {
				paths = append(paths, p)
			}
		}
		if len(paths) == 0 || c.id == "" {
			continue
		}
		if err := s.addUsageMark(ctx, f.ID, ev.repo, c.id, now, c.author, markTitle(c.message), c.url, paths); err != nil {
			s.log.WarnContext(ctx, "record an instruction change", "repo", ev.repo, "sha", c.id, "err", err)
		}
	}
}

func (s *Service) addUsageMark(ctx context.Context, forgeID int64, repo, sha string, at int64, author, title, link string, paths []string) error {
	pj, err := json.Marshal(paths)
	if err != nil {
		return err
	}
	_, err = s.db.W.ExecContext(ctx, `INSERT INTO usage_marks(forge_id, repo, sha, kind, at, author, title, url, paths) VALUES (?,?,?,'instructions',?,?,?,?,?)
		ON CONFLICT(forge_id, repo, sha) DO UPDATE SET paths = CASE WHEN length(excluded.paths) > length(usage_marks.paths) THEN excluded.paths ELSE usage_marks.paths END`,
		forgeID, repo, sha, at, author, title, link, string(pj))
	return err
}

// PathCommit is a commit that changed a path, as the forge's history lists it.
type PathCommit struct {
	SHA     string
	Message string
	Author  string
	URL     string
	At      time.Time
}

// ForgePathHistory is the optional part of a ForgeClient that lists the
// commits of the default branch that changed a path (a file or a folder).
type ForgePathHistory interface {
	PathCommits(ctx context.Context, f *Forge, repo, path string, since time.Time) ([]PathCommit, error)
}

// pathHistoryPages bounds what one path's history reads (pages of 100).
const pathHistoryPages = 3

func (c *httpForgeClient) PathCommits(ctx context.Context, f *Forge, repo, p string, since time.Time) ([]PathCommit, error) {
	var out []PathCommit
	for page := 1; page <= pathHistoryPages; page++ {
		var u string
		q := url.QueryEscape(p)
		switch f.Kind {
		case "gitlab":
			u = fmt.Sprintf("%s/repository/commits?path=%s&since=%s&per_page=100&page=%d", glProject(repo), q, url.QueryEscape(since.UTC().Format(time.RFC3339)), page)
		case "github":
			u = fmt.Sprintf("/repos/%s/commits?path=%s&since=%s&per_page=100&page=%d", repo, q, url.QueryEscape(since.UTC().Format(time.RFC3339)), page)
		default:
			u = fmt.Sprintf("/repos/%s/commits?path=%s&limit=100&page=%d&stat=false&files=false&verification=false", repo, q, page)
		}
		var list []struct {
			SHA     string `json:"sha"`
			ID      string `json:"id"` // GitLab
			HTMLURL string `json:"html_url"`
			WebURL  string `json:"web_url"`
			Message string `json:"message"` // GitLab
			Created string `json:"created"` // Forgejo
			// GitLab
			CommittedDate string `json:"committed_date"`
			AuthorName    string `json:"author_name"`
			Commit        struct {
				Message   string `json:"message"`
				Committer struct {
					Date string `json:"date"`
				} `json:"committer"`
				Author struct {
					Name string `json:"name"`
				} `json:"author"`
			} `json:"commit"`
			Author *struct {
				Login    string `json:"login"`
				Username string `json:"username"`
			} `json:"author"`
		}
		if err := c.do(ctx, f, http.MethodGet, u, nil, &list); err != nil {
			if isStatus(err, http.StatusNotFound) {
				return nil, ErrNotFound
			}
			return nil, err
		}
		for _, x := range list {
			at, err := time.Parse(time.RFC3339, cmp(x.Commit.Committer.Date, cmp(x.CommittedDate, x.Created)))
			if err != nil {
				continue
			}
			pc := PathCommit{SHA: cmp(x.SHA, x.ID), Message: cmp(x.Commit.Message, x.Message), URL: cmp(x.HTMLURL, x.WebURL), At: at,
				Author: cmp(x.AuthorName, x.Commit.Author.Name)}
			if x.Author != nil {
				pc.Author = cmp(x.Author.Login, cmp(x.Author.Username, pc.Author))
			}
			if !at.Before(since) {
				out = append(out, pc)
			}
		}
		if len(list) < 100 {
			break
		}
	}
	return out, nil
}

// instructionHistoryPaths are what the backfill asks the forge's history
// about. Nested CLAUDE.md files only show up from pushes on.
var instructionHistoryPaths = []string{"CLAUDE.md", "AGENTS.md", ".claude"}

const usageMarksBackfillKey = "usage_marks_backfill|"

var usageMarksBackfillBusy atomic.Bool

// BackfillUsageMarks reads, once per attached repo, the last year of commits
// that changed instruction files, so before/after comparisons work on usage
// from before Waypoint kept them. A repo whose read fails is tried again
// the next hour.
func (s *Service) BackfillUsageMarks(ctx context.Context) {
	cl, ok := s.Forges.(ForgePathHistory)
	if !ok {
		jobSkip(ctx, "the forge client cannot read the history of a path")
		return
	}
	if !usageMarksBackfillBusy.CompareAndSwap(false, true) {
		jobBusy(ctx, "the last backfill is still going")
		return
	}
	defer usageMarksBackfillBusy.Store(false)
	repos, err := s.forgeRepoRefs(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "instruction history backfill: list repos", "err", err)
		return
	}
	since := time.UnixMilli(s.nowMS()).AddDate(-1, 0, 0)
	for _, r := range repos {
		if r.forge.token == "" || ctx.Err() != nil {
			continue
		}
		key := usageMarksBackfillKey + strconv.FormatInt(r.forge.ID, 10) + "|" + strings.ToLower(r.repo)
		var v string
		if s.db.R.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = ?`, key).Scan(&v) == nil {
			continue
		}
		n, failed := 0, false
		for _, p := range instructionHistoryPaths {
			list, err := cl.PathCommits(ctx, r.forge, r.repo, p, since)
			if err != nil && !errors.Is(err, ErrNotFound) {
				s.log.WarnContext(ctx, "instruction history backfill", "repo", r.repo, "path", p, "err", err)
				failed = true
				break
			}
			for _, c := range list {
				if c.SHA == "" {
					continue
				}
				if err := s.addUsageMark(ctx, r.forge.ID, r.repo, c.SHA, c.At.UnixMilli(), c.Author, markTitle(c.Message), c.URL, []string{p}); err != nil {
					failed = true
					break
				}
				n++
			}
		}
		if failed {
			continue
		}
		if _, err := s.db.W.ExecContext(ctx, `INSERT INTO server_settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			key, itoa64(s.nowMS())); err != nil {
			s.log.WarnContext(ctx, "best-effort write failed", "err", err)
		}
		if n > 0 {
			s.log.Info("instruction history backfilled", "repo", r.repo, "commits", n)
		}
	}
}

type forgeRepoRef struct {
	forge *Forge
	repo  string
}

// forgeRepoRefs is every repo attached to a project, on any forge, once.
func (s *Service) forgeRepoRefs(ctx context.Context) ([]forgeRepoRef, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT forge_id, min(repo) FROM project_repos GROUP BY forge_id, lower(repo) ORDER BY forge_id, lower(repo)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []forgeRepoRef
	forges := map[int64]*Forge{}
	for rows.Next() {
		var fid int64
		var repo string
		if err := rows.Scan(&fid, &repo); err != nil {
			return nil, err
		}
		if forges[fid] == nil {
			forges[fid] = &Forge{ID: fid}
		}
		out = append(out, forgeRepoRef{forge: forges[fid], repo: repo})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for id, f := range forges {
		full, err := s.forgeByID(ctx, id)
		if err != nil {
			return nil, err
		}
		*f = *full
	}
	return out, nil
}
