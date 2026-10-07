package index

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

// permissionEnforced reports whether the process is actually constrained by
// Unix permission bits. It returns false when running as root (or in an
// environment where DAC is overridden), in which case permission-based tests
// cannot exercise the real kernel behaviour and must skip.
func permissionEnforced(t *testing.T) bool {
	t.Helper()
	if runtime.GOOS == "windows" {
		return false
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "probe")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f, 0o000); err != nil {
		return false
	}
	fd, err := os.Open(f)
	if err == nil {
		fd.Close()
		os.Chmod(f, 0o600)
		return false // access granted despite 0000 bits -> root-like environment
	}
	os.Chmod(f, 0o600)
	return true
}

// TestUnreadableDirectoryDoesNotAbortUpdate: a directory without traverse
// permission is reported as an Issue with a distinct reason code and path, the
// update still succeeds, every readable file outside that directory is
// indexed, and the result matches a full rebuild.
func TestUnreadableDirectoryDoesNotAbortUpdate(t *testing.T) {
	if !permissionEnforced(t) {
		t.Skip("permission bits are not enforced in this environment (e.g. running as root)")
	}
	root := newTestWorkspace(t)
	writeFile(t, root, "ok.go", "package a\n\nconst OK = 1\n")
	writeFile(t, root, "sub/inside.go", "package sub\n\nconst Inside = 1\n")
	writeFile(t, root, "other/keep.go", "package other\n\nconst Keep = 1\n")

	bad := filepath.Join(root, "sub")
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(bad, 0o755)

	idx, err := New(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	r, err := idx.Update(context.Background())
	if err != nil {
		t.Fatalf("update must not fail because of one unreadable dir: %v", err)
	}
	if len(r.Issues) != 1 {
		t.Fatalf("Issues = %+v, want exactly 1", r.Issues)
	}
	is := r.Issues[0]
	if is.Path != "sub" {
		t.Fatalf("issue path = %q, want sub", is.Path)
	}
	if is.Scope != "dir" {
		t.Fatalf("issue scope = %q, want dir", is.Scope)
	}
	if is.ReasonCode != ReasonPermissionDenied {
		t.Fatalf("issue reason = %q, want %s", is.ReasonCode, ReasonPermissionDenied)
	}
	if is.Reason == "" {
		t.Fatal("issue Reason should be non-empty")
	}

	// Readable content is indexed normally.
	if _, ok := idx.Snapshot().File("ok.go"); !ok {
		t.Fatal("ok.go should be indexed")
	}
	if _, ok := idx.Snapshot().File("other/keep.go"); !ok {
		t.Fatal("other/keep.go should be indexed")
	}
	if _, ok := idx.Snapshot().File("sub/inside.go"); ok {
		t.Fatal("files inside an unreadable dir must not be indexed")
	}
	if got := idx.Snapshot().Search(Query{Name: "OK"}); len(got) != 1 {
		t.Fatalf("OK search: %v", got)
	}

	// Incremental result equals full rebuild (a rebuild run on the same
	// workspace faces the same permission state).
	full, err := New(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	fr, err := full.Rebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(fr.Issues) != 1 {
		t.Fatalf("full rebuild issues = %+v", fr.Issues)
	}
	requireSnapshotsEqual(t, idx.Snapshot(), full.Snapshot())

	// Restore access: the previously inaccessible subtree is picked up
	// incrementally as added and the issue disappears.
	if err := os.Chmod(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	r2, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.Issues) != 0 {
		t.Fatalf("restored dir should clear the issue, got %+v", r2.Issues)
	}
	if !reflect.DeepEqual(r2.Added, []string{"sub/inside.go"}) {
		t.Fatalf("Added = %v, want [sub/inside.go]", r2.Added)
	}
	if _, ok := idx.Snapshot().File("sub/inside.go"); !ok {
		t.Fatal("sub/inside.go should be indexed after restoring permissions")
	}
}

// TestUnreadableFileDoesNotAbortUpdate: a .go file without read permission is
// recorded as a skipped FileResult with a permission reason code; sibling
// files are still indexed.
func TestUnreadableFileDoesNotAbortUpdate(t *testing.T) {
	if !permissionEnforced(t) {
		t.Skip("permission bits are not enforced in this environment (e.g. running as root)")
	}
	root := newTestWorkspace(t)
	writeFile(t, root, "ok.go", "package a\n\nconst OK = 1\n")
	writeFile(t, root, "secret.go", "package a\n\nconst Secret = 1\n")

	bad := filepath.Join(root, "secret.go")
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(bad, 0o644)

	idx, err := New(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	r, err := idx.Update(context.Background())
	if err != nil {
		t.Fatalf("unreadable file must not abort the update: %v", err)
	}
	if len(r.Issues) != 0 {
		t.Fatalf("a visible-but-unreadable file is handled per-file, Issues = %+v", r.Issues)
	}
	fr, ok := idx.Snapshot().File("secret.go")
	if !ok {
		t.Fatal("secret.go should have a snapshot entry")
	}
	if fr.Status != StatusSkipped || fr.ReasonCode != ReasonPermissionDenied {
		t.Fatalf("secret.go = %s/%s, want skipped/%s", fr.Status, fr.ReasonCode, ReasonPermissionDenied)
	}
	if got := idx.Snapshot().Search(Query{Name: "Secret"}); len(got) != 0 {
		t.Fatalf("Secret must not be queryable: %v", got)
	}
	if _, ok := idx.Snapshot().File("ok.go"); !ok {
		t.Fatal("ok.go should still be indexed")
	}

	full := fullBuildWith(t, testConfig(root))
	requireSnapshotsEqual(t, idx.Snapshot(), full.Snapshot())

	// Restore read access: the file is picked up incrementally.
	if err := os.Chmod(bad, 0o644); err != nil {
		t.Fatal(err)
	}
	r2, err := idx.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r2.Modified, []string{"secret.go"}) {
		t.Fatalf("Modified = %v, want [secret.go]", r2.Modified)
	}
	if got := idx.Snapshot().Search(Query{Name: "Secret"}); len(got) != 1 {
		t.Fatalf("Secret should be queryable after restore: %v", got)
	}
}

// TestDanglingSymlinkIsSkipped works without permission enforcement:
// WalkDir lstat()s entries without following symlinks, so a dangling symlink
// surfaces later as a stat failure inside indexFile and must degrade to a
// not_readable skip rather than killing the run.
func TestDanglingSymlinkIsSkipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks behave differently on windows")
	}
	root := newTestWorkspace(t)
	writeFile(t, root, "ok.go", "package a\n\nconst OK = 1\n")
	if err := os.Symlink(filepath.Join(root, "does-not-exist"), filepath.Join(root, "dangle.go")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	idx, err := New(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	r, err := idx.Update(context.Background())
	if err != nil {
		t.Fatalf("dangling symlink must not abort update: %v", err)
	}
	if len(r.Issues) != 0 {
		t.Fatalf("walk itself should not flag a dangling symlink: %+v", r.Issues)
	}
	fr, ok := idx.Snapshot().File("dangle.go")
	if !ok {
		t.Fatal("dangle.go should have a snapshot entry")
	}
	if fr.Status != StatusSkipped || fr.ReasonCode != ReasonNotReadable {
		t.Fatalf("dangle.go = %s/%s, want skipped/%s", fr.Status, fr.ReasonCode, ReasonNotReadable)
	}
	if _, ok := idx.Snapshot().File("ok.go"); !ok {
		t.Fatal("ok.go must be indexed despite the broken symlink")
	}
	full := fullBuildWith(t, testConfig(root))
	requireSnapshotsEqual(t, idx.Snapshot(), full.Snapshot())
}

// TestMissingRootStillFails keeps the hard-failure contract for the workspace
// root itself: when the root cannot be opened at all, no snapshot is
// published and the error is surfaced.
func TestMissingRootStillFails(t *testing.T) {
	root := newTestWorkspace(t)
	missing := filepath.Join(root, "nope")
	idx, err := New(testConfig(missing))
	if err != nil {
		t.Fatal(err)
	}
	before := idx.Snapshot()
	if _, err := idx.Update(context.Background()); err == nil {
		t.Fatal("update over a nonexistent root should fail")
	}
	if idx.Snapshot() != before {
		t.Fatal("a failed walk must not publish a new snapshot")
	}
}

// TestWalkFilesIssuesDirectly exercises walkFiles independently of the
// permission-enforced integration test.
func TestWalkFilesCollectsIssues(t *testing.T) {
	root := newTestWorkspace(t)
	writeFile(t, root, "a.go", "package a\n")
	writeFile(t, root, "sub/b.go", "package sub\n")

	paths, issues, err := walkFiles(root, []string{".git", ".codeidx"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{"a.go", "sub/b.go"}) {
		t.Fatalf("paths = %v", paths)
	}
	if len(issues) != 0 {
		t.Fatalf("issues = %+v", issues)
	}

	// Missing root still errors.
	if _, _, err := walkFiles(filepath.Join(root, "missing"), nil); err == nil {
		t.Fatal("walk of a missing root should error")
	}
}
