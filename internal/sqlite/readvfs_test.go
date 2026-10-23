package sqlite

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/vfs"
)

type countedFile struct {
	vfs.File
	data  []byte
	reads int
	err   error
}

func (f *countedFile) ReadAt(p []byte, off int64) (int, error) {
	f.reads++
	if f.data == nil && f.File != nil {
		return f.File.ReadAt(p, off)
	}
	if f.err != nil {
		return 0, f.err
	}
	return bytes.NewReader(f.data).ReadAt(p, off)
}

func TestReadAheadReducesSQLiteOverflowReads(t *testing.T) {
	readAheadProfiles(t, func(t *testing.T, interleaved bool) {
		ctx := context.Background()
		p := filepath.Join(t.TempDir(), "large.db")
		w, err := Open(ctx, p, OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{"CREATE TABLE t(body BLOB)", "INSERT INTO t VALUES(zeroblob(2097152))"} {
			if _, err = w.Execute(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		if err = w.Close(); err != nil {
			t.Fatal(err)
		}
		r, err := Open(ctx, p, OpenOptions{ReadOnly: true, InterleavedReads: interleaved})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if r.reader == nil {
			t.Fatal("read-only database did not use read-ahead VFS")
		}
		count := &countedFile{File: r.reader.File}
		r.reader.File = count
		before := r.ReadStats()
		rows, err := r.Query(ctx, "SELECT body FROM t")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(rows.Values[0][0].([]byte), make([]byte, 2097152)) {
			t.Fatal("overflow data changed")
		}
		after := r.ReadStats()
		if after.FileReads-before.FileReads != uint64(count.reads) || after.FileBytes <= before.FileBytes || after.CacheHits <= before.CacheHits || after.Requests-before.Requests <= after.FileReads-before.FileReads {
			t.Fatalf("read counters disagree with actual VFS reads: before=%+v after=%+v reads=%d", before, after, count.reads)
		}
		if count.reads >= 128 {
			t.Fatalf("2 MiB query used %d file reads; expected coalesced overflow pages", count.reads)
		}
		for _, block := range r.reader.blocks {
			if len(block.buffer) != 0 {
				t.Fatal("read-ahead buffer survived statement")
			}
		}
		if r.reader.active {
			t.Fatal("read-ahead enabled outside statement")
		}
	})
}
func (*countedFile) Lock(vfs.LockLevel) error   { return nil }
func (*countedFile) Unlock(vfs.LockLevel) error { return nil }

func TestReadAheadCoalescesPagesAndInvalidates(t *testing.T) {
	readAheadProfiles(t, func(t *testing.T, interleaved bool) {
		blockSize := readAheadSize
		if interleaved {
			blockSize = 64 << 10
		}
		base := &countedFile{data: bytes.Repeat([]byte("abcd"), blockSize)}
		f := &readFile{File: base, active: true}
		if interleaved {
			f.useInterleavedReadAhead()
		}
		page := make([]byte, 4096)
		for off := 0; off < len(base.data); off += len(page) {
			n, err := f.ReadAt(page, int64(off))
			if err != nil || n != len(page) || !bytes.Equal(page, base.data[off:off+n]) {
				t.Fatalf("page %d: n=%d err=%v", off, n, err)
			}
		}
		if base.reads != len(base.data)/blockSize {
			t.Fatalf("got %d reads for %d pages", base.reads, len(base.data)/len(page))
		}
		for _, invalidate := range []func() error{func() error { f.reset(); return nil }, func() error { return f.Lock(vfs.LOCK_SHARED) }, func() error { return f.Unlock(vfs.LOCK_NONE) }} {
			if _, err := f.ReadAt(page, 0); err != nil {
				t.Fatal(err)
			}
			base.data[0]++
			if err := invalidate(); err != nil {
				t.Fatal(err)
			}
			if _, err := f.ReadAt(page, 0); err != nil || page[0] != base.data[0] {
				t.Fatal("stale page after invalidation")
			}
		}
		f.active = false
		base.data[0]++
		if _, err := f.ReadAt(page, 0); err != nil || page[0] != base.data[0] {
			t.Fatal("inactive buffer reused old data")
		}
	})
}

func TestReadAheadShortReadsBoundariesAndErrors(t *testing.T) {
	readAheadProfiles(t, func(t *testing.T, interleaved bool) {
		blockSize := readAheadSize
		if interleaved {
			blockSize = 64 << 10
		}
		base := &countedFile{data: bytes.Repeat([]byte("x"), blockSize+7)}
		f := &readFile{File: base, active: true}
		if interleaved {
			f.useInterleavedReadAhead()
		}
		for _, tc := range []struct {
			off        int64
			size, want int
			err        error
		}{
			{int64(blockSize - 3), 8, 8, nil},
			{int64(blockSize), 10, 7, io.EOF},
			{int64(blockSize + 8), 1, 0, io.EOF},
			{0, blockSize + 10, blockSize + 7, io.EOF},
			{0, 0, 0, nil},
		} {
			n, err := f.ReadAt(make([]byte, tc.size), tc.off)
			if n != tc.want || !errors.Is(err, tc.err) {
				t.Fatalf("offset=%d size=%d: n=%d err=%v", tc.off, tc.size, n, err)
			}
		}
		f.reset()
		base.err = errors.New("read failure")
		if _, err := f.ReadAt(make([]byte, 1), 0); err != base.err {
			t.Fatal(err)
		}
	})
}

func TestReadOnlyURIShapes(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/tmp/a b#c%.db", "file:/tmp/a%20b%23c%25.db?mode=ro&vfs=aum-read"},
		{"/tmp/雪.db", "file:/tmp/%E9%9B%AA.db?mode=ro&vfs=aum-read"},
		// Windows drive letters must not gain a leading slash: SQLite hands the
		// text after "file:" to the VFS, and its Windows path code only strips a
		// "/X:" prefix that SQLite itself never sees in this form.
		{"C:/work/data/a.db", "file:C:/work/data/a.db?mode=ro&vfs=aum-read"},
		// UNC keeps both slashes after an empty authority.
		{"//server/share/a.db", "file:////server/share/a.db?mode=ro&vfs=aum-read"},
	} {
		if got := readOnlyURI(tc.path); got != tc.want {
			t.Errorf("readOnlyURI(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestReadAheadObservesWALAndCheckpointChanges(t *testing.T) {
	readAheadProfiles(t, func(t *testing.T, interleaved bool) {
		ctx := context.Background()
		p := filepath.Join(t.TempDir(), "history # % 雪.db")
		w, err := Open(ctx, p, OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		for _, q := range []string{"PRAGMA journal_mode=WAL", "CREATE TABLE t(id INTEGER PRIMARY KEY, body BLOB)", "INSERT INTO t VALUES(1,zeroblob(131072))", "PRAGMA wal_checkpoint(TRUNCATE)"} {
			if _, err = w.Query(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		r, err := Open(ctx, p, OpenOptions{ReadOnly: true, InterleavedReads: interleaved})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if r.reader == nil {
			t.Fatal("read-only database did not use read-ahead VFS")
		}
		check := func(want byte) {
			t.Helper()
			rows, e := r.Query(ctx, "SELECT body FROM t WHERE id=1")
			if e != nil {
				t.Fatal(e)
			}
			body := rows.Values[0][0].([]byte)
			if len(body) != 131072 || body[65536] != want {
				t.Fatal("reader returned stale overflow content")
			}
		}
		check(0)
		if _, err = r.Execute(ctx, "BEGIN"); err != nil {
			t.Fatal(err)
		}
		check(0)
		body := bytes.Repeat([]byte{7}, 131072)
		if _, err = w.Execute(ctx, "UPDATE t SET body=? WHERE id=1", body); err != nil {
			t.Fatal(err)
		}
		check(0)
		if _, err = r.Execute(ctx, "COMMIT"); err != nil {
			t.Fatal(err)
		}
		check(7)
		if _, err = w.Query(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			t.Fatal(err)
		}
		check(7)
		if _, err = w.Execute(ctx, "UPDATE t SET body=zeroblob(131072) WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		if _, err = w.Query(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			t.Fatal(err)
		}
		check(0)
	})
}

func TestEachStopsEvaluatingAfterCallbackError(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "stop.db"), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	calls := 0
	if err = db.conn.CreateFunction("observe", 1, 0, func(ctx sqlite3.Context, args ...sqlite3.Value) { calls++; ctx.ResultInt(1) }); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop")
	err = db.Each(ctx, "WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<100) SELECT observe(x) FROM n", func([]string, []any) error { return stop })
	if err != stop || calls != 1 {
		t.Fatalf("err=%v evaluated=%d, want stop after 1 row", err, calls)
	}
}

func TestReadAheadObservesRollbackJournalWrites(t *testing.T) {
	readAheadProfiles(t, func(t *testing.T, interleaved bool) {
		ctx := context.Background()
		p := filepath.Join(t.TempDir(), "rollback.db")
		w, err := Open(ctx, p, OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		for _, q := range []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, body BLOB)", "INSERT INTO t VALUES(1,zeroblob(131072))"} {
			if _, err = w.Execute(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		r, err := Open(ctx, p, OpenOptions{ReadOnly: true, InterleavedReads: interleaved})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		check := func(want byte) {
			t.Helper()
			rows, e := r.Query(ctx, "SELECT body FROM t WHERE id=1")
			if e != nil {
				t.Fatal(e)
			}
			body := rows.Values[0][0].([]byte)
			if len(body) != 131072 || body[65536] != want {
				t.Fatal("reader returned stale overflow content")
			}
		}
		check(0)
		if _, err = w.Execute(ctx, "UPDATE t SET body=? WHERE id=1", bytes.Repeat([]byte{7}, 131072)); err != nil {
			t.Fatal(err)
		}
		check(7)
	})
}

func TestWritableConnectionHasNoReadAhead(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "rw.db"), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.reader != nil {
		t.Fatal("writable connection wrapped the main file")
	}
	if _, err = db.Execute(ctx, "CREATE TABLE t(x)"); err != nil {
		t.Fatal(err)
	}
}

func TestEachAbortLeavesConnectionUsable(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "abort.db"), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Execute(ctx, "CREATE TABLE t(x)"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"INSERT INTO t VALUES(1)", "INSERT INTO t VALUES(2)"} {
		if _, err = db.Execute(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	stop := errors.New("stop")
	if err = db.Each(ctx, "SELECT x FROM t", func([]string, []any) error { return stop }); err != stop {
		t.Fatalf("Each returned %v", err)
	}
	rows, err := db.Query(ctx, "SELECT count(*) FROM t")
	if err != nil || rows.Values[0][0] != int64(2) {
		t.Fatalf("connection unusable after abort: %v %v", rows.Values, err)
	}
}

func readAheadProfiles(t *testing.T, run func(*testing.T, bool)) {
	t.Helper()
	t.Run("default", func(t *testing.T) { run(t, false) })
	t.Run("interleaved", func(t *testing.T) { run(t, true) })
}

func TestInterleavedReadAheadBoundsScatteredReads(t *testing.T) {
	base := &countedFile{data: bytes.Repeat([]byte("abcd"), interleavedBudget/4)}
	f := &readFile{File: base, active: true}
	f.useInterleavedReadAhead()
	page := make([]byte, 4096)
	// Interleave distant pages, then revisit them as an index/payload scan does.
	for pass := 0; pass < 2; pass++ {
		for off := 0; off < len(base.data); off += 256 << 10 {
			n, err := f.ReadAt(page, int64(off))
			if err != nil || n != len(page) || !bytes.Equal(page, base.data[off:off+n]) {
				t.Fatalf("page mismatch: %d %v", n, err)
			}
		}
	}
	if f.stats.FileBytes > interleavedBudget/4 || base.reads > interleavedBudget/(256<<10) {
		t.Fatalf("scattered pages amplified reads: bytes=%d calls=%d", f.stats.FileBytes, base.reads)
	}
	// Even after visiting every slot, buffers stay inside the per-connection budget.
	for off := 0; off < len(base.data); off += 64 << 10 {
		if _, err := f.ReadAt(page, int64(off)); err != nil {
			t.Fatal(err)
		}
	}
	capacity := 0
	for _, b := range f.blocks {
		capacity += cap(b.buffer)
	}
	if capacity > interleavedBudget {
		t.Fatalf("cache exceeded %d bytes: %d", interleavedBudget, capacity)
	}
}
