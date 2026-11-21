package core

// Feedback to the gateway on the review models it picks (WP-514, under WP-362).
// A review asks the gateway's review purpose for a model; the gateway ranks
// that purpose's models by benchmarks plus what clients report of how their
// answers' findings fared (POST /v1/feedback). So each
// review request carries an X-Request-ID of Waypoint's own and asks for
// paid models ranked with the free ones (the owner agreed to the spend
// 2026-11-03); each finding remembers the requests whose answers raised it;
// and a sweep reports each request's findings as they are judged, again
// whenever the count changes, which the gateway takes as a replacement.
//
// What counts, from the finding's state and its thread's verdict:
//   - verified: fixed (a later push removed the problem, or an audit
//     agreed), or still open or resolved with its thread's verdict saying
//     an agent took it (accepted, fixing, fixed, verified);
//   - false positive: dismissed (by a person or an agent, with a note),
//     withdrawn (the bot agreed it was wrong), or dropped before posting
//     because the code it quoted is not in the file.
// Anything else is not judged yet. What the verifier model dropped is its
// opinion, not a judgment of the finding, and is not reported.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// gatewayRouting is what a caller wants of its requests to the gateway: an
// X-Request-ID on each, starting with prefix, a routing.paid choice, a
// routing.priority and a routing.weakest minimum. runChainAt turns it into a gatewayAsk for each request
// to the gateway, and never sends it to another provider.
type gatewayRouting struct {
	prefix   string // token characters, short
	paid     string // never, fallback, allowed; "" leaves the gateway's default
	priority string // "" (the gateway's default) or prioritySurplus (WP-906)
	weakest  string // the weakest model it accepts, by a name the gateway rates; "" sets no minimum (WP-1440)
}

type gatewayRoutingKey struct{}

func withGatewayRouting(ctx context.Context, r *gatewayRouting) context.Context {
	return context.WithValue(ctx, gatewayRoutingKey{}, r)
}

func gatewayRoutingOf(ctx context.Context) *gatewayRouting {
	r, _ := ctx.Value(gatewayRoutingKey{}).(*gatewayRouting)
	return r
}

// newID is a fresh X-Request-ID: the prefix and 8 random hex digits, so
// two Waypoints on one key, or a run's retry, never share one. The gateway
// takes up to 64 token characters.
func (r *gatewayRouting) newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	id := r.prefix + "-" + hex.EncodeToString(b)
	if len(id) > 64 {
		id = id[len(id)-64:]
	}
	return id
}

// reviewRouting is a review run's: its requests are named by the run and
// may get paid models ranked with the free ones.
func reviewRouting(runID int64) *gatewayRouting {
	return &gatewayRouting{prefix: fmt.Sprintf("wp-review-%d", runID), paid: "allowed", weakest: reviewFloorBar}
}

// verifyRouting is the verifier's: a code review's second half, so it asks
// for the same minimum. Its request ids reach the run's log only; the
// feedback sweep reads review_requests, which only the reviewers' answers
// fill, so the verifier's opinion is still not reported.
func verifyRouting(runID int64) *gatewayRouting {
	return &gatewayRouting{prefix: fmt.Sprintf("wp-verify-%d", runID), weakest: reviewFloorBar}
}

// fixCheckRouting, replyRouting and heldRouting are the other review-chain
// tasks': each decides something about a code review's findings or a change
// to CI, so each asks the gateway for the same minimum (WP-1440), under its
// own request ids. Those ids reach no feedback: the sweep reads
// review_requests, which only the reviewers' answers fill. paid stays "".
func fixCheckRouting(runID int64) *gatewayRouting {
	return &gatewayRouting{prefix: fmt.Sprintf("wp-fixcheck-%d", runID), weakest: reviewFloorBar}
}

func replyRouting(runID int64) *gatewayRouting {
	return &gatewayRouting{prefix: fmt.Sprintf("wp-reply-%d", runID), weakest: reviewFloorBar}
}

func heldRouting(prID int64) *gatewayRouting {
	return &gatewayRouting{prefix: fmt.Sprintf("wp-held-%d", prID), weakest: reviewFloorBar}
}

// gatewayAsk is what one request to the gateway carries beyond the chat: the
// httpLLM client sends requestID as X-Request-ID, paid as routing.paid,
// priority as routing.priority and weakest as routing.weakest.
type gatewayAsk struct {
	requestID string
	paid      string
	priority  string
	weakest   string
}

type gatewayAskKey struct{}

func withGatewayAsk(ctx context.Context, a *gatewayAsk) context.Context {
	return context.WithValue(ctx, gatewayAskKey{}, a)
}

func gatewayAskOf(ctx context.Context) *gatewayAsk {
	a, _ := ctx.Value(gatewayAskKey{}).(*gatewayAsk)
	return a
}

// feedbackLLM is a gateway that takes reports on its answers.
type feedbackLLM interface {
	Feedback(ctx context.Context, p *AIProvider, r feedbackReport) error
}

// feedbackReport is one report on one request's answer: how many of its
// findings held up and how many did not, or (noFindings) that it raised
// none at all (WP-1230).
type feedbackReport struct {
	requestID          string
	verified, falsePos int
	noFindings         bool
}

// Feedback reports how a request's findings fared (the gateway: POST
// /v1/feedback). A 404 is the gateway not knowing the request (yet).
//
// An answer that raised nothing is reported as no_findings (the gateway's ADR
// 0091): a report of none verified and none false, which lowers the
// model's yield (findings verified per task) but not its precision, since
// there may have been nothing to find. Counts of 0 without it would
// withdraw a report instead.
func (c *httpLLM) Feedback(ctx context.Context, p *AIProvider, r feedbackReport) error {
	body := map[string]any{"request_id": r.requestID, "verified": r.verified, "false_positives": r.falsePos}
	if r.noFindings {
		body = map[string]any{"request_id": r.requestID, "no_findings": true}
	}
	return c.call(ctx, p, http.MethodPost, "/feedback", body, nil)
}

// recordEmptyRequests records, inside t, the requests whose answers raised
// no findings at all, so the sweep reports them as no_findings (WP-1230):
// a request is otherwise only known by the findings it raised.
func (s *Service) recordEmptyRequests(ctx context.Context, t *tx, runID int64, requests []string, now int64) error {
	for _, id := range requests {
		if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO review_requests(request_id, run_id, created_at) VALUES (?,?,?)`, id, runID, now); err != nil {
			return err
		}
	}
	return nil
}

// linkFindingRequests records, inside t, that the requests raised finding.
func (s *Service) linkFindingRequests(ctx context.Context, t *tx, runID, findingID int64, requests []string, now int64) error {
	for _, id := range requests {
		if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO review_requests(request_id, run_id, created_at) VALUES (?,?,?)`, id, runID, now); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO review_finding_requests(finding_id, request_id) VALUES (?,?)`, findingID, id); err != nil {
			return err
		}
	}
	return nil
}

const (
	// feedbackWindow: the gateway ranks by the past 30 days of reports, so
	// an older request's findings no longer matter to it.
	feedbackWindow = 30 * 24 * time.Hour
	// feedbackBatch bounds the reports one sweep sends.
	feedbackBatch = 200
	// feedbackMaxCount is the most either count may be in one report.
	feedbackMaxCount = 100
	// feedbackUnknownAfter: a request the gateway still doesn't know this
	// long after it was answered is another key's or gone; its usage
	// record is written a second or two after the answer.
	feedbackUnknownAfter = time.Hour
	feedbackRetryMin     = 5 * time.Minute
	feedbackRetryMax     = 6 * time.Hour
	feedbackCallTimeout  = 20 * time.Second
)

// reviewFeedbackDue is one request whose report is out of date.
type reviewFeedbackDue struct {
	id                   string
	createdAt            int64
	tries                int
	verified, falsePos   int // now
	sentVerified, sentFP int // as last reported
	everReported         bool
	// noFindings: the answer raised no findings at all; reported once, as
	// no_findings.
	noFindings bool
}

// feedbackDue says whether a report on d is to be sent: one never sent
// that has judged findings or raised none, or one whose counts changed.
func feedbackDue(d reviewFeedbackDue) bool {
	if !d.everReported {
		return d.noFindings || d.verified+d.falsePos > 0
	}
	return d.verified != d.sentVerified || d.falsePos != d.sentFP
}

// ReportReviewFeedback sends the gateway a report for each review request
// whose findings' verdicts changed since its last report, or that it has
// never had one for. A sweep that meets a gateway that is down stops
// there; the rest wait for the next.
func (s *Service) ReportReviewFeedback(ctx context.Context) {
	if !s.feedbackBusy.CompareAndSwap(false, true) {
		jobBusy(ctx, "the last report is still going")
		return
	}
	defer s.feedbackBusy.Store(false)
	fl, ok := s.LLM.(feedbackLLM)
	pid, err := s.gatewayWithErr(ctx, GatewayFeedback)
	if err != nil {
		s.log.WarnContext(ctx, "review feedback: could not tell whether a gateway is set up", "err", err)
		return
	}
	if !ok || pid == 0 {
		jobSkip(ctx, "no gateway is set up to take review feedback")
		return
	}
	p, err := s.aiProvider(ctx, pid)
	if err != nil {
		s.log.WarnContext(ctx, "review feedback: read the gateway provider", "err", err)
		return
	}
	due, err := s.reviewFeedbackDue(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "review feedback: read the findings", "err", err)
		return
	}
	if !s.sendFeedback(ctx, fl, p, due, reviewRequestReports) {
		return
	}
	// The audits the gateway ran as agent tasks (WP-617), by task id.
	s.reportAuditFeedback(ctx, fl, p)
	// How preps' and prep trials' file picks fared (WP-906).
	s.reportPrepFeedback(ctx, fl, p)
	// How the judge purpose's grades of meaning fared (WP-1035).
	s.reportMeaningFeedback(ctx, fl, p)
}

// feedbackStore is where one kind of report is remembered: the statement
// for a report the gateway took (the counts, the time, the id) and for one
// it didn't (the next try, the error, the id); window is how long after its
// request a report is still sent (feedbackWindow when 0).
type feedbackStore struct {
	took, failed string
	window       time.Duration
}

var reviewRequestReports = feedbackStore{
	took:   `UPDATE review_requests SET verified = ?, false_positives = ?, reported_at = ?, tries = 0, next_try_at = 0, error = '' WHERE request_id = ?`,
	failed: `UPDATE review_requests SET tries = tries + 1, next_try_at = ?, error = ? WHERE request_id = ?`,
}

// sendFeedback sends the reports that are due and remembers each in st.
// One the gateway doesn't know (yet) waits its turn again; any other
// failure ends the sweep, and the answer is false: the gateway is down or
// refusing this key, and the rest would meet the same.
func (s *Service) sendFeedback(ctx context.Context, fl feedbackLLM, p *AIProvider, due []reviewFeedbackDue, st feedbackStore) bool {
	for _, d := range due {
		if ctx.Err() != nil {
			return false
		}
		cctx, cancel := context.WithTimeout(ctx, feedbackCallTimeout)
		err := fl.Feedback(cctx, p, feedbackReport{requestID: d.id, verified: min(d.verified, feedbackMaxCount),
			falsePos: min(d.falsePos, feedbackMaxCount), noFindings: d.noFindings && d.verified+d.falsePos == 0})
		cancel()
		if err == nil {
			s.recordFeedback(ctx, st.took, d.verified, d.falsePos, s.nowMS(), d.id)
			continue
		}
		if ctx.Err() != nil {
			return false
		}
		var le *llmError
		unknown := errors.As(err, &le) && le.status == http.StatusNotFound && strings.Contains(le.body, "request_not_found")
		next := s.now().Add(min(feedbackRetryMin<<min(d.tries, 10), feedbackRetryMax)).UnixMilli()
		if unknown && s.now().Sub(msTime(d.createdAt)) > feedbackUnknownAfter {
			// Never to be known: try again only past the window, i.e. never.
			window := st.window
			if window == 0 {
				window = feedbackWindow
			}
			next = d.createdAt + window.Milliseconds()
		}
		s.recordFeedback(ctx, st.failed, next, excerpt(aiPublicError(err), 300), d.id)
		if !unknown {
			s.log.WarnContext(ctx, "review feedback: the gateway did not take a report; the rest wait", "request", d.id, "err", err)
			return false
		}
	}
	return true
}

func (s *Service) recordFeedback(ctx context.Context, q string, args ...any) {
	if _, err := s.db.W.ExecContext(ctx, q, args...); err != nil {
		s.log.WarnContext(ctx, "review feedback: record a report", "err", err)
	}
}

// reviewFeedbackDue reads the review requests of the feedback window whose
// counts differ from what was last reported (or that have counts and were
// never reported), oldest first, at most feedbackBatch.
func (s *Service) reviewFeedbackDue(ctx context.Context) ([]reviewFeedbackDue, error) {
	now := s.now()
	rows, err := s.db.R.QueryContext(ctx, `
		SELECT q.request_id, q.created_at, q.tries, q.verified, q.false_positives, q.reported_at IS NOT NULL,
			COALESCE(SUM(CASE WHEN f.state = ? OR (f.state IN (?, ?) AND EXISTS (
				SELECT 1 FROM review_comments c WHERE c.finding_id = f.id AND c.verdict IN (?, ?, ?, ?))) THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN f.state IN (?, ?) OR (f.state = ? AND f.verify = ? AND f.verify_reason = ?) THEN 1 ELSE 0 END), 0),
			count(f.id) = 0
		FROM review_requests q
		LEFT JOIN review_finding_requests l ON l.request_id = q.request_id
		LEFT JOIN bot_findings f ON f.id = l.finding_id
		WHERE q.created_at >= ? AND q.next_try_at <= ?
		GROUP BY q.request_id
		ORDER BY q.created_at`,
		FindingFixed, FindingOpen, FindingResolved, VerdictAccepted, VerdictFixing, VerdictFixed, VerdictVerified,
		FindingDismissed, FindingWithdrawn, FindingDropped, VerifyUnchecked, evidenceMissing,
		now.Add(-feedbackWindow).UnixMilli(), now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []reviewFeedbackDue
	for rows.Next() {
		var d reviewFeedbackDue
		if err := rows.Scan(&d.id, &d.createdAt, &d.tries, &d.sentVerified, &d.sentFP, &d.everReported, &d.verified, &d.falsePos, &d.noFindings); err != nil {
			return nil, err
		}
		if !feedbackDue(d) {
			continue
		}
		if out = append(out, d); len(out) == feedbackBatch {
			break
		}
	}
	return out, rows.Err()
}
