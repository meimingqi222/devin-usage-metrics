//go:build windows

package quota

import (
	"errors"
	"syscall"
	"unsafe"
)

// Antigravity 2.x on Windows keeps its sign-in in the Credential Manager,
// the way go-keyring stores it: a generic credential named
// "<service>:<user>" whose blob is the item's UTF-8 text. The desktop
// client and its language service read it there; the state database and
// ~/.gemini files are mirrors. Pure syscalls keep the build cgo-free.
var (
	advapi32       = syscall.NewLazyDLL("advapi32.dll")
	procCredReadW  = advapi32.NewProc("CredReadW")
	procCredWriteW = advapi32.NewProc("CredWriteW")
	procCredDelete = advapi32.NewProc("CredDeleteW")
	procCredFree   = advapi32.NewProc("CredFree")
)

const (
	credTypeGeneric          = 1
	credPersistLocalMachine  = 2
	winCredUnavailableReason = "credential manager item unavailable"
)

type winCredential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        syscall.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

func winCredRead(target string) ([]byte, error) {
	name, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return nil, err
	}
	var cred *winCredential
	r, _, _ := procCredReadW.Call(uintptr(unsafe.Pointer(name)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&cred)))
	if r == 0 {
		return nil, errors.New(winCredUnavailableReason)
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(cred)))
	if cred.CredentialBlobSize == 0 || cred.CredentialBlob == nil {
		return []byte{}, nil
	}
	blob := unsafe.Slice(cred.CredentialBlob, cred.CredentialBlobSize)
	return append([]byte(nil), blob...), nil
}

func winCredWrite(target, user string, blob []byte) error {
	name, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	userName, err := syscall.UTF16PtrFromString(user)
	if err != nil {
		return err
	}
	cred := winCredential{
		Type:               credTypeGeneric,
		TargetName:         name,
		CredentialBlobSize: uint32(len(blob)),
		Persist:            credPersistLocalMachine,
		UserName:           userName,
	}
	if len(blob) > 0 {
		cred.CredentialBlob = &blob[0]
	}
	if r, _, _ := procCredWriteW.Call(uintptr(unsafe.Pointer(&cred)), 0); r == 0 {
		return errors.New("credential manager update failed")
	}
	return nil
}

func winCredDelete(target string) error {
	name, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	if r, _, _ := procCredDelete.Call(uintptr(unsafe.Pointer(name)), credTypeGeneric, 0); r == 0 {
		return errors.New("credential manager delete failed")
	}
	return nil
}
