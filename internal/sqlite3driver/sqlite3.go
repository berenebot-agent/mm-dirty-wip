package sqlite3driver

/*
#cgo LDFLAGS: -lsqlite3
#include <sqlite3.h>
#include <stdlib.h>

static int bind_text_transient(sqlite3_stmt *stmt, int idx, const char *p, int n) {
    return sqlite3_bind_text(stmt, idx, p, n, SQLITE_TRANSIENT);
}
static int bind_blob_transient(sqlite3_stmt *stmt, int idx, const void *p, int n) {
    return sqlite3_bind_blob(stmt, idx, p, n, SQLITE_TRANSIENT);
}
*/
import "C"

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

func init() { sql.Register("sqlite3", &Driver{}) }

type Driver struct{}

type conn struct{ db *C.sqlite3 }
type stmt struct {
	c  *conn
	st *C.sqlite3_stmt
	n  int
	q  string
}
type tx struct {
	c    *conn
	done bool
}
type rows struct {
	st   *C.sqlite3_stmt
	cols []string
	done bool
}
type result struct {
	last     int64
	affected int64
}

func (result result) LastInsertId() (int64, error) { return result.last, nil }
func (result result) RowsAffected() (int64, error) { return result.affected, nil }

func (d *Driver) Open(name string) (driver.Conn, error) {
	path := name
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	path = strings.TrimPrefix(path, "file:")
	if path == "" {
		path = ":memory:"
	}
	cs := C.CString(path)
	defer C.free(unsafe.Pointer(cs))
	var db *C.sqlite3
	flags := C.int(C.SQLITE_OPEN_READWRITE | C.SQLITE_OPEN_CREATE | C.SQLITE_OPEN_FULLMUTEX)
	if rc := C.sqlite3_open_v2(cs, &db, flags, nil); rc != C.SQLITE_OK {
		err := dbErr(db, rc)
		if db != nil {
			C.sqlite3_close_v2(db)
		}
		return nil, err
	}
	c := &conn{db: db}
	for _, q := range []string{
		"PRAGMA foreign_keys=ON",
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := c.exec(context.Background(), q, nil); err != nil {
			c.Close()
			return nil, err
		}
	}
	return c, nil
}

func dbErr(db *C.sqlite3, rc C.int) error {
	if db == nil {
		return fmt.Errorf("sqlite error %d", int(rc))
	}
	return fmt.Errorf("sqlite error %d: %s", int(rc), C.GoString(C.sqlite3_errmsg(db)))
}
func stmtErr(st *C.sqlite3_stmt, rc C.int) error {
	if st == nil {
		return fmt.Errorf("sqlite error %d", int(rc))
	}
	return dbErr(C.sqlite3_db_handle(st), rc)
}

func (c *conn) Prepare(query string) (driver.Stmt, error) { return c.prepare(query) }
func (c *conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.prepare(query)
}
func (c *conn) prepare(query string) (*stmt, error) {
	cq := C.CString(query)
	defer C.free(unsafe.Pointer(cq))
	var st *C.sqlite3_stmt
	if rc := C.sqlite3_prepare_v2(c.db, cq, -1, &st, nil); rc != C.SQLITE_OK {
		return nil, dbErr(c.db, rc)
	}
	if st == nil {
		return nil, errors.New("sqlite: empty statement")
	}
	return &stmt{c: c, st: st, n: int(C.sqlite3_bind_parameter_count(st)), q: query}, nil
}
func (c *conn) Close() error {
	if c.db == nil {
		return nil
	}
	rc := C.sqlite3_close_v2(c.db)
	c.db = nil
	if rc != C.SQLITE_OK {
		return fmt.Errorf("sqlite close: %d", int(rc))
	}
	return nil
}
func (c *conn) Begin() (driver.Tx, error) { return c.BeginTx(context.Background(), driver.TxOptions{}) }
func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.ReadOnly {
		return nil, errors.New("sqlite: read-only transactions not supported")
	}
	if _, err := c.exec(ctx, "BEGIN", nil); err != nil {
		return nil, err
	}
	return &tx{c: c}, nil
}
func (t *tx) Commit() error {
	if t.done {
		return errors.New("sqlite: transaction done")
	}
	t.done = true
	_, e := t.c.exec(context.Background(), "COMMIT", nil)
	return e
}
func (t *tx) Rollback() error {
	if t.done {
		return driver.ErrBadConn
	}
	t.done = true
	_, e := t.c.exec(context.Background(), "ROLLBACK", nil)
	return e
}

func namedValues(args []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, len(args))
	for i, a := range args {
		out[i] = a.Value
	}
	return out
}
func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.exec(ctx, query, namedValues(args))
}
func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.query(ctx, query, namedValues(args))
}

func (c *conn) exec(ctx context.Context, query string, args []driver.Value) (driver.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	remaining := query
	argPos := 0
	var last, affected int64
	for strings.TrimSpace(remaining) != "" {
		cq := C.CString(remaining)
		var st *C.sqlite3_stmt
		var tail *C.char
		rc := C.sqlite3_prepare_v2(c.db, cq, -1, &st, &tail)
		consumed := 0
		if tail != nil {
			consumed = int(uintptr(unsafe.Pointer(tail)) - uintptr(unsafe.Pointer(cq)))
		}
		if rc != C.SQLITE_OK {
			C.free(unsafe.Pointer(cq))
			return nil, dbErr(c.db, rc)
		}
		if consumed <= 0 || consumed > len(remaining) {
			consumed = len(remaining)
		}
		remaining = remaining[consumed:]
		C.free(unsafe.Pointer(cq))
		if st == nil {
			continue
		}
		n := int(C.sqlite3_bind_parameter_count(st))
		if argPos+n > len(args) {
			C.sqlite3_finalize(st)
			return nil, fmt.Errorf("sqlite: expected at least %d args, got %d", argPos+n, len(args))
		}
		if err := bindAll(st, args[argPos:argPos+n]); err != nil {
			C.sqlite3_finalize(st)
			return nil, err
		}
		argPos += n
		for {
			rc = C.sqlite3_step(st)
			if rc == C.SQLITE_ROW {
				continue
			}
			if rc == C.SQLITE_DONE {
				break
			}
			err := stmtErr(st, rc)
			C.sqlite3_finalize(st)
			return nil, err
		}
		affected = int64(C.sqlite3_changes(c.db))
		last = int64(C.sqlite3_last_insert_rowid(c.db))
		if rc = C.sqlite3_finalize(st); rc != C.SQLITE_OK {
			return nil, dbErr(c.db, rc)
		}
	}
	if argPos != len(args) {
		return nil, fmt.Errorf("sqlite: unused arguments: %d", len(args)-argPos)
	}
	return result{last: last, affected: affected}, nil
}

func (c *conn) query(ctx context.Context, query string, args []driver.Value) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	st, err := c.prepare(query)
	if err != nil {
		return nil, err
	}
	if len(args) != st.n {
		st.Close()
		return nil, fmt.Errorf("sqlite: expected %d args, got %d", st.n, len(args))
	}
	if err = bindAll(st.st, args); err != nil {
		st.Close()
		return nil, err
	}
	n := int(C.sqlite3_column_count(st.st))
	cols := make([]string, n)
	for i := 0; i < n; i++ {
		cols[i] = C.GoString(C.sqlite3_column_name(st.st, C.int(i)))
	}
	return &rows{st: st.st, cols: cols}, nil
}

func bindAll(st *C.sqlite3_stmt, args []driver.Value) error {
	for i, v := range args {
		if err := bind(st, i+1, v); err != nil {
			return err
		}
	}
	return nil
}
func bind(st *C.sqlite3_stmt, idx int, v driver.Value) error {
	var rc C.int
	ci := C.int(idx)
	switch x := v.(type) {
	case nil:
		rc = C.sqlite3_bind_null(st, ci)
	case int64:
		rc = C.sqlite3_bind_int64(st, ci, C.sqlite3_int64(x))
	case int:
		rc = C.sqlite3_bind_int64(st, ci, C.sqlite3_int64(x))
	case bool:
		if x {
			rc = C.sqlite3_bind_int64(st, ci, 1)
		} else {
			rc = C.sqlite3_bind_int64(st, ci, 0)
		}
	case float64:
		rc = C.sqlite3_bind_double(st, ci, C.double(x))
	case string:
		p := C.CString(x)
		rc = C.bind_text_transient(st, ci, p, C.int(len(x)))
		C.free(unsafe.Pointer(p))
	case []byte:
		if len(x) == 0 {
			rc = C.bind_blob_transient(st, ci, nil, 0)
		} else {
			rc = C.bind_blob_transient(st, ci, unsafe.Pointer(&x[0]), C.int(len(x)))
		}
	case time.Time:
		s := x.UTC().Format(time.RFC3339Nano)
		p := C.CString(s)
		rc = C.bind_text_transient(st, ci, p, C.int(len(s)))
		C.free(unsafe.Pointer(p))
	default:
		return fmt.Errorf("sqlite: unsupported bind type %T", v)
	}
	if rc != C.SQLITE_OK {
		return stmtErr(st, rc)
	}
	return nil
}

func (s *stmt) Close() error {
	if s.st == nil {
		return nil
	}
	rc := C.sqlite3_finalize(s.st)
	s.st = nil
	if rc != C.SQLITE_OK {
		return dbErr(s.c.db, rc)
	}
	return nil
}
func (s *stmt) NumInput() int { return s.n }
func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	if s.st == nil {
		return nil, driver.ErrBadConn
	}
	C.sqlite3_reset(s.st)
	C.sqlite3_clear_bindings(s.st)
	if len(args) != s.n {
		return nil, fmt.Errorf("sqlite: expected %d args, got %d", s.n, len(args))
	}
	if err := bindAll(s.st, args); err != nil {
		return nil, err
	}
	for {
		rc := C.sqlite3_step(s.st)
		if rc == C.SQLITE_ROW {
			continue
		}
		if rc == C.SQLITE_DONE {
			break
		}
		return nil, stmtErr(s.st, rc)
	}
	return result{last: int64(C.sqlite3_last_insert_rowid(s.c.db)), affected: int64(C.sqlite3_changes(s.c.db))}, nil
}
func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	if s.st == nil {
		return nil, driver.ErrBadConn
	}
	return s.c.query(context.Background(), s.q, args)
}

func (r *rows) Columns() []string { return r.cols }
func (r *rows) Close() error {
	if r.done {
		return nil
	}
	r.done = true
	if r.st != nil {
		rc := C.sqlite3_finalize(r.st)
		r.st = nil
		if rc != C.SQLITE_OK {
			return fmt.Errorf("sqlite rows finalize: %d", int(rc))
		}
	}
	return nil
}
func (r *rows) Next(dest []driver.Value) error {
	if r.done || r.st == nil {
		return io.EOF
	}
	rc := C.sqlite3_step(r.st)
	if rc == C.SQLITE_DONE {
		_ = r.Close()
		return io.EOF
	}
	if rc != C.SQLITE_ROW {
		return stmtErr(r.st, rc)
	}
	n := int(C.sqlite3_column_count(r.st))
	if len(dest) < n {
		return errors.New("sqlite: destination too small")
	}
	for i := 0; i < n; i++ {
		dest[i] = columnValue(r.st, i)
	}
	return nil
}
func columnValue(st *C.sqlite3_stmt, i int) driver.Value {
	ci := C.int(i)
	switch C.sqlite3_column_type(st, ci) {
	case C.SQLITE_INTEGER:
		return int64(C.sqlite3_column_int64(st, ci))
	case C.SQLITE_FLOAT:
		return float64(C.sqlite3_column_double(st, ci))
	case C.SQLITE_TEXT:
		p := C.sqlite3_column_text(st, ci)
		n := C.sqlite3_column_bytes(st, ci)
		if p == nil {
			return ""
		}
		return C.GoStringN((*C.char)(unsafe.Pointer(p)), n)
	case C.SQLITE_BLOB:
		p := C.sqlite3_column_blob(st, ci)
		n := int(C.sqlite3_column_bytes(st, ci))
		if p == nil || n == 0 {
			return []byte{}
		}
		return C.GoBytes(p, C.int(n))
	default:
		return nil
	}
}

// Optional interfaces improve database/sql behaviour.
func (c *conn) CheckNamedValue(nv *driver.NamedValue) error {
	switch v := nv.Value.(type) {
	case nil, int64, float64, bool, string, []byte, time.Time:
		return nil
	case int:
		nv.Value = int64(v)
		return nil
	case int32:
		nv.Value = int64(v)
		return nil
	case uint:
		nv.Value = int64(v)
		return nil
	case uint64:
		if v > 1<<63-1 {
			return errors.New("sqlite: uint64 overflow")
		}
		nv.Value = int64(v)
		return nil
	default:
		if s, ok := v.(fmt.Stringer); ok {
			nv.Value = s.String()
			return nil
		}
		return driver.ErrSkip
	}
}

func (r *rows) ColumnTypeDatabaseTypeName(index int) string {
	if r.st == nil {
		return ""
	}
	d := C.sqlite3_column_decltype(r.st, C.int(index))
	if d == nil {
		return ""
	}
	return strings.ToUpper(C.GoString(d))
}
func (r *rows) ColumnTypeNullable(index int) (bool, bool) { return true, true }
func (r *rows) ColumnTypeScanType(index int) any          { return nil }

// Silence strconv import possibility across toolchains if build tags alter code paths.
var _ = strconv.IntSize
