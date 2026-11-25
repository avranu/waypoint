package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// traceTx builds a transcript: each call is an assistant line and its result.
type traceTx struct {
	t     *testing.T
	root  string
	lines []string
	n     int
	ts    time.Time
	ctx   int
}

func newTraceTx(t *testing.T) *traceTx {
	return &traceTx{t: t, root: "/work/app", ts: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC), ctx: 40000}
}

func (x *traceTx) add(v map[string]any) {
	b, err := json.Marshal(v)
	if err != nil {
		x.t.Fatal(err)
	}
	x.lines = append(x.lines, string(b))
}

// call adds a tool call and its result; out is the result text.
func (x *traceTx) call(name string, input map[string]any, out string, isErr bool, sidechain bool) {
	x.n++
	x.ts = x.ts.Add(5 * time.Second)
	x.ctx += 1000
	id := fmt.Sprintf("toolu_%d", x.n)
	x.add(map[string]any{"type": "assistant", "timestamp": x.ts.Format(time.RFC3339Nano), "cwd": x.root, "isSidechain": sidechain,
		"message": map[string]any{"id": fmt.Sprintf("msg_%d", x.n), "model": "claude-opus-5-5",
			"usage":   map[string]any{"input_tokens": 3, "output_tokens": 10, "cache_creation_input_tokens": 100, "cache_read_input_tokens": x.ctx},
			"content": []any{map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}}}})
	x.ts = x.ts.Add(time.Second)
	x.add(map[string]any{"type": "user", "timestamp": x.ts.Format(time.RFC3339Nano), "cwd": x.root, "isSidechain": sidechain,
		"message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "is_error": isErr, "content": out}}}})
}

func (x *traceTx) bash(cmd, out string) {
	x.call("Bash", map[string]any{"command": cmd}, out, false, false)
}
func (x *traceTx) take(key string)        { x.bash("wp next "+key, key+" \"Title\" (story)\nbody") }
func (x *traceTx) file(rel string) string { return filepath.Join(x.root, rel) }

func (x *traceTx) write(t *testing.T) usageFile {
	p := filepath.Join(t.TempDir(), "projects", "-work-app", "sess1.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(x.lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return usageFile{Path: p, Session: "sess1", Kind: "main", Main: true}
}

func readTraces(t *testing.T, f usageFile, st usageFileState) (*usageParsed, []usageTraceRow) {
	t.Helper()
	p, err := readUsageFile(f, st, func(string) string { return "owner/app" }, false)
	if err != nil {
		t.Fatal(err)
	}
	return p, p.Chunk.Traces
}

func TestTraceSegments(t *testing.T) {
	tests := []struct {
		name  string
		build func(x *traceTx)
		want  []usageTraceRow
	}{
		{"take to ship", func(x *traceTx) {
			x.take("WP-1")
			x.call("Grep", map[string]any{"pattern": "foo"}, "a.go\nb.go", false, false)
			x.call("Grep", map[string]any{"pattern": "foo", "path": x.file("a.go")}, "hit", false, false)
			x.call("Read", map[string]any{"file_path": x.file("a.go")}, "code", false, false)
			x.call("Edit", map[string]any{"file_path": x.file("a.go")}, "ok", false, false)
			x.call("Write", map[string]any{"file_path": x.file("new.go")}, "ok", false, false)
			x.call("Agent", map[string]any{"prompt": "x"}, "done", false, false)
			x.bash("go test ./...", "ok")
			x.bash("wp ship -m done", "shipped")
		}, []usageTraceRow{{Key: "WP-1", Seq: 1, Ended: "ship", Calls: 7, Locate: 1, Infile: 1, Reads: 1, Edits: 2, Delegated: 1, Other: 1, PreCalls: 3}}},
		{"two tasks in one session", func(x *traceTx) {
			x.take("WP-1")
			x.call("Read", map[string]any{"file_path": x.file("a.go")}, "code", false, false)
			x.take("WP-2")
			x.call("Read", map[string]any{"file_path": x.file("b.go")}, "code", false, false)
			x.bash("wp done", "ok")
		}, []usageTraceRow{
			{Key: "WP-1", Seq: 1, Ended: "next", Calls: 1, Reads: 1, PreCalls: 1},
			{Key: "WP-2", Seq: 2, Ended: "done", Calls: 1, Reads: 1, PreCalls: 1},
		}},
		{"calls outside segments are ignored", func(x *traceTx) {
			x.call("Read", map[string]any{"file_path": x.file("a.go")}, "code", false, false)
			x.take("WP-1")
			x.call("Read", map[string]any{"file_path": x.file("a.go")}, "code", false, false)
			x.bash("wp release", "ok")
			x.call("Read", map[string]any{"file_path": x.file("z.go")}, "code", false, false)
		}, []usageTraceRow{{Key: "WP-1", Seq: 1, Ended: "release", Calls: 1, Reads: 1, PreCalls: 1}}},
		{"error result on the take starts nothing", func(x *traceTx) {
			x.call("Bash", map[string]any{"command": "wp next WP-9"}, "KEY \"x\"", true, false)
			x.call("Read", map[string]any{"file_path": x.file("a.go")}, "code", false, false)
		}, nil},
		{"a take that names no key starts nothing", func(x *traceTx) {
			x.bash("wp next", "Nothing to do.")
		}, nil},
		{"sidechain calls are not counted", func(x *traceTx) {
			x.take("WP-1")
			x.call("Read", map[string]any{"file_path": x.file("a.go")}, "code", false, true)
			x.call("Read", map[string]any{"file_path": x.file("a.go")}, "code", false, false)
		}, []usageTraceRow{{Key: "WP-1", Seq: 1, Ended: "", Calls: 1, Reads: 1, PreCalls: 1}}},
		{"a take whose answer opens with a note", func(x *traceTx) {
			x.bash("wp next", "(Passed over WP-5: its files are being changed in WP-4.)\nWP-1 \"Title\" (story)\nbody")
			x.call("Read", map[string]any{"file_path": x.file("a.go")}, "code", false, false)
			x.bash("wp ship WP-1 -m done", "shipped")
		}, []usageTraceRow{{Key: "WP-1", Seq: 1, Ended: "ship", Calls: 1, Reads: 1, PreCalls: 1}}},
		{"a task autopilot gave", func(x *traceTx) {
			x.bash("wp next", "Autopilot assigned WP-7 to you: \"Title\" (story)\nbody")
			x.call("Read", map[string]any{"file_path": x.file("a.go")}, "code", false, false)
		}, []usageTraceRow{{Key: "WP-7", Seq: 1, Ended: "", Calls: 1, Reads: 1, PreCalls: 1}}},
		{"reading the held task again goes on with its take", func(x *traceTx) {
			x.take("WP-1")
			x.call("Read", map[string]any{"file_path": x.file("a.go")}, "code", false, false)
			x.bash("wp next WP-1", "(You already hold this task.)\nWP-1 \"Title\" (story)\nbody")
			x.call("Read", map[string]any{"file_path": x.file("b.go")}, "code", false, false)
		}, []usageTraceRow{{Key: "WP-1", Seq: 1, Ended: "", Calls: 2, Reads: 2, PreCalls: 2}}},
		{"an edit the tool refused is no edit", func(x *traceTx) {
			x.take("WP-1")
			x.call("Edit", map[string]any{"file_path": x.file("a.go")}, "File has not been read yet.", true, false)
			x.call("Read", map[string]any{"file_path": x.file("a.go")}, "code", false, false)
			x.call("Edit", map[string]any{"file_path": x.file("a.go")}, "ok", false, false)
		}, []usageTraceRow{{Key: "WP-1", Seq: 1, Ended: "", Calls: 3, Reads: 1, Edits: 1, Other: 1, PreCalls: 2}}},
		{"a look, a usage text, a refused ship and another task's ship end nothing", func(x *traceTx) {
			x.take("WP-1")
			x.bash("wp next --peek", "WP-2 \"Other\" (story)\nbody")
			x.bash("wp ship -h", "usage")
			x.call("Bash", map[string]any{"command": "wp ship -m done"}, "wp: tests failed", true, false)
			x.bash("wp done WP-9 -m x", "ok")
			x.call("Read", map[string]any{"file_path": x.file("a.go")}, "code", false, false)
			x.bash("wp ship WP-1 -m done", "shipped")
		}, []usageTraceRow{{Key: "WP-1", Seq: 1, Ended: "ship", Calls: 5, Reads: 1, Other: 4, PreCalls: 5}}},
		{"open segment stays open", func(x *traceTx) {
			x.take("WP-3")
			x.call("Edit", map[string]any{"file_path": x.file("a.go")}, "ok", false, false)
		}, []usageTraceRow{{Key: "WP-3", Seq: 1, Ended: "", Calls: 1, Edits: 1}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x := newTraceTx(t)
			tc.build(x)
			_, rows := readTraces(t, x.write(t), usageFileState{})
			if len(rows) != len(tc.want) {
				t.Fatalf("got %d rows, want %d: %+v", len(rows), len(tc.want), rows)
			}
			for i, w := range tc.want {
				g := rows[i]
				got := usageTraceRow{Key: g.Key, Seq: g.Seq, Ended: g.Ended, Calls: g.Calls, Locate: g.Locate, Infile: g.Infile, Reads: g.Reads,
					Edits: g.Edits, Delegated: g.Delegated, Other: g.Other, PreCalls: g.PreCalls}
				if !reflect.DeepEqual(got, w) {
					t.Errorf("row %d = %+v, want %+v", i, got, w)
				}
				if g.Session != "sess1" {
					t.Errorf("session = %q", g.Session)
				}
			}
		})
	}
}

func TestTraceRowContents(t *testing.T) {
	x := newTraceTx(t)
	x.take("WP-1")
	x.call("Grep", map[string]any{"pattern": "foo"}, "internal/a.go:3: foo", false, false)
	x.call("Read", map[string]any{"file_path": x.file("internal/a.go")}, "code", false, false)
	x.call("Edit", map[string]any{"file_path": x.file("internal/a.go")}, "ok", false, false)
	x.call("Write", map[string]any{"file_path": x.file("cmd/new.go")}, "ok", false, false)
	x.bash("go test ./...", "ok")
	f := x.write(t)
	_, rows := readTraces(t, f, usageFileState{})
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	r := rows[0]
	if r.FirstEditAt == nil || r.CtxFirstEdit == nil || *r.CtxFirstEdit <= r.CtxStart {
		t.Errorf("first edit = %v ctx %v start %d", r.FirstEditAt, r.CtxFirstEdit, r.CtxStart)
	}
	if r.Model != "claude-opus-5-5" {
		t.Errorf("model = %q", r.Model)
	}
	byPath := map[string]usageTraceFile{}
	for _, fl := range r.Files {
		byPath[fl.Path] = fl
	}
	a, n := byPath["internal/a.go"], byPath["cmd/new.go"]
	if !a.Edited || !a.Read || !a.Seen {
		t.Errorf("a.go = %+v: want edited, read, named earlier", a)
	}
	if !n.Created || !n.Edited {
		t.Errorf("new.go = %+v: want created and edited", n)
	}
	blob, _ := json.Marshal(r)
	if strings.Contains(string(blob), x.root) || strings.Contains(string(blob), "go test") {
		t.Errorf("row leaks an absolute path or a command: %s", blob)
	}
}

func TestTraceResumeEqualsSinglePass(t *testing.T) {
	x := newTraceTx(t)
	x.take("WP-1")
	x.call("Grep", map[string]any{"pattern": "foo"}, "a.go", false, false)
	x.call("Read", map[string]any{"file_path": x.file("a.go")}, "code", false, false)
	x.call("Edit", map[string]any{"file_path": x.file("a.go")}, "ok", false, false)
	x.bash("go vet ./...", "ok")
	x.take("WP-2")
	x.call("Read", map[string]any{"file_path": x.file("b.go")}, "code", false, false)
	full := x.write(t)
	_, want := readTraces(t, full, usageFileState{})
	if len(want) != 2 {
		t.Fatalf("single pass rows = %+v", want)
	}
	data, err := os.ReadFile(full.Path)
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n < len(x.lines); n++ {
		prefix := strings.Join(x.lines[:n], "\n") + "\n"
		if err := os.WriteFile(full.Path, []byte(prefix), 0o600); err != nil {
			t.Fatal(err)
		}
		p1, rows1 := readTraces(t, full, usageFileState{})
		raw, err := json.Marshal(p1.State)
		if err != nil {
			t.Fatal(err)
		}
		var st usageFileState
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full.Path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, rows2 := readTraces(t, full, st)
		last := map[int]usageTraceRow{}
		for _, r := range append(rows1, rows2...) {
			last[r.Seq] = r
		}
		if len(last) != len(want) {
			t.Fatalf("split at %d: %d rows, want %d", n, len(last), len(want))
		}
		for _, w := range want {
			if g := last[w.Seq]; !reflect.DeepEqual(g, w) {
				t.Errorf("split at %d seq %d:\n got %+v\nwant %+v", n, w.Seq, g, w)
			}
		}
	}
	_, again := readTraces(t, full, usageFileState{})
	if !reflect.DeepEqual(again, want) {
		t.Errorf("re-read differs:\n got %+v\nwant %+v", again, want)
	}
}

func TestTraceStateOutdatedReread(t *testing.T) {
	x := newTraceTx(t)
	x.take("WP-1")
	x.call("Read", map[string]any{"file_path": x.file("a.go")}, "code", false, false)
	f := x.write(t)
	p, _ := readTraces(t, f, usageFileState{})
	old := p.State
	old.TV, old.Trace = 0, nil
	p2, rows := readTraces(t, f, old)
	if !p2.TraceOnly || len(rows) != 1 {
		t.Fatalf("TraceOnly=%v rows=%+v", p2.TraceOnly, rows)
	}
	p3, rows3 := readTraces(t, f, p2.State)
	if p3.TraceOnly || len(rows3) != 0 {
		t.Errorf("a current state re-read: TraceOnly=%v rows=%+v", p3.TraceOnly, rows3)
	}
}

func TestClassifyTraceCall(t *testing.T) {
	root := "/work/app"
	in := func(m map[string]any) json.RawMessage { b, _ := json.Marshal(m); return b }
	bash := func(c string) json.RawMessage { return in(map[string]any{"command": c}) }
	tests := []struct {
		name  string
		tool  string
		input json.RawMessage
		class string
		paths []string
	}{
		{"read", "Read", in(map[string]any{"file_path": filepath.Join(root, "a/b.go")}), trRead, []string{"a/b.go"}},
		{"grep over the tree", "Grep", in(map[string]any{"pattern": "x"}), trLocate, nil},
		{"grep in one file", "Grep", in(map[string]any{"pattern": "x", "path": filepath.Join(root, "a/b.go")}), trInfile, []string{"a/b.go"}},
		{"grep in a directory", "Grep", in(map[string]any{"pattern": "x", "path": filepath.Join(root, "a")}), trLocate, nil},
		{"glob", "Glob", in(map[string]any{"pattern": "**/*.go"}), trLocate, nil},
		{"edit", "Edit", in(map[string]any{"file_path": filepath.Join(root, "a/b.go")}), trEdit, []string{"a/b.go"}},
		{"agent", "Agent", in(map[string]any{"prompt": "go"}), trDelegated, nil},
		{"unknown tool", "WebFetch", in(map[string]any{"url": "x"}), trOther, nil},
		{"rg", "Bash", bash("rg foo internal/"), trLocate, nil},
		{"find", "Bash", bash("find . -name '*.go'"), trLocate, nil},
		{"grep a file", "Bash", bash("grep -n foo internal/a.go"), trInfile, []string{"internal/a.go"}},
		{"cat a file", "Bash", bash("cat internal/a.go"), trRead, []string{"internal/a.go"}},
		{"sed -n a file", "Bash", bash("sed -n 1,20p internal/a.go"), trRead, []string{"internal/a.go"}},
		{"sed -i edits", "Bash", bash("sed -i s/a/b/ internal/a.go"), trEdit, []string{"internal/a.go"}},
		{"cat redirect writes", "Bash", bash("cat > internal/a.go <<'EOF'\nx\nEOF"), trEdit, []string{"internal/a.go"}},
		{"a copy by cat edits only what it writes", "Bash", bash("cat internal/a.go > internal/b.go"), trEdit, []string{"internal/b.go"}},
		{"go test is other", "Bash", bash("go test ./..."), trOther, nil},
		{"cd then grep", "Bash", bash("cd internal && grep -rn foo ."), trLocate, nil},
		{"pipe with a read first", "Bash", bash("cat internal/a.go | head -5"), trRead, []string{"internal/a.go"}},
		{"git status is other", "Bash", bash("git status --short"), trOther, nil},

		{"cd then read", "Bash", bash("cd web && cat src/x.ts"), trRead, []string{"web/src/x.ts"}},
		{"cd to the root then grep", "Bash", bash("cd /work/app/web && grep -n foo src/x.ts | head"), trInfile, []string{"web/src/x.ts"}},
		{"cd nowhere the line says", "Bash", bash(`cd "$d" && cat src/x.ts`), trRead, nil},
		{"bash -c", "Bash", bash("bash -c 'cd web && echo x > a.ts'"), trEdit, []string{"web/a.ts"}},

		{"output sent to scratch is not seen", "Bash", bash("cat internal/a.go > /tmp/out.txt"), trOther, nil},
		{"a scratch script run on a file", "Bash", bash("cat > /tmp/x/s.py <<'EOF'\nprint(1)\nEOF\npython3 /tmp/x/s.py internal/a.go"), trOther, nil},
		{"a scratch file by a variable", "Bash", bash("S=/tmp/s; cat > $S/x.py <<'EOF'\nx\nEOF"), trOther, nil},
		{"a scratch file after cd", "Bash", bash("mkdir -p /tmp/s && cd $_ && cat > x.py <<'EOF'\nx\nEOF"), trOther, nil},
		{"a file the line doesn't name", "Bash", bash(`cat > "$f" <<'EOF'` + "\nx\nEOF"), trOther, nil},
		{"echo into a file", "Bash", bash("echo hi >> .gitignore"), trEdit, []string{".gitignore"}},
		{"a later command edits", "Bash", bash("go build ./... && sed -i s/a/b/ internal/a.go"), trEdit, []string{"internal/a.go"}},
		{"sed -i -e", "Bash", bash("sed -i -e s/a/b/ -e s/c/d/ internal/a.go web/b.ts"), trEdit, []string{"internal/a.go", "web/b.ts"}},
		{"sed -i on a glob", "Bash", bash("sed -i s/a/b/ internal/*.go"), trEdit, nil},
		{"sed -i by a path whose start is spelt out", "Bash", bash("for s in a b; do sed -i s/a/b/ clusters/$s/app.yaml; done"), trEdit, nil},
		{"perl -pi", "Bash", bash("perl -0pi -e 's/a/b/' internal/a.go"), trEdit, []string{"internal/a.go"}},
		{"perl prints", "Bash", bash("perl -ne 'print if /a/' internal/a.go"), trOther, nil},
		{"gofmt -w", "Bash", bash("gofmt -w internal/a.go cmd/b.go"), trEdit, []string{"internal/a.go", "cmd/b.go"}},
		{"goimports -w", "Bash", bash("goimports -w internal/a.go"), trEdit, []string{"internal/a.go"}},
		{"prettier --write", "Bash", bash("cd web && npx prettier --write src/x.ts"), trEdit, []string{"web/src/x.ts"}},
		{"prettier -w", "Bash", bash("prettier -w web/src/x.ts"), trEdit, []string{"web/src/x.ts"}},
		{"curl -o", "Bash", bash("curl -sL -o docs/spec.md https://example.com/x"), trEdit, []string{"docs/spec.md"}},
		{"curl --output", "Bash", bash("curl --output docs/spec.md https://example.com/x"), trEdit, []string{"docs/spec.md"}},
		{"curl to scratch is not seen", "Bash", bash("curl -o /tmp/x.json https://example.com/x"), trOther, nil},
		{"gofmt -l lists", "Bash", bash("gofmt -l ."), trOther, nil},
		{"prettier --check", "Bash", bash("prettier --check ."), trOther, nil},
		{"tee", "Bash", bash("tee internal/a.go <<'EOF'\nx\nEOF"), trEdit, []string{"internal/a.go"}},
		{"a generated document", "Bash", bash("go run ./cmd/wp help --all > docs/CLI.md"), trEdit, []string{"docs/CLI.md"}},
		{"a log kept in the checkout", "Bash", bash("go test ./... > test.log 2>&1"), trOther, nil},
		{"git mv", "Bash", bash("git mv internal/a.go internal/b.go"), trEdit, []string{"internal/b.go", "internal/a.go"}},
		{"cp into a directory", "Bash", bash("cp /tmp/s/a.go internal/"), trEdit, []string{"internal/a.go"}},
		{"cp out to scratch", "Bash", bash("cp internal/a.go /tmp/s/a.go"), trOther, nil},
		{"git rm", "Bash", bash("git rm -q internal/a.go"), trEdit, []string{"internal/a.go"}},
		{"rm is as often clearing up", "Bash", bash("rm -f internal/zz_probe_test.go"), trOther, nil},
		{"git apply", "Bash", bash("git apply /tmp/s/fix.patch"), trEdit, nil},
		{"git apply --check", "Bash", bash("git apply --check /tmp/s/fix.patch"), trOther, nil},
		{"git -C elsewhere", "Bash", bash("git -C /tmp/s/clone apply /tmp/s/fix.patch"), trOther, nil},

		{"python heredoc", "Bash", bash("python3 - <<'EOF'\np='internal/a.go'\ns=open(p).read()\ns=s.replace('a','b')\nopen(p,'w').write(s)\nEOF"), trEdit, []string{"internal/a.go"}},
		{"python heredoc after cd", "Bash", bash("cd web && python3 - <<'EOF'\np='src/x.ts'; s=open(p).read()\nopen(p, 'w').write(s)\nEOF"), trEdit, []string{"web/src/x.ts"}},
		{"python reads the work, writes scratch", "Bash", bash("python3 - <<'EOF'\ns=open('internal/a.go').read()\nopen('/tmp/out.txt','w').write(s)\nEOF"), trOther, nil},
		{"python reads only", "Bash", bash("python3 - <<'EOF'\nimport json\nprint(len(open('internal/a.go').read()))\nEOF"), trOther, nil},
		{"python -c with pathlib", "Bash", bash(`python3 -c "from pathlib import Path; Path('docs/x.md').write_text('x')"`), trEdit, []string{"docs/x.md"}},
		{"python over a list", "Bash", bash("python3 - <<'EOF'\nfor p in ['a.go', 'b/c.go']:\n    s=open(p).read()\n    open(p,'w').write(s)\nEOF"), trEdit, []string{"a.go", "b/c.go"}},
		{"python helper function", "Bash", bash("uv run python - <<'EOF'\nimport pathlib\ndef sub(path, old, new):\n    p=pathlib.Path(path); t=p.read_text(); p.write_text(t.replace(old,new))\nW=\"web/b.ts\"\nsub('internal/a.go', 'a', 'b')\nsub(W, 'c', 'd')\nEOF"), trEdit, []string{"internal/a.go", "web/b.ts"}},
		{"python path the script computes", "Bash", bash("python3 - <<'EOF'\nimport sys\ns=open('deploy/config.yaml').read()\nopen(sys.argv[1]+'/gw.yaml','w').write(s)\nEOF"), trOther, nil},
		{"python pairs the parser can't follow", "Bash", bash("python3 - <<'EOF'\nfor p, a, b in [\n ('internal/a.go', 'x', 'y'),\n]:\n    s=open(p).read()\n    open(p,'w').write(s.replace(a,b))\nEOF"), trEdit, []string{"internal/a.go"}},
		{"python writes a scratch file by a shell variable", "Bash", bash("S=/tmp/s\npython3 - <<EOF\np=\"$S/plan.md\"; s=open(p).read()\nopen(p,'w').write(s)\nEOF"), trOther, nil},
		{"node -e", "Bash", bash(`node -e "require('fs').writeFileSync('web/b.ts', 'x')"`), trEdit, []string{"web/b.ts"}},
		{"a scratch script that edits, written and run", "Bash", bash("cat > /tmp/x/e.py <<'EOF'\nopen('internal/a.go','w').write('x')\nEOF\npython3 /tmp/x/e.py"), trEdit, []string{"internal/a.go"}},
		{"a scratch script by a variable the line doesn't set", "Bash", bash("cat > $S/e.py <<'EOF'\nopen('internal/a.go','w').write('x')\nEOF\ncd web && uv run python $S/e.py"), trEdit, []string{"web/internal/a.go"}},
		{"a scratch script only written", "Bash", bash("cat > /tmp/x/e.py <<'EOF'\nopen('internal/a.go','w').write('x')\nEOF"), trOther, nil},
		{"python -m", "Bash", bash("python3 -m pytest tests/test_a.py"), trOther, nil},

		{"sed with joined flags", "Bash", bash("sed -ri s/a/b/ internal/a.go"), trEdit, []string{"internal/a.go"}},
		{"a test's output kept", "Bash", bash("go test ./... > out.txt 2>&1"), trOther, nil},
		{"a search's output kept", "Bash", bash("grep -rn foo . > hits.txt"), trOther, nil},
		{"a fetched file of the work", "Bash", bash("curl -s https://example.com/schema.sql > internal/schema.sql"), trEdit, []string{"internal/schema.sql"}},
		{"a file out of history", "Bash", bash("git show HEAD~1:internal/a.go > internal/a.go"), trEdit, []string{"internal/a.go"}},
		{"a backup copy", "Bash", bash("cp internal/a.go internal/a.go.bak"), trOther, nil},
		{"a copy to a new file", "Bash", bash("cp internal/a.go internal/b.go"), trEdit, []string{"internal/b.go"}},
		{"copies into a directory", "Bash", bash("cp x/a.go x/b.go internal/"), trEdit, []string{"internal/a.go", "internal/b.go"}},
		{"a move to a name without a dot", "Bash", bash("mv x/a.go internal"), trEdit, []string{"internal/a.go", "x/a.go"}},
		{"a move to a file named without a dot", "Bash", bash("mv build/Makefile.new Makefile"), trEdit, []string{"Makefile", "build/Makefile.new"}},
		{"git apply behind a global flag", "Bash", bash("git --no-pager apply /tmp/x/fix.patch"), trEdit, nil},
		{"sed over what find names", "Bash", bash("find internal -name '*.go' | xargs sed -i s/a/b/"), trEdit, nil},
		{"sed by find", "Bash", bash("find internal -name '*.go' -exec sed -i s/a/b/ {} +"), trEdit, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := classifyTraceCall(tc.tool, tc.input, root, 1000, 1, "m", &traceScripts{})
			if c.Class != tc.class {
				t.Errorf("class = %q, want %q", c.Class, tc.class)
			}
			var got []string
			for _, p := range c.Paths {
				got = append(got, p.P)
			}
			if !reflect.DeepEqual(got, tc.paths) {
				t.Errorf("paths = %v, want %v", got, tc.paths)
			}
		})
	}
	for _, tc := range []struct{ cmd, start, end string }{
		{"wp next WP-1", "start", ""}, {"wp take WP-2", "start", ""}, {"cd x && wp ship -m y", "", "ship"},
		{"wp done", "", "done"}, {"wp release", "", "release"}, {"wp show WP-1", "", ""},
		{"wp next --peek", "", ""}, {"wp next -h", "", ""}, {"wp ship --help", "", ""}, {"wp take WP-3 --peek", "", ""},
	} {
		c := classifyTraceCall("Bash", bash(tc.cmd), root, 0, 0, "", nil)
		if (tc.start != "") != c.Start || c.End != tc.end {
			t.Errorf("%q: start=%v end=%q", tc.cmd, c.Start, c.End)
		}
	}
}

// A script the session keeps in a scratch file edits what it writes when it
// is run, in a later call too; what is remembered is the paths, not the text.
func TestTraceKeptScripts(t *testing.T) {
	root := "/work/app"
	call := func(mem *traceScripts, tool string, in map[string]any) *traceCall {
		b, _ := json.Marshal(in)
		return classifyTraceCall(tool, b, root, 1, 1, "m", mem)
	}
	mem := &traceScripts{}
	body := "from pathlib import Path\ndef sub(p, a, b):\n    f = Path(p); f.write_text(f.read_text().replace(a, b))\nsub('internal/a.go', 'x', 'y')\n"
	if c := call(mem, "Write", map[string]any{"file_path": "/tmp/s/edit1.py", "content": body}); c.Class != trOther {
		t.Fatalf("writing a scratch script is %q, want other", c.Class)
	}
	if len(*mem) != 1 || !reflect.DeepEqual((*mem)[0], traceScript{At: "/tmp/s/edit1.py", Writes: []string{"internal/a.go"}}) {
		t.Fatalf("remembered %+v", *mem)
	}
	if b, _ := json.Marshal(mem); strings.Contains(string(b), "write_text") {
		t.Fatalf("the script's text is kept: %s", b)
	}
	c := call(mem, "Bash", map[string]any{"command": "python3 /tmp/s/edit1.py && go build ./..."})
	if c.Class != trEdit || len(c.Paths) != 1 || c.Paths[0].P != "internal/a.go" {
		t.Fatalf("running it: %q %v, want an edit of internal/a.go", c.Class, c.Paths)
	}
	if c := call(mem, "Bash", map[string]any{"command": "python3 /tmp/s/other.py"}); c.Class != trOther {
		t.Fatalf("a script nobody wrote is %q", c.Class)
	}
	for cmd, want := range map[string]bool{
		"cat > internal/new.go <<'EOF'\nx\nEOF":                        true,
		"cp internal/a.go internal/b.go":                               true,
		"git mv internal/a.go internal/b.go":                           false,
		"echo x >> internal/a.go":                                      false,
		"sed -i s/a/b/ internal/a.go":                                  false,
		"python3 - <<'EOF'\nopen('internal/a.go','w').write('x')\nEOF": false,
	} {
		if c := call(nil, "Bash", map[string]any{"command": cmd}); c.Class != trEdit || c.Write != want {
			t.Errorf("%q: class %q, whole-file write %v, want an edit and %v", cmd, c.Class, c.Write, want)
		}
	}
	call(mem, "Bash", map[string]any{"command": "cat > /tmp/s/edit1.py <<'EOF'\nprint(open('internal/a.go').read())\nEOF"})
	if c := call(mem, "Bash", map[string]any{"command": "python3 /tmp/s/edit1.py"}); c.Class != trOther || len(*mem) != 0 {
		t.Fatalf("after it was rewritten to read only: %q, remembered %+v", c.Class, *mem)
	}
	for i := 0; i < traceMaxScripts+3; i++ {
		call(mem, "Write", map[string]any{"file_path": fmt.Sprintf("/tmp/s/e%d.py", i), "content": "open('a.go','w').write('x')\n"})
	}
	if len(*mem) != traceMaxScripts || (*mem)[0].At != "/tmp/s/e3.py" {
		t.Fatalf("kept %d, first %q", len(*mem), (*mem)[0].At)
	}
	st := &traceState{Scripts: *mem}
	cl := st.clone()
	cl.Scripts.Keep("/tmp/s/e3.py", "print(1)")
	if len(st.Scripts) != traceMaxScripts || st.Scripts[0].At != "/tmp/s/e3.py" {
		t.Fatalf("a clone's change reached the original: %d %q", len(st.Scripts), st.Scripts[0].At)
	}
}

// traceRepos lays out checkouts on disk: a main checkout with a Claude Code
// worktree inside it and a sibling worktree beside it, another repository,
// and a directory in no checkout. Scratch space is switched off, because
// t.TempDir is under /tmp.
func traceRepos(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mk := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	worktree := func(at, name string) {
		git := filepath.Join(dir, "main", ".git", "worktrees", name)
		mk(filepath.Join(git, "commondir"), "../..\n")
		mk(filepath.Join(at, ".git"), "gitdir: "+git+"\n")
	}
	mk(filepath.Join(dir, "main", ".git", "config"), "[remote \"origin\"]\n\turl = https://git.example/acme/app.git\n")
	worktree(filepath.Join(dir, "main", ".claude", "worktrees", "wt1"), "wt1")
	worktree(filepath.Join(dir, "main-side"), "side")
	mk(filepath.Join(dir, "other", ".git", "config"), "[remote \"origin\"]\n\turl = https://git.example/acme/other.git\n")
	if err := os.MkdirAll(filepath.Join(dir, "plain"), 0o750); err != nil {
		t.Fatal(err)
	}
	reset := func() {
		traceGit.Lock()
		traceGit.dirs = map[string]traceCheckout{}
		traceGit.Unlock()
	}
	old := traceScratch
	traceScratch = nil
	reset()
	t.Cleanup(func() { traceScratch = old; reset() })
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "home", ".claude"))
	return dir
}

// A file of the work is a file in a checkout, by whichever of a repository's
// checkouts the path names: autopilot sessions run in .claude/worktrees
// (WP-ADR57), others in the main checkout while working in a worktree beside
// it, and both name every file by its absolute path.
func TestTraceFile(t *testing.T) {
	dir := traceRepos(t)
	j := func(parts ...string) string { return filepath.Join(append([]string{dir}, parts...)...) }
	main, wt, side, other, plain := j("main"), j("main", ".claude", "worktrees", "wt1"), j("main-side"), j("other"), j("plain")
	gone := j("main", ".claude", "worktrees", "gone")
	const app, oth = "acme/app", "acme/other"
	for _, tc := range []struct {
		name, p, cwd string
		want         tracePath
		work         bool
	}{
		{"worktree session, its own file", wt + "/internal/x.go", wt, tracePath{"internal/x.go", app}, true},
		{"worktree session, a relative path", "internal/x.go", wt, tracePath{"internal/x.go", app}, true},
		{"worktree session, the main checkout's file", main + "/internal/x.go", wt, tracePath{"internal/x.go", app}, true},
		{"worktree session, a worktree beside the main checkout", side + "/internal/x.go", wt, tracePath{"internal/x.go", app}, true},
		{"worktree session, in a subdirectory", "../cmd/y.go", wt + "/internal", tracePath{"cmd/y.go", app}, true},
		{"main session, a worktree's file", wt + "/internal/x.go", main, tracePath{"internal/x.go", app}, true},
		{"main session, a worktree beside it", side + "/internal/x.go", main, tracePath{"internal/x.go", app}, true},
		{"main session, a settings file of the repository", main + "/.claude/settings.json", main, tracePath{".claude/settings.json", app}, true},
		{"main session, the worktree directory itself", wt, main, tracePath{}, true},
		{"a checkout's root itself (gofmt -w .)", main, main, tracePath{}, true},
		{"a Claude Code worktree's root itself", wt, wt, tracePath{}, true},
		{"a worktree root that is gone", gone, gone, tracePath{}, true},
		{"a directory in no checkout, itself", plain, plain, tracePath{}, false},
		{"another repository's file is that repository's", other + "/internal/x.go", main, tracePath{"internal/x.go", oth}, true},
		{"a session in no checkout", other + "/internal/x.go", plain, tracePath{"internal/x.go", oth}, true},
		{"a directory that is there, in no checkout", plain + "/notes.md", main, tracePath{}, false},
		{"the same, relative", "notes.md", plain, tracePath{}, false},
		{"above the checkout", "../../x.go", main + "/internal", tracePath{}, false},

		{"a worktree that is gone, its own file", gone + "/internal/x.go", gone, tracePath{"internal/x.go", app}, true},
		{"a worktree that is gone, from a subdirectory", "x.go", gone + "/internal", tracePath{"internal/x.go", app}, true},
		{"a worktree that is gone, read from the main checkout", gone + "/internal/x.go", main, tracePath{"internal/x.go", app}, true},
		{"a worktree beside the checkout that is gone", j("main-fix") + "/internal/x.go", main, tracePath{"internal/x.go", app}, true},
		{"the same, from a worktree", j("main-fix") + "/internal/x.go", wt, tracePath{"internal/x.go", app}, true},
		{"the same, from the worktree beside it", j("main-fix") + "/internal/x.go", side, tracePath{"internal/x.go", app}, true},
		{"a session in a worktree beside the checkout that is gone", "internal/x.go", j("main-fix"), tracePath{"internal/x.go", app}, true},
		{"the same, another such worktree's file", j("main-two") + "/web/x.ts", j("main-fix"), tracePath{"web/x.ts", app}, true},
		{"a directory that is gone and unplaced is work with no name", j("elsewhere", "wt") + "/internal/x.go", main, tracePath{}, true},
		{"a directory beside the checkout, not named after it", j("mainly") + "/internal/x.go", main, tracePath{}, true},

		{"nothing on disk: the working directory was the root", "/nowhere/app/internal/x.go", "/nowhere/app", tracePath{"internal/x.go", ""}, true},
		{"nothing on disk: a Claude Code worktree's main checkout", "/nowhere/app/internal/x.go", "/nowhere/app/.claude/worktrees/a", tracePath{"internal/x.go", ""}, true},
		{"nothing on disk: the main checkout's worktree", "/nowhere/app/.claude/worktrees/a/web/x.ts", "/nowhere/app", tracePath{"web/x.ts", ""}, true},
		{"nothing on disk: a worktree beside it", "/nowhere/app-fix/web/x.ts", "/nowhere/app/.claude/worktrees/a", tracePath{"web/x.ts", ""}, true},
		{"nothing on disk: a worktree kept in .worktrees", "/nowhere/app/.worktrees/a/web/x.ts", "/nowhere/app", tracePath{"web/x.ts", ""}, true},
		{"nothing on disk: a session in one", "web/x.ts", "/nowhere/app/.worktrees/a", tracePath{"web/x.ts", ""}, true},
		{"a worktree in .worktrees that is gone", j("main", ".worktrees", "gone") + "/web/x.ts", main, tracePath{"web/x.ts", app}, true},

		{"Claude Code's own directory", j("home", ".claude", "plans", "p.md"), main, tracePath{}, false},
		{"home directory shorthand", "~/x.go", main, tracePath{}, false},
		{"no working directory, relative", "internal/x.go", "", tracePath{"internal/x.go", ""}, true},
		{"no path", "", main, tracePath{}, false},
	} {
		if got, work := traceFile(tc.p, tc.cwd); got != tc.want || work != tc.work {
			t.Errorf("%s: traceFile(%q, %q) = %+v, %v; want %+v, %v", tc.name, strings.TrimPrefix(tc.p, dir), strings.TrimPrefix(tc.cwd, dir), got, work, tc.want, tc.work)
		}
	}
	traceScratch = []string{dir + "/other/"}
	if got, work := traceFile(other+"/internal/x.go", main); got.P != "" || work {
		t.Errorf("a checkout in scratch space = %+v, %v; want nothing", got, work)
	}
	traceScratch = nil

	edit := func(p string) json.RawMessage { b, _ := json.Marshal(map[string]any{"file_path": p}); return b }
	for _, tc := range []struct {
		name, tool, p, cwd, class string
		paths                     []tracePath
	}{
		{"a worktree session", "Edit", wt + "/internal/x.go", wt, trEdit, []tracePath{{"internal/x.go", app}}},
		{"a new file in a worktree session", "Write", wt + "/internal/new.go", wt, trEdit, []tracePath{{"internal/new.go", app}}},
		{"another repository's worktree", "Edit", other + "/internal/x.go", main, trEdit, []tracePath{{"internal/x.go", oth}}},
		{"a worktree that is gone and unplaced", "Edit", j("elsewhere", "wt") + "/internal/x.go", main, trEdit, nil},
		{"a note outside any checkout", "Write", plain + "/notes.md", main, trOther, nil},
		{"Claude Code's memory", "Write", j("home", ".claude", "projects", "m", "memory", "x.md"), main, trOther, nil},
	} {
		c := classifyTraceCall(tc.tool, edit(tc.p), tc.cwd, 1, 1, "m", nil)
		if c.Class != tc.class || !reflect.DeepEqual(c.Paths, tc.paths) || c.Write != (tc.tool == "Write" && tc.class == trEdit) {
			t.Errorf("%s: class %q paths %+v write %v", tc.name, c.Class, c.Paths, c.Write)
		}
	}
	bash := func(c string) json.RawMessage { b, _ := json.Marshal(map[string]any{"command": c}); return b }
	for _, tc := range []struct {
		name, cmd, class string
		paths            []tracePath
	}{
		{"sed -i in a worktree beside the checkout", "sed -i s/a/b/ " + side + "/internal/x.go", trEdit, []tracePath{{"internal/x.go", app}}},
		{"sed -i in a worktree that is gone and unplaced", "sed -i s/a/b/ " + j("elsewhere", "wt") + "/internal/x.go", trEdit, nil},
		{"a redirect to a note outside any checkout", "cat > " + plain + "/notes.md <<'EOF'\nx\nEOF", trOther, nil},
		{"reading another repository", "cat " + other + "/internal/x.go", trRead, []tracePath{{"internal/x.go", oth}}},
		{"gofmt -w . at the checkout root", "gofmt -w .", trEdit, nil},
	} {
		if c := classifyTraceCall("Bash", bash(tc.cmd), main, 1, 1, "m", nil); c.Class != tc.class || !reflect.DeepEqual(c.Paths, tc.paths) {
			t.Errorf("%s: class %q paths %+v", tc.name, c.Class, c.Paths)
		}
	}
}
