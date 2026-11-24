package wplint

import (
	"go/ast"
	"go/token"
	"go/types"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// APIDB keeps database access out of internal/api: every read and write goes
// through internal/core.Service, which does the access checks (AGENTS.md,
// REVIEW.md). Tests may set up data directly.
var APIDB = &analysis.Analyzer{
	Name: "apidb",
	Doc:  "internal/api must not import the database or touch database/sql values; go through core.Service (REVIEW.md)",
	Run:  runAPIDB,
}

func runAPIDB(pass *analysis.Pass) (any, error) {
	if !inPkg(pass.Pkg.Path(), "internal/api") {
		return nil, nil
	}
	r := newReporter(pass)
	for _, f := range pass.Files {
		if isTestFile(pass, f) {
			continue
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == "database/sql" || inPkg(p, "internal/db") {
				r.report(imp.Pos(), "internal/api must not import %s: go through core.Service, which checks access", p)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			tv, ok := pass.TypesInfo.Types[sel.X]
			if !ok || tv.IsType() {
				return true
			}
			if n := sqlHandle(tv.Type); n != "" {
				r.report(sel.Pos(), "internal/api must not use a %s: go through core.Service, which checks access", n)
			}
			return true
		})
	}
	return nil, nil
}

func sqlHandle(t types.Type) string {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := types.Unalias(t).(*types.Named)
	if !ok || n.Obj().Pkg() == nil || n.Obj().Pkg().Path() != "database/sql" {
		return ""
	}
	switch n.Obj().Name() {
	case "DB", "Tx", "Conn", "Stmt":
		return "sql." + n.Obj().Name()
	}
	return ""
}

// serviceSideEffects are internal/core's own functions that reach outside
// the database. Forge clients are found by type (see directEffect).
var serviceSideEffects = map[string]string{
	"notifyFor":       "a notification",
	"notify":          "a notification",
	"notifyURL":       "a notification",
	"deliverWebhooks": "a webhook",
	"sendLogged":      "a webhook",
	"sendPush":        "a push notification",
	"sendEmail":       "an email",
	"runChain":        "a model call",
	"runChainAt":      "a model call",
	"runAI":           "a model call",
	"dispatch":        "event dispatch",
}

// WriteClosure keeps side effects out of a write transaction: a webhook,
// notification, forge, HTTP or model call belongs in event dispatch after
// t.emit (AGENTS.md, REVIEW.md). Inside Service.write it would run before the
// commit, for a change that can still roll back, and hold SQLite's only
// writer for the length of a network call. Helpers in internal/core are
// followed.
var WriteClosure = &analysis.Analyzer{
	Name: "writeclosure",
	Doc:  "no forge, notify, webhook, HTTP or model calls inside a Service.write closure; use t.emit and dispatch (REVIEW.md)",
	Run:  runWriteClosure,
}

func directEffect(fn *types.Func) string {
	if fn == nil || fn.Pkg() == nil {
		return ""
	}
	switch p := fn.Pkg().Path(); {
	case p == "net/http":
		if recv := recvName(fn); recv == "Client" || (recv == "" && (fn.Name() == "Get" || fn.Name() == "Post" || fn.Name() == "PostForm" || fn.Name() == "Head")) {
			return "an HTTP call"
		}
		return ""
	case p == "os/exec":
		if fn.Name() == "Command" || fn.Name() == "CommandContext" {
			return "a process"
		}
		return ""
	case inPkg(p, "internal/core"):
		if recv := recvName(fn); strings.HasPrefix(recv, "Forge") && isInterfaceMethod(fn) || recv == "httpForgeClient" {
			return "a forge call"
		}
		if recvName(fn) == "Service" || recvName(fn) == "tx" || recvName(fn) == "" {
			return serviceSideEffects[fn.Name()]
		}
	}
	return ""
}

func isInterfaceMethod(fn *types.Func) bool {
	sig, ok := fn.Type().(*types.Signature)
	return ok && sig.Recv() != nil && types.IsInterface(sig.Recv().Type())
}

func runWriteClosure(pass *analysis.Pass) (any, error) {
	if !inPkg(pass.Pkg.Path(), "internal/core") {
		return nil, nil
	}
	r := newReporter(pass)
	decls := funcDecls(pass)

	type reach struct {
		what string
		via  []string
	}
	effect := map[*types.Func]reach{}
	calls := map[*types.Func][]*types.Func{}
	for fn, fd := range decls {
		if fn.Name() == "write" && recvName(fn) == "Service" {
			// write dispatches after its commit, which is the point: what
			// calls write is not inside one because of it.
			continue
		}
		// Work in goBG or go statements counts toward the effect; a write started that way does not, since write is skipped above.
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				c := callee(pass, n)
				if what := directEffect(c); what != "" {
					effect[fn] = reach{what: what, via: []string{fn.Name(), c.Name()}}
				} else if c != nil && decls[c] != nil {
					calls[fn] = append(calls[fn], c)
				}
			}
			return true
		})
	}
	for changed := true; changed; {
		changed = false
		for fn, callees := range calls {
			if _, done := effect[fn]; done {
				continue
			}
			for _, c := range callees {
				if e, ok := effect[c]; ok {
					effect[fn] = reach{what: e.what, via: append([]string{fn.Name()}, e.via...)}
					changed = true
					break
				}
			}
		}
	}

	for _, f := range pass.Files {
		if isTestFile(pass, f) {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			c := callee(pass, call)
			if c == nil || c.Name() != "write" || recvName(c) != "Service" || len(call.Args) == 0 {
				return true
			}
			closure, ok := ast.Unparen(call.Args[len(call.Args)-1]).(*ast.FuncLit)
			if !ok {
				return true
			}
			ast.Inspect(closure.Body, func(m ast.Node) bool {
				if call, ok := m.(*ast.CallExpr); ok {
					inner := callee(pass, call)
					if what := directEffect(inner); what != "" {
						r.report(call.Pos(), "%s (%s) inside Service.write runs before the commit: use t.emit and dispatch (events.go)", what, inner.Name())
						return true
					}
					if inner != nil {
						if e, ok := effect[inner]; ok {
							r.report(call.Pos(), "%s reaches %s (%s) inside Service.write, before the commit: use t.emit and dispatch (events.go)", inner.Name(), e.what, strings.Join(e.via, " → "))
						}
					}
				}
				return true
			})
			return false
		})
	}
	return nil, nil
}

// ActorUsed flags an exported Service method that takes an *Actor and never
// reads it: the access check it promises can't be happening (REVIEW.md, "skips
// the *Actor access check"). It can't prove a check is right, only that one
// is there to be right.
var ActorUsed = &analysis.Analyzer{
	Name: "actorused",
	Doc:  "an exported core.Service method that takes an *Actor must use it (REVIEW.md)",
	Run:  runActorUsed,
}

func runActorUsed(pass *analysis.Pass) (any, error) {
	if !inPkg(pass.Pkg.Path(), "internal/core") {
		return nil, nil
	}
	r := newReporter(pass)
	for _, f := range pass.Files {
		if isTestFile(pass, f) {
			continue
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Recv == nil || !fd.Name.IsExported() {
				continue
			}
			fn, ok := pass.TypesInfo.Defs[fd.Name].(*types.Func)
			if !ok || recvName(fn) != "Service" {
				continue
			}
			for _, field := range fd.Type.Params.List {
				if typeName(pass.TypesInfo.TypeOf(field.Type)) != "Actor" {
					continue
				}
				if len(field.Names) == 0 {
					r.report(field.Pos(), "%s takes an *Actor and drops it: no access check can run", fd.Name.Name)
				}
				for _, name := range field.Names {
					if name.Name == "_" {
						r.report(name.Pos(), "%s takes an *Actor and drops it: no access check can run", fd.Name.Name)
						continue
					}
					if !usesObject(pass, fd.Body, pass.TypesInfo.Defs[name]) {
						r.report(name.Pos(), "%s takes an *Actor and never reads it: no access check can run", fd.Name.Name)
					}
				}
			}
		}
	}
	return nil, nil
}

func usesObject(pass *analysis.Pass, body ast.Node, obj types.Object) bool {
	used := false
	ast.Inspect(body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && pass.TypesInfo.Uses[id] == obj {
			used = true
		}
		return !used
	})
	return used
}

// Clock keeps internal/core on its clock: where a Service or tx is at hand,
// the time comes from s.now, which tests control and tests' background work
// reads (WP-393). time.Now stays fine for measuring a duration.
var Clock = &analysis.Analyzer{
	Name: "clock",
	Doc:  "in internal/core, use s.now instead of time.Now where a Service is at hand; durations are exempt (WP-393)",
	Run:  runClock,
}

func runClock(pass *analysis.Pass) (any, error) {
	if !inPkg(pass.Pkg.Path(), "internal/core") {
		return nil, nil
	}
	r := newReporter(pass)
	for _, f := range pass.Files {
		if isTestFile(pass, f) {
			continue
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || !hasClock(pass, fd) {
				continue
			}
			checkClock(pass, r, fd)
		}
	}
	return nil, nil
}

// hasClock says whether the function can reach s.now: a Service or tx
// receiver or parameter.
func hasClock(pass *analysis.Pass, fd *ast.FuncDecl) bool {
	lists := []*ast.FieldList{fd.Type.Params}
	if fd.Recv != nil {
		lists = append(lists, fd.Recv)
	}
	for _, l := range lists {
		for _, field := range l.List {
			if n := typeName(pass.TypesInfo.TypeOf(field.Type)); n == "Service" || n == "tx" {
				return true
			}
		}
	}
	return false
}

func checkClock(pass *analysis.Pass, r *reporter, fd *ast.FuncDecl) {
	real := realVars(pass, fd)
	var stack []ast.Node
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		c := callee(pass, call)
		if (isPkgFunc(c, "time", "Since") || isPkgFunc(c, "time", "Until")) && len(call.Args) == 1 && !isReal(pass, real, call.Args[0]) {
			r.report(call.Pos(), "time.%s measures against the real clock, but this time isn't from time.Now: use s.now().Sub, so a test clock stays consistent", c.Name())
			return true
		}
		if !isPkgFunc(c, "time", "Now") || len(stack) < 2 {
			return true
		}
		if measured(pass, real, stack, call) {
			return true
		}
		r.report(call.Pos(), "use s.now(), the clock tests control, instead of time.Now (a duration between two time.Now readings is exempt)")
		return true
	})
}

// measured says whether a time.Now call only measures elapsed real time: the
// argument or receiver of a Sub whose other side is real time too, or the
// value of a local that only ever holds time.Now (see realVars).
func measured(pass *analysis.Pass, real map[types.Object]bool, stack []ast.Node, call *ast.CallExpr) bool {
	switch p := stack[len(stack)-2].(type) {
	case *ast.SelectorExpr:
		if p.X != call || p.Sel.Name != "Sub" || len(stack) < 3 {
			return false
		}
		sub, ok := stack[len(stack)-3].(*ast.CallExpr)
		return ok && sub.Fun == p && len(sub.Args) == 1 && isReal(pass, real, sub.Args[0])
	case *ast.CallExpr:
		if sel, ok := ast.Unparen(p.Fun).(*ast.SelectorExpr); ok && sel.Sel.Name == "Sub" && len(p.Args) == 1 && p.Args[0] == call {
			return isReal(pass, real, sel.X)
		}
	case *ast.AssignStmt:
		for i, rhs := range p.Rhs {
			if rhs != call || len(p.Lhs) != len(p.Rhs) {
				continue
			}
			if id, ok := p.Lhs[i].(*ast.Ident); ok {
				obj := pass.TypesInfo.Defs[id]
				if obj == nil {
					obj = pass.TypesInfo.Uses[id]
				}
				return real[obj]
			}
		}
	case *ast.ValueSpec:
		for i, v := range p.Values {
			if v == call && len(p.Names) == len(p.Values) {
				return real[pass.TypesInfo.Defs[p.Names[i]]]
			}
		}
	}
	return false
}

// isReal says whether e is real time: a time.Now call or a local that only
// ever holds one.
func isReal(pass *analysis.Pass, real map[types.Object]bool, e ast.Expr) bool {
	switch e := ast.Unparen(e).(type) {
	case *ast.CallExpr:
		return isPkgFunc(callee(pass, e), "time", "Now")
	case *ast.Ident:
		return real[pass.TypesInfo.Uses[e]]
	}
	return false
}

// realVars are the locals that only measure real time: every assignment is
// time.Now(), and every use is a time.Since, or a Sub against real time, or
// another such assignment (`start = time.Now()` for the next step).
func realVars(pass *analysis.Pass, fd *ast.FuncDecl) map[types.Object]bool {
	cand := map[types.Object]bool{}
	bad := map[types.Object]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			if vs, ok := n.(*ast.ValueSpec); ok {
				for i, id := range vs.Names {
					obj := pass.TypesInfo.Defs[id]
					switch {
					case len(vs.Values) == 0:
					case len(vs.Names) == len(vs.Values) && isNowCall(pass, vs.Values[i]):
						cand[obj] = true
					default:
						bad[obj] = true
					}
				}
			}
			return true
		}
		for i, lhs := range as.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			obj := pass.TypesInfo.Defs[id]
			if obj == nil {
				obj = pass.TypesInfo.Uses[id]
			}
			if obj == nil || pkgLevel(obj) {
				continue
			}
			if len(as.Lhs) == len(as.Rhs) && (as.Tok == token.DEFINE || as.Tok == token.ASSIGN) && isNowCall(pass, as.Rhs[i]) {
				cand[obj] = true
			} else {
				bad[obj] = true
			}
		}
		return true
	})
	real := map[types.Object]bool{}
	for obj := range cand {
		if !bad[obj] {
			real[obj] = true
		}
	}
	// Dropping one variable can disqualify another (a Sub between two candidates needs both real), so repeat until nothing changes.
	for changed := true; changed; {
		changed = false
		var stack []ast.Node
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			use, ok := n.(*ast.Ident)
			if !ok || !real[pass.TypesInfo.Uses[use]] || len(stack) < 2 {
				return true
			}
			if !realUse(pass, real, stack, use) {
				delete(real, pass.TypesInfo.Uses[use])
				changed = true
			}
			return true
		})
	}
	return real
}

// realUse says whether a use of a real-time local keeps it one.
func realUse(pass *analysis.Pass, real map[types.Object]bool, stack []ast.Node, use *ast.Ident) bool {
	switch p := stack[len(stack)-2].(type) {
	case *ast.AssignStmt:
		for _, l := range p.Lhs {
			if l == use {
				return true
			}
		}
	case *ast.CallExpr:
		c := callee(pass, p)
		if isPkgFunc(c, "time", "Since") || isPkgFunc(c, "time", "Until") {
			return true
		}
		if sel, ok := ast.Unparen(p.Fun).(*ast.SelectorExpr); ok && sel.Sel.Name == "Sub" && len(p.Args) == 1 && p.Args[0] == use {
			return isReal(pass, real, sel.X)
		}
	case *ast.SelectorExpr:
		if p.X == use && p.Sel.Name == "Sub" && len(stack) >= 3 {
			if call, ok := stack[len(stack)-3].(*ast.CallExpr); ok && call.Fun == p && len(call.Args) == 1 {
				return isReal(pass, real, call.Args[0])
			}
		}
	}
	return false
}

func isNowCall(pass *analysis.Pass, e ast.Expr) bool {
	c, ok := ast.Unparen(e).(*ast.CallExpr)
	return ok && isPkgFunc(callee(pass, c), "time", "Now")
}

// ProtectedDiff: a protected-path check reads what a change touched from git.
// `git diff --name-only` follows renames, so moving a protected file away
// lists only its new name and the check passes (WP-415, 20 days in
// production). Names need -z too, or a new file's odd name is quoted or
// split. gitPaths adds -z itself.
var ProtectedDiff = &analysis.Analyzer{
	Name: "protecteddiff",
	Doc:  "a git --name-only diff that feeds a protected-path check must pass --no-renames and -z (WP-415)",
	Run:  runProtectedDiff,
}

func runProtectedDiff(pass *analysis.Pass) (any, error) {
	r := newReporter(pass)
	for _, f := range pass.Files {
		if isTestFile(pass, f) {
			continue
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || !callsProtected(pass, fd) {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				args := literalArgs(pass, call)
				if !args["--name-only"] || (!args["diff"] && !args["diff-tree"]) {
					return true
				}
				if !args["--no-renames"] {
					r.report(call.Pos(), "this diff feeds a protected-path check and follows renames: moving a protected file away would pass; add --no-renames")
				}
				if c := callee(pass, call); !args["-z"] && (c == nil || c.Name() != "gitPaths") {
					r.report(call.Pos(), "this diff feeds a protected-path check without -z: a new file's odd name would be quoted or split")
				}
				return false
			})
		}
	}
	return nil, nil
}

func callsProtected(pass *analysis.Pass, fd *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if c := callee(pass, call); c != nil && strings.HasPrefix(strings.ToLower(c.Name()), "protected") {
				found = true
			}
		}
		return !found
	})
	return found
}

// literalArgs is the string constants among a call's arguments, including
// those inside slice literals and appends in them.
func literalArgs(pass *analysis.Pass, call *ast.CallExpr) map[string]bool {
	out := map[string]bool{}
	for _, a := range call.Args {
		ast.Inspect(a, func(n ast.Node) bool {
			if e, ok := n.(ast.Expr); ok {
				if s, ok := constString(pass, e); ok {
					out[s] = true
					return false
				}
			}
			return true
		})
	}
	return out
}

// StatusCreator: anyone with write access to a repo can post a commit status
// under any name, so who posted one is part of what it says (WP-416). Code
// that decodes a forge's statuses keeps the creator, and code that tests a
// status against one of Waypoint's own waypoint/* names also looks at it.
// WP-452's instance is shell in ci.yml, which this can't see.
var StatusCreator = &analysis.Analyzer{
	Name: "statuscreator",
	Doc:  "forge status decodes must keep the creator; waypoint/* status checks must look at it (WP-416)",
	Run:  runStatusCreator,
}

func runStatusCreator(pass *analysis.Pass) (any, error) {
	if !inPkg(pass.Pkg.Path(), "internal/core") {
		return nil, nil
	}
	r := newReporter(pass)
	for _, f := range pass.Files {
		if isTestFile(pass, f) {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.TypeSpec:
				if strings.HasSuffix(n.Name.Name, "Post") {
					return false
				}
			case *ast.StructType:
				tags := jsonTags(n)
				if tags["context"] && (tags["state"] || tags["status"]) && !tags["creator"] && !tags["author"] {
					r.report(n.Pos(), "this decodes a forge's commit statuses without who posted them; keep creator (anyone with write access can post any status)")
				}
			case *ast.FuncDecl:
				if n.Body != nil {
					checkOwnStatus(pass, r, n)
				}
				return true
			}
			return true
		})
	}
	return nil, nil
}

func jsonTags(st *ast.StructType) map[string]bool {
	out := map[string]bool{}
	for _, field := range st.Fields.List {
		if field.Tag == nil {
			continue
		}
		tag, err := strconv.Unquote(field.Tag.Value)
		if err != nil {
			continue
		}
		_, rest, ok := strings.Cut(tag, `json:"`)
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(rest, `"`)
		name, _, _ = strings.Cut(name, ",")
		out[name] = true
	}
	return out
}

func checkOwnStatus(pass *analysis.Pass, r *reporter, fd *ast.FuncDecl) {
	readsCreator := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Creator" {
			readsCreator = true
		}
		return !readsCreator
	})
	if readsCreator {
		return
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		cmp, ok := n.(*ast.BinaryExpr)
		if !ok || (cmp.Op != token.EQL && cmp.Op != token.NEQ) {
			return true
		}
		for _, pair := range [][2]ast.Expr{{cmp.X, cmp.Y}, {cmp.Y, cmp.X}} {
			sel, ok := pair[0].(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Name" || typeName(pass.TypesInfo.TypeOf(sel.X)) != "CommitCheck" {
				continue
			}
			if s, ok := constString(pass, pair[1]); ok && strings.HasPrefix(s, "waypoint/") {
				r.report(cmp.Pos(), "%s is compared by name alone: anyone with write access can post it; check Creator too", s)
			}
		}
		return true
	})
}

// prodEnv is what points a process at the real server (WP-537).
var prodEnv = map[string]bool{"WAYPOINT_SERVER": true, "WAYPOINT_TOKEN": true}

// TestEnv: a test run from a runner job or an agent's shell inherits the real
// server's address and a token, and a test that expects no server posts to
// production (WP-537: a guard test's hook events showed up as idle sessions
// on the autopilot tab). A package whose code reads them has a TestMain that
// clears them, so the only values a test sees are ones it set.
var TestEnv = &analysis.Analyzer{
	Name: "testenv",
	Doc:  "a package that reads WAYPOINT_SERVER or WAYPOINT_TOKEN clears them in TestMain (WP-537)",
	Run:  runTestEnv,
}

func runTestEnv(pass *analysis.Pass) (any, error) {
	var firstRead token.Pos
	hasTests := false
	cleared := map[string]bool{}
	for _, f := range pass.Files {
		test := isTestFile(pass, f)
		hasTests = hasTests || test
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			if test && fd.Recv == nil && fd.Name.Name == "TestMain" {
				collectCleared(pass, fd, cleared)
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				c := callee(pass, call)
				if !isPkgFunc(c, "os", "Getenv") && !isPkgFunc(c, "os", "LookupEnv") {
					return true
				}
				if s, ok := constString(pass, call.Args[0]); ok && prodEnv[s] && (firstRead == token.NoPos || call.Pos() < firstRead) {
					firstRead = call.Pos()
				}
				return true
			})
		}
	}
	if !hasTests || firstRead == token.NoPos {
		return nil, nil
	}
	r := newReporter(pass)
	var missing []string
	for _, k := range []string{"WAYPOINT_SERVER", "WAYPOINT_TOKEN"} {
		if !cleared[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		r.report(firstRead, "this package reads the real server's settings, and its tests inherit them: add a TestMain that clears %s", strings.Join(missing, " and "))
	}
	return nil, nil
}

// collectCleared adds the variables TestMain clears: Unsetenv or Clearenv of
// a literal, including a range over a literal list.
// unsetsRangeVar says whether the loop's body calls os.Unsetenv on its value
// variable.
func unsetsRangeVar(pass *analysis.Pass, rng *ast.RangeStmt) bool {
	v, ok := rng.Value.(*ast.Ident)
	if !ok {
		return false
	}
	obj := pass.TypesInfo.ObjectOf(v)
	found := false
	ast.Inspect(rng.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || found || len(call.Args) != 1 || !isPkgFunc(callee(pass, call), "os", "Unsetenv") {
			return !found
		}
		if id, ok := ast.Unparen(call.Args[0]).(*ast.Ident); ok && obj != nil && pass.TypesInfo.Uses[id] == obj {
			found = true
		}
		return !found
	})
	return found
}

func collectCleared(pass *analysis.Pass, fd *ast.FuncDecl, cleared map[string]bool) {
	unsets := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			c := callee(pass, call)
			if isPkgFunc(c, "os", "Clearenv") {
				for k := range prodEnv {
					cleared[k] = true
				}
			}
			if isPkgFunc(c, "os", "Unsetenv") {
				unsets = true
				if len(call.Args) == 1 {
					if s, ok := constString(pass, call.Args[0]); ok {
						cleared[s] = true
					}
				}
			}
		}
		return true
	})
	if !unsets {
		return
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		rng, ok := n.(*ast.RangeStmt)
		if !ok {
			return true
		}
		lit, ok := rng.X.(*ast.CompositeLit)
		if !ok || !unsetsRangeVar(pass, rng) {
			return true
		}
		for _, e := range lit.Elts {
			if s, ok := constString(pass, e); ok {
				cleared[s] = true
			}
		}
		return true
	})
}
