// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Goroutine-local memory regions (prototype).
//
// A region is a dynamic extent of one goroutine (region.Do) during which
// small heap allocations are served from a private allocation cache
// whose spans are tagged with the region. When Do returns, every object
// allocated in the region that was not observed to escape is freed in
// bulk by resetting the spans' allocation bitmaps, so the memory is
// reused by the goroutine's next region without waiting for a GC cycle.
//
// Escape detection ("fading") is dynamic. The compiler emits a call to
// wbRegion before every pointer store that may target heap memory when
// g.region is non-nil, and the runtime's bulk copy paths do the same.
// If the stored pointer refers to a region object and the destination
// slot is not itself a (non-faded) region object, nor a stack slot in a
// frame younger than the Do call, the object and everything reachable
// from it inside the region are marked faded. Faded objects are left to
// the garbage collector; they are ordinary heap objects from then on.
//
// Other ways for a region pointer to become visible outside the goroutine
// are handled explicitly: the closure of a go statement (newproc1),
// objects given specials (finalizers, cleanups, weak pointers, profiling),
// and panic values that propagate out of Do. A region whose fade work
// stack overflows, or that ends while the GC is marking, or that ends
// by panicking, frees nothing: its objects are handed to the GC.
//
// Region spans use the ordinary mcache/mcentral protocol. At GC mark
// termination all region caches are flushed (regionFlushAll), which
// demotes any objects allocated so far to ordinary heap objects; the
// barrier then stops considering them region objects, which is safe
// because their lifetime is now the GC's responsibility.

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

// Check that the compiler and the runtime agree on the offset of g.region.
const _ = uint(unsafe.Offsetof(g{}.region) - abi.GRegionOffset)
const _ = uint(abi.GRegionOffset - unsafe.Offsetof(g{}.region))

const (
	// regionStackSize is the capacity of the transitive fade work stack.
	// Overflow poisons the region (nothing is freed at exit).
	regionStackSize = 2048

	// regionBitsBytes is the size of the per-span region bitmaps. Spans
	// have at most 8192/8 = 1024 objects, so 128 bytes always suffice.
	regionBitsBytes = 128
)

// regionState is the per-goroutine region state. It lives in
// non-GC'd memory and is reused across region.Do calls on the same
// goroutine.
type regionState struct {
	_ sys.NotInHeap

	c     *mcache // private allocation cache; its spans have s.region == this
	owner *g
	next  *regionState // in allRegions

	// stackOffset is gp.stack.hi minus the stack pointer at the Do call.
	// Stack slots at a smaller offset (younger frames) are inside the
	// region; older frames are outside.
	stackOffset uintptr

	active   bool  // inside region.Do
	poisoned bool  // free nothing at exit
	ignore   int32 // depth of region.Ignore; allocations go to the P's cache

	// full holds spans of the region that became full during the
	// region (they stay owned by the region rather than being returned
	// to the mcentral).
	full *mspan

	// Transitive fade work stack of object base addresses.
	sp    int
	stack [regionStackSize]uintptr

	// Per-region statistics, added to the global counters at exit.
	allocs, allocBytes uint64
	faded, fadedBytes  uint64
}

var (
	allRegions     *regionState // all live region states, for GC-time flushing
	allRegionsLock mutex

	regionFreeList *regionState // released states for reuse

	// regionStats are cumulative counters, read by region.Stats.
	regionStats struct {
		regions      atomic.Uint64 // completed region.Do calls
		freedRegions atomic.Uint64 // regions that freed eagerly
		freedObjects atomic.Uint64
		freedBytes   atomic.Uint64
		faded        atomic.Uint64
		fadedBytes   atomic.Uint64
		poisoned     atomic.Uint64 // regions abandoned (overflow, panic, GC marking)
		flushes      atomic.Uint64 // caches flushed at GC
		spanResets   atomic.Uint64
	}
)

// regionsEnabled reports whether region.Do should create regions.
func regionsEnabled() bool { return debug.regions != 0 }

// regionDo runs f in a new region of the current goroutine.
//
//go:linkname regionDo region.runtime_regionDo
func regionDo(f func()) {
	gp := getg()
	if !regionsEnabled() || gp.region != nil || gp.m.curg != gp {
		// Disabled, nested (the outer region covers f), or not a user goroutine.
		f()
		return
	}
	r := gp.regionCache
	if r == nil {
		r = newRegionState(gp)
		gp.regionCache = r
	}
	regionBegin(gp, r, sys.GetCallerSP())
	defer regionEnd(gp, r)
	f()
}

// regionIgnore runs f with allocations excluded from the current
// region (they go to the ordinary heap). The escape barrier stays active.
//
//go:linkname regionIgnore region.runtime_regionIgnore
func regionIgnore(f func()) {
	gp := getg()
	r := gp.region
	if r == nil {
		f()
		return
	}
	// No defer: a panic abandons the region anyway (regionEnd), and
	// regionBegin resets ignore.
	r.ignore++
	f()
	r.ignore--
}

// regionActive reports whether the current goroutine is inside a region.
//
//go:linkname regionActive region.runtime_regionActive
//go:linkname sync_runtime_regionActive sync.runtime_regionActive
func regionActive() bool {
	return getg().region != nil
}

func sync_runtime_regionActive() bool { return getg().region != nil }

// regionReadStats copies the global counters.
//
//go:linkname regionReadStats region.runtime_regionReadStats
func regionReadStats(out *[9]uint64) {
	out[0] = regionStats.regions.Load()
	out[1] = regionStats.freedRegions.Load()
	out[2] = regionStats.freedObjects.Load()
	out[3] = regionStats.freedBytes.Load()
	out[4] = regionStats.faded.Load()
	out[5] = regionStats.fadedBytes.Load()
	out[6] = regionStats.poisoned.Load()
	out[7] = regionStats.flushes.Load()
	out[8] = regionStats.spanResets.Load()
}

func newRegionState(gp *g) *regionState {
	lock(&allRegionsLock)
	r := regionFreeList
	if r != nil {
		regionFreeList = r.next
	}
	unlock(&allRegionsLock)
	if r == nil {
		r = (*regionState)(persistentalloc(unsafe.Sizeof(regionState{}), 64, &memstats.other_sys))
	}
	r.c = allocmcache()
	r.c.region = r
	r.owner = gp
	r.full = nil
	r.sp = 0
	r.poisoned = false
	r.ignore = 0
	lock(&allRegionsLock)
	r.next = allRegions
	allRegions = r
	unlock(&allRegionsLock)
	return r
}

// regionRelease frees the region state of a dying goroutine. Called
// from gdestroy on the system stack.
func regionRelease(gp *g) {
	r := gp.regionCache
	if r == nil {
		return
	}
	gp.regionCache = nil
	gp.region = nil
	lock(&allRegionsLock)
	for pp := &allRegions; *pp != nil; pp = &(*pp).next {
		if *pp == r {
			*pp = r.next
			break
		}
	}
	unlock(&allRegionsLock)
	r.flush()
	freemcache(r.c)
	r.c = nil
	r.owner = nil
	lock(&allRegionsLock)
	r.next = regionFreeList
	regionFreeList = r
	unlock(&allRegionsLock)
}

func regionBegin(gp *g, r *regionState, sp uintptr) {
	r.stackOffset = gp.stack.hi - sp
	r.active = true
	r.poisoned = false
	r.sp = 0
	r.ignore = 0
	r.allocs, r.allocBytes, r.faded, r.fadedBytes = 0, 0, 0, 0
	// Publish last: from here on the barrier and allocator consult r.
	gp.region = r
}

// regionEnd ends the region: frees unfaded objects if it is safe to do
// so, otherwise hands everything to the GC.
func regionEnd(gp *g, r *regionState) {
	// Panic values (and anything they reference) propagate past the
	// region. Rather than fading them precisely, abandon the region.
	if gp._panic != nil {
		r.poisoned = true
	}

	mp := acquirem()
	mp.mallocing = 1 // keep the GC from starting underneath us
	regionStats.regions.Add(1)
	regionStats.faded.Add(int64(r.faded))
	regionStats.fadedBytes.Add(int64(r.fadedBytes))
	if r.poisoned || gcphase != _GCoff || writeBarrier.enabled {
		// Freeing while the collector is marking could let a grey
		// object be scanned after its slot was reused. Keep the
		// objects (they become ordinary heap objects for the GC to
		// reclaim) but keep the spans cached for the next region.
		regionStats.poisoned.Add(1)
		gp.region = nil
		r.active = false
		r.abandonAll()
		mp.mallocing = 0
		releasem(mp)
		return
	}
	gp.region = nil
	r.active = false
	r.scanOuterStack(gp)
	r.freeAll()
	mp.mallocing = 0
	releasem(mp)
}

// scanOuterStack fades every region object that a word in the frames
// older than the Do call points to. Such pointers got there through
// stores into the goroutine's own stack, which the barrier does not
// act on (see regionCheckStore). The scan is conservative: a non-pointer
// word that looks like a region pointer only keeps an object alive.
func (r *regionState) scanOuterStack(gp *g) {
	lo := gp.stack.hi - r.stackOffset
	if lo < gp.stack.lo || lo > gp.stack.hi {
		throw("region: bad stack offset")
	}
	for p := lo; p < gp.stack.hi; p += goarch.PtrSize {
		w := *(*uintptr)(unsafe.Pointer(p))
		if w == 0 {
			continue
		}
		if s, idx, ok := r.isRegionObject(w); ok && s.isAllocatedIndex(idx) {
			if debug.regiontrace > 0 {
				print("region: fade-stack obj=", hex(w), " size=", s.elemsize, " slot=", hex(p), "\n")
			}
			r.fade(s, idx)
		}
	}
}

// tag marks s as owned by r. Objects already allocated in s (from
// before the region acquired it) are recorded as foreign.
func (r *regionState) tag(s *mspan) {
	if s.regionBase == nil {
		s.regionBase = (*gcBits)(persistentalloc(regionBitsBytes, 8, &memstats.other_sys))
		s.regionFaded = (*gcBits)(persistentalloc(regionBitsBytes, 8, &memstats.other_sys))
	}
	// Copy whole 64-bit words: allocBits is allocated in 8-byte blocks
	// (newMarkBits), and resetSpan works on words. Bits at or beyond
	// nelems are masked off there.
	nbytes := regionBitsWords(s.nelems) * 8
	memmove(unsafe.Pointer(s.regionBase), unsafe.Pointer(s.allocBits), nbytes)
	memclrNoHeapPointers(unsafe.Pointer(s.regionFaded), nbytes)
	// allocBits is a snapshot from the last sweep: objects allocated
	// since then are not recorded in it, they are implied by being
	// below freeindex (see mspan.isFree). Everything below freeindex is
	// allocated and foreign to the region.
	bw := (*[regionBitsBytes / 8]uint64)(unsafe.Pointer(s.regionBase))
	fi := uintptr(s.freeindex)
	for i := uintptr(0); i < fi/64; i++ {
		bw[i] = ^uint64(0)
	}
	if fi%64 != 0 {
		bw[fi/64] |= 1<<(fi%64) - 1
	}
	s.region = r
}

// regionBitsWords returns the number of 64-bit words needed for a
// bitmap of nelems objects.
func regionBitsWords(nelems uint16) uintptr {
	return (uintptr(nelems) + 63) / 64
}

// untag releases s from r.
func untagRegionSpan(s *mspan) {
	s.region = nil
}

// isRegionObject reports whether p points into a non-foreign, non-faded
// object of r. It returns the span and object index on success.
//
//go:nosplit
func (r *regionState) isRegionObject(p uintptr) (*mspan, uintptr, bool) {
	s := spanOf(p)
	if s == nil || s.region != r {
		return nil, 0, false
	}
	if s.state.get() != mSpanInUse || p < s.base() || p >= s.limit {
		return nil, 0, false
	}
	idx := s.objIndex(p)
	if bp, m := s.regionBase.bitp(idx); *bp&m != 0 {
		return nil, 0, false
	}
	if bp, m := s.regionFaded.bitp(idx); *bp&m != 0 {
		return nil, 0, false
	}
	return s, idx, true
}

// isAllocated reports whether object idx of s is currently allocated
// (see mspan.isFree): either below freeindex or marked in allocBits.
// Pointers found by conservative scanning must pass this check before
// their referent is treated as an object.
//
//go:nosplit
func (s *mspan) isAllocatedIndex(idx uintptr) bool {
	if idx < uintptr(s.freeindex) {
		return true
	}
	bp, m := s.allocBits.bitp(idx)
	return *bp&m != 0
}

// wbRegion is called by compiled code after storing ptr into *slot
// while the goroutine is inside a region. If ptr is a region object and
// slot is outside the region, ptr is faded.
//
// The compiler calls this with the arguments in registers as uintptrs;
// see cmd/compile/internal/ssa/writebarrier.go. It is nosplit so that
// the common case (ptr is not an object of this region, or both ptr
// and slot are) costs a span lookup and a few loads, with no
// preemption bookkeeping.
//
//go:nosplit
func wbRegion(slot *uintptr, ptr uintptr) {
	gp := getg()
	r := gp.region
	if r == nil || ptr == 0 {
		return
	}
	s := spanOf(ptr)
	if s == nil || s.region != r {
		return
	}
	wbRegionSlow(gp, r, uintptr(unsafe.Pointer(slot)), ptr)
}

func wbRegionSlow(gp *g, r *regionState, slot, ptr uintptr) {
	mp := acquirem()
	if debug.regiontrace > 0 {
		regionTracePC = sys.GetCallerPC()
	}
	regionCheckStore(gp, r, slot, ptr)
	releasem(mp)
}

// regionTracePC is the PC of the store being checked, for GODEBUG=regiontrace=1.
var regionTracePC uintptr

// regionCheckStore implements the escape check for storing ptr into slot.
// A slot of 0 means an unknown destination outside the region.
// Must run with preemption disabled.
func regionCheckStore(gp *g, r *regionState, slot, ptr uintptr) {
	s, idx, ok := r.isRegionObject(ptr)
	if debug.regiontrace > 1 {
		sp := spanOf(ptr)
		print("region: store slot=", hex(slot), " ptr=", hex(ptr), " regionobj=", ok)
		if sp != nil {
			print(" span.region=", sp.region, " r=", r, " state=", sp.state.get())
		} else {
			print(" nospan")
		}
		print(" pc=", hex(regionTracePC), "\n")
	}
	if !ok {
		return
	}
	// Destination inside this goroutine's stack? No other goroutine can
	// read our stack, so nothing needs to happen now. Pointers left in
	// frames older than the Do call are found by scanOuterStack when
	// the region ends.
	if gp.stack.lo <= slot && slot < gp.stack.hi {
		return
	}
	// Destination inside a (non-faded) region object?
	if _, _, ok := r.isRegionObject(slot); ok {
		return
	}
	if debug.regiontrace > 0 {
		print("region: fade obj=", hex(ptr), " size=", s.elemsize, " noscan=", s.spanclass.noscan(), " slot=", hex(slot))
		if ss := spanOf(slot); ss != nil && ss.state.get() == mSpanInUse {
			print(" (heap object size=", ss.elemsize, ")")
		} else {
			print(" (global or non-heap)")
		}
		print(" pc=", hex(regionTracePC))
		if debug.regiontrace > 2 {
			var pcs [8]uintptr
			n := callers(3, pcs[:])
			print(" stack=")
			for i := 0; i < n; i++ {
				print(hex(pcs[i]), ",")
			}
		}
		print("\n")
	}
	r.fade(s, idx)
}

// wbRegionMove is called by compiled code before a typed bulk copy of
// typ from src to dst while the goroutine is inside a region.
func wbRegionMove(typ *_type, dst, src unsafe.Pointer) {
	gp := getg()
	r := gp.region
	if r == nil {
		return
	}
	mp := acquirem()
	regionBulkCheck(gp, r, uintptr(dst), uintptr(src), typ.Size_, typ)
	releasem(mp)
}

// regionBulkCheck checks every pointer word of [src, src+size) that is
// about to be copied to the corresponding word of dst. typ describes the
// layout if non-nil; otherwise every aligned word is treated as a
// potential pointer (conservative, which only fades more). A dst of 0
// means the destination is unknown and outside the region.
// Must run with preemption disabled.
func regionBulkCheck(gp *g, r *regionState, dst, src, size uintptr, typ *_type) {
	if typ != nil && !typ.Pointers() {
		return
	}
	if typ != nil {
		// Walk the type's pointer mask, tiling over array elements.
		ptrmask := getGCMask(typ)
		for off := uintptr(0); off < size; off += typ.Size_ {
			for i := uintptr(0); i < typ.PtrBytes; i += goarch.PtrSize {
				if *addb(ptrmask, i/goarch.PtrSize/8)>>(i/goarch.PtrSize%8)&1 == 0 {
					continue
				}
				p := *(*uintptr)(unsafe.Pointer(src + off + i))
				if p != 0 {
					regionCheckStore(gp, r, dst+off+i, p)
				}
			}
			if typ.Size_ == 0 {
				break
			}
		}
		return
	}
	for i := uintptr(0); i < size; i += goarch.PtrSize {
		p := *(*uintptr)(unsafe.Pointer(src + i))
		if p != 0 {
			regionCheckStore(gp, r, dst+i, p)
		}
	}
}

// regionFadePtr fades the region object p points into, if any. Used for
// pointers that become visible outside the goroutine through paths the
// write barrier does not see (go statements, specials, finalizer funcs).
func regionFadePtr(gp *g, p uintptr) {
	r := gp.region
	if r == nil || p == 0 {
		return
	}
	mp := acquirem()
	if s, idx, ok := r.isRegionObject(p); ok {
		if debug.regiontrace > 0 {
			print("region: fade-ptr obj=", hex(p), " size=", s.elemsize, " noscan=", s.spanclass.noscan(), " pc=", hex(sys.GetCallerPC()), "\n")
		}
		r.fade(s, idx)
	}
	releasem(mp)
}

// fade marks object idx of span s, and transitively every region object
// reachable from it, as faded. Must run with preemption disabled.
func (r *regionState) fade(s *mspan, idx uintptr) {
	r.markFaded(s, idx)
	r.push(s.base() + idx*s.elemsize)
	pops := 0
	for r.sp > 0 {
		if pops++; pops > 1<<24 {
			print("region: fade outer loop, sp=", r.sp, " faded=", r.faded, " poisoned=", r.poisoned, "\n")
			throw("region: fade outer loop")
		}
		r.sp--
		base := r.stack[r.sp]
		sp := spanOf(base)
		if sp.spanclass.noscan() {
			continue
		}
		if debug.regiontrace > 1 {
			print("region:   walk obj=", hex(base), " size=", sp.elemsize, " spc=", sp.spanclass, " objBase=", hex(sp.objBase(base)), " word0=", hex(*(*uintptr)(unsafe.Pointer(sp.objBase(base)))))
			if heapBitsInSpan(sp.elemsize) {
				print(" hbits=", hex(sp.heapBitsSmallForAddr(sp.objBase(base))), " nelems=", sp.nelems, " npages=", sp.npages)
			}
			print("\n")
		}
		// Enumerate the pointer words of the object.
		tp := sp.typePointersOfUnchecked(sp.objBase(base))
		limit := sp.objBase(base) + sp.elemsize
		guard := 0
		for {
			var addr uintptr
			if tp, addr = tp.next(limit); addr == 0 {
				break
			}
			if guard++; guard > 1<<20 {
				print("region: fade loop on obj=", hex(base), " objBase=", hex(sp.objBase(base)), " elemsize=", sp.elemsize, " spc=", sp.spanclass, " addr=", hex(addr), " limit=", hex(limit), " tp.typ=", tp.typ, " tp.elem=", hex(tp.elem), " tp.addr=", hex(tp.addr), "\n")
				throw("region: fade loop")
			}
			q := *(*uintptr)(unsafe.Pointer(addr))
			if q == 0 {
				continue
			}
			if qs, qidx, ok := r.isRegionObject(q); ok {
				if debug.regiontrace > 1 {
					print("region:   fade-transitive obj=", hex(qs.base()+qidx*qs.elemsize), " via=", hex(addr), " size=", qs.elemsize, " noscan=", qs.spanclass.noscan(), "\n")
				}
				r.markFaded(qs, qidx)
				r.push(qs.base() + qidx*qs.elemsize)
			}
		}
	}
}

func (r *regionState) markFaded(s *mspan, idx uintptr) {
	bp, m := s.regionFaded.bitp(idx)
	*bp |= m
	if debug.regiontrace > 0 {
		print("region:   markFaded span=", hex(s.base()), " idx=", idx, " elemsize=", s.elemsize, " faded=", s.regionFaded, " byte=", hex(*bp), " region=", s.region, "\n")
	}
	r.faded++
	r.fadedBytes += uint64(s.elemsize)
}

func (r *regionState) push(p uintptr) {
	if r.poisoned {
		return
	}
	if r.sp == len(r.stack) {
		// Give up on precision: keep everything.
		r.poisoned = true
		r.sp = 0
		return
	}
	r.stack[r.sp] = p
	r.sp++
}

// refillFull is called by mcache.refill when the region's current span
// for a class is full: keep it owned by the region instead of returning
// it to the mcentral.
func (r *regionState) keepFull(s *mspan) {
	s.regionNext = r.full
	r.full = s
}

// freeAll frees every unfaded region object in the region's spans and
// prepares the spans for reuse by the next region. Must run with
// preemption disabled and gcphase == _GCoff.
func (r *regionState) freeAll() {
	c := r.c
	// Reusable (freegc) lists point into slots we are about to free.
	clear(c.reusableNoscan[:])
	c.tiny = 0
	c.tinyoffset = 0

	freedObjs, freedBytes := uint64(0), uint64(0)
	reset := func(s *mspan) {
		n, b := r.resetSpan(s)
		freedObjs += n
		freedBytes += b
	}
	for i := range c.alloc {
		if s := c.alloc[i]; s != &emptymspan && s.region == r {
			reset(s)
		}
	}
	// Full spans: reset, then either keep one per class as the current
	// span (preferring the emptiest), or return the rest.
	for s := r.full; s != nil; {
		next := s.regionNext
		s.regionNext = nil
		reset(s)
		spc := s.spanclass
		cur := c.alloc[spc]
		if cur == &emptymspan || cur.allocCount > s.allocCount {
			if cur != &emptymspan {
				r.returnSpan(cur)
			}
			c.alloc[spc] = s
		} else {
			r.returnSpan(s)
		}
		s = next
	}
	r.full = nil
	regionStats.freedRegions.Add(1)
	regionStats.freedObjects.Add(int64(freedObjs))
	regionStats.freedBytes.Add(int64(freedBytes))
}

// abandonAll turns every object allocated in the region so far into a
// foreign object (never freed by a region), without releasing the
// spans. Must run with preemption disabled.
func (r *regionState) abandonAll() {
	c := r.c
	clear(c.reusableNoscan[:])
	c.tiny = 0
	c.tinyoffset = 0
	for i := range c.alloc {
		if s := c.alloc[i]; s != &emptymspan && s.region == r {
			r.abandonSpan(s)
		}
	}
	for s := r.full; s != nil; s = s.regionNext {
		r.abandonSpan(s)
	}
}

// abandonSpan marks all allocated objects of s as foreign.
func (r *regionState) abandonSpan(s *mspan) {
	bw := (*[regionBitsBytes / 8]uint64)(unsafe.Pointer(s.regionBase))
	fw := (*[regionBitsBytes / 8]uint64)(unsafe.Pointer(s.regionFaded))
	aw := (*[regionBitsBytes / 8]uint64)(unsafe.Pointer(s.allocBits))
	nwords := regionBitsWords(s.nelems)
	fi := uintptr(s.freeindex)
	for i := uintptr(0); i < nwords; i++ {
		w := bw[i] | fw[i] | aw[i]
		if i < fi/64 {
			w = ^uint64(0)
		} else if i == fi/64 && fi%64 != 0 {
			w |= 1<<(fi%64) - 1
		}
		bw[i] = w
		fw[i] = 0
	}
	// The region's allocations are now accounted as ordinary ones.
	allocated := int64(s.allocCount) - int64(s.allocCountBeforeCache)
	if allocated > 0 {
		stats := memstats.heapStats.acquire()
		atomic.Xadd64(&stats.smallAllocCount[s.spanclass.sizeclass()], allocated)
		memstats.heapStats.release()
		gcController.totalAlloc.Add(allocated * int64(s.elemsize))
	}
	s.allocCountBeforeCache = s.allocCount
}

// resetSpan frees the unfaded region objects of s: the new allocation
// bitmap is the foreign objects plus the faded ones, which become
// foreign for the next region. Returns the number of objects and bytes
// freed.
func (r *regionState) resetSpan(s *mspan) (uint64, uint64) {
	regionStats.spanResets.Add(1)
	if debug.regiontrace > 0 {
		print("region:   reset span=", hex(s.base()), " elemsize=", s.elemsize, " nelems=", s.nelems, " allocCount=", s.allocCount, " faded=", s.regionFaded, " fw0=", hex(*(*uint64)(unsafe.Pointer(s.regionFaded))), " bw0=", hex(*(*uint64)(unsafe.Pointer(s.regionBase))), " aw0=", hex(*(*uint64)(unsafe.Pointer(s.allocBits))), "\n")
	}
	before := s.allocCount
	// base |= faded; allocBits = base; faded = 0
	bw := (*[regionBitsBytes / 8]uint64)(unsafe.Pointer(s.regionBase))
	fw := (*[regionBitsBytes / 8]uint64)(unsafe.Pointer(s.regionFaded))
	aw := (*[regionBitsBytes / 8]uint64)(unsafe.Pointer(s.allocBits))
	nwords := regionBitsWords(s.nelems)
	count := 0
	for i := uintptr(0); i < nwords; i++ {
		w := bw[i] | fw[i]
		if i == nwords-1 && s.nelems%64 != 0 {
			// Bits at or beyond nelems are meaningless; keep them clear.
			w &= ^uint64(0) >> (64 - uint(s.nelems%64))
		}
		bw[i] = w
		fw[i] = 0
		aw[i] = w
		count += sys.OnesCount64(w)
	}
	if debug.clobberfree != 0 {
		// Poison freed slots to make use-after-free visible.
		for idx := uintptr(0); idx < uintptr(s.nelems); idx++ {
			if bp, m := s.allocBits.bitp(idx); *bp&m == 0 {
				clobberfree(unsafe.Pointer(s.base()+idx*s.elemsize), s.elemsize)
			}
		}
	}
	s.allocCount = uint16(count)
	s.freeindex = 0
	s.freeIndexForScan = 0
	s.refillAllocCache(0)
	s.needzero = 1

	// Every object with a special must have been faded (see addspecial);
	// a special on a freed slot would be attached to its next occupant.
	for sp := s.specials; sp != nil; sp = sp.next {
		idx := uintptr(sp.offset) / s.elemsize
		if bp, m := s.allocBits.bitp(idx); *bp&m == 0 {
			print("region: freed object with special: span=", hex(s.base()), " elemsize=", s.elemsize, " idx=", idx, " kind=", sp.kind, " faded=", r.faded, "\n")
			throw("region: freed object with special")
		}
	}

	freed := uint64(0)
	if before > uint16(count) {
		freed = uint64(before - uint16(count))
	}
	// Account the objects allocated in this span since the cache last
	// accounted for it (allocCountBeforeCache) as allocated, and the
	// unfaded ones as freed, so that Mallocs/Frees and the pacer's
	// totalAlloc-totalFree stay consistent. Objects of spans that
	// filled up were already accounted by refill (which then set
	// allocCountBeforeCache = allocCount).
	allocated := int64(before) - int64(s.allocCountBeforeCache)
	stats := memstats.heapStats.acquire()
	if allocated > 0 {
		atomic.Xadd64(&stats.smallAllocCount[s.spanclass.sizeclass()], allocated)
		gcController.totalAlloc.Add(allocated * int64(s.elemsize))
	}
	if freed > 0 {
		atomic.Xadd64(&stats.smallFreeCount[s.spanclass.sizeclass()], int64(freed))
		gcController.totalFree.Add(int64(freed) * int64(s.elemsize))
	}
	memstats.heapStats.release()
	if freed > 0 && !s.spanclass.noscan() {
		// The pacer's estimate of scannable heap grew when these
		// objects were allocated; they are gone now. Without this the
		// estimate grows with every region and triggers cycles early.
		gcController.update(0, -int64(freed)*int64(s.elemsize))
	}
	s.allocCountBeforeCache = s.allocCount
	return freed, freed * uint64(s.elemsize)
}

// returnSpan gives a reset span back to the heap or its mcentral.
// Must run with preemption disabled.
func (r *regionState) returnSpan(s *mspan) {
	regionUncacheSpan(s)
}

// regionUncacheSpan releases a span cached by a region: to the heap if
// no object in it is allocated (uncacheSpan does not accept empty
// spans; a region span can be empty after a bulk free), otherwise to
// its mcentral through the usual protocol.
func regionUncacheSpan(s *mspan) {
	if s.sweepgen == mheap_.sweepgen+1 {
		// About to be swept: check for region-freed objects that the
		// collector found reachable (dangling references).
		regionCheckZombies(s)
	}
	untagRegionSpan(s)
	if s.allocCount != 0 {
		mheap_.central[s.spanclass].mcentral.uncacheSpan(s)
		return
	}
	// Completely free. Nothing in it can be marked: regions never free
	// while the collector is marking.
	sg := mheap_.sweepgen
	if s.sweepgen != sg+3 && s.sweepgen != sg+1 {
		throw("region: bad sweepgen on empty region span")
	}
	atomic.Store(&s.sweepgen, sg)
	s.needzero = 1
	systemstack(func() {
		mheap_.freeSpan(s)
	})
}

// flush hands all spans of the region cache back to the mcentrals,
// demoting their objects to ordinary heap objects. Used at GC mark
// termination (world stopped), when a region is abandoned, and when a
// goroutine dies.
func (r *regionState) flush() {
	c := r.c
	for s := r.full; s != nil; {
		next := s.regionNext
		s.regionNext = nil
		// Full spans count as fully allocated for the pacer already
		// (refill overestimates), so no heapLive adjustment is needed.
		// An in-place sweep may have emptied the span; regionUncacheSpan
		// handles that.
		regionUncacheSpan(s)
		s = next
	}
	r.full = nil
	c.releaseAll() // also untags the cached spans
	c.flushGen.Store(mheap_.sweepgen)
	regionStats.flushes.Add(1)
}

// regionSweepAll sweeps the spans cached by every region in place.
// Called during the mark termination stop-the-world, after the sweep
// generation has advanced. The spans stay cached: releasing them and
// re-acquiring them after every cycle made the pacer count each region's
// working set as fresh allocation every cycle, which at low GOGC fed
// back into more cycles. Objects that died are freed by the sweep and
// leave the region bitmaps; objects that survived keep their region
// status.
func regionSweepAll() {
	lock(&allRegionsLock)
	for r := allRegions; r != nil; r = r.next {
		r.sweepInPlace()
	}
	unlock(&allRegionsLock)
}

// sweepInPlace sweeps every span of the region cache and keeps it cached.
// Must run with the world stopped, right after mheap_.sweepgen advanced.
func (r *regionState) sweepInPlace() {
	c := r.c
	sg := mheap_.sweepgen
	// The tiny block and reusable objects may be freed by the sweep.
	clear(c.reusableNoscan[:])
	c.tiny = 0
	c.tinyoffset = 0
	c.scanAlloc = 0 // heapScan is recomputed at mark termination

	one := func(s *mspan) {
		if s.sweepgen != sg+1 {
			print("runtime: region span sweepgen=", s.sweepgen, " heap sweepgen=", sg, "\n")
			throw("region: bad sweepgen on cached span at mark termination")
		}
		regionCheckZombies(s)
		// Allocations since the span was cached are accounted the way
		// releaseAll accounts them.
		if used := int64(s.allocCount) - int64(s.allocCountBeforeCache); used > 0 {
			stats := memstats.heapStats.acquire()
			atomic.Xadd64(&stats.smallAllocCount[s.spanclass.sizeclass()], used)
			memstats.heapStats.release()
			gcController.totalAlloc.Add(used * int64(s.elemsize))
		}
		atomic.Store(&s.sweepgen, sg-1)
		ss := sweepLocked{s}
		ss.sweep(true)
		// Cached and swept.
		atomic.Store(&s.sweepgen, sg+3)
		// Dead objects leave the region bitmaps.
		bw := (*[regionBitsBytes / 8]uint64)(unsafe.Pointer(s.regionBase))
		fw := (*[regionBitsBytes / 8]uint64)(unsafe.Pointer(s.regionFaded))
		aw := (*[regionBitsBytes / 8]uint64)(unsafe.Pointer(s.allocBits))
		for i := uintptr(0); i < regionBitsWords(s.nelems); i++ {
			bw[i] &= aw[i]
			fw[i] &= aw[i]
		}
		s.allocCountBeforeCache = s.allocCount
	}
	for i := range c.alloc {
		if s := c.alloc[i]; s != &emptymspan && s.region == r {
			one(s)
		}
	}
	for s := r.full; s != nil; s = s.regionNext {
		one(s)
	}
	c.flushGen.Store(sg)
	regionStats.flushes.Add(1)
}

// regionMCache returns the allocation cache to use for an allocation by
// gp, which is mp.curg: the region's if gp is inside a region and not
// in region.Ignore, otherwise nil.
//
//go:nosplit
func regionMCache(mp *m) *mcache {
	gp := mp.curg
	if gp == nil || gp != getg() {
		return nil
	}
	r := gp.region
	if r == nil || r.ignore != 0 {
		return nil
	}
	return r.c
}

// regionCheckZombies looks for objects of s that a region freed but the
// collector marked in the cycle that just ended: a dangling reference.
// For each one it prints the referrers it can find by scanning the
// heap, stacks and globals conservatively, then throws. Debug aid.
func regionCheckZombies(s *mspan) {
	for idx := uintptr(s.freeindex); idx < uintptr(s.nelems); idx++ {
		if !s.markBitsForIndex(idx).isMarked() {
			continue
		}
		if bp, m := s.allocBits.bitp(idx); *bp&m != 0 {
			continue
		}
		addr := s.base() + idx*s.elemsize
		print("region: zombie obj=", hex(addr), " size=", s.elemsize, " noscan=", s.spanclass.noscan(), " idx=", idx, " freeindex=", s.freeindex, " region=", s.region, "\n")
		if bp, m := s.regionBase.bitp(idx); *bp&m != 0 {
			print("region:   (foreign bit set)\n")
		}
		if bp, m := s.regionFaded.bitp(idx); *bp&m != 0 {
			print("region:   (faded bit set)\n")
		}
		regionFindReferrers(addr, s.elemsize)
		throw("region: freed object is reachable")
	}
}

func regionFindReferrers(addr, size uintptr) {
	hit := func(p uintptr) bool { return p >= addr && p < addr+size }
	// Heap.
	for _, sp := range mheap_.allspans {
		if sp.state.get() != mSpanInUse {
			continue
		}
		n := uintptr(sp.nelems)
		for i := uintptr(0); i < n; i++ {
			base := sp.base() + i*sp.elemsize
			for off := uintptr(0); off < sp.elemsize; off += goarch.PtrSize {
				if hit(*(*uintptr)(unsafe.Pointer(base + off))) {
					print("region:   referrer heap obj=", hex(base), " size=", sp.elemsize, " noscan=", sp.spanclass.noscan(), " off=", off, " region=", sp.region, " allocated=", sp.isAllocatedIndex(i), " marked=", sp.markBitsForIndex(i).isMarked())
					if sp.region != nil {
						fb, fm := sp.regionFaded.bitp(i)
						bb, bm := sp.regionBase.bitp(i)
						print(" faded=", *fb&fm != 0, " foreign=", *bb&bm != 0)
					}
					print("\n")
					dumpEnd := base + 64
					if dumpEnd > base+sp.elemsize {
						dumpEnd = base + sp.elemsize
					}
					hexdumpWords(base, dumpEnd, nil)
					break
				}
			}
		}
	}
	// Stacks.
	forEachG(func(gp *g) {
		if gp.stack.lo == 0 || readgstatus(gp) == _Gdead {
			return
		}
		for p := gp.stack.lo; p < gp.stack.hi; p += goarch.PtrSize {
			if hit(*(*uintptr)(unsafe.Pointer(p))) {
				print("region:   referrer stack goid=", gp.goid, " slot=", hex(p), " start=", funcname(findfunc(gp.startpc)), " status=", readgstatus(gp), "\n")
			}
		}
	})
	// Globals.
	for _, datap := range activeModules() {
		for p := datap.data; p < datap.edata; p += goarch.PtrSize {
			if hit(*(*uintptr)(unsafe.Pointer(p))) {
				print("region:   referrer data slot=", hex(p), "\n")
			}
		}
		for p := datap.bss; p < datap.ebss; p += goarch.PtrSize {
			if hit(*(*uintptr)(unsafe.Pointer(p))) {
				print("region:   referrer bss slot=", hex(p), "\n")
			}
		}
	}
}
