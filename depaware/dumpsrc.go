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
	"runtime"
	"sort"
	"strings"

	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"

	"github.com/tailscale/depaware/internal/edit"
	"golang.org/x/tools/go/ast/astutil"
	goimports "golang.org/x/tools/imports"
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
	ar *txtar.Archive
	goos    string // if set, override GOOS for build and package loading
	goarch  string // if set, override GOARCH for build and package loading
	dir     string // if set, working directory for package loading
}

func (w *walker) dead(sym string) bool {
	return !w.symLive[sym]
}

// evalGoosGoarch evaluates comparisons of runtime.GOOS or runtime.GOARCH
// against string literals. Returns (result, true) if the expression can be
// statically evaluated, or (false, false) if not.
func (w *walker) evalGoosGoarch(expr ast.Expr) (result, ok bool) {
	bin, isBin := expr.(*ast.BinaryExpr)
	if !isBin || (bin.Op != token.EQL && bin.Op != token.NEQ) {
		return false, false
	}
	// Match runtime.GOOS or runtime.GOARCH on either side.
	var varName, litVal string
	if sel, lit, ok := matchRuntimeSelector(bin.X, bin.Y); ok {
		varName, litVal = sel, lit
	} else if sel, lit, ok := matchRuntimeSelector(bin.Y, bin.X); ok {
		varName, litVal = sel, lit
	} else {
		return false, false
	}
	var actual string
	switch varName {
	case "GOOS":
		actual = w.goos
	case "GOARCH":
		actual = w.goarch
	default:
		return false, false
	}
	eq := actual == litVal
	if bin.Op == token.NEQ {
		eq = !eq
	}
	return eq, true
}

// matchRuntimeSelector checks if selExpr is runtime.GOOS or runtime.GOARCH
// and litExpr is a string literal. Returns (selector name, string value, ok).
func matchRuntimeSelector(selExpr, litExpr ast.Expr) (string, string, bool) {
	sel, ok := selExpr.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok || ident.Name != "runtime" {
		return "", "", false
	}
	name := sel.Sel.Name
	if name != "GOOS" && name != "GOARCH" {
		return "", "", false
	}
	lit, ok := litExpr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", "", false
	}
	// Strip quotes.
	val := strings.Trim(lit.Value, `"`)
	return name, val, true
}

func (w *walker) walk(mainPkg string) (*txtar.Archive, error) {
	if w.goos == "" {
		w.goos = runtime.GOOS
	}
	if w.goarch == "" {
		w.goarch = runtime.GOARCH
	}

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

	for _, pkg := range pkgs {
		if err := w.walkPackage(pkg); err != nil {
			return nil, err
		}
	}
	sort.Slice(w.ar.Files, func(i, j int) bool {
		return w.ar.Files[i].Name < w.ar.Files[j].Name
	})
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

	if len(pkg.Errors) > 0 {
		return fmt.Errorf("errors reading %q: %q", pkg.PkgPath, pkg.Errors)
	}

	// Mark types as live if any of their methods are live.
	for _, f := range pkg.Syntax {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil {
				continue
			}
			if w.symLive[funcName(pkg, fd)] {
				if tName := recvTypeName(pkg, fd); tName != "" {
					w.symLive[tName] = true
				}
			}
		}
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
			case *ast.IfStmt:
				if n.Init != nil {
					break // has init statement, too complex
				}
				val, ok := w.evalGoosGoarch(n.Cond)
				if !ok {
					break
				}
				ifStart := offset(pkg.Fset, n.Pos())
				ifEnd := offset(pkg.Fset, n.End())
				if val {
					// Condition is true: keep the if body, drop else.
					bodyStart := offset(pkg.Fset, n.Body.Lbrace) + 1
					bodyEnd := offset(pkg.Fset, n.Body.Rbrace)
					body := bytes.TrimSpace(src[bodyStart:bodyEnd])
					editBuf.Replace(ifStart, ifEnd, string(body))
				} else {
					// Condition is false: drop if body, keep else (if any).
					if n.Else != nil {
						elseBody, ok := n.Else.(*ast.BlockStmt)
						if !ok {
							break // else-if chain, too complex
						}
						bodyStart := offset(pkg.Fset, elseBody.Lbrace) + 1
						bodyEnd := offset(pkg.Fset, elseBody.Rbrace)
						body := bytes.TrimSpace(src[bodyStart:bodyEnd])
						editBuf.Replace(ifStart, ifEnd, string(body))
					} else {
						editBuf.Delete(ifStart, ifEnd)
					}
				}
				return false // don't recurse into children
			}
			return true
		}
		astutil.Apply(f, pre, post)

		src = editBuf.Bytes()
		if fmtSrc, err := goimports.Process(fileName, src, nil); err == nil {
			src = fmtSrc
		} else if fmtSrc, err := format.Source(src); err == nil {
			src = fmtSrc
		}

		// Skip files with no remaining top-level declarations.
		if fileIsEmpty(src) {
			continue
		}

		relPath := fileName
		if pkg.Module != nil && pkg.Module.Dir != "" {
			if rel, err := filepath.Rel(pkg.Module.Dir, fileName); err == nil {
				relPath = pkg.Module.Path + "/" + filepath.ToSlash(rel)
			}
		} else if goroot := runtime.GOROOT(); goroot != "" {
			if rel, err := filepath.Rel(goroot, fileName); err == nil && !strings.HasPrefix(rel, "..") {
				relPath = "go-" + goVersion() + "/" + filepath.ToSlash(rel)
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

// recvTypeName returns the type name (e.g. "main.FooType") for the
// receiver of a method declaration, or "" if it can't be determined.
func recvTypeName(pkg *packages.Package, fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	typ := fd.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if ident, ok := typ.(*ast.Ident); ok {
		return pkgSym(pkg) + "." + ident.Name
	}
	return ""
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

// fileIsEmpty reports whether src contains no top-level declarations
// other than imports (i.e. only comments, package clause, and imports).
func fileIsEmpty(src []byte) bool {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ImportsOnly)
	if err != nil {
		return false
	}
	// parser.ImportsOnly stops after imports, so f.Decls only contains
	// import GenDecls. Compare against the full file size: if the file
	// has more content after the imports section, it has real decls.
	endPos := fset.Position(f.Package).Offset + len("package ") + len(f.Name.Name)
	for _, d := range f.Decls {
		end := fset.Position(d.End()).Offset
		if end > endPos {
			endPos = end
		}
	}
	// Check if there's any non-whitespace/comment content after the
	// last import (or package clause if no imports).
	rest := bytes.TrimSpace(src[endPos:])
	return len(rest) == 0
}

// goVersion returns the Go version number without the "go" prefix
// (e.g. "1.26.1").
func goVersion() string {
	v := runtime.Version()
	if strings.HasPrefix(v, "go") {
		return v[len("go"):]
	}
	return v
}

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
