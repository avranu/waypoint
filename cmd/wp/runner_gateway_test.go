//go:build !windows

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Audits the LLM gateway runs, as the runner sees them (WP-617).

// unpack lists a packed checkout: each file's name, mode and content.
func unpack(t *testing.T, packed []byte) map[string]string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for tr := tar.NewReader(zr); ; {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		if h.Typeflag != tar.TypeReg {
			t.Errorf("%s isn't a regular file (%c)", h.Name, h.Typeflag)
		}
		out[h.Name] = os.FileMode(h.Mode).String() + " " + string(body)
	}
}

func TestPackCheckout(t *testing.T) {
	work := t.TempDir()
	write := func(name, body string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "one\n", 0o600)
	write("cmd/run.sh", "#!/bin/sh\n", 0o700)
	write(".git/config", "[remote]\n\turl = https://token@git.example/me/app.git\n", 0o600)
	write("vendor/mod/.git", "gitdir: ../../.git/modules/mod\n", 0o600)
	write("vendor/mod/m.go", "package mod\n", 0o600)
	write(".waypoint-audit/change.diff", "+new\n", 0o600)
	// Nothing outside the checkout comes along through a link.
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(work, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(work, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	// A file past the gateway's limit for one stays out, and is named.
	big, err := os.Create(filepath.Join(work, "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := big.Truncate(gatewayFileFallback + 1); err != nil {
		t.Fatal(err)
	}
	_ = big.Close()

	var buf bytes.Buffer
	opt := packOptions{ArchiveMax: gatewayArchiveFallback, FileMax: gatewayFileFallback}
	res, err := packCheckout(work, &buf, opt)
	if err != nil {
		t.Fatal(err)
	}
	files, left := res.Files, res.Left
	got := unpack(t, buf.Bytes())
	want := map[string]string{
		"a.txt": "-rw-r--r-- one\n", "cmd/run.sh": "-rwxr-xr-x #!/bin/sh\n", "vendor/mod/m.go": "-rw-r--r-- package mod\n",
		".waypoint-audit/change.diff": "-rw-r--r-- +new\n",
	}
	if files != len(want) || len(got) != len(want)+1 || len(left) != 1 || left[0].Path != "data.bin" || left[0].Reason != "binary" {
		t.Errorf("packed %d files (%d in the archive), left %v", files, len(got), left)
	}
	for name, body := range want {
		if got[name] != body {
			t.Errorf("%s = %q, want %q", name, got[name], body)
		}
	}
	// What was left out is listed, in the archive only.
	if list := got[".waypoint-audit/left-out.txt"]; !strings.Contains(list, "not a finding") || !strings.HasSuffix(list, fmt.Sprintf("data.bin\t%d\tbinary\n", gatewayFileFallback+1)) {
		t.Errorf("the list of what's left out: %q", list)
	}
	if _, err := os.Stat(filepath.Join(work, ".waypoint-audit/left-out.txt")); err == nil {
		t.Error("the list was written into the checkout")
	}

	// More than the gateway takes in all: said so, not sent short.
	noise := make([]byte, 64<<10)
	for i := range noise {
		noise[i] = byte(i*7 + i>>8*13 + i>>3)
	}
	write("noise.bin", string(noise), 0o600)
	if _, err := packCheckout(work, io.Discard, packOptions{ArchiveMax: 300, FileMax: gatewayFileFallback}); !errors.Is(err, errArchiveTooLarge) {
		t.Errorf("past the limit: %v", err)
	}
	if _, err := packCheckout(filepath.Join(work, "missing"), io.Discard, packOptions{ArchiveMax: 300, FileMax: gatewayFileFallback}); err == nil || errors.Is(err, errArchiveTooLarge) {
		t.Errorf("no checkout: %v", err)
	}
}

// fakeRelay is Waypoint's side of a job's gateway task: what each upload
// and each question is answered with, in turn (the last answer stays).
type fakeRelay struct {
	mu      sync.Mutex
	starts  []any // a gatewayTaskView, or an int: that HTTP status
	polls   []any
	prompts []string
	packed  [][]byte
	asked   int
	cancels int
}

func (f *fakeRelay) next(list *[]any) any {
	if len(*list) == 0 {
		return http.StatusInternalServerError
	}
	v := (*list)[0]
	if len(*list) > 1 {
		*list = (*list)[1:]
	}
	return v
}

func (f *fakeRelay) serve(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var answer any
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/runner/jobs/7/gateway":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("the upload isn't a form: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			file, head, err := r.FormFile("repo")
			if err != nil {
				t.Errorf("the upload has no repo: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			packed, _ := io.ReadAll(file)
			if head.Filename != "repo.tar.gz" || head.Header.Get("Content-Type") != "application/gzip" || r.Header.Get("Authorization") != "Bearer wp_test" {
				t.Errorf("upload = %q %q as %q", head.Filename, head.Header.Get("Content-Type"), r.Header.Get("Authorization"))
			}
			f.prompts, f.packed = append(f.prompts, r.FormValue("prompt")), append(f.packed, packed)
			answer = f.next(&f.starts)
		case "GET /api/v1/runner/jobs/7/gateway":
			f.asked++
			answer = f.next(&f.polls)
		case "POST /api/v1/runner/jobs/7/gateway/cancel":
			f.cancels++
			w.WriteHeader(http.StatusNoContent)
			return
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if status, ok := answer.(int); ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"the relay says no"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	t.Setenv("WAYPOINT_TOKEN", "wp_test")
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
}

func (f *fakeRelay) counts() (uploads, asked, cancels int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts), f.asked, f.cancels
}

func quickGatewayPolls(t *testing.T) {
	t.Helper()
	old := gatewayPollEvery
	gatewayPollEvery = 5 * time.Millisecond
	t.Cleanup(func() { gatewayPollEvery = old })
}

// The whole job: the checkout is made as for any audit, packed without
// git's directory and handed to Waypoint with the prompt; the runner waits
// out a busy gateway and a queued task, and reports the answer as a CLI's
// audit would, with the model that ran it.
func TestGatewayAuditJob(t *testing.T) {
	quickGatewayPolls(t)
	remote := newGitRepo(t)
	p := newTestProc(t)
	findings := json.RawMessage(`{"summary":"Read it.","findings":[{"title":"Merge reads a nil map"}]}`)
	relay := &fakeRelay{
		starts: []any{
			gatewayTaskView{Status: "refused", ErrorKind: "queue_full", Error: "2 tasks are waiting", RetryAfter: 1},
			gatewayTaskView{ID: "task_1", Status: "queued"},
		},
		polls: []any{
			gatewayTaskView{ID: "task_1", Status: "queued"},
			http.StatusBadGateway, // Waypoint restarting: the task runs on
			gatewayTaskView{ID: "task_1", Status: "unknown", ErrorKind: "unreachable", RetryAfter: 30},
			gatewayTaskView{ID: "task_1", Status: "running"},
			gatewayTaskView{ID: "task_1", Status: "succeeded", Model: "deepseek/deepseek-v4.1-flash", Text: "(as JSON)", JSON: findings, InputTokens: 9000, OutputTokens: 700, CostUSD: 0.002},
		},
	}
	relay.serve(t)
	run := newRun(time.Minute)
	o := p.execAgent(context.Background(), run, "audit", agentJob{CLI: gatewayCLI, Model: "audit", Prompt: "Audit this checkout.", Repo: remote, Base: "main", ReadOnly: true, JSONSchema: `{"type":"object"}`})
	if o.State != "done" {
		t.Fatalf("outcome = %+v\n%s", o, run.out.text())
	}
	res, _ := o.Result.(map[string]any)
	raw, _ := json.Marshal(res)
	var got struct {
		CLI, Model, Answer, Base string
		Task                     string `json:"gateway_task"`
		Structured               struct {
			Findings []struct{ Title string }
		}
		Tokens struct {
			Input, Output int64
			CostUSD       float64 `json:"cost_usd"`
		}
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.CLI != gatewayCLI || got.Model != "deepseek/deepseek-v4.1-flash" || got.Task != "task_1" || got.Answer != "(as JSON)" || len(got.Base) != 40 ||
		len(got.Structured.Findings) != 1 || got.Tokens.Input != 9000 || got.Tokens.Output != 700 || res["gateway_error"] != nil {
		t.Errorf("result = %s", raw)
	}
	uploads, asked, cancels := relay.counts()
	if uploads != 2 || asked != 5 || cancels != 0 || relay.prompts[1] != "Audit this checkout." || !bytes.Equal(relay.packed[0], relay.packed[1]) {
		t.Errorf("%d uploads, %d questions, %d cancels; prompts %q", uploads, asked, cancels, relay.prompts)
	}
	files := unpack(t, relay.packed[1])
	if files["a.txt"] != "-rw-r--r-- one\n" || files["src/b.txt"] == "" {
		t.Errorf("the checkout as packed: %v", files)
	}
	for name := range files {
		if strings.HasPrefix(name, ".git/") {
			t.Fatalf("git's directory went along: %s", name)
		}
	}
	for _, want := range []string{"Packed the checkout for the gateway", "The gateway is busy (queue_full)", "took the audit as task task_1", "Task task_1 is running", "deepseek/deepseek-v4.1-flash answered"} {
		if !strings.Contains(run.out.text(), want) {
			t.Errorf("the log lacks %q:\n%s", want, run.out.text())
		}
	}
}

// How a task that doesn't answer ends the job: the gateway's kind goes in
// the result, where Waypoint reads what the audit's chain does next.
func TestGatewayAuditJobFailures(t *testing.T) {
	quickGatewayPolls(t)
	exec := func(t *testing.T, relay *fakeRelay, ctx context.Context, timeout time.Duration) (outcome, map[string]any, *jobRun) {
		t.Helper()
		relay.serve(t)
		work := t.TempDir()
		if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("one\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		run := newRun(timeout)
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		p := newTestProc(t)
		if err := os.MkdirAll(p.cacheDir(), 0o750); err != nil { // the checkout's making leaves it there
			t.Fatal(err)
		}
		o := p.execGatewayAudit(ctx, run, agentJob{CLI: gatewayCLI, Model: "audit", Prompt: "Audit it."}, work, map[string]any{"cli": gatewayCLI})
		res, _ := o.Result.(map[string]any)
		return o, res, run
	}
	taken := gatewayTaskView{ID: "task_1", Status: "queued"}

	for name, c := range map[string]struct {
		relay            *fakeRelay
		kind, says       string
		uploads, cancels int
	}{
		"refused for good": {&fakeRelay{starts: []any{gatewayTaskView{Status: "refused", ErrorKind: "not_found", Error: "no model named audit"}}}, "not_found", "no model named audit", 1, 0},
		"spent":            {&fakeRelay{starts: []any{gatewayTaskView{Status: "refused", ErrorKind: "quota", Error: "the allowance is spent"}}}, "quota", "allowance", 1, 0},
		"a wait past the job's patience": {&fakeRelay{starts: []any{gatewayTaskView{Status: "refused", ErrorKind: "rate_limit", RetryAfter: int(gatewayAdmitWait/time.Second) + 1}}},
			"rate_limit", "didn't take the audit", 1, 0},
		"Waypoint refuses": {&fakeRelay{starts: []any{http.StatusBadRequest}}, "refused", "Waypoint didn't take", 1, 0},
		"Waypoint is away": {&fakeRelay{starts: []any{http.StatusBadGateway}}, "unreachable", "Waypoint didn't take", 1, 0},
		"failed there": {&fakeRelay{starts: []any{taken}, polls: []any{gatewayTaskView{ID: "task_1", Status: "failed", Model: "cerebras/gpt-oss-120b", ErrorKind: "invalid_output", Error: "not the schema", Text: "I found nothing."}}},
			"invalid_output", "not the schema", 1, 0},
		"cancelled there": {&fakeRelay{starts: []any{taken}, polls: []any{gatewayTaskView{ID: "task_1", Status: "cancelled"}}}, "server", "was cancelled there", 1, 0},
		"restarted under it, again and again": {&fakeRelay{starts: []any{taken}, polls: []any{gatewayTaskView{ID: "task_1", Status: "failed", ErrorKind: "interrupted", Error: "the gateway restarted"}}},
			"interrupted", "restarted", 1 + gatewayResends, 0},
		"more findings than a result holds": {&fakeRelay{starts: []any{taken}, polls: []any{gatewayTaskView{ID: "task_1", Status: "succeeded", Model: "m/x",
			JSON: json.RawMessage(`{"summary":"` + strings.Repeat("x", gatewayJSONMax) + `"}`)}}}, "output_limit", "more than a job's result holds", 1, 0},
	} {
		t.Run(name, func(t *testing.T) {
			o, res, run := exec(t, c.relay, context.Background(), time.Minute)
			if o.State != "failed" || res["gateway_error"] != c.kind || !strings.Contains(o.Error, c.says) || res["cli"] != gatewayCLI {
				t.Errorf("outcome = %s %q, result %v\n%s", o.State, o.Error, res, run.out.text())
			}
			if uploads, _, cancels := c.relay.counts(); uploads != c.uploads || cancels != c.cancels {
				t.Errorf("%d uploads, %d cancels", uploads, cancels)
			}
		})
	}
	// What the failed task did say is kept for the audit.
	t.Run("the answer is kept", func(t *testing.T) {
		_, res, _ := exec(t, &fakeRelay{starts: []any{taken}, polls: []any{gatewayTaskView{ID: "task_1", Status: "failed", Model: "cerebras/gpt-oss-120b", ErrorKind: "invalid_output", Text: "I found nothing."}}},
			context.Background(), time.Minute)
		if res["answer"] != "I found nothing." || res["model"] != "cerebras/gpt-oss-120b" || res["gateway_task"] != "task_1" {
			t.Errorf("result = %v", res)
		}
	})
	t.Run("a task interrupted once is sent again", func(t *testing.T) {
		relay := &fakeRelay{starts: []any{taken, gatewayTaskView{ID: "task_2", Status: "running"}},
			polls: []any{gatewayTaskView{ID: "task_1", Status: "failed", ErrorKind: "interrupted"}, gatewayTaskView{ID: "task_2", Status: "succeeded", Model: "m/x", Text: "Nothing found."}}}
		o, res, run := exec(t, relay, context.Background(), time.Minute)
		if o.State != "done" || res["gateway_task"] != "task_2" || res["structured"] != nil || !strings.Contains(run.out.text(), "sending the audit again") {
			t.Errorf("outcome = %+v\n%s", o, run.out.text())
		}
	})
	// The job's time over, or the job stopped: the task is stopped too.
	t.Run("out of time", func(t *testing.T) {
		relay := &fakeRelay{starts: []any{taken}, polls: []any{gatewayTaskView{ID: "task_1", Status: "running"}}}
		o, res, _ := exec(t, relay, context.Background(), 300*time.Millisecond)
		if _, _, cancels := relay.counts(); o.State != "failed" || res["gateway_error"] != "timeout" || res["gateway_task"] != "task_1" || cancels != 1 || !strings.Contains(o.Error, "timed out") {
			t.Errorf("outcome = %+v (%d cancels)", o, cancels)
		}
	})
	t.Run("stopped", func(t *testing.T) {
		relay := &fakeRelay{starts: []any{taken}, polls: []any{gatewayTaskView{ID: "task_1", Status: "running"}}}
		ctx, stop := context.WithCancel(context.Background())
		time.AfterFunc(200*time.Millisecond, stop)
		o, _, _ := exec(t, relay, ctx, time.Minute)
		if _, _, cancels := relay.counts(); o.State != "cancelled" || cancels != 1 {
			t.Errorf("outcome = %+v (%d cancels)", o, cancels)
		}
	})
	t.Run("Waypoint forgot the task", func(t *testing.T) {
		relay := &fakeRelay{starts: []any{taken}, polls: []any{http.StatusNotFound}}
		o, _, _ := exec(t, relay, context.Background(), time.Minute)
		if o.State != "failed" || !strings.Contains(o.Error, "no longer knows") {
			t.Errorf("outcome = %+v", o)
		}
	})
}

// noise is n bytes that gzip can't shrink and git takes for text: random,
// none of them NUL.
func noise(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	for i := range b {
		if b[i] == 0 {
			b[i] = 1
		}
	}
	return string(b)
}

// leftList reads the packed archive's list of what was left out: path to reason.
func leftList(t *testing.T, packed []byte) map[string]string {
	t.Helper()
	list, ok := unpack(t, packed)[".waypoint-audit/left-out.txt"]
	if !ok {
		return nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(strings.TrimPrefix(list, "-rw-r--r-- ")), "\n") {
		if f := strings.Split(line, "\t"); len(f) == 3 {
			out[f[0]] = f[2]
		}
	}
	return out
}

func TestPackCheckoutLeavesOutWhatItIsAskedTo(t *testing.T) {
	work := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("code.go", "package x\n")
	write("model.npz", "PK\x00\x01binary")
	write("late-nul.dat", strings.Repeat("a", 7999)+"\x00")
	write("after-8000.dat", strings.Repeat("a", 8000)+"\x00") // git looks at the first 8000 bytes only
	write("big.txt", strings.Repeat("a", 20000))
	write("gen/out.txt", "generated")
	write(".waypoint-audit/change.diff", "diff --git a/big.txt b/big.txt\n")
	var buf bytes.Buffer
	res, err := packCheckout(work, &buf, packOptions{ArchiveMax: 1 << 20, FileMax: 10000, Over: 100,
		Out:  func(p string) bool { return strings.HasPrefix(p, "gen/") },
		Keep: func(p string) bool { return p == "big.txt" || strings.HasPrefix(p, ".waypoint-audit/") }})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"model.npz": "binary", "late-nul.dat": "binary", "big.txt": "large", "gen/out.txt": "out of scope", "after-8000.dat": "over 100 B"}
	got := leftList(t, buf.Bytes())
	for name, reason := range want {
		if got[name] != reason {
			t.Errorf("%s left out as %q, want %q (%v)", name, got[name], reason, got)
		}
	}
	if len(got) != len(want) || res.Files != 2 || res.leftOutText() == "" {
		t.Errorf("left out %v; %d files packed", got, res.Files)
	}
	packed := unpack(t, buf.Bytes())
	if packed["code.go"] == "" || packed[".waypoint-audit/change.diff"] == "" {
		t.Errorf("packed %v", packed)
	}
}

func TestLeftOutText(t *testing.T) {
	for name, c := range map[string]struct {
		left []leftOut
		want string
	}{
		"nothing":    {nil, ""},
		"one binary": {[]leftOut{{"a.npz", 1 << 20, "binary"}}, "1 binary file (1 MiB)"},
		"two kinds": {[]leftOut{{"a", 3 << 19, "binary"}, {"b", 1 << 19, "binary"}, {"c", 3 << 20, "over 256 KiB"}, {"d", 1 << 20, "over 256 KiB"}},
			"2 binary files (2 MiB), 2 files over 256 KiB (4 MiB)"},
		"scope and large": {[]leftOut{{"a", 6 << 20, "large"}, {"b", 100 << 10, "out of scope"}}, "1 file over the gateway's per-file limit (6 MiB), 1 file out of scope (100 KiB)"},
		"three large":     {[]leftOut{{"a", 6 << 20, "large"}, {"b", 6 << 20, "large"}, {"c", 6 << 20, "large"}}, "3 files over the gateway's per-file limit (18 MiB)"},
	} {
		if got := (packResult{Left: c.left}).leftOutText(); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

func TestDiffGitNames(t *testing.T) {
	for in, want := range map[string][]string{
		"a/x.go b/x.go":                         {"x.go"},
		"a/dir/my file.go b/dir/my file.go":     {"dir/my file.go"},
		"a/old.go b/new/old2.go":                {"old.go", "new/old2.go"},
		`"a/caf\303\251.go" "b/caf\303\251.go"`: {"café.go", "café.go"},
	} {
		if got := diffGitNames(in); !slices.Equal(got, want) {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

// A checkout too large for the gateway is packed again with stricter limits,
// each only when the one before didn't fit.
func TestPackForGatewayTiers(t *testing.T) {
	type file struct {
		path string
		size int
	}
	for name, c := range map[string]struct {
		files          []file
		changed        []string
		paths, exclude []string
		limit          int64
		left           map[string]string // path to reason; nil: nothing
		tooLarge       bool
	}{
		"fits whole":        {files: []file{{"a.txt", 5000}}, limit: 100 << 10},
		"over 1 MiB out":    {files: []file{{"big.txt", 2 << 20}, {"s.txt", 300 << 10}}, limit: 400 << 10, left: map[string]string{"big.txt": "over 1 MiB"}},
		"over 256 KiB out":  {files: []file{{"x.txt", 600 << 10}, {"y.txt", 100 << 10}}, limit: 300 << 10, left: map[string]string{"x.txt": "over 256 KiB"}},
		"over 64 KiB out":   {files: []file{{"x.txt", 100 << 10}, {"y.txt", 100 << 10}, {"z.txt", 30 << 10}}, limit: 80 << 10, left: map[string]string{"x.txt": "over 64 KiB", "y.txt": "over 64 KiB"}},
		"outside the paths": {files: []file{{"src/in.txt", 100 << 10}, {"other/out.txt", 200 << 10}}, paths: []string{"src"}, limit: 150 << 10, left: map[string]string{"other/out.txt": "out of scope"}},
		"excluded":          {files: []file{{"src/in.txt", 100 << 10}, {"gen/out.txt", 200 << 10}}, exclude: []string{"gen"}, limit: 150 << 10, left: map[string]string{"gen/out.txt": "out of scope"}},
		"scope is no help":  {files: []file{{"src/a.txt", 100 << 10}, {"src/b.txt", 100 << 10}, {"src/c.txt", 30 << 10}}, paths: []string{"src"}, limit: 80 << 10, left: map[string]string{"src/a.txt": "over 64 KiB", "src/b.txt": "over 64 KiB"}},
		"the change stays":  {files: []file{{"c.txt", 600 << 10}, {"o.txt", 200 << 10}}, changed: []string{"c.txt"}, limit: 700 << 10, left: map[string]string{"o.txt": "over 64 KiB"}},
		"the change stays in scope's tier": {files: []file{{"in/c.txt", 100 << 10}, {"out/o.txt", 100 << 10}}, changed: []string{"out/o.txt"}, paths: []string{"in"}, limit: 150 << 10,
			left: map[string]string{"in/c.txt": "over 64 KiB"}},
		"still too large": {files: []file{{"c.txt", 600 << 10}}, changed: []string{"c.txt"}, limit: 300 << 10, tooLarge: true},
	} {
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			gitOut(t, work, "init", "-q")
			for _, f := range c.files {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(work, f.path)), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(work, f.path), []byte(noise(t, f.size)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			gitOut(t, work, "add", "-A")
			if err := os.MkdirAll(filepath.Join(work, ".waypoint-audit"), 0o750); err != nil {
				t.Fatal(err)
			}
			var diff strings.Builder
			for _, f := range c.changed {
				fmt.Fprintf(&diff, "diff --git a/%s b/%s\n+x\n", f, f)
			}
			if err := os.WriteFile(filepath.Join(work, ".waypoint-audit/change.diff"), []byte(diff.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			archive, err := os.CreateTemp(t.TempDir(), "pack")
			if err != nil {
				t.Fatal(err)
			}
			defer archive.Close()
			p, run := newTestProc(t), newRun(time.Minute)
			_, tier, err := p.packForGateway(context.Background(), run, agentJob{DiffPaths: c.paths, DiffExclude: c.exclude}, work, archive, c.limit, 5<<20)
			if c.tooLarge {
				if !errors.Is(err, errArchiveTooLarge) {
					t.Fatalf("err = %v\n%s", err, run.out.text())
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v\n%s", err, run.out.text())
			}
			size, _ := archive.Seek(0, io.SeekCurrent)
			if size > c.limit {
				t.Errorf("packed %d bytes, limit %d", size, c.limit)
			}
			packed, _ := os.ReadFile(archive.Name())
			files := unpack(t, packed)
			got := leftList(t, packed)
			if len(got) != len(c.left) || (c.left == nil) != (tier == "") {
				t.Errorf("left out %v (tier %q), want %v\n%s", got, tier, c.left, run.out.text())
			}
			for path, reason := range c.left {
				if got[path] != reason || files[path] != "" {
					t.Errorf("%s: %q, in the archive: %v", path, got[path], files[path] != "")
				}
			}
			for _, f := range c.files {
				if _, out := c.left[f.path]; !out && files[f.path] == "" {
					t.Errorf("%s is missing", f.path)
				}
			}
			if files[".waypoint-audit/change.diff"] == "" {
				t.Error("the change's own copy is missing")
			}
		})
	}
}

// With no way to tell the audit's scope, only sizes tighten, and the log says so.
func TestPackForGatewayScopeUnknown(t *testing.T) {
	work := t.TempDir() // not a git repository
	for f, n := range map[string]int{"x.txt": 100 << 10, "y.txt": 30 << 10} {
		if err := os.WriteFile(filepath.Join(work, f), []byte(noise(t, n)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archive, err := os.CreateTemp(t.TempDir(), "pack")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	p, run := newTestProc(t), newRun(time.Minute)
	res, _, err := p.packForGateway(context.Background(), run, agentJob{DiffPaths: []string{"src"}}, work, archive, 110<<10, 5<<20)
	if err != nil || len(res.Left) != 1 || res.Left[0].Reason != "over 64 KiB" || !strings.Contains(run.out.text(), "going by file sizes alone") {
		t.Errorf("%+v, %v\n%s", res, err, run.out.text())
	}
}

// The limits a job carries are the gateway's; without them the runner's own.
func TestGatewayAuditJobLimits(t *testing.T) {
	quickGatewayPolls(t)
	run := func(t *testing.T, in agentJob, files map[string]string) (outcome, *fakeRelay, *jobRun) {
		t.Helper()
		relay := &fakeRelay{starts: []any{gatewayTaskView{ID: "task_1", Status: "queued"}},
			polls: []any{gatewayTaskView{ID: "task_1", Status: "succeeded", Model: "m/x", Text: "Nothing found."}}}
		relay.serve(t)
		work := t.TempDir()
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		p := newTestProc(t)
		if err := os.MkdirAll(p.cacheDir(), 0o750); err != nil {
			t.Fatal(err)
		}
		in.CLI, in.Model, in.Prompt = gatewayCLI, "audit", "Audit it."
		r := newRun(time.Minute)
		return p.execGatewayAudit(context.Background(), r, in, work, map[string]any{"cli": gatewayCLI}), relay, r
	}
	t.Run("the job's file limit", func(t *testing.T) {
		o, relay, r := run(t, agentJob{GatewayFileMax: 100 << 10}, map[string]string{"a.txt": "one\n", "b.txt": strings.Repeat("b", 100<<10+1)})
		res, _ := o.Result.(map[string]any)
		if o.State != "done" || res["left_out"] != "1 file over the gateway's per-file limit (100.0 KiB)" {
			t.Fatalf("outcome = %+v\n%s", o, r.out.text())
		}
		if got := leftList(t, relay.packed[0]); got["b.txt"] != "large" || len(got) != 1 {
			t.Errorf("left out %v", got)
		}
		if p := relay.prompts[0]; !strings.HasPrefix(p, "Audit it.") || !strings.Contains(p, ".waypoint-audit/left-out.txt") || !strings.Contains(p, "1 file over") {
			t.Errorf("prompt = %q", p)
		}
		if !strings.Contains(r.out.text(), "Left out 1 file") {
			t.Errorf("log:\n%s", r.out.text())
		}
	})
	t.Run("the fallback without them", func(t *testing.T) {
		o, relay, _ := run(t, agentJob{}, map[string]string{"a.txt": "one\n", "b.txt": strings.Repeat("b", gatewayFileFallback+1)})
		res, _ := o.Result.(map[string]any)
		if o.State != "done" || !strings.HasPrefix(fmt.Sprint(res["left_out"]), "1 file over") || leftList(t, relay.packed[0])["b.txt"] != "large" {
			t.Errorf("outcome = %+v", o)
		}
	})
	t.Run("nothing left out says nothing", func(t *testing.T) {
		o, relay, _ := run(t, agentJob{}, map[string]string{"a.txt": "one\n"})
		res, _ := o.Result.(map[string]any)
		if _, ok := res["left_out"]; ok || o.State != "done" || relay.prompts[0] != "Audit it." || leftList(t, relay.packed[0]) != nil {
			t.Errorf("outcome = %+v, prompt %q", o, relay.prompts[0])
		}
	})
	t.Run("past the job's archive limit", func(t *testing.T) {
		o, relay, _ := run(t, agentJob{GatewayArchiveMax: 20 << 10}, map[string]string{"a.txt": noise(t, 30<<10)})
		res, _ := o.Result.(map[string]any)
		if uploads, _, _ := relay.counts(); o.State != "failed" || res["gateway_error"] != "body_too_large" || !strings.Contains(o.Error, "20 KiB") || !strings.Contains(o.Error, "64 KiB") || uploads != 0 {
			t.Errorf("outcome = %+v", o)
		}
		if strings.Contains(o.Error, "paths") {
			t.Errorf("no scope, but the error speaks of one: %s", o.Error)
		}
	})
	// The failed attempt says what was left out, and, with a scope, that the
	// last pass left out what the audit's paths do.
	t.Run("too large with files left out", func(t *testing.T) {
		o, relay, _ := run(t, agentJob{GatewayArchiveMax: 20 << 10}, map[string]string{"a.txt": noise(t, 30<<10), "b.bin": "x\x00y"})
		res, _ := o.Result.(map[string]any)
		if uploads, _, _ := relay.counts(); o.State != "failed" || res["gateway_error"] != "body_too_large" || res["left_out"] != "1 binary file (3 B)" || uploads != 0 {
			t.Errorf("outcome = %+v", o)
		}
	})
	t.Run("too large with a scope", func(t *testing.T) {
		work := t.TempDir()
		gitOut(t, work, "init", "-q")
		for name, body := range map[string]string{"src/in.txt": noise(t, 30<<10), "other/out.txt": "outside\n"} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		gitOut(t, work, "add", "-A")
		p := newTestProc(t)
		if err := os.MkdirAll(p.cacheDir(), 0o750); err != nil {
			t.Fatal(err)
		}
		relay := &fakeRelay{}
		relay.serve(t)
		o := p.execGatewayAudit(context.Background(), newRun(time.Minute), agentJob{CLI: gatewayCLI, Model: "audit", Prompt: "Audit it.", GatewayArchiveMax: 20 << 10, DiffPaths: []string{"src"}}, work, map[string]any{})
		res, _ := o.Result.(map[string]any)
		if o.State != "failed" || res["gateway_error"] != "body_too_large" || !strings.Contains(o.Error, "outside the audit's paths") || res["left_out"] != "1 file out of scope (8 B)" {
			t.Errorf("outcome = %+v", o)
		}
	})
}

// An untracked file under the audit's paths is in scope.
func TestScopeOutCountsUntrackedFiles(t *testing.T) {
	work := t.TempDir()
	gitOut(t, work, "init", "-q")
	for _, name := range []string{"specs/tracked.md", "specs/untracked.md", "gen/tracked.go", "gen/untracked.go", "other/x.go"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, name), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitOut(t, work, "add", "specs/tracked.md", "gen/tracked.go", "other/x.go")
	p, run := newTestProc(t), newRun(time.Minute)
	out := p.scopeOut(context.Background(), run, work, agentJob{DiffPaths: []string{"specs"}, DiffExclude: []string{"gen"}})
	if out == nil {
		t.Fatalf("no scope:\n%s", run.out.text())
	}
	for path, want := range map[string]bool{"specs/tracked.md": false, "specs/untracked.md": false, "gen/tracked.go": true, "gen/untracked.go": true, "other/x.go": true} {
		if got := out(path); got != want {
			t.Errorf("out(%s) = %v, want %v", path, got, want)
		}
	}
}

// git's rule is a NUL in the first 8000 bytes: one later doesn't make a file
// binary, and with no size limit set it stays.
func TestPackCheckoutNulAfter8000Stays(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "late.dat"), []byte(strings.Repeat("a", 8000)+"\x00tail"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	res, err := packCheckout(work, &buf, packOptions{ArchiveMax: 1 << 20, FileMax: 1 << 20})
	if err != nil || res.Files != 1 || len(res.Left) != 0 {
		t.Fatalf("%+v, %v", res, err)
	}
	if got := unpack(t, buf.Bytes()); !strings.HasSuffix(got["late.dat"], "\x00tail") {
		t.Errorf("packed %d files", len(got))
	}
}

// The list's header names the reasons that apply: size and scope only when
// the whole checkout was too big.
func TestLeftOutHeader(t *testing.T) {
	plain := leftOutHeader([]leftOut{{"a", 1, "binary"}, {"b", 1, "large"}})
	tight := leftOutHeader([]leftOut{{"a", 1, "binary"}, {"b", 1, "over 256 KiB"}, {"c", 1, "out of scope"}})
	for name, h := range map[string]string{"plain": plain, "tight": tight} {
		for _, want := range []string{"binary", "per-file limit", "Their absence is not a finding. Columns: path, bytes, reason.\n"} {
			if !strings.Contains(h, want) {
				t.Errorf("%s header lacks %q:\n%s", name, want, h)
			}
		}
		if strings.ContainsAny(h, "\u2013\u2014") || strings.Contains(h, " - ") {
			t.Errorf("%s header has a dash:\n%s", name, h)
		}
	}
	if strings.Contains(plain, "too big") || strings.Contains(plain, "audit's paths") {
		t.Errorf("plain header speaks of the fit cuts:\n%s", plain)
	}
	if !strings.Contains(tight, "too big for the gateway") || !strings.Contains(tight, "audit's paths") {
		t.Errorf("tight header:\n%s", tight)
	}
}
