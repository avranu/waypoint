package wplint

import (
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// CoreLayers keeps the layers under internal/core pointing one way
// (WP-ADR151): kernel imports no other package under internal/core, and a
// package under internal/core never imports package core, which composes
// them. The compiler refuses a cycle only once one exists; this refuses the
// first import in the wrong direction. A domain imports only the domains
// listed before it in coreOrder, so a domain is added there, in its place,
// before it may import another.
var CoreLayers = &analysis.Analyzer{
	Name: "corelayers",
	Doc:  "internal/core/kernel imports nothing else under internal/core, a package under internal/core does not import internal/core, and a domain imports only domains below it (WP-ADR151)",
	Run:  runCoreLayers,
}

// coreOrder lists the domains under internal/core from the bottom up: each
// may import those before it. The order is the measured one kept on WP-1499.
var coreOrder = []string{"strength", "udiff", "meaning"}

// coreDomain is the domain of a package path under internal/core ("pr" for
// …/internal/core/pr/sub), or "" for package core and anything elsewhere.
func coreDomain(path string) string {
	const mark = "internal/core/"
	i := strings.LastIndex(path, mark)
	if i < 0 || (i > 0 && path[i-1] != '/') {
		return ""
	}
	d, _, _ := strings.Cut(path[i+len(mark):], "/")
	// A package's tests ("pr_test", and the generated "pr.test") are its domain.
	return strings.TrimSuffix(strings.TrimSuffix(d, ".test"), "_test")
}

func runCoreLayers(pass *analysis.Pass) (any, error) {
	self := strings.TrimSuffix(pass.Pkg.Path(), "_test")
	if !underPkg(self, "internal/core") || inPkg(self, "internal/core") {
		return nil, nil
	}
	kernel := inPkg(self, "internal/core/kernel")
	dom := coreDomain(self)
	at := slices.Index(coreOrder, dom)
	r := newReporter(pass)
	for _, f := range pass.Files {
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			switch {
			case inPkg(p, "internal/core"):
				r.report(imp.Pos(), "a package under internal/core must not import internal/core, which is built from it: move what this needs into kernel or a domain below this one (WP-ADR151)")
			case kernel && underPkg(p, "internal/core") && p != self:
				r.report(imp.Pos(), "internal/core/kernel must not import %s: the kernel holds what every domain needs and depends on none (WP-ADR151)", p)
			case kernel:
			default:
				to := coreDomain(p)
				if to == "" || to == "kernel" || to == dom {
					continue
				}
				switch below := slices.Index(coreOrder, to); {
				case at < 0:
					r.report(imp.Pos(), "%s is not in wplint's coreOrder, so it may import only the kernel: add it there, above %s (WP-ADR151)", dom, to)
				case below < 0 || below > at:
					r.report(imp.Pos(), "%s must not import %s, which is not below it in wplint's coreOrder: a domain imports only the kernel and the domains below it (WP-ADR151)", dom, to)
				}
			}
		}
	}
	return nil, nil
}
