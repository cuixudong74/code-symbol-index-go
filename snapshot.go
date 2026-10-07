package index

import (
	"sort"
	"strings"
)

// Snapshot is an immutable, complete index version. Queries run against a
// Snapshot without any locking; updates build a new Snapshot and publish it
// atomically, so readers always observe one complete version and slow queries
// never block updates.
type Snapshot struct {
	// Version is the monotonically increasing version number.
	Version int64 `json:"version"`
	// PolicyVersion identifies the indexing rules (binary detection, encoding
	// rules, ...) that produced this snapshot. When the running binary's
	// CurrentPolicyVersion differs, an incremental update re-evaluates every
	// unchanged file so its outcome stays identical to a full rebuild.
	PolicyVersion int `json:"policy_version"`
	// MaxFileSize is the per-file size limit (Config.MaxFileSize) that was in
	// effect when this snapshot was produced. It is persisted so that after a
	// restart an incremental update can still detect a size-limit change and
	// reprocess exactly the files whose outcome it alters.
	MaxFileSize int64 `json:"max_file_size,omitempty"`
	// Files maps relative file paths to their indexing outcome.
	Files map[string]*FileResult `json:"files"`
	// Skipped maps relative (or absolute when the path cannot be relativized)
	// paths that could not even be read or traversed to the cause.
	Skipped map[string]*SkippedPath `json:"skipped,omitempty"`

	// symbols is the globally sorted symbol table. It is derived from Files
	// and never persisted.
	symbols []Symbol
}

// Query selects symbols from a Snapshot. All fields are optional; an empty
// Query matches every symbol.
type Query struct {
	// Name matches symbols with exactly this name (case-sensitive).
	Name string
	// Prefix matches symbols whose name starts with this prefix
	// (case-sensitive).
	Prefix string
	// Kind restricts results to this symbol kind.
	Kind Kind
	// File restricts results to this file (relative path).
	File string
	// Offset skips the first N matches (after filtering). Offset >= number of
	// matches yields an empty result.
	Offset int
	// Limit caps the number of returned matches (after Offset). Limit <= 0
	// means no cap.
	Limit int
}

// buildSymbols derives the sorted global symbol table from the per-file
// results. Sorting makes output independent of filesystem walk order and map
// iteration order.
func buildSymbols(files map[string]*FileResult) []Symbol {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	syms := make([]Symbol, 0)
	for _, p := range paths {
		fr := files[p]
		for _, s := range fr.Symbols {
			s.File = p
			syms = append(syms, s)
		}
	}
	sort.SliceStable(syms, func(i, j int) bool {
		a, b := syms[i], syms[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Column != b.Column {
			return a.Column < b.Column
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Kind < b.Kind
	})
	return syms
}

// SymbolCount returns the total number of symbols in the snapshot.
func (s *Snapshot) SymbolCount() int { return len(s.symbols) }

// FileCount returns the number of indexed files in the snapshot.
func (s *Snapshot) FileCount() int { return len(s.Files) }

// File returns the FileResult for the given relative path.
func (s *Snapshot) File(path string) (*FileResult, bool) {
	fr, ok := s.Files[path]
	return fr, ok
}

// SortedFiles returns all file results ordered by path.
func (s *Snapshot) SortedFiles() []*FileResult {
	paths := make([]string, 0, len(s.Files))
	for p := range s.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]*FileResult, 0, len(paths))
	for _, p := range paths {
		out = append(out, s.Files[p])
	}
	return out
}

// Search returns the symbols matching q. Results are always sorted by
// (File, Line, Column, Name, Kind). Offset and Limit are applied after
// filtering in that order.
func (s *Snapshot) Search(q Query) []Symbol {
	out := make([]Symbol, 0)
	for _, sym := range s.symbols {
		if q.Name != "" && sym.Name != q.Name {
			continue
		}
		if q.Prefix != "" && !strings.HasPrefix(sym.Name, q.Prefix) {
			continue
		}
		if q.Kind != "" && sym.Kind != q.Kind {
			continue
		}
		if q.File != "" && sym.File != q.File {
			continue
		}
		out = append(out, sym)
	}
	if q.Offset > 0 {
		if q.Offset >= len(out) {
			return []Symbol{}
		}
		out = out[q.Offset:]
	}
	if q.Limit > 0 && q.Limit < len(out) {
		out = out[:q.Limit]
	}
	return out
}

// Outline returns all symbols of a single file, sorted by position.
func (s *Snapshot) Outline(file string) []Symbol {
	return s.Search(Query{File: file})
}

// StatusCounts returns the number of files in each status.
func (s *Snapshot) StatusCounts() (ok, degraded, skipped int) {
	for _, fr := range s.Files {
		switch fr.Status {
		case StatusOK:
			ok++
		case StatusDegraded:
			degraded++
		case StatusSkipped:
			skipped++
		}
	}
	return
}

// SkippedCount returns the number of paths that could not be read or
// traversed (files recorded separately from FileResult-level skips).
func (s *Snapshot) SkippedCount() int { return len(s.Skipped) }

// SortedSkipped returns all unreadable/un-traversable path records ordered by
// path.
func (s *Snapshot) SortedSkipped() []*SkippedPath {
	paths := make([]string, 0, len(s.Skipped))
	for p := range s.Skipped {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]*SkippedPath, 0, len(paths))
	for _, p := range paths {
		out = append(out, s.Skipped[p])
	}
	return out
}
