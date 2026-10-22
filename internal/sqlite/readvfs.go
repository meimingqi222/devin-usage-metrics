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
const readAheadSize = 256 << 10

type readBlock struct {
	buffer []byte
	offset int64
}

type readFile struct {
	vfs.File
	blocks    []readBlock
	blockSize int
	active    bool
	stats     ReadStats
}

func (f *readFile) ReadAt(p []byte, off int64) (int, error) {
	f.stats.Requests++
	if len(p) == 0 {
		return 0, nil
	}
	if !f.active || off < 0 {
		return f.readFileAt(p, off)
	}
	if f.blocks == nil {
		f.blocks = make([]readBlock, 16)
		f.blockSize = readAheadSize
	}
	size := int64(f.blockSize)
	if int64(len(p)) > size {
		return f.readFileAt(p, off)
	}
	base := off / size * size
	if off-base+int64(len(p)) > size {
		return f.readFileAt(p, off)
	}
	block := &f.blocks[base/size%int64(len(f.blocks))]
	if block.offset == base && off-base+int64(len(p)) <= int64(len(block.buffer)) {
		f.stats.CacheHits++
		return copy(p, block.buffer[off-base:]), nil
	}
	if cap(block.buffer) == 0 {
		block.buffer = make([]byte, f.blockSize)
	}
	n, err := f.readFileAt(block.buffer[:cap(block.buffer)], base)
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

func (f *readFile) readFileAt(p []byte, off int64) (int, error) {
	f.stats.FileReads++
	n, err := f.File.ReadAt(p, off)
	f.stats.FileBytes += uint64(n)
	return n, err
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

// Smaller blocks avoid reading 256 KiB for scattered 4 KiB index pages. More
// slots retain the alternating index/payload working set without whole-file caching.
func (f *readFile) useInterleavedReadAhead() {
	f.blockSize = 64 << 10
	f.blocks = make([]readBlock, 128)
}
