package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Runners and their jobs (WP-D17): `wp runner` registers, long polls for
// work and reports back; people list runners and jobs, queue a command and
// cancel.
func (s *Server) runnersRoutes(mux *http.ServeMux) {
	a := s.authed
	// The runner's side (the owner's own token).
	mux.HandleFunc("POST /api/v1/runner/register", a(s.runnerRegister))
	mux.HandleFunc("POST /api/v1/runner/poll", a(s.runnerPoll))
	mux.HandleFunc("POST /api/v1/runner/jobs/{id}/log", a(s.runnerJobLog))
	mux.HandleFunc("POST /api/v1/runner/jobs/{id}/finish", a(s.runnerJobFinish))
	mux.HandleFunc("POST /api/v1/runner/jobs/{id}/cancel", a(s.runnerJobCancel))
	// Background jobs on its machine that made no progress (WP-605).
	mux.HandleFunc("POST /api/v1/runner/stalled-jobs", a(s.runnerStalledJobs))
	// An audit the LLM gateway runs (WP-617): the runner hands over
	// its checkout, and Waypoint, which has the gateway's key, sends it on.
	mux.HandleFunc("POST /api/v1/runner/jobs/{id}/gateway", a(s.runnerGatewayStart))
	mux.HandleFunc("GET /api/v1/runner/jobs/{id}/gateway", a(s.runnerGatewayTask))
	mux.HandleFunc("POST /api/v1/runner/jobs/{id}/gateway/cancel", a(s.runnerGatewayCancel))
	// People.
	mux.HandleFunc("GET /api/v1/runners", a(s.listRunners))
	mux.HandleFunc("GET /api/v1/runners/{id}", a(s.getRunner))
	mux.HandleFunc("DELETE /api/v1/runners/{id}", a(s.deleteRunner))
	mux.HandleFunc("GET /api/v1/runner-jobs", a(s.listRunnerJobs))
	mux.HandleFunc("POST /api/v1/runner-jobs", a(s.enqueueRunnerJob))
	mux.HandleFunc("GET /api/v1/runner-jobs/{id}", a(s.getRunnerJob))
	mux.HandleFunc("POST /api/v1/runner-jobs/{id}/cancel", a(s.runnerJobCancel))
}

func (s *Server) runnerRegister(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.RunnerRegistration
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	run, err := s.svc.RegisterRunner(r.Context(), actorFrom(r), in)
	if err != nil {
		return nil, err
	}
	return struct {
		*core.Runner
		ServerVersion string `json:"server_version"`
	}{run, s.cfg.Version}, nil
}

// runnerPoll holds the request up to 30 seconds for work; a shutdown ends it.
func (s *Server) runnerPoll(w http.ResponseWriter, r *http.Request) (any, error) {
	waits(r)
	var in core.RunnerPoll
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		select {
		case <-s.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	work, err := s.svc.RunnerPoll(ctx, actorFrom(r), in)
	if err != nil {
		return nil, err
	}
	// The server's version: a runner on a release build that differs
	// updates itself to it, between jobs (wp upgrade).
	return struct {
		*core.RunnerWork
		ServerVersion string `json:"server_version"`
	}{work, s.cfg.Version}, nil
}

func (s *Server) runnerStalledJobs(w http.ResponseWriter, r *http.Request) (any, error) {
	var in core.StalledJobsReport
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	jobs, err := s.svc.ReportStalledJobs(r.Context(), actorFrom(r), in)
	if err != nil {
		return nil, err
	}
	return map[string]any{"jobs": jobs, "policy": core.StallPolicyVersion}, nil
}

func (s *Server) runnerJobLog(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in struct {
		Offset int64  `json:"offset"`
		Text   string `json:"text"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return s.svc.AppendRunnerLog(r.Context(), actorFrom(r), id, in.Offset, in.Text)
}

func (s *Server) runnerJobFinish(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var in core.RunnerFinish
	// An audit's result holds its helpers' runs: up to core.RunnerResultMax.
	if err := decodeBodyLarge(r, &in, core.RunnerFinishBodyMax, core.RunnerResultTooLarge); err != nil {
		return nil, err
	}
	state, err := s.svc.FinishRunnerJob(r.Context(), actorFrom(r), id, in)
	if err != nil {
		return nil, err
	}
	return map[string]string{"state": state}, nil
}

func (s *Server) runnerJobCancel(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.CancelRunnerJob(r.Context(), actorFrom(r), id)
}

func (s *Server) listRunners(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.svc.ListRunners(r.Context(), actorFrom(r), queryBool(r, "all"))
}

func (s *Server) getRunner(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.GetRunner(r.Context(), actorFrom(r), id)
}

func (s *Server) deleteRunner(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.DeleteRunner(r.Context(), actorFrom(r), id)
}

func (s *Server) listRunnerJobs(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	f := core.RunnerJobFilter{
		RunnerID: int64(queryInt(r, "runner", 0)),
		Source:   v.Get("source"),
		SourceID: int64(queryInt(r, "source_id", 0)),
		All:      queryBool(r, "all"),
		Limit:    queryInt(r, "limit", 50),
		Before:   int64(queryInt(r, "before", 0)),
	}
	for _, st := range strings.Split(v.Get("state"), ",") {
		if st = strings.TrimSpace(st); st != "" {
			f.States = append(f.States, st)
		}
	}
	return s.svc.ListRunnerJobs(r.Context(), actorFrom(r), f)
}

// getRunnerJob returns a job; ?log=1 includes its output, ?after=N only the
// output from byte N (what a page that has the first N bytes still lacks).
func (s *Server) getRunnerJob(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	from := int64(-1)
	if queryBool(r, "log") {
		from = 0
	}
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, &core.InvalidError{Msg: "after must be a byte offset"}
		}
		from = max(n, 0)
	}
	return s.svc.GetRunnerJob(r.Context(), actorFrom(r), id, from)
}

// enqueueRunnerJob is a person's click: run this on my runner. Agents and
// read-only tokens can't.
func (s *Server) enqueueRunnerJob(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		Kind     string          `json:"kind"`
		RunnerID int64           `json:"runner_id"`
		Label    string          `json:"label"`
		Timeout  int             `json:"timeout"`
		Input    json.RawMessage `json:"input"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	j, err := s.svc.EnqueueRunnerJob(r.Context(), actorFrom(r), core.RunnerJobInput{
		Kind: in.Kind, RunnerID: in.RunnerID, Label: in.Label, Input: in.Input, Timeout: in.Timeout, Source: "manual",
	})
	if errors.Is(err, core.ErrNoRunner) {
		return nil, &core.InvalidError{Msg: "you have no runner for that yet: connect one in Settings → Runners"}
	}
	if err != nil {
		return nil, err
	}
	return s.svc.GetRunnerJob(r.Context(), actorFrom(r), j.ID, -1)
}

// runnerGatewayStart takes a multipart body: the field "prompt" and the
// file "repo" (the job's checkout as a tar archive, gzipped or not), and
// answers the task the gateway started, or why it took none.
func (s *Server) runnerGatewayStart(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "multipart/form-data" {
		return nil, fmt.Errorf("%w: send multipart/form-data with a prompt field and a repo file", errBadRequest)
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.svc.GatewayTaskLimits(r.Context()).ArchiveBytes+core.GatewayPromptMax+1<<20)
	// #nosec G120 -- the body is capped by MaxBytesReader above
	if err := r.ParseMultipartForm(2 << 20); err != nil {
		return nil, fmt.Errorf("%w: %v", errBadRequest, err)
	}
	// What didn't fit in memory is in temporary files.
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	f, h, err := r.FormFile("repo")
	if err != nil {
		return nil, fmt.Errorf("%w: missing repo file", errBadRequest)
	}
	defer f.Close()
	return s.svc.StartGatewayTask(r.Context(), actorFrom(r), id, r.FormValue("prompt"), f, h.Size)
}

func (s *Server) runnerGatewayTask(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return s.svc.GatewayTask(r.Context(), actorFrom(r), id)
}

func (s *Server) runnerGatewayCancel(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return nil, s.svc.CancelGatewayTask(r.Context(), actorFrom(r), id)
}
