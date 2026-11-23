package core

// Opening a pull request on the project's forge (WP-44): the CLI pushes the
// branch, Waypoint opens the pull request with the forge's token, so access
// is checked here and an agent never needs forge credentials of its own.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// OpenPRInput says what to open. Repo and Base are optional: the project's
// only repo, and the repo's default branch.
type OpenPRInput struct {
	Project string
	Repo    string
	Head    string
	Base    string
	Title   string
	Body    string
	Draft   bool
	// Ready opens an agent's pull request ready; by default it opens as a
	// draft ("WIP: ") that Waypoint marks ready (WP-158).
	Ready bool
	Issue string // an issue key: the pull request is linked to it
	// For is the task the opener is finishing (wp ship, WP-608), when it
	// isn't Issue: a pull request already open for other tasks is theirs.
	For string
	// Urgent opens it with the ci-urgent label, so its first CI run is
	// already on the urgent lane (WP-528); one already open gets the label.
	// A new pull request for a task tagged ci-urgent gets it anyway (WP-697).
	Urgent bool
	// Review is wp ship's intent about the task's adversarial review before
	// its pull request (WP-688): ShipAuto, ShipReview, ShipPR (the same as
	// ShipAuto) or ShipSkip (Reason says why). "" is a CLI from before intents: a held pull
	// request is its error. Force reviews again a change already reviewed.
	Review string
	Reason string
	// noSweep: Waypoint's own skip (a held review released), not the agent's,
	// so no other round is recorded as skipped by the agent.
	noSweep bool
	// facts: what the texts of this ship need of the agent (see shipFacts).
	facts *shipFacts
	// byWaypoint: Waypoint opens it (shipHeld), not the agent's wp ship, so
	// the notes it leaves say so (WP-1325).
	byWaypoint bool
	// stillMay asks, once the forge has answered, whether the one opening
	// still may: "" yes, else why not. It is the server ship's, whose
	// authority can go while the forge call is out. A pull request that
	// exists on the forge is recorded either way, but a one that may not
	// go on gets no link, claim or urgent lane, and comes back in
	// OpenPRResult.Stopped.
	stillMay func(context.Context) string
	Force    bool
	// HeadTime is the branch head's commit time (unix seconds; 0: unknown),
	// to tell whether anything was committed since a review finished.
	HeadTime int64
	// StackedOn are the branches Head was built on, nearest first, when
	// Base isn't given: the first with a pull request open becomes the base,
	// so the new pull request shows only its own change (WP-ADR153, WP-1670).
	StackedOn []string
	// HeadSHA is the head's commit (empty: an older wp), which tells two
	// heads committed in the same second apart (roundBehind).
	HeadSHA string
	// Message (-m) and Summary (--summary, the reply to the person with
	// {task} {pr} {url} unfilled) are kept with a held ship, so a pull
	// request Waypoint opens for it carries them (WP-1370). nil is a ship
	// that gave none (what the round holds stays); "" clears it.
	Message *string
	Summary *string
	// Change is what the checkout says about the change, for review
	// triage (WP-692); nil from an older wp, which keeps the review.
	Change *ChangeFacts
	// BodyInputs: Waypoint rendered Body from these, so a model may reword
	// its Summary (WP-55); nil for a description the opener wrote.
	BodyInputs *PRBodyInputs
}

// OpenPRResult is the pull request, whether it was already open, and a
// warning when the issue could not be linked.
type OpenPRResult struct {
	Existing bool      `json:"existing"`
	Warning  string    `json:"warning,omitempty"`
	Pull     *PRDetail `json:"pull"`
	// ShipReply is the template for the opener's last message after wp
	// ship (WP-390), filled in by the CLI.
	ShipReply string `json:"ship_reply,omitempty"`
	// PutOff quotes the linked task's steps that may not be done (WP-479),
	// for wp ship to show before the reply.
	PutOff *PutOffSteps `json:"put_off,omitempty"`
	// OtherTasks are the tasks an already-open pull request is linked to
	// when the task being finished (For, else Issue) isn't one of them: it
	// is their work, so Issue isn't linked and wp ship neither arms nor
	// babysits it (WP-608).
	OtherTasks []string `json:"other_tasks,omitempty"`
	// Held: the branch is pushed but no pull request opened, because the
	// task's adversarial review comes first (WP-688); what to tell the
	// agent. Pull is nil then.
	Held string `json:"held,omitempty"`
	// HeldNeedsAgent: Held asks the agent to act (marks to make, then wp
	// ship), so --summary doesn't end its turn (WP-1370).
	HeldNeedsAgent bool `json:"held_needs_agent"`
	// NoticeNeedsAgent: Notice is something to act on, not only news. A
	// ship whose Held and Notice need nothing ends the turn (WP-1370).
	NoticeNeedsAgent bool `json:"notice_needs_agent"`
	// Reviewed: the pull request opened after the task's review round for
	// this branch ran (WP-1370): the round's completeness check was given
	// the task's put-off notes, so wp ship doesn't ask the agent again.
	Reviewed bool `json:"reviewed,omitempty"`
	// Notice goes before the rest: what wp ship did that its intent didn't
	// say, and how to override it.
	Notice string `json:"notice,omitempty"`
	// UrgentHeld: it is urgent because the wp ship --urgent its review held
	// asked for that, so the CLI says what that costs (WP-543).
	UrgentHeld bool `json:"urgent_held,omitempty"`
	// UrgentPR: the open pull request a held wp ship --urgent put on the
	// urgent lane before its review (WP-774), for the CLI's note.
	UrgentPR int `json:"urgent_pr,omitempty"`
	// ClaimWarning: another session holds this pull request (WP-117).
	ClaimWarning string `json:"claim_warning,omitempty"`
	// Stopped: why the pull request was recorded and nothing more was done
	// with it (the opener's authority went while the forge call was out).
	Stopped string `json:"-"`
}

// OpenPullRequest opens a pull request from a branch already pushed to the
// project's repo, or returns the one open from that branch. Editors of the
// project may; an agent's pull request is remembered as its own so it can
// arm it.
func (s *Service) OpenPullRequest(ctx context.Context, actor *Actor, in OpenPRInput) (*OpenPRResult, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	if err := actor.need(PermPullsWrite); err != nil {
		return nil, err
	}
	in.Head, in.Title = strings.TrimSpace(in.Head), strings.TrimSpace(in.Title)
	in.facts = &shipFacts{}
	if in.Head == "" {
		return nil, invalid("say which branch to open the pull request from")
	}
	// The words a ship carries are kept on its round: bounded like a comment
	// (which -m becomes), and refused rather than cut (WP-1370).
	for _, w := range []struct {
		name string
		v    *string
	}{{"message", in.Message}, {"summary", in.Summary}} {
		if w.v != nil && len(*w.v) > maxCommentLen {
			return nil, invalid("the %s is too long (%d bytes; at most %d)", w.name, len(*w.v), maxCommentLen)
		}
	}
	p, err := s.projectByRef(ctx, s.db.R, in.Project)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleEditor); err != nil {
		return nil, err
	}
	// A diagnose round ships no code: its pull request would be what the
	// round is for. The round reopens as a work round once the diagnosis
	// is recorded (WP-1449). Whoever opens it, a person included.
	for _, ref := range []string{in.Issue, in.For} {
		if ref == "" {
			continue
		}
		i, _, err := s.getIssueFor(ctx, s.db.R, actor, ref, RoleEditor, false)
		if err != nil {
			continue // not a task this can be said of; the rest of the open says so
		}
		o, err := s.objectiveRow(ctx, s.db.R, i.ID)
		if err != nil {
			return nil, err // not knowing is not leave to open it
		}
		if o != nil && o.diagnoseRound() {
			return nil, invalid("%s's round %d is a diagnose round, which ships no code: record the diagnosis first (wp objective diagnose %s -f FILE). The same round then reopens as a work round, and that is the round to ship in. A person can skip the diagnosis with wp objective next %s",
				i.Key, o.Round, i.Key, i.Key)
		}
	}
	repos, err := s.ListProjectRepos(ctx, actor, p.Key)
	if err != nil {
		return nil, err
	}
	var pick *ProjectRepo
	for i, r := range repos {
		if in.Repo != "" && strings.EqualFold(r.Repo, in.Repo) {
			pick = &repos[i]
		}
	}
	switch {
	case pick != nil:
	case in.Repo != "":
		return nil, invalid("%s isn't attached to %s", in.Repo, p.Key)
	case len(repos) == 1:
		pick = &repos[0]
	case len(repos) == 0:
		return nil, invalid("%s has no repository attached, so there is nowhere to open a pull request", p.Key)
	default:
		return nil, invalid("%s has several repositories; say which with --repo", p.Key)
	}
	defer s.branchLocks.lock(fmt.Sprintf("%d/%s/%s", pick.ForgeID, strings.ToLower(pick.Repo), in.Head))()
	// A person waits on this: the forge gets longer to answer, within
	// a budget that answers before the CLI gives up (WP-265).
	fctx, cancel := context.WithTimeout(forgePatient(ctx), openPRBudget)
	defer cancel()
	existing := s.openFrom(ctx, fctx, pick, in.Head)
	alreadyOpen := existing != 0
	task := cmp(in.For, in.Issue)
	res := &OpenPRResult{}
	tagged := ""    // the task whose ci-urgent tag made a new pull request urgent
	linked := false // the task's link was made as the pull request was recorded
	if existing == 0 && in.Title == "" {
		return nil, invalid("the pull request needs a title")
	}
	if existing != 0 {
		res.Existing = true
		// Before any hold for the review: the wp ship --pr that follows
		// one doesn't repeat --base.
		if res.Notice, err = s.moveToBase(ctx, actor, existing, in.Base, task); err != nil {
			return nil, err
		}
		if in.Review == ShipReview || in.Review == ShipSkip {
			pr, err := s.prByID(ctx, s.db.R, existing)
			if err != nil {
				return nil, err
			}
			open, held, notice, err := s.reviewFirstOnce(ctx, actor, p, in, pick.Repo, in.Head, pr.BaseRef, true)
			if err != nil {
				return nil, err
			}
			if !open {
				// The push has happened: held or not, this session is on it (WP-117).
				hr := &OpenPRResult{Existing: true, Held: held, Notice: joinNotes(res.Notice, notice), HeldNeedsAgent: in.facts.heldAsks(held), NoticeNeedsAgent: in.facts.noticeAsksAgent(notice)}
				if in.Urgent {
					// The pull request is open, so --urgent applies now: the
					// ship after the review needn't say it again, and its
					// round doesn't carry it, so --off stays off (WP-774).
					if _, err := s.SetPRUrgent(ctx, actor, existing, true); err != nil {
						hr.Warning = "couldn't put it on the urgent CI lane: " + err.Error()
					} else {
						hr.UrgentPR = pr.Number
					}
				}
				hr.ClaimWarning = s.claimPR(ctx, actor, existing)
				return hr, nil
			}
			res.Notice = joinNotes(res.Notice, notice)
		}
	} else {
		cl, ok := s.Forges.(ForgeCommitter)
		if !ok {
			return nil, invalid("this forge client can't open pull requests")
		}
		f, err := s.forgeByID(ctx, pick.ForgeID)
		if err != nil {
			return nil, err
		}
		if f.token == "" {
			return nil, invalid("the forge has no API token; add one in Admin → Forges")
		}
		if strings.TrimSpace(in.Base) == "" {
			in.Base = s.askedBase(ctx, actor, task, in.Head)
		}
		base, stackNote, stackedOn := strings.TrimSpace(in.Base), "", 0
		if base == "" {
			start := time.Now()
			if base, err = cl.DefaultBranch(fctx, f, pick.Repo); err != nil {
				return nil, s.forgeUnanswered(f, time.Since(start), err)
			}
			base, stackNote, stackedOn = s.stackBase(ctx, fctx, pick, in.Head, base, in.StackedOn)
		}
		if base == in.Head {
			return nil, invalid("%s is the base branch; open the pull request from a feature branch", base)
		}
		if !in.Urgent && s.heldUrgent(ctx, actor, cmp(in.For, in.Issue), in.Head) {
			in.Urgent, res.UrgentHeld = true, true
		}
		// Autopilot's adversarial review of the task, before its pull
		// request opens (WP-688).
		open, held, notice, err := s.reviewFirstOnce(ctx, actor, p, in, pick.Repo, in.Head, base, false)
		if err != nil {
			return nil, err
		}
		if !open {
			if in.Review == "" {
				return nil, invalid("%s", strings.TrimSpace(notice+" "+held))
			}
			return &OpenPRResult{Held: held, Notice: joinNotes(stackNote, notice), HeldNeedsAgent: in.facts.heldAsks(held), NoticeNeedsAgent: in.facts.noticeAsksAgent(notice)}, nil
		}
		res.Notice = joinNotes(stackNote, notice)
		if id, err := s.resolveIssueID(ctx, s.db.R, cmp(in.For, in.Issue)); err == nil && in.Review != ShipSkip && actor.Session != 0 {
			if run, err := s.openRunIn(ctx, s.db.R, actor.Session, id); err != nil {
				s.log.WarnContext(ctx, "read the session's open run", "session", actor.Session, "err", err) // Reviewed stays false: the open says nothing of a review
			} else {
				res.Reviewed = s.reviewedRound(ctx, id, run, in.Head)
			}
		}
		// After the review: a held ship reads the tag when it really opens.
		if !in.Urgent && s.taskUrgent(ctx, actor, task) {
			in.Urgent, tagged = true, task
		}
		managed := actor.agentName() != "" && !in.Draft && !in.Ready && s.draftFirstRepo(fctx, f, pick.Repo, base)
		if managed {
			defer s.expectOwnDraft(f.ID, pick.Repo, in.Head)()
		}
		np := NewPR{Title: in.Title, Body: in.Body, Head: in.Head, Base: base, Draft: in.Draft || managed}
		if in.Urgent {
			np.Labels = []string{CIUrgentLabel}
		}
		start := time.Now()
		// An earlier attempt may have opened it and failed or died before
		// recording it: what the forge already has open from the branch is
		// adopted, not opened a second time.
		var fp *ForgePR
		if open := s.openOnForge(fctx, f, pick.Repo, in.Head, base); open != nil {
			open.wipAuto = managed && open.State == "draft"
			fp, res.Existing = open, true
		} else {
			fp, err = cl.CreatePR(fctx, f, pick.Repo, np)
		}
		if err == nil && !res.Existing {
			fp.wipAuto = managed
		}
		if err != nil {
			// A concurrent open of the same branch won: return its pull request.
			if again := s.openFrom(ctx, fctx, pick, in.Head); again != 0 {
				existing, res.Existing = again, true
			} else if open := s.openOnForge(fctx, f, pick.Repo, in.Head, base); open != nil {
				// An attempt the forge was too slow to answer opened it, and
				// no webhook has told us yet: this is the retry (WP-265).
				open.wipAuto = managed && open.State == "draft"
				fp, res.Existing = open, true
			} else {
				var ae *forgeAPIError
				if errors.As(err, &ae) && (ae.status == 409 || ae.status == 422) && strings.Contains(strings.ToLower(err.Error()), "already exist") {
					// "Already exists" with the pull request not found: it may
					// be one the forge hasn't listed yet, so this is tried
					// again, not a refusal that ends the round.
					return nil, fmt.Errorf("open the pull request: the forge says one from %s may already exist, but didn't list it: %v", in.Head, err)
				}
				return nil, fmt.Errorf("open the pull request: %w", s.forgeUnanswered(f, time.Since(start), err))
			}
		}
		if fp != nil {
			if existing, err = s.recordOpened(ctx, actor, p.ID, f, pick.Repo, fp, res.Existing, stackedOn); err != nil {
				return nil, err
			}
			if !res.Existing {
				s.queuePRBody(ctx, existing, in.Issue, in.Body, in.BodyInputs)
				// Linked at once, before any more forge calls: a pull request
				// recorded and not linked would read as someone else's (WP-1370).
				if in.Issue != "" {
					linked = true
					if pr, err := s.prByID(ctx, s.db.R, existing); err != nil {
						res.Warning = "couldn't link " + in.Issue + ": " + err.Error()
					} else if _, err := s.AddLink(ctx, actor, in.Issue, pr.URL, ""); err != nil {
						res.Warning = strings.TrimPrefix(res.Warning+"; couldn't link "+in.Issue+": "+err.Error(), "; ")
					}
				}
			}
		}
	}
	if in.stillMay != nil {
		if why := in.stillMay(ctx); why != "" {
			res.Stopped = why
			if res.Pull, err = s.GetPullRequest(ctx, actor, existing); err != nil {
				return nil, err
			}
			return res, nil
		}
	}
	if pr, err := s.prByID(ctx, s.db.R, existing); err == nil {
		// Opened into another pull request's branch: the forge says whether
		// that is a branch the repo keeps, so its stack wait reads right
		// (WP-1670). Within the forge budget, and nothing for the default.
		s.settleStack(fctx, pr, false)
	}
	if in.Urgent {
		pr, err := s.prByID(ctx, s.db.R, existing)
		if err != nil {
			return nil, err
		}
		if !pr.urgent() {
			// Already open, or a forge that labels after opening and failed.
			if _, err := s.SetPRUrgent(ctx, actor, pr.ID, true); err != nil {
				res.Warning = "couldn't put it on the urgent CI lane: " + err.Error()
			}
		}
		if tagged != "" && res.Warning == "" {
			res.Notice = strings.TrimSpace(res.Notice + " " + taggedUrgentNotice(tagged, pr.Number))
		}
	}
	if res.Existing {
		res.OtherTasks = s.otherTasks(ctx, actor, existing, task)
		if !alreadyOpen { // another attempt opened it while this one was at it
			note, err := s.moveToBase(ctx, actor, existing, in.Base, task)
			if err != nil {
				return nil, err
			}
			res.Notice = joinNotes(res.Notice, note)
		}
	}
	if in.Issue != "" && res.OtherTasks == nil && !linked {
		pr, err := s.prByID(ctx, s.db.R, existing)
		if err != nil {
			return nil, err
		}
		if _, err := s.AddLink(ctx, actor, in.Issue, pr.URL, ""); err != nil {
			res.Warning = strings.TrimPrefix(res.Warning+"; couldn't link "+in.Issue+": "+err.Error(), "; ")
		}
	}
	if res.Pull, err = s.GetPullRequest(ctx, actor, existing); err != nil {
		return nil, err
	}
	if in.facts != nil && in.facts.released != 0 {
		s.carryAudits(ctx, in.facts.released, res.Pull.ID, res.Pull.Number)
	}
	res.ClaimWarning = s.claimPR(ctx, actor, existing)
	return res, nil
}

// openPRBudget bounds the forge calls of opening a pull request, so the
// API answers within the CLI's minute even when every call is slow.
const openPRBudget = 50 * time.Second

// openOnForge is the open pull request from head into base the forge
// lists, or nil: one opened by an attempt whose answer never came.
func (s *Service) openOnForge(ctx context.Context, f *Forge, repo, head, base string) *ForgePR {
	var prs []*ForgePR
	if hf, ok := s.Forges.(ForgeHeadPRFinder); ok {
		list, err := hf.OpenPRsFromHead(ctx, f, repo, head)
		if err != nil {
			return nil
		}
		prs = list
	} else if pl, ok := s.Forges.(ForgePRLister); ok {
		list, err := pl.ListPRs(ctx, f, repo)
		if err != nil {
			return nil
		}
		prs = list
	} else {
		return nil
	}
	for _, fp := range prs {
		if fp.HeadRef == head && fp.BaseRef == base && (fp.State == "open" || fp.State == "draft") &&
			(fp.HeadRepo == "" || strings.EqualFold(fp.HeadRepo, repo)) {
			return fp
		}
	}
	return nil
}

// recordOpened stores a pull request just opened and remembers an agent as
// its author.
func (s *Service) recordOpened(ctx context.Context, actor *Actor, projectID int64, f *Forge, repo string, fp *ForgePR, adopted bool, stackedOn int) (int64, error) {
	pr, err := s.recordPR(ctx, f, repo, fp, true)
	if err != nil {
		return 0, err
	}
	agent := actor.agentName()
	if agent != "" && adopted && fp.Author != "" {
		// A pull request the forge already had open from the branch is the
		// agent's only when its author is an account Waypoint opens agents'
		// pull requests as: a person's own isn't made the agent's by being
		// adopted (WP-1370).
		var n int
		if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM pull_requests WHERE forge_id = ? AND agent != '' AND forge_author = ? COLLATE NOCASE AND id != ?`, f.ID, fp.Author, pr.ID).Scan(&n); err != nil {
			return 0, err
		}
		if n == 0 {
			agent = ""
		}
	}
	if agent != "" {
		detail := "opened “" + excerpt(pr.Title, 120) + "”"
		if pr.BaseRef != "" {
			detail += " into " + pr.BaseRef
			if stackedOn != 0 { // stackBase chose the base (WP-1670)
				detail += fmt.Sprintf(" (stacked on #%d)", stackedOn)
			}
		}
		err = s.write(ctx, actor, func(t *tx) error {
			if _, err := t.ExecContext(ctx, `UPDATE pull_requests SET agent = ? WHERE id = ? AND agent = ''`, agent, pr.ID); err != nil {
				return err
			}
			t.audit(AuditEntry{Action: "pr.open", Category: AuditControl, ProjectID: projectID, TargetType: "pr", TargetID: pr.ID,
				Target: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), URL: pr.URL, Detail: detail, About: agent})
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	s.afterPRRecorded(ctx, f, pr)
	return pr.ID, nil
}

// reviewFirstOnce is reviewFirst, asked once more when another ship of the
// same task changed its round in the same moment (ErrConflict): the second
// read sees where that one left it, so the session never has to run the
// same command again to find out (WP-1370).
func (s *Service) reviewFirstOnce(ctx context.Context, actor *Actor, p *Project, in OpenPRInput, repo, head, base string, existing bool) (bool, string, string, error) {
	open, held, notice, err := s.reviewFirst(ctx, actor, p, in, repo, head, base, existing)
	if errors.Is(err, ErrConflict) {
		*in.facts = shipFacts{}
		open, held, notice, err = s.reviewFirst(ctx, actor, p, in, repo, head, base, existing)
	}
	return open, held, notice, err
}

// reviewedRound says whether the pull request opens after a review round of
// this ship's own run (run, the session's open run of the task: a round of an
// earlier run says nothing of this change) whose audit ran (it ended done),
// and whose completeness check was asked about every note the task puts off
// as the notes stand now. The round's panel is fixed when its audit is
// requested, so notes written after that, or a round made before the notes
// were handed over, leave this false and the open shows them itself
// (WP-1370). A run that resumed an earlier run's round counts as unreviewed:
// the block is shown again, which is the safe side.
func (s *Service) reviewedRound(ctx context.Context, issueID, run int64, branch string) bool {
	if run == 0 {
		return false
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM work_log WHERE kind = 'review' AND issue_id = ? AND run_id = ? AND before_pr != ''
		AND json_extract(before_pr, '$.branch') = ? AND state IN ('running', 'done') ORDER BY id DESC`, issueID, run, branch)
	if err != nil {
		return false
	}
	var rounds []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			rounds = append(rounds, id)
		}
	}
	_ = rows.Close()
	leads := s.putOffLeads(ctx, issueID)
	for _, id := range rounds {
		ra, err := s.roundAuditOf(ctx, id)
		if err != nil || ra.id == 0 {
			continue
		}
		au, err := s.auditRow(ctx, ra.id)
		if err != nil || au.Status != AuditDone {
			continue
		}
		ask := ""
		if au.Helpers != nil {
			for _, c := range au.Helpers.Panel {
				if c.Name == "complete" {
					ask = c.Ask
				}
			}
		}
		asked := true
		for _, l := range leads {
			asked = asked && strings.Contains(ask, l)
		}
		if asked {
			return true
		}
	}
	return false
}
