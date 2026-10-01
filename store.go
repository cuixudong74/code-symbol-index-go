package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// FormatVersion is the on-disk format version.
const FormatVersion = 1

// ErrCorrupt is returned when a persisted index file cannot be decoded or
// fails validation.
var ErrCorrupt = errors.New("index: persisted index is corrupt")

// envelope is the on-disk wrapper.
type envelope struct {
	FormatVersion int       `json:"format_version"`
	Snapshot      *Snapshot `json:"snapshot"`
}

// SaveSnapshot atomically persists snap to path. It writes to a temporary
// file in the same directory, fsyncs it, renames it over the destination and
// fsyncs the directory, so a crash at any point leaves either the previous
// complete file or the new complete file (never a partial one).
func SaveSnapshot(snap *Snapshot, path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("index: create index dir: %w", err)
	}

	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("index: create temp index file: %w", err)
	}

	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(envelope{FormatVersion: FormatVersion, Snapshot: snap}); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("index: encode index: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("index: sync index file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("index: close index file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("index: publish index file: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("index: sync index directory: %w", err)
	}
	return nil
}

// LoadSnapshot loads a persisted snapshot from path. The returned snapshot
// has its derived symbol table rebuilt. A missing file returns an error
// satisfying os.IsNotExist; a corrupt file returns ErrCorrupt.
func LoadSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrCorrupt, err)
	}
	if env.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("%w: unsupported format version %d", ErrCorrupt, env.FormatVersion)
	}
	if env.Snapshot == nil || env.Snapshot.Files == nil {
		return nil, fmt.Errorf("%w: empty snapshot", ErrCorrupt)
	}
	env.Snapshot.symbols = buildSymbols(env.Snapshot.Files)
	return env.Snapshot, nil
}

// Save persists the current snapshot of idx to path.
func (idx *Index) Save(path string) error {
	return SaveSnapshot(idx.current.Load(), path)
}

// Open creates an index for cfg and, if path exists, loads its persisted
// snapshot. A missing file yields an empty index; a corrupt file returns
// ErrCorrupt.
func Open(cfg Config, path string) (*Index, error) {
	idx, err := New(cfg)
	if err != nil {
		return nil, err
	}
	snap, err := LoadSnapshot(path)
	if err != nil {
		if os.IsNotExist(err) {
			return idx, nil
		}
		return nil, err
	}
	idx.current.Store(snap)
	return idx, nil
}

// syncDir fsyncs a directory so that a rename is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
