// Package codeindex is the per-checkout code index behind `wp code` (ADR3):
// files, symbols with line ranges, import edges and git signals in a small
// SQLite database under the checkout's git directory. It needs no server, no
// daemon and no model; it updates itself lazily before a query.
//
// The scanners here are outline scanners: Go is parsed exactly, the other
// languages are read line by line, well enough for outlines and line ranges.
// Callers that need the truth (wp code show) read the real lines from disk.
package codeindex

import (
	"path"
	"strings"
)

// Symbol is one named thing in a file: a function, type, heading, key.
type Symbol struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`             // func, method, type, class, const, var, heading, key, table, ...
	Parent   string `json:"parent,omitempty"` // enclosing type, class or heading
	Sig      string `json:"sig,omitempty"`    // declaration header, whitespace collapsed
	Doc      string `json:"doc,omitempty"`    // first line of the doc comment
	Start    int    `json:"start"`            // 1-based, inclusive; includes doc comments and decorators
	Decl     int    `json:"decl"`             // the line the declaration itself is on
	End      int    `json:"end"`
	Exported bool   `json:"exported,omitempty"`
}

// Qualified is Parent.Name, or Name for a top-level symbol.
func (s Symbol) Qualified() string {
	if s.Parent == "" {
		return s.Name
	}
	return s.Parent + "." + s.Name
}

// Parsed is what a scanner finds in one file.
type Parsed struct {
	Symbols []Symbol
	Imports []string // import specs as written (import paths, module names)
}

// Language names a file's language from its path, "" for files we skip and
// "text" for plain text we only full-text index.
func Language(p string) string {
	switch strings.ToLower(path.Base(p)) {
	case "dockerfile", "makefile", "gnumakefile", "readme", "license", "codeowners":
		return "text"
	}
	switch strings.ToLower(path.Ext(p)) {
	case ".go":
		return "go"
	case ".py", ".pyi":
		return "python"
	case ".ts", ".mts", ".cts":
		return "typescript"
	case ".tsx":
		return "tsx"
	case ".js", ".mjs", ".cjs", ".jsx":
		return "javascript"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".kt", ".kts":
		return "kotlin"
	case ".sh", ".bash", ".zsh":
		return "shell"
	case ".sql":
		return "sql"
	case ".md", ".markdown":
		return "markdown"
	case ".yaml", ".yml":
		return "yaml"
	case ".html", ".htm", ".tmpl", ".jinja", ".j2":
		return "html"
	case ".scss", ".css", ".sass", ".less":
		return "css"
	case ".toml", ".json", ".txt", ".rst", ".ini", ".cfg", ".conf", ".service", ".timer", ".env", ".xml", ".proto", ".graphql", ".c", ".h", ".cpp", ".hpp", ".cs", ".rb", ".php", ".swift", ".lua", ".tf", ".mod", ".sum", ".gradle", ".properties":
		return "text"
	}
	return ""
}

// Scan reads one file's symbols and imports. It never panics: a scanner
// that trips on odd input returns nothing for that file.
func Scan(lang string, src []byte) (p Parsed) {
	defer func() {
		if r := recover(); r != nil {
			p = Parsed{}
		}
	}()
	defer func() {
		for i := range p.Symbols {
			if p.Symbols[i].Decl == 0 {
				p.Symbols[i].Decl = p.Symbols[i].Start
			}
		}
	}()
	switch lang {
	case "go":
		return scanGo(src)
	case "python":
		return scanPython(src)
	case "typescript", "tsx", "javascript", "rust", "java", "kotlin":
		return scanBrace(lang, src)
	case "shell":
		return scanShell(src)
	case "sql":
		return scanSQL(src)
	case "markdown":
		return scanMarkdown(src)
	case "yaml":
		return scanYAML(src)
	case "html":
		return scanHTML(src)
	}
	return Parsed{}
}

// splitLines splits on \n, dropping a trailing \r and the empty piece after
// a final newline, so index i is line i+1.
func splitLines(src []byte) []string {
	s := string(src)
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// collapse squeezes whitespace runs to single spaces and caps the length.
func collapse(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > limit {
		cut := limit
		for cut > 0 && s[cut]&0xC0 == 0x80 {
			cut--
		}
		s = strings.TrimSpace(s[:cut]) + "…"
	}
	return s
}

func isExportedName(n string) bool {
	return n != "" && n[0] >= 'A' && n[0] <= 'Z'
}

// docLine is the first non-empty line of a comment block, markers removed.
func docLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "/*#!-\"'"))
		l = strings.TrimSpace(strings.TrimSuffix(l, "*/"))
		if l != "" {
			return collapse(l, 140)
		}
	}
	return ""
}
