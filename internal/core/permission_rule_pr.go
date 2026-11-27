package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// checkRuleTarget: the repo a rule would go to, for a person approving r
// with one.
func (s *Service) checkRuleTarget(ctx context.Context, actor *Actor, r *Request, rule string) (*setupRepo, error) {
	if r.Kind != RequestPermission {
		return nil, invalid("only a permission request is approved with a rule")
	}
	if r.Tool == nil {
		return nil, invalid("%s doesn't say which call was refused", r.Code)
	}
	if err := checkPermissionRule(r.Tool.Name, rule); err != nil {
		return nil, err
	}
	if r.Session == nil || r.Session.Repo == "" {
		return nil, invalid("%s's session didn't say which repo it ran in, so there is no .claude/settings.json to add the rule to; approve it without one", r.Code)
	}
	repo, err := s.setupRepoFor(ctx, actor, 0, r.Session.Repo, RoleEditor)
	if errors.Is(err, ErrNotFound) {
		return nil, invalid("%s isn't attached to a project you can see, so Waypoint can't add the rule there; approve without one", r.Session.Repo)
	}
	return repo, err
}

// permRuleJob is an allow rule a person picked, added in event dispatch
// once the pick has committed (REVIEW.md: forge calls don't run inside a
// mutation): for an approved permission request, or for a refused call in
// the log (refusal).
type permRuleJob struct {
	request int64
	refusal int64
	at      int64
	repo    *setupRepo
	rule    string
}

// addPermissionRule adds the job's rule to the repo's .claude/settings.json
// by pull request, labelled automerge (the person approved this one line),
// and records the outcome on the request. Result.Rule is set only when the
// rule is in effect or on its way (already in the file, or a PR adds it);
// when it couldn't be added, Reason says why and Rule stays empty, so
// nobody is told a rule was added.
func (s *Service) addPermissionRule(ctx context.Context, actor *Actor, j *permRuleJob) {
	if j.refusal != 0 {
		s.addRefusalRule(ctx, actor, j)
		return
	}
	list, err := s.loadRequests(ctx, SystemActor(), true, `r.id = ?`, j.request)
	if err != nil || len(list) == 0 {
		s.log.ErrorContext(ctx, "add a permission rule: load", "request", j.request, "err", err)
		return
	}
	r := list[0]
	res := r.Result
	pr, err := s.openRulePR(ctx, actor, r.Tool, "with "+r.Ref+", a call auto mode refused", j.repo, j.rule)
	switch {
	case errors.Is(err, errRulePresent):
		res.Rule = j.rule
		res.Reason = fmt.Sprintf("%s already allows %s on its default branch, so no pull request was needed.", j.repo.repo, j.rule)
	case err != nil:
		res.Reason = fmt.Sprintf("The rule %s wasn't added: %s", j.rule, excerpt(err.Error(), 400))
		s.log.WarnContext(ctx, "add a permission rule", "request", r.ID, "err", err)
	default:
		res.Rule = j.rule
		res.Branch, res.PRURL, res.PRNumber = pr.Branch, pr.URL, pr.Number
		res.PRTitle = ruleTitle(j.rule)
	}
	b, _ := json.Marshal(res)
	if _, err := s.db.W.ExecContext(ctx, `UPDATE requests SET result = ?, updated_at = ? WHERE id = ?`, string(b), s.nowMS(), r.ID); err != nil {
		s.log.ErrorContext(ctx, "record a permission rule", "request", r.ID, "rule", j.rule, "pr", res.PRURL, "err", err)
	}
}

const settingsPath = ".claude/settings.json"

// openRulePR opens the pull request adding rule for tool, a refused call;
// from says where the person picked it ("with R-12, a call auto mode
// refused").
func (s *Service) openRulePR(ctx context.Context, actor *Actor, tool *ToolCall, from string, repo *setupRepo, rule string) (*AgentFileEditResult, error) {
	af, err := s.agentForge(ctx, repo.forgeID)
	if err != nil {
		return nil, err
	}
	base, err := af.commit.DefaultBranch(ctx, af.f, repo.repo)
	if err != nil {
		return nil, err
	}
	cur, err := af.commit.FileBlobSHA(ctx, af.f, repo.repo, settingsPath, base)
	if err != nil {
		return nil, err
	}
	old := ""
	if cur != "" {
		if old, _, err = af.readAt(ctx, repo.repo, settingsPath, base); err != nil {
			return nil, err
		}
	}
	content, err := addAllowRule(old, rule)
	if err != nil {
		return nil, err
	}
	title := ruleTitle(rule)
	text := truncateBytes(toolText(tool), 4000)
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	body := fmt.Sprintf("Adds `%s` to `permissions.allow` in %s, so agents' sessions in this repo run it without asking Claude Code's auto-mode classifier.\n\n"+
		"%s picked it in Waypoint %s (WP-233):\n\n%s\n%s\n%s",
		rule, settingsPath, actor.User.DisplayName, from, fence, text, fence)
	return s.openAgentFilePR(ctx, actor, repo, af, agentFilePR{base: base, path: settingsPath, baseSHA: cur, content: content,
		branch: "allow-" + slug(rule, 30), message: title + "\n\n" + body, title: title, body: body, labels: []string{automergeLabel}})
}

func ruleTitle(rule string) string {
	return "Allow " + excerpt(rule, 100) + " for agents"
}

// toolText is the refused call as a person reads it.
func toolText(t *ToolCall) string {
	if t == nil {
		return ""
	}
	if _, full := toolSummary(t.Name, t.Input); full != "" {
		return full
	}
	return string(t.Input)
}

// permissionApprovedText tells the agent what approving its refused call
// changed. By then it has usually moved on, so nothing here asks it to retry
// (WP-ADR114): it runs the call again only if it still needs it.
func permissionApprovedText(r *Request) string {
	switch res := r.Result; {
	case res.Rule != "" && res.PRURL != "":
		return fmt.Sprintf("They allowed %s for later sessions: %s adds it to %s and merges once its checks pass. Auto mode in this session is unchanged; run the call again only if you still need it.\n", res.Rule, res.PRURL, settingsPath)
	case res.Rule != "":
		return fmt.Sprintf("They chose the rule %s for later sessions. %s\n", res.Rule, res.Reason)
	case res.Reason != "":
		return fmt.Sprintf("They picked a rule for later sessions, but it wasn't added, so auto mode still decides this call. %s\n", res.Reason)
	}
	return "No rule was added, so auto mode still decides this call; run it again only if you still need it.\n"
}
