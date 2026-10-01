package symbolindex

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"unicode/utf8"
)

// ExtractSymbols parses one Go source file and returns package-level symbols.
//
// The result is deterministic: declarations are traversed in source order and
// bulk const/var/type declarations contribute value names in declaration order.
// A file with a syntax error is degraded to StatusParseError while still
// exposing declarations recovered by the parser; it never fails a complete
// workspace update.
func ExtractSymbols(path string, content []byte) ([]Symbol, FileStatus, string, string) {
	if !utf8.Valid(content) {
		return nil, StatusSkippedInvalidUTF8, "invalid_utf8", "file is not valid UTF-8"
	}
	fset := token.NewFileSet()
	file, parseErr := parser.ParseFile(fset, path, content, parser.AllErrors|parser.DeclarationErrors|parser.SkipObjectResolution)
	if file == nil {
		if parseErr == nil {
			parseErr = errors.New("parser returned no syntax tree")
		}
		return nil, StatusParseError, "syntax_error", parseErr.Error()
	}
	if parseErr != nil {
		// ParseFile commonly returns a partial AST for local syntax errors.
		// Keep the recoverable declarations, but expose the deterministic
		// degraded status on the containing file.
		symbols := collectFileSymbols(fset, path, file)
		return symbols, StatusParseError, "syntax_error", parseErr.Error()
	}
	symbols := collectFileSymbols(fset, path, file)
	return symbols, StatusOK, "", ""
}

func collectFileSymbols(fset *token.FileSet, path string, file *ast.File) []Symbol {
	var symbols []Symbol
	makeSymbol := func(ident *ast.Ident, kind SymbolKind) Symbol {
		start := fset.Position(ident.Pos())
		end := fset.Position(ident.End())
		return Symbol{
			Name:        ident.Name,
			Kind:        kind,
			Path:        path,
			Line:        start.Line,
			Column:      start.Column,
			StartOffset: start.Offset,
			EndOffset:   end.Offset,
		}
	}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			kind := KindFunction
			if d.Recv != nil {
				kind = KindMethod
			}
			if d.Name != nil {
				symbols = append(symbols, makeSymbol(d.Name, kind))
			}
		case *ast.GenDecl:
			var kind SymbolKind
			switch d.Tok {
			case token.TYPE:
				kind = KindType
			case token.CONST:
				kind = KindConstant
			case token.VAR:
				kind = KindVariable
			default:
				continue
			}
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name != nil {
						symbols = append(symbols, makeSymbol(s.Name, kind))
					}
				case *ast.ValueSpec:
					for _, name := range s.Names {
						if name != nil {
							symbols = append(symbols, makeSymbol(name, kind))
						}
					}
				}
			}
		}
	}
	return symbols
}
