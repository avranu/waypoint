package depage

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A lockfile's resolved URL, a package name and a path are the pushed tree's
// text: none may start a line of its own, where CI's runner would read a
// workflow command (audit #924.1).
func TestReportEscapes(t *testing.T) {
	evil := "https://x\n::add-path::/tmp/evil\r\n%0A"
	rs := []Result{{
		Dep:     Dep{Eco: NPM, Name: "p\n::warning::n", Version: "unverifiable:" + evil, Where: "a,b:c%\n::stop-commands::x/package-lock.json"},
		Verdict: Unknown,
		Detail:  "it comes from " + evil + ", not a registry",
	}}
	var out bytes.Buffer
	if Report(&out, rs, 7*24*time.Hour, false, true) {
		t.Fatal("passed")
	}
	var errLine string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "::") && !strings.HasPrefix(l, "::error ") {
			t.Errorf("a line the runner reads as a command: %q\n%s", l, out.String())
		}
		if strings.HasPrefix(l, "::error ") {
			errLine = l
		}
		if strings.ContainsRune(l, '\r') {
			t.Errorf("a carriage return in %q", l)
		}
	}
	if !strings.HasPrefix(errLine, "::error file=a%2Cb%3Ac%25") || !strings.Contains(errLine, "%250A") {
		t.Errorf("::error line not escaped: %q", errLine)
	}
}

// A version whose age couldn't be read because a source failed gets no
// waiver line: running again is the remedy, and a waiver would land it
// unchecked for good (audit #924.5).
func TestReportLookupFailure(t *testing.T) {
	rs := []Result{
		{Dep: Dep{Eco: NPM, Name: "gone", Version: "1.0.0"}, Verdict: Unknown, Lookup: true, Detail: "reading its publish time: 503"},
		{Dep: Dep{Eco: NPM, Name: "fromgit", Version: "unverifiable:git+ssh://x"}, Verdict: Unknown, Detail: "it comes from git+ssh://x"},
	}
	var out bytes.Buffer
	if Report(&out, rs, 7*24*time.Hour, false, false) {
		t.Fatal("passed")
	}
	s := out.String()
	if strings.Contains(s, "npm gone 1.0.0 REASON") {
		t.Errorf("suggests waiving a version whose age wasn't read:\n%s", s)
	}
	if !strings.Contains(s, "npm fromgit unverifiable:git+ssh://x REASON") || !strings.Contains(s, "run it again") {
		t.Errorf("report lacks the waiver line or the rerun advice:\n%s", s)
	}
}

func TestCheckLookup(t *testing.T) {
	got := Check(context.Background(), fakeSources{}, []Dep{
		{Eco: NPM, Name: "gone", Version: "1.0.0"},
		{Eco: NPM, Name: "fromgit", Version: "unverifiable:git+ssh://x"},
	}, nil, time.Now(), 7*24*time.Hour)
	if !got[0].Lookup || got[1].Lookup {
		t.Errorf("Lookup = %v, %v; want true for a failed read, false for an unverifiable source", got[0].Lookup, got[1].Lookup)
	}
}

// An error answer's first line is read without copying the whole body
// (audit #924.6).
func TestFirstLineCopiesLittle(t *testing.T) {
	b := bytes.Repeat([]byte("x"), 64<<20)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	s := firstLine(b)
	runtime.ReadMemStats(&after)
	if len(s) > 210 {
		t.Errorf("firstLine is %d bytes", len(s))
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 1<<20 {
		t.Errorf("firstLine allocated %d bytes for a 64 MiB answer", n)
	}
}
