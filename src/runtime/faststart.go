// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/runtime/atomic"
	"internal/runtime/gc"
)

// Fast start mode.
//
// A program built with the "faststart" build tag
//
//	go build -tags faststart
//
// gets a runtime tuned for processes whose time from exec to exit matters
// more than their steady-state throughput: container runtime shims, CNI
// plugins, command line tools and the like. A long-lived server should not
// use it.
//
// By default the runtime does a fair amount of work before main.main that
// only pays off if the process lives long enough to run a garbage
// collection. In fast start mode that work is deferred until the first GC
// cycle is about to start (see faststartDeferred), so a process that exits before
// then never pays for it:
//
//   - The background sweeper and scavenger goroutines are not started.
//     Neither has anything to do before the first GC cycle: there are no
//     unswept spans, and all free heap memory is still in the state the
//     OS handed it to us in.
//
//   - The forcegc helper goroutine is not started. Periodic GCs are
//     only ever forced once a first cycle has completed (see
//     gcTrigger.test).
//
//   - The cgroup CPU limit is not consulted when choosing the initial
//     GOMAXPROCS, and the goroutine that keeps GOMAXPROCS up to date is
//     not started.
//
//   - At most faststartInitialProcs Ps are created, however many CPUs
//     the machine has. Each P is a sizable heap allocation that has to
//     be initialized, so on a large machine creating them all dominates
//     the cost of starting up.
//
// The last two mean that, unless the GOMAXPROCS environment variable is
// set, GOMAXPROCS starts out as the smaller of faststartInitialProcs and
// the number of CPUs in the affinity mask, and only takes on its usual
// default value, including any cgroup CPU limit, when the first GC cycle
// is about to start. A program that wants the usual default sooner can
// call [SetDefaultGOMAXPROCS].
//
// Starting those goroutines eagerly is expensive mostly because each one
// wakes an idle P, which creates an OS thread. With them deferred, a
// program that does not start goroutines of its own runs main.main with
// only the main thread and sysmon.
//
// Fast start mode also changes the heap so that fewer pages are touched
// for the first time during startup. See faststartSizeClass.

// faststartInitialProcs is the largest default value of GOMAXPROCS that
// fast start mode uses before faststartDeferred has run.
const faststartInitialProcs = 4

// faststartState holds the state of the work that fast start mode
// defers.
var faststartState struct {
	// deferred is the state of faststartDeferred: one of the
	// faststartDeferred* constants.
	deferred atomic.Uint32

	// cgroupProbed is set once the deferred defaultGOMAXPROCSInit
	// has run. Protected by computeMaxProcsLock.
	cgroupProbed bool
}

const (
	faststartDeferredPending uint32 = iota
	faststartDeferredRunning
	faststartDeferredDone
)

// faststartDeferred performs the runtime initialization that fast start
// mode skips during startup. It is called by gcStart on a user goroutine
// that is allowed to block, before the first GC cycle starts.
//
// It reports whether the caller may go on to start a GC cycle. It
// returns false while the deferred initialization is in progress on
// another goroutine, or further up the stack of this one. That is
// important because the initialization allocates, both here and on the
// goroutines it starts, and any of those allocations may decide that it
// is time to start a cycle and call gcStart. They must not wait for the
// initialization to complete, since the initialization may be waiting
// for them.
//
// Refusing to start a cycle does not lose it. The goroutine running the
// initialization only got here because its trigger was satisfied. No
// cycle can start until it is done, so its trigger is still satisfied
// when it returns to gcStart, and it starts the cycle itself.
func faststartDeferred() bool {
	if faststartState.deferred.Load() == faststartDeferredDone {
		return true
	}
	if !faststartState.deferred.CompareAndSwap(faststartDeferredPending, faststartDeferredRunning) {
		return faststartState.deferred.Load() == faststartDeferredDone
	}

	// The sweeper and scavenger must be parked and ready to be woken
	// before the cycle reaches sweep termination.
	gcStartBackground()

	// Periodic GC can only trigger after a first cycle completes.
	go forcegchelper()

	// Pick up the cgroup CPU limit and switch to the usual default
	// value of GOMAXPROCS, then start keeping it up to date.
	faststartCgroupProbe()
	faststartDefaultGOMAXPROCS()
	defaultGOMAXPROCSUpdateEnable()

	faststartState.deferred.Store(faststartDeferredDone)
	return true
}

// faststartCgroupProbe performs the cgroup probe that the runtime
// normally does in schedinit, if it has not been done yet.
//
// In fast start mode, it must be called before the first call to
// defaultGOMAXPROCS that wants to observe cgroup limits.
func faststartCgroupProbe() {
	lock(&computeMaxProcsLock)
	if faststartState.cgroupProbed {
		unlock(&computeMaxProcsLock)
		return
	}
	// defaultGOMAXPROCSInit only makes raw system calls and uses a
	// static scratch buffer, so it is safe to call with the lock
	// held. sysmon holds the same lock while it reads the state
	// this sets up.
	defaultGOMAXPROCSInit()
	faststartState.cgroupProbed = true
	unlock(&computeMaxProcsLock)

	// schedinit's call to defaultGOMAXPROCSInit relies on
	// internal/godebug registration to report the result. That may
	// already have happened.
	defaultGOMAXPROCSUpdateGODEBUG()
}

// faststartDefaultGOMAXPROCS sets GOMAXPROCS to its usual default value,
// which schedinit did not do in fast start mode, unless the program has
// chosen a value of its own in the meantime.
//
// It stops the world, so the caller must be able to block and must not
// hold gcsema or worldsema.
func faststartDefaultGOMAXPROCS() {
	// Synchronize with GOMAXPROCS. See comment on computeMaxProcsLock.
	lock(&computeMaxProcsLock)
	lock(&sched.lock)
	custom := sched.customGOMAXPROCS
	curr := gomaxprocs
	unlock(&sched.lock)
	if custom {
		unlock(&computeMaxProcsLock)
		return
	}
	procs := defaultGOMAXPROCS(0)
	unlock(&computeMaxProcsLock)
	if procs == curr {
		return
	}

	stw := stopTheWorldGC(stwGOMAXPROCS)

	// Still OK to update?
	lock(&sched.lock)
	custom = sched.customGOMAXPROCS
	unlock(&sched.lock)
	if !custom {
		// newprocs will be processed by startTheWorld.
		newprocs = procs
	}

	startTheWorldGC(stw)
}

// faststartSizeClass maps each size class to the size class that fast
// start mode allocates from instead: the smallest one that is at least as
// large and whose size is a power of two.
//
// Every span class that a program allocates from needs a span of its own,
// and the first allocation from a span touches a page of the heap, and a
// good deal of heap metadata, for the first time. A short-lived program
// typically makes a handful of allocations from each of a large number of
// size classes, so it pays that cost over and over for spans it then
// leaves almost empty. Folding the 67 size classes into 13 makes such a
// program touch fewer pages, at the cost of more internal fragmentation.
//
// The mapping preserves the properties of the size classes that the
// allocator relies on. In particular, an object is small enough to keep
// its pointer bitmap in its span (see heapBitsInSpan) if and only if the
// same is true of the size class it is mapped to.
//
// The table is checked by TestFaststartSizeClass.
var faststartSizeClass = [gc.NumSizeClasses]uint8{
	0, 1, 2, 4, 4, 6, 6, 10, 10, 10, 10, 18,
	18, 18, 18, 18, 18, 18, 18, 26, 26, 26, 26, 26,
	26, 26, 26, 32, 32, 32, 32, 32, 32, 38, 38, 38,
	38, 38, 38, 44, 44, 44, 44, 44, 44, 51, 51, 51,
	51, 51, 51, 51, 59, 59, 59, 59, 59, 59, 59, 59,
	67, 67, 67, 67, 67, 67, 67, 67,
}
