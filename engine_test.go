package symbolindex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeWorkspaceFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testConfig(root string) Config {
	cfg := DefaultConfig(root)
	cfg.Workers = 1
	cfg.MaxFileSize = 1 << 20
	return cfg
}

func updateNow(t *testing.T, e *Engine) Report {
	t.Helper()
	report, err := e.Update(context.Background())
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	return report
}

func waitForMTimeSkew() { time.Sleep(20 * time.Millisecond) }

func TestExtractSymbolsDeterministic(t *testing.T) {
	content := []byte(`package p
var Z, A = 1, 2
const C = 3
type T struct{}
func B() {}
func (T) A() {}
func main() {}
`)
	first, status, code, reason := ExtractSymbols("x.go", content)
	if status != StatusOK || code != "" || reason != "" {
		t.Fatalf("status=%q code=%q reason=%q", status, code, reason)
	}
	for i := 0; i < 10; i++ {
		got, _, _, _ := ExtractSymbols("x.go", content)
		if len(got) != len(first) {
			t.Fatalf("symbol count changed: %d vs %d", len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("run %d symbol %d changed: %+v vs %+v", i, j, got[j], first[j])
			}
		}
	}
	wantNames := []string{"Z", "A", "C", "T", "B", "A", "main"}
	if len(first) != len(wantNames) {
		t.Fatalf("got %d symbols, want %d", len(first), len(wantNames))
	}
	for i, want := range wantNames {
		if first[i].Name != want {
			t.Fatalf("symbol %d = %s, want %s", i, first[i].Name, want)
		}
	}
}

func TestIncrementalMatchesFullRebuildAndReportsAffected(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package p\nfunc A() {}\n")
	writeWorkspaceFile(t, root, "z/b.go", "package z\nfunc B() {}\n")

	e, err := NewEngine(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	r1 := updateNow(t, e)
	if got := e.Snapshot(); got.Version != 1 || r1.IndexedFiles != 2 || got.SymbolCount() != 2 {
		t.Fatalf("initial version=%d report=%+v symbols=%d", got.Version, r1, got.SymbolCount())
	}
	if len(r1.Affected.Added) != 2 || r1.Affected.Added[0] != "a.go" || r1.Affected.Added[1] != "z/b.go" {
		t.Fatalf("unexpected added files: %+v", r1.Affected.Added)
	}

	full, err := NewEngine(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	updateNow(t, full)
	// Snapshot timestamps and versions intentionally differ; compare semantic
	// file records and global symbol order.
	assertSnapshotsEqual(t, full.Snapshot(), e.Snapshot())

	waitForMTimeSkew()
	writeWorkspaceFile(t, root, "z/b.go", "package z\nfunc B() {}\nfunc B2() {}\n")
	writeWorkspaceFile(t, root, "c.go", "package p\nfunc C() {}\n")
	if err := os.Remove(filepath.Join(root, "a.go")); err != nil {
		t.Fatal(err)
	}
	r2 := updateNow(t, e)
	if r2.Affected.Added[0] != "c.go" ||
		len(r2.Affected.Deleted) != 1 || r2.Affected.Deleted[0] != "a.go" ||
		len(r2.Affected.Modified) != 1 || r2.Affected.Modified[0] != "z/b.go" {
		t.Fatalf("unexpected affected files: %+v", r2.Affected)
	}
	if e.Snapshot().Version != 2 {
		t.Fatalf("version = %d, want 2", e.Snapshot().Version)
	}

	full2, err := NewEngine(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	updateNow(t, full2)
	assertSnapshotsEqual(t, full2.Snapshot(), e.Snapshot())
}

func TestSearchFiltersSortingAndPagination(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package p\nfunc Apple() {}\nconst Approve=1\nfunc Banana() {}\n")
	writeWorkspaceFile(t, root, "z/b.go", "package z\nfunc Application() {}\nvar Beta int\n")
	e, err := NewEngine(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	updateNow(t, e)

	page, err := e.Search(Query{Prefix: "App", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 3 || len(page.Symbols) != 2 || !page.HasMore || page.Limit != 2 {
		t.Fatalf("unexpected first page: %+v", page)
	}
	if page.Symbols[0].Name != "Apple" || page.Symbols[1].Name != "Approve" {
		t.Fatalf("unexpected page order: %+v", page.Symbols)
	}
	page2, err := e.Search(Query{Prefix: "App", Offset: 2, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if page2.Total != 3 || len(page2.Symbols) != 1 || page2.HasMore || page2.Symbols[0].Name != "Application" {
		t.Fatalf("unexpected second page: %+v", page2)
	}

	kindOnly, err := e.Search(Query{Kind: KindFunction})
	if err != nil {
		t.Fatal(err)
	}
	if kindOnly.Total != 3 {
		t.Fatalf("functions=%d, want 3", kindOnly.Total)
	}
	fileOnly, err := e.Search(Query{Path: "z/b.go"})
	if err != nil {
		t.Fatal(err)
	}
	if fileOnly.Total != 2 || fileOnly.Symbols[0].Path != "z/b.go" {
		t.Fatalf("unexpected file query: %+v", fileOnly)
	}
	exact, err := e.Search(Query{Name: "Apple"})
	if err != nil {
		t.Fatal(err)
	}
	if exact.Total != 1 || exact.Symbols[0].Kind != KindFunction {
		t.Fatalf("unexpected exact query: %+v", exact)
	}

	outline, ok := e.Outline("z/b.go")
	if !ok || len(outline.Symbols) != 2 {
		t.Fatalf("outline ok=%v result=%+v", ok, outline)
	}
	if outline.Symbols[0].Name != "Application" || outline.Symbols[1].Name != "Beta" {
		t.Fatalf("outline not source ordered: %+v", outline.Symbols)
	}
}

func assertSnapshotsEqual(t *testing.T, want, got *Snapshot) {
	t.Helper()
	wantFiles := want.Files()
	gotFiles := got.Files()
	if len(wantFiles) != len(gotFiles) {
		t.Fatalf("file count %d != %d", len(gotFiles), len(wantFiles))
	}
	for i := range wantFiles {
		wantFiles[i].ModTime = wantFiles[i].ModTime.UTC()
		gotFiles[i].ModTime = gotFiles[i].ModTime.UTC()
		// Symbol slices are normalized through JSON comparison below.
		if wantFiles[i].Path != gotFiles[i].Path ||
			wantFiles[i].Size != gotFiles[i].Size ||
			!wantFiles[i].ModTime.Equal(gotFiles[i].ModTime) ||
			wantFiles[i].Hash != gotFiles[i].Hash ||
			wantFiles[i].Status != gotFiles[i].Status ||
			wantFiles[i].ReasonCode != gotFiles[i].ReasonCode ||
			len(wantFiles[i].Symbols) != len(gotFiles[i].Symbols) {
			t.Fatalf("record mismatch\nwant=%+v\ngot =%+v", wantFiles[i], gotFiles[i])
		}
		for j := range wantFiles[i].Symbols {
			if wantFiles[i].Symbols[j] != gotFiles[i].Symbols[j] {
				t.Fatalf("symbol mismatch at %s[%d]: %+v != %+v", wantFiles[i].Path, j, gotFiles[i].Symbols[j], wantFiles[i].Symbols[j])
			}
		}
	}
	wantSearch, err := want.Search(Query{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	gotSearch, err := got.Search(Query{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(wantSearch.Symbols) != len(gotSearch.Symbols) {
		t.Fatalf("global symbol count %d != %d", len(gotSearch.Symbols), len(wantSearch.Symbols))
	}
	for i := range wantSearch.Symbols {
		if wantSearch.Symbols[i] != gotSearch.Symbols[i] {
			t.Fatalf("global symbol %d mismatch: %+v != %+v", i, gotSearch.Symbols[i], wantSearch.Symbols[i])
		}
	}
}

func TestMetadataOnlyChangeDoesNotAlterIndex(t *testing.T) {
	root := t.TempDir()
	rel := "same.go"
	path := filepath.Join(root, rel)
	writeWorkspaceFile(t, root, rel, "package p\nfunc Same() {}\n")
	e, err := NewEngine(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	updateNow(t, e)
	oldRecord, ok := e.Snapshot().File(rel)
	if !ok {
		t.Fatal("initial file missing")
	}
	newTime := oldRecord.ModTime.Add(time.Hour)
	if err := os.Chtimes(path, newTime, newTime); err != nil {
		t.Fatal(err)
	}
	r := updateNow(t, e)
	if len(r.Affected.MetadataChanged) != 1 || r.Affected.MetadataChanged[0] != rel || len(r.Affected.Modified) != 0 {
		t.Fatalf("unexpected affected metadata result: %+v", r.Affected)
	}
	got, ok := e.Snapshot().File(rel)
	if !ok {
		t.Fatal("file missing after metadata-only update")
	}
	if got.Hash != oldRecord.Hash || got.Status != StatusOK || len(got.Symbols) != 1 {
		t.Fatalf("index content changed: old=%+v got=%+v", oldRecord, got)
	}
	if !got.ModTime.Equal(newTime) {
		t.Fatalf("modtime not updated: got=%s want=%s", got.ModTime, newTime)
	}
}

func TestMissingRootIsFatal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	e, err := NewEngine(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Update(context.Background()); err == nil {
		t.Fatal("missing workspace root must fail the update")
	}
}
