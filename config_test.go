package symbolindex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigNormalizationAndBoundaries(t *testing.T) {
	cfg, err := Config{Root: "."}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workers <= 0 || cfg.MaxFileSize != defaultMaxFileSize || cfg.DefaultPageSize != defaultPageSize || cfg.MaxQueryLimit != defaultMaxQueryLimit {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	invalid, err := Config{Root: ".", IgnorePatterns: []string{"["}, DefaultPageSize: 10, MaxQueryLimit: 5}.Normalize()
	if err == nil {
		t.Fatalf("expected invalid config, got %+v", invalid)
	}
}

func TestMaxFileSizeBoundary(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "exact.go", "package p\nvar X = `1234567890`\n")
	sizeOrig := func() int64 {
		info, err := os.Stat(filepath.Join(root, "exact.go"))
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}()
	cfg := testConfig(root)
	cfg.MaxFileSize = sizeOrig
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := updateNow(t, e)
	if r.IndexedFiles != 1 {
		t.Fatalf("file equal to limit should be allowed: %+v", r)
	}

	cfg.MaxFileSize = sizeOrig - 1
	e2, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r2 := updateNow(t, e2)
	if r2.SkippedFiles != 1 {
		t.Fatalf("one-byte over limit should skip: %+v", r2)
	}
}

func TestNegativeMaxFileSizeDisablesLimit(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "large.go", "package p\nvar Big = `"+strings.Repeat("x", 2048)+"`\n")
	cfg := testConfig(root)
	cfg.MaxFileSize = -1
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := updateNow(t, e)
	if r.IndexedFiles != 1 || r.SkippedFiles != 0 {
		t.Fatalf("negative limit should disable size skip: %+v", r)
	}
}

func TestSearchLimitIsCapped(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package p\nfunc A() {}\nfunc B() {}\n")
	cfg := testConfig(root)
	cfg.DefaultPageSize = 1
	cfg.MaxQueryLimit = 1
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	updateNow(t, e)
	r, err := e.Search(Query{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if r.Limit != 1 || len(r.Symbols) != 1 || r.Total != 2 || !r.HasMore {
		t.Fatalf("limit cap not applied: %+v", r)
	}
}

func TestIgnoreDoubleStarPatterns(t *testing.T) {
	m, err := newIgnoreMatcher([]string{"a/**/keep.go", "**/skip.go"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path string
		dir  bool
		want bool
	}{
		{"a/keep.go", false, true},
		{"a/b/keep.go", false, true},
		{"a/b/c/keep.go", false, true},
		{"x/a/keep.go", false, false},
		{"skip.go", false, true},
		{"deep/skip.go", false, true},
		{"a/other.go", false, false},
	}
	for _, tc := range cases {
		if got := m.ignores(tc.path, tc.dir); got != tc.want {
			t.Errorf("ignores(%q)=%v want %v", tc.path, got, tc.want)
		}
	}
}

func TestPrefixUpperBound(t *testing.T) {
	cases := map[string]string{
		"a":     "b",
		"ab":    "ac",
		"a\xff": "b",
	}
	for in, want := range cases {
		if got := prefixUpperBound(in); got != want {
			t.Errorf("prefixUpperBound(%q)=%q want %q", in, got, want)
		}
	}
	if got := prefixUpperBound("\xff\xff"); got != "" {
		t.Errorf("max prefix upper bound=%q want empty", got)
	}
}
