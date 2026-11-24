// Package core is Waypoint's domain: users, projects, issues, sprints, time
// tracking, automations, notifications and reports, stored in SQLite.
//
// Every operation takes the acting user (an Actor) and checks access itself,
// so the HTTP API, the MCP server and automations all go through the same
// rules. Mutations record Events in the same transaction; once the
// transaction commits the events are dispatched to automations,
// notifications, outbound webhooks and live-update subscribers.
package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"git.jmann.me/claude/waypoint/internal/core/meaning"
	"git.jmann.me/claude/waypoint/internal/db"
	"git.jmann.me/claude/waypoint/internal/tracing"
)

// Config holds settings the domain needs from the environment.
type Config struct {
	// BaseURL is the externally visible URL, used in links sent elsewhere
	// (notifications, webhooks, calendar feeds, invite links).
	BaseURL string
	// BackupDir, if set, receives a daily copy of the database; the newest
	// BackupKeep copies are kept.
	BackupDir  string
	BackupKeep int
	// TranscriptDir holds the transcript archive's compressed pieces
	// (WAYPOINT_TRANSCRIPT_DIR, WP-1466); empty keeps no archive.
	TranscriptDir string
	// TranscriptMaxMB caps the archive's stored bytes (0: no cap); an upload
	// that would pass it is refused (ErrTranscriptQuota).
	TranscriptMaxMB int
	// MaxAttachmentMB bounds one attached file (WAYPOINT_MAX_ATTACHMENT_MB);
	// 0 means DefaultMaxAttachmentMB.
	MaxAttachmentMB int
	// Egress limits where webhooks, Gotify and automation calls may go.
	Egress EgressPolicy
	// Logs keeps the recent log records for `wp server log` (WP-375); nil
	// keeps none.
	Logs *LogRing
	// Version is the running build's, for `wp server status`.
	Version string
	// Assistant is the site's defaults for an assistant's gate, for the
	// assistants that leave their own empty (WP-459).
	Assistant AssistantDefaults
	// MeaningURL is the meaning service's (WAYPOINT_AI_MEANING_URL): the
	// embedding and NLI models the goal judgements score with (WP-1035).
	// Empty: they go without. MeaningToken, when set, is sent as a bearer
	// token.
	MeaningURL, MeaningToken string
}

// Service is the domain API.
type Service struct {
	db       *db.DB
	cfg      Config
	log      *slog.Logger
	http     *http.Client
	pushHTTP *http.Client
	Hub      *Hub

	now func() time.Time

	afterTranscriptFile func()

	transcriptFilesChecked atomic.Bool

	removeAll func(string) error

	diskSpace func(dir string) (free, total int64, err error)

	started time.Time
	jobs    *jobLedger

	feedbackBusy atomic.Bool

	rnd             func() float64
	beta            func(a, b float64) float64
	trustSeeds      []string
	againTrustFault error
	healthReadFault func(projectID int64) error
	healthMu        sync.Mutex

	tracesLanded func(issueIDs []int64)

	beforeRoundWrite func(ctx context.Context)

	failFanoutItems func() error

	baseUpdated     sync.Map
	gatewayRefused  sync.Map
	staleChecksRead sync.Map

	baseMerged     sync.Map
	baseRed        sync.Map
	rerunRefusedAt sync.Map
	stackOpt       sync.Map
	stackCommits   sync.Map
	poolSat        sync.Map
	poolSatReading atomic.Bool
	workLogReading atomic.Bool
	prBodyWriting  atomic.Bool

	contains sync.Map

	bg sync.WaitGroup

	assistants assistantClients

	// forge API access, swappable in tests
	Forges ForgeClient
	// language models for AI tasks, swappable in tests
	LLM LLM
	// Meaning is the meaning service (package meaning); nil without one.
	Meaning       meaning.Models
	aiSlots       chan struct{}
	surplusSlot   chan struct{}
	aiMu          sync.Mutex
	aib           aiBudget
	aiQueued      map[int64]bool
	relating      int
	titling       int
	storying      map[int64]bool
	prepping      int
	prepAhead     map[int64]prepAheadMark
	trialing      bool
	prepPolMu     sync.Mutex
	prepPol       *prepPolicy
	prepPolStale  bool
	delegPolMu    sync.Mutex
	delegPolComp  sync.Mutex
	delegPol      *delegPolicy
	delegPolBusy  bool
	delegPolRetry time.Time
	delegPolErr   error
	goalPol       goalCtxCache
	screening     map[int64]bool
	drilling      bool
	driftChecks   map[int64]bool

	botKick         chan struct{}
	botMu           sync.Mutex
	botCancels      map[int64]context.CancelFunc
	ownDraftsMu     sync.Mutex
	ownDrafts       map[string]int
	tidyCache       sync.Map
	goneRepos       sync.Map
	defaultBranches sync.Map
	botLogins       sync.Map
	foreignStatuses sync.Map
	foreignAnswers  sync.Map

	runners   runnerState
	prWait    prWaiters
	autoRuns  autoRuns
	countMu   sync.Mutex
	dashFixMu sync.Mutex

	terms   termsCache
	similar similarCache
	graph   graphCache
	meaning conceptState
	ranker  rankerState
	goals   goalState

	sessCache        sessionCache
	stopWaits        stopWaits
	shipWaits        shipWaits
	babysitLocks     keyedLocks
	shipLocks        keyedLocks
	branchLocks      namedLocks
	objectiveLocks   keyedLocks
	objectiveWatched atomic.Int64
	heldChecks       sync.Map
	auditLocks       keyedLocks
	ciBoard          ciBoardCache
	runnerFaults     runnerFaultLog
	ciLimits         workflowLimitCache
	envs             *envRunner

	bgCtx    context.Context
	bgCancel context.CancelFunc
}

// New returns a Service backed by d.
func New(d *db.DB, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	s := &Service{
		db:   d,
		cfg:  cfg,
		log:  log,
		http: cfg.Egress.client(15 * time.Second),
		Hub:  NewHub(),
		now:  time.Now,
	}
	s.started = s.now()
	s.jobs = newJobLedger(cfg.Logs, func() time.Time { return s.now() })
	s.log = slog.New(&sourceHandler{ledger: s.jobs, next: log.Handler()})
	forgeEgress := cfg.Egress
	forgeEgress.AllowLoopback = true
	s.Forges = &httpForgeClient{http: forgeEgress.client(forgeTimeout), patient: forgeEgress.client(forgePatientTimeout)}
	s.LLM = &httpLLM{http: forgeEgress.streamClient(llmHeaderTimeout)}
	if strings.TrimSpace(cfg.MeaningURL) != "" {
		s.Meaning = meaning.NewHTTP(cfg.MeaningURL, cfg.MeaningToken, forgeEgress.client(meaning.Timeout), s.log, func() time.Time { return s.now() })
	}
	s.envs = newEnvRunner(cfg.Egress)
	s.botKick = make(chan struct{}, 1)
	s.botCancels = map[int64]context.CancelFunc{}
	s.aiSlots = make(chan struct{}, 2)
	s.surplusSlot = make(chan struct{}, 1)
	s.aiQueued = map[int64]bool{}
	s.storying = map[int64]bool{}
	s.screening = map[int64]bool{}
	s.driftChecks = map[int64]bool{}
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	s.Hub.observe = s.onGoalEvent
	pushEgress := cfg.Egress
	pushEgress.BlockPrivate = true
	s.pushHTTP = pushEgress.client(15 * time.Second)
	return s
}

// SetClock replaces the clock; for tests.
func (s *Service) SetClock(f func() time.Time) { s.now = f }

// Close stops queued background work (AI estimates); in-flight deliveries
// still finish, so call Wait after it.
func (s *Service) Close() {
	s.jobs.stop()
	s.bgCancel()
}

// Wait blocks until background deliveries finish.
func (s *Service) Wait() { s.bg.Wait() }

// Ping checks the database can answer, for health checks.
func (s *Service) Ping(ctx context.Context) error {
	var one int
	return s.db.R.QueryRowContext(ctx, `SELECT 1`).Scan(&one)
}

// BaseURL is the configured external URL without a trailing slash.
func (s *Service) BaseURL() string { return strings.TrimRight(s.cfg.BaseURL, "/") }

func (s *Service) nowMS() int64 { return s.now().UnixMilli() }

func (s *Service) goBG(f func()) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		f()
	}()
}

// Actor is who is performing an operation.
type Actor struct {
	User *User
	// ReadOnly is set for API tokens created read-only.
	ReadOnly bool
	// Permissions is what a permission token may do (WP-111); nil for
	// people in the browser and unrestricted tokens.
	Permissions PermissionSet
	// TokenID is the API token behind the request, if any.
	TokenID int64
	// RunnerJob is the runner job whose own token (WP-418) is behind the
	// request, if any.
	RunnerJob int64
	scope     map[int64]int
	// Agent is the acting LLM's identity ("anthropic/claude-opus-5-5"), from
	// an agent token or the X-Waypoint-Agent header; empty for people.
	Agent string
	// MCP is set for requests over the MCP endpoint, which only agents use.
	MCP bool
	// AgentEnv names the coding agent whose shell the request came from
	// (the CLI's X-Waypoint-Agent-Env header), even when the caller said it
	// is a person (WAYPOINT_AGENT=none): whatever it reads lands in the
	// agent's context, so it never sees drafts (WP-42).
	AgentEnv string
	// Prestart: a launcher takes a task for a task session it is about to
	// start (the CLI's X-Waypoint-Prestart header, WP-1037). No person drives
	// that session, so the take counts as unattended.
	Prestart bool
	// Automation is set when an automation performs the change on behalf of
	// User (the person whose action triggered it).
	Automation *Automation
	depth      int
	session    string
	// Session is the agent session (WP-D16) the request came from, set from
	// the X-Waypoint-Session header: what the actor does is recorded on it.
	Session int64
	// ClientVersion is the wp that made the request, from its User-Agent
	// (WP-256); "" for anything else.
	ClientVersion string
	budget        *int
	forgeSync     bool
	textAgent     string
}

func (a *Actor) id() int64 {
	if a == nil || a.User == nil {
		return 0
	}
	return a.User.ID
}

// IsAgent reports whether an LLM agent is acting (WP-D10): it has an
// agent identity or came in over MCP. Everyone else counts as a person.
// A permission token always counts as an agent (WP-111).
func (a *Actor) IsAgent() bool { return a != nil && (a.Agent != "" || a.MCP || a.Permissions != nil) }

// HidesDrafts reports whether drafts (WP-42) are hidden from the actor:
// they are for people, so agents, anything run in an agent's shell or
// session, and automations never see them, wherever they look.
func (a *Actor) HidesDrafts() bool {
	return a == nil || a.User == nil || a.IsAgent() || a.AgentEnv != "" || a.Session != 0 || a.Automation != nil
}

// HidesCommandText reports whether the text of commands agents ran is kept
// from the actor: the same readers as drafts, as whatever it reads lands in
// an agent's context. They still see that a command was refused or risky,
// and its name.
func (a *Actor) HidesCommandText() bool { return a.HidesDrafts() }

// draftsHidden is a SQL condition that leaves out drafts in table alias
// t when the actor mustn't see them ("1" otherwise).
func draftsHidden(actor *Actor, t string) string {
	if actor.HidesDrafts() {
		return t + ".draft = 0"
	}
	return "1"
}

// agentName is the agent's identity for provenance ("mcp" when an MCP
// client didn't say); empty for people.
func (a *Actor) agentName() string {
	switch {
	case a == nil:
		return ""
	case a.Agent != "":
		return a.Agent
	case a.MCP:
		return "mcp"
	}
	return ""
}

// requireHuman refuses agents: approving, locking and deleting intent is
// for people.
func (a *Actor) requireHuman(what string) error {
	if a.IsAgent() {
		return forbidden("%s is for people, not agents", what)
	}
	return nil
}

// requirePersonDirect is requireHuman for what an agent mustn't do even
// with its person's token (WP-ADR9): the request comes from the web or the
// person's own terminal, not from a coding agent's shell, an agent session
// or an automation.
func (a *Actor) requirePersonDirect(what string) error {
	if err := a.requireHuman(what); err != nil {
		return err
	}
	if a.AgentEnv != "" || a.Session != 0 || a.Automation != nil {
		return forbidden("%s is for people, in the web or their own terminal, not in an agent's shell", what)
	}
	return nil
}

// actsAsPerson: the actor is a person acting directly (requirePersonDirect
// would let them through). Only such an act counts as a person's intent
// in the trace to goals (WP-ADR50); anything else counts as an agent's.
func (a *Actor) actsAsPerson() bool {
	return a != nil && a.User != nil && a.requirePersonDirect("") == nil
}

// writesAsAgent: what the actor writes isn't a person's own act (an agent's
// token, or a person's token in an agent's shell, session or automation), so
// it never approves itself: a design note it writes is a proposal, or counts
// on a person's task, and its edits are applied or pending (WP-ADR96).
func (a *Actor) writesAsAgent() bool { return !a.actsAsPerson() }

// creditAgent is the agent an act is credited to in provenance: the
// agent's own name, or for a person's account acting through an agent's
// shell, session or an automation, a stand-in, so it never reads as the
// person's own act. "" only for a person acting directly (or no user).
func (a *Actor) creditAgent() string {
	if n := a.agentName(); n != "" {
		return n
	}
	switch {
	case a == nil || a.User == nil || a.actsAsPerson():
		return ""
	case a.Automation != nil:
		return "automation"
	}
	return "agent-shell"
}

// isAdmin: a job's scoped token is never an admin, whoever's it is.
func (a *Actor) isAdmin() bool { return a != nil && a.User != nil && a.User.IsAdmin && a.scope == nil }

func (a *Actor) viaAutomation() any {
	if a == nil || a.Automation == nil || a.Automation.ID <= 0 {
		return nil
	}
	return a.Automation.ID
}

func (a *Actor) canWrite() error {
	if a == nil || a.User == nil {
		return ErrUnauthorized
	}
	if a.ReadOnly {
		return fmt.Errorf("read-only token: %w", ErrForbidden)
	}
	return nil
}

// SystemActor acts with full access, for CLI bootstrap and background jobs.
func SystemActor() *Actor {
	return &Actor{User: &User{ID: 0, Username: "system", DisplayName: "Waypoint", IsAdmin: true}}
}

// tx wraps a write transaction and collects events to dispatch on commit.
type tx struct {
	*sql.Tx
	s       *Service
	actor   *Actor
	ctx     context.Context
	events  []*Event
	pending []func()
}

type queryer interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// write runs f in a write transaction and dispatches its events afterwards.
func (s *Service) write(ctx context.Context, actor *Actor, f func(t *tx) error) error {
	if actor != nil && actor.ReadOnly {
		return fmt.Errorf("read-only token: %w", ErrForbidden)
	}
	var span trace.Span
	if tracing.Enabled() {
		ctx, span = tracing.Start(ctx, "db.write "+callerName(2))
		defer span.End()
	}
	queued := time.Now()
	sqlTx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		if span != nil {
			tracing.Fail(span, "begin")
		}
		return err
	}
	if span != nil {
		span.SetAttributes(attribute.Int64("db.wait_ms", time.Since(queued).Milliseconds()))
	}
	t := &tx{Tx: sqlTx, s: s, actor: actor, ctx: ctx}
	defer func() {
		if p := recover(); p != nil {
			_ = sqlTx.Rollback()
			panic(p)
		}
	}()
	if err := f(t); err != nil {
		_ = sqlTx.Rollback()
		if span != nil {
			tracing.Fail(span, errorKind(err))
		}
		return err
	}
	if err := sqlTx.Commit(); err != nil {
		if span != nil {
			tracing.Fail(span, "commit")
		}
		return err
	}
	s.dispatch(ctx, actor, t.events)
	return nil
}

// callerName is the Service method skip frames up ("CreateIssue"), for
// span names.
func callerName(skip int) string {
	pc, _, _, ok := runtime.Caller(skip)
	if !ok {
		return "unknown"
	}
	f := runtime.FuncForPC(pc)
	if f == nil {
		return "unknown"
	}
	name := f.Name()
	name = name[strings.LastIndex(name, "/")+1:]
	name = strings.TrimPrefix(name, "core.")
	name = strings.TrimPrefix(name, "(*Service).")
	name, _, _ = strings.Cut(name, ".")
	return name
}

// errorKind sorts an error into a category safe to put in a trace.
func errorKind(err error) string {
	var inv *InvalidError
	switch {
	case errors.As(err, &inv):
		return "invalid"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrUnauthorized):
		return "forbidden"
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	}
	return "error"
}

// queryIDs runs a query returning one integer column.
func queryIDs(ctx context.Context, q queryer, query string, args ...any) ([]int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// nullUser is a user id column value; the system actor (id 0) is NULL.
func nullUser(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func nullInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullStr(v *string) any {
	if v == nil || *v == "" {
		return nil
	}
	return *v
}

func nullFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func ptrInt(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func ptrStr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}

func ptrFloat(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}

func ptrTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.UnixMilli(n.Int64).UTC()
	return &t
}

func msTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// placeholders returns "?,?,?" for n values.
func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func int64Args(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// Opt is a JSON field that distinguishes "absent" from "null": Set is true
// when the field was present, and Value is nil when it was null.
type Opt[T any] struct {
	Set   bool
	Value *T
}

func (o *Opt[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var v T
	if err := jsonUnmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

// Some returns a set Opt holding v.
func Some[T any](v T) Opt[T] { return Opt[T]{Set: true, Value: &v} }

// Null returns a set Opt holding null.
func Null[T any]() Opt[T] { return Opt[T]{Set: true} }

// Log returns the service's logger.
func (s *Service) Log() *slog.Logger { return s.log }
