package codetools

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
)

func outline(ctx context.Context, t *readTool, root string, in input) (tools.ToolOutput, error) {
	p, content, err := t.readBlob(ctx, root, in.Ref, in.Path)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	var items []string
	if strings.HasSuffix(p, ".go") {
		items = goOutline(content)
	}
	if items == nil {
		items = genericOutline(string(content))
	}
	if len(items) == 0 {
		return tools.ToolOutput{Text: p + ": no declarations found", Summary: "0 items"}, nil
	}
	more := 0
	if len(items) > maxOutlineItems {
		more = len(items) - maxOutlineItems
		items = items[:maxOutlineItems]
	}
	text := p + "\n" + strings.Join(items, "\n")
	if more > 0 {
		text += fmt.Sprintf("\n(%d more)", more)
	}
	return tools.ToolOutput{Text: text, Summary: fmt.Sprintf("%d items", len(items)+more)}, nil
}

// goOutline lists top-level declarations; nil when the file does not parse.
func goOutline(src []byte) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	items := []string{}
	for _, d := range f.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			name := decl.Name.Name
			if decl.Recv != nil && len(decl.Recv.List) > 0 {
				name = "(" + recvType(decl.Recv.List[0].Type) + ") " + name
			}
			items = append(items, fmt.Sprintf("%d: func %s", fset.Position(decl.Pos()).Line, name))
		case *ast.GenDecl:
			for _, s := range decl.Specs {
				switch spec := s.(type) {
				case *ast.TypeSpec:
					items = append(items, fmt.Sprintf("%d: type %s", fset.Position(spec.Pos()).Line, spec.Name.Name))
				case *ast.ValueSpec:
					for _, n := range spec.Names {
						items = append(items, fmt.Sprintf("%d: %s %s", fset.Position(n.Pos()).Line, decl.Tok, n.Name))
					}
				}
			}
		}
	}
	return items
}

func recvType(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return "*" + recvType(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return recvType(t.X)
	case *ast.IndexListExpr:
		return recvType(t.X)
	}
	return "?"
}

var declRe = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:pub(?:\([a-z]+\))?\s+)?(?:async\s+)?(?:abstract\s+)?(?:def|class|function|func|fn|interface|type|struct|enum|impl|trait|module|object)\s+[A-Za-z_$]`)

func genericOutline(src string) []string {
	var items []string
	for i, line := range strings.Split(src, "\n") {
		if declRe.MatchString(line) {
			items = append(items, fmt.Sprintf("%d: %s", i+1, clipRunes(strings.TrimSpace(line), 120)))
		}
	}
	return items
}
