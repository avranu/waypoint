package main

// An audit the LLM gateway runs (WP-617, WP-ADR84): its model is
// gateway/<model>, no CLI on this machine. The runner clones and prepares
// the checkout as for any audit, packs it and hands it to Waypoint, which
// has the gateway's key and sends it on as an agent task; then it asks
// Waypoint how the task goes until it ends. Nothing here knows the
// gateway's address or key.

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/textproto"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

const (
	gatewayCLI = "gateway"
	// gatewayArchiveFallback is the most the gateway takes of a packed
	// checkout, and gatewayFileFallback its limit for one file, which it
	// would leave out anyway: leaving it out here keeps it from counting
	// against the first. The job says the real figures (agentJob.
	// GatewayArchiveMax, GatewayFileMax: Waypoint reads them from the
	// gateway); these are for a server that doesn't, the server's own
	// defaults.
	gatewayArchiveFallback = core.GatewayArchiveMax
	gatewayFileFallback    = core.GatewayFileMax
	// gatewayAdmitWait is how long a job keeps offering its task to a
	// gateway that says busy (its queue is full, it is restarting) before
	// the audit goes to its next model.
	gatewayAdmitWait = 10 * time.Minute
	// gatewayUploadTimeout bounds one upload: to Waypoint, and from there
	// to the gateway, which answers once it has the task.
	gatewayUploadTimeout = 12 * time.Minute
	// gatewayResends is how often a task is sent again after the gateway
	// restarted under it (its kind "interrupted": safe to send again).
	gatewayResends = 2
	// A job's result is held to what the server takes (core.RunnerResultMax, 2 MB, and the helpers' runs share it): the answer's text (Waypoint keeps
	// 60 KB of it) and its findings as JSON.
	gatewayAnswerMax = 60 << 10
	gatewayJSONMax   = 180 << 10
)

// gatewayPollEvery is how often a job asks Waypoint how its task goes.
var gatewayPollEvery = 10 * time.Second

// gatewayTaskView is Waypoint's answer about the job's task.
type gatewayTaskView struct {
	ID           string          `json:"id"`
	Status       string          `json:"status"` // queued, running, succeeded, failed, cancelled; refused, unknown
	Model        string          `json:"model"`
	Text         string          `json:"text"`
	JSON         json.RawMessage `json:"json"`
	ErrorKind    string          `json:"error_kind"`
	Error        string          `json:"error"`
	RetryAfter   int             `json:"retry_after"`
	InputTokens  int64           `json:"input_tokens"`
	OutputTokens int64           `json:"output_tokens"`
	CostUSD      float64         `json:"cost_usd"`
}

// execGatewayAudit runs the audit's model at the gateway over the checkout
// in work. result holds what the job reports of the checkout already.
func (p *runnerProc) execGatewayAudit(ctx context.Context, run *jobRun, in agentJob, work string, result map[string]any) outcome {
	ended := func(view *gatewayTaskView) (outcome, bool) {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			p.gatewayCancel(run)
			result["gateway_error"] = "timeout"
			if view != nil {
				result["gateway_task"] = view.ID
			}
			return outcome{State: "failed", Error: fmt.Sprintf("The audit through the gateway timed out after %s and was stopped.", run.timeout.Round(time.Second)), Result: result}, true
		case ctx.Err() != nil:
			p.gatewayCancel(run)
			return outcome{State: "cancelled", Error: "Stopped."}, true
		}
		return outcome{}, false
	}
	gave := func(view *gatewayTaskView, kind, format string, a ...any) outcome {
		result["gateway_error"] = kind
		if view != nil {
			result["gateway_task"], result["model"] = view.ID, view.Model
			result["answer"] = excerpt(view.Text, gatewayAnswerMax)
		}
		return outcome{State: "failed", Error: fmt.Sprintf(format, a...), Result: result}
	}

	archive, err := os.CreateTemp(p.cacheDir(), fmt.Sprintf("job%d-*.tar.gz", run.id))
	if err != nil {
		return failed("%v", err)
	}
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
	}()
	limit, fileMax := in.GatewayArchiveMax, in.GatewayFileMax
	if limit <= 0 {
		limit = gatewayArchiveFallback
	}
	if fileMax <= 0 {
		fileMax = gatewayFileFallback
	}
	res, tier, err := p.packForGateway(ctx, run, in, work, archive, limit, fileMax)
	tooLarge := errors.Is(err, errArchiveTooLarge)
	if left := res.leftOutText(); left != "" && (err == nil || tooLarge) {
		result["left_out"] = left
	}
	if tooLarge {
		what := "files over " + fmtMiB(gatewayTiers[len(gatewayTiers)-1].over)
		if res.Scoped {
			what += " and files outside the audit's paths"
		}
		return gave(nil, "body_too_large", "The checkout packs to more than %s, the most the gateway takes for an audit, even with %s left out.", fmtMiB(limit), what)
	}
	if err != nil {
		return p.agentError(ctx, run, "can't pack the checkout: %v", err)
	}
	size, err := archive.Seek(0, io.SeekCurrent)
	if err != nil {
		return failed("%v", err)
	}
	fmt.Fprintf(run.out, "Packed the checkout for the gateway: %d file%s, %.1f MiB\n", res.Files, pluralS(res.Files), float64(size)/(1<<20))
	prompt := in.Prompt
	if len(res.Left) > 0 {
		fmt.Fprintf(run.out, "Left out %d file%s (%s)%s: %s\n", len(res.Left), pluralS(len(res.Left)), fmtMiB(res.leftBytes()), tier, excerpt(res.leftOutText()+"; e.g. "+res.sample(4), 400))
		prompt += fmt.Sprintf("\n\nLeft out of this checkout, though they are in the repository (listed in %s/%s; their absence is not a finding): %s.", auditChangeDir, leftOutName, res.leftOutText())
	}

	path := fmt.Sprintf("/api/v1/runner/jobs/%d/gateway", run.id)
	for sent := 0; ; sent++ {
		// Offer the task until the gateway takes it.
		var view *gatewayTaskView
		for began := time.Now(); ; {
			view, err = p.gatewayUpload(ctx, path, prompt, archive, size)
			if out, ok := ended(view); ok {
				return out
			}
			if err != nil {
				// Waypoint refusing it says nothing a later try changes; not
				// reaching Waypoint (or it not reaching on) may pass.
				kind := "unreachable"
				var ae *apiError
				if errors.As(err, &ae) && ae.status >= 400 && ae.status < 500 {
					kind = "refused"
				}
				return gave(nil, kind, "Waypoint didn't take the audit for the gateway: %v", err)
			}
			if view.Status != "refused" {
				break
			}
			wait := time.Duration(view.RetryAfter) * time.Second
			if wait <= 0 || time.Since(began)+wait > gatewayAdmitWait {
				return gave(nil, orStr(view.ErrorKind, "server"), "The gateway didn't take the audit (%s): %s", orStr(view.ErrorKind, "no reason given"), view.Error)
			}
			fmt.Fprintf(run.out, "The gateway is busy (%s); offering the audit again in %s\n", orStr(view.ErrorKind, "no reason given"), wait)
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
			if out, ok := ended(nil); ok {
				return out
			}
		}
		fmt.Fprintf(run.out, "The gateway took the audit as task %s (%s)\n", view.ID, view.Status)

		// Ask how it goes until it ends.
		said, misses := view.Status, 0
		for view.Status == "queued" || view.Status == "running" || view.Status == "unknown" {
			select {
			case <-ctx.Done():
			case <-time.After(gatewayPollEvery):
			}
			if out, ok := ended(view); ok {
				return out
			}
			var next gatewayTaskView
			if err := p.c.callInto(ctx, "GET", path, nil, &next); err != nil {
				if out, ok := ended(view); ok {
					return out
				}
				var ae *apiError
				if errors.As(err, &ae) && ae.status >= 400 && ae.status < 500 {
					return failed("Waypoint no longer knows the audit's task: %v", err)
				}
				// Waypoint is restarting or away: the task runs on.
				if misses++; misses == 1 {
					fmt.Fprintf(run.out, "Can't ask Waypoint about the task (%v); asking again\n", err)
				}
				continue
			}
			misses = 0
			if next.ID == "" {
				next.ID = view.ID
			}
			view = &next
			if view.Status != said && (view.Status == "queued" || view.Status == "running") {
				fmt.Fprintf(run.out, "Task %s is %s\n", view.ID, view.Status)
				said = view.Status
			}
		}
		switch {
		case view.Status == "succeeded" && len(view.JSON) > gatewayJSONMax:
			return gave(view, "output_limit", "The audit through the gateway answered %d KB of findings, more than a job's result holds.", len(view.JSON)>>10)
		case view.Status == "succeeded":
			fmt.Fprintf(run.out, "%s answered (%d tokens in, %d out)\n\n%s\n", orStr(view.Model, "The gateway"), view.InputTokens, view.OutputTokens, excerpt(view.Text, 4000))
			result["answer"], result["model"], result["gateway_task"] = excerpt(view.Text, gatewayAnswerMax), view.Model, view.ID
			result["tokens"] = tokenUse{Input: view.InputTokens, Output: view.OutputTokens, CostUSD: view.CostUSD}
			if js := bytes.TrimSpace(view.JSON); len(js) > 0 && string(js) != "null" {
				result["structured"] = view.JSON
			}
			return outcome{State: "done", Result: result}
		case view.ErrorKind == "interrupted" && sent < gatewayResends:
			fmt.Fprintf(run.out, "The gateway restarted under task %s; sending the audit again\n", view.ID)
		default:
			kind := orStr(view.ErrorKind, "server")
			return gave(view, kind, "The audit through the gateway %s (%s): %s", map[bool]string{true: "was cancelled there", false: "failed"}[view.Status == "cancelled"], kind, orStr(view.Error, "no reason given"))
		}
	}
}

// gatewayUpload hands Waypoint the prompt and the packed checkout for the
// job's task: multipart, the archive streamed from its file.
func (p *runnerProc) gatewayUpload(ctx context.Context, path, prompt string, archive *os.File, size int64) (*gatewayTaskView, error) {
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	if err := mw.WriteField("prompt", prompt); err != nil {
		return nil, err
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="repo"; filename="repo.tar.gz"`)
	h.Set("Content-Type", "application/gzip")
	if _, err := mw.CreatePart(h); err != nil {
		return nil, err
	}
	body := io.MultiReader(&head, io.LimitReader(archive, size), strings.NewReader("\r\n--"+mw.Boundary()+"--\r\n"))
	// The runner's other requests go on at their own pace: this one's
	// longer limit is a copy's.
	c := *p.c
	c.longPoll = gatewayUploadTimeout
	raw, err := c.send(ctx, "POST", path, mw.FormDataContentType(), body)
	if err != nil {
		return nil, err
	}
	var view gatewayTaskView
	if err := json.Unmarshal(raw, &view); err != nil {
		return nil, fmt.Errorf("can't read what Waypoint said of the task: %w", err)
	}
	return &view, nil
}

// gatewayCancel stops the job's task at the gateway after the job itself
// was stopped or ran out of time; the job's own context is over by then.
func (p *runnerProc) gatewayCancel(run *jobRun) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := p.c.callCtx(ctx, "POST", fmt.Sprintf("/api/v1/runner/jobs/%d/gateway/cancel", run.id), nil); err != nil && !isNotFound(err) {
		fmt.Fprintf(run.out, "Couldn't stop the task at the gateway (%v); it ends at its own time limit\n", err)
	}
}

var errArchiveTooLarge = errors.New("the archive is too large")

// limitWriter fails once more than max bytes were written through it.
type limitWriter struct {
	w    io.Writer
	left int64
}

func (l *limitWriter) Write(b []byte) (int, error) {
	if int64(len(b)) > l.left {
		return 0, errArchiveTooLarge
	}
	l.left -= int64(len(b))
	return l.w.Write(b)
}

// leftOutName is the generated list, in auditChangeDir of the archive only,
// of what the checkout leaves out of what the model is given.
const leftOutName = "left-out.txt"

// leftOutHeader heads the list of what was left out. Binary files and those
// over the gateway's per-file limit are always among the reasons; files over
// a size or outside the audit's paths only when the whole checkout was too
// big for the gateway, which is when left holds them.
func leftOutHeader(left []leftOut) string {
	var b strings.Builder
	b.WriteString("These files exist in the repository but are not in this copy of it.\n")
	b.WriteString("Some are binary or over the gateway's per-file limit.\n")
	if slices.ContainsFunc(left, func(l leftOut) bool { return l.Reason != "binary" && l.Reason != "large" }) {
		b.WriteString("The whole checkout was too big for the gateway, so files over a size or outside the audit's paths were left out too.\n")
	}
	b.WriteString("Their absence is not a finding. Columns: path, bytes, reason.\n")
	return b.String()
}

// gatewayTier is one stricter way to pack a checkout that was too large:
// files over `over` bytes stay out and, with scope, so does every file the
// audit's paths and exclusions leave out.
type gatewayTier struct {
	over  int64
	scope bool
}

// gatewayTiers are tried in order after a plain pack didn't fit, stopping at
// the first that does. The scope tier only applies to an audit with a scope.
var gatewayTiers = []gatewayTier{{over: 1 << 20}, {over: 256 << 10}, {over: 256 << 10, scope: true}, {over: 64 << 10, scope: true}}

// leftOut is a file the archive leaves out.
type leftOut struct {
	Path   string
	Size   int64
	Reason string // binary, large, "over 256 KiB", "out of scope"
}

// packOptions says what packCheckout leaves out and how much it may write.
type packOptions struct {
	ArchiveMax int64 // bytes in all (errArchiveTooLarge)
	FileMax    int64 // a file over it is left out as "large"
	// Over, when > 0, leaves out files over that many bytes ("over 256 KiB"),
	// and Out, when set, those it says are out of scope; Keep names the files
	// neither applies to (the change's own, and auditChangeDir's). Keep
	// protects a file from those two cuts (the fit cuts) only: a binary file
	// and one over FileMax stay out whatever Keep says.
	Over int64
	Out  func(path string) bool
	Keep func(path string) bool
}

// packResult is what a pack put in and left out, by path.
type packResult struct {
	Files int
	Left  []leftOut
	// Scoped: the pass also left out what the audit's paths and exclusions
	// leave out (set by packForGateway).
	Scoped bool
}

func (r packResult) leftBytes() (n int64) {
	for _, l := range r.Left {
		n += l.Size
	}
	return n
}

// leftOutText is the left-out files said in a sentence's worth, by reason:
// "350 binary files (16.9 MiB), 5 files over 256 KiB (2.1 MiB)"; "" if none.
func (r packResult) leftOutText() string {
	var order []string
	n, size := map[string]int{}, map[string]int64{}
	for _, l := range r.Left {
		if n[l.Reason] == 0 {
			order = append(order, l.Reason)
		}
		n[l.Reason]++
		size[l.Reason] += l.Size
	}
	var parts []string
	for _, reason := range order {
		files := map[bool]string{true: "file", false: "files"}[n[reason] == 1]
		var what string
		switch reason {
		case "binary":
			what = "binary " + files
		case "large":
			what = files + " over the gateway's per-file limit"
		default:
			what = files + " " + reason
		}
		parts = append(parts, fmt.Sprintf("%d %s (%s)", n[reason], what, fmtMiB(size[reason])))
	}
	return strings.Join(parts, ", ")
}

// sample names up to n of the left-out files, the largest first.
func (r packResult) sample(n int) string {
	left := slices.Clone(r.Left)
	slices.SortStableFunc(left, func(a, b leftOut) int { return cmp.Compare(b.Size, a.Size) })
	var names []string
	for _, l := range left[:min(n, len(left))] {
		names = append(names, l.Path)
	}
	return strings.Join(names, ", ")
}

// fmtMiB is a size in MiB, to a tenth when it isn't whole.
func fmtMiB(n int64) string {
	if n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	if n < 1<<10 {
		return fmt.Sprintf("%d B", n)
	}
	if n < 1<<20 {
		if n%(1<<10) == 0 {
			return fmt.Sprintf("%d KiB", n>>10)
		}
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

// packForGateway packs the checkout into archive for a gateway that takes
// limit bytes of it and fileMax of one file: whole, and if that is too
// large, again with gatewayTiers' stricter ones, the first that fits. tier
// says which one applied (" (the checkout was over 20 MiB: files over 1 MiB
// left out)"; "" for a plain pack). Past the last it answers
// errArchiveTooLarge, and the result is what that last pass left out. The
// change's own files (keep) are protected from the stricter passes' cuts, not
// from the binary and per-file-limit rules, which apply in every pass.
func (p *runnerProc) packForGateway(ctx context.Context, run *jobRun, in agentJob, work string, archive *os.File, limit, fileMax int64) (res packResult, tier string, err error) {
	changed := changedFiles(work)
	keep := func(path string) bool {
		return path == auditChangeDir || strings.HasPrefix(path, auditChangeDir+"/") || changed[path]
	}
	passes := append([]gatewayTier{{}}, gatewayTiers...)
	var out func(string) bool
	resolved := false
	for i, t := range passes {
		if t.scope && !resolved {
			// Only now, once sizes alone weren't enough.
			out, resolved = p.scopeOut(ctx, run, work, in), true
		}
		if t.scope && out == nil && t.over == passes[i-1].over {
			continue // no scope to go by: this pass would repeat the last
		}
		if i > 0 {
			if err := archive.Truncate(0); err != nil {
				return res, "", err
			}
		}
		if _, err := archive.Seek(0, io.SeekStart); err != nil {
			return res, "", err
		}
		opt := packOptions{ArchiveMax: limit, FileMax: fileMax, Over: t.over, Keep: keep}
		if t.scope {
			opt.Out = out
		}
		res, err = packCheckout(work, archive, opt)
		res.Scoped = opt.Out != nil
		if err == nil {
			if i > 0 {
				what := "files over " + fmtMiB(t.over)
				if t.scope && out != nil {
					what += " and files out of scope"
				}
				tier = fmt.Sprintf(" (the checkout was over %s: %s left out)", fmtMiB(limit), what)
			}
			return res, tier, nil
		}
		if !errors.Is(err, errArchiveTooLarge) {
			return res, "", err
		}
		fmt.Fprintf(run.out, "The checkout packs to more than %s", fmtMiB(limit))
		if i+1 < len(passes) {
			fmt.Fprintf(run.out, "; packing again with stricter limits\n")
		} else {
			fmt.Fprintf(run.out, " even so\n")
		}
	}
	return res, "", err
}

// scopeOut answers which files the audit's paths and exclusions leave out,
// by what git lists of them in work, untracked files too (nil when git can't say, which the log
// notes: the audit then has no scope to go by).
func (p *runnerProc) scopeOut(ctx context.Context, run *jobRun, work string, in agentJob) func(string) bool {
	if len(in.DiffPaths) == 0 && len(in.DiffExclude) == 0 {
		return nil // the audit names no scope
	}
	list := func(specs []string) (map[string]bool, error) {
		raw, err := p.gitRaw(ctx, work, nil, nil, append([]string{"ls-files", "-z", "--cached", "--others", "--"}, specs...)...)
		if err != nil {
			return nil, err
		}
		set := map[string]bool{}
		for _, f := range strings.FieldsFunc(raw, func(r rune) bool { return r == 0 }) {
			set[f] = true
		}
		return set, nil
	}
	var inside, outside map[string]bool
	var err error
	if len(in.DiffPaths) > 0 {
		inside, err = list(in.DiffPaths)
	}
	if err == nil && len(in.DiffExclude) > 0 {
		outside, err = list(in.DiffExclude)
	}
	if err != nil {
		fmt.Fprintf(run.out, "Can't tell which files the audit's paths leave out (%v); going by file sizes alone\n", err)
		return nil
	}
	return func(path string) bool {
		return (inside != nil && !inside[path]) || outside[path]
	}
}

// changedFiles are the files the change under audit touches, as its diff in
// the checkout names them (auditChangeDir/change.diff; files.txt holds git's
// --stat, whose paths are cut and merged for renames). Empty for an audit of
// a whole repository.
func changedFiles(work string) map[string]bool {
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil
	}
	defer func() { _ = root.Close() }()
	diff, err := root.ReadFile(auditChangeDir + "/change.diff")
	if err != nil {
		return nil
	}
	names := map[string]bool{}
	for _, line := range strings.Split(string(diff), "\n") {
		if rest, ok := strings.CutPrefix(line, "diff --git "); ok {
			for _, n := range diffGitNames(rest) {
				names[n] = true
			}
		}
	}
	return names
}

// diffGitNames reads the paths from a "diff --git a/X b/Y" line (after the
// prefix): X and Y, which differ for a rename. Quoted names are unquoted.
func diffGitNames(rest string) []string {
	var out []string
	if strings.HasPrefix(rest, `"`) || strings.Contains(rest, ` "b/`) {
		// Quoted (a name with odd bytes): each side by itself.
		for rest = strings.TrimSpace(rest); rest != ""; rest = strings.TrimSpace(rest) {
			var word string
			if strings.HasPrefix(rest, `"`) {
				q, err := strconv.QuotedPrefix(rest)
				if err != nil {
					return out
				}
				word, _ = strconv.Unquote(q)
				rest = rest[len(q):]
			} else if i := strings.Index(rest, ` "`); i >= 0 {
				word, rest = rest[:i], rest[i:]
			} else {
				word, rest = rest, ""
			}
			if len(word) > 2 {
				out = append(out, word[2:])
			}
		}
		return out
	}
	// Both sides alike: "a/X b/X", so X is half of what's left of the
	// prefixes. Otherwise a rename: split at the last " b/".
	if n := len(rest) - 5; n > 0 && n%2 == 0 && strings.HasPrefix(rest, "a/") && rest[2+n/2:5+n/2] == " b/" && rest[2:2+n/2] == rest[5+n/2:] {
		return []string{rest[2 : 2+n/2]}
	}
	if i := strings.LastIndex(rest, " b/"); i > 2 && strings.HasPrefix(rest, "a/") {
		return []string{rest[2:i], rest[i+3:]}
	}
	return nil
}

// packCheckout writes the files of the checkout in work to w as a gzipped
// tar: regular files only (git's own directory, links and devices stay
// out, and nothing is followed out of work), no binary file (a NUL in its
// first 8000 bytes, as git tells them), none over opt.FileMax, those opt
// asks to leave out, and no more than opt.ArchiveMax bytes in all
// (errArchiveTooLarge). Binary and over-FileMax are checked before opt.Keep
// on purpose (the gateway refuses a file over its per-file limit, and binary
// is never sent), so Keep protects a file from opt.Out and opt.Over only. When something was left out, a list of it goes in as
// auditChangeDir/left-out.txt, last.
func packCheckout(work string, w io.Writer, opt packOptions) (res packResult, err error) {
	root, err := os.OpenRoot(work)
	if err != nil {
		return res, err
	}
	defer func() { _ = root.Close() }()
	zw := gzip.NewWriter(&limitWriter{w: w, left: opt.ArchiveMax})
	tw := tar.NewWriter(zw)
	listName := auditChangeDir + "/" + leftOutName
	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil // a work tree's pointer to its repository
		}
		if !d.Type().IsRegular() || path == listName {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		size := info.Size()
		skip := func(reason string) error {
			res.Left = append(res.Left, leftOut{Path: path, Size: size, Reason: reason})
			return nil
		}
		f, err := root.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		head := make([]byte, 8000)
		n, err := io.ReadFull(f, head)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return err
		}
		head = head[:n]
		switch {
		case bytes.IndexByte(head, 0) >= 0:
			return skip("binary")
		case size > opt.FileMax:
			return skip("large")
		case opt.Keep != nil && opt.Keep(path):
		case opt.Out != nil && opt.Out(path):
			return skip("out of scope")
		case opt.Over > 0 && size > opt.Over:
			return skip("over " + fmtMiB(opt.Over))
		}
		mode := int64(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: path, Mode: mode, Size: size}); err != nil {
			return err
		}
		// Exactly the size the header said, whatever the file does meanwhile.
		if _, err := io.CopyN(tw, io.MultiReader(bytes.NewReader(head), f), size); err != nil {
			return err
		}
		res.Files++
		return nil
	})
	if err == nil && len(res.Left) > 0 {
		slices.SortFunc(res.Left, func(a, b leftOut) int { return cmp.Compare(a.Path, b.Path) })
		var list strings.Builder
		list.WriteString(leftOutHeader(res.Left))
		for _, l := range res.Left {
			fmt.Fprintf(&list, "%s\t%d\t%s\n", l.Path, l.Size, l.Reason)
		}
		err = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: listName, Mode: 0o644, Size: int64(list.Len())})
		if err == nil {
			_, err = io.WriteString(tw, list.String())
		}
	}
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = zw.Close()
	}
	return res, err
}
