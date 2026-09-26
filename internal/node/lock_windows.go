//go:build windows

package node

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"

	"agent-gateway/internal/protocol"
)

// acquireInstanceLock takes an exclusive lock on one byte of path and returns
// the release function.
//
// Two node processes sharing a directory would each claim work and each run it,
// so the lock is held for the whole lifetime of the loop. Windows releases the
// lock when the handle is closed, including on process death, so a crash cannot
// leave a stale lock behind the way a marker file would.
func acquireInstanceLock(path string) (func() error, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open instance lock: %w", err)
	}

	handle := windows.Handle(file.Fd())
	overlapped := new(windows.Overlapped)
	err = windows.LockFileEx(handle,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, overlapped)
	if err != nil {
		file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, fmt.Errorf("%w: another node process already holds %s", protocol.ErrConflict, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	return func() error {
		defer file.Close()
		return windows.UnlockFileEx(handle, 0, 1, 0, overlapped)
	}, nil
}
