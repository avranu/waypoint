package api

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Plain-text answers keep agent token use low; ?format=json asks for structured output.

//go:embed agent_help.md
var agentHelpText string

// agentHelp returns one "# topic" section of agent_help.md.
func agentHelp(topic string) (string, bool) {
	for _, sec := range strings.Split("\n"+agentHelpText, "\n# ")[1:] {
		name, body, _ := strings.Cut(sec, "\n")
		if strings.EqualFold(strings.TrimSpace(name), topic) {
			return strings.TrimSpace(body) + "\n", true
		}
	}
	return "", false
}

func textResponse(s string) rawResponse {
	return rawResponse{contentType: "text/plain; charset=utf-8", body: []byte(s)}
}

func wantsJSON(r *http.Request) bool { return r.URL.Query().Get("format") == "json" }

func (s *Server) agentIndex(w http.ResponseWriter, r *http.Request) (any, error) {
	t, _ := agentHelp("index")
	return textResponse(t), nil
}

func (s *Server) agentHelpTopic(w http.ResponseWriter, r *http.Request) (any, error) {
	t, ok := agentHelp(r.PathValue("topic"))
	if !ok {
		return nil, fmt.Errorf("%w: topics are next, batch, done, workflow, design, glossary, search, pages, provenance, usage, requests, cli", core.ErrNotFound)
	}
	return textResponse(t), nil
}

func (s *Server) agentNext(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.agentTake(w, r, false)
}

func (s *Server) agentBatch(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.agentTake(w, r, true)
}

func (s *Server) agentTake(w http.ResponseWriter, r *http.Request, batch bool) (any, error) {
	var q core.AgentQuery
	if r.Method == http.MethodGet {
		v := r.URL.Query()
		q = core.AgentQuery{Agent: v.Get("agent"), Project: v.Get("project"), Repo: v.Get("repo"),
			Step: v.Get("step"), Query: v.Get("query"), Peek: true, Delegated: queryBool(r, "delegated")}
	} else if err := decodeOptional(r, &q); err != nil {
		return nil, err
	}
	take := s.svc.AgentNext
	if batch {
		take = s.svc.AgentBatch
	}
	t, err := take(r.Context(), actorFrom(r), q)
	// A pause by a person is an answer to the agent, not an error.
	if p := (*core.ErrAgentsPaused)(nil); errors.As(err, &p) {
		if wantsJSON(r) {
			return map[string]any{"task": nil, "paused": p.Pause, "message": p.Error()}, nil
		}
		return textResponse(p.Error() + "\n"), nil
	}
	if err != nil {
		return nil, err
	}
	answered := s.svc.AnsweredRequests(r.Context(), actorFrom(r))
	warning := ""
	if !q.Peek && t != nil {
		s.svc.NoteCrossProject(r.Context(), actorFrom(r), q.CrossProject, t)
		warning = s.svc.TaskSessionWarning(r.Context(), actorFrom(r), t, sessionHint(r))
	}
	if wantsJSON(r) {
		if t == nil {
			return map[string]any{"task": nil, "answered_requests": answered}, nil
		}
		return map[string]any{"task": t.Issue, "workflow": t.Workflow, "step": t.Step, "related": t.Related, "resumed": t.Resumed, "skipped": t.Skipped, "design": t.Design, "why": t.Why, "comments": t.Comments, "named": t.Named,
			"resume": t.Resume, "answered_requests": answered, "warning": warning}, nil
	}
	note := ""
	if answered > 0 {
		note = fmt.Sprintf("\n%d answered request%s: wp request ls --answered\n", answered, map[bool]string{true: "", false: "s"}[answered == 1])
	}
	if t == nil {
		return textResponse("Nothing to do: no open task for an agent like you.\n" + note), nil
	}
	return textResponse(agentTaskText(t, s.taskTextFor(r, t, !q.Peek && s.onlyClaim(r, t))) + note + warningText(warning)), nil
}

// sessionHint reads what the CLI says about the session a take comes from
// (WP-34): its WP_TASK binding and its current context size. Headers, so an
// older server ignores them.
func sessionHint(r *http.Request) core.SessionHint {
	h := core.SessionHint{Task: strings.TrimSpace(r.Header.Get("X-Waypoint-Task"))}
	if len(h.Task) > 1000 {
		// Cut a long task hint at a separator, so no issue key is split in half.
		h.Task = h.Task[:1000]
		h.Task = h.Task[:max(0, strings.LastIndexAny(h.Task, ", \t"))]
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get("X-Waypoint-Context")), 10, 64); err == nil && n > 0 && n < 100_000_000 {
		h.ContextTokens = n
	}
	return h
}

// warningText puts a task-session warning after the task, where an agent
// reading the answer meets it last.
func warningText(w string) string {
	if w == "" {
		return ""
	}
	return "\n" + w + "\n"
}

// onlyClaim says whether the task handed out is the only one wp done
// without a KEY could mean: the CLI lists what this directory and session
// already took (X-Waypoint-Claims, "none" for nothing), and a batch takes
// several. Without the header (MCP, the API, an older wp) it's false. A
// task listed that is no longer held (finished on the board, given back)
// doesn't count: the CLI drops it too when wp done asks which.
func (s *Server) onlyClaim(r *http.Request, t *core.AgentTask) bool {
	h := strings.TrimSpace(r.Header.Get("X-Waypoint-Claims"))
	if h == "" || len(t.Related) > 0 {
		return false
	}
	keys := strings.Split(h, ",")
	if len(keys) > 20 {
		return false
	}
	for _, k := range keys {
		if k = strings.TrimSpace(k); k == "none" || strings.EqualFold(k, t.Issue.Key) {
			continue
		}
		if d, err := s.svc.GetIssue(r.Context(), actorFrom(r), k); err != nil || (d.ClaimedBy != "" && !d.Closed()) {
			return false
		}
	}
	return true
}

// taskText is how to word a task's answer.
type taskText struct {
	// Keyless shows the commands without the KEY, for the CLI that took the
	// task and remembers it as the only one (WP-242).
	Keyless bool
	// Assigned: autopilot handed this task to the session (WP-770).
	Assigned bool
	// StartedBy: the person who started a session for this task (wp start,
	// the queue's Start, a fan-out); the answer is its first prompt
	// (WP-1037).
	StartedBy string
	// ReviewFirst: the task's adversarial review comes before its pull
	// request, so wp ship is told to schedule it.
	ReviewFirst bool
}

// taskTextFor is how to word the answer to this request.
func (s *Server) taskTextFor(r *http.Request, t *core.AgentTask, keyless bool) taskText {
	o := taskText{Keyless: keyless, Assigned: r.URL.Query().Get("assigned") == "1",
		ReviewFirst: s.svc.ReviewsFirst(r.Context(), actorFrom(r), t.Issue.Key)}
	if a := actorFrom(r); r.URL.Query().Get("assigned") == "start" && a != nil && a.User != nil {
		o.StartedBy = cmpStr(a.User.DisplayName, a.User.Username)
	}
	return o
}

// agentTaskText is everything an agent needs to start, compactly.
func agentTaskText(t *core.AgentTask, o taskText) string {
	d := t.Issue
	var b strings.Builder
	for _, k := range t.Skipped {
		fmt.Fprintf(&b, "(Passed over %s.)\n", k)
	}
	// Priority, type, points and status don't change this text, so they are left out.
	switch {
	case o.Assigned:
		fmt.Fprintf(&b, "Autopilot assigned %s to you: %q %s\n", d.Key, d.Title, provenance(d, false))
	case o.StartedBy != "":
		fmt.Fprintf(&b, "%s started this session for %s: %q %s\n", o.StartedBy, d.Key, d.Title, provenance(d, false))
	default:
		if t.Resumed {
			b.WriteString("(You already hold this task.)\n")
		}
		fmt.Fprintf(&b, "%s %q %s\n", d.Key, d.Title, provenance(d, false))
	}
	if o.Assigned {
		// SessionStart can't tell autopilot sessions, so the note goes here; a session a person started is not told.
		b.WriteString(core.AutopilotSessionNote + "\n")
	}
	b.WriteString(writtenLine(d, true, time.Now()))
	b.WriteString(safetyLine(d))
	b.WriteString(riskLine(d, true))
	b.WriteString(goalLine(d))
	b.WriteString(crossLine(d))
	var meta []string
	switch {
	case t.Why != "" && t.Why != core.WhyBoardOrder:
		meta = append(meta, t.Why)
	case d.DueDate != nil:
		meta = append(meta, "due "+*d.DueDate)
	}
	if d.Category != "" {
		meta = append(meta, d.Category)
	}
	var tags []string
	for _, l := range d.Labels {
		tags = append(tags, l.Name)
	}
	if len(tags) > 0 {
		meta = append(meta, "tags: "+strings.Join(tags, ", "))
	}
	var repos []string
	for _, l := range d.Links {
		if l.Repo != "" && !contains(repos, l.Repo) {
			repos = append(repos, l.Repo)
		}
	}
	if len(repos) > 0 {
		meta = append(meta, "repo: "+strings.Join(repos, ", "))
	}
	if d.Parent != nil && t.ParentText == "" {
		meta = append(meta, fmt.Sprintf("part of: %s %s", d.Parent.Key, d.Parent.Title))
	}
	if len(meta) > 0 {
		b.WriteString(strings.Join(meta, " · ") + "\n")
	}
	b.WriteString(core.ObjectiveText(d.Objective, true, time.Now()))
	if t.Resume != nil {
		b.WriteString(t.Resume.ResumeText(d.Key) + "\n")
	}
	if n := len(d.Files); n > 0 {
		fmt.Fprintf(&b, "Attachments: %d — wp files %s saves them to view\n", n, d.Key)
	}
	own := 0
	if desc := strings.TrimSpace(d.Description); desc != "" {
		desc = core.ClipText(desc, 4000, "… (truncated; GET /api/v1/issues/"+d.Key+" for all)")
		own = len(desc)
		b.WriteString("\n" + desc + "\n")
	}
	comments := taskComments(d.Key, t.Comments)
	b.WriteString(comments)
	b.WriteString(parentSection(d, t.ParentText, own+len(comments)))
	if t.Step != nil {
		fmt.Fprintf(&b, "\nStep %d/%d of %s: %s\n", t.StepNo, len(t.Workflow.Steps), t.Workflow.Name, t.Step.Name)
		if t.Step.Instructions != "" {
			b.WriteString(t.Step.Instructions + "\n")
		}
	}
	if len(d.Children) > 0 {
		b.WriteString("\nSub-tasks:\n")
		for _, c := range d.Children {
			fmt.Fprintf(&b, "  %s %s [%s]\n", c.Key, c.Title, c.Status.Name)
		}
	}
	b.WriteString(relatedText(d.Relations, t.Named, time.Now()))
	if len(d.Pages) > 0 {
		var pages []string
		for _, p := range d.Pages {
			pages = append(pages, pageRefText(p))
		}
		b.WriteString("Pages: " + strings.Join(pages, "; ") + "\n")
	}
	if len(t.Design) > 0 {
		var notes []string
		for _, n := range t.Design {
			s := fmt.Sprintf("#%d %s [%s]", n.ID, n.Title, n.Kind)
			if !n.Approved {
				s += " (proposed)"
			}
			if n.Tension {
				s += " (in tension with this task: weigh both)"
			}
			notes = append(notes, s)
		}
		b.WriteString("Design: " + strings.Join(notes, "; ") + "\n")
	}
	b.WriteString(nearbyText(t.Nearby))
	if len(t.Related) > 0 {
		b.WriteString("\nAlso claimed for you:\n")
		for _, i := range t.Related {
			b.WriteString("  " + issueLine(i) + "\n")
		}
	}
	b.WriteString("\n" + taskClosing(d, o, time.Now()) + "\n")
	return b.String()
}

// A sub-task's answer carries its parent's description after its own text
// (WP-733), which comes first: the parent gets at most parentMax bytes, less
// when the task's description and comments already take most of
// taskTextRoom, and only a pointer when under parentMin is left.
const (
	parentMax    = 2000
	parentMin    = 300
	taskTextRoom = 6000
)

// parentSection is the parent's description under its own heading, ""
// with none. used is how much of taskTextRoom the task's own text took.
func parentSection(d *core.IssueDetail, text string, used int) string {
	if d.Parent == nil || text == "" {
		return ""
	}
	head := fmt.Sprintf("\nPart of %s %q:\n", d.Parent.Key, d.Parent.Title)
	room := min(parentMax, taskTextRoom-used)
	if room < parentMin {
		return head + "(Its description is in `wp show " + d.Parent.Key + "`.)\n"
	}
	return head + core.ClipText(text, room, "… (cut; `wp show "+d.Parent.Key+"` has the rest)") + "\n"
}

// How old a task has to be before its answer offers the way to decline it
// (WP-770). A well-defined, recent task doesn't need the invitation to
// question it.
const (
	declineReleases = 20
	declineAge      = 14 * 24 * time.Hour
)

// taskClosing is the paragraph that says what to do with a task. wp prints
// it last, finding it by its first sentence (cmd/wp taskClosing).
func taskClosing(d *core.IssueDetail, o taskText, now time.Time) string {
	key := " " + d.Key
	if o.Keyless {
		key = ""
	}
	ship := "wp ship" + key
	if o.ReviewFirst {
		ship += " --review"
	}
	s := fmt.Sprintf("Assess and complete the task as appropriate. When done, run `%s -m \"what you did\" --summary \"your reply to the person\"`; if it ships cleanly, that reply ends your turn (add `--check \"CMD\"` to run your checks first, and `--shot URL` for each new frontend change).", ship)
	if why := declineReasons(d, now); why != "" {
		s += fmt.Sprintf(" This task %s: if it's stale, poorly designed, superseded by recent work, or in conflict with other tasks or project goals, tell the user and decline it with `wp hold%s -m \"why\"`, which keeps it out of autopilot until someone lifts the hold.", why, key)
	}
	return s + fmt.Sprintf(" File questions with `wp ask \"question\"`, and if you need to give the task back, use `wp release%s -m \"why\"`.", key)
}

// declineReasons says why a task may not be worth doing as written, "" when
// nothing does.
func declineReasons(d *core.IssueDetail, now time.Time) string {
	var why []string
	if d.Written != nil && d.Written.Releases >= declineReleases {
		why = append(why, fmt.Sprintf("is %d releases old", d.Written.Releases))
	} else if old := now.Sub(d.CreatedAt); old >= declineAge {
		why = append(why, "is "+age(old)+" old")
	}
	if d.Unreviewed {
		why = append(why, "hasn't been reviewed by a person")
	}
	if d.Risk != nil && d.Risk.Level == core.RiskHigh {
		why = append(why, "is high risk")
	}
	return strings.Join(why, " and ")
}

// taskComments is the comments section of a task's answer, "" with none.
// It comes right after the description: a comment often corrects or narrows
// what the description asks (WP-449).
func taskComments(key string, cs []core.Comment) string {
	if len(cs) == 0 {
		return ""
	}
	return "\nComments, oldest first (they can change the task; read them):\n" + core.TaskCommentLines(key, cs)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Server) agentDone(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.AgentDoneInput
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	res, err := s.svc.AgentDone(r.Context(), actorFrom(r), r.PathValue("key"), in)
	if err != nil {
		return nil, err
	}
	if wantsJSON(r) {
		return res, nil
	}
	return textResponse(res.Message + "\n"), nil
}

func (s *Server) agentRelease(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.ReleaseInput
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	res, err := s.svc.AgentRelease(r.Context(), actorFrom(r), r.PathValue("key"), in)
	if err != nil {
		return nil, err
	}
	if wantsJSON(r) {
		return res, nil
	}
	return textResponse(res.Message + "\n"), nil
}

// agentPushed records the committed work a machine's wp pushed by itself
// when a session let go of a task, or why it couldn't (WP-843).
func (s *Server) agentPushed(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.AutoPushInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.RecordAutoPush(r.Context(), actorFrom(r), in)
}

// agentDecline closes a task filed from another repo that doesn't belong
// here, with the reason; the filing side hears it (WP-378).
func (s *Server) agentDecline(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Agent   string `json:"agent"`
		Comment string `json:"comment"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	i, err := s.svc.DeclineIssue(r.Context(), actorFrom(r), r.PathValue("key"), in.Agent, in.Comment)
	if err != nil {
		return nil, err
	}
	if wantsJSON(r) {
		return i, nil
	}
	return textResponse("Declined " + i.Key + "; the repo that filed it hears why.\n"), nil
}

func (s *Server) agentUsage(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Agent string `json:"agent"`
		core.AgentUsage
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if err := s.svc.AgentReportUsage(r.Context(), actorFrom(r), r.PathValue("key"), in.Agent, in.AgentUsage); err != nil {
		return nil, err
	}
	return textResponse("Recorded.\n"), nil
}

// agentPause and agentResume stop and restart the agent's clock on a task
// it holds; tools call them when the agent goes idle and gets back to work.
func (s *Server) agentPause(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.agentClock(r, s.svc.AgentPause, nil)
}

func (s *Server) agentResume(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.agentClock(r, s.svc.AgentResume, s.svc.AgentResumeOwn)
}

func (s *Server) agentClock(r *http.Request, f, own func(context.Context, *core.Actor, string, string) (string, error)) (any, error) {
	var in struct {
		Agent string `json:"agent"`
		// OnlyOwn: the Claude Code hook resumes the directory's task only
		// if this session was the one working on it (WP-720).
		OnlyOwn bool `json:"only_own"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	if in.OnlyOwn && own != nil {
		f = own
	}
	msg, err := f(r.Context(), actorFrom(r), r.PathValue("key"), in.Agent)
	if err != nil {
		return nil, err
	}
	return textResponse(msg + "\n"), nil
}

func (s *Server) listWorkflows(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListWorkflows(r.Context(), actorFrom(r), r.URL.Query().Get("project"))
}

func (s *Server) createWorkflow(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Project string `json:"project"`
		core.WorkflowInput
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreateWorkflow(r.Context(), actorFrom(r), in.Project, in.WorkflowInput)
}

func (s *Server) updateWorkflow(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.WorkflowInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.UpdateWorkflow(r.Context(), actorFrom(r), id, in)
}

func (s *Server) deleteWorkflow(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteWorkflow(r.Context(), actorFrom(r), id)
}

func (s *Server) listPages(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	q := core.PageQuery{Project: v.Get("project"), Category: v.Get("category"), Text: v.Get("q"), Issue: v.Get("issue"), Limit: queryInt(r, "limit", 200)}
	if p := v.Get("parent"); p != "" {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: parent must be a page id (0 for top level)", errBadRequest)
		}
		q.Parent = &n
	}
	pages, err := s.svc.ListPages(r.Context(), actorFrom(r), q)
	if err != nil || !wantsText(r) {
		return pages, err
	}
	return textResponse(pagesText(pages)), nil
}

// pageID is the page a path names: its number, or a code (PL13, APP-PL13;
// ?project= says where a bare code is).
func (s *Server) pageID(r *http.Request) (int64, error) {
	return s.svc.ResolvePage(r.Context(), actorFrom(r), r.PathValue("id"), r.URL.Query().Get("project"))
}

func (s *Server) getPage(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := s.pageID(r)
	if err != nil {
		return nil, err
	}
	pg, err := s.svc.GetPage(r.Context(), actorFrom(r), id)
	if err == nil && !queryBool(r, "listed") { // listed=1 marks a page wp brief listed unasked; that is not recorded as a lookup.
		s.svc.NoteLookup(r.Context(), actorFrom(r), core.KPage, id, "")
	}
	if err != nil || !wantsText(r) {
		return pg, err
	}
	return textResponse(pageText(pg)), nil
}

func (s *Server) createPage(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.PageInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.CreatePage(r.Context(), actorFrom(r), in)
}

func (s *Server) updatePage(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := s.pageID(r)
	if err != nil {
		return nil, err
	}
	var in core.PageInput
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.UpdatePage(r.Context(), actorFrom(r), id, in)
}

func (s *Server) deletePage(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := s.pageID(r)
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeletePage(r.Context(), actorFrom(r), id)
}

func (s *Server) linkPage(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := s.pageID(r)
	if err != nil {
		return nil, err
	}
	return nil, s.svc.LinkPage(r.Context(), actorFrom(r), id, r.PathValue("key"), r.Method == http.MethodPut)
}

// dropPageCode stops an old code leading to a page (DELETE /pages/{id}/codes/{code}).
func (s *Server) dropPageCode(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := s.pageID(r)
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DropPageCode(r.Context(), actorFrom(r), id, r.PathValue("code"))
}

func (s *Server) listTags(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListTags(r.Context(), actorFrom(r))
}

func (s *Server) suggestRelations(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.SuggestRelations(r.Context(), actorFrom(r), r.PathValue("key"))
}

// rewrittenWork names the tasks whose recent lines a change replaces (WP-782)
// and relates them to the task, inferred (WP-1370).
func (s *Server) rewrittenWork(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Repo string             `json:"repo"`
		Head string             `json:"head"`
		PRs  []core.RewrittenPR `json:"prs"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.NoteRewrites(r.Context(), actorFrom(r), r.PathValue("key"), in.Repo, in.Head, in.PRs)
}

func (s *Server) cliLoginStart(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Name  string `json:"name"`
		Agent string `json:"agent"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	if !s.limiter.allow("device:" + s.clientIP(r)) {
		return nil, errTooMany
	}
	return s.svc.StartDeviceLogin(r.Context(), in.Name, in.Agent, s.clientIP(r), r.UserAgent())
}

func (s *Server) cliLoginPoll(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		DeviceCode string `json:"device_code"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	tok, err := s.svc.PollDeviceLogin(r.Context(), in.DeviceCode)
	if err != nil {
		return nil, err
	}
	if tok == "" {
		return map[string]string{"status": "pending"}, nil
	}
	return map[string]string{"status": "approved", "token": tok}, nil
}

func (s *Server) cliLoginInfo(w http.ResponseWriter, r *http.Request) (any, error) {
	req, err := s.svc.DeviceLoginRequest(r.Context(), actorFrom(r), r.PathValue("code"))
	if err != nil {
		return nil, err
	}
	// The approval page warns when the login was started somewhere else: a
	// code someone sent you is how this flow gets phished.
	ip := s.clientIP(r)
	return struct {
		*core.DeviceRequest
		YourIP      string `json:"your_ip"`
		SameAddress bool   `json:"same_address"`
	}{req, ip, ip == req.IP}, nil
}

func (s *Server) cliLoginApprove(w http.ResponseWriter, r *http.Request) (any, error) {
	return nil, s.svc.ApproveDeviceLogin(r.Context(), actorFrom(r), r.PathValue("code"))
}

// agentHandoff picks and reserves the next task for a new task session (WP-97).
func (s *Server) agentHandoff(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		core.AgentQuery
		Batch bool `json:"batch"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	res, err := s.svc.Handoff(r.Context(), actorFrom(r), in.AgentQuery, in.Batch)
	if p := (*core.ErrAgentsPaused)(nil); errors.As(err, &p) {
		return map[string]any{"status": "paused", "message": p.Error()}, nil
	}
	return res, err
}

func (s *Server) agentHandoffRelease(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Keys []string `json:"keys"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, s.svc.ReleaseHandoff(r.Context(), actorFrom(r), in.Keys)
}

func (s *Server) agentHandoffOutcome(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Outcome string `json:"outcome"`
		Detail  string `json:"detail"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, s.svc.RecordHandoff(r.Context(), actorFrom(r), in.Outcome, in.Detail)
}
