package sqlite

import (
	"io"

	"github.com/ncruces/go-sqlite3/util/vfsutil"
	"github.com/ncruces/go-sqlite3/vfs"
)

const readVFSName = "aum-read"

func init() { vfs.Register(readVFSName, readVFS{vfs.Find("os")}) }

type readVFS struct{ vfs.VFS }

func (v readVFS) OpenFilename(name *vfs.Filename, flags vfs.OpenFlag) (vfs.File, vfs.OpenFlag, error) {
	f, out, err := vfsutil.WrapOpenFilename(v.VFS, name, flags)
	if err != nil || flags&(vfs.OPEN_MAIN_DB|vfs.OPEN_READONLY) != vfs.OPEN_MAIN_DB|vfs.OPEN_READONLY {
		return f, out, err
	}
	return &readFile{File: f}, out, nil
}

// A small direct-mapped cache coalesces SQLite's overflow-page reads without
// retaining entire databases. Only DB.run enables it, for one statement.
const readAheadSize = 64 << 10

type readBlock struct {
	buffer []byte
	offset int64
}

type readFile struct {
	vfs.File
	blocks [16]readBlock
	active bool
}

func (f *readFile) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !f.active || off < 0 || len(p) > readAheadSize {
		return f.File.ReadAt(p, off)
	}
	base := off / readAheadSize * readAheadSize
	if off-base+int64(len(p)) > readAheadSize {
		return f.File.ReadAt(p, off)
	}
	block := &f.blocks[base/readAheadSize%int64(len(f.blocks))]
	if block.offset == base && off-base+int64(len(p)) <= int64(len(block.buffer)) {
		return copy(p, block.buffer[off-base:]), nil
	}
	if cap(block.buffer) == 0 {
		block.buffer = make([]byte, readAheadSize)
	}
	n, err := f.File.ReadAt(block.buffer[:cap(block.buffer)], base)
	block.buffer, block.offset = block.buffer[:n], base
	if err != nil && err != io.EOF {
		block.buffer = block.buffer[:0]
		return 0, err
	}
	if off-base >= int64(n) {
		return 0, io.EOF
	}
	n = copy(p, block.buffer[off-base:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *readFile) Lock(level vfs.LockLevel) error {
	f.reset()
	return f.File.Lock(level)
}

func (f *readFile) Unlock(level vfs.LockLevel) error {
	f.reset()
	return f.File.Unlock(level)
}

func (f *readFile) SharedMemory() vfs.SharedMemory { return vfsutil.WrapSharedMemory(f.File) }
func (f *readFile) MemoryMapper() vfs.MemoryMapper { return vfsutil.WrapMemoryMapper(f.File) }
func (f *readFile) LockState() vfs.LockLevel       { return vfsutil.WrapLockState(f.File) }
func (f *readFile) HasMoved() (bool, error)        { return vfsutil.WrapHasMoved(f.File) }
func (f *readFile) Pragma(name, value string) (string, error) {
	return vfsutil.WrapPragma(f.File, name, value)
}
func (f *readFile) Unwrap() vfs.File { return f.File }

func (f *readFile) reset() {
	for i := range f.blocks {
		f.blocks[i].buffer = f.blocks[i].buffer[:0]
		f.blocks[i].offset = -1
	}
}
