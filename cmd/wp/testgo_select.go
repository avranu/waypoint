package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Go test selection for a diff (WP-ADR161 point 3, WP-1875). A changed
// package runs whole; a package the repository lists as heavy runs the tests
// in the changed test files and in the test files beside the changed source
// files. It is a heuristic: a change in foo.go can break a test in
// bar_test.go, and widening it to importers would bring back the whole run
// (most changes touch a package nearly everything imports).

const (
	// heavyTestsFile lists the packages that run by file, one directory per
	// line, relative to the repository root.
	heavyTestsFile = ".waypoint/heavy-tests"
	// maxRunRegexLen keeps one -run argument far below the kernel's limit on
	// a single argument (128 KiB on Linux).
	maxRunRegexLen = 100000
)

// goSelection is what to run for a diff.
type goSelection struct {
	// Packages are all the changed packages, as go takes them (./internal/x).
	Packages []string
	// Whole run as they are; Heavy run only the named tests.
	Whole []string
	Heavy []heavyRun
	// NoTests are changed files, in heavy packages, with no test selected
	// for them.
	NoTests []string
}

type heavyRun struct {
	Pkg   string // ./internal/core
	Tests []string
}

// selectGoTests works out what to run for the changed paths (relative to
// root, slash-separated, deleted ones included) in the Go module at root.
// heavy are the package directories that run by file. It reads only the
// directories and test files it must.
func selectGoTests(root string, changed, heavy []string) goSelection {
	heavySet := map[string]bool{}
	for _, h := range heavy {
		heavySet[cleanPkgDir(h)] = true
	}
	byDir := map[string][]string{}
	for _, p := range changed {
		p = path.Clean(filepath.ToSlash(p))
		if !strings.HasSuffix(p, ".go") || !inSelectableTree(p) {
			continue
		}
		dir := path.Dir(p)
		if !isModulePackage(root, dir) {
			continue
		}
		if !slices.Contains(byDir[dir], path.Base(p)) {
			byDir[dir] = append(byDir[dir], path.Base(p))
		}
	}
	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	slices.Sort(dirs)
	var sel goSelection
	for _, dir := range dirs {
		pkg := pkgArg(dir)
		sel.Packages = append(sel.Packages, pkg)
		if !heavySet[dir] {
			sel.Whole = append(sel.Whole, pkg)
			continue
		}
		tests, none := heavyTests(filepath.Join(root, filepath.FromSlash(dir)), byDir[dir])
		if len(tests) > 0 {
			sel.Heavy = append(sel.Heavy, heavyRun{Pkg: pkg, Tests: tests})
		}
		for _, f := range none {
			sel.NoTests = append(sel.NoTests, path.Join(dir, f))
		}
	}
	return sel
}

// cleanPkgDir is a package directory as .waypoint/heavy-tests or --by-file
// writes it ("./internal/core/", "internal/core") in the form the changed
// paths have.
func cleanPkgDir(s string) string {
	s = path.Clean(filepath.ToSlash(strings.TrimSpace(s)))
	return s
}

// pkgArg is dir as an argument to go: ./dir, or . for the root.
func pkgArg(dir string) string {
	if dir == "." {
		return "."
	}
	return "./" + dir
}

// inSelectableTree: the path is inside the module and not in a folder go
// ignores (testdata, vendor, names starting with . or _).
func inSelectableTree(p string) bool {
	if p == "" || path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") {
		return false
	}
	for _, e := range strings.Split(path.Dir(p), "/") {
		if e == "." {
			continue
		}
		if e == "testdata" || e == "vendor" || strings.HasPrefix(e, ".") || strings.HasPrefix(e, "_") {
			return false
		}
	}
	return true
}

// isModulePackage: dir still exists holding Go files, and no go.mod between
// it and root makes it another module.
func isModulePackage(root, dir string) bool {
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
	if err != nil || !slices.ContainsFunc(entries, func(e os.DirEntry) bool { return !e.IsDir() && strings.HasSuffix(e.Name(), ".go") }) {
		return false
	}
	for d := dir; d != "."; d = path.Dir(d) {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(d), "go.mod")); err == nil {
			return false
		}
	}
	return true
}

// heavyTests are the top-level tests to run in the package folder dir for
// its changed files: the tests of each changed _test.go, and of the test
// files beside each changed source file (foo_test.go and foo_*_test.go; for
// foo_linux.go, foo's too). none are the changed files with no test selected.
func heavyTests(dir string, files []string) (tests, none []string) {
	entries, _ := os.ReadDir(dir)
	var testFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), "_test.go") {
			testFiles = append(testFiles, e.Name())
		}
	}
	parsed := map[string][]string{}
	namesIn := func(f string) []string {
		if n, ok := parsed[f]; ok {
			return n
		}
		n := topLevelTests(filepath.Join(dir, f))
		parsed[f] = n
		return n
	}
	for _, f := range files {
		var found []string
		if strings.HasSuffix(f, "_test.go") {
			if slices.Contains(testFiles, f) {
				found = namesIn(f)
			}
		} else {
			for _, tf := range testFiles {
				if besideTestFile(strings.TrimSuffix(f, ".go"), tf) {
					found = append(found, namesIn(tf)...)
				}
			}
		}
		if len(found) == 0 {
			none = append(none, f)
		}
		tests = append(tests, found...)
	}
	slices.Sort(tests)
	return slices.Compact(tests), none
}

// besideTestFile: test file tf belongs to the source file base (no .go):
// base_test.go or base_*_test.go, for base and, with a GOOS or GOARCH suffix
// cut off, for what is left.
func besideTestFile(base, tf string) bool {
	for _, stem := range fileStems(base) {
		if tf == stem+"_test.go" || (strings.HasPrefix(tf, stem+"_") && strings.HasSuffix(tf, "_test.go")) {
			return true
		}
	}
	return false
}

// fileStems is base and what is left of it with a platform suffix cut:
// foo_linux_amd64 gives foo_linux_amd64, foo_linux, foo.
func fileStems(base string) []string {
	stems := []string{base}
	for cur := base; ; {
		i := strings.LastIndex(cur, "_")
		if i <= 0 || (!knownGOOS[cur[i+1:]] && !knownGOARCH[cur[i+1:]]) {
			return stems
		}
		cur = cur[:i]
		stems = append(stems, cur)
	}
}

func setOf(words string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(words) {
		m[w] = true
	}
	return m
}

// The platforms go's file-name suffixes name (go tool dist list).
var (
	knownGOOS   = setOf("aix android darwin dragonfly freebsd hurd illumos ios js linux nacl netbsd openbsd plan9 solaris wasip1 windows zos")
	knownGOARCH = setOf("386 amd64 amd64p32 arm arm64 arm64be armbe loong64 mips mips64 mips64le mips64p32 mips64p32le mipsle ppc ppc64 ppc64le riscv riscv64 s390 s390x sparc sparc64 wasm")
)

// topLevelTests are the Test, Fuzz and Example functions a file declares, as
// go test names them. A file that doesn't parse has none.
func topLevelTests(file string) []string {
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var names []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Type.TypeParams != nil {
			continue
		}
		name, params := fn.Name.Name, fn.Type.Params.NumFields()
		if (isTestName(name, "Test") && name != "TestMain" && params == 1) ||
			(isTestName(name, "Fuzz") && params == 1) ||
			(isTestName(name, "Example") && params == 0) {
			names = append(names, name)
		}
	}
	return names
}

// isTestName is go test's rule: the prefix, then nothing or not a lower-case
// letter.
func isTestName(name, prefix string) bool {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return false
	}
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return !unicode.IsLower(r)
}

// commands are the go test commands for the selection, with flags after
// -count=1: at most one for the whole packages and one for the heavy ones,
// which are split by package when one -run argument would be too long.
func (s goSelection) commands(flags []string) [][]string {
	base := append([]string{"go", "test", "-count=1"}, flags...)
	var out [][]string
	if len(s.Whole) > 0 {
		out = append(out, slices.Concat(base, s.Whole))
	}
	if len(s.Heavy) == 0 {
		return out
	}
	var names, pkgs []string
	for _, h := range s.Heavy {
		names = append(names, h.Tests...)
		pkgs = append(pkgs, h.Pkg)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	if runRegexLen(names) <= maxRunRegexLen {
		return append(out, slices.Concat(base, []string{"-run", runRegex(names)}, pkgs))
	}
	for _, h := range s.Heavy {
		for _, chunk := range chunkNames(h.Tests) {
			out = append(out, slices.Concat(base, []string{"-run", runRegex(chunk)}, []string{h.Pkg}))
		}
	}
	return out
}

func runRegex(names []string) string { return "^(" + strings.Join(names, "|") + ")$" }

func runRegexLen(names []string) int {
	n := 4
	for _, s := range names {
		n += len(s) + 1
	}
	return n
}

// chunkNames splits names so each chunk's regex fits maxRunRegexLen.
func chunkNames(names []string) [][]string {
	var out [][]string
	for len(names) > 0 {
		n, size := 0, 4
		for n < len(names) && (n == 0 || size+len(names[n])+1 <= maxRunRegexLen) {
			size += len(names[n]) + 1
			n++
		}
		out = append(out, names[:n])
		names = names[n:]
	}
	return out
}
