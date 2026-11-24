package codeindex

import (
	"regexp"
	"slices"
	"strings"
	"sync"
)

type bLine struct {
	code        string
	comment     string
	onlyComment bool
	depth       int
	pdepth      int
	opens       []int
	semis       []int
}

type bOpener struct {
	line   int
	close  int
	pdepth int
}

func lexBrace(lang string, lines []string) ([]bLine, []bOpener) {
	out := make([]bLine, len(lines))
	var openers []bOpener
	var stack []int
	pdepth := 0
	var block bool
	var tmpl string
	jsLike := lang == "typescript" || lang == "tsx" || lang == "javascript"
	for i, l := range lines {
		bl := bLine{depth: len(stack), pdepth: pdepth}
		var code, cmt strings.Builder
		sawCode := false
		j := 0
		for j < len(l) {
			if block {
				k := strings.Index(l[j:], "*/")
				if k < 0 {
					cmt.WriteString(l[j:])
					j = len(l)
				} else {
					cmt.WriteString(l[j : j+k])
					j += k + 2
					block = false
				}
				continue
			}
			if tmpl != "" {
				k := strings.Index(l[j:], tmpl)
				if k < 0 {
					j = len(l)
				} else {
					j += k + len(tmpl)
					tmpl = ""
				}
				continue
			}
			ch := l[j]
			switch {
			case strings.HasPrefix(l[j:], "//"):
				cmt.WriteString(l[j+2:])
				j = len(l)
			case strings.HasPrefix(l[j:], "/*"):
				block = true
				j += 2
			case ch == '`' && jsLike:
				tmpl = "`"
				code.WriteString("``")
				sawCode = true
				j++
			case lang == "rust" && ch == 'r' && rustRawStart(l[j:]) > 0 && (j == 0 || !isIdentByte(l[j-1])):
				hashes := rustRawStart(l[j:]) - 2
				tmpl = `"` + strings.Repeat("#", hashes)
				j += 2 + hashes
				code.WriteString(`""`)
				sawCode = true
			case (lang == "java" || lang == "kotlin") && strings.HasPrefix(l[j:], `"""`):
				tmpl = `"""`
				j += 3
				code.WriteString(`""`)
				sawCode = true
			case ch == '"' || (ch == '\'' && lang != "rust") || (ch == '\'' && rustCharLen(l[j:]) > 0):
				k := j + 1
				if ch == '\'' && lang == "rust" {
					k = j + rustCharLen(l[j:]) - 1
				} else {
					for k < len(l) && l[k] != ch {
						if l[k] == '\\' {
							k++
						}
						k++
					}
				}
				code.WriteString(`""`)
				sawCode = true
				j = k + 1
			case ch == '/' && jsLike && regexAllowed(code.String()):
				k := j + 1
				inClass := false
				for k < len(l) && (l[k] != '/' || inClass) {
					switch l[k] {
					case '\\':
						k++
					case '[':
						inClass = true
					case ']':
						inClass = false
					}
					k++
				}
				if k >= len(l) {
					code.WriteByte(ch)
					j++
					break
				}
				code.WriteString("/r/")
				sawCode = true
				j = k + 1
			case ch == '{':
				openers = append(openers, bOpener{line: i, close: -1, pdepth: pdepth})
				stack = append(stack, len(openers)-1)
				bl.opens = append(bl.opens, len(openers)-1)
				code.WriteByte(ch)
				sawCode = true
				j++
			case ch == '}':
				if n := len(stack); n > 0 {
					openers[stack[n-1]].close = i
					stack = stack[:n-1]
				}
				code.WriteByte(ch)
				sawCode = true
				j++
			case ch == '(' || ch == '[':
				pdepth++
				code.WriteByte(ch)
				sawCode = true
				j++
			case ch == ')' || ch == ']':
				if pdepth > 0 {
					pdepth--
				}
				code.WriteByte(ch)
				sawCode = true
				j++
			case ch == ';':
				bl.semis = append(bl.semis, pdepth)
				code.WriteByte(ch)
				sawCode = true
				j++
			default:
				if ch != ' ' && ch != '\t' {
					sawCode = true
				}
				code.WriteByte(ch)
				j++
			}
		}
		bl.code = code.String()
		bl.comment = cmt.String()
		bl.onlyComment = !sawCode && (strings.TrimSpace(bl.comment) != "" || block)
		out[i] = bl
	}
	last := len(lines) - 1
	for i := range openers {
		if openers[i].close < 0 {
			openers[i].close = last
		}
	}
	return out, openers
}

func isIdentByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// rustRawStart returns the length of a raw-string opener (r#"), 0 if none.
func rustRawStart(s string) int {
	if len(s) < 2 || s[0] != 'r' {
		return 0
	}
	n := 1
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n < len(s) && s[n] == '"' {
		return n + 1
	}
	return 0
}

// rustCharLen is the length of a char literal at s ('x', '\n', '\u{..}'),
// or 0 when the quote starts a lifetime.
func rustCharLen(s string) int {
	if len(s) >= 3 && s[1] != '\\' && s[1] != '\'' {
		for k := 2; k < len(s) && k <= 5; k++ {
			if s[k] == '\'' {
				return k + 1
			}
			if s[k]&0xC0 != 0x80 {
				break
			}
		}
		return 0
	}
	if len(s) >= 4 && s[1] == '\\' {
		if k := strings.IndexByte(s[2:], '\''); k >= 0 && k < 10 {
			return k + 3
		}
	}
	return 0
}

// regexAllowed guesses whether a '/' here starts a regex literal rather
// than a division, from the code before it on the line.
func regexAllowed(before string) bool {
	t := strings.TrimRight(before, " \t")
	if t == "" {
		return true
	}
	switch t[len(t)-1] {
	case '(', ',', '=', ':', '[', '!', '&', '|', '?', '{', '}', ';', '+', '-', '*', '%', '<', '>', '~', '^':
		return true
	}
	return strings.HasSuffix(t, "return") || strings.HasSuffix(t, "typeof")
}

// braceRule is one declaration pattern.
type braceRule struct {
	re        *regexp.Regexp
	kind      string
	nameGroup int
	scope     int
	container bool
	transpar  bool
	callWrap  bool
	anon      bool
	raw       bool
	resolve   func(m []string) (name string)
}

const (
	scopeAny = iota
	scopeTop
	scopeMember
)

const jsMods = `(?:(?:public|private|protected|static|readonly|abstract|override|async|declare|accessor)\s+)*`

var ctrlWords = map[string]bool{"if": true, "for": true, "while": true, "switch": true, "catch": true, "return": true, "function": true, "new": true, "else": true, "do": true, "try": true, "throw": true, "super": true, "this": true, "await": true, "typeof": true, "delete": true, "void": true, "synchronized": true, "when": true, "match": true, "with": true}

const javaMods = `(?:(?:public|private|protected|static|final|abstract|synchronized|native|default|strictfp|sealed|non-sealed|transient|volatile)\s+)*`
const ktMods = `(?:(?:public|private|protected|internal|abstract|open|override|suspend|inline|operator|infix|tailrec|external|final|data|sealed|enum|annotation|inner|value|companion|const|lateinit|actual|expect)\s+)*`
const rsVis = `(?:pub(?:\([^)]*\))?\s+)?`

// braceRules builds on first use: its patterns cost ~1 MB at every process
// start, and the hooks that never scan code run under a 2 s deadline (WP-1398).
var braceRules = sync.OnceValue(func() map[string][]braceRule {
	ts := tsRules()
	return map[string][]braceRule{
		"typescript": ts,
		"tsx":        ts,
		"javascript": ts,
		"rust": {
			{re: regexp.MustCompile(`^` + rsVis + `(?:default\s+)?(?:const\s+)?(?:async\s+)?(?:unsafe\s+)?(?:extern\s+"[^"]*"\s+)?fn\s+(\w+)`), kind: "func", nameGroup: 1},
			{re: regexp.MustCompile(`^` + rsVis + `(struct|enum|union)\s+(\w+)`), kind: "type", nameGroup: 2},
			{re: regexp.MustCompile(`^` + rsVis + `(?:unsafe\s+)?trait\s+(\w+)`), kind: "trait", nameGroup: 1, container: true},
			{re: regexp.MustCompile(`^(?:unsafe\s+)?impl\b(.*)$`), kind: "impl", nameGroup: 1, container: true, resolve: rustImplName},
			{re: regexp.MustCompile(`^` + rsVis + `mod\s+(\w+)`), kind: "mod", nameGroup: 1, container: true, transpar: true},
			{re: regexp.MustCompile(`^` + rsVis + `(?:const|static)\s+(?:mut\s+)?(\w+)`), kind: "const", nameGroup: 1, scope: scopeTop},
			{re: regexp.MustCompile(`^` + rsVis + `type\s+(\w+)`), kind: "type", nameGroup: 1},
			{re: regexp.MustCompile(`^macro_rules!\s*(\w+)`), kind: "macro", nameGroup: 1},
		},
		"java": {
			{re: regexp.MustCompile(`^(?:@\w+(?:\([^)]*\))?\s+)*` + javaMods + `(class|interface|enum|record|@interface)\s+(\w+)`), kind: "class", nameGroup: 2, container: true},
			{re: regexp.MustCompile(`^(?:@\w+(?:\([^)]*\))?\s+)*` + javaMods + `(?:<[^>]+>\s+)?([A-Z]\w*)\s*\(`), kind: "ctor", nameGroup: 1, scope: scopeMember},
			{re: regexp.MustCompile(`^(?:@\w+(?:\([^)]*\))?\s+)*` + javaMods + `(?:<[^>]+>\s+)?[\w.\[\]?]+(?:<[^()]*>)?(?:\[\])*\s+(\w+)\s*\(`), kind: "method", nameGroup: 1, scope: scopeMember},
		},
		"kotlin": {
			{re: regexp.MustCompile(`^(?:@\w+\s+)*` + ktMods + `(class|interface|object)\s+(\w+)`), kind: "class", nameGroup: 2, container: true},
			{re: regexp.MustCompile(`^(?:@\w+\s+)*` + ktMods + `fun\s+(?:<[^>]*>\s*)?(?:[\w.<>?, ]+\.)?(\w+)\s*\(`), kind: "func", nameGroup: 1},
			{re: regexp.MustCompile(`^(?:@\w+\s+)*` + ktMods + `typealias\s+(\w+)`), kind: "type", nameGroup: 1},
			{re: regexp.MustCompile(`^(?:@\w+\s+)*` + ktMods + `(?:val|var)\s+(\w+)`), kind: "var", nameGroup: 1, scope: scopeTop},
		},
	}
})

func tsRules() []braceRule {
	return []braceRule{
		{re: regexp.MustCompile(`^(?:describe|suite|context)(?:\.\w+)*\(\s*(['"` + "`" + `])(.+?)['"` + "`" + `]`), kind: "suite", nameGroup: 2, container: true, transpar: true, callWrap: true, raw: true},
		{re: regexp.MustCompile(`^(?:test|it)(?:\.\w+)*\(\s*(['"` + "`" + `])(.+?)['"` + "`" + `]`), kind: "test", nameGroup: 2, callWrap: true, raw: true},
		{re: regexp.MustCompile(`^(?:[;!]\s*)?\(\s*(?:async\s+)?(?:function\b|\([^)]*\)\s*=>)`), kind: "wrapper", container: true, transpar: true, callWrap: true, anon: true, scope: scopeTop},
		{re: regexp.MustCompile(`^\$\(\s*(?:document\)\.ready\(\s*)?(?:function\b|\([^)]*\)\s*=>)`), kind: "wrapper", container: true, transpar: true, callWrap: true, anon: true, scope: scopeTop},
		{re: regexp.MustCompile(`^(?:document|window)\.addEventListener\(\s*['"][^'"]*['"]\s*,\s*(?:async\s+)?(?:function\b|\([^)]*\)\s*=>)`), kind: "wrapper", container: true, transpar: true, callWrap: true, anon: true, scope: scopeTop},
		{re: regexp.MustCompile(`^(?:export\s+)?(?:default\s+)?(?:declare\s+)?(?:abstract\s+)?class\s+(\w+)`), kind: "class", nameGroup: 1, container: true},
		{re: regexp.MustCompile(`^(?:export\s+)?(?:declare\s+)?interface\s+(\w+)`), kind: "interface", nameGroup: 1, container: true},
		{re: regexp.MustCompile(`^(?:export\s+)?(?:declare\s+)?type\s+(\w+)\s*(?:<[^=]*>)?\s*=`), kind: "type", nameGroup: 1},
		{re: regexp.MustCompile(`^(?:export\s+)?(?:declare\s+)?(?:const\s+)?enum\s+(\w+)`), kind: "enum", nameGroup: 1},
		{re: regexp.MustCompile(`^(?:export\s+)?(?:declare\s+)?(?:namespace|module)\s+(\w+)`), kind: "namespace", nameGroup: 1, container: true, transpar: true},
		{re: regexp.MustCompile(`^(?:export\s+)?(?:default\s+)?(?:declare\s+)?(?:async\s+)?function\s*\*?\s*(\w+)`), kind: "func", nameGroup: 1, scope: scopeTop},
		{re: regexp.MustCompile(`^(?:export\s+)?(?:declare\s+)?(const|let|var)\s+(\w+)`), kind: "const", nameGroup: 2, scope: scopeTop},
		{re: regexp.MustCompile(`^` + jsMods + `(?:get\s+|set\s+)?\*?\s*(#?\w+)\s*(?:<[^()]*>)?\s*\??\s*\(`), kind: "method", nameGroup: 1, scope: scopeMember},
		{re: regexp.MustCompile(`^` + jsMods + `(#?\w+)\s*\??\s*(?::[^=]+)?=\s*(?:async\s*)?(?:\([^)]*\)|\w+)\s*(?::[^=]+)?=>`), kind: "method", nameGroup: 1, scope: scopeMember},
	}
}

func rustImplName(m []string) string {
	rest := m[1]
	if i := strings.Index(rest, " for "); i >= 0 {
		rest = rest[i+5:]
	}
	rest = strings.TrimSpace(rest)
	for _, p := range []string{"&", "'a ", "mut ", "dyn "} {
		rest = strings.TrimPrefix(rest, p)
	}
	if strings.HasPrefix(rest, "<") {
		depth := 0
		for k, r := range rest {
			if r == '<' {
				depth++
			} else if r == '>' {
				depth--
				if depth == 0 {
					rest = strings.TrimSpace(rest[k+1:])
					break
				}
			}
		}
		if i := strings.Index(rest, " for "); i >= 0 {
			rest = rest[i+5:]
		}
	}
	end := 0
	for end < len(rest) && (isIdentByte(rest[end]) || rest[end] == ':') {
		end++
	}
	name := rest[:end]
	if i := strings.LastIndex(name, "::"); i >= 0 {
		name = name[i+2:]
	}
	return name
}

var (
	jsImportRE   = regexp.MustCompile(`(?:^|[\s;(])(?:import|export)\s+(?:type\s+)?(?:[^'"` + "`" + `;]*?\s+from\s+)?['"]([^'"]+)['"]`)
	jsRequireRE  = regexp.MustCompile(`\b(?:require|import)\(\s*['"]([^'"]+)['"]\s*\)`)
	rustUseRE    = regexp.MustCompile(`^\s*(?:pub\s+)?use\s+([\w:]+)`)
	javaImportRE = regexp.MustCompile(`^\s*import\s+(?:static\s+)?([\w.]+)`)
)

func scanBrace(lang string, src []byte) Parsed {
	lines := splitLines(src)
	bl, openers := lexBrace(lang, lines)
	rules := braceRules()[lang]
	var out Parsed
	type cont struct {
		name        string
		bodyDepth   int
		end         int
		transparent bool
		pdepth      int
	}
	var cs []cont
	for i := range lines {
		for len(cs) > 0 && cs[len(cs)-1].end < i {
			cs = cs[:len(cs)-1]
		}
		b := bl[i]
		t := strings.TrimSpace(b.code)
		base := 0
		if len(cs) > 0 {
			base = cs[len(cs)-1].pdepth
		}
		if t == "" || b.onlyComment || b.pdepth != base {
			continue
		}
		switch lang {
		case "typescript", "tsx", "javascript":
			if b.depth == 0 {
				if m := jsImportRE.FindStringSubmatch(lines[i]); m != nil {
					out.Imports = append(out.Imports, m[1])
				}
			}
			for _, m := range jsRequireRE.FindAllStringSubmatch(lines[i], -1) {
				out.Imports = append(out.Imports, m[1])
			}
		case "rust":
			if m := rustUseRE.FindStringSubmatch(lines[i]); m != nil && b.depth == 0 {
				out.Imports = append(out.Imports, strings.TrimRight(m[1], ":"))
			}
		case "java", "kotlin":
			if m := javaImportRE.FindStringSubmatch(lines[i]); m != nil {
				out.Imports = append(out.Imports, strings.TrimRight(m[1], "."))
			}
		}
		want := 0
		var parent string
		var inMember, transparent bool
		if len(cs) > 0 {
			top := cs[len(cs)-1]
			want, parent, transparent = top.bodyDepth, top.name, top.transparent
			inMember = !transparent
		}
		if b.depth != want {
			continue
		}
		for _, r := range rules {
			if r.scope == scopeTop && inMember || r.scope == scopeMember && !inMember {
				continue
			}
			src := t
			if r.raw {
				src = strings.TrimSpace(lines[i])
			}
			m := r.re.FindStringSubmatch(src)
			if m == nil {
				continue
			}
			name := m[r.nameGroup]
			if r.anon {
				name = ""
			}
			if r.raw {
				name = collapse(name, 100)
			}
			if r.resolve != nil {
				name = r.resolve(m)
			}
			if name == "" && !r.anon || r.scope == scopeMember && ctrlWords[name] && lang != "rust" && lang != "kotlin" {
				continue
			}
			extra := 0
			if r.callWrap {
				extra = 1
			}
			end := declEnd(bl, openers, i, extra)
			kind := r.kind
			if kind == "func" && inMember {
				kind = "method"
			}
			sym := Symbol{Name: name, Kind: kind, Parent: parent, Start: i + 1, Decl: i + 1, End: end + 1, Sig: collapse(declHeader(lines, bl, i, end), 200), Exported: braceExported(lang, t, name)}
			for j := i - 1; j >= 0; j-- {
				tj := strings.TrimSpace(bl[j].code)
				if bl[j].onlyComment || strings.HasPrefix(tj, "@") || strings.HasPrefix(tj, "#[") {
					sym.Start = j + 1
					continue
				}
				break
			}
			sym.Doc = docAbove(bl, i)
			if r.kind == "const" && lang != "rust" {
				for j := i; j <= min(end, i+4); j++ {
					if strings.Contains(bl[j].code, "=>") || strings.Contains(bl[j].code, "function") {
						sym.Kind = "func"
						break
					}
				}
				if m[1] == "let" || m[1] == "var" {
					sym.Kind = "var"
				}
			}
			if !r.anon {
				out.Symbols = append(out.Symbols, sym)
			}
			if r.container {
				if body, ok := bodyOpener(bl, openers, i, end, extra); ok {
					cs = append(cs, cont{name: name, bodyDepth: bl[body.line].depth + 1, end: body.close, transparent: r.transpar, pdepth: body.pdepth})
				}
			}
			break
		}
	}
	return out
}

func braceExported(lang, t, name string) bool {
	switch lang {
	case "rust":
		return strings.HasPrefix(t, "pub")
	case "java", "kotlin":
		return !strings.HasPrefix(t, "private") && !strings.Contains(t[:min(len(t), 40)], " private ")
	}
	return strings.HasPrefix(t, "export") || !strings.HasPrefix(name, "_") && !strings.HasPrefix(name, "#")
}

// bodyOpener finds the brace that opens the declaration starting at line i
// (ending by line end): the first brace at paren depth 0 that closes on a
// later line.
func bodyOpener(bl []bLine, openers []bOpener, i, end, extra int) (bOpener, bool) {
	for j := i; j <= end && j < len(bl); j++ {
		for _, oi := range bl[j].opens {
			if o := openers[oi]; o.pdepth == bl[i].pdepth+extra && o.close > o.line {
				return o, true
			}
		}
	}
	return bOpener{}, false
}

// declEnd is the last line of the declaration starting at line i: the close
// of its body brace, or the line where its statement ends.
func declEnd(bl []bLine, openers []bOpener, i, extra int) int {
	limit := min(len(bl)-1, i+60)
	last := i
	for j := i; j <= limit; j++ {
		if j > i && strings.TrimSpace(bl[j].code) == "" && !bl[j].onlyComment {
			continue
		}
		if bl[j].onlyComment {
			continue
		}
		last = j
		for _, oi := range bl[j].opens {
			if o := openers[oi]; o.pdepth == bl[i].pdepth+extra {
				return o.close
			}
		}
		if slices.Contains(bl[j].semis, bl[i].pdepth) {
			return j
		}
		k := j + 1
		for k < len(bl) && strings.TrimSpace(bl[k].code) == "" {
			k++
		}
		if k >= len(bl) {
			return j
		}
		if bl[k].pdepth == bl[i].pdepth && !trailingContinues(bl[j].code) && !leadingContinues(bl[k].code) {
			return j
		}
	}
	return last
}

func trailingContinues(code string) bool {
	t := strings.TrimSpace(code)
	if t == "" {
		return true
	}
	switch t[len(t)-1] {
	case ',', '=', '(', '[', '+', '-', '*', '/', '&', '|', ':', '.', '<':
		return true
	}
	return strings.HasSuffix(t, "=>") || strings.HasPrefix(t, "where")
}

func leadingContinues(code string) bool {
	t := strings.TrimSpace(code)
	if t == "" {
		return false
	}
	switch t[0] {
	case '.', '?', ':', '+', '-', '*', '/', '&', '|', ')', ']', '=', '<', '>', ',':
		return true
	}
	for _, w := range []string{"extends", "implements", "where", "throws", "else", "catch", "finally"} {
		if strings.HasPrefix(t, w) && (len(t) == len(w) || !isIdentByte(t[len(w)])) {
			return true
		}
	}
	return false
}

// declHeader joins a declaration's lines up to its body brace or end.
func declHeader(lines []string, bl []bLine, i, end int) string {
	var b strings.Builder
	for j := i; j <= end && j < i+6; j++ {
		c := bl[j].code
		if k := strings.IndexByte(c, '{'); k >= 0 && bl[j].depth == bl[i].depth {
			b.WriteString(strings.TrimSpace(lines[j][:min(k, len(lines[j]))]))
			break
		}
		b.WriteString(strings.TrimSpace(lines[j]))
		b.WriteByte(' ')
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(b.String()), "{"))
}

// docAbove is the first line of the comment block directly above line i
// (skipping decorators and annotations).
func docAbove(bl []bLine, i int) string {
	var parts []string
	j := i - 1
	for ; j >= 0; j-- {
		tj := strings.TrimSpace(bl[j].code)
		if bl[j].onlyComment {
			parts = append(parts, bl[j].comment)
			continue
		}
		if strings.HasPrefix(tj, "@") || strings.HasPrefix(tj, "#[") {
			continue
		}
		break
	}
	for l, r := 0, len(parts)-1; l < r; l, r = l+1, r-1 {
		parts[l], parts[r] = parts[r], parts[l]
	}
	return docLine(strings.Join(parts, "\n"))
}
