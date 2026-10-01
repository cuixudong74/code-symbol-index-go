package symbolindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	storeManifestName = "manifest.json"
	storeFormatV1     = "code-symbol-index/v1"
)

// Store manages crash-safe snapshot persistence inside one directory.
//
// A complete data file is written and synced first under a version-specific
// name. The manifest that names the usable data file is then atomically
// replaced. Therefore a crash during data writing leaves the old manifest and
// old data intact; a crash during manifest replacement exposes either the old
// or new complete manifest, never a partially written one.
type Store struct {
	dir string
	mu  sync.Mutex
}

type storedSnapshotV1 struct {
	Format    string        `json:"format"`
	Version   uint64        `json:"version"`
	CreatedAt time.Time     `json:"createdAt"`
	Files     []*FileRecord `json:"files"`
}

type storedManifestV1 struct {
	Format    string    `json:"format"`
	DataFile  string    `json:"dataFile"`
	Version   uint64    `json:"version"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("store directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve store directory: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("create store directory: %w", err)
	}
	return &Store{dir: abs}, nil
}

func (s *Store) Save(ctx context.Context, snapshot *Snapshot) error {
	if s == nil || snapshot == nil {
		return errors.New("nil snapshot cannot be saved")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.saveLocked(ctx, snapshot)
}

func (s *Store) saveLocked(ctx context.Context, snapshot *Snapshot) (retErr error) {
	lock, err := lockStoreExclusive(s.dir)
	if err != nil {
		return err
	}
	defer func() {
		if unlockErr := lock.Close(); retErr == nil {
			retErr = unlockErr
		}
	}()

	payload := storedSnapshotV1{
		Format:    storeFormatV1,
		Version:   snapshot.Version,
		CreatedAt: snapshot.CreatedAt,
		Files:     snapshot.Files(),
	}
	data, err := json.MarshalIndent(&payload, "", "  ")
	if err != nil {
		return fmt.Errorf("encode index: %w", err)
	}
	data = append(data, '\n')

	dataName := fmt.Sprintf("index-%020d.json.tmp", snapshot.Version)
	dataPath := filepath.Join(s.dir, dataName)
	if err := writeFileAtomic(ctx, s.dir, dataName, data, 0o644); err != nil {
		return err
	}
	finalDataName := fmt.Sprintf("index-%020d.json", snapshot.Version)
	if err := os.Rename(dataPath, filepath.Join(s.dir, finalDataName)); err != nil {
		return fmt.Errorf("publish index data: %w", err)
	}
	if err := syncDirectory(s.dir); err != nil {
		return err
	}

	manifest := storedManifestV1{
		Format:    storeFormatV1,
		DataFile:  finalDataName,
		Version:   snapshot.Version,
		UpdatedAt: time.Now().UTC(),
	}
	manifestData, err := json.MarshalIndent(&manifest, "", "  ")
	if err != nil {
		return err
	}
	manifestData = append(manifestData, '\n')
	if err := writeFileAtomic(ctx, s.dir, storeManifestName, manifestData, 0o644); err != nil {
		return err
	}
	if err := syncDirectory(s.dir); err != nil {
		return err
	}
	s.removeUnusedDataLocked(finalDataName)
	return nil
}

func (s *Store) Load(ctx context.Context) (*Snapshot, error) {
	if s == nil {
		return nil, errors.New("nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.loadLocked(ctx)
}

func (s *Store) loadLocked(ctx context.Context) (*Snapshot, error) {
	lock, err := lockStoreShared(s.dir)
	if err != nil {
		return nil, err
	}
	defer lock.Close()

	manifestPath := filepath.Join(s.dir, storeManifestName)
	data, err := os.ReadFile(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read index manifest: %w", err)
	}
	var manifest storedManifestV1
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode index manifest: %w", err)
	}
	if manifest.Format != storeFormatV1 || filepath.Base(manifest.DataFile) != manifest.DataFile || manifest.DataFile == "" {
		return nil, errors.New("invalid index manifest")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	payloadData, err := os.ReadFile(filepath.Join(s.dir, manifest.DataFile))
	if err != nil {
		return nil, fmt.Errorf("read index data: %w", err)
	}
	var payload storedSnapshotV1
	if err := json.Unmarshal(payloadData, &payload); err != nil {
		return nil, fmt.Errorf("decode index data: %w", err)
	}
	if payload.Format != storeFormatV1 || payload.Version != manifest.Version {
		return nil, errors.New("index data does not match manifest")
	}
	return newSnapshot(Config{}, payload.Version, payload.Files, payload.CreatedAt), nil
}

func (s *Store) removeUnusedDataLocked(keep string) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == keep || name == storeManifestName || entry.IsDir() {
			continue
		}
		if hasIndexTempPrefix(name) || isIndexDataName(name) || isAtomicTempName(name) {
			_ = os.Remove(filepath.Join(s.dir, name))
		}
	}
	_ = syncDirectory(s.dir)
}

func isIndexDataName(name string) bool {
	base, ok := cutPrefix(name, "index-")
	return ok && len(base) > 5 && hasSuffix(base, ".json")
}

func hasIndexTempPrefix(name string) bool {
	base, ok := cutPrefix(name, "index-")
	return ok && hasSuffix(base, ".json.tmp")
}

func isAtomicTempName(name string) bool {
	return strings.HasPrefix(name, "."+storeManifestName+".tmp-") ||
		strings.HasPrefix(name, ".index-") && strings.Contains(name, ".tmp-")
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || s[:len(prefix)] != prefix {
		return s, false
	}
	return s[len(prefix):], true
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// writeFileAtomic creates, fsyncs and renames a file. The returned file is
// visible only after rename; stale *.tmp files are never named as a manifest.
func writeFileAtomic(ctx context.Context, dir, name string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(perm); err != nil {
		return fmt.Errorf("chmod %s: %w", name, err)
	}
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := tmp.Write(data)
		if err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
		data = data[n:]
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	cleanup = false
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("publish %s: %w", name, err)
	}
	return nil
}
