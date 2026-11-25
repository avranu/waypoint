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
	for _, c := range cmds {
		for _, w := range c.Wrote {
			if len(w.Path) > 4*shellMaxValue {
				t.Errorf("a filled word is %d bytes", len(w.Path))
			}
		}
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
		(&ShellSession{}).Edit(cmd, "/r")
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s: %v", name, d)
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
		(&ShellSession{}).Edit(cmd, "/r")
		runtime.ReadMemStats(&after)
		if d, mb := time.Since(start), (after.TotalAlloc-before.TotalAlloc)>>20; d > 2*time.Second || mb > 200 {
			t.Errorf("%s: %v and %d MB allocated", name, d, mb)
		}
	}
}
