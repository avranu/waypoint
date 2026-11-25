package codeindex

import (
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

var (
	shFunc2RE = lazyre.New(`^\s*(?:function\s+([A-Za-z_][\w:.-]*)|([A-Za-z_][\w:.-]*)\s*\(\s*\))\s*\{?`)
	shSrcRE   = lazyre.New(`^\s*(?:source|\.)\s+["']?([^\s"';]+)`)
)

// scanShell finds function definitions. A function ends at the first
// closing brace in column 0 (or at the indentation of its header).
func scanShell(src []byte) Parsed {
	lines := splitLines(src)
	var out Parsed
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if m := shSrcRE.FindStringSubmatch(l); m != nil {
			out.Imports = append(out.Imports, m[1])
		}
		m := shFunc2RE.FindStringSubmatch(l)
		if m == nil || strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if !strings.Contains(l, "{") && !strings.Contains(l, "function") && (i+1 >= len(lines) || !strings.HasPrefix(strings.TrimSpace(lines[i+1]), "{")) {
			continue
		}
		indent := len(l) - len(strings.TrimLeft(l, " \t"))
		end := i
		if !strings.Contains(l, "{") || !strings.Contains(l, "}") {
			for j := i + 1; j < len(lines); j++ {
				t := strings.TrimRight(lines[j], " \t")
				if strings.HasPrefix(t, "}") && len(lines[j])-len(strings.TrimLeft(lines[j], " \t")) <= indent {
					end = j
					break
				}
				end = j
			}
		}
		doc := ""
		start := i
		if i > 0 && strings.HasPrefix(strings.TrimSpace(lines[i-1]), "#") {
			doc = docLine(lines[i-1])
			start = i - 1
			for start > 0 && strings.HasPrefix(strings.TrimSpace(lines[start-1]), "#") && !strings.HasPrefix(lines[start-1], "#!") {
				start--
				doc = docLine(lines[start])
			}
		}
		out.Symbols = append(out.Symbols, Symbol{Name: name, Kind: "func", Sig: collapse(l, 120), Doc: doc, Start: start + 1, End: end + 1, Exported: true})
		i = end
	}
	return out
}

var sqlCreateRE = lazyre.New(`(?i)^\s*(create|alter|drop)\s+(?:or\s+replace\s+)?(?:temp(?:orary)?\s+|unique\s+|materialized\s+|virtual\s+)*(table|view|index|function|procedure|trigger|type|sequence|schema|extension|policy|domain)\s+(?:if\s+(?:not\s+)?exists\s+)?([\w."` + "`" + `\[\]]+)`)

// scanSQL finds DDL statements; a statement runs to its semicolon.
func scanSQL(src []byte) Parsed {
	lines := splitLines(src)
	var out Parsed
	inBlock := false
	dollar := false
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		t := strings.TrimSpace(l)
		if inBlock {
			if strings.Contains(t, "*/") {
				inBlock = false
			}
			continue
		}
		if strings.HasPrefix(t, "/*") && !strings.Contains(t, "*/") {
			inBlock = true
			continue
		}
		if strings.HasPrefix(t, "--") {
			continue
		}
		m := sqlCreateRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		name := strings.Trim(m[3], "\"`[]")
		verb := strings.ToLower(m[1])
		kind := strings.ToLower(m[2])
		end := i
		dollar = false
		for j := i; j < len(lines); j++ {
			end = j
			code := lines[j]
			if k := strings.Index(code, "--"); k >= 0 {
				code = code[:k]
			}
			if strings.Count(code, "$$")%2 == 1 {
				dollar = !dollar
			}
			if !dollar && strings.HasSuffix(strings.TrimSpace(code), ";") {
				break
			}
		}
		sym := Symbol{Name: name, Kind: kind, Sig: collapse(l, 140), Start: i + 1, End: end + 1, Exported: true}
		if verb != "create" {
			sym.Kind = verb + " " + kind
		}
		if i > 0 && strings.HasPrefix(strings.TrimSpace(lines[i-1]), "--") {
			sym.Doc = docLine(lines[i-1])
		}
		out.Symbols = append(out.Symbols, sym)
		i = end
	}
	return out
}

var mdHeadRE = lazyre.New(`^(#{1,6})\s+(.+?)\s*#*\s*$`)

// scanMarkdown makes a symbol of every heading; a section runs to the next
// heading of the same or a higher level. Fenced code is skipped.
func scanMarkdown(src []byte) Parsed {
	lines := splitLines(src)
	var out Parsed
	type open struct{ level, idx int }
	var stack []open
	fence := ""
	i := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i = 1; i < len(lines) && strings.TrimSpace(lines[i]) != "---"; i++ {
		}
		i++
	}
	for ; i < len(lines); i++ {
		l := lines[i]
		t := strings.TrimSpace(l)
		if fence != "" {
			if strings.HasPrefix(t, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fence = t[:3]
			continue
		}
		m := mdHeadRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		level := len(m[1])
		for len(stack) > 0 && stack[len(stack)-1].level >= level {
			out.Symbols[stack[len(stack)-1].idx].End = i
			stack = stack[:len(stack)-1]
		}
		sym := Symbol{Name: collapse(m[2], 100), Kind: "heading", Sig: strings.Repeat("#", level) + " " + collapse(m[2], 100), Start: i + 1, End: len(lines), Exported: true}
		if len(stack) > 0 {
			sym.Parent = out.Symbols[stack[len(stack)-1].idx].Name
		}
		out.Symbols = append(out.Symbols, sym)
		stack = append(stack, open{level, len(out.Symbols) - 1})
	}
	for k := range out.Symbols {
		e := out.Symbols[k].End
		for e > out.Symbols[k].Start && strings.TrimSpace(lines[e-1]) == "" {
			e--
		}
		out.Symbols[k].End = e
	}
	return out
}

var (
	yamlKeyRE  = lazyre.New(`^([A-Za-z_][\w.\-/]*|"[^"]+"|'[^']+'):(?:\s|$)`)
	yamlKey2RE = lazyre.New(`^(\s+)([A-Za-z_][\w.\-/]*):(?:\s|$)`)
	yamlKindRE = lazyre.New(`^kind:\s*([A-Za-z]\w*)`)
	yamlNameRE = lazyre.New(`^\s+name:\s*["']?([^\s"'#]+)`)
)

// yamlNested lists top-level keys whose children are worth listing too
// (workflow jobs, compose services).
var yamlNested = map[string]bool{"jobs": true, "services": true, "stages": true, "volumes": false}

// scanYAML lists top-level keys, the children of jobs/services, and for
// Kubernetes-style documents a "Kind name" resource symbol.
func scanYAML(src []byte) Parsed {
	lines := splitLines(src)
	var out Parsed
	var top []int
	docStart := 0
	kind, name := "", ""
	closeDoc := func(end int) {
		if kind != "" && name != "" {
			out.Symbols = append(out.Symbols, Symbol{Name: name, Kind: strings.ToLower(kind), Sig: kind + " " + name, Start: docStart + 1, End: end, Exported: true})
		}
		kind, name = "", ""
	}
	lastChild := -1
	closeKey := func(end int) {
		if n := len(top); n > 0 {
			out.Symbols[top[n-1]].End = end
		}
		if lastChild >= 0 {
			out.Symbols[lastChild].End = end
			lastChild = -1
		}
	}
	lastContent := 0
	parentKey := ""
	childIndent := -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if l == "---" || strings.HasPrefix(l, "--- ") {
			closeKey(lastContent)
			closeDoc(lastContent)
			docStart = i + 1
			top = top[:0]
			parentKey, childIndent = "", -1
			continue
		}
		if l[0] != ' ' && l[0] != '\t' && l[0] != '-' {
			if m := yamlKeyRE.FindStringSubmatch(l); m != nil {
				closeKey(lastContent)
				key := strings.Trim(m[1], `"'`)
				out.Symbols = append(out.Symbols, Symbol{Name: key, Kind: "key", Sig: collapse(l, 100), Start: i + 1, End: i + 1, Exported: true})
				top = append(top, len(out.Symbols)-1)
				parentKey, childIndent = key, -1
				if km := yamlKindRE.FindStringSubmatch(l); km != nil {
					kind = km[1]
				}
			}
		} else if yamlNested[parentKey] || parentKey == "metadata" {
			if m := yamlKey2RE.FindStringSubmatch(l); m != nil {
				if childIndent < 0 {
					childIndent = len(m[1])
				}
				if len(m[1]) == childIndent && yamlNested[parentKey] {
					if lastChild >= 0 {
						out.Symbols[lastChild].End = i
					}
					out.Symbols = append(out.Symbols, Symbol{Name: m[2], Kind: "key", Parent: parentKey, Sig: parentKey + "." + m[2], Start: i + 1, End: i + 1, Exported: true})
					lastChild = len(out.Symbols) - 1
				}
			}
			if parentKey == "metadata" && name == "" {
				if m := yamlNameRE.FindStringSubmatch(l); m != nil {
					name = m[1]
				}
			}
		}
		lastContent = i + 1
	}
	closeKey(lastContent)
	closeDoc(lastContent)
	return out
}

var (
	htmlBlockRE   = lazyre.New(`\{%-?\s*block\s+(\w+)`)
	htmlExtendsRE = lazyre.New(`\{%-?\s*(?:extends|include)\s+["']([^"']+)["']`)
	htmlTitleRE   = lazyre.New(`(?i)<title>([^<]+)</title>`)
)

// scanHTML finds Django/Jinja blocks, the templates a file extends or
// includes, and the page title.
func scanHTML(src []byte) Parsed {
	lines := splitLines(src)
	var out Parsed
	for i, l := range lines {
		if m := htmlBlockRE.FindStringSubmatch(l); m != nil {
			end := i
			for j := i; j < len(lines) && j < i+400; j++ {
				if strings.Contains(lines[j], "endblock") {
					end = j
					break
				}
			}
			out.Symbols = append(out.Symbols, Symbol{Name: m[1], Kind: "block", Sig: collapse(l, 100), Start: i + 1, End: end + 1, Exported: true})
		}
		for _, m := range htmlExtendsRE.FindAllStringSubmatch(l, -1) {
			out.Imports = append(out.Imports, m[1])
		}
		if len(out.Symbols) == 0 || out.Symbols[0].Kind != "title" {
			if m := htmlTitleRE.FindStringSubmatch(l); m != nil && i < 80 {
				out.Symbols = append([]Symbol{{Name: collapse(m[1], 80), Kind: "title", Start: i + 1, End: i + 1, Exported: true}}, out.Symbols...)
			}
		}
	}
	return out
}
