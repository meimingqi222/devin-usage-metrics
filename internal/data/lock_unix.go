//go:build !windows

package data

import (
	"os"
	"path/filepath"
	"syscall"
)

func lockDevice() (func(), error) {
	if e := os.MkdirAll(filepath.Dir(configPath()), 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(filepath.Join(filepath.Dir(configPath()), "device.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); e != nil {
		f.Close()
		return nil, e
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
