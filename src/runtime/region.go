// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Goroutine-local memory regions (prototype, second version).
//
// A region is a dynamic extent of one goroutine (region.Do) during which
// heap allocations of up to regionMaxObject bytes are bump-allocated into
// region blocks: 64 KB spans divided into 128-byte lines. When Do
// returns, every line that holds only objects not observed to escape is
// freed at once, without a GC cycle, and is reused by the next region.
//
// Escape detection ("fading") is dynamic. The compiler emits a call to
// wbRegion after each batch of pointer stores when g.region is non-nil,
// and the runtime's bulk copy paths do the same. If the stored pointer
// refers to a live object of the current region and the destination slot
// is not itself inside such an object, nor a stack slot in a frame
// younger than the Do call, the object and everything reachable from it
// inside the region are marked faded. At exit the lines of faded objects are pinned
// and left to the garbage collector, which treats a region block like
// any other span: objects are found through a per-word start bitmap,
// scanned through a per-word pointer bitmap, and marked in the span's
// mark bits. Dead pinned lines are unpinned when the block is swept.
//
// Region blocks are swept in place at mark termination (regionMarkTermination),
// never by the background sweeper, so a region may keep allocating into
// its current block across GC cycles. A region that ends while the GC is
// marking, or by panicking, or whose fade work stack overflowed, frees
// nothing: all its lines are pinned and handed to the GC.
//
// Other ways for a region pointer to become visible outside the goroutine
// are handled explicitly: the closure of a go statement (newproc1),
// objects given specials (finalizers, cleanups, weak pointers,
// profiling), and panic values that propagate out of Do.
package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

// Check that the compiler and the runtime agree on the layout of g.
const _ = uint(unsafe.Offsetof(g{}.region) - abi.GRegionOffset)
const _ = uint(abi.GRegionOffset - unsafe.Offsetof(g{}.region))
const _ = uint(unsafe.Offsetof(g{}.regionIgnore) - abi.GRegionIgnoreOffset)
const _ = uint(abi.GRegionIgnoreOffset - unsafe.Offsetof(g{}.regionIgnore))

const (
	regionBlockBytes = 64 << 10
	regionBlockPages = regionBlockBytes / pageSize
	regionLineBytes  = 128
	regionLineWords  = regionLineBytes / goarch.PtrSize
	regionHdrBytes   = 4096
	regionDataBytes  = regionBlockBytes - regionHdrBytes
	regionDataWords  = regionDataBytes / goarch.PtrSize
	regionLines      = regionDataBytes / regionLineBytes
	regionBitWords   = regionDataWords / 64

	// regionMaxObject is the largest allocation served from a region.
	// Larger objects come from the ordinary heap and are "outside".
	regionMaxObject = 16 << 10

	// regionStackSize is the capacity of the transitive fade work stack.
	// Overflow poisons the region (nothing is freed at exit).
	regionStackSize = 2048

	// Line states.
	regionLineFree   = 0 // no live object; available for allocation
	regionLineActive = 1 // allocated by the owning region; freed at exit unless pinned
	regionLinePinned = 2 // holds an object that escaped or that the GC owns
)

// regionBlockHdr is the metadata at the start of every region block. The
// GC never scans it: the block's data begins regionHdrBytes in.
type regionBlockHdr struct {
	typ        abi.Type                   // synthetic type whose GC mask is ptr, for the GC's pointer iterator
	owner      *regionState               // region allocating from this block (nil when unowned)
	next       *mspan                     // next block of owner (list of blocks touched in the active region)
	allocEnd   uint32                     // end offset (from data start) of the highest allocation so far
	pinned     uint32                     // number of pinned lines
	nfaded     uint32                     // faded objects whose lines are not yet pinned (reset at exit)
	dbg        *[regionDataWords]uint32   // per-word history, GODEBUG=regioncheck: flags | sizewords<<8 | cycle<<20
	fadedLines [regionLines/64 + 1]uint64 // lines holding a faded object (fast path of the store check)
	freeLines  [regionLines/64 + 1]uint64 // lines in state regionLineFree (bit set), for run search
	activeLo   uint32                     // active lines of the owner are within [activeLo, activeHi)
	activeHi   uint32

	start [regionBitWords]uint64 // object start bits, one per word
	ptr   [regionBitWords]uint64 // pointer bits, one per word
	faded [regionBitWords]uint64 // faded bits, at object starts
	lines [regionLines]uint8
}

// regionState is the per-goroutine region state. It lives in non-GC'd
// memory and is reused across region.Do calls on the same goroutine.
type regionState struct {
	_     sys.NotInHeap
	owner *g
	next  *regionState // in allRegions

	// Bump allocation state: [cursor, limit) is the unused part of the
	// current free line run of cur. Lines of the run become active (and
	// have their stale bits cleared) only as the cursor reaches them;
	// activeTo is the end of the last active line.
	cur                     *mspan
	cursor, limit, activeTo uintptr
	// blocks is the list of blocks touched in the active region (via the
	// header's next field); cur is always on it while a region is active.
	blocks *mspan

	// stackOffset is gp.stack.hi minus the stack pointer at the Do call.
	// Stack slots at a smaller offset (younger frames) are inside the
	// region; older frames are outside.
	stackOffset uintptr

	active   bool // inside region.Do
	poisoned bool // free nothing at exit

	// Transitive fade work stack of object base addresses.
	sp    int
	stack [regionStackSize]uintptr

	// Per-region statistics, added to the global counters at exit.
	// Byte accounting is by lines (claimedLines), which balances exactly
	// against the lines freed at exit and by the GC; allocBytes is the
	// sum of object sizes, for region.Stats.
	claimedLines       uint64
	allocs, allocBytes uint64
	faded, fadedBytes  uint64
	scanBytes          uint64 // allocated bytes of pointerful objects, for the pacer
}

var (
	allRegions     *regionState // all live region states
	allRegionsLock mutex
	regionFreeList *regionState // released states for reuse

	// regionBlocks is the list of all region blocks (via mspan.regionNext/
	// regionPrev), owned or not, for sweeping at mark termination.
	// regionRecycle is a stack (via mspan.regionRecycleNext) of unowned
	// blocks that have free lines. Both under regionLock.
	regionLock    mutex
	regionBlocks  *mspan
	regionRecycle *mspan
	regionNBlocks int

	// regionStats are cumulative counters, read by region.Stats.
	regionStats struct {
		regions      atomic.Uint64 // completed region.Do calls
		freedRegions atomic.Uint64 // regions that freed eagerly
		freedObjects atomic.Uint64
		freedBytes   atomic.Uint64
		faded        atomic.Uint64
		fadedBytes   atomic.Uint64
		poisoned     atomic.Uint64 // regions abandoned (overflow, panic, GC marking)
		blocks       atomic.Uint64 // blocks taken from the heap
		barrierCalls atomic.Uint64 // wbRegion slow-path calls (when regiontrace > 0)
	}
)

// regionsEnabled reports whether region.Do should create regions.
func regionsEnabled() bool {
	return debug.regions != 0 && !raceenabled && !msanenabled && !asanenabled
}

// regionDo runs f in a region on the calling goroutine.
//
//go:linkname regionDo region.runtime_regionDo
func regionDo(f func()) {
	gp := getg()
	if !regionsEnabled() || gp.region != nil || gp.m.curg != gp {
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

// regionIgnore runs f with allocations placed outside the region.
//
//go:linkname regionIgnore region.runtime_regionIgnore
func regionIgnore(f func()) {
	gp := getg()
	if gp.region == nil {
		f()
		return
	}
	gp.regionIgnore++
	f()
	gp.regionIgnore--
}

// regionInherit runs f with allocations placed where p lives: inside the
// region if p is a live object of the region, a stack slot, or nil;
// outside it otherwise.
//
//go:linkname regionInherit region.runtime_regionInherit
func regionInherit(p unsafe.Pointer, f func()) {
	gp := getg()
	if gp.region == nil || gp.regionIgnore != 0 || regionIsLocal(gp, uintptr(p)) {
		f()
		return
	}
	gp.regionIgnore++
	f()
	gp.regionIgnore--
}

// regionIsLocal reports whether p is nil, a stack address of gp, or a
// live object of gp's region.
func regionIsLocal(gp *g, p uintptr) bool {
	if p == 0 || (gp.stack.lo <= p && p < gp.stack.hi) {
		return true
	}
	r := gp.region
	if r == nil {
		return false
	}
	mp := acquirem()
	_, _, ok := r.isRegionObject(p)
	releasem(mp)
	return ok
}

//go:linkname regionActive region.runtime_regionActive
func regionActive() bool {
	return getg().region != nil
}

//go:linkname regionOwns region.runtime_regionOwns
func regionOwns(p unsafe.Pointer) bool {
	gp := getg()
	if gp.region == nil {
		return false
	}
	mp := acquirem()
	_, _, ok := gp.region.isRegionObject(uintptr(p))
	releasem(mp)
	return ok
}

//go:linkname regionReadStats region.runtime_regionReadStats
func regionReadStats(out *[9]uint64) {
	out[0] = regionStats.regions.Load()
	out[1] = regionStats.freedRegions.Load()
	out[2] = regionStats.freedObjects.Load()
	out[3] = regionStats.freedBytes.Load()
	out[4] = regionStats.faded.Load()
	out[5] = regionStats.fadedBytes.Load()
	out[6] = regionStats.poisoned.Load()
	out[7] = regionStats.blocks.Load()
	out[8] = regionStats.barrierCalls.Load()
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
	r.owner = gp
	r.cur, r.cursor, r.limit, r.activeTo, r.blocks = nil, 0, 0, 0, nil
	r.sp = 0
	r.poisoned = false
	lock(&allRegionsLock)
	r.next = allRegions
	allRegions = r
	unlock(&allRegionsLock)
	return r
}

// regionRelease gives up a goroutine's region state (gdestroy).
func regionRelease(gp *g) {
	r := gp.regionCache
	if r == nil {
		return
	}
	gp.regionCache = nil
	gp.region = nil
	gp.regionIgnore = 0
	lock(&allRegionsLock)
	for pp := &allRegions; *pp != nil; pp = &(*pp).next {
		if *pp == r {
			*pp = r.next
			break
		}
	}
	unlock(&allRegionsLock)
	mp := acquirem()
	mp.mallocing = 1
	if r.cur != nil {
		r.releaseBlock(r.cur)
		r.cur = nil
	}
	mp.mallocing = 0
	releasem(mp)
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
	r.allocs, r.allocBytes, r.faded, r.fadedBytes, r.scanBytes, r.claimedLines = 0, 0, 0, 0, 0, 0
	gp.regionIgnore = 0
	if r.cur != nil {
		// The current block was kept from the previous region. Start
		// allocating at its first free run.
		r.blocks = r.cur
		r.cur.regionHdr().next = nil
		r.cursor, r.limit, r.activeTo = 0, 0, 0
	}
	gp.region = r
}

func regionEnd(gp *g, r *regionState) {
	if gp._panic != nil {
		r.poisoned = true
	}
	mp := acquirem()
	mp.mallocing = 1 // keep the GC from starting underneath us
	gp.region = nil
	gp.regionIgnore = 0
	r.active = false
	regionStats.regions.Add(1)
	abandon := r.poisoned || gcphase != _GCoff || writeBarrier.enabled
	if abandon {
		regionStats.poisoned.Add(1)
	} else {
		r.scanOuterStack(gp)
		if debug.regioncheck != 0 {
			r.verifyExit()
		}
	}
	regionStats.faded.Add(int64(r.faded))
	regionStats.fadedBytes.Add(int64(r.fadedBytes))
	r.finish(abandon)
	mp.mallocing = 0
	releasem(mp)
}

// scanOuterStack fades every live region object referenced from the
// stack frames older than the Do call, conservatively.
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
		if s, base, ok := r.isRegionObject(w); ok {
			if debug.regiontrace > 0 {
				print("region: fade-stack obj=", hex(base), " size=", s.regionObjSize(base), " slot=", hex(p), "\n")
			}
			r.fade(s, base)
		}
	}
}

// ---- Allocation ------------------------------------------------------------

func (s *mspan) regionHdr() *regionBlockHdr { return (*regionBlockHdr)(unsafe.Pointer(s.base())) }
func (s *mspan) regionData() uintptr        { return s.base() + regionHdrBytes }

// regionAlloc allocates size bytes of type typ (which may be nil for
// pointer-free memory) in gp's region. It returns nil if the allocation
// does not belong in the region: an ignored scope, or too large. The
// caller then allocates from the ordinary heap.
//
// The caller has already deducted GC assist credit for size.
func regionAlloc(gp *g, size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	if gp.regionIgnore != 0 || size > regionMaxObject {
		return nil
	}
	r := gp.region
	mp := acquirem()
	if mp.mallocing != 0 {
		throw("region: malloc deadlock")
	}
	mp.mallocing = 1

	asize := alignUp(size, goarch.PtrSize)
	x := r.cursor
	newBlock := false
	if x+asize > r.limit {
		x, newBlock = r.refill(asize)
	}
	r.cursor = x + asize
	s := r.cur
	hdr := s.regionHdr()
	if r.cursor > r.activeTo {
		r.activateTo(r.cursor)
	}
	off := x - s.regionData()
	w := off / goarch.PtrSize
	if hdr.dbg != nil {
		// Overlap check against the exact recorded size of the
		// nearest preceding object.
		if pb := s.regionObjBase(x); pb != 0 {
			pw := (pb - s.regionData()) / goarch.PtrSize
			if psz := uintptr(hdr.dbg[pw]>>8&0xfff) * goarch.PtrSize; pb+psz > x && hdr.lines[pw*goarch.PtrSize/regionLineBytes] != regionLineFree && hdr.dbg[pw]&regionDbgDead == 0 {
				print("region: alloc self-check: new object ", hex(x), " size ", asize, " overlaps object ", hex(pb), " size ", psz, " linestate=", hdr.lines[pw*goarch.PtrSize/regionLineBytes], " cursor=", hex(r.cursor), " limit=", hex(r.limit), " allocEnd=", hdr.allocEnd, "\n")
				throw("region: overlapping allocation")
			}
		}
		for j := w; j < w+asize/goarch.PtrSize; j++ {
			if hdr.start[j/64]>>(j%64)&1 != 0 || hdr.ptr[j/64]>>(j%64)&1 != 0 {
				print("region: alloc self-check: stale bits at word ", j, " (start=", hdr.start[j/64]>>(j%64)&1, " ptr=", hdr.ptr[j/64]>>(j%64)&1, ") for new object ", hex(x), " size ", asize, " line ", off/regionLineBytes, " cursor=", hex(r.cursor), " limit=", hex(r.limit), " worddbg=", hex(hdr.dbg[j]), "\n")
				throw("region: stale bits in new object")
			}
		}
	}
	if end := uint32(off + asize); end > hdr.allocEnd {
		hdr.allocEnd = end
	}
	hdr.start[w/64] |= 1 << (w % 64)
	if hdr.dbg != nil {
		f := uint32(work.cycles.Load())<<20 | uint32(asize/goarch.PtrSize)<<8
		if writeBarrier.enabled {
			f |= regionDbgBlack
		}
		hdr.dbg[w] = f
	}
	if needzero && s.needzero != 0 {
		memclrNoHeapPointers(unsafe.Pointer(x), asize)
	}
	if typ != nil && typ.Pointers() {
		regionWritePtrBits(hdr, w, size, typ)
		r.scanBytes += uint64(asize)
	}
	if hdr.dbg != nil {
		// Self-check of the object bitmaps.
		if b := s.regionObjBase(x); b != x {
			print("region: alloc self-check: base of ", hex(x), " is ", hex(b), "\n")
			throw("region: bad object base")
		}
		if b := s.regionObjBase(x + asize - goarch.PtrSize); b != x {
			print("region: alloc self-check: base of last word of ", hex(x), " size ", asize, " is ", hex(b), "\n")
			throw("region: bad object base (last word)")
		}
		if sz := s.regionObjSize(x); sz < asize {
			// The size of the last object of a run is inflated until
			// the next allocation; it must never be too small.
			print("region: alloc self-check: size of ", hex(x), " is ", sz, " want ", asize, " allocEnd=", hdr.allocEnd, " off=", off, "\n")
			throw("region: bad object size")
		}
		if hdr.lines[off/regionLineBytes] != regionLineActive || hdr.lines[(off+asize-1)/regionLineBytes] != regionLineActive {
			throw("region: allocation in inactive line")
		}
	}
	r.allocs++
	r.allocBytes += uint64(asize)

	// Ensure that the stores above that initialize x to type-safe memory
	// and set the pointer bitmap occur before the caller can make x
	// observable to the garbage collector.
	publicationBarrier()

	if writeBarrier.enabled {
		// Allocate black during marking.
		s.markBitsForIndex(w).setMarked()
		arena, pageIdx, pageMask := pageIndexOf(s.base())
		if arena.pageMarks[pageIdx]&pageMask == 0 {
			atomic.Or8(&arena.pageMarks[pageIdx], pageMask)
		}
		mp.p.ptr().gcw.bytesMarked += uint64(asize)
	}

	// Profiling sample, using the P's cache state like mallocgc does.
	c := getMCache(mp)
	c.nextSample -= int64(asize)
	if c.nextSample < 0 || MemProfileRate != c.memProfRate {
		profilealloc(mp, unsafe.Pointer(x), asize)
	}

	mp.mallocing = 0
	releasem(mp)

	if newBlock {
		if t := (gcTrigger{kind: gcTriggerHeap}); t.test() {
			gcStart(t)
		}
	}
	return unsafe.Pointer(x)
}

// regionWritePtrBits sets the pointer bits of the object at word w of
// hdr, of size bytes tiled with typ.
func regionWritePtrBits(hdr *regionBlockHdr, w, size uintptr, typ *_type) {
	mask := getGCMask(typ)
	nbits := typ.PtrBytes / goarch.PtrSize
	ewords := typ.Size_ / goarch.PtrSize
	if ewords <= 32 && 64%ewords == 0 && size > typ.Size_ {
		// An array of small elements: tile the element's mask into a
		// 64-bit pattern and write it a word at a time.
		pat := uint64(readUintptr(mask)) & (1<<ewords - 1)
		for sh := ewords; sh < 64; sh *= 2 {
			pat |= pat << sh
		}
		end := w + size/goarch.PtrSize
		for i := w; i < end; {
			n := min(64-i%64, end-i)
			// Rotate the pattern so that element boundaries line up with i.
			rot := (i - w) % ewords
			p := pat>>rot | pat<<(ewords-rot)
			hdr.ptr[i/64] |= (p & (1<<n - 1)) << (i % 64)
			i += n
		}
		return
	}
	for e := uintptr(0); e < size/typ.Size_; e++ {
		dst := w + e*ewords
		for i := uintptr(0); i < nbits; i += 64 {
			v := readUintptr(addb(mask, i/8))
			if n := nbits - i; n < 64 {
				v &= 1<<n - 1
			}
			if v == 0 {
				continue
			}
			d := dst + i
			hdr.ptr[d/64] |= uint64(v) << (d % 64)
			if d%64 != 0 && d%64+uintptr(64-sys.LeadingZeros64(uint64(v))) > 64 {
				hdr.ptr[d/64+1] |= uint64(v) >> (64 - d%64)
			}
		}
		if typ.Size_ == 0 {
			break
		}
	}
}

// refill finds room for an allocation of asize bytes: the next free line
// run of the current block, or a block with free lines. It returns the
// new cursor and whether a block was taken from the heap.
func (r *regionState) refill(asize uintptr) (uintptr, bool) {
	newBlock := false
	r.closeRun()
	for {
		if r.cur != nil {
			// Look for a free run after the current one.
			from := uintptr(0)
			if r.limit != 0 {
				from = (r.limit - r.cur.regionData()) / regionLineBytes
			}
			if lo, hi, ok := r.cur.regionClaimRun(from, asize); ok {
				r.cursor, r.limit, r.activeTo = lo, hi, lo
				return lo, newBlock
			}
		}
		s, fresh := regionGetBlock()
		newBlock = newBlock || fresh
		hdr := s.regionHdr()
		hdr.owner = r
		hdr.next = r.blocks
		r.blocks = s
		r.cur = s
		r.cursor, r.limit, r.activeTo = 0, 0, 0
	}
}

// activateTo makes the lines of the current run up to address end
// active, clearing their stale bits.
func (r *regionState) activateTo(end uintptr) {
	hdr := r.cur.regionHdr()
	data := r.cur.regionData()
	l := (r.activeTo - data) / regionLineBytes
	e := (end - data + regionLineBytes - 1) / regionLineBytes
	if hdr.activeHi == 0 || uint32(l) < hdr.activeLo {
		hdr.activeLo = uint32(l)
	}
	if uint32(e) > hdr.activeHi {
		hdr.activeHi = uint32(e)
	}
	r.claimedLines += uint64(e - l)
	r.activeTo = data + e*regionLineBytes
	for i := l; i < e; i++ {
		hdr.lines[i] = regionLineActive
	}
	// Clear the stale object bits of lines [l, e) a bitmap word at a
	// time, and take them out of the free set.
	for i := l; i < e; {
		n := min(64/regionLineWords-i%(64/regionLineWords), e-i) // lines in this word
		mask := (uint64(1)<<(n*regionLineWords) - 1) << ((i % (64 / regionLineWords)) * regionLineWords)
		wi := i / (64 / regionLineWords)
		hdr.start[wi] &^= mask
		hdr.ptr[wi] &^= mask
		hdr.faded[wi] &^= mask
		i += n
	}
	for i := l; i < e; {
		n := min(64-i%64, e-i)
		m := (uint64(1)<<n - 1) << (i % 64)
		hdr.fadedLines[i/64] &^= m
		hdr.freeLines[i/64] &^= m
		i += n
	}
}

// closeRun ends the current line run: the unused tail gets a sentinel
// start bit, so that the size of the last object stays exact (see
// regionObjSize) when later lines are reused. The sentinel is a dummy
// object without pointers that is freed with its line.
func (r *regionState) closeRun() {
	if r.cur == nil || r.cursor >= r.activeTo {
		r.cursor = r.limit
		return
	}
	hdr := r.cur.regionHdr()
	w := (r.cursor - r.cur.regionData()) / goarch.PtrSize
	hdr.start[w/64] |= 1 << (w % 64)
	if hdr.dbg != nil {
		hdr.dbg[w] = uint32(work.cycles.Load())<<20 | uint32((r.activeTo-r.cursor)/goarch.PtrSize)<<8 | regionDbgSentinel
	}
	r.cursor = r.limit
}

// regionClaimRun finds the first run of free lines at or after line from
// that can hold asize bytes and returns its address range. The lines are
// activated as they are used (activateTo).
func (s *mspan) regionClaimRun(from, asize uintptr) (lo, hi uintptr, ok bool) {
	hdr := s.regionHdr()
	need := (asize + regionLineBytes - 1) / regionLineBytes
	l := from
	for l < regionLines {
		// Next free line at or after l.
		w := hdr.freeLines[l/64] &^ (1<<(l%64) - 1)
		for w == 0 {
			if l = (l/64 + 1) * 64; l >= regionLines {
				return 0, 0, false
			}
			w = hdr.freeLines[l/64]
		}
		l = l/64*64 + uintptr(sys.TrailingZeros64(w))
		if l >= regionLines {
			return 0, 0, false
		}
		// End of the run: next non-free line after l.
		e := l
		for {
			nw := ^hdr.freeLines[e/64] &^ (1<<(e%64) - 1)
			if nw != 0 {
				e = e/64*64 + uintptr(sys.TrailingZeros64(nw))
				break
			}
			if e = (e/64 + 1) * 64; e >= regionLines {
				break
			}
		}
		if e > regionLines {
			e = regionLines
		}
		if e-l >= need {
			data := s.regionData()
			return data + l*regionLineBytes, data + e*regionLineBytes, true
		}
		l = e
	}
	return 0, 0, false
}

// regionGetBlock returns an unowned block with free lines, reusing one
// from the recycle stack if possible. fresh reports a heap allocation.
func regionGetBlock() (s *mspan, fresh bool) {
	lock(&regionLock)
	for regionRecycle != nil {
		s = regionRecycle
		regionRecycle = s.regionRecycleNext
		s.regionRecycleNext = nil
		hdr := s.regionHdr()
		if hdr.owner == nil && hdr.pinned < regionLines {
			unlock(&regionLock)
			return s, false
		}
	}
	unlock(&regionLock)
	return regionNewBlock(), true
}

// Region blocks live in arenas of their own (64 MB each, obtained
// from the heap's arena allocator with a dedicated hint list), so that
// they are 64 KB aligned and occupy a known address range. The store
// barrier uses this: a pointer outside [regionLo, regionHi) is not a
// region pointer, a 64 MB arena of the range belongs to regions iff its
// bit is set in regionArenaSet, and a pointer and a slot are in the
// same block iff they agree above bit 16. Blocks are never returned to
// the heap: freed blocks go on a ready list for reuse (like user arena
// chunks).
const (
	regionArenaBytes  = heapArenaBytes
	regionMaxArenas   = 4096 // 256 GB of address range
	regionArenaHint   = 0x00d0 << 32
	regionArenaHintLo = 0x30
	regionArenaHintHi = 0x3f
)

var (
	regionArenaHints *arenaHint
	regionArenas     []arenaIdx // for sysAlloc's bookkeeping
	regionArenaCur   uintptr    // next unused block of the newest arena
	regionArenaEnd   uintptr
	regionReady      *mspan // free block spans, via regionRecycleNext
	regionLo         uintptr
	regionHi         uintptr
	regionArenaSet   [regionMaxArenas / 64]uint64
)

// regionArenaIsBlock reports whether p is inside a region arena; it is
// exact and needs no span lookup.
//
//go:nosplit
func regionArenaIsBlock(p uintptr) bool {
	lo := atomic.Loaduintptr(&regionLo)
	if p < lo || p >= atomic.Loaduintptr(&regionHi) {
		return false
	}
	i := (p - lo) / regionArenaBytes
	return i < regionMaxArenas && regionArenaSet[i/64]>>(i%64)&1 != 0
}

// regionNewBlock takes a block: a free one from the ready list, the
// next unused one of the current arena, or the first of a new arena.
func regionNewBlock() *mspan {
	var s *mspan
	var base uintptr
	fresh := false
	systemstack(func() {
		lock(&mheap_.lock)
		if regionReady != nil {
			s = regionReady
			regionReady = s.regionRecycleNext
			s.regionRecycleNext = nil
			base = s.base()
		} else {
			if regionArenaCur == regionArenaEnd {
				if regionArenaHints == nil {
					for i := regionArenaHintHi; i >= regionArenaHintLo; i-- {
						hint := (*arenaHint)(mheap_.arenaHintAlloc.alloc())
						hint.addr = uintptr(i)<<40 | uintptrMask&regionArenaHint
						hint.next, regionArenaHints = regionArenaHints, hint
					}
				}
				v, size := mheap_.sysAlloc(regionArenaBytes, &regionArenaHints, &regionArenas)
				if v == nil {
					throw("region: out of address space")
				}
				regionArenaCur = uintptr(v)
				regionArenaEnd = uintptr(v) + size
				lo, hi := regionLo, regionHi
				if lo == 0 || uintptr(v) < lo {
					lo = uintptr(v)
				}
				if uintptr(v)+size > hi {
					hi = uintptr(v) + size
				}
				if (hi-lo)/regionArenaBytes > regionMaxArenas {
					throw("region: arena range too wide")
				}
				// Growing the range before setting the bits keeps the
				// test conservative for racing readers (see regionArenaIsBlock).
				if lo != regionLo {
					// The range grew downwards: shift the bit set.
					d := (regionLo - lo) / regionArenaBytes
					var ns [regionMaxArenas / 64]uint64
					for i := range regionArenaSet {
						for b := 0; b < 64; b++ {
							if regionArenaSet[i]>>b&1 != 0 {
								j := uintptr(i)*64 + uintptr(b) + d
								ns[j/64] |= 1 << (j % 64)
							}
						}
					}
					regionArenaSet = ns
					atomic.Storeuintptr(&regionLo, lo)
				}
				atomic.Storeuintptr(&regionHi, hi)
				for a := uintptr(v); a < uintptr(v)+size; a += regionArenaBytes {
					i := (a - lo) / regionArenaBytes
					regionArenaSet[i/64] |= 1 << (i % 64)
				}
			}
			base = regionArenaCur
			regionArenaCur += regionBlockBytes
			s = mheap_.allocMSpanLocked()
			fresh = true
		}
		unlock(&mheap_.lock)

		if fresh {
			sysMap(unsafe.Pointer(base), regionBlockBytes, &gcController.heapReleased, "region block")
			sysUsed(unsafe.Pointer(base), regionBlockBytes, regionBlockBytes)
			stats := memstats.heapStats.acquire()
			atomic.Xaddint64(&stats.inHeap, int64(regionBlockBytes))
			atomic.Xaddint64(&stats.committed, int64(regionBlockBytes))
			memstats.heapStats.release()
			gcController.heapReleased.add(-int64(regionBlockBytes))
		}
		gcController.heapInUse.add(int64(regionBlockBytes))

		s.init(base, regionBlockPages)
		lock(&mheap_.lock)
		mheap_.initSpan(s, spanAllocHeap, makeSpanClass(0, false), base, regionBlockPages, 0)
		unlock(&mheap_.lock)
	})
	if !fresh {
		s.needzero = 1
	}
	// From the GC's point of view the block is a span of
	// regionDataWords one-word slots, found through the start bitmap.
	s.isRegionBlock = true
	s.nelems = regionDataWords
	s.freeindex = regionDataWords
	s.freeIndexForScan = regionDataWords
	s.allocCount = 1 // nonzero while in use
	s.gcmarkBits = newMarkBits(regionDataWords)
	s.allocBits = newAllocBits(regionDataWords)
	s.limit = s.base() + regionBlockBytes
	hdr := s.regionHdr()
	memclrNoHeapPointers(unsafe.Pointer(hdr), regionHdrBytes)
	for l := uintptr(0); l < regionLines; l++ {
		hdr.freeLines[l/64] |= 1 << (l % 64)
	}
	hdr.typ = abi.Type{
		Size_:       regionDataBytes,
		PtrBytes:    regionDataBytes,
		Align_:      goarch.PtrSize,
		FieldAlign_: goarch.PtrSize,
		Kind_:       abi.Struct,
		GCData:      (*byte)(unsafe.Pointer(&hdr.ptr[0])),
	}
	atomic.StorepNoWB(unsafe.Pointer(&s.largeType), unsafe.Pointer(&hdr.typ))
	if debug.regioncheck != 0 {
		hdr.dbg = (*[regionDataWords]uint32)(persistentalloc(regionDataWords*4, 8, &memstats.other_sys))
	}

	gcController.update(regionBlockBytes, 0)
	lock(&regionLock)
	s.regionPrev = nil
	s.regionNext = regionBlocks
	if regionBlocks != nil {
		regionBlocks.regionPrev = s
	}
	regionBlocks = s
	regionNBlocks++
	unlock(&regionLock)
	regionStats.blocks.Add(1)
	return s
}

// regionFreeBlock puts an unowned block with no live lines on the ready
// list. It must not be on the recycle stack.
func regionFreeBlock(s *mspan) {
	hdr := s.regionHdr()
	if hdr.owner != nil || hdr.pinned != 0 {
		throw("region: freeing a live block")
	}
	lock(&regionLock)
	if s.regionPrev != nil {
		s.regionPrev.regionNext = s.regionNext
	} else {
		regionBlocks = s.regionNext
	}
	if s.regionNext != nil {
		s.regionNext.regionPrev = s.regionPrev
	}
	s.regionNext, s.regionPrev = nil, nil
	regionNBlocks--
	unlock(&regionLock)
	if s.specials != nil {
		throw("region: freeing a block with specials")
	}
	s.isRegionBlock = false
	atomic.StorepNoWB(unsafe.Pointer(&s.largeType), nil)
	s.allocCount = 0
	// Take the block out of the pacer's live heap estimate, which the
	// block allocation added to. heapLive is recomputed at each GC from
	// the marked bytes and blocks freed since then must not take it
	// below that mark, so the estimate stays a valid heap size.
	for {
		live := gcController.heapLive.Load()
		if live < gcController.heapMarked+regionBlockBytes {
			break
		}
		if gcController.heapLive.CompareAndSwap(live, live-regionBlockBytes) {
			break
		}
	}
	gcController.heapInUse.add(-int64(regionBlockBytes))
	mheap_.pagesInUse.Add(^uintptr(regionBlockPages - 1))
	arena, pageIdx, pageMask := pageIndexOf(s.base())
	atomic.And8(&arena.pageInUse[pageIdx], ^pageMask)
	s.state.set(mSpanDead)
	systemstack(func() {
		lock(&mheap_.lock)
		s.regionRecycleNext = regionReady
		regionReady = s
		unlock(&mheap_.lock)
	})
}

// releaseBlock drops r's ownership of s after its active lines have been
// freed or pinned: the block is freed if empty, or made available to
// other regions.
func (r *regionState) releaseBlock(s *mspan) {
	hdr := s.regionHdr()
	hdr.owner = nil
	hdr.next = nil
	if hdr.pinned == 0 {
		regionFreeBlock(s)
		return
	}
	if hdr.pinned < regionLines {
		lock(&regionLock)
		s.regionRecycleNext = regionRecycle
		regionRecycle = s
		unlock(&regionLock)
	}
}

// ---- Object lookup ---------------------------------------------------------

// regionObjBase returns the base address of the object of region block s
// that contains p, or 0 if p is not inside an allocated object.
//
//go:nosplit
func (s *mspan) regionObjBase(p uintptr) uintptr {
	data := s.regionData()
	if p < data || p >= s.limit {
		return 0
	}
	hdr := s.regionHdr()
	w := (p - data) / goarch.PtrSize
	i := w / 64
	bits := hdr.start[i] & (^uint64(0) >> (63 - w%64))
	// Objects are at most regionMaxObject bytes, so the start bit is
	// within that many words.
	for n := 0; bits == 0; n++ {
		if i == 0 || n >= regionMaxObject/goarch.PtrSize/64 {
			return 0
		}
		i--
		bits = hdr.start[i]
	}
	b := i*64 + 63 - uintptr(sys.LeadingZeros64(bits))
	if hdr.lines[b*goarch.PtrSize/regionLineBytes] == regionLineFree {
		// A stale start bit in a freed line.
		return 0
	}
	base := data + b*goarch.PtrSize
	if p >= base+s.regionObjSize(base) {
		return 0
	}
	return base
}

// regionObjSize returns the size of the object at base in region block s:
// the distance to the next object start, bounded by the end of the
// allocated area and the maximum object size.
//
//go:nosplit
func (s *mspan) regionObjSize(base uintptr) uintptr {
	hdr := s.regionHdr()
	data := s.regionData()
	w := (base - data) / goarch.PtrSize
	i := w / 64
	var bits uint64
	if w%64 != 63 {
		bits = hdr.start[i] &^ (^uint64(0) >> (63 - w%64))
	}
	for n := 0; bits == 0; n++ {
		i++
		if i >= regionBitWords || n >= regionMaxObject/goarch.PtrSize/64 {
			end := data + uintptr(hdr.allocEnd)
			if end <= base {
				return goarch.PtrSize
			}
			if end-base > regionMaxObject {
				return regionMaxObject
			}
			return end - base
		}
		bits = hdr.start[i]
	}
	n := i*64 + uintptr(sys.TrailingZeros64(bits))
	size := (n - w) * goarch.PtrSize
	if end := uintptr(hdr.allocEnd); end < n*goarch.PtrSize && data+end > base {
		size = data + end - base
	}
	return size
}

// regionTypePointers returns the GC's pointer iterator for the object at
// base of region block s. The iterator reads the block's pointer bitmap
// through the synthetic type in the block header.
//
//go:nosplit
func (s *mspan) regionTypePointers(base uintptr) typePointers {
	hdr := s.regionHdr()
	data := s.regionData()
	w := (base - data) / goarch.PtrSize
	addr := data + (w/64)*64*goarch.PtrSize
	mask := uintptr(hdr.ptr[w/64]) &^ (1<<(w%64) - 1)
	if end := w + s.regionObjSize(base)/goarch.PtrSize; end < (w/64+1)*64 {
		mask &= 1<<(end%64) - 1
	}
	return typePointers{elem: data, addr: addr, mask: mask, typ: &hdr.typ}
}

// isRegionObject reports whether p points into a live object of r: an
// object in an active line of a block owned by r that has not faded. It
// returns the block and the object's base address.
//
//go:nosplit
func (r *regionState) isRegionObject(p uintptr) (*mspan, uintptr, bool) {
	s := spanOf(p)
	if s == nil || !s.isRegionBlock || s.state.get() != mSpanInUse {
		return nil, 0, false
	}
	hdr := s.regionHdr()
	if hdr.owner != r {
		return nil, 0, false
	}
	base := s.regionObjBase(p)
	if base == 0 {
		return nil, 0, false
	}
	off := base - s.regionData()
	if hdr.lines[off/regionLineBytes] != regionLineActive {
		return nil, 0, false
	}
	w := off / goarch.PtrSize
	if hdr.faded[w/64]&(1<<(w%64)) != 0 {
		return nil, 0, false
	}
	return s, base, true
}

// ---- Escape detection ------------------------------------------------------

// wbRegion is called by compiled code after a pointer store *slot = ptr
// when g.region != nil. The fast path rejects pointers that are not into
// a block of the current region.
//
//go:nosplit
func wbRegion(slot *uintptr, ptr uintptr) {
	gp := getg()
	r := gp.region
	if r == nil || ptr == 0 {
		return
	}
	// Not a pointer into a region block: nothing can escape.
	if !regionArenaIsBlock(ptr) {
		return
	}
	// Pointer and slot in the same block, in active lines without a
	// faded object: a store of a live region object into a live region
	// object, which is the common case. One header read, no span lookup.
	sl := uintptr(unsafe.Pointer(slot))
	if ptr^sl < regionBlockBytes {
		hdr := (*regionBlockHdr)(unsafe.Pointer(ptr &^ (regionBlockBytes - 1)))
		data := uintptr(unsafe.Pointer(hdr)) + regionHdrBytes
		if hdr.owner == r && ptr >= data && sl >= data &&
			hdr.lineLiveUnfaded((ptr-data)/regionLineBytes) && hdr.lineLiveUnfaded((sl-data)/regionLineBytes) {
			return
		}
	}
	s := spanOf(ptr)
	if s == nil || !s.isRegionBlock || s.regionHdr().owner != r {
		return
	}
	if debug.regiontrace > 0 {
		regionTracePC = sys.GetCallerPC()
	}
	wbRegionSlow(gp, r, sl, ptr)
}

//go:nosplit
func wbRegionSlow(gp *g, r *regionState, slot, ptr uintptr) {
	mp := acquirem()
	if debug.regiontrace > 0 {
		regionStats.barrierCalls.Add(1)
	}
	regionCheckStore(gp, r, slot, ptr)
	releasem(mp)
}

var regionTracePC uintptr

// regionCheckStore handles the store *slot = ptr: if ptr is a live object
// of r and slot is outside the region, the object fades.
func regionCheckStore(gp *g, r *regionState, slot, ptr uintptr) {
	if gp.stack.lo <= slot && slot < gp.stack.hi {
		// A stack slot. Older frames are scanned at exit.
		return
	}
	// Fast path: both the pointer and the slot are in active lines of
	// the region's blocks that hold no faded object. Then both are live
	// region objects and nothing escapes. (wbRegion has established
	// that ptr is in a block owned by r.)
	if ss := spanOf(slot); ss != nil && ss.isRegionBlock && ss.state.get() == mSpanInUse && ss.regionHdr().owner == r && slot >= ss.regionData() {
		ps := spanOf(ptr)
		if ps != nil && ps.isRegionBlock && ps.state.get() == mSpanInUse && ps.regionHdr().owner == r && ptr >= ps.regionData() &&
			ps.regionHdr().lineLiveUnfaded((ptr-ps.regionData())/regionLineBytes) && ss.regionHdr().lineLiveUnfaded((slot-ss.regionData())/regionLineBytes) {
			return
		}
	}
	s, base, ok := r.isRegionObject(ptr)
	if !ok {
		if debug.regioncheck != 0 {
			regionCheckDangling(ptr, slot)
		}
		return
	}
	if _, _, ok := r.isRegionObject(slot); ok {
		return
	}
	if debug.regiontrace > 0 {
		print("region: fade obj=", hex(base), " size=", s.regionObjSize(base), " slot=", hex(slot))
		if ss := spanOf(slot); ss != nil && ss.state.get() == mSpanInUse {
			if ss.isRegionBlock {
				print(" (region block, faded or pinned object)")
			} else {
				print(" (heap object size=", ss.elemsize, ")")
			}
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
	r.fade(s, base)
}

// wbRegionMove is called by compiled code after a typed move into *dst
// when g.region != nil: every pointer now in *dst is checked.
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

// regionBulkCheck checks the pointers of the size bytes at src, of type
// typ (nil: every word), as if they had been stored to the corresponding
// slots at dst. dst == 0 means an unknown destination outside the region.
func regionBulkCheck(gp *g, r *regionState, dst, src, size uintptr, typ *_type) {
	if typ != nil && !typ.Pointers() {
		return
	}
	if typ != nil {
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

// regionFadePtr fades the object p if it is a live object of gp's region.
// Used where a pointer becomes reachable from outside the goroutine in a
// way the store barrier does not see (go statements, specials).
func regionFadePtr(gp *g, p uintptr) {
	r := gp.region
	if r == nil || p == 0 {
		return
	}
	mp := acquirem()
	if s, base, ok := r.isRegionObject(p); ok {
		if debug.regiontrace > 0 {
			print("region: fade-ptr obj=", hex(base), " size=", s.regionObjSize(base), " pc=", hex(sys.GetCallerPC()), "\n")
		}
		r.fade(s, base)
	}
	releasem(mp)
}

// fade marks the object at base of block s and, transitively, every
// live region object reachable from it as escaped.
func (r *regionState) fade(s *mspan, base uintptr) {
	r.markFaded(s, base)
	r.push(base)
	for r.sp > 0 {
		r.sp--
		base := r.stack[r.sp]
		sp := spanOf(base)
		hdr := sp.regionHdr()
		data := sp.regionData()
		w := (base - data) / goarch.PtrSize
		end := w + sp.regionObjSize(base)/goarch.PtrSize
		for i := w / 64; i*64 < end; i++ {
			bits := hdr.ptr[i]
			if i == w/64 {
				bits &^= 1<<(w%64) - 1
			}
			for bits != 0 {
				b := i*64 + uintptr(sys.TrailingZeros64(bits))
				bits &= bits - 1
				if b >= end {
					break
				}
				q := *(*uintptr)(unsafe.Pointer(data + b*goarch.PtrSize))
				if q == 0 {
					continue
				}
				if qs, qbase, ok := r.isRegionObject(q); ok {
					r.markFaded(qs, qbase)
					r.push(qbase)
				}
			}
		}
	}
}

func (r *regionState) markFaded(s *mspan, base uintptr) {
	hdr := s.regionHdr()
	w := (base - s.regionData()) / goarch.PtrSize
	hdr.faded[w/64] |= 1 << (w % 64)
	hdr.nfaded++
	size := s.regionObjSize(base)
	for l := w * goarch.PtrSize / regionLineBytes; l <= (w*goarch.PtrSize+size-1)/regionLineBytes && l < regionLines; l++ {
		hdr.fadedLines[l/64] |= 1 << (l % 64)
	}
	if hdr.dbg != nil {
		hdr.dbg[w] |= regionDbgFaded
	}
	r.faded++
	r.fadedBytes += uint64(size)
}

func (r *regionState) push(p uintptr) {
	if r.poisoned {
		return
	}
	if r.sp == len(r.stack) {
		r.poisoned = true
		r.sp = 0
		return
	}
	r.stack[r.sp] = p
	r.sp++
}

// ---- Region exit -----------------------------------------------------------

// finish ends the region: faded objects pin their lines, every other
// active line is freed (or, when abandoning, pinned), and blocks other
// than the current one are released.
func (r *regionState) finish(abandon bool) {
	r.closeRun()
	var freedLines, freedObjs uint64
	for s := r.blocks; s != nil; {
		hdr := s.regionHdr()
		next := hdr.next
		hdr.next = nil
		if abandon {
			for l := uintptr(hdr.activeLo); l < uintptr(hdr.activeHi); l++ {
				if hdr.lines[l] == regionLineActive {
					hdr.lines[l] = regionLinePinned
					hdr.pinned++
					if hdr.dbg != nil {
						mask, i := lineBitMask(l)
						for bits := hdr.start[i] & mask; bits != 0; bits &= bits - 1 {
							hdr.dbg[i*64+uintptr(sys.TrailingZeros64(bits))] |= regionDbgAbandoned
						}
					}
				}
			}
			hdr.activeLo, hdr.activeHi = 0, 0
		} else {
			r.pinFaded(s)
			s.regionFreeSpecials()
			fl, fo := s.regionFreeActiveLines()
			freedLines += fl
			freedObjs += fo
		}
		hdr.nfaded = 0
		if s != r.cur {
			r.releaseBlock(s)
		}
		s = next
	}
	r.blocks = nil
	r.cursor, r.limit, r.activeTo = 0, 0, 0
	if r.cur != nil && r.cur.regionHdr().pinned == regionLines {
		r.releaseBlock(r.cur)
		r.cur = nil
	}

	// Account the region's memory by lines: the lines it claimed as
	// allocated (large objects, in the heap statistics) and the lines
	// freed now as freed; lines that stay pinned are freed by the GC
	// sweep, which accounts them the same way. Object sizes are not
	// used because the size of an object is only known up to the next
	// object start, which the GC may have removed.
	allocBytes := r.claimedLines * regionLineBytes
	freedBytes := freedLines * regionLineBytes
	stats := memstats.heapStats.acquire()
	atomic.Xadd64(&stats.largeAllocCount, int64(r.allocs))
	atomic.Xadd64(&stats.largeAlloc, int64(allocBytes))
	atomic.Xadd64(&stats.largeFreeCount, int64(freedObjs))
	atomic.Xadd64(&stats.largeFree, int64(freedBytes))
	memstats.heapStats.release()
	gcController.totalAlloc.Add(int64(allocBytes))
	gcController.totalFree.Add(int64(freedBytes))
	// The pacer: pointerful bytes that escaped add to the scannable heap.
	if r.fadedBytes < r.scanBytes {
		gcController.update(0, int64(r.fadedBytes))
	} else {
		gcController.update(0, int64(r.scanBytes))
	}
	if !abandon {
		regionStats.freedRegions.Add(1)
		regionStats.freedObjects.Add(int64(freedObjs))
		regionStats.freedBytes.Add(int64(freedBytes))
	}
}

// pinFaded pins the lines of every faded object of s.
func (r *regionState) pinFaded(s *mspan) {
	hdr := s.regionHdr()
	if hdr.nfaded == 0 {
		return
	}
	data := s.regionData()
	var newly [regionLines]bool
	for i := range hdr.faded {
		bits := hdr.faded[i]
		for bits != 0 {
			w := uintptr(i)*64 + uintptr(sys.TrailingZeros64(bits))
			bits &= bits - 1
			base := data + w*goarch.PtrSize
			size := s.regionObjSize(base)
			for l := w * goarch.PtrSize / regionLineBytes; l <= (w*goarch.PtrSize+size-1)/regionLineBytes && l < regionLines; l++ {
				if hdr.lines[l] == regionLineActive {
					hdr.lines[l] = regionLinePinned
					hdr.pinned++
					newly[l] = true
				}
			}
		}
	}
	// The other objects of the newly pinned lines are dead: they were
	// accounted as freed with the rest of the region, so they must not
	// be counted again when the line is freed by the GC. Drop them,
	// clearing their pointer bits so that no scan reads them. (Lines
	// pinned earlier hold objects the GC owns, faded or abandoned, and
	// are left alone.)
	for l := uintptr(hdr.activeLo); l < uintptr(hdr.activeHi); l++ {
		if !newly[l] {
			continue
		}
		mask, i := lineBitMask(l)
		dead := hdr.start[i] & mask &^ hdr.faded[i]
		for bits := dead; bits != 0; bits &= bits - 1 {
			w := i*64 + uintptr(sys.TrailingZeros64(bits))
			hdr.clearPtrBits(w, s.regionObjSize(data+w*goarch.PtrSize)/goarch.PtrSize)
		}
		hdr.start[i] &^= dead
	}
}

// clearPtrBits clears n pointer bits starting at word w.
func (hdr *regionBlockHdr) clearPtrBits(w, n uintptr) {
	for e := w + n; w < e; {
		k := min(64-w%64, e-w)
		hdr.ptr[w/64] &^= (1<<k - 1) << (w % 64)
		w += k
	}
}

// regionFreeActiveLines frees every active line of s. It returns the
// number of lines and of objects freed.
//
// The start bits of the freed objects are kept: they bound the size of
// the preceding object exactly (see regionObjSize) and are cleared when
// the line is claimed again. Lookups ignore objects in free lines.
func (s *mspan) regionFreeActiveLines() (lines, objs uint64) {
	hdr := s.regionHdr()
	data := s.regionData()
	for l := uintptr(hdr.activeLo); l < uintptr(hdr.activeHi); l++ {
		if hdr.lines[l] != regionLineActive {
			continue
		}
		hdr.lines[l] = regionLineFree
		hdr.freeLines[l/64] |= 1 << (l % 64)
		lines++
		mask, i := lineBitMask(l)
		// Clear the pointer bits of the dead objects over their whole
		// extent: an object may end in a later line that stays pinned
		// for the sake of another object, and a stale pointer bit
		// there would be scanned as part of its (inflated) predecessor.
		for bits := hdr.start[i] & mask; bits != 0; bits &= bits - 1 {
			w := i*64 + uintptr(sys.TrailingZeros64(bits))
			if hdr.dbg == nil || hdr.dbg[w]&regionDbgSentinel == 0 {
				objs++
			}
			hdr.clearPtrBits(w, s.regionObjSize(data+w*goarch.PtrSize)/goarch.PtrSize)
		}
		hdr.ptr[i] &^= mask
		hdr.faded[i] &^= mask
		if debug.clobberfree != 0 {
			clobberfree(unsafe.Pointer(data+l*regionLineBytes), regionLineBytes)
		}
	}
	if lines > 0 {
		s.needzero = 1
	}
	hdr.activeLo, hdr.activeHi = 0, 0
	return
}

// lineBitMask returns the mask of the bits of line l within the bitmap
// word that holds them, and the index of that word.
func lineBitMask(l uintptr) (uint64, uintptr) {
	const perWord = 64 / regionLineWords
	return (uint64(1)<<regionLineWords - 1) << ((l % perWord) * regionLineWords), l / perWord
}

// clearLineBits clears the start, pointer and faded bits of line l.
func (hdr *regionBlockHdr) clearLineBits(l uintptr) {
	mask, i := lineBitMask(l)
	hdr.start[i] &^= mask
	hdr.ptr[i] &^= mask
	hdr.faded[i] &^= mask
	hdr.fadedLines[l/64] &^= 1 << (l % 64)
}

// lineLiveUnfaded reports whether line l is active and holds no faded
// object, in which case every object in it is a live region object.
//
//go:nosplit
func (hdr *regionBlockHdr) lineLiveUnfaded(l uintptr) bool {
	return hdr.lines[l] == regionLineActive && hdr.fadedLines[l/64]>>(l%64)&1 == 0
}

// lineObjectBytes returns the number and total size of the objects
// starting in line l.
func (s *mspan) lineObjectBytes(l uintptr) (objs, bytes uint64) {
	hdr := s.regionHdr()
	mask, i := lineBitMask(l)
	bits := hdr.start[i] & mask
	for bits != 0 {
		w := i*64 + uintptr(sys.TrailingZeros64(bits))
		bits &= bits - 1
		objs++
		bytes += uint64(s.regionObjSize(s.regionData() + w*goarch.PtrSize))
	}
	return
}

// ---- GC integration --------------------------------------------------------

// regionMarkTermination sweeps every region block in place. It runs
// during the stop-the-world mark termination, after the heap's sweepgen
// has advanced, so no region is allocating and the mark bits are final.
// Unowned blocks without live lines are returned to the heap.
func regionMarkTermination() {
	if debug.regioncheck != 0 {
		regionVerify()
	}
	lock(&regionLock)
	regionRecycle = nil
	var free *mspan
	for s := regionBlocks; s != nil; s = s.regionNext {
		hdr := s.regionHdr()
		regionSweepBlock(s)
		if hdr.owner == nil && hdr.pinned == 0 {
			s.regionRecycleNext = free
			free = s
		} else if hdr.owner == nil && hdr.pinned < regionLines {
			s.regionRecycleNext = regionRecycle
			regionRecycle = s
		}
	}
	unlock(&regionLock)
	for free != nil {
		s := free
		free = s.regionRecycleNext
		s.regionRecycleNext = nil
		regionFreeBlock(s)
	}
}

// regionSweepBlock sweeps one region block: specials of dead objects are
// run or freed, dead objects lose their bits, pinned lines with no live
// object are freed, and the mark bits are reset. Active lines of an
// owned block are left to the owner.
func regionSweepBlock(s *mspan) {
	hdr := s.regionHdr()
	data := s.regionData()
	sweepgen := mheap_.sweepgen
	mheap_.pagesSwept.Add(int64(s.npages))

	// Specials. An unmarked object with a finalizer is revived.
	hadSpecials := s.specials != nil
	siter := newSpecialsIter(s)
	for siter.valid() {
		p := s.base() + siter.s.offset
		w := (p - data) / goarch.PtrSize
		size := s.regionObjSize(p)
		mbits := s.markBitsForIndex(w)
		endOffset := siter.s.offset + size
		if !mbits.isMarked() {
			hasFin := false
			for tmp := siter.s; tmp != nil && tmp.offset < endOffset; tmp = tmp.next {
				if tmp.kind == _KindSpecialFinalizer {
					mbits.setMarkedNonAtomic()
					hasFin = true
					break
				}
			}
			for siter.valid() && siter.s.offset < endOffset {
				special := siter.s
				q := s.base() + special.offset
				if !hasFin || special.kind == _KindSpecialFinalizer || special.kind == _KindSpecialWeakHandle {
					siter.unlinkAndNext()
					freeSpecial(special, unsafe.Pointer(q), size)
				} else {
					siter.next()
				}
			}
		} else {
			if siter.s.kind == _KindSpecialReachable {
				special := siter.unlinkAndNext()
				(*specialReachable)(unsafe.Pointer(special)).reachable = true
				freeSpecial(special, unsafe.Pointer(p), size)
			} else {
				siter.next()
			}
		}
	}
	if hadSpecials && s.specials == nil {
		spanHasNoSpecials(s)
	}

	// Live objects keep their lines. Dead ones lose their pointer bits,
	// so that a later conservative mark cannot scan their stale words
	// (which may now point to freed memory), but keep their start bit
	// until the line is reclaimed, which keeps the size of the
	// preceding object exact. Marking is complete and the owner is
	// stopped, so an unmarked object in an active line is dead too; its
	// line is still freed or pinned by the owner at exit.
	var live [regionLines]bool
	var nlive uintptr
	for i := range hdr.start {
		bits := hdr.start[i]
		for bits != 0 {
			w := uintptr(i)*64 + uintptr(sys.TrailingZeros64(bits))
			bits &= bits - 1
			l := w * goarch.PtrSize / regionLineBytes
			if hdr.lines[l] == regionLineFree {
				continue
			}
			base := data + w*goarch.PtrSize
			size := s.regionObjSize(base)
			if s.markBitsForIndex(w).isMarked() {
				nlive++
				if hdr.dbg != nil {
					hdr.dbg[w] |= regionDbgMarked
				}
				for ; l <= (w*goarch.PtrSize+size-1)/regionLineBytes && l < regionLines; l++ {
					live[l] = true
				}
				continue
			}
			hdr.clearPtrBits(w, size/goarch.PtrSize)
			if hdr.dbg != nil {
				hdr.dbg[w] |= regionDbgDead
			}
			if debug.clobberfree != 0 && hdr.lines[l] == regionLinePinned {
				clobberfree(unsafe.Pointer(base), size)
			}
		}
	}
	// Pinned lines with no live object are freed. Every object that
	// starts in such a line was accounted as allocated and not yet as
	// freed; count it now.
	var deadObjs, deadBytes uint64
	for l := uintptr(0); l < regionLines; l++ {
		if hdr.lines[l] == regionLinePinned && !live[l] {
			o, _ := s.lineObjectBytes(l)
			deadObjs += o
			deadBytes += regionLineBytes
			hdr.lines[l] = regionLineFree
			hdr.freeLines[l/64] |= 1 << (l % 64)
			hdr.pinned--
			s.needzero = 1
		}
	}
	s.allocBits = s.gcmarkBits
	s.gcmarkBits = newMarkBits(regionDataWords)
	atomic.Store(&s.sweepgen, sweepgen)
	if deadObjs > 0 {
		stats := memstats.heapStats.acquire()
		atomic.Xadd64(&stats.largeFreeCount, int64(deadObjs))
		atomic.Xadd64(&stats.largeFree, int64(deadBytes))
		memstats.heapStats.release()
		gcController.totalFree.Add(int64(deadBytes))
	}
	_ = nlive
}

// regionFindReferrers prints every word of a region block that points
// into [addr, addr+size), for debugging zombie reports.
func regionFindReferrers(addr, size uintptr) {
	for s := regionBlocks; s != nil; s = s.regionNext {
		hdr := s.regionHdr()
		data := s.regionData()
		for w := uintptr(0); w < regionDataWords; w++ {
			v := *(*uintptr)(unsafe.Pointer(data + w*goarch.PtrSize))
			if v < addr || v >= addr+size {
				continue
			}
			l := w * goarch.PtrSize / regionLineBytes
			base := s.regionObjBase(data + w*goarch.PtrSize)
			print("region: referrer word=", hex(data+w*goarch.PtrSize), " -> ", hex(v), " block=", hex(s.base()), " owner=", hdr.owner, " line=", l, " state=", hdr.lines[l], " ptrbit=", hdr.ptr[w/64]>>(w%64)&1, " obj=", hex(base))
			if base != 0 {
				ow := (base - data) / goarch.PtrSize
				print(" objsize=", s.regionObjSize(base), " marked=", s.markBitsForIndex(ow).isMarked(), " faded=", hdr.faded[ow/64]>>(ow%64)&1)
			}
			print("\n")
		}
	}
}

// regionVerify checks, at mark termination, that every pointer held by a
// marked object of a region block refers to a marked heap object (or to
// non-heap memory). A violation means the marker missed an edge, or a
// region freed something still referenced. GODEBUG=regioncheck=1.
func regionVerify() {
	bad := 0
	for s := regionBlocks; s != nil; s = s.regionNext {
		hdr := s.regionHdr()
		data := s.regionData()
		for i := range hdr.start {
			bits := hdr.start[i]
			for bits != 0 {
				w := uintptr(i)*64 + uintptr(sys.TrailingZeros64(bits))
				bits &= bits - 1
				l := w * goarch.PtrSize / regionLineBytes
				if hdr.lines[l] == regionLineFree || !s.markBitsForIndex(w).isMarked() {
					continue
				}
				if hdr.dbg != nil && hdr.dbg[w]&regionDbgBlack != 0 && hdr.dbg[w]>>20 == work.cycles.Load()&0xfff {
					// Allocated black this cycle: marked without being
					// reachable, so its contents prove nothing.
					continue
				}
				base := data + w*goarch.PtrSize
				size := s.regionObjSize(base)
				for j := w; j < w+size/goarch.PtrSize; j++ {
					if hdr.ptr[j/64]>>(j%64)&1 == 0 {
						continue
					}
					q := *(*uintptr)(unsafe.Pointer(data + j*goarch.PtrSize))
					if q == 0 {
						continue
					}
					qs := spanOfHeap(q)
					if qs == nil {
						continue
					}
					var qbase uintptr
					var marked bool
					if qs.isRegionBlock {
						qbase = qs.regionObjBase(q)
						if qbase != 0 {
							marked = qs.markBitsForIndex((qbase - qs.regionData()) / goarch.PtrSize).isMarked()
						}
					} else {
						idx := qs.objIndex(q)
						if !qs.isFreeOrNewlyAllocated(idx) || idx < uintptr(qs.freeindex) {
							qbase = qs.base() + idx*qs.elemsize
							marked = qs.markBitsForIndex(idx).isMarked()
						}
					}
					if qbase != 0 && marked {
						continue
					}
					bad++
					print("region: verify: marked object ", hex(base), " size=", size, " block=", hex(s.base()), " owner=", hdr.owner, " line=", l, " state=", hdr.lines[l], " faded=", hdr.faded[w/64]>>(w%64)&1,
						" word ", hex(data+j*goarch.PtrSize), " -> ", hex(q), " target span base=", hex(qs.base()), " elemsize=", qs.elemsize, " regionblock=", qs.isRegionBlock, " targetbase=", hex(qbase), " targetmarked=", marked)
					if qs.isRegionBlock {
						qh := qs.regionHdr()
						ql := (q - qs.regionData()) / regionLineBytes
						qw := (q - qs.regionData()) / goarch.PtrSize
						print(" targetline=", ql, " state=", qh.lines[ql], " owner=", qh.owner, " targetstartbit=", qh.start[qw/64]>>(qw%64)&1)
					}
					if hdr.dbg != nil {
						print(" holderdbg=", hex(hdr.dbg[w]))
						if qs.isRegionBlock && qbase != 0 {
							print(" targetdbg=", hex(qs.regionHdr().dbg[(qbase-qs.regionData())/goarch.PtrSize]))
						} else if qs.isRegionBlock {
							print(" targetdbg(word)=", hex(qs.regionHdr().dbg[(q-qs.regionData())/goarch.PtrSize]))
						}
						print(" cycle=", work.cycles.Load())
					}
					print("\n  holder words:")
					for k := uintptr(0); k < size/goarch.PtrSize && k < 16; k++ {
						print(" ", hex(*(*uintptr)(unsafe.Pointer(base + k*goarch.PtrSize))))
					}
					print("\n  target words:")
					for k := uintptr(0); k < 8; k++ {
						print(" ", hex(*(*uintptr)(unsafe.Pointer(q + k*goarch.PtrSize))))
					}
					print("\n")
				}
			}
		}
	}
	if bad > 0 {
		throw("region: verify failed")
	}
}

// Debug history flags (GODEBUG=regioncheck).
const (
	regionDbgBlack     = 1 << 0 // allocated while the GC was marking
	regionDbgFaded     = 1 << 1
	regionDbgAbandoned = 1 << 2 // pinned by an abandoned region
	regionDbgMarked    = 1 << 3 // marked at some mark termination
	regionDbgDead      = 1 << 4 // found unmarked at a mark termination
	regionDbgSentinel  = 1 << 5 // dummy object marking the unused tail of a run
)

// verifyExit checks, before a region frees its lines, that every live
// region object referenced from a faded object has faded too: a
// violation means a store into an escaped object was not checked, or the
// transitive fade missed an edge. GODEBUG=regioncheck=1.
func (r *regionState) verifyExit() {
	bad := 0
	for s := r.blocks; s != nil; s = s.regionHdr().next {
		hdr := s.regionHdr()
		data := s.regionData()
		for i := range hdr.faded {
			bits := hdr.faded[i]
			for bits != 0 {
				w := uintptr(i)*64 + uintptr(sys.TrailingZeros64(bits))
				bits &= bits - 1
				if hdr.lines[w*goarch.PtrSize/regionLineBytes] != regionLineActive {
					continue
				}
				base := data + w*goarch.PtrSize
				size := s.regionObjSize(base)
				for j := w; j < w+size/goarch.PtrSize; j++ {
					if hdr.ptr[j/64]>>(j%64)&1 == 0 {
						continue
					}
					q := *(*uintptr)(unsafe.Pointer(data + j*goarch.PtrSize))
					if q == 0 {
						continue
					}
					qs, qbase, ok := r.isRegionObject(q)
					if !ok {
						continue
					}
					bad++
					qh := qs.regionHdr()
					qw := (qbase - qs.regionData()) / goarch.PtrSize
					print("region: verifyExit: faded object ", hex(base), " size=", size, " line=", w*goarch.PtrSize/regionLineBytes, " word ", hex(data+j*goarch.PtrSize), " -> live region object ", hex(qbase), " size=", qs.regionObjSize(qbase), " block=", hex(qs.base()), " line=", qw*goarch.PtrSize/regionLineBytes)
					if hdr.dbg != nil {
						print(" holderdbg=", hex(hdr.dbg[w]), " targetdbg=", hex(qh.dbg[qw]))
					}
					print("\n  holder words:")
					for k := uintptr(0); k < size/goarch.PtrSize && k < 16; k++ {
						print(" ", hex(*(*uintptr)(unsafe.Pointer(base + k*goarch.PtrSize))))
					}
					print("\n  holder words with bits (word:start,ptr,dbg):")
					for k := w; k < w+size/goarch.PtrSize+8 && k < regionDataWords; k++ {
						st, pt := hdr.start[k/64]>>(k%64)&1, hdr.ptr[k/64]>>(k%64)&1
						var d uint32
						if hdr.dbg != nil {
							d = hdr.dbg[k]
						}
						if st != 0 || pt != 0 || d != 0 {
							print(" ", k-w, ":", st, pt, ":", hex(d))
						}
					}
					print("\n  target words:")
					for k := uintptr(0); k < 8 && k < qs.regionObjSize(qbase)/goarch.PtrSize; k++ {
						print(" ", hex(*(*uintptr)(unsafe.Pointer(qbase + k*goarch.PtrSize))))
					}
					print("\n")
				}
			}
		}
	}
	if bad > 0 {
		throw("region: verifyExit failed")
	}
}

// regionCheckDangling reports a stored pointer into a free line of a
// region block: memory a region freed that the program still refers
// to. GODEBUG=regioncheck=1.
func regionCheckDangling(ptr, slot uintptr) {
	s := spanOf(ptr)
	if s == nil || !s.isRegionBlock || s.state.get() != mSpanInUse || ptr < s.regionData() {
		return
	}
	hdr := s.regionHdr()
	l := (ptr - s.regionData()) / regionLineBytes
	if hdr.lines[l] != regionLineFree {
		return
	}
	w := (ptr - s.regionData()) / goarch.PtrSize
	print("region: store of a pointer into freed region memory: ptr=", hex(ptr), " slot=", hex(slot), " block=", hex(s.base()), " owner=", hdr.owner, " line=", l)
	if hdr.dbg != nil {
		print(" worddbg=", hex(hdr.dbg[w]), " cycle=", work.cycles.Load())
	}
	print(" pc=", hex(regionTracePC), "\n")
	var pcs [16]uintptr
	n := callers(2, pcs[:])
	for i := 0; i < n; i++ {
		f := findfunc(pcs[i])
		if f.valid() {
			print("  ", funcname(f), " +", hex(pcs[i]-f.entry()), "\n")
		}
	}
	throw("region: dangling region pointer stored")
}

// maps_regionInheritBegin makes allocations go outside the region when
// the map at p is not a live object of the calling goroutine's region.
// It reports whether maps_regionInheritEnd must be called.
//
//go:linkname maps_regionInheritBegin
func maps_regionInheritBegin(p unsafe.Pointer) bool {
	gp := getg()
	if gp.region == nil || gp.regionIgnore != 0 || regionIsLocal(gp, uintptr(p)) {
		return false
	}
	gp.regionIgnore++
	return true
}

//go:linkname maps_regionInheritEnd
func maps_regionInheritEnd() {
	getg().regionIgnore--
}

// regionFreeSpecials frees the specials (profile records) of the objects
// in the active lines of s, which are about to be freed. Objects with
// finalizers, cleanups or weak pointers fade when the special is added,
// so their lines are pinned by then and are not active.
func (s *mspan) regionFreeSpecials() {
	if s.specials == nil {
		return
	}
	hdr := s.regionHdr()
	lock(&s.speciallock)
	for pp := &s.specials; *pp != nil; {
		sp := *pp
		if sp.offset < regionHdrBytes {
			throw("region: special in block header")
		}
		if hdr.lines[(uintptr(sp.offset)-regionHdrBytes)/regionLineBytes] != regionLineActive {
			pp = &sp.next
			continue
		}
		if sp.kind != _KindSpecialProfile {
			throw("region: freeing an object with a non-profile special")
		}
		*pp = sp.next
		p := s.base() + uintptr(sp.offset)
		freeSpecial(sp, unsafe.Pointer(p), s.regionObjSize(p))
	}
	if s.specials == nil {
		spanHasNoSpecials(s)
	}
	unlock(&s.speciallock)
}

// ---- sync hooks --------------------------------------------------------------

// sync_runtime_regionMayPool reports whether the object p may be kept in
// a sync.Pool: not if it is an object of the calling goroutine's region,
// which is freed when the region ends.
//
//go:linkname sync_runtime_regionMayPool sync.runtime_regionMayPool
func sync_runtime_regionMayPool(p unsafe.Pointer) bool {
	gp := getg()
	if gp.region == nil || p == nil {
		return true
	}
	s := spanOf(uintptr(p))
	return s == nil || !s.isRegionBlock
}

//go:linkname sync_runtime_regionActive sync.runtime_regionActive
func sync_runtime_regionActive() bool {
	return getg().region != nil
}

//go:linkname sync_runtime_regionIgnore sync.runtime_regionIgnore
func sync_runtime_regionIgnore(f func()) {
	regionIgnore(f)
}
