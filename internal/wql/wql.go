// Package wql parses Waypoint's issue query language.
//
// A query is a sequence of space separated terms:
//
//	assignee:me is:open priority:>=high "login page" -label:wontfix order:-updated
//
// Terms are `key:value`, optionally prefixed with `-` to negate, with an
// optional comparison after the colon (`>`, `<`, `>=`, `<=`, `!=`). A value
// may list alternatives separated by commas (`type:bug,task`) and may be
// quoted (`status:"In Review"`). Anything that is not a key:value term is free
// text, matched against titles, descriptions and comments.
//
// This package only parses; the meaning of each key lives with the code that
// turns a Query into SQL.
package wql

import (
	"fmt"
	"strings"
	"unicode"
)

// Term is one key:value filter.
type Term struct {
	Key    string   // lower-cased key
	Op     string   // "=", "!=", ">", ">=", "<", "<="
	Values []string // alternatives, ORed together
	Negate bool
}

// Order is one sort key; Desc reverses it.
type Order struct {
	Field string
	Desc  bool
}

// Query is a parsed query.
type Query struct {
	Terms []Term
	Text  []string // free text words and quoted phrases
	Order []Order
}

// Empty reports whether the query has no filters and no text.
func (q Query) Empty() bool { return len(q.Terms) == 0 && len(q.Text) == 0 }

// Has reports whether the query contains a term with key.
func (q Query) Has(key string) bool {
	for _, t := range q.Terms {
		if t.Key == key {
			return true
		}
	}
	return false
}

// Error is a parse error at a byte offset.
type Error struct {
	Pos int
	Msg string
}

func (e *Error) Error() string { return fmt.Sprintf("query: %s at position %d", e.Msg, e.Pos+1) }

// Parse parses s. It is forgiving: an unterminated quote runs to the end of
// the string rather than failing, since queries are typed interactively.
func Parse(s string) (Query, error) {
	var q Query
	toks, err := tokenize(s)
	if err != nil {
		return q, err
	}
	for _, tk := range toks {
		if tk.key == "" {
			if tk.value != "" {
				q.Text = append(q.Text, tk.value)
			}
			continue
		}
		key := strings.ToLower(tk.key)
		if key == "order" || key == "sort" {
			for _, v := range splitValues(tk.value) {
				o := Order{Field: strings.ToLower(v)}
				if strings.HasPrefix(o.Field, "-") {
					o.Desc, o.Field = true, o.Field[1:]
				} else if strings.HasPrefix(o.Field, "+") {
					o.Field = o.Field[1:]
				}
				if o.Field != "" {
					q.Order = append(q.Order, o)
				}
			}
			continue
		}
		op, rest := splitOp(tk.op)
		if rest != "" {
			return q, &Error{Pos: tk.pos, Msg: fmt.Sprintf("%q isn't an operator; use >, <, >=, <= or !=", tk.op)}
		}
		vals := tk.values
		if len(vals) == 0 {
			return q, &Error{Pos: tk.pos, Msg: fmt.Sprintf("%q needs a value", key)}
		}
		q.Terms = append(q.Terms, Term{Key: key, Op: op, Values: vals, Negate: tk.negate})
	}
	return q, nil
}

func splitOp(v string) (string, string) {
	for _, op := range []string{">=", "<=", "!=", ">", "<", "="} {
		if strings.HasPrefix(v, op) {
			return op, v[len(op):]
		}
	}
	return "=", v
}

func splitValues(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

type token struct {
	pos    int
	key    string
	value  string
	negate bool
	quoted bool
	op     string
	values []string
}

func isKeyRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func tokenize(s string) ([]token, error) {
	var out []token
	rs := []rune(s)
	i := 0
	for i < len(rs) {
		for i < len(rs) && unicode.IsSpace(rs[i]) {
			i++
		}
		if i >= len(rs) {
			break
		}
		start := i
		tk := token{pos: start}
		if rs[i] == '-' && i+1 < len(rs) && !unicode.IsSpace(rs[i+1]) {
			tk.negate = true
			i++
		}
		if rs[i] == '"' {
			v, n := readQuoted(rs, i)
			i = n
			if tk.negate {
				v = "-" + v
			}
			tk.value = v
			out = append(out, tk)
			continue
		}
		j := i
		for j < len(rs) && isKeyRune(rs[j]) {
			j++
		}
		if j < len(rs) && j > i && rs[j] == ':' && (j+1 >= len(rs) || rs[j+1] != '/') {
			tk.key = string(rs[i:j])
			j++
			k := j
			for k < len(rs) && strings.ContainsRune("<>=!", rs[k]) {
				k++
			}
			op := string(rs[j:k])
			for {
				if k < len(rs) && rs[k] == '"' {
					v, n := readQuoted(rs, k)
					tk.quoted = true
					if v != "" {
						tk.values = append(tk.values, v)
					}
					k = n
				} else {
					m := k
					for m < len(rs) && !unicode.IsSpace(rs[m]) && rs[m] != ',' {
						m++
					}
					if v := strings.TrimSpace(string(rs[k:m])); v != "" {
						tk.values = append(tk.values, v)
					}
					k = m
				}
				if k < len(rs) && rs[k] == ',' {
					k++
					continue
				}
				break
			}
			tk.op, tk.value = op, op+strings.Join(tk.values, ",")
			i = k
			out = append(out, tk)
			continue
		}
		for j < len(rs) && !unicode.IsSpace(rs[j]) {
			j++
		}
		w := string(rs[i:j])
		if tk.negate {
			w = "-" + w
		}
		tk.value = w
		i = j
		out = append(out, tk)
	}
	return out, nil
}

// readQuoted reads a "..." string starting at rs[i] == '"'. Backslash escapes
// the next rune. Returns the unquoted value and the index after the closing quote.
func readQuoted(rs []rune, i int) (string, int) {
	var b strings.Builder
	i++
	for i < len(rs) {
		r := rs[i]
		if r == '\\' && i+1 < len(rs) {
			b.WriteRune(rs[i+1])
			i += 2
			continue
		}
		if r == '"' {
			return b.String(), i + 1
		}
		b.WriteRune(r)
		i++
	}
	return b.String(), i
}

// Quote renders v so that Parse reads it back as one value.
func Quote(v string) string {
	if v == "" || strings.ContainsAny(v, " \t\",:") {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `"`, `\"`) + `"`
	}
	return v
}
