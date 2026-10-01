package symbolindex

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestConcurrentUpdatesAndSearches(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 32; i++ {
		writeWorkspaceFile(t, root, fmt.Sprintf("pkg%02d/f.go", i),
			fmt.Sprintf("package pkg%02d\nfunc Symbol%02d() {}\n", i, i))
	}
	cfg := testConfig(root)
	cfg.Workers = 8
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Update(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 200 && ctx.Err() == nil; i++ {
				snap := e.Snapshot()
				if snap == nil {
					t.Errorf("nil snapshot")
					return
				}
				result, err := snap.Search(Query{Prefix: "Symbol", Limit: 64})
				if err != nil {
					t.Errorf("search: %v", err)
					return
				}
				// Each page is taken from one immutable snapshot. Total must
				// always match the unfiltered complete version's symbol count.
				if result.Total != snap.SymbolCount() {
					t.Errorf("mixed result: page total=%d snapshot=%d", result.Total, snap.SymbolCount())
					return
				}
				for _, sym := range result.Symbols {
					if filepath.ToSlash(sym.Path) != sym.Path || sym.Name == "" || sym.Kind == "" {
						t.Errorf("incomplete symbol: %+v", sym)
						return
					}
				}
			}
		}(w)
	}
	for u := 0; u < 3; u++ {
		if _, err := e.Update(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	wg.Wait()
}

func TestBuilderConcurrencyDoesNotAffectResult(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 64; i++ {
		writeWorkspaceFile(t, root, fmt.Sprintf("d%02d/f%02d.go", i%8, i),
			fmt.Sprintf("package d%02d\nfunc Name%02d() {}\nvar V%02d = 1\n", i%8, i, i))
	}
	base := testConfig(root)
	one := base
	one.Workers = 1
	many := base
	many.Workers = 16

	e1, err := NewEngine(one)
	if err != nil {
		t.Fatal(err)
	}
	e16, err := NewEngine(many)
	if err != nil {
		t.Fatal(err)
	}
	updateNow(t, e1)
	updateNow(t, e16)
	assertSnapshotsEqual(t, e1.Snapshot(), e16.Snapshot())
}

func TestSlowSearchDoesNotBlockUpdate(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package p\nfunc Old() {}\n")
	e, err := NewEngine(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	updateNow(t, e)
	old := e.Snapshot()

	waitForMTimeSkew()
	writeWorkspaceFile(t, root, "a.go", "package p\nfunc New() {}\n")
	finished := make(chan Report, 1)
	errCh := make(chan error, 1)
	go func() {
		r, err := e.Update(context.Background())
		errCh <- err
		finished <- r
	}()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
		<-finished
	case <-time.After(time.Second):
		t.Fatal("update blocked while a slow query retained an old snapshot")
	}
	if got, _ := e.Search(Query{Name: "New"}); got.Total != 1 {
		t.Fatalf("new snapshot not published: %+v", got)
	}
	// The retained old version remains fully readable after publication.
	if got, _ := old.Search(Query{Name: "Old"}); got.Total != 1 {
		t.Fatalf("old snapshot changed during slow query: %+v", got)
	}
}
