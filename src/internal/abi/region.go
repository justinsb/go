// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package abi

import "internal/goarch"

// GRegionOffset is the offset of the g.region field within runtime.g.
// The compiler loads it in the write barrier sequence to decide whether
// a goroutine is inside a memory region (see runtime/region.go). The
// runtime checks that the offset matches the struct layout.
const GRegionOffset = 4 * goarch.PtrSize

// GRegionIgnoreOffset is the offset of g.regionIgnore, the depth of
// region.Ignore scopes; nonzero means allocations bypass the region.
const GRegionIgnoreOffset = GRegionOffset + goarch.PtrSize

// Layout the compiler's inline region allocation relies on; the runtime
// checks each against its structs (runtime/region.go).
const (
	RegionCursorOffset    = 0              // regionState.cursor
	RegionFastLimitOffset = goarch.PtrSize // regionState.fastLimit
	RegionBlockBytes      = 64 << 10       // a region block, 64 KB aligned
	RegionHdrBytes        = 4096           // block header; object data follows
	RegionHdrStartOffset  = 0              // regionBlockHdr.start, one bit per data word
	RegionHdrPtrOffset    = 960            // regionBlockHdr.ptr: after the 960-byte start bitmap
	RegionMaxObject       = 16 << 10
)
