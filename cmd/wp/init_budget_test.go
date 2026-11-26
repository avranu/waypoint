package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"testing"
)

// initBudget is the most heap the whole binary's package init may allocate
// (WP-1398). Every `wp hook` call pays process start under a 2 s deadline:
// init cost 35 MB and ~270 ms of core's package alone when hundreds of
// regexps and an argon2 hash were built at start. Now ~1.3 MB, nearly all of
// it registration tables and grpc/prometheus. A package-level
// regexp.MustCompile belongs behind lazyre.New, a hash or parse behind
// sync.OnceValue; raise this only for a reason that survives that question.
const initBudget = 2 << 20

var initLine = regexp.MustCompile(`(?m)^init (\S+) @[\d.]+ ms, [\d.]+ ms clock, (\d+) bytes, \d+ allocs$`)

// The wp binary's package init allocates under initBudget. Bytes, not time:
// they do not depend on how loaded the machine is. It builds the real binary
// (the test binary's own init includes the testing package and every test
// file), so it is skipped under -short.
func TestInitStaysCheap(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the wp binary")
	}
	bin := filepath.Join(t.TempDir(), "wp")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "version")
	cmd.Env = append(os.Environ(), "GODEBUG=inittrace=1", "HOME="+t.TempDir())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run()
	type pkg struct {
		name  string
		bytes int
	}
	var pkgs []pkg
	total := 0
	for _, m := range initLine.FindAllStringSubmatch(stderr.String(), -1) {
		n, _ := strconv.Atoi(m[2])
		pkgs = append(pkgs, pkg{m[1], n})
		total += n
	}
	if len(pkgs) == 0 {
		t.Fatalf("no init trace in the output:\n%s", stderr.String())
	}
	if total <= initBudget {
		return
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].bytes > pkgs[j].bytes })
	msg := fmt.Sprintf("package init allocates %.2f MB, over the %.2f MB budget; top packages:", float64(total)/(1<<20), float64(initBudget)/(1<<20))
	for _, p := range pkgs[:min(5, len(pkgs))] {
		msg += fmt.Sprintf("\n  %8d B  %s", p.bytes, p.name)
	}
	t.Error(msg + "\nMake the heavy package-level value lazy (lazyre.New, sync.OnceValue); see WP-1398.")
}
