package testevidence

import (
	"bufio"
	"encoding/json"
	"io"
	"slices"
	"strings"
)

// event is one line of `go test -json` (cmd/test2json).
type event struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
	Output  string  `json:"Output"`
}

// GoTest reads `go test -json` output, returns its top-level tests' results
// and writes to text what plain `go test` would have printed: the output of
// failed tests, build errors and each package's ok/FAIL line. `go test
// -json` runs the tests verbosely, and a log of every passing test's
// framing would bury the failures that the log is read for (wp pr log
// finds "--- FAIL" blocks). module is stripped from package paths. A line
// that isn't JSON (a toolchain message) is passed through.
func GoTest(r io.Reader, text io.Writer, module string) ([]Test, error) {
	var tests []Test
	held := map[string][]string{}
	key := func(pkg, test string) string {
		top, _, _ := strings.Cut(test, "/")
		return pkg + "\x00" + top
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	var err error
	write := func(s string) {
		if err == nil {
			_, err = io.WriteString(text, s)
		}
	}
	for sc.Scan() {
		line := sc.Bytes()
		var e event
		if len(line) == 0 || line[0] != '{' || json.Unmarshal(line, &e) != nil {
			write(string(line) + "\n")
			continue
		}
		switch {
		case e.Action == "build-output":
			write(e.Output)
		case e.Test == "":
			switch e.Action {
			case "output":
				if !framing(e.Output) && e.Output != "PASS\n" && !strings.HasPrefix(e.Output, "coverage: ") {
					write(e.Output)
				}
			case Pass, Fail, Skip:
				for _, k := range sortedKeys(held) {
					if strings.HasPrefix(k, e.Package+"\x00") {
						if e.Action == Fail {
							write(strings.Join(held[k], ""))
						}
						delete(held, k)
					}
				}
			}
		case e.Action == "output":
			if !framing(e.Output) {
				k := key(e.Package, e.Test)
				held[k] = append(held[k], e.Output)
			}
		case e.Action == Pass || e.Action == Fail || e.Action == Skip:
			if strings.Contains(e.Test, "/") {
				continue
			}
			k := key(e.Package, e.Test)
			if e.Action == Fail {
				write(strings.Join(held[k], ""))
			}
			delete(held, k)
			tests = append(tests, Test{Package: trimModule(e.Package, module), Name: e.Test, Outcome: e.Action, Ms: int64(e.Elapsed*1000 + 0.5)})
		}
	}
	for _, k := range sortedKeys(held) {
		write(strings.Join(held[k], ""))
	}
	if serr := sc.Err(); serr != nil {
		return tests, serr
	}
	return tests, err
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// framing is a line verbose test output adds around every test.
func framing(s string) bool {
	t := strings.TrimLeft(s, " ")
	for _, p := range []string{"=== RUN", "=== PAUSE", "=== CONT", "=== NAME", "--- PASS:", "--- SKIP:"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

func trimModule(path, module string) string {
	if module == "" {
		return path
	}
	if path == module {
		return "."
	}
	return strings.TrimPrefix(path, module+"/")
}
