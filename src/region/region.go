// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package region provides goroutine-local memory regions (prototype).
//
// Do runs a function in a region: heap allocations the function and its
// callees make on the calling goroutine are placed in blocks owned by the
// region and are freed at once when Do returns, except for objects that
// were observed to escape (stored somewhere outside the region, sent on
// a channel, captured by a goroutine, given a finalizer, ...), which the
// garbage collector takes over. Escapes are detected by the runtime, so
// correct programs stay correct; the region only changes when memory is
// reclaimed.
//
// See runtime/region.go for the implementation and doc/gc-gap for the
// study this prototype belongs to.
package region

import "unsafe"

// Do runs f in a memory region on the calling goroutine. If the goroutine
// is already in a region, f simply runs in it.
func Do(f func()) {
	runtime_regionDo(f)
}

// Ignore runs f with its allocations placed outside the current region,
// as ordinary heap objects. Use it around code that builds data meant to
// outlive the region, such as filling a cache.
func Ignore(f func()) {
	runtime_regionIgnore(f)
}

// Inherit runs f with its allocations placed where the object p lives:
// in the region if p is a live object of the current region, a stack
// variable, or nil; outside the region otherwise. Decoders use it so
// that what they attach to a target outside the region is allocated
// outside the region too.
func Inherit(p unsafe.Pointer, f func()) {
	runtime_regionInherit(p, f)
}

// Active reports whether the calling goroutine is inside a region.
func Active() bool {
	return runtime_regionActive()
}

// Owns reports whether p points into a live object of the calling
// goroutine's region.
func Owns(p unsafe.Pointer) bool {
	return runtime_regionOwns(p)
}

// Stats are cumulative counters about regions.
type Stats struct {
	Regions      uint64 // completed Do calls that created a region
	FreedRegions uint64 // regions whose memory was freed eagerly
	FreedObjects uint64 // objects freed eagerly
	FreedBytes   uint64 // bytes of lines freed eagerly
	FadedObjects uint64 // objects that escaped their region
	FadedBytes   uint64
	Abandoned    uint64 // regions handed to the GC (panic, GC marking, overflow)
	Blocks       uint64 // 64 KB blocks taken from the heap
	BarrierCalls uint64 // store barrier slow-path calls (GODEBUG=regiontrace=1 only)
}

// ReadStats returns the current statistics.
func ReadStats() Stats {
	var v [9]uint64
	runtime_regionReadStats(&v)
	return Stats{v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7], v[8]}
}

//go:linkname runtime_regionDo
//go:noescape
func runtime_regionDo(f func())

//go:linkname runtime_regionIgnore
//go:noescape
func runtime_regionIgnore(f func())

//go:linkname runtime_regionInherit
//go:noescape
func runtime_regionInherit(p unsafe.Pointer, f func())

//go:linkname runtime_regionActive
func runtime_regionActive() bool

//go:linkname runtime_regionOwns
func runtime_regionOwns(p unsafe.Pointer) bool

//go:linkname runtime_regionReadStats
func runtime_regionReadStats(out *[9]uint64)
