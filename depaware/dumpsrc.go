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
	"sync"

	"go/ast"
	"go/constant"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/txtar"
)

func dumpSource(pkgMain string, cgo string) (*txtar.Archive, error) {
	w := walker{cgo: cgo}
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

	var goos, goarch, cgo string
	for _, s := range info.Settings {
		switch s.Key {
		case "GOOS":
			goos = s.Value
		case "GOARCH":
			goarch = s.Value
		case "CGO_ENABLED":
			cgo = s.Value
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
		cgo:    cgo,
		dir:    tmpDir,
	}
	return w.walk(mainPkg)
}

type walker struct {
	done    map[string]bool
	symLive map[string]bool

	mu sync.Mutex // guards ar
	ar *txtar.Archive

	goos   string // if set, override GOOS for build and package loading
	goarch string // if set, override GOARCH for build and package loading
	cgo    string // if set, "true" or "false" for CGO_ENABLED
	dir    string // if set, working directory for package loading
	goroot string // GOROOT of the target go toolchain
}

func (w *walker) dead(sym string) bool {
	return !w.symLive[sym]
}

// evalBoolConst checks whether expr is a statically known boolean expression.
// It handles constant bools, runtime.GOOS/GOARCH comparisons, and logical
// operators (&&, ||, !) with partial constant evaluation.
func evalBoolConst(pkg *packages.Package, w *walker, expr ast.Expr) (result, ok bool) {
	if pkg.TypesInfo != nil {
		// Check if the type checker evaluated this as a constant bool.
		tv, exists := pkg.TypesInfo.Types[expr]
		if exists && tv.Value != nil && tv.Value.Kind() == constant.Bool {
			return constant.BoolVal(tv.Value), true
		}
		// TypesInfo.Types excludes bare identifiers; check Uses for
		// references to const objects (e.g. "if debug" where debug is
		// a const bool).
		if ident, ok := expr.(*ast.Ident); ok {
			if obj, exists := pkg.TypesInfo.Uses[ident]; exists {
				if c, ok := obj.(*types.Const); ok && c.Val().Kind() == constant.Bool {
					return constant.BoolVal(c.Val()), true
				}
			}
		}
	}

	switch e := expr.(type) {
	case *ast.UnaryExpr:
		if e.Op == token.NOT {
			if val, ok := evalBoolConst(pkg, w, e.X); ok {
				return !val, true
			}
		}
	case *ast.BinaryExpr:
		switch e.Op {
		case token.LAND: // &&
			lv, lok := evalBoolConst(pkg, w, e.X)
			if lok && !lv {
				return false, true // false && anything = false
			}
			rv, rok := evalBoolConst(pkg, w, e.Y)
			if rok && !rv {
				return false, true // anything && false = false
			}
			if lok && rok {
				return lv && rv, true
			}
		case token.LOR: // ||
			lv, lok := evalBoolConst(pkg, w, e.X)
			if lok && lv {
				return true, true // true || anything = true
			}
			rv, rok := evalBoolConst(pkg, w, e.Y)
			if rok && rv {
				return true, true // anything || true = true
			}
			if lok && rok {
				return lv || rv, true
			}
		}
	case *ast.ParenExpr:
		return evalBoolConst(pkg, w, e.X)
	}

	// Fall back to matching runtime.GOOS/GOARCH comparisons.
	return evalRuntimeGOOS(w, expr)
}

// evalConstValue evaluates expr to a constant.Value if possible.
// Handles compile-time constants (via TypesInfo) and runtime.GOOS/GOARCH
// (which are vars but fixed for a given build target).
func evalConstValue(pkg *packages.Package, w *walker, expr ast.Expr) (constant.Value, bool) {
	if pkg.TypesInfo != nil {
		if tv, ok := pkg.TypesInfo.Types[expr]; ok && tv.Value != nil {
			return tv.Value, true
		}
		if ident, ok := expr.(*ast.Ident); ok {
			if obj, ok := pkg.TypesInfo.Uses[ident].(*types.Const); ok {
				return obj.Val(), true
			}
		}
	}
	// Special-case runtime.GOOS / runtime.GOARCH.
	if sel, ok := expr.(*ast.SelectorExpr); ok {
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "runtime" {
			switch sel.Sel.Name {
			case "GOOS":
				return constant.MakeString(w.goos), true
			case "GOARCH":
				return constant.MakeString(w.goarch), true
			}
		}
	}
	if pe, ok := expr.(*ast.ParenExpr); ok {
		return evalConstValue(pkg, w, pe.X)
	}
	return nil, false
}

// evalRuntimeGOOS evaluates comparisons of runtime.GOOS or runtime.GOARCH
// against string literals. These are technically variables but are fixed
// at build time.
func evalRuntimeGOOS(w *walker, expr ast.Expr) (result, ok bool) {
	bin, isBin := expr.(*ast.BinaryExpr)
	if !isBin || (bin.Op != token.EQL && bin.Op != token.NEQ) {
		return false, false
	}
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

	// Look up GOROOT from the go toolchain that will be used,
	// not from the depaware binary's own runtime.GOROOT().
	if w.goroot == "" {
		cmd := exec.Command("go", "env", "GOROOT")
		if w.dir != "" {
			cmd.Dir = w.dir
		}
		if out, err := cmd.Output(); err == nil {
			w.goroot = strings.TrimSpace(string(out))
		}
	}

	var env []string
	if w.goos != "" || w.goarch != "" || w.cgo != "" {
		env = os.Environ()
		if w.goos != "" {
			env = append(env, "GOOS="+w.goos)
		}
		if w.goarch != "" {
			env = append(env, "GOARCH="+w.goarch)
		}
		if w.cgo != "" {
			env = append(env, "CGO_ENABLED="+w.cgo)
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
			packages.NeedTypesInfo |
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

	// Phase 1: walk packages serially to mark liveness and collect
	// per-file work items.
	var work []fileWork
	for _, pkg := range pkgs {
		more, err := w.collectWork(pkg)
		if err != nil {
			return nil, err
		}
		work = append(work, more...)
	}

	// Phase 2: process all files in parallel.
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	var wg sync.WaitGroup
	var errOnce sync.Once
	var firstErr error

	for _, item := range work {
		wg.Add(1)
		sem <- struct{}{}
		go func(item fileWork) {
			defer wg.Done()
			defer func() { <-sem }()

			src, _, err := w.processFile(item.pkg, item.f, item.fileName)
			if err != nil {
				errOnce.Do(func() { firstErr = err })
				return
			}
			if src == nil || fileIsEmpty(src) {
				return
			}

			relPath := item.fileName
			if item.pkg.Module != nil && item.pkg.Module.Dir != "" {
				if rel, err := filepath.Rel(item.pkg.Module.Dir, item.fileName); err == nil {
					relPath = item.pkg.Module.Path + "/" + filepath.ToSlash(rel)
				}
			} else if w.goroot != "" {
				if rel, err := filepath.Rel(w.goroot, item.fileName); err == nil && !strings.HasPrefix(rel, "..") {
					relPath = "GOROOT/" + filepath.ToSlash(rel)
				}
			}

			w.mu.Lock()
			w.ar.Files = append(w.ar.Files, txtar.File{
				Name: relPath,
				Data: src,
			})
			w.mu.Unlock()
		}(item)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	sort.Slice(w.ar.Files, func(i, j int) bool {
		return w.ar.Files[i].Name < w.ar.Files[j].Name
	})
	return w.ar, nil
}

type fileWork struct {
	pkg      *packages.Package
	f        *ast.File
	fileName string
}

// collectWork walks pkg and its imports recursively, marking types-with-live-
// methods as live and returning the per-file work items to be processed in
// parallel. This phase must be serial because liveness mutations on
// w.symLive are not synchronized.
func (w *walker) collectWork(pkg *packages.Package) ([]fileWork, error) {
	if w.done[pkg.PkgPath] {
		return nil, nil
	}
	if w.done == nil {
		w.done = map[string]bool{}
	}
	w.done[pkg.PkgPath] = true

	if len(pkg.Errors) > 0 {
		return nil, fmt.Errorf("errors reading %q: %q", pkg.PkgPath, pkg.Errors)
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

	// Use CompiledGoFiles (not GoFiles) because pkg.Syntax corresponds
	// to CompiledGoFiles. These can differ for cgo packages.
	goFiles := pkg.CompiledGoFiles
	if len(goFiles) != len(pkg.Syntax) {
		goFiles = pkg.GoFiles // fallback
	}

	var work []fileWork
	for i, f := range pkg.Syntax {
		if i >= len(goFiles) {
			break
		}
		fileName := goFiles[i]
		if !strings.HasSuffix(fileName, ".go") || strings.Contains(fileName, "/go-build/") {
			continue
		}
		work = append(work, fileWork{pkg, f, fileName})
	}

	// Recurse into imports.
	var imports []*packages.Package
	for _, p := range pkg.Imports {
		imports = append(imports, p)
	}
	sort.Slice(imports, func(i, j int) bool {
		return imports[i].PkgPath < imports[j].PkgPath
	})
	for _, p := range imports {
		more, err := w.collectWork(p)
		if err != nil {
			return nil, err
		}
		work = append(work, more...)
	}
	return work, nil
}

// processFile applies AST mutations (dead-code stripping and constant DCE)
// to f and returns the formatted source. If no mutations were needed, the
// original source bytes are returned unmodified (skipping the format pass).
// Returns nil src to indicate the file should be skipped.
func (w *walker) processFile(pkg *packages.Package, f *ast.File, fileName string) (src []byte, mutated bool, err error) {
	// Capture comment associations before mutating.
	cmap := ast.NewCommentMap(pkg.Fset, f, f.Comments)

	deleted, mutated := w.mutateAST(pkg, f)

	if mutated {
		// Drop unused imports based on what's still referenced.
		removeUnusedImports(pkg, f)
		// Restore comments from surviving nodes only, then drop any
		// comments whose positions fall inside a deleted node's
		// original range (e.g. a free comment inside a deleted block).
		f.Comments = filterComments(cmap.Filter(f).Comments(), deleted)

		var buf bytes.Buffer
		if err := format.Node(&buf, pkg.Fset, f); err != nil {
			return nil, false, err
		}
		return buf.Bytes(), true, nil
	}

	// No mutations: read the original file bytes from disk.
	src, err = os.ReadFile(fileName)
	if err != nil {
		return nil, false, err
	}
	return src, false, nil
}

// filterComments drops comment groups whose Pos() falls within any of
// the deleted position ranges.
func filterComments(comments []*ast.CommentGroup, deleted []posRange) []*ast.CommentGroup {
	if len(deleted) == 0 {
		return comments
	}
	out := comments[:0]
	for _, cg := range comments {
		pos := cg.Pos()
		drop := false
		for _, d := range deleted {
			if pos >= d.start && pos < d.end {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, cg)
		}
	}
	return out
}

type posRange struct{ start, end token.Pos }

// mutateAST walks f and applies dead-code stripping and constant-expression
// DCE via in-place AST mutations. Returns the original position ranges of
// deleted nodes (for comment filtering) and whether any mutation was made.
func (w *walker) mutateAST(pkg *packages.Package, f *ast.File) (deleted []posRange, mutated bool) {
	astutil.Apply(f, func(c *astutil.Cursor) bool {
		n := c.Node()
		switch n := n.(type) {
		case *ast.FuncDecl:
			if w.dead(funcName(pkg, n)) {
				deleted = append(deleted, posRange{n.Pos(), n.End()})
				c.Delete()
				mutated = true
				return false
			}
		case *ast.GenDecl:
			// Strip dead TypeSpecs from this GenDecl's Specs slice.
			// Also drop empty type groups (e.g. "type ()" with no specs).
			survivors := n.Specs[:0]
			for _, spec := range n.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					survivors = append(survivors, spec)
					continue
				}
				if w.dead(typeName(pkg, ts)) {
					mutated = true
					continue
				}
				survivors = append(survivors, spec)
			}
			// Only Delete when in a slice context (i.e. a top-level
			// declaration in *ast.File.Decls). GenDecls inside function
			// bodies are wrapped in *ast.DeclStmt and aren't directly
			// in a slice.
			if len(survivors) == 0 && n.Tok == token.TYPE && c.Index() >= 0 {
				deleted = append(deleted, posRange{n.Pos(), n.End()})
				c.Delete()
				mutated = true
				return false
			}
			n.Specs = survivors
		case *ast.IfStmt:
			if n.Init != nil {
				return true
			}
			// IfStmt may be in a non-slice context (e.g. the Else
			// field of an outer IfStmt for else-if chains). We can't
			// InsertBefore/Delete in that case.
			if c.Index() < 0 {
				return true
			}
			val, ok := evalBoolConst(pkg, w, n.Cond)
			if !ok {
				return true
			}
			mutated = true
			if val {
				// Condition true: replace if with body statements.
				deleted = append(deleted, posRange{n.Pos(), n.Body.Lbrace + 1})
				if n.Else != nil {
					deleted = append(deleted, posRange{n.Body.Rbrace, n.Else.End()})
				}
				for _, stmt := range n.Body.List {
					c.InsertBefore(stmt)
				}
				c.Delete()
				return false
			}
			// Condition false.
			if n.Else == nil {
				deleted = append(deleted, posRange{n.Pos(), n.End()})
				c.Delete()
				return false
			}
			elseBlock, ok := n.Else.(*ast.BlockStmt)
			if !ok {
				// else-if; keep as-is rather than over-complicating.
				return true
			}
			deleted = append(deleted, posRange{n.Pos(), elseBlock.Lbrace + 1})
			deleted = append(deleted, posRange{elseBlock.Rbrace, elseBlock.End()})
			for _, stmt := range elseBlock.List {
				c.InsertBefore(stmt)
			}
			c.Delete()
			return false
		case *ast.SwitchStmt:
			if n.Init != nil {
				return true
			}
			if c.Index() < 0 {
				return true
			}
			if w.dceSwitch(pkg, n, c, &deleted) {
				mutated = true
				return false
			}
		}
		return true
	}, nil)
	return deleted, mutated
}

// dceSwitch attempts dead-code elimination on a SwitchStmt. Returns true if
// the switch was replaced (and the cursor's Delete/InsertBefore was used).
func (w *walker) dceSwitch(pkg *packages.Package, n *ast.SwitchStmt, c *astutil.Cursor, deleted *[]posRange) bool {
	// Determine the tag value (if statically known). Tag is nil for
	// tagless switches; in that case each case's expressions must
	// individually evaluate to bool constants.
	var tagVal constant.Value
	tagKnown := false
	if n.Tag != nil {
		tagVal, tagKnown = evalConstValue(pkg, w, n.Tag)
	}

	var winner *ast.CaseClause
	var defaultClause *ast.CaseClause
	survivors := []ast.Stmt{}

	for _, stmt := range n.Body.List {
		cc, ok := stmt.(*ast.CaseClause)
		if !ok {
			return false // unexpected; bail out
		}
		if len(cc.List) == 0 {
			defaultClause = cc
			continue
		}
		if winner != nil {
			continue // earlier case already won; drop the rest
		}

		anyMatch := false
		allMismatch := true
		for _, expr := range cc.List {
			var matched bool
			var known bool
			if n.Tag == nil {
				// Tagless: each expr is a boolean.
				if v, ok := evalBoolConst(pkg, w, expr); ok {
					matched = v
					known = true
				}
			} else if tagKnown {
				// Tag switch: compare expr to tagVal.
				if v, ok := evalConstValue(pkg, w, expr); ok {
					matched = constant.Compare(tagVal, token.EQL, v)
					known = true
				}
			}
			if known {
				if matched {
					anyMatch = true
					break
				}
			} else {
				allMismatch = false
			}
		}

		if anyMatch {
			winner = cc
		} else if allMismatch {
			// All this case's expressions are statically false; drop it.
			continue
		} else {
			survivors = append(survivors, cc)
		}
	}

	// If a case won, replace switch with that case's body.
	if winner != nil {
		*deleted = append(*deleted, posRange{n.Pos(), n.End()})
		for _, stmt := range winner.Body {
			c.InsertBefore(stmt)
		}
		c.Delete()
		return true
	}

	// No survivors: only default remains (or nothing).
	if len(survivors) == 0 {
		*deleted = append(*deleted, posRange{n.Pos(), n.End()})
		if defaultClause != nil {
			for _, stmt := range defaultClause.Body {
				c.InsertBefore(stmt)
			}
		}
		c.Delete()
		return true
	}

	// Some cases survive but not all — rewrite the switch body.
	if len(survivors) != len(n.Body.List) ||
		(defaultClause != nil && !containsClause(survivors, defaultClause)) {
		// Track removed clauses' positions.
		for _, stmt := range n.Body.List {
			cc, _ := stmt.(*ast.CaseClause)
			if cc == nil {
				continue
			}
			if cc == defaultClause {
				continue
			}
			if !containsClause(survivors, cc) {
				*deleted = append(*deleted, posRange{cc.Pos(), cc.End()})
			}
		}
		newBody := append([]ast.Stmt(nil), survivors...)
		if defaultClause != nil {
			newBody = append(newBody, defaultClause)
		}
		n.Body.List = newBody
		return true
	}
	return false
}

func containsClause(stmts []ast.Stmt, target *ast.CaseClause) bool {
	for _, s := range stmts {
		if s == target {
			return true
		}
	}
	return false
}

// removeUnusedImports removes ImportSpec entries from f whose imported
// package is no longer referenced by any surviving identifier. Blank (_)
// and dot (.) imports are preserved. Returns true if any import was removed.
func removeUnusedImports(pkg *packages.Package, f *ast.File) bool {
	// Find packages still referenced via TypesInfo.
	referenced := map[string]bool{}
	if pkg.TypesInfo != nil {
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			if pn, ok := pkg.TypesInfo.Uses[id].(*types.PkgName); ok {
				referenced[pn.Imported().Path()] = true
			}
			return true
		})
	}

	removed := false
	survivingDecls := f.Decls[:0]
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			survivingDecls = append(survivingDecls, decl)
			continue
		}
		survivors := gd.Specs[:0]
		for _, spec := range gd.Specs {
			is, ok := spec.(*ast.ImportSpec)
			if !ok {
				survivors = append(survivors, spec)
				continue
			}
			path := strings.Trim(is.Path.Value, `"`)
			// Keep blank/dot imports.
			if is.Name != nil && (is.Name.Name == "_" || is.Name.Name == ".") {
				survivors = append(survivors, spec)
				continue
			}
			if referenced[path] {
				survivors = append(survivors, spec)
				continue
			}
			removed = true
		}
		if len(survivors) == 0 {
			removed = true
			continue
		}
		gd.Specs = survivors
		survivingDecls = append(survivingDecls, gd)
	}
	f.Decls = survivingDecls
	return removed
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
