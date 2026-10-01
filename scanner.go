package symbolindex

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type scannedFile struct {
	path    string
	size    int64
	modTime time.Time
}

// isGoFile identifies the files understood by the built-in parser. Other
// regular files are scanned but intentionally do not produce file records.
func isGoFile(relPath string) bool {
	return strings.HasSuffix(relPath, ".go")
}

// scanWorkspace returns paths in lexicographic order relative to cfg.Root.
// filepath.WalkDir already reads directory entries in lexical order, and the
// final sort makes the contract independent of any platform-specific walker
// change. Ignored directories are pruned before descending.
func scanWorkspace(ctx context.Context, cfg Config) ([]scannedFile, error) {
	matcher, err := newIgnoreMatcher(cfg.IgnorePatterns)
	if err != nil {
		return nil, err
	}

	var files []scannedFile
	err = filepath.WalkDir(cfg.Root, func(absPath string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if absPath == cfg.Root {
				return &FileError{Path: ".", Op: "open_root", Err: err}
			}
			// Skip only the unreadable entry. A per-directory permission
			// failure must not prevent independent directories from indexing.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(cfg.Root, absPath)
		if err != nil {
			return &FileError{Path: absPath, Op: "rel_path", Err: err}
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if isRelPathUnsafe(rel) {
			return &FileError{Path: rel, Op: "validate_path", Err: errors.New("path escapes workspace root")}
		}

		isDir := d.IsDir()
		if matcher.ignores(rel, isDir) {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}
		if isDir {
			return nil
		}
		// Only regular files have stable read semantics. Symlinks and special
		// device files are ignored deterministically and are not reported as
		// parse failures.
		if d.Type()&fs.ModeType != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return &FileError{Path: rel, Op: "stat_file", Err: err}
		}
		if !isGoFile(rel) {
			return nil
		}
		files = append(files, scannedFile{
			path:    rel,
			size:    info.Size(),
			modTime: info.ModTime().UTC(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files, nil
}

func isRelPathUnsafe(rel string) bool {
	clean := filepath.ToSlash(filepath.Clean(rel))
	if clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(rel) {
		return true
	}
	return false
}
