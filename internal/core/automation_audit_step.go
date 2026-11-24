package core

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// auditWait bounds how long a step waits: every model of a long chain at
// the full timeout, the busy retries, and time in the queue.
const auditWait = 12 * time.Hour

var auditTargetOptions = opts("", "The trigger's pull request, else the repository", "repo", "The whole repository", "branch", "A branch's change",
	"commit", "A commit", "range", "A range of commits", "recent", "What landed on a branch recently", "pr", "A pull request")

var auditSeverityOptions = opts("", "Everything", "medium", "Medium and worse", "high", "High and critical", "critical", "Critical only")

func init() {
	registerAction(ActionDef{Name: "audit", Label: "Audit the code", Group: "Agents", Phrase: "audit {target}", Long: true, Runner: true,
		Doc: "Ask a model to audit the code adversarially, read-only, from a checkout on a runner of the person who last saved the automation, and wait for its findings. " +
			"Unless `models` says otherwise the model is Grok 4.7 through Cursor on a runner first, then the gateway's audit model (`gateway/audit`); on a Waypoint with no LLM gateway, Grok 4.7 through Cursor alone. " +
			"`target` is repo, branch, commit, range, recent or pr (empty: the trigger's pull request, else the repository); `ref` the branch, commit, FROM..TO range or pull request number; `instructions` what to look for. " +
			"For recent, `since` is the period (7d, 2w, 1mo or a date; on a schedule, 7d each Monday audits the week), and `ref` the branch (empty: the default branch). " +
			"`focus` names areas to look hardest at (security, data, concurrency, errors, logic, performance, compat, tests); `paths` and `exclude` the parts of the repository to read and to leave out; " +
			"`min_severity` what to report, and `max_findings` how many findings are shown at a time (the model is told no number; the rest stay one click or `wp audit show ID --all` away). " +
			"The audit is kept in Waypoint (Audits), posted on a pull request, and handed to Claude only when `fix` is on. Outputs AuditID, URL, Status, Findings, Critical, High, Medium, Low, Summary, Model, FixIssue, Limited, Failed",
		Params: []ParamDef{
			{Name: "target", Label: "Audit", Type: PSelect, Options: auditTargetOptions},
			{Name: "ref", Label: "Which", Type: PTemplate, Placeholder: "main, a1b2c3d, v1.2..v1.3 or 42",
				Help: "The branch, commit, range (FROM..TO) or pull request number. For the repository or recent changes, the branch (empty: the default branch)."},
			{Name: "since", Label: "Since", Type: PTemplate, Default: auditDefaultSince, Placeholder: "7d, 2w, 1mo or 2026-10-28",
				Help: "What landed in this period, back from when the step runs.", When: &ParamWhen{Param: "target", In: []string{"recent"}}},
			{Name: "focus", Label: "Look hardest at", Type: PText, Placeholder: "security, data, concurrency",
				Help: "Any of security, data, concurrency, errors, logic, performance, compat and tests, comma separated."},
			{Name: "paths", Label: "Only these paths", Type: PGlobs, Advanced: true, Placeholder: "internal/core\nweb/src/**/*.tsx", Help: "From the repository's root, one per line (empty: all of it)."},
			{Name: "exclude", Label: "Leave out", Type: PGlobs, Advanced: true, Placeholder: "*_test.go\ndocs", Help: "Paths not to read or report on, one per line."},
			{Name: "min_severity", Label: "Report", Type: PSelect, Advanced: true, Options: auditSeverityOptions},
			{Name: "max_findings", Label: "Show at a time", Type: PNumber, Advanced: true, Placeholder: "15", Min: f64(1), Max: f64(auditMaxFindings),
				Help: "How many findings are listed first; the rest are counted and one click away. The model is told no number."},
			{Name: "base", Label: "Against", Type: PTemplate, Advanced: true, Placeholder: "main", Help: "What a branch is compared with (empty: the default branch)."},
			{Name: "repo", Label: "Repository", Type: PRepo, Advanced: true, Placeholder: "owner/name", Help: "Leave empty for the project's repository."},
			{Name: "instructions", Label: "Look for", Type: PTemplate, Default: DefaultAuditInstructions, Help: "A sentence is plenty. Wrap outside text in {{untrusted …}}."},
			{Name: "models", Label: "CLI and model", Type: PAgents, Advanced: true, Help: "Tried in order when one is busy, out of usage or down. Empty: Grok 4.7 (high, then medium) through Cursor on a runner, then the model the gateway picks for audits; Grok through Cursor alone on a Waypoint with no LLM gateway."},
			{Name: "comment", Label: "Post a pull request's audit on it", Type: PBool, Default: true},
			{Name: "fix", Label: "Have Claude check and fix the findings", Type: PBool, Default: false,
				Help: "Makes a low-priority task for Claude to check each finding against the code and fix the real ones."},
		},
		Outputs: []OutputDef{{Name: "AuditID", Type: "number"}, {Name: "URL", Type: "text", Doc: "the audit in Waypoint"},
			{Name: "Status", Type: "text", Doc: "done, failed, limited or cancelled"}, {Name: "Findings", Type: "number"},
			{Name: "Critical", Type: "number"}, {Name: "High", Type: "number"}, {Name: "Medium", Type: "number"}, {Name: "Low", Type: "number"},
			{Name: "Summary", Type: "text", Doc: "the model's summary"}, {Name: "Model", Type: "text", Doc: "the CLI and model that answered"},
			{Name: "FixIssue", Type: "text", Doc: "Claude's task, when fix is on and there were findings"}, {Name: "CommentURL", Type: "text"},
			{Name: "Limited", Type: "bool", Doc: "a usage limit stopped every model (the plan's pool is spent)"}, {Name: "Failed", Type: "bool"}},
		Check: func(_ *Rule, a *Action) error {
			if t := anyString(a.Extra["target"]); t != "" && !strings.Contains(t, "{{") && !slices.Contains(auditTargets, t) {
				return invalid("audit: target is one of %s", strings.Join(auditTargets, ", "))
			}
			if _, err := cleanAuditCriteria(AuditInput{Focus: anyStrings(a.Extra["focus"]), Paths: anyStrings(a.Extra["paths"]), Exclude: anyStrings(a.Extra["exclude"]),
				MinSeverity: anyString(a.Extra["min_severity"]), MaxFindings: anyInt(a.Extra["max_findings"])}); err != nil {
				return invalid("audit: %s", PublicError(err))
			}
			if v := anyString(a.Extra["since"]); v != "" && !strings.Contains(v, "{{") {
				if _, _, err := parseAuditSince(v, time.Now()); err != nil {
					return invalid("audit: %s", PublicError(err))
				}
			}
			if a.Extra["models"] != nil {
				if _, err := agentChain(a.Extra["models"]); err != nil {
					return err
				}
			}
			return nil
		},
		Run: auditStep,
		Describe: func(c *StepCtx) string {
			what, _ := c.Render("instructions")
			target := cmp(c.Str("target"), "the pull request or repository")
			if target == "recent" {
				since, _ := c.Render("since")
				target = "what landed in the last " + cmp(since, auditDefaultSince)
			}
			return fmt.Sprintf("Would ask for an audit of %s on a runner of the person who last saved this, looking for: %s", target,
				excerpt(cmp(what, DefaultAuditInstructions), 200))
		}})
}

func auditStep(c *StepCtx) error {
	const slot = 0
	var id int64
	if w, err := c.S.stepWait(c.Ctx, c.Run.ID, c.ID, c.Iteration, c.slotSeq(slot)); err != nil {
		return err
	} else if w != nil {
		id, _ = strconv.ParseInt(strings.TrimPrefix(w.key, "audit:"), 10, 64)
	}
	if id == 0 {
		in, err := c.auditInput()
		if err != nil {
			return err
		}
		a, err := c.S.RequestAudit(c.Ctx, c.Actor, in)
		if err != nil {
			return err
		}
		id = a.ID
		c.Logf("asked for audit #%d", id)
	}
	c.Set("AuditID", id)
	c.Set("URL", c.S.BaseURL()+fmt.Sprintf("/audits/%d", id))
	settled := func() (*Event, bool) {
		if c.S.auditSettled(c.Ctx, id) {
			return &Event{Kind: EvCodeAudit, Field: "settled", Data: map[string]any{"audit": id}}, true
		}
		return nil, false
	}
	_, timedOut, err := c.WaitSlot(slot, EvCodeAudit, "audit:"+itoa64(id), "field=settled", auditWait, settled)
	if err != nil {
		return err
	}
	if timedOut {
		return fmt.Errorf("audit #%d hadn't finished after %s", id, auditWait)
	}
	a, err := c.S.auditRow(c.Ctx, id)
	if err != nil {
		return err
	}
	c.Set("Status", a.Status)
	c.Set("Findings", a.Counts.Total)
	c.Set("Critical", a.Counts.Critical)
	c.Set("High", a.Counts.High)
	c.Set("Medium", a.Counts.Medium)
	c.Set("Low", a.Counts.Low)
	c.Set("Summary", a.Summary)
	c.Set("Model", a.Model)
	c.Set("FixIssue", a.FixIssue)
	c.Set("CommentURL", a.CommentURL)
	c.Set("Limited", a.Status == AuditLimited)
	c.Set("Failed", a.Status == AuditFailed || a.Status == AuditLimited)
	switch a.Status {
	case AuditFailed:
		return fmt.Errorf("audit #%d failed: %s", id, cmp(a.Error, "every model failed"))
	case AuditLimited:
		return fmt.Errorf("audit #%d stopped at a usage limit: %s", id, a.Error)
	case AuditCancelled:
		return fmt.Errorf("audit #%d was cancelled", id)
	}
	return nil
}

// auditInput reads the step's settings into a request.
func (c *StepCtx) auditInput() (AuditInput, error) {
	var in AuditInput
	pid := c.stepProject()
	if pid == 0 {
		return in, invalid("audit needs a project with a repository")
	}
	repo, err := c.Repo("repo")
	if err != nil {
		return in, err
	}
	ref, err := c.Render("ref")
	if err != nil {
		return in, err
	}
	base, err := c.Render("base")
	if err != nil {
		return in, err
	}
	what, err := c.Render("instructions")
	if err != nil {
		return in, err
	}
	chain, err := agentChain(c.Param("models"))
	if err != nil {
		return in, err
	}
	target, err := c.Render("target")
	if err != nil {
		return in, err
	}
	since, err := c.Render("since")
	if err != nil {
		return in, err
	}
	comment := c.BoolDefault("comment", true)
	in = AuditInput{Project: itoa64(pid), Repo: repo.Repo, Target: strings.ToLower(strings.TrimSpace(target)), Ref: strings.TrimSpace(ref), Base: strings.TrimSpace(base),
		Instructions: what, chain: chain, Comment: &comment, Fix: c.Bool("fix"), Via: "automation",
		Focus: splitList([]string{c.Str("focus")}), Paths: c.Strings("paths"), Exclude: c.Strings("exclude"), MinSeverity: c.Str("min_severity"),
		MaxFindings: c.Int("max_findings", 0), requestKey: c.autoRequestKey(0)}
	if in.Target == "recent" {
		in.Since = cmp(strings.TrimSpace(since), auditDefaultSince)
	}
	pr := c.Data().PR
	switch in.Target {
	case "":
		if pr != nil {
			in.Target, in.PRID = "pr", pr.ID
		} else if strings.Contains(in.Ref, "..") {
			in.Target = "range"
		} else {
			in.Target = "repo"
		}
	case "pr":
		if in.Ref == "" && pr != nil {
			in.PRID = pr.ID
			break
		}
		n, err := strconv.Atoi(strings.TrimPrefix(in.Ref, "#"))
		if err != nil || n <= 0 {
			return in, invalid("audit: say which pull request by its number")
		}
		in.PR, in.Ref = n, ""
	}
	return in, nil
}

// anyInt is a number setting, 0 when it isn't one.
func anyInt(v any) int {
	f, _ := anyFloat(v)
	return int(f)
}
