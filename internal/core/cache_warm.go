package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Warm prompt caches (WP-192, WP-ADR34). A Claude Code session's prompt
// cache lives an hour after its last model call. A session used again after
// that writes its whole context to the cache again, at 2x the input price,
// where a warm one reads it at 0.1x. So when a session is likely to be used
// again soon, Waypoint posts it a keep-warm ping a few minutes before its
// cache cools: one short turn that reads the cache and so keeps it another
// hour. Likely means: it waits on its own background work or babysits (for
// a few hours at most), it waits on open requests while its person is
// active, or its person was working with it and is still active. Otherwise
// the cache cools; and when a session that will likely be used again
// (it waits on work or requests) is too big to keep warm any longer, it is
// compacted first, so the cold start later writes a summary, not the lot.
// Each decision is on the session's timeline and its Cache field.
//
// WP-534: a person may have Waypoint compact instead of pinging
// (UserPrefs.CacheWarm "compact"), and a session whose context is already at
// the context limit is always compacted instead: the summary's cache is the
// one written fresh, and nothing pings it after. A session whose last Stop
// said it can be cleared (the ship reply's last line) is left alone: nothing
// more is expected of it (WP-ADR52).
//
// WP-644: a ping is the exception, compaction the rule. A session is pinged
// at most once per wait, and only while its context is small; a second
// keep-warm decision, or a bigger context, compacts it instead. A session
// autopilot runs (no person has typed into it since autopilot's prompt) is
// kept warm only while its own work runs (background tasks, wakeups,
// babysits): nobody is expected to answer it or come back to it, so its
// requests and its person's activity count for nothing (WP-ADR61).
const (
	// KeepWarmMark starts every keep-warm ping, so the hooks know it from a
	// prompt: it is no turn of the session's work.
	KeepWarmMark = "Waypoint keep-warm"
	// keepWarmBody is the ping. A runner that predates if_idle may post it
	// to a busy session, which reads it between tool calls: it must not stop
	// real work. It is typed into the session's pane (MessageJob.Type), so
	// it says where it came from itself.
	keepWarmBody = KeepWarmMark + ` ping: nothing has changed. If you are idle, reply with only "ok" and end your turn; run no tools. If you are in the middle of work, ignore this and carry on. This came from another Claude session.`

	// warmPingAt: how long after a stop the decision is made, and
	// warmPingLate the last moment it is still worth making (the runner
	// takes a few seconds, the turn a few more).
	warmPingAt   = cacheWarm - 5*time.Minute
	warmPingLate = cacheWarm - 90*time.Second
	// How long after its last real turn a session is kept warm, by why:
	// waiting on its own work (a command of under a few hours), on requests
	// while its person is active, or on its person's next prompt.
	warmWorkFor     = 3 * time.Hour
	warmRequestsFor = 8 * time.Hour
	warmChatFor     = 2 * time.Hour
	// personActiveWithin: a person is active when they used the web or typed
	// into a session this recently (the web session's last-seen moves every
	// 10 minutes at most).
	personActiveWithin = 20 * time.Minute
	// compactAbove is the context limit unless the kit sets a lower
	// compact_window: a session that will likely be used again but cools is
	// compacted first when its context is at least this big, and one that
	// would be kept warm is compacted instead (WP-534). Compacting reads the
	// context once (0.1x) and writes a summary; resuming it cold rewrites the
	// whole context (2x).
	compactAbove = 200_000
	// pingBelow: a session whose context is at least this big is compacted
	// rather than pinged, and unattendedPingBelow the same for a session
	// autopilot runs (WP-644). A ping reads the whole context at 0.1x each
	// hour; a compaction reads it once and leaves a summary.
	pingBelow           = 100_000
	unattendedPingBelow = 40_000
	// unmergedCompactFrom: a session whose task isn't merged yet is compacted
	// before its cache cools from this much context (WP-1306). Below it a
	// compaction can't pay even if every such session is used again:
	// compactFrom(1) at Opus prices (read 0.1, 1-hour write 2, output 5) is
	// (2·36k + 5·8k) / (2 − 0.1) ≈ 59k. Above it, the class's evidence
	// decides once there is enough (weighWarm).
	unmergedCompactFrom = 60_000
	// unmergedWithin: how recently the session must have worked on the task
	// for its being unmerged to count (a pull request waits days at most).
	unmergedWithin = 3 * 24 * time.Hour
	// postedEcho: a prompt this soon after Waypoint posted to the session's
	// inbox is that post, not a person typing.
	postedEcho = 3 * time.Minute

	// cacheWarmSource is the runner job that pinged or compacted a session.
	cacheWarmSource = "cache_warm"
)

// Warm states: what was decided for a session's latest stop.
const (
	warmNone       = ""
	warmPinged     = "warm"
	warmInPing     = "in_ping"
	warmCool       = "cool"
	warmCompacting = "compacting"
	warmCompacted  = "compacted"
	warmFailed     = "failed"
	// warmReview: autopilot is compacting the session before its
	// adversarial review (WP-285): the compaction is quiet, and no ping
	// comes until the review's prompt starts a turn.
	warmReview = "review"
)

// SessionCache is what Waypoint knows and decided about a session's prompt
// cache (WP-192).
type SessionCache struct {
	TTL string `json:"ttl,omitempty"` // "1h" or "5m": the tier its last calls asked for
	// Decision for its latest stop: "warm" (pinged to keep it warm), "cool"
	// (left to cool), "compacting", "compacted", "failed"; "" not yet.
	Decision string     `json:"decision,omitempty"`
	Note     string     `json:"note,omitempty"`
	Pings    int        `json:"pings"` // since its last real turn
	At       *time.Time `json:"at,omitempty"`
}

// isKeepWarmPing: the prompt is a keep-warm ping.
func isKeepWarmPing(prompt string) bool {
	return strings.HasPrefix(strings.TrimSpace(prompt), KeepWarmMark)
}

// warmFacts is what the keep-warm decision looks at.
type warmFacts struct {
	Now, WarmSince time.Time
	Kind           string // SessionYours, SessionTask
	Finished       bool
	Pending        string // the last Stop's: background work, "?", "requests" or ""
	Babysits       int    // moving babysits
	Requests       int    // open requests
	PersonActive   bool
	TypedLast      bool // its person typed the prompt of its last turn
	ContextTokens  int64
	Paused         bool // its person paused its sessions (WP-146)
	// LeftAlone: its person typed /wp stop in it (WP-562).
	LeftAlone bool
	// Limit is the context limit (compactAbove, or the kit's lower
	// compact_window); 0 means compactAbove.
	Limit int64
	// CompactInstead: its person has Waypoint compact instead of pinging
	// (WP-534).
	CompactInstead bool
	// Clearable: its last real Stop said the session can be cleared.
	Clearable bool
	// Pings: keep-warm pings since its last real turn.
	Pings int
	// Unattended: autopilot runs it, and no person has typed into it since
	// autopilot's prompt (WP-644).
	Unattended bool
	// Unmerged: a task it worked on isn't merged or closed yet, and no
	// other session has taken it since (WP-1306).
	Unmerged bool
}

// warmChoice is what to do with a session's cache, and why (for its
// timeline: "kept warm: waiting on a background task").
type warmChoice struct {
	Action string // warmPinged, warmCool, warmCompacting
	Why    string
	// Warm: it is to be kept warm, by a compaction instead of a ping when
	// Action is warmCompacting; batched news may go instead of either.
	Warm bool
	// Class is the kind of wait the reason names (warmClassWork …): the
	// evidence for a ping or a compaction is counted per class (WP-1000).
	Class string
}

// decideWarm decides, a few minutes before a session's cache cools,
// whether to keep it warm, let it cool, or compact it and let it cool.
func decideWarm(f warmFacts) warmChoice {
	if f.Paused {
		return warmChoice{Action: warmCool, Why: "its person paused agents", Class: warmClassPaused} // no ping, no compact: both cost tokens
	}
	if f.LeftAlone {
		return warmChoice{Action: warmCool, Why: "its person stopped Waypoint in it (/wp stop)", Class: warmClassPaused}
	}
	if f.Clearable && !f.Unmerged {
		return warmChoice{Action: warmCool, Why: "it said it can be cleared", Class: warmClassClearable} // nothing more is expected of it
	}
	limit := f.Limit
	if limit <= 0 {
		limit = compactAbove
	}
	since := f.Now.Sub(f.WarmSince)
	cool := func(class, why string, likely bool) warmChoice {
		if likely && f.ContextTokens >= limit {
			return warmChoice{Action: warmCompacting, Why: why, Class: class}
		}
		return warmChoice{Action: warmCool, Why: why, Class: class}
	}
	pingMax := int64(pingBelow)
	if f.Unattended {
		pingMax = unattendedPingBelow
	}
	warm := func(class, why string) warmChoice {
		switch {
		case f.ContextTokens >= limit:
			why += fmt.Sprintf("; compacted instead of a ping, its context being over the %dk limit", limit/1000)
		case f.CompactInstead:
			why += "; compacted instead of a ping, as its person asks"
		case f.Pings > 0:
			why += "; compacted instead of a second ping"
		case f.ContextTokens >= pingMax:
			why += fmt.Sprintf("; compacted instead of a ping, its context being over %dk", pingMax/1000)
		default:
			return warmChoice{Action: warmPinged, Why: why, Warm: true, Class: class}
		}
		return warmChoice{Action: warmCompacting, Why: why, Warm: true, Class: class}
	}
	// A session whose task isn't merged yet (WP-1306): its pull request may
	// still fail CI or a review, or merge with something to follow up, and
	// its person asks the session what happened. Compacted, that question
	// starts from a summary, not the whole context written cold. It applies
	// where the session would otherwise cool; what keeps it warm (its work,
	// a request or chat with its person active) still does. Whether it pays
	// is weighed by the class's evidence (weighWarm).
	//
	// Every cool goes through orUnmerged, so none loses its own reason or
	// class: under the compaction floor it stays the cool it was; over it,
	// it compacts, keeping why it would cool, in the unmerged class, since
	// that is what the compaction is for and whose evidence weighs it (the
	// cooling class's would weigh it by sessions that were merged).
	orUnmerged := func(c warmChoice) warmChoice {
		if f.Unmerged && c.Action == warmCool && f.ContextTokens >= unmergedCompactFrom {
			c.Action, c.Class = warmCompacting, warmClassUnmerged
			c.Why += "; its task isn't merged yet, so compacted before its cache cools"
		}
		return c
	}
	if work := waitedWork(f.Pending, f.Babysits); work != "" {
		if since < warmWorkFor {
			return warm(warmClassWork, "waiting on "+work)
		}
		return orUnmerged(cool(warmClassWorkCap, fmt.Sprintf("waited over %s on %s", roundHours(warmWorkFor), work), true))
	}
	if f.Clearable {
		// Unmerged (a merged one cooled above), once its own work is weighed.
		return orUnmerged(warmChoice{Action: warmCool, Why: "it said it can be cleared", Class: warmClassClearable})
	}
	if f.Unattended {
		// Nobody is expected to answer it or come back to it: a request's
		// answer or a person can start it cold.
		return orUnmerged(cool(warmClassUnattended, "autopilot runs it, and no work of its own is running", false))
	}
	if f.Requests > 0 {
		reqs := "an open request"
		if f.Requests > 1 {
			reqs = fmt.Sprintf("%d open requests", f.Requests)
		}
		switch {
		case !f.PersonActive:
			return orUnmerged(cool(warmClassRequestsAway, reqs+", and its person is away", true))
		case since < warmRequestsFor:
			return warm(warmClassRequests, reqs+" while its person is active")
		}
		return orUnmerged(cool(warmClassRequestsCap, fmt.Sprintf("%s, unanswered for over %s", reqs, roundHours(warmRequestsFor)), true))
	}
	if f.Finished && f.Kind != SessionTask && f.TypedLast && f.PersonActive {
		if since < warmChatFor {
			return warm(warmClassChat, "its person was working with it and is active")
		}
		return orUnmerged(cool(warmClassChatCap, fmt.Sprintf("its person hasn't come back to it in %s", roundHours(warmChatFor)), false))
	}
	if f.Finished {
		return orUnmerged(cool(warmClassFinished, "finished, nothing waits on it", false))
	}
	return orUnmerged(cool(warmClassOther, "nothing it waits on is known", false))
}

// waitedWork names the session's own work that will wake it: background
// tasks and wakeups its last Stop reported, and babysits still moving.
func waitedWork(pending string, babysits int) string {
	var parts []string
	if pending != "" && pending != "?" && pending != "requests" {
		parts = append(parts, pending)
	}
	if babysits > 0 {
		parts = append(parts, countNoun(babysits, "babysat pull request"))
	}
	return strings.Join(parts, " and ")
}

func roundHours(d time.Duration) string {
	if h := int(d / time.Hour); h == 1 {
		return "an hour"
	} else {
		return fmt.Sprintf("%d hours", h)
	}
}

// KeepCachesWarm decides, for each session whose cache cools in a few
// minutes, whether to keep it warm (from the scheduler, each minute).
func (s *Service) KeepCachesWarm(ctx context.Context) {
	now := s.now()
	ids, err := queryIDs(ctx, s.db.R, `SELECT id FROM agent_sessions WHERE stopped_at IS NOT NULL AND ended_at IS NULL
		AND stopped_at <= ? AND stopped_at > ? AND warm_state = '' AND state != 'ended' AND cache_ttl = '1h' AND cli = 'claude' ORDER BY stopped_at LIMIT 100`,
		now.Add(-warmPingAt).UnixMilli(), now.Add(-warmPingLate).UnixMilli())
	if err != nil {
		s.log.WarnContext(ctx, "warm caches", "err", err)
		return
	}
	for _, id := range ids {
		if err := s.warmSession(ctx, id); err != nil {
			s.log.WarnContext(ctx, "warm a cache", "session", id, "err", err)
		}
	}
}

// warmSession decides for one session and acts on it.
func (s *Service) warmSession(ctx context.Context, id int64) error {
	now := s.now()
	var cli, machine, pending, warmState, kind, agent, model string
	var user, ctxTokens int64
	var pings int
	var stopped, since, finished sql.NullInt64
	var typed, clearable, left bool
	err := s.db.R.QueryRowContext(ctx, `SELECT cli_id, machine, user_id, pending, finished_at, warm_state, kind, context_tokens, stopped_at, warm_since, prompt_typed, agent, said_clearable, warm_pings, model,
		left_alone_at IS NOT NULL FROM agent_sessions WHERE id = ?`, id).Scan(&cli, &machine, &user, &pending, &finished, &warmState, &kind, &ctxTokens, &stopped, &since, &typed, &agent, &clearable, &pings, &model, &left)
	if err != nil || warmState != warmNone || !stopped.Valid || !cliSessionRE.MatchString(cli) {
		return err
	}
	if !since.Valid {
		since = stopped
	}
	f := warmFacts{Now: now, WarmSince: msTime(since.Int64), Kind: kind, Finished: finished.Valid, Pending: pending,
		TypedLast: typed, ContextTokens: ctxTokens, Clearable: clearable, LeftAlone: left, Limit: s.contextLimit(ctx), Pings: pings,
		Unattended: s.unattendedSession(ctx, id), Unmerged: s.holdsUnmerged(ctx, id)}
	if u, err := s.userByID(ctx, s.db.R, user); err == nil {
		f.CompactInstead = u.Prefs.CacheWarm == CacheWarmCompact
	}
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM babysits WHERE session_id = ? AND ended_at IS NULL AND state IN (?,?,?,?)`,
		id, babysitMoving[0], babysitMoving[1], babysitMoving[2], babysitMoving[3]).Scan(&f.Babysits); err != nil {
		return err
	}
	if f.Requests, err = s.openSessionRequests(ctx, s.db.R, id); err != nil {
		return err
	}
	f.PersonActive = s.personActive(ctx, user)
	paused, err := s.sessionPause(ctx, user, agent)
	if err != nil {
		return err
	}
	f.Paused = paused != nil
	c := decideWarm(f)
	rule := c.Action
	c = s.weighWarm(ctx, user, model, ctxTokens, c) // WP-1000: only while it pays
	evidenceChanged := rule != c.Action             // before the runner check: that is no evidence
	if c.Action != warmCool && !runnerOnline(ctx, s, user, machine) {
		c = warmChoice{Action: warmCool, Why: c.Why + "; no runner of its person's is online on " + machine + " to keep it warm", Class: c.Class}
	}
	rec := warmRecord{State: c.Action, Why: c.Why, Ping: c.Action == warmPinged, Class: c.Class, Model: model, Context: ctxTokens}
	if evidenceChanged {
		rec.Rule = rule
	}
	// What it is told when its cache would cool anyway goes now instead, and
	// keeps the cache warm just the same: batched news, and a background job
	// quiet long enough to tell of early (WP-1045, WP-ADR118).
	if c.Warm {
		held := s.hasHeldNews(ctx, id)
		early := s.tellEarly(ctx, id, user)
		if held || early != "" {
			why, _, _ := strings.Cut(c.Why, "; compacted instead")
			rec.State, rec.News, rec.Ping, rec.Rule = warmPinged, true, false, ""
			what := "its batched news"
			switch {
			case held && early != "":
				what = "its batched news and its quiet background job's notice"
			case early != "":
				what = "its quiet background job's notice"
			}
			rec.Why = what + " went now, a few minutes before its cache would cool (" + why + ")"
			if err := s.recordWarm(ctx, id, rec); err != nil {
				return err
			}
			if held {
				s.releaseBatchWaiting(ctx, id, early != "") // the early notice goes in the same prompt
			} else {
				s.postToSession(ctx, id, true)
			}
			return nil
		}
	}
	if err := s.recordWarm(ctx, id, rec); err != nil {
		return err
	}
	if c.Action == warmCool {
		return nil
	}
	job := MessageJob{CLIID: cli, Body: keepWarmBody, IfIdle: true, Type: true}
	if c.Action == warmCompacting {
		job = MessageJob{CLIID: cli, Compact: true}
	}
	raw, _ := json.Marshal(job)
	if _, err := enqueueRunnerJob(ctx, s, &Actor{User: &User{ID: user}}, RunnerJobInput{Kind: JobMessage, OwnerID: user, Label: machine, Input: raw,
		Source: cacheWarmSource, SourceID: id}); err != nil {
		s.warmFailed(ctx, id, err.Error())
	}
	return nil
}

// holdsUnmerged: a task the session worked on in the last unmergedWithin
// isn't merged or closed (its status not done or canceled), and its latest
// run is the session's and wasn't given back or a diagnosis (released, cancelled,
// refused, diagnosed): the session holds work still on its way (WP-1306).
func (s *Service) holdsUnmerged(ctx context.Context, id int64) bool {
	var unmerged bool
	if err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM agent_runs r JOIN issues i ON i.id = r.issue_id JOIN statuses st ON st.id = i.status_id
		WHERE r.session_id = ? AND coalesce(r.ended_at, r.started_at) > ? AND st.category NOT IN ('done', 'canceled')
		AND r.outcome NOT IN ('released', 'cancelled', 'refused', 'diagnosed')
		AND r.id = (SELECT max(l.id) FROM agent_runs l WHERE l.issue_id = r.issue_id))`,
		id, s.nowMS()-unmergedWithin.Milliseconds()).Scan(&unmerged); err != nil {
		s.log.WarnContext(ctx, "is its task merged", "session", id, "err", err)
		return false
	}
	return unmerged
}

// unattendedSession: autopilot runs the session, and no person has typed
// into it since autopilot's latest prompt (like the CLI's unattended mark).
// Autopilot's prompts keep their mark even under --no-prompts; a session
// autopilot started with no prompt of its recorded counts until its person
// types.
func (s *Service) unattendedSession(ctx context.Context, id int64) bool {
	var prompted, typed sql.NullInt64
	var started bool
	if err := s.db.R.QueryRowContext(ctx, `SELECT (SELECT max(created_at) FROM session_events WHERE session_id = ? AND kind = 'prompt' AND (detail LIKE ? OR detail LIKE ?)),
		(SELECT typed_at FROM agent_sessions WHERE id = ?), EXISTS (SELECT 1 FROM autopilot_steps WHERE next_session = ?)`,
		id, AutopilotPromptMark+"%", "<pasted_content%>"+AutopilotPromptMark+"%", id, id).Scan(&prompted, &typed, &started); err != nil {
		s.log.WarnContext(ctx, "is it autopilot's session", "session", id, "err", err)
		return false
	}
	if prompted.Valid {
		return !typed.Valid || typed.Int64 < prompted.Int64
	}
	return started && !typed.Valid
}

// hasHeldNews: news for the session waits in a batch (WP-ADR6).
func (s *Service) hasHeldNews(ctx context.Context, sessionID int64) bool {
	var held bool
	_ = s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM session_messages WHERE session_id = ? AND state = 'queued' AND batch_until IS NOT NULL AND passive = 0)
		OR EXISTS (SELECT 1 FROM requests WHERE session_id = ? AND delivered_at IS NULL AND batch_until IS NOT NULL)`, sessionID, sessionID).Scan(&held)
	var state string
	_ = s.db.R.QueryRowContext(ctx, `SELECT state FROM agent_sessions WHERE id = ?`, sessionID).Scan(&state)
	return held && state == SessionIdle // only an idle session is posted its batch
}

// personActive: the person used the web, or typed into one of their
// sessions, in the last personActiveWithin.
func (s *Service) personActive(ctx context.Context, userID int64) bool {
	after := s.now().Add(-personActiveWithin).UnixMilli()
	var active bool
	_ = s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE user_id = ? AND last_seen_at >= ? AND expires_at > ?)
		OR EXISTS (SELECT 1 FROM agent_sessions WHERE user_id = ? AND typed_at >= ?)`, userID, after, s.nowMS(), userID, after).Scan(&active)
	return active
}

// warmRecord is a decision as recordWarm keeps it.
type warmRecord struct {
	State, Why string
	Ping       bool   // it is a ping: counts toward the session's pings
	News       bool   // batched news went instead of a ping
	Class      string // the kind of wait (warmChoice.Class)
	Rule       string // the rule's action, when the evidence changed it
	Model      string
	Context    int64
}

// recordWarm records a decision on the session, its timeline and the
// evidence (warm_decisions, WP-1000).
func (s *Service) recordWarm(ctx context.Context, id int64, d warmRecord) error {
	state, why := d.State, d.Why
	now := s.nowMS()
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		x, err := s.sessionByID(ctx, t, SystemActor(), id)
		if err != nil {
			return err
		}
		inc := 0
		if d.Ping {
			inc = 1
		}
		res, err := t.ExecContext(ctx, `UPDATE agent_sessions SET warm_state = ?, warm_note = ?, warm_at = ?, warm_pings = warm_pings + ? WHERE id = ? AND warm_state = ''`,
			state, excerpt(why, 300), now, inc, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errWarmTaken
		}
		if err := s.noteWarmDecision(ctx, t, id, x.userID, now, d); err != nil {
			return err
		}
		return s.sessionTimeline(ctx, t, id, now, "cache", "", 0, "", "", warmLine(state, why, x), x.projectID, map[string]any{"decision": state})
	})
	if errors.Is(err, errWarmTaken) {
		return nil
	}
	if err == nil {
		s.publishSession(ctx, id)
	}
	return err
}

var errWarmTaken = errors.New("decided already")

// warmLine is a decision as its timeline says it.
func warmLine(state, why string, x *AgentSession) string {
	switch state {
	case warmPinged:
		return "Kept the prompt cache warm: " + why
	case warmCompacting:
		return fmt.Sprintf("Compacting it (%dk tokens of context) before its prompt cache cools: %s", x.ContextTokens/1000, why)
	}
	return "Let the prompt cache cool: " + why
}

// warmFailed records that a ping or compaction didn't happen.
func (s *Service) warmFailed(ctx context.Context, id int64, why string) {
	now := s.nowMS()
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		var project sql.NullInt64
		var state string
		if err := t.QueryRowContext(ctx, `SELECT project_id, warm_state FROM agent_sessions WHERE id = ?`, id).Scan(&project, &state); err != nil {
			return err
		}
		if state != warmPinged && state != warmCompacting {
			return nil // a hook moved on meanwhile
		}
		what := "keep its prompt cache warm"
		if state == warmCompacting {
			what = "compact it"
		}
		if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET warm_state = ?, warm_note = ?, warm_at = ? WHERE id = ?`, warmFailed, excerpt(why, 300), now, id); err != nil {
			return err
		}
		return s.sessionTimeline(ctx, t, id, now, "cache", "", 0, "", "", "Couldn't "+what+": "+why, project, map[string]any{"decision": warmFailed})
	})
	if err != nil {
		s.log.WarnContext(ctx, "record a failed keep-warm", "session", id, "err", err)
	}
	s.publishSession(ctx, id)
}

// cacheWarmJobFinished records how a ping or compaction job went (from
// afterDispatch): a runner that found the session busy or something typed
// in its pane posted nothing.
func (s *Service) cacheWarmJobFinished(ctx context.Context, e *Event) {
	if src, _ := e.Data["source"].(string); src != cacheWarmSource {
		return
	}
	id := dataInt(e, "source_id")
	var result, msg sql.NullString
	_ = s.db.R.QueryRowContext(ctx, `SELECT result, error FROM runner_jobs WHERE id = ?`, dataInt(e, "job")).Scan(&result, &msg)
	if e.Data["state"] != "done" {
		s.warmFailed(ctx, id, cmp(excerpt(msg.String, 200), fmt.Sprint(e.Data["state"])))
		return
	}
	var r struct {
		Delivered *bool  `json:"delivered"`
		Held      string `json:"held"`
	}
	_ = json.Unmarshal([]byte(result.String), &r)
	if r.Delivered != nil && !*r.Delivered {
		s.warmFailed(ctx, id, cmp(r.Held, "the runner posted nothing"))
	}
}

// markPosted notes that Waypoint posts to a session's inbox, so the prompt
// that post starts doesn't read as its person typing.
// It returns the previous marker and the one written, for restorePosted.
func markPosted(ctx context.Context, s *Service, sessionID int64) (previous sql.NullInt64, at int64, ok bool) {
	// Read and write in one write transaction, so each post sees the marker
	// the previous one wrote.
	tx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		s.log.WarnContext(ctx, "note a post to a session", "session", sessionID, "err", err)
		return previous, 0, false
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRowContext(ctx, `SELECT posted_at FROM agent_sessions WHERE id = ?`, sessionID).Scan(&previous); err != nil {
		s.log.WarnContext(ctx, "read previous post marker", "session", sessionID, "err", err)
		return previous, 0, false
	}
	at = s.nowMS()
	if _, err := tx.ExecContext(ctx, `UPDATE agent_sessions SET posted_at = ? WHERE id = ?`, at, sessionID); err != nil {
		s.log.WarnContext(ctx, "note a post to a session", "session", sessionID, "err", err)
		return previous, 0, false
	}
	if err := tx.Commit(); err != nil {
		s.log.WarnContext(ctx, "note a post to a session", "session", sessionID, "err", err)
		return previous, 0, false
	}
	return previous, at, true
}

// restorePosted undoes markPosted when the post failed, unless a newer
// marker has been written since.
func restorePosted(ctx context.Context, s *Service, sessionID int64, previous sql.NullInt64, markedAt int64) {
	var prev any
	if previous.Valid {
		prev = previous.Int64
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE agent_sessions SET posted_at = ? WHERE id = ? AND posted_at = ?`, prev, sessionID, markedAt); err != nil {
		s.log.WarnContext(ctx, "restore post marker", "session", sessionID, "err", err)
	}
}

// ---- the hooks' side ----

// warmQuietPrompt: the prompt is a keep-warm ping, or the /compact Waypoint
// typed: no turn of the session's work. The hooks leave the session as it
// was.
func (s *Service) warmQuietPrompt(ctx context.Context, t *tx, x *AgentSession, ev *HookEvent) (bool, error) {
	var state string
	if err := t.QueryRowContext(ctx, `SELECT warm_state FROM agent_sessions WHERE id = ?`, x.ID).Scan(&state); err != nil {
		return false, err
	}
	p := strings.TrimSpace(ev.Prompt)
	switch {
	case (state == warmCompacting || state == warmReview) && strings.HasPrefix(p, "/compact"):
	case isKeepWarmPing(p):
		state = warmInPing
	default:
		return false, nil
	}
	_, err := t.ExecContext(ctx, `UPDATE agent_sessions SET warm_state = ?, stopped_at = NULL WHERE id = ?`, state, x.ID)
	return true, err
}

// warmPromptStarted: any other prompt starts a turn of real work. One a
// person typed (not autopilot's, not a background task reporting back, not
// in a task session, not what Waypoint just posted to its inbox; an empty
// prompt, sent with --no-prompts, counts as a person's) says the person is
// active. A background task's report leaves a review job's warm state: the
// job's /compact and the Stops before its review stay quiet (WP-700).
func (s *Service) warmPromptStarted(ctx context.Context, t *tx, x *AgentSession, ev *HookEvent, at int64) error {
	var posted sql.NullInt64
	if err := t.QueryRowContext(ctx, `SELECT posted_at FROM agent_sessions WHERE id = ?`, x.ID).Scan(&posted); err != nil {
		return err
	}
	if err := warmUsed(ctx, t, x.ID, at); err != nil { // WP-1000
		return err
	}
	typed := !isOwnPrompt(ev.Prompt) && x.Kind != SessionTask &&
		(!posted.Valid || at-posted.Int64 > postedEcho.Milliseconds())
	_, err := t.ExecContext(ctx, `UPDATE agent_sessions SET stopped_at = NULL, warm_state = CASE WHEN ? AND warm_state = ? THEN warm_state ELSE '' END, prompt_typed = ?,
		typed_at = CASE WHEN ? THEN max(coalesce(typed_at, 0), ?) ELSE typed_at END WHERE id = ?`, isHarnessPrompt(ev.Prompt), warmReview, typed, typed, at, x.ID)
	return err
}

// warmKept: keep (or /keep) starts no turn, but its person typed it. The
// session is theirs from now, not autopilot's (WP-644), and they are active.
func (s *Service) warmKept(ctx context.Context, t *tx, x *AgentSession, at int64) error {
	if x.Kind == SessionTask {
		return nil
	}
	_, err := t.ExecContext(ctx, `UPDATE agent_sessions SET typed_at = max(coalesce(typed_at, 0), ?) WHERE id = ?`, at, x.ID)
	return err
}

// warmStop handles a Stop for the cache: the stop of a ping (or of a
// compaction) is quiet, and keeps the run of pings going; any other stop
// starts a new one.
func (s *Service) warmStop(ctx context.Context, t *tx, x *AgentSession, ev *HookEvent, at int64) (quiet bool, err error) {
	var state string
	if err := t.QueryRowContext(ctx, `SELECT warm_state FROM agent_sessions WHERE id = ?`, x.ID).Scan(&state); err != nil {
		return false, err
	}
	ttl := ev.CacheTTL
	if ttl != "1h" && ttl != "5m" {
		ttl = ""
	}
	quiet = !ev.StopHookActive && (state == warmInPing || state == warmCompacting || state == warmCompacted || state == warmReview)
	q := `UPDATE agent_sessions SET stopped_at = ?, warm_since = ?, warm_pings = 0, warm_state = '', warm_note = '', warm_at = NULL, cache_ttl = coalesce(nullif(?, ''), cache_ttl),
		said_clearable = ? WHERE id = ?`
	args := []any{at, at, ttl, saysClearable(ev.LastMessage), x.ID}
	if quiet {
		next := warmNone
		switch state {
		case warmCompacting:
			next = warmCompacted // nothing more for this stop: it was let cool
		case warmReview:
			next = warmReview // the review's prompt comes next
		}
		q = `UPDATE agent_sessions SET stopped_at = ?, warm_state = ?, cache_ttl = coalesce(nullif(?, ''), cache_ttl) WHERE id = ?`
		args = []any{at, next, ttl, x.ID}
	}
	_, err = t.ExecContext(ctx, q, args...)
	return quiet, err
}

// clearableRE: a last message that says the session can be cleared, as the
// ship reply's last line does ("The session can be cleared without losing
// work."). "cannot be cleared" and "can't be cleared" don't match.
var clearableRE = lazyre.New(`(?i)\bcan (?:now |safely )?be cleared\b`)

// saysClearable: the agent's last message says nothing more is expected of
// the session (WP-534).
func saysClearable(msg string) bool {
	return clearableRE.MatchString(msg)
}

// contextLimit is the context at which a session is compacted rather than
// kept warm: compactAbove, or the kit's compact_window when lower (Claude
// Code compacts there by itself at its next turn anyway).
func (s *Service) contextLimit(ctx context.Context) int64 {
	limit := int64(compactAbove)
	if k, err := s.theKit(ctx, s.db.R); err == nil && k != nil && k.kit != nil && k.kit.CompactWindow > 0 && int64(k.kit.CompactWindow) < limit {
		limit = int64(k.kit.CompactWindow)
	}
	return limit
}

// warmSessionStart handles a SessionStart for the cache. Waypoint's own
// /compact ends with one (source compact): the session stays as it was,
// done for this stop. Any other start runs a turn.
func (s *Service) warmSessionStart(ctx context.Context, t *tx, x *AgentSession, ev *HookEvent, at int64) (quiet bool, err error) {
	var state string
	if err := t.QueryRowContext(ctx, `SELECT warm_state FROM agent_sessions WHERE id = ?`, x.ID).Scan(&state); err != nil {
		return false, err
	}
	if ev.Source == "compact" && state == warmReview {
		// warm_at: compactedForReview's evidence that it really happened.
		if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET context_tokens = 0, warm_at = ? WHERE id = ?`, at, x.ID); err != nil {
			return true, err
		}
		return true, s.sessionTimeline(ctx, t, x.ID, at, "autopilot", "", 0, "", "", "Compacted it before the adversarial review", x.projectID, map[string]any{"decision": warmReview})
	}
	// The compaction's own stop may have come first (state compacted).
	if ev.Source == "compact" && (state == warmCompacting || state == warmCompacted) {
		if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET warm_state = ?, warm_at = ?, context_tokens = 0 WHERE id = ?`, warmCompacted, at, x.ID); err != nil {
			return true, err
		}
		return true, s.sessionTimeline(ctx, t, x.ID, at, "cache", "", 0, "", "", "Compacted it; its prompt cache now cools", x.projectID, map[string]any{"decision": warmCompacted})
	}
	_, err = t.ExecContext(ctx, `UPDATE agent_sessions SET stopped_at = NULL, warm_state = '' WHERE id = ?`, x.ID)
	return false, err
}
