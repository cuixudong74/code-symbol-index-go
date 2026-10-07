package index

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// TestConcurrentConfigUpdateRead mixes SetConfig (alternating MaxFileSize),
// Update and concurrent snapshot readers. Readers must only ever observe
// complete, internally consistent snapshots (run with -race).
func TestConcurrentConfigUpdateRead(t *testing.T) {
	root := newTestWorkspace(t)
	for i := 0; i < 6; i++ {
		writeFile(t, root, fmt.Sprintf("f%d.go", i),
			fmt.Sprintf("package p%d\n\nconst C%d = %d\n", i, i, i))
	}

	cfg := testConfig(root)
	cfg.MaxFileSize = 1 << 20
	idx, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var readerWG, writerWG sync.WaitGroup

	// Readers continuously validate and query the committed snapshot.
	for r := 0; r < 4; r++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if err := validateSnapshot(idx.Snapshot()); err != nil {
					t.Error(err)
					return
				}
				_ = idx.Snapshot().Search(Query{Prefix: "C", Limit: 10})
			}
		}()
	}

	// Reconfigurer alternates the size limit and updates. Both limits cover
	// every test file, so this stresses the settings-signature / reprocessing
	// path without changing the expected symbol set.
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		for i := 0; i < 20; i++ {
			c := idx.Config()
			if i%2 == 0 {
				c.MaxFileSize = 0
			} else {
				c.MaxFileSize = 1 << 20
			}
			if err := idx.SetConfig(c); err != nil {
				t.Error(err)
				return
			}
			if _, err := idx.Update(context.Background()); err != nil {
				t.Error(err)
				return
			}
		}
	}()

	writerWG.Wait()
	cancel()
	readerWG.Wait()

	if err := validateSnapshot(idx.Snapshot()); err != nil {
		t.Fatal(err)
	}
	if got := idx.Snapshot().Search(Query{Prefix: "C"}); len(got) != 6 {
		t.Fatalf("expected 6 C symbols in final snapshot, got %d", len(got))
	}
}
