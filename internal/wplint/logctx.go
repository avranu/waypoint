package wplint

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
)

// LogCtx keeps a warning tied to the work that raised it. The log handlers
// read the context: the trace id comes from it, and a scheduled job's run
// counts the warnings and errors logged with its context, which is how a job
// that fails every round is told from one that works (WP-999). Warn and Error
// drop the context, so the record belongs to nothing.
var LogCtx = &analysis.Analyzer{
	Name: "logctx",
	Doc:  "in internal/core and internal/api, log warnings and errors with WarnContext/ErrorContext where a context is in scope, so they count on the job or request that raised them (WP-999)",
	Run:  runLogCtx,
}

func runLogCtx(pass *analysis.Pass) (any, error) {
	if !inPkg(pass.Pkg.Path(), "internal/core") && !inPkg(pass.Pkg.Path(), "internal/api") {
		return nil, nil
	}
	r := newReporter(pass)
	for _, f := range pass.Files {
		if isTestFile(pass, f) {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn := callee(pass, call)
			if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != "log/slog" || recvName(fn) != "Logger" {
				return true
			}
			if name := fn.Name(); name == "Warn" || name == "Error" {
				if ctx := contextInScope(pass, call.Pos()); ctx != "" {
					r.report(call.Pos(), "use %sContext(%s, …): logged without the context, this isn't counted on the job or request that raised it", name, ctx)
				}
			}
			return true
		})
	}
	return nil, nil
}

// contextInScope names a context.Context variable the code at pos can use:
// a parameter or a local declared before it. Package variables and struct
// fields don't count; they aren't the caller's context.
func contextInScope(pass *analysis.Pass, pos token.Pos) string {
	pkg := pass.Pkg.Scope()
	for s := pkg.Innermost(pos); s != nil && s != pkg; s = s.Parent() {
		for _, name := range s.Names() {
			v, ok := s.Lookup(name).(*types.Var)
			if !ok || name == "_" || v.Pos() >= pos || !isContext(v.Type()) {
				continue
			}
			return name
		}
	}
	return ""
}

func isContext(t types.Type) bool {
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == "context" && n.Obj().Name() == "Context"
}
