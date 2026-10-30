//go:build windows

package ext

import (
	"os"
	"syscall"
	"unsafe"
)

func lockFile(f *os.File) (func(), error) {
	var ov syscall.Overlapped
	kernel := syscall.NewLazyDLL("kernel32.dll")
	r, _, err := kernel.NewProc("LockFileEx").Call(f.Fd(), 2, 0, 1, 0, uintptr(unsafe.Pointer(&ov)))
	if r == 0 {
		return nil, err
	}
	return func() { kernel.NewProc("UnlockFileEx").Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ov))) }, nil
}

func syncDirectory(string) error { return nil }
