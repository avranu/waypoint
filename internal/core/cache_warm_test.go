package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Keeping prompt caches warm (WP-192).

func TestDecideWarm(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	for name, c := range map[string]struct {
		f      warmFacts
		action string
		why    string
	}{
		"background task":       {warmFacts{WarmSince: ago(time.Hour), Pending: "a background task"}, warmPinged, "waiting on a background task"},
		"paused":                {warmFacts{WarmSince: ago(time.Hour), Pending: "a background task", ContextTokens: 300_000, Paused: true}, warmCool, "its person paused agents"},
		"babysits":              {warmFacts{WarmSince: ago(time.Hour), Babysits: 2}, warmPinged, "waiting on 2 babysat pull requests"},
		"work too long":         {warmFacts{WarmSince: ago(3 * time.Hour), Pending: "a background task"}, warmCool, "waited over 3 hours on a background task"},
		"work too long and big": {warmFacts{WarmSince: ago(3 * time.Hour), Babysits: 1, ContextTokens: 250_000}, warmCompacting, "waited over 3 hours on a babysat pull request"},
		"requests, active":      {warmFacts{WarmSince: ago(time.Hour), Pending: "requests", Requests: 1, PersonActive: true}, warmPinged, "an open request while its person is active"},
		"requests, away":        {warmFacts{WarmSince: ago(time.Hour), Pending: "requests", Requests: 2}, warmCool, "2 open requests, and its person is away"},
		"requests, away, big":   {warmFacts{WarmSince: ago(time.Hour), Requests: 1, ContextTokens: 300_000}, warmCompacting, "an open request, and its person is away"},
		"requests too long":     {warmFacts{WarmSince: ago(8 * time.Hour), Requests: 1, PersonActive: true}, warmCool, "an open request, unanswered for over 8 hours"},
		"chat":                  {warmFacts{WarmSince: ago(time.Hour), Finished: true, TypedLast: true, PersonActive: true}, warmPinged, "its person was working with it and is active"},
		"chat, big":             {warmFacts{WarmSince: ago(3 * time.Hour), Finished: true, TypedLast: true, PersonActive: true, ContextTokens: 400_000}, warmCool, "its person hasn't come back to it in 2 hours"},
		"chat, away":            {warmFacts{WarmSince: ago(time.Hour), Finished: true, TypedLast: true}, warmCool, "finished, nothing waits on it"},
		"autopilot's prompt":    {warmFacts{WarmSince: ago(time.Hour), Finished: true, PersonActive: true}, warmCool, "finished, nothing waits on it"},
		"task session":          {warmFacts{WarmSince: ago(time.Hour), Kind: SessionTask, Finished: true, TypedLast: true, PersonActive: true}, warmCool, "finished, nothing waits on it"},
		"unknown":               {warmFacts{WarmSince: ago(time.Hour), Pending: "?", ContextTokens: 500_000}, warmCool, "nothing it waits on is known"},
		// WP-534.
		"said it can be cleared": {warmFacts{WarmSince: ago(time.Hour), Babysits: 1, ContextTokens: 300_000, Clearable: true}, warmCool, "it said it can be cleared"},
		"compact instead":        {warmFacts{WarmSince: ago(time.Hour), Pending: "a background task", CompactInstead: true}, warmCompacting, "waiting on a background task; compacted instead of a ping, as its person asks"},
		"over the limit":         {warmFacts{WarmSince: ago(time.Hour), Pending: "a background task", ContextTokens: 210_000}, warmCompacting, "waiting on a background task; compacted instead of a ping, its context being over the 200k limit"},
		"over the kit's limit":   {warmFacts{WarmSince: ago(time.Hour), Babysits: 1, ContextTokens: 160_000, Limit: 150_000, CompactInstead: true}, warmCompacting, "waiting on a babysat pull request; compacted instead of a ping, its context being over the 150k limit"},
		"compact instead, cools": {warmFacts{WarmSince: ago(3 * time.Hour), Pending: "a background task", CompactInstead: true}, warmCool, "waited over 3 hours on a background task"},
		// WP-644: one ping per wait, and only for a small context.
		"second ping":       {warmFacts{WarmSince: ago(2 * time.Hour), Pending: "a background task", Pings: 1, ContextTokens: 20_000}, warmCompacting, "waiting on a background task; compacted instead of a second ping"},
		"too big to ping":   {warmFacts{WarmSince: ago(time.Hour), Requests: 1, PersonActive: true, ContextTokens: 101_292}, warmCompacting, "an open request while its person is active; compacted instead of a ping, its context being over 100k"},
		"chat, second ping": {warmFacts{WarmSince: ago(time.Hour + 55*time.Minute), Finished: true, TypedLast: true, PersonActive: true, Pings: 1}, warmCompacting, "its person was working with it and is active; compacted instead of a second ping"},
		// Autopilot's sessions: only their own work keeps them warm.
		"autopilot, small, working":  {warmFacts{WarmSince: ago(time.Hour), Pending: "a background task", ContextTokens: 30_000, Unattended: true}, warmPinged, "waiting on a background task"},
		"autopilot, working":         {warmFacts{WarmSince: ago(time.Hour), Babysits: 1, ContextTokens: 60_000, Unattended: true}, warmCompacting, "waiting on a babysat pull request; compacted instead of a ping, its context being over 40k"},
		"autopilot, second ping":     {warmFacts{WarmSince: ago(2 * time.Hour), Pending: "a background task", ContextTokens: 30_000, Pings: 1, Unattended: true}, warmCompacting, "waiting on a background task; compacted instead of a second ping"},
		"autopilot, requests":        {warmFacts{WarmSince: ago(time.Hour), Pending: "requests", Requests: 1, PersonActive: true, ContextTokens: 300_000, Unattended: true}, warmCool, "autopilot runs it, and no work of its own is running"},
		"autopilot, finished, typed": {warmFacts{WarmSince: ago(time.Hour), Finished: true, TypedLast: true, PersonActive: true, Unattended: true}, warmCool, "autopilot runs it, and no work of its own is running"},
		// WP-1306: a session whose task isn't merged yet is compacted before
		// it cools, said it can be cleared or not, keeping why it would cool;
		// under 60k it cools as it would have; its own work still keeps it
		// warm first.
		"unmerged":                   {warmFacts{WarmSince: ago(time.Hour), Finished: true, ContextTokens: 120_000, Unattended: true, Clearable: true, Unmerged: true}, warmCompacting, "it said it can be cleared; its task isn't merged yet, so compacted before its cache cools"},
		"unmerged, clearable small":  {warmFacts{WarmSince: ago(time.Hour), Finished: true, ContextTokens: 59_000, Clearable: true, Unmerged: true}, warmCool, "it said it can be cleared"},
		"unmerged, unattended":       {warmFacts{WarmSince: ago(time.Hour), Finished: true, ContextTokens: 120_000, Unattended: true, Unmerged: true}, warmCompacting, "autopilot runs it, and no work of its own is running; its task isn't merged yet, so compacted before its cache cools"},
		"unmerged, unattended small": {warmFacts{WarmSince: ago(time.Hour), Finished: true, ContextTokens: 59_000, Unattended: true, Unmerged: true}, warmCool, "autopilot runs it, and no work of its own is running"},
		"unmerged, work too long":    {warmFacts{WarmSince: ago(3 * time.Hour), Babysits: 1, ContextTokens: 120_000, Unmerged: true}, warmCompacting, "waited over 3 hours on a babysat pull request; its task isn't merged yet, so compacted before its cache cools"},
		"unmerged, small":            {warmFacts{WarmSince: ago(time.Hour), Finished: true, ContextTokens: 59_000, Unmerged: true}, warmCool, "finished, nothing waits on it"},
		"unmerged, working":          {warmFacts{WarmSince: ago(time.Hour), Pending: "a background task", ContextTokens: 30_000, Unattended: true, Unmerged: true}, warmPinged, "waiting on a background task"},
		"unmerged, asking":           {warmFacts{WarmSince: ago(10 * time.Minute), Finished: true, ContextTokens: 30_000, Requests: 1, PersonActive: true, Unmerged: true}, warmPinged, "an open request while its person is active"},
		"unmerged, chat":             {warmFacts{WarmSince: ago(10 * time.Minute), Finished: true, Kind: SessionYours, TypedLast: true, ContextTokens: 30_000, PersonActive: true, Unmerged: true}, warmPinged, "its person was working with it and is active"},
		"unmerged, asking away":      {warmFacts{WarmSince: ago(10 * time.Minute), Finished: true, ContextTokens: 120_000, Requests: 1, Unmerged: true}, warmCompacting, "an open request, and its person is away; its task isn't merged yet, so compacted before its cache cools"},
		"unmerged, paused":           {warmFacts{WarmSince: ago(time.Hour), ContextTokens: 120_000, Paused: true, Unmerged: true}, warmCool, "its person paused agents"},
		"merged, clearable":          {warmFacts{WarmSince: ago(time.Hour), ContextTokens: 120_000, Clearable: true}, warmCool, "it said it can be cleared"},
		"merged, unattended":         {warmFacts{WarmSince: ago(time.Hour), ContextTokens: 120_000, Unattended: true}, warmCool, "autopilot runs it, and no work of its own is running"},
		"unmerged, left alone":       {warmFacts{WarmSince: ago(time.Hour), ContextTokens: 120_000, LeftAlone: true, Unmerged: true}, warmCool, "its person stopped Waypoint in it (/wp stop)"},
	} {
		c.f.Now = now
		got := decideWarm(c.f)
		if got.Action != c.action || got.Why != c.why {
			t.Errorf("%s: %+v, want %s %q", name, got, c.action, c.why)
		}
		// An unmerged session keeps the class it cools in; one compacted
		// because it is unmerged is weighed and counted as such (WP-1306).
		if want, ok := unmergedClass[name]; ok && got.Class != want {
			t.Errorf("%s: class %q, want %q", name, got.Class, want)
		}
	}
}

var unmergedClass = map[string]string{
	"unmerged":                   warmClassUnmerged,
	"unmerged, clearable small":  warmClassClearable,
	"unmerged, unattended":       warmClassUnmerged,
	"unmerged, unattended small": warmClassUnattended,
	"unmerged, work too long":    warmClassUnmerged,
	"unmerged, small":            warmClassFinished,
	"unmerged, asking away":      warmClassUnmerged,
}

// warmEnv: the agent's session stopped with a background task running, on a
// 1h cache, its runner on m1 online.
func warmEnv(t *testing.T) *bsEnv {
	be := newBabysitEnv(t)
	be.runner(be.admin, "m1", nil)
	be.stop(&HookPending{Tasks: []PendingTask{{Type: "shell", Status: "running"}}}, "1h", "Started the build.")
	return be
}

func (be *bsEnv) hookEv(ev HookEvent) *HookResult {
	be.t.Helper()
	ev.SessionID, ev.Machine, ev.CLI = "cli-1", "m1", "claude"
	res, err := be.s.SessionHook(be.ctx, be.agent, []HookEvent{ev})
	if err != nil {
		be.t.Fatal(err)
	}
	return res
}

func (be *bsEnv) stop(p *HookPending, ttl, msg string) *HookResult {
	return be.hookEv(HookEvent{Event: "Stop", Pending: p, CacheTTL: ttl, LastMessage: msg, ContextTokens: 50_000})
}

// warmSweep moves the clock on, keeps the runner checking in, and runs the
// minute's keep-warm pass.
func (be *bsEnv) warmSweep(d time.Duration) {
	be.t.Helper()
	be.setClock(be.clockNow().Add(d))
	be.runner(be.admin, "m1", nil)
	be.s.KeepCachesWarm(be.ctx)
	be.s.Wait()
}

func (be *bsEnv) warmJobs() []MessageJob {
	be.t.Helper()
	rows, err := be.s.db.R.QueryContext(be.ctx, `SELECT input FROM runner_jobs WHERE source = ? ORDER BY id`, cacheWarmSource)
	if err != nil {
		be.t.Fatal(err)
	}
	defer rows.Close()
	var out []MessageJob
	for rows.Next() {
		var raw string
		var j MessageJob
		_ = rows.Scan(&raw)
		_ = json.Unmarshal([]byte(raw), &j)
		out = append(out, j)
	}
	return out
}

func (be *bsEnv) session() *AgentSession {
	be.t.Helper()
	x, err := be.s.sessionByID(be.ctx, be.s.db.R, be.admin, be.agent.Session)
	if err != nil {
		be.t.Fatal(err)
	}
	return x
}

func (be *bsEnv) timeline(kind string) []string {
	be.t.Helper()
	rows, err := be.s.db.R.QueryContext(be.ctx, `SELECT detail FROM session_events WHERE session_id = ? AND kind = ? ORDER BY id`, be.agent.Session, kind)
	if err != nil {
		be.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		_ = rows.Scan(&d)
		out = append(out, d)
	}
	return out
}

func TestKeepWarmPingsASessionWaitingOnWork(t *testing.T) {
	be := warmEnv(t)
	be.warmSweep(30 * time.Minute)
	if jobs := be.warmJobs(); len(jobs) != 0 {
		t.Fatalf("pinged with half an hour left: %+v", jobs)
	}
	be.warmSweep(25 * time.Minute) // 55 minutes after the stop
	jobs := be.warmJobs()
	if len(jobs) != 1 || jobs[0].Body != keepWarmBody || !jobs[0].IfIdle || !jobs[0].Type || jobs[0].CLIID != "cli-1" {
		t.Fatalf("want one ping: %+v", jobs)
	}
	x := be.session()
	if x.Cache == nil || x.Cache.TTL != "1h" || x.Cache.Decision != "warm" || x.Cache.Pings != 1 || !strings.Contains(x.Cache.Note, "a background task") {
		t.Fatalf("cache = %+v", x.Cache)
	}
	if tl := be.timeline("cache"); len(tl) != 1 || tl[0] != "Kept the prompt cache warm: waiting on a background task" {
		t.Fatalf("timeline: %q", tl)
	}
	be.warmSweep(time.Minute)
	if jobs := be.warmJobs(); len(jobs) != 1 {
		t.Fatalf("pinged twice for one stop: %+v", jobs)
	}

	// The ping's turn leaves the session as it was.
	prompts := len(be.timeline("prompt"))
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: keepWarmBody})
	if x := be.session(); x.LastPrompt == keepWarmBody || x.Pending != "a background task" {
		t.Fatalf("the ping's prompt changed the session: %q %q", x.LastPrompt, x.Pending)
	}
	be.stop(&HookPending{Tasks: []PendingTask{{Type: "shell", Status: "running"}}}, "1h", "ok")
	x = be.session()
	if x.LastUpdate != "Started the build." || x.Cache.Pings != 1 || x.Cache.Decision != "" || x.State != SessionIdle {
		t.Fatalf("after the ping: update %q cache %+v state %s", x.LastUpdate, x.Cache, x.State)
	}
	if n := len(be.timeline("prompt")); n != prompts || len(be.timeline("update")) != 1 {
		t.Errorf("the ping is on the timeline: %d prompts, updates %q", n, be.timeline("update"))
	}

	// 55 minutes after the ping's stop it is compacted, not pinged again
	// (WP-644), and nothing more happens for this stop.
	be.warmSweep(55 * time.Minute)
	if jobs := be.warmJobs(); len(jobs) != 2 || !jobs[1].Compact || be.session().Cache.Pings != 1 {
		t.Fatalf("want a compaction second: %+v %+v", jobs, be.session().Cache)
	}
	if x := be.session(); x.Cache.Decision != "compacting" || x.Cache.Note != "waiting on a background task; compacted instead of a second ping" {
		t.Fatalf("cache = %+v", x.Cache)
	}
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: "/compact"})
	be.hookEv(HookEvent{Event: "SessionStart", Source: "compact"})
	be.warmSweep(55 * time.Minute)
	if jobs := be.warmJobs(); len(jobs) != 2 {
		t.Fatalf("acted after the compaction: %+v", jobs)
	}

	// A real turn starts the count again.
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: "How did the build go?"})
	be.stop(&HookPending{Tasks: []PendingTask{{Type: "shell", Status: "running"}}}, "1h", "Still building.")
	if x := be.session(); x.Cache.Pings != 0 || x.Cache.Decision != "" || x.LastUpdate != "Still building." {
		t.Fatalf("after a real turn: %+v", x.Cache)
	}
	be.warmSweep(55 * time.Minute)
	if jobs := be.warmJobs(); len(jobs) != 3 || jobs[2].Body != keepWarmBody {
		t.Fatalf("no ping after a real turn: %+v", jobs)
	}
}

// WP-644: a session autopilot runs is kept warm only by its own work, and
// pinged only when its context is small; a person's prompt makes it theirs.
func TestKeepWarmAutopilotSession(t *testing.T) {
	be := newBabysitEnv(t)
	be.runner(be.admin, "m1", nil)
	// Autopilot's task finished, its person on the web.
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: AutopilotPromptMark + " started this session for WP-1"})
	if _, err := be.s.db.W.Exec(`INSERT INTO sessions(token_hash, user_id, created_at, last_seen_at, expires_at, user_agent, ip, reauth_at) VALUES ('h', ?, ?, ?, ?, '', '', 0)`,
		be.admin.id(), be.clockNow().UnixMilli(), be.clockNow().Add(time.Hour).UnixMilli(), be.clockNow().Add(2*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	be.hookEv(HookEvent{Event: "Stop", Pending: &HookPending{}, CacheTTL: "1h", LastMessage: "Shipped.", ContextTokens: 100_000})
	be.warmSweep(55 * time.Minute)
	if x := be.session(); len(be.warmJobs()) != 0 || x.Cache.Decision != "cool" || x.Cache.Note != "autopilot runs it, and no work of its own is running" {
		t.Fatalf("autopilot's finished session: %+v %+v", be.warmJobs(), x.Cache)
	}

	// Its own background task, at 60k: compacted, where a person's session
	// would be pinged.
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: AutopilotPromptMark + " review of WP-1"})
	running := &HookPending{Tasks: []PendingTask{{Type: "shell", Status: "running"}}}
	be.hookEv(HookEvent{Event: "Stop", Pending: running, CacheTTL: "1h", LastMessage: "Started the build.", ContextTokens: 60_000})
	be.warmSweep(55 * time.Minute)
	if jobs := be.warmJobs(); len(jobs) != 1 || !jobs[0].Compact {
		t.Fatalf("want a compaction: %+v", jobs)
	}

	// A person types into it: it is theirs, and the person rules apply.
	be.setClock(be.clockNow().Add(time.Minute))
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: "Why that file?"})
	be.hookEv(HookEvent{Event: "Stop", Pending: &HookPending{}, CacheTTL: "1h", LastMessage: "Because.", ContextTokens: 60_000})
	if be.s.unattendedSession(be.ctx, be.agent.Session) {
		t.Fatal("still autopilot's after its person typed")
	}

	// Autopilot takes it again; its person keeps it: no turn, but theirs.
	be.setClock(be.clockNow().Add(time.Minute))
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: AutopilotPromptMark + " started this session for WP-2"})
	if !be.s.unattendedSession(be.ctx, be.agent.Session) {
		t.Fatal("not autopilot's after its prompt")
	}
	be.setClock(be.clockNow().Add(time.Minute))
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: "/keep"})
	if be.s.unattendedSession(be.ctx, be.agent.Session) {
		t.Fatal("still autopilot's after its person kept it")
	}
}

func TestKeepWarmOnlyOnAnHourCache(t *testing.T) {
	for _, ttl := range []string{"5m", ""} {
		be := newBabysitEnv(t)
		be.runner(be.admin, "m1", nil)
		be.stop(&HookPending{Tasks: []PendingTask{{Type: "shell", Status: "running"}}}, ttl, "Started.")
		be.warmSweep(55 * time.Minute)
		if jobs := be.warmJobs(); len(jobs) != 0 {
			t.Errorf("ttl %q: pinged %+v", ttl, jobs)
		}
	}
}

func TestKeepWarmNeedsTheRunner(t *testing.T) {
	be := warmEnv(t)
	be.setClock(be.clockNow().Add(55 * time.Minute)) // its runner stopped checking in
	be.s.KeepCachesWarm(be.ctx)
	if jobs := be.warmJobs(); len(jobs) != 0 {
		t.Fatalf("pinged with no runner: %+v", jobs)
	}
	if x := be.session(); x.Cache.Decision != "cool" || !strings.Contains(x.Cache.Note, "no runner") {
		t.Fatalf("cache = %+v", x.Cache)
	}
}

func TestFinishedSessionCoolsUnlessItsPersonIsWorkingWithIt(t *testing.T) {
	be := newBabysitEnv(t)
	be.runner(be.admin, "m1", nil)
	// Autopilot's prompt: not the person's.
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: AutopilotPromptMark + ": next task"})
	be.stop(&HookPending{}, "1h", "Done.")
	be.warmSweep(55 * time.Minute)
	if x := be.session(); len(be.warmJobs()) != 0 || x.Cache.Decision != "cool" || x.Cache.Note != "autopilot runs it, and no work of its own is running" {
		t.Fatalf("autopilot's finished session: %+v", x.Cache)
	}
	finished := len(be.timeline("finished"))

	// The person types; it finishes; they're still about.
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: "Rename it."})
	be.stop(&HookPending{}, "1h", "Renamed.")
	if n := len(be.timeline("finished")); n != finished+1 {
		t.Fatalf("finished entries %d, want %d", n, finished+1)
	}
	be.warmSweep(55 * time.Minute)
	if x := be.session(); len(be.warmJobs()) != 0 || x.Cache.Decision != "cool" {
		t.Fatalf("person away (typed 55 minutes ago): %+v", x.Cache)
	}
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: "And this."})
	be.stop(&HookPending{}, "1h", "Done too.")
	be.setClock(be.clockNow().Add(50 * time.Minute))
	if _, err := be.s.db.W.Exec(`INSERT INTO sessions(token_hash, user_id, created_at, last_seen_at, expires_at, user_agent, ip, reauth_at) VALUES ('h', ?, ?, ?, ?, '', '', 0)`,
		be.admin.id(), be.clockNow().UnixMilli(), be.clockNow().UnixMilli(), be.clockNow().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	be.warmSweep(5 * time.Minute)
	if x := be.session(); len(be.warmJobs()) != 1 || x.Cache.Decision != "warm" || x.Cache.Note != "its person was working with it and is active" {
		t.Fatalf("person on the web: %+v", x.Cache)
	}
	// The ping's stop doesn't finish it again.
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: keepWarmBody})
	be.stop(&HookPending{}, "1h", "ok")
	if n := len(be.timeline("finished")); n != finished+2 {
		t.Errorf("finished entries %d, want %d: the ping finished it again", n, finished+2)
	}
}

// A post Waypoint makes to the inbox starts a prompt that isn't the person's.
func TestPostedPromptIsNotTyped(t *testing.T) {
	be := newBabysitEnv(t)
	markPosted(be.ctx, be.s, be.agent.Session)
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: "Waypoint's babysitter: o/r#7 merged."})
	var typed bool
	var at *int64
	_ = be.s.db.R.QueryRow(`SELECT prompt_typed, typed_at FROM agent_sessions WHERE id = ?`, be.agent.Session).Scan(&typed, &at)
	if typed || at != nil {
		t.Fatalf("a post read as typed: %v %v", typed, at)
	}
	be.setClock(be.clockNow().Add(postedEcho + time.Second))
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: "Now this."})
	_ = be.s.db.R.QueryRow(`SELECT prompt_typed, typed_at FROM agent_sessions WHERE id = ?`, be.agent.Session).Scan(&typed, &at)
	if !typed || at == nil || *at != be.clockNow().UnixMilli() {
		t.Fatalf("a typed prompt: %v %v", typed, at)
	}
}

func TestCompactBeforeCooling(t *testing.T) {
	be := newBabysitEnv(t)
	be.runner(be.admin, "m1", nil)
	running := &HookPending{Tasks: []PendingTask{{Type: "shell", Status: "running"}}}
	be.hookEv(HookEvent{Event: "Stop", Pending: running, CacheTTL: "1h", LastMessage: "Started.", ContextTokens: 260_000})
	// Its last real turn was over 3 hours ago (the ping's stop keeps it).
	if _, err := be.s.db.W.Exec(`UPDATE agent_sessions SET warm_since = ? WHERE id = ?`, be.clockNow().Add(-3*time.Hour).UnixMilli(), be.agent.Session); err != nil {
		t.Fatal(err)
	}
	be.warmSweep(55 * time.Minute)
	jobs := be.warmJobs()
	if len(jobs) != 1 || !jobs[0].Compact || jobs[0].Body != "" {
		t.Fatalf("want a compact job last: %+v", jobs)
	}
	if x := be.session(); x.Cache.Decision != "compacting" {
		t.Fatalf("cache = %+v", x.Cache)
	}
	// Claude Code compacts and starts again from the summary.
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: "/compact"})
	if res := be.hookEv(HookEvent{Event: "SessionStart", Source: "compact"}); res.Text != "" {
		t.Errorf("the compaction's start said %q", res.Text)
	}
	x := be.session()
	if x.Cache.Decision != "compacted" || x.ContextTokens != 0 || x.Pending != "a background task" || x.LastPrompt == "/compact" {
		t.Fatalf("after compacting: %+v ctx %d pending %q prompt %q", x.Cache, x.ContextTokens, x.Pending, x.LastPrompt)
	}
	tl := be.timeline("cache")
	if len(tl) != 2 || !strings.HasPrefix(tl[0], "Compacting it (260k tokens of context) before its prompt cache cools: waited over 3 hours") || tl[1] != "Compacted it; its prompt cache now cools" {
		t.Fatalf("timeline: %q", tl)
	}
	be.warmSweep(55 * time.Minute)
	if jobs := be.warmJobs(); len(jobs) != 1 {
		t.Fatalf("acted again after compacting: %+v", jobs)
	}
}

func TestKeepWarmJobThatPostedNothing(t *testing.T) {
	be := warmEnv(t)
	be.warmSweep(55 * time.Minute)
	var job int64
	if err := be.s.db.R.QueryRow(`SELECT id FROM runner_jobs WHERE source = ?`, cacheWarmSource).Scan(&job); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.db.W.Exec(`UPDATE runner_jobs SET state = 'done', result = ? WHERE id = ?`, `{"delivered":false,"held":"the session is busy, not idle: nothing posted"}`, job); err != nil {
		t.Fatal(err)
	}
	be.s.dispatch(be.ctx, SystemActor(), []*Event{{Kind: EvRunnerJobFinished, Data: map[string]any{"job": job, "kind": JobMessage, "state": "done", "source": cacheWarmSource, "source_id": be.agent.Session}}})
	be.s.Wait()
	x := be.session()
	if x.Cache.Decision != "failed" || !strings.Contains(x.Cache.Note, "busy") {
		t.Fatalf("cache = %+v", x.Cache)
	}
	if tl := be.timeline("cache"); len(tl) != 2 || !strings.HasPrefix(tl[1], "Couldn't keep its prompt cache warm: the session is busy") {
		t.Fatalf("timeline: %q", tl)
	}
}

// WP-534: a session whose last stop said it can be cleared is left alone.
func TestKeepWarmLeavesAClearableSessionAlone(t *testing.T) {
	be := newBabysitEnv(t)
	be.runner(be.admin, "m1", nil)
	running := &HookPending{Tasks: []PendingTask{{Type: "shell", Status: "running"}}}
	be.hookEv(HookEvent{Event: "Stop", Pending: running, CacheTTL: "1h", ContextTokens: 300_000,
		LastMessage: "WP-1 shipped in PR #3.\n  The session can be cleared without losing work."})
	be.warmSweep(55 * time.Minute)
	if x := be.session(); len(be.warmJobs()) != 0 || x.Cache.Decision != "cool" || x.Cache.Note != "it said it can be cleared" {
		t.Fatalf("pinged or compacted a cleared session: %+v %+v", be.warmJobs(), x.Cache)
	}
	// A later real turn that says no such thing is kept warm again.
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: "One more thing."})
	be.hookEv(HookEvent{Event: "Stop", Pending: running, CacheTTL: "1h", ContextTokens: 50_000, LastMessage: "On it; this cannot be cleared yet."})
	be.warmSweep(55 * time.Minute)
	if jobs := be.warmJobs(); len(jobs) != 1 || jobs[0].Body != keepWarmBody {
		t.Fatalf("want a ping: %+v", jobs)
	}
}

// WP-1306: a session that shipped a task not merged yet is compacted before
// its cache cools, though it said it can be cleared; once the task is done,
// or another session took it, it is left to cool.
func TestKeepWarmCompactsAnUnmergedSession(t *testing.T) {
	be := newBabysitEnv(t)
	be.runner(be.admin, "m1", nil)
	task, err := be.s.CreateIssue(be.ctx, be.admin, IssueInput{Project: be.p.Key, Title: "Export rows"})
	if err != nil {
		t.Fatal(err)
	}
	now := be.clockNow().UnixMilli()
	if _, err := be.s.db.W.Exec(`INSERT INTO agent_runs(issue_id, agent, step, user_id, started_at, ended_at, outcome, session_id)
		VALUES (?, 'claude', 'work', ?, ?, ?, 'done', ?)`, task.ID, be.admin.User.ID, now-time.Hour.Milliseconds(), now, be.agent.Session); err != nil {
		t.Fatal(err)
	}
	stop := func() {
		be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: "Ship it."})
		be.hookEv(HookEvent{Event: "Stop", CacheTTL: "1h", ContextTokens: 150_000,
			LastMessage: "WP-1 shipped in PR #3.\n  The session can be cleared without losing work."})
		be.warmSweep(55 * time.Minute)
	}
	stop()
	if jobs, x := be.warmJobs(), be.session(); len(jobs) != 1 || !jobs[0].Compact || x.Cache.Note != "it said it can be cleared; its task isn't merged yet, so compacted before its cache cools" {
		t.Fatalf("not compacted: %+v %+v", jobs, x.Cache)
	}
	// Another session took the task: this one holds nothing unmerged.
	if _, err := be.s.db.W.Exec(`INSERT INTO agent_runs(issue_id, agent, step, user_id, started_at, outcome) VALUES (?, 'claude', 'work', ?, ?, '')`,
		task.ID, be.admin.User.ID, be.clockNow().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if be.s.holdsUnmerged(be.ctx, be.agent.Session) {
		t.Error("a task another session took counts as this one's")
	}
	if _, err := be.s.db.W.Exec(`DELETE FROM agent_runs WHERE session_id IS NULL AND issue_id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	// Merged: the task is done.
	if _, err := be.s.db.W.Exec(`UPDATE issues SET status_id = (SELECT id FROM statuses WHERE category = 'done' ORDER BY id LIMIT 1) WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if be.s.holdsUnmerged(be.ctx, be.agent.Session) {
		t.Fatal("a done task counts as unmerged")
	}
	stop()
	if jobs, x := be.warmJobs(), be.session(); len(jobs) != 1 || x.Cache.Decision != "cool" || x.Cache.Note != "it said it can be cleared" {
		t.Fatalf("after the merge: %+v %+v", jobs, x.Cache)
	}
}

// WP-534: its person's setting compacts instead of pinging, and no ping
// comes after the compaction.
func TestKeepWarmCompactsInsteadWhenItsPersonAsks(t *testing.T) {
	be := newBabysitEnv(t)
	be.runner(be.admin, "m1", nil)
	compact := UserPrefs{CacheWarm: CacheWarmCompact, WeekStart: 1}
	if _, err := be.s.UpdateUser(be.ctx, be.admin, be.admin.id(), UserPatch{Prefs: &compact}); err != nil {
		t.Fatal(err)
	}
	running := &HookPending{Tasks: []PendingTask{{Type: "shell", Status: "running"}}}
	be.hookEv(HookEvent{Event: "Stop", Pending: running, CacheTTL: "1h", LastMessage: "Started.", ContextTokens: 80_000})
	be.warmSweep(55 * time.Minute)
	jobs := be.warmJobs()
	if len(jobs) != 1 || !jobs[0].Compact || jobs[0].Body != "" {
		t.Fatalf("want a compact job: %+v", jobs)
	}
	if x := be.session(); x.Cache.Decision != "compacting" || !strings.Contains(x.Cache.Note, "as its person asks") {
		t.Fatalf("cache = %+v", x.Cache)
	}
	be.hookEv(HookEvent{Event: "UserPromptSubmit", Prompt: "/compact"})
	be.hookEv(HookEvent{Event: "Stop", Pending: running, CacheTTL: "1h", LastMessage: "", ContextTokens: 8_000})
	be.hookEv(HookEvent{Event: "SessionStart", Source: "compact"})
	for range 3 {
		be.warmSweep(55 * time.Minute)
	}
	if jobs := be.warmJobs(); len(jobs) != 1 {
		t.Fatalf("kept warm after compacting: %+v", jobs)
	}
	if x := be.session(); x.Cache.Decision != "compacted" {
		t.Fatalf("cache = %+v", x.Cache)
	}
}
