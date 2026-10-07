package index

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// fileEntry is the query-visible state of one file in a snapshot.
type fileEntry struct {
	status     FileStatus
	reasonCode ReasonCode
	symbols    []Symbol
}

// snapshotEntries extracts the full per-file state of snap, keyed by path.
func snapshotEntries(snap *Snapshot) map[string]fileEntry {
	out := make(map[string]fileEntry, snap.FileCount())
	for p, fr := range snap.Files {
		syms := make([]Symbol, len(fr.Symbols))
		copy(syms, fr.Symbols)
		out[p] = fileEntry{status: fr.Status, reasonCode: fr.ReasonCode, symbols: syms}
	}
	return out
}

// requireSnapshotsEqual fails unless the two snapshots are identical in every
// query-visible detail (file set, per-file status/reason code and every symbol).
func requireSnapshotsEqual(t *testing.T, got, want *Snapshot) {
	t.Helper()
	g, w := snapshotEntries(got), snapshotEntries(want)
	gk := make([]string, 0, len(g))
	for k := range g {
		gk = append(gk, k)
	}
	sort.Strings(gk)
	wk := make([]string, 0, len(w))
	for k := range w {
		wk = append(wk, k)
	}
	sort.Strings(wk)
	if !reflect.DeepEqual(gk, wk) {
		t.Fatalf("file sets differ:\n got: %v\nwant: %v", gk, wk)
	}
	for _, p := range gk {
		ge, we := g[p], w[p]
		if ge.status != we.status || ge.reasonCode != we.reasonCode {
			t.Fatalf("file %q status differs: got %s/%s want %s/%s",
				p, ge.status, ge.reasonCode, we.status, we.reasonCode)
		}
		requireSameSymbols(t, ge.symbols, we.symbols)
	}
	requireSameSymbols(t, snapshotSymbols(got), snapshotSymbols(want))
}

// fullBuildWith builds a fresh index with cfg and runs one full rebuild.
func fullBuildWith(t *testing.T, cfg Config) *Index {
	t.Helper()
	idx, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	return idx
}

// TestMaxFileSizeRaiseMakesSkippedFileIndexed covers raising the size limit:
// files previously skipped as too large must become indexed, appear in the
// affected list, and the incremental result must equal a full rebuild.
func TestMaxFileSizeRaiseMakesSkippedFileIndexed(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "small.go", "package a\n\nconst Small = 1\n")
	big := "package a\n\n// " + repeat("x", 180) + "\nconst Big = 2\n" // ~200 bytes
	writeFile(t, root, "big.go", big)

	cfg := testConfig(root)
	cfg.MaxFileSize = 100
	idx, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r1, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r1.Skipped != 1 {
		t.Fatalf("first update skipped = %d, want 1", r1.Skipped)
	}
	if fr, ok := idx.Snapshot().File("big.go"); !ok || fr.ReasonCode != ReasonTooLarge {
		t.Fatalf("big.go should be skipped as too large, got %+v", fr)
	}

	// Raise the limit. Nothing on disk changed, yet the previously skipped
	// file must be reported as affected and become indexed.
	newCfg := cfg
	newCfg.MaxFileSize = 0 // unlimited
	if err := idx.SetConfig(newCfg); err != nil {
		t.Fatal(err)
	}
	r2, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !r2.ConfigChanged {
		t.Fatal("ConfigChanged should be true after raising the limit")
	}
	if !reflect.DeepEqual(r2.Affected, []string{"big.go"}) {
		t.Fatalf("Affected = %v, want [big.go]", r2.Affected)
	}
	if !reflect.DeepEqual(r2.Modified, []string{"big.go"}) {
		t.Fatalf("Modified = %v, want [big.go]", r2.Modified)
	}
	fr, _ := idx.Snapshot().File("big.go")
	if fr.Status != StatusOK || len(fr.Symbols) == 0 {
		t.Fatalf("big.go should now be indexed, got %s/%s", fr.Status, fr.ReasonCode)
	}

	// The incrementally updated snapshot must equal a fresh full rebuild under
	// the new configuration, file by file and symbol by symbol.
	full := fullBuildWith(t, newCfg)
	requireSnapshotsEqual(t, idx.Snapshot(), full.Snapshot())

	// A follow-up update is a no-op: same configuration, unchanged files.
	r3, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r3.ConfigChanged || len(r3.Affected) != 0 {
		t.Fatalf("follow-up update should be no-op, got changed=%v affected=%v",
			r3.ConfigChanged, r3.Affected)
	}
	requireSnapshotsEqual(t, idx.Snapshot(), full.Snapshot())
}

// TestMaxFileSizeLowerSkipsPreviouslyIndexedFile covers lowering the limit:
// files that were indexed must be re-evaluated and skipped, and they must
// appear in the affected list; symbols must disappear from queries.
func TestMaxFileSizeLowerSkipsPreviouslyIndexedFile(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "small.go", "package a\n\nconst Small = 1\n")
	big := "package a\n\n// " + repeat("x", 180) + "\nconst Big = 2\n"
	writeFile(t, root, "big.go", big)

	cfg := testConfig(root)
	cfg.MaxFileSize = 0 // unlimited
	idx, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := idx.Snapshot().Search(Query{Name: "Big"}); len(got) != 1 {
		t.Fatalf("Big should be indexed before lowering the limit: %v", got)
	}

	newCfg := cfg
	newCfg.MaxFileSize = 100
	if err := idx.SetConfig(newCfg); err != nil {
		t.Fatal(err)
	}
	r, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !r.ConfigChanged {
		t.Fatal("ConfigChanged should be true after lowering the limit")
	}
	if !reflect.DeepEqual(r.Affected, []string{"big.go"}) {
		t.Fatalf("Affected = %v, want [big.go]", r.Affected)
	}
	fr, _ := idx.Snapshot().File("big.go")
	if fr.Status != StatusSkipped || fr.ReasonCode != ReasonTooLarge {
		t.Fatalf("big.go should now be too_large, got %s/%s", fr.Status, fr.ReasonCode)
	}
	if got := idx.Snapshot().Search(Query{Name: "Big"}); len(got) != 0 {
		t.Fatalf("Big must no longer be queryable: %v", got)
	}
	// The unaffected file is untouched.
	if _, ok := idx.Snapshot().File("small.go"); !ok {
		t.Fatal("small.go should still be indexed")
	}

	full := fullBuildWith(t, newCfg)
	requireSnapshotsEqual(t, idx.Snapshot(), full.Snapshot())
}

// TestIgnorePatternChangeIncrementalMatchesFull covers adding/removing ignore
// patterns: newly ignored files leave the snapshot as removed, newly visible
// files enter as added, and the final snapshot matches a full rebuild.
func TestIgnorePatternChangeIncrementalMatchesFull(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "keep.go", "package a\n\nconst Keep = 1\n")
	writeFile(t, root, "vendor/v.go", "package vendor\n\nconst V = 1\n")
	writeFile(t, root, "gen/x.go", "package gen\n\nconst X = 1\n")

	cfg := testConfig(root)
	idx, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if idx.Snapshot().FileCount() != 3 {
		t.Fatalf("initial files = %d, want 3", idx.Snapshot().FileCount())
	}

	// Ignore vendor and gen.
	cfg2 := cfg
	cfg2.IgnorePatterns = []string{".git", ".codeidx", "vendor", "gen"}
	if err := idx.SetConfig(cfg2); err != nil {
		t.Fatal(err)
	}
	r, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !r.ConfigChanged {
		t.Fatal("ConfigChanged should be true after changing ignores")
	}
	if !reflect.DeepEqual(r2Strings(r.Removed), []string{"gen/x.go", "vendor/v.go"}) {
		t.Fatalf("Removed = %v", r.Removed)
	}
	if len(r.Added) != 0 || len(r.Modified) != 0 {
		t.Fatalf("unexpected added=%v modified=%v", r.Added, r.Modified)
	}
	if !reflect.DeepEqual(r.Affected, []string{"gen/x.go", "vendor/v.go"}) {
		t.Fatalf("Affected = %v", r.Affected)
	}
	full := fullBuildWith(t, cfg2)
	requireSnapshotsEqual(t, idx.Snapshot(), full.Snapshot())

	// Remove the ignores again: the files come back as added.
	if err := idx.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	r2, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r2Strings(r2.Added), []string{"gen/x.go", "vendor/v.go"}) {
		t.Fatalf("Added = %v", r2.Added)
	}
	fullOrig := fullBuildWith(t, cfg)
	requireSnapshotsEqual(t, idx.Snapshot(), fullOrig.Snapshot())
}

// TestConfigChangeCombinedWithFileChange ensures that when settings and file
// contents change in the same update, both effects are captured and the result
// still matches a full rebuild.
func TestConfigChangeCombinedWithFileChange(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "small.go", "package a\n\nconst Small = 1\n")
	big := "package a\n\n// " + repeat("x", 180) + "\nconst Big = 2\n"
	writeFile(t, root, "big.go", big)

	cfg := testConfig(root)
	cfg.MaxFileSize = 100
	idx, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Raise the limit and simultaneously edit the small file.
	newCfg := cfg
	newCfg.MaxFileSize = 0
	if err := idx.SetConfig(newCfg); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "small.go", "package a\n\nconst Small = 1\nconst Small2 = 2\n")

	r, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !r.ConfigChanged {
		t.Fatal("ConfigChanged should be true")
	}
	// big.go outcome flips skipped -> ok; small.go content changed.
	if !reflect.DeepEqual(r2Strings(r.Affected), []string{"big.go", "small.go"}) {
		t.Fatalf("Affected = %v", r.Affected)
	}
	if got := idx.Snapshot().Search(Query{Name: "Small2"}); len(got) != 1 {
		t.Fatalf("Small2 should be indexed: %v", got)
	}
	if got := idx.Snapshot().Search(Query{Name: "Big"}); len(got) != 1 {
		t.Fatalf("Big should be indexed: %v", got)
	}
	requireSnapshotsEqual(t, idx.Snapshot(), fullBuildWith(t, newCfg).Snapshot())
}

// TestSignatureStableAndSensitive checks that ordering/whitespace of ignore
// patterns do not change the signature, while content changes do.
func TestSignatureStableAndSensitive(t *testing.T) {
	c1 := DefaultConfig(".")
	c1.MaxFileSize = 10
	c1.IgnorePatterns = []string{"vendor", "gen", ".git"}
	c2 := c1
	c2.IgnorePatterns = []string{"gen", ".git", "vendor"}
	if c1.Signature() != c2.Signature() {
		t.Fatal("signature must be insensitive to pattern order")
	}
	c4 := c1
	c4.MaxFileSize = 11
	if c1.Signature() == c4.Signature() {
		t.Fatal("signature must change when MaxFileSize changes")
	}
	c5 := c1
	c5.IgnorePatterns = []string{"vendor", "gen", ".git", "extra"}
	if c1.Signature() == c5.Signature() {
		t.Fatal("signature must change when an ignore pattern is added")
	}
	// Root and Concurrency are excluded from the signature.
	c6 := c1
	c6.Root = "/somewhere/else"
	c6.Concurrency = c1.Concurrency*2 + 1
	if c1.Signature() != c6.Signature() {
		t.Fatal("Root and Concurrency must not affect the signature")
	}
	// Whitespace-trimmed equivalent.
	c7 := c1
	c7.IgnorePatterns = []string{" vendor ", "gen", ".git"}
	if c1.Signature() != c7.Signature() {
		t.Fatal("signature must be insensitive to surrounding whitespace")
	}
}

// TestConfigChangeAcrossPersistence ensures an on-disk index produced under
// old settings is transparently reconciled after Open + Update.
func TestConfigChangeAcrossPersistence(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "big.go", "package a\n\n// "+repeat("x", 180)+"\nconst Big = 2\n")

	cfg := testConfig(root)
	cfg.MaxFileSize = 100
	idx, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(root, ".codeidx", "index.json")
	if err := idx.Save(indexPath); err != nil {
		t.Fatal(err)
	}

	newCfg := cfg
	newCfg.MaxFileSize = 0
	reopened, err := Open(newCfg, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if fr, _ := reopened.Snapshot().File("big.go"); fr == nil || fr.Status != StatusSkipped {
		t.Fatalf("loaded snapshot should still show skipped state, got %+v", fr)
	}
	r, err := reopened.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !r.ConfigChanged {
		t.Fatal("update after Open with new config should report ConfigChanged")
	}
	if !reflect.DeepEqual(r.Affected, []string{"big.go"}) {
		t.Fatalf("Affected = %v", r.Affected)
	}
	requireSnapshotsEqual(t, reopened.Snapshot(), fullBuildWith(t, newCfg).Snapshot())

	// And the reconciled snapshot persists with the new signature: another
	// process opening and updating must see a no-op.
	if err := reopened.Save(indexPath); err != nil {
		t.Fatal(err)
	}
	reopened2, err := Open(newCfg, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := reopened2.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.Affected) != 0 || r2.ConfigChanged {
		t.Fatalf("second reopen should be no-op, got affected=%v changed=%v",
			r2.Affected, r2.ConfigChanged)
	}
}

// TestOldFormatSnapshotReconciledOnce simulates an index written before
// settings signatures existed (empty SettingsSig): the first update must
// reprocess everything once, and subsequent updates are no-ops.
func TestOldFormatSnapshotReconciledOnce(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "a.go", "package a\n\nconst A = 1\n")
	cfg := testConfig(root)
	idx, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Pretend it came from an old version.
	idx.Snapshot().SettingsSig = ""

	r, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !r.ConfigChanged {
		t.Fatal("snapshot without signature should trigger reconciliation")
	}
	// The file outcome is unchanged by the reconciliation, so it must not be
	// reported as affected even though every file was reprocessed.
	if len(r.Affected) != 0 {
		t.Fatalf("Affected = %v, want none (outcome unchanged)", r.Affected)
	}
	r2, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r2.ConfigChanged || len(r2.Affected) != 0 {
		t.Fatalf("reconciliation must happen only once: %v %v", r2.ConfigChanged, r2.Affected)
	}
}

func repeat(s string, n int) string {
	b := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		b = append(b, s...)
	}
	return string(b)
}

// r2Strings is a defensive nil->empty conversion for DeepEqual comparisons.
func r2Strings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// TestSetConfigRejectsInvalid ensures SetConfig validates its argument and
// leaves the previous configuration untouched on error.
func TestSetConfigRejectsInvalid(t *testing.T) {
	root := newTestWorkspace(t)
	idx, err := New(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	bad := idx.Config()
	bad.MaxFileSize = -1
	if err := idx.SetConfig(bad); err == nil {
		t.Fatal("negative MaxFileSize should be rejected")
	}
	bad = idx.Config()
	bad.Concurrency = 0
	if err := idx.SetConfig(bad); err == nil {
		t.Fatal("zero Concurrency should be rejected")
	}
	good := idx.Config()
	if good.Concurrency != 2 || good.MaxFileSize != DefaultMaxFileSize {
		t.Fatalf("valid config must remain in place after rejected SetConfig: %+v", good)
	}
}

// TestOpenLegacyOnDiskFormatReconciles writes a raw index file in the shape
// produced before settings_sig / reason_code existed, then opens it and runs
// an update: it must load, reconcile once under current settings, and end up
// equal to a full rebuild.
func TestOpenLegacyOnDiskFormatReconciles(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "a.go", "package a\n\nconst A = 1\n")
	indexPath := filepath.Join(root, ".codeidx", "index.json")
	// Hand-written legacy envelope: no settings_sig, file result has no
	// reason_code field.
	legacy := `{
  "format_version": 1,
  "snapshot": {
    "version": 1,
    "files": {
      "a.go": {
        "path": "a.go",
        "status": "ok",
        "fingerprint": "stat:0:0",
        "size": 0,
        "mod_time": 0,
        "symbols": [
          {"file": "", "name": "a", "kind": "package", "line": 1, "column": 1},
          {"file": "", "name": "A", "kind": "constant", "line": 3, "column": 7}
        ]
      }
    }
  }
}`
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	idx, err := Open(testConfig(root), indexPath)
	if err != nil {
		t.Fatalf("legacy file must open: %v", err)
	}
	if idx.Snapshot().SettingsSig != "" {
		t.Fatal("loaded legacy snapshot should have empty signature")
	}
	r, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !r.ConfigChanged {
		t.Fatal("legacy snapshot should trigger one reconciliation")
	}
	if idx.Snapshot().SettingsSig == "" {
		t.Fatal("reconciled snapshot should carry a signature")
	}
	// Content fingerprint differed (legacy used a placeholder), so a.go's
	// outcome metadata was refreshed; symbols themselves are unchanged.
	requireSnapshotsEqual(t, idx.Snapshot(), fullBuildWith(t, testConfig(root)).Snapshot())

	// Persist and reopen: now it must be a clean no-op.
	if err := idx.Save(indexPath); err != nil {
		t.Fatal(err)
	}
	idx2, err := Open(testConfig(root), indexPath)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := idx2.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r2.ConfigChanged || len(r2.Affected) != 0 {
		t.Fatalf("second open/update should be no-op, changed=%v affected=%v",
			r2.ConfigChanged, r2.Affected)
	}
}
