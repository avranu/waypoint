package core

import "context"

// Runner steps use the runners of whoever last saved the automation, never the trigger's, so an editable rule can't run commands on another person's machine.

// AutomationRunners is what the editor says about the runners an automation
// will use.
type AutomationRunners struct {
	Owner  *UserRef `json:"owner"`  // nil: nobody can be named (an agent saved it)
	Total  int      `json:"total"`  // the owner's runners
	Online int      `json:"online"` // … that are online now
}

// automationRunnerOwner is the person whose runners an automation may use:
// whoever last saved it (0: nobody, so runner steps are refused).
func automationRunnerOwner(a *Automation) int64 {
	if a == nil {
		return 0
	}
	return a.savedBy
}

// usesRunner: some step runs on a runner.
func (r *Rule) usesRunner() bool {
	for _, a := range flatSteps(r.Actions) {
		if d := actionDef(a.Type); d != nil && d.Runner {
			return true
		}
	}
	return false
}

// fillOwner adds who saved an automation and, for one with runner steps,
// the state of that person's runners.
func (s *Service) fillOwner(ctx context.Context, a *Automation) {
	if a.savedBy != 0 {
		if u, err := s.userByID(ctx, s.db.R, a.savedBy); err == nil {
			ref := u.Ref()
			a.SavedBy = &ref
		}
	}
	if !a.Rule.usesRunner() {
		return
	}
	rn := &AutomationRunners{Owner: a.SavedBy}
	if a.savedBy != 0 {
		if list, err := s.ownerRunners(ctx, a.savedBy); err == nil {
			rn.Total = len(list)
			for _, r := range list {
				if r.Online {
					rn.Online++
				}
			}
		}
	}
	a.Runners = rn
}
