package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
)

// Index is a symbol index over a workspace root. It is safe for concurrent
// use: any number of goroutines may call Snapshot/Search while another
// goroutine calls Rebuild/Update.
type Index struct {
	cfg Config
	// current holds the latest committed *Snapshot. Updates publish a new
	// snapshot via atomic.Store; readers load it without locking.
	current atomic.Pointer[Snapshot]
}

// UpdateReport describes the outcome of a rebuild or update.
type UpdateReport struct {
	// Version is the snapshot version produced by this run.
	Version int64
	// Added are files that were not present in the previous snapshot.
	Added []string
	// Modified are files whose content changed.
	Modified []string
	// Removed are files that disappeared from the workspace.
	Removed []string
	// Affected is the sorted union of Added, Modified and Removed.
	Affected []string
	// TotalFiles is the number of files in the new snapshot.
	TotalFiles int
	// TotalSymbols is the number of symbols in the new snapshot.
	TotalSymbols int
	// Degraded is the number of files parsed with errors.
	Degraded int
	// Skipped is the number of files skipped.
	Skipped int
}

// New creates an empty index for cfg.
func New(cfg Config) (*Index, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	idx := &Index{cfg: cfg}
	idx.current.Store(&Snapshot{Version: 0, Files: map[string]*FileResult{}})
	return idx, nil
}

// Config returns a copy of the index configuration.
func (idx *Index) Config() Config { return idx.cfg }

// Snapshot returns the currently committed snapshot. The returned snapshot is
// immutable and safe to query concurrently with updates.
func (idx *Index) Snapshot() *Snapshot { return idx.current.Load() }

// Rebuild performs a full rebuild from scratch. The new snapshot is only
// published after every file has been processed successfully.
func (idx *Index) Rebuild(ctx context.Context) (*UpdateReport, error) {
	return idx.run(ctx, true)
}

// Update performs an incremental update: only added, modified or removed
// files are reprocessed. The resulting snapshot is guaranteed to contain
// exactly the same symbols as a full rebuild. If no prior snapshot exists,
// a full rebuild is performed.
func (idx *Index) Update(ctx context.Context) (*UpdateReport, error) {
	return idx.run(ctx, false)
}

func (idx *Index) run(ctx context.Context, full bool) (*UpdateReport, error) {
	paths, err := walkFiles(idx.cfg.Root, idx.cfg.IgnorePatterns)
	if err != nil {
		return nil, fmt.Errorf("index: walk workspace: %w", err)
	}

	old := idx.current.Load()
	oldFiles := old.Files
	if full {
		oldFiles = map[string]*FileResult{}
	}

	added, modified, removed := diffFiles(idx.cfg.Root, paths, oldFiles)
	changed := make([]string, 0, len(added)+len(modified))
	changed = append(changed, added...)
	changed = append(changed, modified...)

	results, err := idx.process(ctx, changed)
	if err != nil {
		return nil, err
	}

	newFiles := make(map[string]*FileResult, len(oldFiles)+len(results))
	for p, fr := range oldFiles {
		newFiles[p] = fr
	}
	for _, p := range removed {
		delete(newFiles, p)
	}
	for _, fr := range results {
		newFiles[fr.Path] = fr
	}

	snap := &Snapshot{
		Version: old.Version + 1,
		Files:   newFiles,
		symbols: buildSymbols(newFiles),
	}
	idx.current.Store(snap)

	report := &UpdateReport{
		Version:      snap.Version,
		Added:        added,
		Modified:     modified,
		Removed:      removed,
		Affected:     union(added, modified, removed),
		TotalFiles:   len(newFiles),
		TotalSymbols: len(snap.symbols),
	}
	for _, fr := range newFiles {
		switch fr.Status {
		case StatusDegraded:
			report.Degraded++
		case StatusSkipped:
			report.Skipped++
		}
	}
	return report, nil
}

// process indexes the given files concurrently. Results are returned in input
// order, independent of goroutine scheduling.
func (idx *Index) process(ctx context.Context, rels []string) ([]*FileResult, error) {
	results := make([]*FileResult, len(rels))
	sem := make(chan struct{}, idx.cfg.Concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for i, rel := range rels {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		wg.Add(1)
		go func(i int, rel string) {
			defer wg.Done()
			defer func() { <-sem }()
			fr := indexFile(rel, absPath(idx.cfg.Root, rel), idx.cfg.MaxFileSize)
			mu.Lock()
			results[i] = fr
			mu.Unlock()
		}(i, rel)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// walkFiles collects all .go files under root that are not ignored.
func walkFiles(root string, ignore []string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && matchIgnore(mustRel(root, path), ignore) {
				return filepath.SkipDir
			}
			return nil
		}
		rel := mustRel(root, path)
		if matchIgnore(rel, ignore) {
			return nil
		}
		if filepath.Ext(rel) == ".go" {
			paths = append(paths, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

func mustRel(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

// diffFiles compares the current file list against the previous snapshot and
// classifies each file as added, modified or removed.
func diffFiles(root string, paths []string, old map[string]*FileResult) (added, modified, removed []string) {
	seen := make(map[string]struct{}, len(paths))
	for _, rel := range paths {
		seen[rel] = struct{}{}
		oldFR, ok := old[rel]
		if !ok {
			added = append(added, rel)
			continue
		}
		if !unchanged(root, rel, oldFR) {
			modified = append(modified, rel)
		}
	}
	for rel := range old {
		if _, ok := seen[rel]; !ok {
			removed = append(removed, rel)
		}
	}
	sort.Strings(added)
	sort.Strings(modified)
	sort.Strings(removed)
	return
}

// unchanged reports whether rel is unchanged compared to fr. It first uses
// the cheap size/mtime check and only hashes content when that check is
// inconclusive (e.g. the file was touched but not modified).
func unchanged(root, rel string, fr *FileResult) bool {
	info, err := os.Stat(absPath(root, rel))
	if err != nil {
		return false
	}
	if info.Size() != fr.Size || info.ModTime().UnixNano() != fr.ModTime {
		// Size or mtime changed: confirm with a content hash.
		data, err := os.ReadFile(absPath(root, rel))
		if err != nil {
			return false
		}
		return hashBytes(data) == fr.Fingerprint
	}
	return true
}

// union returns the sorted union of the given string slices.
func union(slices ...[]string) []string {
	set := make(map[string]struct{})
	for _, s := range slices {
		for _, v := range s {
			set[v] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
