package core

// Opening a pull request on the project's forge (WP-44): the CLI pushes the
// branch, Waypoint opens the pull request with the forge's token, so access
// is checked here and an agent never needs forge credentials of its own.

import (
	"context"
	"fmt"
	"slices"
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
	// its pull request (WP-688): ShipAuto, ShipReview, ShipPR or ShipSkip
	// (Reason says why). "" is a CLI from before intents: a held pull
	// request is its error. Force reviews again a change already reviewed.
	Review string
	Reason string
	// noSweep: Waypoint's own skip (a held review released), not the agent's,
	// so no other round is recorded as skipped by the agent.
	noSweep bool
	// byWaypoint: Waypoint opens it (shipHeld), not the agent's wp ship, so
	// the notes it leaves say so (WP-1325).
	byWaypoint bool
	Force      bool
	// HeadTime is the branch head's commit time (unix seconds; 0: unknown),
	// to tell whether anything was committed since a review finished.
	HeadTime int64
	// HeadSHA is the head's commit (empty: an older wp), which tells two
	// heads committed in the same second apart (roundBehind).
	HeadSHA string
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
}

// OpenPullRequest opens a pull request from a branch already pushed to the
// project's repo, or returns the one open from that branch. Editors of the
// project may; an agent's pull request is remembered as its own so it can
// arm it.
func (s *Service) OpenPullRequest(ctx context.Context, actor *Actor, in OpenPRInput) (*OpenPRResult, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	in.Head, in.Title = strings.TrimSpace(in.Head), strings.TrimSpace(in.Title)
	if in.Head == "" {
		return nil, invalid("say which branch to open the pull request from")
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
		if o, err := s.objectiveRow(ctx, s.db.R, i.ID); err == nil && o != nil && o.diagnoseRound() {
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
	var existing int64
	_ = s.db.R.QueryRowContext(ctx, `SELECT id FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND head_ref = ? AND state IN ('open','draft') ORDER BY id DESC LIMIT 1`,
		pick.ForgeID, pick.Repo, in.Head).Scan(&existing)
	res := &OpenPRResult{}
	tagged := "" // the task whose ci-urgent tag made a new pull request urgent
	if existing == 0 && in.Title == "" {
		return nil, invalid("the pull request needs a title")
	}
	if existing != 0 {
		res.Existing = true
		if in.Review == ShipReview || in.Review == ShipSkip {
			pr, err := s.prByID(ctx, s.db.R, existing)
			if err != nil {
				return nil, err
			}
			open, held, notice, err := s.reviewFirst(ctx, actor, p, in, pick.Repo, in.Head, pr.BaseRef, true)
			if err != nil {
				return nil, err
			}
			if !open {
				// The push has happened: held or not, this session is on it (WP-117).
				hr := &OpenPRResult{Existing: true, Held: held, Notice: notice}
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
			res.Notice = notice
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
		// A person waits on this: the forge gets longer to answer, within
		// a budget that answers before the CLI gives up (WP-265).
		fctx, cancel := context.WithTimeout(forgePatient(ctx), openPRBudget)
		defer cancel()
		base := strings.TrimSpace(in.Base)
		if base == "" {
			start := time.Now()
			if base, err = cl.DefaultBranch(fctx, f, pick.Repo); err != nil {
				return nil, s.forgeUnanswered(f, time.Since(start), err)
			}
		}
		if base == in.Head {
			return nil, invalid("%s is the base branch; open the pull request from a feature branch", base)
		}
		if !in.Urgent && s.heldUrgent(ctx, actor, cmp(in.For, in.Issue), in.Head) {
			in.Urgent, res.UrgentHeld = true, true
		}
		// Autopilot's adversarial review of the task, before its pull
		// request opens (WP-688).
		open, held, notice, err := s.reviewFirst(ctx, actor, p, in, pick.Repo, in.Head, base, false)
		if err != nil {
			return nil, err
		}
		if !open {
			if in.Review == "" {
				return nil, invalid("%s", strings.TrimSpace(notice+" "+held))
			}
			return &OpenPRResult{Held: held, Notice: notice}, nil
		}
		res.Notice = notice
		// After the review: a held ship reads the tag when it really opens.
		if task := cmp(in.For, in.Issue); !in.Urgent && s.taskUrgent(ctx, actor, task) {
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
		fp, err := cl.CreatePR(fctx, f, pick.Repo, np)
		if err == nil {
			fp.wipAuto = managed
		}
		if err != nil {
			// A concurrent open of the same branch won: return its pull request.
			var again int64
			if s.db.R.QueryRowContext(ctx, `SELECT id FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND head_ref = ? AND state IN ('open','draft') ORDER BY id DESC LIMIT 1`,
				pick.ForgeID, pick.Repo, in.Head).Scan(&again) == nil && again != 0 {
				existing, res.Existing = again, true
			} else if open := s.openOnForge(fctx, f, pick.Repo, in.Head, base); open != nil {
				// An attempt the forge was too slow to answer opened it, and
				// no webhook has told us yet: this is the retry (WP-265).
				open.wipAuto = managed && open.State == "draft"
				fp, res.Existing = open, true
			} else {
				return nil, fmt.Errorf("open the pull request: %w", s.forgeUnanswered(f, time.Since(start), err))
			}
		}
		if fp != nil {
			if existing, err = s.recordOpened(ctx, actor, p.ID, f, pick.Repo, fp); err != nil {
				return nil, err
			}
			if !res.Existing {
				s.queuePRBody(ctx, existing, in.Issue, in.Body, in.BodyInputs)
			}
		}
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
	task := in.For
	if task == "" {
		task = in.Issue
	}
	if task != "" && res.Existing {
		if cur, err := s.GetPullRequest(ctx, actor, existing); err == nil && len(cur.Issues) > 0 {
			var keys []string
			for _, i := range cur.Issues {
				keys = append(keys, i.Key)
			}
			if !slices.ContainsFunc(keys, func(k string) bool { return strings.EqualFold(k, task) }) {
				res.OtherTasks = keys
			}
		}
	}
	if in.Issue != "" && res.OtherTasks == nil {
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
	res.ClaimWarning = s.claimPR(ctx, actor, existing)
	return res, nil
}

// openPRBudget bounds the forge calls of opening a pull request, so the
// API answers within the CLI's minute even when every call is slow.
const openPRBudget = 50 * time.Second

// openOnForge is the open pull request from head into base the forge
// lists, or nil: one opened by an attempt whose answer never came.
func (s *Service) openOnForge(ctx context.Context, f *Forge, repo, head, base string) *ForgePR {
	pl, ok := s.Forges.(ForgePRLister)
	if !ok {
		return nil
	}
	prs, err := pl.ListPRs(ctx, f, repo)
	if err != nil {
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
func (s *Service) recordOpened(ctx context.Context, actor *Actor, projectID int64, f *Forge, repo string, fp *ForgePR) (int64, error) {
	pr, err := s.recordPR(ctx, f, repo, fp, true)
	if err != nil {
		return 0, err
	}
	if agent := actor.agentName(); agent != "" {
		err = s.write(ctx, actor, func(t *tx) error {
			if _, err := t.ExecContext(ctx, `UPDATE pull_requests SET agent = ? WHERE id = ? AND agent = ''`, agent, pr.ID); err != nil {
				return err
			}
			t.audit(AuditEntry{Action: "pr.open", Category: AuditControl, ProjectID: projectID, TargetType: "pr", TargetID: pr.ID,
				Target: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), URL: pr.URL, Detail: "opened “" + excerpt(pr.Title, 120) + "”", About: agent})
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	s.afterPRRecorded(ctx, f, pr)
	return pr.ID, nil
}
