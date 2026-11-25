package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// The round ladder (WP-439, WP-ADR55): rounds start on the babysit's model; a round
// that didn't settle its head, the first repeat or caused finding, or a
// pushed fix that didn't clear its check goes to the stronger model with the
// history; when that doesn't settle it either, or rounds stop making
// progress, the assessor (the stronger model at high effort, fresh, with the
// task's intent and every round) takes it; and when the assessor didn't
// settle it, the pull request leaves the merge line.
//
// Who hears about that is the assessor's judgment, not a rule's (WP-930,
// WP-ADR107). Its owner is asked only when the assessor ends with a question
// and its reason no agent can settle it. Anything else rests: nobody is
// asked, and it is tried again when what it rests over changes, or after a
// time that grows only while nothing does. The round after a rest is the
// assessor's again, told what happened since, with the same ways to end.
// Nothing gives up: the only resting states are merged, stopped by someone,
// and waiting on a person's question.
const (
	// babysitStallBudget is how many rounds that made no progress (a
	// repeat, a finding on the last round's change, nothing pushed) run
	// before the assessor; rounds with new work don't count.
	babysitStallBudget = 3
	// babysitMaxAssessors is how many assessor rounds run between two of a
	// person's answers.
	babysitMaxAssessors = 3
	assessorEffort      = "high"
	assessorJobTimeout  = 90 * 60
	// parkWait starts the wait of a babysit parked for a person's answer.
	parkWait = "waiting for a person's answer: "
	// parkRequestKey keys the question a parked babysit asks:
	// auto:babysit-park:<babysit>:<round>.
	parkRequestKey = "auto:babysit-park:"
	// restWait starts the wait of a babysit resting out of the merge line
	// with nobody asked (WP-930).
	restWait = "resting out of the merge line: "
	// restFirst is a rest over something new; one over what the rest before
	// it was over is twice as long as that one, up to restMax.
	restFirst = time.Hour
	restMax   = 24 * time.Hour
)

type rung int

const (
	rungBase rung = iota
	rungStrong
	rungAssessor
	rungPark
)

// roundPlan is who takes a round, and why the ones before didn't settle it.
type roundPlan struct {
	rung rung
	prev *BabysitRound
	why  string
}

// sinceAnswer are the rounds after the last one a person answered: an
// answer starts the assessor's budget again.
func sinceAnswer(b *Babysit) []BabysitRound {
	for i := len(b.Rounds) - 1; i >= 0; i-- {
		if b.Rounds[i].Reply != "" {
			return b.Rounds[i+1:]
		}
	}
	return b.Rounds
}

// lastRound is the babysit's latest round (nil before the first).
func lastRound(b *Babysit) *BabysitRound {
	if n := len(b.Rounds); n > 0 {
		return &b.Rounds[n-1]
	}
	return nil
}

// stalled counts the rounds since the last assessor round or answer that
// made no progress: the same point again, a point on the last round's own
// change, or nothing pushed.
func stalled(b *Babysit) int {
	n := 0
	for i := len(b.Rounds) - 1; i >= 0; i-- {
		r := b.Rounds[i]
		if r.Assessor || r.Reply != "" {
			break
		}
		if r.Class == RoundRepeat || r.Class == RoundCaused || r.Class == RoundNothingPushed {
			n++
		}
	}
	return n
}

// planRound picks a round's rung before its work packet is read. stuck: the
// head had its round and what that left still needs work.
func planRound(b *Babysit, head string, stuck bool, worst string, worstN int, what string) roundPlan {
	if r := lastRound(b); r != nil && r.Reply != "" {
		if !r.RestUntil.IsZero() {
			return roundPlan{rung: rungAssessor, why: "it rested out of the merge line and its rest is over"}
		}
		return roundPlan{rung: rungAssessor, why: "a person answered the question it was waiting on"}
	}
	since := sinceAnswer(b)
	onHead := func(f func(BabysitRound) bool) bool {
		return slices.ContainsFunc(since, func(r BabysitRound) bool { return sameSHA(r.SHA, head) && f(r) })
	}
	var plan roundPlan
	switch {
	case stuck:
		last := b.Rounds[len(b.Rounds)-1]
		why := fmt.Sprintf("round %d (%s) left %s on %s", last.N, cmp(last.Outcome, "no outcome"), what, short(head))
		if b.Model == escalateModel || onHead(func(r BabysitRound) bool { return r.Model == escalateModel || r.Assessor }) {
			plan = roundPlan{rung: rungAssessor, why: why}
		} else {
			plan = roundPlan{rung: rungStrong, prev: &last}
		}
	case worstN >= 2 && b.Model != escalateModel && !strongerTried(b, worst):
		if prev := pushedFixFor(b, worst); prev != nil {
			plan = roundPlan{rung: rungStrong, prev: prev}
		}
	}
	if plan.rung == rungAssessor {
		return plan
	}
	switch {
	case b.Round >= b.MaxRounds:
		return roundPlan{rung: rungAssessor, why: fmt.Sprintf("%d round%s, the most a babysit runs, and still %s", b.Round, plural(b.Round), what)}
	case worstN >= b.MaxRepeats && plan.rung != rungStrong:
		return roundPlan{rung: rungAssessor, why: fmt.Sprintf("%s failed %d rounds in a row", worst, worstN)}
	case stalled(b) >= babysitStallBudget:
		return roundPlan{rung: rungAssessor, why: fmt.Sprintf("%d rounds made no progress (the same point again, a point on the last round's own change, or nothing pushed)", stalled(b))}
	}
	return plan
}

// raiseForLedger moves a round up the ladder once its ledger is known: the
// first repeat or caused finding goes to the stronger model; a repeat of
// what the stronger model fixed goes to the assessor; a repeat of what the
// assessor fixed parks.
func raiseForLedger(b *Babysit, plan roundPlan, round *BabysitRound) roundPlan {
	if plan.rung == rungPark {
		return plan
	}
	for _, e := range round.Ledger {
		if e.Class != RoundRepeat && e.Class != RoundCaused {
			continue
		}
		var fixer *BabysitRound
		if e.Class == RoundRepeat {
			for i := range b.Rounds {
				if b.Rounds[i].N == e.Round {
					fixer = &b.Rounds[i]
				}
			}
		}
		switch {
		case fixer != nil && fixer.Assessor && !answeredSince(b, fixer.N):
			return roundPlan{rung: rungPark, why: fmt.Sprintf("%s came back after the assessor's fix in round %d", ledgerWhat(e), fixer.N)}
		case fixer != nil && fixer.Model == escalateModel && plan.rung < rungAssessor:
			plan = roundPlan{rung: rungAssessor, why: fmt.Sprintf("%s came back after the stronger model's fix in round %d", ledgerWhat(e), fixer.N)}
		case plan.rung < rungStrong:
			plan.rung = rungStrong
		}
	}
	return plan
}

// answeredSince: a person answered on or after round n.
func answeredSince(b *Babysit, n int) bool {
	return slices.ContainsFunc(b.Rounds, func(r BabysitRound) bool { return r.N >= n && r.Reply != "" })
}

// assessorBlocked says why the assessor may not take a round, so the pull
// request parks instead ("" when it may): one assessor round per head and
// per unresolved item, at most babysitMaxAssessors between a person's
// answers, and past the cap one per answer.
func assessorBlocked(b *Babysit, head string, round *BabysitRound) string {
	since := sinceAnswer(b)
	var assessors []BabysitRound
	for _, r := range since {
		if r.Assessor {
			assessors = append(assessors, r)
		}
	}
	if len(assessors) == 0 {
		return ""
	}
	last := assessors[len(assessors)-1]
	for _, a := range assessors {
		if sameSHA(a.SHA, head) {
			return fmt.Sprintf("the assessor's round %d on this head (%s) didn't settle it", a.N, cmp(a.Outcome, "no outcome"))
		}
		var again []string
		for _, c := range round.Checks {
			if slices.Contains(a.Checks, c) {
				again = append(again, c)
			}
		}
		for _, c := range round.Comments {
			if slices.Contains(a.Comments, c) {
				again = append(again, c)
			}
		}
		for _, f := range round.Findings {
			if slices.Contains(a.Findings, f) {
				again = append(again, f)
			}
		}
		if len(again) > 0 {
			return fmt.Sprintf("%s still open after the assessor's round %d", joinAnd(again), a.N)
		}
	}
	switch {
	case len(assessors) >= babysitMaxAssessors:
		return fmt.Sprintf("%d assessor rounds and still more to fix", len(assessors))
	case b.Round >= b.MaxRounds && last.N > b.MaxRounds:
		return fmt.Sprintf("%d round%s, the most a babysit runs", b.Round, plural(b.Round))
	}
	return ""
}

// parkedOn: the babysit waits for a person's answer about this head.
func parkedOn(b *Babysit, head string) bool {
	r := lastRound(b)
	return r != nil && r.Parked != "" && r.Reply == "" && (sameSHA(r.ParkSHA, head) || sameSHA(r.SHA, head))
}

// closingRE is a line that starts one of an assessor's closing lines:
// "Question: …", "Because: …" or "Wait: …", in any case and whatever markdown
// emphasis, heading or list mark is around the word. The word alone on its
// line starts one too, its text on the lines after.
var closingRE = lazyre.New(`(?i)^[\s*_#-]*(question|because|wait)[*_]*\s*(?::[*_\s]*(.*)|[*_\s]*)$`)

// closingLines reads how an assessor's answer ended: its question for a
// person, its reason no agent can settle that, and what it is waiting for
// outside the pull request. Each is the last one written, "" when there is
// none, and runs on over the lines after it to the next blank line or
// closing line. Code blocks and quoted lines aren't read: they are logs and
// other people's words, not how the assessor ended.
func closingLines(answer string) (question, because, waiting string) {
	var into *string
	var text string
	end := func() {
		if into != nil && text != "" {
			*into = excerpt(text, 500)
		}
		into, text = nil, ""
	}
	fenced := false
	for _, l := range strings.Split(answer, "\n") {
		l = strings.TrimSpace(l)
		fence := strings.HasPrefix(l, "```") || strings.HasPrefix(l, "~~~")
		if fence {
			fenced = !fenced
		}
		if fence || fenced || l == "" || strings.HasPrefix(l, ">") {
			end()
			continue
		}
		if m := closingRE.FindStringSubmatch(l); m != nil {
			end()
			switch strings.ToLower(m[1]) {
			case "question":
				into = &question
			case "because":
				into = &because
			default:
				into = &waiting
			}
			text = strings.TrimRight(m[2], "*_ ")
			continue
		}
		if into != nil {
			text = strings.TrimSpace(text + " " + strings.TrimLeft(l, "-*• "))
		}
	}
	end()
	return question, because, waiting
}

// asksPerson: the round ended with a question and its reason no agent can
// settle it. Only that reaches a person.
func asksPerson(r *BabysitRound) bool { return r.Question != "" && r.Because != "" }

// closingWhy says why a round that ended with closing lines left the merge
// line.
func closingWhy(r *BabysitRound) string {
	switch {
	case asksPerson(r):
		return "the assessor says a person has to settle it: " + excerpt(r.Because, 200)
	case r.Waiting != "":
		return "the assessor says it waits on something outside the pull request: " + excerpt(r.Waiting, 200)
	}
	return "the assessor asked a question without saying why no agent can settle it"
}

// takeUpHeldComments sends the review comments an agent left for a person to
// the assessor (WP-759, WP-ADR65): a cheap round's "needs a person" is a
// hand-off, not an end. The assessor settles them, or ends with the question
// that really is a person's, which parks the babysit and comes back to it
// answered. It reports whether that took the step; false when only a person's
// own verdicts are left, which no agent can change.
func (s *Service) takeUpHeldComments(ctx context.Context, b *Babysit, pr *PullRequest, stale bool) bool {
	held := s.heldByAgent(ctx, b, pr)
	if len(held) == 0 {
		return false
	}
	if r := lastRound(b); r != nil && sameSHA(b.RoundSHA, pr.HeadSHA) {
		switch {
		case r.To == "session" && !stale:
			return true
		case r.To == "session":
		case r.Pushed:
			s.setState(ctx, b, pr, BabysitWatching, fmt.Sprintf("waiting for round %d's push to arrive", b.Round))
			return true
		}
	}
	why := "an earlier round left " + joinAnd(held) + " for a person"
	s.babysitRoundBy(ctx, b, pr, nil, false, held, nil, &roundPlan{rung: rungAssessor, why: why})
	return true
}

// heldByAgent are the open review comments, by ref, an agent left for a person.
func (s *Service) heldByAgent(ctx context.Context, b *Babysit, pr *PullRequest) []string {
	var refs []string
	for _, rc := range s.heldComments(ctx, b, pr) {
		if byPerson, err := s.verdictByPerson(ctx, s.db.R, rc.id); err == nil && !byPerson {
			refs = append(refs, rc.Ref)
		}
	}
	return refs
}

// heldComments are the open review comments waiting for a person.
func (s *Service) heldComments(ctx context.Context, b *Babysit, pr *PullRequest) []*PRReviewComment {
	comments, err := s.reviewComments(ctx, s.db.R, pr.ID)
	if err != nil {
		return nil
	}
	_, _, person := s.commentWork(ctx, pr, b, comments)
	return slices.DeleteFunc(comments, func(rc *PRReviewComment) bool { return !slices.Contains(person, rc.Ref) })
}

// parkBabysit takes a pull request out of its merge line: Waypoint's rounds
// didn't settle it. When the assessor asked a question and said why no agent
// can settle it (asksPerson), its owner is asked it plainly, and it goes on
// when they answer (one more assessor round, told the answer) or a push
// arrives; a dismissed question leaves it out of the line. Otherwise nobody
// is asked (WP-930): it rests, and restOver tries again.
//
// failed, conflict, comments and findings are what the round was over.
// Every call site passes them all and parkBabysit alone decides what ends
// the park, so none of them can lift a park over more than its checks:
// the failing checks passing on the head ends it only when they were its
// whole subject (parkLifts), and settling the review comments answers it
// only when they were (parkSubject).
//
// A person isn't asked what they already were (WP-914): when the round named
// an open request, or another pull request parked over the same check waits
// on one, it waits on that instead, and its closing is the answer.
func (s *Service) parkBabysit(ctx context.Context, b *Babysit, pr *PullRequest, why, question string, failed []string, conflict bool, comments, findings []string) {
	about := parkSubject(failed, conflict, comments, findings)
	failed = parkLifts(failed, conflict, comments, findings)
	s.leaveQueue(ctx, pr)
	s.nudgeLine(ctx, pr)
	r := lastRound(b)
	if r == nil {
		s.setState(ctx, b, pr, BabysitNeedsPerson, parkWait+why)
		return
	}
	r.Parked, r.ParkSHA = why, pr.HeadSHA
	r.ParkedOn = s.stillOpen(ctx, pr, about)
	r.ParkedChecks = slices.Clone(failed)
	r.WaitsOn = s.namedRequests(ctx, pr, question+"\n"+r.Answer)
	if len(r.WaitsOn) == 0 {
		r.WaitsOn = s.sharedWaits(ctx, pr, failed)
	}
	if r.Pushed && r.PushSHA != "" && sameSHA(r.SHA, pr.HeadSHA) {
		r.ParkSHA = r.PushSHA
	}
	rest := len(r.WaitsOn) == 0 && !asksPerson(r)
	if rest {
		r.RestedAt = s.now()
		r.RestUntil = r.RestedAt.Add(restLength(b))
	}
	raw, _ := json.Marshal(b.Rounds)
	rounds := string(raw)
	if err := s.setBabysit(ctx, b, map[string]any{"rounds": rounds}); err != nil {
		return
	}
	if len(r.WaitsOn) > 0 {
		waits := parkWaitsText(r.WaitsOn)
		s.setState(ctx, b, pr, BabysitNeedsPerson, parkWait+excerpt(waits+", asked already", 200))
		s.babysitTell(ctx, b, pr, fmt.Sprintf("parked@%d", r.N), fmt.Sprintf(
			"%s#%d is out of the merge line: Waypoint's rounds didn't settle it (%s). It waits on %s, which asks a person already, instead of asking again; it goes on when that is closed, its failing checks pass, or a push arrives (%s).",
			pr.Repo, pr.Number, why, waits, pr.URL))
		return
	}
	if rest {
		s.setState(ctx, b, pr, BabysitResting, restWaitText(r))
		s.log.Info("babysit rests out of the merge line", "repo", pr.Repo, "number", pr.Number, "why", why, "until", r.RestUntil)
		return
	}
	s.setState(ctx, b, pr, BabysitNeedsPerson, parkWait+excerpt(question, 200))
	s.askAboutPark(ctx, b, pr, r, question)
	s.babysitTell(ctx, b, pr, fmt.Sprintf("parked@%d", r.N), fmt.Sprintf(
		"%s#%d is out of the merge line until its owner answers: %s (%s).", pr.Repo, pr.Number, question, pr.URL))
}

// blocker is what a round was over, as one value: two rounds with the same
// blocker left the same things standing.
func blocker(r *BabysitRound) string {
	sorted := func(x []string) string {
		x = slices.Clone(x)
		slices.Sort(x)
		return strings.Join(x, ",")
	}
	return fmt.Sprintf("%s|%t|%s|%s", sorted(r.Checks), r.Conflict, sorted(r.Comments), sorted(r.Findings))
}

// sameRests counts the rests before the latest round's that were over the
// same things as it, and says when the first of them began: how long nothing
// has changed. A rest over anything else, or a park for a person, ends the
// count; rounds that didn't park are passed over.
func sameRests(b *Babysit) (n int, since time.Time) {
	last := lastRound(b)
	if last == nil {
		return 0, time.Time{}
	}
	for i := len(b.Rounds) - 2; i >= 0; i-- {
		r := &b.Rounds[i]
		if r.Parked == "" {
			continue
		}
		if r.RestUntil.IsZero() || blocker(r) != blocker(last) {
			break
		}
		n, since = n+1, r.RestedAt
	}
	return n, since
}

// restLength is how long the latest round's rest lasts unless what it is
// over changes first. It measures progress, not rests: restFirst when the
// rest is over something new, doubling up to restMax for each rest before it
// over the same things.
func restLength(b *Babysit) time.Duration {
	n, _ := sameRests(b)
	d := restFirst
	for range n {
		if d *= 2; d >= restMax {
			return restMax
		}
	}
	return d
}

// resting: the round's park asked nobody and waits to be tried again.
func resting(r *BabysitRound) bool {
	return r != nil && r.Parked != "" && r.Reply == "" && !r.RestUntil.IsZero()
}

// restNext says what a resting round waits for next.
func restNext(r *BabysitRound) string {
	if !r.RerunAt.IsZero() {
		return "its failing checks are running again"
	}
	return "it tries again at " + r.RestUntil.UTC().Format("2006-01-02 15:04 UTC")
}

// restWaitText is a resting babysit's wait.
func restWaitText(r *BabysitRound) string {
	return restWait + excerpt(r.Parked, 160) + "; " + restNext(r)
}

// restOver moves a park nobody was asked about (WP-930, WP-ADR107), and
// reports whether it ended: the next round is the assessor's, told what
// happened while it rested.
//
// A rest ends when its time is up, or sooner when what it is over may have
// changed (restChange). A rest over failing checks alone re-runs them first:
// passing ends the park (settledWhileParked), and failing again ends the rest
// as soon as that result is in.
//
// A park from before WP-930 whose question had no reason with it takes its
// request back and rests, due at once.
func (s *Service) restOver(ctx context.Context, b *Babysit, pr *PullRequest) bool {
	r := lastRound(b)
	if r == nil || r.Parked == "" || r.Reply != "" {
		return false
	}
	save := func() bool {
		raw, _ := json.Marshal(b.Rounds)
		return s.setBabysit(ctx, b, map[string]any{"rounds": string(raw)}) == nil
	}
	now := s.now()
	if r.RestUntil.IsZero() {
		if len(r.WaitsOn) > 0 || asksPerson(r) {
			return false
		}
		if answer := s.parkAnswer(ctx, b, r); answer != "" {
			r.Reply = capText(answer, 4000)
			if !save() {
				r.Reply = ""
				return false
			}
			return true
		}
		r.RestedAt, r.RestUntil = now, now
		if !save() {
			r.RestedAt, r.RestUntil = time.Time{}, time.Time{}
			return false
		}
		s.withdrawParkRequest(ctx, b, r)
	}
	failedAgain := false
	if !r.RerunAt.IsZero() {
		if now.Sub(r.RerunAt) < faultRerunSettle {
			return false
		}
		s.rereadHead(ctx, pr, faultRerunSettle)
		fresh, err := s.prByID(ctx, s.db.R, pr.ID)
		if err != nil || !sameSHA(fresh.HeadSHA, pr.HeadSHA) {
			return false
		}
		*pr = *fresh
		failedAgain = sameSHA(pr.ciSHA, pr.HeadSHA) && pr.CIState == CIFailed
	} else {
		if now.Before(r.RestUntil) {
			if r.RestChange = s.restChange(ctx, b, pr, r); r.RestChange == "" {
				return false
			}
		}
		if len(r.ParkedChecks) > 0 {
			r.RerunAt = now
			if !save() {
				r.RerunAt = time.Time{}
				return false
			}
			if s.rerunParkedChecks(ctx, pr) {
				return false
			}
			r.RerunAt = time.Time{}
		}
	}
	r.Reply = restReply(b, r, failedAgain)
	if !save() {
		r.Reply = ""
		return false
	}
	return true
}

// parkAnswer is a person's answer to the park's request ("" when it has
// none).
func (s *Service) parkAnswer(ctx context.Context, b *Babysit, r *BabysitRound) string {
	var answer, note string
	if s.db.R.QueryRowContext(ctx, `SELECT answer, note FROM requests WHERE tool_use_id = ? AND state = ?`,
		parkRequestKey+strconv.FormatInt(b.ID, 10)+":"+strconv.Itoa(r.N), RequestAnswered).Scan(&answer, &note) != nil {
		return ""
	}
	return strings.TrimSpace(strings.Join(slices.DeleteFunc([]string{answer, note}, func(x string) bool { return strings.TrimSpace(x) == "" }), "\n\n"))
}

// restChange says what changed outside a resting pull request that makes
// trying again worth it before its time ("" when nothing has): a check the
// assessor's round left failing, waiting for something outside the pull
// request, has since passed on another head of the repository. It is
// evidence only for the first rest over those checks: when the same rest
// follows, it was wrong once already.
func (s *Service) restChange(ctx context.Context, b *Babysit, pr *PullRequest, r *BabysitRound) string {
	if n, _ := sameRests(b); r.Waiting == "" || n > 0 {
		return ""
	}
	for _, name := range r.ParkedChecks {
		var one int
		if s.db.R.QueryRowContext(ctx, `SELECT 1 FROM ci_checks c JOIN ci_heads h ON h.id = c.head_id
			WHERE h.forge_id = ? AND h.repo = ? COLLATE NOCASE AND h.sha != ? AND c.name = ? AND c.state = ? AND c.updated_at > ? LIMIT 1`,
			pr.ForgeID, pr.Repo, pr.HeadSHA, name, CheckSuccess, r.RestedAt.UnixMilli()).Scan(&one) == nil {
			return name + " has passed on another head of the repository since the rest began"
		}
	}
	return ""
}

// rerunParkedChecks re-runs what failed on a resting head; false when the
// forge can't, or re-ran nothing.
func (s *Service) rerunParkedChecks(ctx context.Context, pr *PullRequest) bool {
	if pr.Outside {
		return false
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	cl, ok := s.Forges.(ForgeCIRetry)
	if err != nil || !ok || f.token == "" {
		return false
	}
	if can, err := cl.CanRetryChecks(ctx, f); err != nil || !can {
		return false
	}
	s.keepFailedEvidence(ctx, f, pr.Repo, pr.HeadSHA)
	n, err := cl.RetryChecks(ctx, f, pr.Repo, pr.HeadSHA)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "re-run a resting pull request's checks", "repo", pr.Repo, "number", pr.Number, "pr", pr.ID, "err", err)
		}
		return false
	}
	return n > 0
}

// restReply is what Waypoint tells the assessor's round after a rest: what
// happened while it rested, and for how long nothing has changed. Facts
// only: what to do about them is the assessor's judgment, and its prompt
// gives it every way to end, a question for a person among them. The earlier
// round's own words are with the rounds (writeAllRounds).
func restReply(b *Babysit, r *BabysitRound, failedAgain bool) string {
	var p strings.Builder
	p.WriteString("Nobody was asked about it.")
	if r.Question != "" && r.Because == "" {
		fmt.Fprintf(&p, " Round %d's question reached nobody: Waypoint read no `Because:` line with it (it reads them outside code blocks and quotes).", r.N)
	}
	checks := joinAnd(r.ParkedChecks)
	again := map[bool]string{true: " and failed again", false: ""}[failedAgain]
	switch {
	case !r.RerunAt.IsZero() && r.RestChange != "":
		fmt.Fprintf(&p, " %s, so %s was re-run here%s.", upperFirst(r.RestChange), checks, again)
	case !r.RerunAt.IsZero():
		fmt.Fprintf(&p, " When the rest was over %s was re-run%s.", checks, again)
	case r.RestChange != "":
		p.WriteString(" " + upperFirst(r.RestChange) + ".")
	default:
		p.WriteString(" Nothing about it changed while it rested.")
	}
	if n, since := sameRests(b); n > 0 {
		fmt.Fprintf(&p, " It has rested %d times in a row over the same things, out of the merge line since %s.", n+1, since.UTC().Format("2006-01-02 15:04 UTC"))
	}
	return p.String()
}

// askAboutPark files the parked babysit's question for its owner, once per
// park (a dismissed one isn't asked again).
func (s *Service) askAboutPark(ctx context.Context, b *Babysit, pr *PullRequest, r *BabysitRound, question string) {
	key := parkRequestKey + strconv.FormatInt(b.ID, 10) + ":" + strconv.Itoa(r.N)
	var id int64
	if s.db.R.QueryRowContext(ctx, `SELECT id FROM requests WHERE tool_use_id = ? LIMIT 1`, key).Scan(&id) == nil {
		return
	}
	u, err := s.userByID(ctx, s.db.R, b.ownerID)
	if err != nil || u.Disabled {
		return
	}
	var body strings.Builder
	fmt.Fprintf(&body, "%s#%d (%q) can't merge until you answer this.\n\nWhy no agent can settle it, as the assessor put it:\n\n> %s\n\n", pr.Repo, pr.Number, pr.Title, strings.ReplaceAll(strings.TrimSpace(r.Because), "\n", "\n> "))
	if comments := s.commentsAskedAbout(ctx, pr, r); len(comments) > 0 {
		body.WriteString(quoteComments(comments) + "\n")
	}
	body.WriteString("If nobody answers, it stays out of the merge line and the rest merge without it. " +
		"Answer in plain words and Waypoint carries it out, or close the pull request to drop it.\n\n" + pr.URL)
	in := RequestInput{Kind: RequestQuestion, Title: fmt.Sprintf("%s#%d: %s", pr.Repo, pr.Number, excerpt(question, 120)), Body: body.String(),
		Questions: []Question{{Question: question}}, ToolUseID: key, Followup: "never"}
	projectKey, err := s.attachedProjectKey(ctx, pr.ForgeID, pr.Repo)
	if readFault(err) {
		s.log.WarnContext(ctx, "read the project to ask about a parked pull request", "repo", pr.Repo, "number", pr.Number, "err", err)
	}
	in.Project = projectKey
	if _, _, err := s.CreateRequest(ctx, &Actor{User: u, Agent: "waypoint/babysit"}, in); err != nil {
		s.log.WarnContext(ctx, "ask about a parked pull request", "repo", pr.Repo, "number", pr.Number, "err", err)
	}
}

// parkSubject are the review comments a park is about: the round's comments,
// when it had nothing else to do. A park over a failing check is not answered
// by settling a comment that happens to be open.
func parkSubject(failed []string, conflict bool, comments, findings []string) []string {
	if len(failed) > 0 || conflict || len(findings) > 0 {
		return nil
	}
	return comments
}

// parkLifts are the failing checks whose passing on the parked head ends a
// park: the round's, when it had nothing else to do. A park over a conflict,
// review comments or audit findings as well is not answered by its checks
// going green, which leave those standing.
func parkLifts(failed []string, conflict bool, comments, findings []string) []string {
	if conflict || len(comments) > 0 || len(findings) > 0 {
		return nil
	}
	return failed
}

// stillOpen are those of refs not dealt with. If they can't be read it is
// none, so nothing later takes the park for answered.
func (s *Service) stillOpen(ctx context.Context, pr *PullRequest, refs []string) []string {
	comments, err := s.reviewComments(ctx, s.db.R, pr.ID)
	if err != nil {
		return nil
	}
	var open []string
	for _, rc := range comments {
		if !rc.Dealt && slices.Contains(refs, rc.Ref) {
			open = append(open, rc.Ref)
		}
	}
	return open
}

// commentRefRE is a review comment's ref in text: c412.
var commentRefRE = lazyre.New(`\bc\d+\b`)

// commentsAskedAbout are the review comments a person needs to see to answer
// a parked round's question: the ones the assessor named in it. When it
// named none, they are the ones the park is about that an agent set aside
// for a person. The rest stay on the pull request.
func (s *Service) commentsAskedAbout(ctx context.Context, pr *PullRequest, r *BabysitRound) []*PRReviewComment {
	comments, err := s.reviewComments(ctx, s.db.R, pr.ID)
	if err != nil {
		return nil
	}
	named := commentRefRE.FindAllString(r.Question+"\n"+r.Because, -1)
	named = slices.DeleteFunc(named, func(ref string) bool {
		return !slices.ContainsFunc(comments, func(rc *PRReviewComment) bool { return rc.Ref == ref && !rc.Gone })
	})
	return slices.DeleteFunc(comments, func(rc *PRReviewComment) bool {
		if len(named) > 0 {
			return rc.Dealt || rc.Gone || !slices.Contains(named, rc.Ref)
		}
		return rc.Dealt || rc.Gone || rc.Verdict != VerdictNeedsPerson || !slices.Contains(r.ParkedOn, rc.Ref)
	})
}

// quoteComments shows review comments to a person: a link to each, where it
// is, what it says, and why it was set aside.
func quoteComments(comments []*PRReviewComment) string {
	var b strings.Builder
	for _, rc := range comments {
		label := mdLabel(rc.Author) + "'s comment"
		if rc.Path != "" {
			label += " on " + mdLabel(rc.Path)
			if rc.Line > 0 {
				label += ":" + strconv.Itoa(rc.Line)
			}
		}
		if u := mdURL(rc.URL); u != "" {
			label = "[" + label + "](" + u + ")"
		}
		fmt.Fprintf(&b, "%s:\n\n> %s\n\n", label, excerpt(cmp(headline(rc.Body), packetBody(rc.Body)), 300))
		if rc.Verdict == VerdictNeedsPerson && rc.Note != "" {
			fmt.Fprintf(&b, "Set aside because: %s\n\n", excerpt(rc.Note, 400))
		}
	}
	return b.String()
}

// onParkAnswered hands a person's answer to a parked babysit: its next round
// is the assessor's, told the answer.
func (s *Service) onParkAnswered(ctx context.Context, e *Event) {
	if e.Kind != EvRequestUpdated || e.Data["state"] != RequestAnswered {
		return
	}
	id, _ := e.Data["request"].(int64)
	var key, answer, note string
	if id == 0 || s.db.R.QueryRowContext(ctx, `SELECT tool_use_id, answer, note FROM requests WHERE id = ?`, id).Scan(&key, &answer, &note) != nil {
		return
	}
	rest, ok := strings.CutPrefix(key, parkRequestKey)
	if !ok {
		return
	}
	bs, ns, _ := strings.Cut(rest, ":")
	bid, err1 := strconv.ParseInt(bs, 10, 64)
	n, err2 := strconv.Atoi(ns)
	var prID int64
	if err1 != nil || err2 != nil || s.db.R.QueryRowContext(ctx, `SELECT pr_id FROM babysits WHERE id = ? AND ended_at IS NULL`, bid).Scan(&prID) != nil {
		return
	}
	reply := strings.TrimSpace(strings.Join(slices.DeleteFunc([]string{answer, note}, func(x string) bool { return strings.TrimSpace(x) == "" }), "\n\n"))
	if reply == "" {
		return
	}
	s.goBG(func() {
		bg := withoutLineLocks(context.WithoutCancel(ctx))
		unlock := s.babysitLocks.lock(prID)
		b, err := s.activeBabysit(bg, s.db.R, prID)
		ok := err == nil && b != nil && b.ID == bid
		if ok {
			r := lastRound(b)
			ok = r != nil && r.N == n && r.Parked != "" && r.Reply == ""
			if ok {
				r.Reply = capText(reply, 4000)
				raw, _ := json.Marshal(b.Rounds)
				rounds := string(raw)
				ok = s.setBabysit(bg, b, map[string]any{"rounds": rounds}) == nil
			}
		}
		unlock()
		if ok {
			s.babysitStep(bg, prID, true)
		}
	})
}

// settledWhileParked carries on a parked babysit once what it was parked
// over is settled without its question: the review comments it asked about
// were dealt with on the pull request, the failing checks it was parked over
// passed on the same head (what failed them was outside the code, WP-914), or
// the requests it waited on were closed. The question is taken back, and
// anything else still wrong gets the round an answer would have started,
// told what settled it.
func (s *Service) settledWhileParked(ctx context.Context, b *Babysit, pr *PullRequest) bool {
	r := lastRound(b)
	reply := s.parkCommentsSettled(ctx, pr, r)
	if reply == "" {
		reply = s.parkChecksPassed(ctx, pr, r)
	}
	if reply == "" {
		reply = s.parkWaitsClosed(ctx, r)
	}
	if reply == "" {
		return false
	}
	r.Reply = capText(reply, 4000)
	raw, _ := json.Marshal(b.Rounds)
	if err := s.setBabysit(ctx, b, map[string]any{"rounds": string(raw)}); err != nil {
		r.Reply = ""
		return false
	}
	s.withdrawParkRequest(ctx, b, r)
	return true
}

// parkCommentsSettled is the reply when every review comment the park asked
// about has been dealt with on the pull request, else "".
func (s *Service) parkCommentsSettled(ctx context.Context, pr *PullRequest, r *BabysitRound) string {
	if len(r.ParkedOn) == 0 {
		return ""
	}
	comments, err := s.reviewComments(ctx, s.db.R, pr.ID)
	if err != nil || slices.ContainsFunc(comments, func(rc *PRReviewComment) bool { return !rc.Dealt && slices.Contains(r.ParkedOn, rc.Ref) }) {
		return ""
	}
	return "The review comments it asked about (" + joinAnd(r.ParkedOn) + ") were settled on the pull request."
}

// parkReread is how often a head parked over failing checks is read from the
// forge again: PollCI follows only pending heads, and a re-run's status can
// arrive while its webhook is missed.
const parkReread = 15 * time.Minute

// parkChecksPassed is the reply when the failing checks a park was over
// passed on its head with no push, else "". Nothing in the code changed, so
// what failed them was outside it: #368, #369 and #372 waited for a person's
// answer after their CodeQL check passed once its image was rebuilt (WP-914).
func (s *Service) parkChecksPassed(ctx context.Context, pr *PullRequest, r *BabysitRound) string {
	if len(r.ParkedChecks) == 0 {
		return ""
	}
	if !s.checksPassed(ctx, pr, r.ParkedChecks) {
		fresh := s.rereadHead(ctx, pr, parkReread)
		if fresh == nil || !sameSHA(fresh.HeadSHA, pr.HeadSHA) || !s.checksPassed(ctx, fresh, r.ParkedChecks) {
			return ""
		}
	}
	return fmt.Sprintf("The failing %s it was parked over (%s) passed on the same head (%s) with no push: what failed %s was outside the code, and it has been put right. Nobody needs to answer the question.",
		map[bool]string{true: "check", false: "checks"}[len(r.ParkedChecks) == 1], joinAnd(r.ParkedChecks), shortSHA(pr.HeadSHA),
		map[bool]string{true: "it", false: "them"}[len(r.ParkedChecks) == 1])
}

// checksPassed: every one of names passed on the pull request's head ("checks"
// stands for its CI as a whole, when no check was named).
func (s *Service) checksPassed(ctx context.Context, pr *PullRequest, names []string) bool {
	checks, err := s.prChecks(ctx, pr)
	if err != nil {
		return false
	}
	for _, n := range names {
		if n == "checks" && !slices.ContainsFunc(checks, func(c PRCheck) bool { return c.Name == n }) {
			if !sameSHA(pr.ciSHA, pr.HeadSHA) || pr.CIState != CIPassed {
				return false
			}
			continue
		}
		if !slices.ContainsFunc(checks, func(c PRCheck) bool { return c.Name == n && c.State == CheckSuccess }) {
			return false
		}
	}
	return true
}

// ParkRequest is an open request a parked babysit waits on (WP-914).
type ParkRequest struct {
	ID    int64  `json:"id"`
	Ref   string `json:"ref"` // WP:R-277
	Title string `json:"title,omitempty"`
}

// requestMention is a request named in text: R-277 or WP:R-277.
var requestMention = lazyre.New(`(?i)\b(?:([a-z][a-z0-9_.-]*):)?R-(\d+)\b`)

// maxParkWaits is how many requests one park waits on.
const maxParkWaits = 3

// namedRequests are the open requests text names: a bare R-277 is the pull
// request's project's. A round that says what it waits on ("rebuild the
// image, request R-277") waits on that, instead of a person being asked it
// again (R-279, R-283 and R-285 each asked what R-277 did).
func (s *Service) namedRequests(ctx context.Context, pr *PullRequest, text string) []ParkRequest {
	home, err := s.attachedProjectKey(ctx, pr.ForgeID, pr.Repo)
	if readFault(err) {
		s.log.WarnContext(ctx, "read the project to find the requests a round names", "repo", pr.Repo, "number", pr.Number, "err", err)
		home = ""
	}
	var out []ParkRequest
	for _, m := range requestMention.FindAllStringSubmatch(text, -1) {
		key := cmp(m[1], home)
		if key == "" || strings.EqualFold(key, "me") || len(out) == maxParkWaits {
			continue
		}
		var w ParkRequest
		var pkey string
		var n int
		if s.db.R.QueryRowContext(ctx, `SELECT r.id, r.number, p.key, r.title FROM requests r JOIN projects p ON p.id = r.project_id
			WHERE p.key = ? COLLATE NOCASE AND r.number = ? AND r.state = ?`, key, m[2], RequestOpen).Scan(&w.ID, &n, &pkey, &w.Title) != nil {
			continue
		}
		w.Ref = requestRef(pkey, n)
		if !slices.ContainsFunc(out, func(o ParkRequest) bool { return o.ID == w.ID }) {
			out = append(out, w)
		}
	}
	return out
}

// sharedWaits are the open requests another pull request of the repository,
// parked over one of the same failing checks, waits on: the same check
// failing on several pull requests at once is usually one cause outside
// them, asked about once. Only those waits are joined, never another pull
// request's own question, which is about that pull request.
func (s *Service) sharedWaits(ctx context.Context, pr *PullRequest, failed []string) []ParkRequest {
	failed = slices.DeleteFunc(slices.Clone(failed), func(n string) bool { return n == "checks" })
	if len(failed) == 0 {
		return nil
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT b.pr_id FROM babysits b JOIN pull_requests p ON p.id = b.pr_id
		WHERE b.ended_at IS NULL AND p.forge_id = ? AND p.repo = ? COLLATE NOCASE AND p.id != ? ORDER BY b.id`, pr.ForgeID, pr.Repo, pr.ID)
	if err != nil {
		return nil
	}
	var others []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			others = append(others, id)
		}
	}
	_ = rows.Close()
	var out []ParkRequest
	for _, id := range others {
		ob, err := s.activeBabysit(ctx, s.db.R, id)
		if err != nil || ob == nil {
			continue
		}
		r := lastRound(ob)
		if r == nil || r.Parked == "" || r.Reply != "" || !slices.ContainsFunc(r.ParkedChecks, func(n string) bool { return slices.Contains(failed, n) }) {
			continue
		}
		for _, w := range r.WaitsOn {
			var state string
			if s.db.R.QueryRowContext(ctx, `SELECT state FROM requests WHERE id = ?`, w.ID).Scan(&state) == nil && state == RequestOpen &&
				len(out) < maxParkWaits && !slices.ContainsFunc(out, func(o ParkRequest) bool { return o.ID == w.ID }) {
				out = append(out, w)
			}
		}
	}
	return out
}

// parkWaitsText names the requests a park waits on: WP:R-277 ("Rebuild the
// CodeQL image").
func parkWaitsText(waits []ParkRequest) string {
	parts := make([]string, 0, len(waits))
	for _, w := range waits {
		if w.Title != "" {
			parts = append(parts, fmt.Sprintf("%s (%q)", w.Ref, excerpt(w.Title, 80)))
		} else {
			parts = append(parts, w.Ref)
		}
	}
	return joinAnd(parts)
}

// parkWaitsClosed is the reply once every request the park waits on is
// closed, else "": how each ended, and what its person said.
func (s *Service) parkWaitsClosed(ctx context.Context, r *BabysitRound) string {
	if len(r.WaitsOn) == 0 {
		return ""
	}
	parts := make([]string, 0, len(r.WaitsOn))
	for _, w := range r.WaitsOn {
		var state, answer, note, by string
		err := s.db.R.QueryRowContext(ctx, `SELECT r.state, r.answer, r.note, coalesce(nullif(u.display_name, ''), u.username, '')
			FROM requests r LEFT JOIN users u ON u.id = r.decided_by WHERE r.id = ?`, w.ID).Scan(&state, &answer, &note, &by)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			parts = append(parts, parkWaitsText([]ParkRequest{w})+", which it waited on, is gone.")
			continue
		case err != nil, state == RequestOpen:
			return ""
		}
		line := parkWaitsText([]ParkRequest{w}) + ", which it waited on, was " + state
		if by != "" {
			line += " by " + by
		}
		if said := strings.TrimSpace(strings.Join(slices.DeleteFunc([]string{answer, note}, func(x string) bool { return strings.TrimSpace(x) == "" }), "\n\n")); said != "" {
			line += ": " + excerpt(said, 1500)
		}
		parts = append(parts, line+".")
	}
	return strings.Join(parts, "\n\n")
}

// withdrawParkRequest takes back a park's question nobody needs answered now.
func (s *Service) withdrawParkRequest(ctx context.Context, b *Babysit, r *BabysitRound) {
	var id int64
	if s.db.R.QueryRowContext(ctx, `SELECT id FROM requests WHERE tool_use_id = ? AND state = ?`,
		parkRequestKey+strconv.FormatInt(b.ID, 10)+":"+strconv.Itoa(r.N), RequestOpen).Scan(&id) != nil {
		return
	}
	u, err := s.userByID(ctx, s.db.R, b.ownerID)
	if err != nil {
		return
	}
	if _, err := s.WithdrawRequest(ctx, &Actor{User: u, Agent: "waypoint/babysit"}, strconv.FormatInt(id, 10), "Settled without an answer: "+capText(r.Reply, 1900)); err != nil {
		s.log.WarnContext(ctx, "withdraw parked question", "request", id, "err", err)
	}
}

// babysitParked is what a parked babysit holds the merge for ("" if it
// isn't parked): its question waits for a person. A babysit that can't be
// read is an error, not "not parked".
func (s *Service) babysitParked(ctx context.Context, pr *PullRequest) (string, error) {
	b, err := s.activeBabysit(ctx, s.db.R, pr.ID)
	if err != nil {
		return "", err
	}
	if b == nil || !parkedOn(b, pr.HeadSHA) {
		return "", nil
	}
	if r := lastRound(b); resting(r) {
		return "Waypoint's rounds didn't settle it: " + r.Parked + "; " + restNext(r), nil
	}
	return "Waypoint's rounds didn't settle it: " + lastRound(b).Parked + "; " + strings.TrimSuffix(parkWait, ": "), nil
}

const (
	assessorDiffBudget  = 100_000
	assessorIssueChars  = 4000
	assessorIssues      = 3
	assessorDecisionCap = 3000
	assessorDecisions   = 4
)

// assessorPrompt is the assessor's task: short, with what the change is
// for, every round and how it ended, and the work packet.
func (s *Service) assessorPrompt(ctx context.Context, b *Babysit, pr *PullRequest, round *BabysitRound, t *PRTodo, why string) string {
	var p strings.Builder
	fmt.Fprintf(&p, "PR %d %s, branch %s at %s. Its babysitting rounds haven't settled it: %s.\n\n", pr.Number, promptPRTitle(pr.Title), pr.HeadRef, short(pr.HeadSHA), why)
	fmt.Fprintf(&p, "Assess what is really wrong and fix it properly, with evidence: reproduce failures and run the tests that cover your change. "+
		"You may redo what earlier rounds did, or rebuild the change from %s, in new commits (the push isn't forced). "+
		"A review comment that is wrong or belongs in later work: `wp pr ack ID --reject -m WHY` (file the later work with `wp new`); an audit finding that is wrong: `wp audit mark ID rejected -m WHY`, "+
		"or real but for later work: file it with `wp new` and `wp audit mark ID confirmed --issue KEY` (not a critical or high one: fix or reject those); "+
		"one you fixed: `wp pr fixed ID`. Commit each piece of work as soon as it is done: Waypoint pushes your commits when you end, and if your time runs out first (90 minutes, waits for a test slot included) it pushes the commits you had made and anything uncommitted is lost. "+
		"Commits that change CI configuration, lockfiles or secrets are refused.\n"+
		"What the code, its tests and a reviewer can settle is yours to settle: fix it, defer a finding to a follow-up task, reject a comment with a reason, take one side of a conflict. "+
		"You don't have to settle what you can't or shouldn't, and a change you doubt is worse than none. Two other ways to end:\n"+
		"- What holds it up is outside the pull request and nothing you can change (a broken CI image, a service that is down, another change that has to land first): "+
		"end with one line `Wait: …` naming what has to change. Nobody is asked; it rests out of the merge line and is tried again, sooner when that changes.\n"+
		"- A person has to settle it (it needs access or authority you don't have, it can't be undone, it would change what the task asks for, or anything else you judge isn't an agent's to decide): "+
		"end with two lines, `Question: …` and `Because: …` (why no agent can settle it), in plain words they can answer without reading the pull request, naming any review comment it is about by its ID. "+
		"Waypoint asks its owner and it waits for their answer. A question with no `Because:` line reaches nobody.\n\n", cmp(pr.BaseRef, "its base"))
	s.writeIntent(ctx, &p, b, pr)
	// Parked carries the assessor's own words and Reply a person's (or, for
	// a rest, check names from the forge): fenced, as writeAllRounds does.
	if r := lastRound(b); r != nil && r.Reply != "" && !r.RestUntil.IsZero() {
		fmt.Fprintf(&p, "It rested out of the merge line and its rest is over:\n<untrusted>\n%s\n</untrusted>\n\n", untrusted(r.Reply))
	} else if r != nil && r.Reply != "" {
		fmt.Fprintf(&p, "A person answered the question it was waiting on:\n<untrusted>\nWhy it waited: %s\n\nTheir answer:\n%s\n</untrusted>\n\n", untrusted(r.Parked), untrusted(r.Reply))
	}
	writeAllRounds(&p, b)
	shown := map[int]bool{}
	for _, r := range b.Rounds {
		shown[r.N] = true
	}
	writeLedgerHistory(&p, b, round, shown)
	p.WriteString(t.work)
	return p.String()
}

// writeIntent is what the change is for: the tasks linked to the pull
// request, its description and the decisions those tasks asked for, as its
// owner may read them.
func (s *Service) writeIntent(ctx context.Context, p *strings.Builder, b *Babysit, pr *PullRequest) {
	p.WriteString("What the change is for:\n")
	var issueIDs []any
	if u, err := s.userByID(ctx, s.db.R, b.ownerID); err == nil {
		links, rerr := s.prLinks(ctx, &Actor{User: u}, []any{pr.ID})
		if rerr != nil {
			if readFault(rerr) {
				s.log.WarnContext(ctx, "pr links failed", "repo", pr.Repo, "number", pr.Number, "pr", pr.ID, "err", rerr)
			}
		}
		for _, l := range links {
			if len(issueIDs) == assessorIssues {
				break
			}
			is, err := s.issueByID(ctx, s.db.R, l.issue.ID, true)
			if err != nil {
				continue
			}
			issueIDs = append(issueIDs, l.issue.ID)
			if is.Outside {
				fmt.Fprintf(p, "\nTask %s: %s\n", l.issue.Key, outsideTitleInPrompt)
				continue
			}
			writeFenced(p, "Task "+l.issue.Key, is.Title, capText(is.Description, assessorIssueChars))
		}
	}
	label, body := "The pull request's description when it shipped", ""
	if fp := s.footprintOf(ctx, pr.ID); fp.Ship != nil {
		body = fp.Ship.Body
	} else {
		label, body = "The pull request's description", s.prBodyNow(ctx, pr)
	}
	if strings.TrimSpace(body) != "" {
		fmt.Fprintf(p, "\n%s:\n<untrusted>\n%s\n</untrusted>\n", label, untrusted(capText(body, assessorIssueChars)))
	}
	if len(issueIDs) > 0 {
		// #nosec G202 -- a fixed column list and placeholders; values are bound
		rows, err := s.db.R.QueryContext(ctx, designSelect(true)+` WHERE n.kind = 'decision' AND n.status = 'active' AND n.approved_at IS NOT NULL
			AND (n.requested_issue_id IN (`+idList(len(issueIDs))+`) OR n.id IN (SELECT src_id FROM knowledge_links WHERE src_type = ? AND dst_type = ? AND dst_id IN (`+idList(len(issueIDs))+`)))
			ORDER BY n.id LIMIT ?`, append(append(append(slices.Clone(issueIDs), KDesign, KIssue), issueIDs...), assessorDecisions)...)
		if err == nil {
			var notes []*DesignNote
			for rows.Next() {
				if n, err := scanDesign(rows); err == nil {
					notes = append(notes, n)
				}
			}
			rows.Close()
			for _, n := range notes {
				writeFenced(p, "Decision "+n.Code, n.Title, capText(n.Body, assessorDecisionCap))
			}
		}
	}
	p.WriteString("\n")
}

// writeFenced writes a task or decision under its key: the title and its
// text are whoever wrote them (an issue a person published from outside
// keeps its stranger's words), so both sit inside the <untrusted> wrapper
// and neither can close it or add a line of the prompt.
func writeFenced(p *strings.Builder, key, title, text string) {
	fmt.Fprintf(p, "\n%s:\n<untrusted>\n%s\n\n%s\n</untrusted>\n", key, untrusted(excerpt(oneLine(title), 300)), untrusted(text))
}

// writeAllRounds is every round so far: what it was given, who took it,
// how it ended and what it pushed, the diffs newest first within a budget.
func writeAllRounds(p *strings.Builder, b *Babysit) {
	if len(b.Rounds) == 0 {
		return
	}
	withDiff := map[int]bool{}
	budget := assessorDiffBudget
	for i := len(b.Rounds) - 1; i >= 0; i-- {
		if d := len(b.Rounds[i].Diff); d > 0 && d <= budget {
			withDiff[b.Rounds[i].N] = true
			budget -= d
		}
	}
	p.WriteString("The rounds so far:\n")
	for _, r := range b.Rounds {
		who := cmp(r.Model, b.Model)
		if r.Assessor {
			who += ", the assessor"
		}
		if r.To == "session" {
			who = "handed to a person: " + r.Why
		}
		fmt.Fprintf(p, "\nRound %d on %s (%s), given %s; %s.\n", r.N, short(r.SHA), who, cmp(roundWork(r.Checks, r.Conflict, r.Comments, r.Findings), "nothing"), cmp(r.Outcome, "no outcome"))
		if r.Answer == "" && !withDiff[r.N] && r.Reply == "" {
			continue
		}
		// A round's diff, answer and the person's reply are others' words,
		// fenced whole so none of them can close the wrapper.
		var w strings.Builder
		if withDiff[r.N] {
			w.WriteString("```diff\n" + strings.TrimRight(r.Diff, "\n") + "\n```\n")
		} else if r.Diff != "" {
			w.WriteString("(its diff is left out for length)\n")
		}
		if r.Answer != "" {
			w.WriteString(r.Answer + "\n")
		}
		if r.Question != "" {
			w.WriteString("It ended asking a person: " + r.Question + "\n")
		}
		if r.Because != "" {
			w.WriteString("Its reason no agent can settle that: " + r.Because + "\n")
		}
		if r.Waiting != "" {
			w.WriteString("It ended waiting for: " + r.Waiting + "\n")
		}
		switch {
		case r.Parked != "" && !r.RestUntil.IsZero():
			w.WriteString("Waypoint rested it, asking nobody: " + r.Parked + "\n")
		case r.Parked != "":
			w.WriteString("Waypoint parked it for a person: " + r.Parked + "\n")
		}
		if r.Reply != "" && r.RestUntil.IsZero() {
			w.WriteString("The person answered: " + r.Reply + "\n")
		}
		p.WriteString("<untrusted>\n" + untrusted(w.String()) + "</untrusted>\n")
	}
	p.WriteString("\n")
}

// capText cuts s to about n bytes on a rune boundary, keeping its lines.
func capText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "\n…"
}

// parkStats counts assessor rounds, rounds parked for a person and rounds
// rested with nobody asked (WP-930) for round stats.
func parkStats(rounds []BabysitRound) (strong, assessor, parked, rested int) {
	for _, r := range rounds {
		switch {
		case r.Assessor:
			assessor++
		case r.Model == escalateModel:
			strong++
		}
		switch {
		case r.Parked != "" && !r.RestUntil.IsZero():
			rested++
		case r.Parked != "":
			parked++
		}
	}
	return
}

// mdLabel makes text from the forge safe inside a markdown link label.
func mdLabel(s string) string {
	return strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]", "\n", " ", "\r", " ").Replace(s)
}

// mdURL is a comment's link when it is a plain http(s) one, else "".
func mdURL(u string) string {
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return ""
	}
	return strings.NewReplacer(" ", "%20", "(", "%28", ")", "%29", "<", "%3C", ">", "%3E", "\n", "", "\r", "").Replace(u)
}
