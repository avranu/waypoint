package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Automations that run in steps (WP-D18): the catalog the editor draws
// from, runs and their steps, running by hand, tests, the incoming webhook
// and the admin side.
func (s *Server) automationRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/automation-catalog", a(s.automationCatalog))
	mux.HandleFunc("GET /api/v1/automation-cron", a(s.automationCron))
	mux.HandleFunc("GET /api/v1/automations/{id}", a(s.getAutomation))
	mux.HandleFunc("GET /api/v1/automations/{id}/runs", a(s.automationRuns))
	mux.HandleFunc("POST /api/v1/automations/{id}/run", a(s.runAutomationNow))
	mux.HandleFunc("POST /api/v1/automations/{id}/test", a(s.testAutomation))
	mux.HandleFunc("POST /api/v1/automations/{id}/webhook-address", a(s.newAutomationHook))
	mux.HandleFunc("GET /api/v1/automation-runs/{id}", a(s.automationRun))
	mux.HandleFunc("POST /api/v1/automation-runs/{id}/cancel", a(s.cancelAutomationRun))
	mux.HandleFunc("POST /api/v1/automation-runs/{id}/rerun", a(s.rerunAutomation))

	mux.HandleFunc("GET /api/v1/admin/automations", a(s.adminAutomations))
	mux.HandleFunc("POST /api/v1/admin/automations", a(s.createAdminAutomation))
	mux.HandleFunc("GET /api/v1/admin/system", a(s.adminSystem))
	mux.HandleFunc("PUT /api/v1/admin/system-limits", a(s.setSystemLimits))

	// The incoming webhook trigger: the secret address is the credential.
	mux.HandleFunc("POST /hooks/automations/{token}", s.automationHook)
}

func (s *Server) automationCatalog(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.AutomationCatalog(r.Context(), actorFrom(r)), nil
}

func (s *Server) automationCron(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	runs, err := s.svc.NextCronRuns(v.Get("expr"), v.Get("tz"), queryIntIn(r, "n", 5, 1, 20))
	if err != nil {
		return nil, err
	}
	return map[string]any{"next": runs}, nil
}

func (s *Server) getAutomation(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.GetAutomation(r.Context(), actorFrom(r), id)
}

func (s *Server) automationRuns(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.ListAutomationRuns(r.Context(), actorFrom(r), id, queryInt(r, "limit", 30), int64(queryInt(r, "before", 0)))
}

func (s *Server) automationRun(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.GetAutomationRun(r.Context(), actorFrom(r), id)
}

func (s *Server) runAutomationNow(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Inputs map[string]any `json:"inputs"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	via := "api"
	if r.Header.Get("X-Waypoint-Client") == "wp" {
		via = "cli"
	} else if r.Header.Get("X-Waypoint-Client") == "web" {
		via = "manual"
	}
	return s.svc.RunAutomationNow(r.Context(), actorFrom(r), id, in.Inputs, via)
}

func (s *Server) testAutomation(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.TestInput
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.TestAutomation(r.Context(), actorFrom(r), id, in)
}

func (s *Server) newAutomationHook(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.RegenerateAutomationHook(r.Context(), actorFrom(r), id)
}

func (s *Server) cancelAutomationRun(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.CancelAutomationRun(r.Context(), actorFrom(r), id)
}

func (s *Server) rerunAutomation(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.RerunAutomation(r.Context(), actorFrom(r), id)
}

// ---- admin ----

func (s *Server) adminAutomations(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListAdminAutomations(r.Context(), actorFrom(r))
}

func (s *Server) createAdminAutomation(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.AutomationInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateAdminAutomation(r.Context(), actorFrom(r), in)
}

// adminSystem reports the resources admin automations watch and their limits.
func (s *Server) adminSystem(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.svc.ResourceStatuses(r.Context(), actorFrom(r))
	if err != nil {
		return nil, err
	}
	lim, err := s.svc.GetResourceLimits(r.Context(), actorFrom(r))
	if err != nil {
		return nil, err
	}
	return map[string]any{"resources": st, "limits": lim}, nil
}

func (s *Server) setSystemLimits(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.ResourceLimits
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.SetResourceLimits(r.Context(), actorFrom(r), in)
}

// ---- the incoming webhook ----

var (
	// hookLimiter caps requests per webhook address; hookMisses caps wrong
	// addresses and bad signatures per client, so addresses can't be guessed.
	hookLimiter = newRateLimiter(60, time.Minute)
	hookMisses  = newRateLimiter(20, time.Minute)
)

// maxHookBody is the largest body an automation webhook takes: it is stored
// with every run it starts.
const maxHookBody = 256 << 10

func (s *Server) automationHook(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if hookMisses.blocked(ip) {
		s.writeError(w, r, errTooMany)
		return
	}
	token := r.PathValue("token")
	if !hookLimiter.allow(token) {
		s.writeError(w, r, errTooMany)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxHookBody+1))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if len(body) > maxHookBody {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("the body is larger than %d KB", maxHookBody>>10)})
		return
	}
	sig := cmpStr(r.Header.Get("X-Hub-Signature-256"), r.Header.Get("X-Waypoint-Signature"))
	run, err := s.svc.RunAutomationWebhook(r.Context(), token, body, sig)
	switch {
	case errors.Is(err, core.ErrNoHook):
		hookMisses.hit(ip)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such webhook"})
		return
	case errors.Is(err, core.ErrBadSignature):
		hookMisses.hit(ip)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "the signature doesn't match"})
		return
	case err != nil:
		s.writeError(w, r, err)
		return
	}
	if run == nil { // the trigger's filters turned it away
		writeJSON(w, http.StatusAccepted, map[string]any{"run": nil})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run": run.ID, "status": run.Status})
}

// ---- MCP ----

var automationTools = []mcpTool{
	{Name: "list_automations", Title: "List a project's automations", Annotations: readOnly,
		Description: "A project's automations: id, name, trigger, whether you can run it (manual ones that allow agents) and its inputs.",
		InputSchema: obj([]string{"project"}, map[string]any{"project": str("Project key")})},
	{Name: "run_automation", Title: "Run an automation by hand",
		Description: "Run a manual automation that allows agents, with the inputs it asks for. Returns the run and its status; long ones finish in the background.",
		InputSchema: obj([]string{"id"}, map[string]any{"id": map[string]any{"type": "integer", "description": "Automation id from list_automations"},
			"inputs": map[string]any{"type": "object", "description": "Input values by name"}})},
}

func init() { mcpTools = append(mcpTools, automationTools...) }

func (s *Server) mcpAutomations(ctx context.Context, actor *core.Actor, name string, args mcpArgs) (string, bool, error) {
	switch name {
	case "list_automations":
		list, err := s.svc.ListAutomations(ctx, actor, args.str("project"))
		if err != nil {
			return "", true, err
		}
		var b strings.Builder
		for _, a := range list {
			runnable := "not runnable by hand"
			if a.Rule.Trigger.Event == "manual" {
				runnable = "run by hand: you may not"
				if a.Rule.AllowAgents {
					runnable = "run by hand: you may"
				}
			}
			fmt.Fprintf(&b, "#%d %s (%s; %s)", a.ID, a.Name, a.Rule.Trigger.Event, runnable)
			if !a.Enabled {
				b.WriteString(" [off]")
			}
			for _, in := range a.Rule.Inputs {
				fmt.Fprintf(&b, "\n    input %s (%s)%s", in.Name, in.Type, map[bool]string{true: " required", false: ""}[in.Required])
			}
			b.WriteString("\n")
		}
		if b.Len() == 0 {
			return "No automations.", true, nil
		}
		return strings.TrimRight(b.String(), "\n"), true, nil
	case "run_automation":
		in, ok := args["inputs"].(map[string]any)
		if _, given := args["inputs"]; given && args["inputs"] != nil && !ok {
			return "", true, fmt.Errorf("inputs must be an object of name: value")
		}
		run, err := s.svc.RunAutomationNow(ctx, actor, int64(args.int("id", 0)), in, "api")
		if err != nil {
			return "", true, err
		}
		return fmt.Sprintf("Run #%d of %s: %s. %s", run.ID, run.Automation, run.Status, run.Error), true, nil
	}
	return "", false, nil
}
