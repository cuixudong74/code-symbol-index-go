package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cli %v: %v\n%s", args, err, out)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		// Some outputs are arrays.
		var arr []map[string]any
		if err2 := json.Unmarshal(out, &arr); err2 != nil {
			t.Fatalf("parse output: %v\n%s", err, out)
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
	if int(rep["TotalFiles"].(float64)) != 2 {
		t.Fatalf("TotalFiles = %v", rep["TotalFiles"])
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
	removed := rep2["Removed"].([]any)
	if len(removed) != 1 || removed[0].(string) != "b.go" {
		t.Fatalf("Removed = %v", rep2["Removed"])
	}

	// Index file exists on disk.
	if _, err := os.Stat(filepath.Join(root, ".codeidx", "index.json")); err != nil {
		t.Fatalf("index file not persisted: %v", err)
	}
}
