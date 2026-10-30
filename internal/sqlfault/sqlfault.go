// Package sqlfault is a fault-injecting database/sql driver for tests: it wraps the real SQLite
// driver and fails the nth statement, commit, or row of a query that matches a substring of its
// SQL. It reaches the error paths only a broken disk or driver would (H20): a failed write
// mid-transaction, a scan or iteration error mid-result, a PRAGMA that answers something
// surprising.
//
// Each Driver owns its state and registers under its own database/sql name, so the sqlite store
// and the embedding cache arm faults independently. A Driver's armed fault is shared by every
// connection opened through it, so tests that arm one run sequentially (no t.Parallel) and
// disarm at cleanup. It is test-support code: production binaries never import it.
package sqlfault

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// ErrFault is the error every injected failure carries (unless a hook returns its own).
var ErrFault = errors.New("injected fault")

// RowFault says where in a result the row fault lands.
type RowFault int

const (
	// RowsIter fails Rows.Next at the nth row, so Rows.Err reports it.
	RowsIter RowFault = iota + 1
	// RowsScan gives the nth row a value no destination accepts, so Scan fails.
	RowsScan
)

// Driver is one registered fault driver.
type Driver struct {
	name string

	mu      sync.Mutex
	match   string
	nth     int
	seen    int
	always  bool
	hook    func() error // when set, runs at the matched statement and its error is returned
	rewrite struct {
		match string
		vals  []string
	}
	rows struct {
		match string
		nth   int
		mode  RowFault
	}
}

// Register registers the driver under name (once per process; name must be unique) and returns
// it. Call it from a package-level var initialiser in the test package.
func Register(name string) *Driver {
	d := &Driver{name: name}
	sql.Register(name, faultDriver{d: d, inner: &sqlite3.SQLiteDriver{}})
	return d
}

// Name is the database/sql driver name to open connections with.
func (d *Driver) Name() string { return d.name }

// Fail arms the fault for the nth (1-based) statement containing match ("COMMIT" matches
// Tx.Commit, "BEGIN" a transaction start, "PREPARE <sql>" a prepare) and disarms at cleanup.
func (d *Driver) Fail(t testing.TB, match string, nth int) {
	t.Helper()
	d.Arm(match, nth)
	t.Cleanup(d.Disarm)
}

// FailAlways makes every statement containing match fail, until cleanup.
func (d *Driver) FailAlways(t testing.TB, match string) {
	t.Helper()
	d.Arm(match, 1)
	d.mu.Lock()
	d.always = true
	d.mu.Unlock()
	t.Cleanup(d.Disarm)
}

// Arm is Fail without the cleanup, for a test that re-arms inside a loop. "" disarms.
func (d *Driver) Arm(match string, nth int) {
	d.mu.Lock()
	d.match, d.nth, d.seen, d.always, d.hook = match, nth, 0, false, nil
	d.mu.Unlock()
}

// Disarm clears the statement fault (and its hook).
func (d *Driver) Disarm() { d.Arm("", 0) }

// SetHook makes the armed fault call h at the matched statement and return its error instead of
// ErrFault (h may return nil to let the statement run, e.g. after cancelling a context). Arm or
// Fail first: arming clears the hook.
func (d *Driver) SetHook(h func() error) {
	d.mu.Lock()
	d.hook = h
	d.mu.Unlock()
}

// Rewrite makes the first len(vals) queries containing match answer one row, one column with
// vals in order instead of running; afterwards queries run for real. A PRAGMA can "succeed" with
// a surprising value.
func (d *Driver) Rewrite(t testing.TB, match string, vals ...string) {
	t.Helper()
	d.mu.Lock()
	d.rewrite.match, d.rewrite.vals = match, vals
	d.mu.Unlock()
	t.Cleanup(func() {
		d.mu.Lock()
		d.rewrite.match, d.rewrite.vals = "", nil
		d.mu.Unlock()
	})
}

// FailRows makes the nth (1-based) row of the first query containing match fail, at Rows.Next
// (RowsIter) or at Scan (RowsScan). It reaches the scan and iteration error branches that a
// statement-level fault cannot. It disarms after firing and at cleanup.
func (d *Driver) FailRows(t testing.TB, match string, nth int, mode RowFault) {
	t.Helper()
	d.mu.Lock()
	d.rows.match, d.rows.nth, d.rows.mode = match, nth, mode
	d.mu.Unlock()
	t.Cleanup(func() {
		d.mu.Lock()
		d.rows.match, d.rows.nth, d.rows.mode = "", 0, 0
		d.mu.Unlock()
	})
}

// rewritten reports the canned answer for query, if one is due.
func (d *Driver) rewritten(query string) (driver.Rows, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := &d.rewrite
	if r.match == "" || !strings.Contains(query, r.match) || len(r.vals) == 0 {
		return nil, false
	}
	v := r.vals[0]
	r.vals = r.vals[1:]
	return &oneRow{col: "value", val: v}, true
}

// wrapRows returns rows wrapped to fail at the armed row when query matches, and disarms.
func (d *Driver) wrapRows(query string, rows driver.Rows) driver.Rows {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := &d.rows
	if r.match == "" || !strings.Contains(query, r.match) {
		return rows
	}
	f := &faultRows{Rows: rows, nth: r.nth, mode: r.mode}
	r.match, r.nth, r.mode = "", 0, 0
	return f
}

// faulty is the error to inject for query, nil to run it. A hook runs outside the lock, so it
// may call back into the Driver.
func (d *Driver) faulty(query string) error {
	d.mu.Lock()
	if d.match == "" || !strings.Contains(query, d.match) {
		d.mu.Unlock()
		return nil
	}
	d.seen++
	if d.seen != d.nth && !(d.always && d.seen > d.nth) {
		d.mu.Unlock()
		return nil
	}
	hook := d.hook
	d.mu.Unlock()
	if hook != nil {
		return hook()
	}
	return ErrFault
}

type oneRow struct {
	col  string
	val  string
	done bool
}

func (r *oneRow) Columns() []string { return []string{r.col} }
func (r *oneRow) Close() error      { return nil }
func (r *oneRow) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done, dest[0] = true, r.val
	return nil
}

type faultRows struct {
	driver.Rows
	nth  int
	mode RowFault
	seen int
}

func (r *faultRows) Next(dest []driver.Value) error {
	if err := r.Rows.Next(dest); err != nil {
		return err
	}
	r.seen++
	if r.seen != r.nth {
		return nil
	}
	if r.mode == RowsIter {
		return ErrFault
	}
	for i := range dest {
		dest[i] = struct{ bad bool }{true} // database/sql cannot convert this into any destination
	}
	return nil
}

type faultDriver struct {
	d     *Driver
	inner driver.Driver
}

func (d faultDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return faultConn{d.d, c.(*sqlite3.SQLiteConn)}, nil
}

type faultConn struct {
	d *Driver
	c *sqlite3.SQLiteConn
}

func (f faultConn) Prepare(q string) (driver.Stmt, error) {
	return f.PrepareContext(context.Background(), q)
}
func (f faultConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	if err := f.d.faulty("PREPARE " + q); err != nil {
		return nil, err
	}
	s, err := f.c.PrepareContext(ctx, q)
	if err != nil {
		return nil, err
	}
	return faultStmt{f.d, s, q}, nil
}
func (f faultConn) Close() error { return f.c.Close() }
func (f faultConn) Begin() (driver.Tx, error) {
	return f.BeginTx(context.Background(), driver.TxOptions{})
}
func (f faultConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	if err := f.d.faulty("BEGIN"); err != nil {
		return nil, err
	}
	tx, err := f.c.BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return faultTx{f.d, tx}, nil
}
func (f faultConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if err := f.d.faulty(q); err != nil {
		return nil, err
	}
	return f.c.ExecContext(ctx, q, args)
}
func (f faultConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if rows, ok := f.d.rewritten(q); ok {
		return rows, nil
	}
	if err := f.d.faulty(q); err != nil {
		return nil, err
	}
	rows, err := f.c.QueryContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	return f.d.wrapRows(q, rows), nil
}
func (f faultConn) Ping(ctx context.Context) error { return f.c.Ping(ctx) }

type faultTx struct {
	d  *Driver
	tx driver.Tx
}

func (t faultTx) Commit() error {
	if err := t.d.faulty("COMMIT"); err != nil {
		_ = t.tx.Rollback() // the injected commit failure leaves nothing applied
		return err
	}
	return t.tx.Commit()
}
func (t faultTx) Rollback() error { return t.tx.Rollback() }

type faultStmt struct {
	d *Driver
	s driver.Stmt
	q string
}

func (s faultStmt) Close() error  { return s.s.Close() }
func (s faultStmt) NumInput() int { return s.s.NumInput() }
func (s faultStmt) Exec(args []driver.Value) (driver.Result, error) {
	return nil, errors.New("faultStmt: Exec without context is not used")
}
func (s faultStmt) Query(args []driver.Value) (driver.Rows, error) {
	return nil, errors.New("faultStmt: Query without context is not used")
}
func (s faultStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if err := s.d.faulty(s.q); err != nil {
		return nil, err
	}
	return s.s.(driver.StmtExecContext).ExecContext(ctx, args)
}
func (s faultStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if err := s.d.faulty(s.q); err != nil {
		return nil, err
	}
	rows, err := s.s.(driver.StmtQueryContext).QueryContext(ctx, args)
	if err != nil {
		return nil, err
	}
	return s.d.wrapRows(s.q, rows), nil
}
