package core

import (
	"context"
	"fmt"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

var pathTokenRE = lazyre.New(`[A-Za-z0-9_][A-Za-z0-9_./-]*\.[A-Za-z0-9]{1,5}\b`)

const (
	estimateMaxTokens = 24
	estimateMaxFiles  = 6
)

// estimateRiskNotes describes the files named in text that a summary of the
// project's repos knows; "" when none is named or no repo has a summary.
func (s *Service) estimateRiskNotes(ctx context.Context, projectID int64, text string) string {
	var tokens []string
	seen := map[string]bool{}
	for _, t := range pathTokenRE.FindAllString(text, -1) {
		t = strings.Trim(t, "./-")
		if len(t) < 4 || seen[t] || strings.Contains(t, "..") {
			continue
		}
		seen[t] = true
		if tokens = append(tokens, t); len(tokens) >= estimateMaxTokens {
			break
		}
	}
	if len(tokens) == 0 {
		return ""
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT r.forge_id, r.forge_repo_id FROM project_repos r
		JOIN code_summaries c ON c.forge_id = r.forge_id AND c.forge_repo_id = r.forge_repo_id WHERE r.project_id = ?`, projectID)
	if err != nil {
		return ""
	}
	type repoKey struct{ forge, id int64 }
	var repos []repoKey
	for rows.Next() {
		var k repoKey
		if rows.Scan(&k.forge, &k.id) == nil {
			repos = append(repos, k)
		}
	}
	_ = rows.Close()
	var b strings.Builder
	n := 0
	for _, k := range repos {
		have := map[string]bool{}
		for _, tok := range tokens {
			if n >= estimateMaxFiles {
				break
			}
			var matches []string
			r2, err := s.db.R.QueryContext(ctx, `SELECT path FROM code_summary_files WHERE forge_id = ?1 AND forge_repo_id = ?2 AND (path = ?3 OR path LIKE '%/' || ?4 ESCAPE '\')
				ORDER BY path = ?3 DESC, churn DESC LIMIT 2`, k.forge, k.id, tok, likeEscape(tok))
			if err != nil {
				continue
			}
			for r2.Next() {
				var p string
				if r2.Scan(&p) == nil {
					matches = append(matches, p)
				}
			}
			_ = r2.Close()
			if len(matches) != 1 {
				continue
			}
			if have[matches[0]] {
				continue
			}
			have[matches[0]] = true
			files, err := s.codeFiles(ctx, k.forge, k.id, matches, 1)
			if err != nil || len(files) == 0 {
				continue
			}
			f := files[0]
			tags := []string{fmt.Sprintf("%d commits", f.Commits)}
			if f.Hotspot {
				tags = append(tags, "a hotspot")
			}
			if f.Fixes > 0 {
				tags = append(tags, fmt.Sprintf("%d past bug fixes", f.Fixes))
			}
			if f.Importers > 0 {
				tags = append(tags, fmt.Sprintf("%d importers", f.Importers))
			}
			tags = append(tags, fmt.Sprintf("%d lines", f.Lines))
			if ps, err := s.codePartners(ctx, k.forge, k.id, f.Path, 2); err == nil && len(ps) > 0 {
				names := make([]string, len(ps))
				for i, p := range ps {
					names[i] = p.Path
				}
				tags = append(tags, "usually changes with "+strings.Join(names, ", "))
			}
			fmt.Fprintf(&b, "- %s: %s\n", f.Path, strings.Join(tags, ", "))
			n++
		}
	}
	return b.String()
}
