package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Counts what agents do before their first edit (wp code, wp brief, search nudge), to compare adoption across time windows.

// sugOrientation counts transcripts (main threads and subagents) with at
// least one call.
type sugOrientation struct {
	Transcripts int   `json:"transcripts"`
	Mains       int   `json:"mains"`
	Edited      int   `json:"edited"`                 // reached an edit or a commit
	CallsMain   int64 `json:"calls_median_main"`      // calls before the first edit, median
	CallsSub    int64 `json:"calls_median_subagent"`  // the same for subagents
	CtxMedian   int64 `json:"context_median"`         // context those calls carried, summed per transcript, median
	ChainShare  int   `json:"search_slice_share_pct"` // of orientation calls: searches and slices
	Chains      int   `json:"chains"`                 // transcripts that ran a search chain before their first edit
	CodeTools   int   `json:"code_tools"`             // transcripts that ran wp code or wp brief
	Next        int   `json:"wp_next"`                // ran wp next, which prints a brief
	Nudged      int   `json:"nudged"`                 // were shown the search-chain nudge
	Followed    int   `json:"followed"`               // and ran wp code or wp brief after it
	// From the call the nudge told on, or would have told on (chainPoint),
	// to the first edit: told against not told. Not told is every transcript
	// before the nudge shipped (WP-183, 2026-11-04) and the search_nudge
	// experiment's off arm, so a window before it set against one after
	// compares transcripts picked by the same rule (WP-981).
	Told   sugAfterPoint `json:"after_nudge"`
	Untold sugAfterPoint `json:"after_point_untold"`
}

// sugAfterPoint: transcripts that reached the nudge's point before their
// first edit, and what they did from there to it (or to their end).
type sugAfterPoint struct {
	Transcripts int   `json:"transcripts"`
	Edited      int   `json:"edited"`         // reached an edit
	Calls       int64 `json:"calls_median"`   // calls after the point
	Ctx         int64 `json:"context_median"` // context those calls carried, summed per transcript
	CodeTools   int   `json:"code_tools"`     // ran wp code or wp brief after the point
}

// afterPoints gathers one arm's transcripts for its medians.
type afterPoints struct {
	sugAfterPoint
	calls, ctx []int64
}

func (a *afterPoints) done() sugAfterPoint {
	a.Calls, a.Ctx = median(a.calls), median(a.ctx)
	return a.sugAfterPoint
}

// chainPoint is the index of the call the search-chain nudge told on before
// the first edit (calls[end]), or of the call it would have told on had it
// been on: the one that brings the chain to chainFinds searches in chainCalls
// searches and slices, with no wp code or wp brief before it, as the hook
// counts (chainNudge). -1: neither. The hook also skips background calls and
// calls outside a checkout, and tells later when another note takes the
// call; transcripts don't record those, so the would-have point can come a
// little early.
func chainPoint(calls []sugCall, end int) int {
	for i := range calls {
		if calls[i].Nudge != "" {
			if i < end {
				return i
			}
			return -1
		}
	}
	finds, steps := 0, 0
	for i := range end {
		if calls[i].Tool == "Grep" {
			continue // Grep is never nudged: the nudge hook matches only Bash, Read and Monitor (hook.go:106)
		}
		switch callStep(calls[i]) {
		case stepWP:
			return -1
		case stepFind:
			finds++
			steps++
		case stepLook:
			steps++
		}
		if finds >= chainFinds && steps >= chainCalls {
			return i
		}
	}
	return -1
}

// add counts a transcript's calls after its chain point (p) up to the first
// edit (end): calls the agent made in the same request as the point's call
// didn't see the note, so they don't count.
func (a *afterPoints) add(calls []sugCall, p, end int) {
	a.Transcripts++
	if end < len(calls) {
		a.Edited++
	}
	var n, tokens int64
	code := false
	seen := map[string]bool{}
	for _, c := range calls[p+1 : end] {
		if c.msg != "" && c.msg == calls[p].msg {
			continue
		}
		n++
		if c.msg == "" || !seen[c.msg] {
			tokens += c.Ctx
			seen[c.msg] = true
		}
		code = code || callStep(c) == stepWP
	}
	a.CodeTools += b2i(code)
	a.calls, a.ctx = append(a.calls, n), append(a.ctx, tokens)
}

// editCall: the call ends orientation (X7's rule: the first Edit, Write or
// NotebookEdit, or a git commit).
func editCall(c sugCall) bool {
	switch c.Tool {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return true
	case "Bash":
		for _, h := range c.Heads {
			if h == "git commit" {
				return true
			}
		}
		return bashWrites(c.Command)
	}
	return false
}

// bashWrites: the command edits files in place (sed -i, perl -i, tee,
// patch, git apply).
func bashWrites(cmd string) bool {
	for _, seg := range shellWords(cmd) {
		w := stripWords(seg)
		if len(w) == 0 {
			continue
		}
		switch filepath.Base(w[0]) {
		case "sed", "perl":
			if inPlaceFlag(filepath.Base(w[0]), w[1:]) {
				return true
			}
		case "tee":
			return true
		case "patch":
			if !patchReadOnly(w[1:]) {
				return true
			}
		case "git":
			if gitApplies(w[1:]) {
				return true
			}
		}
	}
	return false
}

// patchReadOnly: patch --dry-run, -C/--check only test whether a patch applies.
func patchReadOnly(args []string) bool {
	for _, a := range args {
		switch a {
		case "--dry-run", "--check", "-C":
			return true
		}
	}
	return false
}

// callStep is the chain step of a recorded call (see chainStep).
func callStep(c sugCall) string {
	switch c.Tool {
	case "Grep":
		return stepFind
	case "Read":
		return chainStep("Read", "", c.Path)
	case "Bash":
		return chainStep("Bash", c.Command, "")
	}
	return ""
}

func wpNextCall(c sugCall) bool {
	for _, seg := range shellWords(c.Command) {
		if w := stripWords(seg); len(w) > 1 && filepath.Base(w[0]) == "wp" && w[1] == "next" {
			return true
		}
	}
	return false
}

func orientation(sessions []*sugSession) *sugOrientation {
	o := &sugOrientation{}
	var mains, subs, ctx []int64
	var orientCalls, stepCalls int
	var told, untold afterPoints
	for _, s := range sessions {
		if len(s.Calls) == 0 {
			continue
		}
		end := len(s.Calls)
		for i, c := range s.Calls {
			if editCall(c) {
				end = i
				break
			}
		}
		if p := chainPoint(s.Calls, end); p >= 0 && s.Calls[p].Nudge != "" {
			told.add(s.Calls, p, end)
		} else if p >= 0 {
			untold.add(s.Calls, p, end)
		}
		o.Transcripts++
		if s.Kind == "main" {
			o.Mains++
		}
		var n, finds, steps int
		var tokens int64
		edited, chain, code, next, nudged, followed := false, false, false, false, false, false
		seenMsg := map[string]bool{}
		for _, c := range s.Calls {
			if edited || editCall(c) {
				edited = true
				continue
			}
			if c.Tool == "Bash" && wpNextCall(c) {
				next = true
			}
			step := callStep(c)
			if step == stepWP {
				code = true
				followed = followed || nudged
			}
			if c.Nudge != "" {
				nudged = true
			}
			n++
			// parallel calls of one assistant message share one request
			if c.msg == "" || !seenMsg[c.msg] {
				tokens += c.Ctx
				seenMsg[c.msg] = true
			}
			switch step {
			case stepFind:
				finds++
				steps++
			case stepLook:
				steps++
			}
			chain = chain || finds >= chainFinds && steps >= chainCalls
		}
		orientCalls += n
		stepCalls += steps
		if s.Kind == "main" {
			mains = append(mains, int64(n))
		} else {
			subs = append(subs, int64(n))
		}
		ctx = append(ctx, tokens)
		o.Edited += b2i(edited)
		o.Chains += b2i(chain)
		o.CodeTools += b2i(code)
		o.Next += b2i(next)
		o.Nudged += b2i(nudged)
		o.Followed += b2i(followed)
	}
	if o.Transcripts == 0 {
		return nil
	}
	o.CallsMain, o.CallsSub, o.CtxMedian = median(mains), median(subs), median(ctx)
	o.Told, o.Untold = told.done(), untold.done()
	if orientCalls > 0 {
		o.ChainShare = stepCalls * 100 / orientCalls
	}
	return o
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// orientationText is the report's lines about orientation.
func orientationText(o *sugOrientation) string {
	if o == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Before the first edit (%d of %s reached one): a median %d calls in a session and %d in a subagent, %s tokens of context per transcript; %d%% of the calls grep, sed, cat or Read.\n",
		o.Edited, plural1(o.Transcripts, "transcript"), o.CallsMain, o.CallsSub, tok(o.CtxMedian), o.ChainShare)
	fmt.Fprintf(&b, "%d ran a search chain before it (%d+ searches in %d+ search or read calls); wp code or wp brief ran in %d, wp next in %d", o.Chains, chainFinds, chainCalls, o.CodeTools, o.Next)
	if o.Nudged > 0 {
		fmt.Fprintf(&b, "; %d were shown the search-chain nudge and %d ran wp code or wp brief after it", o.Nudged, o.Followed)
	}
	b.WriteString(".\n")
	for _, arm := range []struct {
		name string
		a    sugAfterPoint
	}{{"told", o.Told}, {"not told", o.Untold}} {
		if a := arm.a; a.Transcripts > 0 {
			fmt.Fprintf(&b, "From the nudge's point to the first edit, %s: %s (%d reached an edit), a median %d calls and %s tokens of context; %d ran wp code or wp brief.\n",
				arm.name, plural1(a.Transcripts, "transcript"), a.Edited, a.Calls, tok(a.Ctx), a.CodeTools)
		}
	}
	return b.String()
}

// adviceOrientation: most transcripts orient by search chains and few use
// the code index.
func adviceOrientation(in *sugInput) *sugAdvice {
	o := orientation(in.Sessions)
	if o == nil || o.Chains < sugMinSessions || o.CodeTools*2 >= o.Chains {
		return nil
	}
	ev := []string{fmt.Sprintf("%d of %s searched and read file slices call by call (%d searches in %d calls) before their first edit; %d ran wp code or wp brief.",
		o.Chains, plural1(o.Transcripts, "transcript"), chainFinds, chainCalls, o.CodeTools)}
	return &sugAdvice{ID: "orientation", Level: "info", Title: "Agents find their way by grep → sed chains", Evidence: ev,
		Action: "Each call re-reads the whole context. wp code find returns the best lines of every matching file in one call, wp code show FILE:SYMBOL one function, wp brief an orientation for a task or question; wp hook claude installs the nudge that points agents at them mid-chain."}
}

// inPlaceFlag: args of sed or perl carry -i / --in-place. Short options are
// read letter by letter, stopping at one that takes a value (perl -Ilib is
// not -i).
func inPlaceFlag(prog string, args []string) bool {
	valued := "efl"
	if prog == "perl" {
		valued = "IeEMmxdDCFV"
	}
	for k := 0; k < len(args); k++ {
		a := args[k]
		if a == "--in-place" || strings.HasPrefix(a, "--in-place=") {
			return true
		}
		if !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "--") || len(a) < 2 {
			continue
		}
		for j := 1; j < len(a); j++ {
			if a[j] == 'i' {
				return true
			}
			if strings.IndexByte(valued, a[j]) >= 0 {
				if j == len(a)-1 {
					k++
				}
				break
			}
		}
	}
	return false
}

// gitApplies: git's args (after the program name) run an apply that edits
// the working tree: global options skipped, --check and the like excluded
// unless --apply is also given.
func gitApplies(args []string) bool {
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		switch args[i] {
		case "-C", "-c", "--git-dir", "--work-tree", "--namespace", "--exec-path", "--super-prefix", "--config-env":
			i++
		}
	}
	if i >= len(args) || args[i] != "apply" {
		return false
	}
	readOnly := false
	for _, a := range args[i+1:] {
		switch a {
		case "--apply":
			return true
		case "--check", "--stat", "--numstat", "--summary":
			readOnly = true
		}
	}
	return !readOnly
}
