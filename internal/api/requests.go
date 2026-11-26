package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Requests (WP-D16): what an agent asks of a person while it keeps working.
func (s *Server) requestRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/waiting", a(s.waitingOnYou))
	mux.HandleFunc("GET /api/v1/requests", a(s.listRequests))
	mux.HandleFunc("GET /api/v1/refusals", a(s.listRefusals))
	mux.HandleFunc("POST /api/v1/refusals/{id}/rule", a(s.allowRefusedCall))
	mux.HandleFunc("POST /api/v1/requests", a(s.createRequest))
	mux.HandleFunc("GET /api/v1/requests/{ref}", a(s.getRequest))
	mux.HandleFunc("POST /api/v1/requests/{ref}/decide", a(s.decideRequest))
	mux.HandleFunc("POST /api/v1/requests/{ref}/snooze", a(s.snoozeRequest))
	mux.HandleFunc("POST /api/v1/requests/{ref}/withdraw", a(s.withdrawRequest))
	mux.HandleFunc("POST /api/v1/requests/{ref}/watch", a(s.watchRequest))
	mux.HandleFunc("POST /api/v1/requests/{ref}/cells/{n}/run", a(s.runRequestCell))
}

func (s *Server) waitingOnYou(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.WaitingOnYou(r.Context(), actorFrom(r))
}

func (s *Server) listRequests(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	return s.svc.ListRequests(r.Context(), actorFrom(r), core.RequestQuery{
		State: v.Get("state"), Mine: queryBool(r, "mine"), Session: int64(queryInt(r, "session", 0)), Project: v.Get("project"),
		Kind: v.Get("kind"), Agent: v.Get("agent"), Limit: queryInt(r, "limit", 50), Offset: queryInt(r, "offset", 0),
	})
}

// listRefusals reads the log of tool calls the auto-mode classifier refused
// (WP-232): ?project= &agent= &days= &session= &before=ID &limit=.
func (s *Server) listRefusals(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	return s.svc.ListRefusals(r.Context(), actorFrom(r), core.RefusalQuery{
		Project: v.Get("project"), Agent: v.Get("agent"), Days: queryInt(r, "days", 30), Session: int64(queryInt(r, "session", 0)),
		Before: int64(queryInt(r, "before", 0)), Limit: queryInt(r, "limit", 50),
	})
}

// allowRefusedCall adds an allow rule a person picked for a refused call to
// its session repo's .claude/settings.json by pull request (WP-233):
// {"rule": "Bash(make lint *)"}.
func (s *Server) allowRefusedCall(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Rule string `json:"rule"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.AllowRefusedCall(r.Context(), actorFrom(r), id, in.Rule)
}

// createRequest files a request. A repeat of a permission request within ten
// minutes answers with the earlier one (new: false).
func (s *Server) createRequest(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.RequestInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	req, created, err := s.svc.CreateRequest(r.Context(), actorFrom(r), in)
	if err != nil {
		return nil, err
	}
	return struct {
		*core.Request
		New bool `json:"new"`
	}{req, created}, nil
}

// getRequest reads one; ?wait=SECONDS holds an open request that long (at
// most 25) until it is decided. ?peek=1 only looks: an agent's answer read
// that way still counts as not yet told.
func (s *Server) getRequest(w http.ResponseWriter, r *http.Request) (any, error) {
	if queryBool(r, "peek") {
		return s.svc.PeekRequest(r.Context(), actorFrom(r), r.PathValue("ref"))
	}
	wait := time.Duration(min(max(queryInt(r, "wait", 0), 0), 25)) * time.Second
	if wait > 0 {
		waits(r)
	}
	return s.svc.GetRequest(r.Context(), actorFrom(r), r.PathValue("ref"), wait)
}

// watchRequest adds a condition that closes the request when it holds:
// {"kind": "pr-merged", "target": "owner/repo#15"}.
func (s *Server) watchRequest(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.WatchInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.AddRequestWatch(r.Context(), actorFrom(r), r.PathValue("ref"), in)
}

func (s *Server) decideRequest(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.RequestDecision
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.DecideRequest(r.Context(), actorFrom(r), r.PathValue("ref"), in)
}

// snoozeRequest hides a request until a time: {"until": RFC 3339}, or
// {"minutes": n}; with neither it comes back at once.
func (s *Server) snoozeRequest(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Until   *time.Time `json:"until"`
		Minutes int        `json:"minutes"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	until := in.Until
	if in.Minutes > 0 {
		t := time.Now().Add(time.Duration(in.Minutes) * time.Minute)
		until = &t
	}
	return s.svc.SnoozeRequest(r.Context(), actorFrom(r), r.PathValue("ref"), until)
}

// withdrawRequest takes back an open request: {"note": why} (optional) is
// kept for the person to read.
func (s *Server) withdrawRequest(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Note string `json:"note"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.WithdrawRequest(r.Context(), actorFrom(r), r.PathValue("ref"), in.Note)
}

func (s *Server) runRequestCell(w http.ResponseWriter, r *http.Request) (any, error) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		return nil, fmt.Errorf("%w: n must be a number", errBadRequest)
	}
	var in struct {
		Command string `json:"command"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.RunCell(r.Context(), actorFrom(r), r.PathValue("ref"), n, in.Command)
}

// requestLine is a request on one line, for lists.
func requestLine(r *core.Request) string {
	who := ""
	if r.Project != nil {
		who = " [" + r.Project.Key + "]"
	}
	return fmt.Sprintf("%s  %s  %s  %s%s", r.Ref, r.State, r.Kind, r.Title, who)
}

// requestText is a request for an agent to read.
func requestText(r *core.Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n%s · %s", r.Ref, r.Title, r.Kind, r.State)
	if r.DecidedBy != nil {
		fmt.Fprintf(&b, " by %s", r.DecidedBy.DisplayName)
	}
	b.WriteString("\n")
	if r.Answer != "" {
		b.WriteString("\nAnswer:\n" + r.Answer + "\n")
	}
	if r.Note != "" {
		b.WriteString("Note: " + r.Note + "\n")
	}
	switch r.State {
	case core.RequestApplied:
		switch {
		case r.Result.NoChange:
			fmt.Fprintf(&b, "Changed nothing: %s No commit or PR was made.\n", r.Result.Reason)
		case r.Result.PRURL != "":
			fmt.Fprintf(&b, "Opened %s on branch %s (not in your worktree).\n", r.Result.PRURL, r.Result.Branch)
		case r.Result.Commit != "":
			fmt.Fprintf(&b, "Applied in your worktree as commit %s.\n", r.Result.Commit)
		}
	case core.RequestFailed, core.RequestConflict:
		fmt.Fprintf(&b, "Not applied: %s\n", r.Error)
	}
	if r.Body != "" && r.State == core.RequestOpen {
		b.WriteString("\n" + r.Body + "\n")
	}
	for _, c := range r.Cells {
		if len(c.Runs) == 0 {
			continue
		}
		run := c.Runs[0]
		code := "?"
		if run.ExitCode != nil {
			code = fmt.Sprint(*run.ExitCode)
		}
		fmt.Fprintf(&b, "\n$ %s  (%s, exit %s)\n%s\n", run.Command, run.State, code, strings.TrimRight(run.Output, "\n"))
	}
	return b.String()
}

var requestTools = []mcpTool{
	{Name: "create_request", Title: "Ask a person",
		Description: "File a request for the person you work for, and keep working on something else. Use it when a hook, rule or the classifier blocks you, or you need a decision only they can make (instead of stopping to ask in the chat, where the question is lost). Kinds: question (free text or choices), change (file edits you were stopped making: pass files), command (commands they should run), action (a step only a person can do). The answer reaches you when you next stop, or read it with get_request. A decision that wakes you re-reads your whole context: set followup to never when you do not need to act on the answer, or on (with followup_on) to be woken only for some.",
		InputSchema: obj([]string{"kind", "title"}, map[string]any{
			"kind":  str("question, change, command, permission or action"),
			"title": str("The ask in one line"), "body": str("Details (markdown)"),
			"choices":     strList("question: options to pick from"),
			"multi":       map[string]any{"type": "boolean", "description": "question: more than one choice may be picked"},
			"files":       map[string]any{"type": "array", "description": "change: [{path, content, base_sha?, base?, delete?}]; base is the file's current text, base_sha its git blob (git hash-object); both empty for a new file", "items": map[string]any{"type": "object"}},
			"message":     str("change: the commit message"),
			"commands":    strList("command or action: shell commands, a cell each"),
			"dir":         str("command: the directory to run them in"),
			"project":     str("Project key; defaults to your session's, else the request is personal"),
			"issue":       str("Issue key it is about"),
			"closes_when": strList("Conditions that close the request by themselves when one holds, each KIND=TARGET: pr-merged=owner/repo#15 (or #15), issue-done=WP-12, url-ok=https://…"),
			"followup":    str("any (default: any decision wakes you), never (it waits for your next prompt; read it with get_request) or on (only the followup_on answers wake you)"),
			"followup_on": strList("followup on: the choices (or approved, denied, done, dismissed) that wake you"),
			"blocking":    map[string]any{"type": "boolean", "description": "true only when you can't go on with other work until it is answered: an answer then wakes you within minutes, instead of waiting to share a wake-up with your other news"}})},
	{Name: "list_requests", Title: "List requests", Annotations: readOnly,
		Description: "Your requests. state: open (default), answered (decided, you haven't been told), decided, snoozed or all.",
		InputSchema: obj(nil, map[string]any{"state": str("open, answered, decided, snoozed or all"), "limit": map[string]any{"type": "integer"}})},
	{Name: "get_request", Title: "Read a request", Annotations: readOnly,
		Description: "A request with its answer or outcome. Give its reference (R-12, or WP:R-12). Reading a decided request counts as being told.",
		InputSchema: obj([]string{"ref"}, map[string]any{"ref": str("R-12 or KEY:R-12")})},
}

func init() { mcpTools = append(mcpTools, requestTools...) }

// mcpRequests handles the request tools.
func (s *Server) mcpRequests(ctx context.Context, actor *core.Actor, name string, args mcpArgs) (string, bool, error) {
	svc := s.svc
	switch name {
	case "create_request":
		in := core.RequestInput{Kind: args.str("kind"), Title: args.str("title"), Body: args.str("body"), Project: args.str("project"), Issue: args.str("issue"),
			Message: args.str("message"), Followup: args.str("followup"), FollowupOn: args.list("followup_on"), Blocking: args["blocking"] == true, ClosesWhen: parseClosesWhen(args.list("closes_when"))}
		if choices := args.list("choices"); len(choices) > 0 || in.Kind == core.RequestQuestion {
			q := core.Question{Question: in.Title, Multi: args["multi"] == true}
			if in.Body != "" {
				q.Question = in.Title + "\n\n" + in.Body
			}
			for _, c := range choices {
				q.Choices = append(q.Choices, core.Choice{Label: c})
			}
			in.Questions = []core.Question{q}
		}
		for _, c := range args.list("commands") {
			in.Cells = append(in.Cells, core.CellInput{Command: c, Dir: args.str("dir")})
		}
		if raw, ok := args["files"].([]any); ok {
			for _, f := range raw {
				m, _ := f.(map[string]any)
				fi := core.RequestFileInput{Path: mcpArgs(m).str("path"), Content: mcpArgs(m).str("content"), BaseSHA: mcpArgs(m).str("base_sha"), Base: mcpArgs(m).str("base")}
				fi.Delete, _ = m["delete"].(bool)
				in.Files = append(in.Files, fi)
			}
		}
		req, _, err := svc.CreateRequest(ctx, actor, in)
		if err != nil {
			return "", true, err
		}
		return fmt.Sprintf("Filed %s. The decision reaches you when you next stop, or with your next prompt or session start if followup is never.", req.Ref), true, nil
	case "list_requests":
		page, err := svc.ListRequests(ctx, actor, core.RequestQuery{State: args.str("state"), Mine: true, Limit: args.int("limit", 50)})
		if err != nil {
			return "", true, err
		}
		if len(page.Requests) == 0 {
			return "No requests.", true, nil
		}
		var b strings.Builder
		for _, r := range page.Requests {
			b.WriteString(requestLine(r) + "\n")
		}
		return b.String(), true, nil
	case "get_request":
		req, err := svc.GetRequest(ctx, actor, args.str("ref"), 0)
		if err != nil {
			return "", true, err
		}
		return requestText(req), true, nil
	}
	return "", false, nil
}

// parseClosesWhen reads "kind=target" conditions; the service refuses a
// malformed one by name.
func parseClosesWhen(list []string) []core.WatchInput {
	var out []core.WatchInput
	for _, c := range list {
		kind, target, _ := strings.Cut(c, "=")
		out = append(out, core.WatchInput{Kind: kind, Target: target})
	}
	return out
}
