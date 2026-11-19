// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sync/atomic"
	"syscall"
	"time"
)

func init() {
	register("Faststart", Faststart)
	register("FaststartGC", FaststartGC)
	register("FaststartGOMAXPROCS", FaststartGOMAXPROCS)
	register("FaststartGOMAXPROCSGo", FaststartGOMAXPROCSGo)
	register("FaststartTimerSpin", FaststartTimerSpin)
	register("FaststartTimerSyscall", FaststartTimerSyscall)
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

// schedGOMAXPROCS returns the scheduler's current value of GOMAXPROCS
// without calling runtime.GOMAXPROCS, which in fast start mode has the
// side effect of applying the default value.
func schedGOMAXPROCS() int {
	s := []metrics.Sample{{Name: "/sched/gomaxprocs:threads"}}
	metrics.Read(s)
	return int(s[0].Value.Uint64())
}

// FaststartGOMAXPROCS checks that a program built in fast start mode
// starts with one P, but that it cannot tell by calling GOMAXPROCS.
func FaststartGOMAXPROCS() {
	if got := schedGOMAXPROCS(); got != 1 {
		fmt.Printf("at start, scheduler has GOMAXPROCS = %d, want 1\n", got)
		os.Exit(1)
	}

	got := runtime.GOMAXPROCS(0)
	if sched := schedGOMAXPROCS(); got != sched {
		fmt.Printf("GOMAXPROCS(0) = %d, but scheduler has GOMAXPROCS = %d\n", got, sched)
		os.Exit(1)
	}

	// The default GOMAXPROCS is now in effect.
	// Calling SetDefaultGOMAXPROCS must not change it.
	debug.SetGCPercent(-1)
	runtime.SetDefaultGOMAXPROCS()
	if want := runtime.GOMAXPROCS(0); got != want {
		fmt.Printf("GOMAXPROCS(0) = %d, want the default, %d\n", got, want)
		os.Exit(1)
	}
	if got > runtime.NumCPU() {
		fmt.Printf("GOMAXPROCS(0) = %d, want at most NumCPU = %d\n", got, runtime.NumCPU())
		os.Exit(1)
	}
	fmt.Println("OK")
}

// FaststartGOMAXPROCSGo checks that a program built in fast start mode
// moves to the default GOMAXPROCS once it starts a goroutine.
func FaststartGOMAXPROCSGo() {
	if got := schedGOMAXPROCS(); got != 1 {
		fmt.Printf("at start, scheduler has GOMAXPROCS = %d, want 1\n", got)
		os.Exit(1)
	}

	done := make(chan bool)
	go func() { done <- true }()
	<-done

	// The change is made by another goroutine. Give it a while.
	deadline := time.Now().Add(10 * time.Second)
	for runtime.NumCPU() > 1 && schedGOMAXPROCS() == 1 {
		if time.Now().After(deadline) {
			fmt.Printf("scheduler still has GOMAXPROCS = 1 with NumCPU = %d\n", runtime.NumCPU())
			os.Exit(1)
		}
		time.Sleep(time.Millisecond)
	}
	if got, want := schedGOMAXPROCS(), runtime.GOMAXPROCS(0); got != want {
		fmt.Printf("scheduler has GOMAXPROCS = %d, want the default, %d\n", got, want)
		os.Exit(1)
	}
	fmt.Println("OK")
}

// FaststartTimerSpin checks that a timer fires in a program whose only
// goroutine never yields. In fast start mode that takes sysmon, which
// is not running when the timer is set, to preempt the main goroutine.
func FaststartTimerSpin() {
	var fired atomic.Bool
	time.AfterFunc(10*time.Millisecond, func() { fired.Store(true) })
	for !fired.Load() {
	}
	fmt.Println("OK")
}

// FaststartTimerSyscall checks that a timer fires in a program whose
// only goroutine is blocked in a system call. In fast start mode that
// takes sysmon, which is not running when the timer is set, to retake
// the main goroutine's P.
func FaststartTimerSyscall() {
	var p [2]int
	if err := syscall.Pipe(p[:]); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	time.AfterFunc(10*time.Millisecond, func() { syscall.Write(p[1], []byte("x")) })
	var buf [1]byte
	if n, err := syscall.Read(p[0], buf[:]); n != 1 || err != nil {
		fmt.Println("read:", n, err)
		os.Exit(1)
	}
	fmt.Println("OK")
}
