package core

// Audits the LLM gateway runs (WP-617, WP-ADR84), where it declares the
// tasks extension (GatewayTasks). An audit's chain may name gateway/<model>
// beside the CLIs: the gateway then runs an agent of
// its own (an agent task, POST /v1/tasks) over the whole checkout, on
// whichever of its models has room, so a review round's audit spends none
// of a coding plan and comes from a model that isn't the author's.
//
// The checkout is on the runner and the gateway's key is here, so the
// runner clones as for any audit, packs its work tree and hands it to
// Waypoint, which sends it on with the key (the relay below). The runner
// then asks Waypoint how the task goes until it ends. The key never reaches
// a runner.
//
// The gateway ranks the models of its audit purpose by what callers report
// of how each answer's findings fared, so each task is remembered
// (gateway_tasks) and a sweep reports its audit's findings as the session
// marks them: fixed or confirmed held up, rejected did not.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const (
	// GatewayCLI is the chain entry "gateway/<model>": no CLI on the
	// runner's machine, but the gateway's agent (FeatureGatewayAudit).
	GatewayCLI = "gateway"
	// gatewayAuditModel is the gateway's purpose for audits, whose models it
	// ranks by the reports below; gatewayFreeModel is every free model it has.
	gatewayAuditModel = "audit"
	gatewayFreeModel  = "free"

	// gatewayAuditTimeout is the runner job's time (seconds): a round waits
	// five hours for each of its audit's jobs (reviewAuditWait), so the job
	// ends itself before that. gatewayTaskRunMax is the most of it the
	// agent may run: the gateway's own hard cap, which
	// ends a task earlier only when it stalls, so a slow model that keeps
	// working isn't cut off. gatewayTaskSlack is what is kept for the clone,
	// the upload and the answer. The gateway doesn't count a task's time in
	// its queue, so the runner's own deadline bounds the whole.
	gatewayAuditTimeout = 15000
	gatewayTaskRunMax   = 14400
	gatewayTaskRunMin   = 60
	gatewayTaskSlack    = 120

	// GatewayTasksInFlight is how many of Waypoint's tasks the gateway runs
	// at once (its key's limits.tasks_in_flight, 6 since 2026-11-13); the
	// runners together take no more audits through it than that (WP-1064),
	// so none waits in the gateway's queue, where its time counts against
	// the runner's deadline and not against the gateway's cap.
	GatewayTasksInFlight = 6

	// GatewayArchiveMax is the gateway's limit for a repository's archive
	// and GatewayFileMax its limit for one file, as far as Waypoint knows
	// until the gateway says (its key's task_limits, GatewayTaskLimits):
	// these are the defaults for a gateway that doesn't.
	// One up to gatewayJSONArchiveMax goes inside the JSON body as base64,
	// which the gateway also caps at 20 MiB; a larger one as an upload.
	GatewayArchiveMax     = 20 << 20
	GatewayFileMax        = 5 << 20
	gatewayJSONArchiveMax = 14 << 20
	// GatewayPromptMax: an audit's prompt (200 KB at most) and the change's
	// diff the runner adds.
	GatewayPromptMax = 512 << 10
	// gatewayTasksPerJob bounds the tasks one job may start: its first and
	// the ones sent again after the gateway restarted under them.
	gatewayTasksPerJob = 4

	gatewayUploadTimeout = 10 * time.Minute
	gatewayCallTimeout   = 30 * time.Second
	// gatewayProgressTimeout bounds the look a job's page takes at its task.
	gatewayProgressTimeout = 5 * time.Second
	// gatewayBusyRetry is how long a runner waits before sending again
	// when the gateway said busy without saying how long.
	gatewayBusyRetry = 30 * time.Second
	// gatewayIntakeRetry is that wait after the gateway timed out reading
	// the task's body (HTTP 408).
	gatewayIntakeRetry = 2 * time.Minute
	// gatewayRefusedFor: a model the gateway refused this key outright
	// isn't asked for again this long, so an audit doesn't spend a clone
	// and an upload on learning it again.
	gatewayRefusedFor = time.Hour
)

// The task's states as a runner sees them: the gateway's own, and two of
// the relay's. Refused: the gateway took no task (ErrorKind says why, and
// RetryAfter when sending it again later may work). Unknown: the gateway
// couldn't be asked just now; ask again.
const (
	GatewayTaskSucceeded = "succeeded"
	GatewayTaskFailed    = "failed"
	GatewayTaskCancelled = "cancelled"
	GatewayTaskRefused   = "refused"
	GatewayTaskUnknown   = "unknown"
)

// gatewayNoModelAtBar is the gateway's code for a task no model at its
// quality bar can take.
const gatewayNoModelAtBar = "no_model_at_bar"

// gatewayTaskSpec is what Waypoint asks of a task, beyond the repository.
type gatewayTaskSpec struct {
	Model   string
	Prompt  string
	Schema  string // a JSON Schema the answer must match ("": plain text)
	Timeout int    // seconds the agent may run
	Exclude []string
	// Weakest is the task's quality bar (routing.weakest): no model the gateway rates below it for the purpose runs the
	// task. "": none.
	Weakest string
	// Priority and Paid are routing.priority and routing.paid ("": normal,
	// and a paid model only as the fallback); Arm labels the task for the
	// gateway's comparisons. Name is the answer schema's name.
	Priority, Paid, Arm, Name string
}

// gatewayTask is a task as the gateway describes it.
type gatewayTask struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	ServedBy string `json:"served_by"`
	Result   *struct {
		Text string          `json:"text"`
		JSON json.RawMessage `json:"json"`
	} `json:"result"`
	Error *struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	} `json:"error"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	CostUSD float64 `json:"cost_usd"`
	// Bar is the response's X-OpenLLMs-Bar header, when it had one (WP-1440).
	Bar string `json:"-"`
}

// gatewayTaskLLM is a gateway that runs agent tasks over an uploaded
// repository (the gateway: /v1/tasks).
type gatewayTaskLLM interface {
	// CreateTask sends the task with size bytes of archive (a tar file,
	// gzipped or not). A gateway that doesn't take it answers a
	// *gatewayRefusal.
	CreateTask(ctx context.Context, p *AIProvider, spec gatewayTaskSpec, archive io.Reader, size int64) (*gatewayTask, error)
	Task(ctx context.Context, p *AIProvider, id string) (*gatewayTask, error)
	CancelTask(ctx context.Context, p *AIProvider, id string) error
}

// gatewayRefusal is the gateway's answer to a task request it didn't take:
// the status, and the code and message of its error envelope.
type gatewayRefusal struct {
	status     int
	code, msg  string
	retryAfter time.Duration
}

func (e *gatewayRefusal) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.status, cmp(e.msg, cmp(e.code, "no reason given")))
}

var gatewayTaskIDRE = lazyre.New(`^[A-Za-z0-9_-]{1,100}$`)

// readerCloser is a request body whose Close stops what feeds it.
type readerCloser struct {
	io.Reader
	close func() error
}

func (r readerCloser) Close() error { return r.close() }

// CreateTask posts a read-only agent task with the repository. The archive
// is streamed, never held whole: inside the JSON as base64 while that fits
// the gateway's body limit, else as the repo part of a multipart upload.
func (c *httpLLM) CreateTask(ctx context.Context, p *AIProvider, spec gatewayTaskSpec, archive io.Reader, size int64) (*gatewayTask, error) {
	// Free models first; a paid one only when no free one can take it.
	routing := map[string]any{"paid": cmp(spec.Paid, "fallback")}
	task := map[string]any{"model": spec.Model, "prompt": spec.Prompt, "profile": "readonly", "timeout_seconds": spec.Timeout, "routing": routing}
	if spec.Weakest != "" {
		routing["weakest"] = spec.Weakest
	}
	if spec.Priority != "" {
		routing["priority"] = spec.Priority
	}
	if spec.Arm != "" {
		task["arm"] = spec.Arm
	}
	if len(spec.Exclude) > 0 {
		task["exclude"] = spec.Exclude
	}
	if spec.Schema != "" {
		task["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": cmp(spec.Name, "audit"), "schema": json.RawMessage(spec.Schema)}}
	}
	head, err := json.Marshal(task)
	if err != nil {
		return nil, err
	}
	archive = io.LimitReader(archive, size)
	if size > gatewayJSONArchiveMax {
		var pre bytes.Buffer
		mw := multipart.NewWriter(&pre)
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="task"`)
		h.Set("Content-Type", "application/json")
		w, err := mw.CreatePart(h)
		if err != nil {
			return nil, err
		}
		_, _ = w.Write(head)
		h = textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="repo"; filename="repo.tar.gz"`)
		h.Set("Content-Type", "application/gzip")
		if _, err := mw.CreatePart(h); err != nil {
			return nil, err
		}
		end := "\r\n--" + mw.Boundary() + "--\r\n"
		body := readerCloser{io.MultiReader(bytes.NewReader(pre.Bytes()), archive, strings.NewReader(end)), func() error { return nil }}
		return c.taskCall(ctx, p, http.MethodPost, "/tasks", mw.FormDataContentType(), body, int64(pre.Len())+size+int64(len(end)))
	}
	// {"model":…,"repo":{"archive_base64":"…"}}
	pre := append(head[:len(head)-1:len(head)-1], `,"repo":{"archive_base64":"`...)
	const end = `"}}`
	pr, pw := io.Pipe()
	go func() {
		enc := base64.NewEncoder(base64.StdEncoding, pw)
		_, err := io.Copy(enc, archive)
		if cerr := enc.Close(); err == nil {
			err = cerr
		}
		pw.CloseWithError(err)
	}()
	body := readerCloser{io.MultiReader(bytes.NewReader(pre), pr, strings.NewReader(end)), pr.Close}
	return c.taskCall(ctx, p, http.MethodPost, "/tasks", "application/json", body, int64(len(pre))+int64(base64.StdEncoding.EncodedLen(int(size)))+int64(len(end)))
}

// Task reads a task's state.
func (c *httpLLM) Task(ctx context.Context, p *AIProvider, id string) (*gatewayTask, error) {
	if !gatewayTaskIDRE.MatchString(id) {
		return nil, invalid("%q isn't a task id", id)
	}
	return c.taskCall(ctx, p, http.MethodGet, "/tasks/"+id, "", nil, 0)
}

// CancelTask stops a task; one that ended already is left as it is.
func (c *httpLLM) CancelTask(ctx context.Context, p *AIProvider, id string) error {
	if !gatewayTaskIDRE.MatchString(id) {
		return invalid("%q isn't a task id", id)
	}
	_, err := c.taskCall(ctx, p, http.MethodPost, "/tasks/"+id+"/cancel", "", nil, 0)
	return err
}

// taskCall does one task request, once: the caller decides about a retry,
// because a task request that was sent again could run twice. Any status
// but a success is a *gatewayRefusal read from the gateway's error
// envelope.
func (c *httpLLM) taskCall(ctx context.Context, p *AIProvider, method, path, contentType string, body io.ReadCloser, length int64) (*gatewayTask, error) {
	var rd io.Reader
	if body != nil {
		rd = body
		defer body.Close()
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(p.BaseURL, "/")+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", contentType)
		req.ContentLength = length
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := c.http.Do(req)
	usageSinkOf(ctx).sentOne(resp)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var env gatewayErrorEnvelope
		_ = json.Unmarshal(eb, &env)
		r := &gatewayRefusal{status: resp.StatusCode, code: gatewayWord(env.Error.Code), msg: excerpt(env.Error.Message, 300)}
		if r.code == "" && r.msg == "" {
			r.msg = excerpt(string(eb), 300)
		}
		if wait, ok := retryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
			r.retryAfter = wait
		}
		return nil, r
	}
	var t gatewayTask
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswer)).Decode(&t); err != nil {
		return nil, fmt.Errorf("read the gateway's task: %w", err)
	}
	t.Bar = excerpt(resp.Header.Get(barHeader), 300)
	return &t, nil
}

// gatewayErrorEnvelope is the gateway's error body.
type gatewayErrorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

// gatewayErrorCode is the code in a gateway error body, "" when it has none:
// the one reading of it, for tasks and chat requests alike.
func gatewayErrorCode(body string) string {
	var env gatewayErrorEnvelope
	_ = json.Unmarshal([]byte(body), &env)
	return gatewayWord(env.Error.Code)
}

// gatewayWord keeps a code or kind of the gateway's to a short word, as it
// goes into job results and is matched on.
func gatewayWord(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) > 40 {
		s = s[:40]
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return ""
		}
	}
	return s
}

// gatewayModelName keeps the name of the model that ran a task to the
// characters model names have: it is the gateway's text, and goes into the
// audit's record and a round's prompt.
func gatewayModelName(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if b.Len() >= 100 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("._:/@+-", r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ---- which models, for whose code ----

// gatewayFamilies maps the words a session's model name may hold to the
// globs that leave that model's family out at the gateway (its exclude
// takes globs whose * also crosses "/").
var gatewayFamilies = []struct {
	words []string
	globs []string
}{
	{[]string{"claude", "opus", "sonnet", "haiku", "fable", "mythos"}, []string{"*claude*"}},
	{[]string{"gpt", "codex"}, []string{"*gpt*", "*codex*"}},
	{[]string{"gemini"}, []string{"*gemini*"}},
	{[]string{"grok"}, []string{"*grok*"}},
	{[]string{"deepseek"}, []string{"*deepseek*"}},
	{[]string{"muse"}, []string{"*muse*"}},
}

// gatewayExcludeFor is the globs that keep the gateway from auditing code
// with the family of the model that wrote it; none for a model no family
// here knows, or when nobody says who wrote it.
func gatewayExcludeFor(author string) []string {
	author = strings.ToLower(author)
	var out []string
	for _, f := range gatewayFamilies {
		if slices.ContainsFunc(f.words, func(w string) bool { return strings.Contains(author, w) }) {
			out = append(out, f.globs...)
		}
	}
	return out
}

var gatewayGlobRE = lazyre.New(`^[A-Za-z0-9*][A-Za-z0-9._:/@+*-]{0,99}$`)

// gatewayWeakestRE is a model name as a task's quality bar takes it: an id
// or a loose name, never a pattern.
var gatewayWeakestRE = lazyre.New(`^[A-Za-z0-9][A-Za-z0-9._:/@+ -]{0,99}$`)

// gatewayArmRE: an arm label a task is compared under at the gateway.
var gatewayArmRE = lazyre.New(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// cleanGatewayExclude checks a job's exclude globs (the gateway takes 32).
func cleanGatewayExclude(globs []string) ([]string, error) {
	if len(globs) > 32 {
		return nil, invalid("at most 32 models can be excluded")
	}
	for _, g := range globs {
		if !gatewayGlobRE.MatchString(g) {
			return nil, invalid("%q isn't a model pattern", g)
		}
	}
	return globs, nil
}

// gatewayGlobMatch: does name match the glob, as the gateway reads one
// (letters' case aside, * is any run of characters, "/" included)?
func gatewayGlobMatch(glob, name string) bool {
	parts := strings.Split(strings.ToLower(glob), "*")
	name = strings.ToLower(name)
	if len(parts) == 1 {
		return name == parts[0]
	}
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(name, p)
		if i < 0 {
			return false
		}
		name = name[i+len(p):]
	}
	return strings.HasSuffix(name, last)
}

// GatewayLimits is what the gateway takes of a task's upload: the archive
// in all and one file in it.
type GatewayLimits struct {
	ArchiveBytes int64 `json:"archive_bytes"`
	FileBytes    int64 `json:"file_bytes"`
}

// gatewayLimitsTTL is how long the gateway's task limits are believed.
const gatewayLimitsTTL = 10 * time.Minute

// taskLimitsLLM is a gateway client that can read the key's task limits.
type taskLimitsLLM interface {
	// TaskLimits reads the upload limits the gateway's key response states
	// (task_limits): 0 for one it doesn't state.
	TaskLimits(ctx context.Context, p *AIProvider) (GatewayLimits, error)
}

type cachedGatewayLimits struct {
	at  time.Time
	lim GatewayLimits
}

// TaskLimits reads the gateway key's task_limits (GET /key).
func (c *httpLLM) TaskLimits(ctx context.Context, p *AIProvider) (GatewayLimits, error) {
	var out struct {
		TaskLimits struct {
			ArchiveBytes int64 `json:"archive_bytes"`
			FileBytes    int64 `json:"file_bytes"`
		} `json:"task_limits"`
	}
	if err := c.call(ctx, p, http.MethodGet, "/key", nil, &out); err != nil {
		return GatewayLimits{}, err
	}
	return GatewayLimits{ArchiveBytes: out.TaskLimits.ArchiveBytes, FileBytes: out.TaskLimits.FileBytes}, nil
}

// GatewayTaskLimits is what the gateway takes of an audit's upload now: the
// figures its key response states, kept ten minutes, or the defaults
// (GatewayArchiveMax, GatewayFileMax) where it states none, states one out of
// range (1 MiB to 512 MiB for the archive, 64 KiB to the archive for a file)
// or can't be asked.
func (s *Service) GatewayTaskLimits(ctx context.Context) GatewayLimits {
	def := GatewayLimits{ArchiveBytes: GatewayArchiveMax, FileBytes: GatewayFileMax}
	tl, p, why := s.gatewayTasksLLM(ctx)
	ll, ok := tl.(taskLimitsLLM)
	if why != "" || !ok {
		return def
	}
	if c, ok := s.gatewayLimits.Load(p.ID); ok && s.now().Sub(c.(cachedGatewayLimits).at) <= gatewayLimitsTTL {
		return c.(cachedGatewayLimits).lim
	}
	cctx, cancel := context.WithTimeout(ctx, gatewayCallTimeout)
	defer cancel()
	got, err := ll.TaskLimits(cctx, p)
	lim := def
	switch {
	case err != nil:
		s.log.Debug("read the gateway's task limits", "err", err)
	case got.ArchiveBytes >= 1<<20 && got.ArchiveBytes <= 512<<20:
		lim.ArchiveBytes = got.ArchiveBytes
		if got.FileBytes >= 64<<10 && got.FileBytes <= got.ArchiveBytes {
			lim.FileBytes = got.FileBytes
		} else {
			// The default file limit may be above a small archive limit.
			lim.FileBytes = min(GatewayFileMax, got.ArchiveBytes)
		}
	case got.ArchiveBytes == 0 && got.FileBytes >= 64<<10 && got.FileBytes <= GatewayArchiveMax:
		lim.FileBytes = got.FileBytes
	}
	s.gatewayLimits.Store(p.ID, cachedGatewayLimits{at: s.now(), lim: lim})
	return lim
}

// gatewayTasksLLM is the gateway and its provider, or why audits can't go
// through it.
func (s *Service) gatewayTasksLLM(ctx context.Context) (gatewayTaskLLM, *AIProvider, string) {
	tl, ok := s.LLM.(gatewayTaskLLM)
	pid := s.gatewayWith(ctx, GatewayTasks)
	if !ok || pid == 0 {
		return nil, nil, "no LLM gateway that runs agent tasks is set up on this Waypoint"
	}
	p, err := s.aiProvider(ctx, pid)
	if err != nil {
		return nil, nil, "no LLM gateway that runs agent tasks is set up on this Waypoint"
	}
	return tl, p, ""
}

// gatewayAuditWhy says why an audit can't be sent to the gateway's model
// just now ("" when it can): no gateway, or it refused this key the model
// within the hour.
func (s *Service) gatewayAuditWhy(ctx context.Context, model string) string {
	if _, _, why := s.gatewayTasksLLM(ctx); why != "" {
		return why
	}
	if v, ok := s.gatewayRefused.Load(model); ok {
		r := v.(gatewayRefusedModel)
		if s.now().Before(r.until) {
			return fmt.Sprintf("The gateway refused %s (%s); it is asked again after %s", model, r.why, r.until.UTC().Format("15:04 UTC"))
		}
		s.gatewayRefused.Delete(model)
	}
	return ""
}

// gatewaySkip says why an audit's chain passes over a gateway entry
// without sending anything ("" when it is sent), and the outcome that is:
// unavailable, or what the gateway's entry before it met this round. Busy
// and spent are the gateway's own state (its queue, its allowance), which
// its next model name would meet again after another clone and upload.
func (s *Service) gatewaySkip(ctx context.Context, a *CodeAudit, m AgentModel) (outcome, why string) {
	if why := s.gatewayAuditWhy(ctx, m.Model); why != "" {
		return "unavailable", why
	}
	for _, x := range a.Attempts {
		if x.Round == a.Retries && strings.HasPrefix(x.Model, GatewayCLI+"/") && (x.Outcome == "busy" || x.Outcome == "spent") {
			return x.Outcome, "The gateway was " + map[string]string{"busy": "busy", "spent": "out of allowance"}[x.Outcome] + " for " + x.Model + " just before"
		}
	}
	return "", ""
}

// gatewayRefusedModel is a model the gateway refused this key, until when
// it isn't asked for.
type gatewayRefusedModel struct {
	until time.Time
	why   string
}

// ---- the relay a runner speaks to ----

// GatewayTaskView is what a runner sees of its audit's task at the gateway.
type GatewayTaskView struct {
	ID     string `json:"id,omitempty"`
	Status string `json:"status"` // queued, running, succeeded, failed, cancelled; refused, unknown
	// Model is the provider/model that ran the task to its end.
	Model string          `json:"model,omitempty"`
	Text  string          `json:"text,omitempty"`
	JSON  json.RawMessage `json:"json,omitempty"`
	// ErrorKind is the gateway's kind for a failed task or its code for a
	// refused one (or the relay's: unreachable, excluded_model, no_gateway,
	// too_late, too_many), and Error its words.
	ErrorKind string `json:"error_kind,omitempty"`
	Error     string `json:"error,omitempty"`
	// RetryAfter (seconds): the same request may work after this long.
	RetryAfter   int     `json:"retry_after,omitempty"`
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

// gatewayJob is the audit job a runner speaks for: an audit's, or an
// agentic prep trial's (WP-250), whose task is issueID.
type gatewayJob struct {
	id, auditID, issueID int64
	in                   AgentJob
	left                 time.Duration // of the job's time
	running              bool
}

// gatewayJobFor reads a job a runner asks the gateway about: its owner's
// own, and an audit's or a prep trial's whose model is the gateway's.
func (s *Service) gatewayJobFor(ctx context.Context, actor *Actor, id int64) (*gatewayJob, error) {
	if err := runnerActor(actor); err != nil {
		return nil, err
	}
	var owner, sourceID, timeout int64
	var started sql.NullInt64
	var state, kind, input, source string
	err := s.db.R.QueryRowContext(ctx, `SELECT owner_id, state, kind, input, source, source_id, timeout, started_at FROM runner_jobs WHERE id = ?`, id).
		Scan(&owner, &state, &kind, &input, &source, &sourceID, &timeout, &started)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != actor.User.ID) {
		return nil, notFound("job")
	}
	if err != nil {
		return nil, err
	}
	j := &gatewayJob{id: id, running: state == "running"}
	switch source {
	case auditSource:
		j.auditID = sourceID
	case prepAgentSource:
		j.issueID = sourceID
	}
	if kind != JobAudit || sourceID == 0 || (j.auditID == 0 && j.issueID == 0) || json.Unmarshal([]byte(input), &j.in) != nil || j.in.CLI != GatewayCLI {
		return nil, invalid("job %d isn't an audit through the gateway", id)
	}
	if started.Valid {
		j.left = time.Duration(timeout)*time.Second - s.now().Sub(msTime(started.Int64))
	}
	return j, nil
}

// StartGatewayTask sends a running audit job's checkout (size bytes of tar
// archive) and prompt to the gateway as an agent task, with the gateway's
// key. What to run it on, the answer's form and the models left out are
// the job's own, as Waypoint queued it, not the runner's to say. A gateway
// that doesn't take the task is a view with status refused, not an error.
func (s *Service) StartGatewayTask(ctx context.Context, actor *Actor, jobID int64, prompt string, archive io.Reader, size int64) (*GatewayTaskView, error) {
	j, err := s.gatewayJobFor(ctx, actor, jobID)
	if err != nil {
		return nil, err
	}
	if !j.running {
		return nil, invalid("job %d isn't running", jobID)
	}
	if strings.TrimSpace(prompt) == "" || len(prompt) > GatewayPromptMax {
		return nil, invalid("the task needs a prompt (up to %d KB)", GatewayPromptMax>>10)
	}
	if lim := s.GatewayTaskLimits(ctx); size <= 0 || size > lim.ArchiveBytes {
		return nil, invalid("the checkout's archive is %s, and the gateway takes up to %s", humanBytes(size), humanBytes(lim.ArchiveBytes))
	}
	refused := func(kind, why string) (*GatewayTaskView, error) {
		return &GatewayTaskView{Status: GatewayTaskRefused, ErrorKind: kind, Error: why}, nil
	}
	tl, p, why := s.gatewayTasksLLM(ctx)
	if why != "" {
		return refused("no_gateway", why)
	}
	var sent int
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM gateway_tasks WHERE job_id = ?`, jobID).Scan(&sent); err != nil {
		return nil, err
	}
	if sent >= gatewayTasksPerJob {
		return refused("too_many", fmt.Sprintf("this job already started %d tasks", sent))
	}
	run := min(gatewayTaskRunMax, int(j.left.Seconds())-gatewayTaskSlack)
	if run < gatewayTaskRunMin {
		return refused("too_late", "too little of the job's time is left to run a task")
	}
	cctx, cancel := context.WithTimeout(ctx, gatewayUploadTimeout)
	defer cancel()
	spec := gatewayTaskSpec{Model: j.in.Model, Prompt: prompt, Schema: j.in.JSONSchema, Timeout: run, Exclude: j.in.GatewayExclude, Weakest: j.in.GatewayWeakest,
		Priority: j.in.GatewayPriority, Paid: j.in.GatewayPaid, Arm: j.in.GatewayArm}
	if j.issueID != 0 {
		spec.Name = "prep"
	}
	t, err := tl.CreateTask(cctx, p, spec, archive, size)
	if err != nil {
		v := gatewayRefusedView(err)
		var r *gatewayRefusal
		if errors.As(err, &r) && (r.status == http.StatusForbidden || r.status == http.StatusNotFound) {
			// The key may not use this model, or the gateway has none by the
			// name: the next audits skip it for a while.
			s.gatewayRefused.Store(j.in.Model, gatewayRefusedModel{until: s.now().Add(gatewayRefusedFor), why: excerpt(cmp(r.msg, v.ErrorKind), 120)})
		}
		s.log.Info("gateway audit: the task was not taken", "job", jobID, "model", j.in.Model, "kind", v.ErrorKind, "err", err)
		return v, nil
	}
	if !gatewayTaskIDRE.MatchString(t.ID) {
		return refused("server", "the gateway answered without a task id")
	}
	err = s.write(ctx, actor, func(tx *tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO gateway_tasks(task_id, audit_id, issue_id, job_id, model, served_by, created_at, bar) VALUES (?,?,?,?,?,?,?,?)`,
			t.ID, nullID(j.auditID), nullID(j.issueID), jobID, j.in.Model, gatewayModelName(t.ServedBy), s.nowMS(), t.Bar)
		return err
	})
	if err == nil && t.Bar != "" {
		s.log.Info("gateway audit: the task's bar", "job", jobID, "task", t.ID, "bar", t.Bar)
	}
	if err != nil {
		// Unrecorded, it could never be read or reported on: stop it.
		bg, stop := context.WithTimeout(context.WithoutCancel(ctx), gatewayCallTimeout)
		defer stop()
		_ = tl.CancelTask(bg, p, t.ID)
		return nil, err
	}
	return s.gatewayView(ctx, j, t), nil
}

// GatewayTask reads how a job's (latest) task goes at the gateway.
func (s *Service) GatewayTask(ctx context.Context, actor *Actor, jobID int64) (*GatewayTaskView, error) {
	j, err := s.gatewayJobFor(ctx, actor, jobID)
	if err != nil {
		return nil, err
	}
	id, err := s.gatewayJobTask(ctx, jobID)
	if err != nil {
		return nil, err
	}
	tl, p, why := s.gatewayTasksLLM(ctx)
	if why != "" {
		return &GatewayTaskView{ID: id, Status: GatewayTaskFailed, ErrorKind: "no_gateway", Error: why}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, gatewayCallTimeout)
	defer cancel()
	t, err := tl.Task(cctx, p, id)
	if err != nil {
		var r *gatewayRefusal
		if errors.As(err, &r) && r.status >= 400 && r.status < 500 && r.status != http.StatusTooManyRequests {
			// The gateway doesn't have it (any more), or won't show it.
			return &GatewayTaskView{ID: id, Status: GatewayTaskFailed, ErrorKind: cmp(r.code, "not_found"), Error: r.Error()}, nil
		}
		return &GatewayTaskView{ID: id, Status: GatewayTaskUnknown, ErrorKind: "unreachable", Error: excerpt(aiPublicError(err), 300), RetryAfter: int(gatewayBusyRetry.Seconds())}, nil
	}
	t.ID = id
	return s.gatewayView(ctx, j, t), nil
}

// gatewayProgress is how a running job's task goes at the gateway, for a
// person or agent asking whether a long audit still makes progress: its
// status and the tokens it has used so far. nil when the job has no task
// yet or there is no gateway; a gateway that doesn't answer in time leaves
// it unknown.
func (s *Service) gatewayProgress(ctx context.Context, jobID int64) *GatewayTaskView {
	id, err := s.gatewayJobTask(ctx, jobID)
	if err != nil {
		return nil
	}
	tl, p, why := s.gatewayTasksLLM(ctx)
	if why != "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, gatewayProgressTimeout)
	defer cancel()
	t, err := tl.Task(cctx, p, id)
	if err != nil {
		return &GatewayTaskView{ID: id, Status: GatewayTaskUnknown}
	}
	v := &GatewayTaskView{ID: id, Status: gatewayWord(t.Status), Model: gatewayModelName(t.ServedBy), CostUSD: t.CostUSD}
	if t.Usage != nil {
		v.InputTokens, v.OutputTokens = t.Usage.PromptTokens, t.Usage.CompletionTokens
	}
	return v
}

// CancelGatewayTask stops a job's task at the gateway: its runner was told
// to stop, or ran out of time. A gateway that can't be told is left; the
// task ends at its own timeout.
func (s *Service) CancelGatewayTask(ctx context.Context, actor *Actor, jobID int64) error {
	if _, err := s.gatewayJobFor(ctx, actor, jobID); err != nil {
		return err
	}
	id, err := s.gatewayJobTask(ctx, jobID)
	if err != nil {
		return err
	}
	tl, p, why := s.gatewayTasksLLM(ctx)
	if why != "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, gatewayCallTimeout)
	defer cancel()
	if err := tl.CancelTask(cctx, p, id); err != nil {
		s.log.Info("gateway audit: cancel a task", "job", jobID, "task", id, "err", err)
	}
	return nil
}

// gatewayJobTask is the id of the last task a job started.
func (s *Service) gatewayJobTask(ctx context.Context, jobID int64) (string, error) {
	var id string
	err := s.db.R.QueryRowContext(ctx, `SELECT task_id FROM gateway_tasks WHERE job_id = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`, jobID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", notFound("task")
	}
	return id, err
}

// gatewayView is a gateway's task as its runner sees it. A task that ended
// has the model that ran it recorded; one that succeeded on a model the
// job left out (a gateway that didn't honour the exclusion) is failed here,
// because the audit must not come from the author's own family.
func (s *Service) gatewayView(ctx context.Context, j *gatewayJob, t *gatewayTask) *GatewayTaskView {
	v := &GatewayTaskView{ID: t.ID, Status: gatewayWord(t.Status), Model: gatewayModelName(t.ServedBy), CostUSD: t.CostUSD}
	if t.Usage != nil {
		v.InputTokens, v.OutputTokens = t.Usage.PromptTokens, t.Usage.CompletionTokens
	}
	if t.Result != nil {
		v.Text, v.JSON = t.Result.Text, t.Result.JSON
		if string(v.JSON) == "null" {
			v.JSON = nil
		}
	}
	if t.Error != nil {
		v.ErrorKind, v.Error = gatewayWord(t.Error.Kind), excerpt(t.Error.Message, 600)
	}
	s.noteGatewayProgress(ctx, v)
	switch v.Status {
	case "queued", "running":
		v.Text, v.JSON = "", nil
		return v
	case GatewayTaskSucceeded:
		for _, g := range j.in.GatewayExclude {
			if gatewayGlobMatch(g, v.Model) {
				v.Status, v.ErrorKind = GatewayTaskFailed, "excluded_model"
				v.Error = fmt.Sprintf("The gateway ran the audit on %s, which the job left out (%s): it is the family of the model that wrote the code.", v.Model, g)
				v.Text, v.JSON = "", nil
				break
			}
		}
	case GatewayTaskFailed, GatewayTaskCancelled:
		if v.ErrorKind == "" {
			v.ErrorKind = cmp(map[string]string{GatewayTaskCancelled: "cancelled"}[v.Status], "server")
		}
		if v.Status == GatewayTaskFailed && v.ErrorKind == "cancelled" {
			// Failed "cancelled" is the gateway's sandbox stopping under a task
			// nobody called off, as a deploy restarting it does; a task
			// cancelled through its API ends in status cancelled instead.
			// Like the gateway's own restart it is safe to send again: the
			// runner resends it, and the chain goes again later (WP-994).
			v.ErrorKind = "interrupted"
			v.Error = "The gateway stopped the task's sandbox under it (" + cmp(v.Error, "the task was cancelled") + "); it is safe to send again"
		}
	default:
		// A state this Waypoint doesn't know: the runner asks again.
		v.Status, v.RetryAfter = GatewayTaskUnknown, int(gatewayBusyRetry.Seconds())
		return v
	}
	if v.Model != "" {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE gateway_tasks SET served_by = ? WHERE task_id = ? AND served_by != ?`, v.Model, v.ID, v.Model); err != nil {
			s.log.WarnContext(ctx, "gateway audit: record the task's model", "task", v.ID, "err", err)
		}
	}
	return v
}

// noteGatewayProgress keeps how a task goes as its runner just heard it:
// its status, its tokens and when they last grew, for /wp review (WP-562)
// to read without asking the gateway inside a hook. Bookkeeping only: a
// failed write loses one look.
func (s *Service) noteGatewayProgress(ctx context.Context, v *GatewayTaskView) {
	if v.ID == "" || v.Status == GatewayTaskUnknown || v.Status == GatewayTaskRefused {
		return
	}
	now := s.nowMS()
	in, out := int64(v.InputTokens), int64(v.OutputTokens)
	if _, err := s.db.W.ExecContext(ctx, `UPDATE gateway_tasks SET status = ?, polled_at = ?,
		grew_at = CASE WHEN ? + ? > input_tokens + output_tokens THEN ? ELSE grew_at END,
		input_tokens = max(input_tokens, ?), output_tokens = max(output_tokens, ?) WHERE task_id = ?`,
		v.Status, now, in, out, now, in, out, v.ID); err != nil {
		s.log.WarnContext(ctx, "gateway audit: note the task's progress", "task", v.ID, "err", err)
	}
}

// gatewayRefusedView reads why the gateway took no task. Busy answers
// carry how long to wait before sending it again.
func gatewayRefusedView(err error) *GatewayTaskView {
	v := &GatewayTaskView{Status: GatewayTaskRefused, Error: excerpt(aiPublicError(err), 300)}
	var r *gatewayRefusal
	if !errors.As(err, &r) {
		// It never answered: down, restarting, or the upload broke.
		v.ErrorKind, v.RetryAfter = "unreachable", int(gatewayBusyRetry.Seconds())
		return v
	}
	v.Error = r.Error()
	wait := int(max(r.retryAfter, gatewayBusyRetry).Seconds())
	switch {
	case r.code == gatewayNoModelAtBar:
		// No model at the task's quality bar can take it (WP-916). When
		// some are only busy or resting the gateway says how long; with
		// none at the bar at all it doesn't, and sending it again can't
		// help, so the audit's chain ends and its round goes on by its tier: an
		// elevated one asks again at the floor, another to its Claude reviewer.
		v.ErrorKind = gatewayNoModelAtBar
		if r.retryAfter > 0 {
			v.RetryAfter = wait
		}
	case r.status == http.StatusTooManyRequests && r.code == "quota":
		v.ErrorKind = "quota"
	case r.status == http.StatusTooManyRequests:
		v.ErrorKind, v.RetryAfter = cmp(r.code, "rate_limit"), wait
	case r.status >= 500:
		v.ErrorKind, v.RetryAfter = "unavailable", wait
	case r.status == http.StatusRequestTimeout:
		// The gateway's intake gave up on the body (WP-1078): it was too
		// slow to read it, which says nothing about any model. The runner
		// sends it again after a longer wait than a plain busy answer, so
		// an overloaded gateway gets it two or three times in the runner's
		// admit window, not every 30s.
		v.ErrorKind, v.RetryAfter = "intake_timeout", int(max(r.retryAfter, gatewayIntakeRetry).Seconds())
	case r.status == http.StatusUnauthorized:
		v.ErrorKind = "auth"
	case r.status == http.StatusForbidden:
		v.ErrorKind = cmp(r.code, "permission")
	case r.status == http.StatusNotFound:
		v.ErrorKind = cmp(r.code, "not_found")
	case r.status == http.StatusRequestEntityTooLarge:
		v.ErrorKind = cmp(r.code, "body_too_large")
	default:
		v.ErrorKind = cmp(r.code, "invalid_request")
	}
	return v
}

// gatewayOutcome sorts a gateway audit's failure by the kind its runner
// reported, as auditFailureKind sorts a CLI's by its words: spent (the
// gateway's allowance), busy (the chain may try it again later), unreadable
// (an answer not in the findings form), timeout, or model (another model
// may do). Never error: nothing here says the audit itself can't be done.
func gatewayOutcome(kind string) string {
	switch kind {
	case "quota":
		return "spent"
	case "rate_limit", "blocked", "unavailable", "queue_full", "unreachable", "interrupted", "intake_timeout":
		return "busy"
	case "invalid_output":
		return "unreadable"
	case "timeout", "too_late":
		return "timeout"
	}
	return "model"
}

// gatewayAnswered and gatewayAnsweredEmpty are gateway_tasks.answered: the
// task's answer is its audit's findings, and (empty) it raised none at all,
// before any severity floor or cap, so the sweep reports it as no_findings.
const (
	gatewayAnswered      = 1
	gatewayAnsweredEmpty = 2
)

// gatewayAuditAnswered records that a job's task gave its audit's findings
// (so the sweep below reports on it), raised being how many the answer
// had, and answers the audit's model name: gateway/ and the model that
// ran it.
func (s *Service) gatewayAuditAnswered(ctx context.Context, jobID int64, asked AgentModel, raised int) string {
	var id, served string
	err := s.db.R.QueryRowContext(ctx, `SELECT task_id, served_by FROM gateway_tasks WHERE job_id = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`, jobID).Scan(&id, &served)
	if err != nil {
		return auditModelName(asked)
	}
	answered := gatewayAnswered
	if raised == 0 {
		answered = gatewayAnsweredEmpty
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE gateway_tasks SET answered = ? WHERE task_id = ?`, answered, id); err != nil {
		s.log.WarnContext(ctx, "gateway audit: record the answer", "task", id, "err", err)
	}
	if served == "" {
		return auditModelName(asked)
	}
	return GatewayCLI + "/" + served
}

// ---- reports on how the findings fared ----

// reportAuditFeedback sends the gateway a report for each task whose
// audit's findings were marked since its last report (or that never had
// one): fixed or confirmed held up, rejected did not. A task whose answer
// raised nothing is reported once, as no_findings (WP-1230). The same
// rules as the review requests' reports (ReportReviewFeedback), which
// calls it.
func (s *Service) reportAuditFeedback(ctx context.Context, fl feedbackLLM, p *AIProvider) {
	now := s.now()
	rows, err := s.db.R.QueryContext(ctx, `
		SELECT g.task_id, g.created_at, g.tries, g.verified, g.false_positives, g.reported_at IS NOT NULL,
			COALESCE(SUM(CASE WHEN f.state IN ('fixed', 'confirmed') THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN f.state = 'rejected' THEN 1 ELSE 0 END), 0),
			g.answered = ? AND count(f.id) = 0
		FROM gateway_tasks g
		LEFT JOIN code_audit_findings f ON f.audit_id = g.audit_id
		WHERE g.answered IN (?, ?) AND g.created_at >= ? AND g.next_try_at <= ?
		GROUP BY g.task_id
		ORDER BY g.created_at`, gatewayAnsweredEmpty, gatewayAnswered, gatewayAnsweredEmpty, now.Add(-feedbackWindow).UnixMilli(), now.UnixMilli())
	if err != nil {
		s.log.WarnContext(ctx, "audit feedback: read the findings", "err", err)
		return
	}
	var due []reviewFeedbackDue
	for rows.Next() {
		var d reviewFeedbackDue
		if err := rows.Scan(&d.id, &d.createdAt, &d.tries, &d.sentVerified, &d.sentFP, &d.everReported, &d.verified, &d.falsePos, &d.noFindings); err != nil {
			rows.Close()
			s.log.WarnContext(ctx, "audit feedback: read the findings", "err", err)
			return
		}
		if !feedbackDue(d) {
			continue
		}
		if due = append(due, d); len(due) == feedbackBatch {
			break
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return
	}
	s.sendFeedback(ctx, fl, p, due, gatewayTaskReports)
}

var gatewayTaskReports = feedbackStore{
	took:   `UPDATE gateway_tasks SET verified = ?, false_positives = ?, reported_at = ?, tries = 0, next_try_at = 0, error = '' WHERE task_id = ?`,
	failed: `UPDATE gateway_tasks SET tries = tries + 1, next_try_at = ?, error = ? WHERE task_id = ?`,
}

// gatewayRunning counts the audits running through the gateway now. The
// runners run no more than GatewayTasksInFlight at once: another waits here,
// where a runner takes it the moment one ends (WP-1064), and a prep trial
// (WP-ADR104) waits unless more than prepAgentReserve places are free. Two
// runners claiming at once may start one more, which the gateway queues.
func (s *Service) gatewayRunning(ctx context.Context) (int, error) {
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM runner_jobs WHERE state = 'running' AND kind = ? AND cli = ?`, JobAudit, GatewayCLI).Scan(&n)
	return n, err
}
