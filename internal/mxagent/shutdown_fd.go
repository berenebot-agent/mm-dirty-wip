package mxagent

import (
	"context"
	"os"
	"strconv"
	"strings"
)

// ShutdownFDEnv names an environment variable holding an inherited file
// descriptor. When set, the process treats EOF on that descriptor as a
// shutdown request. This is how the embedded edge (a child of cmd/server) is
// told to stop: the parent has dropped to a different uid and cannot signal the
// edge, so it closes the write end of a pipe instead.
const ShutdownFDEnv = "MX_SHUTDOWN_FD"

// WatchShutdownFD returns a context cancelled when the inherited descriptor
// named by MX_SHUTDOWN_FD reaches EOF, or the parent context otherwise. When
// the variable is unset or invalid (standalone edge), it returns the parent
// context unchanged.
func WatchShutdownFD(parent context.Context) (context.Context, context.CancelFunc) {
	raw := strings.TrimSpace(os.Getenv(ShutdownFDEnv))
	if raw == "" {
		return context.WithCancel(parent)
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return context.WithCancel(parent)
	}
	f := os.NewFile(uintptr(fd), "mx-shutdown")
	if f == nil {
		return context.WithCancel(parent)
	}
	ctx, cancel := context.WithCancel(parent)
	go func() {
		defer f.Close()
		buf := make([]byte, 1)
		for {
			// EOF (0, nil) or a read error means the parent closed the pipe or
			// exited: treat both as shutdown.
			n, err := f.Read(buf)
			if n == 0 || err != nil {
				break
			}
		}
		cancel()
	}()
	return ctx, cancel
}
