package core

// What happens to a bot's findings after they are posted (WP-D21): each
// keeps its forge comment and thread, its state (open, fixed, withdrawn,
// dismissed, or resolved by a person on the forge) and a history of who or
// what changed it. A review reads the forge's threads first, so Waypoint
// knows what was resolved or answered; a later push has the open findings
// in changed files checked against the new head.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Finding states.
const (
	FindingOpen      = "open"
	FindingFixed     = "fixed"     // checked against a later head: gone
	FindingWithdrawn = "withdrawn" // the bot agreed it was wrong
	FindingDismissed = "dismissed" // a person or an agent said so in Waypoint, or a person with 👎 on the forge
	FindingResolved  = "resolved"  // the thread was resolved: by a person on the forge, or in Waypoint (wp pr resolve)
	// FindingDropped: the verifier (or the evidence check) dropped it before
	// posting. Kept as a row to judge the verifier; never open, never posted.
	FindingDropped = "dropped"
)

// EvPRComment: a person replied in a bot's thread on a pull request.
const EvPRComment = "pr.comment"

// FindingEvent is one change of a finding's state.
type FindingEvent struct {
	Old   string    `json:"old"`
	State string    `json:"state"`
	Kind  string    `json:"kind"` // person, forge, verify, reply, agent, system
	By    string    `json:"by"`
	SHA   string    `json:"sha,omitempty"`
	Note  string    `json:"note,omitempty"`
	At    time.Time `json:"at"`
}

// findingBy says who changed a finding.
type findingBy struct {
	kind, name string
	userID     int64
}

var (
	byForge  = func(login string) findingBy { return findingBy{kind: "forge", name: login} }
	byVerify = func(model string) findingBy { return findingBy{kind: "verify", name: model} }
	byReply  = func(model string) findingBy { return findingBy{kind: "reply", name: model} }
)

// findingMarkerRE finds the run and place a finding's comment names.
var findingMarkerRE = lazyre.New(`<!-- waypoint-finding run=(\d+) n=(\d+) -->`)

// isWaypointText: a comment Waypoint wrote (a finding, a reply, a review).
func isWaypointText(body string) bool { return strings.Contains(body, "<!-- waypoint") }

// isPersonComment: a person wrote it, not a bot or Waypoint.
func isPersonComment(c ThreadComment) bool {
	return !isWaypointText(c.Body) && !isBotLogin(c.Author) && !isBotText(c.Body)
}

// blocksGate says whether a finding of this severity counts at a gate set
// to sev: critical is P0 ... low is P3.
func blocksGate(severity, gate string) bool { return severityRank(severity) <= severityRank(gate) }

// priority is "P1" for a severity.
func priority(sev string) string { return fmt.Sprintf("P%d", severityRank(sev)) }

// parseSeverity reads a severity as people write it: P2, medium, Medium.
func parseSeverity(v string) (string, bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	if len(v) == 2 && v[0] == 'p' && v[1] >= '0' && v[1] <= '3' {
		return severities[v[1]-'0'], true
	}
	if slices.Contains(severities, v) {
		return v, true
	}
	return "", false
}

// findingRow is a finding with what syncing needs.
type findingRow struct {
	BotFinding
	prID        int64
	runID       int64
	ordinal     int
	threadID    string
	commentID   string
	headSHA     string
	checkedSHA  string
	repliesSeen int
	private     bool
}

const findingCols = `f.id, f.run_id, f.pr_id, f.path, f.line, f.severity, f.claim, f.model, f.placement, f.state, f.ordinal, f.thread_id, f.comment_id,
	f.head_sha, f.checked_sha, f.replies_seen, f.private, f.created_at, f.updated_at, f.agreed, f.verified_by,
	f.prompt_version, f.strategy, f.verify, f.verify_reason`

func scanFindingRow(sc interface{ Scan(...any) error }) (*findingRow, error) {
	var f findingRow
	var line sql.NullInt64
	var created, updated int64
	if err := sc.Scan(&f.ID, &f.runID, &f.prID, &f.Path, &line, &f.Severity, &f.Claim, &f.Model, &f.Placement, &f.State, &f.ordinal, &f.threadID, &f.commentID,
		&f.headSHA, &f.checkedSHA, &f.repliesSeen, &f.private, &created, &updated, &f.Agreed, &f.VerifiedBy,
		&f.PromptVersion, &f.Strategy, &f.Verify, &f.VerifyReason); err != nil {
		return nil, err
	}
	if line.Valid {
		n := int(line.Int64)
		f.Line = &n
	}
	f.HeadSHA, f.Private, f.CreatedAt, f.UpdatedAt = f.headSHA, f.private, msTime(created), msTime(updated)
	return &f, nil
}

// prFindings are a pull request's findings, oldest first, without the ones
// dropped before posting (FindingDropped): everything that gates, syncs or
// shows a pull request's findings reads these.
func (s *Service) prFindings(ctx context.Context, q queryer, prID int64) ([]*findingRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+findingCols+` FROM bot_findings f WHERE f.pr_id = ? AND f.state != 'dropped' ORDER BY f.id`, prID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*findingRow
	for rows.Next() {
		f, err := scanFindingRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Service) findingByID(ctx context.Context, q queryer, id int64) (*findingRow, error) {
	f, err := scanFindingRow(q.QueryRowContext(ctx, `SELECT `+findingCols+` FROM bot_findings f WHERE f.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("finding")
	}
	return f, err
}

// findingHistory loads the state changes of a pull request's findings,
// oldest first, by finding.
func (s *Service) findingHistory(ctx context.Context, prID int64) (map[int64][]FindingEvent, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT finding_id, old_state, state, by_kind, by_name, sha, note, created_at FROM finding_events WHERE pr_id = ? ORDER BY id`, prID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]FindingEvent{}
	for rows.Next() {
		var id, at int64
		var e FindingEvent
		if err := rows.Scan(&id, &e.Old, &e.State, &e.Kind, &e.By, &e.SHA, &e.Note, &at); err != nil {
			return nil, err
		}
		e.At = msTime(at)
		out[id] = append(out[id], e)
	}
	return out, rows.Err()
}

// setFindingState changes a finding's state inside t and records it.
// Changing to the state it has is a no-op; it reports whether it changed.
func (s *Service) setFindingState(ctx context.Context, t *tx, id int64, to string, by findingBy, sha, note string) (bool, error) {
	var old string
	var prID int64
	if err := t.QueryRowContext(ctx, `SELECT state, pr_id FROM bot_findings WHERE id = ?`, id).Scan(&old, &prID); err != nil {
		return false, err
	}
	if old == to {
		return false, nil
	}
	now := s.nowMS()
	if _, err := t.ExecContext(ctx, `UPDATE bot_findings SET state = ?, updated_at = ? WHERE id = ?`, to, now, id); err != nil {
		return false, err
	}
	var user any
	if by.userID != 0 {
		user = by.userID
	}
	_, err := t.ExecContext(ctx, `INSERT INTO finding_events(finding_id, pr_id, old_state, state, by_kind, by_name, user_id, sha, note, created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		id, prID, old, to, by.kind, by.name, user, sha, excerpt(note, 300), now)
	return true, err
}

// ---- syncing with the forge's threads ----

// threadReply is a person's new comment in a bot's thread.
type threadReply struct {
	finding *findingRow
	comment ThreadComment
	thread  ReviewThread
}

// Reaction is a person's emoji on a comment.
type Reaction struct {
	Emoji string
	User  string
}

// syncResult is what reading the forge's threads taught.
type syncResult struct {
	threads  []ReviewThread
	findings []*findingRow
	replies  []threadReply
	changed  bool
}

// byThread finds the thread a finding sits in.
func (r *syncResult) byThread(f *findingRow) *ReviewThread {
	if f.threadID == "" {
		return nil
	}
	for n := range r.threads {
		if r.threads[n].ID == f.threadID {
			return &r.threads[n]
		}
	}
	return nil
}

// matchThread finds the finding a thread is about: by the marker in the
// comment Waypoint wrote, by the comment id it already knows, or (comments
// from before markers named their place) by file and wording. Markers count
// only in comments Waypoint's accounts posted: a person who typed one would
// take the finding over (WP-934).
func matchThread(th ReviewThread, findings []*findingRow, ours posters) *findingRow {
	root := th.root()
	marked := ours.ours(root.Author)
	if m := findingMarkerRE.FindStringSubmatch(root.Body); m != nil && marked {
		run, _ := strconv.ParseInt(m[1], 10, 64)
		n, _ := strconv.Atoi(m[2])
		for _, f := range findings {
			if f.runID == run && f.ordinal == n && f.Placement == "inline" {
				return f
			}
		}
	}
	for _, f := range findings {
		if f.commentID != "" && f.commentID == root.ID {
			return f
		}
	}
	if strings.Contains(root.Body, "<!-- waypoint-finding -->") && marked {
		for _, f := range findings {
			if f.commentID == "" && f.Path == th.Path && f.Placement == "inline" && similarClaim(f.Claim, root.Body) {
				return f
			}
		}
	}
	return nil
}

// syncFindings reads the pull request's threads and brings its findings in
// line: links them to their comments, notes threads people resolved (or
// reopened), 👎 from people, and replies waiting for an answer. Replies are
// announced as pr.comment events, once each.
func (s *Service) syncFindings(ctx context.Context, f *Forge, pr *PullRequest, withReactions bool) (*syncResult, error) {
	res := &syncResult{}
	cl, ok := s.Forges.(ForgeThreads)
	if !ok {
		var rerr error
		res.findings, rerr = s.prFindings(ctx, s.db.R, pr.ID)
		if rerr != nil {
			if readFault(rerr) {
				s.log.WarnContext(ctx, "pr findings failed", "pr", pr.ID, "err", rerr)
			}
		}
		return res, nil
	}
	threads, err := cl.ReviewThreads(ctx, f, pr.Repo, pr.Number)
	if err != nil {
		return nil, err
	}
	threads = s.screenThreads(ctx, f, pr, threads) // what strangers wrote waits (WP-281)
	res.threads = threads
	findings, err := s.prFindings(ctx, s.db.R, pr.ID)
	if err != nil {
		return nil, err
	}
	res.findings = findings
	type link struct {
		f  *findingRow
		th ReviewThread
	}
	var links []link
	ours := s.waypointPosters(ctx, f)
	for _, th := range threads {
		if len(th.Comments) == 0 {
			continue
		}
		if fnd := matchThread(th, findings, ours); fnd != nil {
			links = append(links, link{f: fnd, th: th})
		}
	}
	// 👎 from people, on findings still open.
	downvoted := map[int64]string{}
	if withReactions {
		for _, l := range links {
			if l.f.State != FindingOpen || l.th.Resolved {
				continue
			}
			rs, err := cl.CommentReactions(ctx, f, pr.Repo, pr.Number, l.th.root().ID)
			if err != nil {
				s.log.Debug("read reactions", "repo", pr.Repo, "number", pr.Number, "err", err)
				continue // one comment's failure (a deleted one) mustn't skip the rest
			}
			for _, r := range rs {
				if (r.Emoji == "-1" || r.Emoji == "thumbsdown" || r.Emoji == "👎") && !isBotLogin(r.User) {
					downvoted[l.f.ID] = r.User
					break
				}
			}
		}
	}
	projects, err := s.projectsOfRepo(ctx, f.ID, pr.Repo)
	if err != nil {
		return nil, err
	}
	err = s.write(ctx, SystemActor(), func(t *tx) error {
		for _, l := range links {
			fnd, th := l.f, l.th
			root := th.root()
			if fnd.threadID != th.ID || fnd.commentID != root.ID {
				if _, err := t.ExecContext(ctx, `UPDATE bot_findings SET thread_id = ?, comment_id = ? WHERE id = ?`, th.ID, root.ID, fnd.ID); err != nil {
					return err
				}
				fnd.threadID, fnd.commentID = th.ID, root.ID
			}
			switch {
			case th.Resolved && fnd.State == FindingOpen:
				ch, err := s.setFindingState(ctx, t, fnd.ID, FindingResolved, byForge(th.ResolvedBy), "", "resolved the thread")
				if err != nil {
					return err
				}
				fnd.State, res.changed = FindingResolved, res.changed || ch
			case !th.Resolved && fnd.State == FindingResolved:
				// Not when Waypoint holds the resolution itself (wp pr resolve):
				// a forge that can't resolve threads never shows it.
				byForgeClick, err := s.resolvedOnForge(ctx, t, fnd.ID)
				if err != nil {
					return err // unread is not "a person's click": nothing is reopened
				}
				if !byForgeClick {
					break
				}
				ch, err := s.setFindingState(ctx, t, fnd.ID, FindingOpen, byForge(""), "", "the thread was reopened")
				if err != nil {
					return err
				}
				fnd.State, res.changed = FindingOpen, res.changed || ch
			}
			if who, ok := downvoted[fnd.ID]; ok && fnd.State == FindingOpen {
				ch, err := s.setFindingState(ctx, t, fnd.ID, FindingDismissed, byForge(who), "", "👎 on the comment")
				if err != nil {
					return err
				}
				fnd.State, res.changed = FindingDismissed, res.changed || ch
			}
			// A person's comment after the last one seen waits for an answer.
			last := th.Comments[len(th.Comments)-1]
			if len(th.Comments) > fnd.repliesSeen {
				if fnd.State == FindingOpen && !th.Resolved && len(th.Comments) > 1 && isPersonComment(last) {
					res.replies = append(res.replies, threadReply{finding: fnd, comment: last, thread: th})
					for _, pid := range projects {
						t.emit(&Event{Kind: EvPRComment, ProjectID: pid, New: "reply", Data: prEventData(pr, map[string]any{
							"finding_id": fnd.ID, "comment_id": last.ID, "thread_id": th.ID, "by": last.Author})})
					}
				}
				if _, err := t.ExecContext(ctx, `UPDATE bot_findings SET replies_seen = ? WHERE id = ?`, len(th.Comments), fnd.ID); err != nil {
					return err
				}
				fnd.repliesSeen = len(th.Comments)
			}
		}
		return nil
	})
	return res, err
}

// resolvedOnForge says whether a finding's resolution came from the forge
// (a person's click) rather than from Waypoint (wp pr resolve). No event is
// an answer (the forge's); a failed read is an error.
func (s *Service) resolvedOnForge(ctx context.Context, t *tx, id int64) (bool, error) {
	var kind string
	err := t.QueryRowContext(ctx, `SELECT by_kind FROM finding_events WHERE finding_id = ? AND state = ? ORDER BY id DESC LIMIT 1`, id, FindingResolved).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return kind == "forge", nil
}

// projectsOfRepo are the projects a repo is attached to.
func (s *Service) projectsOfRepo(ctx context.Context, forgeID int64, repo string) ([]int64, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT project_id FROM project_repos WHERE forge_id = ? AND repo = ? COLLATE NOCASE ORDER BY project_id`, forgeID, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---- what people do to findings in Waypoint ----

// prAuthority finds a project the actor holds at least min in that has the
// pull request's repo attached; ErrNotFound when none shows it to them.
func (s *Service) prAuthority(ctx context.Context, actor *Actor, pr *PullRequest, min int) (int64, error) {
	if pr.Outside && actor.HidesDrafts() {
		return 0, notFound("pull request") // from outside, for people only (WP-281)
	}
	projects, err := s.projectsOfRepo(ctx, pr.ForgeID, pr.Repo)
	if err != nil {
		return 0, err
	}
	seen := false
	for _, pid := range projects {
		role, err := s.projectRole(ctx, s.db.R, actor, pid)
		if err != nil {
			return 0, err
		}
		if role == RoleNone {
			continue
		}
		seen = true
		if role >= min {
			if min > RoleViewer {
				if err := actor.canWrite(); err != nil {
					return 0, err
				}
			}
			return pid, nil
		}
	}
	if !seen {
		return 0, notFound("pull request")
	}
	return 0, ErrForbidden
}

// DismissFinding says a finding is wrong or not worth fixing: it stops
// blocking the review gate. Reopen undoes it. Agents may dismiss too, saying
// why: the review comes from free models that know the code less well than
// the agents working on it, and its false positives shouldn't wait on a
// person (the owner, WP-128).
func (s *Service) DismissFinding(ctx context.Context, actor *Actor, id int64, note string) (*BotFinding, error) {
	return s.decideFinding(ctx, actor, id, FindingDismissed, note)
}

// ReopenFinding takes back a dismissal (or a withdrawal).
func (s *Service) ReopenFinding(ctx context.Context, actor *Actor, id int64) (*BotFinding, error) {
	return s.decideFinding(ctx, actor, id, FindingOpen, "")
}

func (s *Service) decideFinding(ctx context.Context, actor *Actor, id int64, to, note string) (*BotFinding, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	note = strings.TrimSpace(note)
	by := findingBy{kind: "person", name: actor.User.DisplayName, userID: actor.User.ID}
	if agent := actor.agentName(); actor.IsAgent() {
		if to == FindingDismissed && note == "" {
			return nil, invalid("say why the finding is wrong or not worth fixing (a note)")
		}
		if agent == "" {
			agent = "agent"
		}
		by.kind, by.name = "agent", fmt.Sprintf("%s (%s)", actor.User.DisplayName, agent)
	}
	fnd, err := s.findingByID(ctx, s.db.R, id)
	if err != nil {
		return nil, err
	}
	pr, err := s.prByID(ctx, s.db.R, fnd.prID)
	if err != nil {
		return nil, err
	}
	projectID, err := s.prAuthority(ctx, actor, pr, RoleEditor)
	if err != nil {
		return nil, err
	}
	if to == FindingOpen && fnd.State != FindingDismissed && fnd.State != FindingWithdrawn && fnd.State != FindingResolved {
		return nil, invalid("only a dismissed, withdrawn or resolved finding can be reopened")
	}
	var changed bool
	err = s.write(ctx, actor, func(t *tx) error {
		changed, err = s.setFindingState(ctx, t, id, to, by, "", note)
		if err != nil || !changed {
			return err
		}
		verb := map[string]string{FindingDismissed: "dismissed", FindingOpen: "reopened"}[to]
		detail := fmt.Sprintf("%s a %s finding at %s", verb, fnd.Severity, findingPlace(fnd))
		if note != "" {
			detail += ": " + note
		}
		t.audit(AuditEntry{Action: "finding." + strings.TrimSuffix(verb, "ed"), Category: AuditControl, ProjectID: projectID, TargetType: "pr", TargetID: pr.ID,
			Target: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), URL: pr.URL, Detail: detail})
		t.emit(&Event{Kind: EvPRActivity, ProjectID: projectID, New: "finding", Data: map[string]any{"pr_id": pr.ID}})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if changed {
		msg := "Dismissed in Waypoint by " + by.name + "."
		if note != "" {
			msg = "Dismissed in Waypoint by " + by.name + ": " + note
		}
		s.goBG(func() {
			bg := context.WithoutCancel(ctx)
			if to == FindingDismissed {
				s.closeFindingThread(bg, pr, fnd, msg, false)
			}
			s.checkPR(bg, pr.ID, "dismiss")
		})
	}
	out, err := s.findingByID(ctx, s.db.R, id)
	if err != nil {
		return nil, err
	}
	return &out.BotFinding, nil
}

func findingPlace(f *findingRow) string {
	if f.Line != nil && *f.Line > 0 {
		return fmt.Sprintf("%s:%d", f.Path, *f.Line)
	}
	return f.Path
}

// closeFindingThread replies in a finding's thread and resolves it where
// the forge lets Waypoint. Best effort: the finding's state is what counts.
func (s *Service) closeFindingThread(ctx context.Context, pr *PullRequest, fnd *findingRow, msg string, resolve bool) {
	cl, ok := s.Forges.(ForgeThreads)
	if !ok || fnd.threadID == "" {
		return
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return
	}
	bf := botForge(f)
	th := ReviewThread{ID: fnd.threadID, Path: fnd.Path, Comments: []ThreadComment{{ID: fnd.commentID, SHA: fnd.headSHA}}}
	if fnd.Line != nil {
		th.Line = *fnd.Line
	}
	// The thread as the forge has it, for the line and commit to answer on.
	if ts, err := cl.ReviewThreads(ctx, f, pr.Repo, pr.Number); err == nil {
		for _, x := range ts {
			if x.ID == fnd.threadID {
				th = x
			}
		}
	}
	fp := &ForgePR{Number: pr.Number, HeadSHA: pr.HeadSHA, URL: pr.URL}
	posted := false
	if msg != "" {
		if _, err := cl.ReplyToThread(ctx, bf, pr.Repo, fp, th, msg+"\n<!-- waypoint-reply -->"); err != nil {
			s.log.Debug("reply in finding thread", "repo", pr.Repo, "number", pr.Number, "err", err)
		} else {
			posted = true
		}
	}
	if resolve {
		if err := cl.ResolveThread(ctx, bf, pr.Repo, fp, th); err != nil && !errors.Is(err, ErrUnsupported) {
			s.log.Debug("resolve finding thread", "repo", pr.Repo, "number", pr.Number, "err", err)
		}
	}
	// What was just said is read, not a new reply. A reply that wasn't
	// posted adds no comment, so it mustn't hide the next person's.
	if posted {
		_, _ = s.db.W.ExecContext(ctx, `UPDATE bot_findings SET replies_seen = replies_seen + 1 WHERE id = ?`, fnd.ID)
	}
}

// ---- the repeat filter ----

// dropRepeated leaves out findings that restate one already raised on the
// pull request, in the form the forge shows: open ones, ones a person
// resolved, dismissed or the bot withdrew (a fixed finding may come back:
// that is a regression), and other reviewers' open comments.
func (s *Service) dropRepeated(ctx context.Context, prID int64, found []Finding, threads []ReviewThread) ([]Finding, int, error) {
	findings, err := s.prFindings(ctx, s.db.R, prID)
	if err != nil {
		return nil, 0, err
	}
	type prior struct{ path, text string }
	var earlier []prior
	for _, f := range findings {
		if f.State != FindingFixed {
			earlier = append(earlier, prior{f.Path, f.Claim})
		}
	}
	for _, th := range threads {
		if th.Resolved || len(th.Comments) == 0 || isWaypointText(th.root().Body) {
			continue
		}
		earlier = append(earlier, prior{th.Path, th.root().Body})
	}
	var out []Finding
	repeated := 0
	for _, f := range found {
		if slices.ContainsFunc(earlier, func(p prior) bool {
			return (p.path == f.File || strings.HasSuffix(p.path, "/"+f.File)) && similarClaim(p.text, f.Claim)
		}) {
			repeated++
			continue
		}
		out = append(out, f)
	}
	return out, repeated, nil
}

// ---- the conversation, for the review prompt ----

const conversationMax = 6000

// reviewConversation describes the pull request's earlier findings and
// other reviewers' open comments, as data for the review prompt; "" when
// there is nothing to say.
func reviewConversation(findings []*findingRow, threads []ReviewThread) string {
	byID := map[string]ReviewThread{}
	for _, th := range threads {
		byID[th.ID] = th
	}
	var b strings.Builder
	line := func(format string, args ...any) {
		if b.Len() < conversationMax {
			fmt.Fprintf(&b, format+"\n", args...)
		}
	}
	say := func(c ThreadComment) string {
		who := c.Author
		if isWaypointText(c.Body) {
			who = "Waypoint"
		}
		body := strings.TrimSpace(strings.Split(c.Body, "<!--")[0])
		return fmt.Sprintf("    %s said: %s", who, excerpt(strings.Join(strings.Fields(body), " "), 240))
	}
	handled := map[string]bool{}
	first := true
	for _, f := range findings {
		if f.Placement == "withheld" && f.private {
			continue // never shown, even to a model: it may be copied
		}
		if first {
			line("Waypoint's earlier findings on this pull request:")
			first = false
		}
		state := f.State
		if f.State == FindingFixed && f.checkedSHA != "" {
			state = "fixed in " + short(f.checkedSHA)
		}
		line("- [%s] %s %s: %s", state, priority(f.Severity), findingPlace(f), excerpt(f.Claim, 300))
		if th, ok := byID[f.threadID]; ok {
			handled[th.ID] = true
			for n, c := range th.Comments {
				if n == 0 {
					continue
				}
				line("%s", say(c))
			}
			if th.Resolved {
				line("    (a person resolved this thread)")
			}
		}
	}
	firstOther := true
	for _, th := range threads {
		if handled[th.ID] || th.Resolved || len(th.Comments) == 0 || isWaypointText(th.root().Body) {
			continue
		}
		if firstOther {
			line("Other reviewers' open comments:")
			firstOther = false
		}
		root := th.root()
		line("- %s on %s:%d: %s", root.Author, th.Path, th.Line, excerpt(strings.Join(strings.Fields(root.Body), " "), 300))
		for _, c := range th.Comments[1:] {
			line("%s", say(c))
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String()
}

// conversationRules are what the review does with that conversation.
const conversationRules = `Report only problems not already raised above. Don't restate an open finding or an open comment of another reviewer.
Never raise again a finding a person resolved, or that was dismissed, withdrawn or answered, unless the code has changed for the worse.
A finding marked fixed may be raised again only if the problem is back.`
