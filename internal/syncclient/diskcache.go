package syncclient

import (
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Compressed blocks persist across restarts so a cold start does not download
// every device's year of history again. Each is content-addressed and every
// read is checked against the referencing manifest's hash and size, so a copy
// on disk is as trustworthy as a download; a damaged copy is dropped and
// fetched again. The cache is bounded and evicts least recently used files.
const diskBudget = 256 << 20

var disk struct {
	sync.Mutex
	sized bool
	bytes int64
}

func diskRoot() string {
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "devin-usage-metrics", "sync-objects")
}

func (t transport) diskPath(name string) string {
	root := diskRoot()
	if root == "" || name == "" || filepath.Base(name) != name || name[0] == '.' {
		return ""
	}
	return filepath.Join(root, t.account()[:32], name)
}

func (t transport) cachedBlock(name string) []byte {
	p := t.diskPath(name)
	if p == "" {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	now := time.Now()
	_ = os.Chtimes(p, now, now) // recency for eviction
	return b
}

func (t transport) forgetBlock(name string) {
	if p := t.diskPath(name); p != "" {
		if info, err := os.Stat(p); err == nil && os.Remove(p) == nil {
			disk.Lock()
			disk.bytes -= info.Size()
			disk.Unlock()
		}
	}
}

func (t transport) rememberBlock(name string, b []byte) {
	p := t.diskPath(name)
	if p == "" {
		return
	}
	if info, err := os.Stat(p); err == nil && info.Size() == int64(len(b)) {
		return
	}
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return
	}
	_, err = f.Write(b)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil || os.Rename(f.Name(), p) != nil {
		os.Remove(f.Name())
		return
	}
	disk.Lock()
	defer disk.Unlock()
	if !disk.sized {
		disk.bytes, disk.sized = diskUsage(), true
	} else {
		disk.bytes += int64(len(b))
	}
	if disk.bytes > diskBudget {
		disk.bytes = trimDisk(diskBudget * 3 / 4)
	}
}

type diskFile struct {
	path string
	size int64
	used time.Time
}

func diskFiles() []diskFile {
	var files []diskFile
	_ = filepath.WalkDir(diskRoot(), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			files = append(files, diskFile{p, info.Size(), info.ModTime()})
		}
		return nil
	})
	return files
}

func diskUsage() int64 {
	var n int64
	for _, f := range diskFiles() {
		n += f.size
	}
	return n
}

// trimDisk removes least recently used files until the cache fits target.
func trimDisk(target int64) int64 {
	files := diskFiles()
	var total int64
	for _, f := range files {
		total += f.size
	}
	sort.Slice(files, func(i, j int) bool { return files[i].used.Before(files[j].used) })
	for _, f := range files {
		if total <= target {
			break
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
	return total
}
