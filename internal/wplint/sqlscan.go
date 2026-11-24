package wplint

import (
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// SQLScan: a query that selects 4 columns and scans 3 compiles, passes any
// test that doesn't run it, and fails only in production (WP-682, found by a
// WARN). Where the column list and the Scan arguments are both known, they
// must match. Column-list constants (webhookCols) are resolved, and a row
// handed to a helper in the same package (scanForge) is counted there.
var SQLScan = &analysis.Analyzer{
	Name: "sqlscan",
	Doc:  "a query's selected columns must match its Scan arguments where both are literal (WP-682)",
	Run:  runSQLScan,
}

var queryMethods = map[string]int{"QueryContext": 1, "QueryRowContext": 1, "Query": 0, "QueryRow": 0}

// querySQL is the SQL of a *sql query call; ok is false for anything else.
func querySQL(pass *analysis.Pass, locals map[types.Object]ast.Expr, call *ast.CallExpr) (columns, known, ok bool, n int) {
	sel, isSel := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !isSel {
		return false, false, false, 0
	}
	idx, isQuery := queryMethods[sel.Sel.Name]
	if !isQuery || len(call.Args) <= idx {
		return false, false, false, 0
	}
	arg := call.Args[idx]
	if basic, isBasic := pass.TypesInfo.TypeOf(arg).Underlying().(*types.Basic); !isBasic || basic.Info()&types.IsString == 0 {
		return false, false, false, 0
	}
	text, complete := sqlText(pass, locals, arg)
	n, known = countColumns(text, complete)
	return true, known, true, n
}

// sqlText is the known start of a SQL expression, and whether that is all of
// it: constants, + of constants, the format of a Sprintf up to its first verb,
// or a local assigned once from one of those.
func sqlText(pass *analysis.Pass, locals map[types.Object]ast.Expr, e ast.Expr) (string, bool) {
	e = ast.Unparen(e)
	if s, ok := constString(pass, e); ok {
		return s, true
	}
	switch e := e.(type) {
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, complete := sqlText(pass, locals, e.X)
		if !complete {
			return left, false
		}
		right, complete := sqlText(pass, locals, e.Y)
		return left + right, complete
	case *ast.CallExpr:
		if isPkgFunc(callee(pass, e), "fmt", "Sprintf") && len(e.Args) > 0 {
			format, ok := constString(pass, e.Args[0])
			if !ok {
				return "", false
			}
			if i := strings.IndexByte(format, '%'); i >= 0 {
				return format[:i], false
			}
			return format, true
		}
	case *ast.Ident:
		if def := locals[pass.TypesInfo.Uses[e]]; def != nil {
			return sqlText(pass, locals, def)
		}
	}
	return "", false
}

// countColumns counts the result columns of a SQL statement, known only when
// the column list is entirely present: it stops at a top-level FROM, or at
// the end of complete text.
func countColumns(sql string, complete bool) (int, bool) {
	sql = stripSQLComments(sql)
	words := topLevelWords(sql)
	start, end := -1, -1
	first := ""
	for _, w := range words {
		u := strings.ToUpper(w.word)
		if first == "" {
			first = u
		}
		if first == "WITH" {
			switch u {
			case "SELECT", "INSERT", "UPDATE", "DELETE", "REPLACE":
				first = u
			}
		}
	}
	if first == "WITH" {
		return 0, false
	}
	if first == "INSERT" || first == "UPDATE" || first == "DELETE" || first == "REPLACE" {
		for _, w := range words {
			if strings.EqualFold(w.word, "RETURNING") {
				start = w.end
				end = len(sql)
				if !complete {
					return 0, false
				}
			}
		}
		if start < 0 {
			return 0, false
		}
		return countList(sql[start:end])
	}
	prev := ""
	for _, w := range words {
		if start < 0 && strings.EqualFold(w.word, "SELECT") {
			start, prev = w.end, ""
			continue
		}
		// IS [NOT] DISTINCT FROM is a comparison inside a select item, not
		// the list's end.
		if start >= 0 && isListEnd(w.word) && (!strings.EqualFold(w.word, "FROM") || !strings.EqualFold(prev, "DISTINCT")) {
			end = w.start
			break
		}
		prev = w.word
	}
	if start < 0 {
		return 0, false
	}
	if end < 0 {
		if !complete {
			return 0, false
		}
		end = len(sql)
	}
	return countList(sql[start:end])
}

// isListEnd says whether a top-level word ends a select list.
func isListEnd(w string) bool {
	switch strings.ToUpper(w) {
	case "FROM", "WHERE", "GROUP", "HAVING", "ORDER", "LIMIT", "OFFSET", "FETCH", "UNION", "INTERSECT", "EXCEPT", "WINDOW", "FOR":
		return true
	}
	return false
}

type sqlWord struct {
	word       string
	start, end int
}

// topLevelWords lists the bare words outside quotes and parentheses.
func topLevelWords(sql string) []sqlWord {
	var out []sqlWord
	depth := 0
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			i = closeQuote(sql, i)
		case c == '(':
			depth++
		case c == ')':
			depth--
		case isWordByte(c):
			j := i
			for j < len(sql) && isWordByte(sql[j]) {
				j++
			}
			if depth == 0 {
				out = append(out, sqlWord{sql[i:j], i, j})
			}
			i = j - 1
		}
	}
	return out
}

func closeQuote(sql string, i int) int {
	q := sql[i]
	for j := i + 1; j < len(sql); j++ {
		if sql[j] == q {
			if j+1 < len(sql) && sql[j+1] == q {
				j++
				continue
			}
			return j
		}
	}
	return len(sql)
}

func isWordByte(c byte) bool {
	return c == '_' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// countList counts top-level comma-separated items; a star's count isn't
// known here.
func countList(list string) (int, bool) {
	n, depth, itemStart := 1, 0, 0
	star := func(item string) bool {
		item = strings.TrimSpace(item)
		return item == "*" || strings.HasSuffix(item, ".*")
	}
	for i := 0; i < len(list); i++ {
		switch c := list[i]; {
		case c == '\'' || c == '"' || c == '`':
			i = closeQuote(list, i)
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			if star(list[itemStart:i]) {
				return 0, false
			}
			n++
			itemStart = i + 1
		}
	}
	last := strings.TrimSpace(list[itemStart:])
	if last == "" || star(last) {
		return 0, false
	}
	if f := strings.Fields(last); star(f[len(f)-1]) {
		return 0, false
	}
	return n, true
}

func stripSQLComments(sql string) string {
	var b strings.Builder
	for i := 0; i < len(sql); i++ {
		switch {
		case sql[i] == '\'' || sql[i] == '"' || sql[i] == '`':
			j := min(closeQuote(sql, i), len(sql)-1)
			b.WriteString(sql[i : j+1])
			i = j
		case strings.HasPrefix(sql[i:], "--"):
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		case strings.HasPrefix(sql[i:], "/*"):
			end := strings.Index(sql[i:], "*/")
			if end < 0 {
				return b.String()
			}
			i += end + 1
			b.WriteByte(' ')
		default:
			b.WriteByte(sql[i])
		}
	}
	return b.String()
}

// scanCount is how many arguments a Scan call takes; false when it spreads.
func scanCount(call *ast.CallExpr) (int, bool) {
	if call.Ellipsis != token.NoPos {
		return 0, false
	}
	return len(call.Args), true
}

func isScan(call *ast.CallExpr) (ast.Expr, bool) {
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Scan" {
		return nil, false
	}
	return sel.X, true
}

// helperScans is, per function in the package, how many columns it Scans from
// each parameter that it Scans exactly once.
func helperScans(pass *analysis.Pass, decls map[*types.Func]*ast.FuncDecl) map[*types.Func]map[int]int {
	out := map[*types.Func]map[int]int{}
	for fn, fd := range decls {
		params := map[types.Object]int{}
		i := 0
		for _, field := range fd.Type.Params.List {
			for _, name := range field.Names {
				params[pass.TypesInfo.Defs[name]] = i
				i++
			}
			if len(field.Names) == 0 {
				i++
			}
		}
		seen := map[int]int{}
		counts := map[int]int{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			x, ok := isScan(call)
			if !ok {
				return true
			}
			id, ok := ast.Unparen(x).(*ast.Ident)
			if !ok {
				return true
			}
			idx, ok := params[pass.TypesInfo.Uses[id]]
			if !ok {
				return true
			}
			seen[idx]++
			if n, ok := scanCount(call); ok {
				counts[idx] = n
			} else {
				seen[idx] = 2 // 2 is a sentinel: a spread's count is unknown, so the parameter is skipped below.
			}
			return true
		})
		for idx, times := range seen {
			if times != 1 {
				continue
			}
			if out[fn] == nil {
				out[fn] = map[int]int{}
			}
			out[fn][idx] = counts[idx]
		}
	}
	return out
}

type queryBind struct {
	pos   token.Pos
	known bool
	n     int
	scope ast.Node
}

func runSQLScan(pass *analysis.Pass) (any, error) {
	r := newReporter(pass)
	decls := funcDecls(pass)
	helpers := helperScans(pass, decls)
	for _, fd := range decls {
		if isTestFile(pass, fileOf(pass, fd)) {
			continue
		}
		checkScans(pass, r, helpers, fd)
	}
	return nil, nil
}

func fileOf(pass *analysis.Pass, fd *ast.FuncDecl) *ast.File {
	for _, f := range pass.Files {
		if f.Pos() <= fd.Pos() && fd.End() <= f.End() {
			return f
		}
	}
	return pass.Files[0]
}

func checkScans(pass *analysis.Pass, r *reporter, helpers map[*types.Func]map[int]int, fd *ast.FuncDecl) {
	locals := singleAssignments(pass, fd)
	binds := map[types.Object][]queryBind{}
	// bindAt is the query obj holds at pos: the latest assignment before
	// pos, back to one whose block holds pos (so every path passes it).
	// Assignments in branches between must agree, or no width is known.
	bindAt := func(obj types.Object, pos token.Pos) (queryBind, bool) {
		var before []queryBind
		for _, b := range binds[obj] {
			if b.pos < pos {
				before = append(before, b)
			}
		}
		sort.Slice(before, func(i, j int) bool { return before[i].pos > before[j].pos })
		var res queryBind
		for i, b := range before {
			if i == 0 {
				res = b
			} else if b.known != res.known || b.n != res.n {
				return queryBind{}, false
			}
			if b.scope == nil || (b.scope.Pos() <= pos && pos < b.scope.End()) {
				break
			}
		}
		return res, len(before) > 0
	}
	mismatch := func(pos token.Pos, sel, scanned int) {
		r.report(pos, "the query selects %d column%s but %d %s scanned", sel, plural(sel), scanned, map[bool]string{true: "is", false: "are"}[scanned == 1])
	}

	var stack []ast.Node
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			return true
		}
		call, ok := ast.Unparen(as.Rhs[0]).(*ast.CallExpr)
		if !ok || len(as.Lhs) == 0 {
			return true
		}
		_, known, isQuery, cols := querySQL(pass, locals, call)
		if !isQuery {
			return true
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		obj := pass.TypesInfo.Defs[id]
		if obj == nil {
			obj = pass.TypesInfo.Uses[id]
		}
		if obj != nil {
			binds[obj] = append(binds[obj], queryBind{pos: as.End(), known: known, n: cols, scope: innerBlock(stack)})
		}
		return true
	})

	source := func(e ast.Expr, pos token.Pos) (queryBind, bool) {
		switch x := ast.Unparen(e).(type) {
		case *ast.CallExpr:
			_, known, isQuery, cols := querySQL(pass, locals, x)
			return queryBind{known: known, n: cols}, isQuery
		case *ast.Ident:
			return bindAt(pass.TypesInfo.Uses[x], pos)
		}
		return queryBind{}, false
	}

	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if x, ok := isScan(call); ok {
			if src, ok := source(x, call.Pos()); ok && src.known {
				if got, ok := scanCount(call); ok && got != src.n {
					mismatch(call.Pos(), src.n, got)
				}
			}
			return true
		}
		c := callee(pass, call)
		if c == nil || helpers[c] == nil {
			return true
		}
		for idx, want := range helpers[c] {
			if idx >= len(call.Args) {
				continue
			}
			if src, ok := source(call.Args[idx], call.Pos()); ok && src.known && src.n != want {
				mismatch(call.Pos(), src.n, want)
			}
		}
		return true
	})
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// singleAssignments maps locals assigned exactly once, by `x := expr`, to
// that expression.
func singleAssignments(pass *analysis.Pass, fd *ast.FuncDecl) map[types.Object]ast.Expr {
	out := map[types.Object]ast.Expr{}
	assigned := map[types.Object]int{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			if obj := pass.TypesInfo.Defs[id]; obj != nil {
				assigned[obj]++
				if as.Tok == token.DEFINE && len(as.Rhs) == len(as.Lhs) {
					out[obj] = as.Rhs[i]
				}
			} else if obj := pass.TypesInfo.Uses[id]; obj != nil {
				assigned[obj]++
			}
		}
		return true
	})
	for obj := range out {
		if assigned[obj] != 1 {
			delete(out, obj)
		}
	}
	return out
}

// SchemaChain: a model's strict mode rejects a JSON schema that leaves a
// property out of required, and the answer fails only when the call is made
// (WP-662, 10 days in production). schema_strict_test.go checks every schema
// in llmSchemas; this checks that every schema sent to a model is in that map.
var SchemaChain = &analysis.Analyzer{
	Name: "schemachain",
	Doc:  "every JSON schema passed to runChain must be in llmSchemas so the strict-mode test walks it (WP-662)",
	Run:  runSchemaChain,
}

func runSchemaChain(pass *analysis.Pass) (any, error) {
	if !inPkg(pass.Pkg.Path(), "internal/core") {
		return nil, nil
	}
	decls := funcDecls(pass)
	var mapDecl *ast.FuncDecl
	hasTests := false
	for _, f := range pass.Files {
		hasTests = hasTests || isTestFile(pass, f)
	}
	for fn, fd := range decls {
		if fn.Name() == "llmSchemas" {
			mapDecl = fd
		}
	}
	if !hasTests {
		return nil, nil // the package's test variant has the map; this pass runs on that too
	}
	listed := map[types.Object]bool{}
	if mapDecl != nil {
		ast.Inspect(mapDecl.Body, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				if obj := pass.TypesInfo.Uses[id]; pkgLevel(obj) {
					listed[obj] = true
				}
			}
			return true
		})
	}

	type slot struct {
		fn  *types.Func
		idx int
	}
	sinks := map[slot]bool{}
	for fn, fd := range decls {
		if fn.Name() == "runChain" || fn.Name() == "runChainAt" {
			if i := paramIndex(fd, "schema"); i >= 0 {
				sinks[slot{fn, i}] = true
			}
		}
	}
	type site struct {
		fd  *ast.FuncDecl
		arg ast.Expr
	}
	var sites []site
	seen := map[slot]bool{}
	for changed := true; changed; {
		changed = false
		for fn, fd := range decls {
			if isTestFile(pass, fileOf(pass, fd)) {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				c := callee(pass, call)
				for s := range sinks {
					if s.fn != c || s.idx >= len(call.Args) {
						continue
					}
					arg := ast.Unparen(call.Args[s.idx])
					if id, ok := arg.(*ast.Ident); ok {
						if i := paramObjectIndex(pass, fd, pass.TypesInfo.Uses[id]); i >= 0 {
							up := slot{fn, i}
							if !sinks[up] {
								sinks[up] = true
								changed = true
							}
							continue
						}
					}
					if !seen[slot{c, int(call.Pos())}] {
						seen[slot{c, int(call.Pos())}] = true
						sites = append(sites, site{fd, arg})
					}
				}
				return true
			})
		}
	}

	r := newReporter(pass)
	for _, s := range sites {
		objs := schemaObjects(pass, s.fd, s.arg)
		if objs == nil {
			r.report(s.arg.Pos(), "this schema sent to a model isn't a package-level schema, so TestModelSchemasStrict can't walk it: name one and add it to llmSchemas")
			continue
		}
		for _, obj := range objs {
			if !listed[obj] {
				r.report(s.arg.Pos(), "%s is sent to a model but isn't in llmSchemas (schema_strict_test.go), so strict mode isn't checked", obj.Name())
			}
		}
	}
	return nil, nil
}

func paramIndex(fd *ast.FuncDecl, name string) int {
	i := 0
	for _, field := range fd.Type.Params.List {
		for _, id := range field.Names {
			if id.Name == name {
				return i
			}
			i++
		}
	}
	return -1
}

func paramObjectIndex(pass *analysis.Pass, fd *ast.FuncDecl, obj types.Object) int {
	if obj == nil {
		return -1
	}
	i := 0
	for _, field := range fd.Type.Params.List {
		for _, id := range field.Names {
			if pass.TypesInfo.Defs[id] == obj {
				return i
			}
			i++
		}
	}
	return -1
}

// schemaObjects resolves a schema expression to the package-level schemas it
// can be: a name, or a local assigned only from names. nil when it can be
// anything else.
func schemaObjects(pass *analysis.Pass, fd *ast.FuncDecl, e ast.Expr) []types.Object {
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return nil
	}
	obj := pass.TypesInfo.Uses[id]
	if pkgLevel(obj) {
		return []types.Object{obj}
	}
	var out []types.Object
	valid := true
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			l, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			if pass.TypesInfo.Defs[l] != obj && pass.TypesInfo.Uses[l] != obj {
				continue
			}
			found = true
			if len(as.Rhs) != len(as.Lhs) {
				valid = false
				continue
			}
			rhs, ok := ast.Unparen(as.Rhs[i]).(*ast.Ident)
			if !ok || !pkgLevel(pass.TypesInfo.Uses[rhs]) {
				valid = false
				continue
			}
			out = append(out, pass.TypesInfo.Uses[rhs])
		}
		return true
	})
	if !valid || !found {
		return nil
	}
	return out
}

// innerBlock is the innermost block on stack: a block statement or a case's
// body.
func innerBlock(stack []ast.Node) ast.Node {
	for i := len(stack) - 1; i >= 0; i-- {
		switch stack[i].(type) {
		case *ast.BlockStmt, *ast.CaseClause, *ast.CommClause:
			return stack[i]
		}
	}
	return nil
}
