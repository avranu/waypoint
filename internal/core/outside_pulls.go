package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Text from outside on pull requests (WP-281, WP-ADR14). What a forge user
// who isn't a Waypoint person writes on a pull request reaches no agent,
// automation or model until a person has read and approved it:
//
//   - a pull request from outside (its title, description and code) is
//     recorded with outside = 1: agents can't list or read it (prVisible,
//     prAuthority), automation steps can't take it (StepCtx.PullRequest),
//     bots don't review or answer on it, it sets off no event, and its
//     title is blank until a person approves it. A new head or title from
//     its author waits again;
//   - a review, review comment or conversation comment from outside is
//     dropped where Waypoint reads the forge (screenThreads, screenNotes),
//     before anything else sees it, and waits in outside_pr_text;
//   - their history keeps that they acted, not what they wrote.
//
// Waypoint's own accounts (the token's login is a person's; the bot
// account's text is Waypoint's) aren't outside.

// Kinds of outside text on a pull request.
const (
	OutsidePull = "pull"
)

// Outside text on a pull request, as a person decides on it.
const (
	outsideHeld      = "held"
	outsideApproved  = "approved"
	outsideDiscarded = "discarded"
)

// OutsidePRText is text from outside on a pull request, waiting for a
// person (WP-281).
type OutsidePRText struct {
	ID     int64  `json:"id"`
	PRID   int64  `json:"pr_id"`
	Kind   string `json:"kind"`   // pull, inline, review, comment
	Author string `json:"author"` // the forge login ("" when the forge didn't say)
	URL    string `json:"url,omitempty"`
	Path   string `json:"path,omitempty"`
	Line   int    `json:"line,omitempty"`
	Title  string `json:"title,omitempty"` // pull
	Body   string `json:"body"`
	// HeadSHA is the code a pull request's approval covers.
	HeadSHA   string    `json:"head_sha,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func textHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:12])
}

// prVoice says whose words on a forge's pull requests reach agents.
type prVoice struct {
	s   *Service
	f   *Forge
	bot string
}

func (s *Service) prVoice(ctx context.Context, f *Forge) *prVoice {
	return &prVoice{s: s, f: f, bot: s.forgeBotLogin(ctx, f)}
}

// outside: a login that isn't a Waypoint person's, nor Waypoint's own.
// Doubt counts as outside for showing text; outsideErr is for recording.
func (v *prVoice) outside(ctx context.Context, q queryer, login string) bool {
	if v.bot != "" && strings.EqualFold(strings.TrimSpace(login), v.bot) {
		return false
	}
	return v.s.fromOutside(ctx, q, v.f, login)
}

func (v *prVoice) outsideErr(ctx context.Context, q queryer, login string) (bool, error) {
	if v.bot != "" && strings.EqualFold(strings.TrimSpace(login), v.bot) {
		return false, nil
	}
	outside, _, err := v.s.forgeWriterErr(ctx, q, v.f, login)
	return outside, err
}

// authorOutside: a pull request's author isn't a Waypoint person. For
// a mirror's copies of GitHub pull requests, the GitHub author named in
// the text must be a person on some forge too, as the copying account is.
func (v *prVoice) authorOutside(ctx context.Context, q queryer, forgeAuthor, author string) bool {
	outside, err := v.authorOutsideErr(ctx, q, forgeAuthor, author)
	if err != nil {
		if readFault(err) {
			v.s.log.WarnContext(ctx, "read whether a pull request's author is a person", "forge", forgeLogID(v.f), "err", err)
		}
		return true
	}
	return outside
}

// authorOutsideErr is authorOutside with a failed read as an error, for a
// caller that records the answer: a read that failed marks nothing.
func (v *prVoice) authorOutsideErr(ctx context.Context, q queryer, forgeAuthor, author string) (bool, error) {
	if out, err := v.outsideErr(ctx, q, forgeAuthor); err != nil || out {
		return out, err
	}
	if strings.EqualFold(author, forgeAuthor) {
		return false, nil
	}
	// The original is on GitHub: its author must be a person there.
	var ok bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM forge_people fp JOIN forges g ON g.id = fp.forge_id
		WHERE g.kind = 'github' AND fp.login = ?)`, strings.TrimSpace(author)).Scan(&ok); err != nil {
		return false, err
	}
	return !ok, nil
}

// heldPR is a comment from outside on a pull request to keep for a person.
type heldPR struct {
	externalID, kind  string
	author, url, path string
	line              int
	body              string
}

// passOrHold reports whether a person approved this very text; otherwise it
// is held for one (a text they discarded stays out).
func (s *Service) passOrHold(ctx context.Context, q queryer, prID int64, h heldPR) bool {
	hash := textHash(h.body)
	var state, was string
	err := q.QueryRowContext(ctx, `SELECT state, body_hash FROM outside_pr_text WHERE pr_id = ? AND external_id = ?`, prID, h.externalID).Scan(&state, &was)
	if err == nil && was == hash && state == outsideApproved {
		return true
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false
	}
	now := s.nowMS()
	if _, err := s.db.W.ExecContext(ctx, `INSERT INTO outside_pr_text(pr_id, external_id, kind, author, url, path, line, body, body_hash, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(pr_id, external_id) DO UPDATE SET author = excluded.author, url = excluded.url, path = excluded.path,
			line = excluded.line, body = excluded.body, body_hash = excluded.body_hash, state = 'held', decided_by = NULL, updated_at = excluded.updated_at
		WHERE outside_pr_text.body_hash != excluded.body_hash`,
		prID, h.externalID, h.kind, h.author, h.url, h.path, h.line, excerpt(h.body, 20000), hash, now, now); err != nil {
		s.log.WarnContext(ctx, "hold text from outside", "pr", prID, "err", err)
	}
	return false
}

// screenThreads leaves out of a pull request's review threads what someone
// from outside wrote and no person approved, holding it for a person. Every
// reader of the forge's threads goes through it.
func (s *Service) screenThreads(ctx context.Context, f *Forge, pr *PullRequest, threads []ReviewThread) []ReviewThread {
	v := s.prVoice(ctx, f)
	out := threads[:0]
	for _, t := range threads {
		var kept []ThreadComment
		for _, c := range t.Comments {
			if v.outside(ctx, s.db.R, c.Author) && !s.passOrHold(ctx, s.db.R, pr.ID, heldPR{externalID: c.ID, kind: "inline", author: c.Author,
				url: commentURL(f, pr, "inline", c.ID), path: t.Path, line: t.Line, body: c.Body}) {
				continue
			}
			kept = append(kept, c)
		}
		if len(kept) > 0 {
			t.Comments = kept
			out = append(out, t)
		}
	}
	return out
}

// screenNotes does the same for reviews and conversation comments.
func (s *Service) screenNotes(ctx context.Context, f *Forge, pr *PullRequest, notes []PRNote) []PRNote {
	v := s.prVoice(ctx, f)
	out := notes[:0]
	for _, n := range notes {
		if strings.TrimSpace(n.Body) != "" && v.outside(ctx, s.db.R, n.Author) && !s.passOrHold(ctx, s.db.R, pr.ID, heldPR{externalID: n.ID, kind: n.Kind,
			author: n.Author, url: cmp(n.URL, commentURL(f, pr, n.Kind, n.ID)), body: n.Body}) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// screenPull decides, as recordPR stores a pull request, whether it waits
// for a person: from outside and not approved at this title and head. A
// waiting one keeps an approved title, or none.
func (s *Service) screenPull(ctx context.Context, t *tx, v *prVoice, pr *PullRequest, fp *ForgePR) error {
	title := excerpt(stripWIP(fp.Title), 300)
	forgeAuthor := fp.Author
	if forgeAuthor == "" {
		// A GitLab hook, or a refresh, doesn't say: who it was recorded as.
		forgeAuthor = pr.forgeAuthor
	}
	outsider, err := v.authorOutsideErr(ctx, t, forgeAuthor, pr.Author)
	if err != nil {
		return err
	}
	if !outsider {
		if !pr.Outside {
			return nil
		}
		pr.Outside = false
		_, err := t.ExecContext(ctx, `UPDATE pull_requests SET outside = 0 WHERE id = ?`, pr.ID)
		return err
	}
	var state, okTitle, okHead, okHash string
	err = t.QueryRowContext(ctx, `SELECT state, title, head_sha, body_hash FROM outside_pr_text WHERE pr_id = ? AND external_id = ''`, pr.ID).
		Scan(&state, &okTitle, &okHead, &okHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	approved := state == outsideApproved
	if approved && okTitle == title && okHead == pr.HeadSHA {
		pr.Outside = false
		_, err := t.ExecContext(ctx, `UPDATE pull_requests SET outside = 0 WHERE id = ?`, pr.ID)
		return err
	}
	shown := ""
	if approved && okTitle == title {
		shown = title
	}
	body, hash := fp.Body, textHash(fp.Body)
	now := s.nowMS()
	if _, err := t.ExecContext(ctx, `INSERT INTO outside_pr_text(pr_id, external_id, kind, author, url, title, body, body_hash, head_sha, created_at, updated_at)
		VALUES (?, '', 'pull', ?,?,?,?,?,?,?,?) ON CONFLICT(pr_id, external_id) DO UPDATE SET author = excluded.author, url = excluded.url,
			title = excluded.title, head_sha = excluded.head_sha, updated_at = excluded.updated_at,
			body = CASE WHEN ? THEN excluded.body ELSE outside_pr_text.body END,
			body_hash = CASE WHEN ? THEN excluded.body_hash ELSE outside_pr_text.body_hash END,
			state = CASE WHEN outside_pr_text.state = 'approved' THEN 'held' ELSE outside_pr_text.state END, decided_by = NULL
		WHERE outside_pr_text.title != excluded.title OR outside_pr_text.head_sha != excluded.head_sha OR outside_pr_text.state = 'approved'
			OR (? AND outside_pr_text.body_hash != excluded.body_hash)`,
		pr.ID, pr.Author, pr.URL, title, excerpt(body, 20000), hash, pr.HeadSHA, now, now, body != "", body != "", body != ""); err != nil {
		return err
	}
	pr.Outside, pr.Title = true, shown
	_, err = t.ExecContext(ctx, `UPDATE pull_requests SET outside = 1, title = ? WHERE id = ?`, shown, pr.ID)
	return err
}

// approvedBody is a pull request's description as a person approved it
// ("" when they didn't): the review prompt reads no other.
func (s *Service) approvedBody(ctx context.Context, f *Forge, pr *PullRequest, fp *ForgePR) string {
	v := s.prVoice(ctx, f)
	if !v.authorOutside(ctx, s.db.R, fp.Author, pr.Author) {
		return fp.Body
	}
	var hash string
	_ = s.db.R.QueryRowContext(ctx, `SELECT body_hash FROM outside_pr_text WHERE pr_id = ? AND external_id = '' AND state = 'approved'`, pr.ID).Scan(&hash)
	if hash != "" && hash == textHash(fp.Body) {
		return fp.Body
	}
	return ""
}

// errPROutside refuses work on a pull request from outside no person
// approved.
func errPROutside(pr *PullRequest) error {
	return forbidden("%s#%d is from outside (its author isn't a Waypoint person) and waits for a person to approve it", pr.Repo, pr.Number)
}

// ListOutsidePR lists the text from outside waiting on a pull request,
// oldest first: people only.
func (s *Service) ListOutsidePR(ctx context.Context, actor *Actor, prID int64) ([]OutsidePRText, error) {
	if actor.HidesDrafts() {
		return nil, errOutsideByPerson
	}
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return nil, err
	}
	if _, err := s.prAuthority(ctx, actor, pr, RoleViewer); err != nil {
		return nil, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, kind, author, url, path, line, title, body, head_sha, created_at, updated_at
		FROM outside_pr_text WHERE pr_id = ? AND state = 'held' ORDER BY external_id != '', created_at, id`, pr.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutsidePRText{}
	for rows.Next() {
		x := OutsidePRText{PRID: pr.ID}
		var c, u int64
		if err := rows.Scan(&x.ID, &x.Kind, &x.Author, &x.URL, &x.Path, &x.Line, &x.Title, &x.Body, &x.HeadSHA, &c, &u); err != nil {
			return nil, err
		}
		x.CreatedAt, x.UpdatedAt = msTime(c), msTime(u)
		out = append(out, x)
	}
	return out, rows.Err()
}

type outsidePRRow struct {
	id, prID         int64
	externalID, kind string
	author, title    string
	headSHA, state   string
	updated          int64
}

// outsidePRFor checks that a person may decide on outside text on a pull
// request, and returns it with its pull request.
func (s *Service) outsidePRFor(ctx context.Context, actor *Actor, id int64) (*outsidePRRow, *PullRequest, int64, error) {
	if actor.HidesDrafts() {
		return nil, nil, 0, errOutsideByPerson
	}
	if err := actor.canWrite(); err != nil {
		return nil, nil, 0, err
	}
	var o outsidePRRow
	err := s.db.R.QueryRowContext(ctx, `SELECT id, pr_id, external_id, kind, author, title, head_sha, state, updated_at FROM outside_pr_text WHERE id = ?`, id).
		Scan(&o.id, &o.prID, &o.externalID, &o.kind, &o.author, &o.title, &o.headSHA, &o.state, &o.updated)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && o.state != outsideHeld) {
		return nil, nil, 0, notFound("text from outside")
	} else if err != nil {
		return nil, nil, 0, err
	}
	pr, err := s.prByID(ctx, s.db.R, o.prID)
	if err != nil {
		return nil, nil, 0, err
	}
	pid, err := s.prAuthority(ctx, actor, pr, RoleEditor)
	if errors.Is(err, ErrNotFound) {
		return nil, nil, 0, notFound("text from outside")
	}
	return &o, pr, pid, err
}

// ApproveOutsidePR lets outside text on a pull request in, where agents,
// automations and models read it: a person's decision, after reading it.
// Approving a pull request covers its title, description and code at the
// head the person saw; bots then act on it as on one just opened.
func (s *Service) ApproveOutsidePR(ctx context.Context, actor *Actor, id int64) error {
	o, pr, pid, err := s.outsidePRFor(ctx, actor, id)
	if err != nil {
		return err
	}
	opened := false
	err = s.write(ctx, actor, func(t *tx) error {
		res, err := t.ExecContext(ctx, `UPDATE outside_pr_text SET state = 'approved', decided_by = ? WHERE id = ? AND state = 'held' AND updated_at = ?`,
			nullUser(actor.id()), o.id, o.updated)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return invalid("that text changed on the forge meanwhile; read it again")
		}
		what := "a comment by " + cmp(o.author, "someone")
		if o.kind == OutsidePull {
			what = "the pull request by " + cmp(o.author, "someone") + " at " + short(o.headSHA)
			res, err := t.ExecContext(ctx, `UPDATE pull_requests SET outside = 0, title = ? WHERE id = ? AND head_sha = ?`, o.title, pr.ID, o.headSHA)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			opened = n > 0
		}
		t.audit(AuditEntry{Action: "outside.approve", Category: AuditApproval, ProjectID: pid, TargetType: "pr", TargetID: pr.ID,
			Target: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), URL: pr.URL, Detail: "approved " + what + " from outside"})
		return nil
	})
	if err != nil {
		return err
	}
	bg := context.WithoutCancel(ctx)
	if o.kind != OutsidePull {
		s.goBG(func() {
			if _, err := s.syncReviewComments(bg, pr); err != nil {
				if readFault(err) && !errors.Is(err, ErrNotFound) && !isStatus(err, http.StatusNotFound) {
					s.log.WarnContext(bg, "sync an outside pull request's review comments", "pr", pr.ID, "err", err)
				}
			}
		})
		return nil
	}
	if !opened {
		return nil
	}
	cur, err := s.prByID(ctx, s.db.R, pr.ID)
	if err != nil {
		return err
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return err
	}
	if projects, err := s.botProjects(ctx, pr.ForgeID, pr.Repo); err == nil && (cur.State == "open" || cur.State == "draft") {
		if err := s.write(ctx, SystemActor(), func(t *tx) error {
			for _, p := range projects {
				t.emit(&Event{Kind: EvPRUpdated, ProjectID: p, New: "opened", Data: prEventData(cur, map[string]any{"reason": "opened"})})
			}
			return nil
		}); err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "approve an outside pull request", "pr", id, "err", err)
			}
		}
	}
	s.afterPRRecorded(bg, f, cur)
	return nil
}

// DiscardOutsidePR keeps a comment from outside out for good, as a person
// decided: only a new edit of it waits again. A pull request isn't
// discarded: it waits until approved, or closed on the forge.
func (s *Service) DiscardOutsidePR(ctx context.Context, actor *Actor, id int64) error {
	o, pr, pid, err := s.outsidePRFor(ctx, actor, id)
	if err != nil {
		return err
	}
	if o.kind == OutsidePull {
		return invalid("a pull request from outside waits until a person approves it; close it on the forge to turn it down")
	}
	return s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `UPDATE outside_pr_text SET state = 'discarded', decided_by = ? WHERE id = ? AND state = 'held'`, nullUser(actor.id()), o.id); err != nil {
			return err
		}
		t.audit(AuditEntry{Action: "outside.discard", Category: AuditApproval, ProjectID: pid, TargetType: "pr", TargetID: pr.ID,
			Target: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), URL: pr.URL, Detail: "discarded a comment by " + cmp(o.author, "someone") + " from outside"})
		return nil
	})
}

// releaseOutsidePRs lets in what waited on a forge's pull requests from a
// login now mapped to a person: their pull requests, with their titles, and
// their comments (read again from the forge next time).
func (s *Service) releaseOutsidePRs(ctx context.Context, forgeID int64, login string) error {
	if _, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET outside = 0, title = coalesce((SELECT o.title FROM outside_pr_text o
			WHERE o.pr_id = pull_requests.id AND o.external_id = ''), title)
		WHERE forge_id = ? AND outside = 1 AND author = ? COLLATE NOCASE`, forgeID, login); err != nil {
		return err
	}
	_, err := s.db.W.ExecContext(ctx, `DELETE FROM outside_pr_text WHERE author = ? COLLATE NOCASE AND state = 'held'
		AND pr_id IN (SELECT id FROM pull_requests WHERE forge_id = ?)`, login, forgeID)
	return err
}

// outsidePullsWaiting counts the pull requests with text from outside
// waiting for the actor: in projects they can edit, and only for people.
func (s *Service) outsidePullsWaiting(ctx context.Context, actor *Actor, project string) int {
	if actor.HidesDrafts() {
		return 0
	}
	ids, err := queryIDs(ctx, s.db.R, `SELECT DISTINCT o.pr_id FROM outside_pr_text o JOIN pull_requests p ON p.id = o.pr_id
		WHERE o.state = 'held' AND p.state IN ('open', 'draft') LIMIT 1000`)
	if err != nil {
		return 0
	}
	var pid int64
	if project != "" {
		if p, err := s.projectByRef(ctx, s.db.R, project); err == nil {
			pid = p.ID
		}
	}
	n := 0
	for _, id := range ids {
		pr, err := s.prByID(ctx, s.db.R, id)
		if err != nil {
			continue
		}
		if got, err := s.prAuthority(ctx, actor, pr, RoleEditor); err == nil && (pid == 0 || got == pid) {
			n++
		}
	}
	return n
}

// prTitleShown: a pull request's title may show on links. As Waypoint
// recorded it: not from outside, or this very title approved. One it hasn't
// recorded: by a Waypoint person, the original author of a mirror's copy
// included.
func (s *Service) prTitleShown(ctx context.Context, f *Forge, item *ForgeItem) bool {
	title := excerpt(stripWIP(item.Title), 300)
	var outside bool
	var stored string
	err := s.db.R.QueryRowContext(ctx, `SELECT outside, title FROM pull_requests WHERE forge_id = ? AND url = ?`, f.ID, item.URL).Scan(&outside, &stored)
	switch {
	case err == nil:
		return stored == title && (!outside || stored != "")
	case !errors.Is(err, sql.ErrNoRows):
		return false
	}
	v := s.prVoice(ctx, f)
	return !v.authorOutside(ctx, s.db.R, item.Author, effectiveAuthor(item.Author, item.Body))
}
