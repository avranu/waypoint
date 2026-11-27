package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// EvBabysit: a babysit or a review comment changed (Data: pr_id).
const EvBabysit = "pr.babysit"

// Review comment verdicts.
const (
	VerdictNew         = "new"
	VerdictSeen        = "seen"
	VerdictAccepted    = "accepted"
	VerdictRejected    = "rejected"
	VerdictPartial     = "partial"
	VerdictNeedsPerson = "needs_person"
	VerdictFixing      = "fixing"
	VerdictFixed       = "fixed"
	VerdictVerified    = "verified"
	VerdictDismissed   = "dismissed"
	VerdictResolved    = "resolved"
	VerdictDeferred    = "deferred"
)

// verdictEmoji is the reaction each verdict puts on the forge (GitHub's
// names; Forgejo and GitHub allow eight): 👀 seen, 👍 accepted, 👎
// rejected, 😕 partly accepted or needs a person, 🚀 being fixed, 🎉 fixed,
// ❤️ the fix was checked by a second reviewer, 👀 put off to a follow-up
// task (as a mirror marks a deferred GitHub thread). Earlier marks stay,
// so the sequence is the history.
var verdictEmoji = map[string]string{
	VerdictSeen: "eyes", VerdictAccepted: "+1", VerdictRejected: "-1", VerdictPartial: "confused",
	VerdictNeedsPerson: "confused", VerdictFixing: "rocket", VerdictFixed: "hooray", VerdictVerified: "heart",
	VerdictDeferred: "eyes",
}

// ackVerdicts are what `wp pr ack` records; the ones that need a reason.
var (
	ackVerdicts     = []string{VerdictAccepted, VerdictRejected, VerdictPartial, VerdictNeedsPerson, VerdictDeferred}
	verdictsWithWhy = []string{VerdictRejected, VerdictPartial, VerdictNeedsPerson, VerdictDeferred}
)

// PRReviewComment is one review comment and what was done about it.
type PRReviewComment struct {
	Ref        string    `json:"id"`   // c<id>: what `wp pr ack` and `wp pr fixed` take
	Kind       string    `json:"kind"` // inline, review, comment, finding, relayed (an inline comment in a review a mirror copied, WP-413)
	Author     string    `json:"author"`
	Bot        bool      `json:"bot,omitempty"`
	Path       string    `json:"path,omitempty"`
	Line       int       `json:"line,omitempty"`
	SHA        string    `json:"sha,omitempty"`
	Body       string    `json:"body"`
	URL        string    `json:"url,omitempty"`
	Outdated   bool      `json:"outdated,omitempty"`
	Resolved   bool      `json:"resolved,omitempty"`    // the forge shows its thread resolved
	ResolvedBy string    `json:"resolved_by,omitempty"` // who dismissed or resolved it: "name (kind)"; the forge account is shared, so this is Waypoint's record
	ThreadID   string    `json:"thread,omitempty"`
	Replied    bool      `json:"replied,omitempty"` // Waypoint (an agent or the babysitter) answered it on the forge
	Verdict    string    `json:"verdict"`
	Note       string    `json:"note,omitempty"`
	FixSHA     string    `json:"fix_sha,omitempty"`
	Reactions  []string  `json:"reactions"`
	FindingID  int64     `json:"finding_id,omitempty"`
	Gone       bool      `json:"gone,omitempty"`
	Dealt      bool      `json:"dealt"`
	CreatedAt  time.Time `json:"created_at"`

	id, prID        int64
	extID           string
	threadID        string
	findingState    string
	findingSeverity string
	relayID         string
	relayAuthor     string
	mirror          string
}

// dealt says whether nothing more is owed on a comment: fixed (or the fix
// checked), dismissed, deferred to a follow-up task, rejected or partly
// accepted with a reply, on code a later push changed after it was
// accepted, its thread resolved, its finding settled, or deleted.
func (rc *PRReviewComment) dealt() bool {
	switch {
	case rc.Gone, rc.Resolved:
		return true
	case slices.Contains([]string{VerdictFixed, VerdictVerified, VerdictDismissed, VerdictResolved, VerdictDeferred}, rc.Verdict):
		return true
	case (rc.Verdict == VerdictRejected || rc.Verdict == VerdictPartial) && rc.Note != "":
		return true
	case rc.Outdated && (rc.Verdict == VerdictAccepted || rc.Verdict == VerdictFixing):
		return true
	case slices.Contains([]string{FindingFixed, FindingWithdrawn, FindingDismissed, FindingResolved}, rc.findingState):
		return true
	case rc.crAddressed():
		return true
	}
	return false
}

// crAddressed: CodeRabbit marked its own inline comment (or a relayed copy
// of one) addressed. Like Waypoint's bot verifying its finding fixed, the
// reviewer has re-read the code: the comment needs no round. Without this a
// comment CodeRabbit had already closed came back as work after every split
// or re-read, and babysits spent their rounds on it. A review's whole body
// can carry one finding's mark among others, so only one-finding comments count.
func (rc *PRReviewComment) crAddressed() bool {
	if rc.Kind != "inline" && rc.Kind != "relayed" || rc.Verdict == VerdictNeedsPerson || !rc.botComment() {
		return false
	}
	return addressedRE.MatchString(rc.Body)
}

// commentRef parses "c12" or "12".
func commentRef(ref string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(strings.ToLower(ref)), "c"), 10, 64)
	if err != nil || n <= 0 {
		return 0, invalid("%q isn't a review comment id (like c12)", ref)
	}
	return n, nil
}

const rcCols = `c.id, c.pr_id, c.kind, c.ext_id, c.thread_id, coalesce(c.finding_id, 0), c.author, c.bot, c.path, coalesce(c.line, 0), c.sha, c.body, c.url,
	c.outdated, c.resolved, c.verdict, c.note, c.fix_sha, c.reply_id, c.reactions, c.created_at, c.gone_at IS NOT NULL, coalesce(f.state, ''), coalesce(f.severity, ''), c.relay_id, c.relay_author,
	coalesce(mf.mirror_account, '')`

// rcFrom joins what rcCols reads: the finding a comment is, and its
// forge's mirror account.
const rcFrom = `review_comments c LEFT JOIN bot_findings f ON f.id = c.finding_id
	LEFT JOIN pull_requests mp ON mp.id = c.pr_id LEFT JOIN forges mf ON mf.id = mp.forge_id`

func scanRC(sc interface{ Scan(...any) error }) (*PRReviewComment, error) {
	var rc PRReviewComment
	var reactions string
	var replyID string
	var at int64
	if err := sc.Scan(&rc.id, &rc.prID, &rc.Kind, &rc.extID, &rc.threadID, &rc.FindingID, &rc.Author, &rc.Bot, &rc.Path, &rc.Line, &rc.SHA, &rc.Body, &rc.URL,
		&rc.Outdated, &rc.Resolved, &rc.Verdict, &rc.Note, &rc.FixSHA, &replyID, &reactions, &at, &rc.Gone, &rc.findingState, &rc.findingSeverity, &rc.relayID, &rc.relayAuthor, &rc.mirror); err != nil {
		return nil, err
	}
	rc.Ref = "c" + strconv.FormatInt(rc.id, 10)
	rc.ThreadID, rc.Replied = rc.threadID, replyID != ""
	rc.CreatedAt = msTime(at)
	_ = json.Unmarshal([]byte(reactions), &rc.Reactions)
	if rc.Reactions == nil {
		rc.Reactions = []string{}
	}
	rc.Dealt = rc.dealt()
	return &rc, nil
}

// reviewComments lists a pull request's review comments, oldest first.
func (s *Service) reviewComments(ctx context.Context, q queryer, prID int64) ([]*PRReviewComment, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+rcCols+` FROM `+rcFrom+`
		WHERE c.pr_id = ? ORDER BY c.created_at, c.id`, prID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*PRReviewComment{}
	for rows.Next() {
		rc, err := scanRC(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	who, err := q.QueryContext(ctx, `SELECT comment_id, by_name, by_kind FROM review_comment_events WHERE pr_id = ? AND verdict IN (?, ?) ORDER BY id`, prID, VerdictDismissed, VerdictResolved)
	if err != nil {
		return nil, err
	}
	defer who.Close()
	by := map[int64]string{}
	for who.Next() {
		var id int64
		var name, kind string
		if err := who.Scan(&id, &name, &kind); err != nil {
			return nil, err
		}
		by[id] = cmp(name, "someone") + " (" + kind + ")"
	}
	for _, rc := range out {
		if rc.Verdict == VerdictDismissed || rc.Verdict == VerdictResolved {
			rc.ResolvedBy = by[rc.id]
		}
	}
	return out, who.Err()
}

func (s *Service) reviewComment(ctx context.Context, q queryer, id int64) (*PRReviewComment, error) {
	rc, err := scanRC(q.QueryRowContext(ctx, `SELECT `+rcCols+` FROM `+rcFrom+` WHERE c.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("review comment")
	}
	return rc, err
}

// undealtComments lists the review comments not yet dealt with on a pull
// request Waypoint babysits (or gave up on, until a person stops it):
// readiness waits while there are any (ADR2 §3, "no comment left undealt
// with"). One that needs a person waits for a person. A bot's late minor
// finding doesn't hold it (WP-428, late_findings.go), nor a waived nitpick
// (WP-695, nitpicks.go), nor a copied finding
// on a file the pull request doesn't change (WP-432, outside_diff.go). A
// pull request that isn't babysat has none.
func (s *Service) undealtComments(ctx context.Context, pr *PullRequest) ([]UndealtComment, error) {
	if !s.babysitBlocks(ctx, pr.ID) {
		return nil, nil
	}
	list, err := s.reviewComments(ctx, s.db.R, pr.ID)
	if err != nil {
		return nil, err
	}
	late := map[string]bool{}
	if b, err := s.latestBabysit(ctx, s.db.R, pr.ID); err == nil && b != nil {
		late = s.lateMinors(ctx, pr, b, list)
	}
	elsewhere := s.outsideDiff(ctx, pr, list)
	var out []UndealtComment
	for _, rc := range list {
		if !rc.Dealt && !late[rc.Ref] && !elsewhere[rc.Ref] {
			out = append(out, UndealtComment{ID: rc.Ref, Author: rc.Author, URL: rc.URL})
		}
	}
	return out, nil
}

// rcIn is a review comment as the forge has it.
type rcIn struct {
	kind, extID, threadID, author, path, sha, body, url string
	line                                                int
	outdated, resolved, bot                             bool
	findingID                                           int64
	at, updated                                         int64
	resolvedBy                                          string
	relayID, relayAuthor                                string
}

// isReviewComment says whether a comment on pr is one someone has to deal
// with (see the file comment).
func isReviewComment(pr *PullRequest, author, body, kind, state string) bool {
	b := strings.TrimSpace(body)
	switch {
	case b == "", strings.EqualFold(author, pr.Author) && author != "":
		return false
	case isWaypointText(b) && !strings.Contains(b, "<!-- waypoint-finding"):
		return false
	case strings.HasPrefix(strings.ToLower(b), "@waypoint"):
		return false
	case kind == "review" && (state == "approved" || state == "pending"):
		return false
	}
	return true
}

// commentURL links to a comment where the forge gave no link.
func commentURL(f *Forge, pr *PullRequest, kind, id string) string {
	if pr.URL == "" || id == "" {
		return ""
	}
	switch f.Kind {
	case "github":
		if kind == "inline" {
			return pr.URL + "#discussion_r" + id
		}
		return pr.URL + "#issuecomment-" + id
	case "gitlab":
		return pr.URL + "#note_" + id
	}
	if kind == "inline" {
		return pr.URL + "/files#issuecomment-" + id
	}
	return pr.URL + "#issuecomment-" + id
}

// syncReviewComments reads a pull request's review comments from the forge
// and records them: new ones are added, edits update the text, deleted ones
// are marked gone; a thread resolved on the forge dismisses its comments,
// and a fix a second reviewer checked is marked verified (❤️). It returns
// how many comments are new.
func (s *Service) syncReviewComments(ctx context.Context, pr *PullRequest) (int, error) {
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return 0, err
	}
	ours := s.waypointPosters(ctx, f)
	var in []rcIn
	if th, ok := s.Forges.(ForgeThreads); ok {
		threads, err := th.ReviewThreads(ctx, f, pr.Repo, pr.Number)
		if err != nil {
			return 0, fmt.Errorf("read review threads: %w", err)
		}
		for _, t := range s.screenThreads(ctx, f, pr, threads) {
			for _, c := range t.Comments {
				if !isReviewComment(pr, c.Author, c.Body, "inline", "") {
					continue
				}
				in = append(in, rcIn{kind: "inline", extID: c.ID, threadID: t.ID, author: c.Author, path: t.Path, line: t.Line, sha: c.SHA,
					body: c.Body, url: commentURL(f, pr, "inline", c.ID), outdated: t.Outdated, resolved: t.Resolved, resolvedBy: t.ResolvedBy,
					bot: isBotLogin(c.Author) || ours.botText(c.Author, c.Body), at: c.At.UnixMilli(), updated: c.At.UnixMilli()})
			}
		}
	}
	if nt, ok := s.Forges.(ForgeReviewNotes); ok {
		notes, err := nt.PRNotes(ctx, f, pr.Repo, pr.Number)
		if err != nil {
			return 0, fmt.Errorf("read reviews and comments: %w", err)
		}
		whole := map[string]bool{}
		if prior, err := s.reviewComments(ctx, s.db.R, pr.ID); err == nil {
			split := map[string]bool{}
			for _, rc := range prior {
				if rc.Kind == "relayed" {
					if i := strings.LastIndex(rc.extID, "."); i > 0 {
						split[rc.extID[:i]] = true
					}
				}
			}
			for _, rc := range prior {
				if !rc.Gone && rc.Dealt && !split[rc.extID] {
					whole[rc.Kind+"|"+rc.extID] = true
				}
			}
		}
		own := s.ownCommentIDs(ctx, pr.ID)
		for _, n := range s.screenNotes(ctx, f, pr, notes) {
			if !isReviewComment(pr, n.Author, n.Body, n.Kind, n.State) {
				continue
			}
			// What Waypoint posted itself, by id; the exact text of its
			// CodeRabbit request from an account it posts as, for those
			// posted before the ids were kept (WP-1732).
			if n.Kind == "comment" && (own[n.ID] || (strings.TrimSpace(n.Body) == askCodeRabbit && ours.ours(n.Author))) {
				continue
			}
			for _, p := range noteParts(n, whole[n.Kind+"|"+n.ID], f.MirrorAccount, ours) {
				if !isReviewComment(pr, n.Author, p.body, p.kind, n.State) {
					continue
				}
				in = append(in, rcIn{kind: p.kind, extID: p.extID, author: n.Author, path: p.path, line: p.line, sha: n.SHA, body: p.body,
					url: cmp(n.URL, commentURL(f, pr, n.Kind, n.ID)), bot: p.bot, at: n.At.UnixMilli(), updated: max(n.UpdatedAt.UnixMilli(), n.At.UnixMilli()),
					relayID: p.relayID, relayAuthor: p.relayAuthor})
			}
		}
	}
	type fnd struct {
		id, run     int64
		ordinal     int
		commentID   string
		placement   string
		path, claim string
		severity    string
		line        int
		sha, state  string
		private     bool
		createdAt   int64
		reviewURL   string
	}
	frows, err := s.db.R.QueryContext(ctx, `SELECT f.id, f.run_id, f.ordinal, f.comment_id, f.placement, f.path, coalesce(f.line, 0), f.claim, f.severity, f.head_sha, f.state,
		f.private, f.created_at, coalesce(r.review_url, '') FROM bot_findings f LEFT JOIN bot_runs r ON r.id = f.run_id WHERE f.pr_id = ? AND f.state != 'dropped'`, pr.ID)
	if err != nil {
		return 0, err
	}
	var findings []fnd
	for frows.Next() {
		var x fnd
		if err := frows.Scan(&x.id, &x.run, &x.ordinal, &x.commentID, &x.placement, &x.path, &x.line, &x.claim, &x.severity, &x.sha, &x.state, &x.private, &x.createdAt, &x.reviewURL); err != nil {
			frows.Close()
			return 0, err
		}
		findings = append(findings, x)
	}
	frows.Close()
	for i := range in {
		if m := findingMarkerRE.FindStringSubmatch(in[i].body); m != nil && ours.ours(in[i].author) {
			run, _ := strconv.ParseInt(m[1], 10, 64)
			n, _ := strconv.Atoi(m[2])
			if j := slices.IndexFunc(findings, func(x fnd) bool { return x.run == run && x.ordinal == n }); j >= 0 {
				in[i].findingID = findings[j].id
			}
		}
		if in[i].findingID == 0 {
			if j := slices.IndexFunc(findings, func(x fnd) bool { return x.commentID != "" && x.commentID == in[i].extID }); j >= 0 {
				in[i].findingID = findings[j].id
			}
		}
		if in[i].findingID != 0 {
			in[i].author, in[i].bot = "waypoint", true
		}
	}
	for _, x := range findings {
		if (x.placement != "body" && x.placement != placementLater) || x.private || x.state != FindingOpen {
			continue
		}
		link := cmp(x.reviewURL, pr.URL)
		if x.placement == placementLater {
			link = prPageURL(s.BaseURL(), pr.ID)
		}
		in = append(in, rcIn{kind: "finding", extID: "f" + strconv.FormatInt(x.id, 10), author: "waypoint", bot: true, path: x.path, line: x.line,
			sha: x.sha, body: "[" + x.severity + "] " + x.claim, url: link, findingID: x.id, at: x.createdAt, updated: x.createdAt})
	}

	now := s.nowMS()
	var added, reopened int
	type change struct {
		id      int64
		to, who string
	}
	var dismiss []change
	type heldFix struct {
		id          int64
		fix, author string
	}
	var held []heldFix
	err = s.write(ctx, SystemActor(), func(t *tx) error {
		have := map[string]int64{}
		rows, err := t.QueryContext(ctx, `SELECT id, kind, ext_id, verdict, body, fix_sha FROM review_comments WHERE pr_id = ? AND gone_at IS NULL`, pr.ID)
		if err != nil {
			return err
		}
		verdicts, bodies, fixes := map[int64]string{}, map[int64]string{}, map[int64]string{}
		for rows.Next() {
			var id int64
			var kind, ext, v, body, fix string
			if err := rows.Scan(&id, &kind, &ext, &v, &body, &fix); err != nil {
				rows.Close()
				return err
			}
			have[kind+"|"+ext] = id
			verdicts[id], bodies[id], fixes[id] = v, body, fix
		}
		rows.Close()
		seen := map[string]bool{}
		for _, c := range in {
			key := c.kind + "|" + c.extID
			if seen[key] {
				continue
			}
			seen[key] = true
			var finding any
			if c.findingID != 0 {
				finding = c.findingID
			}
			var line any
			if c.line > 0 {
				line = c.line
			}
			body := excerpt(tidyReviewBody(c.author, c.body), storedCommentChars)
			var id int64
			err := t.QueryRowContext(ctx, `INSERT INTO review_comments(pr_id, kind, ext_id, thread_id, finding_id, author, bot, path, line, sha, body, url,
				outdated, resolved, created_at, updated_at, seen_at, relay_id, relay_author) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
				ON CONFLICT(pr_id, kind, ext_id) DO UPDATE SET thread_id = excluded.thread_id, finding_id = coalesce(excluded.finding_id, finding_id),
				path = excluded.path, line = coalesce(excluded.line, line), body = excluded.body, url = excluded.url, outdated = excluded.outdated,
				resolved = excluded.resolved, updated_at = max(updated_at, excluded.updated_at), gone_at = NULL,
				relay_id = CASE WHEN excluded.relay_id != '' THEN excluded.relay_id ELSE relay_id END,
				relay_author = CASE WHEN excluded.relay_author != '' THEN excluded.relay_author ELSE relay_author END
				RETURNING id`, pr.ID, c.kind, c.extID, c.threadID, finding, c.author, c.bot, c.path, line, c.sha, body, c.url,
				c.outdated, c.resolved, orMS(c.at, now), orMS(c.updated, now), now, c.relayID, c.relayAuthor).Scan(&id)
			if err != nil {
				return err
			}
			if _, ok := have[key]; !ok {
				var old int
				if err := t.QueryRowContext(ctx, `SELECT count(*) FROM review_comment_events WHERE comment_id = ?`, id).Scan(&old); err != nil {
					return err
				}
				if old == 0 {
					added++
					if _, err := t.ExecContext(ctx, `INSERT INTO review_comment_events(comment_id, pr_id, verdict, by_kind, by_name, sha, created_at) VALUES (?,?,?,?,?,?,?)`,
						id, pr.ID, VerdictNew, "forge", c.author, c.sha, now); err != nil {
						return err
					}
				}
			}
			_, known := have[key]
			edited := known && c.findingID == 0 && !c.resolved && reopenOnEdit(verdicts[id]) && botTextChanged(bodies[id], body)
			byPerson := false
			if edited && verdicts[id] == VerdictNeedsPerson {
				var perr error
				if byPerson, perr = s.verdictByPerson(ctx, t, id); perr != nil {
					return perr
				}
			}
			if edited && !byPerson {
				if fixedVerdict(verdicts[id]) && fixes[id] != "" && similarText(stripMarkers(bodies[id]), stripMarkers(body)) {
					held = append(held, heldFix{id: id, fix: fixes[id], author: c.author})
					delete(have, key)
					continue
				}
				if err := s.recordVerdict(ctx, t, id, pr.ID, VerdictNew, "", "", "", rcBy{kind: "forge", name: c.author}, 0, "edited on the forge"); err != nil {
					return err
				}
				reopened++
			}
			delete(have, key)
			if c.resolved && !slices.Contains([]string{VerdictFixed, VerdictVerified, VerdictDismissed, VerdictRejected, VerdictResolved, VerdictDeferred}, verdicts[id]) {
				dismiss = append(dismiss, change{id: id, to: VerdictDismissed, who: cmp(c.resolvedBy, "someone")})
			}
		}
		for _, id := range have {
			if _, err := t.ExecContext(ctx, `UPDATE review_comments SET gone_at = ?, body = CASE WHEN EXISTS (SELECT 1 FROM outside_pr_text o
				WHERE o.pr_id = review_comments.pr_id AND o.state != 'approved' AND (o.external_id = review_comments.ext_id
				OR (review_comments.kind = 'relayed' AND review_comments.ext_id LIKE o.external_id || '.%'))) THEN '' ELSE body END
				WHERE id = ?`, now, id); err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `INSERT INTO review_comment_events(comment_id, pr_id, old_verdict, verdict, by_kind, note, created_at) VALUES (?,?,?,?,?,?,?)`,
				id, pr.ID, verdicts[id], verdicts[id], "forge", "deleted", now); err != nil {
				return err
			}
		}
		for _, d := range dismiss {
			if err := s.recordVerdict(ctx, t, d.id, pr.ID, d.to, "", "", "", rcBy{kind: "forge", name: d.who}, 0, "thread resolved on the forge"); err != nil {
				return err
			}
		}
		if added > 0 || reopened > 0 || len(have) > 0 || len(dismiss) > 0 {
			projectID, err := s.attachedProject(ctx, t, pr.ForgeID, pr.Repo)
			if err != nil {
				return err
			}
			t.emit(&Event{Kind: EvBabysit, ProjectID: projectID, New: "comments", Data: map[string]any{"pr_id": pr.ID}})
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, h := range held {
		if s.fixOnHead(ctx, f, pr, h.fix) {
			continue
		}
		err := s.write(ctx, SystemActor(), func(t *tx) error {
			var cur, curFix string
			if err := t.QueryRowContext(ctx, `SELECT verdict, fix_sha FROM review_comments WHERE id = ?`, h.id).Scan(&cur, &curFix); err != nil {
				return err
			}
			if !fixedVerdict(cur) || curFix != h.fix {
				return nil
			}
			if err := s.recordVerdict(ctx, t, h.id, pr.ID, VerdictNew, "", "", "", rcBy{kind: "forge", name: h.author}, 0, "edited on the forge"); err != nil {
				return err
			}
			projectID, err := s.attachedProject(ctx, t, pr.ForgeID, pr.Repo)
			if err != nil {
				return err
			}
			t.emit(&Event{Kind: EvBabysit, ProjectID: projectID, New: "comments", Data: map[string]any{"pr_id": pr.ID}})
			return nil
		})
		if err != nil {
			s.log.WarnContext(ctx, "reopen an edited review comment", "pr", pr.ID, "comment", h.id, "err", err)
		}
	}
	s.verifyFixes(ctx, pr)
	return added, nil
}

// fixedVerdict: the comment was fixed by a pushed commit.
func fixedVerdict(v string) bool { return v == VerdictFixed || v == VerdictVerified }

// fixOnHead says whether the commit fix is in the pull request head's
// history. Not knowing (no way to ask the forge, or it fails) counts as no.
func (s *Service) fixOnHead(ctx context.Context, f *Forge, pr *PullRequest, fix string) bool {
	if sameSHA(fix, pr.HeadSHA) {
		return true
	}
	cl, ok := s.Forges.(ForgeUpToDate)
	if !ok || f == nil || pr.HeadSHA == "" {
		return false
	}
	has, err := cl.Contains(ctx, f, pr.Repo, fix, pr.HeadSHA)
	if err != nil {
		if readFault(err) && !isStatus(err, http.StatusNotFound) {
			s.log.WarnContext(ctx, "read whether a fix is in a pull request's head", "repo", pr.Repo, "number", pr.Number, "fix", short(fix), "err", err)
		}
		return false
	}
	return has
}

// orMS is a, or b when a is unset.
func orMS(a, b int64) int64 {
	if a > 0 {
		return a
	}
	return b
}

// verifyFixes marks fixed comments verified (❤️) once a second reviewer
// checked the fix: Waypoint's review found its finding fixed on the new
// head; the bot that made the comment reviewed a later head and didn't
// raise it again; or the person who made it approved since.
func (s *Service) verifyFixes(ctx context.Context, pr *PullRequest) {
	list, err := s.reviewComments(ctx, s.db.R, pr.ID)
	if err != nil {
		return
	}
	owed := verifiedRepliesPerSync
	for _, rc := range list {
		if rc.Gone {
			continue
		}
		if rc.Verdict == VerdictVerified {
			if owed > 0 && s.answerVerified(ctx, pr, rc) {
				owed--
			}
			continue
		}
		by := rcBy{kind: "verify"}
		switch {
		case rc.FindingID != 0 && rc.findingState == FindingFixed:
			by.name = "waypoint"
		case rc.FindingID == 0 && rc.Verdict == VerdictFixed:
			who, ok := s.checkedAgain(ctx, pr, rc, list)
			if !ok {
				continue
			}
			by.name = who
		default:
			continue
		}
		if err := s.markComment(ctx, pr, rc, VerdictVerified, "", "", by, 0); err != nil {
			s.log.WarnContext(ctx, "mark a review comment verified", "pr", pr.ID, "comment", rc.Ref, "err", err)
			continue
		}
		if owed > 0 && s.answerVerified(ctx, pr, rc) {
			owed--
		}
	}
}

// verifiedRepliesPerSync caps the replies one sync posts, so a pull request
// with a long backlog of verified comments doesn't flood its thread.
const verifiedRepliesPerSync = 5

// verifiedReply says on the forge what a reaction doesn't: Forgejo has no way
// to resolve a thread from its API, so a verified comment with no reply reads
// as ignored (WP-750).
const verifiedReply = "Fixed and checked: a later review of the fix didn't raise this again."

// answerVerified replies to a verified comment that has no reply yet, on an
// open pull request, and reports whether it posted one.
func (s *Service) answerVerified(ctx context.Context, pr *PullRequest, rc *PRReviewComment) bool {
	if rc.Replied || rc.Resolved || rc.Kind == "finding" || (pr.State != "open" && pr.State != "draft") {
		return false
	}
	if err := s.postReply(ctx, pr, rc, verifiedReply, rcBy{kind: "babysitter", name: "waypoint"}, s.roundOf(ctx, pr.ID)); err != nil {
		s.log.WarnContext(ctx, "reply to a verified review comment", "pr", pr.ID, "comment", rc.Ref, "err", err)
		return false
	}
	return true
}

// checkedAgain says whether the author of a fixed comment checked the fix:
// a bot reviewed a later head without raising it again, or a person
// approved since it was fixed.
func (s *Service) checkedAgain(ctx context.Context, pr *PullRequest, rc *PRReviewComment, all []*PRReviewComment) (string, bool) {
	var fixedAt int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT coalesce(max(created_at), 0) FROM review_comment_events WHERE comment_id = ? AND verdict = ?`,
		rc.id, VerdictFixed).Scan(&fixedAt); err != nil || fixedAt == 0 {
		return "", false
	}
	kinds := "'approved'"
	if rc.Bot {
		kinds = "'commented','reviewed','approved','changes_requested'"
	}
	var n int
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM pr_events WHERE pr_id = ? AND actor = ? COLLATE NOCASE AND kind IN (`+kinds+`)
		AND created_at > ? AND (sha = '' OR sha != ?)`, pr.ID, rc.Author, fixedAt, rc.SHA).Scan(&n); err != nil || n == 0 {
		return "", false
	}
	if rc.Bot {
		for _, o := range all {
			if o.id != rc.id && strings.EqualFold(o.Author, rc.Author) && o.CreatedAt.UnixMilli() > fixedAt &&
				(rc.Path == "" || o.Path == rc.Path) && similarText(o.Body, rc.Body) {
				return "", false
			}
		}
	}
	return rc.Author, true
}

var simWordRE = lazyre.New(`[a-z0-9_]{4,}`)

// similarText says whether two comments make much the same point: the
// longer words both use are half or more of the words either uses (Jaccard
// 0.5, about two thirds of each for bodies of equal length).
func similarText(a, b string) bool {
	wa, wb := map[string]bool{}, map[string]bool{}
	for _, w := range simWordRE.FindAllString(strings.ToLower(a), -1) {
		wa[w] = true
	}
	for _, w := range simWordRE.FindAllString(strings.ToLower(b), -1) {
		wb[w] = true
	}
	if len(wa) == 0 || len(wb) == 0 {
		return false
	}
	both := 0
	for w := range wa {
		if wb[w] {
			both++
		}
	}
	return float64(both)/float64(len(wa)+len(wb)-both) >= 0.5
}

// rcBy is who gave a verdict: person, agent, babysitter, forge, verify.
type rcBy struct {
	kind, name string
	userID     int64
}

func rcByActor(a *Actor) rcBy {
	if a.IsAgent() {
		return rcBy{kind: "agent", name: cmp(a.Agent, "an agent"), userID: a.ID()}
	}
	if a.User == nil || a.User.ID == 0 {
		return rcBy{kind: "babysitter", name: "waypoint"}
	}
	return rcBy{kind: "person", name: a.User.Username, userID: a.User.ID}
}

// recordVerdict changes a comment's verdict inside t and records it.
func (s *Service) recordVerdict(ctx context.Context, t *tx, id, prID int64, to, note, fixSHA, reaction string, by rcBy, round int, why string) error {
	var old string
	if err := t.QueryRowContext(ctx, `SELECT verdict FROM review_comments WHERE id = ?`, id).Scan(&old); err != nil {
		return err
	}
	if to == VerdictSeen && old != VerdictNew {
		return nil
	}
	if _, err := t.ExecContext(ctx, `UPDATE review_comments SET verdict = ?, note = CASE WHEN ? != '' THEN ? ELSE note END,
		fix_sha = CASE WHEN ? != '' THEN ? ELSE fix_sha END WHERE id = ?`, to, note, excerpt(note, 2000), fixSHA, fixSHA, id); err != nil {
		return err
	}
	var user any
	if by.userID != 0 {
		user = by.userID
	}
	_, err := t.ExecContext(ctx, `INSERT INTO review_comment_events(comment_id, pr_id, old_verdict, verdict, reaction, by_kind, by_name, user_id, round, sha, note, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, id, prID, old, to, reaction, by.kind, by.name, user, round, fixSHA, excerpt(cmp(note, why), 2000), s.nowMS())
	return err
}

// markComment gives a comment a verdict and marks it on the forge, as
// Waypoint's bot where one is set up: the verdict's emoji (once), a reply
// when there is something to say (always for a rejection, a partial
// acceptance or a comment that needs a person; for a fix, only with a
// note), and the thread resolved when it is fixed or rejected (where the
// forge allows). A rejected or dismissed Waypoint finding is dismissed, so
// it stops blocking the review gate.
func (s *Service) markComment(ctx context.Context, pr *PullRequest, rc *PRReviewComment, to, note, fixSHA string, by rcBy, round int) error {
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return err
	}
	bot := botForge(f)
	text := note
	emoji := verdictEmoji[to]
	replyID := ""
	sha := cmp(cmp(fixSHA, rc.FixSHA), pr.HeadSHA)
	if body := verdictReply(rc, to, text, sha, sha); body != "" {
		replyID = s.replyToComment(ctx, f, bot, pr, rc, body)
		if replyID == "" && rc.relayID != "" {
			return invalid("the forge didn't take the verdict reply on %s; try again", rc.Ref)
		}
	}
	posted := ""
	if emoji != "" && !slices.Contains(rc.Reactions, emoji) && rc.Kind != "finding" && rc.Kind != "relayed" {
		if nt, ok := s.Forges.(ForgeReviewNotes); ok {
			if err := nt.ReactTo(ctx, bot, pr.Repo, pr.Number, rc.Kind, rc.extID, emoji); err == nil {
				posted = emoji
			} else if !errors.Is(err, ErrUnsupported) {
				s.log.WarnContext(ctx, "react to a review comment", "pr", pr.ID, "comment", rc.Ref, "err", err)
			}
		}
	}
	onForge := false
	if (to == VerdictFixed || to == VerdictRejected || to == VerdictResolved || to == VerdictDeferred) && rc.Kind == "inline" && rc.threadID != "" {
		if th, ok := s.Forges.(ForgeThreads); ok {
			fp := &ForgePR{Number: pr.Number, HeadSHA: pr.HeadSHA}
			err := th.ResolveThread(ctx, bot, pr.Repo, fp, ReviewThread{ID: rc.threadID, Path: rc.Path, Line: rc.Line})
			onForge = err == nil
			if err != nil && !errors.Is(err, ErrUnsupported) {
				s.log.WarnContext(ctx, "resolve a review thread", "pr", pr.ID, "comment", rc.Ref, "err", err)
			}
		}
	}
	return s.write(ctx, SystemActor(), func(t *tx) error {
		if err := s.recordVerdict(ctx, t, rc.id, pr.ID, to, note, fixSHA, posted, by, round, ""); err != nil {
			return err
		}
		if posted != "" {
			rc.Reactions = append(rc.Reactions, posted)
			b, _ := json.Marshal(rc.Reactions)
			if _, err := t.ExecContext(ctx, `UPDATE review_comments SET reactions = ? WHERE id = ?`, string(b), rc.id); err != nil {
				return err
			}
		}
		if replyID != "" {
			if _, err := t.ExecContext(ctx, `UPDATE review_comments SET reply_id = ? WHERE id = ?`, replyID, rc.id); err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `UPDATE review_comment_events SET replied = 1 WHERE id = (SELECT max(id) FROM review_comment_events WHERE comment_id = ?)`, rc.id); err != nil {
				return err
			}
		}
		if onForge {
			if _, err := t.ExecContext(ctx, `UPDATE review_comments SET resolved = 1 WHERE id = ?`, rc.id); err != nil {
				return err
			}
			rc.Resolved = true
		}
		if rc.FindingID != 0 && slices.Contains([]string{VerdictRejected, VerdictDismissed, VerdictResolved, VerdictDeferred}, to) && rc.findingState == FindingOpen {
			fb := findingBy(by)
			if fb.kind == "babysitter" {
				fb.kind = "system"
			}
			state := FindingDismissed
			if to == VerdictResolved || to == VerdictDeferred {
				state = FindingResolved
			}
			if _, err := s.setFindingState(ctx, t, rc.FindingID, state, fb, pr.HeadSHA, cmp(note, state)); err != nil {
				return err
			}
			rc.findingState = state
		}
		rc.Verdict = to
		projectID, err := s.attachedProject(ctx, t, pr.ForgeID, pr.Repo)
		if err != nil {
			return err
		}
		t.emit(&Event{Kind: EvBabysit, ProjectID: projectID, New: to, Data: map[string]any{"pr_id": pr.ID, "comment": rc.Ref}})
		return nil
	})
}

// packetCommentChars is how much of a comment the work packet shows;
// storedCommentChars, how much Waypoint keeps.
const (
	packetCommentChars = 4000
	storedCommentChars = 20000
)

// reopenOnEdit says whether a comment with this verdict is to deal with
// again when an edit changes what it asks: once it was dealt with or given
// a verdict, but not while it is being fixed.
func reopenOnEdit(verdict string) bool {
	return verdict != VerdictNew && verdict != VerdictSeen && verdict != VerdictFixing
}

// verdictReply is what Waypoint says on the forge with a verdict ("" for
// nothing: the emoji speaks for itself). A comment with no emoji of its own
// or one copied from GitHub always gets one, with the marker that takes it
// to its GitHub thread (verdict_replies.go). markerSHA is the commit the
// marker names for a fix: a mirror resolves the thread once GitHub has it.
func verdictReply(rc *PRReviewComment, to, note, fixSHA, markerSHA string) string {
	note = strings.TrimSpace(note)
	always := rc.repliesToVerdicts()
	var lead, bare string
	switch to {
	case VerdictRejected:
		lead, bare = "Not changing this: ", "Not changing this."
	case VerdictPartial:
		lead, bare = "Partly taken: ", "Partly taken."
	case VerdictNeedsPerson:
		lead, bare = "This needs a person: ", "This needs a person to decide."
	case VerdictAccepted:
		bare = "Taken; fixing in this round."
	case VerdictResolved:
		bare = "Resolved."
	case VerdictDeferred:
		bare = "Put off to a follow-up task."
	case VerdictFixed:
		bare = "Fixed."
		if fixSHA != "" {
			lead, bare = "Fixed in `"+short(fixSHA)+"`: ", "Fixed in `"+short(fixSHA)+"`."
		}
	case VerdictDismissed:
		if !always {
			return ""
		}
		lead, bare = "Not changing this: ", "Not changing this."
		if deferredRE.MatchString(note) {
			lead, bare = "Out of scope here: ", "Out of scope here."
		}
	default:
		return ""
	}
	text := lead + note
	switch {
	case note != "":
	case always:
		text = bare
	default:
		return ""
	}
	if rc.Kind != "inline" {
		text = replyLead(rc, "") + text
	}
	return verdictMarker(rc, to, note, markerSHA) + text
}

var htmlCommentRE = lazyre.New(`(?s)<!--.*?-->`)

func stripMarkers(s string) string { return strings.TrimSpace(htmlCommentRE.ReplaceAllString(s, "")) }

// replyToComment answers a comment on the forge, in its thread where it has
// one, and returns the reply's id ("" when it couldn't).
func (s *Service) replyToComment(ctx context.Context, f, bot *Forge, pr *PullRequest, rc *PRReviewComment, body string) string {
	body += "\n<!-- waypoint-reply -->"
	switch rc.Kind {
	case "finding":
	case "inline":
		th, ok := s.Forges.(ForgeThreads)
		if !ok {
			return ""
		}
		threads, err := th.ReviewThreads(ctx, f, pr.Repo, pr.Number)
		if err != nil {
			s.log.WarnContext(ctx, "read review threads to reply", "pr", pr.ID, "err", err)
			return ""
		}
		for _, t := range threads {
			if t.ID != rc.threadID && !slices.ContainsFunc(t.Comments, func(c ThreadComment) bool { return c.ID == rc.extID }) {
				continue
			}
			id, err := th.ReplyToThread(ctx, bot, pr.Repo, &ForgePR{Number: pr.Number, HeadSHA: pr.HeadSHA}, t, body)
			if err == nil {
				return cmp(id, "posted")
			}
			if !errors.Is(err, ErrUnsupported) {
				s.log.WarnContext(ctx, "reply in a review thread", "pr", pr.ID, "comment", rc.Ref, "err", err)
				return ""
			}
			break
		}
		body = replyLead(rc, rc.Path) + body
	}
	prc, ok := s.Forges.(ForgePRs)
	if !ok {
		return ""
	}
	c, err := prc.PostPRComment(ctx, bot, pr.Repo, pr.Number, body)
	if err != nil {
		s.log.WarnContext(ctx, "reply to a review comment", "pr", pr.ID, "comment", rc.Ref, "err", err)
		return ""
	}
	return cmp(c.ID, "posted")
}

// AckInput is a verdict on review comments.
type AckInput struct {
	IDs     []string `json:"ids"`
	Verdict string   `json:"verdict"` // accepted, rejected, partial, deferred, needs_person
	Note    string   `json:"note"`    // why; required but for accepted
}

// FixedInput says review comments are fixed.
type FixedInput struct {
	IDs  []string `json:"ids"`
	Note string   `json:"note"`
	// SHA is the commit with the fix. Not yet the pull request's head (not
	// pushed yet), the comments are being fixed (🚀) until the next push.
	SHA string `json:"sha"`
}

// commentsFor loads the comments named and checks the actor may give them a
// verdict: an editor of a project the pull request's repo is attached to.
func (s *Service) commentsFor(ctx context.Context, actor *Actor, ids []string) ([]*PRReviewComment, map[int64]*PullRequest, error) {
	return s.loadComments(ctx, actor, ids, true)
}

// loadComments is commentsFor; guard keeps agents from changing what a person decided.
func (s *Service) loadComments(ctx context.Context, actor *Actor, ids []string, guard bool) ([]*PRReviewComment, map[int64]*PullRequest, error) {
	if err := actor.Need(PermPullsWrite); err != nil {
		return nil, nil, err
	}
	if len(ids) == 0 {
		return nil, nil, invalid("name the review comments (like c12; `wp pr todo` lists them)")
	}
	if len(ids) > 100 {
		return nil, nil, invalid("at most 100 comments at a time")
	}
	prs := map[int64]*PullRequest{}
	var out []*PRReviewComment
	for _, ref := range ids {
		id, err := commentRef(ref)
		if err != nil {
			return nil, nil, err
		}
		rc, err := s.reviewComment(ctx, s.db.R, id)
		if err != nil {
			return nil, nil, err
		}
		pr := prs[rc.prID]
		if pr == nil {
			if pr, err = s.prByID(ctx, s.db.R, rc.prID); err != nil {
				return nil, nil, err
			}
			if _, err := s.prAuthority(ctx, actor, pr, RoleEditor); err != nil {
				return nil, nil, err
			}
			prs[pr.ID] = pr
		}
		if rc.Gone {
			return nil, nil, invalid("%s was deleted on the forge", rc.Ref)
		}
		byPerson := false
		if guard && actor.IsAgent() {
			if byPerson, err = s.verdictByPerson(ctx, s.db.R, rc.id); err != nil {
				return nil, nil, err
			}
		}
		if byPerson {
			return nil, nil, invalid("%s waits for a person (%s); an agent can't change that", rc.Ref, rc.Verdict)
		}
		out = append(out, rc)
	}
	return out, prs, nil
}

// verdictByPerson says whether a person gave the comment the verdict it has
// now: the latest event with a verdict says who did (a reply has none). A
// forge's event counts: a comment reopened by an edit is nobody's call yet.
// No event is a plain no; a read that failed is an error, never "nobody's
// call" (an agent could then overrule a person's verdict, WP-1406).
func (s *Service) verdictByPerson(ctx context.Context, q queryer, id int64) (bool, error) {
	var kind string
	err := q.QueryRowContext(ctx, `SELECT by_kind FROM review_comment_events WHERE comment_id = ? AND verdict != '' ORDER BY id DESC LIMIT 1`, id).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read verdict by person", "id", id, "err", err)
		}
		return false, err
	}
	return kind == "person", nil
}

// roundOf is the round of the pull request's babysit, 0 when none.
func (s *Service) roundOf(ctx context.Context, prID int64) int {
	var n int
	_ = s.db.R.QueryRowContext(ctx, `SELECT round FROM babysits WHERE pr_id = ? ORDER BY id DESC LIMIT 1`, prID).Scan(&n)
	return n
}

// AckReviewComments records a verdict on review comments (`wp pr ack`):
// accepted 👍, rejected 👎 (with a reply saying why), partly accepted or
// needing a person 😕 (with a reply), or deferred 👀: a follow-up task is
// filed, the reply names it and the thread is resolved (WP-440).
func (s *Service) AckReviewComments(ctx context.Context, actor *Actor, in AckInput) ([]*PRReviewComment, error) {
	in.Verdict = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(in.Verdict)), "-", "_")
	switch in.Verdict {
	case "accept":
		in.Verdict = VerdictAccepted
	case "reject":
		in.Verdict = VerdictRejected
	}
	if !slices.Contains(ackVerdicts, in.Verdict) {
		return nil, invalid("verdict must be one of %s", strings.Join(ackVerdicts, ", "))
	}
	in.Note = strings.TrimSpace(in.Note)
	if in.Note == "" && slices.Contains(verdictsWithWhy, in.Verdict) {
		return nil, invalid("say why (-m): a %s comment gets a reply", strings.ReplaceAll(in.Verdict, "_", " "))
	}
	list, prs, err := s.commentsFor(ctx, actor, in.IDs)
	if err != nil {
		return nil, err
	}
	if in.Verdict == VerdictDeferred {
		if err := s.deferComments(ctx, actor, list, prs, in.Note); err != nil {
			return nil, err
		}
		s.nudgeBabysits(prs)
		return list, nil
	}
	by := rcByActor(actor)
	for _, rc := range list {
		pr := prs[rc.prID]
		if err := s.markComment(ctx, pr, rc, in.Verdict, in.Note, "", by, s.roundOf(ctx, pr.ID)); err != nil {
			return nil, err
		}
	}
	s.nudgeBabysits(prs)
	return list, nil
}

// FixedReviewComments says review comments are fixed (`wp pr fixed`): 🎉,
// the thread resolved, a reply only with a note (always on a finding with no
// emoji of its own or a comment copied from GitHub, WP-442). A fix not pushed
// yet is marked being fixed (🚀) and becomes fixed, with its reply, when the
// next push arrives.
func (s *Service) FixedReviewComments(ctx context.Context, actor *Actor, in FixedInput) ([]*PRReviewComment, error) {
	list, prs, err := s.commentsFor(ctx, actor, in.IDs)
	if err != nil {
		return nil, err
	}
	in.Note, in.SHA = strings.TrimSpace(in.Note), strings.TrimSpace(in.SHA)
	if in.SHA != "" && !shaRE.MatchString(in.SHA) {
		return nil, invalid("sha %q isn't a commit id", in.SHA)
	}
	by := rcByActor(actor)
	for _, rc := range list {
		pr := prs[rc.prID]
		to := VerdictFixed
		if in.SHA != "" && !sameSHA(in.SHA, pr.HeadSHA) {
			to = VerdictFixing
		}
		if err := s.markComment(ctx, pr, rc, to, in.Note, in.SHA, by, s.roundOf(ctx, pr.ID)); err != nil {
			return nil, err
		}
	}
	s.nudgeBabysits(prs)
	return list, nil
}

var shaRE = lazyre.New(`^[0-9a-fA-F]{7,64}$`)

// DismissReviewComment says a comment needs nothing done (a person, or an
// agent saying why; only a person changes a person's own verdict).
func (s *Service) DismissReviewComment(ctx context.Context, actor *Actor, ref, note string) (*PRReviewComment, error) {
	note = strings.TrimSpace(note)
	if actor.IsAgent() && note == "" {
		return nil, invalid("say why (note): agents dismiss with a reason")
	}
	list, prs, err := s.commentsFor(ctx, actor, []string{ref})
	if err != nil {
		return nil, err
	}
	rc := list[0]
	if err := s.markComment(ctx, prs[rc.prID], rc, VerdictDismissed, note, "", rcByActor(actor), s.roundOf(ctx, rc.prID)); err != nil {
		return nil, err
	}
	s.nudgeBabysits(prs)
	return rc, nil
}

// promoteFixing marks comments being fixed as fixed once a push arrives
// after the fix: the runner pushes a round's commits when its agent ends.
func (s *Service) promoteFixing(ctx context.Context, pr *PullRequest) {
	list, err := s.reviewComments(ctx, s.db.R, pr.ID)
	if err != nil {
		return
	}
	for _, rc := range list {
		if rc.Verdict != VerdictFixing || rc.Gone || rc.FixSHA == "" {
			continue
		}
		var note string
		_ = s.db.R.QueryRowContext(ctx, `SELECT note FROM review_comment_events WHERE comment_id = ? AND verdict = ? ORDER BY id DESC LIMIT 1`,
			rc.id, VerdictFixing).Scan(&note)
		if err := s.markComment(ctx, pr, rc, VerdictFixed, note, rc.FixSHA, rcBy{kind: "babysitter", name: "waypoint"}, s.roundOf(ctx, pr.ID)); err != nil {
			s.log.WarnContext(ctx, "mark a review comment fixed", "pr", pr.ID, "comment", rc.Ref, "err", err)
		}
	}
}

// PRTodo is what is left to do on a pull request: failed checks, conflicts
// and the review comments not dealt with.
type PRTodo struct {
	PRID     int64      `json:"pr_id"`
	Repo     string     `json:"repo"`
	Number   int        `json:"number"`
	URL      string     `json:"url"`
	HeadRef  string     `json:"head_ref"`
	BaseRef  string     `json:"base_ref"`
	HeadSHA  string     `json:"head_sha"`
	Checks   []CheckLog `json:"checks"`
	Conflict bool       `json:"conflict"`
	Rebase   string     `json:"rebase,omitempty"` // the commands that bring it up to its base
	// Ejected: the failed checks were seen on the merge line's candidate,
	// with the head merged onto the base's tip (WP-1659).
	Ejected  string        `json:"ejected,omitempty"`
	Comments []TodoComment `json:"comments"`
	// Findings are the audit findings still to do (WP-355).
	Findings []TodoFinding `json:"findings"`
	Babysit  *Babysit      `json:"babysit,omitempty"`
	// Refused is the latest round on the head whose fix was refused for
	// protected files (refusedRound), for the session its handoff asks to
	// push one (WP-1266); `wp pr todo` only, never a round's packet.
	Refused *BabysitRound `json:"refused,omitempty"`
	// Intent is what the pull request is for, which every verdict is
	// weighed against (WP-440).
	Intent       *PRIntent `json:"intent,omitempty"`
	Text         string    `json:"text"` // all of it, as `wp pr todo` prints it
	intent, work string
}

// TodoComment is a review comment left to deal with, with the code it is on.
type TodoComment struct {
	*PRReviewComment
	Code string `json:"code,omitempty"` // ±4 lines at the head, the line marked
	// HeavyLift: a bot marked it a big change, a deferral unless the task
	// needs it (WP-440).
	HeavyLift bool `json:"heavy_lift,omitempty"`
}

// Empty says there is nothing to do.
func (t *PRTodo) Empty() bool {
	return len(t.Checks) == 0 && !t.Conflict && len(t.Comments) == 0 && len(t.Findings) == 0 && t.Refused == nil
}

// PRWorkPacket is the work packet for a pull request (`wp pr todo`). Its
// review comments are read from the forge first.
func (s *Service) PRWorkPacket(ctx context.Context, actor *Actor, prID int64) (*PRTodo, error) {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return nil, err
	}
	if _, err := s.prAuthority(ctx, actor, pr, RoleViewer); err != nil {
		return nil, err
	}
	if _, err := s.syncReviewComments(ctx, pr); err != nil {
		s.log.WarnContext(ctx, "read review comments", "pr", pr.ID, "err", err)
	}
	return s.workPacket(ctx, actor, pr, true)
}

// workPacket builds the packet; all includes the comments that wait for a
// person (a round's agent isn't given those).
func (s *Service) workPacket(ctx context.Context, actor *Actor, pr *PullRequest, all bool) (*PRTodo, error) {
	return s.packetOf(ctx, actor, pr, all, func(rc *PRReviewComment) bool { return inPacket(rc, all) })
}

// assessorPacket is a round's packet plus the comments an earlier agent
// left for a person, which are the assessor's to settle (WP-759). A person's
// own verdict stays out: no agent can change it.
func (s *Service) assessorPacket(ctx context.Context, pr *PullRequest) (*PRTodo, error) {
	return s.packetOf(ctx, SystemActor(), pr, false, func(rc *PRReviewComment) bool {
		if rc.Dealt {
			return false
		}
		if rc.Verdict != VerdictNeedsPerson {
			return true
		}
		byPerson, err := s.verdictByPerson(ctx, s.db.R, rc.id)
		if err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "leave a comment out of the assessor's packet: who decided its verdict is unread", "comment", rc.id, "pr", pr.ID, "err", err)
			}
			return false
		}
		return !byPerson
	})
}

// packetOf builds the packet from the comments listed picks.
func (s *Service) packetOf(ctx context.Context, actor *Actor, pr *PullRequest, all bool, listed func(*PRReviewComment) bool) (*PRTodo, error) {
	t := &PRTodo{PRID: pr.ID, Repo: pr.Repo, Number: pr.Number, URL: pr.URL, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, HeadSHA: pr.HeadSHA,
		Checks: []CheckLog{}, Comments: []TodoComment{}, Findings: []TodoFinding{}}
	if pr.ciSHA == pr.HeadSHA && pr.CIState == CIFailed {
		logs, err := s.PRCheckLogs(ctx, actor, pr.ID, "", false)
		if err != nil {
			s.log.WarnContext(ctx, "read failed checks' logs", "pr", pr.ID, "err", err)
		} else {
			t.Checks = logs.Checks
		}
		t.Ejected = s.candEjectText(ctx, pr)
	}
	if s.packetConflict(pr) {
		t.Conflict = true
		base := cmp(pr.BaseRef, "main")
		t.Rebase = fmt.Sprintf("git fetch origin %s && git merge FETCH_HEAD, resolve, commit; don't rebase, the push isn't forced", base)
	}
	list, err := s.reviewComments(ctx, s.db.R, pr.ID)
	if err != nil {
		return nil, err
	}
	f, rerr := s.forgeByID(ctx, pr.ForgeID)
	if rerr != nil {
		if readFault(rerr) && !errors.Is(rerr, ErrNotFound) {
			s.log.WarnContext(ctx, "forge by id failed", "forgeid", pr.ForgeID, "err", rerr)
		}
	}
	files := map[string][]string{}
	elsewhere := s.outsideDiff(ctx, pr, list)
	for _, rc := range list {
		if !listed(rc) || elsewhere[rc.Ref] {
			continue
		}
		tc := TodoComment{PRReviewComment: rc, HeavyLift: rc.heavyLift()}
		if rc.Path != "" && rc.Line > 0 && f != nil && pr.HeadSHA != "" {
			lines, ok := files[rc.Path]
			if !ok {
				if prc, ok := s.Forges.(ForgePRs); ok {
					if body, err := prc.FileAt(ctx, f, pr.Repo, rc.Path, pr.HeadSHA); err == nil {
						lines = strings.Split(body, "\n")
					}
				}
				files[rc.Path] = lines
			}
			tc.Code = codeAround(lines, rc.Line, 4)
		}
		t.Comments = append(t.Comments, tc)
	}
	t.Findings = s.todoFindings(ctx, pr.ID)
	t.Intent = s.prIntent(ctx, actor, pr)
	if b, err := s.activeBabysit(ctx, s.db.R, pr.ID); err == nil && b != nil {
		t.Babysit = b
		if all {
			t.Refused = refusedRound(b, pr.HeadSHA)
		}
		if !all {
			t.Findings = slices.DeleteFunc(t.Findings, func(f TodoFinding) bool {
				return f.startsRound() && slices.ContainsFunc(b.Rounds, func(r BabysitRound) bool { return slices.Contains(r.Findings, f.ID) })
			})
		}
	}
	t.Text = t.render()
	return t, nil
}

// packetConflict: the work packet lists a conflict, the forge's repeated
// one (not while it is still checking, WP-138) or the one Waypoint's update
// hit (WP-430).
func (s *Service) packetConflict(pr *PullRequest) bool {
	conflict, _ := pr.mergeState(s.now())
	return conflict || s.updateConflicted(pr)
}

// inPacket: the work packet lists the comment (with all, also one that
// waits for a person).
func inPacket(rc *PRReviewComment, all bool) bool {
	return !rc.Dealt && (all || rc.Verdict != VerdictNeedsPerson)
}

// packetEmpty says, without reading any code or logs, that a round's work
// packet would hold no conflict and no comment. Failed checks and audit
// findings are the caller's to count. A read error counts as not empty: the
// round decides.
func (s *Service) packetEmpty(ctx context.Context, pr *PullRequest) bool {
	if s.packetConflict(pr) {
		return false
	}
	list, err := s.reviewComments(ctx, s.db.R, pr.ID)
	if err != nil {
		return false
	}
	elsewhere := s.outsideDiff(ctx, pr, list)
	return !slices.ContainsFunc(list, func(rc *PRReviewComment) bool { return inPacket(rc, false) && !elsewhere[rc.Ref] })
}

// codeAround is the lines around line n (1-based), numbered, n marked.
func codeAround(lines []string, n, ctxLines int) string {
	if n < 1 || n > len(lines) {
		return ""
	}
	var b strings.Builder
	for i := max(1, n-ctxLines); i <= min(len(lines), n+ctxLines); i++ {
		mark := "  "
		if i == n {
			mark = "> "
		}
		line := lines[i-1]
		if r := []rune(line); len(r) > 300 {
			line = string(r[:300]) + "…"
		}
		fmt.Fprintf(&b, "%s%5d  %s\n", mark, i, strings.TrimRight(line, "\r"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// render is the packet as text: a line naming the pull request, what it is
// for, and the work.
func (t *PRTodo) render() string {
	head := fmt.Sprintf("%s#%d (%s → %s) at %s\n", t.Repo, t.Number, t.HeadRef, t.BaseRef, short(t.HeadSHA))
	if t.Empty() {
		t.work = "\nNothing to do: no failed checks, conflicts, open review comments or audit findings.\n"
		return head + t.work
	}
	var b strings.Builder
	if t.Intent != nil {
		t.Intent.render(&b)
	}
	t.intent = b.String()
	b.Reset()
	if len(t.Checks) > 0 {
		fmt.Fprintf(&b, "\n## Failed checks (%d)\n", len(t.Checks))
		b.WriteString("Reproduce each here, fix it, and commit once it passes; if you can't run one, say the fix is unverified.\n")
		if t.Ejected != "" {
			b.WriteString(t.Ejected + "\n")
		}
		for _, c := range t.Checks {
			fmt.Fprintf(&b, "\n### %s\n", c.Name)
			if c.URL != "" {
				fmt.Fprintf(&b, "%s\n", c.URL)
			}
			if c.Fault != nil {
				fmt.Fprintf(&b, "Failed on its runner: %s. This is %s\n", c.Fault.Words(), c.Fault.Advice())
			}
			switch {
			case c.Text != "":
				b.WriteString("<untrusted>\n" + untrusted(c.Text) + "\n</untrusted>\n")
			case c.Error != "":
				fmt.Fprintf(&b, "(no log: %s)\n", c.Error)
			}
			if h := reproduceHint(c.Name); h != "" {
				b.WriteString(h + "\n")
			}
		}
	}
	if t.Conflict {
		fmt.Fprintf(&b, "\n## Conflicts with %s\n%s\n", t.BaseRef, t.Rebase)
	}
	if r := t.Refused; r != nil {
		files, _ := protectedRefusal(*r)
		fmt.Fprintf(&b, "\n## Babysitting round %d's refused fix\n", r.N)
		if plainProtected(strings.Split(files, ", ")) {
			fmt.Fprintf(&b, "It changes protected files (%s), which rounds may not push (WP-D14); a session may. It is another agent's work: check it before reusing any of it.\n", files)
		} else {
			b.WriteString("It changes protected files, which rounds may not push (WP-D14); a session may. It is another agent's work: check it before reusing any of it. The runner named them:\n<untrusted>\n" + untrusted(files) + "\n</untrusted>\n")
		}
		if r.Answer != "" {
			b.WriteString("\nHow it ended:\n<untrusted>\n" + untrusted(r.Answer) + "\n</untrusted>\n")
		}
		if r.Diff != "" {
			b.WriteString("\nIts diff, not pushed:\n<untrusted>\n" + untrusted(strings.TrimRight(r.Diff, "\n")) + "\n</untrusted>\n")
		}
	}
	if len(t.Comments) > 0 {
		fmt.Fprintf(&b, "\n## Open review comments (%d)\n", len(t.Comments))
		b.WriteString(commentsHowTo)
		for _, c := range t.Comments {
			where := ""
			if c.Path != "" {
				where = ", " + c.Path
				if c.Line > 0 {
					where += ":" + strconv.Itoa(c.Line)
				}
			}
			state := ""
			if c.Verdict != VerdictNew && c.Verdict != VerdictSeen {
				state = " [" + strings.ReplaceAll(c.Verdict, "_", " ") + "]"
			}
			if c.Outdated {
				state += " [outdated]"
			}
			if c.HeavyLift {
				state += " [heavy lift: defer unless the PR needs it]"
			}
			fmt.Fprintf(&b, "\n### %s %s by %s%s%s\n", c.Ref, kindName(c.Kind), c.Author, where, state)
			text := packetBody(c.Body)
			b.WriteString("<untrusted>\n" + untrusted(excerpt(text, packetCommentChars)) + "\n</untrusted>\n")
			if n := utf8.RuneCountInString(strings.Join(strings.Fields(text), " ")); n > packetCommentChars {
				fmt.Fprintf(&b, "(Cut at %d of %d characters: a verdict covers only this part.)\n", packetCommentChars, n)
			}
			if c.Verdict == VerdictNeedsPerson && c.Note != "" {
				b.WriteString("Left for a person because:\n<untrusted>\n" + untrusted(c.Note) + "\n</untrusted>\n")
			}
			if c.Code != "" {
				b.WriteString("```\n" + c.Code + "\n```\n")
			}
		}
	}
	renderFindings(&b, t.Findings)
	t.work = b.String()
	return head + t.intent + t.work
}

// commentsHowTo is how to answer review comments, said once above them.
const commentsHowTo = `Weigh what the PR is for. Give each a verdict: wp pr ack ID --accept | --reject -m WHY | --partial -m WHAT | --defer -m WHY | --needs-person -m WHAT.
--defer: real, but the PR doesn't need it; files a follow-up task instead of widening the PR.
--reject: wrong, or its worst case is the safe side (a check that runs when it could skip).
--needs-person: only product calls, credentials, policy or scope; the assessor takes it next.
wp pr fixed ID… once their fixes are committed. wp pr reply ID -m TEXT answers without a verdict; wp pr resolve ID -m TEXT closes one that needs none.
`

// packetBody is a comment's text for the packet: no markers, and on
// Waypoint's own comments not the review note it signs them with (WP-D15,
// worded however an admin chose), which the heading's author already says.
func packetBody(s string) string {
	own := isWaypointText(s)
	s = stripMarkers(s)
	if own && strings.HasSuffix(s, "</sub>") {
		if i := strings.LastIndex(s, "<sub>"); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
	}
	return s
}

// untrustedTagRE matches an <untrusted> mark, open or closing, in any case,
// with spaces (any unicode.IsSpace one, not only ASCII: oneLine folds them
// all) or Unicode format characters (zero-width and the like) inside it: a
// reader that drops or folds those still sees the mark, so the match has to.
// The text around a mark keeps its format characters (an emoji's joiner, a
// right-to-left mark).
var untrustedTagRE = lazyre.New(`(?i)<` + markSpace + `*/?` + markSpace + `*` +
	strings.Join(strings.Split("untrusted", ""), `\p{Cf}*`) + markSpace + `*>`)

// markSpace is what may pad a mark: \s is ASCII only, so the vertical tab,
// NEL and the Unicode spaces and separators are named too.
const markSpace = `[\s\v\x{85}\p{Z}\p{Cf}]`

// Untrusted is untrusted for the runner's own prompts (cmd/wp).
func Untrusted(s string) string { return untrusted(s) }

// untrusted makes text safe to put between <untrusted> tags: it can't
// close them, however it nests the tag ("</untru</untrusted>sted>").
func untrusted(s string) string {
	for {
		t := untrustedTagRE.ReplaceAllString(s, "")
		if t == s {
			return t
		}
		s = t
	}
}

func kindName(kind string) string {
	switch kind {
	case "inline":
		return "comment"
	case "review":
		return "review"
	case "finding":
		return "Waypoint finding"
	case "relayed":
		return "comment (relayed)"
	}
	return "comment"
}

// reproduceHint says how to run a failed check here when its log doesn't:
// code scanning runs on the forge, not in the repo's CI (WP-251).
func reproduceHint(check string) string {
	if strings.Contains(strings.ToLower(check), "codeql") {
		return "Reproduce it with `wp codeql scan` (the same scan, on the languages that failed, a few minutes each; `wp upgrade` if wp lacks it; it needs the CodeQL image your check runs, named by `--image IMAGE` or `WP_CODEQL_IMAGE`). Exit 0 means nothing blocking; exit 2 means it couldn't run here (no image named, or no docker); exit 3 means the scan itself failed."
	}
	return ""
}
