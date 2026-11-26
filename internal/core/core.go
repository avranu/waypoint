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
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"git.jmann.me/claude/waypoint/internal/core/email"
	"git.jmann.me/claude/waypoint/internal/core/kernel"
	"git.jmann.me/claude/waypoint/internal/core/meaning"
	"git.jmann.me/claude/waypoint/internal/db"
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
// emailService is the email domain's operations; Service embeds it, so their
// methods are the facade's.
type emailService = email.Service

type Service struct {
	*emailService

	db       *db.DB
	cfg      Config
	log      *slog.Logger
	http     *http.Client
	pushHTTP *http.Client
	Hub      *Hub

	k      *kernel.Kernel
	mailer *email.Mailer // no access checks: automation sends through it

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
	gatewayLimits   sync.Map
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
	cool          coolings
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
	coverLooked      sync.Map
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
		k:    kernel.New(d, log),
	}
	s.mailer = email.NewMailer(s.k)
	s.emailService = email.New(s.mailer)
	s.k.Persist(persistedKinds...)
	s.k.OnEmit(noteSession)
	s.k.OnEmit(stampEvent)
	s.k.AfterCommit = s.dispatch
	s.subscribe()
	s.started = s.now()
	s.jobs = newJobLedger(cfg.Logs, func() time.Time { return s.now() })
	s.log = slog.New(&sourceHandler{ledger: s.jobs, next: log.Handler()})
	s.k.Log = s.log
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
func (s *Service) SetClock(f func() time.Time) { s.k.SetClock(f) }

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

// now is the time by the service's clock (the kernel's).
func (s *Service) now() time.Time { return s.k.Now() }

func (s *Service) nowMS() int64 { return s.k.NowMS() }

func (s *Service) goBG(f func()) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		f()
	}()
}

// draftsHidden is a SQL condition that leaves out drafts in table alias
// t when the actor mustn't see them ("1" otherwise).
func draftsHidden(actor *Actor, t string) string {
	if actor.HidesDrafts() {
		return t + ".draft = 0"
	}
	return "1"
}

// tx wraps a write transaction and collects events to dispatch on commit; the
// transaction itself is the kernel's.
type tx struct {
	*kernel.Tx
	s *Service
}

type queryer interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// write runs f in a write transaction and dispatches its events afterwards.
func (s *Service) write(ctx context.Context, actor *Actor, f func(t *tx) error) error {
	return s.k.WriteSkip(ctx, actor, 1, func(kt *kernel.Tx) error { return f(&tx{Tx: kt, s: s}) })
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
