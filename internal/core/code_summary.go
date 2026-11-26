package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	codeSummaryMaxFiles = 20000
	codeSummaryMaxPairs = 50000
)

// CodeFile is one file's numbers in a summary.
type CodeFile struct {
	Path       string     `json:"path"`
	Lang       string     `json:"lang,omitempty"`
	Size       int        `json:"size"`
	Lines      int        `json:"lines"`
	Symbols    int        `json:"symbols"`
	Importers  int        `json:"importers"`
	Commits    int        `json:"commits"`
	Churn      float64    `json:"churn"`
	HotPct     float64    `json:"hot_pct"`
	Hotspot    bool       `json:"hotspot"`
	Fixes      int        `json:"fixes"`
	LastFix    *time.Time `json:"last_fix,omitempty"`
	LastChange *time.Time `json:"last_change,omitempty"`
	LastAuthor string     `json:"last_author,omitempty"`
	Owner      string     `json:"owner,omitempty"`
	OwnerPct   float64    `json:"owner_pct,omitempty"`
}

// CodePair is two files that change together.
type CodePair struct {
	A       string  `json:"a"`
	B       string  `json:"b"`
	Weight  float64 `json:"weight"`
	Support int     `json:"support"`
}

// CodeSummaryUpload is the body of an upload (the client's codeindex.Summary).
type CodeSummaryUpload struct {
	Commit    string         `json:"commit"`
	IndexedAt time.Time      `json:"indexed_at"`
	Files     int            `json:"files"`
	Symbols   int            `json:"symbols"`
	Commits   int            `json:"commits"`
	Languages map[string]int `json:"languages"`
	Churn     []CodeFile     `json:"churn"`
	CoChange  []CodePair     `json:"cochange"`
}

// CodeSummaryInfo describes the summary a repo has.
type CodeSummaryInfo struct {
	RepoID     int64          `json:"repo_id"` // the project_repos row
	Repo       string         `json:"repo"`
	Commit     string         `json:"commit"`
	IndexedAt  time.Time      `json:"indexed_at"`
	UploadedAt time.Time      `json:"uploaded_at"`
	Files      int            `json:"files"`   // files in the repo
	Symbols    int            `json:"symbols"` // symbols in the repo
	Commits    int            `json:"history_commits"`
	Languages  map[string]int `json:"languages"`
	Stored     int            `json:"stored_files"` // files with history kept
	Pairs      int            `json:"pairs"`
}

// CodePartner is a file that usually changes with another.
type CodePartner struct {
	Path    string  `json:"path"`
	Support int     `json:"support"`
	Weight  float64 `json:"weight"`
}

// CodeFileRisk is a requested file's numbers and its co-change partners.
type CodeFileRisk struct {
	CodeFile
	Partners []CodePartner `json:"partners,omitempty"`
}

// CodeSummaryRead is the answer for a repo: its summary and some files.
type CodeSummaryRead struct {
	CodeSummaryInfo
	Requested []CodeFileRisk `json:"files"`   // the files asked for (those the summary has), or the top hotspots
	Missing   []string       `json:"unknown"` // files asked for that it has no history of
}

type summaryRepo struct {
	id, forgeID, forgeRepoID, projectID int64
	repo                                string
}

// summaryRepoRow finds an attached repo of a project; the forge's repo id
// must be known (Waypoint records it from webhooks and the periodic sync).
func (s *Service) summaryRepoRow(ctx context.Context, projectID, id int64) (summaryRepo, error) {
	r := summaryRepo{id: id, projectID: projectID}
	err := s.db.R.QueryRowContext(ctx, `SELECT forge_id, forge_repo_id, repo FROM project_repos WHERE id = ? AND project_id = ?`, id, projectID).
		Scan(&r.forgeID, &r.forgeRepoID, &r.repo)
	if err == sql.ErrNoRows {
		return r, notFound("repo")
	}
	return r, err
}

// UploadCodeSummary replaces a repo's summary (project editors).
func (s *Service) UploadCodeSummary(ctx context.Context, actor *Actor, projectRef string, repoID int64, in *CodeSummaryUpload) (*CodeSummaryInfo, error) {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleEditor); err != nil {
		return nil, err
	}
	r, err := s.summaryRepoRow(ctx, p.ID, repoID)
	if err != nil {
		return nil, err
	}
	if r.forgeRepoID == 0 {
		return nil, invalid("Waypoint doesn't know %s's forge id yet (it records it from the next webhook or sync); upload again then", r.repo)
	}
	if in == nil || in.Commit == "" {
		return nil, invalid("the summary names no commit")
	}
	if len(in.Churn) > codeSummaryMaxFiles || len(in.CoChange) > codeSummaryMaxPairs {
		return nil, invalid("the summary is too large (%d files, %d pairs; at most %d and %d)", len(in.Churn), len(in.CoChange), codeSummaryMaxFiles, codeSummaryMaxPairs)
	}
	langs, _ := json.Marshal(in.Languages)
	if in.Languages == nil {
		langs = []byte("{}")
	}
	indexed := in.IndexedAt
	now := s.now()
	if indexed.IsZero() || indexed.After(now.Add(time.Hour)) {
		indexed = now
	}
	err = s.write(ctx, actor, func(t *tx) error {
		for _, tbl := range []string{"code_summary_files", "code_summary_pairs", "code_summaries"} {
			if _, err := t.ExecContext(ctx, `DELETE FROM `+tbl+` WHERE forge_id = ? AND forge_repo_id = ?`, r.forgeID, r.forgeRepoID); err != nil {
				return err
			}
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO code_summaries(forge_id, forge_repo_id, repo, commit_sha, indexed_at, uploaded_at, uploaded_by, files, symbols, commits, languages)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`, r.forgeID, r.forgeRepoID, r.repo, in.Commit, indexed.Unix(), now.Unix(), actor.ID(), in.Files, in.Symbols, in.Commits, string(langs)); err != nil {
			return err
		}
		fs, err := t.PrepareContext(ctx, `INSERT OR REPLACE INTO code_summary_files(forge_id, forge_repo_id, path, lang, size, lines, symbols, importers, commits, churn, hot_pct, hotspot, fixes, last_fix_at, last_change_at, last_author, owner, owner_pct)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer func() { _ = fs.Close() }()
		for _, f := range in.Churn {
			if f.Path == "" {
				continue
			}
			if _, err := fs.ExecContext(ctx, r.forgeID, r.forgeRepoID, f.Path, f.Lang, f.Size, f.Lines, f.Symbols, f.Importers, f.Commits, f.Churn, f.HotPct, b2int(f.Hotspot), f.Fixes,
				unixOrZero(f.LastFix), unixOrZero(f.LastChange), f.LastAuthor, f.Owner, f.OwnerPct); err != nil {
				return err
			}
		}
		ps, err := t.PrepareContext(ctx, `INSERT OR REPLACE INTO code_summary_pairs(forge_id, forge_repo_id, a, b, weight, support) VALUES (?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer func() { _ = ps.Close() }()
		for _, c := range in.CoChange {
			a, b := c.A, c.B
			if a == "" || b == "" || a == b {
				continue
			}
			if a > b {
				a, b = b, a
			}
			if _, err := ps.ExecContext(ctx, r.forgeID, r.forgeRepoID, a, b, c.Weight, c.Support); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.codeSummaryInfo(ctx, r)
}

func b2int(b bool) int {
	if b {
		return 1
	}
	return 0
}

func unixOrZero(t *time.Time) int64 {
	if t == nil || t.IsZero() {
		return 0
	}
	return t.Unix()
}

func (s *Service) codeSummaryInfo(ctx context.Context, r summaryRepo) (*CodeSummaryInfo, error) {
	var info CodeSummaryInfo
	var indexed, uploaded int64
	var langs string
	err := s.db.R.QueryRowContext(ctx, `SELECT repo, commit_sha, indexed_at, uploaded_at, files, symbols, commits, languages,
		(SELECT count(*) FROM code_summary_files f WHERE f.forge_id = c.forge_id AND f.forge_repo_id = c.forge_repo_id),
		(SELECT count(*) FROM code_summary_pairs p WHERE p.forge_id = c.forge_id AND p.forge_repo_id = c.forge_repo_id)
		FROM code_summaries c WHERE forge_id = ? AND forge_repo_id = ?`, r.forgeID, r.forgeRepoID).
		Scan(&info.Repo, &info.Commit, &indexed, &uploaded, &info.Files, &info.Symbols, &info.Commits, &langs, &info.Stored, &info.Pairs)
	if err == sql.ErrNoRows {
		return nil, notFound("code summary")
	}
	if err != nil {
		return nil, err
	}
	info.RepoID, info.IndexedAt, info.UploadedAt = r.id, time.Unix(indexed, 0), time.Unix(uploaded, 0)
	_ = json.Unmarshal([]byte(langs), &info.Languages)
	return &info, nil
}

// CodeSummary reads a repo's summary, with the numbers and co-change
// partners of the given files (the top hotspots when none are named).
func (s *Service) CodeSummary(ctx context.Context, actor *Actor, projectRef string, repoID int64, files []string) (*CodeSummaryRead, error) {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
		return nil, err
	}
	r, err := s.summaryRepoRow(ctx, p.ID, repoID)
	if err != nil {
		return nil, err
	}
	info, err := s.codeSummaryInfo(ctx, r)
	if err != nil {
		return nil, err
	}
	out := &CodeSummaryRead{CodeSummaryInfo: *info, Requested: []CodeFileRisk{}, Missing: []string{}}
	if len(files) > 200 {
		files = files[:200]
	}
	var list []CodeFile
	if len(files) == 0 {
		list, err = s.codeFiles(ctx, r.forgeID, r.forgeRepoID, nil, 30)
	} else {
		list, err = s.codeFiles(ctx, r.forgeID, r.forgeRepoID, files, 0)
	}
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, f := range list {
		have[f.Path] = true
		partners, err := s.codePartners(ctx, r.forgeID, r.forgeRepoID, f.Path, 8)
		if err != nil {
			return nil, err
		}
		out.Requested = append(out.Requested, CodeFileRisk{CodeFile: f, Partners: partners})
	}
	for _, f := range files {
		if !have[f] {
			out.Missing = append(out.Missing, f)
		}
	}
	return out, nil
}

// codeFiles reads summary rows: the named paths, or the top limit by churn.
func (s *Service) codeFiles(ctx context.Context, forgeID, repoID int64, paths []string, limit int) ([]CodeFile, error) {
	q := `SELECT path, lang, size, lines, symbols, importers, commits, churn, hot_pct, hotspot, fixes, last_fix_at, last_change_at, last_author, owner, owner_pct
		FROM code_summary_files WHERE forge_id = ? AND forge_repo_id = ?`
	args := []any{forgeID, repoID}
	if len(paths) > 0 {
		q += ` AND path IN (` + strings.TrimSuffix(strings.Repeat("?,", len(paths)), ",") + `)` // #nosec G202 -- only "?" placeholders are joined in
		for _, p := range paths {
			args = append(args, p)
		}
	}
	q += ` ORDER BY hotspot DESC, churn DESC, path LIMIT ?`
	if limit <= 0 {
		limit = -1
	}
	args = append(args, limit)
	rows, err := s.db.R.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CodeFile
	for rows.Next() {
		var f CodeFile
		var hot int
		var fix, last int64
		if err := rows.Scan(&f.Path, &f.Lang, &f.Size, &f.Lines, &f.Symbols, &f.Importers, &f.Commits, &f.Churn, &f.HotPct, &hot, &f.Fixes, &fix, &last, &f.LastAuthor, &f.Owner, &f.OwnerPct); err != nil {
			return nil, err
		}
		f.Hotspot = hot != 0
		if fix > 0 {
			t := time.Unix(fix, 0)
			f.LastFix = &t
		}
		if last > 0 {
			t := time.Unix(last, 0)
			f.LastChange = &t
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Service) codePartners(ctx context.Context, forgeID, repoID int64, path string, limit int) ([]CodePartner, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT CASE WHEN a = ?3 THEN b ELSE a END, support, weight FROM code_summary_pairs
		WHERE forge_id = ?1 AND forge_repo_id = ?2 AND (a = ?3 OR b = ?3) ORDER BY weight DESC LIMIT ?4`, forgeID, repoID, path, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CodePartner
	for rows.Next() {
		var c CodePartner
		if err := rows.Scan(&c.Path, &c.Support, &c.Weight); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// summaryRepoID is the forge repo id an attached repo name has a summary
// under (0: none). Internal readers (the review bot, the estimator) use it
// without an actor: they act for the project already.
func (s *Service) summaryRepoID(ctx context.Context, forgeID int64, repo string) int64 {
	var id int64
	_ = s.db.R.QueryRowContext(ctx, `SELECT coalesce(max(c.forge_repo_id), 0) FROM project_repos r
		JOIN code_summaries c ON c.forge_id = r.forge_id AND c.forge_repo_id = r.forge_repo_id
		WHERE r.forge_id = ? AND lower(r.repo) = lower(?)`, forgeID, repo).Scan(&id)
	return id
}

// codeRiskNotes describes, for changed paths, what the repo's history says:
// hotspots, files with past bug fixes, and the partners that usually change
// with them but are not in the change. Short on purpose; "" when the repo has
// no summary or nothing stands out.
func (s *Service) codeRiskNotes(ctx context.Context, forgeID int64, repo string, changed []string) string {
	id := s.summaryRepoID(ctx, forgeID, repo)
	if id == 0 || len(changed) == 0 {
		return ""
	}
	if len(changed) > 100 {
		changed = changed[:100]
	}
	files, err := s.codeFiles(ctx, forgeID, id, changed, 0)
	if err != nil {
		return ""
	}
	in := map[string]bool{}
	for _, p := range changed {
		in[p] = true
	}
	var b strings.Builder
	n := 0
	for _, f := range files {
		if !f.Hotspot && f.Fixes < 2 {
			continue
		}
		if n++; n > 8 {
			break
		}
		var tags []string
		if f.Hotspot {
			tags = append(tags, "a hotspot (changes a lot)")
		}
		if f.Fixes > 0 {
			tags = append(tags, fmt.Sprintf("%d past bug fix%s", f.Fixes, map[bool]string{true: "", false: "es"}[f.Fixes == 1]))
		}
		if f.Importers >= 10 {
			tags = append(tags, fmt.Sprintf("%d importers", f.Importers))
		}
		fmt.Fprintf(&b, "- %s: %s\n", f.Path, strings.Join(tags, ", "))
	}
	type miss struct {
		path    string
		with    []string
		support int
	}
	missing := map[string]*miss{}
	for _, f := range files {
		if f.Commits == 0 {
			continue
		}
		ps, err := s.codePartners(ctx, forgeID, id, f.Path, 8)
		if err != nil {
			continue
		}
		for _, p := range ps {
			if in[p.Path] || p.Support < 3 || float64(p.Support)/float64(f.Commits) < 0.25 {
				continue
			}
			m := missing[p.Path]
			if m == nil {
				m = &miss{path: p.Path}
				missing[p.Path] = m
			}
			m.with = append(m.with, f.Path)
			m.support = max(m.support, p.Support)
		}
	}
	var ms []*miss
	for _, m := range missing {
		ms = append(ms, m)
	}
	sort.Slice(ms, func(i, j int) bool {
		if len(ms[i].with) != len(ms[j].with) {
			return len(ms[i].with) > len(ms[j].with)
		}
		if ms[i].support != ms[j].support {
			return ms[i].support > ms[j].support
		}
		return ms[i].path < ms[j].path
	})
	for i, m := range ms {
		if i == 0 {
			b.WriteString("Files that usually change with these but are not in this change:\n")
		}
		if i == 5 {
			break
		}
		fmt.Fprintf(&b, "- %s (with %s, %d commits)\n", m.path, m.with[0], m.support)
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String()
}
