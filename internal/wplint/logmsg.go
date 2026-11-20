package wplint

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"log/slog"

	"golang.org/x/tools/go/analysis"
)

// LogMsg keeps a warning's message fixed text. Job health tells one problem
// from another by where it was logged, its message and what it was about
// (WP-ADR139): a message with a number or a name written into it is a new
// problem each time, never persists, and fills the list.
var LogMsg = &analysis.Analyzer{
	Name: "logmsg",
	Doc:  "in internal/core and internal/api, a warning's or error's message is fixed text, with what varies in an attribute, so the same problem is counted as one (WP-ADR139)",
	Run:  runLogMsg,
}

// logMsgArg is where the message is among the arguments of a method of
// *slog.Logger, or of the package-level function of the same name.
var logMsgArg = map[string]int{
	"Warn": 0, "Error": 0, "WarnContext": 1, "ErrorContext": 1,
	"Log": 2, "LogAttrs": 2, // after the context and the level
}

func runLogMsg(pass *analysis.Pass) (any, error) {
	if !inPkg(pass.Pkg.Path(), "internal/core") && !inPkg(pass.Pkg.Path(), "internal/api") {
		return nil, nil
	}
	r := newReporter(pass)
	for _, f := range pass.Files {
		if isTestFile(pass, f) {
			continue
		}
		// check looks at the calls in one function's body, a function
		// declared or a literal at package level (var x = func() { … }).
		check := func(body *ast.BlockStmt) {
			ast.Inspect(body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				fn := callee(pass, call)
				if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != "log/slog" {
					return true
				}
				if rn := recvName(fn); rn != "" && rn != "Logger" {
					return true
				}
				at, ok := logMsgArg[fn.Name()]
				if !ok || at >= len(call.Args) {
					return true
				}
				if at == 2 && belowWarn(pass, call.Args[1]) {
					return true
				}
				if msg := call.Args[at]; builtHere(pass, msg) || builtInVar(pass, body, msg) {
					r.report(msg.Pos(), "a warning's message is fixed text: put what varies in an attribute, or each value is a problem of its own")
				}
				return true
			})
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				if fd.Body != nil {
					check(fd.Body)
				}
				continue
			}
			ast.Inspect(d, func(n ast.Node) bool {
				if lit, ok := n.(*ast.FuncLit); ok {
					check(lit.Body)
					return false
				}
				return true
			})
		}
	}
	return nil, nil
}

// belowWarn says whether a level is a constant below Warn: such a record
// makes no problem. A level that is only known at run time is checked.
func belowWarn(pass *analysis.Pass, level ast.Expr) bool {
	tv, ok := pass.TypesInfo.Types[level]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.Int {
		return false
	}
	v, exact := constant.Int64Val(tv.Value)
	return exact && v < int64(slog.LevelWarn)
}

// builtInVar says whether a message that is a local variable gets a value
// put together at run time anywhere in the function: msg := fmt.Sprintf(…).
// A variable only ever given constants passes, as does a parameter.
func builtInVar(pass *analysis.Pass, body *ast.BlockStmt, e ast.Expr) bool {
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return false
	}
	v, ok := pass.TypesInfo.Uses[id].(*types.Var)
	if !ok || v.IsField() || v.Parent() == pass.Pkg.Scope() {
		return false
	}
	is := func(lhs ast.Expr) bool {
		l, ok := ast.Unparen(lhs).(*ast.Ident)
		return ok && (pass.TypesInfo.Uses[l] == v || pass.TypesInfo.Defs[l] == v)
	}
	built := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if len(n.Lhs) != len(n.Rhs) {
				return true // a call that returns several values: not a message put together here
			}
			for k, lhs := range n.Lhs {
				if is(lhs) && n.Tok != token.ASSIGN && n.Tok != token.DEFINE {
					built = true // msg += name
				} else if is(lhs) && builtHere(pass, n.Rhs[k]) {
					built = true
				}
			}
		case *ast.ValueSpec:
			if len(n.Names) != len(n.Values) {
				return true
			}
			for k, name := range n.Names {
				if is(name) && builtHere(pass, n.Values[k]) {
					built = true
				}
			}
		}
		return !built
	})
	return built
}

// builtHere says whether a message is put together where it is logged: a
// call or a concatenation that isn't constant. A variable passes: what a
// helper is told to call its step is one of a few fixed texts.
func builtHere(pass *analysis.Pass, e ast.Expr) bool {
	if tv, ok := pass.TypesInfo.Types[e]; ok && tv.Value != nil {
		return false
	}
	switch e := ast.Unparen(e).(type) {
	case *ast.CallExpr, *ast.BinaryExpr:
		return true
	case *ast.IndexExpr, *ast.SliceExpr:
		return true
	default:
		_ = e
		return false
	}
}
