package index

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// indexFile reads and parses a single file and returns its FileResult.
// It never panics or returns an error: all failure modes are reported through
// the returned FileResult so that a single bad file can never fail a whole
// indexing run.
func indexFile(rel, abs string, maxFileSize int64) *FileResult {
	fr := &FileResult{Path: rel, Status: StatusOK}

	info, err := os.Stat(abs)
	if err != nil {
		return skip(fr, ReasonAccessDenied, "stat error: %v", err)
	}
	fr.Size = info.Size()
	fr.ModTime = info.ModTime().UnixNano()

	if maxFileSize > 0 && info.Size() > maxFileSize {
		fr.Status = StatusSkipped
		fr.ReasonCode = ReasonTooLarge
		fr.Reason = fmt.Sprintf("file too large: %d bytes exceeds limit %d", info.Size(), maxFileSize)
		fr.Fingerprint = statFingerprint(info.Size(), info.ModTime().UnixNano())
		return fr
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return skip(fr, ReasonAccessDenied, "read error: %v", err)
	}
	fr.Fingerprint = hashBytes(data)

	// Binary files (compiled artifacts, images, object files, ...) are never
	// source code. The check runs before UTF-8 validation: a binary that is
	// also non-UTF-8 is reported as binary, the more specific and actionable
	// cause. A single NUL byte anywhere is definitive for text sources.
	if isBinaryData(data) {
		fr.Status = StatusSkipped
		fr.ReasonCode = ReasonBinary
		fr.Reason = "binary file: NUL byte detected; not a text source file"
		return fr
	}
	if !utf8.Valid(data) {
		fr.Status = StatusSkipped
		fr.ReasonCode = ReasonInvalidEncoding
		fr.Reason = "invalid UTF-8 encoding"
		return fr
	}

	fset := token.NewFileSet()
	file, perr := parser.ParseFile(fset, rel, data, parser.ParseComments)
	if perr != nil {
		fr.Status = StatusDegraded
		fr.ReasonCode = ReasonParseError
		fr.Reason = fmt.Sprintf("parse error: %v", perr)
	}
	if file == nil {
		return fr
	}
	fr.Symbols = extractSymbols(fset, file)
	return fr
}

// isBinaryData reports whether data contains a NUL byte, an unambiguous sign
// that the content is binary rather than UTF-8 text.
func isBinaryData(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0
}

// classifyAccessError maps an I/O error encountered while reading a path to
// the stable access reason code. Permission errors and every other
// unreadable-path I/O error map to access_denied: from the indexer's point of
// view the path simply cannot be read, while the wrapped message preserves
// the precise cause (permission denied, no such file, I/O error, ...).
func classifyAccessError(err error) ReasonCode {
	if err == nil {
		return ReasonNone
	}
	if errors.Is(err, fs.ErrPermission) {
		return ReasonAccessDenied
	}
	return ReasonAccessDenied
}

func skip(fr *FileResult, code ReasonCode, format string, args ...any) *FileResult {
	fr.Status = StatusSkipped
	fr.ReasonCode = code
	fr.Reason = fmt.Sprintf(format, args...)
	return fr
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func statFingerprint(size, modTime int64) string {
	return fmt.Sprintf("stat:%d:%d", size, modTime)
}

// extractSymbols collects symbols from a parsed Go AST in source order.
func extractSymbols(fset *token.FileSet, f *ast.File) []Symbol {
	var syms []Symbol

	add := func(name string, kind Kind, pos token.Pos, receiver string) {
		p := fset.Position(pos)
		syms = append(syms, Symbol{
			Name:     name,
			Kind:     kind,
			Line:     p.Line,
			Column:   p.Column,
			Receiver: receiver,
		})
	}

	if f.Name != nil {
		add(f.Name.Name, KindPackage, f.Package, "")
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			kind := KindFunction
			receiver := ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = KindMethod
				receiver = exprName(d.Recv.List[0].Type)
			}
			add(d.Name.Name, kind, d.Pos(), receiver)

		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					add(s.Name.Name, KindType, s.Pos(), "")
					if st, ok := s.Type.(*ast.StructType); ok && st.Fields != nil {
						for _, fld := range st.Fields.List {
							if len(fld.Names) == 0 {
								add(exprName(fld.Type), KindField, fld.Pos(), "")
								continue
							}
							for _, n := range fld.Names {
								add(n.Name, KindField, n.Pos(), "")
							}
						}
					}
					if it, ok := s.Type.(*ast.InterfaceType); ok && it.Methods != nil {
						for _, m := range it.Methods.List {
							if len(m.Names) == 0 {
								add(exprName(m.Type), KindMethod, m.Pos(), "")
								continue
							}
							for _, n := range m.Names {
								add(n.Name, KindMethod, m.Pos(), "")
							}
						}
					}
				case *ast.ValueSpec:
					kind := KindVariable
					if d.Tok == token.CONST {
						kind = KindConstant
					}
					for _, n := range s.Names {
						add(n.Name, kind, n.Pos(), "")
					}
				}
			}
		}
	}
	return syms
}

// exprName renders a type expression as a name (for receivers, embedded
// fields/interfaces).
func exprName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return exprName(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.IndexExpr:
		return exprName(t.X)
	case *ast.IndexListExpr:
		return exprName(t.X)
	case *ast.ParenExpr:
		return exprName(t.X)
	}
	return ""
}

// absPath joins root and rel.
func absPath(root, rel string) string {
	return filepath.Join(root, filepath.FromSlash(rel))
}
