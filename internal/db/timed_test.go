package db

import (
	"context"
	"database/sql/driver"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const slowCTE = `WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 600000)
	SELECT count(*) FROM c WHERE x != ?`

func openSlow(t *testing.T) (*DB, *[]SlowStatement) {
	t.Helper()
	t.Setenv("WAYPOINT_SLOW_STATEMENT", "20ms")
	d, err := Open(context.Background(), filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	d.mon.floor = 20 * time.Millisecond
	d.mon.callers = map[string]*CallerTotal{} // what migrating recorded
	var mu sync.Mutex
	var got []SlowStatement
	d.SetSlowHandler(func(s SlowStatement) { mu.Lock(); got = append(got, s); mu.Unlock() })
	return d, &got
}

func TestSlowStatementRecordsCaller(t *testing.T) {
	d, got := openSlow(t)
	ctx := context.Background()
	var n int
	if err := d.R.QueryRowContext(ctx, slowCTE, "distinctive-arg-8841").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 {
		t.Fatalf("want one record, got %d: %v", len(*got), *got)
	}
	s := (*got)[0]
	if s.Pool != "read" || s.Tx || !strings.Contains(s.Caller, "TestSlowStatementRecordsCaller") || !strings.Contains(s.Caller, "timed_test.go:") {
		t.Errorf("caller: %+v", s)
	}
	if !strings.Contains(s.Query, "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL") || strings.Contains(s.Query, "\n") || strings.Contains(s.Query, "\t") {
		t.Errorf("query text: %q", s.Query)
	}
	if line := s.String(); strings.Contains(line, "distinctive-arg-8841") || !strings.HasPrefix(line, "slow statement: ") || !strings.Contains(line, " read ") {
		t.Errorf("line: %s", line)
	}
	if top, _ := d.SlowCallers(15); len(top) != 1 || top[0].Count != 1 || top[0].Total < 20*time.Millisecond {
		t.Errorf("totals: %+v", top)
	}

	// A prepared statement is timed the same way.
	*got = nil
	st, err := d.R.PrepareContext(ctx, slowCTE)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.QueryRowContext(ctx, "other-arg-5512").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || strings.Contains((*got)[0].String(), "other-arg-5512") {
		t.Errorf("prepared: %v", *got)
	}
}

func TestSlowStatementFastIsQuiet(t *testing.T) {
	d, got := openSlow(t)
	d.mon.threshold = time.Second
	var n int
	if err := d.R.QueryRowContext(context.Background(), `SELECT 1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 0 {
		t.Errorf("a fast statement was recorded: %v", *got)
	}
	if top, _ := d.SlowCallers(15); len(top) != 0 {
		t.Errorf("totals: %+v", top)
	}
}

func TestSlowStatementWorkBetweenRows(t *testing.T) {
	d, got := openSlow(t)
	rows, err := d.R.QueryContext(context.Background(), `SELECT 1 UNION ALL SELECT 2`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		time.Sleep(40 * time.Millisecond)
	}
	rows.Close()
	if len(*got) != 1 || !(*got)[0].AfterFirstRow || !strings.Contains((*got)[0].String(), "caller's work between rows") {
		t.Fatalf("got %v", *got)
	}
}

func TestSlowStatementWriteTransaction(t *testing.T) {
	d, got := openSlow(t)
	ctx := context.Background()
	hold := func() {
		tx, err := d.W.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(60 * time.Millisecond)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	hold()
	if len(*got) != 1 {
		t.Fatalf("want the transaction only: %v", *got)
	}
	s := (*got)[0]
	if !s.Tx || s.Pool != "write" || !strings.Contains(s.Caller, "TestSlowStatementWriteTransaction") {
		t.Errorf("tx: %+v", s)
	}
	if !strings.HasPrefix(s.String(), "slow write transaction: ") {
		t.Errorf("line: %s", s)
	}
	top, _ := d.SlowCallers(15)
	if len(top) != 1 || top[0].Pool != "write transaction" {
		t.Errorf("totals: %+v", top)
	}

	*got = nil
	tx, _ := d.W.BeginTx(ctx, nil)
	time.Sleep(40 * time.Millisecond)
	_ = tx.Rollback()
	if len(*got) != 1 || !(*got)[0].Tx {
		t.Errorf("a rollback is timed too: %v", *got)
	}
}

func TestSlowStatementOff(t *testing.T) {
	t.Setenv("WAYPOINT_SLOW_STATEMENT", "0")
	d, err := Open(context.Background(), filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.Slow() != 0 {
		t.Fatal("threshold not off")
	}
	if _, ok := d.W.Driver().(interface {
		Open(string) (driver.Conn, error)
	}); !ok {
		t.Fatal("no driver")
	}
	var n int
	if err := d.R.QueryRow(`SELECT 1`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

// A modernc upgrade that adds an interface to its conn, stmt or rows has to be
// forwarded too, so the wrapper is compared with the driver rather than a list.
func TestTimedDriverForwardsInterfaces(t *testing.T) {
	d, err := Open(context.Background(), filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	inner, err := sqliteDriver().Open(dsn(d.path, true))
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	wrapped, err := (&timedConnector{dsn: dsn(d.path, true), pool: "read", mon: d.mon}).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer wrapped.Close()

	check := func(what string, inner, wrapped any) {
		t.Helper()
		for name, has := range map[string][2]bool{
			"ConnBeginTx":                    assert[driver.ConnBeginTx](inner, wrapped),
			"ConnPrepareContext":             assert[driver.ConnPrepareContext](inner, wrapped),
			"ExecerContext":                  assert[driver.ExecerContext](inner, wrapped),
			"QueryerContext":                 assert[driver.QueryerContext](inner, wrapped),
			"Pinger":                         assert[driver.Pinger](inner, wrapped),
			"SessionResetter":                assert[driver.SessionResetter](inner, wrapped),
			"Validator":                      assert[driver.Validator](inner, wrapped),
			"NamedValueChecker":              assert[driver.NamedValueChecker](inner, wrapped),
			"ColumnConverter":                assert[driver.ColumnConverter](inner, wrapped), //nolint:staticcheck // deprecated, but database/sql still uses it
			"StmtExecContext":                assert[driver.StmtExecContext](inner, wrapped),
			"StmtQueryContext":               assert[driver.StmtQueryContext](inner, wrapped),
			"RowsNextResultSet":              assert[driver.RowsNextResultSet](inner, wrapped),
			"RowsColumnTypeDatabaseTypeName": assert[driver.RowsColumnTypeDatabaseTypeName](inner, wrapped),
			"RowsColumnTypeLength":           assert[driver.RowsColumnTypeLength](inner, wrapped),
			"RowsColumnTypeNullable":         assert[driver.RowsColumnTypeNullable](inner, wrapped),
			"RowsColumnTypePrecisionScale":   assert[driver.RowsColumnTypePrecisionScale](inner, wrapped),
			"RowsColumnTypeScanType":         assert[driver.RowsColumnTypeScanType](inner, wrapped),
		} {
			if has[0] != has[1] {
				t.Errorf("%s: %s: driver has it %v, wrapper %v", what, name, has[0], has[1])
			}
		}
	}
	check("conn", inner, wrapped)

	ist, err := inner.(driver.ConnPrepareContext).PrepareContext(ctx, `SELECT 1`)
	if err != nil {
		t.Fatal(err)
	}
	wst, err := wrapped.(driver.ConnPrepareContext).PrepareContext(ctx, `SELECT 1`)
	if err != nil {
		t.Fatal(err)
	}
	check("stmt", ist, wst)
	irows, err := ist.(driver.StmtQueryContext).QueryContext(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrows, err := wst.(driver.StmtQueryContext).QueryContext(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	check("rows", irows, wrows)
	_ = irows.Close()
	_ = wrows.Close()
}

func assert[T any](inner, wrapped any) [2]bool {
	_, a := inner.(T)
	_, b := wrapped.(T)
	return [2]bool{a, b}
}

func benchmarkQuery(b *testing.B, threshold string) {
	b.Setenv("WAYPOINT_SLOW_STATEMENT", threshold)
	d, err := Open(context.Background(), filepath.Join(b.TempDir(), "w.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	var n int
	b.ResetTimer()
	for b.Loop() {
		if err := d.R.QueryRowContext(ctx, `SELECT 1`).Scan(&n); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStatementWrapped(b *testing.B)   { benchmarkQuery(b, "1s") }
func BenchmarkStatementUnwrapped(b *testing.B) { benchmarkQuery(b, "0") }

func TestSkipFrameKeepsTheCallerOfWrite(t *testing.T) {
	for fn, want := range map[string]bool{
		"git.jmann.me/claude/waypoint/internal/core.(*Service).write":       true,
		"git.jmann.me/claude/waypoint/internal/core.(*Service).writeRows":   false,
		"git.jmann.me/claude/waypoint/internal/core.(*Service).CreateIssue": false,
		"database/sql.(*DB).BeginTx":                                        true,
		"runtime.goexit":                                                    true,
		"git.jmann.me/claude/waypoint/internal/db.(*timedConn).BeginTx":     true,
	} {
		if got := skipFrame(fn); got != want {
			t.Errorf("skipFrame(%q) = %v", fn, got)
		}
	}
}

func TestOneLineCollapsesAndCuts(t *testing.T) {
	got := oneLine("SELECT\n\t a,\n b FROM t WHERE x = '" + strings.Repeat("y", 400) + "'")
	if strings.ContainsAny(got, "\n\t") || strings.HasPrefix(got, "SELECT  ") || !strings.HasPrefix(got, "SELECT a, b FROM t") {
		t.Errorf("not collapsed: %q", got)
	}
	if n := len([]rune(got)); n != queryCut+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("cut to %d runes: %q", n, got)
	}
}
