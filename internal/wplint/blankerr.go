package wplint

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
)

// BlankErr keeps a failed read from becoming an answer. A check that throws
// away the error of a read (`_ = s.something()`), or a helper that can only
// say yes or no and says it from `err != nil`, turns "the database or the
// forge didn't answer" into "no" or "yes", and a merge, a gate or an
// "outside" mark follows from a guess (WP-1406). A helper returns (answer,
// error), tells sql.ErrNoRows and a forge's not-found apart with errors.Is as
// answers, and lets every other error through.
var BlankErr = &analysis.Analyzer{
	Name: "blankerr",
	Doc:  "in internal/core and internal/api, don't drop or assign to _ an error result of a method on Service or tx, don't leave an `if err != nil` empty, and don't have a function that returns only a bool decide it by testing err (WP-1406)",
	Run:  runBlankErr,
}

func runBlankErr(pass *analysis.Pass) (any, error) {
	if !inPkg(pass.Pkg.Path(), "internal/core") && !inPkg(pass.Pkg.Path(), "internal/api") {
		return nil, nil
	}
	r := newReporter(pass)
	for _, f := range pass.Files {
		if isTestFile(pass, f) {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				blankedError(pass, r, n)
			case *ast.ExprStmt:
				if call, ok := ast.Unparen(n.X).(*ast.CallExpr); ok {
					droppedCall(pass, r, call, n.Pos(), "")
				}
			case *ast.DeferStmt:
				droppedCall(pass, r, n.Call, n.Pos(), "deferred ")
			case *ast.GoStmt:
				droppedCall(pass, r, n.Call, n.Pos(), "started with go: ")
			case *ast.IfStmt:
				emptyErrBranch(pass, r, n)
			case *ast.FuncDecl:
				if n.Body != nil {
					boolOnly(pass, r, n.Type, n.Body)
				}
			case *ast.FuncLit:
				boolOnly(pass, r, n.Type, n.Body)
			}
			return true
		})
	}
	return nil, nil
}

// blankedError reports an error result of a Service or tx method that is
// assigned to the blank identifier.
func blankedError(pass *analysis.Pass, r *reporter, as *ast.AssignStmt) {
	isBlank := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && id.Name == "_"
	}
	check := func(call *ast.CallExpr, types []types.Type, lhs []ast.Expr) {
		fn := callee(pass, call)
		if fn == nil {
			return
		}
		if rn := recvName(fn); rn != "Service" && rn != "tx" {
			return
		}
		for i, l := range lhs {
			if i < len(types) && isBlank(l) && isErrorType(types[i]) {
				r.report(as.Pos(), "the error from %s is discarded: return it, or log it and hold; a failed read is not an answer (WP-1406)", fn.Name())
				return
			}
		}
	}
	if len(as.Rhs) == 1 {
		call, ok := ast.Unparen(as.Rhs[0]).(*ast.CallExpr)
		if !ok {
			return
		}
		tv, ok := pass.TypesInfo.Types[call]
		if !ok {
			return
		}
		if tup, ok := tv.Type.(*types.Tuple); ok {
			ts := make([]types.Type, tup.Len())
			for i := range ts {
				ts[i] = tup.At(i).Type()
			}
			check(call, ts, as.Lhs)
			return
		}
		check(call, []types.Type{tv.Type}, as.Lhs)
		return
	}
	for i, rhs := range as.Rhs {
		call, ok := ast.Unparen(rhs).(*ast.CallExpr)
		if !ok || i >= len(as.Lhs) {
			continue
		}
		if tv, ok := pass.TypesInfo.Types[call]; ok {
			check(call, []types.Type{tv.Type}, as.Lhs[i:i+1])
		}
	}
}

// droppedCall reports a statement that calls a Service or tx method and
// lets its error fall on the floor: `s.save(x)`, `defer s.close()` or
// `go s.send()`.
func droppedCall(pass *analysis.Pass, r *reporter, call *ast.CallExpr, pos token.Pos, how string) {
	fn := callee(pass, call)
	if fn == nil {
		return
	}
	if rn := recvName(fn); rn != "Service" && rn != "tx" {
		return
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return
	}
	for i := 0; i < sig.Results().Len(); i++ {
		if isErrorType(sig.Results().At(i).Type()) {
			r.report(pos, "the error from %s%s is dropped by a statement that ignores it: handle it, or log it and hold (WP-1406)", how, fn.Name())
			return
		}
	}
}

// emptyErrBranch reports `if err != nil {}`: the failure is noticed and then
// nothing happens.
func emptyErrBranch(pass *analysis.Pass, r *reporter, st *ast.IfStmt) {
	if len(st.Body.List) != 0 {
		return
	}
	b, ok := ast.Unparen(st.Cond).(*ast.BinaryExpr)
	if !ok || b.Op != token.NEQ {
		return
	}
	for _, side := range []ast.Expr{b.X, b.Y} {
		if id, ok := ast.Unparen(side).(*ast.Ident); ok && id.Name != "nil" && isErrorType(pass.TypesInfo.TypeOf(id)) {
			r.report(st.Pos(), "this tests an error and does nothing with it: return it, or log it and hold (WP-1406)")
			return
		}
	}
}

// boolOnly reports a return in a function whose only result is a bool when
// its expression tests an error: the failure has nowhere to go but the answer.
func boolOnly(pass *analysis.Pass, r *reporter, ft *ast.FuncType, body *ast.BlockStmt) {
	if ft.Results == nil || len(ft.Results.List) != 1 || len(ft.Results.List[0].Names) > 1 {
		return
	}
	if t := pass.TypesInfo.TypeOf(ft.Results.List[0].Type); t == nil || !types.Identical(t, types.Typ[types.Bool]) {
		return
	}
	from := errSources(pass, body)
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false // its own results are its own
		case *ast.ReturnStmt:
			if len(n.Results) == 1 && testsError(pass, n.Results[0], ft, from) {
				r.report(n.Pos(), "this returns only a bool, decided by testing an error: return (bool, error) and let the caller hold on the error (WP-1406)")
			}
		}
		return true
	})
}

// answerPkgs are packages whose errors are answers, not failed reads: a
// string that doesn't parse, or a write to a client that has gone.
var answerPkgs = map[string]bool{
	"net": true, "net/netip": true, "net/url": true, "strconv": true, "time": true,
	"fmt": true, "io": true, "encoding/json": true, "regexp": true, "strings": true,
}

// errSources maps each error variable assigned in body to the call that
// gave it its value.
func errSources(pass *analysis.Pass, body *ast.BlockStmt) map[types.Object]*ast.CallExpr {
	from := map[types.Object]*ast.CallExpr{}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			return true
		}
		call, ok := ast.Unparen(as.Rhs[0]).(*ast.CallExpr)
		if !ok {
			return true
		}
		for _, l := range as.Lhs {
			if id, ok := l.(*ast.Ident); ok {
				if obj := pass.TypesInfo.ObjectOf(id); obj != nil && isErrorType(obj.Type()) {
					from[obj] = call
				}
			}
		}
		return true
	})
	return from
}

// testsError says whether e compares to nil an error variable that holds the
// result of a read. An error the function was handed to classify (a
// parameter), or one from parsing or writing, is the answer itself.
func testsError(pass *analysis.Pass, e ast.Expr, ft *ast.FuncType, from map[types.Object]*ast.CallExpr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		b, ok := n.(*ast.BinaryExpr)
		if !ok || (b.Op != token.EQL && b.Op != token.NEQ) {
			return true
		}
		for _, side := range []ast.Expr{b.X, b.Y} {
			id, ok := ast.Unparen(side).(*ast.Ident)
			if !ok || id.Name == "nil" || !isErrorType(pass.TypesInfo.TypeOf(id)) {
				continue
			}
			obj := pass.TypesInfo.ObjectOf(id)
			if obj != nil && obj.Pos() >= ft.Pos() && obj.Pos() < ft.End() && ft.Params != nil &&
				obj.Pos() >= ft.Params.Pos() && obj.Pos() < ft.Params.End() {
				continue // a parameter: this classifies an error it was given
			}
			if call := from[obj]; call != nil {
				if fn := callee(pass, call); fn != nil && fn.Pkg() != nil && answerPkgs[fn.Pkg().Path()] {
					continue
				}
			}
			found = true
		}
		return !found
	})
	return found
}

func isErrorType(t types.Type) bool {
	return t != nil && types.Identical(t, types.Universe.Lookup("error").Type())
}
