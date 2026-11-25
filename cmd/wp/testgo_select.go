package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
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
	// Compile are the heavy packages with changed files and no test
	// selected at all: they are compiled with their tests and none runs
	// (go test -run '^$'), so a file that does not build still fails.
	Compile []string
	// Files are the changed paths that belong to a package, HeavyFiles those
	// in heavy packages.
	Files, HeavyFiles int
}

// testsRun is whether any command of the selection runs a test.
func (s goSelection) testsRun() bool { return len(s.Whole) > 0 || len(s.Heavy) > 0 }

// compileOnly is whether the selection only compiles: packages changed and no
// test runs.
func (s goSelection) compileOnly() bool { return !s.testsRun() && len(s.Compile) > 0 }

// noTestsLine says which changed files have no test beside them and what
// happens to their package, "" when every changed file has one.
func (s goSelection) noTestsLine() string {
	if len(s.NoTests) == 0 {
		return ""
	}
	line := "No test sits beside: " + strings.Join(s.NoTests, ", ") + " (heavy package: only tests in changed test files and beside changed files run)."
	if len(s.Compile) > 0 {
		line += " No test is selected in " + strings.Join(s.Compile, ", ") + ", so the package is compiled with its tests and no test runs."
	}
	return line
}

type heavyRun struct {
	Pkg   string // ./internal/core
	Tests []string
}

// selectGoTests works out what to run for the changed paths (relative to
// root, slash-separated, deleted ones included) in the Go module at root.
// heavy are the package directories that run by file. It reads only the
// directories and test files it must. Besides .go files, two kinds of file
// go compiles into a package select it: a file under testdata/ (the nearest
// package above it) and a file a package embeds (go list); nothing else
// does.
func selectGoTests(root string, changed, heavy []string) goSelection {
	return selectGoTestsIn(root, changed, heavy, nil, listEmbeds, os.Stderr)
}

// embedIndex is, for each embedded file (path relative to the module root),
// the package directories that embed it; dirs are the directories embedded
// files sit in (for a deleted file, which go list no longer shows).
type embedIndex struct {
	owners map[string][]string
	dirs   map[string][]string
}

// selectGoTestsIn is selectGoTests for the packages match accepts (nil: all),
// with embeds the way to learn what packages embed (called at most once, and
// only when a path could be an embedded file) and warn where its failure is
// said.
func selectGoTestsIn(root string, changed, heavy []string, match func(dir string) bool, embeds func(root string) (embedIndex, error), warn io.Writer) goSelection {
	heavySet := map[string]bool{}
	for _, h := range heavy {
		heavySet[cleanPkgDir(h)] = true
	}
	byDir := map[string][]string{}
	data := map[string][]string{} // package dir -> changed non-Go paths it owns
	add := func(dir string) {
		if _, ok := byDir[dir]; !ok {
			byDir[dir] = nil
		}
	}
	var maybeEmbedded []string
	for _, p := range changed {
		p = path.Clean(filepath.ToSlash(p))
		if p == "" || path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") {
			continue
		}
		switch {
		case inTestdata(p) && !strings.HasSuffix(p, ".go"):
			if dir, ok := testdataOwner(root, p); ok && (match == nil || match(dir)) {
				add(dir)
				data[dir] = append(data[dir], p)
			}
		case !strings.HasSuffix(p, ".go"):
			if !inVendorOrHidden(p) {
				maybeEmbedded = append(maybeEmbedded, p)
			}
		case inSelectableTree(p):
			dir := path.Dir(p)
			if isModulePackage(root, dir) && (match == nil || match(dir)) {
				add(dir)
				if !slices.Contains(byDir[dir], path.Base(p)) {
					byDir[dir] = append(byDir[dir], path.Base(p))
				}
			}
		}
	}
	if len(maybeEmbedded) > 0 && embeds != nil {
		idx, err := embeds(root)
		if err != nil {
			_, _ = fmt.Fprintf(warn, "wp: can't list the files packages embed (%v), so changed files that are not Go select no package\n", err)
		}
		for _, p := range maybeEmbedded {
			for _, dir := range idx.ownersOf(root, p) {
				if match == nil || match(dir) {
					add(dir)
					data[dir] = append(data[dir], p)
				}
			}
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
		sel.Files += len(byDir[dir]) + len(data[dir])
		if !heavySet[dir] {
			sel.Whole = append(sel.Whole, pkg)
			continue
		}
		sel.HeavyFiles += len(byDir[dir]) + len(data[dir])
		pdir := filepath.Join(root, filepath.FromSlash(dir))
		files := byDir[dir]
		var unplaced []string
		for _, f := range data[dir] {
			if gofiles := filesNaming(pdir, path.Base(f)); len(gofiles) > 0 {
				for _, g := range gofiles {
					if !slices.Contains(files, g) {
						files = append(files, g)
					}
				}
			} else {
				unplaced = append(unplaced, f)
			}
		}
		tests, none := heavyTests(pdir, files)
		if len(tests) > 0 {
			sel.Heavy = append(sel.Heavy, heavyRun{Pkg: pkg, Tests: tests})
		} else {
			sel.Compile = append(sel.Compile, pkg)
		}
		for _, f := range none {
			sel.NoTests = append(sel.NoTests, path.Join(dir, f))
		}
		sel.NoTests = append(sel.NoTests, unplaced...)
	}
	return sel
}

// inTestdata: a folder of the path is testdata.
func inTestdata(p string) bool {
	return slices.Contains(strings.Split(path.Dir(p), "/"), "testdata")
}

// inVendorOrHidden: the path is under vendor or a folder starting with . or _,
// where no package's embed pattern reaches without naming it.
func inVendorOrHidden(p string) bool {
	for _, e := range strings.Split(path.Dir(p), "/") {
		if e != "." && (e == "vendor" || strings.HasPrefix(e, ".") || strings.HasPrefix(e, "_")) {
			return true
		}
	}
	return false
}

// testdataOwner is the package a file under testdata/ belongs to: the nearest
// folder above the (first) testdata folder that is a package of this module.
func testdataOwner(root, p string) (string, bool) {
	parts := strings.Split(path.Dir(p), "/")
	i := slices.Index(parts, "testdata")
	dir := "."
	if i > 0 {
		dir = strings.Join(parts[:i], "/")
	}
	for {
		if isModulePackage(root, dir) {
			return dir, true
		}
		if dir == "." {
			return "", false
		}
		dir = path.Dir(dir)
	}
}

// ownersOf are the package directories that embed the file p. A file that is
// gone can't be told apart from the list, so the packages that embed
// something in its folder take it.
func (x embedIndex) ownersOf(root, p string) []string {
	if owners, ok := x.owners[p]; ok {
		return owners
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(p))); err == nil {
		return nil
	}
	return x.dirs[path.Dir(p)]
}

// filesNaming are the .go files of the package folder dir that mention name:
// the test files that do, else the other files that do (a //go:embed).
func filesNaming(dir, name string) []string {
	entries, _ := os.ReadDir(dir)
	var tests, sources []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 G122 -- a file of the checkout
		if err != nil || !bytes.Contains(b, []byte(name)) {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") {
			tests = append(tests, e.Name())
		} else {
			sources = append(sources, e.Name())
		}
	}
	if len(tests) > 0 {
		return tests
	}
	return sources
}

// listEmbeds asks go once which files each package of the module embeds.
func listEmbeds(root string) (embedIndex, error) {
	cmd := exec.Command("go", "list", "-e", "-json=Dir,EmbedFiles,TestEmbedFiles,XTestEmbedFiles", "./...") // #nosec G204 -- fixed arguments
	cmd.Dir = root
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return embedIndex{}, fmt.Errorf("go list: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return parseEmbeds(root, out.Bytes())
}

// parseEmbeds reads go list -json output.
func parseEmbeds(root string, data []byte) (embedIndex, error) {
	idx := embedIndex{owners: map[string][]string{}, dirs: map[string][]string{}}
	rootReal := evalDir(root)
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var pk struct {
			Dir                                         string
			EmbedFiles, TestEmbedFiles, XTestEmbedFiles []string
		}
		if err := dec.Decode(&pk); err == io.EOF {
			return idx, nil
		} else if err != nil {
			return idx, err
		}
		pdir, err := filepath.Rel(rootReal, evalDir(pk.Dir))
		if err != nil || pdir == ".." || strings.HasPrefix(pdir, "../") {
			continue
		}
		pdir = filepath.ToSlash(pdir)
		for _, f := range slices.Concat(pk.EmbedFiles, pk.TestEmbedFiles, pk.XTestEmbedFiles) {
			rel := path.Join(pdir, filepath.ToSlash(f))
			if !slices.Contains(idx.owners[rel], pdir) {
				idx.owners[rel] = append(idx.owners[rel], pdir)
			}
			if d := path.Dir(rel); !slices.Contains(idx.dirs[d], pdir) {
				idx.dirs[d] = append(idx.dirs[d], pdir)
			}
		}
	}
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
// -count=1: at most one for the whole packages and one for the heavy ones
// (split by package when one -run argument would be too long), and one that
// only compiles the heavy packages with no test selected.
func (s goSelection) commands(flags []string) [][]string {
	base := append([]string{"go", "test", "-count=1"}, flags...)
	var out [][]string
	if len(s.Whole) > 0 {
		out = append(out, slices.Concat(base, s.Whole))
	}
	out = s.heavyCommands(base, out)
	if len(s.Compile) > 0 {
		out = append(out, slices.Concat(base, []string{"-run", "^$"}, s.Compile))
	}
	return out
}

// heavyCommands adds the commands of the heavy packages' selected tests.
func (s goSelection) heavyCommands(base []string, out [][]string) [][]string {
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
