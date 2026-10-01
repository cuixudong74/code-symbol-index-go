package symbolindex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRestartReuse(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package p\nfunc Keep() {}\n")
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEngineWithStore(testConfig(root), store)
	if err != nil {
		t.Fatal(err)
	}
	r := updateNow(t, e)
	if err := e.Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Version != 1 {
		t.Fatalf("version=%d", r.Version)
	}

	reopenedStore, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := NewEngineWithStore(testConfig(root), reopenedStore)
	if err != nil {
		t.Fatal(err)
	}
	snap := e2.Snapshot()
	if snap.Version != 1 || snap.SymbolCount() != 1 {
		t.Fatalf("loaded version=%d symbols=%d", snap.Version, snap.SymbolCount())
	}
	got, err := e2.Search(Query{Name: "Keep"})
	if err != nil || got.Total != 1 {
		t.Fatalf("loaded search err=%v result=%+v", err, got)
	}
}

func TestSaveCleansStaleTemporaryFiles(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package p\nfunc V2() {}\n")
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEngineWithStore(testConfig(root), store)
	if err != nil {
		t.Fatal(err)
	}
	updateNow(t, e)
	stale := []string{
		".index-00000000000000000002.json.tmp-abc",
		".manifest.json.tmp-def",
		"index-00000000000000000099.json",
	}
	for _, name := range stale {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range stale {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("stale file %s still exists, stat err=%v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".index.lock")); err != nil {
		t.Fatalf("lock file should remain usable: %v", err)
	}
}

func TestInterruptedWritesDoNotCorruptPreviousVersion(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package p\nfunc V1() {}\n")
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEngineWithStore(testConfig(root), store)
	if err != nil {
		t.Fatal(err)
	}
	updateNow(t, e)
	if err := e.Save(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Simulate crashes that leave an incomplete data file and an fsynced temp
	// file. The manifest still atomically names the prior complete version.
	if err := os.WriteFile(filepath.Join(dir, ".index-00000000000000000002.json.tmp-bad"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index-00000000000000000002.json.tmp"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.Load(context.Background())
	if err != nil {
		t.Fatalf("stale temp files should be ignored: %v", err)
	}
	if loaded.Version != 1 || loaded.SymbolCount() != 1 {
		t.Fatalf("loaded version=%d symbols=%d", loaded.Version, loaded.SymbolCount())
	}

	// A fully renamed corrupt data file is pointed to by a valid manifest and
	// is reported instead of silently returning a partial index.
	corrupt := filepath.Join(dir, "index-00000000000000000003.json")
	if err := os.WriteFile(corrupt, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `{
  "format": "code-symbol-index/v1",
  "dataFile": "index-00000000000000000003.json",
  "version": 3,
  "updatedAt": "2026-01-01T00:00:00Z"
}
`
	if err := os.WriteFile(filepath.Join(dir, storeManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	badStore, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := badStore.Load(context.Background()); err == nil {
		t.Fatal("corrupt published index must not load as a partial snapshot")
	}
}
