package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// heavyLiftRE is CodeRabbit's effort badge for a big change ("** Minor** ·
// **Functional Correctness** · **Heavy lift**"), also in a mirror's relays.
var heavyLiftRE = lazyre.New(`(?i)·\s*\*\*\s*heavy lift\s*\*\*`)

// heavyLift: a bot marked the comment a heavy lift. Such a finding is a
// deferral unless the task needs it to be correct.
func (rc *PRReviewComment) heavyLift() bool {
	return rc.botComment() && heavyLiftRE.MatchString(rc.Body)
}

// PRIntent is what a pull request is for, as a round's packet opens with
// it: the tasks it is linked to, its description when it shipped, and the
// decisions they cite. Every verdict is weighed against it.
type PRIntent struct {
	Tasks     []IntentTask     `json:"tasks,omitempty"`
	Body      string           `json:"body,omitempty"`
	Decisions []IntentDecision `json:"decisions,omitempty"`
}

// IntentTask is a task the pull request is linked to.
type IntentTask struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`

	id, projectID int64
	projectKey    string
}

// IntentDecision is a decision the task or the description cites.
type IntentDecision struct {
	Code    string `json:"code"`
	Title   string `json:"title"`
	Status  string `json:"status,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

const (
	intentTaskChars     = 1500
	intentBodyChars     = 2000
	intentDecisionChars = 300
	intentMaxTasks      = 3
	intentMaxDecisions  = 5
)

// prIssues are the tasks linked to the pull request that the actor may
// see, oldest link first; text from outside waiting for a person is left
// out (WP-279).
func (s *Service) prIssues(ctx context.Context, actor *Actor, pr *PullRequest) ([]IntentTask, error) {
	vis, vargs := visibleSQL("i.project_id", actor)
	args := append([]any{pr.URL, pr.Repo, pr.Number}, vargs...)
	// #nosec G202 -- visibleSQL and draftsHidden are fixed SQL; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT i.id, i.project_id, p.key, i.number, i.title, i.description
		FROM external_links el JOIN issues i ON i.id = el.issue_id JOIN projects p ON p.id = i.project_id
		WHERE el.kind = 'pull' AND (el.url = ? OR (el.repo = ? COLLATE NOCASE AND el.number = ?))
		  AND i.outside = 0 AND `+vis+` AND `+draftsHidden(actor, "i")+`
		GROUP BY i.id ORDER BY min(el.id)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IntentTask
	for rows.Next() {
		var t IntentTask
		var n int64
		if err := rows.Scan(&t.id, &t.projectID, &t.projectKey, &n, &t.Title, &t.Description); err != nil {
			return nil, err
		}
		t.Key = fmt.Sprintf("%s-%d", t.projectKey, n)
		out = append(out, t)
	}
	return out, rows.Err()
}

// prIntent is the pull request's intent anchor (nil when nothing is known
// of it). The description is the one it shipped with, else today's.
func (s *Service) prIntent(ctx context.Context, actor *Actor, pr *PullRequest) *PRIntent {
	tasks, err := s.prIssues(ctx, actor, pr)
	if err != nil {
		s.log.WarnContext(ctx, "read a pull request's tasks", "pr", pr.ID, "err", err)
	}
	in := &PRIntent{}
	if fp := s.footprintOf(ctx, pr.ID); fp.Ship != nil {
		in.Body = fp.Ship.Body
	} else {
		in.Body = s.prBodyNow(ctx, pr)
	}
	in.Body = clipLines(strings.TrimSpace(in.Body), intentBodyChars)
	texts := []string{in.Body}
	var scope *int64
	for i, t := range tasks {
		if i == intentMaxTasks {
			break
		}
		if scope == nil {
			scope = &t.projectID
		}
		texts = append(texts, t.Title, t.Description)
		t.Description = clipLines(strings.TrimSpace(t.Description), intentTaskChars)
		in.Tasks = append(in.Tasks, t)
	}
	if scope == nil {
		pid, err := s.attachedProject(ctx, s.db.R, pr.ForgeID, pr.Repo)
		if err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "read the project to scope a pull request's intent", "repo", pr.Repo, "number", pr.Number, "err", err)
			}
		} else if pid != 0 {
			scope = &pid
		}
	}
	seen := map[int64]bool{}
	for _, text := range texts {
		for _, m := range proseADRRE.FindAllString(text, 50) {
			if len(in.Decisions) == intentMaxDecisions {
				break
			}
			id, ok := s.designByCodeLoose(ctx, s.db.R, m, scope)
			if !ok || seen[id] {
				continue
			}
			seen[id] = true
			d := IntentDecision{Code: m}
			if c, ok := ParseDesignCode(m); ok {
				d.Code = c.String()
			}
			if _, _, err := s.designFor(ctx, s.db.R, actor, id, RoleViewer); err != nil {
				continue
			}
			var body string
			if s.db.R.QueryRowContext(ctx, `SELECT title, status, body FROM design_notes WHERE id = ?`, id).Scan(&d.Title, &d.Status, &body) != nil {
				continue
			}
			d.Excerpt = excerpt(body, intentDecisionChars)
			in.Decisions = append(in.Decisions, d)
		}
	}
	if len(in.Tasks) == 0 && in.Body == "" && len(in.Decisions) == 0 {
		return nil
	}
	return in
}

// prBodyNow is the pull request's description as the forge has it now, as
// a person approved it when its author is from outside ("" when unknown).
func (s *Service) prBodyNow(ctx context.Context, pr *PullRequest) string {
	body, rerr := s.prBodyRead(ctx, pr)
	if rerr != nil {
		if readFault(rerr) {
			s.log.WarnContext(ctx, "pr body read failed", "repo", pr.Repo, "number", pr.Number, "pr", pr.ID, "err", rerr)
		}
	}
	return body
}

// prBodyRead is prBodyNow for a caller that must tell a description the
// forge couldn't give from one it doesn't have. A forge with no pull request
// by that number has no description; any other failure is an error.
func (s *Service) prBodyRead(ctx context.Context, pr *PullRequest) (string, error) {
	prc, ok := s.Forges.(ForgePRs)
	if !ok {
		return "", nil
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return "", err
	}
	fp, err := prc.GetPR(ctx, botForge(f), pr.Repo, pr.Number)
	if isStatus(err, http.StatusNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if fp == nil {
		return "", nil
	}
	return s.approvedBody(ctx, f, pr, fp), nil
}

// render is the anchor as a packet's opening section.
func (in *PRIntent) render(b *strings.Builder) {
	b.WriteString("\n## What the PR is for\n")
	in.write(b, "### ")
}

// write is the anchor's text, with headings at the given level.
func (in *PRIntent) write(b *strings.Builder, h string) {
	for _, t := range in.Tasks {
		fmt.Fprintf(b, "\n%sTask %s: %s\n", h, t.Key, oneLine(t.Title))
		if t.Description != "" {
			b.WriteString("<untrusted>\n" + untrusted(t.Description) + "\n</untrusted>\n")
		}
	}
	if in.Body != "" {
		fmt.Fprintf(b, "\n%sPR description when it shipped\n", h)
		b.WriteString("<untrusted>\n" + untrusted(in.Body) + "\n</untrusted>\n")
	}
	if len(in.Decisions) > 0 {
		fmt.Fprintf(b, "\n%sDecisions cited\n", h)
		for _, d := range in.Decisions {
			status := ""
			if d.Status != "" {
				status = " (" + d.Status + ")"
			}
			fmt.Fprintf(b, "- %s%s: %s", d.Code, status, oneLine(d.Title))
			if d.Excerpt != "" {
				b.WriteString(" <untrusted>" + untrusted(d.Excerpt) + "</untrusted>")
			}
			b.WriteString("\n")
		}
	}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clipLines cuts s to about n characters at a line's end, keeping its lines.
func clipLines(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)[:n]
	cut := string(r)
	if i := strings.LastIndexByte(cut, '\n'); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " \n") + "\n…"
}

// fileDeferred files the follow-up task for review comments deferred on one
// pull request, in the project of its first task (else its repository's),
// related to that task. It returns the new task's key.
func (s *Service) fileDeferred(ctx context.Context, actor *Actor, pr *PullRequest, list []*PRReviewComment, why string) (string, error) {
	tasks, err := s.prIssues(ctx, actor, pr)
	if err != nil {
		return "", err
	}
	var project, linked string
	if len(tasks) > 0 {
		project, linked = tasks[0].projectKey, tasks[0].Key
	} else {
		key, err := s.attachedProjectKey(ctx, pr.ForgeID, pr.Repo)
		if err != nil {
			return "", err
		}
		project = key
	}
	if project == "" {
		return "", invalid("%s isn't attached to a project, so there is nowhere to file the follow-up task: --reject or --needs-person instead", pr.Repo)
	}
	title := ""
	for _, rc := range list {
		if h := headline(rc.Body); h != "" {
			title = h
			break
		}
	}
	if title == "" {
		title = "review comment " + list[0].Ref
	}
	title = excerpt(fmt.Sprintf("Follow-up from %s#%d: %s", pr.Repo, pr.Number, strings.TrimRight(title, ".")), 110)
	var d strings.Builder
	fmt.Fprintf(&d, "Deferred from %s#%d (%s)", pr.Repo, pr.Number, pr.URL)
	if linked != "" {
		fmt.Fprintf(&d, ", the pull request for %s", linked)
	}
	d.WriteString(": real, but not needed for that task.\n\n")
	fmt.Fprintf(&d, "Why it was put off: %s\n", why)
	for _, rc := range list {
		where := ""
		if rc.Path != "" {
			where = " on `" + rc.Path
			if rc.Line > 0 {
				where += fmt.Sprintf(":%d", rc.Line)
			}
			where += "`"
		}
		fmt.Fprintf(&d, "\n%s by %s%s", rc.Ref, cmp(rc.Author, "someone"), where)
		if rc.URL != "" {
			d.WriteString(" (" + rc.URL + ")")
		}
		d.WriteString(":\n\n> " + strings.ReplaceAll(clipLines(strings.TrimSpace(stripMarkers(rc.Body)), 500), "\n", "\n> ") + "\n")
	}
	issue, err := s.CreateIssue(ctx, actor, IssueInput{Project: project, Title: title, Description: d.String()})
	if err != nil {
		return "", err
	}
	if linked != "" {
		if err := s.AddRelation(ctx, actor, issue.Key, "relates", linked); err != nil {
			s.log.WarnContext(ctx, "relate a deferred follow-up to its task", "issue", issue.Key, "task", linked, "err", err)
		}
	}
	return issue.Key, nil
}

// deferComments files one follow-up task per pull request for the
// comments, then marks them deferred with "Filed as KEY: why" as the
// reply, which resolves their threads.
func (s *Service) deferComments(ctx context.Context, actor *Actor, list []*PRReviewComment, prs map[int64]*PullRequest, why string) error {
	if err := actor.need(PermIssuesCreate); err != nil {
		return err
	}
	var order []int64
	byPR := map[int64][]*PRReviewComment{}
	for _, rc := range list {
		if _, ok := byPR[rc.prID]; !ok {
			order = append(order, rc.prID)
		}
		byPR[rc.prID] = append(byPR[rc.prID], rc)
	}
	by := rcByActor(actor)
	for _, id := range order {
		pr := prs[id]
		key, err := s.fileDeferred(ctx, actor, pr, byPR[id], why)
		if err != nil {
			return err
		}
		note := fmt.Sprintf("Filed as %s: %s", key, why)
		for _, rc := range byPR[id] {
			if err := s.markComment(ctx, pr, rc, VerdictDeferred, note, "", by, s.roundOf(ctx, pr.ID)); err != nil {
				return err
			}
			rc.Note = note
		}
	}
	return nil
}

// prFootprint is what a pull request was when it shipped (when its first
// babysitting round was sent) and the footprint checks made since, kept
// with it as JSON.
type prFootprint struct {
	Ship   *footprintShip   `json:"ship,omitempty"`
	Checks []footprintCheck `json:"checks,omitempty"`
	// next is the reference the check footprintNow found due will leave for
	// the one after, and upTo the last round it covers; never stored.
	next footprintAt
	upTo int
	// rounds are the files the rounds since the last check brought in.
	rounds footprintRounds
}

// footprintAt is the files a pull request touched at a head and its size.
type footprintAt struct {
	SHA   string   `json:"sha"`
	Paths []string `json:"paths"`
	Lines int      `json:"lines"`
}

type footprintShip struct {
	footprintAt
	Known bool      `json:"known"` // its files and size were read
	Body  string    `json:"body,omitempty"`
	At    time.Time `json:"at"`
}

type footprintCheck struct {
	footprintAt
	Outcome string    `json:"outcome"` // audit, or skipped (Why says why)
	Audit   int64     `json:"audit,omitempty"`
	Why     string    `json:"why,omitempty"`
	At      time.Time `json:"at"`
	// Ahead: asked by footprintAhead, before the pull request was otherwise
	// ready (WP-1853). Queued: when the babysit first saw its audit still
	// queued once it was ready; the 15-minute queue wait is counted from there.
	Ahead  bool      `json:"ahead,omitempty"`
	Queued time.Time `json:"queued,omitzero"`
	// Round is the last round the check covered (WP-1939): later rounds are
	// measured from it. Zero in a record made before: every pushed round
	// since the pull request shipped counts.
	Round int `json:"round,omitempty"`
}

const (
	footprintAudited = "audit"
	footprintSkipped = "skipped"
	// footprintMaxChecks is how many footprint checks a pull request gets:
	// one after the rounds that spread, one more after the rounds that
	// undid it.
	footprintMaxChecks = 2
	// footprintMaxAhead is how many of them footprintAhead may ask for.
	footprintMaxAhead = 2
	// A diff grew when it is half again as big and more than this many
	// lines bigger.
	footprintMinLines = 30
	// What a check is told of the rounds' changes, at most.
	footprintDiffChars = 24000
	footprintRoundDiff = 8000
	footprintMaxPaths  = 40
)

func (s *Service) footprintOf(ctx context.Context, prID int64) prFootprint {
	var raw string
	var fp prFootprint
	if s.db.R.QueryRowContext(ctx, `SELECT footprint FROM pull_requests WHERE id = ?`, prID).Scan(&raw) == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &fp)
	}
	return fp
}

func (s *Service) saveFootprint(ctx context.Context, prID int64, fp prFootprint) {
	raw, _ := json.Marshal(fp)
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		_, err := t.ExecContext(ctx, `UPDATE pull_requests SET footprint = ? WHERE id = ?`, string(raw), prID)
		return err
	})
	if err != nil {
		s.log.WarnContext(ctx, "keep a pull request's footprint", "pr", prID, "err", err)
	}
}

// recordShip keeps what the pull request is before its first round: the
// footprint every later one is measured against. A babysit whose rounds
// pushed before there was a drift guard gets none, so it is never checked.
func (s *Service) recordShip(ctx context.Context, b *Babysit, pr *PullRequest) {
	if pr.HeadSHA == "" || slices.ContainsFunc(b.Rounds, func(r BabysitRound) bool { return r.Pushed }) {
		return
	}
	fp := s.footprintOf(ctx, pr.ID)
	if fp.Ship != nil {
		return
	}
	s.loadPRFiles(ctx, pr)
	lines, known := pr.changedLines()
	fp.Ship = &footprintShip{footprintAt: footprintAt{SHA: pr.HeadSHA, Paths: pr.files, Lines: lines}, Known: pr.filesKnown && known,
		Body: clipLines(strings.TrimSpace(s.prBodyNow(ctx, pr)), intentBodyChars), At: s.now()}
	s.saveFootprint(ctx, pr.ID, fp)
}

// footprintRounds is what the fix-up rounds pushed since the last check
// (WP-1939): their own files and lines, not the forge's size of the whole
// pull request, which counts what main gained when the merge base is stale.
type footprintRounds struct {
	added   []string // files they touched that the reference didn't have
	files   []string // every file they touched
	lines   int
	last    int   // the last round counted
	unknown []int // counted rounds that reported neither files nor a diff
}

// roundLines is the lines a round pushed: as its runner counted them, else
// from its kept diff (a capped diff is a lower bound).
func roundLines(r BabysitRound) int {
	if r.Lines > 0 {
		return r.Lines
	}
	return diffLines(r.Diff)
}

// diffLines counts the added and removed lines of a unified diff, not its
// file headers.
func diffLines(diff string) int {
	n, header := 0, false
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			header = true
		case strings.HasPrefix(l, "@@"):
			header = false
		case header:
		case strings.HasPrefix(l, "+"), strings.HasPrefix(l, "-"):
			n++
		}
	}
	return n
}

// measureRounds reads the pushed rounds after round `after` that came once
// the pull request shipped, against ref.
func measureRounds(rounds []BabysitRound, shipped time.Time, after int, ref footprintAt) footprintRounds {
	var m footprintRounds
	for _, r := range rounds {
		if !r.Pushed || r.At.Before(shipped) || r.N <= after {
			continue
		}
		m.last = max(m.last, r.N)
		m.lines += roundLines(r)
		if len(r.Files) == 0 && r.Diff == "" {
			m.unknown = append(m.unknown, r.N)
		}
		for _, f := range r.Files {
			if !slices.Contains(m.files, f) {
				m.files = append(m.files, f)
				if !slices.Contains(ref.Paths, f) {
					m.added = append(m.added, f)
				}
			}
		}
	}
	return m
}

// grew is whether the rounds changed more than footprintMinLines lines and
// more than half of what the reference was.
func (m footprintRounds) grew(ref footprintAt) bool {
	return m.lines > footprintMinLines && m.lines*2 > ref.Lines
}

// footprintNow says whether the head needs a footprint check: rounds pushed
// since it shipped, and since the last check they touched new files or
// changed more than half of what it was before them. A round that
// reported nothing of what it pushed fails open, as does anything else not
// known; an earlier check's audit that can't be read is an error.
func (s *Service) footprintNow(ctx context.Context, b *Babysit, pr *PullRequest) (prFootprint, string, bool, error) {
	fp := s.footprintOf(ctx, pr.ID)
	if b == nil || fp.Ship == nil || !fp.Ship.Known || pr.HeadSHA == "" {
		return fp, "", false, nil
	}
	// A check asked ahead for a head the pull request has since left doesn't
	// use up the budget: the late path still asks for the head it ends on.
	ref, audited, seen, covered := fp.Ship.footprintAt, 0, fp.Ship.SHA, 0
	for _, c := range fp.Checks {
		// An audit of a head since left that was stopped, or is about to be
		// (cancelStaleFootprints), looked at nothing: growth isn't measured
		// from it.
		if c.Outcome == footprintAudited && !sameSHA(c.SHA, pr.HeadSHA) {
			unanswered, err := s.auditUnanswered(ctx, c.Audit)
			if err != nil {
				return fp, "", false, err
			}
			if unanswered {
				continue
			}
		}
		seen = c.SHA
		if c.Outcome == footprintAudited {
			ref, covered = c.footprintAt, c.Round
			if !c.Ahead || sameSHA(c.SHA, pr.HeadSHA) {
				audited++
			}
		}
	}
	if audited >= footprintMaxChecks || sameSHA(seen, pr.HeadSHA) {
		return fp, "", false, nil
	}
	m := measureRounds(b.Rounds, fp.Ship.At, covered, ref)
	if m.last == 0 {
		return fp, "", false, nil
	}
	if len(m.unknown) > 0 {
		for _, n := range m.unknown {
			if _, seen := s.footprintUnknown.LoadOrStore(fmt.Sprintf("%d:%d", pr.ID, n), true); !seen {
				s.log.InfoContext(ctx, "footprint not measured: a round reported neither files nor a diff", "repo", pr.Repo, "number", pr.Number, "round", n)
			}
		}
		return fp, "", false, nil
	}
	grew := m.grew(ref)
	var why []string
	if len(m.added) > 0 {
		why = append(why, fmt.Sprintf("touched %d new file%s", len(m.added), plural(len(m.added))))
	}
	if grew {
		why = append(why, fmt.Sprintf("changed %d lines (it was %d before them)", m.lines, ref.Lines))
	}
	if len(why) == 0 {
		return fp, "", false, nil
	}
	fp.rounds, fp.upTo = m, m.last
	fp.next = footprintAt{Paths: append(slices.Clone(ref.Paths), m.added...), Lines: ref.Lines + m.lines}
	return fp, "its fix-up rounds " + strings.Join(why, " and "), true, nil
}

// footprintDue: a readiness reason while the check is owed and not asked
// for yet, or asked ahead and still queued ("" when none), and when to look
// again. A babysit or queued audit that can't be read is an error, not
// "nothing due".
func (s *Service) footprintDue(ctx context.Context, pr *PullRequest) (string, *time.Time, error) {
	b, err := s.activeBabysit(ctx, s.db.R, pr.ID)
	if err != nil || b == nil {
		return "", nil, err
	}
	_, why, due, err := s.footprintNow(ctx, b, pr)
	if err != nil {
		return "", nil, err
	}
	if due {
		return "footprint check due: " + why, nil, nil
	}
	// A check asked ahead, whose audit no runner has taken: the queue wait
	// runs from when the babysit first saw it so once the pull request was
	// otherwise ready (stamped by footprintStep), not from the ask.
	for _, c := range s.footprintOf(ctx, pr.ID).Checks {
		if !c.Ahead || c.Outcome != footprintAudited || !sameSHA(c.SHA, pr.HeadSHA) {
			continue
		}
		queued, err := s.auditQueued(ctx, c.Audit)
		if err != nil {
			return "", nil, err
		}
		if !queued {
			continue
		}
		wake := c.Queued.Add(auditQueueWait)
		if c.Queued.IsZero() {
			// Not stamped yet: the babysit's next step does it. One it never
			// reaches (it rests, or was set off) must not hold the merge for
			// good: an ask older than an audit may take holds nothing.
			if s.now().Sub(c.At) > auditWaitMax {
				continue
			}
			wake = s.now().Add(driftRetry)
		} else if !s.now().Before(wake) {
			continue
		}
		return fmt.Sprintf("footprint check queued (audit #%d)", c.Audit), &wake, nil
	}
	return "", nil, nil
}

// auditQueued says whether an audit is still queued, no runner having taken it.
func (s *Service) auditQueued(ctx context.Context, id int64) (bool, error) {
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM code_audits a LEFT JOIN runner_jobs j ON j.id = a.job_id
		WHERE a.id = ? AND a.status IN ('queued','running') AND coalesce(j.state, 'queued') = 'queued'`, id).Scan(&n)
	return n > 0, err
}

// auditUnanswered says whether an audit was cancelled or hasn't finished.
func (s *Service) auditUnanswered(ctx context.Context, id int64) (bool, error) {
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM code_audits WHERE id = ? AND status IN (?, ?, ?)`,
		id, AuditCancelled, AuditQueued, AuditRunning).Scan(&n)
	return n > 0, err
}

// stampQueuedAhead notes the first time the babysit, with the pull request
// otherwise ready, sees the check it asked ahead still queued.
func (s *Service) stampQueuedAhead(ctx context.Context, pr *PullRequest) {
	fp := s.footprintOf(ctx, pr.ID)
	changed := false
	for i, c := range fp.Checks {
		if !c.Ahead || c.Outcome != footprintAudited || !c.Queued.IsZero() || !sameSHA(c.SHA, pr.HeadSHA) {
			continue
		}
		queued, err := s.auditQueued(ctx, c.Audit)
		if err != nil {
			// The next step stamps it; the merge's own read holds on a fault.
			s.log.WarnContext(ctx, "read whether a footprint check is queued", "audit", c.Audit, "err", err)
		} else if queued {
			fp.Checks[i].Queued, changed = s.now(), true
		}
	}
	if changed {
		s.saveFootprint(ctx, pr.ID, fp)
	}
}

// cancelStaleFootprints stops footprint audits asked for a head the pull
// request has left: their answer is about code that is gone, and they would
// hold the merge for it.
func (s *Service) cancelStaleFootprints(ctx context.Context, pr *PullRequest) {
	for _, c := range s.footprintOf(ctx, pr.ID).Checks {
		if c.Outcome != footprintAudited || sameSHA(c.SHA, pr.HeadSHA) {
			continue
		}
		var n int
		if s.db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM code_audits WHERE id = ? AND status IN ('queued','running')`, c.Audit).Scan(&n) != nil || n == 0 {
			continue
		}
		if _, err := s.CancelAudit(ctx, SystemActor(), c.Audit); err != nil {
			s.log.WarnContext(ctx, "cancel a footprint check of an old head", "repo", pr.Repo, "number", pr.Number, "audit", c.Audit, "err", err)
		}
	}
}

// footprintStep asks for the footprint check the head needs, if any, and
// says whether it did (the babysit then waits for it). A check that can't
// be asked for is recorded as skipped: it fails open.
func (s *Service) footprintStep(ctx context.Context, b *Babysit, pr *PullRequest) bool {
	s.stampQueuedAhead(ctx, pr)
	a, why, asked := s.requestFootprint(ctx, b, pr, false)
	if !asked {
		return false
	}
	s.setState(ctx, b, pr, BabysitWatching, fmt.Sprintf("footprint check #%d of what its rounds added (%s)", a.ID, why))
	return true
}

// footprintAhead asks for the footprint check as soon as it is due for a head
// whose checks passed, without taking the step over: it runs beside the quiet
// period or a rest, so it isn't asked only once nothing else holds the pull
// request and then holds the merge for the whole audit (WP-1853). It isn't
// asked beside a review that is going: that review's round may move the head.
// Asking again for the same head is a no-op (its request key, and the check
// recorded for the head). At most footprintMaxAhead are asked this way, and
// one that can't be asked for records nothing, so the late path decides.
func (s *Service) footprintAhead(ctx context.Context, b *Babysit, pr *PullRequest) {
	if pr.CIState != CIPassed || !sameSHA(pr.ciSHA, pr.HeadSHA) {
		return // a failed head gets a round, and its push makes a new head
	}
	n := 0
	for _, c := range s.footprintOf(ctx, pr.ID).Checks {
		if c.Ahead {
			n++
		}
	}
	if n >= footprintMaxAhead {
		return
	}
	if a, why, asked := s.requestFootprint(ctx, b, pr, true); asked {
		s.log.InfoContext(ctx, "footprint check asked early", "repo", pr.Repo, "number", pr.Number, "audit", a.ID, "why", why)
	}
}

// requestFootprint asks for the footprint check the head needs and records
// it; asked is false when none is due or it could not be asked for (then it
// is recorded as skipped, so the merge doesn't wait for it; an ahead ask
// records nothing, and the late path asks again).
func (s *Service) requestFootprint(ctx context.Context, b *Babysit, pr *PullRequest, ahead bool) (a *CodeAudit, why string, asked bool) {
	fp, why, due, err := s.footprintNow(ctx, b, pr)
	if err != nil {
		// Not known whether one is due: the merge's own read holds on it.
		s.log.WarnContext(ctx, "read whether a footprint check is due", "pr", pr.ID, "err", err)
		return nil, "", false
	}
	if !due {
		return nil, "", false
	}
	check := footprintCheck{footprintAt: fp.next, Round: fp.upTo, At: s.now(), Ahead: ahead}
	check.SHA = pr.HeadSHA
	err = errors.New("no member's runner to run it on")
	if owner := s.babysitOwnerFor(ctx, pr); owner != 0 {
		a, err = s.RequestAudit(ctx, SystemActor(), AuditInput{PRID: pr.ID, Via: "system", owner: owner,
			requestKey: fmt.Sprintf("pr-footprint:%d:%s", pr.ID, strings.ToLower(pr.HeadSHA)), purpose: AuditFootprint,
			Instructions: footprintInstructions, prContext: s.footprintContext(ctx, b, pr, fp, why)})
	}
	if err != nil && ahead {
		s.log.Info("footprint check not asked early", "repo", pr.Repo, "number", pr.Number, "err", err)
		return nil, why, false
	}
	if err != nil {
		check.Outcome, check.Why = footprintSkipped, PublicError(err)
		fp.Checks = append(fp.Checks, check)
		s.saveFootprint(ctx, pr.ID, fp)
		s.log.Info("footprint check skipped", "repo", pr.Repo, "number", pr.Number, "err", err)
		return nil, why, false
	}
	check.Outcome, check.Audit = footprintAudited, a.ID
	fp.Checks = append(fp.Checks, check)
	s.saveFootprint(ctx, pr.ID, fp)
	return a, why, true
}

// footprintInstructions is what the footprint check looks for.
const footprintInstructions = `Compare what the fix-up rounds added after the pull request shipped with what it is for (its task, its description when it shipped, the decisions they cite). ` +
	`Report each change the task doesn't need as one medium finding at the file and line where it starts: what it is, and whether to undo it or split it out into its own pull request. ` +
	`A change a review comment or a failed check needed for the task to be correct is in scope, and so is what the task's own description asks for. ` +
	`Don't report bugs, style or anything the pull request had when it shipped: only scope. If everything the rounds added is in scope, report no findings and say so.`

// footprintContext replaces the pull request's description in the check's
// prompt: what it shipped as, what it is now, what it is for, and what each
// round pushed.
func (s *Service) footprintContext(ctx context.Context, b *Babysit, pr *PullRequest, fp prFootprint, why string) string {
	var c strings.Builder
	s.loadPRFiles(ctx, pr)
	lines, _ := pr.changedLines()
	fmt.Fprintf(&c, "This is pull request %s#%d, <untrusted>%s</untrusted>. It shipped at %s touching %d file%s (%d lines changed); babysitting rounds have changed it since, and at its head %s it touches %d file%s (%d lines): %s.\n",
		pr.Repo, pr.Number, untrusted(pr.Title), short(fp.Ship.SHA), len(fp.Ship.Paths), plural(len(fp.Ship.Paths)), fp.Ship.Lines,
		short(pr.HeadSHA), len(pr.files), plural(len(pr.files)), lines, why)
	added := measureRounds(b.Rounds, fp.Ship.At, 0, fp.Ship.footprintAt).added
	writePaths := func(title string, paths []string) {
		if len(paths) == 0 {
			return
		}
		fmt.Fprintf(&c, "\n%s:\n", title)
		for i, p := range paths {
			if i == footprintMaxPaths {
				fmt.Fprintf(&c, "- … and %d more\n", len(paths)-i)
				break
			}
			c.WriteString("- " + untrusted(p) + "\n")
		}
	}
	writePaths("The files it touched when it shipped", fp.Ship.Paths)
	writePaths("The files the rounds brought in since", added)
	if in := s.prIntent(ctx, SystemActor(), pr); in != nil {
		c.WriteString("\nWhat it is for:\n")
		in.write(&c, "#### ")
	}
	var diffs strings.Builder
	for _, r := range b.Rounds {
		if !r.Pushed || r.At.Before(fp.Ship.At) {
			continue
		}
		fmt.Fprintf(&diffs, "\nRound %d (on %s, for %s) pushed:\n", r.N, short(r.SHA), roundWork(r.Checks, r.Conflict, r.Comments, r.Findings))
		if r.Diff == "" {
			diffs.WriteString("(its diff wasn't kept)\n")
			continue
		}
		d := r.Diff
		if utf8.RuneCountInString(d) > footprintRoundDiff {
			d = string([]rune(d)[:footprintRoundDiff]) + "\n… (cut short)"
		}
		diffs.WriteString("```diff\n" + strings.TrimRight(d, "\n") + "\n```\n")
		if diffs.Len() > footprintDiffChars {
			diffs.WriteString("(later rounds left out: the whole change is at the end)\n")
			break
		}
	}
	if diffs.Len() > 0 {
		c.WriteString("\nWhat each fix-up round pushed:\n<untrusted>" + untrusted(diffs.String()) + "</untrusted>")
	}
	c.WriteString("\nThe whole change at its head, against its base, is at the end of this prompt.")
	return c.String()
}
