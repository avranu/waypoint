package transcript

import "strings"

// ShellCmd is one simple command of a parsed shell line. The guard, the
// usage heads and the write recogniser all read lines with this parser.
type ShellCmd struct {
	Words   []string
	Open    []bool
	Subs    [][]*ShellCmd
	Redirs  []string
	Ins     []string // files it reads with <
	Heredoc string
	PipeIn  bool
	AndIn   bool

	Detached bool

	// Cut, on the last command of a line, says the line nests deeper than the
	// parser reads (shellMaxNest): what stands past that depth was not read.
	Cut bool

	// Group is the innermost ( ) group the command stands in (nil at the top
	// level of the line): a subshell, whose cd and variables end with it.
	Group *ShellGroup
}

// ShellGroup is a ( ) group; Parent is the group around it. Groups nested
// deeper than shellMaxGroups share the one outside them.
type ShellGroup struct {
	Parent *ShellGroup
	depth  int
}

const shellMaxGroups = 64

type shellHeredoc struct {
	cmd   *ShellCmd
	delim string
	strip bool
}

type shellParser struct {
	s       string
	i       int
	end     byte
	nest    int
	pending []shellHeredoc
	open    bool
	cut     bool
}

const shellMaxNest = 12

// ParseShell reads a command line into simple commands.
func ParseShell(s string) []*ShellCmd {
	p := &shellParser{s: s}
	out := p.list(0)
	if p.cut {
		if len(out) == 0 {
			out = append(out, &ShellCmd{})
		}
		out[len(out)-1].Cut = true
	}
	return out
}

// shellKeywords begin a command without being one.
var shellKeywords = map[string]bool{"then": true, "do": true, "else": true, "elif": true, "if": true, "while": true, "until": true, "!": true, "{": true, "}": true, "fi": true, "done": true, "esac": true}

func isShellMeta(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', ';', '&', '|', '(', ')', '<', '>':
		return true
	}
	return false
}

// IsDigits says s is one or more ASCII digits.
func IsDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (p *shellParser) peek(n int) byte {
	if p.i+n < len(p.s) {
		return p.s[p.i+n]
	}
	return 0
}

// list reads commands until end (or the end of the text).
func (p *shellParser) list(end byte) []*ShellCmd {
	prevEnd := p.end
	p.end = end
	defer func() { p.end = prevEnd }()
	p.nest++
	defer func() { p.nest-- }()
	var out []*ShellCmd
	cur := &ShellCmd{}
	pipe := false
	and := false
	listStart := 0
	var groups []int
	var grp *ShellGroup
	var outer []*ShellGroup
	flush := func(nextPipe bool) bool {
		added := false
		if len(cur.Words) > 0 || len(cur.Redirs) > 0 || len(cur.Subs) > 0 {
			cur.PipeIn, cur.AndIn, cur.Group = pipe, and, grp
			out = append(out, cur)
			pipe, and, added = nextPipe, false, true
		} else if !nextPipe {
			pipe = false
		}
		cur = &ShellCmd{}
		return added
	}
	if p.nest > shellMaxNest {
		p.i = len(p.s)
		p.cut = true
		return nil
	}
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			p.i++
		case c == '\\' && p.peek(1) == '\n':
			p.i += 2 // a line continued: no word of its own
		case c == '\n':
			p.i++
			flush(false)
			listStart = len(out)
			p.readHeredocs()
		case c == '#':
			// A word that begins with # begins a comment, wherever on the line
			// (a # inside a word is read by word, and never comes here).
			for p.i < len(p.s) && p.s[p.i] != '\n' {
				p.i++
			}
		case c == ';':
			p.i++
			flush(false)
			listStart = len(out)
		case c == '&' && p.peek(1) == '&':
			p.i += 2
			and = flush(false)
		case c == '&' && p.peek(1) == '>':
			p.i++
			p.redirect(cur)
		case c == '&':
			p.i++
			flush(false)
			for _, x := range out[listStart:] {
				x.Detached = true
			}
			listStart = len(out)
		case c == '|':
			p.i++
			switch p.peek(0) {
			case '|':
				p.i++
				flush(false)
			case '&':
				p.i++
				flush(true)
			default:
				flush(true)
			}
		case c == '(':
			p.i++
			flush(false)
			groups = append(groups, listStart)
			outer = append(outer, grp)
			if grp == nil || grp.depth < shellMaxGroups {
				d := 0
				if grp != nil {
					d = grp.depth
				}
				grp = &ShellGroup{Parent: grp, depth: d + 1}
			}
			listStart = len(out)
		case c == ')':
			p.i++
			flush(false)
			if n := len(groups); n > 0 {
				listStart, groups = groups[n-1], groups[:n-1]
				grp, outer = outer[n-1], outer[:n-1]
			} else if end == ')' {
				return out
			}
		case c == '`' && end == '`':
			p.i++
			flush(false)
			return out
		case (c == '<' || c == '>') && p.peek(1) != '(':
			p.redirect(cur)
		default:
			w, quoted := p.word(cur)
			if !quoted && IsDigits(w) && (p.peek(0) == '<' || p.peek(0) == '>') {
				continue
			}
			if len(cur.Words) == 0 && !quoted && shellKeywords[w] {
				continue
			}
			cur.Words = append(cur.Words, w)
			cur.Open = append(cur.Open, p.open)
		}
	}
	flush(false)
	return out
}

// redirect reads a redirection at p.i (on its < or >) into cur.
func (p *shellParser) redirect(cur *ShellCmd) {
	c := p.s[p.i]
	p.i++
	amp := false
	switch {
	case c == '>' && p.peek(0) == '>':
		p.i++
	case c == '>' && p.peek(0) == '|':
		p.i++
	case c == '>' && p.peek(0) == '&':
		p.i++
		amp = true
	case c == '<' && p.peek(0) == '<':
		p.i++
		if p.peek(0) == '<' {
			p.i++
			p.skipSpaces()
			p.word(cur)
			return
		}
		strip := p.peek(0) == '-'
		if strip {
			p.i++
		}
		p.skipSpaces()
		d, _ := p.word(cur)
		p.pending = append(p.pending, shellHeredoc{cmd: cur, delim: d, strip: strip})
		return
	case c == '<' && p.peek(0) == '&':
		p.i++
		p.skipSpaces()
		p.word(cur)
		return
	}
	p.skipSpaces()
	w, _ := p.word(cur)
	if c == '<' && w != "" {
		cur.Ins = append(cur.Ins, w)
	}
	if c == '<' || w == "" || (amp && (IsDigits(w) || w == "-")) {
		return
	}
	cur.Redirs = append(cur.Redirs, w)
}

func (p *shellParser) skipSpaces() {
	for p.i < len(p.s) {
		switch {
		case p.s[p.i] == ' ' || p.s[p.i] == '\t':
			p.i++
		case p.s[p.i] == '\\' && p.peek(1) == '\n':
			p.i += 2 // > \<newline> FILE
		default:
			return
		}
	}
}

// readHeredocs takes the bodies of the heredocs started on the line just
// ended: lines up to the one that is the delimiter.
func (p *shellParser) readHeredocs() {
	for _, h := range p.pending {
		var b strings.Builder
		for p.i < len(p.s) {
			line := p.s[p.i:]
			if j := strings.IndexByte(line, '\n'); j >= 0 {
				line = line[:j]
				p.i += j + 1
			} else {
				p.i = len(p.s)
			}
			t := line
			if h.strip {
				t = strings.TrimLeft(line, "\t")
			}
			if t == h.delim {
				break
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
		h.cmd.Heredoc += b.String()
	}
	p.pending = nil
}

// ShellSubPlaceholder stands for a substitution in a word.
const ShellSubPlaceholder = "$(…)"

// word reads one word, unquoted; quoted says any of it was quoted or escaped.
// The code a substitution in it runs is read into cur.Subs. p.open says the
// shell may make other words of it, which the guard doesn't see: an
// unquoted $ or substitution (split into words), or a glob or brace at its
// start ({-d,x=1} is -d x=1; ?d can be a file named -d).
func (p *shellParser) word(cur *ShellCmd) (string, bool) {
	var b strings.Builder
	quoted := false
	open := false
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '\\':
			if p.peek(1) == '\n' {
				p.i += 2 // a continued line quotes nothing
			} else if p.i+1 < len(p.s) {
				quoted = true
				b.WriteByte(p.s[p.i+1])
				p.i += 2
			} else {
				quoted = true
				p.i++
			}
		case c == '\'':
			quoted = true
			rest := p.s[p.i+1:]
			if j := strings.IndexByte(rest, '\''); j >= 0 {
				b.WriteString(rest[:j])
				p.i += j + 2
			} else {
				b.WriteString(rest)
				p.i = len(p.s)
			}
		case c == '"':
			quoted = true
			p.i++
			p.dquote(&b, cur)
		case c == '$' && p.peek(1) == '(':
			p.i += 2
			cur.Subs = append(cur.Subs, p.list(')'))
			b.WriteString(ShellSubPlaceholder)
			open = true
		case c == '$' && p.peek(1) == '\'':
			p.i++
		case c == '`' && p.end != '`':
			p.i++
			cur.Subs = append(cur.Subs, p.list('`'))
			b.WriteString(ShellSubPlaceholder)
			open = true
		case (c == '<' || c == '>') && p.peek(1) == '(' && b.Len() == 0:
			p.i += 2
			cur.Subs = append(cur.Subs, p.list(')'))
			b.WriteString(ShellSubPlaceholder)
		case isShellMeta(c) || (c == '`' && p.end == '`'):
			p.open = open
			return b.String(), quoted
		default:
			if c == '$' || b.Len() == 0 && (strings.IndexByte("{*?[", c) >= 0 || strings.IndexByte("@!+", c) >= 0 && p.peek(1) == '(') {
				open = true
			}
			b.WriteByte(c)
			p.i++
		}
	}
	p.open = open
	return b.String(), quoted
}

// dquote reads the inside of "..." (p.i is past the opening quote).
func (p *shellParser) dquote(b *strings.Builder, cur *ShellCmd) {
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return
		case c == '\\' && p.i+1 < len(p.s):
			n := p.s[p.i+1]
			if n == '"' || n == '\\' || n == '$' || n == '`' {
				b.WriteByte(n)
			} else if n != '\n' {
				b.WriteByte(c)
				b.WriteByte(n)
			}
			p.i += 2
		case c == '$' && p.peek(1) == '(':
			p.i += 2
			cur.Subs = append(cur.Subs, p.list(')'))
			b.WriteString(ShellSubPlaceholder)
		case c == '`' && p.end != '`':
			p.i++
			cur.Subs = append(cur.Subs, p.list('`'))
			b.WriteString(ShellSubPlaceholder)
		default:
			b.WriteByte(c)
			p.i++
		}
	}
}
