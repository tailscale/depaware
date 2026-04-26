// Package minifmt is a tiny fmt-like package for testing dead code stripping.
package minifmt

// Println prints a line.
func Println(s string) {
	println(s)
}

// Unused is not called by anything.
func Unused() {
	println("unused")
}
