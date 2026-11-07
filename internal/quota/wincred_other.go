//go:build !windows

package quota

import "errors"

// The Credential Manager exists only on Windows; other systems keep the
// keychain path in discover.go.

func winCredRead(string) ([]byte, error) {
	return nil, errors.New("credential manager item unavailable")
}

func winCredWrite(string, string, []byte) error {
	return errors.New("credential manager unavailable")
}

func winCredDelete(string) error {
	return errors.New("credential manager unavailable")
}
