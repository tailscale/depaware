# dumpsrc TODO

## Dead code detection gaps

- **Interface types**: only used if their itab is in the binary; currently not checked
- **`go:linkname` directives**: symbols referenced via `go:linkname` bypass normal import/call graphs and won't be detected as live
- **Constants and variables**: `ValueSpec` (const/var declarations) are never stripped; no liveness analysis for them
- **Even broader constant DCE**: constant bool expressions and `runtime.GOOS`/`runtime.GOARCH` comparisons in if-statements are stripped, but switch statements and more complex constant patterns (e.g. `const x = 1; if x == 2`) are not yet handled

## Single platform

- `dumpSource` only analyzes for the current GOOS/GOARCH, unlike `process()` which iterates over multiple platforms
- No flag to specify a target GOOS/GOARCH for cross-platform analysis

## Symbol matching

- `funcName`/`typeName` construct symbol names that may not match `go tool nm` output for generics, compiler-generated wrappers, or complex receiver types
- Anonymous functions and closures are filtered out by the `..` check but never analyzed

## Test coverage

- Only one test binary (`test/smallbin`): single file, single package, no external imports
- No coverage for multi-file packages, multi-package binaries, init functions, or blank imports (`import _ "pkg"`)
