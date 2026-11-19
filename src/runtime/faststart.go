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
//   - Only one P is created, however many CPUs the machine has. Each P
//     is a sizable heap allocation that has to be initialized, so on a
//     large machine creating them all dominates the cost of starting up.
//
// The last two mean that, unless the GOMAXPROCS environment variable is
// set, the program starts running with a single P. It moves to the usual
// default value of GOMAXPROCS, including any cgroup CPU limit, as soon
// as it could make a difference: when the program starts a goroutine,
// calls [GOMAXPROCS] or [SetDefaultGOMAXPROCS], or is about to start its
// first GC cycle. See faststartDefaultGOMAXPROCS.
//
// Starting those goroutines eagerly is expensive mostly because each one
// wakes an idle P, which creates an OS thread.
//
// sysmon is not started until the program first does something that
// could need it. See faststartSysmon. Together with the above, this
// means that a program that does not start goroutines or set timers
// runs main.main with no threads other than the main thread, and so
// also exits without having to stop any.
//
// A few smaller things are skipped altogether. The runtime does not ask
// the kernel for the transparent huge page size, so it does not manage
// huge pages (see osinit), and without cgo it does not copy the
// environment into the heap (see goenvs_unix).
//
// Fast start mode also changes the heap so that fewer pages are touched
// for the first time during startup. See faststartSizeClass.

// faststartState holds the state of the work that fast start mode
// defers.
var faststartState struct {
	// deferred is the state of faststartDeferred: one of the
	// faststartDeferred* constants.
	deferred atomic.Uint32

	// procs is the state of the default value of GOMAXPROCS: one of
	// the faststartProcs* constants.
	procs atomic.Uint32

	// single is 0 until the program starts a goroutine of its own,
	// other than the main goroutine.
	single atomic.Uint32

	// sysmon is 1 once sysmon has been started.
	sysmon atomic.Uint32

	// cgroupProbed is set once the deferred defaultGOMAXPROCSInit
	// has run. Protected by computeMaxProcsLock.
	cgroupProbed bool
}

const (
	// GOMAXPROCS has its usual default value, or one the program
	// chose. This is the zero value because it is the state when the
	// GOMAXPROCS environment variable is set.
	faststartProcsDone uint32 = iota
	// schedinit set GOMAXPROCS to 1 instead of its usual default.
	faststartProcsDeferred
	// As above, and newproc has started a goroutine to fix that.
	faststartProcsStarting
)

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

	// The GC relies on sysmon to wake the scavenger and to force
	// periodic cycles.
	faststartSysmon()

	// The sweeper and scavenger must be parked and ready to be woken
	// before the cycle reaches sweep termination.
	gcStartBackground()

	// Periodic GC can only trigger after a first cycle completes.
	go forcegchelper()

	// Switch to the usual default value of GOMAXPROCS, if that has
	// not happened already, then start keeping it up to date.
	faststartDefaultGOMAXPROCS()
	defaultGOMAXPROCSUpdateEnable()

	faststartState.deferred.Store(faststartDeferredDone)
	return true
}

// faststartSysmon starts sysmon if it is not already running.
//
// In fast start mode sysmon is not started by runtime.main, which saves
// creating a thread, and tearing it down again at exit, in a program
// that never needs it. A program whose only goroutine is the main
// goroutine, and which has not yet run a GC cycle, needs nothing that
// sysmon does: there is no other goroutine that the main goroutine
// could be keeping from running, either by not yielding or by holding
// its P in a system call. So sysmon is started by the first thing that
// could change that:
//
//   - a go statement (newproc) that starts anything but one of the
//     runtime's own goroutines, once runtime.main is running;
//   - a timer being set (timer.modify), since timers are run by Ps;
//   - the network poller being initialized (netpollGenericInit);
//   - the first GC cycle (faststartDeferred).
//
// It is started unconditionally if the program uses cgo, since C code
// can call into Go on threads the runtime knows nothing about, or if
// GODEBUG=schedtrace is set.
//
// The caller must have a P, and must be able to allocate and to acquire
// sched.lock. It may be running on a user stack or on the system stack.
func faststartSysmon() {
	if !haveSysmon || faststartState.sysmon.Load() != 0 {
		return
	}
	if !faststartState.sysmon.CompareAndSwap(0, 1) {
		return
	}
	systemstack(func() {
		newm(sysmon, nil, -1)
	})
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
// unless the program has chosen a value of its own in the meantime.
//
// In fast start mode schedinit does not consult the cgroup CPU limit and
// creates a single P. This function does both of the things it left out.
// It is called, whichever comes first,
//
//   - on a goroutine of its own, when the program first starts a
//     goroutine (see newproc);
//   - by [GOMAXPROCS], so that the program does not normally observe
//     the temporary value;
//   - by faststartDeferred, when the first GC cycle is about to start.
//
// It stops the world, so the caller must be able to block and must not
// hold gcsema or worldsema. It is safe to call more than once, and from
// more than one goroutine at a time.
func faststartDefaultGOMAXPROCS() {
	if faststartState.procs.Load() == faststartProcsDone {
		return
	}
	faststartCgroupProbe()
	defer faststartState.procs.Store(faststartProcsDone)

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
