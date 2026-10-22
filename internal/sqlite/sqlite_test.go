package sqlite

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestValuesFollowStorageClassNotDeclaredType(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "x.db")
	db, err := Open(ctx, p, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"CREATE TABLE t(i INTEGER, r REAL, s TEXT, b BLOB, n, at DATETIME, day DATE)",
		"INSERT INTO t VALUES(42, 1.5, 'x', x'0001', NULL, '2026-10-22 09:00:00', '2026-10-22')",
		"INSERT INTO t VALUES(0, 0, '', x'', NULL, 1700000000, NULL)",
	} {
		if _, err = db.Execute(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	db, err = Open(ctx, p, OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r, err := db.Query(ctx, "SELECT * FROM t WHERE i >= ? ORDER BY i DESC", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Columns) != 7 || r.Columns[5] != "at" || len(r.Values) != 2 {
		t.Fatalf("shape: %v %d", r.Columns, len(r.Values))
	}
	row := r.Values[0]
	if row[0] != int64(42) || row[1] != 1.5 || row[2] != "x" || !bytes.Equal(row[3].([]byte), []byte{0, 1}) || row[4] != nil {
		t.Fatalf("storage classes: %#v", row)
	}
	// The MyGo plugin returned dates as stored; modernc.org/sqlite would not.
	if row[5] != "2026-10-22 09:00:00" || row[6] != "2026-10-22" || r.Values[1][5] != int64(1700000000) {
		t.Fatalf("declared date types must not convert: %#v %#v", row[5:], r.Values[1][5])
	}
	if b, ok := r.Values[1][3].([]byte); !ok || b == nil || len(b) != 0 || r.Values[1][2] != "" {
		t.Fatalf("empty values: %#v", r.Values[1])
	}
	if _, err = db.Execute(ctx, "DELETE FROM t"); err == nil {
		t.Fatal("read-only connection wrote")
	}
}

func TestReadOnlyNeverCreatesFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing.db")
	if db, err := Open(context.Background(), p, OpenOptions{ReadOnly: true}); err == nil {
		db.Close()
		t.Fatal("opened a missing database")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("read-only open created the file")
	}
}

// Agents keep writing while we read; a reader must see committed WAL frames.
func TestReadsWALWhileWriterHoldsConnection(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "wal.db")
	w, err := Open(ctx, p, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, s := range []string{"PRAGMA journal_mode=WAL", "CREATE TABLE t(v)", "INSERT INTO t VALUES(1)"} {
		if _, err = w.Query(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = w.Execute(ctx, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Execute(ctx, "INSERT INTO t VALUES(2)"); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, p, OpenOptions{ReadOnly: true, BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	rows, err := r.Query(ctx, "SELECT count(*) FROM t")
	if err != nil || rows.Values[0][0] != int64(1) {
		t.Fatalf("reader must see only committed rows: %v %v", rows.Values, err)
	}
	if _, err = w.Execute(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	rows, err = r.Query(ctx, "SELECT count(*) FROM t")
	if err != nil || rows.Values[0][0] != int64(2) {
		t.Fatalf("reader missed the commit: %v %v", rows.Values, err)
	}
}

func TestQueryHonoursCancellation(t *testing.T) {
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "c.db"), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = db.Query(ctx, "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c) SELECT count(*) FROM c"); err == nil {
		t.Fatal("canceled query ran to completion")
	}
}
