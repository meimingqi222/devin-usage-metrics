//go:build windows

package data

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

func lockDevice() (func(), error) {
	if e := os.MkdirAll(filepath.Dir(configPath()), 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(filepath.Join(filepath.Dir(configPath()), "device.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	var ov syscall.Overlapped
	kernel := syscall.NewLazyDLL("kernel32.dll")
	lock := kernel.NewProc("LockFileEx")
	r, _, err := lock.Call(f.Fd(), 2, 0, 1, 0, uintptr(unsafe.Pointer(&ov)))
	if r == 0 {
		f.Close()
		return nil, err
	}
	return func() { kernel.NewProc("UnlockFileEx").Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ov))); f.Close() }, nil
}
