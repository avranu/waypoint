package transcript

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// allocated is the bytes the Go heap handed out while f ran.
func allocated(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

func TestShellVariableDoublingIsBounded(t *testing.T) {
	for _, n := range []int{26, 60} {
		cmd := "a=x;" + strings.Repeat("a=$a$a;", n) + "echo $a > /r/out.go"
		start := time.Now()
		bytes := allocated(func() { (&ShellSession{}).Edit(cmd, "/r") })
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%d doublings took %v", n, d)
		}
		if bytes > 16<<20 {
			t.Errorf("%d doublings allocated %d MB", n, bytes>>20)
		}
	}
}

func TestShellFillAndStoredValuesAreCapped(t *testing.T) {
	vars := map[string]string{"a": strings.Repeat("x", shellMaxValue)}
	cmds, _ := ReadShell("cat > "+strings.Repeat("$a", 1000)+"/f.go", "/r", vars, nil, false)
	wrote := 0
	for _, c := range cmds {
		for _, w := range c.Wrote {
			wrote++
			if len(w.Path) > 4*shellMaxValue {
				t.Errorf("a filled word is %d bytes", len(w.Path))
			}
		}
	}
	if wrote != 1 {
		t.Errorf("%d writes recorded, want 1", wrote)
	}
	v := map[string]string{}
	ReadShell("a=x; "+strings.Repeat("a=$a$a$a$a; ", 8), "/r", v, nil, false)
	if len(v["a"]) > shellMaxValue {
		t.Errorf("a stored value is %d bytes", len(v["a"]))
	}
	v = map[string]string{}
	ReadShell("a="+strings.Repeat("y", shellMaxValue+1)+"; b=1", "/r", v, nil, false)
	if _, ok := v["a"]; ok || v["b"] != "1" {
		t.Errorf("a literal over the cap is stored: %d bytes, b=%q", len(v["a"]), v["b"])
	}
	v = map[string]string{}
	var b strings.Builder
	for i := 0; i < 3*shellMaxVars; i++ {
		b.WriteString("v" + strings.Repeat("a", i%50) + string(rune('a'+i%26)) + strings.Repeat("b", i/50) + "=1; ")
	}
	ReadShell(b.String(), "/r", v, nil, false)
	if len(v) > shellMaxVars {
		t.Errorf("%d variables kept", len(v))
	}
}

func TestShellLongCommandIsUnknownAndFast(t *testing.T) {
	long := "echo " + strings.Repeat("x ", shellMaxCommand/2) + "> /r/a.go"
	start := time.Now()
	if _, known := (&ShellSession{}).Edit(long, "/r"); known {
		t.Errorf("a command over the cap should be unknown")
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("giving up took %v", d)
	}
	for name, cmd := range map[string]string{
		"many commands": strings.Repeat("a=1;", shellMaxCommand/4),
		"nested shells": strings.Repeat("bash -c 'bash -c \"bash -c x\"';", shellMaxCommand/32),
		"heredocs":      strings.Repeat("cat > /tmp/x.py <<E\nopen('a.go','w')\nE\n", shellMaxCommand/40),
		"python script": "python3 - <<E\n" + strings.Repeat("p=q\nq=p\nopen(p,'w')\nopen(q,'w')\n", shellMaxCommand/40) + "E\n",
		"many strings":  "python3 -c \"" + strings.Repeat("open('a.go');write('b.go');", shellMaxCommand/30) + "\"",
		"subs":          strings.Repeat("echo $(echo $(echo `x`));", shellMaxCommand/26),
	} {
		cmd = cmd[:min(len(cmd), shellMaxCommand)]
		start := time.Now()
		wrote, known := (&ShellSession{}).Edit(cmd, "/r")
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s: %v", name, d)
		}
		// None writes a file it names: the kept script is never run, and the
		// python names no path that can be followed, so what it writes is
		// unknown. The inline script of "many strings" is longer than the part
		// that is read and that part writes nothing, so that is unknown too.
		if wantKnown := name != "many strings" && name != "python script"; wrote || known != wantKnown {
			t.Errorf("%s: wrote %v known %v, want false %v", name, wrote, known, wantKnown)
		}
	}
}

// A long word reached through $_ or a for-loop variable is not copied again
// for every word that names it (audit 1923.2).
func TestShellEditLongWordVariables(t *testing.T) {
	big := strings.Repeat("x", 100<<10)
	for name, cmd := range map[string]string{
		"loop variable": "for f in " + big + "; do echo " + strings.Repeat("$f ", 20000) + "; done",
		"last word":     "echo " + big + "; echo " + strings.Repeat("$_ ", 20000),
	} {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		wrote, known := (&ShellSession{}).Edit(cmd, "/r")
		runtime.ReadMemStats(&after)
		if d, mb := time.Since(start), (after.TotalAlloc-before.TotalAlloc)>>20; d > 2*time.Second || mb > 200 {
			t.Errorf("%s: %v and %d MB allocated", name, d, mb)
		}
		if wrote || !known {
			t.Errorf("%s: wrote %v known %v; an echo writes nothing", name, wrote, known)
		}
	}
}

// Literal words are bounded too: a destination or directory far longer than
// any path is not copied once per source or per cd (audit 1933.1), and a
// heredoc is not read again for every file it is sent to.
func TestShellEditLongLiteralWords(t *testing.T) {
	big := strings.Repeat("b", 100<<10)
	for name, c := range map[string]struct {
		cmd   string
		wrote bool
	}{
		"cp many sources":   {"cp " + strings.Repeat("a ", 3000) + big + "/", true},
		"cp into a dir":     {"cp " + strings.Repeat("a ", 3000) + "internal/", true},
		"cd long then many": {"cd /" + big + ";" + strings.Repeat("cd b;", 2000) + "echo x > a.go", false},
		"cd deeper":         {strings.Repeat("cd "+strings.Repeat("d", 3000)+";", 40) + "echo x > a.go", false},
		"tee many scripts":  {"tee " + strings.Repeat("/tmp/s/a.py ", 8000) + "<<E\n" + strings.Repeat("open('/tmp/x','w')\n", 3000) + "E\n", false},
	} {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		wrote, known := (&ShellSession{}).Edit(c.cmd, "/r")
		runtime.ReadMemStats(&after)
		if d, mb := time.Since(start), (after.TotalAlloc-before.TotalAlloc)>>20; d > 2*time.Second || mb > 50 {
			t.Errorf("%s: %v and %d MB allocated", name, d, mb)
		}
		if wrote != c.wrote || !known {
			t.Errorf("%s: wrote %v known %v, want %v true", name, wrote, known, c.wrote)
		}
	}
}

// A variable under the per-word cap, named by tens of thousands of words, is
// not filled in for all of them: the line's expansions have a total bound
// (audit 1931.10).
func TestShellEditManyExpansionsBounded(t *testing.T) {
	cmd := "a=" + strings.Repeat("x", 4000) + "; echo " + strings.Repeat("$a ", 80000)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	wrote, known := (&ShellSession{}).Edit(cmd, "/r")
	runtime.ReadMemStats(&after)
	if mb := (after.TotalAlloc - before.TotalAlloc) >> 20; mb > 50 {
		t.Errorf("%d MB allocated", mb)
	}
	if wrote || !known {
		t.Errorf("wrote %v known %v; an echo writes nothing", wrote, known)
	}
}

// A script longer than the part that is read, whose write lies past the cut,
// is unknown, not "wrote nothing" (audit 1931.7). One whose write is in the
// part read is still counted.
func TestShellEditScriptPastTheCutIsUnknown(t *testing.T) {
	pad := strings.Repeat("# padding\n", traceMaxScript/10+10)
	write := "open('a.go','w').write('x')\n"
	if wrote, known := (&ShellSession{}).Edit("python3 - <<'E'\n"+pad+write+"E\n", "/r"); wrote || known {
		t.Errorf("write past the cut: wrote %v known %v, want false false", wrote, known)
	}
	if wrote, known := (&ShellSession{}).Edit("python3 - <<'E'\n"+write+pad+"E\n", "/r"); !wrote || !known {
		t.Errorf("write before the cut: wrote %v known %v, want true true", wrote, known)
	}
	if wrote, known := (&ShellSession{}).Edit("python3 - <<'E'\nprint(1)\nE\n", "/r"); wrote || !known {
		t.Errorf("a short script that writes nothing: wrote %v known %v, want false true", wrote, known)
	}
}
