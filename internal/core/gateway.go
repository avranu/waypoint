package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// The extensions of the OpenAI API a gateway may declare. Each is one
// gateway's protocol; OpenLLMs is one gateway that implements them all.
const (
	// GatewayRoutes: the gateway answers named routes and purposes as
	// models: "free", "review", "review-verify", "prep" (gatewayModels),
	// and says which model answered in servedModelHeader. Without it every
	// task asks for the one model setup names (LLM_GATEWAY_MODEL).
	GatewayRoutes = "routes"
	// GatewayFallbacks: a chat request takes "fallbacks", the models to try
	// in the same request when the first can't answer.
	GatewayFallbacks = "fallbacks"
	// GatewayRouting: a chat request takes "routing" (paid, priority,
	// weakest) and an X-Request-ID the gateway keeps; surplus work, sent
	// only on capacity the gateway would otherwise leave idle, needs it and
	// routes.
	GatewayRouting = "routing"
	// GatewayExclude: a chat request takes "exclude", model globs to leave
	// out (a second reviewer's or a verifier's other model).
	GatewayExclude = "exclude"
	// GatewayKey: GET /key reports the key's daily request limit and use.
	GatewayKey = "key"
	// GatewayTasks: /tasks runs an agent over a packed checkout (audits
	// through the gateway, agentic prep trials).
	GatewayTasks = "tasks"
	// GatewayFeedback: POST /feedback takes how a request's answer fared.
	GatewayFeedback = "feedback"
	// GatewayDecide: a decision model answers questions about a state
	// (jev.go).
	GatewayDecide = "decide"
)

// GatewayExtensions are all of them, in the order help names them.
var GatewayExtensions = []string{GatewayRoutes, GatewayFallbacks, GatewayRouting, GatewayExclude, GatewayKey, GatewayTasks, GatewayFeedback, GatewayDecide}

// ParseGatewayExtensions reads LLM_GATEWAY_EXTENSIONS: names from
// GatewayExtensions, comma or space separated, or "all" or "none" alone.
// Unset is nil, which keeps what was declared before (none on a first
// setup).
func ParseGatewayExtensions(v string) ([]string, error) {
	fields := strings.FieldsFunc(strings.ToLower(v), func(r rune) bool { return r == ',' || r == ' ' })
	if len(fields) == 0 {
		return nil, nil
	}
	if len(fields) == 1 && fields[0] == "all" {
		return slices.Clone(GatewayExtensions), nil
	}
	out := []string{}
	for _, f := range fields {
		switch {
		case (f == "all" || f == "none") && len(fields) > 1:
			return nil, fmt.Errorf("%q stands alone: say it, or the extensions by name", f)
		case f == "none":
		case !slices.Contains(GatewayExtensions, f):
			return nil, fmt.Errorf("unknown gateway extension %q: say any of %s, all or none", f, strings.Join(GatewayExtensions, ", "))
		case !slices.Contains(out, f):
			out = append(out, f)
		}
	}
	return out, nil
}

// servedModelHeader is the response header a gateway with routes names the
// model that answered a route in.
const servedModelHeader = "X-OpenLLMs-Model"

// barHeader is the response header of a request that carried a minimum model
// (routing.weakest): weakest=…;model=…;quality=…;purpose=…;from=request
// (WP-1440). A response without a bar has none.
const barHeader = "X-OpenLLMs-Bar"

// GatewaySetup is the gateway as the environment describes it.
type GatewaySetup struct {
	BaseURL string // its OpenAI API, or its root (/v1 is added); "" keeps the saved one
	APIKey  string
	// Model is what every task asks for when the gateway has no routes;
	// with them it is unused. "" keeps the one saved before.
	Model string
	// Extensions are what it declares; nil keeps what a start before
	// declared, so a deploy that doesn't say keeps them.
	Extensions []string
	// Legacy: the setup came from the settings' names before WP-1187, whose
	// gateway had every extension, so a first setup with nil Extensions
	// declares them all.
	Legacy bool
}

// gatewayFreeRoute is a route: the gateway answers it with the first of its
// free models that can.
const gatewayFreeRoute = "free"

// gatewayModels are the models a task asks a gateway with routes for, in
// one request (the first, then the rest as fallbacks). A review asks for
// the gateway's review purpose, whose models the gateway ranks by
// benchmarks and by the feedback Waypoint reports on how their findings
// fared (WP-514, review_feedback.go); the verifier asks for review-verify,
// a hand-ordered route, so a different model checks the reviewer's
// findings (WP-D14). Either ends with the free route for whatever else can.
// A prep asks for the gateway's prep purpose, which it ranks by the reports
// Waypoint sends on how each prep's picks fared (WP-906, prep_models.go).
// A judgement of meaning asks for extract/judge alone, a custom purpose
// under the gateway's extract (fast, off the free pools other clients use)
// that starts ranked as extract and then by the reports Waypoint sends on
// how each grade fared against what later happened (WP-1035,
// goal_meaning_labels.go); the name always routes, so it needs no free
// route after it. The other tasks ask for the free route alone (WP-414).
func gatewayModels(task string) []string {
	switch task {
	case "review":
		return []string{gatewayReviewRoute, gatewayFreeRoute}
	case "verify_review":
		return []string{gatewayVerifyRoute, gatewayFreeRoute}
	case "prep":
		return []string{gatewayPrepPurpose, gatewayFreeRoute}
	case gatewayJudgePurpose:
		return []string{gatewayJudgePurpose}
	}
	return []string{gatewayFreeRoute}
}

// The gateway's routes for reviews, and its purpose for preps (see
// gatewayModels).
const (
	gatewayReviewRoute = "review"
	gatewayVerifyRoute = "review-verify"
	gatewayPrepPurpose = "prep"
	// gatewayJudgePurpose: judging how two short texts relate in meaning.
	// The gateway's custom purposes (its ADR 0093) rank extract/judge by
	// extract's benchmarks plus its own reports only, so the reports on
	// these grades never move extract's ranking, nor extract's this one.
	gatewayJudgePurpose = "extract/judge"
)

// gatewayJudgeExclude are the models a judgement of meaning leaves out, from
// WP_JUDGE_EXCLUDE (patterns separated by commas or spaces): a custom
// purpose can't carry the gateway's min_params, so a site whose gateway
// holds slow CPU models (a minute an answer, no use to a fast judge) names
// them here. A site's setting, so it lives in the environment, not the tree.
func gatewayJudgeExclude() []string {
	return strings.FieldsFunc(os.Getenv("WP_JUDGE_EXCLUDE"), func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
}

// maxGatewayFallbacks is how many fallbacks the gateway takes in one
// request.
const maxGatewayFallbacks = 8

// gatewayLLM is an LLM that can hand a gateway the models to try after the
// one asked for, so it skips one that is resting or rate limited at once.
type gatewayLLM interface {
	CompleteWithFallbacks(ctx context.Context, p *AIProvider, model string, fallbacks []string, system, prompt string, schema map[string]any) (json.RawMessage, string, error)
}

// The server_settings keys of the gateway's provider, its extensions, and
// the model the tasks ask it for when it has no routes.
const (
	settingGatewayProvider   = "llm_gateway_provider"
	settingGatewayExtensions = "llm_gateway_extensions"
	settingGatewayModel      = "llm_gateway_model"
)

// gatewayID is the gateway provider's id, 0 if there is none. The setting
// outlives a provider an admin removed (so a start doesn't add it back),
// which is no gateway.
func (s *Service) gatewayID(ctx context.Context) int64 {
	var id int64
	_ = s.db.R.QueryRowContext(ctx, `SELECT a.id FROM server_settings p JOIN ai_providers a ON a.id = CAST(p.value AS INTEGER)
		WHERE p.key = ?`, settingGatewayProvider).Scan(&id)
	return id
}

// gatewayWith is the gateway provider's id when it declares every one of
// exts, else 0: what needs an extension asks for the gateway this way.
// A failed lookup is logged and reads as no gateway here; a caller that
// must tell the two apart (a job deciding it has nothing to do) asks
// gatewayWithErr.
func (s *Service) gatewayWith(ctx context.Context, exts ...string) int64 {
	id, err := s.gatewayWithErr(ctx, exts...)
	if err != nil && ctx.Err() == nil {
		s.log.WarnContext(ctx, "read the gateway setting", "err", err)
	}
	return id
}

// gatewayWithErr is gatewayWith with the failure of the lookup returned: no
// gateway, or one without the extensions, is (0, nil).
func (s *Service) gatewayWithErr(ctx context.Context, exts ...string) (int64, error) {
	var id int64
	var have string
	err := s.db.R.QueryRowContext(ctx, `SELECT a.id, COALESCE(e.value, '') FROM server_settings p
		JOIN ai_providers a ON a.id = CAST(p.value AS INTEGER)
		LEFT JOIN server_settings e ON e.key = ? WHERE p.key = ?`, settingGatewayExtensions, settingGatewayProvider).Scan(&id, &have)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil || id == 0 {
		return 0, err
	}
	got := strings.Split(have, ",")
	for _, x := range exts {
		if !slices.Contains(got, x) {
			return 0, nil
		}
	}
	return id, nil
}

// EnsureLLMGateway sets up the gateway's provider. The first time, every AI
// task asks it first and keeps what it had as fallbacks, so a gateway that
// is down, or answers 502 because every model behind it failed, still
// leaves the old models: a gateway with routes is asked for gatewayModels,
// one without for g.Model. Later starts bring its URL, key, extensions and
// model up to date with the environment, and the tasks that still ask it
// for what setup gave them with them (gatewayLead); once an admin removes
// it, it stays removed. Whatever the environment leaves empty or unset
// keeps what was saved before, so a deploy that sets only the key keeps
// the gateway it has; the first setup needs a URL.
//
// The first setup records the extensions last: a start that finds the
// provider without them finishes a setup that stopped part way.
func (s *Service) EnsureLLMGateway(ctx context.Context, g GatewaySetup) error {
	var base string
	if g.BaseURL != "" {
		base = gatewayBase(g.BaseURL)
	}
	var v string
	err := s.db.R.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = ?`, settingGatewayProvider).Scan(&v)
	if err == nil {
		id, _ := strconv.ParseInt(v, 10, 64)
		cur, err := s.aiProvider(ctx, id)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if base == "" {
			base = cur.BaseURL
		}
		if cur.BaseURL != base || (g.APIKey != "" && cur.apiKey != g.APIKey) {
			in := AIProviderInput{BaseURL: &base}
			if g.APIKey != "" {
				in.APIKey = &g.APIKey
			}
			if _, err := s.UpdateAIProvider(ctx, SystemActor(), id, in); err != nil {
				return err
			}
		}
		var declared int
		if err := s.db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM server_settings WHERE key = ?`, settingGatewayExtensions).Scan(&declared); err != nil {
			return err
		}
		if declared == 0 {
			return s.finishGatewaySetup(ctx, id, g)
		}
		return s.updateGatewayLead(ctx, id, g)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if base == "" {
		return errors.New("LLM_GATEWAY_URL is not set: the first setup needs the gateway's URL")
	}
	if err := g.checkFirst(); err != nil {
		return err
	}
	name, kind := "LLM gateway", "openai"
	p, err := s.CreateAIProvider(ctx, SystemActor(), AIProviderInput{Name: &name, Kind: &kind, BaseURL: &base, APIKey: &g.APIKey})
	if err != nil {
		return err
	}
	if _, err := s.db.W.ExecContext(ctx, `INSERT INTO server_settings(key, value) VALUES (?, ?)`, settingGatewayProvider, itoa64(p.ID)); err != nil {
		return err
	}
	return s.finishGatewaySetup(ctx, p.ID, g)
}

// firstExtensions are what a first setup declares: nil is none, or every
// one for a setup from the old names.
func (g GatewaySetup) firstExtensions() []string {
	if g.Extensions == nil && g.Legacy {
		return GatewayExtensions
	}
	return g.Extensions
}

// checkFirst says what a first setup lacks.
func (g GatewaySetup) checkFirst() error {
	if !slices.Contains(g.firstExtensions(), GatewayRoutes) && strings.TrimSpace(g.Model) == "" {
		return errNoGatewayModel
	}
	return nil
}

// errNoGatewayModel: a gateway without routes has no model to ask for.
var errNoGatewayModel = errors.New("LLM_GATEWAY_MODEL is not set: a gateway without the routes extension needs the model every AI task asks it for")

// gatewayLead is what setup has task ask a gateway with exts for first: its
// routes, or the one model.
func gatewayLead(task string, exts []string, model string) []string {
	if slices.Contains(exts, GatewayRoutes) {
		return gatewayModels(task)
	}
	return []string{model}
}

// updateGatewayLead takes up a later start's extensions and model. When
// that changes what setup has the tasks ask the gateway for first (routes
// declared or dropped, another model), each task whose chain still starts
// with what it was given asks for the new lead instead, the rest of its
// chain kept; a chain an admin changed is theirs. The settings are written
// last, so a start that stops part way is finished by the next.
func (s *Service) updateGatewayLead(ctx context.Context, pid int64, g GatewaySetup) error {
	var had, oldModel string
	if err := s.db.R.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = ?`, settingGatewayExtensions).Scan(&had); err != nil {
		return err
	}
	err := s.db.R.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = ?`, settingGatewayModel).Scan(&oldModel)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	oldExts := strings.Split(had, ",")
	exts, model := g.Extensions, strings.TrimSpace(g.Model)
	if exts == nil {
		exts = oldExts
	}
	if model == "" {
		model = oldModel
	}
	if !slices.Contains(exts, GatewayRoutes) && model == "" {
		return errNoGatewayModel
	}
	for _, t := range aiTasks {
		from, to := gatewayLead(t.Task, oldExts, oldModel), gatewayLead(t.Task, exts, model)
		if slices.Equal(from, to) {
			continue
		}
		chain, err := s.taskChain(ctx, t.Task)
		if err != nil {
			return err
		}
		if !leadsWith(chain, pid, from) {
			continue
		}
		if err := s.setGatewayChain(ctx, pid, t.Task, to, chain[len(from):]); err != nil {
			return err
		}
	}
	if model != oldModel {
		if err := s.setGatewaySetting(ctx, settingGatewayModel, model); err != nil {
			return err
		}
	}
	if g.Extensions == nil {
		return nil
	}
	return s.setGatewayExtensions(ctx, g.Extensions)
}

// leadsWith says whether chain starts by asking gateway pid for lead.
func leadsWith(chain []ChainModel, pid int64, lead []string) bool {
	if len(chain) < len(lead) {
		return false
	}
	for i, m := range lead {
		if c := chain[i]; c.CLI != "" || c.ProviderID != pid || c.Model != m {
			return false
		}
	}
	return true
}

// finishGatewaySetup has each AI task that doesn't ask gateway pid first
// yet ask it first, then records the extensions, which mark the setup
// done.
func (s *Service) finishGatewaySetup(ctx context.Context, pid int64, g GatewaySetup) error {
	if err := g.checkFirst(); err != nil {
		return err
	}
	exts := g.firstExtensions()
	for _, t := range aiTasks {
		had, err := s.taskChain(ctx, t.Task)
		if err != nil {
			return err
		}
		if len(had) > 0 && had[0].CLI == "" && had[0].ProviderID == pid {
			continue
		}
		lead := []string{strings.TrimSpace(g.Model)}
		if slices.Contains(exts, GatewayRoutes) {
			lead = gatewayModels(t.Task)
		}
		if err := s.setGatewayChain(ctx, pid, t.Task, lead, had); err != nil {
			return err
		}
	}
	if err := s.setGatewaySetting(ctx, settingGatewayModel, strings.TrimSpace(g.Model)); err != nil {
		return err
	}
	return s.setGatewayExtensions(ctx, exts)
}

// setGatewayExtensions records what the gateway declares.
func (s *Service) setGatewayExtensions(ctx context.Context, exts []string) error {
	return s.setGatewaySetting(ctx, settingGatewayExtensions, strings.Join(exts, ","))
}

// setGatewaySetting writes one of the gateway's settings.
func (s *Service) setGatewaySetting(ctx context.Context, key, value string) error {
	_, err := s.db.W.ExecContext(ctx, `INSERT INTO server_settings(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// setGatewayChain has task ask the gateway for lead first, then the rest
// of what it had.
func (s *Service) setGatewayChain(ctx context.Context, pid int64, task string, lead []string, rest []ChainModel) error {
	var chain []ChainModel
	for _, m := range lead {
		chain = append(chain, ChainModel{ProviderID: pid, Model: m})
	}
	for _, cm := range rest {
		if _, err := s.aiProvider(ctx, cm.ProviderID); err == nil || !errors.Is(err, ErrNotFound) {
			chain = append(chain, cm)
		}
	}
	chain = chain[:min(len(chain), 11)]
	return s.SetAITask(ctx, SystemActor(), task, pid, chain[0].Model, chain[1:])
}

// gatewayBase is the gateway's OpenAI API: its root, as Anthropic clients
// take it, gets /v1.
func gatewayBase(base string) string {
	base = providerBase("openai", base)
	if u, err := url.Parse(base); err == nil && strings.Trim(u.Path, "/") == "" {
		base += "/v1"
	}
	return base
}
