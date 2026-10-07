package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "codeidx")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build cli: %v\n%s", err, out)
	}
	return bin
}

func runCLI(t *testing.T, bin, root string, args ...string) map[string]any {
	t.Helper()
	cmd := exec.Command(bin, append(append([]string{}, args...), "--root", root)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("cli %v: %v\nstderr: %s\nstdout: %s", args, err, stderr.String(), out)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		// Some outputs are arrays.
		var arr []map[string]any
		if err2 := json.Unmarshal(out, &arr); err2 != nil {
			t.Fatalf("parse output: %v\nstderr: %s\n%s", err, stderr.String(), out)
		}
		v = map[string]any{"_array": arr}
	}
	return v
}

func TestCLIEndToEnd(t *testing.T) {
	bin := buildCLI(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\nconst Hello = 1\n\nfunc HelloFunc() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.go"), []byte("package a\n\ntype HelloType struct{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Index.
	rep := runCLI(t, bin, root, "index")
	if int(rep["total_files"].(float64)) != 2 {
		t.Fatalf("total_files = %v", rep["total_files"])
	}

	// Query prefix.
	q := runCLI(t, bin, root, "query", "--prefix", "Hello")
	arr := q["_array"].([]map[string]any)
	if len(arr) != 3 {
		t.Fatalf("prefix Hello got %d results", len(arr))
	}

	// Outline.
	o := runCLI(t, bin, root, "outline", "--file", "a.go")
	arr = o["_array"].([]map[string]any)
	if len(arr) != 3 { // package, const, func
		t.Fatalf("outline a.go got %d results", len(arr))
	}

	// Status.
	st := runCLI(t, bin, root, "status")
	if int(st["files"].(float64)) != 2 {
		t.Fatalf("status files = %v", st["files"])
	}

	// Incremental: remove b.go, re-index.
	if err := os.Remove(filepath.Join(root, "b.go")); err != nil {
		t.Fatal(err)
	}
	rep2 := runCLI(t, bin, root, "index")
	removed := rep2["removed"].([]any)
	if len(removed) != 1 || removed[0].(string) != "b.go" {
		t.Fatalf("removed = %v", rep2["removed"])
	}

	// Index file exists on disk.
	if _, err := os.Stat(filepath.Join(root, ".codeidx", "index.json")); err != nil {
		t.Fatalf("index file not persisted: %v", err)
	}
}

func TestCLIMaxSizeChangeReconciles(t *testing.T) {
	bin := buildCLI(t)
	root := t.TempDir()
	// ~200 byte file with a symbol.
	content := "package a\n\n// " + strings.Repeat("x", 180) + "\nconst Big = 2\n"
	if err := os.WriteFile(filepath.Join(root, "big.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Small limit: file is skipped.
	rep := runCLI(t, bin, root, "index", "--max-size", "100")
	if int(rep["skipped"].(float64)) != 1 {
		t.Fatalf("skipped = %v, want 1", rep["skipped"])
	}

	// Raise the limit via configuration only; the index must reconcile and
	// report the file as affected.
	rep2 := runCLI(t, bin, root, "index", "--max-size", "10485760")
	if rep2["config_changed"] != true {
		t.Fatalf("config_changed = %v, want true", rep2["config_changed"])
	}
	aff, ok := rep2["affected"].([]any)
	if !ok || len(aff) != 1 || aff[0] != "big.go" {
		t.Fatalf("affected = %v, want [big.go]", rep2["affected"])
	}
	if int(rep2["skipped"].(float64)) != 0 {
		t.Fatalf("skipped after raise = %v, want 0", rep2["skipped"])
	}

	q := runCLI(t, bin, root, "query", "--name", "Big")
	arr := q["_array"].([]map[string]any)
	if len(arr) != 1 {
		t.Fatalf("Big query got %d results, want 1", len(arr))
	}
}
