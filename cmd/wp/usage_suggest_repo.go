package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// sugDoc is a file read into every session.
type sugDoc struct {
	Name     string       `json:"name"` // as shown: AGENTS.md, ~/.claude/CLAUDE.md
	Path     string       `json:"-"`
	Bytes    int64        `json:"bytes"`
	Lines    int          `json:"lines"`
	Sections []sugSection `json:"sections,omitempty"` // largest first
	Unread   int          `json:"unread_lines,omitempty"`
	Memory   bool         `json:"memory,omitempty"`
}

type sugSection struct {
	Heading string `json:"heading"`
	Bytes   int64  `json:"bytes"`
}

const (
	memoryLines   = 200
	importDepth   = 4
	maxDocBytes   = 2 << 20
	maxScanBytes  = 1 << 20
	commentBlock  = 15
	approxTokenOf = 4
)

var importRE = lazyre.New(`(?:^|\s)@((?:~/|\.{0,2}/)?[A-Za-z0-9_./-]+\.[A-Za-z0-9]+)`)

// instructionDocs are the files Claude Code reads into every session that
// starts at root: the user's CLAUDE.md, CLAUDE.md (and CLAUDE.local.md,
// .claude/CLAUDE.md) in root and each directory above it, AGENTS.md in
// root, the files they import with @path, and the auto memory index of
// each directory in memDirs.
func instructionDocs(claudeRoot, root string, memDirs []string) []sugDoc {
	var docs []sugDoc
	seen := map[string]bool{}
	home, _ := os.UserHomeDir()
	show := func(p string) string {
		if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
		if home != "" {
			if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
				return "~/" + filepath.ToSlash(rel)
			}
		}
		return p
	}
	var add func(p string, depth int)
	add = func(p string, depth int) {
		p = filepath.Clean(p)
		if seen[p] || depth > importDepth {
			return
		}
		seen[p] = true
		b, err := readCapped(p, maxDocBytes)
		if err != nil {
			return
		}
		docs = append(docs, docOf(show(p), p, b))
		for _, imp := range docImports(b) {
			switch {
			case strings.HasPrefix(imp, "~/") && home != "":
				imp = filepath.Join(home, imp[2:])
			case !filepath.IsAbs(imp):
				imp = filepath.Join(filepath.Dir(p), imp)
			}
			add(imp, depth+1)
		}
	}
	add(filepath.Join(claudeRoot, "CLAUDE.md"), 0)
	var dirs []string
	for d := filepath.Clean(root); ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if filepath.Dir(d) == d || d == home {
			break
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		for _, name := range []string{"CLAUDE.md", "CLAUDE.local.md", filepath.Join(".claude", "CLAUDE.md")} {
			add(filepath.Join(dirs[i], name), 0)
		}
	}
	add(filepath.Join(root, "AGENTS.md"), 0)
	for _, d := range memDirs {
		p := filepath.Join(claudeRoot, "projects", projectDirName(d), "memory", "MEMORY.md")
		if seen[p] {
			continue
		}
		seen[p] = true
		b, err := readCapped(p, maxDocBytes)
		if err != nil {
			continue
		}
		doc := docOf(show(p), p, b)
		doc.Memory = true
		if doc.Lines > memoryLines {
			doc.Unread = doc.Lines - memoryLines
			doc.Bytes = int64(len(firstLines(b, memoryLines)))
		}
		docs = append(docs, doc)
	}
	return docs
}

// projectDirName is the folder Claude Code keeps a directory's sessions and
// memory in: the path with every character but letters and digits as "-".
func projectDirName(dir string) string {
	var b strings.Builder
	for _, r := range dir {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func readCapped(p string, n int64) ([]byte, error) {
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > n {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(p) // #nosec G304 -- an instruction file Claude Code reads
}

func firstLines(b []byte, n int) []byte {
	i := 0
	for ; n > 0; n-- {
		j := bytes.IndexByte(b[i:], '\n')
		if j < 0 {
			return b
		}
		i += j + 1
	}
	return b[:i]
}

// docImports lists the @path imports of a markdown file, outside code.
func docImports(b []byte) []string {
	var out []string
	fence := false
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fence = !fence
			continue
		}
		if fence {
			continue
		}
		line = inlineCodeRE.ReplaceAllString(line, "")
		for _, m := range importRE.FindAllStringSubmatch(line, -1) {
			out = append(out, m[1])
		}
	}
	return out
}

var inlineCodeRE = lazyre.New("`[^`]*`")

// docOf measures a file and its sections (by heading, level 1 to 3).
func docOf(name, path string, b []byte) sugDoc {
	d := sugDoc{Name: name, Path: path, Bytes: int64(len(b))}
	sec := sugSection{Heading: "(before the first heading)"}
	fence := false
	flush := func() {
		if sec.Bytes > 0 {
			d.Sections = append(d.Sections, sec)
		}
	}
	for _, line := range strings.SplitAfter(string(b), "\n") {
		if line == "" {
			continue
		}
		d.Lines++
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			fence = !fence
		}
		if !fence && strings.HasPrefix(t, "#") {
			level := len(t) - len(strings.TrimLeft(t, "#"))
			if level <= 3 && len(t) > level && t[level] == ' ' {
				flush()
				sec = sugSection{Heading: strings.TrimSpace(t[level:])}
			}
		}
		sec.Bytes += int64(len(line))
	}
	flush()
	sort.SliceStable(d.Sections, func(i, j int) bool { return d.Sections[i].Bytes > d.Sections[j].Bytes })
	if len(d.Sections) < 2 {
		d.Sections = nil
	}
	return d
}

// sugSource is one tracked source file's comments.
type sugSource struct {
	Path         string
	Lines        int
	CommentLines int
	CommentBytes int64
	BlockStart   int // the longest run of comment lines: its first line
	BlockLines   int
}

var commentStyles = map[string]string{
	".go": "//", ".ts": "//", ".tsx": "//", ".js": "//", ".jsx": "//", ".mjs": "//", ".cjs": "//", ".rs": "//",
	".java": "//", ".kt": "//", ".swift": "//", ".c": "//", ".h": "//", ".cc": "//", ".cpp": "//", ".hpp": "//",
	".cs": "//", ".scala": "//", ".dart": "//", ".php": "//",
	".py": "#", ".sh": "#", ".bash": "#", ".rb": "#", ".yaml": "#", ".yml": "#", ".toml": "#", ".pl": "#",
	".ex": "#", ".exs": "#", ".nix": "#", ".tf": "#", ".r": "#",
	".sql": "--", ".lua": "--", ".hs": "--",
}

var skipSourceRE = lazyre.New(`(^|/)(node_modules|vendor|dist|build|third_party|testdata)/|\.min\.|\.pb\.go$|_gen\.|\.gen\.|generated`)

// scanComments measures the comments in the tracked source files.
func scanComments(root string, tracked []string) []sugSource {
	var out []sugSource
	for _, rel := range tracked {
		style := commentStyles[strings.ToLower(filepath.Ext(rel))]
		if style == "" || skipSourceRE.MatchString(rel) {
			continue
		}
		b, err := readCapped(filepath.Join(root, filepath.FromSlash(rel)), maxScanBytes)
		if err != nil || bytes.Contains(b[:min(len(b), 512)], []byte("Code generated")) {
			continue
		}
		s := commentsOf(b, style)
		s.Path = rel
		out = append(out, s)
	}
	return out
}

// commentsOf counts comment lines: lines that start with the language's
// line comment, and /* … */ blocks in C-like languages. Compiler
// directives (//go:build, //nolint, #!) are not prose and don't count.
func commentsOf(b []byte, style string) sugSource {
	var s sugSource
	inBlock := false
	run, runStart := 0, 0
	end := func() {
		if run > s.BlockLines {
			s.BlockLines, s.BlockStart = run, runStart
		}
		run = 0
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), maxScanBytes)
	for sc.Scan() {
		s.Lines++
		t := strings.TrimSpace(sc.Text())
		comment := false
		switch {
		case inBlock:
			comment = true
			if strings.Contains(t, "*/") {
				inBlock = false
			}
		case style == "//" && strings.HasPrefix(t, "/*"):
			comment = true
			inBlock = !strings.Contains(t[2:], "*/")
		case strings.HasPrefix(t, style):
			comment = !strings.HasPrefix(t, "//go:") && !strings.HasPrefix(t, "//nolint") && !strings.HasPrefix(t, "#!")
		}
		if !comment {
			end()
			continue
		}
		s.CommentLines++
		s.CommentBytes += int64(len(t))
		if run == 0 {
			runStart = s.Lines
		}
		run++
	}
	end()
	return s
}

// sugPaths turns a path a session used into one relative to the repository.
type sugPaths struct {
	roots    map[string]bool
	mainRoot string
	tracked  map[string]bool
}

// rel is p (absolute, or relative to cwd) as a tracked file's path, or "".
func (sp *sugPaths) rel(p, cwd string) string {
	if p == "" {
		return ""
	}
	if i := strings.LastIndexByte(p, ':'); i > 0 && !strings.ContainsAny(p[i+1:], `/\`) {
		p = p[:i]
	}
	if !filepath.IsAbs(p) {
		if cwd == "" {
			return ""
		}
		p = filepath.Join(cwd, p)
	}
	p = filepath.Clean(p)
	parent, base := filepath.Dir(sp.mainRoot), filepath.Base(sp.mainRoot)
	for d := filepath.Dir(p); d != filepath.Dir(d); d = filepath.Dir(d) {
		b := filepath.Base(d)
		if sp.roots[d] || filepath.Dir(d) == parent && (b == base || strings.HasPrefix(b, base+"-")) {
			r := filepath.ToSlash(p[len(d)+1:])
			if sp.tracked[r] {
				return r
			}
			return ""
		}
	}
	return ""
}

// readPaths are the tracked files a call read whole or in part: Read, and
// cat, head, tail, sed, less, nl, bat or wp code show in a Bash command.
func (sp *sugPaths) readPaths(c sugCall, cwd string) []string {
	var out []string
	add := func(p string) {
		if r := sp.rel(p, cwd); r != "" {
			out = append(out, r)
		}
	}
	switch c.Tool {
	case "Read":
		add(c.Path)
	case "Bash":
		for _, seg := range shellWords(c.Command) {
			w := stripWords(seg)
			if len(w) < 2 {
				continue
			}
			args := w[1:]
			switch filepath.Base(w[0]) {
			case "cat", "head", "tail", "sed", "less", "more", "nl", "bat":
			case "wp":
				if len(w) < 4 || w[1] != "code" || w[2] != "show" {
					continue
				}
				args = w[3:]
			default:
				continue
			}
			for _, a := range args {
				if !strings.HasPrefix(a, "-") {
					add(a)
				}
			}
		}
	}
	return out
}
