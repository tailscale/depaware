package main

import "runtime"

const debug = false

func main() {
	println(PlatformMessage())
	println(PlatformNot())
	println(PlatformElse())
	println(ConstBranch())
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
