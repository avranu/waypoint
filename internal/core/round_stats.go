package core

import (
	"context"
	"encoding/json"
	"slices"
)

// RoundStats counts babysitting rounds by class over a span (WP-438).
type RoundStats struct {
	Days    int `json:"days"`
	PRs     int `json:"prs"`
	Rounds  int `json:"rounds"`
	Classed int `json:"classed"` // rounds with a class: recorded, or replayed
	// Class: what each round came to, nothing_pushed when its agent pushed
	// nothing. WorkClass: what it was given, whatever came of it.
	Class     map[string]int `json:"class"`
	WorkClass map[string]int `json:"work_class"`
	// Replayed: rounds from before the ledger, classified now from the
	// comments as they stand (replay only).
	Replayed int `json:"replayed"`
	// The ladder (WP-439): rounds on the stronger model, assessor rounds,
	// rounds after which the pull request parked for a person, and rounds
	// after which it rested with nobody asked (WP-930).
	Strong   int `json:"strong"`
	Assessor int `json:"assessor"`
	Parked   int `json:"parked"`
	Rested   int `json:"rested"`
	// Merged: babysat pull requests that merged in the span; MergedRounds,
	// all the rounds they took.
	Merged       int `json:"merged"`
	MergedRounds int `json:"merged_rounds"`
	// Nitpicks: what scoring review comments and review findings did
	// (WP-695).
	Nitpicks *NitpickStats `json:"nitpicks,omitempty"`
	// Failed checks a waiting author took instead of a round (WP-1878): how
	// many; of them a new head came before the deadline (any new head, not
	// only the author's), a round took, the checks passed on the same head, or
	// the hand-off is still pending. The four add up to the first.
	AuthorHandoffs int `json:"author_handoffs"`
	AuthorFixed    int `json:"author_fixed"`
	AuthorFellBack int `json:"author_fell_back"`
	AuthorCleared  int `json:"author_cleared"`
	AuthorPending  int `json:"author_pending"`
}

// maxReplayDays bounds a replay: it reads each pull request's files from
// its forge.
const maxReplayDays = 31

// GetRoundStats counts the babysitting rounds of the last days (1-365) on
// the pull requests the actor can see, in a project or everywhere. With
// replay, rounds from before the ledger are classified now.
func (s *Service) GetRoundStats(ctx context.Context, actor *Actor, projectRef string, days int, replay bool) (*RoundStats, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	if days <= 0 {
		days = 7
	}
	if days > 365 {
		return nil, invalid("days must be 1-365")
	}
	if replay && days > maxReplayDays {
		return nil, invalid("a replay covers at most 31 days")
	}
	var projectID int64
	if projectRef != "" {
		p, err := s.projectByRef(ctx, s.db.R, projectRef)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		projectID = p.ID
	}
	since := s.now().AddDate(0, 0, -days)
	vis, args := prVisible(actor, projectID)
	// #nosec G202 -- fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT b.pr_id, b.rounds, b.state = ? AND coalesce(b.ended_at, 0) >= ?, b.author_handoffs FROM babysits b JOIN pull_requests p ON p.id = b.pr_id
		WHERE `+vis+` AND b.updated_at >= ? ORDER BY b.id`, append([]any{BabysitMerged, since.UnixMilli()}, append(args, since.UnixMilli())...)...)
	if err != nil {
		return nil, err
	}
	type kept struct {
		pr       int64
		rounds   []BabysitRound
		merged   bool
		handoffs string
	}
	var all []kept
	for rows.Next() {
		var k kept
		var raw string
		if err := rows.Scan(&k.pr, &raw, &k.merged, &k.handoffs); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = json.Unmarshal([]byte(raw), &k.rounds)
		all = append(all, k)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := &RoundStats{Days: days, Class: map[string]int{}, WorkClass: map[string]int{}}
	prs := map[int64]bool{}
	for _, k := range all {
		if k.merged {
			out.Merged++
			out.MergedRounds += len(k.rounds)
		}
		h := handoffCounts(k.handoffs, since)
		out.AuthorHandoffs += h.Handed
		out.AuthorFixed += h.Fixed
		out.AuthorFellBack += h.FellBack
		out.AuthorCleared += h.Cleared
		out.AuthorPending += h.Pending
		var inSpan []BabysitRound
		for _, r := range k.rounds {
			if !r.At.Before(since) {
				inSpan = append(inSpan, r)
			}
		}
		strong, assessor, parked, rested := parkStats(inSpan)
		out.Strong += strong
		out.Assessor += assessor
		out.Parked += parked
		out.Rested += rested
		if replay && slices.ContainsFunc(k.rounds, func(r BabysitRound) bool { return r.Class == "" && !r.At.Before(since) }) {
			out.Replayed += s.replayPR(ctx, k.pr, k.rounds)
		}
		for _, r := range k.rounds {
			if r.At.Before(since) {
				continue
			}
			out.Rounds++
			prs[k.pr] = true
			if r.Class == "" {
				continue
			}
			out.Classed++
			out.Class[r.Class]++
			if r.WorkClass != "" {
				out.WorkClass[r.WorkClass]++
			}
		}
	}
	out.PRs = len(prs)
	n, err := s.nitpickStats(ctx, actor, projectID, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	if len(n.Decisions) > 0 || len(n.Again) > 0 {
		out.Nitpicks = n
	}
	return out, nil
}

// replayPR classifies a pull request's unclassified rounds, reading its
// files from the forge. It returns how many it classified.
func (s *Service) replayPR(ctx context.Context, prID int64, rounds []BabysitRound) int {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return 0
	}
	comments, err := s.reviewComments(ctx, s.db.R, prID)
	if err != nil {
		return 0
	}
	return replayRounds(rounds, comments, s.forgeFileReader(ctx, pr))
}

// replayRounds classifies the rounds that have no class, in place, from the
// pull request's comments as they stand now. Their verdicts now are later
// than the rounds, so an earlier comment counts as fixed only through the
// rounds (one that pushed while given it), and as dealt with once a round
// was given it. It returns how many rounds it classified.
func replayRounds(rounds []BabysitRound, comments []*PRReviewComment, file func(path, sha string) string) int {
	byRef := map[string]*PRReviewComment{}
	var earlier []ledgerItem
	for _, c := range comments {
		byRef[c.Ref] = c
		it := commentItem(c)
		it.Fixed, it.Dealt = false, false
		earlier = append(earlier, it)
	}
	given := map[string]bool{}
	n := 0
	for i := range rounds {
		r := &rounds[i]
		if r.Class == "" {
			in := ledgerInput{At: r.At, Head: r.SHA, Conflict: r.Conflict, Rounds: rounds[:i], File: file}
			for _, ref := range r.Comments {
				if c := byRef[ref]; c != nil {
					in.Items = append(in.Items, commentItem(c))
				}
			}
			for _, c := range r.Checks {
				in.Checks = append(in.Checks, ledgerCheck{Name: c})
			}
			in.Earlier = make([]ledgerItem, len(earlier))
			for k, it := range earlier {
				it.Dealt = given[it.ID]
				in.Earlier[k] = it
			}
			r.Ledger, r.Class = classifyRound(in)
			r.WorkClass = r.Class
			if r.Class != "" && r.Outcome != "" && !r.Pushed {
				r.Class = RoundNothingPushed
			}
			if r.Class != "" {
				n++
			}
		}
		for _, ref := range r.Comments {
			given[ref] = true
		}
	}
	return n
}
