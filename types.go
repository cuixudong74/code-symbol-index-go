package symbolindex

import "time"

type SymbolKind string

const (
	KindFunction SymbolKind = "function"
	KindMethod   SymbolKind = "method"
	KindType     SymbolKind = "type"
	KindConstant SymbolKind = "constant"
	KindVariable SymbolKind = "variable"
)

type FileStatus string

const (
	StatusOK                 FileStatus = "ok"
	StatusSkippedTooLarge    FileStatus = "skipped_too_large"
	StatusSkippedInvalidUTF8 FileStatus = "skipped_invalid_utf8"
	StatusParseError         FileStatus = "parse_error"
	StatusReadError          FileStatus = "read_error"
)

// Symbol is a named source-level symbol with a file-relative location.
type Symbol struct {
	Name        string     `json:"name"`
	Kind        SymbolKind `json:"kind"`
	Path        string     `json:"path"`
	Line        int        `json:"line"`
	Column      int        `json:"column"`
	StartOffset int        `json:"startOffset"`
	EndOffset   int        `json:"endOffset"`
}

type FileRecord struct {
	Path       string     `json:"path"`
	Size       int64      `json:"size"`
	ModTime    time.Time  `json:"modTime"`
	Hash       string     `json:"hash,omitempty"`
	Status     FileStatus `json:"status"`
	ReasonCode string     `json:"reasonCode,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	Symbols    []Symbol   `json:"symbols,omitempty"`
}

type Query struct {
	Name   string
	Prefix string
	Kind   SymbolKind
	Path   string
	Offset int
	Limit  int
}

type SearchResult struct {
	Symbols []Symbol `json:"symbols"`
	Total   int      `json:"total"`
	Offset  int      `json:"offset"`
	Limit   int      `json:"limit"`
	HasMore bool     `json:"hasMore"`
}

type OutlineResult struct {
	Path    string     `json:"path"`
	Status  FileStatus `json:"status"`
	Reason  string     `json:"reason,omitempty"`
	Symbols []Symbol   `json:"symbols"`
}

type AffectedFiles struct {
	Added           []string `json:"added"`
	Deleted         []string `json:"deleted"`
	Modified        []string `json:"modified"`
	MetadataChanged []string `json:"metadataChanged"`
}

type Report struct {
	Version      uint64        `json:"version"`
	ScannedFiles int           `json:"scannedFiles"`
	IndexedFiles int           `json:"indexedFiles"`
	SkippedFiles int           `json:"skippedFiles"`
	FailureFiles int           `json:"failureFiles"`
	SymbolCount  int           `json:"symbolCount"`
	Duration     time.Duration `json:"duration"`
	Affected     AffectedFiles `json:"affected"`
	Errors       []FileError   `json:"errors,omitempty"`
}
