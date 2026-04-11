package main

import "runtime"

const debug = false

func main() {
	println(PlatformMessage())
	println(PlatformNot())
	println(PlatformElse())
	println(ConstBranch())
	println(ConstAnd(nil))
	println(NotConstAnd())
	println(ConstOr())
}

// PlatformMessage returns a platform-specific message.
func PlatformMessage() string {
	if runtime.GOOS == "android" {
		return "android"
	}
	return "not android"
}

// PlatformNot uses != comparison.
func PlatformNot() string {
	if runtime.GOOS != "linux" {
		return "not linux"
	}
	return "linux"
}

// PlatformElse has an else branch.
func PlatformElse() string {
	if runtime.GOOS == "android" {
		return "android"
	} else {
		return "not android"
	}
}

// ConstBranch tests user-defined constant DCE.
func ConstBranch() string {
	if debug {
		return "debug mode"
	}
	return "release mode"
}

type hasMethod struct{}

func (hasMethod) check() bool { return true }

// ConstAnd tests "false const && method call" — whole condition is false.
func ConstAnd(v *hasMethod) string {
	if debug && v.check() {
		return "debug and check"
	}
	return "no debug"
}

// NotConstAnd tests "!false const && ..." — !false is true, can't determine result.
func NotConstAnd() string {
	if !debug && runtime.GOOS == "linux" {
		return "not debug and linux"
	}
	return "other"
}

// ConstOr tests "true const || ..." — whole condition is true.
func ConstOr() string {
	if !debug || runtime.GOOS == "android" {
		return "not debug or android"
	} else {
		return "neither"
	}
}
