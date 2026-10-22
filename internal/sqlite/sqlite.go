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
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/util/vfsutil"
	"github.com/ncruces/go-sqlite3/vfs"
)

type OpenOptions struct {
	ReadOnly bool
	// InterleavedReads uses 64 KiB blocks with an 8 MiB budget for read-only
	// scans alternating between distant index and payload pages.
	InterleavedReads bool
	// BusyTimeout bounds waits for a writer's lock (default 5s, like the plugin).
	BusyTimeout time.Duration
}

// Rows holds a fully read result set.
type Rows struct {
	Columns []string
	Values  [][]any
}

// ReadStats counts main-file VFS requests and actual reads on this connection.
// WAL reads are excluded. A connection without the read-ahead VFS returns zero.
type ReadStats struct {
	Requests, CacheHits, FileReads, FileBytes uint64
}

func (d *DB) ReadStats() ReadStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reader == nil {
		return ReadStats{}
	}
	return d.reader.stats
}

// DB is safe for use from several goroutines; calls serialize on the connection.
type DB struct {
	mu     sync.Mutex
	conn   *sqlite3.Conn
	reader *readFile
}

// Open opens path as a plain filename, not a URI. ReadOnly never creates or
// writes the file; otherwise a missing file is created. Read-only connections
// go through readVFSName so overflow-page reads are coalesced; the path is
// therefore converted to a file: URI internally.
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
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		path = readOnlyURI(filepath.ToSlash(abs))
		flags |= sqlite3.OPEN_URI
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
	db := &DB{conn: conn}
	if opts.ReadOnly {
		// Best effort: a connection whose file was not wrapped still works, it
		// just reads pages one at a time instead of in blocks.
		if file, err := conn.FileControl("main", sqlite3.FCNTL_FILE_POINTER); err == nil {
			if f, ok := file.(vfs.File); ok {
				db.reader, _ = vfsutil.UnwrapFile[*readFile](f)
				if db.reader != nil && opts.InterleavedReads {
					db.reader.useInterleavedReadAhead()
				}
			}
		}
	}
	return db, nil
}

// readOnlyURI builds the file: URI handed to SQLite for a slash-separated
// absolute path. SQLite passes the text after "file:" to the VFS, so its shape
// matters: a drive letter must not gain a leading slash ("file:C:/x", not
// "file:///C:/x", whose VFS path is "/C:/x"), while a UNC path keeps its two
// slashes after an empty authority ("file:////server/share/x").
func readOnlyURI(slashAbs string) string {
	escaped := (&url.URL{Path: slashAbs}).EscapedPath()
	if strings.HasPrefix(escaped, "//") {
		escaped = "//" + escaped
	}
	return "file:" + escaped + "?mode=ro&vfs=" + readVFSName
}

func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return nil
	}
	err := d.conn.Close()
	d.conn = nil
	d.reader = nil
	return err
}

// Query runs one statement and reads every row. ctx interrupts it.
func (d *DB) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	rows := Rows{Columns: []string{}, Values: [][]any{}}
	err := d.run(ctx, sql, args, func(s *sqlite3.Stmt) error {
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
		return nil
	})
	return rows, err
}

// Each runs one statement and calls fn for every row as it is read, so large
// result sets never sit in memory at once. fn must not keep row; an error from
// fn aborts the statement and is returned.
func (d *DB) Each(ctx context.Context, sql string, fn func(columns []string, row []any) error, args ...any) error {
	return d.each(ctx, sql, "", fn, args...)
}

// BorrowedText is read-only SQLite-owned text, valid only inside the EachBorrowedText
// callback. Copy it before retaining it. Other column storage classes are unchanged.
type BorrowedText []byte

// EachBorrowedText avoids copying one large TEXT column. It retrieves that column
// last so no subsequent statement method can invalidate it before fn returns.
func (d *DB) EachBorrowedText(ctx context.Context, sql, textColumn string, fn func(columns []string, row []any) error, args ...any) error {
	return d.each(ctx, sql, textColumn, fn, args...)
}

func (d *DB) each(ctx context.Context, sql, textColumn string, fn func(columns []string, row []any) error, args ...any) error {
	var columns []string
	borrowed := -1
	return d.run(ctx, sql, args, func(s *sqlite3.Stmt) error {
		if columns == nil {
			columns = make([]string, s.ColumnCount())
			for i := range columns {
				columns[i] = s.ColumnName(i)
				if textColumn != "" && columns[i] == textColumn {
					borrowed = i
				}
			}
		}
		row := make([]any, len(columns))
		borrowText := borrowed >= 0 && s.ColumnType(borrowed) == sqlite3.TEXT
		for i := range row {
			if !borrowText || i != borrowed {
				row[i] = column(s, i)
			}
		}
		if borrowText {
			row[borrowed] = BorrowedText(s.ColumnRawText(borrowed))
		}
		return fn(columns, row)
	})
}

// Result reports what a statement changed.
type Result struct {
	Changes      int64
	LastInsertID int64
}

// Execute runs one statement and discards any rows.
func (d *DB) Execute(ctx context.Context, sql string, args ...any) (Result, error) {
	var r Result
	err := d.run(ctx, sql, args, func(*sqlite3.Stmt) error { return nil })
	if err == nil {
		d.mu.Lock()
		if d.conn != nil {
			r = Result{d.conn.Changes(), d.conn.LastInsertRowID()}
		}
		d.mu.Unlock()
	}
	return r, err
}

func (d *DB) run(ctx context.Context, sql string, args []any, row func(*sqlite3.Stmt) error) error {
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
	if d.reader != nil {
		// The cache lives for exactly one statement, and WAL files keep the
		// original VFS and are never cached here. In rollback-journal mode the
		// pager holds LOCK_SHARED for the whole statement, so main-file pages
		// cannot change underneath it. In WAL mode the reader holds no DB-file
		// lock, so correctness rests on this reset plus SQLite's read mark,
		// which stops a checkpoint from backfilling frames the snapshot needs.
		// Do not move this reset past the step loop.
		d.reader.active = true
		defer func() { d.reader.active = false; d.reader.reset() }()
	}
	for s.Step() {
		if err := row(s); err != nil {
			return err
		}
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
