package transcript

import "strings"

// The shell reader below is a copy of cmd/wp/guard_shell.go (the guard still
// uses that one); a change to how a command line is read belongs in both.

// shellCmd is one simple command.
type shellCmd struct {
	words   []string
	open    []bool
	subs    [][]*shellCmd
	redirs  []string
	heredoc string
	pipeIn  bool
	andIn   bool

	detached bool
}

type shellHeredoc struct {
	cmd   *shellCmd
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
}

const shellMaxNest = 12

// parseShell reads a command line into simple commands.
func parseShell(s string) []*shellCmd {
	p := &shellParser{s: s}
	return p.list(0)
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

func isDigits(s string) bool {
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
func (p *shellParser) list(end byte) []*shellCmd {
	prevEnd := p.end
	p.end = end
	defer func() { p.end = prevEnd }()
	p.nest++
	defer func() { p.nest-- }()
	var out []*shellCmd
	cur := &shellCmd{}
	pipe := false
	and := false
	listStart := 0
	var groups []int
	flush := func(nextPipe bool) bool {
		added := false
		if len(cur.words) > 0 || len(cur.redirs) > 0 || len(cur.subs) > 0 {
			cur.pipeIn, cur.andIn = pipe, and
			out = append(out, cur)
			pipe, and, added = nextPipe, false, true
		} else if !nextPipe {
			pipe = false
		}
		cur = &shellCmd{}
		return added
	}
	if p.nest > shellMaxNest {
		p.i = len(p.s)
		return nil
	}
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			p.i++
		case c == '\n':
			p.i++
			flush(false)
			listStart = len(out)
			p.readHeredocs()
		case c == '#' && len(cur.words) == 0:
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
				x.detached = true
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
			listStart = len(out)
		case c == ')':
			p.i++
			flush(false)
			if n := len(groups); n > 0 {
				listStart, groups = groups[n-1], groups[:n-1]
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
			if !quoted && isDigits(w) && (p.peek(0) == '<' || p.peek(0) == '>') {
				continue
			}
			if len(cur.words) == 0 && !quoted && shellKeywords[w] {
				continue
			}
			cur.words = append(cur.words, w)
			cur.open = append(cur.open, p.open)
		}
	}
	flush(false)
	return out
}

// redirect reads a redirection at p.i (on its < or >) into cur.
func (p *shellParser) redirect(cur *shellCmd) {
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
	if c == '<' || w == "" || (amp && (isDigits(w) || w == "-")) {
		return
	}
	cur.redirs = append(cur.redirs, w)
}

func (p *shellParser) skipSpaces() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
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
		h.cmd.heredoc += b.String()
	}
	p.pending = nil
}

// subPlaceholder stands for a substitution in a word.
const subPlaceholder = "$(…)"

// word reads one word, unquoted; quoted says any of it was quoted or escaped.
// The code a substitution in it runs is read into cur.subs. p.open says the
// shell may make other words of it, which the guard doesn't see: an
// unquoted $ or substitution (split into words), or a glob or brace at its
// start ({-d,x=1} is -d x=1; ?d can be a file named -d).
func (p *shellParser) word(cur *shellCmd) (string, bool) {
	var b strings.Builder
	quoted := false
	open := false
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '\\':
			quoted = true
			if p.peek(1) == '\n' {
				p.i += 2
			} else if p.i+1 < len(p.s) {
				b.WriteByte(p.s[p.i+1])
				p.i += 2
			} else {
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
			cur.subs = append(cur.subs, p.list(')'))
			b.WriteString(subPlaceholder)
			open = true
		case c == '$' && p.peek(1) == '\'':
			p.i++
		case c == '`' && p.end != '`':
			p.i++
			cur.subs = append(cur.subs, p.list('`'))
			b.WriteString(subPlaceholder)
			open = true
		case (c == '<' || c == '>') && p.peek(1) == '(' && b.Len() == 0:
			p.i += 2
			cur.subs = append(cur.subs, p.list(')'))
			b.WriteString(subPlaceholder)
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
func (p *shellParser) dquote(b *strings.Builder, cur *shellCmd) {
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
			cur.subs = append(cur.subs, p.list(')'))
			b.WriteString(subPlaceholder)
		case c == '`' && p.end != '`':
			p.i++
			cur.subs = append(cur.subs, p.list('`'))
			b.WriteString(subPlaceholder)
		default:
			b.WriteByte(c)
			p.i++
		}
	}
}
