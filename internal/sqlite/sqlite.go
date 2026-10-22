// Package sqlite reads agent databases through a cgo-free SQLite compiled to
// Go (github.com/ncruces/go-sqlite3), so packages ship no native library.
//
// It keeps the small surface the MyGo SQLite plugin offered: values come back
// by storage class as nil, int64, float64, string or []byte, never converted
// by declared column type.
package sqlite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ncruces/go-sqlite3"
)

type OpenOptions struct {
	ReadOnly bool
	// BusyTimeout bounds waits for a writer's lock (default 5s, like the plugin).
	BusyTimeout time.Duration
}

// Rows holds a fully read result set.
type Rows struct {
	Columns []string
	Values  [][]any
}

// DB is safe for use from several goroutines; calls serialize on the connection.
type DB struct {
	mu   sync.Mutex
	conn *sqlite3.Conn
}

// Open opens path as a plain filename, not a URI. ReadOnly never creates or
// writes the file; otherwise a missing file is created.
func Open(ctx context.Context, path string, opts OpenOptions) (*DB, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, errors.New("sqlite: database path must be nonempty")
	}
	flags := sqlite3.OPEN_READWRITE | sqlite3.OPEN_CREATE
	if opts.ReadOnly {
		flags = sqlite3.OPEN_READONLY
	}
	conn, err := sqlite3.OpenFlags(path, flags)
	if err != nil {
		return nil, err
	}
	timeout := opts.BusyTimeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	if err = conn.BusyTimeout(timeout); err != nil {
		conn.Close()
		return nil, err
	}
	return &DB{conn: conn}, nil
}

func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return nil
	}
	err := d.conn.Close()
	d.conn = nil
	return err
}

// Query runs one statement and reads every row. ctx interrupts it.
func (d *DB) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	rows := Rows{Columns: []string{}, Values: [][]any{}}
	err := d.run(ctx, sql, args, func(s *sqlite3.Stmt) {
		if len(rows.Columns) == 0 {
			for i := range s.ColumnCount() {
				rows.Columns = append(rows.Columns, s.ColumnName(i))
			}
		}
		row := make([]any, len(rows.Columns))
		for i := range row {
			row[i] = column(s, i)
		}
		rows.Values = append(rows.Values, row)
	})
	return rows, err
}

// Result reports what a statement changed.
type Result struct {
	Changes      int64
	LastInsertID int64
}

// Execute runs one statement and discards any rows.
func (d *DB) Execute(ctx context.Context, sql string, args ...any) (Result, error) {
	var r Result
	err := d.run(ctx, sql, args, func(*sqlite3.Stmt) {})
	if err == nil {
		d.mu.Lock()
		if d.conn != nil {
			r = Result{d.conn.Changes(), d.conn.LastInsertRowID()}
		}
		d.mu.Unlock()
	}
	return r, err
}

func (d *DB) run(ctx context.Context, sql string, args []any, row func(*sqlite3.Stmt)) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return errors.New("sqlite: database is closed")
	}
	old := d.conn.SetInterrupt(ctx)
	defer d.conn.SetInterrupt(old)
	s, tail, err := d.conn.Prepare(sql)
	if err != nil {
		return err
	}
	defer s.Close()
	if tail != "" {
		return errors.New("sqlite: one statement per call")
	}
	for i, v := range args {
		if err = bind(s, i+1, v); err != nil {
			return err
		}
	}
	for s.Step() {
		row(s)
	}
	return s.Err()
}

func bind(s *sqlite3.Stmt, i int, v any) error {
	switch x := v.(type) {
	case nil:
		return s.BindNull(i)
	case string:
		return s.BindText(i, x)
	case []byte:
		return s.BindBlob(i, x)
	case int:
		return s.BindInt64(i, int64(x))
	case int64:
		return s.BindInt64(i, x)
	case float64:
		return s.BindFloat(i, x)
	case bool:
		return s.BindBool(i, x)
	default:
		return fmt.Errorf("sqlite: unsupported argument %d of type %T", i, v)
	}
}

// column reads by storage class, as the plugin did.
func column(s *sqlite3.Stmt, i int) any {
	switch s.ColumnType(i) {
	case sqlite3.INTEGER:
		return s.ColumnInt64(i)
	case sqlite3.FLOAT:
		return s.ColumnFloat(i)
	case sqlite3.TEXT:
		return s.ColumnText(i)
	case sqlite3.BLOB:
		return append([]byte{}, s.ColumnRawBlob(i)...)
	default:
		return nil
	}
}
