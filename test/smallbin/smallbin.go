package main

// Comment for main.
//
// Last line.
func main() {
	Foo()

	var ft FooType
	ft.ValueMethod()
	ft.PtrMethod()
}

// Comment for Foo.
//
// Last line.
func Foo() {
	println("Foo")
}

// Bar is a bar.
func Bar() {
	// Unused.
}

// UnusedType is unused.
type UnusedType struct {
	// foo
	// bar
}

type (
	// UnusedFactoredType comment.
	UnusedFactoredType struct {
		a,
		b string
	}
	// UsedFactoredType is used.
	UsedFactoredType int
)

// Comment on a whole group.
type (
// Nothing in here anyway.
)

// FooType is a used type.
type FooType struct {
	x int
}

func (FooType) ValueMethod() {}

func (*FooType) PtrMethod() {}
