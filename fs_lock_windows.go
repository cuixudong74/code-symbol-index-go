//go:build windows

package symbolindex

import "os"

type fileUnlock struct{}

func (*fileUnlock) Close() error { return nil }

// Windows stores remain crash-safe through atomic temp-file replacement.
// Cross-process advisory locking is a no-op on this build target.
func lockStoreExclusive(dir string) (*fileUnlock, error) { _ = dir; return &fileUnlock{}, nil }
func lockStoreShared(dir string) (*fileUnlock, error)    { _ = dir; return &fileUnlock{}, nil }
