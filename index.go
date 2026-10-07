package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Index is a symbol index over a workspace root. It is safe for concurrent
// use: any number of goroutines may call Snapshot/Search while another
// goroutine calls Rebuild/Update.
type Index struct {
	// cfgMu protects cfg. Rebuild/Update take the lock for the run's duration
	// so SetConfig cannot change configuration halfway through an update (which
	// could otherwise publish a snapshot mixing two configurations); readers
	// that only need the current configuration take it briefly.
	cfgMu sync.RWMutex
	cfg   Config
	// current holds the latest committed *Snapshot. Updates publish a new
	// snapshot via atomic.Store; readers load it without locking.
	current atomic.Pointer[Snapshot]
}

// UpdateReport describes the outcome of a rebuild or update.
type UpdateReport struct {
	// Version is the snapshot version produced by this run.
	Version int64 `json:"version"`
	// Added are files that were not present in the previous snapshot.
	Added []string `json:"added"`
	// Modified are files whose content changed.
	Modified []string `json:"modified"`
	// Removed are files that disappeared from the workspace.
	Removed []string `json:"removed"`
	// Affected is the sorted union of Added, Modified and Removed.
	Affected []string `json:"affected"`
	// TotalFiles is the number of files in the new snapshot.
	TotalFiles int `json:"total_files"`
	// TotalSymbols is the number of symbols in the new snapshot.
	TotalSymbols int `json:"total_symbols"`
	// Degraded is the number of files parsed with errors.
	Degraded int `json:"degraded"`
	// Skipped is the number of files skipped.
	Skipped int `json:"skipped"`
	// ConfigChanged reports whether result-affecting configuration changed
	// compared to the previous snapshot (or the index was opened from a
	// snapshot written before configuration signatures existed).
	ConfigChanged bool `json:"config_changed"`
	// Issues lists paths that could not be traversed or read (e.g. a directory
	// without execute permission). Such paths are skipped or degrade to a
	// skipped entry instead of failing the whole run; the rest of the
	// workspace is still indexed normally. Sorted by Path.
	Issues []Issue `json:"issues"`
}

// Issue describes a workspace path that could not be traversed or read.
type Issue struct {
	// Path is the offending path, relative to the workspace root when
	// possible (the root itself is reported as ".").
	Path string `json:"path"`
	// Scope is "dir" for a directory that could not be traversed (its whole
	// subtree was skipped) or "file" for a file that could not be statted.
	Scope string `json:"scope"`
	// ReasonCode is the stable, machine-readable cause, e.g.
	// ReasonPermissionDenied or ReasonNotReadable.
	ReasonCode ReasonCode `json:"reason_code"`
	// Reason is a human-readable description including the underlying error.
	Reason string `json:"reason"`
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
func (idx *Index) Config() Config {
	idx.cfgMu.RLock()
	defer idx.cfgMu.RUnlock()
	return idx.cfg
}

// SetConfig replaces the indexing configuration. The next Update compares the
// new configuration's signature (see Config.Signature) with the configuration
// of the current snapshot; when result-affecting settings (MaxFileSize,
// IgnorePatterns) changed, every file is reprocessed and the report lists
// exactly those files whose outcome changed, so the resulting snapshot is
// identical to a full rebuild under the new configuration. Root and
// Concurrency never force reprocessing.
func (idx *Index) SetConfig(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	idx.cfgMu.Lock()
	idx.cfg = cfg
	idx.cfgMu.Unlock()
	return nil
}

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
	// Hold cfg for the whole run so SetConfig cannot swap configuration in the
	// middle and produce a snapshot that mixes two configurations.
	idx.cfgMu.RLock()
	defer idx.cfgMu.RUnlock()
	cfg := idx.cfg

	paths, walkIssues, err := walkFiles(cfg.Root, cfg.IgnorePatterns)
	if err != nil {
		return nil, fmt.Errorf("index: walk workspace: %w", err)
	}

	old := idx.current.Load()
	sig := cfg.Signature()
	// A committed snapshot with an empty signature was written before setting
	// signatures existed; treat it like a settings change so its files are
	// reprocessed under the current configuration. The version-0 empty
	// baseline is not a real snapshot and must not count as a change.
	settingsChanged := old.Version > 0 && old.SettingsSig != sig

	oldFiles := old.Files
	if full {
		oldFiles = map[string]*FileResult{}
	}

	var added, modified, removed []string
	var changed []string
	switch {
	case full:
		// Full rebuild: every discovered file is "added" relative to an empty
		// base; classification still goes through diffFiles for stable output.
		added, _, _ = diffFiles(cfg.Root, paths, oldFiles)
		changed = append(changed, added...)
	case settingsChanged:
		// Reprocess every discovered file; whether each one is actually
		// affected is decided afterwards by comparing outcomes.
		changed = append(changed, paths...)
	default:
		added, modified, removed = diffFiles(cfg.Root, paths, oldFiles)
		changed = append(changed, added...)
		changed = append(changed, modified...)
	}

	results, err := idx.process(ctx, cfg, changed)
	if err != nil {
		return nil, err
	}

	// Files present in the committed snapshot but absent from the current walk
	// (deleted on disk, or newly ignored / unreadable-to-traverse) must leave
	// the snapshot so the result matches a full rebuild.
	seen := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		seen[p] = struct{}{}
	}
	var gone []string
	for rel := range old.Files {
		if _, ok := seen[rel]; !ok {
			gone = append(gone, rel)
		}
	}
	sort.Strings(gone)

	newFiles := make(map[string]*FileResult, len(oldFiles)+len(results))
	for p, fr := range oldFiles {
		newFiles[p] = fr
	}
	for _, p := range gone {
		delete(newFiles, p)
	}
	for _, fr := range results {
		newFiles[fr.Path] = fr
	}

	if settingsChanged {
		// Classify by actual outcome difference, so Affected lists exactly the
		// files whose index entry changed under the new configuration — no
		// more and no fewer.
		for _, fr := range results {
			oldFR, ok := old.Files[fr.Path]
			if !ok {
				added = append(added, fr.Path)
			} else if fileOutcome(oldFR) != fileOutcome(fr) {
				modified = append(modified, fr.Path)
			}
		}
		removed = gone
		sort.Strings(added)
		sort.Strings(modified)
	} else if full {
		// Preserve full-rebuild report semantics: every discovered file is
		// reported as added; removals are not itemized.
		removed = nil
	}
	// In the default incremental case diffFiles already filled all three lists
	// (removed coincides with "gone").

	snap := &Snapshot{
		Version:     old.Version + 1,
		Files:       newFiles,
		SettingsSig: sig,
		symbols:     buildSymbols(newFiles),
	}
	idx.current.Store(snap)

	report := &UpdateReport{
		Version:       snap.Version,
		Added:         nonNilStrings(added),
		Modified:      nonNilStrings(modified),
		Removed:       nonNilStrings(removed),
		Affected:      nonNilStrings(union(added, modified, removed)),
		TotalFiles:    len(newFiles),
		TotalSymbols:  len(snap.symbols),
		ConfigChanged: settingsChanged && !full,
		Issues:        walkIssues,
	}
	if report.Issues == nil {
		report.Issues = []Issue{}
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

// outcomeKey is the query-visible essence of a FileResult. Volatile metadata
// (size, mtime, fingerprint) is deliberately excluded: two results with the
// same key are indistinguishable to users, so a configuration change that
// leaves the key equal does not mark the file affected.
type outcomeKey struct {
	status     FileStatus
	reasonCode ReasonCode
	symbols    string
}

// fileOutcome computes the comparable outcome key of fr.
func fileOutcome(fr *FileResult) outcomeKey {
	var b strings.Builder
	for _, s := range fr.Symbols {
		fmt.Fprintf(&b, "%d:%d:%s:%s:%s\n", s.Line, s.Column, s.Name, s.Kind, s.Receiver)
	}
	return outcomeKey{status: fr.Status, reasonCode: fr.ReasonCode, symbols: b.String()}
}

// process indexes the given files concurrently. cfg is the configuration of
// the running update (passed in because run already holds cfgMu and must not
// re-enter its lock). Results occupy preassigned input-order slots, so the
// returned slice is independent of goroutine scheduling.
func (idx *Index) process(ctx context.Context, cfg Config, rels []string) ([]*FileResult, error) {
	results := make([]*FileResult, len(rels))
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup

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
			fr := indexFile(rel, absPath(cfg.Root, rel), cfg.MaxFileSize)
			results[i] = fr
		}(i, rel)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// walkFiles collects all .go files under root that are not ignored.
//
// Unreadable paths do not abort the walk: an unreadable directory is recorded
// as an Issue (scope "dir") together with a stable reason code and its whole
// subtree is skipped; an unreadable file entry is recorded as an Issue (scope
// "file") and skipped. All other files are still returned, so a single bad
// path never prevents the rest of the workspace from being indexed. Only a
// failure to access root itself (e.g. it does not exist) yields a non-nil
// error.
func walkFiles(root string, ignore []string) (paths []string, issues []Issue, err error) {
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == root {
				return walkErr
			}
			rel := mustRel(root, path)
			scope := "file"
			if d != nil && d.IsDir() {
				scope = "dir"
			}
			issues = append(issues, Issue{
				Path:       rel,
				Scope:      scope,
				ReasonCode: classifyOSError(walkErr),
				Reason:     fmt.Sprintf("%s access error: %v", scope, walkErr),
			})
			if scope == "dir" {
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
			paths = append(paths, rel)
		}
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}
	sort.Strings(paths)
	sort.Slice(issues, func(i, j int) bool { return issues[i].Path < issues[j].Path })
	return paths, issues, nil
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
//
// Recovery from skips is handled explicitly, otherwise restoring permissions
// on an unreadable file would leave a stale "skipped" entry:
//   - stat failed previously (entry carries no stat metadata) and succeeds now
//     -> the file became accessible, reprocess;
//   - stat succeeded previously (e.g. read denied) and the file can now be
//     read -> reprocess.
func unchanged(root, rel string, fr *FileResult) bool {
	prevStatOK := fr.Size != 0 || fr.ModTime != 0
	info, err := os.Stat(absPath(root, rel))
	if err != nil {
		// Still unstatable: keep the previous stat-failure entry. Anything
		// else that loses stat ability is "modified".
		return !prevStatOK
	}
	if !prevStatOK {
		return false
	}
	if info.Size() != fr.Size || info.ModTime().UnixNano() != fr.ModTime {
		// Size or mtime changed (or a previously unreadable file may now be
		// readable): confirm with a content hash. A read failure means
		// modified.
		data, err := os.ReadFile(absPath(root, rel))
		if err != nil {
			return false
		}
		return hashBytes(data) == fr.Fingerprint
	}
	// Size/mtime equal, but a file that was skipped after a successful stat
	// (read denied) could now be readable with identical metadata. Verify
	// readability directly for such entries.
	if fr.Status == StatusSkipped {
		data, err := os.ReadFile(absPath(root, rel))
		if err != nil {
			return true
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

// nonNilStrings returns an empty (non-nil) slice when in is nil, so reports
// serialize as [] rather than null.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
