//go:build !windows

package node

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"agent-gateway/internal/protocol"
)

// acquireInstanceLock takes an exclusive advisory lock on path and returns the
// release function.
//
// Two node processes sharing a directory would each claim work and each run it,
// so the lock is held for the whole lifetime of the loop. The kernel drops it
// when the process dies, which is why a crash cannot leave a stale lock behind
// the way a marker file would.
func acquireInstanceLock(path string) (func() error, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open instance lock: %w", err)
	}

	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: another node process already holds %s", protocol.ErrConflict, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	return func() error {
		defer file.Close()
		return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	}, nil
}
