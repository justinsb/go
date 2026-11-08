// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package region provides goroutine-local memory regions (prototype).
//
// Do runs a function in a region: small heap allocations made by the
// function and its callees on the same goroutine are placed in memory
// owned by the region, and when Do returns, every such allocation that
// did not become reachable from outside the region is freed immediately,
// without a garbage collection cycle. Allocations that escape (stored
// into a global, into an object that outlives the region, sent on a
// channel, captured by a go statement, ...) are detected at the point
// of escape and handed to the garbage collector, so programs keep their
// ordinary memory-safety guarantees regardless of how regions are used.
//
// Regions are not inherited by goroutines started inside them and do
// not span goroutines. Nested Do calls run inside the outer region.
//
// The implementation is described in runtime/region.go.
package region

import _ "unsafe" // for linkname

// Do runs f in a new memory region on the current goroutine.
func Do(f func()) {
	runtime_regionDo(f)
}

// Ignore runs f with its allocations excluded from the current region,
// if any. It is a hint for memory that is known to outlive the region,
// such as entries added to a cache; without it the memory is still
// handled correctly, at the cost of detecting the escape dynamically.
func Ignore(f func()) {
	runtime_regionIgnore(f)
}

// Active reports whether the current goroutine is inside a region.
func Active() bool {
	return runtime_regionActive()
}

// Stats are cumulative counters about regions in this process.
type Stats struct {
	Regions      uint64 // completed Do calls that created a region
	FreedRegions uint64 // regions whose memory was freed eagerly
	FreedObjects uint64 // objects freed eagerly
	FreedBytes   uint64 // bytes freed eagerly (allocation slot sizes)
	FadedObjects uint64 // objects that escaped their region
	FadedBytes   uint64
	Abandoned    uint64 // regions handed to the GC (panic, GC marking, overflow)
	Flushes      uint64 // region caches flushed (GC cycles and abandonments)
	SpanResets   uint64 // spans reset at region exit
}

// ReadStats returns the current counters.
func ReadStats() Stats {
	var v [9]uint64
	runtime_regionReadStats(&v)
	return Stats{v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7], v[8]}
}

// Implemented in the runtime. The runtime only calls f and does not
// retain it, so f is marked noescape: closures passed to Do and Ignore
// can then live on the stack, and variables they capture stay in the
// caller's frame instead of being moved to the heap (where a store of a
// region pointer into them would count as an escape from the region).

//go:noescape
//go:linkname runtime_regionDo
func runtime_regionDo(f func())

//go:noescape
//go:linkname runtime_regionIgnore
func runtime_regionIgnore(f func())

//go:linkname runtime_regionActive
func runtime_regionActive() bool

//go:linkname runtime_regionReadStats
func runtime_regionReadStats(out *[9]uint64)
