package ext

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	mu           sync.Mutex
	undoName     = "ext-undo"
	errStaleUndo = errors.New("file changed after the last edit; undo would overwrite later changes")
)

type fileSnap struct {
	Path      string `json:"path"`
	Existed   bool   `json:"existed"`
	Dir       bool   `json:"dir,omitempty"`
	Mode      uint32 `json:"mode"`
	Data      []byte `json:"data,omitempty"`
	AfterHash string `json:"afterHash,omitempty"`
	Sealed    bool   `json:"sealed,omitempty"`
}

type snapshot struct {
	ID      string     `json:"id"`
	At      time.Time  `json:"at"`
	Kind    string     `json:"kind"`
	Summary string     `json:"summary"`
	Files   []fileSnap `json:"files"`
}

func undoDir() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(d, "devin-usage-metrics", undoName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}

func withLock(fn func() error) error {
	mu.Lock()
	defer mu.Unlock()
	dir, err := undoDir()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	unlock, err := lockFile(f)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// resolvePath is where a write to path lands. Unlike EvalSymlinks it also
// follows a symlink whose target does not exist yet, so atomicWrite creates
// the target instead of replacing the link with a regular file.
func resolvePath(path string) string {
	if path == "" {
		return path
	}
	if target, err := filepath.EvalSymlinks(path); err == nil {
		return target
	}
	p := path
	for i := 0; i < 40; i++ {
		st, err := os.Lstat(p)
		if err != nil || st.Mode()&os.ModeSymlink == 0 {
			break
		}
		target, err := os.Readlink(p)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
		p = target
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(dir, filepath.Base(p))
	}
	return p
}

func captureFiles(paths []string) ([]fileSnap, error) {
	var out []fileSnap
	seen := map[string]bool{}
	var add func(p string) error
	add = func(p string) error {
		if p == "" || seen[p] {
			return nil
		}
		seen[p] = true
		st, err := os.Lstat(p)
		if os.IsNotExist(err) {
			out = append(out, fileSnap{Path: p})
			return nil
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			target, err := os.Stat(p)
			if err != nil || target.IsDir() {
				snap, e := captureOne(p, st)
				if e != nil {
					return e
				}
				out = append(out, snap)
				if os.IsNotExist(err) {
					// A write follows the dangling link and creates its
					// target; record it as absent so undo removes it.
					return add(resolvePath(p))
				}
				return nil
			}
			return add(resolvePath(p))
		}
		if st.IsDir() {
			tree, err := captureTree(p)
			if err != nil {
				return err
			}
			out = append(out, tree...)
			return nil
		}
		snap, err := captureOne(p, st)
		if err != nil {
			return err
		}
		out = append(out, snap)
		return nil
	}
	for _, p := range paths {
		if err := add(p); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func captureTree(root string) ([]fileSnap, error) {
	var out []fileSnap
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		snap, err := captureOne(p, st)
		if err != nil {
			return err
		}
		out = append(out, snap)
		return nil
	})
	return out, err
}

func captureOne(p string, st os.FileInfo) (fileSnap, error) {
	snap := fileSnap{Path: p, Existed: true, Mode: uint32(st.Mode())}
	if st.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(p)
		if err != nil {
			return fileSnap{}, err
		}
		snap.Data = []byte("symlink:" + target)
		return snap, nil
	}
	if st.IsDir() {
		snap.Dir = true
		return snap, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return fileSnap{}, err
	}
	snap.Data = b
	return snap, nil
}

func sealSnapshot(files []fileSnap) error {
	for i := range files {
		h, err := hashPath(files[i].Path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		files[i].AfterHash = h
		files[i].Sealed = true
	}
	return nil
}

func hashPath(path string) (string, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256([]byte("symlink:" + target))
		return hex.EncodeToString(sum[:]), nil
	}
	if st.IsDir() {
		h := sha256.New()
		err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(path, p)
			if err != nil {
				return err
			}
			io.WriteString(h, rel)
			h.Write([]byte{0})
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h.Write(b)
			h.Write([]byte{0})
			return nil
		})
		if err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func saveSnapshot(kind, summary string, files []fileSnap) error {
	if err := sealSnapshot(files); err != nil {
		return err
	}
	dir, err := undoDir()
	if err != nil {
		return err
	}
	s := snapshot{ID: time.Now().UTC().Format("20060102T150405.000000000"), At: time.Now().UTC(), Kind: kind, Summary: summary, Files: files}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, "current.json"), b)
}

func LastUndo() (kind, summary string, ok bool) {
	dir, err := undoDir()
	if err != nil {
		return "", "", false
	}
	b, err := os.ReadFile(filepath.Join(dir, "current.json"))
	if err != nil {
		return "", "", false
	}
	var s snapshot
	if json.Unmarshal(b, &s) != nil || s.ID == "" {
		return "", "", false
	}
	return s.Kind, s.Summary, true
}

func UndoLast() error {
	return withLock(func() error {
		dir, err := undoDir()
		if err != nil {
			return err
		}
		path := filepath.Join(dir, "current.json")
		b, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return errors.New("no saved change to undo")
			}
			return err
		}
		var s snapshot
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		for _, f := range s.Files {
			if !f.Sealed {
				continue
			}
			h, err := hashPath(f.Path)
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			if h != f.AfterHash {
				return fmt.Errorf("%s: %w", f.Path, errStaleUndo)
			}
		}
		if err := restoreSnaps(s.Files); err != nil {
			return err
		}
		return os.Remove(path)
	})
}

func restoreSnaps(files []fileSnap) error {
	for _, f := range files {
		if !f.Existed {
			if err := os.RemoveAll(f.Path); err != nil {
				return err
			}
		}
	}
	for _, f := range files {
		if !f.Existed || !f.Dir {
			continue
		}
		if err := os.RemoveAll(f.Path); err != nil {
			return err
		}
		perm := os.FileMode(0700)
		if f.Mode != 0 {
			perm = os.FileMode(f.Mode).Perm()
		}
		if err := os.MkdirAll(f.Path, perm); err != nil {
			return err
		}
	}
	for _, f := range files {
		if !f.Existed || f.Dir {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(f.Path), 0700); err != nil {
			return err
		}
		if st := os.FileMode(f.Mode); st&os.ModeSymlink != 0 {
			if err := os.RemoveAll(f.Path); err != nil {
				return err
			}
			target, ok := cutPrefix(string(f.Data), "symlink:")
			if !ok {
				return errors.New("invalid symlink snapshot")
			}
			if err := os.Symlink(target, f.Path); err != nil {
				return err
			}
			continue
		}
		if err := atomicWrite(f.Path, f.Data); err != nil {
			return err
		}
		if f.Mode != 0 {
			_ = os.Chmod(f.Path, os.FileMode(f.Mode).Perm())
		}
	}
	return nil
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return s, false
}

func atomicWrite(path string, b []byte) error {
	path = resolvePath(path)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".ext-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	mode := os.FileMode(0600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func readText(path string) (string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(b), err
}
