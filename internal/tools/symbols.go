package tools

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// SymbolsTool extracts structural code symbols without reading entire files.
type SymbolsTool struct {
	ws *Workspace
}

// NewSymbolsTool creates a SymbolsTool scoped to ws.
func NewSymbolsTool(ws *Workspace) *SymbolsTool {
	return &SymbolsTool{ws: ws}
}

type symbolsArgs struct {
	Path string `json:"path"`
	Kind string `json:"kind,omitempty"`
}

// Symbol represents an extracted code declaration.
type Symbol struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // func, method, type, const, var
	Receiver  string `json:"receiver,omitempty"`
	Signature string `json:"signature,omitempty"`
	File      string `json:"file"`
	Line      int    `json:"line"`
}

// Name implements Tool.
func (t *SymbolsTool) Name() string { return "symbols" }

// Description implements Tool.
func (t *SymbolsTool) Description() string {
	return "Extract structural code declarations (functions, methods, structs, interfaces, consts) " +
		"from Go source files or package directories with line numbers and signatures, saving context tokens."
}

// Schema implements Tool.
func (t *SymbolsTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to a Go source file or directory relative to project root.",
			},
			"kind": map[string]any{
				"type":        "string",
				"enum":        []string{"all", "func", "method", "type", "const", "var"},
				"description": "Filter for symbol kinds (default: all).",
			},
		},
		"required":             []string{"path"},
		"additionalProperties": false,
	}
}

// Permission implements Tool.
func (t *SymbolsTool) Permission(call Call) (permissions.Action, error) {
	var a symbolsArgs
	if err := call.Bind(&a); err != nil {
		return permissions.Action{}, err
	}
	return permissions.Action{
		Category: permissions.CatFilesystemRead,
		Risk:     permissions.RiskLow,
		Tool:     t.Name(),
		Summary:  fmt.Sprintf("Extract symbols from %s", a.Path),
	}, nil
}

// Execute performs symbol parsing using go/ast.
func (t *SymbolsTool) Execute(ctx context.Context, call Call) (Result, error) {
	var a symbolsArgs
	if err := call.Bind(&a); err != nil {
		return Errorf(call, "symbols: %v", err), nil
	}

	absPath, err := t.ws.Resolve(a.Path)
	if err != nil {
		return Errorf(call, "symbols: %v", err), nil
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return Errorf(call, "symbols: %v", err), nil
	}

	fset := token.NewFileSet()
	var symbols []Symbol

	if info.IsDir() {
		pkgs, err := parser.ParseDir(fset, absPath, nil, 0)
		if err != nil {
			return Errorf(call, "symbols: parsing directory %s: %v", a.Path, err), nil
		}
		for _, pkg := range pkgs {
			for fname, file := range pkg.Files {
				rel := t.ws.Rel(fname)
				symbols = append(symbols, t.extractFileSymbols(fset, file, rel)...)
			}
		}
	} else {
		if !strings.HasSuffix(absPath, ".go") {
			return Errorf(call, "symbols: only Go files (.go) are supported currently"), nil
		}
		file, err := parser.ParseFile(fset, absPath, nil, 0)
		if err != nil {
			return Errorf(call, "symbols: parsing file %s: %v", a.Path, err), nil
		}
		symbols = t.extractFileSymbols(fset, file, t.ws.Rel(absPath))
	}

	kindFilter := strings.ToLower(strings.TrimSpace(a.Kind))
	if kindFilter != "" && kindFilter != "all" {
		filtered := make([]Symbol, 0, len(symbols))
		for _, sym := range symbols {
			if sym.Kind == kindFilter {
				filtered = append(filtered, sym)
			}
		}
		symbols = filtered
	}

	if len(symbols) == 0 {
		return Result{
			CallID:  call.ID,
			Tool:    t.Name(),
			Content: fmt.Sprintf("No symbols found in %s", a.Path),
			Display: "0 symbols",
		}, nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Symbols in %s (%d found):\n", a.Path, len(symbols))
	for _, sym := range symbols {
		loc := fmt.Sprintf("%s:%d", sym.File, sym.Line)
		switch sym.Kind {
		case "method":
			fmt.Fprintf(&sb, "  [%-6s] ( %s ) %s%s  (%s)\n", sym.Kind, sym.Receiver, sym.Name, sym.Signature, loc)
		case "func":
			fmt.Fprintf(&sb, "  [%-6s] %s%s  (%s)\n", sym.Kind, sym.Name, sym.Signature, loc)
		case "type":
			fmt.Fprintf(&sb, "  [%-6s] %s %s  (%s)\n", sym.Kind, sym.Name, sym.Signature, loc)
		default:
			fmt.Fprintf(&sb, "  [%-6s] %s  (%s)\n", sym.Kind, sym.Name, loc)
		}
	}

	return Result{
		CallID:  call.ID,
		Tool:    t.Name(),
		Content: strings.TrimSpace(sb.String()),
		Display: fmt.Sprintf("%d symbols", len(symbols)),
	}, nil
}

func (t *SymbolsTool) extractFileSymbols(fset *token.FileSet, file *ast.File, relPath string) []Symbol {
	var symbols []Symbol

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			pos := fset.Position(d.Pos())
			kind := "func"
			receiver := ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = "method"
				receiver = formatNode(d.Recv.List[0].Type)
			}
			sig := formatFuncType(d.Type)
			symbols = append(symbols, Symbol{
				Name:      d.Name.Name,
				Kind:      kind,
				Receiver:  receiver,
				Signature: sig,
				File:      relPath,
				Line:      pos.Line,
			})

		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					typePos := fset.Position(s.Pos())
					typeKind := "type"
					typeSig := formatNode(s.Type)
					symbols = append(symbols, Symbol{
						Name:      s.Name.Name,
						Kind:      typeKind,
						Signature: typeSig,
						File:      relPath,
						Line:      typePos.Line,
					})
				case *ast.ValueSpec:
					valPos := fset.Position(s.Pos())
					kind := "var"
					if d.Tok == token.CONST {
						kind = "const"
					}
					for _, name := range s.Names {
						symbols = append(symbols, Symbol{
							Name: name.Name,
							Kind: kind,
							File: relPath,
							Line: valPos.Line,
						})
					}
				}
			}
		}
	}

	return symbols
}

func formatFuncType(ft *ast.FuncType) string {
	var params []string
	if ft.Params != nil {
		for _, f := range ft.Params.List {
			typeStr := formatNode(f.Type)
			if len(f.Names) > 0 {
				for _, n := range f.Names {
					params = append(params, fmt.Sprintf("%s %s", n.Name, typeStr))
				}
			} else {
				params = append(params, typeStr)
			}
		}
	}
	results := ""
	if ft.Results != nil {
		var res []string
		for _, f := range ft.Results.List {
			res = append(res, formatNode(f.Type))
		}
		if len(res) == 1 {
			results = " " + res[0]
		} else if len(res) > 1 {
			results = fmt.Sprintf(" (%s)", strings.Join(res, ", "))
		}
	}
	return fmt.Sprintf("(%s)%s", strings.Join(params, ", "), results)
}

func formatNode(node ast.Node) string {
	if node == nil {
		return ""
	}
	switch n := node.(type) {
	case *ast.Ident:
		return n.Name
	case *ast.StarExpr:
		return "*" + formatNode(n.X)
	case *ast.SelectorExpr:
		return formatNode(n.X) + "." + n.Sel.Name
	case *ast.ArrayType:
		return "[]" + formatNode(n.Elt)
	case *ast.InterfaceType:
		return "interface{...}"
	case *ast.StructType:
		return "struct{...}"
	case *ast.MapType:
		return fmt.Sprintf("map[%s]%s", formatNode(n.Key), formatNode(n.Value))
	case *ast.Ellipsis:
		return "..." + formatNode(n.Elt)
	default:
		return fmt.Sprintf("%T", n)
	}
}
