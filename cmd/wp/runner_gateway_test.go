//go:build !windows

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
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
	big, err := os.Create(filepath.Join(work, "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := big.Truncate(gatewayFileMax + 1); err != nil {
		t.Fatal(err)
	}
	_ = big.Close()

	var buf bytes.Buffer
	files, left, err := packCheckout(work, &buf, gatewayArchiveMax)
	if err != nil {
		t.Fatal(err)
	}
	got := unpack(t, buf.Bytes())
	want := map[string]string{
		"a.txt": "-rw-r--r-- one\n", "cmd/run.sh": "-rwxr-xr-x #!/bin/sh\n", "vendor/mod/m.go": "-rw-r--r-- package mod\n",
		".waypoint-audit/change.diff": "-rw-r--r-- +new\n",
	}
	if files != len(want) || len(got) != len(want) || !slices.Equal(left, []string{"data.bin"}) {
		t.Errorf("packed %d files (%d in the archive), left %v", files, len(got), left)
	}
	for name, body := range want {
		if got[name] != body {
			t.Errorf("%s = %q, want %q", name, got[name], body)
		}
	}

	noise := make([]byte, 64<<10)
	for i := range noise {
		noise[i] = byte(i*7 + i>>8*13 + i>>3)
	}
	write("noise.bin", string(noise), 0o600)
	if _, _, err := packCheckout(work, io.Discard, 300); !errors.Is(err, errArchiveTooLarge) {
		t.Errorf("past the limit: %v", err)
	}
	if _, _, err := packCheckout(filepath.Join(work, "missing"), io.Discard, 300); err == nil || errors.Is(err, errArchiveTooLarge) {
		t.Errorf("no checkout: %v", err)
	}
}

// fakeRelay is Waypoint's side of a job's gateway task: what each upload
// and each question is answered with, in turn (the last answer stays).
type fakeRelay struct {
	mu      sync.Mutex
	starts  []any
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
			http.StatusBadGateway,
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
		if err := os.MkdirAll(p.cacheDir(), 0o750); err != nil {
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
