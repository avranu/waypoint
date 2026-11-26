package main

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

var usageCompactCmd = &command{
	name: "compact", args: "[--since 14d] [--dir CLAUDE_CONFIG_DIR]",
	summary: "what compactions cost, what agents did after them, what one could carry over",
	about: `Reads every main-session Claude Code transcript on this machine (subagents
left out) and prints, for automatic compactions and manual ones (/compact,
typed by a person or by Waypoint):

- how many, the context before and on the first call after, how long they
  took, the summary's size, how many calls followed before the next
  compaction or the end, how many were never used again, and how many
  started with the prompt cache cold (idle longer than the cache lives);
- what the first call after one wrote to and read from the prompt cache,
  beside the first call of a session;
- dollars at list prices. Transcripts do not record the compaction call, so
  it is estimated: the context before at the cache-read price (the input
  price when cold), plus the summary as output times 1.35 for the analysis
  the model writes and Claude Code discards (an assumption). "rewrite" is
  what the first call after wrote to the cache, at the price of the cache
  (five minutes or an hour) each call's write went to. "later reads" is the
  context dropped, times the calls that followed, at the cache-read price;
  "cold restart" is the context before at the cache-write price when the
  next call came after the cache would have died anyway;
- what followed, over the 30 calls after each compaction, beside things to
  compare with: the 30 calls before a compaction (sessions with 60 or more
  calls before it), the calls after a typed prompt with no compaction near
  (a person's or Waypoint's; a keep-warm ping or an interruption is none),
  those after the report of a background task or subagent, and the start
  of a session, which has nothing before it to look up again. A window
  ends early at the next compaction or the session's end, so "those with
  all 30 calls" is the row to hold beside the 30 calls before. A lookup is
  a Read, Grep, Glob or a shell command that only reads (one whose output
  goes to a file writes that file, and counts as an edit of it); a file
  lookup names its file (Read, cat, head, tail, sed -n, less); "seen
  before" counts those whose file the session had already read, shown or
  edited, by file name, and "tokens again" is what they returned
  (characters / 3.6). Orientation is pwd, ls,
  git status|log|diff|branch and wp next|show|brief|worklog|pr show.
  "asked again" is lookups naming no file (a search, a git log) the session
  had already run word for word; "read a transcript" counts windows that
  read a Claude Code transcript; dollars are those calls at list prices,
  split into cache writes, cache reads and output;
- review rounds: the calls after each of Waypoint's review-round prompts,
  by whether a compaction came just before it, with how many arrived after
  the cache had died and how many had another round follow. A row with no
  prompts is left out, so one row means there is nothing to compare with;
- adversarial reviews: the calls after each adversarial-review subagent a
  session started (by the subagent's type, whoever asked for it), by
  whether the session had compacted in the 5 calls before. One started
  within 30 calls of the one before is not counted again. This is a
  signal that happens with and without a compaction, where review-round
  prompts nearly always follow one;
- files looked up again in the 30 calls after a compaction, each file once:
  how many Claude Code had put back in full, how many it had only named,
  how many were still in the last messages it leaves in place after its
  summary, how many the session had only ever seen through shell commands
  (Claude Code restores what its Read tool read), and how many were then
  edited;
- had the compaction carried: a replay. For each set of files a compaction
  could put back (what Claude Code restored, the last 5, 10 or 20 touched,
  those edited, those the next prompt names), what carrying it would have
  added per compaction (a file at most 5,000 tokens, sized by the most the
  session saw of it at once; a file it edited without reading counts
  none) and how many of the files looked up again it holds. A file still
  in the messages Claude Code leaves in place is in no set: it needs no
  carrying. It says what a carry would have covered, not what agents would
  then have done;
- what a summariser of ours would read: the text of the prompts, replies,
  tool calls and results since the compaction before, in full and with
  each tool call cut to 2,000 characters.

Nothing is sent anywhere.`,
	examples: []string{"wp report behavior compact", "wp report behavior compact --since 30d --json"},
	run:      cmdUsageCompact,
}

const (
	defaultCompactSince = 14 * 24 * time.Hour
	cmpCharsPerToken    = 3.6
	// cmpAnalysisFactor is the output a compaction writes per token of
	// summary kept: the model writes an <analysis> block first, which Claude
	// Code drops. Assumed, not measured (WP-1097).
	cmpAnalysisFactor = 1.35
	cmpPlaceboCalls   = 60
	cmpPromptCalls    = 20
	cmpBigContext     = 90000
	cmpStartCalls     = 10
	cmpHistoryCut     = 2000
)

// cmpReport is what wp report behavior compact prints.
type cmpReport struct {
	Days           float64        `json:"days"`
	Transcripts    int            `json:"transcripts"`
	Unreadable     int            `json:"unreadable,omitempty"` // transcripts that could not be opened, left out
	Sessions       int            `json:"sessions"`
	Compacted      int            `json:"sessions_compacted"`
	WindowCalls    int            `json:"window_calls"`
	AnalysisFactor float64        `json:"analysis_factor"`
	Compactions    []cmpTrigger   `json:"compactions"`
	Idle           []cmpIdle      `json:"by_idle"`
	Start          cmpFirstCall   `json:"session_start"`
	Follows        []cmpFollowRow `json:"what_followed"`
	Reviews        []cmpReviewRow `json:"review_rounds"`
	Signals        []cmpReviewRow `json:"adversarial_reviews"`
}

// cmpTrigger is the compactions of one trigger. Token figures are medians.
type cmpTrigger struct {
	Trigger       string       `json:"trigger"`
	N             int          `json:"n"`
	Before        int64        `json:"context_before"`
	After         int64        `json:"context_after"`
	Seconds       int64        `json:"seconds"`
	SummaryTokens int64        `json:"summary_tokens"`
	CallsAfter    int64        `json:"calls_after"`
	NeverUsed     int          `json:"never_used"`
	Cold          int          `json:"cache_cold"`
	First         cmpFirstCall `json:"first_call_after"`
	ReadUSD       float64      `json:"read_usd"`
	SummaryUSD    float64      `json:"summary_usd"`
	RewriteUSD    float64      `json:"rewrite_usd"`
	LaterReadsUSD float64      `json:"later_reads_usd"`
	ColdStartUSD  float64      `json:"cold_restart_usd"`
	UnusedUSD     float64      `json:"never_used_call_usd"`
	CallUSD       float64      `json:"call_usd_median"`
	// HistoryTokens is what a summariser of ours would read: the session
	// since the compaction before, as text; HistoryCut with each tool input
	// and result cut to cmpHistoryCut characters. Medians.
	HistoryTokens int64         `json:"history_tokens"`
	HistoryCut    int64         `json:"history_tokens_cut"`
	Carry         cmpCarryTotal `json:"looked_up_again"`
}

// cmpCarryTotal is the files looked up again in the windows after one
// trigger's compactions, and what each set of files would have covered.
type cmpCarryTotal struct {
	Windows    int           `json:"windows"`
	Files      int           `json:"files"`
	Tokens     int64         `json:"tokens"`
	Restored   int           `json:"restored_by_claude_code"`
	Referenced int           `json:"only_named_by_claude_code"`
	Kept       int           `json:"in_messages_claude_code_kept"`
	ShellOnly  int           `json:"seen_through_shell_only"`
	ThenEdited int           `json:"edited_after"`
	Sets       []cmpCarrySet `json:"had_it_carried"`
	chars      int64
	sets       [carrySets]cmpCarried
}

// cmpCarrySet is one set of files a compaction could carry over.
type cmpCarrySet struct {
	Set           string  `json:"set"`
	Files         float64 `json:"files_per_compaction"`
	Tokens        int64   `json:"tokens_per_compaction"`
	Covered       int     `json:"covered"`
	CoveredTokens int64   `json:"covered_tokens"`
	text          string
}

var cmpCarryNames = [carrySets][2]string{
	{"restored", "what Claude Code restored"},
	{"restored_or_named", "that and the files it only named"},
	{"last_5", "the last 5 files touched"},
	{"last_10", "the last 10"},
	{"last_20", "the last 20"},
	{"edited", "every file edited before"},
	{"prompt_names", "the files the next prompt names"},
	{"edited_or_prompt_names", "edited before or named by it"},
}

// cmpReviewRow is the windows after one kind of review-round prompt.
type cmpReviewRow struct {
	cmpFollowRow
	ContextBefore int64   `json:"context_before"`
	Cold          int     `json:"arrived_cold"`
	Another       int     `json:"another_round_followed"`
	CallUSD       float64 `json:"compaction_call_usd"`
	ctx           []int64
	callUSD       float64
}

var cmpReviewNames = [3][2]string{
	{"compacted", "compacted just before"},
	{"not_compacted", "not compacted, under 90k"},
	{"not_compacted_big", "not compacted, 90k or more"},
}

// cmpSignalNames are the rows of the adversarial reviews a session started.
var cmpSignalNames = [3][2]string{
	{"compacted", "compacted in the 5 calls before"},
	{"not_compacted", "no compaction near, under 90k"},
	{"not_compacted_big", "no compaction near, 90k or more"},
}

// cmpSignalNear is how many calls after a compaction a review still counts
// as started in a compacted session.
const cmpSignalNear = 5

// cmpFirstCall is the prompt-cache tokens of a first call, as medians.
type cmpFirstCall struct {
	N       int   `json:"n"`
	Written int64 `json:"written"`
	Read    int64 `json:"read"`
	NoRead  int   `json:"read_nothing"`
}

// cmpIdle is the compactions that began so long after the session's last call.
type cmpIdle struct {
	Trigger   string `json:"trigger"`
	Idle      string `json:"idle"`
	N         int    `json:"n"`
	NeverUsed int    `json:"never_used"`
	Late      int    `json:"used_after_cache_died"`
}

// cmpFollowRow is the windows after one kind of point, added up.
type cmpFollowRow struct {
	Point          string  `json:"point"`
	N              int     `json:"n"`
	Calls          float64 `json:"calls"`
	Lookups        float64 `json:"lookups"`
	FileLookups    int     `json:"file_lookups"`
	SeenBefore     int     `json:"seen_before"`
	SeenPerWindow  float64 `json:"seen_before_per_window"`
	TokensAgain    int64   `json:"tokens_again_per_window"`
	Orientation    float64 `json:"orientation"`
	Tools          int     `json:"tool_calls"`
	Failed         int     `json:"tool_errors"`
	ReachedEdit    int     `json:"reached_edit"`
	CallsToEdit    int64   `json:"calls_to_edit"`
	LookupsToEdit  int64   `json:"lookups_to_edit"`
	OutputPerCall  int64   `json:"output_per_call"`
	AskedAgain     float64 `json:"asked_again"`
	OwnTranscript  int     `json:"read_a_transcript"`
	Subagents      float64 `json:"subagents"`
	USD            float64 `json:"usd"`
	USDWrite       float64 `json:"usd_cache_writes"`
	USDRead        float64 `json:"usd_cache_reads"`
	USDOut         float64 `json:"usd_output"`
	USDPerCall     float64 `json:"usd_per_call"`
	text           string
	again, agents  int
	usd            cmpUSD
	calls, lookups int
	chars, out     int64
	orient         int
	toEditC        []int64
	toEditL        []int64
}

func (r *cmpFollowRow) add(w cmpWindow) {
	if w.Calls == 0 {
		return
	}
	r.N++
	r.calls += w.Calls
	r.lookups += w.Lookups
	r.FileLookups += w.FileLookups
	r.SeenBefore += w.Reconsults
	r.chars += w.ReconsultChars
	r.orient += w.Orient
	r.Tools += w.Tools
	r.Failed += w.Failed
	r.out += w.Out
	r.again += w.Again
	r.agents += w.Agents
	r.usd.add(w.USD)
	if w.Own {
		r.OwnTranscript++
	}
	if w.Reached {
		r.ReachedEdit++
		r.toEditC = append(r.toEditC, int64(w.CallsToEdit))
		r.toEditL = append(r.toEditL, int64(w.LookupsToEdit))
	}
}

func (r *cmpFollowRow) finish() {
	if r.N == 0 {
		return
	}
	n := float64(r.N)
	r.Calls, r.Lookups = float64(r.calls)/n, float64(r.lookups)/n
	r.SeenPerWindow, r.Orientation = float64(r.SeenBefore)/n, float64(r.orient)/n
	r.TokensAgain = int64(float64(r.chars) / cmpCharsPerToken / n)
	r.CallsToEdit, r.LookupsToEdit = median(r.toEditC), median(r.toEditL)
	r.AskedAgain, r.Subagents, r.USD = float64(r.again)/n, float64(r.agents)/n, r.usd.total()/n
	r.USDWrite, r.USDRead, r.USDOut = r.usd.Write/n, r.usd.Read/n, r.usd.Out/n
	if r.calls > 0 {
		r.OutputPerCall = r.out / int64(r.calls)
		r.USDPerCall = r.usd.total() / float64(r.calls)
	}
}

// the rows of "what followed", in the order printed
const (
	cmpAfterAuto = iota
	cmpAfterAutoFull
	cmpBeforeAuto
	cmpAfterManual
	cmpAfterManualFull
	cmpBeforeManual
	cmpPrompt
	cmpPromptBig
	cmpNotice
	cmpStart
	cmpRows
)

var cmpRowNames = [cmpRows][2]string{
	{"after_auto", "after an automatic compaction"},
	{"after_auto_full", "  those with all 30 calls"},
	{"before_auto", "  the 30 calls before one"},
	{"after_manual", "after a manual compaction"},
	{"after_manual_full", "  those with all 30 calls"},
	{"before_manual", "  the 30 calls before one"},
	{"prompt", "after a typed prompt, no compaction"},
	{"prompt_big", "  those at 90k context or more"},
	{"notice", "after a background task's report"},
	{"start", "session start"},
}

var cmpIdleBuckets = []struct {
	name string
	upTo int64
}{{"under 5m", 300}, {"5-45m", 2700}, {"45-60m", 3600}, {"over 60m", 1 << 62}}

// cmpTriggerAcc gathers one trigger's compactions.
type cmpTriggerAcc struct {
	t                                      cmpTrigger
	before, after, ms, summary, callsAfter []int64
	written, read, callMicro               []int64
	history, historyCut                    []int64
	idle                                   [4]cmpIdle
}

// cmpAcc gathers the report across sessions.
type cmpAcc struct {
	rep                     cmpReport
	trig                    map[string]*cmpTriggerAcc
	rows                    [cmpRows]cmpFollowRow
	reviews                 [3]cmpReviewRow
	signals                 [3]cmpReviewRow
	startWritten, startRead []int64
	prices                  []burnPrice
}

func newCmpAcc(prices []burnPrice) *cmpAcc {
	a := &cmpAcc{trig: map[string]*cmpTriggerAcc{}, prices: prices}
	for _, t := range []string{"auto", "manual"} {
		ta := &cmpTriggerAcc{t: cmpTrigger{Trigger: t}}
		for i, b := range cmpIdleBuckets {
			ta.idle[i] = cmpIdle{Trigger: t, Idle: b.name}
		}
		a.trig[t] = ta
	}
	for i, n := range cmpRowNames {
		a.rows[i].Point, a.rows[i].text = n[0], n[1]
	}
	for i, n := range cmpReviewNames {
		a.reviews[i].Point, a.reviews[i].text = n[0], n[1]
	}
	for i, n := range cmpSignalNames {
		a.signals[i].Point, a.signals[i].text = n[0], n[1]
	}
	return a
}

// session adds one session's events.
func (a *cmpAcc) session(ev []cmpEvent) {
	var calls, comps []int
	hourCache := false
	for i := range ev {
		switch ev[i].kind {
		case 'c':
			calls = append(calls, i)
			hourCache = hourCache || ev[i].cw1h > 0
		case 'b':
			comps = append(comps, i)
		}
	}
	if len(calls) == 0 {
		return
	}
	a.rep.Sessions++
	if len(comps) > 0 {
		a.rep.Compacted++
	}
	ttl := int64(300_000)
	if hourCache {
		ttl = 3_600_000
	}
	seen := cmpFirstSeen(ev)
	usd := func(e *cmpEvent) cmpUSD {
		p := burnPriceFor(a.prices, e.model)
		hour := min(e.cw1h, e.cw)
		if !e.split && hourCache {
			hour = e.cw
		}
		return cmpUSD{Write: (float64(hour)*p.CacheWrite1h + float64(e.cw-hour)*p.CacheWrite5m) / 1e6, Read: float64(e.cr) * p.CacheRead / 1e6, Out: float64(e.out) * p.Output / 1e6, In: float64(e.in) * p.Input / 1e6}
	}
	end := len(ev)
	if len(comps) > 0 {
		end = comps[0]
	}
	if len(calls) >= cmpStartCalls {
		first := &ev[calls[0]]
		a.rows[cmpStart].add(cmpFollow(ev, -1, end, seen, usd))
		a.rep.Start.N++
		a.startWritten, a.startRead = append(a.startWritten, first.cw), append(a.startRead, first.cr)
		if first.cr == 0 {
			a.rep.Start.NoRead++
		}
	}
	for n, at := range comps {
		from, to := -1, len(ev)
		if n > 0 {
			from = comps[n-1]
		}
		if n+1 < len(comps) {
			to = comps[n+1]
		}
		a.compaction(ev, from, at, to, cmpBetween(calls, from, at), cmpBetween(calls, at, to), ttl, seen, usd)
	}
	a.reviewRounds(ev, comps, ttl, seen, usd)
	a.reviewSignals(ev, comps, seen, usd)
	seenCalls, nextComp := 0, 0
	for i := range ev {
		switch ev[i].kind {
		case 'c':
			seenCalls++
		case 'p':
			if ev[i].pk == 'x' || seenCalls < cmpPromptCalls || cmpNearCompaction(ev, i) {
				continue
			}
			for nextComp < len(comps) && comps[nextComp] < i {
				nextComp++
			}
			to := len(ev)
			if nextComp < len(comps) {
				to = comps[nextComp]
			}
			w := cmpFollow(ev, i, to, seen, usd)
			if ev[i].pk == 'n' {
				a.rows[cmpNotice].add(w)
				continue
			}
			a.rows[cmpPrompt].add(w)
			if prev := lastOfKind(ev[:i], "c"); prev >= 0 && ev[prev].ctx() >= cmpBigContext {
				a.rows[cmpPromptBig].add(w)
			}
		}
	}
}

// cmpBetween is the calls strictly between two events.
func cmpBetween(calls []int, from, to int) []int {
	var out []int
	for _, c := range calls {
		if c > from && c < to {
			out = append(out, c)
		}
	}
	return out
}

// cmpNearCompaction reports a compaction within the eight events before i.
func cmpNearCompaction(ev []cmpEvent, i int) bool {
	for j := max(0, i-8); j < i; j++ {
		if ev[j].kind == 'b' {
			return true
		}
	}
	return false
}

// reviewRounds adds the windows after Waypoint's review-round prompts: those
// it compacted the session for, and those it did not.
func (a *cmpAcc) reviewRounds(ev []cmpEvent, comps []int, ttl int64, seen map[string]int, usd func(*cmpEvent) cmpUSD) {
	nextComp := 0
	for i := range ev {
		if ev[i].kind != 'p' || !ev[i].review {
			continue
		}
		prev := lastOfKind(ev[:i], "cb")
		if prev < 0 {
			continue
		}
		for nextComp < len(comps) && comps[nextComp] < i {
			nextComp++
		}
		to := len(ev)
		if nextComp < len(comps) {
			to = comps[nextComp]
		}
		w := cmpFollow(ev, i, to, seen, usd)
		if w.Calls == 0 {
			continue
		}
		row, ctx := 1, ev[prev].ctx()
		switch {
		case ev[prev].kind == 'b':
			row, ctx = 0, ev[prev].pre
		case ctx >= cmpBigContext:
			row = 2
		}
		r := &a.reviews[row]
		r.add(w)
		r.ctx = append(r.ctx, ctx)
		r.callUSD += ev[prev].usd
		if last := lastOfKind(ev[:i], "c"); last >= 0 && ev[last].ts > 0 && ev[i].ts-ev[last].ts > ttl {
			r.Cold++
		}
		for j := i + 1; j < len(ev); j++ {
			if ev[j].kind == 'p' && ev[j].review {
				r.Another++
				break
			}
		}
	}
}

// reviewSignals adds the windows after the adversarial-review subagents a
// session started, by whether it had just compacted: the same signal with
// and without a compaction before it. Context is that of the call
// that started the review.
func (a *cmpAcc) reviewSignals(ev []cmpEvent, comps []int, seen map[string]int, usd func(*cmpEvent) cmpUSD) {
	nextComp, calls, lastSignal := 0, 0, -cmpWindowCalls
	lastComp, callsAtComp := -1, 0
	for i := range ev {
		switch ev[i].kind {
		case 'c':
			calls++
		case 'b':
			lastComp, callsAtComp = i, calls
		}
		if ev[i].kind != 't' || !ev[i].adversarial || calls-lastSignal < cmpWindowCalls {
			continue
		}
		prev := lastOfKind(ev[:i], "c")
		if prev < 0 {
			continue
		}
		for nextComp < len(comps) && comps[nextComp] < i {
			nextComp++
		}
		to := len(ev)
		if nextComp < len(comps) {
			to = comps[nextComp]
		}
		w := cmpFollow(ev, i, to, seen, usd)
		if w.Calls == 0 {
			continue
		}
		lastSignal = calls
		row, ctx := 1, ev[prev].ctx()
		switch {
		case lastComp >= 0 && calls-callsAtComp <= cmpSignalNear:
			row = 0
		case ctx >= cmpBigContext:
			row = 2
		}
		r := &a.signals[row]
		r.add(w)
		r.ctx = append(r.ctx, ctx)
		seenCalls := 0
		for j := i + 1; j < len(ev); j++ {
			if ev[j].kind == 'c' {
				seenCalls++
			}
			if ev[j].kind == 't' && ev[j].adversarial && seenCalls >= cmpWindowCalls {
				r.Another++
				break
			}
		}
	}
}

// compaction adds the compaction at ev[at], whose segment began after
// ev[from] and ends at to.
func (a *cmpAcc) compaction(ev []cmpEvent, from, at, to int, before, after []int, ttl int64, seen map[string]int, usd func(*cmpEvent) cmpUSD) {
	c := &ev[at]
	ta := a.trig[c.trigger]
	t := &ta.t
	t.N++
	if full, cut := cmpHistory(ev, from, at, cmpHistoryCut); full > 0 {
		ta.history = append(ta.history, int64(float64(full)/cmpCharsPerToken))
		ta.historyCut = append(ta.historyCut, int64(float64(cut)/cmpCharsPerToken))
	}
	ta.callsAfter = append(ta.callsAfter, int64(len(after)))
	if c.pre > 0 {
		ta.before = append(ta.before, c.pre)
	}
	if c.durMs > 0 {
		ta.ms = append(ta.ms, c.durMs)
	}
	summary := int64(float64(c.summaryChars) / cmpCharsPerToken)
	if summary > 0 {
		ta.summary = append(ta.summary, summary)
	}
	var last, first *cmpEvent
	model := ""
	if len(before) > 0 {
		last = &ev[before[len(before)-1]]
		model = last.model
	}
	if len(after) > 0 {
		first = &ev[after[0]]
		if model == "" {
			model = first.model
		}
	}
	price := burnPriceFor(a.prices, model)
	write := price.CacheWrite5m
	if ttl > 300_000 {
		write = price.CacheWrite1h
	}

	cold, bucket := false, -1
	if last != nil && last.ts > 0 && c.ts > 0 {
		idle := c.ts - c.durMs - last.ts
		cold = idle > ttl
		for i, b := range cmpIdleBuckets {
			if idle/1000 < b.upTo {
				bucket = i
				break
			}
		}
	}
	if cold {
		t.Cold++
	}
	late := first != nil && last != nil && last.ts > 0 && first.ts-last.ts > ttl
	if bucket >= 0 {
		ta.idle[bucket].N++
		if first == nil {
			ta.idle[bucket].NeverUsed++
		}
		if late {
			ta.idle[bucket].Late++
		}
	}

	row, full, placebo := cmpAfterAuto, cmpAfterAutoFull, cmpBeforeAuto
	if c.trigger == "manual" {
		row, full, placebo = cmpAfterManual, cmpAfterManualFull, cmpBeforeManual
	}
	if len(before) >= cmpPlaceboCalls {
		a.rows[placebo].add(cmpFollow(ev, before[len(before)-cmpWindowCalls]-1, at, seen, usd))
	}

	read := float64(c.pre) * price.CacheRead / 1e6
	if cold {
		read = float64(c.pre) * price.Input / 1e6
	}
	out := float64(summary) * cmpAnalysisFactor * price.Output / 1e6
	t.ReadUSD += read
	t.SummaryUSD += out
	c.usd = read + out
	if c.pre > 0 {
		ta.callMicro = append(ta.callMicro, int64((read+out)*1e6))
	}
	if first == nil {
		t.NeverUsed++
		t.UnusedUSD += read + out
		return
	}
	ta.after = append(ta.after, first.ctx())
	ta.written, ta.read = append(ta.written, first.cw), append(ta.read, first.cr)
	if first.cr == 0 {
		t.First.NoRead++
	}
	t.RewriteUSD += usd(first).Write
	held := c.pre
	if last != nil {
		held = last.ctx()
	}
	if dropped := held - first.ctx(); dropped > 0 {
		t.LaterReadsUSD += float64(dropped) * float64(len(after)) * price.CacheRead / 1e6
	}
	if late {
		t.ColdStartUSD += float64(c.pre) * write / 1e6
	}

	w := cmpFollow(ev, at, to, seen, usd)
	a.rows[row].add(w)
	if w.Calls == cmpWindowCalls {
		a.rows[full].add(w)
	}

	carry := cmpReplay(ev, at, to)
	ct := &t.Carry
	ct.Windows++
	ct.Files += carry.Again
	ct.chars += carry.AgainChars
	ct.Restored += carry.Restored
	ct.Referenced += carry.Referenced
	ct.Kept += carry.Kept
	ct.ShellOnly += carry.ShellOnly
	ct.ThenEdited += carry.ThenEdited
	for i, s := range carry.Sets {
		ct.sets[i].Files += s.Files
		ct.sets[i].Chars += s.Chars
		ct.sets[i].Covered += s.Covered
		ct.sets[i].CoveredChars += s.CoveredChars
	}
}

func (a *cmpAcc) report() *cmpReport {
	rep := &a.rep
	rep.WindowCalls, rep.AnalysisFactor = cmpWindowCalls, cmpAnalysisFactor
	for _, name := range []string{"auto", "manual"} {
		ta := a.trig[name]
		t := ta.t
		t.Before, t.After, t.Seconds = median(ta.before), median(ta.after), median(ta.ms)/1000
		t.SummaryTokens, t.CallsAfter = median(ta.summary), median(ta.callsAfter)
		t.First.N, t.First.Written, t.First.Read = len(ta.written), median(ta.written), median(ta.read)
		t.CallUSD = float64(median(ta.callMicro)) / 1e6
		t.HistoryTokens, t.HistoryCut = median(ta.history), median(ta.historyCut)
		t.Carry.Tokens = int64(float64(t.Carry.chars) / cmpCharsPerToken)
		if n := float64(t.Carry.Windows); n > 0 {
			for i, s := range t.Carry.sets {
				t.Carry.Sets = append(t.Carry.Sets, cmpCarrySet{
					Set: cmpCarryNames[i][0], text: cmpCarryNames[i][1],
					Files: float64(s.Files) / n, Tokens: int64(float64(s.Chars) / cmpCharsPerToken / n),
					Covered: s.Covered, CoveredTokens: int64(float64(s.CoveredChars) / cmpCharsPerToken),
				})
			}
		}
		rep.Compactions = append(rep.Compactions, t)
		for _, b := range ta.idle {
			if b.N > 0 {
				rep.Idle = append(rep.Idle, b)
			}
		}
	}
	rep.Start.Written, rep.Start.Read = median(a.startWritten), median(a.startRead)
	for i := range a.rows {
		a.rows[i].finish()
		rep.Follows = append(rep.Follows, a.rows[i])
	}
	for i := range a.reviews {
		r := &a.reviews[i]
		r.finish()
		r.ContextBefore = median(r.ctx)
		if r.N > 0 {
			r.CallUSD = r.callUSD / float64(r.N)
		}
		rep.Reviews = append(rep.Reviews, *r)
	}
	for i := range a.signals {
		r := &a.signals[i]
		r.finish()
		r.ContextBefore = median(r.ctx)
		rep.Signals = append(rep.Signals, *r)
	}
	return rep
}

func cmdUsageCompact(c *cli, args []string) error {
	fs := c.flags()
	since := fs.String("since", "", "sessions active within `DURATION` (default 14d)")
	dir := fs.String("dir", "", "Claude Code config `DIR` (default CLAUDE_CONFIG_DIR, ~/.claude)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	d, err := parseSince(*since)
	if err != nil {
		return c.usage("%v", err)
	}
	if d == 0 {
		d = defaultCompactSince
	}
	rep, err := buildCompactReport(claudeDir(*dir), d, c.loadBurnPrices(false))
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(rep)
	return c.emit(raw, func() string { return compactText(rep) })
}

func buildCompactReport(claudeRoot string, since time.Duration, prices []burnPrice) (*cmpReport, error) {
	files, err := discoverTranscripts(claudeRoot, since, nil)
	if err != nil {
		return nil, err
	}
	var mains []usageFile
	for _, f := range files {
		if f.Main {
			mains = append(mains, f)
		}
	}
	sessions := make([][]cmpEvent, len(mains))
	failed := make([]bool, len(mains))
	next := make(chan int)
	var wg sync.WaitGroup
	for range max(1, min(runtime.NumCPU(), 8)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				var err error
				sessions[i], err = readCompactFile(mains[i].Path)
				failed[i] = err != nil
			}
		}()
	}
	for i := range mains {
		next <- i
	}
	close(next)
	wg.Wait()
	a := newCmpAcc(prices)
	a.rep.Days, a.rep.Transcripts = since.Hours()/24, len(mains)
	for i, ev := range sessions {
		if failed[i] {
			a.rep.Unreadable++
			continue
		}
		a.session(ev)
	}
	return a.report(), nil
}

func compactText(r *cmpReport) string {
	var b strings.Builder
	total := 0
	for _, t := range r.Compactions {
		total += t.N
	}
	fmt.Fprintf(&b, "%d compactions in %d of %d sessions on this machine, last %s days.\n", total, r.Compacted, r.Sessions, strconv.FormatFloat(r.Days, 'f', -1, 64))
	if r.Unreadable > 0 {
		fmt.Fprintf(&b, "%d transcripts could not be read and are left out.\n", r.Unreadable)
	}
	if total == 0 {
		return b.String()
	}
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "\n\tn\tcontext before\tafter\ttook\tsummary\tcalls after\tnever used\tcache cold")
	for _, t := range r.Compactions {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%ds\t%d\t%d\t%d\t%d\n", cmpTriggerName(t.Trigger), t.N, kTokens(t.Before), kTokens(t.After), t.Seconds, t.SummaryTokens, t.CallsAfter, t.NeverUsed, t.Cold)
	}
	_ = tw.Flush()
	b.WriteString("(medians, tokens)\n")

	tw = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "\nfirst call\tn\twrote to cache\tread from cache\tread nothing")
	for _, t := range r.Compactions {
		_, _ = fmt.Fprintf(tw, "after %s\t%d\t%s\t%s\t%d\n", cmpTriggerName(t.Trigger), t.First.N, kTokens(t.First.Written), kTokens(t.First.Read), t.First.NoRead)
	}
	_, _ = fmt.Fprintf(tw, "of a session\t%d\t%s\t%s\t%d\n", r.Start.N, kTokens(r.Start.Written), kTokens(r.Start.Read), r.Start.NoRead)
	_ = tw.Flush()

	tw = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "\ndollars, estimated\tcall: read\tsummary\trewrite\tavoided: later reads\tcold restart\tcalls never used\tcall, median")
	for _, t := range r.Compactions {
		_, _ = fmt.Fprintf(tw, "%s\t$%.0f\t$%.0f\t$%.0f\t$%.0f\t$%.0f\t$%.0f\t$%.2f\n", cmpTriggerName(t.Trigger), t.ReadUSD, t.SummaryUSD, t.RewriteUSD, t.LaterReadsUSD, t.ColdStartUSD, t.UnusedUSD, t.CallUSD)
	}
	_ = tw.Flush()

	if len(r.Idle) > 0 {
		tw = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "\nidle before\t\tn\tnever used\tused after the cache died")
		for _, i := range r.Idle {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\n", cmpTriggerName(i.Trigger), i.Idle, i.N, i.NeverUsed, i.Late)
		}
		_ = tw.Flush()
	}

	tw = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "\nwhat followed, %d calls\tn\tcalls\tlookups\tfile lookups\tseen before\tper window\ttokens again\torientation\ttool errors\treached an edit\tcalls to it\tlookups to it\toutput/call\n", r.WindowCalls)
	for _, f := range r.Follows {
		if f.N == 0 {
			continue
		}
		seen, per, tokens := fmt.Sprintf("%d (%s)", f.SeenBefore, cmpShare(f.SeenBefore, f.FileLookups)), fmt.Sprintf("%.2f", f.SeenPerWindow), strconv.FormatInt(f.TokensAgain, 10)
		if f.Point == cmpRowNames[cmpStart][0] {
			seen, per, tokens = "-", "-", "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%.1f\t%.1f\t%d\t%s\t%s\t%s\t%.2f\t%s\t%s\t%d\t%d\t%d\n", f.text, f.N, f.Calls, f.Lookups, f.FileLookups, seen,
			per, tokens, f.Orientation, cmpShare(f.Failed, f.Tools), cmpShare(f.ReachedEdit, f.N), f.CallsToEdit, f.LookupsToEdit, f.OutputPerCall)
	}
	_ = tw.Flush()
	b.WriteString("(per window unless a count; calls and lookups to the edit are medians; a window ends early at the next compaction or the session's end)\n")

	tw = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "\nwhat followed, more\tn\tasked again\tread a transcript\tsubagents\tdollars\tcache writes\treads\toutput\tper call")
	for _, f := range r.Follows {
		if f.N == 0 {
			continue
		}
		asked := fmt.Sprintf("%.2f", f.AskedAgain)
		if f.Point == cmpRowNames[cmpStart][0] {
			asked = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%d (%s)\t%.2f\t$%.2f\t$%.2f\t$%.2f\t$%.2f\t$%.3f\n", f.text, f.N, asked, f.OwnTranscript, cmpShare(f.OwnTranscript, f.N), f.Subagents, f.USD, f.USDWrite, f.USDRead, f.USDOut, f.USDPerCall)
	}
	_ = tw.Flush()

	if cmpAny(r.Reviews) {
		tw = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintf(tw, "\nreview rounds, %d calls\tn\tcontext before\tarrived cold\tcalls\tdollars\tcompaction call\tseen before\ttokens again\ttool errors\treached an edit\tcalls to it\toutput/call\tanother round followed\n", r.WindowCalls)
		for _, f := range r.Reviews {
			if f.N == 0 {
				continue
			}
			_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%.1f\t$%.2f\t$%.2f\t%.2f\t%d\t%s\t%s\t%d\t%d\t%s\n", f.text, f.N, kTokens(f.ContextBefore), f.Cold, f.Calls, f.USD, f.CallUSD,
				f.SeenPerWindow, f.TokensAgain, cmpShare(f.Failed, f.Tools), cmpShare(f.ReachedEdit, f.N), f.CallsToEdit, f.OutputPerCall, cmpShare(f.Another, f.N))
		}
		_ = tw.Flush()
	}

	if cmpAny(r.Signals) {
		tw = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintf(tw, "\nafter an adversarial review was started, %d calls\tn\tcontext at it\tcalls\tdollars\tper call\tlookups\tseen before\ttokens again\ttool errors\treached an edit\tcalls to it\toutput/call\tanother followed\n", r.WindowCalls)
		for _, f := range r.Signals {
			if f.N == 0 {
				continue
			}
			_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%.1f\t$%.2f\t$%.3f\t%.1f\t%.2f\t%d\t%s\t%s\t%d\t%d\t%s\n", f.text, f.N, kTokens(f.ContextBefore), f.Calls, f.USD, f.USDPerCall, f.Lookups,
				f.SeenPerWindow, f.TokensAgain, cmpShare(f.Failed, f.Tools), cmpShare(f.ReachedEdit, f.N), f.CallsToEdit, f.OutputPerCall, cmpShare(f.Another, f.N))
		}
		_ = tw.Flush()
		b.WriteString("(a session's adversarial-review subagents by type; one within 30 calls of the last counted is left out)\n")
	}

	tw = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "\nfiles looked up again\twindows\tfiles\tper window\ttheir tokens\tClaude Code had restored\thad only named\thad kept in its last messages\tseen through the shell only\tedited after")
	for _, t := range r.Compactions {
		c := t.Carry
		if c.Windows == 0 {
			continue
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%.2f\t%s\t%d (%s)\t%d (%s)\t%d (%s)\t%d (%s)\t%d (%s)\n", cmpTriggerName(t.Trigger), c.Windows, c.Files, float64(c.Files)/float64(c.Windows), kTokens(c.Tokens),
			c.Restored, cmpShare(c.Restored, c.Files), c.Referenced, cmpShare(c.Referenced, c.Files), c.Kept, cmpShare(c.Kept, c.Files), c.ShellOnly, cmpShare(c.ShellOnly, c.Files), c.ThenEdited, cmpShare(c.ThenEdited, c.Files))
	}
	_ = tw.Flush()

	tw = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "\nhad the compaction carried\t\tfiles\ttokens\tof the files looked up again\ttheir tokens")
	for _, t := range r.Compactions {
		for _, s := range t.Carry.Sets {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%.1f\t%s\t%d (%s)\t%s\n", cmpTriggerName(t.Trigger), s.text, s.Files, kTokens(s.Tokens), s.Covered, cmpShare(s.Covered, t.Carry.Files), kTokens(s.CoveredTokens))
		}
	}
	_ = tw.Flush()
	b.WriteString("(files and tokens carried are per compaction, a file at most 5,000 tokens; covered are totals; a file still in the messages Claude Code kept is in no set)\n")

	tw = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "\na summariser of ours would read\tthe history as text\twith tool calls cut to %d characters\n", cmpHistoryCut)
	for _, t := range r.Compactions {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", cmpTriggerName(t.Trigger), kTokens(t.HistoryTokens), kTokens(t.HistoryCut))
	}
	_ = tw.Flush()
	b.WriteString("(median tokens since the compaction before; the system prompt and tool definitions are no part of it)\n")
	return b.String()
}

func cmpAny(rows []cmpReviewRow) bool {
	for _, r := range rows {
		if r.N > 0 {
			return true
		}
	}
	return false
}

func cmpTriggerName(t string) string {
	if t == "auto" {
		return "automatic"
	}
	return t
}

func cmpShare(n, of int) string {
	if of == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(of))
}
