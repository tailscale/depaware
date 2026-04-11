// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The dumpsrc mode outputs a txtar archive of source files with dead code
// stripped, for human consumption only. The output is not intended to compile;
// unused imports and type definitions may remain or be missing.
//
// TODO: interface types are only used if their itab is used
// TODO: other types are used if ... something? errorsString. pcln table?
// TODO: go:linkname

package depaware

import (
	"bufio"
	"bytes"
	"debug/buildinfo"
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

// dumpSourceBinary reads build info from an existing binary to discover the
// main package path, module version, and GOOS/GOARCH. It then downloads the
// module source at that version into a temp workspace and rebuilds with
// inlining disabled so nm can see all live symbols.
func dumpSourceBinary(binPath string) (*txtar.Archive, error) {
	info, err := buildinfo.ReadFile(binPath)
	if err != nil {
		return nil, fmt.Errorf("reading build info from %s: %w", binPath, err)
	}

	mainPkg := info.Path
	if mainPkg == "" {
		return nil, fmt.Errorf("binary %s has no main package path in build info", binPath)
	}

	var goos, goarch string
	for _, s := range info.Settings {
		switch s.Key {
		case "GOOS":
			goos = s.Value
		case "GOARCH":
			goarch = s.Value
		}
	}

	mainVer := info.Main.Version
	if mainVer == "" || mainVer == "(devel)" || strings.Contains(mainVer, "+dirty") {
		return nil, fmt.Errorf("binary %s was built from local/dirty sources (version %q); use a package path instead", binPath, mainVer)
	}

	// Create a temp workspace so we don't modify the current directory's go.mod.
	tmpDir, err := os.MkdirTemp("", "depaware-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	// Write a go.mod pinning the exact module versions from the binary.
	var gomod strings.Builder
	gomod.WriteString("module depaware-tmp\n\ngo 1.18\n\nrequire (\n")
	fmt.Fprintf(&gomod, "\t%s %s\n", info.Main.Path, mainVer)
	for _, dep := range info.Deps {
		if dep.Replace != nil {
			fmt.Fprintf(&gomod, "\t%s %s\n", dep.Replace.Path, dep.Replace.Version)
		} else {
			fmt.Fprintf(&gomod, "\t%s %s\n", dep.Path, dep.Version)
		}
	}
	gomod.WriteString(")\n")

	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(gomod.String()), 0644); err != nil {
		return nil, err
	}

	// Download modules into cache.
	dlCmd := exec.Command("go", "mod", "download")
	dlCmd.Dir = tmpDir
	if out, err := dlCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("go mod download: %s\n%w", out, err)
	}

	w := walker{
		goos:   goos,
		goarch: goarch,
		dir:    tmpDir,
	}
	return w.walk(mainPkg)
}

type walker struct {
	done    map[string]bool
	symLive map[string]bool
	ar      *txtar.Archive
	modPath string // only include packages from this module
	modDir  string // module root dir, for computing relative paths
	goos    string // if set, override GOOS for build and package loading
	goarch  string // if set, override GOARCH for build and package loading
	dir     string // if set, working directory for package loading
}

func (w *walker) dead(sym string) bool {
	return !w.symLive[sym]
}

func (w *walker) walk(mainPkg string) (*txtar.Archive, error) {
	var env []string
	if w.goos != "" || w.goarch != "" {
		env = os.Environ()
		if w.goos != "" {
			env = append(env, "GOOS="+w.goos)
		}
		if w.goarch != "" {
			env = append(env, "GOARCH="+w.goarch)
		}
	}

	buildErrc := make(chan error, 1)
	go func() {
		var err error
		w.symLive, err = buildGenLive(mainPkg, env, w.dir)
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
		Env: env,
		Dir: w.dir,
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
						// Left as-is; output is for human reading, not compilation.
					case *ast.TypeSpec:
						name := typeName(pkg, spec)
						if w.dead(name) {
							start, end := offsetRange(pkg.Fset, spec)
							dels = append(dels, delRange{start, end})
						}
					case *ast.ValueSpec:
						// Consts and vars left as-is for readability.
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

// buildGenLive builds pkgpath with inlining/optimization disabled and
// returns the set of live symbols via nm.
func buildGenLive(pkgpath string, env []string, dir string) (map[string]bool, error) {
	tmp, err := os.CreateTemp("", "depaware-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())

	cmd := exec.Command("go", "build", "-o", tmp.Name(), "-gcflags=all=-N -l", pkgpath)
	if len(env) > 0 {
		cmd.Env = env
	}
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%v: %s\n%w", cmd, out, err)
	}

	return nmLive(tmp.Name())
}

// nmLive runs "go tool nm" on a binary and returns the set of live symbols.
func nmLive(binPath string) (map[string]bool, error) {
	cmd := exec.Command("go", "tool", "nm", binPath)
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
		return nil, scan.Err()
	}
	if err := cmd.Wait(); err != nil {
		return nil, err
	}
	return syms, nil
}
