// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package depaware

import (
	"fmt"
	"io/ioutil"
	"log"
	"sort"

	"go/ast"
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

		pre := func(c *astutil.Cursor) bool {
			n := c.Node()
			//log.Printf("Node: %T", n)
			switch n := n.(type) {
			case *ast.FuncDecl:
				name := funcName(pkg, n)
				log.Printf("func %q comment = %p", name, n.Doc)
				switch name {
				case "AnotherUnused", "Bar":
					start, end := offsetRange(pkg.Fset, n)
					editBuf.Delete(start, end)

					// TODO: incr/decr a delete
					// count when on pre/post hook
					// and start deleting on entry
					// to unused, then
					// Cursor.Delete everything
					// inside (including comments
					// apparently) and then stop
					// deleting once isDeleting
					// drops back to zero?
					//
					// Because right now comments inside
					// deleted funcs get promoted to top-level.
					return false
				}
				log.Printf("Func: %v", name)
			}
			return true
		}
		astutil.Apply(f, pre, nil)
		fmt.Printf("// Source of %s:\n\n%s\n", fileName, editBuf.Bytes())
	}

	for _, p := range imports {
		w.walkPackage(p)
	}
}

func funcName(pkg *packages.Package, fd *ast.FuncDecl) string {
	if fd.Recv != nil {
		// TODO: methods
	}
	return fd.Name.Name
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
	}
	return offset(fset, startPos), offset(fset, endPos)
}
