package symbolindex

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Snapshot is one immutable, committed index version. All search and outline
// calls observe exactly this complete version even while an update is building
// a replacement snapshot.
type Snapshot struct {
	Version   uint64
	CreatedAt time.Time

	files           map[string]*FileRecord
	fileOrder       []string
	symbols         []Symbol
	byName          map[string][]int
	names           []string
	byKind          map[SymbolKind][]int
	byFile          map[string][]int
	defaultPageSize int
	maxQueryLimit   int
}

// compareSymbol defines global symbol order: file, declaration position,
// kind and finally name. This tie-breaker remains deterministic even for
// duplicate names or pathological declarations at the same offset.
func compareSymbol(a, b Symbol) int {
	switch {
	case a.Path < b.Path:
		return -1
	case a.Path > b.Path:
		return 1
	case a.Line < b.Line:
		return -1
	case a.Line > b.Line:
		return 1
	case a.Column < b.Column:
		return -1
	case a.Column > b.Column:
		return 1
	case a.StartOffset < b.StartOffset:
		return -1
	case a.StartOffset > b.StartOffset:
		return 1
	case a.Kind < b.Kind:
		return -1
	case a.Kind > b.Kind:
		return 1
	case a.Name < b.Name:
		return -1
	case a.Name > b.Name:
		return 1
	default:
		return 0
	}
}

func cloneFileRecord(r *FileRecord) *FileRecord {
	if r == nil {
		return nil
	}
	cp := *r
	if len(r.Symbols) > 0 {
		cp.Symbols = append([]Symbol(nil), r.Symbols...)
	}
	return &cp
}

// newSnapshot builds immutable lookup structures. Records and symbol slices
// are copied, so later mutation of inputs cannot affect published snapshots.
func newSnapshot(cfg Config, version uint64, records []*FileRecord, createdAt time.Time) *Snapshot {
	s := &Snapshot{
		Version:         version,
		CreatedAt:       createdAt.UTC(),
		files:           make(map[string]*FileRecord, len(records)),
		fileOrder:       make([]string, 0, len(records)),
		byName:          make(map[string][]int),
		byKind:          make(map[SymbolKind][]int),
		byFile:          make(map[string][]int),
		defaultPageSize: cfg.DefaultPageSize,
		maxQueryLimit:   cfg.MaxQueryLimit,
	}
	for _, r := range records {
		if r == nil || r.Path == "" {
			continue
		}
		rec := cloneFileRecord(r)
		rec.Path = normalizeQueryPath(rec.Path)
		rec.Symbols = append([]Symbol(nil), rec.Symbols...)
		for i := range rec.Symbols {
			rec.Symbols[i].Path = rec.Path
		}
		sort.SliceStable(rec.Symbols, func(i, j int) bool {
			return compareSymbol(rec.Symbols[i], rec.Symbols[j]) < 0
		})
		s.files[rec.Path] = rec
		s.fileOrder = append(s.fileOrder, rec.Path)
	}
	sort.Strings(s.fileOrder)

	for _, path := range s.fileOrder {
		rec := s.files[path]
		idx0 := len(s.symbols)
		s.symbols = append(s.symbols, rec.Symbols...)
		for i := idx0; i < len(s.symbols); i++ {
			sym := s.symbols[i]
			s.byName[sym.Name] = append(s.byName[sym.Name], i)
			s.byKind[sym.Kind] = append(s.byKind[sym.Kind], i)
			s.byFile[sym.Path] = append(s.byFile[sym.Path], i)
		}
	}

	// Symbol slices are already ordered by file through fileOrder. Keep the
	// invariant explicit so future construction changes cannot alter it.
	sort.SliceStable(s.symbols, func(i, j int) bool {
		return compareSymbol(s.symbols[i], s.symbols[j]) < 0
	})
	s.byName = map[string][]int{}
	s.byKind = map[SymbolKind][]int{}
	s.byFile = map[string][]int{}
	for i, sym := range s.symbols {
		s.byName[sym.Name] = append(s.byName[sym.Name], i)
		s.byKind[sym.Kind] = append(s.byKind[sym.Kind], i)
		s.byFile[sym.Path] = append(s.byFile[sym.Path], i)
	}
	s.names = make([]string, 0, len(s.byName))
	for name := range s.byName {
		s.names = append(s.names, name)
	}
	sort.Strings(s.names)
	return s
}

func normalizeQueryPath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	p = strings.TrimPrefix(p, "./")
	return strings.TrimPrefix(p, "/")
}

// File returns an independent copy of one file record and whether it exists.
func (s *Snapshot) File(path string) (*FileRecord, bool) {
	if s == nil {
		return nil, false
	}
	r, ok := s.files[normalizeQueryPath(path)]
	if !ok {
		return nil, false
	}
	return cloneFileRecord(r), true
}

// Files returns all file records in lexicographic path order. Each returned
// record is a copy and may be modified by the caller.
func (s *Snapshot) Files() []*FileRecord {
	if s == nil {
		return nil
	}
	out := make([]*FileRecord, 0, len(s.fileOrder))
	for _, path := range s.fileOrder {
		out = append(out, cloneFileRecord(s.files[path]))
	}
	return out
}

// SymbolCount returns the number of successfully indexed symbols.
func (s *Snapshot) SymbolCount() int {
	if s == nil {
		return 0
	}
	return len(s.symbols)
}

// Search filters symbols by exact name, prefix, kind and owning file.
//
// Exact Name and Prefix are mutually exclusive: Name wins when both are set.
// Limit <= 0 uses cfg.DefaultPageSize. A positive Limit is capped at
// cfg.MaxQueryLimit. Offset >= Total yields an empty page. Offset < 0 is an
// error. Total always reports the full filtered count before pagination.
func (s *Snapshot) Search(q Query) (SearchResult, error) {
	if s == nil {
		return SearchResult{Symbols: []Symbol{}}, nil
	}
	if q.Offset < 0 {
		return SearchResult{}, fmt.Errorf("offset must be >= 0")
	}
	indices := s.candidateIndices(q)
	filtered := make([]Symbol, 0, len(indices))
	for _, idx := range indices {
		sym := s.symbols[idx]
		if q.Name != "" && sym.Name != q.Name {
			continue
		}
		if q.Name == "" && q.Prefix != "" && !strings.HasPrefix(sym.Name, q.Prefix) {
			continue
		}
		if q.Kind != "" && sym.Kind != q.Kind {
			continue
		}
		if q.Path != "" && sym.Path != normalizeQueryPath(q.Path) {
			continue
		}
		filtered = append(filtered, sym)
	}

	total := len(filtered)
	offset := q.Offset
	if offset > total {
		offset = total
	}
	limit := q.Limit
	if limit <= 0 {
		limit = s.defaultPageSize
		if limit <= 0 {
			limit = defaultPageSize
		}
	}
	maxLimit := s.maxQueryLimit
	if maxLimit <= 0 {
		maxLimit = defaultMaxQueryLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	end := offset + limit
	if end > total {
		end = total
	}
	page := filtered[offset:end]
	out := make([]Symbol, len(page))
	copy(out, page)
	return SearchResult{
		Symbols: out,
		Total:   total,
		Offset:  offset,
		Limit:   limit,
		HasMore: end < total,
	}, nil
}

// candidateIndices starts with the most selective available simple index and
// filters remaining predicates in Search. Candidates remain globally sorted.
func (s *Snapshot) candidateIndices(q Query) []int {
	switch {
	case q.Name != "":
		return append([]int(nil), s.byName[q.Name]...)
	case q.Prefix != "":
		lo := sort.SearchStrings(s.names, q.Prefix)
		hiName := prefixUpperBound(q.Prefix)
		hi := len(s.names)
		if hiName != "" {
			hi = sort.SearchStrings(s.names, hiName)
		}
		var out []int
		for _, name := range s.names[lo:hi] {
			if strings.HasPrefix(name, q.Prefix) {
				out = append(out, s.byName[name]...)
			}
		}
		sort.Ints(out)
		return out
	case q.Kind != "":
		return append([]int(nil), s.byKind[q.Kind]...)
	case q.Path != "":
		return append([]int(nil), s.byFile[normalizeQueryPath(q.Path)]...)
	default:
		out := make([]int, len(s.symbols))
		for i := range s.symbols {
			out[i] = i
		}
		return out
	}
}

// Outline returns one file's declaration outline sorted by source position.
// The second return value is false when the file is not present in this
// snapshot. Pagination fields are intentionally not included because callers
// receive the complete bounded file outline.
func (s *Snapshot) Outline(path string) (OutlineResult, bool) {
	if s == nil {
		return OutlineResult{}, false
	}
	rec, ok := s.files[normalizeQueryPath(path)]
	if !ok {
		return OutlineResult{}, false
	}
	syms := make([]Symbol, len(rec.Symbols))
	copy(syms, rec.Symbols)
	return OutlineResult{
		Path:    rec.Path,
		Status:  rec.Status,
		Reason:  rec.Reason,
		Symbols: syms,
	}, true
}

func prefixUpperBound(prefix string) string {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1])
		}
	}
	return ""
}
