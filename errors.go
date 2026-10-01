package symbolindex

import "fmt"

// StringError is a stable, JSON-friendly error value used in reports.
type StringError string

func (e StringError) Error() string { return string(e) }

// FileError locates a non-fatal or fatal failure at a concrete workspace file.
type FileError struct {
	Path    string `json:"path"`
	Op      string `json:"op"`
	Message string `json:"message,omitempty"`
	Err     error  `json:"-"`
}

func (e *FileError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return fmt.Sprintf("%s: %s", e.Op, e.Path)
	}
	return fmt.Sprintf("%s %s: %v", e.Op, e.Path, e.Err)
}

func (e *FileError) Unwrap() error { return e.Err }
