package index

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// ---------- helpers ----------

func newTestWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return dir
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testConfig(root string) Config {
	cfg := DefaultConfig(root)
	cfg.Concurrency = 2
	cfg.IgnorePatterns = []string{".git", ".codeidx"}
	return cfg
}

// snapshotSymbols returns the globally sorted symbol slice of a snapshot.
func snapshotSymbols(snap *Snapshot) []Symbol {
	return snap.symbols
}

func symbolKeys(syms []Symbol) []string {
	out := make([]string, len(syms))
	for i, s := range syms {
		out[i] = fmt.Sprintf("%s:%d:%d:%s:%s:%s", s.File, s.Line, s.Column, s.Name, s.Kind, s.Receiver)
	}
	return out
}

func requireSameSymbols(t *testing.T, got, want []Symbol) {
	t.Helper()
	g := symbolKeys(got)
	w := symbolKeys(want)
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("symbol mismatch:\n got: %v\nwant: %v", g, w)
	}
}

// ---------- tests ----------

func TestBuildAndQuery(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "a.go", `package a

import "fmt"

const C = 1

var V = "x"

type T struct {
	F int
}

func (t *T) M() { fmt.Println(t.F) }

func F() {}
`)
	writeFile(t, root, "sub/b.go", `package sub

type I interface {
	N()
}
`)

	idx, err := New(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	report, err := idx.Rebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalFiles != 2 {
		t.Fatalf("TotalFiles = %d, want 2", report.TotalFiles)
	}
	if report.TotalSymbols != idx.Snapshot().SymbolCount() {
		t.Fatalf("report symbols %d != snapshot %d", report.TotalSymbols, idx.Snapshot().SymbolCount())
	}

	snap := idx.Snapshot()

	// Exact name.
	if got := snap.Search(Query{Name: "M"}); len(got) != 1 || got[0].Kind != KindMethod || got[0].Receiver != "T" {
		t.Fatalf("name search M: %+v", got)
	}
	// Prefix.
	if got := snap.Search(Query{Prefix: "F"}); len(got) != 2 { // F func, F field
		t.Fatalf("prefix F: %v", symbolKeys(got))
	}
	// Kind.
	if got := snap.Search(Query{Kind: KindType}); len(got) != 2 {
		t.Fatalf("kind type: %v", symbolKeys(got))
	}
	// File filter.
	if got := snap.Outline("sub/b.go"); len(got) != 3 { // package + type + method
		t.Fatalf("outline sub/b.go: %v", symbolKeys(got))
	}
	// Missing file.
	if got := snap.Outline("nope.go"); len(got) != 0 {
		t.Fatalf("outline missing file should be empty")
	}
}

func TestDeterminismAcrossConcurrency(t *testing.T) {
	root := newTestWorkspace(t)
	for i := 0; i < 12; i++ {
		writeFile(t, root, fmt.Sprintf("pkg%02d/file.go", i), fmt.Sprintf(`package pkg%02d

const C%d = %d

type T%d struct {
	Field%d int
}

func (t *T%d) Method%d() {}

func Func%d() {}
`, i, i, i, i, i, i, i, i))
	}

	var baseline []Symbol
	for _, conc := range []int{1, 2, 4, 8, 16} {
		cfg := testConfig(root)
		cfg.Concurrency = conc
		idx, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := idx.Rebuild(context.Background()); err != nil {
			t.Fatal(err)
		}
		syms := snapshotSymbols(idx.Snapshot())
		if baseline == nil {
			baseline = syms
			continue
		}
		requireSameSymbols(t, syms, baseline)
	}
	if len(baseline) == 0 {
		t.Fatal("expected symbols")
	}
}

func TestIncrementalMatchesFull(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "a.go", `package a

const A = 1
`)
	writeFile(t, root, "b.go", `package a

const B = 2
`)

	incIdx, err := New(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := incIdx.Update(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Series of incremental changes.
	writeFile(t, root, "a.go", `package a

const A = 10
const A2 = 11
`)
	writeFile(t, root, "c.go", `package a

const C = 3
`)
	if err := os.Remove(filepath.Join(root, "b.go")); err != nil {
		t.Fatal(err)
	}
	report, err := incIdx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.Added, []string{"c.go"}) {
		t.Fatalf("Added = %v", report.Added)
	}
	if !reflect.DeepEqual(report.Modified, []string{"a.go"}) {
		t.Fatalf("Modified = %v", report.Modified)
	}
	if !reflect.DeepEqual(report.Removed, []string{"b.go"}) {
		t.Fatalf("Removed = %v", report.Removed)
	}
	if !reflect.DeepEqual(report.Affected, []string{"a.go", "b.go", "c.go"}) {
		t.Fatalf("Affected = %v", report.Affected)
	}

	// Full rebuild on a fresh index over the same final workspace.
	fullIdx, err := New(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fullIdx.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireSameSymbols(t, snapshotSymbols(incIdx.Snapshot()), snapshotSymbols(fullIdx.Snapshot()))

	// A second, no-op update must report no affected files and keep results.
	report2, err := incIdx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report2.Affected) != 0 {
		t.Fatalf("no-op update affected = %v", report2.Affected)
	}
	requireSameSymbols(t, snapshotSymbols(incIdx.Snapshot()), snapshotSymbols(fullIdx.Snapshot()))
}

func TestTouchDoesNotModify(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "a.go", "package a\n\nconst A = 1\n")
	idx, err := New(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Touch with same content but new mtime.
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\nconst A = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Modified) != 0 {
		t.Fatalf("touch-only file marked modified: %v", report.Modified)
	}
}

func TestConcurrentReadWrite(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "a.go", "package a\n\nconst A0 = 0\n")

	idx, err := New(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var readersWG, writersWG sync.WaitGroup

	// Readers: continuously query and validate snapshot invariants.
	for r := 0; r < 4; r++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				snap := idx.Snapshot()
				if err := validateSnapshot(snap); err != nil {
					t.Error(err)
					return
				}
				_ = snap.Search(Query{Prefix: "A", Limit: 5})
				_ = snap.Outline("a.go")
			}
		}()
	}

	// Writer: keep modifying the workspace.
	writersWG.Add(1)
	go func() {
		defer writersWG.Done()
		for i := 1; i <= 20; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			writeFile(t, root, "a.go", fmt.Sprintf("package a\n\nconst A%d = %d\n", i, i))
			if _, err := idx.Update(context.Background()); err != nil {
				t.Error(err)
				return
			}
		}
	}()

	// Wait for the writer to finish, then stop readers.
	writersWG.Wait()
	cancel()
	readersWG.Wait()

	// Final snapshot must be internally consistent.
	if err := validateSnapshot(idx.Snapshot()); err != nil {
		t.Fatal(err)
	}
}

// validateSnapshot checks that a snapshot is a complete, consistent version.
func validateSnapshot(snap *Snapshot) error {
	if snap == nil {
		return fmt.Errorf("nil snapshot")
	}
	total := 0
	for p, fr := range snap.Files {
		if fr.Path != p {
			return fmt.Errorf("file %q stored under %q", fr.Path, p)
		}
		for _, s := range fr.Symbols {
			if s.File != "" {
				return fmt.Errorf("FileResult symbol has File set: %+v", s)
			}
			total++
		}
	}
	if total != len(snap.symbols) {
		return fmt.Errorf("symbol table %d != sum of file symbols %d", len(snap.symbols), total)
	}
	for _, s := range snap.symbols {
		if _, ok := snap.Files[s.File]; !ok {
			return fmt.Errorf("symbol references missing file %q", s.File)
		}
	}
	// Sorted order check.
	for i := 1; i < len(snap.symbols); i++ {
		a, b := snap.symbols[i-1], snap.symbols[i]
		if symbolLess(b, a) {
			return fmt.Errorf("symbols not sorted at %d: %+v after %+v", i, b, a)
		}
	}
	return nil
}

func symbolLess(a, b Symbol) bool {
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
	return string(a.Kind) < string(b.Kind)
}

func TestRestartPersistence(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "a.go", "package a\n\nconst A = 1\n")
	writeFile(t, root, "sub/b.go", "package sub\n\ntype B struct{}\n")

	idx, err := New(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := snapshotSymbols(idx.Snapshot())

	indexPath := filepath.Join(root, ".codeidx", "index.json")
	if err := idx.Save(indexPath); err != nil {
		t.Fatal(err)
	}

	// No temp files left behind.
	entries, _ := os.ReadDir(filepath.Dir(indexPath))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}

	// Reopen.
	idx2, err := Open(testConfig(root), indexPath)
	if err != nil {
		t.Fatal(err)
	}
	got := snapshotSymbols(idx2.Snapshot())
	requireSameSymbols(t, got, want)
	if idx2.Snapshot().Version != idx.Snapshot().Version {
		t.Fatalf("version mismatch after reload: %d vs %d", idx2.Snapshot().Version, idx.Snapshot().Version)
	}

	// Corrupt file: Open returns ErrCorrupt.
	if err := os.WriteFile(indexPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(testConfig(root), indexPath); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt index error = %v, want ErrCorrupt", err)
	}

	// Missing file: Open yields an empty index.
	missing := filepath.Join(root, "nope.json")
	idx3, err := Open(testConfig(root), missing)
	if err != nil {
		t.Fatal(err)
	}
	if idx3.Snapshot().FileCount() != 0 {
		t.Fatal("missing index should open empty")
	}
}

func TestParseFailures(t *testing.T) {
	root := newTestWorkspace(t)
	// Syntax error: degraded, partial symbols.
	writeFile(t, root, "syntax.go", `package syntax

const OK = 1

type Good struct {
	X int
}

func Broken( {
	this is not go
}
`)
	// A separate good file to prove the index stays usable.
	writeFile(t, root, "good.go", `package syntax

const OK2 = 2
`)
	// Bad encoding: skipped.
	badEnc := []byte("package bad\n\nconst Bad = 1\n\xff\xfe\n")
	if err := os.WriteFile(filepath.Join(root, "enc.go"), badEnc, 0o644); err != nil {
		t.Fatal(err)
	}
	// Oversized: skipped.
	large := strings.Repeat("x", 200)
	writeFile(t, root, "big.go", "package big\n\n// "+large+"\n")

	cfg := testConfig(root)
	cfg.MaxFileSize = 100
	idx, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	report, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Degraded != 1 {
		t.Fatalf("Degraded = %d, want 1", report.Degraded)
	}
	if report.Skipped != 2 {
		t.Fatalf("Skipped = %d, want 2", report.Skipped)
	}

	snap := idx.Snapshot()
	syn, ok := snap.File("syntax.go")
	if !ok {
		t.Fatal("syntax.go missing from snapshot")
	}
	if syn.Status != StatusDegraded {
		t.Fatalf("syntax.go status = %s, want degraded", syn.Status)
	}
	if !strings.Contains(syn.Reason, "parse error") {
		t.Fatalf("reason = %q, want parse error", syn.Reason)
	}
	// Partial symbols extracted despite the error.
	if len(syn.Symbols) == 0 {
		t.Fatal("expected partial symbols from degraded file")
	}
	found := false
	for _, s := range syn.Symbols {
		if s.Name == "OK" {
			found = true
		}
	}
	if !found {
		t.Fatal("partial symbol OK not extracted from degraded file")
	}

	enc, _ := snap.File("enc.go")
	if enc.Status != StatusSkipped || !strings.Contains(enc.Reason, "UTF-8") {
		t.Fatalf("enc.go = %s/%s", enc.Status, enc.Reason)
	}
	big, _ := snap.File("big.go")
	if big.Status != StatusSkipped || !strings.Contains(big.Reason, "too large") {
		t.Fatalf("big.go = %s/%s", big.Status, big.Reason)
	}

	// The good file's symbols are still present and the index is usable.
	if got := snap.Search(Query{Name: "OK2"}); len(got) != 1 {
		t.Fatalf("OK2 search: %v", got)
	}
}

func TestIgnoreRules(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "keep.go", "package root\n\nconst Keep = 1\n")
	writeFile(t, root, "vendor/v.go", "package vendor\n\nconst V = 1\n")
	writeFile(t, root, "sub/keep.go", "package sub\n\nconst Keep2 = 2\n")
	writeFile(t, root, "sub/gen.gen.go", "package sub\n\nconst Gen = 3\n")

	cfg := testConfig(root)
	cfg.IgnorePatterns = []string{"vendor", "*.gen.go"}
	idx, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := idx.Snapshot()
	if _, ok := snap.File("vendor/v.go"); ok {
		t.Fatal("vendor file should be ignored")
	}
	if _, ok := snap.File("sub/gen.gen.go"); ok {
		t.Fatal("*.gen.go should be ignored")
	}
	if _, ok := snap.File("keep.go"); !ok {
		t.Fatal("keep.go should be indexed")
	}
	if _, ok := snap.File("sub/keep.go"); !ok {
		t.Fatal("sub/keep.go should be indexed")
	}
}

func TestPaginationAndSorting(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "a.go", `package a

const A1 = 1
const A2 = 2
const A3 = 3
`)
	writeFile(t, root, "b.go", `package b

const B1 = 1
const B2 = 2
`)
	idx, err := New(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := idx.Snapshot()

	// Full result is stable across calls.
	full := snap.Search(Query{Kind: KindConstant})
	full2 := snap.Search(Query{Kind: KindConstant})
	requireSameSymbols(t, full, full2)

	// Sorted by file then line.
	wantOrder := []string{"A1", "A2", "A3", "B1", "B2"}
	gotOrder := make([]string, len(full))
	for i, s := range full {
		gotOrder[i] = s.Name
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("order = %v, want %v", gotOrder, wantOrder)
	}

	// Pagination: offset+limit pages through the full set.
	var paged []Symbol
	for off := 0; ; off += 2 {
		page := snap.Search(Query{Kind: KindConstant, Offset: off, Limit: 2})
		if len(page) == 0 {
			break
		}
		paged = append(paged, page...)
	}
	requireSameSymbols(t, paged, full)

	// Offset beyond the end returns empty.
	if got := snap.Search(Query{Kind: KindConstant, Offset: 100}); len(got) != 0 {
		t.Fatal("offset beyond end should be empty")
	}
	// Limit 0 means no limit.
	if got := snap.Search(Query{Kind: KindConstant, Limit: 0}); len(got) != len(full) {
		t.Fatal("limit 0 should return all")
	}
}

func TestConfigValidation(t *testing.T) {
	cfg := Config{Root: ""}
	if err := cfg.Validate(); err == nil {
		t.Fatal("empty root should be invalid")
	}
	cfg = DefaultConfig(".")
	cfg.Concurrency = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("concurrency 0 should be invalid")
	}
	cfg = DefaultConfig(".")
	cfg.MaxFileSize = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative max size should be invalid")
	}
	// MaxFileSize 0 disables the limit and is valid.
	cfg = DefaultConfig(".")
	cfg.MaxFileSize = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("MaxFileSize 0 should be valid: %v", err)
	}
}

func TestRebuildIsFull(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "a.go", "package a\n\nconst A = 1\n")
	idx, err := New(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Add a file, then Rebuild (full) should pick it up even though the
	// incremental state is stale.
	writeFile(t, root, "b.go", "package a\n\nconst B = 2\n")
	if _, err := idx.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if idx.Snapshot().FileCount() != 2 {
		t.Fatalf("rebuild file count = %d, want 2", idx.Snapshot().FileCount())
	}
}

func TestIgnoreMatcher(t *testing.T) {
	cases := []struct {
		rel     string
		pattern string
		want    bool
	}{
		{"vendor/x.go", "vendor", true},
		{"a/vendor/x.go", "vendor", true},
		{"a/vend/x.go", "vendor", false},
		{"x.gen.go", "*.gen.go", true},
		{"sub/x.gen.go", "*.gen.go", true},
		{"sub/x.go", "*.gen.go", false},
		{".git/HEAD", ".git", true},
		{"a/.git/config", ".git", true},
		{"foo/bar.go", "foo/bar.go", true},
		{"x/foo/bar.go", "foo/bar.go", false},
	}
	for _, c := range cases {
		if got := matchIgnore(c.rel, []string{c.pattern}); got != c.want {
			t.Fatalf("matchIgnore(%q, %q) = %v, want %v", c.rel, c.pattern, got, c.want)
		}
	}
}

// Ensure sort package is used (for future ordering helpers).

func TestSlowReaderDoesNotBlockUpdates(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "a.go", "package a\n\nconst A0 = 0\n")
	idx, err := New(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A "slow" reader holds an old snapshot reference while updates progress.
	old := idx.Snapshot()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= 10; i++ {
			writeFile(t, root, "a.go", fmt.Sprintf("package a\n\nconst A%d = %d\n", i, i))
			if _, err := idx.Update(context.Background()); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	<-done

	// The old snapshot is still fully queryable and internally consistent.
	if err := validateSnapshot(old); err != nil {
		t.Fatal(err)
	}
	if old.Version != 1 {
		t.Fatalf("held snapshot version = %d, want 1", old.Version)
	}
	// The live index has progressed.
	if idx.Snapshot().Version != 11 {
		t.Fatalf("live version = %d, want 11", idx.Snapshot().Version)
	}
	// Old snapshot must not contain new symbols; new one must.
	if got := old.Search(Query{Name: "A10"}); len(got) != 0 {
		t.Fatal("old snapshot should not contain A10")
	}
	if got := idx.Snapshot().Search(Query{Name: "A10"}); len(got) != 1 {
		t.Fatal("new snapshot should contain A10")
	}
}

func TestScaleManyFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scale test in short mode")
	}
	root := newTestWorkspace(t)
	const n = 500
	for i := 0; i < n; i++ {
		writeFile(t, root, fmt.Sprintf("f%04d.go", i), fmt.Sprintf(`package main

import "fmt"

type T%d struct {
	V int
}

func (t *T%d) M%d() { fmt.Println(t.V) }

const C%d = %d
`, i, i, i, i, i))
	}
	cfg := testConfig(root)
	cfg.Concurrency = 8
	idx, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	report, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalFiles != n {
		t.Fatalf("files = %d, want %d", report.TotalFiles, n)
	}
	if report.TotalSymbols != idx.Snapshot().SymbolCount() {
		t.Fatal("symbol count mismatch")
	}
	if err := validateSnapshot(idx.Snapshot()); err != nil {
		t.Fatal(err)
	}
	// Incremental no-op update must not reprocess anything.
	report2, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report2.Affected) != 0 {
		t.Fatalf("no-op update affected %d files", len(report2.Affected))
	}
	// Pagination over the whole symbol set is stable.
	snap := idx.Snapshot()
	var all []Symbol
	for off := 0; ; off += 100 {
		page := snap.Search(Query{Offset: off, Limit: 100})
		if len(page) == 0 {
			break
		}
		all = append(all, page...)
	}
	requireSameSymbols(t, all, snapshotSymbols(snap))
}
