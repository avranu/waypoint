package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// A push that only merges the base in, cleanly, changes nothing the review
// looked at (WP-1808). The server keeps no clone, so "the same change" is the
// pull request's diff against its base, normalised below; a head whose diff
// equals the last reviewed head's carries that review.

// diffHash hashes what a pull request changes and nothing a clean merge of
// its base moves: the files (paths, renames, modes, binary patches) and the
// added and removed lines in order. Left out: the "@@" line numbers, "index"
// and "similarity" lines, hunk boundaries and context lines. A merge that
// edits only the code around the change moves the context and the numbers,
// not the change; one that resolves a conflict changes added or removed
// lines. Context is left out because a base that changed nearby code would
// otherwise defeat the whole check; what that can hide is an interaction
// between the change and new neighbouring code, which CI on the merged tree
// (the full run, the merge line) tests.
func diffHash(diff string) string {
	h := sha256.New()
	inHunk, inBinary := false, false
	for raw := range strings.SplitSeq(diff, "\n") {
		l := strings.TrimRight(raw, "\r")
		switch {
		case strings.HasPrefix(l, "diff --git "):
			inHunk, inBinary = false, false
		case inBinary:
		case strings.HasPrefix(l, "GIT binary patch"):
			inBinary = true
		case inHunk && strings.HasPrefix(l, "@@"):
			continue
		case inHunk:
			if l == "" || l[0] == ' ' {
				continue // context
			}
		case strings.HasPrefix(l, "@@"):
			inHunk = true
			continue
		case strings.HasPrefix(l, "index "), strings.HasPrefix(l, "similarity index "), strings.HasPrefix(l, "dissimilarity index "):
			continue
		}
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// carriedPrefix starts the summary of a run that carried an earlier review.
const carriedPrefix = "Same change as "

func carriedSummary(from, earlier string) string {
	s := carriedPrefix + short(from) + ", already reviewed"
	if earlier != "" {
		s += ": " + earlier
	}
	return s
}

// carriedSignedOff: the review a run carried had signed its head off.
func carriedSignedOff(summary string) bool {
	return strings.HasPrefix(summary, carriedPrefix) && strings.Contains(summary, ": "+signedOffSummary)
}

// contentSince is when the head's content was first seen: the head's own
// time, or, when it only merged the base in over a reviewed head, that of the
// head whose diff last differed. The review wait and the expected reviewers'
// wait count from it; CI and the merge check keep head_at.
func (p *PullRequest) contentSince() *time.Time {
	if p.carriedSHA != "" && p.carriedSHA == p.HeadSHA && p.carriedAt != nil {
		return p.carriedAt
	}
	return p.headAt
}

// contentHeadSHA is the head whose diff last differed from its predecessor's.
func (p *PullRequest) contentHeadSHA() string {
	if p.carriedSHA != "" && p.carriedSHA == p.HeadSHA && p.carriedFromSHA != "" {
		return p.carriedFromSHA
	}
	return p.HeadSHA
}

// headCarry is the decision to carry a review to a new head.
type headCarry struct {
	from, hash, summary, contentSHA string
	contentAt                       int64
}

// noteReviewedHead records that a review finished on head, for later heads
// to carry. since is when the head's content was first seen.
func (s *Service) noteReviewedHead(ctx context.Context, prID int64, head, hash, summary, contentSHA string, since time.Time) {
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		_, err := t.ExecContext(ctx, `INSERT OR REPLACE INTO reviewed_heads(pr_id, head_sha, diff_hash, summary, content_sha, content_at, carried, from_sha, created_at)
			VALUES (?,?,?,?,?,?,0,'',?)`, prID, head, hash, summary, contentSHA, since.UnixMilli(), s.nowMS())
		return err
	})
	if err != nil {
		s.log.WarnContext(ctx, "record a reviewed head", "pr", prID, "head", short(head), "err", err)
	}
}

// carryDecision says whether the push from old's head to fp's only merged the
// base in: the head before was reviewed, and the new diff hashes the same.
// Any doubt (no review, a failed read) is no carry, and the head is reviewed.
func (s *Service) carryDecision(ctx context.Context, f *Forge, repo string, old *PullRequest, fp *ForgePR) *headCarry {
	cl, ok := s.Forges.(ForgePRs)
	if !ok || old == nil || old.HeadSHA == "" || fp.HeadSHA == "" || old.HeadSHA == fp.HeadSHA {
		return nil
	}
	var c headCarry
	var carried int
	err := s.db.R.QueryRowContext(ctx, `SELECT diff_hash, summary, content_sha, content_at, carried FROM reviewed_heads WHERE pr_id = ? AND head_sha = ?`,
		old.ID, old.HeadSHA).Scan(&c.hash, &c.summary, &c.contentSHA, &c.contentAt, &carried)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.log.WarnContext(ctx, "read a reviewed head", "pr", old.ID, "err", err)
		}
		return nil
	}
	var n int
	if s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM reviewed_heads WHERE pr_id = ? AND head_sha = ?`, old.ID, fp.HeadSHA).Scan(&n) != nil || n > 0 {
		return nil // a stale read bringing back a head it had
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	diff, err := cl.PRDiff(ctx, f, repo, fp)
	if err != nil {
		s.log.InfoContext(ctx, "no carry: the diff couldn't be read", "pr", old.ID, "head", short(fp.HeadSHA), "err", err)
		return nil
	}
	if diffHash(diff) != c.hash {
		return nil
	}
	c.from = old.HeadSHA
	return &c
}

// applyCarry records, in the transaction that records the new head, that it
// carries the earlier review: its waits keep counting from the earlier
// content, and a review run for it ends as "same change".
func (s *Service) applyCarry(ctx context.Context, t *tx, prID int64, head string, c *headCarry) error {
	if _, err := t.ExecContext(ctx, `UPDATE pull_requests SET carried_sha = ?, carried_from_sha = ?, carried_at = ? WHERE id = ?`,
		head, c.contentSHA, c.contentAt, prID); err != nil {
		return err
	}
	_, err := t.ExecContext(ctx, `INSERT OR REPLACE INTO reviewed_heads(pr_id, head_sha, diff_hash, summary, content_sha, content_at, carried, from_sha, created_at)
		VALUES (?,?,?,?,?,?,1,?,?)`, prID, head, c.hash, c.summary, c.contentSHA, c.contentAt, c.from, s.nowMS())
	return err
}

// carriedHead is the earlier review a run's head carries (WP-1808), when the
// head was recorded as the same change.
func (s *Service) carriedHead(ctx context.Context, prID int64, head string) (from, summary string, ok bool, err error) {
	err = s.db.R.QueryRowContext(ctx, `SELECT from_sha, summary FROM reviewed_heads WHERE pr_id = ? AND head_sha = ? AND carried = 1`, prID, head).Scan(&from, &summary)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	return from, summary, err == nil, err
}

// askCodeRabbit is the message of the "Ask CodeRabbit to review" automation.
const askCodeRabbit = "@coderabbitai review"

// noteOwnComment records a comment Waypoint posted itself, by the id the
// forge gave it (WP-1732), with what it asked and on which head.
func (s *Service) noteOwnComment(ctx context.Context, prID int64, c *ForgeComment, automationID int64, head, body string) {
	id := "unknown:" + strconv.FormatInt(s.nowMS(), 10)
	if c != nil && c.ID != "" {
		id = c.ID
	}
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		_, err := t.ExecContext(ctx, `INSERT OR REPLACE INTO own_comments(pr_id, ext_id, automation_id, head_sha, body, created_at) VALUES (?,?,?,?,?,?)`,
			prID, id, automationID, head, body, s.nowMS())
		return err
	})
	if err != nil {
		s.log.WarnContext(ctx, "record a comment Waypoint posted", "pr", prID, "err", err)
	}
}

// ownCommentIDs are the ids of the comments Waypoint posted on a pull request.
func (s *Service) ownCommentIDs(ctx context.Context, prID int64) map[string]bool {
	out := map[string]bool{}
	rows, err := s.db.R.QueryContext(ctx, `SELECT ext_id FROM own_comments WHERE pr_id = ?`, prID)
	if err != nil {
		s.log.WarnContext(ctx, "read the comments Waypoint posted", "pr", prID, "err", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// askedOnHead says whether an automation already posted body on a head.
func (s *Service) askedOnHead(ctx context.Context, prID, automationID int64, head, body string) (bool, error) {
	if head == "" {
		return false, nil
	}
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM own_comments WHERE pr_id = ? AND automation_id = ? AND head_sha = ? AND body = ?`,
		prID, automationID, head, body).Scan(&n)
	return n > 0, err
}
