// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/reflectdata"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/objabi"
	"cmd/internal/src"
	"fmt"
	"internal/abi"
	"internal/buildcfg"
)

// A ZeroRegion records parts of an object which are known to be zero.
// A ZeroRegion only applies to a single memory state.
// Each bit in mask is set if the corresponding pointer-sized word of
// the base object is known to be zero.
// In other words, if mask & (1<<i) != 0, then [base+i*ptrSize, base+(i+1)*ptrSize)
// is known to be zero.
type ZeroRegion struct {
	base *Value
	mask uint64
}

// mightBeHeapPointer reports whether v might point to the heap.
// v must have pointer type.
func mightBeHeapPointer(v *Value) bool {
	if IsGlobalAddr(v) {
		return false
	}
	return true
}

// mightContainHeapPointer reports whether the data currently at addresses
// [ptr,ptr+size) might contain heap pointers. "currently" means at memory state mem.
// zeroes contains ZeroRegion data to help make that decision (see computeZeroMap).
func mightContainHeapPointer(ptr *Value, size int64, mem *Value, zeroes map[ID]ZeroRegion) bool {
	if IsReadOnlyGlobalAddr(ptr) {
		// The read-only globals section cannot contain any heap pointers.
		return false
	}

	// See if we can prove that the queried memory is all zero.

	// Find base pointer and offset. Hopefully, the base is the result of a new(T).
	var off int64
	for ptr.Op == OpOffPtr {
		off += ptr.AuxInt
		ptr = ptr.Args[0]
	}

	ptrSize := ptr.Block.Func.Config.PtrSize
	if off%ptrSize != 0 {
		return true // see issue 61187
	}
	if size%ptrSize != 0 {
		ptr.Fatalf("unaligned pointer write")
	}
	if off < 0 || off+size > 64*ptrSize {
		// memory range goes off end of tracked offsets
		return true
	}
	z := zeroes[mem.ID]
	if ptr != z.base {
		// This isn't the object we know about at this memory state.
		return true
	}
	// Mask of bits we're asking about
	m := (uint64(1)<<(size/ptrSize) - 1) << (off / ptrSize)

	if z.mask&m == m {
		// All locations are known to be zero, so no heap pointers.
		return false
	}
	return true
}

// needwb reports whether we need write barrier for store op v.
// v must be Store/Move/Zero.
// zeroes provides known zero information (keyed by ID of memory-type values).
func needwb(v *Value, zeroes map[ID]ZeroRegion) bool {
	t, ok := v.Aux.(*types.Type)
	if !ok {
		v.Fatalf("store aux is not a type: %s", v.LongString())
	}
	if !t.HasPointers() {
		return false
	}
	dst := v.Args[0]
	if IsStackAddr(dst) {
		return false // writes into the stack don't need write barrier
	}
	// If we're writing to a place that might have heap pointers, we need
	// the write barrier.
	if mightContainHeapPointer(dst, t.Size(), v.MemoryArg(), zeroes) {
		return true
	}
	// Lastly, check if the values we're writing might be heap pointers.
	// If they aren't, we don't need a write barrier.
	switch v.Op {
	case OpStore:
		if !mightBeHeapPointer(v.Args[1]) {
			return false
		}
	case OpZero:
		return false // nil is not a heap pointer
	case OpMove:
		if !mightContainHeapPointer(v.Args[1], t.Size(), v.Args[2], zeroes) {
			return false
		}
	default:
		v.Fatalf("store op unknown: %s", v.LongString())
	}
	return true
}

// needWBsrc reports whether GC needs to see v when it is the source of a store.
func needWBsrc(v *Value) bool {
	return !IsGlobalAddr(v)
}

// needWBdst reports whether GC needs to see what used to be in *ptr when ptr is
// the target of a pointer store.
func needWBdst(ptr, mem *Value, zeroes map[ID]ZeroRegion) bool {
	// Detect storing to zeroed memory.
	var off int64
	for ptr.Op == OpOffPtr {
		off += ptr.AuxInt
		ptr = ptr.Args[0]
	}
	ptrSize := ptr.Block.Func.Config.PtrSize
	if off%ptrSize != 0 {
		return true // see issue 61187
	}
	if off < 0 || off >= 64*ptrSize {
		// write goes off end of tracked offsets
		return true
	}
	z := zeroes[mem.ID]
	if ptr != z.base {
		return true
	}
	// If destination is known to be zeroed, we don't need the write barrier
	// to record the old value in *ptr.
	return z.mask>>uint(off/ptrSize)&1 == 0
}

// writebarrier pass inserts write barriers for store ops (Store, Move, Zero)
// when necessary (the condition above). It rewrites store ops to branches
// and runtime calls, like
//
//	if writeBarrier.enabled {
//		buf := gcWriteBarrier2()	// Not a regular Go call
//		buf[0] = val
//		buf[1] = *ptr
//	}
//	*ptr = val
//
// A sequence of WB stores for many pointer fields of a single type will
// be emitted together, with a single branch.
func writebarrier(f *Func) {
	if !f.fe.UseWriteBarrier() {
		return
	}

	// Number of write buffer entries we can request at once.
	// Must match runtime/mwbbuf.go:wbMaxEntriesPerCall.
	// It must also match the number of instances of runtime.gcWriteBarrier{X}.
	const maxEntries = 8

	var sb, sp, wbaddr, const0 *Value
	var cgoCheckPtrWrite, cgoCheckMemmove *obj.LSym
	var wbZero, wbMove, wbRegion, wbRegionMove *obj.LSym
	var stores, after []*Value
	var sset, sset2 *sparseSet
	var storeNumber []int32

	// Compute map from a value to the SelectN [1] value that uses it.
	select1 := f.Cache.allocValueSlice(f.NumValues())
	defer func() { f.Cache.freeValueSlice(select1) }()
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpSelectN {
				continue
			}
			if v.AuxInt != 1 {
				continue
			}
			select1[v.Args[0].ID] = v
		}
	}

	zeroes := f.computeZeroMap(select1)
	rp := regionParamsAnalyze(f)
	for _, b := range f.Blocks { // range loop is safe since the blocks we added contain no stores to expand
		// first, identify all the stores that need to insert a write barrier.
		// mark them with WB ops temporarily. record presence of WB ops.
		nWBops := 0 // count of temporarily created WB ops remaining to be rewritten in the current block
		for _, v := range b.Values {
			switch v.Op {
			case OpStore, OpMove, OpZero:
				if needwb(v, zeroes) {
					switch v.Op {
					case OpStore:
						v.Op = OpStoreWB
					case OpMove:
						v.Op = OpMoveWB
					case OpZero:
						v.Op = OpZeroWB
					}
					nWBops++
				}
			}
		}
		if nWBops == 0 {
			continue
		}

		if wbaddr == nil {
			// lazily initialize global values for write barrier test and calls
			// find SB and SP values in entry block
			initpos := f.Entry.Pos
			sp, sb = f.spSb()
			wbsym := f.fe.Syslook("writeBarrier")
			wbaddr = f.Entry.NewValue1A(initpos, OpAddr, f.Config.Types.UInt32Ptr, wbsym, sb)
			wbZero = f.fe.Syslook("wbZero")
			wbMove = f.fe.Syslook("wbMove")
			wbRegion = f.fe.Syslook("wbRegion")
			wbRegionMove = f.fe.Syslook("wbRegionMove")
			if buildcfg.Experiment.CgoCheck2 {
				cgoCheckPtrWrite = f.fe.Syslook("cgoCheckPtrWrite")
				cgoCheckMemmove = f.fe.Syslook("cgoCheckMemmove")
			}
			const0 = f.ConstInt32(f.Config.Types.UInt32, 0)

			// allocate auxiliary data structures for computing store order
			sset = f.newSparseSet(f.NumValues())
			defer f.retSparseSet(sset)
			sset2 = f.newSparseSet(f.NumValues())
			defer f.retSparseSet(sset2)
			storeNumber = f.Cache.allocInt32Slice(f.NumValues())
			defer f.Cache.freeInt32Slice(storeNumber)
		}

		// order values in store order
		b.Values = storeOrder(b.Values, sset, storeNumber)
	again:
		// find the start and end of the last contiguous WB store sequence.
		// a branch will be inserted there. values after it will be moved
		// to a new block.
		var last *Value
		var start, end int
		var nonPtrStores int
		values := b.Values
		hasMove := false
	FindSeq:
		for i := len(values) - 1; i >= 0; i-- {
			w := values[i]
			switch w.Op {
			case OpStoreWB, OpMoveWB, OpZeroWB:
				start = i
				if last == nil {
					last = w
					end = i + 1
				}
				nonPtrStores = 0
				if w.Op == OpMoveWB {
					hasMove = true
				}
			case OpVarDef, OpVarLive:
				continue
			case OpStore:
				if last == nil {
					continue
				}
				nonPtrStores++
				if nonPtrStores > 2 {
					break FindSeq
				}
				if hasMove {
					// We need to ensure that this store happens
					// before we issue a wbMove, as the wbMove might
					// use the result of this store as its source.
					// Even though this store is not write-barrier
					// eligible, it might nevertheless be the store
					// of a pointer to the stack, which is then the
					// source of the move.
					// See issue 71228.
					break FindSeq
				}
			default:
				if last == nil {
					continue
				}
				break FindSeq
			}
		}
		stores = append(stores[:0], b.Values[start:end]...) // copy to avoid aliasing
		after = append(after[:0], b.Values[end:]...)
		b.Values = b.Values[:start]

		// find the memory before the WB stores
		mem := stores[0].MemoryArg()
		pos := stores[0].Pos

		// If there is a nil check before the WB store, duplicate it to
		// the two branches, where the store and the WB load occur. So
		// they are more likely be removed by late nilcheck removal (which
		// is block-local).
		var nilcheck, nilcheckThen, nilcheckEnd *Value
		if a := stores[0].Args[0]; a.Op == OpNilCheck && a.Args[1] == mem {
			nilcheck = a
		}

		// If the source of a MoveWB is volatile (will be clobbered by a
		// function call), we need to copy it to a temporary location, as
		// marshaling the args of wbMove might clobber the value we're
		// trying to move.
		// Look for volatile source, copy it to temporary before we check
		// the write barrier flag.
		// It is unlikely to have more than one of them. Just do a linear
		// search instead of using a map.
		// See issue 15854.
		type volatileCopy struct {
			src *Value // address of original volatile value
			tmp *Value // address of temporary we've copied the volatile value into
		}
		var volatiles []volatileCopy

		if !(f.ABIDefault == f.ABI1 && len(f.Config.intParamRegs) >= 3) {
			// We don't need to do this if the calls we're going to do take
			// all their arguments in registers.
			// 3 is the magic number because it covers wbZero, wbMove, cgoCheckMemmove.
		copyLoop:
			for _, w := range stores {
				if w.Op == OpMoveWB {
					val := w.Args[1]
					if isVolatile(val) {
						for _, c := range volatiles {
							if val == c.src {
								continue copyLoop // already copied
							}
						}

						t := val.Type.Elem()
						tmp := f.NewLocal(w.Pos, t)
						mem = b.NewValue1A(w.Pos, OpVarDef, types.TypeMem, tmp, mem)
						tmpaddr := b.NewValue2A(w.Pos, OpLocalAddr, t.PtrTo(), tmp, sp, mem)
						siz := t.Size()
						mem = b.NewValue3I(w.Pos, OpMove, types.TypeMem, siz, tmpaddr, val, mem)
						mem.Aux = t
						volatiles = append(volatiles, volatileCopy{val, tmpaddr})
					}
				}
			}
		}

		// Build branch point.
		bThen := f.NewBlock(BlockPlain)
		bEnd := f.NewBlock(b.Kind)
		bThen.Pos = pos
		bEnd.Pos = b.Pos
		b.Pos = pos

		// Set up control flow for end block.
		bEnd.CopyControls(b)
		bEnd.Likely = b.Likely
		for _, e := range b.Succs {
			bEnd.Succs = append(bEnd.Succs, e)
			e.b.Preds[e.i].b = bEnd
		}

		cfgtypes := &f.Config.Types

		// set up control flow for write barrier test
		// load word, test word, avoiding partial register write from load byte.
		flag := b.NewValue2(pos, OpLoad, cfgtypes.UInt32, wbaddr, mem)
		flag = b.NewValue2(pos, OpNeq32, cfgtypes.Bool, flag, const0)
		b.Kind = BlockIf
		b.SetControl(flag)
		b.Likely = BranchUnlikely
		b.Succs = b.Succs[:0]
		b.AddEdgeTo(bThen)
		b.AddEdgeTo(bEnd)
		bThen.AddEdgeTo(bEnd)

		// For each write barrier store, append write barrier code to bThen.
		memThen := mem

		if nilcheck != nil {
			nilcheckThen = bThen.NewValue2(nilcheck.Pos, OpNilCheck, nilcheck.Type, nilcheck.Args[0], memThen)
		}

		// Note: we can issue the write barrier code in any order. In particular,
		// it doesn't matter if they are in a different order *even if* they end
		// up referring to overlapping memory regions. For instance if an OpStore
		// stores to a location that is later read by an OpMove. In all cases
		// any pointers we must get into the write barrier buffer still make it,
		// possibly in a different order and possibly a different (but definitely
		// more than 0) number of times.
		// In light of that, we process all the OpStoreWBs first. This minimizes
		// the amount of spill/restore code we need around the Zero/Move calls.

		// srcs contains the value IDs of pointer values we've put in the write barrier buffer.
		srcs := sset
		srcs.clear()
		// dsts contains the value IDs of locations which we've read a pointer out of
		// and put the result in the write barrier buffer.
		dsts := sset2
		dsts.clear()

		// Buffer up entries that we need to put in the write barrier buffer.
		type write struct {
			ptr *Value   // value to put in write barrier buffer
			pos src.XPos // location to use for the write
		}
		var writeStore [maxEntries]write
		writes := writeStore[:0]

		flush := func() {
			if len(writes) == 0 {
				return
			}
			// Issue a call to get a write barrier buffer.
			t := types.NewTuple(types.Types[types.TUINTPTR].PtrTo(), types.TypeMem)
			call := bThen.NewValue1I(pos, OpWB, t, int64(len(writes)), memThen)
			curPtr := bThen.NewValue1(pos, OpSelect0, types.Types[types.TUINTPTR].PtrTo(), call)
			memThen = bThen.NewValue1(pos, OpSelect1, types.TypeMem, call)
			// Write each pending pointer to a slot in the buffer.
			for i, write := range writes {
				wbuf := bThen.NewValue1I(write.pos, OpOffPtr, types.Types[types.TUINTPTR].PtrTo(), int64(i)*f.Config.PtrSize, curPtr)
				memThen = bThen.NewValue3A(write.pos, OpStore, types.TypeMem, types.Types[types.TUINTPTR], wbuf, write.ptr, memThen)
			}
			writes = writes[:0]
		}
		addEntry := func(pos src.XPos, ptr *Value) {
			writes = append(writes, write{ptr: ptr, pos: pos})
			if len(writes) == maxEntries {
				flush()
			}
		}

		// Find all the pointers we need to write to the buffer.
		for _, w := range stores {
			if w.Op != OpStoreWB {
				continue
			}
			pos := w.Pos
			ptr := w.Args[0]
			val := w.Args[1]
			if !srcs.contains(val.ID) && needWBsrc(val) {
				srcs.add(val.ID)
				addEntry(pos, val)
			}
			if !dsts.contains(ptr.ID) && needWBdst(ptr, w.Args[2], zeroes) {
				dsts.add(ptr.ID)
				// Load old value from store target.
				// Note: This turns bad pointer writes into bad
				// pointer reads, which could be confusing. We could avoid
				// reading from obviously bad pointers, which would
				// take care of the vast majority of these. We could
				// patch this up in the signal handler, or use XCHG to
				// combine the read and the write.
				if ptr == nilcheck {
					ptr = nilcheckThen
				}
				oldVal := bThen.NewValue2(pos, OpLoad, types.Types[types.TUINTPTR], ptr, memThen)
				// Save old value to write buffer.
				addEntry(pos, oldVal)
			}
			f.fe.Func().SetWBPos(pos)
			nWBops--
		}
		flush()

		// Now do the rare cases, Zeros and Moves.
		for _, w := range stores {
			pos := w.Pos
			dst := w.Args[0]
			if dst == nilcheck {
				dst = nilcheckThen
			}
			switch w.Op {
			case OpZeroWB:
				typ := reflectdata.TypeLinksym(w.Aux.(*types.Type))
				// zeroWB(&typ, dst)
				taddr := b.NewValue1A(pos, OpAddr, b.Func.Config.Types.Uintptr, typ, sb)
				memThen = wbcall(pos, bThen, wbZero, sp, memThen, taddr, dst)
				f.fe.Func().SetWBPos(pos)
				nWBops--
			case OpMoveWB:
				src := w.Args[1]
				if isVolatile(src) {
					for _, c := range volatiles {
						if src == c.src {
							src = c.tmp
							break
						}
					}
				}
				typ := reflectdata.TypeLinksym(w.Aux.(*types.Type))
				// moveWB(&typ, dst, src)
				taddr := b.NewValue1A(pos, OpAddr, b.Func.Config.Types.Uintptr, typ, sb)
				memThen = wbcall(pos, bThen, wbMove, sp, memThen, taddr, dst, src)
				f.fe.Func().SetWBPos(pos)
				nWBops--
			}
		}

		// merge memory
		mem = bEnd.NewValue2(pos, OpPhi, types.TypeMem, mem, memThen)

		if nilcheck != nil {
			nilcheckEnd = bEnd.NewValue2(nilcheck.Pos, OpNilCheck, nilcheck.Type, nilcheck.Args[0], mem)
		}

		// Do raw stores after merge point.
		for _, w := range stores {
			pos := w.Pos
			dst := w.Args[0]
			if dst == nilcheck {
				dst = nilcheckEnd
			}
			switch w.Op {
			case OpStoreWB:
				val := w.Args[1]
				if buildcfg.Experiment.CgoCheck2 {
					// Issue cgo checking code.
					mem = wbcall(pos, bEnd, cgoCheckPtrWrite, sp, mem, dst, val)
				}
				mem = bEnd.NewValue3A(pos, OpStore, types.TypeMem, w.Aux, dst, val, mem)
			case OpZeroWB:
				mem = bEnd.NewValue2I(pos, OpZero, types.TypeMem, w.AuxInt, dst, mem)
				mem.Aux = w.Aux
			case OpMoveWB:
				src := w.Args[1]
				if isVolatile(src) {
					for _, c := range volatiles {
						if src == c.src {
							src = c.tmp
							break
						}
					}
				}
				if buildcfg.Experiment.CgoCheck2 {
					// Issue cgo checking code.
					typ := reflectdata.TypeLinksym(w.Aux.(*types.Type))
					taddr := b.NewValue1A(pos, OpAddr, b.Func.Config.Types.Uintptr, typ, sb)
					mem = wbcall(pos, bEnd, cgoCheckMemmove, sp, mem, taddr, dst, src)
				}
				mem = bEnd.NewValue3I(pos, OpMove, types.TypeMem, w.AuxInt, dst, src, mem)
				mem.Aux = w.Aux
			case OpVarDef, OpVarLive:
				mem = bEnd.NewValue1A(pos, w.Op, types.TypeMem, w.Aux, mem)
			case OpStore:
				val := w.Args[1]
				mem = bEnd.NewValue3A(pos, OpStore, types.TypeMem, w.Aux, dst, val, mem)
			}
		}

		// The last store becomes the WBend marker. This marker is used by the liveness
		// pass to determine what parts of the code are preemption-unsafe.
		// All subsequent memory operations use this memory, so we have to sacrifice the
		// previous last memory op to become this new value.
		wbend := bEnd.NewValue1(last.Pos.WithNotStmt(), OpWBend, types.TypeMem, mem)

		// Memory region escape check (see runtime/region.go). After the
		// stores are done, if the current goroutine is inside a region
		// (g.region != nil), call wbRegion for every pointer stored and
		// wbRegionMove for every typed move. The check runs after the
		// whole batch of stores, not before, because it walks the
		// object graph: a pointer stored earlier in the same batch must
		// be visible to it. It is outside the preemption-unsafe region
		// that ends at WBend, so the calls are ordinary calls.
		//
		// bEnd ends with the region test; bRegion holds the calls; bAfter
		// takes over bEnd's role as the continuation of the block.
		//
		// Stores into an object this function just allocated and has
		// not yet published (regionFreshDst) need no check: the object
		// is a live region object whenever the goroutine is in a region
		// and allocating in it. They are checked only when allocations
		// are being placed outside the region (g.regionIgnore != 0), in
		// a further block bIgnore.
		bRegion := f.NewBlock(BlockPlain)
		bAfter := f.NewBlock(bEnd.Kind)
		bRegion.Pos = pos
		bAfter.Pos = bEnd.Pos
		bAfter.CopyControls(bEnd)
		bAfter.Likely = bEnd.Likely
		for _, e := range bEnd.Succs {
			bAfter.Succs = append(bAfter.Succs, e)
			e.b.Preds[e.i].b = bAfter
		}
		bEnd.ResetControls()
		g := bEnd.NewValue1(pos, OpGetG, cfgtypes.BytePtr, wbend)
		{
			raddr := bEnd.NewValue1I(pos, OpOffPtr, cfgtypes.BytePtrPtr, abi.GRegionOffset, g)
			rval := bEnd.NewValue2(pos, OpLoad, cfgtypes.BytePtr, raddr, wbend)
			rflag := bEnd.NewValue1(pos, OpIsNonNil, cfgtypes.Bool, rval)
			bEnd.Kind = BlockIf
			bEnd.SetControl(rflag)
			bEnd.Likely = BranchUnlikely
			// The branch belongs to the store, not to whatever
			// statement ended the original block.
			bEnd.Pos = pos.WithNotStmt()
			bEnd.Succs = bEnd.Succs[:0]
			bEnd.AddEdgeTo(bRegion)
			bEnd.AddEdgeTo(bAfter)
		}
		regionCheck := func(w *Value, blk *Block, mem *Value) *Value {
			dst := w.Args[0]
			if dst == nilcheck {
				dst = nilcheckEnd
			}
			switch w.Op {
			case OpStoreWB:
				return wbcall(w.Pos, blk, wbRegion, sp, mem, dst, w.Args[1])
			case OpMoveWB:
				// The data has been moved; let the runtime read the
				// pointers from the destination.
				typ := reflectdata.TypeLinksym(w.Aux.(*types.Type))
				taddr := b.NewValue1A(pos, OpAddr, cfgtypes.Uintptr, typ, sb)
				return wbcall(w.Pos, blk, wbRegionMove, sp, mem, taddr, dst, dst)
			}
			return mem
		}
		memRegion := wbend
		var fresh []*Value
		var byParam [4][]*Value
		for _, w := range stores {
			if w.Op == OpStoreWB && !needWBsrc(w.Args[1]) {
				continue
			}
			if regionFreshDst(w, select1) {
				fresh = append(fresh, w)
				continue
			}
			if i := rp.regionParamOf(w); i >= 0 {
				byParam[i] = append(byParam[i], w)
				continue
			}
			memRegion = regionCheck(w, bRegion, memRegion)
		}
		// Stores into a parameter's object are checked only when the
		// entry check found it outside the region (regionParams).
		cur := bRegion
		for i, ws := range byParam {
			if len(ws) == 0 {
				continue
			}
			bCall := f.NewBlock(BlockPlain)
			bNext := f.NewBlock(BlockPlain)
			bCall.Pos, bNext.Pos = pos, pos
			bit := cur.NewValue2(pos, OpAnd64, cfgtypes.Uintptr, rp.flags, f.ConstInt64(cfgtypes.Uintptr, 1<<i))
			cur.Kind = BlockIf
			cur.SetControl(cur.NewValue2(pos, OpNeq64, cfgtypes.Bool, bit, f.ConstInt64(cfgtypes.Uintptr, 0)))
			cur.Likely = BranchLikely
			cur.AddEdgeTo(bNext)
			cur.AddEdgeTo(bCall)
			memCall := memRegion
			for _, w := range ws {
				memCall = regionCheck(w, bCall, memCall)
			}
			bCall.AddEdgeTo(bNext)
			merge := bNext.NewValue0(pos, OpPhi, types.TypeMem)
			merge.AddArg2(memRegion, memCall)
			memRegion = merge
			cur = bNext
		}
		if len(fresh) == 0 {
			cur.AddEdgeTo(bAfter)
			// last becomes the merge of the two paths, so that
			// everything that used the last store's memory now uses
			// the merged memory.
			bAfter.Values = append(bAfter.Values, last)
			last.Block = bAfter
			last.reset(OpPhi)
			last.Pos = last.Pos.WithNotStmt()
			last.Type = types.TypeMem
			last.AddArgs(wbend, memRegion)
		} else {
			bIgnore := f.NewBlock(BlockPlain)
			bIgnore.Pos = pos
			iaddr := cur.NewValue1I(pos, OpOffPtr, cfgtypes.BytePtrPtr, abi.GRegionIgnoreOffset, g)
			ival := cur.NewValue2(pos, OpLoad, cfgtypes.BytePtr, iaddr, memRegion)
			iflag := cur.NewValue1(pos, OpIsNonNil, cfgtypes.Bool, ival)
			cur.Kind = BlockIf
			cur.SetControl(iflag)
			cur.Likely = BranchUnlikely
			cur.AddEdgeTo(bIgnore)
			cur.AddEdgeTo(bAfter)
			memIgnore := memRegion
			for _, w := range fresh {
				memIgnore = regionCheck(w, bIgnore, memIgnore)
			}
			bIgnore.AddEdgeTo(bAfter)
			bAfter.Values = append(bAfter.Values, last)
			last.Block = bAfter
			last.reset(OpPhi)
			last.Pos = last.Pos.WithNotStmt()
			last.Type = types.TypeMem
			last.AddArgs(wbend, memRegion, memIgnore)
		}

		// Free all the old stores, except last which became the merge.
		for _, w := range stores {
			if w != last {
				w.resetArgs()
			}
		}
		for _, w := range stores {
			if w != last {
				f.freeValue(w)
			}
		}
		if nilcheck != nil && nilcheck.Uses == 0 {
			nilcheck.reset(OpInvalid)
		}

		// put values after the store sequence into the continuation block
		bAfter.Values = append(bAfter.Values, after...)
		for _, w := range after {
			w.Block = bAfter
		}

		// if we have more stores in this block, do this block again
		if nWBops > 0 {
			goto again
		}
	}
}

// computeZeroMap returns a map from an ID of a memory value to
// a set of locations that are known to be zeroed at that memory value.
func (f *Func) computeZeroMap(select1 []*Value) map[ID]ZeroRegion {

	ptrSize := f.Config.PtrSize
	// Keep track of which parts of memory are known to be zero.
	// This helps with removing write barriers for various initialization patterns.
	// This analysis is conservative. We only keep track, for each memory state, of
	// which of the first 64 words of a single object are known to be zero.
	zeroes := map[ID]ZeroRegion{}
	// Find new objects.
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if mem, ok := IsNewObject(v, select1); ok {
				// While compiling package runtime itself, we might see user
				// calls to newobject, which will have result type
				// unsafe.Pointer instead. We can't easily infer how large the
				// allocated memory is, so just skip it.
				if types.LocalPkg.Path == "runtime" && v.Type.IsUnsafePtr() {
					continue
				}

				nptr := min(64, v.Type.Elem().Size()/ptrSize)
				zeroes[mem.ID] = ZeroRegion{base: v, mask: 1<<uint(nptr) - 1}
			}
		}
	}
	// Find stores to those new objects.
	for {
		changed := false
		for _, b := range f.Blocks {
			// Note: iterating forwards helps convergence, as values are
			// typically (but not always!) in store order.
			for _, v := range b.Values {
				if v.Op != OpStore {
					continue
				}
				z, ok := zeroes[v.MemoryArg().ID]
				if !ok {
					continue
				}
				ptr := v.Args[0]
				var off int64
				size := v.Aux.(*types.Type).Size()
				for ptr.Op == OpOffPtr {
					off += ptr.AuxInt
					ptr = ptr.Args[0]
				}
				if ptr != z.base {
					// Different base object - we don't know anything.
					// We could even be writing to the base object we know
					// about, but through an aliased but offset pointer.
					// So we have to throw all the zero information we have away.
					continue
				}
				// Round to cover any partially written pointer slots.
				// Pointer writes should never be unaligned like this, but non-pointer
				// writes to pointer-containing types will do this.
				if d := off % ptrSize; d != 0 {
					off -= d
					size += d
				}
				if d := size % ptrSize; d != 0 {
					size += ptrSize - d
				}
				// Clip to the 64 words that we track.
				minimum := max(off, 0)
				maximum := min(off+size, 64*ptrSize)

				// Clear bits for parts that we are writing (and hence
				// will no longer necessarily be zero).
				for i := minimum; i < maximum; i += ptrSize {
					bit := i / ptrSize
					z.mask &^= 1 << uint(bit)
				}
				if z.mask == 0 {
					// No more known zeros - don't bother keeping.
					continue
				}
				// Save updated known zero contents for new store.
				if zeroes[v.ID] != z {
					zeroes[v.ID] = z
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}
	if f.pass.debug > 0 {
		fmt.Printf("func %s\n", f.Name)
		for mem, z := range zeroes {
			fmt.Printf("  memory=v%d ptr=%v zeromask=%b\n", mem, z.base, z.mask)
		}
	}
	return zeroes
}

// wbcall emits write barrier runtime call in b, returns memory.
func wbcall(pos src.XPos, b *Block, fn *obj.LSym, sp, mem *Value, args ...*Value) *Value {
	config := b.Func.Config
	typ := config.Types.Uintptr // type of all argument values
	nargs := len(args)

	// TODO (register args) this is a bit of a hack.
	inRegs := b.Func.ABIDefault == b.Func.ABI1 && len(config.intParamRegs) >= 3

	if !inRegs {
		// Store arguments to the appropriate stack slot.
		off := config.ctxt.Arch.FixedFrameSize
		for _, arg := range args {
			stkaddr := b.NewValue1I(pos, OpOffPtr, typ.PtrTo(), off, sp)
			mem = b.NewValue3A(pos, OpStore, types.TypeMem, typ, stkaddr, arg, mem)
			off += typ.Size()
		}
		args = args[:0]
	}

	args = append(args, mem)

	// issue call
	argTypes := make([]*types.Type, nargs, 3) // at most 3 args; allows stack allocation
	for i := 0; i < nargs; i++ {
		argTypes[i] = typ
	}
	call := b.NewValue0A(pos, OpStaticCall, types.TypeResultMem, StaticAuxCall(fn, b.Func.ABIDefault.ABIAnalyzeTypes(argTypes, nil)))
	call.AddArgs(args...)
	call.AuxInt = int64(nargs) * typ.Size()
	return b.NewValue1I(pos, OpSelectN, types.TypeMem, 0, call)
}

// IsStackAddr reports whether v is known to be an address of a stack slot.
func IsStackAddr(v *Value) bool {
	for v.Op == OpOffPtr || v.Op == OpAddPtr || v.Op == OpPtrIndex || v.Op == OpCopy {
		v = v.Args[0]
	}
	switch v.Op {
	case OpSP, OpLocalAddr, OpSelectNAddr, OpGetCallerSP:
		return true
	}
	return false
}

// IsGlobalAddr reports whether v is known to be an address of a global (or nil).
func IsGlobalAddr(v *Value) bool {
	for v.Op == OpOffPtr || v.Op == OpAddPtr || v.Op == OpPtrIndex || v.Op == OpCopy {
		v = v.Args[0]
	}
	if v.Op == OpAddr && v.Args[0].Op == OpSB {
		return true // address of a global
	}
	if v.Op == OpConstNil {
		return true
	}
	if v.Op == OpLoad && IsReadOnlyGlobalAddr(v.Args[0]) {
		return true // loading from a read-only global - the resulting address can't be a heap address.
	}
	return false
}

// IsReadOnlyGlobalAddr reports whether v is known to be an address of a read-only global.
func IsReadOnlyGlobalAddr(v *Value) bool {
	if v.Op == OpConstNil {
		// Nil pointers are read only. See issue 33438.
		return true
	}
	if v.Op == OpAddr && v.Aux != nil && v.Aux.(*obj.LSym).Type == objabi.SRODATA {
		return true
	}
	return false
}

// IsNewObject reports whether v is a pointer to a freshly allocated & zeroed object,
// if so, also returns the memory state mem at which v is zero.
func IsNewObject(v *Value, select1 []*Value) (mem *Value, ok bool) {
	f := v.Block.Func
	c := f.Config
	if f.ABIDefault == f.ABI1 && len(c.intParamRegs) >= 1 {
		if v.Op != OpSelectN || v.AuxInt != 0 {
			return nil, false
		}
		mem = select1[v.Args[0].ID]
		if mem == nil {
			return nil, false
		}
	} else {
		if v.Op != OpLoad {
			return nil, false
		}
		mem = v.MemoryArg()
		if mem.Op != OpSelectN {
			return nil, false
		}
		if mem.Type != types.TypeMem {
			return nil, false
		} // assume it is the right selection if true
	}
	call := mem.Args[0]
	if call.Op != OpStaticCall {
		return nil, false
	}
	// Check for new object, or for new object calls that have been transformed into size-specialized malloc calls.
	// Calls that have return type unsafe pointer may have originally been produced by flushPendingHeapAllocations
	// in the ssa generator, so may have not originally been newObject calls.
	var numParameters int64
	switch {
	case isNewObject(call.Aux):
		numParameters = 1
	case isSpecializedMalloc(call.Aux) && !v.Type.IsUnsafePtr():
		numParameters = 3
	default:
		return nil, false
	}
	if f.ABIDefault == f.ABI1 && len(c.intParamRegs) >= 1 {
		if v.Args[0] == call {
			return mem, true
		}
		return nil, false
	}
	if v.Args[0].Op != OpOffPtr {
		return nil, false
	}
	if v.Args[0].Args[0].Op != OpSP {
		return nil, false
	}
	if v.Args[0].AuxInt != c.ctxt.Arch.FixedFrameSize+numParameters*c.RegSize { // offset of return value
		return nil, false
	}
	return mem, true
}

// IsSanitizerSafeAddr reports whether v is known to be an address
// that doesn't need instrumentation.
func IsSanitizerSafeAddr(v *Value) bool {
	for v.Op == OpOffPtr || v.Op == OpAddPtr || v.Op == OpPtrIndex || v.Op == OpCopy {
		v = v.Args[0]
	}
	switch v.Op {
	case OpSP, OpLocalAddr, OpSelectNAddr:
		// Stack addresses are always safe.
		return true
	case OpITab, OpStringPtr, OpGetClosurePtr:
		// Itabs, string data, and closure fields are
		// read-only once initialized.
		return true
	case OpAddr:
		vt := v.Aux.(*obj.LSym).Type
		return vt == objabi.SRODATA || vt == objabi.SLIBFUZZER_8BIT_COUNTER || vt == objabi.SCOVERAGE_COUNTER || vt == objabi.SCOVERAGE_AUXVAR
	}
	return false
}

// isVolatile reports whether v is a pointer to argument region on stack which
// will be clobbered by a function call.
func isVolatile(v *Value) bool {
	for v.Op == OpOffPtr || v.Op == OpAddPtr || v.Op == OpPtrIndex || v.Op == OpCopy || v.Op == OpSelectNAddr {
		v = v.Args[0]
	}
	return v.Op == OpSP
}

// regionMaxObjectBytes is the largest allocation a memory region serves;
// it must match runtime.regionMaxObject.
const regionMaxObjectBytes = 16 << 10

// regionFreshDst reports whether the destination of store w is an object
// that this function allocated (newobject or a specialized malloc). Such
// an object is a live object of the goroutine's region whenever the
// goroutine is in a region and allocating in it, and a store into a live
// region object needs no region check (see runtime.wbRegion): the
// object stays one for the rest of the function, published or not,
// since faded objects' pointers are faded at region exit.
func regionFreshDst(w *Value, select1 []*Value) bool {
	obj := regionBase(w.Args[0])
	if _, ok := IsNewObject(obj, select1); !ok {
		return false
	}
	t := obj.Type
	return t.IsPtr() && t.Elem().Size() != 0 && t.Elem().Size() <= regionMaxObjectBytes
}

// regionBase strips address arithmetic and nil checks from v: the object
// a store into *v is into.
func regionBase(v *Value) *Value {
	for v.Op == OpOffPtr || v.Op == OpCopy || v.Op == OpAddPtr || v.Op == OpPtrIndex || v.Op == OpNilCheck {
		v = v.Args[0]
	}
	return v
}

// regionParams is the per-function state of the store check elision on
// stores into the objects of pointer parameters. At entry, if the
// goroutine is in a region, runtime.regionParamsLocalN reports as a bit
// mask which parameters point into live objects of the region; a store
// into such an object needs no check (see regionFreshDst), so the check
// is made conditional on the bit. The call costs about as much as one
// check, so only parameters that are stored into are considered, and at
// most four.
type regionParams struct {
	params []*Value // the parameters, by bit
	flags  *Value   // the mask (a phi: 0 when not in a region)
}

// regionParamOf returns the bit of the parameter whose object w stores
// into, or -1.
func (rp *regionParams) regionParamOf(w *Value) int {
	if rp == nil {
		return -1
	}
	base := regionBase(w.Args[0])
	for i, p := range rp.params {
		if p == base {
			return i
		}
	}
	return -1
}

// regionParamsAnalyze picks the parameters whose objects the function
// stores pointers into and inserts the entry check. It must run before
// any block is rewritten.
func regionParamsAnalyze(f *Func) *regionParams {
	if base.Debug.RegionParams == 0 || f.Config.PtrSize != 8 || base.Flag.Cfg.Instrumenting ||
		f.ABIDefault != f.ABI1 || len(f.Config.intParamRegs) < 4 {
		return nil
	}
	// The runtime itself must not call into the check (nosplit
	// functions, the system stack); the map implementation is ordinary
	// code with hot stores into its parameters (Iter.Next).
	if base.Flag.CompilingRuntime && base.Ctxt.Pkgpath != "internal/runtime/maps" {
		return nil
	}
	// Pointer stores by parameter, and whether a parameter has a store
	// that runs on every path to a return: the entry check costs about
	// one store check, so a single store on a cold path (an error
	// field, say) does not pay for it.
	count := make(map[*Value]int)
	always := make(map[*Value]bool)
	var exits []*Block
	for _, b := range f.Blocks {
		if b.Kind == BlockRet || b.Kind == BlockRetJmp {
			exits = append(exits, b)
		}
	}
	sdom := f.Sdom()
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			switch v.Op {
			case OpStore:
				if !v.Aux.(*types.Type).HasPointers() || !needWBsrc(v.Args[1]) {
					continue
				}
			case OpMove:
				if !v.Aux.(*types.Type).HasPointers() {
					continue
				}
			default:
				continue
			}
			p := regionBase(v.Args[0])
			if (p.Op == OpArg || p.Op == OpArgIntReg) && p.Block == f.Entry && (p.Type.IsPtr() || p.Type.IsUnsafePtr()) {
				count[p]++
				if !always[p] {
					all := len(exits) > 0
					for _, e := range exits {
						if !sdom.IsAncestorEq(b, e) {
							all = false
							break
						}
					}
					always[p] = all
				}
			}
		}
	}
	var params []*Value
	for p, n := range count {
		if n >= 2 || always[p] {
			params = append(params, p)
		}
	}
	if len(params) == 0 {
		return nil
	}
	// The most stored-into first; ties by ID for a deterministic order.
	for i := 1; i < len(params); i++ {
		for j := i; j > 0 && (count[params[j]] > count[params[j-1]] || count[params[j]] == count[params[j-1]] && params[j].ID < params[j-1].ID); j-- {
			params[j], params[j-1] = params[j-1], params[j]
		}
	}
	if len(params) > 4 {
		params = params[:4]
	}
	return &regionParams{params: params, flags: regionParamsEntryCheck(f, params)}
}

// regionParamsEntryCheck splits the entry block so that the function
// starts with
//
//	if g.region != nil { flags = regionParamsLocalN(params...) } else { flags = 0 }
//
// and returns flags. The values that must be in the entry block (the
// arguments, InitMem, SP, SB, the closure pointer) move to the new
// entry; everything else stays in the old one, which follows the
// check, and takes its memory from the merge.
func regionParamsEntryCheck(f *Func, params []*Value) *Value {
	old := f.Entry
	pos := old.Pos.WithNotStmt()
	entry := f.NewBlock(BlockPlain)
	bCheck := f.NewBlock(BlockPlain)
	bJoin := f.NewBlock(BlockPlain)
	entry.Pos, bCheck.Pos, bJoin.Pos = pos, pos, pos
	var keep []*Value
	var initMem *Value
	for _, v := range old.Values {
		switch v.Op {
		case OpInitMem, OpSP, OpSB, OpArg, OpArgIntReg, OpArgFloatReg, OpGetClosurePtr:
			if v.Op == OpInitMem {
				initMem = v
			}
			v.Block = entry
			entry.Values = append(entry.Values, v)
		default:
			keep = append(keep, v)
		}
	}
	old.Values = keep
	if initMem == nil {
		initMem = entry.NewValue0(pos, OpInitMem, types.TypeMem)
	}
	cfgtypes := &f.Config.Types
	uptr := cfgtypes.Uintptr

	// entry: in a region?
	g := entry.NewValue1(pos, OpGetG, cfgtypes.BytePtr, initMem)
	r := entry.NewValue2(pos, OpLoad, cfgtypes.BytePtr, entry.NewValue1I(pos, OpOffPtr, cfgtypes.BytePtrPtr, abi.GRegionOffset, g), initMem)
	zero := entry.NewValue0I(pos, OpConst64, uptr, 0)
	entry.Kind = BlockIf
	entry.SetControl(entry.NewValue1(pos, OpIsNonNil, cfgtypes.Bool, r))
	entry.Likely = BranchUnlikely
	entry.AddEdgeTo(bCheck)
	entry.AddEdgeTo(bJoin)

	// bCheck: the runtime call, arguments in registers.
	fn := f.fe.Syslook("regionParamsLocal" + string(rune('0'+len(params))))
	argTypes := make([]*types.Type, len(params))
	for i := range argTypes {
		argTypes[i] = cfgtypes.BytePtr
	}
	aux := StaticAuxCall(fn, f.ABIDefault.ABIAnalyzeTypes(argTypes, []*types.Type{uptr}))
	call := bCheck.NewValue0A(pos, OpStaticCall, aux.LateExpansionResultType(), aux)
	call.AddArgs(params...)
	call.AddArgs(initMem)
	call.AuxInt = aux.abiInfo.ArgWidth()
	flagsC := bCheck.NewValue1I(pos, OpSelectN, uptr, 0, call)
	memC := bCheck.NewValue1I(pos, OpSelectN, types.TypeMem, 1, call)
	bCheck.AddEdgeTo(bJoin)

	// bJoin: merge, then the old entry.
	flags := bJoin.NewValue0(pos, OpPhi, uptr)
	flags.AddArg2(zero, flagsC)
	mem := bJoin.NewValue0(pos, OpPhi, types.TypeMem)
	mem.AddArg2(initMem, memC)
	bJoin.AddEdgeTo(old)

	// The function's memory now starts at the merge.
	for _, b := range f.Blocks {
		if b == entry || b == bCheck || b == bJoin {
			continue
		}
		for _, v := range b.Values {
			for i, a := range v.Args {
				if a == initMem {
					v.SetArg(i, mem)
				}
			}
		}
		for i, c := range b.ControlValues() {
			if c == initMem {
				b.ReplaceControl(i, mem)
			}
		}
	}
	f.Entry = entry
	for i, b := range f.Blocks {
		if b == entry {
			f.Blocks[0], f.Blocks[i] = entry, f.Blocks[0]
			break
		}
	}
	return flags
}
