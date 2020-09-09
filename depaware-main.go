// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The depaware command makes you aware of your dependencies by
// putting them in your face in git and during code review.
//
// The idea is that you store the depaware output next to any desired
// packages or binaries and check them in to git, making it a CI
// failure if they're out of date, and thus make you aware of
// dependency changes during code review.
//
// See https://github.com/tailscale/depaware
package main

import "github.com/tailscale/depaware/depaware"

func main() {
	depaware.Main()
}
