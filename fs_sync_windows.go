//go:build windows

package symbolindex

// Directory entries are made durable by the flushed and renamed data/manifest
// files. There is no portable directory fsync operation used on Windows.
func syncDirectory(dir string) error { _ = dir; return nil }
