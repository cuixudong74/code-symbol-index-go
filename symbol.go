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

// FileResult is the per-file indexing outcome.
type FileResult struct {
	// Path is relative to the workspace root.
	Path string `json:"path"`
	// Status is ok, degraded or skipped.
	Status FileStatus `json:"status"`
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
