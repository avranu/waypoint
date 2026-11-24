package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// Waypoint's own kinds (session, files, after) are guesses; the data serves later review of what the first work should have caught.

// followFoundAfter is found on a follow-up Waypoint recorded because
// someone related a task to one done before it was filed (WP-1003).
const followFoundAfter = "after"

const (
	followUpWindow    = 60 * 24 * time.Hour
	followUpMinShared = 2
	followUpMinShare  = 0.5
	// A file more than one in followUpCommonOf of the window's pull requests
	// changed (lists, help text, the changelog) says nothing about which
	// work this continues; with fewer than followUpCommonMin of them
	// nothing is common.
	followUpCommonOf  = 4
	followUpCommonMin = 8
)

// fileSet is a merged pull request's files and the done tasks it carried.
type fileSet struct {
	pr     int64
	files  []string
	issues []int64
}

// bestFollowed is the index of the set in cands whose files cover most of
// mine, not counting files common across cands; -1 when none covers
// followUpMinShare of them with at least followUpMinShared. cands are
// newest first, so a tie goes to the more recent work.
func bestFollowed(mine []string, cands []fileSet) int {
	n, _ := bestFollowedShare(mine, cands)
	return n
}

// bestFollowedShare is bestFollowed with the share of mine (less the
// common files) that the best set changed: the follow-up's score.
func bestFollowedShare(mine []string, cands []fileSet) (int, float64) {
	common := map[string]bool{}
	if len(cands) >= followUpCommonMin {
		count := map[string]int{}
		for _, c := range cands {
			for _, f := range c.files {
				count[f]++
			}
		}
		for f, n := range count {
			if n*followUpCommonOf > len(cands) {
				common[f] = true
			}
		}
	}
	own := slices.DeleteFunc(slices.Clone(mine), func(f string) bool { return common[f] })
	if len(own) == 0 {
		return -1, 0
	}
	best, bestN := -1, 0
	for n, c := range cands {
		shared := 0
		for _, f := range own {
			if slices.Contains(c.files, f) {
				shared++
			}
		}
		if shared >= followUpMinShared && float64(shared) >= followUpMinShare*float64(len(own)) && shared > bestN {
			best, bestN = n, shared
		}
	}
	return best, float64(bestN) / float64(len(own))
}

// followUpsByFiles runs when a task's linked pull request merged (e is
// EvExternalPRMerged): the task follows the done tasks of the merged pull
// request in the same repo and project, in the window before, whose files
// cover most of this one's, unless the two are already related. Another
// project on the same repo is left out: this acts for no person, and the
// task's people may not see that project (as autoRelate).
func (s *Service) followUpsByFiles(ctx context.Context, e *Event) {
	if err := s.noteFollowUpsByFiles(ctx, e.IssueID, e.New); err != nil {
		s.log.WarnContext(ctx, "note follow-ups by files", "issue", e.IssueKey, "err", err)
	}
}

const linkedMergedPRSQL = `SELECT p.id FROM external_links el CROSS JOIN pull_requests p ON ` + pullLinkFromLinkSQL + `
	WHERE el.issue_id = ? AND el.url = ? AND p.state = 'merged' LIMIT 1`

func (s *Service) noteFollowUpsByFiles(ctx context.Context, issueID int64, linkURL string) error {
	var prID int64
	err := s.db.R.QueryRowContext(ctx, linkedMergedPRSQL, issueID, linkURL).Scan(&prID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return err
	}
	s.loadPRFiles(ctx, pr)
	if !pr.filesKnown || len(pr.files) == 0 || pr.MergedAt == nil {
		return nil
	}
	merged := pr.MergedAt.UnixMilli()
	// Only files read before: a pull request nobody read the diff of
	// isn't worth a forge call per candidate.
	rows, err := s.db.R.QueryContext(ctx, `SELECT p.id, p.files, el.issue_id
		FROM pull_requests p JOIN external_links el ON `+pullLinkSQL+`
		JOIN issues i ON i.id = el.issue_id JOIN statuses st ON st.id = i.status_id
		WHERE p.forge_id = ? AND p.repo = ? COLLATE NOCASE AND p.state = 'merged' AND p.id != ? AND p.files != ''
		AND p.merged_at < ? AND p.merged_at >= ? AND st.category = 'done' AND el.issue_id != ?
		AND i.project_id = (SELECT project_id FROM issues WHERE id = ?)
		ORDER BY p.merged_at DESC, p.id DESC`,
		pr.ForgeID, pr.Repo, pr.ID, merged, merged-followUpWindow.Milliseconds(), issueID, issueID)
	if err != nil {
		return err
	}
	var cands []fileSet
	at := map[int64]int{}
	for rows.Next() {
		var id, issue int64
		var raw string
		if err := rows.Scan(&id, &raw, &issue); err != nil {
			rows.Close()
			return err
		}
		if n, ok := at[id]; ok {
			cands[n].issues = append(cands[n].issues, issue)
			continue
		}
		var pf prFiles
		if json.Unmarshal([]byte(raw), &pf) != nil || pf.TooLarge {
			continue
		}
		at[id] = len(cands)
		cands = append(cands, fileSet{pr: id, files: pf.Paths, issues: []int64{issue}})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	n, share := bestFollowedShare(pr.files, cands)
	if n < 0 {
		return nil
	}
	return s.write(ctx, SystemActor(), func(t *tx) error {
		for _, other := range cands[n].issues {
			var related bool
			if err := t.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM issue_relations
				WHERE (source_id = ? AND target_id = ?) OR (source_id = ? AND target_id = ?))
				OR EXISTS (SELECT 1 FROM issues WHERE (id = ? AND parent_id = ?) OR (id = ? AND parent_id = ?))`,
				issueID, other, other, issueID, issueID, other, other, issueID).Scan(&related); err != nil {
				return err
			}
			if related {
				continue
			}
			if _, err := s.insertRelation(ctx, t, newRelation{Src: issueID, Dst: other, Kind: "follows", Found: "files", Basis: "inferred", Score: &share, Unrelated: true}); err != nil {
				return err
			}
		}
		return nil
	})
}
