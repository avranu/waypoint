package codeindex

import (
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

var (
	pyDefRE    = lazyre.New(`^(\s*)(?:async\s+)?def\s+([A-Za-z_]\w*)`)
	pyClassRE  = lazyre.New(`^(\s*)class\s+([A-Za-z_]\w*)`)
	pyConstRE  = lazyre.New(`^([A-Za-z_]\w*)\s*(?::[^=]+)?=[^=]`)
	pyImportRE = lazyre.New(`^\s*import\s+(.+)$`)
	pyFromRE   = lazyre.New(`^\s*from\s+(\.*[\w.]*)\s+import\b`)
)

// pyLine is one physical line after string/comment masking.
type pyLine struct {
	code     string // strings and comments blanked out
	startsIn bool   // began inside a triple-quoted string or open bracket
	depth0   int    // bracket depth at the start of the line
	blank    bool   // nothing but whitespace/comment (outside a string)
	indent   int
}

// pyMask walks the file once, tracking triple-quoted strings and brackets so
// a def line inside a docstring is never mistaken for code and a statement
// that continues in brackets doesn't end a block early.
func pyMask(lines []string) []pyLine {
	out := make([]pyLine, len(lines))
	var triple string // the open triple-quote delimiter, "" when outside
	depth := 0
	for i, l := range lines {
		pl := pyLine{depth0: depth, startsIn: triple != "" || depth > 0}
		var b strings.Builder
		j := 0
		for j < len(l) {
			if triple != "" {
				if strings.HasPrefix(l[j:], triple) {
					triple = ""
					j += 3
					continue
				}
				if l[j] == '\\' {
					j++
				}
				j++
				continue
			}
			ch := l[j]
			switch ch {
			case '#':
				j = len(l)
			case '"', '\'':
				if strings.HasPrefix(l[j:], strings.Repeat(string(ch), 3)) {
					triple = strings.Repeat(string(ch), 3)
					b.WriteString(`""`)
					j += 3
					continue
				}
				k := j + 1
				for k < len(l) && l[k] != ch {
					if l[k] == '\\' {
						k++
					}
					k++
				}
				b.WriteString(`""`)
				j = k + 1
			case '(', '[', '{':
				depth++
				b.WriteByte(ch)
				j++
			case ')', ']', '}':
				if depth > 0 {
					depth--
				}
				b.WriteByte(ch)
				j++
			default:
				b.WriteByte(ch)
				j++
			}
		}
		pl.code = b.String()
		pl.blank = !pl.startsIn && strings.TrimSpace(pl.code) == "" && triple == ""
		if triple != "" && !pl.startsIn {
			pl.blank = false
		}
		pl.indent = len(l) - len(strings.TrimLeft(l, " \t"))
		out[i] = pl
	}
	return out
}

// scanPython finds classes, functions, methods and module constants with
// ranges by indentation, plus import statements.
func scanPython(src []byte) Parsed {
	lines := splitLines(src)
	ml := pyMask(lines)
	var out Parsed
	type open struct {
		indent int
		idx    int // index into out.Symbols
	}
	var stack []open
	closeTo := func(last, indent int) {
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			out.Symbols[stack[len(stack)-1].idx].End = last
			stack = stack[:len(stack)-1]
		}
	}
	// mk builds a def/class symbol for the statement on lines i..stmtEnd:
	// decorators above it extend its start, a docstring below gives its doc.
	mk := func(name, kind string, i, stmtEnd int) Symbol {
		s := Symbol{Name: name, Kind: kind, Start: i + 1, Decl: i + 1, Exported: !strings.HasPrefix(name, "_")}
		for j := i - 1; j >= 0 && !ml[j].blank && strings.HasPrefix(strings.TrimSpace(lines[j]), "@") && ml[j].indent == ml[i].indent; j-- {
			s.Start = j + 1
		}
		if len(stack) > 0 {
			par := out.Symbols[stack[len(stack)-1].idx]
			s.Parent = par.Name
			if par.Parent != "" {
				s.Parent = par.Parent + "." + par.Name
			}
			if kind == "func" && par.Kind == "class" {
				s.Kind = "method"
			}
		}
		s.Sig = collapse(strings.Join(lines[i:stmtEnd+1], " "), 200)
		s.Sig = strings.TrimSuffix(strings.TrimSpace(s.Sig), ":")
		if stmtEnd+1 < len(lines) {
			if t := strings.TrimSpace(lines[stmtEnd+1]); len(t) > 1 && (t[0] == '"' || t[0] == '\'') {
				body := strings.TrimRight(strings.TrimLeft(t, `"'`), `"'`)
				for k := stmtEnd + 2; body == "" && k < len(lines) && k < stmtEnd+4; k++ {
					body = strings.TrimSpace(lines[k])
				}
				s.Doc = docLine(body)
			}
		}
		return s
	}
	lastCode := 0 // last line (1-based) that held code or string content
	for i := 0; i < len(lines); i++ {
		m := ml[i]
		if m.startsIn || m.blank {
			if !m.blank {
				lastCode = i + 1
			}
			continue
		}
		// A real statement starts here: blocks at or below its indent end at
		// the last code line before it.
		closeTo(lastCode, m.indent)
		lastCode = i + 1
		endStmt := i // statements may continue across lines in brackets or strings
		for endStmt+1 < len(lines) && ml[endStmt+1].startsIn {
			endStmt++
			lastCode = endStmt + 1
		}
		code := m.code
		if mm := pyClassRE.FindStringSubmatch(code); mm != nil {
			out.Symbols = append(out.Symbols, mk(mm[2], "class", i, endStmt))
			stack = append(stack, open{m.indent, len(out.Symbols) - 1})
		} else if mm := pyDefRE.FindStringSubmatch(code); mm != nil {
			out.Symbols = append(out.Symbols, mk(mm[2], "func", i, endStmt))
			stack = append(stack, open{m.indent, len(out.Symbols) - 1})
		} else if m.indent == 0 {
			if mm := pyFromRE.FindStringSubmatch(code); mm != nil {
				out.Imports = append(out.Imports, mm[1])
			} else if mm := pyImportRE.FindStringSubmatch(code); mm != nil {
				for _, part := range strings.Split(mm[1], ",") {
					if f := strings.Fields(part); len(f) > 0 {
						out.Imports = append(out.Imports, f[0])
					}
				}
			} else if mm := pyConstRE.FindStringSubmatch(code); mm != nil && !strings.HasPrefix(mm[1], "__") {
				kind := "var"
				if strings.ToUpper(mm[1]) == mm[1] {
					kind = "const"
				}
				out.Symbols = append(out.Symbols, Symbol{Name: mm[1], Kind: kind, Sig: collapse(lines[i], 120), Start: i + 1, End: endStmt + 1, Exported: !strings.HasPrefix(mm[1], "_")})
			}
		}
		i = endStmt
	}
	closeTo(lastCode, 0)
	return out
}
