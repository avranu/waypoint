package core

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// MeaningReport is how the judgements of meaning stand (wp usage meaning,
// WP-1035): figures, no verdicts.
type MeaningReport struct {
	At         time.Time               `json:"at"`
	EmbedModel string                  `json:"embed_model"`
	NLIModel   string                  `json:"nli_model"`
	LLM        string                  `json:"llm"` // how the language model is asked
	Judgements []MeaningJudgementState `json:"judgements"`
	Feedback   MeaningFeedbackState    `json:"feedback"`
	Shown      map[string]int          `json:"shown"` // suggestions shown, by outcome ("" open)
	Recovery   *MeaningRecovery        `json:"parent_recovery,omitempty"`
}

// MeaningJudgementState is one question's.
type MeaningJudgementState struct {
	Judgement string         `json:"judgement"`
	Decider   string         `json:"decider"`
	Fit       *meaningFit    `json:"fit,omitempty"`
	Labels    map[string]int `json:"labels"` // "source yes|no"
	// Since the fit: labels that came after it, and the decider's errors on
	// them, per answer.
	SinceYes    int `json:"since_yes"`
	SinceYesErr int `json:"since_yes_errors"`
	SinceNo     int `json:"since_no"`
	SinceNoErr  int `json:"since_no_errors"`
	PairsYes    int `json:"pairs_yes"`
	PairsNo     int `json:"pairs_no"`
	// The language model's scores of the question: graded, failed and to be
	// tried again, given up.
	Graded  int `json:"graded"`
	Failing int `json:"failing"`
	GivenUp int `json:"given_up"`
}

// MeaningFeedbackState is the reports on the gateway's answers.
type MeaningFeedbackState struct {
	Sent    int `json:"sent"`
	Pending int `json:"pending"`
	Failing int `json:"failing"`
}

// MeaningRecovery: how often a goal's nearest goal by embedding is its
// parent (hit@1) and the mean reciprocal rank of the parent, over goals
// with a parent: how much the embedding alone knows of the hierarchy.
type MeaningRecovery struct {
	Goals int     `json:"goals"`
	Hit1  float64 `json:"hit_at_1"`
	MRR   float64 `json:"mrr"`
}

const meaningRecoveryMax = 2000

// MeaningState reports the judgements of meaning. Admins only: it reads
// every project's goals.
func (s *Service) MeaningState(ctx context.Context, actor *Actor) (*MeaningReport, error) {
	if err := actor.Need(PermUsageRead); err != nil {
		return nil, err
	}
	if !actor.IsAdmin() {
		return nil, forbidden("the judgements of meaning read every project's goals: admins only")
	}
	s.goals.mu.Lock()
	embed, nli := s.goals.meaningEmbed, s.goals.meaningNLI
	s.goals.mu.Unlock()
	rep := &MeaningReport{At: s.now().UTC(), EmbedModel: embed, NLIModel: nli, LLM: "none", Shown: map[string]int{}}
	if ch := s.meaningChainNow(ctx); ch != nil {
		rep.LLM = "the " + ch.task + " task's models"
		if ch.gateway {
			rep.LLM = "the gateway: " + strings.Join(gatewayModels(gatewayJudgePurpose), ", then ")
			if ex := gatewayJudgeExclude(); len(ex) > 0 && s.gatewayWith(ctx, GatewayExclude) != 0 {
				rep.LLM += ", leaving out " + strings.Join(ex, ", ")
			}
		}
	}
	st, err := s.meaningLoad(ctx, embed, nli)
	if err != nil {
		return nil, err
	}
	goals, order, err := s.meaningGoals(ctx)
	if err != nil {
		return nil, err
	}
	ctxOf := map[string]string{}
	for _, g := range goals {
		ctxOf[g.hash] = g.ctx
	}
	for _, j := range meaningJudgements {
		js := MeaningJudgementState{Judgement: j, Decider: "prior", Labels: map[string]int{}}
		fit, err := s.meaningFitLatest(ctx, j)
		if err != nil {
			return nil, err
		}
		js.Fit = fit
		var d *meaningDecider
		if fit != nil {
			d = fit.decider()
			js.Decider = d.label()
		}
		rows, _, err := s.meaningEvalRows(ctx, st, nil, ctxOf, j)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			ans := "no"
			if r.y {
				ans = "yes"
			}
			js.Labels[r.source+" "+ans]++
			if fit == nil || r.id <= fit.LabelThrough {
				continue
			}
			_, yes, _, _ := d.decide(j, r.f)
			if r.y {
				js.SinceYes++
				if !yes {
					js.SinceYesErr++
				}
			} else {
				js.SinceNo++
				if yes {
					js.SinceNoErr++
				}
			}
		}
		if err := s.db.R.QueryRowContext(ctx, `SELECT coalesce(sum(yes), 0), coalesce(sum(1 - yes), 0) FROM goal_meaning_pairs WHERE judgement = ?`, j).Scan(&js.PairsYes, &js.PairsNo); err != nil {
			return nil, err
		}
		if err := s.db.R.QueryRowContext(ctx, `SELECT coalesce(sum(score IS NOT NULL), 0), coalesce(sum(score IS NULL AND tries < ?), 0), coalesce(sum(score IS NULL AND tries >= ?), 0)
			FROM meaning_scores WHERE scorer = ?`, meaningMaxTries, meaningMaxTries, llmScorer(j)).Scan(&js.Graded, &js.Failing, &js.GivenUp); err != nil {
			return nil, err
		}
		rep.Judgements = append(rep.Judgements, js)
	}
	if err := s.db.R.QueryRowContext(ctx, `SELECT coalesce(sum(reported_at IS NOT NULL), 0), coalesce(sum(reported_at IS NULL AND error = ''), 0), coalesce(sum(reported_at IS NULL AND error != ''), 0) FROM meaning_feedback`).
		Scan(&rep.Feedback.Sent, &rep.Feedback.Pending, &rep.Feedback.Failing); err != nil {
		return nil, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT outcome, count(*) FROM goal_meaning_shown GROUP BY outcome`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var o string
		var n int
		if rows.Scan(&o, &n) == nil {
			rep.Shown[o] = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rep.Recovery = meaningParentRecovery(st, goals, order)
	return rep, nil
}

// meaningParentRecovery ranks each goal's parent among the other goals (not
// its descendants) by title embedding.
func meaningParentRecovery(st *meaningStore, goals map[int64]*meaningGoal, order []int64) *MeaningRecovery {
	if len(order) > meaningRecoveryMax || st.embedModel == "" {
		return nil
	}
	r := &MeaningRecovery{}
	for _, id := range order {
		g := goals[id]
		if g.parent == 0 || st.vec[g.hash] == nil || st.vec[goals[g.parent].hash] == nil {
			continue
		}
		pc := vecCosine(st.vec[g.hash], st.vec[goals[g.parent].hash])
		rank := 1
		for _, o := range order {
			og := goals[o]
			if o == id || o == g.parent || og.ancestors[id] || st.vec[og.hash] == nil {
				continue
			}
			if vecCosine(st.vec[g.hash], st.vec[og.hash]) > pc {
				rank++
			}
		}
		r.Goals++
		if rank == 1 {
			r.Hit1++
		}
		r.MRR += 1 / float64(rank)
	}
	if r.Goals == 0 {
		return nil
	}
	r.Hit1 /= float64(r.Goals)
	r.MRR /= float64(r.Goals)
	return r
}

func (r *MeaningReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Judgements of meaning about goals (WP-1035), as of %s:\n", r.At.Format("2006-01-02 15:04"))
	none := func(s string) string {
		if s == "" {
			return "not known (the meaning service has not answered)"
		}
		return s
	}
	fmt.Fprintf(&b, "  embedding model: %s\n  NLI model: %s\n  language model: %s\n", none(r.EmbedModel), none(r.NLIModel), r.LLM)
	for _, j := range r.Judgements {
		fmt.Fprintf(&b, "\n  %s: decided by %s; %d pairs yes, %d no\n", j.Judgement, j.Decider, j.PairsYes, j.PairsNo)
		fmt.Fprintf(&b, "    language model: %d graded, %d failed and to be tried again, %d given up\n", j.Graded, j.Failing, j.GivenUp)
		keys := make([]string, 0, len(j.Labels))
		for k := range j.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %d", k, j.Labels[k]))
		}
		if len(parts) == 0 {
			parts = []string{"none"}
		}
		fmt.Fprintf(&b, "    labels: %s\n", strings.Join(parts, ", "))
		if f := j.Fit; f != nil {
			fmt.Fprintf(&b, "    evaluated %s (%s, %s): %s at %.3f; held out %d of %d labels; missed %.0f%% of yes, said yes to %.0f%% of no\n",
				msTime(f.FittedAt).Format("2006-01-02 15:04"), f.Reason, f.Note, f.Decider, f.Line, f.HeldOut, f.Labels, f.ErrPos*100, f.ErrNeg*100)
			for _, c := range f.Candidates {
				fmt.Fprintf(&b, "      %-10s line %.3f  balanced accuracy %.3f  missed %.0f%%  false yes %.0f%%  own score on %d\n",
					c.Name, c.Line, c.Balanced, c.ErrPos*100, c.ErrNeg*100, c.Scored)
			}
			fmt.Fprintf(&b, "    since: %d yes labels (%d missed), %d no labels (%d said yes)\n", j.SinceYes, j.SinceYesErr, j.SinceNo, j.SinceNoErr)
		}
	}
	fmt.Fprintf(&b, "\n  reports to the gateway: %d sent, %d to send, %d failed and to be tried again\n", r.Feedback.Sent, r.Feedback.Pending, r.Feedback.Failing)
	if len(r.Shown) > 0 {
		keys := make([]string, 0, len(r.Shown))
		for k := range r.Shown {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			name := k
			if name == "" {
				name = "open"
			}
			parts = append(parts, fmt.Sprintf("%s %d", name, r.Shown[k]))
		}
		fmt.Fprintf(&b, "  suggestions shown to people: %s\n", strings.Join(parts, ", "))
	}
	if r.Recovery != nil {
		fmt.Fprintf(&b, "  parent recovery by embedding: over %d goals, the nearest goal is the parent %.0f%% of the time; mean reciprocal rank %.2f\n",
			r.Recovery.Goals, r.Recovery.Hit1*100, r.Recovery.MRR)
	}
	return b.String()
}
