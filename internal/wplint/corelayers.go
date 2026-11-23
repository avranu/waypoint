package wplint

import (
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// CoreLayers keeps the layers under internal/core pointing one way
// (WP-ADR151): kernel imports no other package under internal/core, and a
// package under internal/core never imports package core, which composes
// them. The compiler refuses a cycle only once one exists; this refuses the
// first import in the wrong direction. Which domain is below which is not
// checked here: an import between two domains that closes a cycle fails to
// build.
var CoreLayers = &analysis.Analyzer{
	Name: "corelayers",
	Doc:  "internal/core/kernel imports nothing else under internal/core, and a package under internal/core does not import internal/core (WP-ADR151)",
	Run:  runCoreLayers,
}

func runCoreLayers(pass *analysis.Pass) (any, error) {
	self := strings.TrimSuffix(pass.Pkg.Path(), "_test")
	if !underPkg(self, "internal/core") || inPkg(self, "internal/core") {
		return nil, nil
	}
	kernel := inPkg(self, "internal/core/kernel")
	r := newReporter(pass)
	for _, f := range pass.Files {
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			switch {
			case inPkg(p, "internal/core"):
				r.report(imp.Pos(), "a package under internal/core must not import internal/core, which is built from it: move what this needs into kernel or a domain below this one (WP-ADR151)")
			case kernel && underPkg(p, "internal/core") && p != self:
				r.report(imp.Pos(), "internal/core/kernel must not import %s: the kernel holds what every domain needs and depends on none (WP-ADR151)", p)
			}
		}
	}
	return nil, nil
}
