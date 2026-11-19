// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build faststart

package runtime

import "unsafe"

// The compiler calls the size-specialized malloc functions directly for
// allocations whose size it knows. Those functions have the size class of
// the allocation built in to them. Fast start mode allocates from a
// reduced set of size classes (see faststartSizeClass), so it replaces the
// generated functions in malloc_generated.go with ones that go through
// mallocgc, which picks the size class at run time.
//
// The set of functions here must match the set that mkmalloc generates.

func mallocgcSmallScanNoHeaderSC1(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	return mallocgc(size, typ, needzero)
}

func mallocgcSmallScanNoHeaderSC2(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	return mallocgc(size, typ, needzero)
}

func mallocgcSmallScanNoHeaderSC3(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	return mallocgc(size, typ, needzero)
}

func mallocgcSmallScanNoHeaderSC4(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	return mallocgc(size, typ, needzero)
}

func mallocgcSmallScanNoHeaderSC5(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	return mallocgc(size, typ, needzero)
}

func mallocgcSmallScanNoHeaderSC6(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	return mallocgc(size, typ, needzero)
}

func mallocgcSmallScanNoHeaderSC7(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	return mallocgc(size, typ, needzero)
}

func mallocgcTinySC2(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	return mallocgc(size, typ, needzero)
}

func mallocgcSmallNoScanSC2(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	return mallocgc(size, typ, needzero)
}

func mallocgcSmallNoScanSC3(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	return mallocgc(size, typ, needzero)
}

func mallocgcSmallNoScanSC4(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	return mallocgc(size, typ, needzero)
}

func mallocgcSmallNoScanSC5(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	return mallocgc(size, typ, needzero)
}

func mallocgcSmallNoScanSC6(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	return mallocgc(size, typ, needzero)
}

func mallocgcSmallNoScanSC7(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	return mallocgc(size, typ, needzero)
}

// mallocgcTinySlowPath is referenced by the templates in malloc_stubs.go.
func mallocgcTinySlowPath(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	return mallocgc(size, typ, needzero)
}

// The tables that mallocgc uses to find a size-specialized malloc function
// are never consulted in fast start mode.
// See sizeSpecializedMallocEnabled.
var (
	mallocScanTable   []func(size uintptr, typ *_type, needzero bool) unsafe.Pointer
	mallocNoScanTable []func(size uintptr, typ *_type, needzero bool) unsafe.Pointer
)
