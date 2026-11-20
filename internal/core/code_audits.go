package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Audits (WP-348, WP-ADR20): an adversarial review a person, an agent or an
// automation asks for, of a repository, a branch, a commit, a range of
// commits or a pull request. A model reads the requester's runner's checkout
// read-only (by default the one the LLM gateway picks, defaultAuditChain;
// a named CLI's model runs on the runner) and reports findings in a fixed
// form, which Waypoint keeps.
// The model never changes anything. A pull request's audit is posted on it;
// a task for Claude to check and fix the findings is made only when someone
// asks for one. The findings are the model's claims: they never feed the
// review gate, babysit or a merge.

const (
	auditSource = "audit"
	// EvCodeAudit: an audit changed (New: its status; Field "fix" when a
	// fix task was asked for). Not the audit log, which is something else.
	EvCodeAudit = "code_audit.updated"
	// NotifyAudit: an audit you asked for finished.
	NotifyAudit = "audit"

	AuditQueued    = "queued"
	AuditRunning   = "running"
	AuditDone      = "done"
	AuditFailed    = "failed"
	AuditLimited   = "limited" // a usage limit stopped every model
	AuditCancelled = "cancelled"

	auditTimeout    = 2700 // seconds: Grok at high effort reads a large repository slowly
	auditMaxRetries = 3    // times a chain whose models were all busy is tried again
	// auditUnreadableRetries: times the chain's last runner model is asked
	// again after an answer out of form.
	auditUnreadableRetries = 1
	auditRetryStep         = 15 * time.Minute
	auditInstructionsMax   = 2000
	auditMaxFindings       = 30
	auditMaxModels         = 6
)

var (
	auditTargets       = []string{"repo", "branch", "commit", "range", "recent", "pr"}
	auditSeverities    = []string{"critical", "high", "medium", "low"}
	auditFindingStates = []string{"open", "confirmed", "fixed", "rejected"}
)

// DefaultAuditInstructions is what an audit looks for when nobody says.
const DefaultAuditInstructions = "Find the defects most likely to hurt the people who use or run this code: bugs, security holes, lost data and errors handled wrongly."

// cursorAuditChain is what audits nobody named a model for ran on before the
// LLM gateway could run them, and still do on a Waypoint without one:
// Grok 4.7 at high effort and standard speed (the -fast slugs cost twice as
// much of Cursor's pool), then at medium effort. cursor-agent names each
// setting as its own slug (WP-347).
var cursorAuditChain = []AgentModel{{CLI: "cursor", Model: "grok-4.7-high"}, {CLI: "cursor", Model: "grok-4.7-medium"}}

// defaultAuditChain is the models of an audit nobody named any for: a pull
// request's own audit, an automation's, a review round's, wp audit by hand.
// It is the gateway's audit purpose alone (WP-871, WP-ADR84): the
// gateway chooses the model, moves to its next when one fails, and knows
// which of its upstreams has usage left, so Waypoint keeps no order of its
// own behind it and calls no coding plan's CLI to find out that it is spent.
// Grok is the gateway's to serve too. Only a Waypoint with no
// gateway asks Cursor on the runner.
func (s *Service) defaultAuditChain(ctx context.Context) []AgentModel {
	if _, _, why := s.gatewayTasksLLM(ctx); why == "" {
		return []AgentModel{{CLI: GatewayCLI, Model: gatewayAuditModel}}
	}
	return cursorAuditChain
}

// CodeAudit is an audit as callers see it.
type CodeAudit struct {
	ID           int64  `json:"id"`
	Project      string `json:"project"`
	ProjectName  string `json:"project_name"`
	Repo         string `json:"repo"`
	Target       string `json:"target"`
	Ref          string `json:"ref,omitempty"`
	Base         string `json:"base,omitempty"`
	PRID         *int64 `json:"pr_id,omitempty"`
	PRNumber     int    `json:"pr_number,omitempty"`
	PRURL        string `json:"pr_url,omitempty"`
	Title        string `json:"title"`
	Instructions string `json:"instructions"`
	// The criteria (WP-548): a recent target's period as asked (7d,
	// 2026-10-28) and its start; the focus areas' keys; the paths read or
	// left out; the lowest severity reported ("": all); the most findings.
	Since       string     `json:"since,omitempty"`
	SinceAt     *time.Time `json:"since_at,omitempty"`
	Focus       []string   `json:"focus"`
	Paths       []string   `json:"paths"`
	Exclude     []string   `json:"exclude"`
	MinSeverity string     `json:"min_severity,omitempty"`
	MaxFindings int        `json:"max_findings,omitempty"`
	// What the runner read: the commits the change ran from and to, and
	// how many commits that is (none when it isn't known).
	FromSHA     string `json:"from_sha,omitempty"`
	ToSHA       string `json:"to_sha,omitempty"`
	CommitCount *int   `json:"commit_count,omitempty"`
	// Purpose is "" for an audit, footprint for the drift guard's check of
	// a pull request's fix-up rounds against its task (WP-440).
	Purpose string `json:"purpose,omitempty"`
	// Weakest is the quality bar of an audit through the LLM gateway:
	// the weakest model it accepts, by the name the gateway rates (WP-916,
	// trustedAuditBar). "": any model of its audit purpose.
	Weakest     string             `json:"weakest,omitempty"`
	Models      []string           `json:"models"`
	PostComment bool               `json:"post_comment"`
	Fix         bool               `json:"fix"`
	Status      string             `json:"status"`
	Slot        int                `json:"slot"`
	JobID       *int64             `json:"job_id,omitempty"`
	JobState    string             `json:"job_state,omitempty"`
	Attempts    []AuditAttempt     `json:"attempts"`
	Retries     int                `json:"retries"`
	RetryAt     *time.Time         `json:"retry_at,omitempty"`
	Model       string             `json:"model,omitempty"`
	Summary     string             `json:"summary,omitempty"`
	Answer      string             `json:"answer,omitempty"`
	Error       string             `json:"error,omitempty"`
	Tokens      map[string]any     `json:"tokens,omitempty"`
	CommentURL  string             `json:"comment_url,omitempty"`
	FixIssue    string             `json:"fix_issue,omitempty"`
	RequestedBy *UserRef           `json:"requested_by,omitempty"`
	Agent       string             `json:"agent,omitempty"`
	Via         string             `json:"via,omitempty"`
	Automation  string             `json:"automation,omitempty"`
	CreatedAt   time.Time          `json:"created_at"`
	StartedAt   *time.Time         `json:"started_at,omitempty"`
	FinishedAt  *time.Time         `json:"finished_at,omitempty"`
	Counts      AuditCounts        `json:"counts"`
	Findings    []CodeAuditFinding `json:"findings,omitempty"`
	URL         string             `json:"url"`
	CanEdit     bool               `json:"can_edit"`
	// Helpers are the Haiku checks the runner runs around the model
	// (WP-1320, review_helpers.go), and HelperRuns how each went (read
	// with the findings).
	Helpers    *AuditHelpers    `json:"helpers,omitempty"`
	HelperRuns []AuditHelperRun `json:"helper_runs,omitempty"`

	projectID    int64
	forgeID      int64
	chain        []AgentModel
	prompt       string
	ownerID      int64
	requestedBy  int64
	automationID int64
	sessionID    int64
	blocking     bool
	fixIssueID   int64
	// authorModel is the model that wrote the code, when Waypoint knows
	// (a review round's session): a gateway audit leaves its family out.
	authorModel string
	// reviewText is the reviewer's brief a person set for review rounds
	// (review_profile.go), and roundContext the task the change was made
	// for with what earlier rounds raised: a review round's audit only.
	reviewText, roundContext string
	// claims are what a check of a round's marks checks (AuditCheck,
	// checkMarks): the rejected and fixed findings with the session's notes.
	claims string
}

// AuditCounts sums an audit's findings.
type AuditCounts struct {
	Total int `json:"total"`
	// Open counts the findings still to do: not yet decided, or confirmed
	// and not yet fixed.
	Open     int `json:"open"`
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
}

// CodeAuditFinding is one thing an audit reported.
type CodeAuditFinding struct {
	ID       string     `json:"id"` // "12.3": audit 12, finding 3
	Ordinal  int        `json:"ordinal"`
	Title    string     `json:"title"`
	Path     string     `json:"path,omitempty"`
	Line     int        `json:"line,omitempty"`
	Severity string     `json:"severity"`
	Problem  string     `json:"problem"`
	Confirm  string     `json:"confirm,omitempty"`
	State    string     `json:"state"`
	Note     string     `json:"note,omitempty"`
	Issue    string     `json:"issue,omitempty"`
	By       string     `json:"by,omitempty"` // who decided its state
	At       *time.Time `json:"at,omitempty"`
	URL      string     `json:"url,omitempty"` // the line on the forge
	// Verdict is a Haiku check's of the finding against the code
	// (confirmed, refuted, unsupported; "" not checked), VerdictNote its
	// evidence, and Lead the panel lead (check.n) the model says it came
	// from (WP-1320).
	Verdict     string `json:"verdict,omitempty"`
	VerdictNote string `json:"verdict_note,omitempty"`
	Lead        string `json:"lead,omitempty"`
}

// AuditAttempt is one model's go at an audit.
type AuditAttempt struct {
	Model   string `json:"model"`
	Job     int64  `json:"job,omitempty"`
	Round   int    `json:"round"`   // 0, then 1… for each retry of a busy chain
	Outcome string `json:"outcome"` // answered, unreadable, busy, spent, timeout, model, unavailable, error, cancelled
	Error   string `json:"error,omitempty"`
	At      int64  `json:"at"`
}

// AuditInput asks for an audit.
type AuditInput struct {
	Project string `json:"project"`
	Repo    string `json:"repo"`
	// Target is repo, branch, commit, range, recent or pr; empty: pr when
	// PR or PRID is set, recent when Since is, range when Ref has "..",
	// else repo.
	Target string `json:"target"`
	// Ref: the branch, the commit, the range ("A..B"), the branch whose
	// recent changes are read, or what to check out for a repository's
	// audit (empty: the default branch).
	Ref string `json:"ref"`
	// Since: a recent target's period, 24h, 7d, 2w, 1mo or a date
	// (default 7d): what landed on the branch since then.
	Since string `json:"since"`
	// Focus: areas to look hardest at (AuditDefaults.Focus keys).
	Focus []string `json:"focus"`
	// Paths and Exclude: the parts of the repository to read, and to leave
	// out (paths or globs from its root).
	Paths   []string `json:"paths"`
	Exclude []string `json:"exclude"`
	// MinSeverity: report nothing below it (empty: everything).
	MinSeverity string `json:"min_severity"`
	// MaxFindings: at most this many (0: 15; up to 30).
	MaxFindings int `json:"max_findings"`
	// Base: what a branch is compared with (empty: the default branch).
	Base         string   `json:"base"`
	PR           int      `json:"pr"`    // a pull request's number in the repository
	PRID         int64    `json:"pr_id"` // or its id in Waypoint
	Instructions string   `json:"instructions"`
	Models       []string `json:"models"` // "cursor/grok-4.7-high", tried in order
	// Comment posts a pull request's audit on it (default yes).
	Comment  *bool  `json:"comment"`
	Fix      bool   `json:"fix"`      // a task for Claude to check and fix the findings
	Blocking bool   `json:"blocking"` // the asking session waits on it
	Via      string `json:"via"`      // web, cli, api, automation

	requestKey string
	chain      []AgentModel
	// owner is whose runner runs it when Waypoint itself asks (WP-355): the
	// system has no runner of its own.
	owner int64
	// purpose is what Waypoint asked it for ("" an audit, AuditFootprint
	// the drift guard's check, WP-440), and prContext then replaces the
	// pull request's description in its prompt.
	purpose   string
	prContext string
	// authorModel: see CodeAudit.authorModel.
	authorModel string
	// weakest: see CodeAudit.Weakest. Only Waypoint sets one (a review
	// round of a high-stakes change, a rerun of such an audit).
	weakest string
	// reviewText and roundContext: see CodeAudit's. Only a review round's
	// audit has them (WP-1320).
	reviewText, roundContext string
	// claims: see CodeAudit's.
	claims string
	// helpers: the Haiku checks around the model (CodeAudit.Helpers); nil
	// leaves them to auditHelpersFor.
	helpers *AuditHelpers
}

// AuditDefaults is what a request starts from.
type AuditDefaults struct {
	Instructions string   `json:"instructions"`
	Models       []string `json:"models"`
	Targets      []string `json:"targets"`
	Severities   []string `json:"severities"`
	States       []string `json:"states"`
	CLIs         []string `json:"clis"`
	// What criteria can say (WP-548).
	Focus       []AuditFocus  `json:"focus"`
	Periods     []AuditPeriod `json:"periods"`
	Since       string        `json:"since"`
	MaxFindings int           `json:"max_findings"`
	FindingsCap int           `json:"findings_cap"`
	PathsCap    int           `json:"paths_cap"`
	// Gateway: an LLM gateway is set up, so an audit can name it
	// (gateway/audit). Without one the form doesn't offer it (WP-459).
	Gateway bool `json:"gateway"`
}

func (s *Service) AuditDefaults(ctx context.Context) AuditDefaults {
	_, _, noGateway := s.gatewayTasksLLM(ctx)
	return AuditDefaults{Gateway: noGateway == "", Instructions: DefaultAuditInstructions, Models: auditModelNames(s.defaultAuditChain(ctx)), Targets: auditTargets,
		Severities: auditSeverities, States: auditFindingStates, CLIs: runnerCLIs,
		Focus: auditFocusAreas, Periods: auditPeriods, Since: auditDefaultSince, MaxFindings: auditDefaultFindings, FindingsCap: auditMaxFindings, PathsCap: auditMaxPaths}
}

// auditModelName is how a chain entry is written: cli/model.
func auditModelName(m AgentModel) string {
	if m.Model == "" {
		return m.CLI
	}
	return m.CLI + "/" + m.Model
}

func auditModelNames(chain []AgentModel) []string {
	out := make([]string, 0, len(chain))
	for _, m := range chain {
		out = append(out, auditModelName(m))
	}
	return out
}

// parseAuditModel reads "cursor/grok-4.7-high", "claude/opus", "claude"; a
// bare Grok slug is Cursor's and a bare Claude model is Claude Code's.
// "gateway/audit" (or "gateway") is the gateway's own agent on the
// models of its audit purpose (audit_gateway.go); the name before WP-1187
// is read too, for one release.
func parseAuditModel(v string) (AgentModel, error) {
	v = strings.TrimSpace(v)
	var m AgentModel
	if cli, model, ok := strings.Cut(v, "/"); strings.EqualFold(cli, GatewayCLI) || strings.EqualFold(cli, "openllms") {
		m = AgentModel{CLI: GatewayCLI, Model: gatewayAuditModel}
		if ok && strings.TrimSpace(model) != "" {
			m.Model = strings.TrimSpace(model)
		}
	} else if ok && slices.Contains(runnerCLIs, strings.ToLower(cli)) {
		m = AgentModel{CLI: strings.ToLower(cli), Model: strings.TrimSpace(model)}
	} else if slices.Contains(runnerCLIs, strings.ToLower(v)) {
		m = AgentModel{CLI: strings.ToLower(v)}
	} else {
		low := strings.ToLower(v)
		switch {
		case strings.HasPrefix(low, "grok"):
			m = AgentModel{CLI: "cursor", Model: v}
		case strings.HasPrefix(low, "claude-") || slices.Contains([]string{"opus", "sonnet", "haiku", "fable"}, low):
			m = AgentModel{CLI: "claude", Model: v}
		default:
			return m, invalid("say which CLI runs %q: cli/model, with cli one of %s (or gateway/audit: the gateway's own agent)", v, strings.Join(runnerCLIs, ", "))
		}
	}
	if m.Model != "" && !modelRE.MatchString(m.Model) {
		return m, invalid("model %q isn't allowed", m.Model)
	}
	return m, nil
}

// ---- asking for one ----

// RequestAudit records an audit and sets it going on the runner of whoever
// asked (an automation: of the person who last saved it).
func (s *Service) RequestAudit(ctx context.Context, actor *Actor, in AuditInput) (*CodeAudit, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	if err := actor.need(PermAuditsRun); err != nil {
		return nil, err
	}
	if in.requestKey != "" {
		var id int64
		if err := s.db.R.QueryRowContext(ctx, `SELECT id FROM code_audits WHERE request_key = ?`, in.requestKey).Scan(&id); err == nil {
			return s.auditFor(ctx, actor, id, false)
		}
	}
	var p *Project
	var known *PullRequest
	var err error
	if in.PRID != 0 {
		if known, err = s.prByID(ctx, s.db.R, in.PRID); err != nil {
			return nil, err
		}
		pid := s.attachedProject(ctx, s.db.R, known.ForgeID, known.Repo)
		if pid == 0 {
			return nil, invalid("%s isn't attached to a project, and an audit belongs to one", known.Repo)
		}
		if p, err = s.projectByID(ctx, s.db.R, pid); err != nil {
			return nil, err
		}
		in.PR, in.Repo, in.Target = known.Number, known.Repo, "pr"
	} else {
		if strings.TrimSpace(in.Project) == "" {
			return nil, invalid("say which project")
		}
		if p, err = s.projectByRef(ctx, s.db.R, in.Project); err != nil {
			return nil, err
		}
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleEditor); err != nil {
		return nil, err
	}
	target := strings.ToLower(strings.TrimSpace(in.Target))
	if target == "" {
		switch {
		case in.PR != 0:
			target = "pr"
		case strings.TrimSpace(in.Since) != "":
			target = "recent"
		case strings.Contains(in.Ref, ".."):
			target = "range"
		default:
			target = "repo"
		}
	}
	if !slices.Contains(auditTargets, target) {
		return nil, invalid("an audit's target is one of %s", strings.Join(auditTargets, ", "))
	}
	repo, err := s.auditRepo(ctx, p, in, target, known)
	if err != nil {
		return nil, err
	}
	chain := in.chain
	if chain == nil {
		for _, v := range in.Models {
			if strings.TrimSpace(v) == "" {
				continue
			}
			m, err := parseAuditModel(v)
			if err != nil {
				return nil, err
			}
			chain = append(chain, m)
		}
	}
	if len(chain) == 0 {
		chain = s.defaultAuditChain(ctx)
	}
	if len(chain) > auditMaxModels {
		return nil, invalid("an audit can try at most %d models", auditMaxModels)
	}
	if in.chain == nil {
		// A chain Waypoint chose skips the gateway when it has none; a
		// person who named it is told.
		for _, m := range chain {
			if m.CLI != GatewayCLI {
				continue
			}
			if _, _, why := s.gatewayTasksLLM(ctx); why != "" {
				return nil, invalid("%s can't run: %s", auditModelName(m), why)
			}
		}
	}
	owner := actor.id()
	if actor.Automation != nil {
		if owner = automationRunnerOwner(actor.Automation); owner == 0 {
			return nil, invalid("this automation has no owner for its runners: an agent saved it, or it was never saved. Save it yourself to use your runners")
		}
	}
	if owner == 0 && actor.Automation == nil {
		owner = in.owner
	}
	if owner == 0 {
		return nil, invalid("an audit runs on your own runner: sign in as a person")
	}
	if why := s.auditRunnersWhy(ctx, owner, chain); why != "" {
		return nil, invalid("%s", why)
	}
	instructions := strings.TrimSpace(in.Instructions)
	if instructions == "" {
		instructions = DefaultAuditInstructions
	}
	if utf8.RuneCountInString(instructions) > auditInstructionsMax {
		return nil, invalid("say what to look for in at most %d characters", auditInstructionsMax)
	}
	crit, err := cleanAuditCriteria(in)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Since) != "" && target != "recent" {
		return nil, invalid("a period (since) is for recent changes: what landed on a branch since then")
	}
	a := &CodeAudit{Target: target, Repo: repo.Repo, Instructions: instructions, chain: chain, Fix: in.Fix,
		Focus: crit.focus, Paths: crit.paths, Exclude: crit.exclude, MinSeverity: crit.minSeverity, MaxFindings: crit.maxFindings}
	var prContext string
	var prID any
	switch target {
	case "pr":
		if in.PR <= 0 {
			return nil, invalid("say which pull request")
		}
		fprs, ok := s.Forges.(ForgePRs)
		if !ok {
			return nil, invalid("this forge can't show pull requests")
		}
		fp, err := fprs.GetPR(ctx, botForge(repo.Forge), repo.Repo, in.PR)
		if err != nil {
			return nil, invalid("can't read %s#%d from the forge: %s", repo.Repo, in.PR, PublicError(err))
		}
		if why := s.runnerGate(ctx, p.ID, repo.Repo, fp); why != "" {
			return nil, invalid("%s", why)
		}
		switch {
		case fp.State == "merged" && fp.MergeSHA != "":
			a.Ref, a.Base = fp.MergeSHA, fp.MergeSHA+"^1"
		case fp.State == "merged" || fp.State == "closed":
			a.Ref, a.Base = prHeadRef(repo.Forge, in.PR), fp.BaseRef
		default:
			a.Ref, a.Base = fp.HeadSHA, fp.BaseRef
		}
		if a.Base == "" {
			if fc, ok := s.Forges.(ForgeCommitter); ok {
				a.Base, _ = fc.DefaultBranch(ctx, botForge(repo.Forge), repo.Repo)
			}
		}
		a.PRNumber = fp.Number
		a.Title = fmt.Sprintf("%s#%d: %s", repo.Repo, fp.Number, excerpt(fp.Title, 160))
		a.PostComment = in.Comment == nil || *in.Comment
		if known == nil {
			known, _ = s.prByNumber(ctx, s.db.R, repo.Forge.ID, repo.Repo, fp.Number)
		}
		if known != nil {
			prID = known.ID
			if in.authorModel == "" {
				in.authorModel = s.prAuthorModel(ctx, known)
			}
		}
		prContext = fmt.Sprintf("This is pull request %s#%d, %q, by %s, at its %s %s. Its description, as its author wrote it:\n\n<untrusted>\n%s\n</untrusted>",
			repo.Repo, fp.Number, fp.Title, cmp(fp.Author, "someone"), map[bool]string{true: "merge commit", false: "head"}[fp.State == "merged" && fp.MergeSHA != ""],
			short(cmp(fp.MergeSHA, fp.HeadSHA)), excerpt(untrusted(cmp(strings.TrimSpace(fp.Body), "(none)")), 4000))
		if in.prContext != "" {
			prContext = in.prContext
		}
	case "branch":
		// No branch: the default branch, which is the whole repository.
		a.Ref, a.Base = strings.TrimSpace(in.Ref), strings.TrimSpace(in.Base)
		if a.Base == "" || a.Ref == "" {
			if fc, ok := s.Forges.(ForgeCommitter); ok {
				a.Base, _ = fc.DefaultBranch(ctx, botForge(repo.Forge), repo.Repo)
			}
			if a.Base == "" {
				return nil, invalid("can't find %s's default branch: say what to compare the branch with (--base)", repo.Repo)
			}
		}
		if a.Ref == "" {
			a.Ref = a.Base
		}
		if a.Base == a.Ref {
			// The default branch itself: there is no change, so the whole tree.
			a.Target, a.Base = "repo", ""
			a.Title = fmt.Sprintf("%s at %s", repo.Repo, a.Ref)
		} else {
			a.Title = fmt.Sprintf("%s branch %s (against %s)", repo.Repo, a.Ref, a.Base)
		}
	case "commit":
		a.Ref = strings.TrimSpace(in.Ref)
		if a.Ref == "" {
			return nil, invalid("say which commit")
		}
		a.Base = a.Ref + "^1"
		a.Title = fmt.Sprintf("%s commit %s", repo.Repo, short(a.Ref))
	case "range":
		from, to, ok := strings.Cut(strings.TrimSpace(in.Ref), "...")
		if !ok {
			from, to, ok = strings.Cut(strings.TrimSpace(in.Ref), "..")
		}
		if !ok || from == "" || to == "" {
			return nil, invalid("a range is FROM..TO")
		}
		a.Ref, a.Base = to, from
		a.Title = fmt.Sprintf("%s %s..%s", repo.Repo, short(from), short(to))
	case "recent":
		// What landed on a branch (default: the default branch) since a
		// time: the runner finds the branch's last commit before it.
		a.Ref = strings.TrimSpace(in.Ref)
		if a.Ref == "" {
			if fc, ok := s.Forges.(ForgeCommitter); ok {
				a.Ref, _ = fc.DefaultBranch(ctx, botForge(repo.Forge), repo.Repo)
			}
		}
		at, spec, err := parseAuditSince(in.Since, s.now())
		if err != nil {
			return nil, err
		}
		a.Since, a.SinceAt = spec, &at
		a.Title = fmt.Sprintf("%s %s, %s", repo.Repo, cmp(a.Ref, "default branch"), sinceWords(spec, at))
	case "repo":
		a.Ref = strings.TrimSpace(in.Ref)
		a.Title = repo.Repo
		if a.Ref != "" {
			a.Title += " at " + a.Ref
		}
	}
	if len(a.Paths) > 0 {
		a.Title += " (" + excerpt(strings.Join(a.Paths, ", "), 60) + ")"
	}
	for _, r := range []string{a.Ref, a.Base} {
		if r != "" && (!gitRefRE.MatchString(r) || strings.Contains(r, "..")) {
			return nil, invalid("%q isn't a branch, tag or commit", r)
		}
	}
	a.Purpose, a.Weakest = in.purpose, in.weakest
	a.reviewText, a.roundContext, a.claims = in.reviewText, in.roundContext, in.claims
	if a.Purpose == AuditObjective {
		// What Waypoint tells a judge of the task and its round
		// (objectiveJudgeContext); it reads the repository, not a pull request.
		prContext = in.prContext
	}
	a.Helpers = auditHelpersFor(in, prContext)
	a.prompt = codeAuditPrompt(a, prContext)
	via := strings.TrimSpace(in.Via)
	switch {
	case actor.Automation != nil:
		via = "automation"
	case via == "":
		via = "api"
	}
	var by, session, auto, key any
	if actor.User != nil && actor.User.ID != 0 {
		by = actor.User.ID
	}
	if actor.Session != 0 {
		session = actor.Session
	}
	if id := actor.viaAutomation(); id != nil {
		auto = id
	}
	if in.requestKey != "" {
		key = in.requestKey
	}
	models, _ := json.Marshal(chain)
	var sinceAt int64
	if a.SinceAt != nil {
		sinceAt = a.SinceAt.UnixMilli()
	}
	var id int64
	err = s.write(ctx, actor, func(t *tx) error {
		if err := t.QueryRowContext(ctx, `INSERT INTO code_audits(project_id, forge_id, repo, target, ref, base, pr_id, pr_number, title, instructions, prompt, models,
			post_comment, fix, owner_id, requested_by, requested_agent, requested_via, automation_id, session_id, blocking, request_key, created_at,
			since_spec, since_at, focus, paths, exclude, min_severity, max_findings, purpose, author_model, weakest, helpers)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,coalesce(?, ''),?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`,
			p.ID, repo.Forge.ID, repo.Repo, a.Target, a.Ref, a.Base, prID, a.PRNumber, a.Title, a.Instructions, a.prompt, string(models),
			a.PostComment, a.Fix, owner, by, actor.agentName(), via, auto, session, in.Blocking, key, s.nowMS(),
			a.Since, sinceAt, jsonList(a.Focus), jsonList(a.Paths), jsonList(a.Exclude), a.MinSeverity, a.MaxFindings, a.Purpose, excerpt(strings.TrimSpace(in.authorModel), 100), a.Weakest, helpersJSON(a.Helpers)).Scan(&id); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvCodeAudit, ProjectID: p.ID, New: AuditQueued, Data: map[string]any{"audit": id, "title": a.Title}})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.auditFor(ctx, actor, id, true)
}

// prHeadRef is the forge's ref for a pull request's head, which outlives
// its branch.
func prHeadRef(f *Forge, number int) string {
	if f.Kind == "gitlab" {
		return fmt.Sprintf("refs/merge-requests/%d/head", number)
	}
	return fmt.Sprintf("refs/pull/%d/head", number)
}

// auditRepo is the project's repository the audit reads.
func (s *Service) auditRepo(ctx context.Context, p *Project, in AuditInput, target string, known *PullRequest) (*stepRepo, error) {
	repos, err := s.projectRepoList(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	if len(repos) == 0 {
		return nil, invalid("%s has no repository: attach one in its settings", p.Key)
	}
	name := strings.TrimSpace(in.Repo)
	if known != nil {
		for i := range repos {
			if repos[i].Forge.ID == known.ForgeID && strings.EqualFold(repos[i].Repo, known.Repo) {
				return &repos[i], nil
			}
		}
		return nil, invalid("%s isn't a repository of %s", known.Repo, p.Key)
	}
	if name != "" {
		for i := range repos {
			if strings.EqualFold(repos[i].Repo, name) {
				return &repos[i], nil
			}
		}
		return nil, invalid("%s isn't a repository of %s", name, p.Key)
	}
	if len(repos) == 1 {
		return &repos[0], nil
	}
	if target == "pr" && in.PR > 0 {
		var hits []int
		for i := range repos {
			if pr, _ := s.prByNumber(ctx, s.db.R, repos[i].Forge.ID, repos[i].Repo, in.PR); pr != nil {
				hits = append(hits, i)
			}
		}
		if len(hits) == 1 {
			return &repos[hits[0]], nil
		}
	}
	var names []string
	for _, r := range repos {
		names = append(names, r.Repo)
	}
	return nil, invalid("%s has several repositories: say which one (%s)", p.Key, strings.Join(names, ", "))
}

// auditRunnersWhy says why no runner of owner could run any model of the
// chain ("" when one can).
func (s *Service) auditRunnersWhy(ctx context.Context, owner int64, chain []AgentModel) string {
	runners, err := s.ownerRunners(ctx, owner)
	if err != nil {
		return "can't read the runners"
	}
	who := "you have"
	if owner != 0 {
		if u, err := s.userByID(ctx, s.db.R, owner); err == nil {
			who = u.DisplayName + " has"
		}
	}
	if len(runners) == 0 {
		return who + " no runner to run the audit on: connect one with wp runner (Settings → Runners)"
	}
	for _, m := range chain {
		if slices.ContainsFunc(runners, func(r *Runner) bool { return runnerCanTake(r, jobNeeds{kind: JobAudit, cli: m.CLI}) }) {
			return ""
		}
	}
	for _, m := range chain {
		if slices.ContainsFunc(runners, func(r *Runner) bool { return runnerCanTake(r, jobNeeds{cli: m.CLI}) }) {
			return oldAuditRunnerWhy(runners, m.CLI)
		}
	}
	var clis []string
	for _, m := range chain {
		if m.CLI != GatewayCLI && !slices.Contains(clis, m.CLI) {
			clis = append(clis, m.CLI)
		}
	}
	if len(clis) == 0 {
		// Only the gateway's models: any runner can send one, once its wp
		// knows how.
		return "no runner of yours runs a wp that can send an audit to the gateway yet: it updates itself, or run wp upgrade"
	}
	name := strings.Join(clis, " or ")
	if name == "cursor" {
		name = "cursor-agent"
	}
	return fmt.Sprintf("no runner of yours has %s installed and signed in (on the runner's machine: install it and sign in, then the runner sees it within a minute)", name)
}

// oldAuditRunnerWhy names a runner that has the CLI but whose wp predates
// audits.
func oldAuditRunnerWhy(runners []*Runner, cli string) string {
	for _, r := range runners {
		if runnerCanTake(r, jobNeeds{cli: cli}) && !slices.Contains(r.Features, FeatureAudit) {
			return fmt.Sprintf("%s runs an older wp that can't run audits: it updates itself, or run wp upgrade", r.Name)
		}
	}
	return "no runner can run audits"
}

// auditSchema is the answer every model is asked for.
const auditSchema = `{"type":"object","additionalProperties":false,"required":["summary","findings"],"properties":{` +
	`"summary":{"type":"string","description":"Two or three sentences: what you read and what you found."},` +
	`"findings":{"type":"array","maxItems":15,"items":{"type":"object","additionalProperties":false,"required":["title","path","line","severity","problem","confirm"],"properties":{` +
	`"title":{"type":"string","description":"One line: the defect."},` +
	`"path":{"type":"string","description":"The file, relative to the repository's root."},` +
	`"line":{"type":"integer","description":"The line the defect is on (0 if it has none)."},` +
	`"severity":{"type":"string","enum":["critical","high","medium","low"]},` +
	`"problem":{"type":"string","description":"What goes wrong, and when: the input or sequence of events that sets it off."},` +
	`"confirm":{"type":"string","description":"How someone could confirm it: a test to write, an input to try, or the lines to read."}}}}}}`

// codeAuditPrompt is what the model is asked: the rules every audit keeps, then
// what the requester asked for. The runner adds the change's diff.
func codeAuditPrompt(a *CodeAudit, prContext string) string {
	if a.Purpose == AuditObjective {
		return objectiveJudgePrompt(a, prContext)
	}
	var b strings.Builder
	switch a.Purpose {
	case AuditFootprint:
		b.WriteString("Check this pull request's footprint for Waypoint: whether the fix-up rounds after it shipped kept to its task. ")
	case AuditCheck:
		b.WriteString("Check, for Waypoint, whether the change's author was right to reject, or mark fixed, the findings of an earlier review: read the code and decide each. ")
	default:
		b.WriteString("Audit this code adversarially for Waypoint: assume it has real defects and find them. ")
	}
	switch a.Target {
	case "repo":
		b.WriteString("The checkout is the whole repository")
		if a.Ref != "" {
			b.WriteString(" at " + a.Ref)
		}
		b.WriteString(". Read the code itself, across the repository, not only the parts that look important.\n\n")
	case "pr":
		if a.Purpose == AuditFootprint {
			b.WriteString("The checkout is the pull request's head. Read what the rounds changed in the checkout, enough to tell what each change is for.\n\n")
		} else {
			b.WriteString("The checkout is a pull request, and its change is at the end of this prompt. Audit the change: read the code around it in the checkout, and what it calls and what calls it, not the diff alone.\n\n")
		}
		b.WriteString(prContext + "\n\n")
	case "recent":
		fmt.Fprintf(&b, "The checkout is at the tip of %s, and the change under audit is everything that landed on it %s (from %s), at the end of this prompt. Audit the change: read the code around it in the checkout, and what it calls and what calls it, not the diff alone.\n\n",
			cmp(a.Ref, "the default branch"), sinceWords(a.Since, *a.SinceAt), a.SinceAt.UTC().Format("2006-01-02 15:04 UTC"))
	default:
		fmt.Fprintf(&b, "The checkout is at %s, and the change under audit (from %s) is at the end of this prompt. Audit the change: read the code around it in the checkout, and what it calls and what calls it, not the diff alone.\n\n", a.Ref, a.Base)
	}
	for _, ask := range roundAsks(a.reviewText, a.roundContext) {
		b.WriteString(ask)
	}
	switch a.Purpose {
	case AuditFootprint:
		b.WriteString("What to look for: " + a.Instructions + "\n\n")
	case AuditCheck:
		b.WriteString("What to check: " + a.Instructions + "\n\n" + a.claims)
	default:
		b.WriteString("What to look for: " + a.Instructions + " Report other serious problems you come across too.\n\n")
	}
	b.WriteString(auditCriteriaPrompt(a))
	fmt.Fprintf(&b, `Rules:
- Report only problems you can point to in the code: the file (relative to the repository's root) and line, what goes wrong, and when.
- No style nits, no "consider adding", nothing you did not check against the code.
- Do not propose patches or plans: say what is wrong and how someone could confirm it.
- Severity: critical (data lost or exposed, a security hole, an outage), high (wrong results people will hit), medium (wrong in some cases), low (minor).
- At most %d findings, worst first. Change nothing: no edits, no commits, no commands that write.
- If nothing is worth reporting, give an empty list of findings and say so in the summary.`, auditFindingsCap(a))
	if a.MinSeverity != "" {
		fmt.Fprintf(&b, "\n- Report only findings of %s severity or worse; leave out anything less.", a.MinSeverity)
	}
	return b.String()
}

// ---- moving it along ----

// advanceAudit queues the next model's job for a queued audit.
func (s *Service) advanceAudit(ctx context.Context, id int64) {
	unlock := s.auditLocks.lock(id)
	defer unlock()
	a, err := s.auditRow(ctx, id)
	if err != nil || a.Status != AuditQueued || a.JobID != nil {
		return
	}
	if a.RetryAt != nil && a.RetryAt.After(s.now()) {
		return
	}
	f, err := s.forgeByID(ctx, a.forgeID)
	if err != nil {
		s.failAudit(ctx, a, "its forge is gone")
		return
	}
	repo := &stepRepo{Forge: f, Repo: a.Repo}
	// ran: a model of this pass of the chain did run, and how it ended (the
	// gateway busy, or out of allowance) is why a later entry was passed
	// over; the chain then ends as it does after a model's failure.
	ran := false
	for a.Slot < len(a.chain) {
		m := a.chain[a.Slot]
		in := AgentJob{CLI: m.CLI, Model: m.Model, Prompt: a.prompt, Repo: repo.cloneURL(), Base: a.Ref, DiffFrom: a.Base, ReadOnly: true, Permission: AgentRead,
			JSONSchema: auditSchemaFor(a), DiffPaths: a.Paths, DiffExclude: a.Exclude, Helpers: a.Helpers}
		if a.SinceAt != nil {
			in.DiffFrom, in.DiffSince = "", a.SinceAt.Unix()
		}
		timeout := auditTimeout + a.Helpers.extraTimeout()
		if m.CLI == GatewayCLI {
			if outcome, why := s.gatewaySkip(ctx, a, m); why != "" {
				a.Attempts = append(a.Attempts, AuditAttempt{Model: auditModelName(m), Round: a.Retries, Outcome: outcome, Error: excerpt(why, 300), At: s.nowMS()})
				a.Slot++
				ran = ran || outcome != "unavailable"
				continue
			}
			// Never the family of the model that wrote the code.
			in.GatewayExclude, timeout = gatewayExcludeFor(a.authorModel), gatewayAuditTimeout+a.Helpers.extraTimeout()
			in.GatewayWeakest = a.Weakest
		}
		raw, _ := json.Marshal(in)
		job, err := s.EnqueueRunnerJob(ctx, SystemActor(), RunnerJobInput{Kind: JobAudit, OwnerID: a.ownerID, Input: raw, Source: auditSource, SourceID: a.ID,
			Timeout: timeout})
		if err == nil {
			err = s.write(ctx, SystemActor(), func(t *tx) error {
				if _, err := t.ExecContext(ctx, `UPDATE code_audits SET status = ?, job_id = ?, slot = ?, attempts = ?, retry_at = NULL, started_at = coalesce(started_at, ?) WHERE id = ?`,
					AuditRunning, job.ID, a.Slot, attemptsJSON(a.Attempts), s.nowMS(), a.ID); err != nil {
					return err
				}
				t.emit(&Event{Kind: EvCodeAudit, ProjectID: a.projectID, Old: AuditQueued, New: AuditRunning, Data: map[string]any{"audit": a.ID, "job": job.ID}})
				return nil
			})
			if err != nil {
				s.cancelStepJob(ctx, job.ID, "Waypoint couldn't record the audit's job.")
				s.log.WarnContext(ctx, "start an audit", "audit", a.ID, "err", err)
			}
			return
		}
		why := "no runner can take it"
		if !errors.Is(err, ErrNoRunner) {
			why = PublicError(err)
		}
		a.Attempts = append(a.Attempts, AuditAttempt{Model: auditModelName(m), Round: a.Retries, Outcome: "unavailable", Error: excerpt(why, 300), At: s.nowMS()})
		a.Slot++
	}
	last := a.Attempts[len(a.Attempts)-1].Error
	if ran {
		s.endAuditChain(ctx, a, last)
		return
	}
	s.failAudit(ctx, a, "no model of the chain could be started: "+last)
}

func attemptsJSON(list []AuditAttempt) string {
	if list == nil {
		return "[]"
	}
	b, _ := json.Marshal(list)
	return string(b)
}

// failAudit ends an audit that can't go on.
func (s *Service) failAudit(ctx context.Context, a *CodeAudit, why string) {
	s.endAudit(ctx, a, AuditFailed, why, nil)
}

// endAudit records a final status (and, when done, the findings).
func (s *Service) endAudit(ctx context.Context, a *CodeAudit, status, why string, found *auditAnswer) {
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		count := -1
		if a.CommitCount != nil {
			count = *a.CommitCount
		}
		res, err := t.ExecContext(ctx, `UPDATE code_audits SET status = ?, error = ?, attempts = ?, slot = ?, answer = ?, model = ?, summary = ?, tokens = ?, finished_at = ?,
			from_sha = ?, to_sha = ?, commit_count = ?
			WHERE id = ? AND status IN ('queued','running')`, status, excerpt(why, 2000), attemptsJSON(a.Attempts), a.Slot, a.Answer, a.Model, a.Summary,
			tokensJSON(a.Tokens), s.nowMS(), a.FromSHA, a.ToSHA, count, a.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // cancelled meanwhile
		}
		if found != nil {
			for i, f := range found.Findings {
				if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO code_audit_findings(audit_id, ordinal, title, path, line, severity, problem, confirm, verdict, verdict_note, lead)
					VALUES (?,?,?,?,?,?,?,?,?,?,?)`, a.ID, i+1, f.Title, f.Path, f.Line, f.Severity, f.Problem, f.Confirm, f.Verdict, f.VerdictNote, f.Lead); err != nil {
					return err
				}
			}
		}
		if err := insertHelperRuns(ctx, t, a.ID, a.HelperRuns, found != nil, s.nowMS()); err != nil {
			return err
		}
		if len(a.HelperRuns) > 0 || a.Helpers != nil { // the review loop's helpers' record changed (review_health.go)
			t.emit(&Event{Kind: EvReviewLoopRecorded, ProjectID: a.projectID})
		}
		t.emit(&Event{Kind: EvCodeAudit, ProjectID: a.projectID, Old: a.Status, New: status, Data: map[string]any{"audit": a.ID}})
		return nil
	})
	if err != nil {
		s.log.WarnContext(ctx, "end an audit", "audit", a.ID, "err", err)
	}
}

func tokensJSON(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// auditJobFinished takes up an audit whose runner job ended.
func (s *Service) auditJobFinished(ctx context.Context, e *Event) {
	if src, _ := e.Data["source"].(string); src != auditSource {
		return
	}
	jobID, _ := e.Data["job"].(int64)
	id, _ := e.Data["source_id"].(int64)
	if jobID == 0 || id == 0 {
		return
	}
	s.goBG(func() { s.settleAudit(context.Background(), id, jobID) })
}

// settleAudit reads a finished job into its audit: the findings, or the
// next model to try, or a retry later, or the end.
func (s *Service) settleAudit(ctx context.Context, id, jobID int64) {
	unlock := s.auditLocks.lock(id)
	defer unlock()
	a, err := s.auditRow(ctx, id)
	if err != nil || a.Status != AuditRunning || a.JobID == nil || *a.JobID != jobID {
		return
	}
	j, err := s.stepJobByID(ctx, jobID)
	if err != nil {
		return
	}
	if j == nil {
		j = &stepJob{ID: jobID, State: "failed", Error: "the runner job is gone"}
	}
	if !j.finished() {
		return
	}
	m := AgentModel{}
	if a.Slot < len(a.chain) {
		m = a.chain[a.Slot]
	}
	att := AuditAttempt{Model: auditModelName(m), Job: jobID, Round: a.Retries, At: s.nowMS()}
	a.readRange(j.Result)
	a.HelperRuns = helperRunsOf(j.Result)
	switch j.State {
	case "done":
		if why := strings.TrimSpace(anyString(j.Result["nothing"])); why != "" {
			// The runner found nothing to read (no change, or none in the
			// paths asked for) and ran no model.
			att.Outcome = "nothing"
			a.Attempts = append(a.Attempts, att)
			a.Summary = excerpt(why, 2000)
			s.endAudit(ctx, a, AuditDone, "", &auditAnswer{Summary: a.Summary})
			return
		}
		found, err := doneAuditAnswer(j.Result, a.HelperRuns)
		a.Answer = excerpt(anyString(j.Result["answer"]), 60000)
		if err == nil {
			raised := found.raised // as written: before blanks, the floor and the cap
			att.Outcome = "answered"
			a.Attempts = append(a.Attempts, att)
			if n := found.keep(a); n > 0 {
				found.Summary += fmt.Sprintf(" (%d more finding%s, below what was asked for, left out.)", n, plural(n))
			}
			a.Model, a.Summary = auditModelName(m), excerpt(found.Summary, 2000)
			if m.CLI == GatewayCLI {
				// Which of the gateway's models answered, and that its
				// findings are to be reported on.
				a.Model = s.gatewayAuditAnswered(ctx, jobID, m, raised)
			}
			if t, ok := j.Result["tokens"].(map[string]any); ok {
				a.Tokens = t
			}
			s.endAudit(ctx, a, AuditDone, "", found)
			return
		}
		att.Outcome, att.Error = "unreadable", err.Error()
	case "cancelled":
		if j.Error == runnerStoppedError {
			// The runner restarting isn't anyone calling the audit off, and
			// says nothing about the model (WP-820): it goes again.
			att.Outcome, att.Error = "stopped", j.Error
			a.Attempts = append(a.Attempts, att)
			stops := 0
			for _, x := range a.Attempts {
				if x.Outcome == "stopped" {
					stops++
				}
			}
			if stops > auditMaxRetries {
				s.failAudit(ctx, a, fmt.Sprintf("The runner was stopped during it %d times.", stops))
				return
			}
			s.requeueAudit(ctx, a, a.Slot, a.Retries, nil)
			return
		}
		att.Outcome, att.Error = "cancelled", cmp(j.Error, "the runner job was cancelled")
		a.Attempts = append(a.Attempts, att)
		s.endAudit(ctx, a, AuditCancelled, att.Error, nil)
		return
	case "expired":
		att.Outcome, att.Error = "unavailable", "no runner took it within a day"
	default:
		att.Outcome, att.Error = auditFailureKind(j), excerpt(jobWhy(j), 300)
		if kind := anyString(j.Result["gateway_error"]); m.CLI == GatewayCLI && kind != "" {
			// The gateway said what went wrong in a word; no guessing
			// from the text.
			if att.Outcome = gatewayOutcome(kind); att.Outcome == "unreadable" {
				a.Answer = excerpt(anyString(j.Result["answer"]), 60000)
			}
		}
	}
	a.Attempts = append(a.Attempts, att)
	next := a.Slot + 1
	switch {
	case j.State == "expired", att.Outcome == "error" && (next >= len(a.chain) || a.Purpose != AuditReview && a.Purpose != AuditObjective && !s.roundsAudit(ctx, a.ID)):
		// A review round's chain goes on after any failure (WP-820): an
		// error text no list knows mustn't end it with models still to try.
		// So does one handed from its round to the pull request (WP-1059),
		// and an objective's judge, which decides whether a task closes
		// (WP-ADR111).
		s.failAudit(ctx, a, att.Error)
		return
	case next < len(a.chain):
		s.requeueAudit(ctx, a, next, a.Retries, nil)
		return
	case att.Outcome == "unreadable" && unreadableTries(a) <= auditUnreadableRetries:
		// The chain's last model answered out of form: it's asked again
		// before the audit fails. Claude Code's structured output can
		// refuse every try in one run and take the next (WP-1320's live
		// round 3: a check of marks that found nothing, answered in text).
		// A gateway's invalid_output gets the same one retry: whether it
		// tried again itself isn't known here.
		s.requeueAudit(ctx, a, a.Slot, a.Retries, nil)
		return
	}
	s.endAuditChain(ctx, a, att.Error)
}

// unreadableTries counts the audit's answers out of form at its current
// model in this pass of the chain.
func unreadableTries(a *CodeAudit) int {
	n := 0
	name := auditModelName(a.chain[a.Slot])
	for _, x := range a.Attempts {
		if x.Round == a.Retries && x.Outcome == "unreadable" && x.Model == name {
			n++
		}
	}
	return n
}

// endAuditChain decides what follows the chain's last model failing, last
// being why it did: the whole chain again later when a model was busy, or
// the audit's end.
func (s *Service) endAuditChain(ctx context.Context, a *CodeAudit, last string) {
	var busy, spent, unreadable bool
	for _, x := range a.Attempts {
		if x.Round != a.Retries {
			continue
		}
		busy = busy || x.Outcome == "busy"
		spent = spent || x.Outcome == "spent"
		unreadable = unreadable || x.Outcome == "unreadable"
	}
	switch {
	case busy && a.Retries < auditMaxRetries:
		at := s.now().Add(auditRetryStep * time.Duration(a.Retries+1))
		s.requeueAudit(ctx, a, 0, a.Retries+1, &at)
	case spent:
		s.endAudit(ctx, a, AuditLimited, "Every model hit a usage limit ("+last+"). "+auditLimitAdvice(a.chain), nil)
	case busy:
		s.failAudit(ctx, a, fmt.Sprintf("Every model was still busy after %d tries: %s", auditMaxRetries+1, last))
	case unreadable:
		s.failAudit(ctx, a, "No model answered in the findings form; the last answer is kept as it came.")
	default:
		s.failAudit(ctx, a, last)
	}
}

// auditLimitAdvice says when a chain whose every model hit a usage limit
// is worth running again: Cursor's allowance comes back with the plan's
// cycle, which is said only of a chain that went through Cursor.
func auditLimitAdvice(chain []AgentModel) string {
	if slices.ContainsFunc(chain, func(m AgentModel) bool { return m.CLI == "cursor" }) {
		return "Cursor's included usage comes back with the plan's next cycle: run it again then, or with another model."
	}
	return "Run it again when the limits reset, or with another model."
}

// requeueAudit sends an audit back to the queue for the next model (or,
// with at, the whole chain again later). The audit is running, or queued
// still when the chain's last entries were passed over (advanceAudit).
func (s *Service) requeueAudit(ctx context.Context, a *CodeAudit, slot, retries int, at *time.Time) {
	var retryAt any
	if at != nil {
		retryAt = at.UnixMilli()
	}
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		res, err := t.ExecContext(ctx, `UPDATE code_audits SET status = ?, job_id = NULL, slot = ?, retries = ?, retry_at = ?, attempts = ?, answer = ? WHERE id = ? AND status IN (?, ?)`,
			AuditQueued, slot, retries, retryAt, attemptsJSON(a.Attempts), a.Answer, a.ID, AuditRunning, AuditQueued)
		if err != nil {
			return err
		}
		// The attempt's helpers ran and cost what they cost though its
		// reviewer didn't answer (WP-1320's live round 6): kept, unanswered.
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 && len(a.HelperRuns) > 0 {
			if err := insertHelperRuns(ctx, t, a.ID, a.HelperRuns, false, s.nowMS()); err != nil {
				return err
			}
			t.emit(&Event{Kind: EvReviewLoopRecorded, ProjectID: a.projectID})
		}
		t.emit(&Event{Kind: EvCodeAudit, ProjectID: a.projectID, Old: AuditRunning, New: AuditQueued, Data: map[string]any{"audit": a.ID}})
		return nil
	})
	if err != nil {
		s.log.WarnContext(ctx, "queue an audit's next model", "audit", a.ID, "err", err)
	}
}

// runnerStoppedError is the error of a job whose runner was stopped under it
// (cmd/wp/runner.go), as against one a person cancelled.
const runnerStoppedError = "The runner was stopped."

// jobWhy is a failed job's reason in a line.
func jobWhy(j *stepJob) string {
	if strings.TrimSpace(j.Error) != "" {
		return j.Error
	}
	lines := strings.Split(strings.TrimSpace(lastBytes(j.Tail, 1500)), "\n")
	return cmp(strings.TrimSpace(lines[len(lines)-1]), "the job failed")
}

// spentWords say a plan's usage for the cycle is used up; busyWords that
// the model or provider is busy for now.
var (
	spentWords = []string{"quota", "usage limit", "spend limit", "out of usage", "hit your limit", "credit balance", "resource_exhausted", "included usage",
		"upgrade your plan", "billing"}
	busyWords = []string{"rate limit", "rate-limit", "429", "too many requests", "overloaded", "capacity", "529", "temporarily unavailable",
		"try again later", "high demand", "server busy", "503"}
)

// auditFailureKind sorts a failed job: spent (a plan's usage), busy (try
// again later), timeout and model (another model may do), or error (the
// work itself failed, which another model won't change).
func auditFailureKind(j *stepJob) string {
	text := strings.ToLower(j.Error + "\n" + lastBytes(j.Tail, 1500))
	has := func(words []string) bool {
		return slices.ContainsFunc(words, func(w string) bool { return strings.Contains(text, w) })
	}
	switch {
	case has(spentWords) || usagePhrase.MatchString(text):
		return "spent"
	case has(busyWords):
		return "busy"
	case strings.Contains(text, "timed out"):
		return "timeout"
	case modelFailure(j):
		return "model"
	}
	return "error"
}

// auditAnswer is a model's answer, cleaned.
type auditAnswer struct {
	Summary  string
	Findings []CodeAuditFinding
	// raised is how many findings the answer held, as written: before any
	// was dropped as blank or past auditMaxFindings, and before a severity
	// floor or cap (WP-1230: only an answer that raised none is reported to
	// the gateway as having found nothing). -1 when not known: free text that
	// says more than "no findings".
	raised int
}

// flexInt is a number a model may write as a string ("123", "L123").
type flexInt int

func (n *flexInt) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil
	}
	switch x := v.(type) {
	case float64:
		*n = flexInt(x)
	case string:
		x = strings.TrimLeft(strings.TrimSpace(x), "Ll#")
		if i := strings.IndexFunc(x, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
			x = x[:i]
		}
		k, _ := strconv.Atoi(x)
		*n = flexInt(k)
	}
	return nil
}

var severityAliases = map[string]string{"p0": "critical", "blocker": "critical", "p1": "high", "major": "high", "p2": "medium", "moderate": "medium",
	"p3": "low", "minor": "low", "info": "low", "nit": "low"}

// parseAuditAnswer reads a job's answer into findings.
func parseAuditAnswer(result map[string]any) (*auditAnswer, error) {
	var out struct {
		Summary  string `json:"summary"`
		Findings []struct {
			Title    string  `json:"title"`
			Path     string  `json:"path"`
			Line     flexInt `json:"line"`
			Severity string  `json:"severity"`
			Problem  string  `json:"problem"`
			Confirm  string  `json:"confirm"`
			// What the runner's helpers add (WP-1320): the check's verdict
			// and evidence, and the panel lead the model names.
			Verdict  string `json:"verdict"`
			Evidence string `json:"verdict_note"`
			Lead     string `json:"lead"`
		} `json:"findings"`
	}
	if err := decodeRunnerAnswer(result, &out); err != nil {
		text := strings.TrimSpace(anyString(result["answer"]))
		if up := strings.ToUpper(text); strings.Contains(up, "NO FINDINGS") && len(text) < 400 {
			a := &auditAnswer{Summary: "No findings."}
			// Text that says more ("no findings in a.go, but b.go…") may
			// name a problem: not known to have raised nothing (WP-1253).
			if strings.Trim(up, " \n.!*_`") != "NO FINDINGS" {
				a.raised = -1
			}
			return a, nil
		}
		return nil, err
	}
	a := &auditAnswer{Summary: strings.TrimSpace(out.Summary), raised: len(out.Findings)}
	for _, f := range out.Findings {
		title, problem := strings.TrimSpace(f.Title), strings.TrimSpace(f.Problem)
		if title == "" && problem == "" {
			continue
		}
		if title == "" {
			title = excerpt(strings.SplitN(problem, "\n", 2)[0], 120)
		}
		sev := strings.ToLower(strings.TrimSpace(f.Severity))
		if v, ok := severityAliases[strings.Fields(sev + " x")[0]]; ok {
			sev = v
		}
		if !slices.Contains(auditSeverities, sev) {
			sev = "medium"
		}
		p, line := cleanAuditPath(f.Path, int(f.Line))
		a.Findings = append(a.Findings, CodeAuditFinding{Title: excerpt(title, 200), Path: p, Line: line, Severity: sev,
			Problem: excerpt(problem, 4000), Confirm: excerpt(strings.TrimSpace(f.Confirm), 2000), Verdict: cleanVerdict(f.Verdict),
			VerdictNote: excerpt(strings.TrimSpace(f.Evidence), 2000), Lead: cleanLead(f.Lead)})
		if len(a.Findings) == auditMaxFindings {
			break
		}
	}
	// Worst first, keeping the model's order within a severity.
	slices.SortStableFunc(a.Findings, func(x, y CodeAuditFinding) int {
		return slices.Index(auditSeverities, x.Severity) - slices.Index(auditSeverities, y.Severity)
	})
	if a.Summary == "" {
		a.Summary = fmt.Sprintf("%d finding%s.", len(a.Findings), plural(len(a.Findings)))
	}
	return a, nil
}

// cleanAuditPath makes a finding's file relative to the repository; a file
// outside it (an absolute path, one that climbs out) is dropped.
func cleanAuditPath(p string, line int) (string, int) {
	p = strings.Trim(strings.TrimSpace(p), "`")
	if i := strings.LastIndex(p, ":"); i > 0 && line <= 0 {
		if n, err := strconv.Atoi(strings.SplitN(p[i+1:], "-", 2)[0]); err == nil {
			p, line = p[:i], n
		}
	}
	p = strings.TrimPrefix(strings.TrimPrefix(p, "a/"), "b/")
	if p == "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") || strings.Contains(p, "\\") || strings.Contains(p, ":") {
		return "", max(line, 0)
	}
	p = path.Clean(p)
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return "", max(line, 0)
	}
	return excerpt(p, 300), max(line, 0)
}

// CheckAudits moves audits along: a queued one whose wait is over, and a
// running one whose job ended while nothing was listening (a restart).
func (s *Service) CheckAudits(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT a.id, a.status, coalesce(a.job_id, 0), coalesce(a.retry_at, 0), coalesce(j.state, '')
		FROM code_audits a LEFT JOIN runner_jobs j ON j.id = a.job_id WHERE a.status IN ('queued','running')`)
	if err != nil {
		return
	}
	type row struct {
		id, job, retryAt int64
		status, state    string
	}
	var list []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.status, &r.job, &r.retryAt, &r.state) == nil {
			list = append(list, r)
		}
	}
	rows.Close()
	now := s.nowMS()
	for _, r := range list {
		switch {
		case r.status == AuditQueued && r.job == 0 && r.retryAt <= now:
			s.advanceAudit(ctx, r.id)
		case r.status == AuditRunning && r.job == 0:
			if a, err := s.auditRow(ctx, r.id); err == nil {
				s.failAudit(ctx, a, "its runner job is gone")
			}
		case r.status == AuditRunning && slices.Contains([]string{"done", "failed", "cancelled", "expired"}, r.state):
			s.settleAudit(ctx, r.id, r.job)
		}
	}
}

// codeAuditEvent: dispatch's part. A queued audit is started; a finished
// one is posted, handed to Claude if asked, and told to whoever asked.
func (s *Service) codeAuditEvent(ctx context.Context, e *Event) {
	id, _ := e.Data["audit"].(int64)
	if id == 0 {
		return
	}
	switch {
	case e.Field == "fix":
		// Asked for after the fact: made now, so the caller gets its key.
		s.auditFixTask(ctx, id)
	case e.New == AuditQueued:
		s.goBG(func() { s.advanceAudit(context.Background(), id) })
	case slices.Contains([]string{AuditDone, AuditFailed, AuditLimited, AuditCancelled}, e.New) && e.Old != e.New:
		s.goBG(func() { s.auditEnded(context.Background(), id) })
	}
}

// auditEnded does what a finished audit sets off.
func (s *Service) auditEnded(ctx context.Context, id int64) {
	a, err := s.auditRow(ctx, id)
	if err != nil {
		return
	}
	if a.Status == AuditDone {
		if err := s.loadFindings(ctx, a); err != nil {
			return
		}
		if a.PostComment && a.PRNumber > 0 && a.CommentURL == "" && !a.readNothing() && !a.quietWhenClean() {
			s.postAuditComment(ctx, a)
		}
		if a.Fix {
			s.auditFixTask(ctx, id)
			if a, err = s.auditRow(ctx, id); err != nil {
				return
			}
			_ = s.loadFindings(ctx, a)
		}
	}
	s.tellAudit(ctx, a)
	// Settled: an automation step waiting on it reads it whole now.
	_ = s.write(ctx, SystemActor(), func(t *tx) error {
		res, err := t.ExecContext(ctx, `UPDATE code_audits SET settled_at = ? WHERE id = ? AND settled_at IS NULL`, s.nowMS(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			t.emit(&Event{Kind: EvCodeAudit, ProjectID: a.projectID, Field: "settled", Old: a.Status, New: a.Status, Data: map[string]any{"audit": id}})
		}
		return nil
	})
}

// auditSettled: the audit ended and what its end sets off is done.
func (s *Service) auditSettled(ctx context.Context, id int64) bool {
	var at sql.NullInt64
	err := s.db.R.QueryRowContext(ctx, `SELECT settled_at FROM code_audits WHERE id = ?`, id).Scan(&at)
	return err == nil && at.Valid
}

// postAuditComment puts a pull request's findings on it, with a link to
// them in Waypoint.
func (s *Service) postAuditComment(ctx context.Context, a *CodeAudit) {
	fprs, ok := s.Forges.(ForgePRs)
	if !ok {
		return
	}
	f, err := s.forgeByID(ctx, a.forgeID)
	if err != nil {
		return
	}
	c, err := fprs.PostPRComment(ctx, botForge(f), a.Repo, a.PRNumber, auditComment(a, s.BaseURL()))
	if err != nil {
		s.log.WarnContext(ctx, "post an audit on its pull request", "audit", a.ID, "err", err)
		_, _ = s.db.W.ExecContext(ctx, `UPDATE code_audits SET error = ? WHERE id = ? AND error = ''`, "Couldn't post it on the pull request: "+excerpt(PublicError(err), 200), a.ID)
		return
	}
	a.CommentURL = c.URL
	_ = s.write(ctx, SystemActor(), func(t *tx) error {
		if _, err := t.ExecContext(ctx, `UPDATE code_audits SET comment_url = ? WHERE id = ?`, cmp(c.URL, "posted"), a.ID); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvCodeAudit, ProjectID: a.projectID, Field: "comment", Old: a.Status, New: a.Status, Data: map[string]any{"audit": a.ID}})
		return nil
	})
}

// forgeSafe keeps a model's words from doing things on a forge: no table
// breaks, no mentions that notify people, no HTML.
func forgeSafe(s string, cell bool) string {
	s = strings.ReplaceAll(s, "@", "@\u200b")
	s = strings.ReplaceAll(strings.ReplaceAll(s, "<", "&lt;"), ">", "&gt;")
	if cell {
		s = strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "|", `\|`)
	}
	return s
}

// auditComment is the pull request comment for an audit.
func auditComment(a *CodeAudit, base string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- waypoint-audit id=%d -->\n", a.ID)
	n := len(a.Findings)
	switch {
	case a.Purpose == AuditFootprint && n == 0:
		fmt.Fprintf(&b, "### Footprint check: in scope\n\n%s found nothing the fix-up rounds added beyond the task", a.Model)
	case a.Purpose == AuditFootprint:
		fmt.Fprintf(&b, "### Footprint check: %d change%s beyond the task\n\n%s compared what the fix-up rounds added with the task; each is to undo or split out", n, plural(n), a.Model)
	case n == 0:
		fmt.Fprintf(&b, "### Audit: no findings\n\n%s found nothing worth reporting", a.Model)
	default:
		fmt.Fprintf(&b, "### Audit: %d finding%s\n\n%s, asked to look for: %s", n, plural(n), a.Model, forgeSafe(excerpt(a.Instructions, 300), true))
	}
	if a.Summary != "" {
		b.WriteString("\n\n> " + forgeSafe(strings.ReplaceAll(a.Summary, "\n", "\n> "), false))
	}
	if n > 0 {
		b.WriteString("\n\n| # | Severity | Where | Finding |\n|---|---|---|---|\n")
		for _, f := range a.Findings {
			where := ""
			if f.Path != "" {
				where = "`" + strings.ReplaceAll(f.Path, "`", "") + lineSuffix(f.Line) + "`"
			}
			fmt.Fprintf(&b, "| %d | %s | %s | %s |\n", f.Ordinal, f.Severity, where, forgeSafe(f.Title, true))
		}
	}
	fmt.Fprintf(&b, "\nThe details, how to confirm each and their verdicts: %s/audits/%d\n\n", base, a.ID)
	b.WriteString("<sub>These are a model's claims, not checked facts. The audit read the code and changed nothing.</sub>\n")
	return b.String()
}

func lineSuffix(n int) string {
	if n <= 0 {
		return ""
	}
	return ":" + strconv.Itoa(n)
}

// auditFixTask makes the task for Claude to check and fix an audit's open
// findings, once.
func (s *Service) auditFixTask(ctx context.Context, id int64) {
	unlock := s.auditLocks.lock(id)
	defer unlock()
	a, err := s.auditRow(ctx, id)
	if err != nil || a.Status != AuditDone || a.fixIssueID != 0 {
		return
	}
	if err := s.loadFindings(ctx, a); err != nil || a.Counts.Open == 0 {
		return
	}
	owner, err := s.userByID(ctx, s.db.R, a.ownerID)
	if err != nil {
		return
	}
	writer := Actor{User: owner}
	writer.textAgent = cmp(a.Model, "audit")
	in := IssueInput{Project: itoa64(a.projectID), Type: "task", Agent: "claude", Labels: []string{"audit"},
		Title:       fmt.Sprintf("Check audit #%d: %d finding%s in %s", a.ID, a.Counts.Open, plural(a.Counts.Open), excerpt(a.Title, 120)),
		Description: auditTaskText(a, s.BaseURL())}
	if a.automationID != 0 || a.Via == "automation" {
		in.Priority = "low"
	}
	made, err := s.CreateIssue(ctx, &writer, in)
	if err != nil {
		s.log.WarnContext(ctx, "make an audit's fix task", "audit", a.ID, "err", err)
		_, _ = s.db.W.ExecContext(ctx, `UPDATE code_audits SET error = ? WHERE id = ?`, "Couldn't make the task for Claude: "+excerpt(PublicError(err), 200), a.ID)
		return
	}
	_ = s.write(ctx, SystemActor(), func(t *tx) error {
		if _, err := t.ExecContext(ctx, `UPDATE code_audits SET fix = 1, fix_issue_id = ? WHERE id = ?`, made.ID, a.ID); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvCodeAudit, ProjectID: a.projectID, Field: "fix_issue", Old: a.Status, New: a.Status, Data: map[string]any{"audit": a.ID, "issue": made.Key}})
		return nil
	})
}

// findingToDo says whether a finding is still to do: undecided, or
// confirmed and not fixed.
func findingToDo(state string) bool { return state == "open" || state == "confirmed" }

func noteSuffix(n string) string {
	if n = strings.TrimSpace(n); n == "" {
		return ""
	}
	return " (" + untrusted(n) + ")"
}

// auditTaskText is the fix task's description.
func auditTaskText(a *CodeAudit, base string) string {
	var b strings.Builder
	if a.Agent == leftoverAgent {
		fmt.Fprintf(&b, "Waypoint's own review (%s) raised these findings on %s, and nobody judged them before it merged (audit #%d: %s/audits/%d).\n\n", a.Model, a.Title, a.ID, base, a.ID)
		b.WriteString("They are a model's claims, not facts.")
	} else {
		fmt.Fprintf(&b, "%s audited %s (audit #%d: %s/audits/%d), asked to look for: %s\n\n", a.Model, a.Title, a.ID, base, a.ID, a.Instructions)
		b.WriteString("Its findings are below. They are another model's claims, not facts.")
	}
	if a.Target == "pr" {
		fmt.Fprintf(&b, " It read pull request #%d: fix on its branch while it is open, else in a new pull request.", a.PRNumber)
	}
	fmt.Fprintf(&b, `

For each finding:
1. Check it against the code. Many will be right; some will be wrong or already fixed.
2. Real and small: fix it, with a test where one fits. Ship the fixes together in one pull request.
3. Real but bigger than this task: file an issue (wp new) that names the file and line.
4. Wrong, already fixed or not worth changing: say why in one line.

Record each verdict on the audit as you go: wp audit mark %d.1 fixed -m "what you did" (or confirmed, rejected; add --issue KEY for one you filed). Finish with wp ship -m "summary" (it ships any fixes); the summary says how many were confirmed, fixed, filed (with keys) and rejected.

`, a.ID)
	var todo []CodeAuditFinding
	for _, f := range a.Findings {
		if findingToDo(f.State) {
			todo = append(todo, f)
		}
	}
	writeAuditFindings(&b, todo)
	return b.String()
}

// writeAuditFindings writes findings for an agent to check, as the model's
// untrusted words.
func writeAuditFindings(b *strings.Builder, findings []CodeAuditFinding) {
	b.WriteString("<untrusted>\n")
	for _, f := range findings {
		writeAuditFinding(b, f)
	}
	b.WriteString("</untrusted>\n")
}

// writeAuditFinding writes one finding (within an untrusted block).
func writeAuditFinding(b *strings.Builder, f CodeAuditFinding) {
	fmt.Fprintf(b, "### %s [%s] %s\n", f.ID, f.Severity, untrusted(f.Title))
	if f.State == "confirmed" {
		fmt.Fprintf(b, "- Confirmed by %s: it's real, fix it%s\n", untrusted(cmp(f.By, "a person")), noteSuffix(f.Note))
	}
	if f.Path != "" {
		fmt.Fprintf(b, "- Where: %s%s\n", untrusted(f.Path), lineSuffix(f.Line))
	}
	fmt.Fprintf(b, "- What goes wrong: %s\n", untrusted(f.Problem))
	if f.Confirm != "" {
		fmt.Fprintf(b, "- How to confirm: %s\n", untrusted(f.Confirm))
	}
	if f.Verdict != "" {
		fmt.Fprintf(b, "- Checked against the code by Claude Haiku: %s%s\n", f.Verdict, noteSuffix(untrusted(f.VerdictNote)))
	}
	b.WriteString("\n")
}

// tellAudit tells whoever asked that an audit ended: the agent session that
// asked (at its next stop), else the person, by a notification. An
// automation's owner hears only of what needs them.
func (s *Service) tellAudit(ctx context.Context, a *CodeAudit) {
	text := auditNews(a, s.BaseURL())
	if a.sessionID != 0 {
		m, err := s.storeMessage(ctx, a.sessionID, text, fmt.Sprintf("audit:%d:%s", a.ID, a.Status), messageFrom{}, false)
		if err != nil {
			s.log.WarnContext(ctx, "tell a session about its audit", "audit", a.ID, "err", err)
		} else if m.State == MessageQueued {
			s.batchOrTell(ctx, a.sessionID, 0, a.blocking, func(until int64) error {
				_, err := s.db.W.ExecContext(ctx, `UPDATE session_messages SET batch_until = ?, blocking = ? WHERE id = ? AND state = 'queued'`, until, a.blocking, m.ID)
				return err
			})
		}
		return
	}
	if a.Status == AuditCancelled {
		return // whoever cancelled it knows
	}
	who := a.requestedBy
	if a.automationID != 0 {
		who = a.ownerID
		needed := a.Status == AuditFailed || (a.Status == AuditDone && !a.Fix && a.Counts.Total > 0)
		if !needed {
			return
		}
	}
	if who == 0 {
		return
	}
	s.notifyURL(ctx, who, nil, NotifyAudit, auditHeadline(a), text, fmt.Sprintf("/audits/%d", a.ID))
}

func auditHeadline(a *CodeAudit) string {
	switch a.Status {
	case AuditDone:
		if a.Counts.Total == 0 {
			return "Audit of " + a.Title + ": no findings"
		}
		return fmt.Sprintf("Audit of %s: %d finding%s", a.Title, a.Counts.Total, plural(a.Counts.Total))
	case AuditLimited:
		return "Audit of " + a.Title + " stopped at a usage limit"
	case AuditCancelled:
		return "Audit of " + a.Title + " cancelled"
	}
	return "Audit of " + a.Title + " failed"
}

// auditNews is what the asker is told when an audit ends.
func auditNews(a *CodeAudit, base string) string {
	var b strings.Builder
	b.WriteString(auditHeadline(a))
	switch a.Status {
	case AuditDone:
		if a.Counts.Total > 0 {
			var parts []string
			for _, x := range []struct {
				n    int
				name string
			}{{a.Counts.Critical, "critical"}, {a.Counts.High, "high"}, {a.Counts.Medium, "medium"}, {a.Counts.Low, "low"}} {
				if x.n > 0 {
					parts = append(parts, fmt.Sprintf("%d %s", x.n, x.name))
				}
			}
			b.WriteString(" (" + strings.Join(parts, ", ") + ")")
		}
		b.WriteString(", by " + a.Model + ".")
		if a.FixIssue != "" {
			b.WriteString(" Claude's task to check and fix them: " + a.FixIssue + ".")
		}
		if a.CommentURL != "" && a.CommentURL != "posted" {
			b.WriteString(" Posted on the pull request: " + a.CommentURL)
		}
	default:
		if a.Error != "" {
			b.WriteString(": " + a.Error)
		}
	}
	fmt.Fprintf(&b, "\n%s/audits/%d · wp audit show %d", base, a.ID, a.ID)
	return b.String()
}

// ---- reading ----

const auditCols = `a.id, a.project_id, p.key, p.name, a.forge_id, a.repo, a.target, a.ref, a.base, a.pr_id, a.pr_number, coalesce(pr.url, ''), a.title, a.instructions,
	a.prompt, a.models, a.post_comment, a.fix, a.status, a.slot, a.job_id, coalesce(j.state, ''), a.attempts, a.retries, a.retry_at, a.model, a.summary, a.answer,
	a.error, a.tokens, a.comment_url, coalesce(a.fix_issue_id, 0), coalesce(fp.key || '-' || fi.number, ''), a.owner_id, coalesce(a.requested_by, 0),
	a.requested_agent, a.requested_via, coalesce(a.automation_id, 0), coalesce(au.name, ''), coalesce(a.session_id, 0), a.blocking, a.created_at, a.started_at, a.finished_at,
	a.since_spec, a.since_at, a.focus, a.paths, a.exclude, a.min_severity, a.max_findings, a.from_sha, a.to_sha, a.commit_count, a.purpose, a.author_model, a.weakest, a.helpers`

const auditFrom = ` FROM code_audits a JOIN projects p ON p.id = a.project_id
	LEFT JOIN pull_requests pr ON pr.id = a.pr_id
	LEFT JOIN runner_jobs j ON j.id = a.job_id
	LEFT JOIN issues fi ON fi.id = a.fix_issue_id LEFT JOIN projects fp ON fp.id = fi.project_id
	LEFT JOIN automations au ON au.id = a.automation_id`

func scanAudit(sc interface{ Scan(...any) error }) (*CodeAudit, error) {
	var a CodeAudit
	var prID, jobID, retryAt, started, finished sql.NullInt64
	var models, attempts, tokens, focus, paths, exclude, helpers string
	var created, sinceAt int64
	var commits int
	err := sc.Scan(&a.ID, &a.projectID, &a.Project, &a.ProjectName, &a.forgeID, &a.Repo, &a.Target, &a.Ref, &a.Base, &prID, &a.PRNumber, &a.PRURL, &a.Title,
		&a.Instructions, &a.prompt, &models, &a.PostComment, &a.Fix, &a.Status, &a.Slot, &jobID, &a.JobState, &attempts, &a.Retries, &retryAt, &a.Model,
		&a.Summary, &a.Answer, &a.Error, &tokens, &a.CommentURL, &a.fixIssueID, &a.FixIssue, &a.ownerID, &a.requestedBy, &a.Agent, &a.Via, &a.automationID,
		&a.Automation, &a.sessionID, &a.blocking, &created, &started, &finished,
		&a.Since, &sinceAt, &focus, &paths, &exclude, &a.MinSeverity, &a.MaxFindings, &a.FromSHA, &a.ToSHA, &commits, &a.Purpose, &a.authorModel, &a.Weakest, &helpers)
	if err != nil {
		return nil, err
	}
	if prID.Valid {
		a.PRID = &prID.Int64
	}
	if jobID.Valid {
		a.JobID = &jobID.Int64
	}
	ts := func(v sql.NullInt64) *time.Time {
		if !v.Valid {
			return nil
		}
		t := msTime(v.Int64)
		return &t
	}
	a.RetryAt, a.StartedAt, a.FinishedAt, a.CreatedAt = ts(retryAt), ts(started), ts(finished), msTime(created)
	_ = json.Unmarshal([]byte(models), &a.chain)
	_ = json.Unmarshal([]byte(attempts), &a.Attempts)
	_ = json.Unmarshal([]byte(tokens), &a.Tokens)
	if helpers != "" {
		a.Helpers = &AuditHelpers{}
		if json.Unmarshal([]byte(helpers), a.Helpers) != nil {
			a.Helpers = nil
		}
	}
	if a.Attempts == nil {
		a.Attempts = []AuditAttempt{}
	}
	if sinceAt > 0 {
		t := msTime(sinceAt)
		a.SinceAt = &t
	}
	a.Focus, a.Paths, a.Exclude = listJSON(focus), listJSON(paths), listJSON(exclude)
	if commits >= 0 {
		a.CommitCount = &commits
	}
	a.Models = auditModelNames(a.chain)
	a.URL = fmt.Sprintf("/audits/%d", a.ID)
	return &a, nil
}

// auditRow reads an audit without access checks (Waypoint's own use).
func (s *Service) auditRow(ctx context.Context, id int64) (*CodeAudit, error) {
	a, err := scanAudit(s.db.R.QueryRowContext(ctx, `SELECT `+auditCols+auditFrom+` WHERE a.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("audit")
	}
	if err != nil {
		return nil, err
	}
	a.Counts = s.auditCounts(ctx, id)
	return a, nil
}

func (s *Service) auditCounts(ctx context.Context, id int64) AuditCounts {
	var c AuditCounts
	_ = s.db.R.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(state IN ('open','confirmed')), 0), coalesce(sum(severity = 'critical'), 0), coalesce(sum(severity = 'high'), 0),
		coalesce(sum(severity = 'medium'), 0), coalesce(sum(severity = 'low'), 0) FROM code_audit_findings WHERE audit_id = ?`, id).
		Scan(&c.Total, &c.Open, &c.Critical, &c.High, &c.Medium, &c.Low)
	return c
}

func (s *Service) loadFindings(ctx context.Context, a *CodeAudit) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT f.ordinal, f.title, f.path, f.line, f.severity, f.problem, f.confirm, f.state, f.note,
		coalesce(ip.key || '-' || i.number, ''), coalesce(nullif(u.display_name, ''), u.username, f.decided_agent, ''), f.decided_at, f.verdict, f.verdict_note, f.lead
		FROM code_audit_findings f LEFT JOIN issues i ON i.id = f.issue_id LEFT JOIN projects ip ON ip.id = i.project_id
		LEFT JOIN users u ON u.id = f.decided_by WHERE f.audit_id = ? ORDER BY f.ordinal`, a.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	a.Findings = []CodeAuditFinding{}
	for rows.Next() {
		var f CodeAuditFinding
		var at sql.NullInt64
		if err := rows.Scan(&f.Ordinal, &f.Title, &f.Path, &f.Line, &f.Severity, &f.Problem, &f.Confirm, &f.State, &f.Note, &f.Issue, &f.By, &at, &f.Verdict, &f.VerdictNote, &f.Lead); err != nil {
			return err
		}
		f.ID = fmt.Sprintf("%d.%d", a.ID, f.Ordinal)
		if at.Valid {
			t := msTime(at.Int64)
			f.At = &t
		}
		a.Findings = append(a.Findings, f)
	}
	a.Counts = AuditCounts{}
	for _, f := range a.Findings {
		a.Counts.Total++
		if findingToDo(f.State) {
			a.Counts.Open++
		}
		switch f.Severity {
		case "critical":
			a.Counts.Critical++
		case "high":
			a.Counts.High++
		case "medium":
			a.Counts.Medium++
		default:
			a.Counts.Low++
		}
	}
	return rows.Err()
}

// auditFor reads an audit the actor may see.
func (s *Service) auditFor(ctx context.Context, actor *Actor, id int64, findings bool) (*CodeAudit, error) {
	if err := actor.need(PermPullsRead); err != nil {
		return nil, err
	}
	a, err := s.auditRow(ctx, id)
	if err != nil {
		return nil, err
	}
	role, err := s.projectRole(ctx, s.db.R, actor, a.projectID)
	if err != nil {
		return nil, err
	}
	if role == RoleNone {
		return nil, notFound("audit")
	}
	a.CanEdit = role >= RoleEditor && actor.canWrite() == nil
	a.RequestedBy = s.userRefByID(ctx, s.db.R, sql.NullInt64{Int64: a.requestedBy, Valid: a.requestedBy != 0})
	if findings {
		if err := s.loadFindings(ctx, a); err != nil {
			return nil, err
		}
		s.findingLinks(ctx, a)
		if a.HelperRuns, err = s.helperRuns(ctx, a.ID); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// findingLinks points each finding at its line on the forge.
func (s *Service) findingLinks(ctx context.Context, a *CodeAudit) {
	f, err := s.forgeByID(ctx, a.forgeID)
	if err != nil {
		return
	}
	ref := a.Ref
	if ref == "" || strings.HasPrefix(ref, "refs/") {
		return
	}
	for i := range a.Findings {
		x := &a.Findings[i]
		if x.Path == "" {
			continue
		}
		x.URL = forgeFileURL(f, a.Repo, ref, x.Path, x.Line)
	}
}

// forgeFileURL is a file's line on the forge's web pages.
func forgeFileURL(f *Forge, repo, ref, file string, line int) string {
	base := strings.TrimRight(f.BaseURL, "/") + "/" + repo
	var u string
	switch f.Kind {
	case "gitlab":
		u = base + "/-/blob/" + ref + "/" + file
	case "github":
		u = base + "/blob/" + ref + "/" + file
	default: // Forgejo, Gitea
		u = base + "/src/commit/" + ref + "/" + file
		if !shaRE.MatchString(ref) {
			u = base + "/src/branch/" + ref + "/" + file
		}
	}
	if line > 0 {
		if f.Kind == "gitlab" {
			u += "#L" + strconv.Itoa(line)
		} else {
			u += "#L" + strconv.Itoa(line)
		}
	}
	return u
}

// GetAudit is an audit with its findings.
func (s *Service) GetAudit(ctx context.Context, actor *Actor, id int64) (*CodeAudit, error) {
	return s.auditFor(ctx, actor, id, true)
}

// AuditFilter narrows ListAudits.
type AuditFilter struct {
	Project string
	PRID    int64
	Status  string
	Limit   int
}

// ListAudits lists the audits the actor may see, newest first.
func (s *Service) ListAudits(ctx context.Context, actor *Actor, f AuditFilter) ([]*CodeAudit, error) {
	if err := actor.need(PermPullsRead); err != nil {
		return nil, err
	}
	where, args := []string{"1"}, []any{}
	if f.Project != "" {
		p, err := s.projectByRef(ctx, s.db.R, f.Project)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		where, args = append(where, "a.project_id = ?"), append(args, p.ID)
	}
	if f.PRID != 0 {
		where, args = append(where, "a.pr_id = ?"), append(args, f.PRID)
	}
	switch f.Status {
	case "":
	case "active":
		where = append(where, "a.status IN ('queued','running')")
	default:
		if !slices.Contains([]string{AuditQueued, AuditRunning, AuditDone, AuditFailed, AuditLimited, AuditCancelled}, f.Status) {
			return nil, invalid("status is one of queued, running, done, failed, limited, cancelled, active")
		}
		where, args = append(where, "a.status = ?"), append(args, f.Status)
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// #nosec G202 -- the conditions are fixed strings; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+auditCols+auditFrom+` WHERE `+strings.Join(where, " AND ")+` ORDER BY a.id DESC LIMIT ?`, append(args, limit*4)...)
	if err != nil {
		return nil, err
	}
	var list []*CodeAudit
	for rows.Next() {
		a, err := scanAudit(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, a)
	}
	rows.Close()
	roles := map[int64]int{}
	people := map[int64]*UserRef{}
	out := []*CodeAudit{}
	for _, a := range list {
		r, ok := roles[a.projectID]
		if !ok {
			r, _ = s.projectRole(ctx, s.db.R, actor, a.projectID)
			roles[a.projectID] = r
		}
		if r == RoleNone {
			continue
		}
		a.CanEdit = r >= RoleEditor && actor.canWrite() == nil
		a.Counts = s.auditCounts(ctx, a.ID)
		a.RequestedBy = s.cachedUser(ctx, people, sql.NullInt64{Int64: a.requestedBy, Valid: a.requestedBy != 0})
		a.Answer, a.prompt = "", ""
		out = append(out, a)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// ---- acting on one ----

// auditToChange reads an audit the actor may change.
func (s *Service) auditToChange(ctx context.Context, actor *Actor, id int64) (*CodeAudit, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	if err := actor.need(PermAuditsRun); err != nil {
		return nil, err
	}
	a, err := s.auditFor(ctx, actor, id, false)
	if err != nil {
		return nil, err
	}
	if !a.CanEdit {
		return nil, ErrForbidden
	}
	return a, nil
}

// auditToMark reads an audit whose findings the actor may record verdicts
// on: one it may change, or, for a babysitting round's own job token (which
// has no audits.run: it can't ask for audits, which spend credit), an audit
// of the pull request that round is working on. Without it a round undid
// what a footprint check named and couldn't say so, and the finding held
// the pull request for good (WP-750).
func (s *Service) auditToMark(ctx context.Context, actor *Actor, id int64) (*CodeAudit, error) {
	if actor == nil || actor.RunnerJob == 0 || actor.Allows(PermAuditsRun) || !s.roundOnAudit(ctx, actor.RunnerJob, id) {
		a, err := s.auditToChange(ctx, actor, id)
		if pe := (*PermissionError)(nil); errors.As(err, &pe) {
			s.RecordRefusal(ctx, actor, pe.Permission, "POST /api/v1/audits/findings")
		}
		return a, err
	}
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	a, err := s.auditFor(ctx, actor, id, false)
	if err != nil {
		return nil, err
	}
	if !a.CanEdit {
		return nil, ErrForbidden
	}
	return a, nil
}

// roundOnAudit: job is the running round of a babysit of the pull request
// audit id is of.
func (s *Service) roundOnAudit(ctx context.Context, job, id int64) bool {
	var one int
	return s.db.R.QueryRowContext(ctx, `SELECT 1 FROM babysits b JOIN code_audits a ON a.pr_id = b.pr_id
		WHERE b.job_id = ? AND b.ended_at IS NULL AND a.id = ?`, job, id).Scan(&one) == nil
}

// CancelAudit stops an audit that hasn't ended.
func (s *Service) CancelAudit(ctx context.Context, actor *Actor, id int64) (*CodeAudit, error) {
	if seen, err := s.auditToChange(ctx, actor, id); err != nil {
		return nil, err
	} else if seen.Purpose == AuditObjective && actor.id() != 0 && !objectivePerson(actor) {
		// The judge of a round isn't stopped by whoever worked the round.
		return nil, forbidden("audit #%d judges an objective: a person stops it, or ends the objective (wp objective end)", id)
	}
	unlock := s.auditLocks.lock(id)
	a, err := s.auditRow(ctx, id)
	if err != nil {
		unlock()
		return nil, err
	}
	if a.Status != AuditQueued && a.Status != AuditRunning {
		unlock()
		return nil, invalid("audit #%d has ended (%s)", id, a.Status)
	}
	who := cmp(actor.agentName(), "")
	if who == "" && actor.User != nil {
		who = actor.User.DisplayName
	}
	err = s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `UPDATE code_audits SET status = ?, error = ?, finished_at = ? WHERE id = ?`, AuditCancelled, "Cancelled by "+cmp(who, "someone")+".", s.nowMS(), id); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvCodeAudit, ProjectID: a.projectID, Old: a.Status, New: AuditCancelled, Data: map[string]any{"audit": id}})
		return nil
	})
	unlock()
	if err != nil {
		return nil, err
	}
	if a.JobID != nil {
		s.cancelStepJob(ctx, *a.JobID, "The audit was cancelled.")
	}
	return s.GetAudit(ctx, actor, id)
}

// RerunAudit asks for the same audit again, as a new one (a pull request's
// at its head now), on the actor's runner.
func (s *Service) RerunAudit(ctx context.Context, actor *Actor, id int64, via string) (*CodeAudit, error) {
	a, err := s.auditToChange(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	comment := a.PostComment
	in := AuditInput{Project: a.Project, Repo: a.Repo, Target: a.Target, Instructions: a.Instructions, Models: a.Models, Comment: &comment, Fix: a.Fix, Via: via,
		Focus: a.Focus, Paths: a.Paths, Exclude: a.Exclude, MinSeverity: a.MinSeverity, MaxFindings: a.MaxFindings, weakest: a.Weakest}
	if slices.Equal(a.Models, auditModelNames(cursorAuditChain)) {
		// The default of before the gateway ran audits (WP-871), which is
		// stored as if it had been named: a rerun asks today's default.
		in.Models = nil
	}
	if a.Agent == leftoverAgent {
		// Made from a review's findings, with no model of its own to repeat:
		// a rerun is the default audit of the pull request.
		in.Models, in.Instructions, in.Fix = nil, "", false
	}
	switch a.Target {
	case "pr":
		in.PR = a.PRNumber
	case "branch":
		in.Ref, in.Base = a.Ref, a.Base
	case "commit", "repo":
		in.Ref = a.Ref
	case "range":
		in.Ref = a.Base + ".." + a.Ref
	case "recent":
		// The same period back from now (a date stays that date).
		in.Ref, in.Since = a.Ref, a.Since
	}
	return s.RequestAudit(ctx, actor, in)
}

// FixAudit asks for the task for Claude to check and fix an audit's open
// findings. For a finished audit it is made now; for one still queued or
// running it is made when the audit finishes (WP-359), as if it had been
// asked for with --fix.
func (s *Service) FixAudit(ctx context.Context, actor *Actor, id int64) (*CodeAudit, error) {
	// The request is written only while the audit is in the status it was
	// checked in; one that moved on meanwhile is checked again. Statuses
	// only move forward, so this ends in a few rounds.
	for range 4 {
		a, err := s.auditToChange(ctx, actor, id)
		if err != nil {
			return nil, err
		}
		going := a.Status == AuditQueued || a.Status == AuditRunning
		switch {
		case a.FixIssue != "", going && a.Fix:
			return s.GetAudit(ctx, actor, id)
		case going:
		case a.Status != AuditDone:
			return nil, invalid("audit #%d ended %s, without findings to fix", id, a.Status)
		case a.Counts.Open == 0:
			return nil, invalid("audit #%d has no findings left to fix: each is fixed or rejected", id)
		}
		kept := false
		err = s.write(ctx, actor, func(t *tx) error {
			res, err := t.ExecContext(ctx, `UPDATE code_audits SET fix = 1 WHERE id = ? AND status = ?`, id, a.Status)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return nil
			}
			kept = true
			t.emit(&Event{Kind: EvCodeAudit, ProjectID: a.projectID, Field: "fix", Old: a.Status, New: a.Status, Data: map[string]any{"audit": id}})
			return nil
		})
		if err != nil {
			return nil, err
		}
		if !kept {
			continue
		}
		// The "fix" event makes the task if the audit is done by now; one
		// that finishes later makes it in auditEnded, which reads fix after
		// its status is written. One that finished in between may not have
		// got there yet: it is made here, so the caller gets its key.
		// auditFixTask makes it once either way.
		b, err := s.GetAudit(ctx, actor, id)
		if err == nil && going && b.Status == AuditDone && b.FixIssue == "" && b.Counts.Open > 0 {
			s.auditFixTask(ctx, id)
			b, err = s.GetAudit(ctx, actor, id)
		}
		if err == nil && b.FixIssue == "" && b.Status == AuditDone && b.Counts.Open > 0 {
			return nil, errors.New("the task for Claude couldn't be made: " + cmp(b.Error, "see the server log"))
		}
		return b, err
	}
	return nil, fmt.Errorf("audit #%d kept changing: try again", id)
}

// MarkFindings records verdicts on findings ("12.3"): confirmed, fixed,
// rejected, or open again.
type MarkFindings struct {
	IDs   []string `json:"ids"`
	State string   `json:"state"`
	Note  string   `json:"note"`
	Issue string   `json:"issue"`
}

func (s *Service) MarkAuditFindings(ctx context.Context, actor *Actor, in MarkFindings) ([]CodeAuditFinding, error) {
	state := strings.ToLower(strings.TrimSpace(in.State))
	if !slices.Contains(auditFindingStates, state) {
		return nil, invalid("a finding is %s", strings.Join(auditFindingStates, ", "))
	}
	if len(in.IDs) == 0 {
		return nil, invalid("say which findings (12.3: audit 12, finding 3)")
	}
	if len(in.IDs) > 100 {
		return nil, invalid("mark at most 100 findings at once")
	}
	note := strings.TrimSpace(in.Note)
	if utf8.RuneCountInString(note) > 2000 {
		return nil, invalid("a note is at most 2000 characters")
	}
	type ref struct {
		audit int64
		n     int
	}
	var refs []ref
	audits := map[int64]*CodeAudit{}
	for _, raw := range in.IDs {
		as, ns, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(raw), "#"), ".")
		aid, err1 := strconv.ParseInt(as, 10, 64)
		n, err2 := strconv.Atoi(ns)
		if !ok || err1 != nil || err2 != nil || aid <= 0 || n <= 0 {
			return nil, invalid("%q isn't a finding: write AUDIT.N, like 12.3", raw)
		}
		if audits[aid] == nil {
			a, err := s.auditToMark(ctx, actor, aid)
			if err != nil {
				return nil, err
			}
			audits[aid] = a
		}
		refs = append(refs, ref{aid, n})
	}
	var issueID any
	if strings.TrimSpace(in.Issue) != "" {
		id, err := s.resolveIssueID(ctx, s.db.R, in.Issue)
		if err != nil {
			return nil, err
		}
		issueID = id
	}
	var by any
	if actor.User != nil && actor.User.ID != 0 && !actor.IsAgent() {
		by = actor.User.ID
	}
	err := s.write(ctx, actor, func(t *tx) error {
		for _, r := range refs {
			res, err := t.ExecContext(ctx, `UPDATE code_audit_findings SET state = ?, note = CASE WHEN ? = '' THEN note ELSE ? END, issue_id = coalesce(?, issue_id),
				decided_by = ?, decided_agent = ?, decided_at = ? WHERE audit_id = ? AND ordinal = ?`,
				state, note, note, issueID, by, actor.agentName(), s.nowMS(), r.audit, r.n)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return notFound(fmt.Sprintf("finding %d.%d", r.audit, r.n))
			}
			// A review finding left open at merge is settled by the verdict (WP-367).
			if err := s.settleLeftoverFindings(ctx, t, actor, r.audit, r.n, state, note); err != nil {
				return err
			}
		}
		for id, a := range audits {
			t.emit(&Event{Kind: EvCodeAudit, ProjectID: a.projectID, Field: "findings", Old: a.Status, New: a.Status, Data: map[string]any{"audit": id}})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []CodeAuditFinding
	for id := range audits {
		a, err := s.GetAudit(ctx, actor, id)
		if err != nil {
			return nil, err
		}
		for _, f := range a.Findings {
			if slices.Contains(refs, ref{id, f.Ordinal}) {
				out = append(out, f)
			}
		}
	}
	slices.SortFunc(out, func(x, y CodeAuditFinding) int { return strings.Compare(x.ID, y.ID) })
	return out, nil
}

// readRange takes what the runner read from a job's result: the commits the
// change ran from and to, and how many commits that is.
func (a *CodeAudit) readRange(result map[string]any) {
	if v := anyString(result["base"]); v != "" {
		a.ToSHA = excerpt(v, 64)
	}
	if v := anyString(result["diff_from"]); v != "" {
		a.FromSHA = excerpt(v, 64)
	}
	if n, ok := result["change_commits"].(float64); ok && n >= 0 {
		c := int(n)
		a.CommitCount = &c
	}
}

// quietWhenClean: an audit Waypoint asked for itself (a footprint check, the
// "audit pull requests" setting's) that found nothing posts no comment
// (WP-867). Agents babysitting the pull request would only read and answer
// it; the audit's page, the pull request's footprint record and the merge's
// readiness already say it ran and cleared. A person's or an automation's
// audit still posts what they asked for.
func (a *CodeAudit) quietWhenClean() bool {
	return a.Via == "system" && len(a.Findings) == 0
}

// readNothing: the runner found nothing to read and ran no model.
func (a *CodeAudit) readNothing() bool {
	return len(a.Attempts) > 0 && a.Attempts[len(a.Attempts)-1].Outcome == "nothing"
}

func jsonList(list []string) string {
	if len(list) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(list)
	return string(b)
}

func listJSON(v string) []string {
	out := []string{}
	_ = json.Unmarshal([]byte(v), &out)
	if out == nil {
		out = []string{}
	}
	return out
}
