package symbolindex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFailureInvalidUTF8AndOversizedFile(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "ok.go", "package p\nfunc OK() {}\n")
	writeWorkspaceFile(t, root, "bad.go", "package p\nfunc Broken( {\n")
	writeWorkspaceFile(t, root, "invalid.go", "package p\n//\xff\n")
	big := strings.Repeat("a", 64)
	writeWorkspaceFile(t, root, "big.go", "package p\nvar Big = `"+big+"`\n")

	cfg := testConfig(root)
	cfg.MaxFileSize = 32
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	report, err := e.Update(context.Background())
	if err != nil {
		t.Fatalf("update failed despite tolerant errors: %v", err)
	}
	if report.FailureFiles != 2 || report.SkippedFiles != 1 || report.IndexedFiles != 1 {
		t.Fatalf("unexpected counts: %+v", report)
	}
	reasons := map[string]FileStatus{}
	codes := map[string]string{}
	for _, rec := range e.Snapshot().Files() {
		reasons[rec.Path] = rec.Status
		codes[rec.Path] = rec.ReasonCode
	}
	if reasons["ok.go"] != StatusOK {
		t.Fatalf("ok file status=%v", reasons["ok.go"])
	}
	if reasons["bad.go"] != StatusParseError || codes["bad.go"] != "syntax_error" {
		t.Fatalf("bad status=%v code=%s reason record=%+v", reasons["bad.go"], codes["bad.go"], e.Snapshot().files["bad.go"])
	}
	if reasons["invalid.go"] != StatusSkippedInvalidUTF8 || codes["invalid.go"] != "invalid_utf8" {
		t.Fatalf("invalid status=%v code=%s", reasons["invalid.go"], codes["invalid.go"])
	}
	if reasons["big.go"] != StatusSkippedTooLarge || codes["big.go"] != "file_too_large" {
		t.Fatalf("big status=%v code=%s", reasons["big.go"], codes["big.go"])
	}
	if e.Snapshot().SymbolCount() != 1 {
		t.Fatalf("symbols=%d, only ok.go should index", e.Snapshot().SymbolCount())
	}
	if len(report.Errors) != 3 {
		t.Fatalf("error links=%d, want 3", len(report.Errors))
	}
	paths := map[string]bool{}
	for _, link := range report.Errors {
		if link.Path == "" || link.Op == "" || link.Err == nil {
			t.Fatalf("incomplete error link: %+v", link)
		}
		paths[link.Path] = true
	}
	for _, want := range []string{"bad.go", "invalid.go", "big.go"} {
		if !paths[want] {
			t.Fatalf("missing error link for %s", want)
		}
	}
}

func TestIgnoreRulesAndRegularFileBoundary(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "keep.go", "package p\nfunc Keep() {}\n")
	writeWorkspaceFile(t, root, "skip.go", "package p\nfunc Skip() {}\n")
	writeWorkspaceFile(t, root, "vendor/v.go", "package vendor\nfunc Vendor() {}\n")
	writeWorkspaceFile(t, root, "nested/generated/a.go", "package a\nfunc A() {}\n")
	writeWorkspaceFile(t, root, "nested/keep/b.go", "package b\nfunc B() {}\n")
	if err := os.Symlink(filepath.Join(root, "keep.go"), filepath.Join(root, "link.go")); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(root)
	cfg.IgnorePatterns = []string{"skip.go", "vendor/", "generated"}
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	updateNow(t, e)
	got, err := e.Search(Query{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, sym := range got.Symbols {
		names = append(names, sym.Path)
	}
	if len(names) != 2 || names[0] != "keep.go" || names[1] != "nested/keep/b.go" {
		t.Fatalf("unexpected indexed paths: %v", names)
	}
	if _, ok := e.Outline("link.go"); ok {
		t.Fatal("symlink should not be indexed")
	}
}

func TestIgnoreConfigChangeDeletesOldFilesIncrementally(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "keep.go", "package keep\nfunc Keep() {}\n")
	writeWorkspaceFile(t, root, "drop.go", "package drop\nfunc Drop() {}\n")
	base := testConfig(root)
	oldEngine, err := NewEngine(base)
	if err != nil {
		t.Fatal(err)
	}
	updateNow(t, oldEngine)

	nextCfg := base
	nextCfg.IgnorePatterns = []string{"drop.go"}
	next, report, err := buildSnapshot(context.Background(), nextCfg, oldEngine.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Affected.Deleted) != 1 || report.Affected.Deleted[0] != "drop.go" {
		t.Fatalf("ignored file was not deleted incrementally: %+v", report.Affected)
	}
	full, err := NewEngine(nextCfg)
	if err != nil {
		t.Fatal(err)
	}
	updateNow(t, full)
	assertSnapshotsEqual(t, full.Snapshot(), next)
}
