package codeindex

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

// scanGo reads Go exactly with go/parser. A file that doesn't parse still
// yields the declarations before the error.
func scanGo(src []byte) Parsed {
	fset := token.NewFileSet()
	f, _ := parser.ParseFile(fset, "", src, parser.ParseComments|parser.SkipObjectResolution)
	if f == nil {
		return Parsed{}
	}
	var out Parsed
	line := func(p token.Pos) int { return fset.Position(p).Line }
	text := func(a, b token.Pos) string {
		i, j := fset.Position(a).Offset, fset.Position(b).Offset
		if i < 0 || j > len(src) || i > j {
			return ""
		}
		return string(src[i:j])
	}
	for _, im := range f.Imports {
		if s, err := strconv.Unquote(im.Path.Value); err == nil {
			out.Imports = append(out.Imports, s)
		}
	}
	add := func(s Symbol, doc *ast.CommentGroup) {
		if doc != nil {
			s.Doc = docLine(doc.Text())
			s.Start = min(s.Start, line(doc.Pos()))
		}
		out.Symbols = append(out.Symbols, s)
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			hdr := d.End()
			if d.Body != nil {
				hdr = d.Body.Lbrace
			}
			s := Symbol{Name: d.Name.Name, Kind: "func", Sig: collapse(text(d.Pos(), hdr), 200), Decl: line(d.Pos()), Start: line(d.Pos()), End: line(d.End()), Exported: d.Name.IsExported()}
			if d.Recv != nil && len(d.Recv.List) > 0 {
				s.Kind = "method"
				s.Parent = recvName(d.Recv.List[0].Type)
				s.Exported = s.Exported && isExportedName(s.Parent)
			}
			add(s, d.Doc)
		case *ast.GenDecl:
			if d.Tok == token.IMPORT {
				continue
			}
			for _, sp := range d.Specs {
				switch sp := sp.(type) {
				case *ast.TypeSpec:
					kind := "type"
					switch sp.Type.(type) {
					case *ast.StructType:
						kind = "struct"
					case *ast.InterfaceType:
						kind = "interface"
					}
					doc := sp.Doc
					if doc == nil && !d.Lparen.IsValid() {
						doc = d.Doc
					}
					start := sp.Pos()
					if !d.Lparen.IsValid() {
						start = d.Pos()
					}
					add(Symbol{Name: sp.Name.Name, Kind: kind, Sig: "type " + sp.Name.Name + " " + typeHead(sp.Type), Start: line(start), Decl: line(sp.Pos()), End: line(sp.End()), Exported: sp.Name.IsExported()}, doc)
					if st, ok := sp.Type.(*ast.StructType); ok && st.Fields != nil {
						for _, fl := range st.Fields.List {
							for _, n := range fl.Names {
								add(Symbol{Name: n.Name, Kind: "field", Parent: sp.Name.Name, Sig: collapse(n.Name+" "+text(fl.Type.Pos(), fl.Type.End()), 120), Decl: line(fl.Pos()), Start: line(fl.Pos()), End: line(fl.End()), Exported: n.IsExported()}, fl.Doc)
							}
						}
					}
				case *ast.ValueSpec:
					kind := strings.ToLower(d.Tok.String())
					doc := sp.Doc
					if doc == nil && !d.Lparen.IsValid() {
						doc = d.Doc
					}
					for _, n := range sp.Names {
						if n.Name == "_" {
							continue
						}
						add(Symbol{Name: n.Name, Kind: kind, Sig: collapse(kind+" "+text(sp.Pos(), sp.End()), 160), Decl: line(sp.Pos()), Start: line(sp.Pos()), End: line(sp.End()), Exported: n.IsExported()}, doc)
					}
				}
			}
		}
	}
	return out
}

func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvName(t.X)
	case *ast.IndexExpr:
		return recvName(t.X)
	case *ast.IndexListExpr:
		return recvName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

func typeHead(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StructType:
		return "struct"
	case *ast.InterfaceType:
		return "interface"
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return typeHead(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + typeHead(t.X)
	case *ast.ArrayType:
		return "[]" + typeHead(t.Elt)
	case *ast.MapType:
		return "map[" + typeHead(t.Key) + "]" + typeHead(t.Value)
	case *ast.FuncType:
		return "func"
	case *ast.ChanType:
		return "chan"
	}
	return ""
}
