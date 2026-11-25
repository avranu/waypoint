package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"git.jmann.me/claude/waypoint/internal/core/udiff"
	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// PR events; they reach automations, live updates and webhooks.
const (
	EvPRUpdated = "pr.updated"
	EvPRCommand = "pr.command"
	EvBotRun    = "bot.run"
	// EvPRActivity: a review or comment joined a pull request's history.
	EvPRActivity = "pr.activity"
)

// PullRequest is a pull (merge) request of an attached repo.
type PullRequest struct {
	ID         int64     `json:"id"`
	ForgeID    int64     `json:"forge_id"`
	Repo       string    `json:"repo"`
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	Author     string    `json:"author"`
	URL        string    `json:"url"`
	BaseRef    string    `json:"base_ref"`
	HeadRef    string    `json:"head_ref"`
	HeadSHA    string    `json:"head_sha"`
	State      string    `json:"state"` // open, draft, closed, merged
	Labels     []string  `json:"labels"`
	BotsPaused bool      `json:"bots_paused"`
	UpdatedAt  time.Time `json:"updated_at"`
	// Its life on the forge (WP-D15).
	OpenedAt     *time.Time `json:"opened_at"`
	MergedAt     *time.Time `json:"merged_at"`
	ClosedAt     *time.Time `json:"closed_at"`
	MergedBy     string     `json:"merged_by,omitempty"`
	Agent        string     `json:"agent,omitempty"` // the coding agent its text names
	Additions    *int       `json:"additions"`
	Deletions    *int       `json:"deletions"`
	ChangedFiles *int       `json:"changed_files"`
	Commits      *int       `json:"commits"`
	// Where it stands for merging (WP-D21): the forge's word on conflicts,
	// the review gate as last posted, CI on the head, and merge when ready.
	Mergeable  *bool      `json:"mergeable"`
	GateState  string     `json:"gate_state,omitempty"`
	CIState    string     `json:"ci_state,omitempty"` // pending, passed, failed, none
	ArmedAt    *time.Time `json:"armed_at,omitempty"`
	ArmedAgent string     `json:"armed_agent,omitempty"`
	ArmedStyle string     `json:"armed_style,omitempty"`
	ArmedLabel bool       `json:"armed_label,omitempty"`
	// Outside: from someone who isn't a Waypoint person, waiting for a
	// person to approve it (WP-281); agents and bots leave it alone.
	Outside        bool `json:"outside,omitempty"`
	forgeAuthor    string
	labelOff       bool
	armedNote      string
	armedBy        *int64
	gateNote       string
	gateSHA        string
	gateOpen       int
	ciSHA          string
	ciAnnouncedSHA string
	files          []string
	filesKnown     bool
	tooLarge       bool
	headRepo       string
	headAt         *time.Time
	armDeclined    int
	carriedSHA     string
	carriedFromSHA string
	carriedAt      *time.Time
	mergeReadAt    *time.Time
	mergeSince     *time.Time
	mergeRetryN    int
	mergeRetrySHA  string
	// Draft-first (WP-158): Waypoint holds it as a draft and marks it ready;
	// when, at which head, and the status id and checks that head had then.
	WipAuto       bool       `json:"wip_auto,omitempty"`
	ReadyAt       *time.Time `json:"ready_at,omitempty"`
	readySHA      string
	readyStatusID int64
	readyChecks   []string
	readySkipped  []string
	// The merge queue (WP-222): when it joined its base's queue.
	QueuedAt *time.Time `json:"queued_at,omitempty"`
	// MergeSHA is the commit it became when merged: a squash or merge commit
	// is what deployments of its base run (WP-23, WP-D19).
	MergeSHA string `json:"merge_sha,omitempty"`
}

const prCols = `id, forge_id, repo, number, title, author, url, base_ref, head_ref, head_sha, state, labels, bots_paused, updated_at,
	opened_at, merged_at, closed_at, merged_by, agent, additions, deletions, changed_files, commits,
	mergeable, gate_state, gate_note, gate_sha, gate_open, ci_state, ci_sha, ci_announced_sha, armed_at, armed_by, armed_agent, armed_style, armed_label, armed_note,
	head_repo, head_at, arm_declined, mergeable_read_at, mergeable_since, merge_retry_n, merge_retry_sha,
	wip_auto, ready_at, ready_sha, ready_status_id, ready_checks, queued_at, outside, forge_author, merge_sha, ready_skipped, carried_sha, carried_from_sha, carried_at`

func scanPR(sc interface{ Scan(...any) error }) (*PullRequest, error) {
	var p PullRequest
	var labels string
	var up int64
	var opened, merged, closed, adds, dels, files, commits, mergeable, armedAt, armedBy, headAt, readAt, since, carriedAt sql.NullInt64
	var armedLabel, wipAuto int
	var readyAt, queuedAt sql.NullInt64
	var readyChecks, readySkipped string
	if err := sc.Scan(&p.ID, &p.ForgeID, &p.Repo, &p.Number, &p.Title, &p.Author, &p.URL, &p.BaseRef, &p.HeadRef,
		&p.HeadSHA, &p.State, &labels, &p.BotsPaused, &up,
		&opened, &merged, &closed, &p.MergedBy, &p.Agent, &adds, &dels, &files, &commits,
		&mergeable, &p.GateState, &p.gateNote, &p.gateSHA, &p.gateOpen, &p.CIState, &p.ciSHA, &p.ciAnnouncedSHA,
		&armedAt, &armedBy, &p.ArmedAgent, &p.ArmedStyle, &armedLabel, &p.armedNote,
		&p.headRepo, &headAt, &p.armDeclined, &readAt, &since, &p.mergeRetryN, &p.mergeRetrySHA,
		&wipAuto, &readyAt, &p.readySHA, &p.readyStatusID, &readyChecks, &queuedAt, &p.Outside, &p.forgeAuthor, &p.MergeSHA, &readySkipped, &p.carriedSHA, &p.carriedFromSHA, &carriedAt); err != nil {
		return nil, err
	}
	p.ArmedLabel, p.labelOff = armedLabel == 1, armedLabel == 2
	p.WipAuto, p.ReadyAt, p.QueuedAt = wipAuto == 1, ptrTime(readyAt), ptrTime(queuedAt)
	_ = json.Unmarshal([]byte(readyChecks), &p.readyChecks)
	_ = json.Unmarshal([]byte(readySkipped), &p.readySkipped)
	if mergeable.Valid {
		p.Mergeable = ptr(mergeable.Int64 != 0)
	}
	p.ArmedAt, p.armedBy, p.headAt = ptrTime(armedAt), ptrInt(armedBy), ptrTime(headAt)
	p.mergeReadAt, p.mergeSince, p.carriedAt = ptrTime(readAt), ptrTime(since), ptrTime(carriedAt)
	_ = json.Unmarshal([]byte(labels), &p.Labels)
	if p.Labels == nil {
		p.Labels = []string{}
	}
	p.UpdatedAt = msTime(up)
	p.OpenedAt, p.MergedAt, p.ClosedAt = ptrTime(opened), ptrTime(merged), ptrTime(closed)
	p.Additions, p.Deletions, p.ChangedFiles, p.Commits = ptrIntN(adds), ptrIntN(dels), ptrIntN(files), ptrIntN(commits)
	return &p, nil
}

func ptrIntN(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

// prPageURL is Waypoint's page for a pull request, where every finding kept
// on it is shown; empty where Waypoint has no address (WP-1421).
func prPageURL(base string, prID int64) string {
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/agents?tab=pulls&pr=%d", base, prID)
}

func (s *Service) prByID(ctx context.Context, q queryer, id int64) (*PullRequest, error) {
	p, err := scanPR(q.QueryRowContext(ctx, `SELECT `+prCols+` FROM pull_requests WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("pull request")
	}
	return p, err
}

func (s *Service) prByNumber(ctx context.Context, q queryer, forgeID int64, repo string, number int) (*PullRequest, error) {
	p, err := scanPR(q.QueryRowContext(ctx, `SELECT `+prCols+` FROM pull_requests WHERE forge_id = ? AND repo = ? AND number = ?`, forgeID, repo, number))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// botProjects are the projects with repo attached and bots on there.
func (s *Service) botProjects(ctx context.Context, forgeID int64, repo string) ([]int64, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT project_id FROM project_repos WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND bots = 1 ORDER BY project_id`, forgeID, repo)
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

// mirrorAuthorRE: a mirror opens a Forgejo copy of a GitHub pull request
// and names who opened the original in its first line.
var mirrorAuthorRE = lazyre.New(`^_Opened on GitHub by \*\*([^*]+)\*\* as `)

// effectiveAuthor is who wrote a pull request: for a mirror's copies of
// GitHub pull requests, the GitHub author.
func effectiveAuthor(author, body string) string {
	if m := mirrorAuthorRE.FindStringSubmatch(strings.TrimSpace(body)); m != nil {
		return m[1]
	}
	return author
}

// knownBots are accounts that aren't people, beyond GitHub's "[bot]" logins.
var knownBots = []string{"renovate", "dependabot", "github-actions", "forgejo-actions", "llm-review", "automerge", "release-please"}

// isBotLogin says whether an account is a bot.
func isBotLogin(login string) bool {
	l := strings.ToLower(login)
	if strings.HasSuffix(l, "[bot]") || strings.HasSuffix(l, "-bot") || strings.HasSuffix(l, "_bot") {
		return true
	}
	return slices.ContainsFunc(knownBots, func(b string) bool { return l == b || strings.HasPrefix(l, b+"-") })
}

// onForgePull records a pull request webhook and, when it brings a new
// head, readiness or a reopening, fires pr.updated in each project with bots
// on for the repo.
func (s *Service) onForgePull(ctx context.Context, f *Forge, repo string, fp *ForgePR) {
	var attached int
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM project_repos WHERE forge_id = ? AND repo = ? COLLATE NOCASE`, f.ID, repo).Scan(&attached); err != nil || attached == 0 {
		return
	}
	if fp.HeadSHA == "" || fp.Author == "" {
		if c, ok := s.Forges.(ForgePRs); ok {
			if full, err := c.GetPR(ctx, f, repo, fp.Number); err == nil {
				full.actor, full.MergedBy, full.requested = fp.actor, cmp(full.MergedBy, fp.MergedBy), fp.requested
				if full.OpenedAt == nil {
					full.OpenedAt = fp.OpenedAt
				}
				fp = full
			} else {
				s.log.WarnContext(ctx, "read pull request", "repo", repo, "number", fp.Number, "err", err)
			}
		}
	}
	pr, err := s.recordPR(ctx, f, repo, fp, true)
	if err != nil {
		s.log.WarnContext(ctx, "record pull request", "repo", repo, "number", fp.Number, "err", err)
		return
	}
	s.afterPRRecorded(ctx, f, pr)
}

// recordPR stores a pull request as its forge reports it, and what changed
// in its history; with fire, a change bots care about fires pr.updated.
// Without it, only a new head of a live pull request that had one does, so
// the bots hear of a push once, whoever reads it first (WP-129).
func (s *Service) recordPR(ctx context.Context, f *Forge, repo string, fp *ForgePR, fire bool) (*PullRequest, error) {
	if fp == nil || fp.Number <= 0 {
		return nil, fmt.Errorf("the forge answered with no pull request number for %s", repo)
	}
	var out *PullRequest
	var later []*Event
	var ended bool
	projects, err := s.botProjects(ctx, f.ID, repo)
	if err != nil {
		return nil, err
	}
	attached, err := s.attachedProject(ctx, s.db.R, f.ID, repo)
	if err != nil {
		return nil, err
	}
	var botLogin string
	if fire && len(fp.requested) > 0 && len(projects) > 0 {
		botLogin = s.forgeBotLogin(ctx, f)
	}
	voice := s.prVoice(ctx, f)
	// A push that only merged the base in carries the earlier review (WP-1808).
	// Decided here, from the diff, and applied in the transaction that records
	// the head, so no read sees the new head with its waits restarted and then
	// not.
	var carry *headCarry
	// A draft someone made ready by editing its title (not markReady): does
	// the edit start a run of the checks? Asked before the transaction.
	var readyEditRuns bool
	if fp.HeadSHA != "" && (fp.State == "open" || fp.State == "draft") {
		if prev, perr := s.prByNumber(ctx, s.db.R, f.ID, repo, fp.Number); perr == nil {
			if prev != nil && prev.State == "draft" && fp.State == "open" && prev.HeadSHA == fp.HeadSHA && !sameSHA(prev.readySHA, fp.HeadSHA) {
				var rerr error
				if readyEditRuns, rerr = s.ciRunsOnEdit(ctx, f, repo, fp.HeadSHA); rerr != nil {
					// A failed read is not "no run": hold it as if the edit runs
					// the checks (fullRunStalled lifts the hold where it does not).
					s.log.WarnContext(ctx, "read the workflows for a draft made ready by an edit", "repo", repo, "pr", fp.Number, "err", rerr)
					readyEditRuns = true
				}
			}
			carry = s.carryDecision(ctx, f, repo, prev, fp)
		}
	}
	err = s.write(ctx, SystemActor(), func(t *tx) error {
		old, err := s.prByNumber(ctx, t, f.ID, repo, fp.Number)
		if err != nil {
			return err
		}
		var headBefore bool
		if !fire && old != nil && old.HeadSHA != "" && fp.HeadSHA != "" && fp.HeadSHA != old.HeadSHA {
			var n int
			if err := t.QueryRowContext(ctx, `SELECT count(*) FROM pr_events WHERE pr_id = ? AND sha = ?`, old.ID, fp.HeadSHA).Scan(&n); err != nil {
				return err
			}
			headBefore = n > 0
		}
		labels := fp.Labels
		if labels == nil {
			labels = []string{}
		}
		lb, _ := json.Marshal(labels)
		author := effectiveAuthor(fp.Author, fp.Body)
		if fp.Author == "" || voice.outside(ctx, t, fp.Author) {
			author = fp.Author
		}
		now := s.nowMS()
		live := fp.State == "open" || fp.State == "draft"
		ended = !live && old != nil && (old.State == "open" || old.State == "draft")
		merged, closed := nullTime(fp.MergedAt), nullTime(fp.ClosedAt)
		if fp.State == "merged" && merged == nil && (old == nil || old.MergedAt == nil) {
			merged = now
		}
		if (fp.State == "merged" || fp.State == "closed") && closed == nil && (old == nil || old.ClosedAt == nil) {
			closed = now
		}
		agent := codingAgent(fp.Body, author)
		var headAt any
		if fp.HeadSHA != "" {
			headAt = now
		}
		_, err = t.ExecContext(ctx, `INSERT INTO pull_requests(forge_id, repo, number, title, author, url, base_ref, head_ref, head_sha, state, labels, created_at, updated_at,
				opened_at, merged_at, closed_at, merged_by, agent, additions, deletions, changed_files, commits, head_repo, head_at, forge_author, merge_sha)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?, coalesce(?, ?),?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(forge_id, repo, number) DO UPDATE SET title = excluded.title, author = CASE WHEN excluded.author = '' THEN author ELSE excluded.author END,
				forge_author = CASE WHEN excluded.forge_author = '' THEN forge_author ELSE excluded.forge_author END,
				url = excluded.url, base_ref = excluded.base_ref, head_ref = excluded.head_ref,
				queued_at = CASE WHEN excluded.base_ref NOT IN ('', base_ref) AND base_ref != '' THEN NULL ELSE queued_at END,
				head_sha = CASE WHEN excluded.head_sha = '' THEN head_sha ELSE excluded.head_sha END,
				head_repo = CASE WHEN excluded.head_repo = '' THEN head_repo ELSE excluded.head_repo END,
				head_at = CASE WHEN excluded.head_sha != '' AND (excluded.head_sha != head_sha OR head_at IS NULL) THEN excluded.head_at ELSE head_at END,
				carried_sha = CASE WHEN excluded.head_sha != '' AND excluded.head_sha != head_sha THEN '' ELSE carried_sha END,
				carried_from_sha = CASE WHEN excluded.head_sha != '' AND excluded.head_sha != head_sha THEN '' ELSE carried_from_sha END,
				carried_at = CASE WHEN excluded.head_sha != '' AND excluded.head_sha != head_sha THEN NULL ELSE carried_at END,
				arm_declined = CASE WHEN arm_declined = 2 AND excluded.state = 'open' AND ((head_sha != '' AND excluded.head_sha NOT IN ('', head_sha)) OR state != 'open')
					THEN 0 ELSE arm_declined END,
				state = excluded.state, labels = excluded.labels, updated_at = excluded.updated_at,
				opened_at = coalesce(?, opened_at),
				merged_at = CASE WHEN ? THEN NULL ELSE coalesce(excluded.merged_at, merged_at) END,
				closed_at = CASE WHEN ? THEN NULL ELSE coalesce(excluded.closed_at, closed_at) END,
				merged_by = CASE WHEN excluded.merged_by = '' THEN merged_by ELSE excluded.merged_by END,
				merge_sha = CASE WHEN ? THEN '' WHEN excluded.merge_sha = '' THEN merge_sha ELSE excluded.merge_sha END,
				agent = CASE WHEN excluded.agent = '' THEN agent ELSE excluded.agent END,
				additions = coalesce(excluded.additions, additions), deletions = coalesce(excluded.deletions, deletions),
				changed_files = coalesce(excluded.changed_files, changed_files), commits = coalesce(excluded.commits, commits)`,
			f.ID, repo, fp.Number, excerpt(stripWIP(fp.Title), 300), author, fp.URL, fp.BaseRef, fp.HeadRef, fp.HeadSHA, fp.State, string(lb), now, now,
			nullTime(fp.OpenedAt), now, merged, closed, fp.MergedBy, agent,
			nullIntP(fp.Additions), nullIntP(fp.Deletions), nullIntP(fp.ChangedFiles), nullIntP(fp.Commits), headRepoOf(fp, repo), headAt, fp.Author, fp.MergeSHA,
			nullTime(fp.OpenedAt), live, live, live)
		if err != nil {
			return err
		}
		pr, err := s.prByNumber(ctx, t, f.ID, repo, fp.Number)
		if err != nil || pr == nil {
			return cmpErr(err, notFound("pull request"))
		}
		if carry != nil && old != nil && old.HeadSHA == carry.from && pr.HeadSHA == fp.HeadSHA {
			if err := s.applyCarry(ctx, t, pr.ID, pr.HeadSHA, carry); err != nil {
				return err
			}
			pr.carriedSHA, pr.carriedFromSHA, pr.carriedAt = pr.HeadSHA, carry.contentSHA, ptrTime(sql.NullInt64{Int64: carry.contentAt, Valid: true})
		}
		if err := s.screenPull(ctx, t, voice, pr, fp); err != nil {
			return err
		}
		if fp.wipAuto || s.ownDraftComing(f.ID, repo, fp) {
			if _, err := t.ExecContext(ctx, `UPDATE pull_requests SET wip_auto = 1 WHERE id = ?`, pr.ID); err != nil {
				return err
			}
			pr.WipAuto = true
		}
		if readyEditRuns && old != nil && old.State == "draft" && pr.State == "open" && old.HeadSHA == pr.HeadSHA && !sameSHA(pr.readySHA, pr.HeadSHA) {
			if err := s.markReadyByEdit(ctx, t, pr, now); err != nil {
				return err
			}
		}
		headChanged := fp.HeadSHA != "" && (old == nil || old.HeadSHA != fp.HeadSHA)
		if fp.mergeMute {
			if err := forgetMergeable(ctx, t, pr.ID); err != nil {
				return err
			}
			pr.Mergeable = nil
		} else if err := s.noteMergeable(ctx, t, pr, headChanged, fp.Mergeable); err != nil {
			return err
		}
		if fp.Mergeable != nil {
			pr.Mergeable = fp.Mergeable
		}
		if err := s.armByLabel(ctx, t, pr, live); err != nil {
			return err
		}
		out = pr
		s.recordPRMentions(ctx, t, f.ID, repo, pr.ID, stripWIP(fp.Title)+"\n"+fp.Body)
		history := prHistory(old, pr, fp.actor, now)
		if err := s.savePREvents(ctx, t, pr, history, attached, old != nil); err != nil {
			return err
		}
		if ended && fp.State == "merged" && attached != 0 {
			t.emit(&Event{Kind: EvPRActivity, ProjectID: attached, New: "merged", quiet: true, stackParent: pr.ID, Data: map[string]any{"pr_id": pr.ID}})
		}
		if pr.Outside {
			return nil
		}
		if len(history) > 0 && attached != 0 {
			t.emit(&Event{Kind: EvPRActivity, ProjectID: attached, New: history[len(history)-1].kind, Data: map[string]any{"pr_id": pr.ID}})
		}
		if fire && live {
			for _, l := range addedLabels(old, pr) {
				for _, pid := range projects {
					t.emit(&Event{Kind: EvPRLabeled, ProjectID: pid, New: l, Data: prEventData(pr, map[string]any{"label": l})})
				}
			}
			for _, who := range fp.requested {
				bot := botLogin != "" && strings.EqualFold(who, botLogin)
				for _, pid := range projects {
					t.emit(&Event{Kind: EvPRReviewRequested, ProjectID: pid, New: who, Data: prEventData(pr, map[string]any{"reviewer": who, "bot": bot})})
				}
			}
		}
		reason := prChange(old, pr)
		if reason == "" && old != nil && old.Outside && live && pr.HeadSHA != "" {
			reason = "opened"
		}
		if !fire {
			if reason == "pushed" && !headBefore {
				for _, pid := range projects {
					later = append(later, &Event{Kind: EvPRUpdated, ProjectID: pid, New: reason, Data: prEventData(pr, map[string]any{"reason": reason})})
				}
			}
			return nil
		}
		if reason == "" {
			return nil
		}
		for _, pid := range projects {
			t.emit(&Event{Kind: EvPRUpdated, ProjectID: pid, New: reason, Data: prEventData(pr, map[string]any{"reason": reason})})
		}
		return nil
	})
	if err == nil && len(later) > 0 {
		at := s.now().UTC()
		for _, e := range later {
			e.At = at
		}
		bg := withoutLineLocks(context.WithoutCancel(ctx))
		s.goBG(func() { s.dispatch(bg, SystemActor(), later) })
	}
	if err == nil && ended && out != nil {
		bg, merged := withoutLineLocks(context.WithoutCancel(ctx)), out
		s.goBG(func() { s.nudgeMergeWaiters(bg, merged) })
	}
	return out, err
}

func cmpErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// prChange names what changed that bots care about: opened, pushed, ready
// or reopened; "" for anything else (an edited title, a label, a repeat
// delivery).
func prChange(old, cur *PullRequest) string {
	live := cur.State == "open" || cur.State == "draft"
	switch {
	case !live || cur.HeadSHA == "":
		return ""
	case old == nil, old.HeadSHA == "":
		return "opened"
	case old.State == "closed" || old.State == "merged":
		return "reopened"
	case old.HeadSHA != cur.HeadSHA:
		return "pushed"
	case old.State == "draft" && cur.State == "open":
		return "ready"
	}
	return ""
}

func prEventData(pr *PullRequest, extra map[string]any) map[string]any {
	d := map[string]any{"pr_id": pr.ID, "repo": pr.Repo, "number": pr.Number, "title": pr.Title, "url": pr.URL, "head_sha": pr.HeadSHA, "author": pr.Author}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

// botCommandRE finds "@waypoint review" and friends at the start of a line.
var botCommandRE = lazyre.New(`(?im)^\s*@waypoint\s+(full review|review|check|pause|resume|help)\b`)

const botHelp = `Waypoint's bots answer these, each at the start of a line of a comment:

- ` + "`@waypoint review`" + `: review this pull request now
- ` + "`@waypoint full review`" + `: review it again from scratch, even at a head already reviewed
- ` + "`@waypoint check`" + `: look at the threads and checks again now and update the ` + "`waypoint/review`" + ` status
- ` + "`@waypoint pause`" + ` / ` + "`@waypoint resume`" + `: stop or restart automatic reviews here
- ` + "`@waypoint help`" + `: this list

What runs, and with which models, is set in the project's automations in Waypoint.`

// onPRComment acts on a new conversation comment on a pull request.
func (s *Service) onPRComment(ctx context.Context, f *Forge, repo string, number int, c ForgeComment) {
	m := botCommandRE.FindStringSubmatch(c.Body)
	if m == nil || strings.Contains(c.Body, "<!-- waypoint") {
		return
	}
	if s.prVoice(ctx, f).outside(ctx, s.db.R, c.Author) {
		return
	}
	projects, err := s.botProjects(ctx, f.ID, repo)
	if err != nil || len(projects) == 0 {
		return
	}
	cl, ok := s.Forges.(ForgePRs)
	if !ok {
		return
	}
	pr, err := s.prByNumber(ctx, s.db.R, f.ID, repo, number)
	if err == nil && (pr == nil || pr.HeadSHA == "") {
		var fp *ForgePR
		if fp, err = cl.GetPR(ctx, f, repo, number); err == nil {
			pr, err = s.recordPR(ctx, f, repo, fp, false)
		}
	}
	if err != nil || pr == nil {
		s.log.WarnContext(ctx, "pull request command", "repo", repo, "number", number, "err", err)
		return
	}
	bf := botForge(f)
	cmd := strings.ToLower(strings.Join(strings.Fields(m[1]), " "))
	switch cmd {
	case "pause", "resume":
		if _, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET bots_paused = ? WHERE id = ?`, boolInt(cmd == "pause"), pr.ID); err != nil {
			s.log.WarnContext(ctx, "pause bots", "err", err)
			return
		}
		_, _ = cl.React(ctx, bf, repo, number, c.ID, "+1")
	case "help":
		_, _ = cl.PostPRComment(ctx, bf, repo, number, botHelp+"\n<!-- waypoint help -->")
	case "check":
		_, _ = cl.React(ctx, bf, repo, number, c.ID, "eyes")
		s.checkPR(ctx, pr.ID, "command")
		s.refreshPRChecks(ctx, pr.ID)
		_, _ = cl.React(ctx, bf, repo, number, c.ID, "+1")
	default:
		err := s.write(ctx, SystemActor(), func(t *tx) error {
			for _, pid := range projects {
				t.emit(&Event{Kind: EvPRCommand, ProjectID: pid, New: cmd,
					Data: prEventData(pr, map[string]any{"command": cmd, "comment_id": c.ID, "by": c.Author})})
			}
			return nil
		})
		if err != nil {
			s.log.WarnContext(ctx, "pull request command", "err", err)
		}
	}
}

// botForge is f as the bot account: its own token when one is set.
func botForge(f *Forge) *Forge {
	if f.botToken == "" {
		return f
	}
	c := *f
	c.token = f.botToken
	return &c
}

// isPREvent: the event is about a pull request rather than an issue.
func isPREvent(kind string) bool {
	return kind == EvPRUpdated || kind == EvPRCommand || kind == EvPRComment
}

// eventHasPR: automations get the pull request the event is about as .PR (the
// bots' events, and the CI, review gate and review events of WP-D21).
func eventHasPR(kind string) bool {
	return isPREvent(kind) || kind == EvPRCI || kind == EvPRReviewGate || kind == EvPRReview || kind == EvPRLabeled || kind == EvPRReviewRequested
}

// eventPR loads the pull request a PR event is about.
func (s *Service) eventPR(ctx context.Context, e *Event) (*PullRequest, error) {
	id := dataInt(e, "pr_id")
	if id == 0 {
		return nil, notFound("pull request")
	}
	pr, err := s.prByID(ctx, s.db.R, id)
	if err == nil && pr.Outside {
		return nil, errPROutside(pr)
	}
	return pr, err
}

// BotRun is one job a bot did or will do on a pull request.
type BotRun struct {
	ID           int64          `json:"id"`
	ProjectID    int64          `json:"project_id"`
	AutomationID *int64         `json:"automation_id"`
	Automation   string         `json:"automation,omitempty"`
	PR           *PullRequest   `json:"pr"`
	Kind         string         `json:"kind"`
	HeadSHA      string         `json:"head_sha"`
	Reason       string         `json:"reason"`
	Status       string         `json:"status"`
	Attempts     int            `json:"attempts"`
	Summary      string         `json:"summary"`
	Error        string         `json:"error,omitempty"`
	Log          []ModelAttempt `json:"log"`
	ReviewURL    string         `json:"review_url,omitempty"`
	Findings     []BotFinding   `json:"findings,omitempty"`
	StartedAt    *time.Time     `json:"started_at"`
	FinishedAt   *time.Time     `json:"finished_at"`
	CreatedAt    time.Time      `json:"created_at"`
	// What the run was made of (WP-363): empty on runs before it.
	PromptVersion string `json:"prompt_version,omitempty"`
	Strategy      string `json:"strategy,omitempty"`
	Arm           string `json:"arm,omitempty"` // the first reviewer's arm (WP-365), e.g. "diff/none"
	Verified      bool   `json:"verified"`
	options       string
	commentID     string
	notBefore     int64
}

// BotFinding is a finding a run posted (or held back).
type BotFinding struct {
	ID        int64  `json:"id"`
	Path      string `json:"path"`
	Line      *int   `json:"line"`
	Severity  string `json:"severity"`
	Claim     string `json:"claim"`
	Model     string `json:"model"`
	Placement string `json:"placement"`
	State     string `json:"state"`              // open, fixed, withdrawn, dismissed, resolved (WP-D21); dropped: left out before posting (WP-363)
	HeadSHA   string `json:"head_sha,omitempty"` // the head it was raised at
	// Private: on a path a mirror keeps off the other forge.
	Private bool `json:"private,omitempty"`
	// Agreed: how many reviewers raised it; VerifiedBy: the model that
	// checked it against the code before it was posted (WP-D14).
	Agreed     int    `json:"agreed"`
	VerifiedBy string `json:"verified_by,omitempty"`
	// What it came from and how the verifier judged it (WP-363): the
	// prompt version and context strategy of its run; verify is kept,
	// corrected, dropped, unchecked or skipped.
	PromptVersion string         `json:"prompt_version,omitempty"`
	Strategy      string         `json:"strategy,omitempty"`
	Verify        string         `json:"verify,omitempty"`
	VerifyReason  string         `json:"verify_reason,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	History       []FindingEvent `json:"history,omitempty"`
}

// reviewOptions are a review action's settings, kept with its runs.
type reviewOptions struct {
	Models      []ChainModel `json:"models,omitempty"`
	Focus       string       `json:"focus,omitempty"`
	Force       bool         `json:"force,omitempty"`
	MaxFindings int          `json:"max_findings,omitempty"`
	// Reviewers are more chains reviewing beside Models, in parallel;
	// Verifier checks each new finding before it is posted (else Admin →
	// AI's verify models), unless NoVerify (WP-D14).
	Reviewers [][]ChainModel `json:"reviewers,omitempty"`
	Verifier  []ChainModel   `json:"verifier,omitempty"`
	NoVerify  bool           `json:"no_verify,omitempty"`
	// A reply run answers one person's comment on one finding (WP-D21).
	FindingID int64 `json:"finding_id,omitempty"`
	// MinSeverity (critical … low, P0 … P3): findings below it aren't
	// posted; SkipFiles: globs left out beside noisePaths (WP-D14).
	MinSeverity string   `json:"min_severity,omitempty"`
	SkipFiles   []string `json:"skip_files,omitempty"`
}

const runCols = `r.id, r.project_id, r.automation_id, COALESCE(a.name, ''), r.pr_id, r.kind, r.head_sha, r.reason, r.status, r.attempts,
	r.summary, r.error, r.log, r.review_url, r.started_at, r.finished_at, r.created_at, r.options, r.comment_id, r.not_before,
	r.prompt_version, r.strategy, r.arm, r.verified`

func (s *Service) scanRun(ctx context.Context, sc interface{ Scan(...any) error }) (*BotRun, error) {
	var r BotRun
	var auto, started, finished sql.NullInt64
	var prID, created int64
	var log string
	if err := sc.Scan(&r.ID, &r.ProjectID, &auto, &r.Automation, &prID, &r.Kind, &r.HeadSHA, &r.Reason, &r.Status, &r.Attempts,
		&r.Summary, &r.Error, &log, &r.ReviewURL, &started, &finished, &created, &r.options, &r.commentID, &r.notBefore,
		&r.PromptVersion, &r.Strategy, &r.Arm, &r.Verified); err != nil {
		return nil, err
	}
	r.AutomationID, r.StartedAt, r.FinishedAt, r.CreatedAt = ptrInt(auto), ptrTime(started), ptrTime(finished), msTime(created)
	_ = json.Unmarshal([]byte(log), &r.Log)
	if r.Log == nil {
		r.Log = []ModelAttempt{}
	}
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return nil, err
	}
	r.PR = pr
	return &r, nil
}

func (s *Service) botRun(ctx context.Context, id int64) (*BotRun, error) {
	r, err := s.scanRun(ctx, s.db.R.QueryRowContext(ctx, `SELECT `+runCols+` FROM bot_runs r LEFT JOIN automations a ON a.id = r.automation_id WHERE r.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("bot run")
	} else if err != nil {
		return nil, err
	}
	r.Findings, err = s.runFindings(ctx, id)
	return r, err
}

// ListBotRuns lists a project's recent bot runs, newest first.
func (s *Service) ListBotRuns(ctx context.Context, actor *Actor, projectRef string, limit int) ([]*BotRun, error) {
	return s.ListBotRunsFiltered(ctx, actor, projectRef, BotRunFilter{Limit: limit})
}

// BotRunFilter narrows a project's bot runs. Status is live (queued or
// running), failed, done or "" for all; Automation is a rule's id (0 for
// any); Before is the id to page from (0 for the newest).
type BotRunFilter struct {
	Status     string
	Automation int64
	Before     int64
	Limit      int
}

// ListBotRunsFiltered is ListBotRuns with a filter and paging.
func (s *Service) ListBotRunsFiltered(ctx context.Context, actor *Actor, projectRef string, f BotRunFilter) ([]*BotRun, error) {
	limit := f.Limit
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	status := f.Status
	if status == "all" {
		status = ""
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+runCols+` FROM bot_runs r LEFT JOIN automations a ON a.id = r.automation_id
		WHERE r.project_id = ?
		AND (? = '' OR (? = 'live' AND r.status IN ('queued','running')) OR r.status = ?)
		AND (? = 0 OR r.automation_id = ?) AND (? = 0 OR r.id < ?)
		AND (? = 0 OR NOT EXISTS (SELECT 1 FROM pull_requests p WHERE p.id = r.pr_id AND p.outside = 1))
		ORDER BY r.id DESC LIMIT ?`, p.ID, status, status, status, f.Automation, f.Automation, f.Before, f.Before, actor.HidesDrafts(), limit)
	if err != nil {
		return nil, err
	}
	var out []*BotRun
	for rows.Next() {
		r, err := s.scanRun(ctx, rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range out {
		if r.Findings, err = s.runFindings(ctx, r.ID); err != nil {
			return nil, err
		}
	}
	if out == nil {
		out = []*BotRun{}
	}
	return out, nil
}

func (s *Service) runFindings(ctx context.Context, runID int64) ([]BotFinding, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+findingCols+` FROM bot_findings f WHERE f.run_id = ? ORDER BY f.id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BotFinding
	for rows.Next() {
		f, err := scanFindingRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f.BotFinding)
	}
	return out, rows.Err()
}

// botRunToAct loads a run the actor may cancel or retry: an editor's, and
// for agents not one on a pull request from outside (WP-281).
func (s *Service) botRunToAct(ctx context.Context, actor *Actor, id int64) (*BotRun, error) {
	r, err := s.botRun(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, r.ProjectID, RoleEditor); err != nil {
		return nil, err
	}
	if actor.HidesDrafts() && r.PR.Outside {
		return nil, notFound("bot run")
	}
	return r, nil
}

// CancelBotRun stops a queued or running run. Agents can't cancel the
// review of an open pull request's head (WP-217): a cancelled review counts
// as the rule having acted and leaves the gate green, so the pull request
// would merge unreviewed.
func (s *Service) CancelBotRun(ctx context.Context, actor *Actor, id int64) (*BotRun, error) {
	r, err := s.botRunToAct(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	return s.cancelRun(ctx, actor, r)
}

// cancelRun cancels r, read earlier: if it finished since, nothing is
// cancelled and the error says so.
func (s *Service) cancelRun(ctx context.Context, actor *Actor, r *BotRun) (*BotRun, error) {
	if r.Status != "queued" && r.Status != "running" {
		return nil, invalid("this run has already finished")
	}
	if actor.writesAsAgent() && r.Kind == "review" && sameSHA(r.HeadSHA, r.PR.HeadSHA) && (r.PR.State == "open" || r.PR.State == "draft") {
		return nil, forbidden("agents can't cancel the review of a pull request's head: a cancelled review counts as done, so it would merge unreviewed; a person can cancel it")
	}
	ended, err := s.endBotRun(ctx, r.ID, "cancelled", "Cancelled by "+actor.User.DisplayName+".", "", nil, "")
	if err != nil {
		return nil, err
	}
	if !ended {
		return nil, invalid("this run has already finished")
	}
	s.botMu.Lock()
	if cancel := s.botCancels[r.ID]; cancel != nil {
		cancel()
	}
	s.botMu.Unlock()
	s.audit(ctx, actor, botRunAudit("bot.cancel", r, "cancelled the "+r.Kind+" at "+short(r.HeadSHA)))
	return s.botRun(ctx, r.ID)
}

// RetryBotRun queues a finished run again, at the pull request's current
// head, reviewing even if that head was reviewed.
func (s *Service) RetryBotRun(ctx context.Context, actor *Actor, id int64) (*BotRun, error) {
	r, err := s.botRunToAct(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if r.Status == "queued" || r.Status == "running" {
		return nil, invalid("this run hasn't finished")
	}
	var opts reviewOptions
	_ = json.Unmarshal([]byte(r.options), &opts)
	opts.Force = true
	newID, err := s.enqueueBotRun(ctx, r.ProjectID, r.AutomationID, r.PR, r.Kind, opts, "retried by "+actor.User.DisplayName, "")
	if err != nil {
		return nil, err
	}
	s.audit(ctx, actor, botRunAudit("bot.retry", r, "ran the "+r.Kind+" again at the current head"))
	return s.botRun(ctx, newID)
}

// enqueueBotRun queues a run, unless the same job is already waiting or
// running for this head.
func (s *Service) enqueueBotRun(ctx context.Context, projectID int64, automationID *int64, pr *PullRequest, kind string, opts reviewOptions, reason, commentID string) (int64, error) {
	ob, _ := json.Marshal(opts)
	var id int64
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		err := t.QueryRowContext(ctx, `SELECT id FROM bot_runs WHERE project_id = ? AND automation_id IS ? AND pr_id = ? AND kind = ? AND head_sha = ? AND status IN ('queued','running')
			AND (kind != 'reply' OR comment_id = ?)`,
			projectID, automationID, pr.ID, kind, pr.HeadSHA, commentID).Scan(&id)
		if err == nil {
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		res, err := t.ExecContext(ctx, `INSERT INTO bot_runs(project_id, automation_id, pr_id, kind, head_sha, options, reason, comment_id, created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
			projectID, automationID, pr.ID, kind, pr.HeadSHA, string(ob), reason, commentID, s.nowMS())
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		t.emit(&Event{Kind: EvBotRun, ProjectID: projectID, New: "queued", Data: prEventData(pr, map[string]any{"run_id": id, "bot_run": id})})
		return nil
	})
	if err == nil {
		if kind == "review" {
			s.skipStaleRuns(ctx)
		}
		s.kickBots()
		if kind == "review" {
			s.goBG(func() {
				if err := s.evalGate(context.WithoutCancel(ctx), pr.ID); err != nil {
					if readFault(err) && !errors.Is(err, ErrNotFound) && !isStatus(err, http.StatusNotFound) {
						s.log.WarnContext(ctx, "eval gate failed", "pr", pr.ID, "err", err)
					}
				}
			})
		}
	}
	return id, err
}

// finishRun records how a run ended.
func (s *Service) finishRun(ctx context.Context, id int64, status, summary, errText string, log []ModelAttempt, reviewURL string) error {
	_, err := s.endBotRun(ctx, id, status, summary, errText, log, reviewURL)
	return err
}

// endBotRun is finishRun, saying whether it ended the run: false when the run
// had finished already.
func (s *Service) endBotRun(ctx context.Context, id int64, status, summary, errText string, log []ModelAttempt, reviewURL string) (bool, error) {
	sets := `status = ?, summary = ?, error = ?, finished_at = ?`
	args := []any{status, summary, errText, s.nowMS()}
	if log != nil {
		lb, _ := json.Marshal(log)
		sets += `, log = ?`
		args = append(args, string(lb))
	}
	if reviewURL != "" {
		sets += `, review_url = ?`
		args = append(args, reviewURL)
	}
	var prID int64
	var kind string
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		var projectID int64
		args[3] = s.nowMS()
		// #nosec G202 -- fixed column names only; values are bound
		err := t.QueryRowContext(ctx, `UPDATE bot_runs SET `+sets+` WHERE id = ? AND status IN ('queued','running') RETURNING project_id, pr_id, kind`, append(args, id)...).Scan(&projectID, &prID, &kind)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		t.emit(&Event{Kind: EvBotRun, ProjectID: projectID, New: status, Data: map[string]any{"run_id": id, "bot_run": id}})
		if kind == "review" && (status == "done" || (status == "failed" && strings.HasPrefix(errText, providersDownText))) {
			s.reviewsDownCheck(ctx, t, projectID, prID, id)
		}
		return nil
	})
	if err == nil && (kind == "review" || kind == "reply") {
		if gerr := s.evalGate(context.WithoutCancel(ctx), prID); gerr != nil {
			s.log.WarnContext(ctx, "review gate", "pull request", prID, "err", gerr)
			s.markGatePending(context.WithoutCancel(ctx), prID)
		} else {
			s.maybeAutoMerge(context.WithoutCancel(ctx), prID)
		}
	}
	return err == nil && kind != "", err
}

// Bot run limits.
const (
	botWorkers     = 4
	botMaxAttempts = 3
	botRetryAfter  = 10 * time.Minute
	botRunTimeout  = 40 * time.Minute
)

func (s *Service) kickBots() {
	select {
	case s.botKick <- struct{}{}:
	default:
	}
}

// RunBots carries out queued bot runs until ctx ends. Runs a restart
// interrupted go back in the queue.
func (s *Service) RunBots(ctx context.Context) {
	ctx = WithLogSource(ctx, sourceBotRuns)
	if _, err := s.db.W.ExecContext(ctx, `UPDATE bot_runs SET status = 'queued' WHERE status = 'running'`); err != nil {
		s.log.ErrorContext(ctx, "requeue bot runs", "err", err)
	}
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	slots := make(chan struct{}, botWorkers)
	for {
		for {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			r, err := s.claimRun(ctx)
			if err != nil || r == nil {
				<-slots
				if err != nil && ctx.Err() == nil {
					s.log.ErrorContext(ctx, "claim bot run", "err", err)
				}
				break
			}
			s.goBG(func() {
				defer func() { <-slots; s.kickBots() }()
				s.carryOut(ctx, r)
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-s.botKick:
		case <-tick.C:
		}
	}
}

// claimRun takes the oldest queued run that may start now.
func (s *Service) claimRun(ctx context.Context) (*BotRun, error) {
	s.skipStaleRuns(ctx)
	now := s.nowMS()
	var id int64
	err := s.db.W.QueryRowContext(ctx, `UPDATE bot_runs SET status = 'running', started_at = ?, attempts = attempts + 1
		WHERE id = (SELECT id FROM bot_runs WHERE status = 'queued' AND not_before <= ? ORDER BY id LIMIT 1) RETURNING id`, now, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return s.botRun(ctx, id)
}

// skipStaleRuns ends queued runs that no longer have a point, without
// calling the forge: a run of a pull request that has merged or closed, and
// a review of a head the pull request has moved past. They would otherwise
// wait their turn behind slow runs, and hold the review gate while they do.
func (s *Service) skipStaleRuns(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT b.id, p.state, p.head_sha FROM bot_runs b JOIN pull_requests p ON p.id = b.pr_id
		WHERE b.status = 'queued' AND (p.state IN ('closed','merged') OR (b.kind = 'review' AND p.head_sha != '' AND b.head_sha != p.head_sha))`)
	if err != nil {
		s.log.WarnContext(ctx, "look for stale bot runs", "err", err)
		return
	}
	type stale struct {
		id  int64
		why string
	}
	var list []stale
	for rows.Next() {
		var id int64
		var state, head string
		if err := rows.Scan(&id, &state, &head); err != nil {
			break
		}
		why := "the pull request was " + state
		if state != "closed" && state != "merged" {
			why = "a newer push (" + short(head) + ") replaced this run's head"
		}
		list = append(list, stale{id, why})
	}
	_ = rows.Close()
	for _, x := range list {
		if err := s.finishRun(ctx, x.id, "skipped", x.why, "", nil, ""); err != nil {
			s.log.WarnContext(ctx, "skip stale bot run", "run", x.id, "err", err)
		}
	}
}

// errSkip ends a run without doing anything, with the reason as summary.
type errSkip struct{ why string }

func (e *errSkip) Error() string { return e.why }

// errProvidersDown: every model of a pass failed with a provider error, so
// the run ends instead of walking the chain again, later.
type errProvidersDown struct {
	noBar bool
}

func (e *errProvidersDown) Error() string {
	if e.noBar {
		return providersDownText + noBarText() + "; not retrying, ask for a review again later"
	}
	return providersDownText + "is unavailable (rate limited, down or timing out); not retrying, ask for a review again later"
}

// noBarText: what a run's error says when no model at the minimum answered.
func noBarText() string {
	return "was unavailable, and no model at the review minimum (" + reviewFloorBar + ") could take it"
}

// errAllModelsFailed: no model answered any part of the diff.
var errAllModelsFailed = errors.New("every model failed on every part of the diff")

// runErrorText is the error a failed run records: what the models did when
// that is why it failed (the log says each outcome), else the request's own
// public error. Counts and outcomes only; a provider's text stays in the log.
func runErrorText(err error, log []ModelAttempt) string {
	var down *errProvidersDown
	sum := attemptSummary(log)
	switch {
	case sum != "" && errors.As(err, &down) && down.noBar:
		return providersDownText + noBarText() + ": " + sum + "; not retrying, ask for a review again later"
	case sum != "" && errors.As(err, &down):
		return providersDownText + "was unavailable: " + sum + "; not retrying, ask for a review again later"
	case errors.As(err, &down):
		return down.Error()
	case sum != "" && (errors.Is(err, errAllModelsFailed) || strings.HasPrefix(err.Error(), "every model failed")):
		return "every model failed: " + sum
	}
	return PublicError(err)
}

// quotaWait: no reviewer answered because the AI key's daily limit is spent.
func quotaWait(answers []reviewerAnswer) *aiWaitError {
	for _, a := range answers {
		var w *aiWaitError
		if a.err != nil && errors.As(a.err, &w) {
			return w
		}
	}
	return nil
}

// providersDown: no reviewer answered, and each attempt failed on the
// provider's side, or the gateway had no model at the review minimum
// (no_model_at_bar: it says retrying cannot help). A bad answer or an auth
// error is not that: another try may differ.
func providersDown(answers []reviewerAnswer) bool {
	n := 0
	for _, a := range answers {
		for _, at := range a.log {
			switch at.Outcome {
			case "rate_limit", "server", "unavailable", "timeout", "no_model_at_bar":
				n++
			default:
				return false
			}
		}
	}
	return n > 0
}

// anyNoBar: an attempt ended no_model_at_bar.
func anyNoBar(answers []reviewerAnswer) bool {
	for _, a := range answers {
		for _, at := range a.log {
			if at.Outcome == "no_model_at_bar" {
				return true
			}
		}
	}
	return false
}

// botResult is what a run did.
type botResult struct {
	summary   string
	reviewURL string
	log       []ModelAttempt
}

// carryOut does one run and records the outcome; failures are retried a
// few times, later.
func (s *Service) carryOut(ctx context.Context, r *BotRun) {
	ctx, cancel := context.WithTimeout(ctx, botRunTimeout)
	defer cancel()
	s.botMu.Lock()
	s.botCancels[r.ID] = cancel
	s.botMu.Unlock()
	defer func() {
		s.botMu.Lock()
		delete(s.botCancels, r.ID)
		s.botMu.Unlock()
	}()
	var res botResult
	var err error
	switch r.Kind {
	case "review":
		res, err = s.reviewPR(ctx, r)
	case "reply":
		res, err = s.replyRun(ctx, r)
	default:
		err = fmt.Errorf("unknown kind of bot run %q", r.Kind)
	}
	done := context.WithoutCancel(ctx)
	var skip *errSkip
	var down *errProvidersDown
	var wait *aiWaitError
	switch {
	case err == nil:
		err = s.finishRun(done, r.ID, "done", res.summary, "", res.log, res.reviewURL)
	case errors.As(err, &wait) && wait.why == "quota":
		s.log.WarnContext(ctx, "bot run waits for the AI key's daily limit to reset", "run", r.ID, "until", wait.until)
		lb, _ := json.Marshal(res.log)
		not := wait.until.Add(time.Duration(r.ID%20) * 30 * time.Second)
		_, err = s.db.W.ExecContext(done, `UPDATE bot_runs SET status = 'queued', error = ?, log = ?, not_before = ?, attempts = max(attempts - 1, 0) WHERE id = ? AND status = 'running'`,
			wait.Error(), string(lb), not.UnixMilli(), r.ID)
	case errors.As(err, &skip):
		if r.commentID != "" {
			s.replySkipped(done, r, skip.why)
		}
		err = s.finishRun(done, r.ID, "skipped", skip.why, "", res.log, "")
	case errors.Is(ctx.Err(), context.Canceled) && s.bgCtx.Err() == nil:
		return
	case s.bgCtx.Err() != nil:
		return
	case errors.As(err, &down):
		s.log.WarnContext(ctx, "bot run failed; every model unavailable", "run", r.ID)
		s.botFailedReaction(done, r)
		err = s.finishRun(done, r.ID, "failed", "", runErrorText(err, res.log), res.log, "")
	case r.Attempts < botMaxAttempts:
		s.log.WarnContext(ctx, "bot run failed; will retry", "run", r.ID, "attempt", r.Attempts, "err", err)
		lb, _ := json.Marshal(res.log)
		_, err = s.db.W.ExecContext(done, `UPDATE bot_runs SET status = 'queued', error = ?, log = ?, not_before = ? WHERE id = ? AND status = 'running'`,
			runErrorText(err, res.log), string(lb), s.now().Add(botRetryAfter).UnixMilli(), r.ID)
	default:
		s.log.WarnContext(ctx, "bot run failed", "run", r.ID, "err", err)
		s.botFailedReaction(done, r)
		err = s.finishRun(done, r.ID, "failed", "", runErrorText(err, res.log), res.log, "")
	}
	if err != nil {
		s.log.ErrorContext(ctx, "record bot run", "run", r.ID, "err", err)
	}
}

// signedOff: an earlier run of this rule (any rule) finished reviewing
// this head, or carried an earlier review to it (WP-1808). A review with nothing to say leaves no review on the forge to
// find (signOff), so the runs are asked too.
func (s *Service) signedOff(ctx context.Context, r *BotRun, sha string, anyRule bool) bool {
	q := `SELECT count(*) FROM bot_runs WHERE pr_id = ? AND kind = 'review' AND head_sha = ? AND (status = 'done' OR (status = 'skipped' AND summary LIKE ? || '%')) AND id != ?`
	args := []any{r.PR.ID, sha, carriedPrefix, r.ID}
	if !anyRule {
		q += ` AND automation_id IS ?`
		args = append(args, r.AutomationID)
	}
	var n int
	_ = s.db.R.QueryRowContext(ctx, q, args...).Scan(&n)
	return n > 0
}

// replySkipped answers a request that won't be carried out.
func (s *Service) replySkipped(ctx context.Context, r *BotRun, why string) {
	f, err := s.forgeByID(ctx, r.PR.ForgeID)
	cl, ok := s.Forges.(ForgePRs)
	if err != nil || !ok {
		return
	}
	msg := "Not reviewing: " + why + "."
	if strings.HasPrefix(why, "already reviewed") {
		msg += " `@waypoint full review` reviews it again."
	}
	_, _ = cl.PostPRComment(ctx, botForge(f), r.PR.Repo, r.PR.Number, msg+"\n<!-- waypoint skipped -->")
}

// botFailedReaction puts 😕 on the pull request when a run gives up, as
// Greptile does, so people know nothing more is coming.
func (s *Service) botFailedReaction(ctx context.Context, r *BotRun) {
	f, err := s.forgeByID(ctx, r.PR.ForgeID)
	cl, ok := s.Forges.(ForgePRs)
	if err != nil || !ok {
		return
	}
	_, _ = cl.React(ctx, botForge(f), r.PR.Repo, r.PR.Number, "", "confused")
}

// reviewPR reviews the pull request at the run's head and posts one review.
func (s *Service) reviewPR(ctx context.Context, r *BotRun) (botResult, error) {
	var res botResult
	var opts reviewOptions
	_ = json.Unmarshal([]byte(r.options), &opts)
	pr := r.PR
	var on int
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM project_repos WHERE project_id = ? AND forge_id = ? AND repo = ? COLLATE NOCASE AND bots = 1`,
		r.ProjectID, pr.ForgeID, pr.Repo).Scan(&on); err != nil {
		return res, err
	}
	if on == 0 {
		return res, &errSkip{"bots are off for " + pr.Repo + " in this project"}
	}
	if cur, err := s.prByID(ctx, s.db.R, pr.ID); err != nil || cur.Outside {
		return res, cmpErr(err, &errSkip{"it is from outside and waits for a person to approve it"})
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if err != nil {
		return res, err
	}
	cl, ok := s.Forges.(ForgePRs)
	if !ok {
		return res, errors.New("this forge client can't review")
	}
	bf := botForge(f)
	fp, err := cl.GetPR(ctx, f, pr.Repo, pr.Number)
	if err != nil {
		return res, err
	}
	// Only a title and description a Waypoint person wrote, or ones
	// approved, are read (WP-281).
	fp.Title, fp.Body = s.approvedTitle(ctx, f, pr, fp), s.approvedBody(ctx, f, pr, fp)
	if fp.State == "closed" || fp.State == "merged" {
		return res, &errSkip{"the pull request was " + fp.State}
	}
	if fp.HeadSHA != r.HeadSHA {
		return res, &errSkip{"a newer push (" + short(fp.HeadSHA) + ") replaced " + short(r.HeadSHA)}
	}
	ctx = s.botRunnerScope(ctx, r, f, fp)
	chain := opts.Models
	if len(chain) == 0 {
		if chain, err = s.taskChain(ctx, "review"); err != nil {
			return res, err
		}
	}
	if !opts.Force && r.Reason == "pushed" {
		from, earlier, ok, err := s.carriedHead(ctx, pr.ID, ruleID(r.AutomationID), fp.HeadSHA)
		if err != nil {
			return res, err
		}
		if ok {
			return res, &errSkip{carriedSummary(from, earlier)}
		}
	}
	sr, err := s.syncFindings(ctx, f, pr, true)
	if err != nil {
		s.log.WarnContext(ctx, "read the review threads", "repo", pr.Repo, "number", pr.Number, "err", err)
		sr = &syncResult{}
	}
	s.gateThreads(ctx, r.ProjectID, sr.threads)
	var verified verifyOutcome
	if len(chain) > 0 && len(sr.findings) > 0 {
		verified = s.verifyFindings(ctx, r, f, cl, fp, sr, chain)
		res.log = append(res.log, verified.log...)
	}
	if !opts.Force {
		posts, err := cl.ReviewBodies(ctx, f, pr.Repo, pr.Number)
		if err != nil {
			return res, fmt.Errorf("check earlier reviews: %w", err)
		}
		rule := ruleTag(r.AutomationID)
		if r.Reason == "review" {
			rule = "*"
		}
		if reviewedSHAs(posts, rule, s.waypointPosters(ctx, f))[fp.HeadSHA] || s.signedOff(ctx, r, fp.HeadSHA, rule == "*") {
			return res, &errSkip{"already reviewed at " + short(fp.HeadSHA)}
		}
	}
	if len(chain) == 0 {
		return res, &errSkip{"no model is set up to review: choose one under Admin → AI → Review pull requests"}
	}

	eyes, _ := cl.React(ctx, bf, pr.Repo, pr.Number, "", "eyes")
	var eyesOnComment string
	if r.commentID != "" {
		eyesOnComment, _ = cl.React(ctx, bf, pr.Repo, pr.Number, r.commentID, "eyes")
	}
	var tidied sync.Once
	tidy := func() {
		tidied.Do(func() {
			done := context.WithoutCancel(ctx)
			if eyes != "" {
				_ = cl.Unreact(done, bf, pr.Repo, pr.Number, "", "eyes", eyes)
			}
			if eyesOnComment != "" {
				_ = cl.Unreact(done, bf, pr.Repo, pr.Number, r.commentID, "eyes", eyesOnComment)
				_, _ = cl.React(done, bf, pr.Repo, pr.Number, r.commentID, "+1")
			}
		})
	}
	defer tidy()

	diff, err := cl.PRDiff(ctx, f, pr.Repo, fp)
	if errors.Is(err, errDiffTooLarge) {
		return res, &errSkip{"the diff is too large to review"}
	} else if err != nil {
		return res, err
	}
	private, err := s.privatePaths(ctx, cl, f, pr.Repo, fp.HeadSHA)
	if err != nil {
		return res, err
	}
	noise, err := newGlobSet(append(slices.Clone(noisePaths), opts.SkipFiles...))
	if err != nil {
		noise, _ = newGlobSet(noisePaths)
	}
	var files []fileDiff
	o := reviewOutcome{private: private}
	all := splitDiff(diff)
	for _, fd := range all {
		if noise.match(fd.path) {
			o.noise++
			continue
		}
		files = append(files, fd)
	}
	if len(files) == 0 {
		return res, &errSkip{"nothing reviewable in the diff"}
	}
	rules := s.repoReviewRules(ctx, cl, f, pr.Repo, fp.HeadSHA)
	earlier, rerr := s.prFindings(ctx, s.db.R, pr.ID)
	if rerr != nil {
		if readFault(rerr) {
			s.log.WarnContext(ctx, "pr findings failed", "pr", pr.ID, "err", rerr)
		}
	}
	conversation := reviewConversation(earlier, sr.threads)
	paths := make([]string, len(files))
	for i, fd := range files {
		paths[i] = fd.path
	}
	history := s.codeRiskNotes(ctx, f.ID, pr.Repo, paths)
	intent := s.reviewIntent(ctx, pr.ID)
	chunks, dropped := chunkDiff(files)
	o.dropped = dropped
	vchain := opts.Verifier
	if len(vchain) == 0 && !opts.NoVerify {
		if vchain, err = s.taskChain(ctx, "verify_review"); err != nil {
			return res, err
		}
	}
	chains := reviewerChains(chain, opts.Reviewers, s.gatewayWith(ctx, GatewayExclude))
	arms := s.drawArms(ctx, len(vchain) > 0 && !opts.NoVerify, len(chains))
	rec := &runRecord{set: true, arm: arms[0].key(), strategy: arms[0].strategy.name, promptVersion: arms[0].strategy.promptVersion()}
	defer func() { s.saveRunRecord(context.WithoutCancel(ctx), r.ID, rec) }()
	hf := newHeadFiles(ctx, cl, f, pr.Repo, fp.HeadSHA)
	budget := s.reviewFileBudget(ctx).Chars
	var noEvidence []droppedFinding
	show := opts.MaxFindings
	if show <= 0 || show > findingsGuard {
		show = reviewDefaultShown
	}
	byReviewer := make([][]Finding, len(chains))
	slotModels := make([]string, len(chains))
	defer func() {
		for _, m := range slotModels {
			if m != "" {
				rec.reviewers = append(rec.reviewers, m)
			}
		}
	}()
	answered := 0
	for n, ch := range chunks {
		o.cut = o.cut || ch.cut
		prompts := make([]string, len(chains))
		for i := range chains {
			prompts[i] = reviewPrompt(fp, rules, opts.Focus, conversation, intent, history, ch, n+1, len(chunks), arms[i].strategy, hf.get, budget)
		}
		answers := s.reviewChunk(withGatewayRouting(aiContext(ctx, "review"), reviewRouting(r.ID)), chains, prompts)
		reviewed := false
		for i, a := range answers {
			res.log = append(res.log, a.log...)
			if a.err != nil {
				continue
			}
			reviewed = true
			if !slices.Contains(o.models, a.model) {
				o.models = append(o.models, a.model)
			}
			if slotModels[i] == "" {
				slotModels[i] = a.model
			}
			if len(a.findings) == 0 && a.requestID != "" {
				o.emptyRequests = append(o.emptyRequests, a.requestID)
			}
			if a.salvaged > 0 {
				o.salvagedAnswers++
				o.salvagedFindings += a.salvaged
			}
			raised := normalizeFindings(a.findings, 0)
			o.guardCut += max(0, len(raised)-findingsGuard)
			for _, fnd := range raised[:min(len(raised), findingsGuard)] {
				fnd.Model = a.model
				fnd.Strategy, fnd.PromptVersion = arms[i].strategy.name, arms[i].strategy.promptVersion()
				if a.requestID != "" {
					fnd.Requests = []string{a.requestID}
				}
				if fnd.Evidence != "" {
					path := fnd.File
					if fd, ok := resolvePath(fnd.File, files); ok {
						path = fd.path
					}
					if !evidenceHolds(fnd.Evidence, hf.get(path)) {
						fnd.Verify, fnd.VerifyReason = VerifyUnchecked, evidenceMissing
						noEvidence = append(noEvidence, droppedFinding{Finding: fnd, reason: evidenceMissing})
						continue
					}
				}
				byReviewer[i] = append(byReviewer[i], fnd)
			}
		}
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if !reviewed {
			if w := quotaWait(answers); w != nil {
				return res, w
			}
			if providersDown(answers) {
				return res, &errProvidersDown{noBar: anyNoBar(answers)}
			}
			for _, fd := range ch.files {
				o.dropped = append(o.dropped, fd.path)
			}
			continue
		}
		answered++
	}
	if answered == 0 {
		return res, errAllModelsFailed
	}
	o.minSev, _ = parseSeverity(opts.MinSeverity)
	found, below := atLeast(normalizeFindings(mergeReviewers(byReviewer), 0), o.minSev)
	o.guardCut += max(0, len(found)-findingsGuard)
	found = normalizeFindings(found, findingsGuard)
	if o.guardCut > 0 {
		s.log.WarnContext(ctx, "a review's findings were cut at the guard", "pr", pr.ID, "cut", o.guardCut, "kept", len(found))
	}
	found, o.repeated, err = s.dropRepeated(ctx, pr.ID, found, sr.threads)
	if err != nil {
		return res, err
	}
	found = s.markTrusted(ctx, found)
	switch {
	case len(found) > 0 && !arms[0].verify:
		found = markVerify(found, VerifySkipped)
	case len(found) > 0 && arms[0].verify:
		v := s.verifyNewIn(withGatewayRouting(aiContext(ctx, "verify_review"), verifyRouting(r.ID)), vchain, hf, pr.Repo, fp.HeadSHA, found, files)
		res.log = append(res.log, v.log...)
		rec.verified = len(v.models) > 0
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		found, o.verifiers = v.kept, v.models
		o.unverified = v.unchecked
		dropPost := s.serverNumber(ctx, settingDropPost, defaultDropPost)
		for _, d := range v.dropped {
			if s.draw() < dropPost {
				found = append(found, d.Finding)
				o.doubted++
				continue
			}
			o.verifyDropped = append(o.verifyDropped, d)
		}
	default:
		found = markVerify(found, VerifyUnchecked)
	}
	o.noEvidence = len(noEvidence)
	var lowered int
	found, lowered = atLeast(found, o.minSev)
	o.belowMin = below + lowered
	found = normalizeFindings(found, findingsGuard)
	o.trusted = countVerify(found, VerifyTrusted)
	o.placed = placeFindings(found, files, private)
	o.later = keepPast(o.placed, show, private)
	if o.later > 0 {
		o.readAt = fmt.Sprintf("`wp pr todo %d` lists them", fp.Number)
		if page := prPageURL(s.BaseURL(), pr.ID); page != "" {
			o.readAt += ", as does " + page
		}
	}
	o.note = s.reviewNote(ctx)

	var comments []LineComment
	for n := range o.placed {
		p := &o.placed[n]
		p.marker = findingMarker(r.ID, n)
		if p.placement == "inline" {
			comments = append(comments, LineComment{Path: p.File, Line: p.Line, Body: inlineComment(*p, o.note)})
		}
	}
	o.rule = ruleTag(r.AutomationID)
	if !signOff(o) {
		posted, err := cl.PostReview(ctx, bf, pr.Repo, fp, reviewBody(fp.HeadSHA, o), comments)
		if err != nil && len(comments) > 0 && ctx.Err() == nil {
			s.log.WarnContext(ctx, "forge refused line comments; folding them into the review", "repo", pr.Repo, "number", pr.Number, "err", err)
			for n, p := range o.placed {
				if p.placement == "inline" {
					if private.match(p.File) {
						o.placed[n].placement = "withheld"
					} else {
						o.placed[n].placement = "body"
					}
				}
			}
			posted, err = cl.PostReview(ctx, bf, pr.Repo, fp, reviewBody(fp.HeadSHA, o), nil)
		}
		if err != nil {
			return res, fmt.Errorf("post the review: %w", err)
		}
		res.reviewURL = posted.URL
	}
	if err := s.saveFindings(ctx, r, fp.HeadSHA, o.placed, append(slices.Clone(o.verifyDropped), noEvidence...), o.emptyRequests, private); err != nil {
		s.log.ErrorContext(ctx, "save findings", "run", r.ID, "err", err)
		o.unsaved = true
		// An earlier review of this head (or one carried to it) no longer
		// stands either: this one's findings, which the gate hasn't seen,
		// are on the same head, so a later merge-only push must not carry it.
		s.forgetReviewedHead(context.WithoutCancel(ctx), pr.ID, ruleID(r.AutomationID), fp.HeadSHA)
	} else if _, err := s.syncFindings(ctx, f, pr, false); err != nil {
		s.log.WarnContext(ctx, "link findings to their threads", "run", r.ID, "err", err)
	}
	shown := 0
	for _, p := range o.placed {
		if p.placement != "withheld" && p.placement != placementLater {
			shown++
		}
	}
	tidy()
	if signOff(o) {
		if _, err := cl.React(ctx, bf, pr.Repo, pr.Number, "", "+1"); err != nil {
			return res, fmt.Errorf("react to sign off: %w", err)
		}
		res.summary = signedOffSummary
	} else if len(o.placed) == 0 {
		res.summary = "Nothing found in what was reviewed"
	} else {
		res.summary = fmt.Sprintf("%d finding%s", shown, plural(shown))
		if o.later > 0 {
			res.summary += fmt.Sprintf(" and %d more kept, not posted", o.later)
		}
	}
	res.summary += " by " + strings.Join(o.models, ", ")
	if len(o.verifiers) > 0 {
		res.summary += ", checked by " + strings.Join(o.verifiers, ", ")
	}
	if n := len(o.verifyDropped); n > 0 {
		res.summary += fmt.Sprintf("; %d dropped by the verifier", n)
	}
	if o.doubted > 0 {
		res.summary += fmt.Sprintf("; %d doubted by the verifier but posted", o.doubted)
	}
	if o.noEvidence > 0 {
		res.summary += fmt.Sprintf("; %d dropped: %s", o.noEvidence, evidenceMissing)
	}
	if o.unverified > 0 {
		res.summary += fmt.Sprintf("; %d not verified", o.unverified)
	}
	if o.trusted > 0 {
		res.summary += fmt.Sprintf("; %d from a trusted reviewer, no verifier needed", o.trusted)
	}
	if o.repeated > 0 {
		res.summary += fmt.Sprintf("; %d already raised", o.repeated)
	}
	if o.belowMin > 0 {
		res.summary += fmt.Sprintf("; %d below %s not posted", o.belowMin, priority(o.minSev))
	}
	res.summary += verified.summary()
	// A review that left part of the diff unreviewed isn't one a later head
	// carries (WP-1808): see reviewedWhole.
	// The diff read at the start must still be the head's: the forge is asked
	// again, as the diff isn't pinned to a head.
	if cur, err := s.prByID(ctx, s.db.R, pr.ID); err == nil && reviewedWhole(o) && cur.HeadSHA == fp.HeadSHA {
		if again, err := cl.GetPR(ctx, f, pr.Repo, pr.Number); err == nil && again != nil && sameSHA(again.HeadSHA, fp.HeadSHA) {
			since := cur.UpdatedAt
			if t := cur.contentSince(); t != nil {
				since = *t
			}
			s.noteReviewedHead(context.WithoutCancel(ctx), pr.ID, ruleID(r.AutomationID), fp.HeadSHA, udiff.ChangeHash(diff), res.summary, cur.contentHeadSHA(), since)
		}
	}
	return res, nil
}

// signedOffSummary starts the summary of a run that signed its head off; the
// gate reads it (WP-234).
const signedOffSummary = "No issues found"

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// privatePaths are the paths a mirror keeps off the forge it copies a repo
// to: the forge's own list (Admin → Forges) and, on Forgejo, the `private`
// list in the repo's .forgejo/sync.toml at the head. None without a mirror.
func (s *Service) privatePaths(ctx context.Context, cl ForgePRs, f *Forge, repo, sha string) (globSet, error) {
	globs := slices.Clone(f.MirrorPrivate)
	if f.Kind == "forgejo" {
		toml, err := cl.FileAt(ctx, f, repo, ".forgejo/sync.toml", sha)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return nil, fmt.Errorf("read .forgejo/sync.toml: %w", err)
		default:
			more, err := syncPrivate(toml)
			if err != nil {
				return nil, &errSkip{"can't read .forgejo/sync.toml (" + err.Error() + "), so findings might reach the mirror; not reviewing"}
			}
			globs = append(globs, more...)
		}
	}
	if len(globs) == 0 {
		return nil, nil
	}
	gs, err := newGlobSet(globs)
	if err != nil {
		return nil, &errSkip{"a private path in .forgejo/sync.toml isn't a valid pattern: " + err.Error()}
	}
	return gs, nil
}

// repoReviewRules reads the review section of REVIEW.md or AGENTS.md at the
// head, as Codex and Copilot do.
func (s *Service) repoReviewRules(ctx context.Context, cl ForgePRs, f *Forge, repo, sha string) string {
	for _, name := range []string{"REVIEW.md", "AGENTS.md", ".github/copilot-instructions.md"} {
		md, err := cl.FileAt(ctx, f, repo, name, sha)
		if err != nil {
			continue
		}
		if rules := reviewRules(name, md); rules != "" {
			return rules
		}
	}
	return ""
}

// saveFindings records a run's findings: those placed on the review, and
// those dropped before posting, as rows in state "dropped" that no gate,
// packet or comment sees (WP-363); and the requests whose answers raised
// none (empty).
func (s *Service) saveFindings(ctx context.Context, r *BotRun, sha string, placed []placedFinding, dropped []droppedFinding, empty []string, private globSet) error {
	return s.write(ctx, SystemActor(), func(t *tx) error {
		if err := s.recordEmptyRequests(ctx, t, r.ID, empty, s.nowMS()); err != nil {
			return err
		}
		const insertSQL = `INSERT INTO bot_findings(run_id, pr_id, path, line, severity, claim, model, placement, state, created_at, updated_at, ordinal, head_sha, checked_sha, private, agreed, verified_by,
				prompt_version, strategy, verify, verify_reason) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
		insert := func(f Finding, placement, state string, n int) error {
			var line any
			if f.Line > 0 {
				line = f.Line
			}
			now := s.nowMS()
			res, err := t.ExecContext(ctx, insertSQL, r.ID, r.PR.ID, f.File, line, f.Severity, f.Claim, f.Model, placement, state, now, now, n, sha, sha,
				boolInt(private.match(f.File)), max(f.Agreed, 1), f.VerifiedBy, f.PromptVersion, f.Strategy, f.Verify, f.VerifyReason)
			if err != nil {
				return err
			}
			id, err := res.LastInsertId()
			if err != nil {
				return err
			}
			return s.linkFindingRequests(ctx, t, r.ID, id, f.Requests, now)
		}
		for n, p := range placed {
			if err := insert(p.Finding, p.placement, FindingOpen, n); err != nil {
				return err
			}
		}
		for n, d := range dropped {
			if err := insert(d.Finding, "withheld", FindingDropped, len(placed)+n); err != nil {
				return err
			}
		}
		return nil
	})
}

// botRunAudit is a person stepping in on a bot's run, for the log.
func botRunAudit(action string, r *BotRun, detail string) AuditEntry {
	if r.Automation != "" {
		detail += " (" + r.Automation + ")"
	}
	return AuditEntry{Action: action, Category: AuditControl, ProjectID: r.ProjectID, TargetType: "pr", TargetID: r.PR.ID,
		Target: fmt.Sprintf("%s#%d", r.PR.Repo, r.PR.Number), URL: r.PR.URL, Detail: detail}
}
