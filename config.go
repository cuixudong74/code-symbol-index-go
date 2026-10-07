package index

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
)

// Default configuration values.
const (
	// DefaultMaxFileSize is the default per-file size limit (5 MiB).
	DefaultMaxFileSize int64 = 5 << 20
	// DefaultIndexDir is the directory (relative to the workspace root) that
	// holds the persisted index and is ignored by default.
	DefaultIndexDir = ".codeidx"
	// DefaultIndexFile is the default index file name inside DefaultIndexDir.
	DefaultIndexFile = "index.json"
	// CurrentPolicyVersion identifies the built-in indexing rules (binary
	// detection, encoding rules, ...). Bump it whenever a rule change could
	// alter a FileResult; an incremental update then re-evaluates every file
	// whose outcome the new rules could change, so its result matches a full
	// rebuild under the new binary.
	CurrentPolicyVersion = 2
)

// Config controls indexing behaviour.
type Config struct {
	// Root is the workspace root directory. It must be non-empty.
	Root string
	// Concurrency is the number of worker goroutines used while indexing.
	// It must be at least 1.
	Concurrency int
	// IgnorePatterns is a list of glob patterns. A file (or directory) is
	// ignored when any pattern matches. Patterns without a path separator are
	// matched against any path component (e.g. "vendor" ignores every vendor
	// directory); patterns with a separator are matched against the full
	// relative path (e.g. "*.gen.go" matches only at root).
	IgnorePatterns []string
	// MaxFileSize is the maximum file size in bytes that will be read and
	// parsed. Files larger than this are skipped with StatusSkipped. It must
	// be >= 0; 0 disables the limit.
	MaxFileSize int64
}

// DefaultConfig returns a configuration with sensible defaults for root.
func DefaultConfig(root string) Config {
	if root == "" {
		root = "."
	}
	return Config{
		Root:           root,
		Concurrency:    runtime.NumCPU(),
		IgnorePatterns: []string{".git", DefaultIndexDir},
		MaxFileSize:    DefaultMaxFileSize,
	}
}

// Validate checks the configuration and returns an error for out-of-range
// values.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Root) == "" {
		return errors.New("index: config: Root must not be empty")
	}
	if c.Concurrency < 1 {
		return errors.New("index: config: Concurrency must be >= 1")
	}
	if c.MaxFileSize < 0 {
		return errors.New("index: config: MaxFileSize must be >= 0")
	}
	return nil
}

// policySignature identifies everything that can affect a FileResult apart
// from file content: the built-in indexing rules (PolicyVersion) and the
// result-affecting config values. IgnorePatterns are not part of it because
// file presence alone already captures ignore changes (a newly ignored file
// becomes "removed", a newly un-ignored one becomes "added").
type policySignature struct {
	PolicyVersion int
	MaxFileSize   int64
}

// signature returns the policySignature produced by cfg.
func (c *Config) signature() policySignature {
	return policySignature{PolicyVersion: CurrentPolicyVersion, MaxFileSize: c.MaxFileSize}
}

// matchIgnore reports whether the relative path rel matches any of the
// ignore patterns.
func matchIgnore(rel string, patterns []string) bool {
	rel = filepath.ToSlash(rel)
	components := strings.Split(rel, "/")
	for _, pat := range patterns {
		pat = filepath.ToSlash(strings.TrimSpace(pat))
		if pat == "" {
			continue
		}
		if strings.Contains(pat, "/") {
			if ok, err := filepath.Match(pat, rel); err == nil && ok {
				return true
			}
			continue
		}
		for _, comp := range components {
			if ok, err := filepath.Match(pat, comp); err == nil && ok {
				return true
			}
		}
	}
	return false
}
