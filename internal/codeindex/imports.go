package codeindex

import (
	"database/sql"
	"os"
	"path"
	"path/filepath"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

var goModRE = lazyre.New(`(?m)^module\s+(\S+)`)

type goModule struct{ path, dir string }

// resolver maps import specs to files in the checkout.
type resolver struct {
	root     string
	paths    map[string]bool
	goDirs   map[string]bool
	suffixes map[string][]string
	mods     []goModule
	modsDone bool
}

func newResolver(tx *sql.Tx, root string) (*resolver, error) {
	r := &resolver{root: root, paths: map[string]bool{}, goDirs: map[string]bool{"": true}}
	rows, err := tx.Query(`SELECT path FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		r.paths[p] = true
		if strings.HasSuffix(p, ".go") {
			for d := path.Dir(p); d != "." && d != "/" && !r.goDirs[d]; d = path.Dir(d) {
				r.goDirs[d] = true
			}
		}
	}
	return r, rows.Err()
}

func (r *resolver) goModules() []goModule {
	if r.modsDone {
		return r.mods
	}
	r.modsDone = true
	for p := range r.paths {
		if path.Base(p) != "go.mod" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(r.root, filepath.FromSlash(p))) // #nosec G304 -- an indexed file of this checkout
		if err != nil {
			continue
		}
		if m := goModRE.FindSubmatch(b); m != nil {
			dir := path.Dir(p)
			if dir == "." {
				dir = ""
			}
			r.mods = append(r.mods, goModule{string(m[1]), dir})
		}
	}
	return r.mods
}

func (r *resolver) bySuffix(suffix string) string {
	if r.suffixes == nil {
		r.suffixes = map[string][]string{}
		for p := range r.paths {
			switch path.Ext(p) {
			case ".py", ".pyi", ".html", ".htm", ".sh", ".md":
			default:
				continue
			}
			parts := strings.Split(p, "/")
			for i := range parts {
				s := strings.Join(parts[i:], "/")
				r.suffixes[s] = append(r.suffixes[s], p)
			}
		}
	}
	best := ""
	for _, p := range r.suffixes[suffix] {
		if best == "" || len(p) < len(best) || len(p) == len(best) && p < best {
			best = p
		}
	}
	return best
}

var jsExts = []string{"", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".json", "/index.ts", "/index.tsx", "/index.js", "/index.jsx"}

func (r *resolver) tryJS(base string) string {
	for _, e := range jsExts {
		if c := base + e; r.paths[c] {
			return c
		}
	}
	return ""
}

// resolve returns the file (or Go package directory) a spec in file p of
// language lang refers to, "" when it is external or unknown.
func (r *resolver) resolve(p, lang, spec string) string {
	dir := path.Dir(p)
	switch lang {
	case "go":
		best := ""
		for _, m := range r.goModules() {
			rest, ok := "", false
			switch {
			case spec == m.path:
				ok = true
			case strings.HasPrefix(spec, m.path+"/"):
				rest, ok = spec[len(m.path)+1:], true
			}
			if ok && len(m.path) > len(best) {
				best = m.path
				d := path.Join(m.dir, rest)
				if d == "." {
					d = ""
				}
				if r.hasDir(d) {
					return d
				}
			}
		}
	case "typescript", "tsx", "javascript":
		switch {
		case strings.HasPrefix(spec, "."):
			return r.tryJS(path.Join(dir, spec))
		case strings.HasPrefix(spec, "@/") || strings.HasPrefix(spec, "~/"):
			for d := dir; ; d = path.Dir(d) {
				if c := r.tryJS(path.Join(d, "src", spec[2:])); c != "" {
					return c
				}
				if d == "." || d == "/" || d == "" {
					break
				}
			}
		}
	case "python":
		if strings.HasPrefix(spec, ".") {
			n := len(spec) - len(strings.TrimLeft(spec, "."))
			base := dir
			for i := 1; i < n; i++ {
				base = path.Dir(base)
			}
			rest := strings.ReplaceAll(strings.TrimLeft(spec, "."), ".", "/")
			joined := path.Join(base, rest)
			for _, c := range []string{joined + ".py", joined + "/__init__.py"} {
				if r.paths[c] {
					return c
				}
			}
			return ""
		}
		rel := strings.ReplaceAll(spec, ".", "/")
		for _, c := range []string{rel + ".py", rel + "/__init__.py"} {
			if b := r.bySuffix(c); b != "" {
				return b
			}
		}
	case "html":
		if b := r.bySuffix("templates/" + spec); b != "" {
			return b
		}
		return r.bySuffix(spec)
	case "shell":
		c := path.Join(dir, spec)
		if r.paths[c] {
			return c
		}
	}
	return ""
}

func (r *resolver) hasDir(d string) bool { return r.goDirs[d] }

// resolveImports points the unresolved import rows at files in the checkout.
// Rows that stay unresolved (external packages) are tried again on the next
// write, so an importer indexed before its target appeared still gets linked.
func (ix *Index) resolveImports(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT i.rowid, f.path, f.lang, i.spec FROM imports i JOIN files f ON f.id = i.file_id WHERE i.target = ''`)
	if err != nil {
		return err
	}
	type pending struct {
		rowid          int64
		path, lang, sp string
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.rowid, &p.path, &p.lang, &p.sp); err != nil {
			_ = rows.Close()
			return err
		}
		todo = append(todo, p)
	}
	_ = rows.Close()
	if len(todo) == 0 {
		return nil
	}
	r, err := newResolver(tx, ix.Root)
	if err != nil {
		return err
	}
	cache := map[[3]string]string{}
	for _, p := range todo {
		key := [3]string{p.path, p.lang, p.sp}
		if p.lang == "go" || p.lang == "python" && !strings.HasPrefix(p.sp, ".") {
			key[0] = "" // the answer doesn't depend on the importing file
		}
		t, ok := cache[key]
		if !ok {
			t = r.resolve(p.path, p.lang, p.sp)
			cache[key] = t
		}
		if t == "" || t == p.path {
			continue
		}
		if _, err := tx.Exec(`UPDATE imports SET target = ? WHERE rowid = ?`, t, p.rowid); err != nil {
			return err
		}
	}
	return nil
}
