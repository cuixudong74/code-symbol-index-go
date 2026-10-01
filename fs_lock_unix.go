//go:build !windows

package symbolindex

import (
	"fmt"
	"os"
	"syscall"
)

type fileUnlock struct{ f *os.File }

func (u *fileUnlock) Close() error {
	defer u.f.Close()
	if err := syscall.Flock(int(u.f.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("unlock index store: %w", err)
	}
	return nil
}

func openStoreLock(dir string, exclusive bool) (*fileUnlock, error) {
	path := dir + "/.index.lock"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open index store lock: %w", err)
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock index store: %w", err)
	}
	return &fileUnlock{f: f}, nil
}

func lockStoreExclusive(dir string) (*fileUnlock, error) { return openStoreLock(dir, true) }
func lockStoreShared(dir string) (*fileUnlock, error)    { return openStoreLock(dir, false) }
