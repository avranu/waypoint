package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// `wp usage sync` (WP-D20): reads Claude Code's transcripts, counts what
// the sessions used, and uploads compact hour-bucketed records to the
// server. The model is never involved and nothing is asked of it.

// usageSyncOptions says what to sync. The runner (WP-D17) calls usageSync
// with these too.
type usageSyncOptions struct {
	All       bool          // every transcript, however old (still only what is new in each)
	Since     time.Duration // otherwise only transcripts modified this recently
	Hook      bool          // from a Claude Code hook: stdin names the session; quiet
	DryRun    bool          // parse and count; upload nothing, remember nothing
	Dir       string        // the Claude config directory (CLAUDE_CONFIG_DIR, else ~/.claude)
	Files     []string      // only these transcripts (and their subagents)
	Inventory bool          // also report what is configured on this machine
	NoArchive bool          // leave the transcript archive upload out (WP-1466)
	Out       io.Writer     // progress lines, or nil
}

// usageSyncResult is what a sync did.
type usageSyncResult struct {
	Machine   string         `json:"machine"`
	DryRun    bool           `json:"dry_run"`
	Files     int            `json:"files"`      // transcripts considered
	FilesRead int            `json:"files_read"` // with something new
	Bytes     int64          `json:"bytes"`
	Chunks    int            `json:"chunks"`
	Accepted  int            `json:"accepted"`
	Duplicate int            `json:"duplicate"`
	Messages  int64          `json:"messages"`
	Tokens    usageTokenSums `json:"tokens"`
	ToolCalls int64          `json:"tool_calls"`
	Refusals  int64          `json:"refusals"`
	Risky     int            `json:"risky"`  // risky commands found (WP-647)
	Traces    int            `json:"traces"` // task segments sent (WP-250)
	// TracesDropped: segments the server could not place: a task it doesn't have or you can't read.
	TracesDropped int              `json:"traces_dropped,omitempty"`
	Sessions      int              `json:"sessions"`
	Parsed        int64            `json:"parsed"`
	Ignored       int64            `json:"ignored"`
	Skipped       int64            `json:"skipped"`
	Unknown       map[string]int64 `json:"unknown,omitempty"`
	Inventory     map[string]int   `json:"inventory,omitempty"`
	SharedIDs     int64            `json:"messages_in_several_files,omitempty"` // dry runs: the same message in two files
	Elapsed       string           `json:"elapsed"`
	// Archive: the transcript archive upload (WP-1466); ArchiveError when it
	// could not run (the counts above are unaffected).
	Archive      *archiveResult `json:"archive,omitempty"`
	ArchiveError string         `json:"archive_error,omitempty"`
}

const (
	usageBatchRows   = 1500
	usageBatchChunks = 150
	usageStateFile   = "usage-state.json"
	inventoryEvery   = 12 * time.Hour
)

// usageState is wp's memory of what was uploaded.
type usageState struct {
	Files        map[string]*usageFileState `json:"files"`
	InventoryAt  int64                      `json:"inventory_at,omitempty"`
	InventorySum string                     `json:"inventory_sum,omitempty"`
}

func loadUsageState(dir string) usageState {
	var s usageState
	if err := readJSON(filepath.Join(dir, usageStateFile), &s); err != nil {
		return usageState{Files: map[string]*usageFileState{}} // scratch data: start over
	}
	if s.Files == nil {
		s.Files = map[string]*usageFileState{}
	}
	return s
}

// claudeDir is Claude Code's config directory.
func claudeDir(override string) string {
	switch {
	case override != "":
		return override
	case os.Getenv("CLAUDE_CONFIG_DIR") != "":
		return os.Getenv("CLAUDE_CONFIG_DIR")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// parseSince reads 30d, 12h, 2w, 90m (a bare number is days).
func parseSince(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, nil
	}
	unit := time.Duration(24) * time.Hour
	if last := s[len(s)-1]; last < '0' || last > '9' {
		switch last {
		case 'm':
			unit = time.Minute
		case 'h':
			unit = time.Hour
		case 'd':
			unit = 24 * time.Hour
		case 'w':
			unit = 7 * 24 * time.Hour
		default:
			return 0, fmt.Errorf("--since is a number of days, or 90m, 12h, 30d, 4w, not %q", s)
		}
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("--since is a number of days, or 90m, 12h, 30d, 4w")
	}
	return time.Duration(n * float64(unit)), nil
}

// discoverTranscripts lists the transcripts under the config directory:
// <projects>/<dir>/<session>.jsonl and the subagents of each session.
func discoverTranscripts(root string, since time.Duration, only []string) ([]usageFile, error) {
	var files []usageFile
	cut := time.Time{}
	if since > 0 {
		cut = time.Now().Add(-since)
	}
	add := func(f usageFile) {
		if fi, err := os.Stat(f.Path); err == nil && fi.Mode().IsRegular() && (cut.IsZero() || fi.ModTime().After(cut)) {
			files = append(files, f)
		}
	}
	addSession := func(path string) {
		base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		add(usageFile{Path: path, Session: base, Kind: "main", Main: true})
		for _, sp := range subagentFiles(filepath.Join(filepath.Dir(path), base, "subagents")) {
			add(usageFile{Path: sp, Session: base, Kind: subagentType(sp)})
		}
	}
	if len(only) > 0 {
		for _, p := range only {
			addSession(p)
		}
		return files, nil
	}
	projects := filepath.Join(root, "projects")
	if _, err := os.Stat(projects); err != nil {
		return nil, fmt.Errorf("no Claude Code transcripts at %s (use --dir if your config lives elsewhere)", projects)
	}
	mains, err := filepath.Glob(filepath.Join(projects, "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	for _, p := range mains {
		addSession(p)
	}
	// sessions whose main file is gone still have subagent files
	known := map[string]bool{}
	for _, f := range files {
		known[f.Path] = true
	}
	dirs, _ := filepath.Glob(filepath.Join(projects, "*", "*", "subagents"))
	for _, d := range dirs {
		for _, sp := range subagentFiles(d) {
			if !known[sp] {
				add(usageFile{Path: sp, Session: filepath.Base(filepath.Dir(d)), Kind: subagentType(sp)})
			}
		}
	}
	return files, nil
}

// outdatedTranscripts lists the transcripts under root that an earlier sync
// read before this wp's newest kinds of row existed, whatever their age and
// whichever files this sync was asked for, leaving out those in have. A
// release that adds a kind of row so fills in the past on every machine's
// next sync, with nothing to run (WP-618), and each file's versions record
// how far it got. Only files already synced count: a transcript never
// uploaded still waits for --all.
func outdatedTranscripts(root string, st *usageState, have []usageFile) []usageFile {
	under := filepath.Join(root, "projects") + string(filepath.Separator)
	old := map[string]bool{}
	for p, s := range st.Files {
		if s != nil && s.outdated() && strings.HasPrefix(p, under) {
			if _, err := os.Stat(p); err == nil { // so deleted ones don't cost a walk per hook
				old[p] = true
			}
		}
	}
	for _, f := range have {
		delete(old, f.Path)
	}
	if len(old) == 0 {
		return nil
	}
	all, err := discoverTranscripts(root, 0, nil)
	if err != nil {
		return nil
	}
	var out []usageFile
	for _, f := range all {
		if old[f.Path] {
			out = append(out, f)
		}
	}
	return out
}

// subagentFiles lists a session's subagent transcripts, including those a
// workflow started (subagents/workflows/<id>/agent-*.jsonl).
func subagentFiles(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "agent-") && strings.HasSuffix(d.Name(), ".jsonl") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// subagentType reads agent-<id>.meta.json beside a subagent transcript.
func subagentType(path string) string {
	meta := strings.TrimSuffix(path, ".jsonl") + ".meta.json"
	b, err := os.ReadFile(meta) // #nosec G304 -- beside a transcript Claude Code wrote
	if err != nil {
		return "subagent"
	}
	var m struct {
		AgentType string `json:"agentType"`
	}
	if json.Unmarshal(b, &m) != nil || strings.TrimSpace(m.AgentType) == "" {
		return "subagent"
	}
	return truncRunes(strings.TrimSpace(m.AgentType), 80)
}

// tryLock takes a lock file without waiting; ok is false when another wp holds it.
func tryLock(path string, stale time.Duration) (unlock func(), ok bool) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false
	}
	for range 2 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- our own lock file
		if err == nil {
			nonce := newNonce()
			_, _ = f.WriteString(nonce)
			_ = f.Close()
			return func() {
				if b, err := os.ReadFile(path); err == nil && string(b) == nonce { // #nosec G304 -- our own lock file
					_ = os.Remove(path)
				}
			}, true
		}
		fi, serr := os.Stat(path)
		if serr != nil || time.Since(fi.ModTime()) <= stale {
			return nil, false
		}
		_ = os.Remove(path) // a crashed sync's lock
	}
	return nil, false
}

// idSet remembers message ids to tell when the same message is in two
// transcripts (a resumed session copies its history).
type idSet struct {
	mu   sync.Mutex
	seen map[uint64]struct{}
	n    int64
}

func (s *idSet) add(id string) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	k := h.Sum64()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[k]; ok {
		s.n++
		return
	}
	s.seen[k] = struct{}{}
}

// usageSync does one sync. It is safe to run from a hook, the runner and
// by hand at the same time: a second one finds the lock taken and returns.
func usageSync(c *cli, o usageSyncOptions) (*usageSyncResult, error) {
	started := time.Now()
	res := &usageSyncResult{Machine: machineName(), DryRun: o.DryRun}
	release := func() {}
	root := claudeDir(o.Dir)
	if root == "" {
		return nil, errors.New("can't find Claude Code's config directory; use --dir")
	}
	if !o.DryRun {
		if c.server() == "" || c.token() == "" {
			return nil, errors.New("not logged in: wp auth login https://your-waypoint")
		}
		unlock, ok := tryLock(filepath.Join(c.dir, "usage-sync.lock"), 15*time.Minute)
		if !ok {
			return nil, errUsageBusy
		}
		release = sync.OnceFunc(unlock) // one release: early, before the archive pass, or on return
		defer release()
	}
	files, err := discoverTranscripts(root, o.Since, o.Files)
	if err != nil {
		return nil, err
	}
	st := loadUsageState(c.dir)
	files = append(files, outdatedTranscripts(root, &st, files)...)
	res.Files = len(files)
	if o.DryRun { // look at the state, never change it
		cp := usageState{Files: map[string]*usageFileState{}, InventoryAt: st.InventoryAt, InventorySum: st.InventorySum}
		for k, v := range st.Files {
			cv := *v
			cp.Files[k] = &cv
		}
		st = cp
	}

	repos := newRepoResolver()
	var shared *idSet
	if o.DryRun {
		shared = &idSet{seen: map[uint64]struct{}{}}
	}
	// the workers read these while the main loop updates st.Files
	prevs := make(map[string]usageFileState, len(files))
	for _, f := range files {
		if p := st.Files[f.Path]; p != nil {
			prevs[f.Path] = *p
		}
	}
	jobs := make(chan usageFile)
	results := make(chan *usageParsed)
	var wg sync.WaitGroup
	workers := min(max(runtime.NumCPU(), 2), 8)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				prev := prevs[f.Path]
				// a file that hasn't grown needs no reading
				if fi, err := os.Stat(f.Path); err == nil && prev.FP != "" && prev.Off == fi.Size() && prev.Size == fi.Size() && !prev.outdated() {
					results <- &usageParsed{File: f, State: prev, From: prev.Off, To: prev.Off}
					continue
				}
				p, err := readUsageFile(f, cloneFileState(prev), repos.of, shared != nil)
				if err != nil {
					results <- &usageParsed{File: f, State: prev, From: prev.Off, To: prev.Off, Stats: usageStats{Unknown: map[string]int64{"(unreadable file)": 1}, Skipped: 1}}
					continue
				}
				results <- p
			}
		}()
	}
	go func() {
		for _, f := range files {
			jobs <- f
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	var pending []*usageParsed
	pendingRows := 0
	var health usageStats // parser health since the last upload
	sessions := map[string]bool{}
	var flushErr error
	flush := func(inv *[]usageInventoryItem, final bool) {
		if flushErr != nil {
			return
		}
		var chunks []usageChunk
		for _, p := range pending {
			if p.Chunk.rows() > 0 {
				p.Chunk.ID = chunkID(res.Machine, p)
				chunks = append(chunks, p.Chunk)
			}
		}
		if !o.DryRun && (len(chunks) > 0 || inv != nil || final && !o.Hook) {
			body := map[string]any{"machine": res.Machine, "version": version, "chunks": chunks,
				"parsed": health.Parsed, "skipped": health.Skipped}
			if len(health.Unknown) > 0 {
				body["unknown"] = health.Unknown
			}
			if inv != nil {
				body["inventory"] = *inv
			}
			var ans struct {
				Accepted  int `json:"accepted"`
				Duplicate int `json:"duplicate"`
				Dropped   int `json:"traces_dropped"`
			}
			if err := c.do("POST", "/api/v1/usage/ingest", body, &ans); err != nil {
				flushErr = err
				return
			}
			res.Accepted += ans.Accepted
			res.Duplicate += ans.Duplicate
			res.TracesDropped += ans.Dropped
		}
		res.Chunks += len(chunks)
		for _, p := range pending {
			ps := p.State
			st.Files[p.File.Path] = &ps
		}
		if !o.DryRun && len(pending) > 0 {
			if err := writeJSON(filepath.Join(c.dir, usageStateFile), st); err != nil {
				flushErr = err
			}
		}
		pending, pendingRows, health = nil, 0, usageStats{}
	}
	for p := range results {
		if p.Changed {
			res.FilesRead++
		}
		res.Bytes += p.Stats.Bytes
		res.Messages += p.Stats.Messages
		for _, m := range p.Chunk.Messages {
			res.Tokens.Input += m.Input
			res.Tokens.Output += m.Output
			res.Tokens.CacheWrite += m.CW5 + m.CW1
			res.Tokens.CacheRead += m.CR
		}
		res.ToolCalls += p.Stats.Calls
		res.Refusals += p.Stats.Refusals
		res.Risky += len(p.Chunk.Risky)
		res.Traces += len(p.Chunk.Traces)
		res.Parsed += p.Stats.Parsed
		res.Ignored += p.Stats.Ignored
		res.Skipped += p.Stats.Skipped
		for k, v := range p.Stats.Unknown {
			if res.Unknown == nil {
				res.Unknown = map[string]int64{}
			}
			res.Unknown[k] += v
		}
		for _, s := range p.Chunk.Sessions {
			sessions[s.Session] = true
		}
		if shared != nil {
			for _, id := range p.messageIDs {
				shared.add(id)
			}
		}
		health.add(p.Stats)
		pending = append(pending, p)
		pendingRows += p.Chunk.rows()
		if pendingRows >= usageBatchRows || len(pending) >= usageBatchChunks {
			flush(nil, false)
		}
		if o.Out != nil && res.Files > 200 && res.FilesRead%200 == 0 && p.Changed {
			_, _ = fmt.Fprintf(o.Out, "  … %d of %d transcripts\n", res.FilesRead, res.Files)
		}
	}
	res.Sessions = len(sessions)
	if shared != nil {
		res.SharedIDs = shared.n
	}

	// what is configured on this machine
	var inv *[]usageInventoryItem
	if o.Inventory {
		cwds := map[string]bool{}
		for _, f := range st.Files {
			if f.Cwd != "" {
				cwds[f.Cwd] = true
			}
		}
		items := collectInventory(root, sortedKeys(cwds))
		res.Inventory = map[string]int{}
		for _, it := range items {
			res.Inventory[it.Kind]++
		}
		sum := inventorySum(items)
		if o.DryRun || sum != st.InventorySum || time.Since(time.UnixMilli(st.InventoryAt)) > inventoryEvery {
			inv = &items
			if !o.DryRun {
				st.InventorySum, st.InventoryAt = sum, time.Now().UnixMilli()
			}
		}
	}
	flush(inv, true)
	if flushErr != nil && inv != nil {
		// the inventory didn't land, so offer it again next time
		st.InventorySum, st.InventoryAt = "", 0
	}
	release() // the archive has its own lock: a long backfill must not hold up the next hook's counts
	if !o.NoArchive && archiveEnabled() {
		ar, aerr := usageArchive(c, o, root)
		res.Archive = ar
		if aerr != nil {
			res.ArchiveError = aerr.Error()
		}
	}
	res.Elapsed = time.Since(started).Round(time.Millisecond).String()
	if flushErr != nil {
		return res, flushErr
	}
	return res, nil
}

// usageTokenSums totals what was read, so a dry run shows the scale.
type usageTokenSums struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheWrite int64 `json:"cache_write"`
	CacheRead  int64 `json:"cache_read"`
}

var errUsageBusy = errors.New("another wp usage sync is running")

// outdated says this wp would count rows the last read of the file didn't.
func (s *usageFileState) outdated() bool {
	return s.LV < usageLedgerVersion || s.RV < usageRiskyVersion || s.CV < usageCLIVersion || s.TV < usageTraceVersion
}

func cloneFileState(s usageFileState) usageFileState {
	s.Last = append([]usageSeenMsg(nil), s.Last...)
	if s.CLIPending != nil {
		s.CLIPending = append([]usageCLIPending(nil), s.CLIPending...)
		for i := range s.CLIPending {
			s.CLIPending[i].Seen = append([]string(nil), s.CLIPending[i].Seen...)
		}
	}
	s.Trace = s.Trace.clone()
	if s.Open != nil {
		o := make(map[string]*usageOpenCall, len(s.Open))
		for k, v := range s.Open {
			c := *v
			o[k] = &c
		}
		s.Open = o
	}
	return s
}

func chunkID(machine string, p *usageParsed) string {
	key := fmt.Sprintf("%s|%s|%s|%d-%d", machine, p.File.Path, p.State.FP, p.From, p.To)
	if p.LedgerOnly {
		key += "|ledger" // the same range was sent before, without these rows
	}
	if p.RiskyOnly {
		key += "|risky"
	}
	if p.CLIOnly {
		key += "|cli"
	}
	if p.TraceOnly {
		key += fmt.Sprintf("|trace%d", usageTraceVersion) // each version's reading is sent
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:12])
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
