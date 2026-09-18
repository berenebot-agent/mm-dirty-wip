//go:build linux

package admincli

import (
	"bufio"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// isTerminalFD reports whether fd refers to a terminal.
func isTerminalFD(fd int) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&t)), 0, 0, 0)
	return errno == 0
}

// readSecretLine reads one line from a terminal with echo disabled, restoring
// the previous terminal state before returning. It is intentionally limited to
// Linux, the only platform Gatehouse ships on; other builds use
// --password-file only.
func readSecretLine(f *os.File) (string, error) {
	fd := int(f.Fd())
	var old syscall.Termios
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&old)), 0, 0, 0); errno != 0 {
		return "", errno
	}
	hidden := old
	hidden.Lflag &^= syscall.ECHO
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&hidden)), 0, 0, 0); errno != 0 {
		return "", errno
	}
	defer func() {
		_, _, _ = syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&old)), 0, 0, 0)
	}()
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
