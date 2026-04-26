// Package main tests overlapping edits: a dead function containing
// a constant-guarded if-statement. Both the function deletion and
// the if-DCE would produce edits on overlapping ranges.
package main

const verbose = false

func main() {
	println(Live())
}

func Live() string {
	if verbose {
		return "verbose"
	}
	return "quiet"
}

// Dead is unused and contains a constant-guarded if.
func Dead() string {
	if verbose {
		return "dead verbose"
	}
	return "dead quiet"
}
