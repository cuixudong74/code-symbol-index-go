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
	// Skipped is the number of files skipped at the file level (oversized,
	// binary, bad encoding, unreadable, ...).
	Skipped int
	// SkippedPaths lists paths that could not be read or traversed at all
	// during this run (unreadable files/directories), with a locatable path
	// and a stable reason code. It never causes the run to fail.
	SkippedPaths []*SkippedPath
	// ConfigChanged reports that result-affecting configuration or the
	// built-in indexing rules changed since the previous committed snapshot.
	// Affected then includes every file whose outcome the change alters.
	ConfigChanged bool
}

// walkResult collects the outcome of a workspace walk.
type walkResult struct {
	// paths are the candidate .go files (relative, sorted).
	paths []string
	// skipped records paths that could not be read or traversed.
	skipped []*SkippedPath
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
	wr, err := walkFiles(idx.cfg.Root, idx.cfg.IgnorePatterns)
	if err != nil {
		return nil, fmt.Errorf("index: walk workspace: %w", err)
	}

	old := idx.current.Load()
	oldFiles := old.Files
	sig := idx.cfg.signature()
	// A config/policy change is only meaningful relative to a previously
	// committed snapshot; the initial empty build is never flagged.
	configChanged := !full && old.Version != 0 &&
		(old.PolicyVersion != sig.PolicyVersion || old.MaxFileSize != sig.MaxFileSize)
	if full {
		oldFiles = map[string]*FileResult{}
	}

	added, modified, removed := diffFiles(idx.cfg.Root, wr.paths, oldFiles, sig, old, configChanged)
	// Normalize to non-nil slices so JSON reports always show arrays.
	if added == nil {
		added = []string{}
	}
	if modified == nil {
		modified = []string{}
	}
	if removed == nil {
		removed = []string{}
	}
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

	skippedPaths := make(map[string]*SkippedPath, len(wr.skipped))
	for _, sp := range wr.skipped {
		skippedPaths[sp.Path] = sp
	}

	snap := &Snapshot{
		Version:       old.Version + 1,
		PolicyVersion: sig.PolicyVersion,
		MaxFileSize:   sig.MaxFileSize,
		Files:         newFiles,
		Skipped:       skippedPaths,
		symbols:       buildSymbols(newFiles),
	}
	idx.current.Store(snap)

	skippedList := wr.skipped
	if skippedList == nil {
		skippedList = []*SkippedPath{}
	}

	report := &UpdateReport{
		Version:       snap.Version,
		Added:         added,
		Modified:      modified,
		Removed:       removed,
		Affected:      union(added, modified, removed),
		TotalFiles:    len(newFiles),
		TotalSymbols:  len(snap.symbols),
		SkippedPaths:  skippedList,
		ConfigChanged: configChanged,
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

// walkFiles collects all .go files under root that are not ignored. A single
// unreadable path never fails the walk: the path (file or directory) is
// recorded with a stable reason code in the returned walkResult and traversal
// continues with the rest of the workspace. Only when root itself cannot be
// opened at all is an error returned, because then there is nothing to index.
func walkFiles(root string, ignore []string) (*walkResult, error) {
	res := &walkResult{}
	recordSkip := func(path string, isDir bool, err error) {
		rel := mustRel(root, path)
		res.skipped = append(res.skipped, &SkippedPath{
			Path:       rel,
			IsDir:      isDir,
			ReasonCode: classifyAccessError(err),
			Reason:     fmt.Sprintf("%s: %v", accessVerb(isDir), err),
		})
	}

	// Fail fast only if the root itself is unusable: WalkDir invokes the
	// callback with the root error before any other entry.
	rootInfo, rootErr := os.Lstat(root)
	if rootErr != nil {
		return nil, fmt.Errorf("index: open workspace root %q: %w", root, rootErr)
	}
	if !rootInfo.IsDir() {
		return nil, fmt.Errorf("index: workspace root %q is not a directory", root)
	}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// Could not lstat the entry, or could not read the directory's
			// contents. SkipDir is only valid/meaningful for directories;
			// returning it after an entry-level error also safely prunes when
			// the entry turns out to be a directory.
			isDir := d != nil && d.IsDir()
			recordSkip(path, isDir, err)
			if isDir {
				return filepath.SkipDir
			}
			return nil
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
			res.paths = append(res.paths, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(res.paths)
	sort.Slice(res.skipped, func(i, j int) bool { return res.skipped[i].Path < res.skipped[j].Path })
	return res, nil
}

// accessVerb renders the operation that failed for a skipped path.
func accessVerb(isDir bool) string {
	if isDir {
		return "read directory"
	}
	return "stat path"
}

func mustRel(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

// diffFiles compares the current file list against the previous snapshot and
// classifies each file as added, modified or removed. In addition, when
// result-affecting configuration (MaxFileSize) or the built-in indexing
// rules (PolicyVersion) changed, every unchanged file whose outcome the
// change can alter is classified as modified, so an incremental update
// produces exactly the files a full rebuild would under the new settings.
func diffFiles(root string, paths []string, old map[string]*FileResult, sig policySignature, oldSnap *Snapshot, configChanged bool) (added, modified, removed []string) {
	seen := make(map[string]struct{}, len(paths))
	for _, rel := range paths {
		seen[rel] = struct{}{}
		oldFR, ok := old[rel]
		if !ok {
			added = append(added, rel)
			continue
		}
		if contentModified(root, rel, oldFR) {
			modified = append(modified, rel)
			continue
		}
		if configChanged && policyAffects(oldFR, sig, oldSnap) {
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

// contentModified reports whether rel's content changed relative to fr. It
// first uses the cheap size/mtime check and only hashes content when that
// check is inconclusive (e.g. the file was touched but not modified).
func contentModified(root, rel string, fr *FileResult) bool {
	info, err := os.Stat(absPath(root, rel))
	if err != nil {
		return true
	}
	if info.Size() != fr.Size || info.ModTime().UnixNano() != fr.ModTime {
		// Size or mtime changed: confirm with a content hash.
		data, err := os.ReadFile(absPath(root, rel))
		if err != nil {
			return true
		}
		return hashBytes(data) != fr.Fingerprint
	}
	return false
}

// policyAffects reports whether an unchanged file's result would differ
// between the old snapshot (produced under oldSnap.PolicyVersion /
// oldSnap.MaxFileSize) and the current policy sig.
func policyAffects(oldFR *FileResult, sig policySignature, oldSnap *Snapshot) bool {
	rulesChanged := oldSnap.PolicyVersion != sig.PolicyVersion
	limitChanged := oldSnap.MaxFileSize != sig.MaxFileSize

	if rulesChanged {
		// A rules upgrade is a rare, one-time event. For exact equivalence
		// with a full rebuild we reprocess every unchanged file, except files
		// whose disposition is and remains "too large": that decision is
		// rule-independent (a pure size comparison against the already
		// recorded size) and reprocessing it would mean reading a potentially
		// huge file for no change. Every other file is reprocessed with no
		// extra probing. Once the new snapshot is stamped with the new
		// PolicyVersion, later runs take the cheap content-only fast path.
		if oldFR.ReasonCode == ReasonTooLarge &&
			sig.MaxFileSize > 0 && oldFR.Size > sig.MaxFileSize {
			return false
		}
		return true
	}

	if limitChanged {
		// Only the size limit changed; binary/encoding/parse outcomes are
		// size-independent. A file is affected exactly when its size-limit
		// disposition differs between old and new.
		tooLargeNow := sig.MaxFileSize > 0 && oldFR.Size > sig.MaxFileSize
		wasTooLarge := oldFR.ReasonCode == ReasonTooLarge
		return tooLargeNow != wasTooLarge
	}

	return false
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
