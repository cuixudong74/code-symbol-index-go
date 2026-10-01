package symbolindex

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const (
	defaultMaxFileSize   int64 = 10 << 20
	defaultPageSize            = 100
	defaultMaxQueryLimit       = 1000
)

// Config controls scanning, parsing and query boundaries.
//
// Zero-valued numeric fields receive documented defaults when Normalize is
// called. MaxFileSize < 0 disables the size limit.
type Config struct {
	// Root is the workspace root. It must exist when an update is run.
	Root string

	// Workers bounds concurrent hashing, reading and parsing. Values <= 0
	// use runtime.NumCPU().
	Workers int

	// MaxFileSize skips files larger than the limit. Zero uses 10 MiB;
	// a negative value disables the limit.
	MaxFileSize int64

	// IgnorePatterns are workspace-rooted gitignore-like rules. A pattern
	// without a slash matches a path component at any depth. A pattern with
	// a slash is anchored at the workspace root.
	IgnorePatterns []string

	// DefaultPageSize is used when Query.Limit <= 0. Values <= 0 use 100.
	DefaultPageSize int

	// MaxQueryLimit caps positive Query.Limit values. Values <= 0 use 1000.
	MaxQueryLimit int
}

// DefaultConfig returns a configuration with documented defaults.
func DefaultConfig(root string) Config {
	return Config{
		Root:            root,
		Workers:         runtime.NumCPU(),
		MaxFileSize:     defaultMaxFileSize,
		DefaultPageSize: defaultPageSize,
		MaxQueryLimit:   defaultMaxQueryLimit,
	}
}

// Normalize validates configuration and fills defaults.
func (c Config) Normalize() (Config, error) {
	out := c
	root := strings.TrimSpace(out.Root)
	if root == "" {
		return out, errors.New("workspace root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return out, fmt.Errorf("resolve workspace root: %w", err)
	}
	out.Root = filepath.Clean(abs)

	if out.Workers <= 0 {
		out.Workers = runtime.NumCPU()
		if out.Workers < 1 {
			out.Workers = 1
		}
	}
	if out.MaxFileSize == 0 {
		out.MaxFileSize = defaultMaxFileSize
	}
	if out.DefaultPageSize <= 0 {
		out.DefaultPageSize = defaultPageSize
	}
	if out.MaxQueryLimit <= 0 {
		out.MaxQueryLimit = defaultMaxQueryLimit
	}
	if out.MaxQueryLimit < out.DefaultPageSize {
		return out, fmt.Errorf("max query limit (%d) must be >= default page size (%d)", out.MaxQueryLimit, out.DefaultPageSize)
	}
	if _, err := newIgnoreMatcher(out.IgnorePatterns); err != nil {
		return out, err
	}
	return out, nil
}

const slashGlobPlaceholder = "\x00SLASH_GLOB\x00"

type ignoreRule struct {
	pattern string
	allow   bool
	dirOnly bool
	regexp  *regexp.Regexp
}

type ignoreMatcher struct {
	rules []ignoreRule
}

func newIgnoreMatcher(patterns []string) (*ignoreMatcher, error) {
	m := &ignoreMatcher{}
	for _, raw := range patterns {
		p := strings.TrimSpace(raw)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		rule := ignoreRule{pattern: p}
		if strings.HasPrefix(p, "!") {
			rule.allow = true
			p = p[1:]
		}
		if p == "" {
			return nil, fmt.Errorf("invalid ignore pattern %q", raw)
		}
		if strings.HasSuffix(p, "/") {
			rule.dirOnly = true
			p = strings.TrimSuffix(p, "/")
		}
		anchored := strings.Contains(p, "/") && !strings.HasPrefix(p, "**/")
		p = strings.TrimPrefix(p, "/")
		// A leading "**/" may match zero directories. Interior occurrences can
		// span zero or more directory components between two slashes.
		for strings.HasPrefix(p, "**/") {
			p = p[3:]
		}
		for {
			next := strings.Replace(p, "/**/", slashGlobPlaceholder, 1)
			if next == p {
				break
			}
			p = next
		}
		if p == "" {
			return nil, fmt.Errorf("invalid ignore pattern %q", raw)
		}
		expr, err := ignorePatternRegexp(p, anchored)
		if err != nil {
			return nil, fmt.Errorf("invalid ignore pattern %q: %w", raw, err)
		}
		rule.regexp = expr
		m.rules = append(m.rules, rule)
	}
	return m, nil
}

func ignorePatternRegexp(p string, anchored bool) (*regexp.Regexp, error) {
	var b strings.Builder
	if !anchored {
		b.WriteString(`(?:^|/)`)
	} else {
		b.WriteString(`^`)
	}
	for i := 0; i < len(p); i++ {
		switch ch := p[i]; ch {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				// "**" crosses directory boundaries.
				b.WriteString(`.*`)
				i++
				// Consume an optional slash after a leading "**/".
				if i+1 < len(p) && p[i+1] == '/' {
					i++
				}
			} else {
				b.WriteString(`[^/]*`)
			}
		case '?':
			b.WriteString(`[^/]`)
		case '[':
			end := strings.IndexByte(p[i+1:], ']')
			if end < 0 {
				return nil, errors.New("unterminated character class")
			}
			end += i + 1
			class := p[i+1 : end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[")
			b.WriteString(class)
			b.WriteString("]")
			i = end
		case '\\', '.', '+', '(', ')', '|', '{', '}', '^', '$':
			b.WriteByte('\\')
			b.WriteByte(ch)
		default:
			b.WriteByte(ch)
		}
	}
	b.WriteString(`$`)
	exprText := strings.ReplaceAll(b.String(), slashGlobPlaceholder, `(?:/|/.*/)`)
	return regexp.Compile(exprText)
}

// ignores returns true if the ordered rules ultimately exclude cleanPath.
// Directory-only rules never match files. If a directory is ignored during
// traversal, its descendants are pruned before they can be re-included.
func (m *ignoreMatcher) ignores(cleanPath string, isDir bool) bool {
	if m == nil || cleanPath == "" || cleanPath == "." {
		return false
	}
	rel := path.Clean(filepath.ToSlash(cleanPath))
	ignored := false
	for _, rule := range m.rules {
		if rule.dirOnly && !isDir {
			continue
		}
		if rule.regexp.MatchString(rel) {
			ignored = !rule.allow
		}
	}
	return ignored
}

// Ensure path is referenced even on platforms where filepath behavior differs
// in future helper additions.
var _ = path.Clean
