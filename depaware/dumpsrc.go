// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// TODO: interface types are only used if their itab is used
// TODO: other types are used if ... something? errorsString. pcln table?
// TODO: go:linkname

package depaware

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"go/ast"
	"go/format"
	"go/printer"
	"go/token"

	"github.com/tailscale/depaware/internal/edit"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/txtar"
)

func dumpSource(pkgMain string) (*txtar.Archive, error) {
	var w walker
	return w.walk(pkgMain)
}

type walker struct {
	done    map[string]bool
	symLive map[string]bool
	ar      *txtar.Archive
	modPath string // only include packages from this module
	modDir  string // module root dir, for computing relative paths
}

func (w *walker) dead(sym string) bool {
	return !w.symLive[sym]
}

func (w *walker) walk(mainPkg string) (*txtar.Archive, error) {
	buildErrc := make(chan error, 1)
	go func() {
		var err error
		w.symLive, err = buildGenLive(mainPkg)
		buildErrc <- err
	}()

	cfg := &packages.Config{
		Mode: (packages.NeedName |
			packages.NeedFiles |
			packages.NeedCompiledGoFiles |
			packages.NeedImports |
			packages.NeedDeps |
			packages.NeedModule |
			packages.NeedTypes |
			packages.NeedSyntax),
	}
	pkgs, err := packages.Load(cfg, mainPkg)
	if err != nil {
		return nil, err
	}

	if err := <-buildErrc; err != nil {
		return nil, fmt.Errorf("building: %v", err)
	}

	w.ar = &txtar.Archive{}
	if len(pkgs) > 0 && pkgs[0].Module != nil {
		w.modPath = pkgs[0].Module.Path
		w.modDir = pkgs[0].Module.Dir
	}

	for _, pkg := range pkgs {
		if err := w.walkPackage(pkg); err != nil {
			return nil, err
		}
	}
	return w.ar, nil
}

func (w *walker) walkPackage(pkg *packages.Package) error {
	if w.done[pkg.PkgPath] {
		return nil
	}
	if w.done == nil {
		w.done = map[string]bool{}
	}
	w.done[pkg.PkgPath] = true

	// Skip packages outside the target module (e.g. stdlib).
	if w.modPath != "" && (pkg.Module == nil || pkg.Module.Path != w.modPath) {
		return nil
	}

	if len(pkg.Errors) > 0 {
		return fmt.Errorf("errors reading %q: %q", pkg.PkgPath, pkg.Errors)
	}

	for i, f := range pkg.Syntax {
		fileName := pkg.GoFiles[i]

		src, err := os.ReadFile(fileName)
		if err != nil {
			return err
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
						if w.dead(name) {
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
				if w.dead(name) {
					start, end := offsetRange(pkg.Fset, n)
					editBuf.Delete(start, end)
					return false
				}
			}
			return true
		}
		astutil.Apply(f, pre, post)

		src = editBuf.Bytes()
		if fmtSrc, err := format.Source(src); err == nil {
			src = fmtSrc
		}

		relPath := fileName
		if w.modDir != "" {
			if rel, err := filepath.Rel(w.modDir, fileName); err == nil {
				relPath = rel
			}
		}
		w.ar.Files = append(w.ar.Files, txtar.File{
			Name: relPath,
			Data: src,
		})
	}

	var imports []*packages.Package
	for _, p := range pkg.Imports {
		imports = append(imports, p)
	}
	sort.Slice(imports, func(i, j int) bool {
		return imports[i].PkgPath < imports[j].PkgPath
	})
	for _, p := range imports {
		if err := w.walkPackage(p); err != nil {
			return err
		}
	}
	return nil
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
			typPart = typPart[1 : len(typPart)-1]
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

func buildGenLive(pkgpath string) (map[string]bool, error) {
	tmp, err := os.CreateTemp("", "")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	// Build
	{
		cmd := exec.Command("go", "build", "-o", tmp.Name(), "-gcflags=all=-N -l", pkgpath)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("%v: %s\n%w", cmd, out, err)
		}
	}

	cmd := exec.Command("go", "tool", "nm", tmp.Name())
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	syms := make(map[string]bool)
	scan := bufio.NewScanner(out)
	for scan.Scan() {
		parts := bytes.Fields(scan.Bytes())
		if len(parts) != 3 {
			continue
		}
		name := string(parts[2])
		if strings.Contains(name, "..") {
			// generated algs, inittask, gobytes, anonymous functions
			continue
		}
		if strings.Contains(name, ",") {
			// go.itab entry
			continue
		}
		if strings.HasPrefix(name, "$") {
			// floating point constant
			continue
		}
		syms[name] = true
	}
	if scan.Err() != nil {
		return nil, err
	}
	if err := cmd.Wait(); err != nil {
		return nil, err
	}
	return syms, nil
}
