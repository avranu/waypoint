package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Merge gates and shipping (WP-D21): checking a pull request, merging it or
// arming it to merge when ready, what people decide about findings, and the
// branches in work.
func (s *Server) shippingRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("GET /api/v1/projects/{key}/pulls/{number}", a(s.findPull))
	mux.HandleFunc("GET /api/v1/projects/{key}/pulls/{number}/timeline", a(s.pullTimeline))
	mux.HandleFunc("POST /api/v1/projects/{key}/pulls", a(s.openPull))
	mux.HandleFunc("POST /api/v1/projects/{key}/pulls/tasks", a(s.checkShipTasks))
	mux.HandleFunc("POST /api/v1/pulls/{id}/check", a(s.checkPull))
	mux.HandleFunc("POST /api/v1/pulls/{id}/rerun", a(s.rerunPull))
	mux.HandleFunc("GET /api/v1/pulls/{id}/readiness", a(s.pullReadiness))
	mux.HandleFunc("GET /api/v1/pulls/{id}/failed-checks", a(s.failedChecks))
	mux.HandleFunc("GET /api/v1/pulls/{id}/logs", a(s.pullLogs))
	mux.HandleFunc("GET /api/v1/pulls/{id}/wait", a(s.waitPull))
	mux.HandleFunc("POST /api/v1/pulls/{id}/merge", a(s.mergePull))
	mux.HandleFunc("DELETE /api/v1/pulls/{id}/merge", a(s.disarmPull))
	mux.HandleFunc("PATCH /api/v1/pulls/{id}", a(s.editPull))
	mux.HandleFunc("POST /api/v1/pulls/{id}/urgent", a(s.urgentPull))
	mux.HandleFunc("DELETE /api/v1/pulls/{id}/urgent", a(s.urgentPull))
	mux.HandleFunc("POST /api/v1/pulls/{id}/after/{number}", a(s.mergeAfterPull))
	mux.HandleFunc("DELETE /api/v1/pulls/{id}/after/{number}", a(s.mergeAfterPull))
	mux.HandleFunc("POST /api/v1/findings/{id}/dismiss", a(s.dismissFinding))
	mux.HandleFunc("POST /api/v1/findings/{id}/reopen", a(s.reopenFinding))
	mux.HandleFunc("GET /api/v1/branches", a(s.listBranches))
	mux.HandleFunc("GET /api/v1/branches/tidy", a(s.remoteTidy))
}

// findPull is a pull request by its number within a project's repos.
func (s *Server) findPull(w http.ResponseWriter, r *http.Request) (any, error) {
	n, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		return nil, fmt.Errorf("%w: number must be a number", errBadRequest)
	}
	actor := actorFrom(r)
	id, err := s.svc.FindPullRequest(r.Context(), actor, r.PathValue("key"), r.URL.Query().Get("repo"), n)
	if err != nil {
		return nil, err
	}
	return s.svc.GetPullRequest(r.Context(), actor, id)
}

// pullTimeline is wp pr timeline: one pull request's ship-to-merge timeline.
func (s *Server) pullTimeline(w http.ResponseWriter, r *http.Request) (any, error) {
	n, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		return nil, fmt.Errorf("%w: number must be a number", errBadRequest)
	}
	return s.svc.PRTimeline(r.Context(), actorFrom(r), r.PathValue("key"), r.URL.Query().Get("repo"), n)
}

// openPullBody is the request of wp pr open and wp ship.
type openPullBody struct {
	Repo  string `json:"repo"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Draft bool   `json:"draft"`
	Ready bool   `json:"ready"`
	Issue string `json:"issue"`
	For   string `json:"for"` // the task wp ship finishes (WP-608)
	// Also: the other tasks the same change does (wp ship A B, WP-1426).
	Also   []string `json:"also"`
	Urgent bool     `json:"urgent"`
	// Review: wp ship's intent about the task's review before the pull
	// request (auto, review, pr, skip; WP-688), with what goes with it.
	Review   string `json:"review"`
	Reason   string `json:"reason"`
	Force    bool   `json:"force"`
	HeadTime int64  `json:"head_time"`
	HeadSHA  string `json:"head_sha"`
	// Message and Summary are left out by a CLI that wasn't given them:
	// absent keeps what the round holds, "" clears it (WP-1370).
	Message *string           `json:"message"`
	Summary *string           `json:"summary"`
	Change  *core.ChangeFacts `json:"change"`
	// BodyInputs: what wp ship rendered the description from (WP-55).
	BodyInputs *core.PRBodyInputs `json:"body_inputs"`
	// StackedOn: the branches head was built on, nearest first (WP-1670).
	StackedOn []string `json:"stacked_on"`
}

// openPull opens a pull request from a branch already pushed (wp pr open).
func (s *Server) openPull(w http.ResponseWriter, r *http.Request) (any, error) {
	var in openPullBody
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	res, err := s.svc.OpenPullRequest(r.Context(), actorFrom(r), core.OpenPRInput{Project: r.PathValue("key"), Repo: in.Repo, Head: in.Head,
		Base: in.Base, Title: in.Title, Body: in.Body, Draft: in.Draft, Ready: in.Ready, Issue: in.Issue, For: in.For, Also: in.Also, Urgent: in.Urgent,
		Review: in.Review, Reason: in.Reason, Force: in.Force, HeadTime: in.HeadTime, HeadSHA: in.HeadSHA, Message: in.Message, Summary: in.Summary, Change: in.Change, BodyInputs: in.BodyInputs, StackedOn: in.StackedOn})
	if err != nil {
		return nil, err
	}
	if res.Held != "" {
		return res, nil
	}
	res.ShipReply = s.svc.ShipReplyTemplate(r.Context(), actorFrom(r))
	if in.Issue != "" {
		res.PutOff = s.putOffAfterOpen(r.Context(), actorFrom(r), in.Issue, res.Reviewed)
	}
	return res, nil
}

// checkShipTasks refuses, as the open would, tasks that can't share one pull
// request; wp ship A B asks it before it commits (WP-1426).
func (s *Server) checkShipTasks(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Keys []string `json:"keys"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if err := s.svc.CheckShipTasks(r.Context(), actorFrom(r), r.PathValue("key"), in.Keys); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, nil
}

// putOffAfterOpen is what the open says about the task's put-off steps. After
// a reviewed round the completeness check had the notes as leads (WP-1370),
// so only the open subtasks, which no check reads, stay; a direct open keeps
// both. Best effort: a task that can't be skimmed just shows nothing.
func (s *Server) putOffAfterOpen(ctx context.Context, actor *core.Actor, issue string, reviewed bool) *core.PutOffSteps {
	p, err := s.svc.PutOffSteps(ctx, actor, issue)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrForbidden) {
		s.log.WarnContext(ctx, "read the steps put off in a task", "issue", issue, "err", err)
	}
	if p == nil || !reviewed {
		return p
	}
	p.Notes, p.More = nil, 0
	if len(p.Open) == 0 {
		return nil
	}
	return p
}

func (s *Server) checkPull(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.CheckPullRequest(r.Context(), actorFrom(r), id)
}

// rerunPull re-runs the failed jobs of the run that counts for each workflow
// on the head (one count for all), or with ?job= only that one, without a
// push (WP-1661).
func (s *Server) rerunPull(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	job := strings.TrimSpace(r.URL.Query().Get("job"))
	if job == "" && r.URL.Query().Has("job") { // not "every failed job"
		return nil, &core.InvalidError{Msg: "job is blank: give a failed job's name, or leave it out to re-run every failed job"}
	}
	return s.svc.RerunPullRequest(r.Context(), actorFrom(r), id, job)
}

func (s *Server) pullReadiness(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.PRReadiness(r.Context(), actorFrom(r), id)
}

func (s *Server) failedChecks(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.FailedChecks(r.Context(), actorFrom(r), id)
}

// pullLogs is the failing part of each failed check's log (?check=NAME for
// one check, ?full=1 for all of it) (WP-132).
func (s *Server) pullLogs(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.PRCheckLogs(r.Context(), actorFrom(r), id, r.URL.Query().Get("check"), queryBool(r, "full"))
}

// waitPull holds until the pull request settles or ?timeout=SECONDS (at
// most 300) pass; ?head=SHA waits for that head first (WP-132).
func (s *Server) waitPull(w http.ResponseWriter, r *http.Request) (any, error) {
	waits(r)
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(queryInt(r, "timeout", 0)) * time.Second
	return s.svc.WaitPullRequest(r.Context(), actorFrom(r), id, core.PRWaitInput{Timeout: timeout, Head: r.URL.Query().Get("head")})
}

func (s *Server) mergePull(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Style        string `json:"style"`
		DeleteBranch *bool  `json:"delete_branch"`
		Title        string `json:"title"`
		Message      string `json:"message"`
		WhenReady    bool   `json:"when_ready"`
		Head         string `json:"head"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.MergePR(r.Context(), actorFrom(r), id, core.MergeInput{Style: in.Style, DeleteBranch: in.DeleteBranch,
		Title: in.Title, Message: in.Message, WhenReady: in.WhenReady, Head: in.Head})
}

func (s *Server) editPull(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
		Base  *string `json:"base"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, s.svc.EditPullRequest(r.Context(), actorFrom(r), id, core.EditPRInput{Title: in.Title, Body: in.Body, Base: in.Base})
}

// urgentPull puts a pull request's CI on the urgent lane (POST) or takes it
// off (DELETE): the ci-urgent label (WP-528).
func (s *Server) urgentPull(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.SetPRUrgent(r.Context(), actorFrom(r), id, r.Method == http.MethodPost)
}

// mergeAfterPull sets a pull request to merge only after pull request
// {number} (of ?repo=, else its own repo) has (POST), or drops that
// (DELETE) (WP-912).
func (s *Server) mergeAfterPull(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || n <= 0 {
		return nil, fmt.Errorf("%w: number must be a pull request number", errBadRequest)
	}
	return s.svc.SetPRMergeAfter(r.Context(), actorFrom(r), id, r.URL.Query().Get("repo"), n, r.Method == http.MethodPost)
}

func (s *Server) disarmPull(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DisarmMerge(r.Context(), actorFrom(r), id)
}

func (s *Server) dismissFinding(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Note string `json:"note"`
	}
	if err := decodeOptional(r, &in); err != nil {
		return nil, err
	}
	return s.svc.DismissFinding(r.Context(), actorFrom(r), id, in.Note)
}

func (s *Server) reopenFinding(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.ReopenFinding(r.Context(), actorFrom(r), id)
}

func (s *Server) listBranches(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListBranches(r.Context(), actorFrom(r), r.URL.Query().Get("project"))
}

// remoteTidy lists the merged branches the forge sweep would delete and the
// stale ones it reports (WP-140).
func (s *Server) remoteTidy(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	return s.svc.RemoteTidy(r.Context(), actorFrom(r), q.Get("project"), q.Get("repo"))
}
