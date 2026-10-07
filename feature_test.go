package index

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// ---------- snapshot comparison ----------

// fileView is the comparable outcome of one file.
type fileView struct {
	Status     string
	ReasonCode string
	Symbols    []string
}

func snapshotView(t *testing.T, snap *Snapshot) (map[string]fileView, map[string]string) {
	t.Helper()
	files := make(map[string]fileView, len(snap.Files))
	for p, fr := range snap.Files {
		var keys []string
		for _, s := range fr.Symbols {
			keys = append(keys, fmt.Sprintf("%d:%d:%s:%s:%s", s.Line, s.Column, s.Name, s.Kind, s.Receiver))
		}
		sort.Strings(keys)
		files[p] = fileView{Status: string(fr.Status), ReasonCode: string(fr.ReasonCode), Symbols: keys}
	}
	skipped := make(map[string]string, len(snap.Skipped))
	for p, sp := range snap.Skipped {
		skipped[p] = string(sp.ReasonCode)
	}
	return files, skipped
}

// requireSameSnapshotAsRebuild asserts that idx's current snapshot is
// file-for-file identical (status, reason code and symbols) to a fresh full
// rebuild over the same workspace with cfg.
func requireSameSnapshotAsRebuild(t *testing.T, idx *Index, cfg Config) {
	t.Helper()
	fresh, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, gotSkip := snapshotView(t, idx.Snapshot())
	want, wantSkip := snapshotView(t, fresh.Snapshot())
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("incremental snapshot != full rebuild\ngot:  %#v\nwant: %#v", got, want)
	}
	if !reflect.DeepEqual(gotSkip, wantSkip) {
		t.Fatalf("skipped paths mismatch\ngot:  %#v\nwant: %#v", gotSkip, wantSkip)
	}
	requireSameSymbols(t, snapshotSymbols(idx.Snapshot()), snapshotSymbols(fresh.Snapshot()))
}

// ---------- 1. config changes: size limit ----------

func TestSizeLimitChangeIncrementalMatchesRebuild(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "ok.go", "package a\n\nconst Keep = 1\n")
	// ~200 bytes, over the 100-byte limit used initially.
	big := "package big\n\n// " + strings.Repeat("z", 190) + "\nconst Big = 1\n"
	writeFile(t, root, "big.go", big)

	cfgSmall := testConfig(root)
	cfgSmall.MaxFileSize = 100
	idx, err := New(cfgSmall)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1", rep.Skipped)
	}
	fr, ok := idx.Snapshot().File("big.go")
	if !ok || fr.Status != StatusSkipped || fr.ReasonCode != ReasonTooLarge {
		t.Fatalf("big.go = %+v, want skipped/too_large", fr)
	}
	if got := idx.Snapshot().Search(Query{Name: "Big"}); len(got) != 0 {
		t.Fatalf("Big must not be indexed under small limit, got %v", got)
	}
	if rep.ConfigChanged {
		t.Fatal("first build must not report ConfigChanged")
	}

	// Raise the limit: the previously skipped file must be listed as affected
	// and become indexed.
	cfgBig := testConfig(root)
	cfgBig.MaxFileSize = DefaultMaxFileSize
	idx.cfg = cfgBig
	rep, err = idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.ConfigChanged {
		t.Fatal("raising the limit must set ConfigChanged")
	}
	if !reflect.DeepEqual(rep.Modified, []string{"big.go"}) {
		t.Fatalf("Modified = %v, want [big.go]", rep.Modified)
	}
	if got := idx.Snapshot().Search(Query{Name: "Big"}); len(got) != 1 {
		t.Fatalf("Big must be indexed after raising the limit, got %v", got)
	}
	requireSameSnapshotAsRebuild(t, idx, cfgBig)

	// A follow-up update is a no-op (config now matches the snapshot).
	rep, err = idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.ConfigChanged || len(rep.Affected) != 0 {
		t.Fatalf("second update should be no-op, changed=%v affected=%v", rep.ConfigChanged, rep.Affected)
	}

	// Shrink the limit again: the indexed file must go back to skipped and be
	// listed as affected.
	idx.cfg = cfgSmall
	rep, err = idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.ConfigChanged || !reflect.DeepEqual(rep.Modified, []string{"big.go"}) {
		t.Fatalf("shrink: changed=%v modified=%v", rep.ConfigChanged, rep.Modified)
	}
	fr, _ = idx.Snapshot().File("big.go")
	if fr.Status != StatusSkipped || fr.ReasonCode != ReasonTooLarge {
		t.Fatalf("big.go after shrink = %s/%s", fr.Status, fr.ReasonCode)
	}
	if got := idx.Snapshot().Search(Query{Name: "Big"}); len(got) != 0 {
		t.Fatalf("Big must disappear after shrinking the limit")
	}
	requireSameSnapshotAsRebuild(t, idx, cfgSmall)
}

func TestSizeLimitChangeAcrossRestart(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "big.go", "package big\n\n// "+strings.Repeat("q", 190)+"\nconst Big = 1\n")

	indexPath := filepath.Join(root, ".codeidx", "index.json")
	cfgSmall := testConfig(root)
	cfgSmall.MaxFileSize = 100
	idx, err := New(cfgSmall)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := idx.Save(indexPath); err != nil {
		t.Fatal(err)
	}

	// Reopen in a new process-style instance with a bigger limit.
	cfgBig := testConfig(root)
	cfgBig.MaxFileSize = DefaultMaxFileSize
	idx2, err := Open(cfgBig, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := idx2.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.ConfigChanged || !reflect.DeepEqual(rep.Modified, []string{"big.go"}) {
		t.Fatalf("restart update: changed=%v modified=%v", rep.ConfigChanged, rep.Modified)
	}
	if got := idx2.Snapshot().Search(Query{Name: "Big"}); len(got) != 1 {
		t.Fatalf("Big should be indexed after restart with bigger limit")
	}
	requireSameSnapshotAsRebuild(t, idx2, cfgBig)
}

// ---------- 1b. policy/rules upgrade ----------

func TestPolicyUpgradeReclassifiesBinary(t *testing.T) {
	root := newTestWorkspace(t)
	// A .go-named binary: under the old (pretend) rules it was recorded as a
	// normal indexed file with stale symbols.
	writeFile(t, root, "bin.go", "package a\n\x00\x00\x01\x02 binary junk\n")
	writeFile(t, root, "ok.go", "package a\n\nconst Keep = 2\n")

	cfg := testConfig(root)
	idx, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Current rules already classify it as binary.
	fr, _ := idx.Snapshot().File("bin.go")
	if fr.Status != StatusSkipped || fr.ReasonCode != ReasonBinary {
		t.Fatalf("bin.go = %s/%s, want skipped/binary", fr.Status, fr.ReasonCode)
	}

	// Forge an old-version snapshot (as produced by a binary that lacked the
	// binary rule) marking the file as ok with bogus symbols.
	old := idx.Snapshot()
	forgedFR := &FileResult{
		Path:        "bin.go",
		Status:      StatusOK,
		Fingerprint: fr.Fingerprint,
		Size:        fr.Size,
		ModTime:     fr.ModTime,
		Symbols:     []Symbol{{Name: "STALE", Kind: KindConstant, Line: 1, Column: 1}},
	}
	oldFiles := make(map[string]*FileResult, len(old.Files))
	for p, f := range old.Files {
		oldFiles[p] = f
	}
	oldFiles["bin.go"] = forgedFR
	stale := &Snapshot{
		Version:       old.Version,
		PolicyVersion: CurrentPolicyVersion - 1,
		MaxFileSize:   cfg.MaxFileSize,
		Files:         oldFiles,
		symbols:       buildSymbols(oldFiles),
	}
	idx.current.Store(stale)

	rep, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.ConfigChanged {
		t.Fatal("policy upgrade must set ConfigChanged")
	}
	if !reflect.DeepEqual(rep.Modified, []string{"bin.go", "ok.go"}) {
		t.Fatalf("Modified = %v, want both unchanged files reprocessed once", rep.Modified)
	}
	if got := idx.Snapshot().Search(Query{Name: "STALE"}); len(got) != 0 {
		t.Fatal("stale symbol must be gone after policy upgrade")
	}
	requireSameSnapshotAsRebuild(t, idx, cfg)

	// Next run is a no-op.
	rep, err = idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.ConfigChanged || len(rep.Affected) != 0 {
		t.Fatalf("post-upgrade update should be no-op, got %v / %v", rep.ConfigChanged, rep.Affected)
	}
}

// ---------- 2. unreadable paths ----------

// inaccessibleWorkspace builds: good.go (readable), noload.go (no permissions)
// and sub/secret/hidden.go behind a 000 directory.
func inaccessibleWorkspace(t *testing.T) string {
	t.Helper()
	// Parent (t.TempDir under /tmp on Linux) is 0700-owned by the runner;
	// when the assertions execute as "nobody" that would hide the root
	// itself. Build the workspace in a world-traversable directory instead.
	base, err := os.MkdirTemp("/tmp", "codeidx-perm-")
	if err != nil {
		t.Fatal(err)
	}
	// MkdirTemp is 0700; open it so the de-privileged child can reach ws.
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	root := filepath.Join(base, "ws")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(root, "sub", "secret"), 0o755)
		_ = os.Chmod(filepath.Join(root, "noload.go"), 0o644)
		_ = os.Chmod(base, 0o755)
	})
	writeFile(t, root, "good.go", "package a\n\nconst Good = 1\n")
	writeFile(t, root, "sub/secret/hidden.go", "package secret\n\nconst Hidden = 1\n")
	writeFile(t, root, "noload.go", "package a\n\nconst Noload = 1\n")
	if err := os.Chmod(filepath.Join(root, "sub", "secret"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "noload.go"), 0o000); err != nil {
		t.Fatal(err)
	}
	return root
}

// assertInaccessibleWorkspaceIndexed holds the shared expectations, whether
// executed in-process (unprivileged user) or in a de-privileged child (root).
func assertInaccessibleWorkspaceIndexed(t *testing.T, root string, reportJSON io.Writer) {
	t.Helper()
	cfg := testConfig(root)
	idx, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := idx.Update(context.Background())
	if err != nil {
		t.Fatalf("update must not fail on unreadable paths: %v", err)
	}
	snap := idx.Snapshot()

	// Readable content still indexed.
	if _, ok := snap.File("good.go"); !ok {
		t.Fatal("good.go must be indexed despite other unreadable paths")
	}
	if got := snap.Search(Query{Name: "Good"}); len(got) != 1 {
		t.Fatalf("Good symbol missing: %v", got)
	}
	// Everything below the unreadable directory is absent (matches rebuild).
	if _, ok := snap.File("sub/secret/hidden.go"); ok {
		t.Fatal("hidden.go must not be indexed under an unreadable directory")
	}
	// The unreadable file is recorded at file level with access_denied.
	nl, ok := snap.File("noload.go")
	if !ok || nl.Status != StatusSkipped || nl.ReasonCode != ReasonAccessDenied {
		t.Fatalf("noload.go = %+v, want skipped/access_denied", nl)
	}
	// The unreadable directory is recorded as a walk-time skipped path.
	sp, ok := snap.Skipped["sub/secret"]
	if !ok {
		t.Fatalf("sub/secret missing from Skipped: %+v", snap.SortedSkipped())
	}
	if !sp.IsDir || sp.ReasonCode != ReasonAccessDenied {
		t.Fatalf("sub/secret record = %+v", sp)
	}
	if !strings.Contains(sp.Reason, "sub/secret") && !strings.Contains(sp.Reason, "secret") {
		t.Fatalf("reason must locate the path: %q", sp.Reason)
	}
	if len(rep.SkippedPaths) != 1 || rep.SkippedPaths[0].Path != "sub/secret" {
		t.Fatalf("report SkippedPaths = %+v", rep.SkippedPaths)
	}

	// Identical to a fresh full rebuild.
	requireSameSnapshotAsRebuild(t, idx, cfg)

	if reportJSON != nil {
		summary := map[string]any{
			"files":         snap.FileCount(),
			"skipped_paths": len(rep.SkippedPaths),
			"good":          rep.TotalFiles,
		}
		_ = json.NewEncoder(reportJSON).Encode(summary)
	}
}

func TestUnreadablePathsDoNotFailUpdate(t *testing.T) {
	root := inaccessibleWorkspace(t)

	if os.Geteuid() != 0 {
		// Unprivileged: permission bits take effect directly.
		assertInaccessibleWorkspaceIndexed(t, root, nil)
		return
	}

	// Running as root: DAC permissions are bypassed. Re-run the same
	// assertions in a child test process de-privileged to "nobody".
	helperBin, args, ok := dePrivilegeCommand(t)
	if !ok {
		t.Skip("cannot drop privileges (need runuser or setpriv); skipping unreadable-path test")
	}
	args = append(args, os.Args[1:]...)
	// Force only the child test to run.
	args = append(args, "-test.run=^TestUnreadablePathsChild$", "-test.v")
	cmd := exec.Command(helperBin, args...)
	cmd.Env = append(os.Environ(), "CODEIDX_PERM_ROOT="+root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("de-privileged child failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "CHILD_OK") {
		t.Fatalf("child did not finish successfully:\n%s", out)
	}
}

// TestUnreadablePathsChild only runs when explicitly invoked by
// TestUnreadablePathsDoNotFailUpdate through a de-privileged helper.
func TestUnreadablePathsChild(t *testing.T) {
	root := os.Getenv("CODEIDX_PERM_ROOT")
	if root == "" {
		t.Skip("helper only")
	}
	assertInaccessibleWorkspaceIndexed(t, root, nil)
	t.Log("CHILD_OK")
}

// dePrivilegeCommand returns a command prefix that executes its arguments as
// the unprivileged "nobody" user, copying the test binary to a world
// executable location first (the go-built test binary under TMPDIR is 0700).
func dePrivilegeCommand(t *testing.T) (string, []string, bool) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(os.TempDir(), fmt.Sprintf("codeidx-helper-%d", os.Getpid()))
	if err := copyFile(self, dst, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(dst) })

	if _, err := exec.LookPath("runuser"); err == nil {
		return "runuser", []string{"-u", "nobody", "--", dst}, true
	}
	if _, err := exec.LookPath("setpriv"); err == nil {
		return "setpriv", []string{"--reuid=65534", "--regid=65534", "--clear-groups", dst}, true
	}
	return "", nil, false
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// Root directory completely unusable must still return an error (nothing to
// index), preserving the documented boundary behaviour.
func TestMissingRootStillErrors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "gone")
	idx, err := New(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background()); err == nil {
		t.Fatal("missing root must return an error")
	}
}

// ---------- 3. binary and oversized files ----------

func TestBinaryGoFileSkipped(t *testing.T) {
	root := newTestWorkspace(t)
	// ELF-ish content with NUL bytes, using a .go name.
	writeBin(t, root, "artifact.go", []byte{0x7f, 'E', 'L', 'F', 0x02, 0x00, 0x00, 0x00, 0, 0, 0, 0})
	writeFile(t, root, "real.go", "package a\n\nconst Real = 1\n")

	idx, err := New(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1", rep.Skipped)
	}
	fr, ok := idx.Snapshot().File("artifact.go")
	if !ok || fr.Status != StatusSkipped || fr.ReasonCode != ReasonBinary {
		t.Fatalf("artifact.go = %+v, want skipped/binary", fr)
	}
	if got := idx.Snapshot().Search(Query{Name: "Real"}); len(got) != 1 {
		t.Fatalf("real.go symbol missing: %v", got)
	}
	// Reasons are distinguishable.
	if fr.ReasonCode == ReasonTooLarge || fr.ReasonCode == ReasonInvalidEncoding {
		t.Fatalf("binary reason code collided: %s", fr.ReasonCode)
	}
	requireSameSnapshotAsRebuild(t, idx, testConfig(root))

	// Editing the binary file still re-detects it incrementally.
	writeBin(t, root, "artifact.go", []byte{0x00, 0x01, 0x02, 0x03})
	rep, err = idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Modified) != 1 || rep.Modified[0] != "artifact.go" {
		t.Fatalf("Modified = %v", rep.Modified)
	}
	fr, _ = idx.Snapshot().File("artifact.go")
	if fr.ReasonCode != ReasonBinary {
		t.Fatalf("re-detect = %s", fr.ReasonCode)
	}
}

func TestNonGoBinaryAndLargeFilesAreCheap(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "real.go", "package a\n\nconst Real = 1\n")
	// A multi-MB non-Go binary artifact: must not even be read by the walker.
	writeBin(t, root, "blob.bin", make([]byte, 6<<20))
	// A multi-MB .go file over the default limit: skipped by stat, never read.
	big := append([]byte("package big\n\n// "), make([]byte, 6<<20)...)
	big = append(big, []byte("\nconst Big = 1\n")...)
	writeBin(t, root, "huge.go", big)

	cfg := testConfig(root) // default 5 MiB limit
	idx, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := idx.Snapshot().File("blob.bin"); ok {
		t.Fatal("non-.go binary must not participate in indexing")
	}
	fr, ok := idx.Snapshot().File("huge.go")
	if !ok || fr.Status != StatusSkipped || fr.ReasonCode != ReasonTooLarge {
		t.Fatalf("huge.go = %+v, want skipped/too_large", fr)
	}
	if !strings.HasPrefix(fr.Fingerprint, "stat:") {
		t.Fatalf("oversized file fingerprint %q should be stat-based (file not read)", fr.Fingerprint)
	}
	if rep.TotalSymbols != 2 { // package + const
		t.Fatalf("TotalSymbols = %d, want 2", rep.TotalSymbols)
	}
	requireSameSnapshotAsRebuild(t, idx, cfg)

	// Incremental no-op over the workspace with big files stays cheap.
	rep2, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep2.Affected) != 0 {
		t.Fatalf("no-op affected = %v", rep2.Affected)
	}
}

func TestDistinctReasonCodes(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "ok.go", "package a\nconst Ok = 1\n")
	writeBin(t, root, "bin.go", []byte{0x00, 0x01, 0x00})
	writeBin(t, root, "enc.go", []byte("package a\n\xff\xfe\n"))
	big := "package a\n// " + strings.Repeat("x", 200)
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
	snap := idx.Snapshot()
	cases := map[string]ReasonCode{
		"bin.go": ReasonBinary,
		"enc.go": ReasonInvalidEncoding,
		"big.go": ReasonTooLarge,
	}
	for name, want := range cases {
		fr, ok := snap.File(name)
		if !ok {
			t.Fatalf("%s missing", name)
		}
		if fr.ReasonCode != want {
			t.Fatalf("%s code = %q (%q), want %q", name, fr.ReasonCode, fr.Reason, want)
		}
	}
	// Ensure the sentinel error classification helper exists and maps
	// permission errors.
	if got := classifyAccessError(errors.New("denied")); got != ReasonAccessDenied {
		t.Fatalf("classifyAccessError = %s", got)
	}
}

func writeBin(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
