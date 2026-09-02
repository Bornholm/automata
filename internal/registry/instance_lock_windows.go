//go:build windows

package registry

import (
	"os"

	"golang.org/x/sys/windows"
)

// tryLockFile pose un verrou exclusif non bloquant par LockFileEx —
// l'équivalent Windows de flock, avec la même propriété : le verrou est
// libéré par le système à la mort du processus.
func tryLockFile(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.LockFileEx(windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &overlapped)
}

func unlockFile(file *os.File) {
	var overlapped windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}
