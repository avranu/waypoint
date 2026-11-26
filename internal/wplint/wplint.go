// Package wplint is Waypoint's own static checks: the rules in AGENTS.md and
// REVIEW.md that code can decide, so they cost nothing and run on every push
// instead of waiting for a review model (WP-707).
//
// Analyzers holds the go/analysis rules; CheckRepo holds the ones about files
// rather than Go code (migrations, e2e). cmd/wplint runs both, and `make lint`
// and CI run cmd/wplint. Each rule's doc names the task that motivated it. A
// recurring mistake's check usually belongs here (WP-710).
//
// A finding that is wrong for one line is waived on the line above, or after
// the code on that line (which covers that line only):
//
//	//wplint:ignore RULE why this one is fine
//
// The reason is required; `wp report ci suppressions` lists the waivers. What the
// rules check and why, waivers and merged migrations: WP-ADR136.
package wplint

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"regexp"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Analyzers is every go/analysis rule, in the order cmd/wplint runs them.
var Analyzers = []*analysis.Analyzer{
	APIDB,
	WriteClosure,
	ActorUsed,
	SQLScan,
	SchemaChain,
	ProtectedDiff,
	StatusCreator,
	TestEnv,
	Clock,
	LogCtx,
	BlankErr,
	LogMsg,
	CoreLayers,
}

var ignoreRE = regexp.MustCompile(`^//\s*wplint:ignore\s+([\w-]+)[ \t]*(.*)$`)

// reporter reports a rule's findings, leaving out the waived ones.
type reporter struct {
	pass   *analysis.Pass
	waived map[string]map[int]waiver
}

type waiver int

const (
	thisLine waiver = iota + 1
	nextLine
)

func newReporter(pass *analysis.Pass) *reporter {
	r := &reporter{pass: pass, waived: map[string]map[int]waiver{}}
	for _, f := range pass.Files {
		for _, group := range f.Comments {
			for _, c := range group.List {
				m := ignoreRE.FindStringSubmatch(c.Text)
				if m == nil || m[1] != pass.Analyzer.Name {
					continue
				}
				if strings.TrimSpace(m[2]) == "" {
					pass.Reportf(c.Pos(), "wplint:ignore %s needs a reason after the rule's name", m[1])
					continue
				}
				pos := pass.Fset.Position(c.Pos())
				lines := r.waived[pos.Filename]
				if lines == nil {
					lines = map[int]waiver{}
					r.waived[pos.Filename] = lines
				}
				lines[pos.Line] = max(lines[pos.Line], reach(pass, c))
			}
		}
	}
	return r
}

func (r *reporter) report(pos token.Pos, format string, args ...any) {
	p := r.pass.Fset.Position(pos)
	if lines := r.waived[p.Filename]; lines[p.Line] != 0 || lines[p.Line-1] == nextLine {
		return
	}
	r.pass.Report(analysis.Diagnostic{Pos: pos, Message: fmt.Sprintf(format, args...)})
}

// reach is how far a directive comment reaches: the next line only when
// nothing but space comes before it on its own line.
func reach(pass *analysis.Pass, c *ast.Comment) waiver {
	tf := pass.Fset.File(c.Pos())
	if tf == nil {
		return thisLine
	}
	src, err := pass.ReadFile(tf.Name())
	if err != nil {
		return thisLine
	}
	start := tf.Offset(tf.LineStart(tf.Line(c.Pos())))
	end := tf.Offset(c.Pos())
	if start > end || end > len(src) || strings.TrimSpace(string(src[start:end])) != "" {
		return thisLine
	}
	return nextLine
}

// inPkg says whether import path p is the repo package suffix, such as
// "internal/core". Rules match on the suffix so test fixtures can use them.
func inPkg(p, suffix string) bool {
	p = strings.TrimSuffix(p, "_test")
	return p == suffix || strings.HasSuffix(p, "/"+suffix)
}

// underPkg says whether p is that package or one below it, such as
// "internal/core/kernel": the rules that are about how code in core is
// written, not about core.Service, follow code that moves into a package
// under core (WP-ADR151).
func underPkg(p, suffix string) bool {
	p = strings.TrimSuffix(p, "_test")
	return inPkg(p, suffix) || strings.HasPrefix(p, suffix+"/") || strings.Contains(p, "/"+suffix+"/")
}

func isTestFile(pass *analysis.Pass, f *ast.File) bool {
	return strings.HasSuffix(pass.Fset.Position(f.Pos()).Filename, "_test.go")
}

// callee is the function or method a call runs, when it is statically known.
func callee(pass *analysis.Pass, call *ast.CallExpr) *types.Func {
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		fn, _ := pass.TypesInfo.Uses[fun].(*types.Func)
		return fn
	case *ast.SelectorExpr:
		fn, _ := pass.TypesInfo.Uses[fun.Sel].(*types.Func)
		return fn
	}
	return nil
}

// recvName is the name of a method's receiver type, "" for a function.
func recvName(fn *types.Func) string {
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return ""
	}
	return typeName(sig.Recv().Type())
}

func typeName(t types.Type) string {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		t = p.Elem()
	}
	if n, ok := types.Unalias(t).(*types.Named); ok {
		return n.Obj().Name()
	}
	return ""
}

// isPkgFunc says whether fn is the package-level function pkg.name.
func isPkgFunc(fn *types.Func, pkg, name string) bool {
	return fn != nil && fn.Pkg() != nil && fn.Pkg().Path() == pkg && fn.Name() == name && recvName(fn) == ""
}

// constString is e's value when e is a string constant.
func constString(pass *analysis.Pass, e ast.Expr) (string, bool) {
	tv, ok := pass.TypesInfo.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

// funcDecls maps each function declared in the package to its declaration.
func funcDecls(pass *analysis.Pass) map[*types.Func]*ast.FuncDecl {
	decls := map[*types.Func]*ast.FuncDecl{}
	for _, f := range pass.Files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				if fn, ok := pass.TypesInfo.Defs[fd.Name].(*types.Func); ok {
					decls[fn] = fd
				}
			}
		}
	}
	return decls
}

// pkgLevel says whether obj is declared at the top of a package.
func pkgLevel(obj types.Object) bool {
	return obj != nil && obj.Pkg() != nil && obj.Parent() == obj.Pkg().Scope()
}
