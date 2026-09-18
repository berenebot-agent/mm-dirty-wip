//go:build !linux

package admincli

import (
	"errors"
	"os"
)

// isTerminalFD is always false off Linux; interactive password entry is not
// supported there and callers must use --password-file.
func isTerminalFD(fd int) bool { return false }

func readSecretLine(f *os.File) (string, error) {
	return "", errors.New("interactive password input is unsupported on this platform; use --password-file")
}
