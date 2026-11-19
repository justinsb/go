// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import "runtime"

func init() {
	register("Faststart", Faststart)
	register("FaststartGC", FaststartGC)
}

// Faststart crashes before the program has run a GC cycle, and
// FaststartGC crashes after it has run one. When run with
// GOTRACEBACK=system, the traceback shows which of the runtime's
// background goroutines exist at that point.
func Faststart() {
	panic("crash")
}

func FaststartGC() {
	runtime.GC()
	panic("crash")
}
