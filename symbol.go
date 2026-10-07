// Package index implements a workspace source-code symbol index with
// incremental updates, lock-free concurrent reads, atomic persistence and
// fault-tolerant parsing.
package index

// Kind is the kind of a symbol.
type Kind string

// Supported symbol kinds.
const (
	KindPackage  Kind = "package"
	KindFunction Kind = "function"
	KindMethod   Kind = "method"
	KindType     Kind = "type"
	KindVariable Kind = "variable"
	KindConstant Kind = "constant"
	KindField    Kind = "field"
)

// AllKinds returns every supported kind in a stable order.
func AllKinds() []Kind {
	return []Kind{
		KindPackage,
		KindFunction,
		KindMethod,
		KindType,
		KindVariable,
		KindConstant,
		KindField,
	}
}

// ValidKind reports whether k is a known symbol kind.
func ValidKind(k Kind) bool {
	switch k {
	case KindPackage, KindFunction, KindMethod, KindType, KindVariable, KindConstant, KindField:
		return true
	}
	return false
}

// Symbol is a named, located code symbol.
type Symbol struct {
	// File is the path of the containing file, relative to the workspace root.
	File string `json:"file"`
	// Name is the symbol name.
	Name string `json:"name"`
	// Kind is the symbol kind.
	Kind Kind `json:"kind"`
	// Line is the 1-based line number.
	Line int `json:"line"`
	// Column is the 1-based column number.
	Column int `json:"column"`
	// Receiver is the receiver type name for methods, empty otherwise.
	Receiver string `json:"receiver,omitempty"`
}

// FileStatus describes how a file was handled during indexing.
type FileStatus string

const (
	// StatusOK means the file was parsed successfully.
	StatusOK FileStatus = "ok"
	// StatusDegraded means the file was parsed but contained syntax errors;
	// partial symbols were extracted and Reason describes the error.
	StatusDegraded FileStatus = "degraded"
	// StatusSkipped means the file was not indexed (oversized, bad encoding,
	// read error, ...). Reason distinguishes the cause.
	StatusSkipped FileStatus = "skipped"
)

// ReasonCode is a stable, machine-readable classification of why a path was
// skipped or a file was parsed in degraded mode. Human-readable details stay in
// FileResult.Reason / SkippedPath.Reason; code should switch on ReasonCode.
type ReasonCode string

const (
	// ReasonNone is set when there is nothing to explain (status ok).
	ReasonNone ReasonCode = ""
	// ReasonParseError: the file was parsed but contained syntax errors;
	// partial symbols were still extracted (status degraded).
	ReasonParseError ReasonCode = "parse_error"
	// ReasonTooLarge: the file is larger than Config.MaxFileSize and was not
	// read (status skipped).
	ReasonTooLarge ReasonCode = "too_large"
	// ReasonBinary: the file contains binary (NUL) bytes and is not treated as
	// source code (status skipped).
	ReasonBinary ReasonCode = "binary"
	// ReasonInvalidEncoding: the file is not valid UTF-8 (status skipped).
	ReasonInvalidEncoding ReasonCode = "invalid_encoding"
	// ReasonAccessDenied: stat/open/read was denied (permission error) or
	// otherwise failed with an I/O error (status skipped / walk-time skip).
	ReasonAccessDenied ReasonCode = "access_denied"
)

// SkippedPath records a path that could not be indexed during a walk or
// indexing run — typically an unreadable file or directory. Entries are
// keyed by path on the snapshot, carry a stable ReasonCode and preserve the
// underlying error message so the exact cause is locatable.
type SkippedPath struct {
	// Path is relative to the workspace root when possible, otherwise
	// absolute.
	Path string `json:"path"`
	// IsDir reports whether the path is a directory whose contents could not
	// be traversed (every file below it is consequently absent).
	IsDir bool `json:"is_dir,omitempty"`
	// ReasonCode is the stable classification of the cause.
	ReasonCode ReasonCode `json:"reason_code"`
	// Reason is the human-readable cause, including the underlying error.
	Reason string `json:"reason"`
}

// FileResult is the per-file indexing outcome.
type FileResult struct {
	// Path is relative to the workspace root.
	Path string `json:"path"`
	// Status is ok, degraded or skipped.
	Status FileStatus `json:"status"`
	// ReasonCode is the stable, machine-readable cause for non-ok outcomes.
	ReasonCode ReasonCode `json:"reason_code,omitempty"`
	// Reason explains degraded/skipped outcomes and is empty for ok files.
	Reason string `json:"reason,omitempty"`
	// Fingerprint is used for incremental change detection. It is a content
	// hash for files that could be read, and a size/mtime based token for
	// files that were skipped without being read (e.g. oversized files).
	Fingerprint string `json:"fingerprint"`
	// Size is the file size in bytes at index time.
	Size int64 `json:"size"`
	// ModTime is the modification time (Unix nanoseconds) at index time.
	ModTime int64 `json:"mod_time"`
	// Symbols are the extracted symbols, in source order.
	Symbols []Symbol `json:"symbols"`
}
