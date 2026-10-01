package symbolindex

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Engine owns the currently committed immutable Snapshot.
//
// Updates are serialized because they establish a causal version chain, but
// searches never acquire the update lock. A long search pins the old snapshot;
// the update can build and publish a replacement concurrently.
type Engine struct {
	cfg      Config
	updateMu sync.Mutex
	current  atomic.Pointer[Snapshot]
	store    *Store
}

// NewEngine creates an engine with an empty initial snapshot.
func NewEngine(cfg Config) (*Engine, error) {
	normalized, err := cfg.Normalize()
	if err != nil {
		return nil, err
	}
	e := &Engine{cfg: normalized}
	e.current.Store(newSnapshot(normalized, 0, nil, time.Now().UTC()))
	return e, nil
}

// NewEngineWithStore creates an engine and attempts to reuse the store. If no
// index exists, the engine starts empty. If an existing on-disk index is
// corrupt, the error is returned rather than silently overwriting evidence.
func NewEngineWithStore(cfg Config, store *Store) (*Engine, error) {
	if store == nil {
		return nil, errors.New("index store is required")
	}
	e, err := NewEngine(cfg)
	if err != nil {
		return nil, err
	}
	e.store = store
	loaded, err := store.Load(context.Background())
	if err != nil {
		return nil, err
	}
	if loaded != nil {
		loaded.defaultPageSize = e.cfg.DefaultPageSize
		loaded.maxQueryLimit = e.cfg.MaxQueryLimit
		e.current.Store(loaded)
	}
	return e, nil
}

// Update scans the workspace, rebuilds only changed file records, then commits
// exactly one complete snapshot. Concurrent Update calls are serialized.
func (e *Engine) Update(ctx context.Context) (Report, error) {
	e.updateMu.Lock()
	defer e.updateMu.Unlock()
	next, report, err := buildSnapshot(ctx, e.cfg, e.current.Load())
	if err != nil {
		return Report{}, err
	}
	e.current.Store(next)
	return report, nil
}

// UpdateAndSave performs an update and persists the committed snapshot. The
// snapshot is visible in-process even if saving fails, because publication
// already happened; callers may retry Save without rebuilding.
func (e *Engine) UpdateAndSave(ctx context.Context) (Report, error) {
	report, err := e.Update(ctx)
	if err != nil {
		return report, err
	}
	if e.store != nil {
		if err := e.store.Save(ctx, e.current.Load()); err != nil {
			return report, err
		}
	}
	return report, nil
}

// Snapshot returns the current complete, immutable index version.
func (e *Engine) Snapshot() *Snapshot {
	if e == nil {
		return nil
	}
	return e.current.Load()
}

// Search runs against the snapshot active at call entry.
func (e *Engine) Search(q Query) (SearchResult, error) {
	snap := e.Snapshot()
	if snap == nil {
		return SearchResult{Symbols: []Symbol{}}, nil
	}
	return snap.Search(q)
}

// Outline returns one file's outline from the active snapshot.
func (e *Engine) Outline(path string) (OutlineResult, bool) {
	snap := e.Snapshot()
	if snap == nil {
		return OutlineResult{}, false
	}
	return snap.Outline(path)
}

// Save persists the currently committed snapshot.
func (e *Engine) Save(ctx context.Context) error {
	if e == nil || e.store == nil {
		return errors.New("engine has no persistent store")
	}
	return e.store.Save(ctx, e.current.Load())
}

// Close releases engine resources. It currently has no background goroutines.
func (e *Engine) Close() error { return nil }
