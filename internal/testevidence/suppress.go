package testevidence

import (
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"slices"
	"strings"

	"git.jmann.me/claude/waypoint/internal/jsscan"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

var (
	// gosec reads #nosec anywhere in a comment, then optional rule ids,
	// then "-- why".
	nosecRE = lazyre.New(`#nosec\b((?:[\s,]+G\d+)*)(?:\s*--\s*(.*))?`)
	// golangci-lint wants //nolint (or //nolint:a,b) to open the comment,
	// with the reason after a second //.
	nolintRE = lazyre.New(`^//\s?nolint(?::([\w,-]+))?(?:\s*//\s*(.*))?`)
	// wplint (internal/wplint) wants //wplint:ignore RULE, then the reason.
	wplintRE = lazyre.New(`^//\s*wplint:ignore\s+([\w-]+)[ \t]*(.*)`)
	// wplint's repo rules take "-- wplint:ignore RULE why" in a migration
	// and "// wplint:ignore RULE why" in an e2e script.
	sqlWaiverRE = lazyre.New(`^--\s*wplint:ignore\s+([\w-]+)[ \t]*(.*)`)
	jsWaiverRE  = lazyre.New(`^//\s*wplint:ignore\s+([\w-]+)[ \t]*(.*)`)
	ruleRE      = lazyre.New(`G\d+`)
	spaceRE     = lazyre.New(`\s+`)
)

// Suppressions lists the #nosec, //nolint and //wplint:ignore comments of the Go files under
// root, and the wplint waivers of SQL migrations and e2e scripts (.sql, .mjs), (vendor, testdata, node_modules and dot directories skipped), with
// the code each applies to: the line the comment ends, or the next line
// with code on it. Comments come from go/scanner, so a "#nosec" in a
// string is not one.
func Suppressions(root string) ([]Suppression, error) {
	fsys := os.DirFS(root)
	var out []Suppression
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != "." && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" || name == "node_modules") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, ".sql") && !strings.HasSuffix(name, ".mjs") {
			return nil
		}
		src, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		out = append(out, FileSuppressions(path, src)...)
		return nil
	})
	return out, err
}

// FileSuppressions is Suppressions of one file's source.
func FileSuppressions(path string, src []byte) []Suppression {
	switch {
	case strings.HasSuffix(path, ".sql"):
		return textWaivers(path, src, sqlCommentAt, sqlWaiverRE)
	case strings.HasSuffix(path, ".mjs"):
		var open byte
		return textWaivers(path, src, func(line string) int {
			var comment string
			_, comment, open = jsscan.StripComments(line, open)
			if comment == "" {
				return -1
			}
			return len(line) - len(comment)
		}, jsWaiverRE)
	}
	fset := token.NewFileSet()
	file := fset.AddFile(path, -1, len(src))
	var s scanner.Scanner
	s.Init(file, src, nil, scanner.ScanComments)
	type comment struct {
		line, end int
		text      string
	}
	var comments []comment
	code := map[int]bool{}
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		p := fset.Position(pos)
		if tok == token.COMMENT {
			end := p.Line + strings.Count(lit, "\n")
			comments = append(comments, comment{line: p.Line, end: end, text: lit})
			continue
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue
		}
		code[p.Line] = true
	}
	lines := strings.Split(string(src), "\n")
	codeAt := func(c comment) string {
		n := c.line
		if !code[n] {
			for n = c.end + 1; n <= len(lines) && !code[n]; n++ {
			}
		}
		if n > len(lines) {
			return ""
		}
		l := lines[n-1]
		if n == c.line {
			first, _, _ := strings.Cut(c.text, "\n")
			if i := strings.Index(l, first); i >= 0 {
				l = l[:i] + l[i+len(first):]
			}
		}
		return strings.TrimSpace(spaceRE.ReplaceAllString(l, " "))
	}
	var out []Suppression
	for _, c := range comments {
		var sup *Suppression
		if m := nolintRE.FindStringSubmatch(c.text); m != nil {
			sup = &Suppression{Kind: NoLint, Reason: strings.TrimSpace(m[2])}
			for r := range strings.SplitSeq(m[1], ",") {
				if r = strings.TrimSpace(r); r != "" && r != allRule {
					sup.Rules = append(sup.Rules, r)
				}
			}
		} else if m := wplintRE.FindStringSubmatch(c.text); m != nil {
			sup = &Suppression{Kind: WPLint, Rules: []string{m[1]}, Reason: strings.TrimSpace(m[2])}
		} else if m := nosecRE.FindStringSubmatch(c.text); m != nil {
			reason := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(m[2]), "*/"))
			sup = &Suppression{Kind: NoSec, Reason: reason, Rules: ruleRE.FindAllString(m[1], -1)}
		}
		if sup == nil {
			continue
		}
		slices.Sort(sup.Rules)
		sup.Rules = slices.Compact(sup.Rules)
		sup.Path, sup.Line, sup.Code = path, c.line, codeAt(c)
		out = append(out, *sup)
	}
	return out
}

// textWaivers finds wplint waivers in a file go/scanner can't read, line by
// line: a comment (where commentAt, called on each line in turn, says one
// starts) that opens with the waiver. One after code applies to that code;
// one on its own line to the next non-blank line.
func textWaivers(path string, src []byte, commentAt func(line string) int, re *lazyre.Regexp) []Suppression {
	lines := strings.Split(string(src), "\n")
	var out []Suppression
	for i, l := range lines {
		at := commentAt(l)
		if at < 0 {
			continue
		}
		m := re.FindStringSubmatch(l[at:])
		if m == nil {
			continue
		}
		code := strings.TrimSpace(l[:at])
		if code == "" {
			for _, next := range lines[i+1:] {
				if code = strings.TrimSpace(next); code != "" {
					break
				}
			}
		}
		out = append(out, Suppression{Kind: WPLint, Rules: []string{m[1]}, Path: path, Line: i + 1,
			Code: spaceRE.ReplaceAllString(code, " "), Reason: strings.TrimSpace(m[2])})
	}
	return out
}

// sqlCommentAt is where line's -- comment starts outside a quote, or -1.
// Backslash is a plain character in SQL strings (SQLite, and Postgres's
// standard_conforming_strings); a doubled quote closes and reopens, which
// leaves the state right.
func sqlCommentAt(line string) int {
	var q byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
			}
		case strings.HasPrefix(line[i:], "--"):
			return i
		case c == '\'' || c == '"':
			q = c
		}
	}
	return -1
}
