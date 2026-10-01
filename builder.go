package symbolindex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// buildSnapshot incrementally builds a snapshot from previous. Only files whose
// size/modtime changed are read and parsed; all other file records are reused.
// The final records are ordered by path and published atomically.
func buildSnapshot(ctx context.Context, cfg Config, previous *Snapshot) (*Snapshot, Report, error) {
	start := time.Now()
	scanned, err := scanWorkspace(ctx, cfg)
	if err != nil {
		return nil, Report{}, err
	}

	old := map[string]*FileRecord{}
	if previous != nil {
		for _, rec := range previous.Files() {
			old[rec.Path] = rec
		}
	}

	records := make([]*FileRecord, len(scanned))
	sem := make(chan struct{}, cfg.Workers)
	var wg sync.WaitGroup
	var workersErr error
	var errMu sync.Mutex
	setErr := func(err error) {
		errMu.Lock()
		if workersErr == nil {
			workersErr = err
		}
		errMu.Unlock()
	}

	for i, sf := range scanned {
		select {
		case <-ctx.Done():
			return nil, Report{}, ctx.Err()
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(i int, sf scannedFile) {
			defer wg.Done()
			defer func() { <-sem }()
			rec, err := processWorkspaceFile(ctx, cfg, sf, old[sf.path])
			if err != nil {
				setErr(err)
				return
			}
			records[i] = rec
		}(i, sf)
	}
	wg.Wait()
	if workersErr != nil {
		return nil, Report{}, workersErr
	}
	if ctx.Err() != nil {
		return nil, Report{}, ctx.Err()
	}

	affected := classifyAffected(scanned, records, old)
	version := uint64(1)
	if previous != nil && previous.Version > 0 {
		version = previous.Version + 1
	}
	snapshot := newSnapshot(cfg, version, records, time.Now().UTC())
	report := Report{
		Version:      version,
		ScannedFiles: len(records),
		Affected:     affected,
		Duration:     time.Since(start),
	}
	for _, rec := range snapshot.Files() {
		if rec.Status == StatusOK {
			report.IndexedFiles++
			report.SymbolCount += len(rec.Symbols)
			continue
		}
		if rec.Status == StatusSkippedTooLarge {
			report.SkippedFiles++
		} else {
			report.FailureFiles++
		}
		report.Errors = append(report.Errors, FileError{
			Path:    rec.Path,
			Op:      string(rec.Status),
			Err:     StringError(rec.Reason),
			Message: rec.Reason,
		})
	}
	return snapshot, report, nil
}

func processWorkspaceFile(ctx context.Context, cfg Config, sf scannedFile, old *FileRecord) (*FileRecord, error) {
	base := &FileRecord{
		Path:    sf.path,
		Size:    sf.size,
		ModTime: sf.modTime,
	}
	if cfg.MaxFileSize >= 0 && sf.size > cfg.MaxFileSize {
		base.Status = StatusSkippedTooLarge
		base.ReasonCode = "file_too_large"
		base.Reason = "file exceeds configured MaxFileSize"
		// Preserve an old hash when available; over-limit content is not read.
		if old != nil && old.Path == sf.path {
			base.Hash = old.Hash
		}
		return base, nil
	}
	if old != nil &&
		old.Path == sf.path &&
		old.Size == sf.size &&
		old.ModTime.Equal(sf.modTime) &&
		// A larger MaxFileSize configuration can make a previously skipped
		// file eligible, so do not reuse the old over-limit placeholder.
		old.Status != StatusSkippedTooLarge {
		rec := cloneFileRecord(old)
		rec.Path = sf.path
		rec.Size = sf.size
		rec.ModTime = sf.modTime
		return rec, nil
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	abs := filepath.Join(cfg.Root, filepath.FromSlash(sf.path))
	content, err := os.ReadFile(abs)
	if err != nil {
		base.Status = StatusReadError
		base.ReasonCode = "read_failed"
		base.Reason = err.Error()
		// A per-file I/O failure is recorded on that file record; it must not
		// prevent the rest of the workspace from becoming queryable.
		return base, nil
	}
	sum := sha256.Sum256(content)
	base.Hash = hex.EncodeToString(sum[:])
	symbols, status, reasonCode, reason := ExtractSymbols(sf.path, content)
	base.Status = status
	base.ReasonCode = reasonCode
	base.Reason = reason
	if status == StatusOK {
		base.Symbols = symbols
	}
	return base, nil
}

func classifyAffected(scanned []scannedFile, records []*FileRecord, old map[string]*FileRecord) AffectedFiles {
	seen := make(map[string]struct{}, len(scanned))
	var affected AffectedFiles
	for i, sf := range scanned {
		seen[sf.path] = struct{}{}
		rec := records[i]
		prev, existed := old[sf.path]
		if !existed {
			affected.Added = append(affected.Added, sf.path)
			continue
		}
		changedMetadata := prev.Size != sf.size || !prev.ModTime.Equal(sf.modTime)
		if !changedMetadata && prev.Status == rec.Status && prev.ReasonCode == rec.ReasonCode && prev.Hash == rec.Hash {
			continue
		}
		// Oversized/read-failed records may have no newly computed hash. Treat
		// observable metadata changes as modifications conservatively.
		contentMayDiffer := rec.Hash == "" || prev.Hash == "" || prev.Hash != rec.Hash
		statusChanged := prev.Status != rec.Status || prev.ReasonCode != rec.ReasonCode
		if changedMetadata && (contentMayDiffer || statusChanged) {
			affected.Modified = append(affected.Modified, sf.path)
		} else if changedMetadata {
			affected.MetadataChanged = append(affected.MetadataChanged, sf.path)
		} else if statusChanged {
			affected.Modified = append(affected.Modified, sf.path)
		}
	}
	for path := range old {
		if _, ok := seen[path]; !ok {
			affected.Deleted = append(affected.Deleted, path)
		}
	}
	sortAffected(&affected)
	return affected
}

func sortAffected(a *AffectedFiles) {
	sortStrings(a.Added)
	sortStrings(a.Deleted)
	sortStrings(a.Modified)
	sortStrings(a.MetadataChanged)
}

func sortStrings(s []string) {
	// Local sort avoids another import dominating the small builder file.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
