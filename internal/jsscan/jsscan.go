// Package jsscan reads the comments in JavaScript source a line at a time,
// for the tools that look for waivers in e2e scripts: wplint's e2e-wait rule
// and the suppression ledger. Both read the same way, so a waiver one sees
// the other counts.
package jsscan

import "strings"

// StripComments is line without its comments, the text of its trailing //
// comment, and what the next line starts inside of, given what this one
// does: 0 for code, '*' for a block comment, '`' for a template literal.
// Strings and regex literals are kept as code, so a // or quote in one is
// read as theirs; a template's ${…} is read as template text.
func StripComments(line string, open byte) (string, string, byte) {
	var b strings.Builder
	quote := byte(0) // the string or literal the scan is inside: " ' ` or /
	if open == '`' {
		quote = '`'
	}
	inBlock := open == '*'
	inClass := false // inside a regex's [...]
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inBlock:
			if c == '*' && i+1 < len(line) && line[i+1] == '/' {
				inBlock = false
				i++
			}
		case quote != 0:
			b.WriteByte(c)
			switch {
			case c == '\\' && i+1 < len(line):
				b.WriteByte(line[i+1])
				i++
			case quote == '/' && c == '[':
				inClass = true
			case quote == '/' && c == ']':
				inClass = false
			case c == quote && !inClass:
				quote = 0
			}
		case c == '/' && i+1 < len(line) && line[i+1] == '/':
			return b.String(), line[i:], 0
		case c == '/' && i+1 < len(line) && line[i+1] == '*':
			inBlock = true
			i++
		default:
			if c == '"' || c == '\'' || c == '`' || c == '/' && regexStarts(b.String()) {
				quote = c
			}
			b.WriteByte(c)
		}
	}
	switch {
	case inBlock:
		return b.String(), "", '*'
	case quote == '`':
		return b.String(), "", '`'
	}
	return b.String(), "", 0
}

// regexStarts says whether a / after code starts a regex literal rather than
// dividing: it does where a value can't end, at the start of a line, after an
// operator or opening bracket, or after a keyword such as return.
func regexStarts(code string) bool {
	code = strings.TrimRight(code, " \t")
	if code == "" {
		return true
	}
	last := code[len(code)-1]
	if last == ')' || last == ']' || last == '"' || last == '\'' || last == '`' || last == '/' {
		return false
	}
	if !isJSIdent(last) {
		return true
	}
	start := len(code)
	for start > 0 && isJSIdent(code[start-1]) {
		start--
	}
	switch code[start:] {
	case "return", "typeof", "case", "do", "else", "in", "of", "new", "delete", "void", "throw", "yield", "await", "instanceof":
		return true
	}
	return false
}

func isJSIdent(c byte) bool {
	return c == '_' || c == '$' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}
