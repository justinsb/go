// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
)

func init() {
	register("Faststart", Faststart)
	register("FaststartGC", FaststartGC)
	register("FaststartGOMAXPROCS", FaststartGOMAXPROCS)
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

// FaststartGOMAXPROCS checks the value of GOMAXPROCS before and after the
// first GC cycle of a program built in fast start mode.
func FaststartGOMAXPROCS() {
	// Keep in sync with runtime.faststartInitialProcs.
	const initialProcs = 4

	if got, want := runtime.GOMAXPROCS(0), min(runtime.NumCPU(), initialProcs); got != want {
		fmt.Printf("at start, GOMAXPROCS = %d, want %d\n", got, want)
		os.Exit(1)
	}

	runtime.GC()

	// The default GOMAXPROCS is in effect as soon as the first cycle
	// has started. Calling SetDefaultGOMAXPROCS must not change it.
	got := runtime.GOMAXPROCS(0)
	debug.SetGCPercent(-1)
	runtime.SetDefaultGOMAXPROCS()
	if want := runtime.GOMAXPROCS(0); got != want {
		fmt.Printf("after GC, GOMAXPROCS = %d, want %d\n", got, want)
		os.Exit(1)
	}
	if runtime.NumCPU() <= initialProcs && got > runtime.NumCPU() {
		fmt.Printf("after GC, GOMAXPROCS = %d, want at most %d\n", got, runtime.NumCPU())
		os.Exit(1)
	}
	fmt.Println("OK")
}
