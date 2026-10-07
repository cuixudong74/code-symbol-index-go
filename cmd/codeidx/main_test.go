package main

import (
	"bytes"
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
	out := cliRun(t, bin, root, args)
	var v map[string]any
	if err := json.Unmarshal(out.stdout, &v); err != nil {
		// Some outputs are arrays.
		var arr []map[string]any
		if err2 := json.Unmarshal(out.stdout, &arr); err2 != nil {
			t.Fatalf("parse output: %v\n%s", err, out.stdout)
		}
		v = map[string]any{"_array": arr}
	}
	return v
}

type cliOutput struct {
	stdout []byte
	stderr []byte
}

func cliRun(t *testing.T, bin, root string, args []string) cliOutput {
	t.Helper()
	cmd := exec.Command(bin, append(append([]string{}, args...), "--root", root)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("cli %v: %v\nstdout: %s\nstderr: %s", args, err, stdout.Bytes(), stderr.Bytes())
	}
	return cliOutput{stdout: stdout.Bytes(), stderr: stderr.Bytes()}
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

func TestCLIBinarySkipAndSizeLimitChange(t *testing.T) {
	bin := buildCLI(t)
	root := t.TempDir()

	// A normal source file and a .go-named binary artifact.
	if err := os.WriteFile(filepath.Join(root, "ok.go"),
		[]byte("package a\n\nconst Keep = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "blob.go"),
		[]byte{0x7f, 'E', 'L', 'F', 0x00, 0x01, 0x00, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}
	// A ~200-byte source file skipped under a 100-byte limit.
	big := "package big\n\n// " + string(bytes.Repeat([]byte("z"), 190)) + "\nconst Big = 1\n"
	if err := os.WriteFile(filepath.Join(root, "big.go"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := cliRun(t, bin, root, []string{"index", "--max-size", "100"})
	var r map[string]any
	if err := json.Unmarshal(rep.stdout, &r); err != nil {
		t.Fatalf("index json: %v\n%s", err, rep.stdout)
	}
	if int(r["Skipped"].(float64)) != 2 { // binary + oversized
		t.Fatalf("Skipped = %v", r["Skipped"])
	}

	// Status reports the file-level skip counts and policy metadata.
	stOut := cliRun(t, bin, root, []string{"status"})
	var st map[string]any
	if err := json.Unmarshal(stOut.stdout, &st); err != nil {
		t.Fatalf("status json: %v\n%s", err, stOut.stdout)
	}
	if int(st["files_skipped"].(float64)) != 2 {
		t.Fatalf("files_skipped = %v", st["files_skipped"])
	}
	if int(st["policy_version"].(float64)) < 1 {
		t.Fatalf("policy_version = %v", st["policy_version"])
	}

	// The big file is not queryable under the small limit.
	q := cliRun(t, bin, root, []string{"query", "--name", "Big"})
	var arr []map[string]any
	if err := json.Unmarshal(q.stdout, &arr); err != nil || len(arr) != 0 {
		t.Fatalf("Big must be absent under small limit: %s", q.stdout)
	}

	// Re-index with the default limit: incremental update must pick up big.go.
	rep2 := cliRun(t, bin, root, []string{"index"})
	var r2 map[string]any
	if err := json.Unmarshal(rep2.stdout, &r2); err != nil {
		t.Fatalf("second index json: %v\n%s", err, rep2.stdout)
	}
	if r2["ConfigChanged"] != true {
		t.Fatalf("ConfigChanged = %v, want true", r2["ConfigChanged"])
	}
	modified := r2["Modified"].([]any)
	if len(modified) != 1 || modified[0] != "big.go" {
		t.Fatalf("Modified = %v, want [big.go]", modified)
	}

	// big.go now queryable; the binary stays skipped and never indexes.
	q = cliRun(t, bin, root, []string{"query", "--name", "Big"})
	if err := json.Unmarshal(q.stdout, &arr); err != nil || len(arr) != 1 {
		t.Fatalf("Big should be indexed after raising limit: %s", q.stdout)
	}
	stOut = cliRun(t, bin, root, []string{"status"})
	if err := json.Unmarshal(stOut.stdout, &st); err != nil {
		t.Fatalf("status json: %v", stOut.stdout)
	}
	if int(st["files_skipped"].(float64)) != 1 { // only the binary remains
		t.Fatalf("files_skipped after raising limit = %v, want 1", st["files_skipped"])
	}
}
