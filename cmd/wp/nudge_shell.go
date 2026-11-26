package main

import "strings"

// shellSpan is the length of the simple command s starts with: up to the first
// ; & | < > comment or newline outside quotes and parentheses (a redirect's
// file descriptor number left out), or a parenthesis it didn't open. outer is
// the quote s sits inside, which ends it too, or 0. ok is false, and the
// length that of s, when the end can't be told: a quote or parenthesis is left
// open, or the outer quote is escaped inside.
func shellSpan(s string, outer byte) (n int, ok bool) {
	var quote byte
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case outer != 0 && c == outer:
			// 'a '\''b'\'' c' goes on after the quote that seems to end it.
			if quote != 0 || depth != 0 || strings.HasPrefix(s[i+1:], `\'`) {
				return len(s), false
			}
			return i, true
		case c == '\\' && quote != '\'':
			if outer == '"' && i+1 < len(s) && s[i+1] == '"' {
				return len(s), false
			}
			i++
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			if depth == 0 {
				return i, true
			}
			depth--
		case depth > 0:
		case c == '<' || c == '>':
			j := i
			for j > 0 && s[j-1] >= '0' && s[j-1] <= '9' {
				j--
			}
			if j > 0 && (s[j-1] == ' ' || s[j-1] == '\t') {
				return j, true
			}
			return i, true
		case c == ';' || c == '&' || c == '|' || c == '\n',
			c == '#' && i > 0 && (s[i-1] == ' ' || s[i-1] == '\t'):
			return i, true
		}
	}
	if quote != 0 || depth != 0 {
		return len(s), false
	}
	return len(s), true
}

// openQuote is the quote left open at the end of s, or 0.
func openQuote(s string) byte {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && quote != '\'':
			i++
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '#' && (i == 0 || strings.IndexByte(" \t\n;", s[i-1]) >= 0):
			for i+1 < len(s) && s[i+1] != '\n' {
				i++
			}
		}
	}
	return quote
}
