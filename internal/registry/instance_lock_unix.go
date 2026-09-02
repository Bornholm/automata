//go:build !windows

package registry

import (
	"os"
	"syscall"
)

// tryLockFile pose un flock exclusif non bloquant. Le verrou tombe de
// lui-même à la mort du processus, y compris tué.
func tryLockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockFile(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
