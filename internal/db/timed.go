package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// WP-1946: every statement and write transaction is timed by wrapping
// modernc's driver, so a slow one is logged with its caller and no caller has
// to change. modernc does a query's work while rows are stepped, so a query
// is timed from the call to Rows.Close.
const (
	defaultSlow = time.Second
	totalsFloor = 50 * time.Millisecond
	maxCallers  = 500
	queryCut    = 300
	stackDepth  = 24
)

// SlowStatement is one statement or write transaction that took longer than
// the threshold. It never holds the arguments.
type SlowStatement struct {
	Duration time.Duration
	Pool     string // "read" or "write"
	Tx       bool   // a write transaction, begin to commit
	Caller   string // internal/core/x.go:412 (core.(*Service).f)
	Query    string // one line, cut at queryCut characters; empty for a transaction
	// AfterFirstRow is set when most of the time passed after the first row
	// came back, which includes the caller's own work between rows.
	AfterFirstRow bool
}

func (s SlowStatement) String() string {
	d := s.Duration.Round(time.Millisecond)
	if d >= time.Second {
		d = s.Duration.Round(10 * time.Millisecond)
	}
	if s.Tx {
		return fmt.Sprintf("slow write transaction: %s %s", d, s.Caller)
	}
	line := fmt.Sprintf("slow statement: %s %s %s: %s", d, s.Pool, s.Caller, s.Query)
	if s.AfterFirstRow {
		line += " (most of the time passed after the first row, including the caller's work between rows)"
	}
	return line
}

// CallerTotal is what one caller's statements over the floor added up to.
type CallerTotal struct {
	Pool    string // "read", "write" or "write transaction"
	Caller  string
	Count   int
	Total   time.Duration
	Longest time.Duration
}

type monitor struct {
	threshold time.Duration
	floor     time.Duration
	handler   atomic.Pointer[func(SlowStatement)]
	mu        sync.Mutex
	callers   map[string]*CallerTotal
	dropped   int
}

// slowThreshold reads WAYPOINT_SLOW_STATEMENT (a Go duration, 0 turns the
// timing off), once, at Open.
func slowThreshold() time.Duration {
	v := strings.TrimSpace(os.Getenv("WAYPOINT_SLOW_STATEMENT"))
	if v == "" {
		return defaultSlow
	}
	if v == "0" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return defaultSlow
	}
	return d
}

// span is the start of one timed call: the time and the caller's stack, in a
// fixed array so that taking it allocates nothing.
type span struct {
	start time.Time
	pcs   [stackDepth]uintptr
	n     int
}

func (sp *span) begin() {
	sp.start = time.Now()
	sp.n = runtime.Callers(2, sp.pcs[:])
}

// finish records the call if it was slow enough to log or to count.
func (m *monitor) finish(sp *span, pool string, tx bool, query string, first time.Time) {
	d := time.Since(sp.start)
	if d < min(m.floor, m.threshold) {
		return
	}
	caller := callerOf(sp.pcs[:sp.n])
	kind := pool
	if tx {
		kind = "write transaction"
	}
	if d >= m.floor {
		m.add(kind, caller, d)
	}
	if d < m.threshold {
		return
	}
	h := m.handler.Load()
	if h == nil {
		return
	}
	ss := SlowStatement{Duration: d, Pool: pool, Tx: tx, Caller: caller}
	if !tx {
		ss.Query = oneLine(query)
		ss.AfterFirstRow = !first.IsZero() && time.Since(first) > d/2
	}
	(*h)(ss)
}

func (m *monitor) add(kind, caller string, d time.Duration) {
	key := kind + " " + caller
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.callers[key]
	if t == nil {
		if len(m.callers) >= maxCallers {
			m.dropped++
			return
		}
		t = &CallerTotal{Pool: kind, Caller: caller}
		m.callers[key] = t
	}
	t.Count++
	t.Total += d
	t.Longest = max(t.Longest, d)
}

// skipFrame is a frame that is not the caller: database/sql, the runtime, this
// wrapper, and the write helper whose caller is the useful frame of a write
// transaction.
func skipFrame(fn string) bool {
	return strings.HasPrefix(fn, "database/sql.") || strings.HasPrefix(fn, "runtime.") ||
		strings.Contains(fn, "internal/db.(*timed") || strings.Contains(fn, "internal/db.timed") ||
		strings.Contains(fn, "internal/db.(*span)") ||
		strings.HasSuffix(fn, "internal/core.(*Service).write")
}

func callerOf(pcs []uintptr) string {
	if len(pcs) == 0 {
		return "unknown"
	}
	frames := runtime.CallersFrames(pcs)
	for {
		f, more := frames.Next()
		if f.Function != "" && !skipFrame(f.Function) {
			file := f.File
			for _, root := range []string{"/internal/", "/cmd/"} {
				if i := strings.LastIndex(file, root); i >= 0 {
					file = file[i+1:]
					break
				}
			}
			fn := f.Function
			fn = fn[strings.LastIndex(fn, "/")+1:]
			return fmt.Sprintf("%s:%d (%s)", file, f.Line, fn)
		}
		if !more {
			return "unknown"
		}
	}
}

func oneLine(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	if r := []rune(q); len(r) > queryCut {
		q = string(r[:queryCut]) + "…"
	}
	return q
}

var sqliteDriver = sync.OnceValue(func() driver.Driver {
	d, err := sql.Open("sqlite", "")
	if err != nil {
		panic(err)
	}
	defer func() { _ = d.Close() }()
	return d.Driver()
})

type timedConnector struct {
	dsn  string
	pool string
	mon  *monitor
}

func (c *timedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	inner, err := sqliteDriver().Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &timedConn{Conn: inner, pool: c.pool, mon: c.mon}, nil
}

func (c *timedConnector) Driver() driver.Driver { return sqliteDriver() }

// openPool opens one pool, timed unless WAYPOINT_SLOW_STATEMENT is 0.
func openPool(dsn, pool string, mon *monitor) *sql.DB {
	if mon.threshold <= 0 {
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			panic(err)
		}
		return db
	}
	return sql.OpenDB(&timedConnector{dsn: dsn, pool: pool, mon: mon})
}

// timedConn forwards every optional interface modernc's conn has that
// database/sql uses (TestTimedDriverForwardsInterfaces keeps that true).
type timedConn struct {
	driver.Conn
	pool string
	mon  *monitor
}

func (c *timedConn) Prepare(q string) (driver.Stmt, error) {
	st, err := c.Conn.Prepare(q)
	if err != nil {
		return nil, err
	}
	return &timedStmt{Stmt: st, query: q, c: c}, nil
}

func (c *timedConn) Begin() (driver.Tx, error) { //nolint:staticcheck // driver.Conn requires it
	t, err := c.Conn.Begin() //nolint:staticcheck
	return c.wrapTx(t, err, nil)
}

func (c *timedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.pool != "write" {
		return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	}
	var sp span
	sp.begin()
	t, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	return c.wrapTx(t, err, &sp)
}

func (c *timedConn) wrapTx(t driver.Tx, err error, sp *span) (driver.Tx, error) {
	if err != nil || c.pool != "write" {
		return t, err
	}
	tt := &timedTx{Tx: t, mon: c.mon}
	if sp != nil {
		tt.sp = *sp
	} else {
		tt.sp.begin()
	}
	return tt, nil
}

func (c *timedConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	st, err := c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, q)
	if err != nil {
		return nil, err
	}
	return &timedStmt{Stmt: st, query: q, c: c}, nil
}

func (c *timedConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	var sp span
	sp.begin()
	r, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
	if err != driver.ErrSkip {
		c.mon.finish(&sp, c.pool, false, q, time.Time{})
	}
	return r, err
}

func (c *timedConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	var sp span
	sp.begin()
	r, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if err != nil {
		if err != driver.ErrSkip {
			c.mon.finish(&sp, c.pool, false, q, time.Time{})
		}
		return r, err
	}
	return &timedRows{Rows: r, c: c, sp: sp, query: q}, nil
}

func (c *timedConn) Ping(ctx context.Context) error {
	return c.Conn.(driver.Pinger).Ping(ctx)
}

func (c *timedConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}

func (c *timedConn) IsValid() bool { return c.Conn.(driver.Validator).IsValid() }

type timedStmt struct {
	driver.Stmt
	query string
	c     *timedConn
}

func (s *timedStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	var sp span
	sp.begin()
	r, err := s.Stmt.(driver.StmtExecContext).ExecContext(ctx, args)
	s.c.mon.finish(&sp, s.c.pool, false, s.query, time.Time{})
	return r, err
}

func (s *timedStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	var sp span
	sp.begin()
	r, err := s.Stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
	if err != nil {
		s.c.mon.finish(&sp, s.c.pool, false, s.query, time.Time{})
		return r, err
	}
	return &timedRows{Rows: r, c: s.c, sp: sp, query: s.query}, nil
}

type timedRows struct {
	driver.Rows
	c      *timedConn
	sp     span
	query  string
	first  time.Time
	closed bool
}

func (r *timedRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	if err == nil && r.first.IsZero() {
		r.first = time.Now()
	}
	return err
}

func (r *timedRows) Close() error {
	err := r.Rows.Close()
	if !r.closed {
		r.closed = true
		r.c.mon.finish(&r.sp, r.c.pool, false, r.query, r.first)
	}
	return err
}

type timedTx struct {
	driver.Tx
	mon  *monitor
	sp   span
	done bool
}

func (t *timedTx) end() {
	if !t.done {
		t.done = true
		t.mon.finish(&t.sp, "write", true, "", time.Time{})
	}
}

func (t *timedTx) Commit() error {
	err := t.Tx.Commit()
	t.end()
	return err
}

func (t *timedTx) Rollback() error {
	err := t.Tx.Rollback()
	t.end()
	return err
}

// The column type interfaces modernc's rows have, which database/sql reads
// through ColumnTypes.
func (r *timedRows) ColumnTypeDatabaseTypeName(i int) string {
	if v, ok := r.Rows.(driver.RowsColumnTypeDatabaseTypeName); ok {
		return v.ColumnTypeDatabaseTypeName(i)
	}
	return ""
}

func (r *timedRows) ColumnTypeLength(i int) (int64, bool) {
	if v, ok := r.Rows.(driver.RowsColumnTypeLength); ok {
		return v.ColumnTypeLength(i)
	}
	return 0, false
}

func (r *timedRows) ColumnTypeNullable(i int) (bool, bool) {
	if v, ok := r.Rows.(driver.RowsColumnTypeNullable); ok {
		return v.ColumnTypeNullable(i)
	}
	return false, false
}

func (r *timedRows) ColumnTypePrecisionScale(i int) (int64, int64, bool) {
	if v, ok := r.Rows.(driver.RowsColumnTypePrecisionScale); ok {
		return v.ColumnTypePrecisionScale(i)
	}
	return 0, 0, false
}

func (r *timedRows) ColumnTypeScanType(i int) reflect.Type {
	if v, ok := r.Rows.(driver.RowsColumnTypeScanType); ok {
		return v.ColumnTypeScanType(i)
	}
	return nil
}

// SetSlowHandler sets what receives a statement or write transaction over the
// threshold. The server logs it.
func (d *DB) SetSlowHandler(f func(SlowStatement)) {
	if f == nil {
		d.mon.handler.Store(nil)
		return
	}
	d.mon.handler.Store(&f)
}

// SlowCallers is the n callers whose statements over 50 ms took the most time
// in total since the server started, and how many such statements and write
// transactions were not counted because their caller did not fit.
func (d *DB) SlowCallers(n int) (top []CallerTotal, dropped int) {
	d.mon.mu.Lock()
	for _, t := range d.mon.callers {
		top = append(top, *t)
	}
	dropped = d.mon.dropped
	d.mon.mu.Unlock()
	sort.Slice(top, func(i, j int) bool { return top[i].Total > top[j].Total })
	return slices.Clip(top[:min(n, len(top))]), dropped
}

// Slow reports the threshold, 0 when timing is off.
func (d *DB) Slow() time.Duration { return d.mon.threshold }
