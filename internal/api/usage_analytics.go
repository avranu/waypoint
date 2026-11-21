package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Usage analytics (WP-D20): `wp usage sync` and `wp statusline` upload, the
// Agents → Usage tab reads. One person's data; admins may read anyone's.
func (s *Server) usageAnalyticsRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("POST /api/v1/usage/ingest", a(s.usageIngest))
	mux.HandleFunc("POST /api/v1/usage/quota", a(s.usageQuota))
	mux.HandleFunc("GET /api/v1/usage/overview", a(s.usageOverview))
	mux.HandleFunc("GET /api/v1/usage/machines", a(s.usageMachinesList))
	mux.HandleFunc("GET /api/v1/usage/ledger", a(s.usageLedger))
	mux.HandleFunc("GET /api/v1/usage/trends", a(s.usageTrends))
	mux.HandleFunc("GET /api/v1/usage/cli", a(s.usageCLI))
	mux.HandleFunc("GET /api/v1/usage/compare", a(s.usageCompare))
	mux.HandleFunc("GET /api/v1/usage/feature", a(s.usageFeature))
	mux.HandleFunc("GET /api/v1/usage/task/{key}", a(s.usageTask))
	mux.HandleFunc("GET /api/v1/usage/summary", a(s.usageSummary))
	mux.HandleFunc("GET /api/v1/usage/breakdown", a(s.usageBreakdown))
	mux.HandleFunc("GET /api/v1/usage/refusals", a(s.usageRefusals))
	mux.HandleFunc("GET /api/v1/usage/risky", a(s.usageRisky))
	mux.HandleFunc("GET /api/v1/usage/warm", a(s.usageWarm))
	mux.HandleFunc("GET /api/v1/usage/delegation", a(s.usageDelegation))
	mux.HandleFunc("GET /api/v1/usage/goal-context", a(s.usageGoalContext))
	mux.HandleFunc("GET /api/v1/usage/meaning", a(s.usageMeaning))
	mux.HandleFunc("GET /api/v1/usage/stalled", a(s.usageStalled))
	mux.HandleFunc("GET /api/v1/usage/guard", a(s.usageGuard))
	mux.HandleFunc("GET /api/v1/usage/guard/live", a(s.usageGuardLive))
	mux.HandleFunc("PUT /api/v1/usage/guard/settings", a(s.setUsageGuard))
	mux.HandleFunc("POST /api/v1/usage/guard/lift", a(s.liftUsageGuard))
	mux.HandleFunc("GET /api/v1/usage/prices", a(s.usagePrices))
	mux.HandleFunc("PUT /api/v1/usage/prices", a(s.setUsagePrice))
	mux.HandleFunc("DELETE /api/v1/usage/prices", a(s.deleteUsagePrice))
}

// maxUsageBody bounds one upload; the CLI sends far less.
const maxUsageBody = 8 << 20

// decodeLarge reads a JSON body bigger than decode allows. Unknown fields
// are ignored, so a newer wp can talk to an older server.
func decodeLarge(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxUsageBody))
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: request body is empty", errBadRequest)
		}
		return fmt.Errorf("%w: %v", errBadRequest, err)
	}
	return nil
}

func (s *Server) usageIngest(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.UsageIngest
	if err := decodeLarge(w, r, &in); err != nil {
		return nil, err
	}
	return s.svc.IngestUsage(r.Context(), actorFrom(r), in)
}

func (s *Server) usageQuota(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.UsageQuotaInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, s.svc.RecordQuota(r.Context(), actorFrom(r), in)
}

func usageQueryFrom(r *http.Request) core.UsageQuery {
	v := r.URL.Query()
	return core.UsageQuery{Days: queryInt(r, "days", 30), Agent: v.Get("agent"), Project: v.Get("project"),
		Machine: v.Get("machine"), Session: v.Get("session"), User: v.Get("user")}
}

func (s *Server) usageOverview(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.UsageOverview(r.Context(), actorFrom(r), usageQueryFrom(r))
}

func (s *Server) usageLedger(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.UsageLedger(r.Context(), actorFrom(r), usageQueryFrom(r))
}

func (s *Server) usageTrends(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.UsageTrends(r.Context(), actorFrom(r), usageQueryFrom(r))
}

func (s *Server) usageCLI(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.UsageCLI(r.Context(), actorFrom(r), usageQueryFrom(r), r.URL.Query().Get("repo"))
}

func (s *Server) usageFeature(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	return s.svc.UsageFeature(r.Context(), actorFrom(r), usageQueryFrom(r), core.FeatureQuery{Ref: v.Get("ref"), Window: queryInt(r, "window", 30), Repo: v.Get("repo")})
}

func (s *Server) usageTask(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.UsageForTask(r.Context(), actorFrom(r), usageQueryFrom(r), r.PathValue("key"))
}

func (s *Server) usageCompare(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	at, err := parseTimeParam(v.Get("at"))
	if err != nil {
		return nil, err
	}
	return s.svc.UsageCompare(r.Context(), actorFrom(r), usageQueryFrom(r), core.CompareQuery{At: at, Window: queryInt(r, "window", 7), Repo: v.Get("repo")})
}

func (s *Server) usageSummary(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.UsageSummary(r.Context(), actorFrom(r))
}

func (s *Server) usageMachinesList(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.UsageMachines(r.Context(), actorFrom(r), r.URL.Query().Get("user"))
}

func (s *Server) usageBreakdown(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.UsageBreakdowns(r.Context(), actorFrom(r), usageQueryFrom(r), r.URL.Query().Get("by"), queryInt(r, "limit", 100))
}

func (s *Server) usageRefusals(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.UsageRefusals(r.Context(), actorFrom(r), usageQueryFrom(r), int64(queryInt(r, "before", 0)), queryInt(r, "limit", 25))
}

func (s *Server) usageRisky(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	return s.svc.UsageRiskyCommands(r.Context(), actorFrom(r), usageQueryFrom(r), core.UsageRiskyFilter{
		Level: v.Get("level"), Kind: v.Get("kind"), Outcome: v.Get("outcome"), Before: int64(queryInt(r, "before", 0)), Limit: queryInt(r, "limit", 50)})
}

// usageWarm: keep-warm decisions and what became of the sessions (WP-1000).
func (s *Server) usageWarm(w http.ResponseWriter, r *http.Request) (any, error) {
	q := usageQueryFrom(r)
	q.Days = queryInt(r, "days", 28)
	return s.svc.WarmDecisions(r.Context(), actorFrom(r), q)
}

// usageDelegation: how agents are told to split work between models, and
// what came of each way (WP-1262).
func (s *Server) usageDelegation(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.DelegationPolicy(r.Context(), actorFrom(r))
}

// usageGoalContext: how much of a task's goal chain agents are shown, and
// what came of each way (WP-1035).
func (s *Server) usageGoalContext(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.GoalContextPolicy(r.Context(), actorFrom(r))
}

// usageMeaning: the judgements of meaning about goals, their deciders,
// labels and scorers (WP-1035).
func (s *Server) usageMeaning(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.MeaningState(r.Context(), actorFrom(r))
}

// usageStalled: quiet background jobs' spells, how their sessions were
// told and how the spells ended (WP-1045).
func (s *Server) usageStalled(w http.ResponseWriter, r *http.Request) (any, error) {
	q := usageQueryFrom(r)
	q.Days = queryInt(r, "days", 28)
	return s.svc.StalledUsage(r.Context(), actorFrom(r), q)
}

// usageGuard: the daily quota guard's checks (WP-932); now=1 adds one of
// the last 24 hours.
func (s *Server) usageGuard(w http.ResponseWriter, r *http.Request) (any, error) {
	q := usageQueryFrom(r)
	q.Days = queryInt(r, "days", 14)
	fresh := r.URL.Query().Get("now")
	return s.svc.QuotaGuard(r.Context(), actorFrom(r), q, fresh == "1" || fresh == "true")
}

// usageGuardLive: the live quota guard, its lines and its trips (WP-932).
func (s *Server) usageGuardLive(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.QuotaGuardLiveState(r.Context(), actorFrom(r), r.URL.Query().Get("user"), queryInt(r, "days", 7))
}

func (s *Server) setUsageGuard(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.QuotaGuardSettingsInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SetQuotaGuard(r.Context(), actorFrom(r), in)
}

// liftUsageGuard ends a hold; {"for": "2h"} keeps a new one off that long.
func (s *Server) liftUsageGuard(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		For string `json:"for"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.LiftQuotaGuard(r.Context(), actorFrom(r), in.For)
}

func (s *Server) usagePrices(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListModelPrices(r.Context(), actorFrom(r))
}

func (s *Server) setUsagePrice(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.ModelPrice
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SetModelPrice(r.Context(), actorFrom(r), in)
}

func (s *Server) deleteUsagePrice(w http.ResponseWriter, r *http.Request) (any, error) {
	return nil, s.svc.DeleteModelPrice(r.Context(), actorFrom(r), r.URL.Query().Get("model"))
}
