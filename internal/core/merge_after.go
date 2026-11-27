package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// PRMergeWait is a pull request this one merges after.
type PRMergeWait struct {
	PRID   int64  `json:"pr_id"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	URL    string `json:"url"`
	State  string `json:"state"`
	// Why: the task link ("WP-282 blocks WP-312"), or who set it.
	Why string `json:"why"`
	// Set: someone set it (wp pr after); else it comes from task links.
	Set bool `json:"set,omitempty"`
	// Mutual: the other one waits for this one too, directly or round a
	// longer loop, so none of them holds.
	Mutual bool `json:"mutual,omitempty"`
	apart  bool
	loop   int
	tasks  [2]int64
	// Stack: the pull request waited for is the one this is stacked on, its
	// base being that one's head branch (WP-ADR153, WP-1670). It isn't set
	// or dropped: moving this one off that branch (wp pr edit --base) or
	// the move when the other merges ends it. State closed: the branch is
	// dead, and this one may not merge into it either.
	Stack bool `json:"stacked,omitempty"`
	onto  string
}

// taskWaitWhy is Why for a wait from task links, to someone who may not see
// the tasks.
const taskWaitWhy = "its task blocks this one's"

// said is Why as anyone who sees the pull request may read it.
func (w PRMergeWait) said() string {
	if w.Stack {
		return "its base is that pull request's branch"
	}
	if w.Set {
		return w.Why
	}
	return taskWaitWhy
}

// open: the pull request waited for hasn't merged or closed yet.
func (w PRMergeWait) open() bool { return w.State == "open" || w.State == "draft" }

// ref names the other pull request from pr's side: "#8", or "owner/repo#8"
// in another repo.
func (w PRMergeWait) ref(pr *PullRequest) string {
	if strings.EqualFold(w.Repo, pr.Repo) {
		return fmt.Sprintf("#%d", w.Number)
	}
	return fmt.Sprintf("%s#%d", w.Repo, w.Number)
}

// mergeWaitPrefix starts the merge check reason of a wait.
const mergeWaitPrefix = "merges after "

// mergeWaitReason is the merge check reason for one wait.
func mergeWaitReason(pr *PullRequest, w PRMergeWait) string {
	if w.Stack {
		r := fmt.Sprintf("%s%s%s%s when %s merges)", mergeWaitPrefix, w.ref(pr), stackedOnPhrase, "Waypoint moves it onto "+cmp(w.onto, "its base"), w.ref(pr))
		if w.Set {
			r += "; it is also set to merge after it"
		}
		return r
	}
	state := map[bool]string{true: "a draft", false: "open"}[w.State == "draft"]
	if w.apart {
		return fmt.Sprintf("%sa pull request in another project, which is still %s", mergeWaitPrefix, state)
	}
	return fmt.Sprintf("%s%s, which is still %s (%s)", mergeWaitPrefix, w.ref(pr), state, w.said())
}

// stackedOnPhrase is in the merge check reason of a stack wait, which is how
// isStackWait tells it from the other merge-after waits (WP-1670).
const stackedOnPhrase = ", which it is stacked on ("

// isStackWait says whether a merge check reason is a stack wait: a stacked
// pull request never merges into its parent's branch, by anyone.
func isStackWait(reason string) bool {
	return strings.HasPrefix(reason, mergeWaitPrefix) && strings.Contains(reason, stackedOnPhrase)
}

// stackMergedPrefix starts the merge check reason of a pull request still
// open into the branch of its parent, which merged: Waypoint moves it, so it
// waits for that and a person isn't asked (WP-1670).
const stackMergedPrefix = "stacked on a merged pull request"

// stackMergedReason is that reason.
func stackMergedReason(pr *PullRequest, w PRMergeWait) string {
	return fmt.Sprintf("%s: %s, whose branch is its base %s, merged; Waypoint moves it onto %s, and if it stays, move it with `wp pr edit %d --base %s`",
		stackMergedPrefix, w.ref(pr), pr.BaseRef, cmp(w.onto, "its base"), pr.Number, cmp(w.onto, "BRANCH"))
}

// stackClosedPrefix starts the merge check reason of a pull request whose
// base is the branch of one closed without merging: a person's call.
const stackClosedPrefix = "stacked on a pull request closed without merging"

// stackClosedReason is that reason (WP-1670).
func stackClosedReason(pr *PullRequest, w PRMergeWait) string {
	return fmt.Sprintf("%s: its base %s is %s's branch; move it with `wp pr edit %d --base %s`",
		stackClosedPrefix, pr.BaseRef, w.ref(pr), pr.Number, cmp(w.onto, "BRANCH"))
}

// withoutMergeWaits drops the waits from merge check reasons, but not a
// stack wait: nobody merges a pull request into its parent's branch (WP-1670).
func withoutMergeWaits(reasons []string) []string {
	return slices.DeleteFunc(slices.Clone(reasons), func(r string) bool {
		return strings.HasPrefix(r, mergeWaitPrefix) && !isStackWait(r)
	})
}

// mergeWaitApart is the SQL for "o's repo is missing from a project pr's
// repo is in", so someone who sees pr may not see o (args: pr's forge_id,
// repo, forge_id, repo).
const mergeWaitApart = `NOT (o.forge_id = ? AND o.repo = ? COLLATE NOCASE) AND EXISTS (SELECT 1 FROM project_repos x
			WHERE x.forge_id = ? AND x.repo = ? COLLATE NOCASE AND NOT EXISTS (SELECT 1 FROM project_repos y
				WHERE y.project_id = x.project_id AND y.forge_id = o.forge_id AND y.repo = o.repo COLLATE NOCASE))`

// mergeWaitIn is the first merge check reason that is a wait, or "".
func mergeWaitIn(reasons []string) string {
	for _, r := range reasons {
		if strings.HasPrefix(r, mergeWaitPrefix) {
			return r
		}
	}
	return ""
}

// mergeAfter lists the pull requests pr merges after, merged and closed
// ones too, set ones first. One that waits for pr in turn, directly or
// through others, is marked Mutual.
func (s *Service) mergeAfter(ctx context.Context, q queryer, pr *PullRequest) ([]PRMergeWait, error) {
	out, err := s.mergeAfterOnce(ctx, q, pr)
	if err != nil {
		return nil, err
	}
	known := map[int64][]PRMergeWait{pr.ID: out}
	for n, w := range out {
		if !w.open() {
			continue
		}
		loop, err := s.mergeLoop(ctx, q, w.PRID, pr.ID, known)
		if err != nil {
			return nil, err
		}
		out[n].Mutual, out[n].loop = loop > 0, loop
	}
	return out, nil
}

// maxMergeLoop bounds how many pull requests' waits mergeLoop reads.
const maxMergeLoop = 32

// mergeLoop is how many pull requests from waits through, over open waits,
// to come back to to (1: from waits for to itself); 0 if it doesn't, as
// far as maxMergeLoop pull requests' waits go. known holds the waits read.
func (s *Service) mergeLoop(ctx context.Context, q queryer, from, to int64, known map[int64][]PRMergeWait) (int, error) {
	type step struct {
		id   int64
		hops int
	}
	next := []step{{from, 1}}
	seen := map[int64]bool{from: true}
	for len(next) > 0 {
		at := next[0]
		next = next[1:]
		waits, ok := known[at.id]
		if !ok {
			if len(known) > maxMergeLoop {
				return 0, nil
			}
			p, err := s.prByID(ctx, q, at.id)
			if err != nil {
				return 0, err
			}
			if waits, err = s.mergeAfterOnce(ctx, q, p); err != nil {
				return 0, err
			}
			known[at.id] = waits
		}
		for _, w := range waits {
			switch {
			case !w.open():
			case w.PRID == to:
				return at.hops, nil
			case !seen[w.PRID]:
				seen[w.PRID] = true
				next = append(next, step{w.PRID, at.hops + 1})
			}
		}
	}
	return 0, nil
}

// mergeAfterOnce is mergeAfter without the look back.
func (s *Service) mergeAfterOnce(ctx context.Context, q queryer, pr *PullRequest) ([]PRMergeWait, error) {
	var out []PRMergeWait
	seen := map[int64]bool{}
	apartArgs := []any{pr.ForgeID, pr.Repo, pr.ForgeID, pr.Repo}
	// #nosec G202 -- the joined SQL is fixed text; values are bound
	rows, err := q.QueryContext(ctx, `SELECT o.id, o.repo, o.number, o.url, o.state,
			coalesce(nullif(u.display_name, ''), u.username, ''), a.created_agent, `+mergeWaitApart+`
		FROM pr_merge_after a JOIN pull_requests o ON o.id = a.after_id LEFT JOIN users u ON u.id = a.created_by
		WHERE a.pr_id = ? ORDER BY a.created_at, o.id`, append(apartArgs, pr.ID)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var w PRMergeWait
		var who, agent string
		if err := rows.Scan(&w.PRID, &w.Repo, &w.Number, &w.URL, &w.State, &who, &agent, &w.apart); err != nil {
			rows.Close()
			return nil, err
		}
		w.Set = true
		switch {
		case agent != "" && who != "":
			w.Why = "set by " + agent + " for " + who
		case agent != "":
			w.Why = "set by " + agent
		default:
			w.Why = "set by " + cmp(who, "someone")
		}
		seen[w.PRID] = true
		out = append(out, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// #nosec G202 -- the joined SQL is fixed text; values are bound
	rows, err = q.QueryContext(ctx, `SELECT o.id, o.repo, o.number, o.url, o.state, bp.key || '-' || bi.number, ap.key || '-' || ai.number,
			`+mergeWaitApart+`, bi.project_id, ai.project_id
		FROM external_links el
		JOIN issue_relations r ON r.kind = 'blocks' AND r.target_id = el.issue_id
		JOIN issues ai ON ai.id = el.issue_id JOIN projects ap ON ap.id = ai.project_id
		JOIN issues bi ON bi.id = r.source_id JOIN projects bp ON bp.id = bi.project_id JOIN statuses bs ON bs.id = bi.status_id
		JOIN external_links bl ON bl.issue_id = bi.id AND bl.kind = 'pull'
		JOIN pull_requests o ON `+linkedPR("o", "bl")+`
		WHERE el.kind = 'pull' AND `+linksPR("el")+`
		  AND bs.category != ? AND o.id != ?
		ORDER BY o.id, bi.id, ai.id`, append(apartArgs, pr.URL, pr.Repo, pr.Number, pr.ForgeID, pr.ForgeID, CatCanceled, pr.ID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var w PRMergeWait
		var blocker, blocked string
		if err := rows.Scan(&w.PRID, &w.Repo, &w.Number, &w.URL, &w.State, &blocker, &blocked, &w.apart, &w.tasks[0], &w.tasks[1]); err != nil {
			return nil, err
		}
		if seen[w.PRID] {
			continue
		}
		seen[w.PRID] = true
		w.Why = blocker + " blocks " + blocked
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.withStackWait(ctx, q, pr, out)
}

// stackParent is the pull request pr is stacked on, if there is one: of the
// repo itself, not a fork's, with pr's base for its head branch, the open or
// draft one, else the newest. Recorded pull requests only.
func (s *Service) stackParent(ctx context.Context, q queryer, pr *PullRequest) (*PullRequest, error) {
	if pr.BaseRef == "" {
		return nil, nil
	}
	p, err := scanPR(q.QueryRowContext(ctx, `SELECT `+prCols+` FROM pull_requests
		WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND head_ref = ? AND (head_repo = '' OR head_repo = repo COLLATE NOCASE) AND id != ?
		ORDER BY state IN ('open','draft') DESC, id DESC LIMIT 1`, pr.ForgeID, pr.Repo, pr.BaseRef, pr.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// endedBefore says whether a pull request ended before pr was opened, when
// both are known: a branch name reused since (WP-1670).
func endedBefore(parent, pr *PullRequest) bool {
	end := parent.ClosedAt
	if parent.MergedAt != nil {
		end = parent.MergedAt
	}
	return end != nil && pr.OpenedAt != nil && end.Before(*pr.OpenedAt)
}

// withStackWait adds the wait a stacked pull request has (WP-ADR153,
// WP-1670): its base is the head branch of a pull request of the repo,
// open or a draft, so it doesn't merge into that branch, which would move
// the parent's head after its review and CI. Waypoint moves it onto the
// parent's base when the parent merges, and the wait ends with that move;
// until then a merged parent is a hold of its own, and a parent closed
// without merging is a person's call. Recorded pull requests only, no forge
// read (settleStack does that): a branch the repo keeps (branchKept) isn't
// a parent's, and nor is one whose pull request ended before this one opened.
func (s *Service) withStackWait(ctx context.Context, q queryer, pr *PullRequest, out []PRMergeWait) ([]PRMergeWait, error) {
	if pr.State != "open" && pr.State != "draft" {
		return out, nil
	}
	parent, err := s.stackParent(ctx, q, pr)
	if err != nil || parent == nil {
		return out, err
	}
	if kept, err := s.branchKept(ctx, q, pr.ForgeID, pr.Repo, pr.BaseRef); err != nil || kept {
		return out, err
	}
	open := parent.State == "open" || parent.State == "draft"
	if !open && endedBefore(parent, pr) {
		return out, nil
	}
	w := PRMergeWait{PRID: parent.ID, Repo: parent.Repo, Number: parent.Number, URL: parent.URL, State: parent.State, Stack: true, onto: parent.BaseRef}
	for n := range out {
		if out[n].PRID == w.PRID {
			out[n].Stack, out[n].onto = true, w.onto
			if !out[n].Set {
				out[n].Why = out[n].said()
			}
			return out, nil
		}
	}
	w.Why = w.said()
	return append(out, w), nil
}

// linkedPR is the SQL join condition for "pull request p is the one link l
// names": its URL, or its repo and number on its forge.
func linkedPR(p, l string) string {
	return fmt.Sprintf(`(%[1]s.url = %[2]s.url OR (%[1]s.repo = %[2]s.repo COLLATE NOCASE AND %[1]s.number = %[2]s.number
			AND %[3]s))`, p, l, linkOnForge(l, p+".forge_id"))
}

// linksPR is the SQL condition for "link l names the pull request whose
// url, repo, number, forge_id and forge_id again are bound", as linkedPR.
func linksPR(l string) string {
	return fmt.Sprintf(`(%[1]s.url = ? OR (%[1]s.repo = ? COLLATE NOCASE AND %[1]s.number = ? AND %[2]s))`, l, linkOnForge(l, "?"))
}

// linkOnForge is the SQL condition for "link l is to the forge whose id is
// forge": one of its kind, at its address (two forges of a kind can each
// have an owner/repo#N).
func linkOnForge(l, forge string) string {
	return fmt.Sprintf(`%[1]s.provider = (SELECT kind FROM forges WHERE id = %[2]s)
			AND %[1]s.url LIKE (SELECT rtrim(base_url, '/') || '/%%' FROM forges WHERE id = %[2]s)`, l, forge)
}

// mergeAfterUnread is mergeWaitReasons' answer when it couldn't read them.
const mergeAfterUnread = "the pull requests it merges after couldn't be read"

// mergeWaitReasons are pr's merge check reasons for the pull requests it
// still waits for, and a note for a pair that waits both ways.
func (s *Service) mergeWaitReasons(ctx context.Context, pr *PullRequest) (reasons, notes []string) {
	waits, err := s.mergeAfter(ctx, s.db.R, pr)
	if err != nil {
		return []string{mergeAfterUnread}, nil
	}
	for _, w := range waits {
		switch {
		case w.Stack && w.State == "closed":
			reasons = append(reasons, stackClosedReason(pr, w))
		case w.Stack && w.State == "merged":
			reasons = append(reasons, stackMergedReason(pr, w))
		case !w.open():
		case w.Mutual && !w.Stack:
			other := "a pull request in another project"
			if !w.apart {
				other = fmt.Sprintf("%s (%s)", w.ref(pr), w.said())
			}
			if w.loop == 1 {
				notes = append(notes, other+" and this one are each set to merge after the other, so neither waits")
			} else {
				notes = append(notes, fmt.Sprintf("this one merges after %s, which merges after this one in turn through %d more, so none of them waits", other, w.loop-1))
			}
		default:
			reasons = append(reasons, mergeWaitReason(pr, w))
		}
	}
	return reasons, notes
}

// SetPRMergeAfter sets pr to merge only after the pull request number in
// repo ("" its own) of the same forge has (on), or drops that (off).
// Whoever may merge pr may. A wait that comes from task links is dropped by
// unlinking the tasks; off says so.
func (s *Service) SetPRMergeAfter(ctx context.Context, actor *Actor, prID int64, repo string, number int, on bool) (*PRDetail, error) {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return nil, err
	}
	projectID, err := s.mayMergeIn(ctx, actor, pr)
	if err != nil {
		return nil, err
	}
	repo = cmp(strings.TrimSpace(repo), pr.Repo)
	other, err := scanPR(s.db.R.QueryRowContext(ctx, `SELECT `+prCols+` FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND number = ?`, pr.ForgeID, repo, number))
	unknown := notFound(fmt.Sprintf("pull request %s#%d (Waypoint knows only the repos attached to it)", repo, number))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, unknown
	} else if err != nil {
		return nil, err
	}
	vis, args := prVisible(actor, 0)
	var n int
	// #nosec G202 -- vis is a fixed condition; values are bound
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM pull_requests p WHERE p.id = ? AND `+vis, append([]any{other.ID}, args...)...).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, unknown
	}
	if other.ID == pr.ID {
		return nil, invalid("a pull request can't merge after itself")
	}
	if !on && other.Repo == pr.Repo && other.HeadRef == pr.BaseRef && (other.State == "open" || other.State == "draft") && pr.BaseRef != "" {
		if waits, err := s.mergeAfter(ctx, s.db.R, pr); err == nil {
			for _, w := range waits {
				if w.PRID == other.ID && w.Stack && !w.Set {
					return nil, invalid("#%d waits for #%d because its base %s is that pull request's branch; Waypoint moves it onto %s when #%d merges. To land it without waiting for #%d, move it yourself: wp pr edit %d --base %s",
						pr.Number, other.Number, pr.BaseRef, cmp(w.onto, "BRANCH"), other.Number, other.Number, pr.Number, cmp(w.onto, "BRANCH"))
				}
			}
		}
	}
	if on && pr.State != "open" && pr.State != "draft" {
		return nil, invalid("it is %s: nothing to wait for", pr.State)
	}
	err = s.write(ctx, actor, func(t *tx) error {
		var wasBy sql.NullInt64
		var wasAgent string
		var wasAt int64
		had := true
		if err := t.QueryRowContext(ctx, `SELECT created_by, created_agent, created_at FROM pr_merge_after WHERE pr_id = ? AND after_id = ?`, pr.ID, other.ID).
			Scan(&wasBy, &wasAgent, &wasAt); errors.Is(err, sql.ErrNoRows) {
			had = false
		} else if err != nil {
			return err
		}
		if had == on {
			return nil
		}
		if !on {
			if _, err := t.ExecContext(ctx, `DELETE FROM pr_merge_after WHERE pr_id = ? AND after_id = ?`, pr.ID, other.ID); err != nil {
				return err
			}
		} else if _, err := t.ExecContext(ctx, `INSERT INTO pr_merge_after(pr_id, after_id, created_at, created_by, created_agent) VALUES (?,?,?,?,?)
			ON CONFLICT (pr_id, after_id) DO NOTHING`, pr.ID, other.ID, s.nowMS(), nullUser(actor.id()), actor.creditAgent()); err != nil {
			return err
		}
		var apart bool
		// #nosec G202 -- the joined SQL is fixed text; values are bound
		if err := t.QueryRowContext(ctx, `SELECT `+mergeWaitApart+` FROM pull_requests o WHERE o.id = ?`,
			pr.ForgeID, pr.Repo, pr.ForgeID, pr.Repo, other.ID).Scan(&apart); err != nil {
			return err
		}
		afterRef := fmt.Sprintf("%s#%d", other.Repo, other.Number)
		if apart {
			afterRef = "a pull request in another project"
		}
		detail, now, before := "set “"+excerpt(pr.Title, 120)+"” to merge after "+afterRef, "merge after "+afterRef, "not set"
		if !on {
			detail, now, before = "cleared the wait of “"+excerpt(pr.Title, 120)+"” for "+afterRef, "merge after cleared", "merge after "+afterRef
		}
		data := map[string]any{"pr_id": pr.ID, "on": on, "was_set": had}
		if !apart {
			data["after_id"], data["after"] = other.ID, afterRef
		}
		if had {
			data["was_created_at"], data["was_created_agent"] = wasAt, wasAgent
			if wasBy.Valid {
				data["was_created_by"] = wasBy.Int64
			}
		}
		t.audit(AuditEntry{Action: "pr.merge_after", Category: AuditControl, ProjectID: projectID, TargetType: "pr", TargetID: pr.ID,
			Target: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), URL: pr.URL, Detail: detail, About: pr.Agent})
		t.emit(&Event{Kind: EvPRActivity, ProjectID: projectID, Old: before, New: now, Data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.nudgeBabysit(pr.ID, false)
	return s.GetPullRequest(ctx, actor, pr.ID)
}

// nudgeMergeWaiters looks again at once at the pull requests that wait for
// pr, which merged or closed: they may merge now. Without it they would
// wait for the babysitting sweep.
func (s *Service) nudgeMergeWaiters(ctx context.Context, pr *PullRequest) {
	// #nosec G202 -- the joined SQL is fixed text; values are bound
	s.lookAgainAt(ctx, "find the pull requests that merge after one", `SELECT pr_id FROM pr_merge_after WHERE after_id = ?
		UNION
		SELECT id FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND base_ref = ? AND ? != '' AND state IN ('open','draft') AND id != ?
		UNION
		SELECT w.id FROM external_links bl
		JOIN issue_relations r ON r.kind = 'blocks' AND r.source_id = bl.issue_id
		JOIN external_links el ON el.issue_id = r.target_id AND el.kind = 'pull'
		JOIN pull_requests w ON `+linkedPR("w", "el")+`
		WHERE bl.kind = 'pull' AND `+linksPR("bl")+`
		  AND w.id != ? AND w.state IN ('open','draft')`, pr.ID, pr.ForgeID, pr.Repo, pr.HeadRef, pr.HeadRef, pr.ID, pr.URL, pr.Repo, pr.Number, pr.ForgeID, pr.ForgeID, pr.ID)
}

// onMergeOrderEvent looks again at once at the pull requests a task change
// may have let go or held (WP-912): a blocking task's status, a relation or
// a pull request link added or removed. Those are the ones a merge wait
// holds now, and the ones linked to the task or a task it blocks.
func (s *Service) onMergeOrderEvent(ctx context.Context, e *Event) {
	switch {
	case e.IssueID == 0:
		return
	case e.Kind == EvIssueStatus, e.Kind == EvIssueUpdated && e.Field == "relation", e.Kind == EvIssueLinked && e.Field == "pull":
	case e.Kind == EvProjectUpdated && e.forgeComment == nil:
	default:
		return
	}
	s.lookAgainAt(ctx, "find the pull requests a task change may let merge", `SELECT b.pr_id FROM babysits b JOIN pull_requests w ON w.id = b.pr_id
			WHERE b.ended_at IS NULL AND b.wait LIKE '%`+mergeWaitPrefix+`%' AND w.state IN ('open','draft')
		UNION
		SELECT w.id FROM external_links el JOIN pull_requests w ON `+linkedPR("w", "el")+`
		WHERE el.kind = 'pull' AND el.issue_id IN (SELECT ? UNION SELECT target_id FROM issue_relations WHERE source_id = ? AND kind = 'blocks')
		  AND w.state IN ('open','draft') AND w.armed_at IS NOT NULL`, e.IssueID, e.IssueID)
}

// lookAgainAt looks again at once at the pull requests query lists: their
// babysits step, and an armed one merges if it is ready.
func (s *Service) lookAgainAt(ctx context.Context, what, query string, args ...any) {
	// #nosec G202 -- callers pass fixed SQL; values are bound
	rows, err := s.db.R.QueryContext(ctx, query, args...)
	if err != nil {
		s.log.WarnContext(ctx, what, "err", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			s.log.WarnContext(ctx, what, "err", err)
			continue
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		s.log.WarnContext(ctx, what, "err", err)
	}
	rows.Close()
	bg := withoutLineLocks(context.WithoutCancel(ctx))
	for _, id := range ids {
		s.nudgeBabysit(id, false)
		s.goBG(func() { s.maybeAutoMerge(bg, id) })
	}
}

// visibleMergeAfter is mergeAfter for pull request id, leaving out the
// ones actor may not see and naming the tasks of a wait from task links
// only if actor may see both.
func (s *Service) visibleMergeAfter(ctx context.Context, actor *Actor, id int64) ([]PRMergeWait, error) {
	pr, err := s.prByID(ctx, s.db.R, id)
	if err != nil {
		return nil, err
	}
	waits, err := s.mergeAfter(ctx, s.db.R, pr)
	if err != nil || len(waits) == 0 {
		return nil, err
	}
	vis, args := prVisible(actor, 0)
	var out []PRMergeWait
	for _, w := range waits {
		var n int
		// #nosec G202 -- vis is a fixed condition; values are bound
		if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM pull_requests p WHERE p.id = ? AND `+vis, append([]any{w.PRID}, args...)...).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		if !w.Set && w.tasks != [2]int64{} {
			pv, pargs := visibleSQL("id", actor)
			// #nosec G202 -- pv is a fixed condition; values are bound
			if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM projects WHERE id IN (?, ?) AND `+pv, append([]any{w.tasks[0], w.tasks[1]}, pargs...)...).Scan(&n); err != nil {
				return nil, err
			}
			if n < len(slices.Compact([]int64{w.tasks[0], w.tasks[1]})) {
				w.Why = w.said()
			}
		}
		out = append(out, w)
	}
	return out, nil
}
