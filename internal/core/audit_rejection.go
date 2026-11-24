package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"git.jmann.me/claude/waypoint/internal/core/kernel"
	"git.jmann.me/claude/waypoint/internal/core/strength"
)

// MaxRejectionEvidence is how long the evidence for rejecting an audit
// finding may be, in characters.
const MaxRejectionEvidence = 4000

// rejectionRecord is what is kept with an agent's rejection of a finding: the
// model and effort it ran as, and how that compared with the finder.
type rejectionRecord struct {
	model, effort, strength string
}

// rejectsOwnRound: the actor is the round's own job, or the session the
// round was started for, rejecting findings of that round's audits. Those
// are the round's to answer and aren't held to the finder's strength (WP-750).
func (s *Service) rejectsOwnRound(ctx context.Context, actor *Actor, audit int64, key string) bool {
	if actor.RunnerJob != 0 && s.roundOnAudit(ctx, actor.RunnerJob, audit) {
		return true
	}
	rest, ok := strings.CutPrefix(key, "review-round:")
	if !ok || actor.Session == 0 {
		return false
	}
	num, _, _ := strings.Cut(rest, ":")
	round, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return false
	}
	var one int
	return s.db.R.QueryRowContext(ctx, `SELECT 1 FROM work_log WHERE id = ? AND kind = 'review' AND session_id = ?`, round, actor.Session).Scan(&one) == nil
}

// findingFinder is the model that found a finding: the helper panel's for a
// finding made of its leads (as PanelFinding reads the lead column: a value
// naming no panel check is no panel finding), else the model that answered
// the audit. Audits don't record an effort.
func (s *Service) findingFinder(ctx context.Context, audit int64, n int) (strength.Model, error) {
	var model, key, lead string
	err := s.db.R.QueryRowContext(ctx, `SELECT a.model, a.request_key, f.lead FROM code_audit_findings f JOIN code_audits a ON a.id = f.audit_id WHERE f.audit_id = ? AND f.ordinal = ?`, audit, n).Scan(&model, &key, &lead)
	if err != nil {
		return strength.Model{}, err
	}
	if PanelFinding(CodeAuditFinding{Lead: lead}) {
		model = helperModel
	}
	if model == ownReportModel {
		// A review's own report: the model is the author session's.
		var author string
		if rest, ok := strings.CutPrefix(key, "review-round:"); ok {
			num, _, _ := strings.Cut(rest, ":")
			_ = s.db.R.QueryRowContext(ctx, `SELECT s.model FROM work_log w JOIN agent_sessions s ON s.id = w.session_id WHERE w.id = ? AND w.kind = 'review'`, num).Scan(&author)
		}
		model = author
	}
	return currentVersion(strength.Parse(model, "")), nil
}

// currentVersion gives a bare haiku, sonnet or opus (the claude CLI's short
// name, as audits and sessions record it) the version it stands for now, so
// an older model can't reject what an unversioned name hides.
func currentVersion(m strength.Model) strength.Model {
	if m.Major == 0 {
		if full, ok := runnerAliases[m.Family]; ok {
			a := strength.Parse(full, m.Effort)
			a.Name = m.Name
			return a
		}
	}
	return m
}

// rejecterModel is the model and effort the actor runs as, as reported: a
// runner job's own input, else its session's (what the hook was told).
func (s *Service) rejecterModel(ctx context.Context, actor *Actor) strength.Model {
	var model, effort string
	if actor.RunnerJob != 0 {
		var input string
		if s.db.R.QueryRowContext(ctx, `SELECT input FROM runner_jobs WHERE id = ?`, actor.RunnerJob).Scan(&input) == nil {
			var in struct{ Model, Effort string }
			_ = json.Unmarshal([]byte(input), &in)
			model, effort = in.Model, in.Effort
		}
	}
	if actor.Session != 0 {
		if model == "" {
			_ = s.db.R.QueryRowContext(ctx, `SELECT model FROM agent_sessions WHERE id = ?`, actor.Session).Scan(&model)
		}
		if effort == "" {
			err := s.db.R.QueryRowContext(ctx, `SELECT value FROM session_observed WHERE session_id = ? AND kind = 'effort' ORDER BY at DESC, id DESC LIMIT 1`, actor.Session).Scan(&effort)
			if errors.Is(err, sql.ErrNoRows) {
				var e sql.NullString
				_ = s.db.R.QueryRowContext(ctx, `SELECT effort FROM session_snapshots WHERE session_id = ? AND effort IS NOT NULL ORDER BY at DESC, id DESC LIMIT 1`, actor.Session).Scan(&e)
				effort = e.String
			}
		}
	}
	return currentVersion(strength.Parse(model, effort))
}

// weighRejections holds an agent's rejection of findings to the rule R-569
// decided (WP-1825): outside the round's own, it gives evidence and is as
// strong as the model that found each finding. It returns what to keep with
// each verdict.
func (s *Service) weighRejections(ctx context.Context, actor *Actor, evidence string, refs []findingRef) (map[findingRef]rejectionRecord, error) {
	out := map[findingRef]rejectionRecord{}
	var rejecter *strength.Model
	for _, r := range refs {
		var key string
		if err := s.db.R.QueryRowContext(ctx, `SELECT request_key FROM code_audits WHERE id = ?`, r.audit).Scan(&key); err != nil {
			return nil, err
		}
		if s.rejectsOwnRound(ctx, actor, r.audit, key) {
			continue
		}
		id := fmt.Sprintf("%d.%d", r.audit, r.n)
		if evidence == "" {
			return nil, kernel.Invalid("rejecting a finding of a pull request you aren't working on takes evidence: wp audit mark %s rejected -m WHY --evidence \"the file and line, or the command output, that shows it\"", id)
		}
		finder, err := s.findingFinder(ctx, r.audit, r.n)
		if errors.Is(err, sql.ErrNoRows) {
			continue // the update says it is not found
		}
		if err != nil {
			return nil, err
		}
		if rejecter == nil {
			m := s.rejecterModel(ctx, actor)
			rejecter = &m
		}
		verdict, reason := strength.Compare(*rejecter, finder)
		if verdict == strength.Weaker {
			who := rejecter.String() + " can't"
			if !rejecter.Known() {
				who = "Waypoint doesn't know which model this session runs, so it can't"
				if rejecter.Name != "" {
					who = fmt.Sprintf("Waypoint doesn't rank %s, the model this session runs, so it can't", rejecter.Name)
				}
			}
			return nil, kernel.Forbidden("%s reject %s, found by %s: leave it for a stronger agent or a person (a medium or low one can be filed: wp audit mark %s confirmed --issue KEY)", who, id, finder, id)
		}
		out[r] = rejectionRecord{model: rejecter.Name, effort: rejecter.Effort, strength: string(verdict) + ": " + reason}
	}
	return out, nil
}

func checkEvidence(evidence string) error {
	if utf8.RuneCountInString(evidence) > MaxRejectionEvidence {
		return kernel.Invalid("evidence is at most %d characters", MaxRejectionEvidence)
	}
	return nil
}

// RejectedAs says what model and effort rejected a finding and how that
// compared with the finder's, for the page that shows the verdict.
func RejectedAs(f CodeAuditFinding) string {
	s := f.DecidedModel
	if f.DecidedEffort != "" {
		s += " (" + f.DecidedEffort + ")"
	}
	if f.DecidedStrength != "" {
		s += ", " + strings.Replace(f.DecidedStrength, "_", " ", 1)
	}
	return s
}
