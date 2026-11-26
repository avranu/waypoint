package wplint

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// DomainActor keeps a domain's Service reachable only through core: core's
// Service embeds each domain's, so an exported method on it is callable from
// the API, and every such method takes the *kernel.Actor whose access it
// checks. An exported field would be promoted the same way, past any check
// (WP-ADR151 section 2).
var DomainActor = &analysis.Analyzer{
	Name: "domainactor",
	Doc:  "in a package under internal/core other than core and kernel, an exported method of Service takes a *kernel.Actor and Service has no exported field (WP-ADR151)",
	Run:  runDomainActor,
}

func runDomainActor(pass *analysis.Pass) (any, error) {
	d := coreDomain(pass.Pkg.Path())
	if d == "" || d == "kernel" {
		return nil, nil
	}
	r := newReporter(pass)
	if obj, ok := pass.Pkg.Scope().Lookup("Service").(*types.TypeName); ok {
		if st, ok := obj.Type().Underlying().(*types.Struct); ok {
			for i := 0; i < st.NumFields(); i++ {
				if f := st.Field(i); f.Exported() && !isTestPos(pass, f) {
					r.report(f.Pos(), "Service.%s is exported, and core.Service embeds Service, so the API could read it past every access check: unexport it (WP-ADR151)", f.Name())
				}
			}
		}
	}
	for _, f := range pass.Files {
		if isTestFile(pass, f) {
			continue
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || !fd.Name.IsExported() {
				continue
			}
			fn, ok := pass.TypesInfo.Defs[fd.Name].(*types.Func)
			if !ok || recvName(fn) != "Service" || takesActor(fn) {
				continue
			}
			r.report(fd.Name.Pos(), "exported Service.%s has no *kernel.Actor parameter, and core.Service embeds Service, so the API can call it with no access check: take the actor, or unexport it (WP-ADR151)", fd.Name.Name)
		}
	}
	return nil, nil
}

func takesActor(fn *types.Func) bool {
	sig := fn.Type().(*types.Signature)
	for i := 0; i < sig.Params().Len(); i++ {
		t := sig.Params().At(i).Type()
		if _, ok := types.Unalias(t).(*types.Pointer); ok && kernelNamed(t, "Actor") {
			return true
		}
	}
	return false
}

func isTestPos(pass *analysis.Pass, o types.Object) bool {
	tf := pass.Fset.File(o.Pos())
	return tf != nil && strings.HasSuffix(tf.Name(), "_test.go")
}
