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

// diffHash hashes what a pull request changes and the lines each change sits
// between: the files (paths, renames, modes, binary patches) and, for each run
// of added and removed lines, the run itself and the unchanged line just before
// and just after it (a marker where the hunk starts or ends there). Left out:
// the "@@" line numbers, "index" and "similarity" lines and the rest of the
// context. A merge of the base that moves a change down the file leaves the
// hash alone; one that edits the lines beside a change, or resolves a conflict,
// changes it, and the head is reviewed (the conservative side). Line endings
// are content: "+b" and "+b\r" differ. Without the neighbours, moving an added
// line from one function to another hashed the same.
func diffHash(diff string) string {
	h := sha256.New()
	put := func(l string) {
		h.Write([]byte(strconv.Itoa(len(l))))
		h.Write([]byte{':'})
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	const start, end = "\x00hunk-start", "\x00hunk-end"
	inHunk, inBinary := false, false
	before := start
	var run []string
	flush := func(after string) {
		if len(run) == 0 {
			return
		}
		put("<" + before)
		for _, l := range run {
			put(l)
		}
		put(">" + after)
		run = run[:0]
	}
	lines := strings.Split(diff, "\n")
	for n, l := range lines {
		if n == len(lines)-1 && l == "" {
			break
		}
		switch {
		case strings.HasPrefix(l, "diff --git "):
			flush(end)
			inHunk, inBinary, before = false, false, start
			put(l)
		case inBinary:
			put(l)
		case strings.HasPrefix(l, "GIT binary patch"):
			inBinary = true
			put(l)
		case strings.HasPrefix(l, "@@"):
			flush(end)
			inHunk, before = true, start
		case inHunk && (strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-")):
			run = append(run, l)
		case inHunk && strings.HasPrefix(l, "\\"):
			if len(run) > 0 {
				run = append(run, l) // "\ No newline at end of file" belongs to the line before
			}
		case inHunk: // an unchanged line
			flush(l)
			before = l
		case strings.HasPrefix(l, "index "), strings.HasPrefix(l, "similarity index "), strings.HasPrefix(l, "dissimilarity index "):
		default:
			put(l)
		}
	}
	flush(end)
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

// commitParenter lists a commit's parents (ForgeStacks has it, httpForgeClient
// answers it for Forgejo, Gitea and GitHub).
type commitParenter interface {
	CommitParents(ctx context.Context, f *Forge, repo, sha string) ([]string, error)
}

// headCarry is the decision to carry the reviews to a new head: one per
// review rule that applies to the pull request.
type headCarry struct {
	from, hash, contentSHA string
	contentAt              int64
	rules                  []carriedRule
}

type carriedRule struct {
	automationID int64
	summary      string
}

// ruleID is the key a review run's rule has in reviewed_heads: 0 for a run
// that has none (a manual one).
func ruleID(id *int64) int64 {
	if id == nil {
		return 0
	}
	return *id
}

// noteReviewedHead records that rule's review finished on head, for later
// heads to carry. since is when the head's content was first seen.
func (s *Service) noteReviewedHead(ctx context.Context, prID, automationID int64, head, hash, summary, contentSHA string, since time.Time) {
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		_, err := t.ExecContext(ctx, `INSERT OR REPLACE INTO reviewed_heads(pr_id, automation_id, head_sha, diff_hash, summary, content_sha, content_at, carried, from_sha, created_at)
			VALUES (?,?,?,?,?,?,?,0,'',?)`, prID, automationID, head, hash, summary, contentSHA, since.UnixMilli(), s.nowMS())
		return err
	})
	if err != nil {
		s.log.WarnContext(ctx, "record a reviewed head", "pr", prID, "head", short(head), "err", err)
	}
}

// carryDecision says whether the push from old's head to fp's only merged the
// base in, so the reviews of the head before still hold. All of these:
//   - the new head is a merge of exactly two parents whose first is the old
//     head, read from the forge (a rebase or an amend with an equal diff is
//     reviewed as before: a rebase rewrites the commits the review read, and
//     a forge that can't list parents, GitLab's, never carries);
//   - every review rule that applies to the pull request has a review that
//     finished on the old head (or carried to it), with this diff's hash: one
//     rule's failed or unfinished review is never taken for another's;
//   - the diff, read now, hashes the same, and the forge still has the pull
//     request at the new head after the read (the diff isn't pinned to a head).
//
// Any doubt (a failed read) is no carry, and every rule reviews the head.
func (s *Service) carryDecision(ctx context.Context, f *Forge, repo string, old *PullRequest, fp *ForgePR) *headCarry {
	cl, ok := s.Forges.(ForgePRs)
	sc, ok2 := s.Forges.(commitParenter)
	if !ok || !ok2 || old == nil || old.HeadSHA == "" || fp.HeadSHA == "" || old.HeadSHA == fp.HeadSHA {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ps, err := sc.CommitParents(ctx, f, repo, fp.HeadSHA)
	if err != nil || len(ps) != 2 || !sameSHA(ps[0], old.HeadSHA) {
		return nil
	}
	rules, err := s.reviewRulesFor(ctx, old)
	if err != nil || len(rules) == 0 {
		return nil
	}
	var n int
	if s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM reviewed_heads WHERE pr_id = ? AND head_sha = ?`, old.ID, fp.HeadSHA).Scan(&n) != nil || n > 0 {
		return nil // a stale read bringing back a head it had
	}
	c := &headCarry{from: old.HeadSHA}
	for _, a := range rules {
		var hash, summary, contentSHA string
		var contentAt int64
		err := s.db.R.QueryRowContext(ctx, `SELECT diff_hash, summary, content_sha, content_at FROM reviewed_heads WHERE pr_id = ? AND automation_id = ? AND head_sha = ?`,
			old.ID, a.ID, old.HeadSHA).Scan(&hash, &summary, &contentSHA, &contentAt)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				s.log.WarnContext(ctx, "read a reviewed head", "pr", old.ID, "err", err)
			}
			return nil
		}
		if c.hash != "" && hash != c.hash {
			return nil
		}
		c.hash = hash
		if c.contentSHA == "" || contentAt > c.contentAt { // the later: the longer wait
			c.contentSHA, c.contentAt = contentSHA, contentAt
		}
		c.rules = append(c.rules, carriedRule{a.ID, summary})
	}
	diff, err := cl.PRDiff(ctx, f, repo, fp)
	if err != nil {
		s.log.InfoContext(ctx, "no carry: the diff couldn't be read", "pr", old.ID, "head", short(fp.HeadSHA), "err", err)
		return nil
	}
	if cur, err := cl.GetPR(ctx, f, repo, fp.Number); err != nil || cur == nil || !sameSHA(cur.HeadSHA, fp.HeadSHA) {
		return nil
	}
	if diffHash(diff) != c.hash {
		return nil
	}
	return c
}

// applyCarry records, in the transaction that records the new head, that it
// carries the earlier reviews: its waits keep counting from the earlier
// content, and a review run for it ends as "same change".
func (s *Service) applyCarry(ctx context.Context, t *tx, prID int64, head string, c *headCarry) error {
	if _, err := t.ExecContext(ctx, `UPDATE pull_requests SET carried_sha = ?, carried_from_sha = ?, carried_at = ? WHERE id = ?`,
		head, c.contentSHA, c.contentAt, prID); err != nil {
		return err
	}
	for _, r := range c.rules {
		if _, err := t.ExecContext(ctx, `INSERT OR REPLACE INTO reviewed_heads(pr_id, automation_id, head_sha, diff_hash, summary, content_sha, content_at, carried, from_sha, created_at)
			VALUES (?,?,?,?,?,?,?,1,?,?)`, prID, r.automationID, head, c.hash, r.summary, c.contentSHA, c.contentAt, c.from, s.nowMS()); err != nil {
			return err
		}
	}
	return nil
}

// carriedHead is the earlier review a rule's run for head carries (WP-1808),
// when the head was recorded as the same change.
func (s *Service) carriedHead(ctx context.Context, prID, automationID int64, head string) (from, summary string, ok bool, err error) {
	err = s.db.R.QueryRowContext(ctx, `SELECT from_sha, summary FROM reviewed_heads WHERE pr_id = ? AND automation_id = ? AND head_sha = ? AND carried = 1`, prID, automationID, head).Scan(&from, &summary)
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
