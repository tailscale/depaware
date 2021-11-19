// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package depaware

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"sort"
	"strings"

	"go/ast"
	"go/printer"
	"go/token"

	"github.com/tailscale/depaware/internal/edit"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/packages"
)

var cfg = &packages.Config{
	Mode: (0 |
		packages.NeedName |
		packages.NeedFiles |
		packages.NeedCompiledGoFiles |
		packages.NeedImports |
		packages.NeedDeps |
		packages.NeedModule |
		packages.NeedTypes |
		packages.NeedSyntax |
		0),
}

func dumpSource(pkgMain string) {
	var w walker
	w.walk(pkgMain)
}

type walker struct {
	done map[string]bool
}

func (w *walker) walk(mainPkg string) {
	pkgs, err := packages.Load(cfg, mainPkg)
	if err != nil {
		log.Fatalf("packages.Load: %v", err)
	}
	for _, pkg := range pkgs {
		w.walkPackage(pkg)
	}
}

func (w *walker) walkPackage(pkg *packages.Package) {
	if w.done[pkg.PkgPath] {
		return
	}
	if w.done == nil {
		w.done = map[string]bool{}
	}
	w.done[pkg.PkgPath] = true

	fmt.Printf("\n### PACKAGE %v\n", pkg.PkgPath)

	if len(pkg.Errors) > 0 {
		log.Fatalf("errors reading %q: %q", pkg.PkgPath, pkg.Errors)
	}

	var imports []*packages.Package
	for _, p := range pkg.Imports {
		imports = append(imports, p)
	}
	sort.Slice(imports, func(i, j int) bool {
		return imports[i].PkgPath < imports[j].PkgPath
	})
	for _, f := range pkg.GoFiles {
		fmt.Printf("file.go %q\n", f)
	}
	for _, f := range pkg.OtherFiles {
		fmt.Printf("file.other %q\n", f)
	}
	for _, p := range imports {
		fmt.Printf("import %q => %q\n", pkg.PkgPath, p.PkgPath)
	}
	fmt.Printf("Fset: %p\n", pkg.Fset)
	fmt.Printf("Syntax: %v\n", len(pkg.Syntax))
	fmt.Printf("Modules: %+v\n", pkg.Module)

	for i, f := range pkg.Syntax {
		fileName := pkg.GoFiles[i]

		src, err := ioutil.ReadFile(fileName)
		if err != nil {
			log.Fatal(err)
		}
		editBuf := edit.NewBuffer(src)

		depth := 0
		post := func(c *astutil.Cursor) bool {
			depth--
			return true
		}
		pre := func(c *astutil.Cursor) bool {
			depth++
			n := c.Node()
			indent := strings.Repeat("  ", depth)
			log.Printf("%sNode: %T", indent, n)
			switch n := n.(type) {
			case *ast.GenDecl:
				genDecl := n

				var dels []delRange
				for _, spec := range n.Specs {
					switch spec := spec.(type) {
					case *ast.ImportSpec:
						// Nothing (yet?)
					case *ast.TypeSpec:
						name := typeName(pkg, spec)
						log.Printf("%stype %q", indent, name)
						switch name {
						case "main.UnusedType", "main.UnusedFactoredType":
							start, end := offsetRange(pkg.Fset, spec)
							dels = append(dels, delRange{start, end})
						}
					case *ast.ValueSpec:
						// Consts and vars.
					}
				}
				if len(dels) == len(n.Specs) {
					// Delete the whole genspec.
					start, end := offsetRange(pkg.Fset, genDecl)
					editBuf.Delete(start, end)
				} else {
					for _, del := range dels {
						editBuf.Delete(del.start, del.end)
					}
				}
			case *ast.FuncDecl:
				name := funcName(pkg, n)
				log.Printf("%sfunc %q comment = %p", indent, name, n.Doc)
				switch name {
				case "main.AnotherUnused", "main.Bar":
					start, end := offsetRange(pkg.Fset, n)
					editBuf.Delete(start, end)
					return false
				}
				log.Printf("%sFunc: %v", indent, name)
			}
			return true
		}
		astutil.Apply(f, pre, post)
		fmt.Printf("// Source of %s:\n\n%s\n", fileName, editBuf.Bytes())
	}

	for _, p := range imports {
		w.walkPackage(p)
	}
}

func pkgSym(pkg *packages.Package) string {
	if pkg.Name == "main" {
		return "main"
	}
	return pkg.PkgPath
}

func typeName(pkg *packages.Package, ts *ast.TypeSpec) string {
	return pkgSym(pkg) + "." + ts.Name.Name
}

func funcName(pkg *packages.Package, fd *ast.FuncDecl) string {
	pkgName := pkgSym(pkg)
	if fd.Recv != nil {
		var buf bytes.Buffer
		buf.WriteByte('(')
		typ := fd.Recv.List[0].Type
		printer.Fprint(&buf, pkg.Fset, typ)
		buf.WriteByte(')')
		typPart := buf.Bytes()
		if typPart[1] != '*' {
			typPart = typPart[1 : len(typPart)-2]
		}
		return fmt.Sprintf("%s.%s.%s", pkgName, typPart, fd.Name.Name)
	}
	return pkgName + "." + fd.Name.Name
}

func offset(fset *token.FileSet, pos token.Pos) int {
	return fset.PositionFor(pos, false).Offset
}

func offsetRange(fset *token.FileSet, n ast.Node) (start, end int) {
	startPos, endPos := n.Pos(), n.End()
	switch n := n.(type) {
	case *ast.FuncDecl:
		if n.Doc != nil {
			startPos = n.Doc.Pos()
		}
	case *ast.TypeSpec:
		if n.Doc != nil {
			startPos = n.Doc.Pos()
		}
	case *ast.ValueSpec:
		if n.Doc != nil {
			startPos = n.Doc.Pos()
		}
	case *ast.GenDecl:
		if n.Doc != nil {
			startPos = n.Doc.Pos()
		}
	default:
		panic(fmt.Sprintf("unhandled type %T", n))
	}
	return offset(fset, startPos), offset(fset, endPos)
}

type delRange struct{ start, end int }
