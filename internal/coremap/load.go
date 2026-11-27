package coremap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Reference classes.
const (
	ClassType      = "type"
	ClassField     = "field"
	ClassFunc      = "func"
	ClassSvcMethod = "service-method"
	ClassMethod    = "method"
	ClassVar       = "var" // variables and constants
)

// classOrder is the order classes are listed in.
var classOrder = []string{ClassType, ClassField, ClassFunc, ClassSvcMethod, ClassMethod, ClassVar}

// Ref is one use of an identifier in one file that resolves to a package-level
// object, method or struct field declared in another file of the package.
type Ref struct {
	FromGroup string `json:"from"`
	ToGroup   string `json:"to"`
	File      string `json:"file"`
	Line      int    `json:"line"`
	Symbol    string `json:"symbol"`
	DeclFile  string `json:"declFile"`
	DeclLine  int    `json:"declLine"`
	Class     string `json:"class"`
	Call      bool   `json:"call"`
	col       int    // the use's column, so that the order of Refs is total
}

// Loaded is the type-checked package: every cross-group reference plus the
// facts the summary needs.
type Loaded struct {
	Dir        string
	ImportPath string
	// Files are the type-checked (build-constraint-selected, non-test) file names.
	Files []string
	// Lines is the line count per type-checked file.
	Lines map[string]int
	// Top and Subpackages count the Go files of Dir and of each directory below
	// it, by file name (build constraints are not applied).
	Top         PkgSize
	Subpackages []PkgSize
	// Refs holds the references between files of different groups, sorted by
	// file, line and symbol.
	Refs          []Ref
	ServiceFields int
	ServiceMeths  int
	TypeErrors    int
}

// Load type-checks the package in dir (as built for linux/amd64 without cgo,
// whatever the host is; no tests) once and collects its references. Phase
// timings go to log. Type errors are counted, not returned.
func Load(dir string, groups *Groups, log io.Writer) (*Loaded, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	lap := func(what string) {
		fmt.Fprintf(log, "coremap: %-12s %6.1fs\n", what, time.Since(t0).Seconds())
		t0 = time.Now()
	}

	out, err := goList(abs, "-json=ImportPath,GoFiles", ".")
	if err != nil {
		return nil, err
	}
	var info struct {
		ImportPath string
		GoFiles    []string
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	lap("go list")

	top, subs, err := sizes(abs)
	if err != nil {
		return nil, fmt.Errorf("sizes: %w", err)
	}

	eout, err := goList(abs, "-export", "-deps", "-f", "{{.ImportPath}}|{{.Export}}", ".")
	if err != nil {
		return nil, err
	}
	exports := map[string]string{}
	for _, ln := range strings.Split(strings.TrimSpace(string(eout)), "\n") {
		if p, e, ok := strings.Cut(ln, "|"); ok && e != "" {
			exports[p] = e
		}
	}
	lap("export data")

	fset := token.NewFileSet()
	ld := &Loaded{Dir: abs, ImportPath: info.ImportPath, Lines: map[string]int{}, Top: top, Subpackages: subs}
	var files []*ast.File
	for _, f := range info.GoFiles {
		src, err := os.ReadFile(filepath.Join(abs, f)) // #nosec G304 -- a file of the package go list named
		if err != nil {
			return nil, err
		}
		af, err := parser.ParseFile(fset, filepath.Join(abs, f), src, 0)
		if err != nil {
			return nil, err
		}
		files = append(files, af)
		ld.Files = append(ld.Files, f)
		ld.Lines[f] = countLines(src)
	}
	lap("parse")

	imp := importer.ForCompiler(fset, "gc", func(p string) (io.ReadCloser, error) {
		e, ok := exports[p]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", p)
		}
		return os.Open(e) // #nosec G304 -- export data path go list reported
	})
	ti := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: imp, Error: func(e error) {
		ld.TypeErrors++
		if ld.TypeErrors <= 3 {
			_, _ = fmt.Fprintln(log, "coremap: typecheck:", e)
		}
	}}
	pkg, _ := conf.Check(info.ImportPath, fset, files, ti)
	lap("type-check")
	if ld.TypeErrors > 0 {
		fmt.Fprintf(log, "coremap: warning: %d type errors, the map may be incomplete\n", ld.TypeErrors)
	}

	if obj := pkg.Scope().Lookup("Service"); obj != nil {
		if n, ok := obj.Type().(*types.Named); ok {
			ld.ServiceMeths = n.NumMethods()
			if st, ok := n.Underlying().(*types.Struct); ok {
				ld.ServiceFields = st.NumFields()
			}
		}
	}

	owners := fieldOwners(pkg, files)
	callees := calleePositions(files)
	for id, obj := range ti.Uses {
		sym, class, ok := describe(pkg, obj, owners)
		if !ok {
			continue
		}
		rp, dp := fset.Position(id.Pos()), fset.Position(obj.Pos())
		rf, df := filepath.Base(rp.Filename), filepath.Base(dp.Filename)
		fg, tg := groups.Of(rf), groups.Of(df)
		if _, known := ld.Lines[rf]; !known || fg == tg {
			continue
		}
		if _, known := ld.Lines[df]; !known {
			continue
		}
		ld.Refs = append(ld.Refs, Ref{FromGroup: fg, ToGroup: tg, File: rf, Line: rp.Line,
			Symbol: sym, DeclFile: df, DeclLine: dp.Line, Class: class,
			Call: callees[id.Pos()] && class != ClassType, col: rp.Column})
	}
	sort.Slice(ld.Refs, func(i, j int) bool {
		a, b := ld.Refs[i], ld.Refs[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		if a.DeclFile != b.DeclFile {
			return a.DeclFile < b.DeclFile
		}
		if a.DeclLine != b.DeclLine {
			return a.DeclLine < b.DeclLine
		}
		return a.col < b.col
	})
	lap("references")
	return ld, nil
}

func goList(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", append([]string{"list"}, args...)...) // #nosec G204 -- go list with this tool's own fixed flags
	cmd.Dir = dir
	// One fixed target, so that every host maps the same files.
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list in %s: %w: %s", dir, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func countLines(b []byte) int {
	n := bytes.Count(b, []byte{'\n'})
	if len(b) > 0 && b[len(b)-1] != '\n' {
		n++
	}
	return n
}

// fieldOwners names the struct type whose declaration writes each field of the
// package's named struct types, so a field reads as Type.field. A type defined
// from another (type storedPrefs UserPrefs) has the same fields but does not
// write them, so it does not own them.
func fieldOwners(pkg *types.Package, files []*ast.File) map[*types.Var]string {
	specs := map[token.Pos]*ast.TypeSpec{} // by the position of the type's name
	for _, af := range files {
		for _, d := range af.Decls {
			if gd, ok := d.(*ast.GenDecl); ok {
				for _, sp := range gd.Specs {
					if ts, ok := sp.(*ast.TypeSpec); ok {
						specs[ts.Name.Pos()] = ts
					}
				}
			}
		}
	}
	m := map[*types.Var]string{}
	sc := pkg.Scope()
	for _, name := range sc.Names() {
		tn, ok := sc.Lookup(name).(*types.TypeName)
		if !ok {
			continue
		}
		spec := specs[tn.Pos()]
		if st, ok := tn.Type().Underlying().(*types.Struct); ok && spec != nil {
			for i := range st.NumFields() {
				if f := st.Field(i); f.Pos() >= spec.Pos() && f.Pos() < spec.End() {
					m[f] = name
				}
			}
		}
	}
	return m
}

// calleePositions returns the positions of the identifiers that are the
// callee of a call expression (f in f(x), m in x.m(y), f in f[T](x)).
func calleePositions(files []*ast.File) map[token.Pos]bool {
	m := map[token.Pos]bool{}
	for _, af := range files {
		ast.Inspect(af, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn := ce.Fun
			for {
				switch f := fn.(type) {
				case *ast.ParenExpr:
					fn = f.X
					continue
				case *ast.IndexExpr:
					fn = f.X
					continue
				case *ast.IndexListExpr:
					fn = f.X
					continue
				case *ast.Ident:
					m[f.Pos()] = true
				case *ast.SelectorExpr:
					m[f.Sel.Pos()] = true
				}
				return true
			}
		})
	}
	return m
}

// describe names and classes a used object, or reports false when it is not
// a package-level object, method or struct field of pkg.
func describe(pkg *types.Package, obj types.Object, owners map[*types.Var]string) (sym, class string, ok bool) {
	if obj == nil || obj.Pkg() != pkg || !obj.Pos().IsValid() {
		return "", "", false
	}
	switch o := obj.(type) {
	case *types.TypeName:
		if o.Parent() != pkg.Scope() {
			return "", "", false
		}
		return o.Name(), ClassType, true
	case *types.Var:
		if o.IsField() {
			o = o.Origin()
			if owner := owners[o]; owner != "" {
				return owner + "." + o.Name(), ClassField, true
			}
			return o.Name(), ClassField, true
		}
		if o.Parent() != pkg.Scope() {
			return "", "", false
		}
		return o.Name(), ClassVar, true
	case *types.Const:
		if o.Parent() != pkg.Scope() {
			return "", "", false
		}
		return o.Name(), ClassVar, true
	case *types.Func:
		o = o.Origin()
		sig, _ := o.Type().(*types.Signature)
		if sig == nil || sig.Recv() == nil {
			if o.Parent() != pkg.Scope() {
				return "", "", false
			}
			return o.Name(), ClassFunc, true
		}
		rt := sig.Recv().Type()
		if p, isPtr := rt.(*types.Pointer); isPtr {
			rt = p.Elem()
		}
		recv := "?"
		if n, isNamed := rt.(*types.Named); isNamed {
			recv = n.Obj().Name()
		}
		if recv == "Service" {
			return "Service." + o.Name(), ClassSvcMethod, true
		}
		return recv + "." + o.Name(), ClassMethod, true
	}
	return "", "", false
}
