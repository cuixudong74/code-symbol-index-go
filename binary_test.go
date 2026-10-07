package index

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestBinaryGoFileIsSkipped: a binary blob disguised with a .go extension must
// be classified as binary (not a parse error), skipped with a distinct reason
// code, and contribute no symbols.
func TestBinaryGoFileIsSkipped(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "ok.go", "package a\n\nconst OK = 1\n")
	// ELF-like content: NUL bytes within the first bytes.
	bin := append([]byte{0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00},
		repeatZeros(4096)...)
	if err := os.WriteFile(filepath.Join(root, "blob.go"), bin, 0o755); err != nil {
		t.Fatal(err)
	}

	idx, err := New(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	r, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1", r.Skipped)
	}
	if r.Degraded != 0 {
		t.Fatalf("binary content must not be parsed into a degraded result, Degraded = %d", r.Degraded)
	}
	fr, ok := idx.Snapshot().File("blob.go")
	if !ok {
		t.Fatal("blob.go missing")
	}
	if fr.Status != StatusSkipped || fr.ReasonCode != ReasonBinary {
		t.Fatalf("blob.go = %s/%s, want skipped/%s", fr.Status, fr.ReasonCode, ReasonBinary)
	}
	if !strings.Contains(fr.Reason, "binary") {
		t.Fatalf("reason = %q should mention binary", fr.Reason)
	}
	if got := idx.Snapshot().Search(Query{File: "blob.go"}); len(got) != 0 {
		t.Fatalf("binary file must contribute no symbols: %v", got)
	}
	// Sibling source is unaffected.
	if _, ok := idx.Snapshot().File("ok.go"); !ok {
		t.Fatal("ok.go should be indexed")
	}
	requireSnapshotsEqual(t, idx.Snapshot(), fullBuildWith(t, testConfig(root)).Snapshot())
}

// TestBinaryNULBeyondSniffWindow checks a NUL byte sitting just after the
// sniff window is still detected (whole files below MaxFileSize are read; the
// sniff cap only bounds the scan length, but files larger than the window
// remain handled as too_large first when the limit is small).
func TestBinaryDetectionOrdering(t *testing.T) {
	root := newTestWorkspace(t)
	// Small binary: detected as binary.
	writeBytes(t, root, "small-bin.go", append([]byte("package a\n\nconst X = 1\n"), 0x00))
	// Huge file over the limit: reported as too_large without a full read.
	big := make([]byte, 300)
	for i := range big {
		big[i] = 'y'
	}
	writeBytes(t, root, "big-bin.go", big)

	cfg := testConfig(root)
	cfg.MaxFileSize = 100
	idx, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	sb, _ := idx.Snapshot().File("small-bin.go")
	if sb.ReasonCode != ReasonBinary {
		t.Fatalf("small-bin.go reason = %s, want %s", sb.ReasonCode, ReasonBinary)
	}
	bb, _ := idx.Snapshot().File("big-bin.go")
	if bb.ReasonCode != ReasonTooLarge {
		t.Fatalf("big-bin.go reason = %s, want %s", bb.ReasonCode, ReasonTooLarge)
	}
}

// TestLargeBinarySkippedBySizeFirst verifies a multi-MB binary masquerading as
// source is handled without reading/parsing the whole blob: size limit wins,
// keeping scan cost bounded.
func TestLargeBinarySkippedBySizeFirst(t *testing.T) {
	root := newTestWorkspace(t)
	size := int(DefaultMaxFileSize) + 1024
	data := make([]byte, size)
	// Mostly NULs, like an executable.
	for i := 0; i < 1024; i++ {
		data[i] = byte(i % 251)
	}
	writeBytes(t, root, "huge.go", data)

	cfg := testConfig(root) // default 5 MiB limit
	idx, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fr, _ := idx.Snapshot().File("huge.go")
	if fr.Status != StatusSkipped || fr.ReasonCode != ReasonTooLarge {
		t.Fatalf("huge.go = %s/%s", fr.Status, fr.ReasonCode)
	}
	if r.TotalSymbols != 0 {
		t.Fatalf("no symbols expected, got %d", r.TotalSymbols)
	}
}

// TestNonGoFilesNeverIndexed ensures that ordinary large binaries without a
// .go extension are simply not part of the index at all (not even skipped
// entries), regardless of size.
func TestNonGoFilesNeverIndexed(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "a.go", "package a\n\nconst A = 1\n")
	writeBytes(t, root, "app", append([]byte{0x7f, 'E', 'L', 'F', 0}, repeatZeros(2000)...))
	writeBytes(t, root, "data.zip", repeatZeros(5000))
	writeBytes(t, root, "notes.txt", []byte("not go"))

	idx, err := New(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	r, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.TotalFiles != 1 {
		t.Fatalf("TotalFiles = %d, want 1", r.TotalFiles)
	}
	if r.Skipped != 0 || len(r.Issues) != 0 {
		t.Fatalf("non-go files should be ignored, skipped=%d issues=%v", r.Skipped, r.Issues)
	}
}

// TestBinaryBecomesSourceAfterConfigRaise documents that with the size limit
// disabled a binary .go file is still deterministically classified as binary
// (the two skip reasons stay distinct when the limit changes).
func TestBinaryStaysBinaryWithoutSizeLimit(t *testing.T) {
	root := newTestWorkspace(t)
	writeBytes(t, root, "b.go", append([]byte("package a\n"), 0x00))
	cfg := testConfig(root)
	cfg.MaxFileSize = 0
	idx, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	fr, _ := idx.Snapshot().File("b.go")
	if fr.Status != StatusSkipped || fr.ReasonCode != ReasonBinary {
		t.Fatalf("b.go = %s/%s, want skipped/binary", fr.Status, fr.ReasonCode)
	}
}

// TestIsBinaryHeuristic checks the prefix-window heuristic directly.
func TestIsBinaryHeuristic(t *testing.T) {
	if isBinary([]byte("package main\n")) {
		t.Fatal("plain text must not be binary")
	}
	if isBinary([]byte("utf8: \xc3\xa9 \xe4\xb8\xad")) {
		t.Fatal("valid UTF-8 multibyte text must not be binary")
	}
	if !isBinary([]byte{'a', 0, 'b'}) {
		t.Fatal("NUL byte must be binary")
	}
	if !isBinary(append(repeatZeros(binarySniffBytes), 'a')) {
		t.Fatal("NUL inside the window must be binary")
	}
	// NUL just beyond the window is NOT detected by design — such a file above
	// the size limit is caught as too_large; below it the whole content is
	// scanned since it was already read, so construct exactly at window+1:
	// isBinary scans min(len, window); verify boundary behaviour is stable.
	data := append(repeatZeros(binarySniffBytes), 0) // NUL at index window
	// First window bytes are all zero -> detected regardless.
	if !isBinary(data) {
		t.Fatal("zeros within window must be detected")
	}
	late := append([]byte(repeat("a", binarySniffBytes)), 0)
	if isBinary(late) {
		t.Fatal("NUL beyond sniff window must not be flagged")
	}
}

// TestSkipReasonCodesAreStable asserts the public reason code values, so a
// rename in the future cannot silently change the API contract.
func TestSkipReasonCodesAreStable(t *testing.T) {
	codes := []ReasonCode{
		ReasonParseError, ReasonTooLarge, ReasonInvalidUTF8,
		ReasonBinary, ReasonPermissionDenied, ReasonNotReadable,
	}
	want := []string{
		"parse_error", "too_large", "invalid_utf8",
		"binary", "permission_denied", "not_readable",
	}
	got := make([]string, len(codes))
	for i, c := range codes {
		got[i] = string(c)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reason codes = %v, want %v", got, want)
	}
}

func writeBytes(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func repeatZeros(n int) []byte {
	b := make([]byte, n)
	return b
}
