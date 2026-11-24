package core

// What happened on pull requests (WP-D15): opened, pushed, reviewed,
// approved, merged, with who did it, as the forges report it. Webhooks
// bring it as it happens; a periodic refresh fills in what they missed
// without ever setting bots off on old pull requests.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

type hookUser struct {
	Login    string `json:"login"`
	Username string `json:"username"`
}

func (u hookUser) name() string { return cmp(u.Login, u.Username) }

// hookComment is a comment in a GitHub or Forgejo webhook: on an issue or
// pull request (issue_comment) or on a pull request's diff.
type hookComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	HTMLURL   string    `json:"html_url"`
	User      hookUser  `json:"user"`
	UpdatedAt time.Time `json:"updated_at"`
	CommitID  string    `json:"commit_id"`
}

// hookReview is GitHub's submitted review, or Forgejo's review summary
// (type and content, no id).
type hookReview struct {
	ID          int64      `json:"id"`
	User        hookUser   `json:"user"`
	Body        string     `json:"body"`
	State       string     `json:"state"` // approved, changes_requested, commented
	HTMLURL     string     `json:"html_url"`
	CommitID    string     `json:"commit_id"`
	SubmittedAt *time.Time `json:"submitted_at"`
	Type        string     `json:"type"` // pull_request_review_approved, …
	Content     string     `json:"content"`
}

// parsePRReview reads a review webhook: GitHub's pull_request_review and
// pull_request_review_comment, Forgejo's pull_request_approved, _rejected
// and _comment (X-Forgejo-Event; X-Forgejo-Event-Type spells them
// pull_request_review_*). Nil for what isn't news (an edited or dismissed
// review).
func parsePRReview(event, action string, number int, sender, commit string, rv *hookReview, c *hookComment) *forgeReviewEvent {
	out := &forgeReviewEvent{number: number, sha: commit}
	switch event {
	case "pull_request_review_comment": // GitHub: a comment on the diff
		if c == nil || action != "created" {
			return nil
		}
		out.kind = "commented"
		out.ForgeComment = ForgeComment{ID: "rc" + strconv.FormatInt(c.ID, 10), Author: c.User.name(), Body: c.Body, URL: c.HTMLURL, UpdatedAt: c.UpdatedAt}
		out.sha = cmp(c.CommitID, commit)
		return out
	case "pull_request_review":
		if rv == nil || (action != "submitted" && action != "") {
			return nil
		}
		switch strings.ToLower(rv.State) {
		case "approved":
			out.kind = "approved"
		case "changes_requested":
			out.kind = "changes_requested"
		case "commented":
			out.kind = "reviewed"
		default:
			return nil // pending or dismissed
		}
		out.ForgeComment = ForgeComment{ID: "r" + strconv.FormatInt(rv.ID, 10), Author: cmp(rv.User.name(), sender), Body: rv.Body, URL: rv.HTMLURL}
		if rv.SubmittedAt != nil {
			out.UpdatedAt = *rv.SubmittedAt
		}
		out.sha = cmp(rv.CommitID, commit)
		return out
	}
	// Forgejo / Gitea.
	switch event {
	case "pull_request_approved", "pull_request_review_approved":
		out.kind = "approved"
	case "pull_request_rejected", "pull_request_review_rejected":
		out.kind = "changes_requested"
	default:
		out.kind = "reviewed"
	}
	body := ""
	if rv != nil {
		body = rv.Content
	}
	// Forgejo gives reviews no id: one review is one person's kind of review
	// with this text at this head.
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%d|%s", out.kind, sender, commit, number, body)))
	out.ForgeComment = ForgeComment{ID: "fr" + hex.EncodeToString(sum[:8]), Author: sender, Body: body}
	return out
}

// ---- which agent wrote a pull request ----

var (
	coAuthorClaudeRE = lazyre.New(`(?im)^co-authored-by:\s*claude\s+([a-z]+)\s+(\d+(?:\.\d+)*)\b`)
	claudeCodeRE     = lazyre.New(`(?i)generated with \[?claude code\]?|claude\.com/claude-code|claude\.ai/code|^claude-session:`)
	codexRE          = lazyre.New(`(?i)chatgpt\.com/codex|co-authored-by:\s*codex\b|generated (?:with|by) codex`)
	cursorRE         = lazyre.New(`(?i)co-authored-by:\s*cursor agent|cursoragent@cursor\.com|generated (?:with|by) cursor`)
	opencodeRE       = lazyre.New(`(?i)generated (?:with|by) \[?opencode\]?|opencode\.ai`)
	copilotRE        = lazyre.New(`(?i)co-authored-by:\s*copilot\b`)
)

// codingAgent names the coding agent a pull request's text says wrote it
// ("" if none does): Claude Code's trailers name the model, which is
// better than the tool.
func codingAgent(body, author string) string {
	if m := coAuthorClaudeRE.FindStringSubmatch(body); m != nil {
		return "anthropic/claude-" + strings.ToLower(m[1]) + "-" + strings.ReplaceAll(m[2], ".", "-")
	}
	switch {
	case claudeCodeRE.MatchString(body):
		return "anthropic/claude-code"
	case codexRE.MatchString(body):
		return "openai/codex"
	case cursorRE.MatchString(body):
		return "cursor/cursor-agent"
	case opencodeRE.MatchString(body):
		return "opencode"
	case copilotRE.MatchString(body), strings.EqualFold(author, "copilot"), strings.HasPrefix(strings.ToLower(author), "copilot-swe-agent"):
		return "github/copilot"
	}
	return ""
}

// ---- history from transitions ----

// prEventRow is one thing that happened, before it's stored.
type prEventRow struct {
	kind, actor, sha, url, body, extID string
	bot                                bool
	own                                bool // Waypoint posted it (WP-455)
	at                                 int64
}

func msOr(t *time.Time, def int64) int64 {
	if t == nil || t.IsZero() {
		return def
	}
	return t.UnixMilli()
}

// prHistory is what the change from old to cur adds to a pull request's
// history. old is nil the first time it's seen, when the forge's dates say
// when it opened (and merged or closed, for an old one). A record with no
// head (made by a refresh) getting one isn't a push.
func prHistory(old, cur *PullRequest, actor string, now int64) []prEventRow {
	var out []prEventRow
	add := func(kind, who, sha, ext string, at int64) {
		out = append(out, prEventRow{kind: kind, actor: who, sha: sha, extID: ext, bot: who != "" && isBotLogin(who), at: at})
	}
	merged := func(at int64) { add("merged", cur.MergedBy, cur.HeadSHA, "merged", at) }
	closed := func(at int64) { add("closed", actor, cur.HeadSHA, "closed:"+strconv.FormatInt(at, 10), at) }
	if old == nil {
		add("opened", cur.Author, cur.HeadSHA, "opened", msOr(cur.OpenedAt, now))
		switch cur.State {
		case "merged":
			merged(msOr(cur.MergedAt, now))
		case "closed":
			closed(msOr(cur.ClosedAt, now))
		}
		return out
	}
	wasLive := old.State == "open" || old.State == "draft"
	live := cur.State == "open" || cur.State == "draft"
	switch {
	case !wasLive && live:
		add("reopened", actor, cur.HeadSHA, "reopened:"+strconv.FormatInt(now, 10), now)
	case old.State == "draft" && cur.State == "open":
		add("ready", actor, cur.HeadSHA, "ready:"+cur.HeadSHA, now)
	}
	if live && old.HeadSHA != "" && cur.HeadSHA != "" && old.HeadSHA != cur.HeadSHA {
		add("pushed", cmp(actor, cur.Author), cur.HeadSHA, "pushed:"+cur.HeadSHA, now)
	}
	switch {
	case cur.State == "merged" && old.State != "merged":
		merged(msOr(cur.MergedAt, now))
	case cur.State == "closed" && wasLive:
		closed(msOr(cur.ClosedAt, now))
	}
	return out
}

// savePREvents stores history rows (a repeated delivery is ignored) and
// records people's merges in the audit log. It reports which were new.
func (s *Service) savePREvents(ctx context.Context, t *tx, pr *PullRequest, rows []prEventRow, projectID int64, audit bool) error {
	for _, r := range rows {
		res, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO pr_events(pr_id, kind, actor, bot, own, sha, url, body, ext_id, created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			pr.ID, r.kind, r.actor, boolInt(r.bot), boolInt(r.own), r.sha, r.url, excerpt(strings.TrimSpace(r.body), 500), r.extID, r.at)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 || !audit || r.bot || r.actor == "" {
			continue
		}
		e := AuditEntry{Category: AuditApproval, ProjectID: projectID, TargetType: "pr", TargetID: pr.ID,
			Target: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), URL: cmp(r.url, pr.URL), Actor: r.actor, About: pr.Agent}
		switch r.kind {
		case "merged":
			e.Action, e.Detail = "pr.merged", fmt.Sprintf("merged “%s” into %s", pr.Title, cmp(pr.BaseRef, "its base"))
		case "approved":
			e.Action, e.Detail = "pr.approved", "approved “"+pr.Title+"”"
		case "changes_requested":
			e.Action, e.Detail = "pr.changes_requested", "asked for changes to “"+pr.Title+"”"
		default:
			continue
		}
		t.audit(e)
	}
	return nil
}

// attachedProject is the first project a repo is attached to (bots on or
// off), which forge-reported decisions are filed under. A repo attached to
// nothing is the answer (0, nil); a lookup that failed is an error, which a
// caller must not read as "no project": events would go to admins only and
// gates and filings would be skipped.
func (s *Service) attachedProject(ctx context.Context, q queryer, forgeID int64, repo string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT project_id FROM project_repos WHERE forge_id = ? AND repo = ? COLLATE NOCASE ORDER BY project_id LIMIT 1`, forgeID, repo).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// attachedProjectKey is attachedProject's key ("" for a repo attached to nothing).
func (s *Service) attachedProjectKey(ctx context.Context, forgeID int64, repo string) (string, error) {
	id, err := s.attachedProject(ctx, s.db.R, forgeID, repo)
	if err != nil || id == 0 {
		return "", err
	}
	p, err := s.projectByID(ctx, s.db.R, id)
	if errors.Is(err, ErrNotFound) {
		return "", nil // gone since: attached to nothing
	}
	if err != nil {
		return "", err
	}
	return p.Key, nil
}

// ---- reviews and comments ----

// onPRActivity records a review, or a comment on a pull request, in its
// history. Waypoint's own reviews aren't repeated here: its bot runs
// record them, with more detail. A person's review with Waypoint's marker is
// recorded: dropped, their request for changes would hold nothing (WP-934).
func (s *Service) onPRActivity(ctx context.Context, f *Forge, repo string, a *forgeReviewEvent) {
	if a == nil || a.number == 0 {
		return
	}
	ours := s.waypointPosters(ctx, f)
	if strings.Contains(a.Body, "<!-- waypoint-review") && a.kind != "commented" && ours.ours(a.Author) {
		return
	}
	projectID, err := s.attachedProject(ctx, s.db.R, f.ID, repo)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read the project to record a pull request's review", "repo", repo, "number", a.number, "err", err)
		}
		return
	}
	if projectID == 0 {
		return
	}
	pr, err := s.prByNumber(ctx, s.db.R, f.ID, repo, a.number)
	if err == nil && pr == nil {
		pr, err = s.learnPR(ctx, f, repo, a.number)
	}
	if err != nil || pr == nil {
		if err != nil {
			s.log.WarnContext(ctx, "pull request activity", "repo", repo, "number", a.number, "err", err)
		}
		return
	}
	at := s.nowMS()
	if !a.UpdatedAt.IsZero() && a.UpdatedAt.UnixMilli() <= at {
		at = a.UpdatedAt.UnixMilli()
	}
	bot := isBotLogin(a.Author) || ours.botText(a.Author, a.Body)
	row := prEventRow{kind: a.kind, actor: a.Author, bot: bot, own: s.ownPost(ctx, f, a, ours), sha: a.sha, url: a.URL, body: a.Body, extID: a.ID, at: at}
	if s.prVoice(ctx, f).outside(ctx, s.db.R, a.Author) {
		row.body = "" // that they acted, not what they wrote (WP-281)
	}
	err = s.write(ctx, SystemActor(), func(t *tx) error {
		if err := s.savePREvents(ctx, t, pr, []prEventRow{row}, projectID, true); err != nil {
			return err
		}
		if pr.Outside {
			return nil
		}
		t.emit(&Event{Kind: EvPRActivity, ProjectID: projectID, New: a.kind, Data: map[string]any{"pr_id": pr.ID}})
		if a.kind != "commented" {
			// A review was submitted (WP-D21).
			t.emit(&Event{Kind: EvPRReview, ProjectID: projectID, New: a.kind, Data: prEventData(pr, map[string]any{
				"pr": pr.ID, "sha": cmp(a.sha, pr.HeadSHA), "reviewer": a.Author, "verdict": a.kind, "bot": bot})})
		}
		return nil
	})
	if err != nil {
		s.log.WarnContext(ctx, "record pull request activity", "repo", repo, "number", a.number, "err", err)
	}
}

// ownPost: Waypoint posted it, by its marker from an account it posts as
// (WP-934), or by its bot account. Forgejo sends a reply in a review thread
// as a review with no text of its own, so the account is all there is to go
// by; without a bot token Waypoint posts as the forge token's person, whose
// own reviews must count, so then the marker only.
func (s *Service) ownPost(ctx context.Context, f *Forge, a *forgeReviewEvent, ours posters) bool {
	if strings.Contains(a.Body, "<!-- waypoint") {
		return ours.ours(a.Author)
	}
	if f.botToken == "" || a.Author == "" {
		return false
	}
	return strings.EqualFold(a.Author, s.forgeBotLogin(ctx, f))
}

// isBotText: a comment has a marker bots leave (Waypoint's own, and the
// llm-review bot's). Anyone can type one: whether a bot wrote it is
// posters.botText's to say (WP-934). Left to the text alone, it only keeps
// Waypoint from reading a comment (forge mentions, people's replies), so a
// typed marker hides no one's words but the typist's.
func isBotText(body string) bool {
	return strings.Contains(body, "<!-- waypoint") || strings.Contains(body, "<!-- llm-review")
}

// learnPR fetches and records a pull request Waypoint hasn't seen, without
// setting bots off (it wasn't news that brought it).
func (s *Service) learnPR(ctx context.Context, f *Forge, repo string, number int) (*PullRequest, error) {
	cl, ok := s.Forges.(ForgePRs)
	if !ok {
		return nil, nil
	}
	fp, err := cl.GetPR(ctx, f, repo, number)
	if err != nil {
		return nil, err
	}
	return s.recordPR(ctx, f, repo, fp, false)
}

// ---- refresh ----

// RefreshPullRequests reads each attached repo's recently updated pull
// requests, so records and history stay right when a webhook was missed.
// It only adds what's settled: a pull request it hasn't seen is recorded
// without a head (so the webhook that does arrive still counts as new and
// sets the bots off), and an open one's head and state are left to webhooks.
// A repo whose webhook has never delivered gets no webhooks to leave them
// to, so there an open pull request is taken as a webhook would bring it:
// a new head sets the bots off and CI is followed, a sync later (WP-31).
func (s *Service) RefreshPullRequests(ctx context.Context) {
	lister, ok := s.Forges.(ForgePRLister)
	if !ok {
		jobSkip(ctx, "the forge client cannot list pull requests")
		return
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT forge_id, repo FROM project_repos ORDER BY forge_id, repo`)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "pull request refresh: find the repos", "err", err)
		}
		return
	}
	type fr struct {
		forge int64
		repo  string
	}
	var list []fr
	for rows.Next() {
		var x fr
		if err := rows.Scan(&x.forge, &x.repo); err != nil {
			s.log.WarnContext(ctx, "pull request refresh: read a repo", "err", err)
			continue
		}
		list = append(list, x)
	}
	err = rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	if readFault(err) {
		// The repos read so far are still refreshed; the rest wait for the next run.
		s.log.WarnContext(ctx, "pull request refresh: read the repos", "err", err)
	}
	seen := map[string]bool{}
	for _, x := range list {
		if ctx.Err() != nil {
			return
		}
		key := fmt.Sprint(x.forge) + "|" + strings.ToLower(x.repo)
		if seen[key] {
			continue
		}
		seen[key] = true
		f, err := s.forgeByID(ctx, x.forge)
		if err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "pull request refresh: read the forge", "repo", x.repo, "err", err)
			}
			continue
		}
		if f.token == "" {
			continue
		}
		prs, err := lister.ListPRs(ctx, f, x.repo)
		if err != nil {
			// The repo's pull requests go unrefreshed this run: that is a
			// problem of the job's, not a detail.
			if isStatus(err, http.StatusNotFound) || errors.Is(err, ErrNotFound) {
				// Moved or deleted on the forge: an answer, not a fault to warn of every tick.
				s.log.DebugContext(ctx, "pull request refresh: the forge has no such repo", "forge", f.Name, "repo", x.repo)
			} else if readFault(err) {
				s.log.WarnContext(ctx, "pull request refresh: list pull requests", "forge", f.Name, "repo", x.repo, "err", err)
			}
			continue
		}
		hooked := s.hookDelivers(ctx, f.ID, x.repo)
		for _, fp := range prs {
			if !hooked && (fp.State == "open" || fp.State == "draft") {
				s.onForgePull(ctx, f, x.repo, fp)
			} else if err := s.refreshPR(ctx, f, x.repo, fp); err != nil {
				s.log.WarnContext(ctx, "refresh pull request", "repo", x.repo, "number", fp.Number, "err", err)
			}
			s.refreshPRMentions(ctx, f, x.repo, fp)
		}
	}
}

// refreshPRMentions records a listed pull request's mentions, as its
// webhook would (WP-379): a missed edit, or a repo whose webhook never
// delivers. Only an open one links its own work; a newly linked one gets
// its state as its webhook would have given it.
func (s *Service) refreshPRMentions(ctx context.Context, f *Forge, repo string, fp *ForgePR) {
	raw := &ForgeItem{Number: fp.Number, Title: stripWIP(fp.Title), Body: fp.Body, State: fp.State, URL: fp.URL, IsPR: true, Author: fp.Author}
	if raw.URL == "" {
		return
	}
	shown := s.linkItem(ctx, f, raw)
	open := fp.State == "open" || fp.State == "draft"
	res, err := s.scanPull(ctx, f, repo, raw, shown, fp.HeadRef, open)
	if err != nil {
		s.log.WarnContext(ctx, "record forge mentions", "url", fp.URL, "err", err)
		return
	}
	if res.Linked > 0 {
		s.applyLinkState(ctx, raw.URL, shown)
	}
}

// hookDelivers says whether a webhook (the repo's own, an organisation's or
// one set up by hand, even where Waypoint couldn't add its own) has
// delivered for a repo.
func (s *Service) hookDelivers(ctx context.Context, forgeID int64, repo string) bool {
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM project_repos WHERE forge_id = ? AND lower(repo) = lower(?)
		AND hook_seen_at IS NOT NULL`, forgeID, repo).Scan(&n)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read hook delivers", "forgeID", forgeID, "repo", repo, "err", err)
		}
		return true
	}
	return n > 0
}

// refreshPR applies a listed pull request: new ones are recorded headless,
// known open ones only get their dates, size and title, unless they are
// still headless (their webhook was missed); closing and merging (which
// never set bots off) are applied in full.
func (s *Service) refreshPR(ctx context.Context, f *Forge, repo string, fp *ForgePR) error {
	old, err := s.prByNumber(ctx, s.db.R, f.ID, repo, fp.Number)
	if err != nil {
		return err
	}
	settled := fp.State == "closed" || fp.State == "merged"
	switch {
	case old != nil && old.HeadSHA == "" && !settled && fp.HeadSHA != "":
		// Recorded headless a sync ago and its webhook never came: take it
		// as that webhook, or it never gets reviewed, followed or merged.
		s.onForgePull(ctx, f, repo, fp)
		return nil
	case old != nil && !settled && fp.Body != "" && s.approvedBodyChanged(ctx, old.ID, fp.Body):
		// An approved outside pull request's description changed and its
		// webhook never came: screen it as that webhook would.
		s.onForgePull(ctx, f, repo, fp)
		return nil
	case old == nil && !settled:
		c := *fp
		c.HeadSHA = ""
		_, err = s.recordPR(ctx, f, repo, &c, false)
		return err
	case old == nil, settled && old.State != fp.State:
		_, err = s.recordPR(ctx, f, repo, fp, false)
		return err
	}
	_, err = s.db.W.ExecContext(ctx, `UPDATE pull_requests SET title = CASE WHEN outside = 1 THEN title ELSE ? END, opened_at = coalesce(?, opened_at), merged_at = coalesce(?, merged_at),
		closed_at = coalesce(?, closed_at), merged_by = CASE WHEN ? != '' THEN ? ELSE merged_by END,
		merge_sha = CASE WHEN ? != '' THEN ? ELSE merge_sha END,
		agent = CASE WHEN ? != '' THEN ? ELSE agent END,
		additions = coalesce(?, additions), deletions = coalesce(?, deletions), changed_files = coalesce(?, changed_files), commits = coalesce(?, commits),
		head_repo = CASE WHEN ? != '' THEN ? ELSE head_repo END
		WHERE id = ?`, excerpt(stripWIP(fp.Title), 300), nullTime(fp.OpenedAt), nullTime(fp.MergedAt), nullTime(fp.ClosedAt), fp.MergedBy, fp.MergedBy, fp.MergeSHA, fp.MergeSHA,
		codingAgent(fp.Body, fp.Author), codingAgent(fp.Body, fp.Author),
		nullIntP(fp.Additions), nullIntP(fp.Deletions), nullIntP(fp.ChangedFiles), nullIntP(fp.Commits), headRepoOf(fp, repo), headRepoOf(fp, repo), old.ID)
	if err == nil && !settled && old.ArmedAt == nil && s.autoArm(ctx, old.ID) {
		// Merge automatically armed it (WP-133): an open pull request whose
		// webhook came before the setting did.
		bg := context.WithoutCancel(ctx)
		s.goBG(func() { s.maybeAutoMerge(bg, old.ID) })
	}
	if err == nil && !settled && fp.mergeMute && old.Mergeable != nil {
		return forgetMergeable(ctx, s.db.W, old.ID)
	}
	if err != nil || fp.Mergeable == nil || settled {
		return err
	}
	// Whether it merges cleanly changes with no webhook: the base moved, or
	// the forge finished working it out after a push (it says "no" while it
	// checks). An armed pull request that now merges may be ready.
	if err := s.noteMergeable(ctx, s.db.W, old, false, fp.Mergeable); err != nil {
		return err
	}
	if *fp.Mergeable && (old.Mergeable == nil || !*old.Mergeable) && old.ArmedAt != nil {
		bg := context.WithoutCancel(ctx)
		s.goBG(func() { s.maybeAutoMerge(bg, old.ID) })
	}
	if !*fp.Mergeable {
		s.scheduleMergeRecheck(ctx, old.ID)
	}
	return nil
}

func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func nullIntP(n *int) any {
	if n == nil {
		return nil
	}
	return *n
}
