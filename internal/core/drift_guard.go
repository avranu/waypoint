package core

// The drift guard (WP-440, under WP-436): babysitting rounds fix what
// reviewers and checks found, and a reviewer can always find more. Each
// round's packet starts from what the pull request is for (its task, its
// description when it shipped, the decisions they cite), a finding the task
// doesn't need is deferred to a follow-up task (`wp pr ack --defer`), and a
// pull request whose rounds spread to new files or grew it by half again
// gets a footprint check before it merges.

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

// ---- heavy lifts ----

// heavyLiftRE is CodeRabbit's effort badge for a big change ("** Minor** ·
// **Functional Correctness** · **Heavy lift**"), also in a mirror's relays.
var heavyLiftRE = lazyre.New(`(?i)·\s*\*\*\s*heavy lift\s*\*\*`)

// heavyLift: a bot marked the comment a heavy lift. Such a finding is a
// deferral unless the task needs it to be correct.
func (rc *PRReviewComment) heavyLift() bool {
	return rc.botComment() && heavyLiftRE.MatchString(rc.Body)
}

// ---- the intent anchor ----

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
				continue // not one this actor may read
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

// ---- deferrals ----

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

// ---- the footprint ----

// prFootprint is what a pull request was when it shipped (when its first
// babysitting round was sent) and the footprint checks made since, kept
// with it as JSON.
type prFootprint struct {
	Ship   *footprintShip   `json:"ship,omitempty"`
	Checks []footprintCheck `json:"checks,omitempty"`
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
}

const (
	footprintAudited = "audit"
	footprintSkipped = "skipped"
	// footprintMaxChecks is how many footprint checks a pull request gets:
	// one after the rounds that spread, one more after the rounds that
	// undid it.
	footprintMaxChecks = 2
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

// footprintGrowth is how the head grew from ref: the files it touches now
// that ref didn't, and whether its size grew by half again (and by more
// than footprintMinLines).
func footprintGrowth(ref footprintAt, paths []string, lines int) (added []string, grew bool) {
	for _, p := range paths {
		if !slices.Contains(ref.Paths, p) {
			added = append(added, p)
		}
	}
	grew = lines*2 > ref.Lines*3 && lines-ref.Lines > footprintMinLines
	return added, grew
}

// footprintNow says whether the head needs a footprint check: rounds pushed
// since it shipped, and since the last check it spread to new files or grew
// by half again. Anything not known (its files, its size) fails open.
func (s *Service) footprintNow(ctx context.Context, b *Babysit, pr *PullRequest) (prFootprint, string, bool) {
	fp := s.footprintOf(ctx, pr.ID)
	if b == nil || fp.Ship == nil || !fp.Ship.Known || pr.HeadSHA == "" {
		return fp, "", false
	}
	// Growth is measured from the last check that ran; a skipped one only
	// marks its head as seen, so the growth since shipping isn't forgotten.
	ref, audited, seen := fp.Ship.footprintAt, 0, fp.Ship.SHA
	for _, c := range fp.Checks {
		seen = c.SHA
		if c.Outcome == footprintAudited {
			ref = c.footprintAt
			audited++
		}
	}
	if audited >= footprintMaxChecks || sameSHA(seen, pr.HeadSHA) {
		return fp, "", false
	}
	if !slices.ContainsFunc(b.Rounds, func(r BabysitRound) bool { return r.Pushed && !r.At.Before(fp.Ship.At) }) {
		return fp, "", false // not the rounds' doing
	}
	s.loadPRFiles(ctx, pr)
	lines, known := pr.changedLines()
	if !pr.filesKnown || !known {
		return fp, "", false
	}
	added, grew := footprintGrowth(ref, pr.files, lines)
	var why []string
	if len(added) > 0 {
		why = append(why, fmt.Sprintf("%d new file%s", len(added), plural(len(added))))
	}
	if grew {
		why = append(why, fmt.Sprintf("%d → %d lines changed", ref.Lines, lines))
	}
	if len(why) == 0 {
		return fp, "", false
	}
	return fp, strings.Join(why, ", "), true
}

// footprintDue: a readiness reason while the check is owed and not asked
// for yet ("" when none). A babysit that can't be read is an error, not
// "nothing due".
func (s *Service) footprintDue(ctx context.Context, pr *PullRequest) (string, error) {
	b, err := s.activeBabysit(ctx, s.db.R, pr.ID)
	if err != nil || b == nil {
		return "", err
	}
	if _, why, due := s.footprintNow(ctx, b, pr); due {
		return "footprint check due: its fix-up rounds grew it since it shipped (" + why + ")", nil
	}
	return "", nil
}

// footprintStep asks for the footprint check the head needs, if any, and
// says whether it did (the babysit then waits for it). A check that can't
// be asked for is recorded as skipped: it fails open.
func (s *Service) footprintStep(ctx context.Context, b *Babysit, pr *PullRequest) bool {
	fp, why, due := s.footprintNow(ctx, b, pr)
	if !due {
		return false
	}
	lines, _ := pr.changedLines()
	check := footprintCheck{footprintAt: footprintAt{SHA: pr.HeadSHA, Paths: pr.files, Lines: lines}, At: s.now()}
	var a *CodeAudit
	err := errors.New("no member's runner to run it on")
	if owner := s.babysitOwnerFor(ctx, pr); owner != 0 {
		// On the default chain, like every audit nobody named models for
		// (the gateway's, WP-ADR84): a scope check reads a few
		// round diffs against the task and needs no Opus run (WP-867).
		a, err = s.RequestAudit(ctx, SystemActor(), AuditInput{PRID: pr.ID, Via: "system", owner: owner,
			requestKey: fmt.Sprintf("pr-footprint:%d:%s", pr.ID, strings.ToLower(pr.HeadSHA)), purpose: AuditFootprint,
			Instructions: footprintInstructions, prContext: s.footprintContext(ctx, b, pr, fp, why)})
	}
	if err != nil {
		check.Outcome, check.Why = footprintSkipped, PublicError(err)
		fp.Checks = append(fp.Checks, check)
		s.saveFootprint(ctx, pr.ID, fp)
		s.log.Info("footprint check skipped", "repo", pr.Repo, "number", pr.Number, "err", err)
		return false
	}
	check.Outcome, check.Audit = footprintAudited, a.ID
	fp.Checks = append(fp.Checks, check)
	s.saveFootprint(ctx, pr.ID, fp)
	s.setState(ctx, b, pr, BabysitWatching, fmt.Sprintf("footprint check #%d of what its rounds added (%s)", a.ID, why))
	return true
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
	lines, _ := pr.changedLines()
	fmt.Fprintf(&c, "This is pull request %s#%d, <untrusted>%s</untrusted>. It shipped at %s touching %d file%s (%d lines changed); babysitting rounds have changed it since, and at its head %s it touches %d file%s (%d lines): %s.\n",
		pr.Repo, pr.Number, untrusted(pr.Title), short(fp.Ship.SHA), len(fp.Ship.Paths), plural(len(fp.Ship.Paths)), fp.Ship.Lines,
		short(pr.HeadSHA), len(pr.files), plural(len(pr.files)), lines, why)
	added, _ := footprintGrowth(fp.Ship.footprintAt, pr.files, lines)
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
