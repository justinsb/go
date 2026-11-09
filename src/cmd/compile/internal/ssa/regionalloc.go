// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/bitvec"
	"cmd/compile/internal/typebits"
	"cmd/compile/internal/types"
	"internal/abi"
)

// regionalloc expands calls to the runtime allocator for small objects
// into an inline bump allocation in the calling goroutine's memory
// region (runtime/region.go), with the call as the fallback:
//
//	if g.region != nil {                      // b
//	    r := g.region                         // bCheck
//	    cursor := r.cursor
//	    if cursor+size <= r.fastLimit {
//	        r.cursor = cursor+size             // bFast
//	        x := cursor
//	        set start bit (and pointer bits) of x in the block header
//	        goto end
//	    }
//	}
//	x = newobject(typ)                         // bSlow
//	end:                                       // bEnd
//
// fastLimit is zero whenever the runtime wants the slow path: no run
// of zeroed lines is available, allocations are being placed outside
// the region, or the GC is marking. The memory below fastLimit is
// already zero, so the fast path only has to bump the cursor and mark
// the object in the block header, which it finds by aligning the
// address down to the block size.
//
// The pass runs after the write barrier pass, so every earlier
// optimization sees an ordinary allocation call: zeroing stores into
// the new object are removed, nil checks on it are dropped, and stores
// into it need neither a write barrier nor a region store check.
func regionalloc(f *Func) {
	if base.Debug.RegionAlloc == 0 || f.Config.PtrSize != 8 || base.Flag.CompilingRuntime || base.Flag.Cfg.Instrumenting {
		return
	}
	// Collect the eligible calls first: expanding one splits its block.
	var calls []*Value
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpStaticCall && isMalloc(v.Aux) {
				calls = append(calls, v)
			}
		}
	}
	if len(calls) == 0 {
		return
	}
	// The results of each call: SelectN [0] (the pointer, maybe unused)
	// and SelectN [1] (the memory).
	sel := make(map[*Value][2]*Value)
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpSelectN || v.Args[0].Op != OpStaticCall || !isMalloc(v.Args[0].Aux) {
				continue
			}
			s := sel[v.Args[0]]
			s[v.AuxInt] = v
			sel[v.Args[0]] = s
		}
	}
	// Uses of the call results are redirected to the merged values at
	// the end, in one pass over the function.
	replace := make(map[*Value]*Value)
	for _, call := range calls {
		s := sel[call]
		if s[1] == nil {
			continue
		}
		if s[0] == nil || s[0].Type.IsUnsafePtr() {
			// The pointer is unused, or the type is unknown.
			continue
		}
		regionExpandAlloc(f, call, s[0], s[1], replace)
	}
	if len(replace) == 0 {
		return
	}
	// The merged pointer stands for the same variable as the call's
	// result, for liveness and debug information.
	for _, name := range f.Names {
		for _, v := range f.NamedValues[*name] {
			if r, ok := replace[v]; ok && r.Op == OpPhi && !r.Type.IsMemory() {
				f.NamedValues[*name] = append(f.NamedValues[*name], r)
			}
		}
	}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			for i, a := range v.Args {
				if r, ok := replace[a]; ok && r != v { // a merge keeps its call result arg
					v.SetArg(i, r)
				}
			}
		}
		for i, c := range b.ControlValues() {
			if r, ok := replace[c]; ok {
				b.ReplaceControl(i, r)
			}
		}
	}
}

// regionExpandAlloc expands one allocation call; ptr and mem are its
// results. It records in replace the merged values that take their
// place.
func regionExpandAlloc(f *Func, call, ptr, mem *Value, replace map[*Value]*Value) {
	typ := ptr.Type.Elem()
	size := types.RoundUp(typ.Size(), 8)
	if size == 0 || size > abi.RegionMaxObject {
		return
	}
	var mask uint64
	nptrwords := int64(0)
	if typ.HasPointers() {
		if types.PtrDataSize(typ) > 64*8 {
			return // the pointer mask must fit one bitmap word
		}
		bv := bitvec.New(int32(size / 8))
		typebits.Set(typ, 0, bv)
		for i, w := range bv.B {
			mask |= uint64(w) << (32 * uint(i))
		}
		nptrwords = types.PtrDataSize(typ) / 8
	}

	b := call.Block
	pos := call.Pos
	npos := pos.WithNotStmt()
	mem0 := call.MemoryArg()
	cfgtypes := &f.Config.Types
	uptr := cfgtypes.Uintptr
	u64 := cfgtypes.UInt64
	u64P := types.NewPtr(u64)
	c := func(v int64) *Value { return f.ConstInt64(uptr, v) }

	// Split b: the call and its results go to bSlow, everything that
	// depends on the call to bEnd. Phis stay. (Values are not in
	// dependency order, hence the fixed point.)
	after := f.newSparseSet(f.NumValues())
	defer f.retSparseSet(after)
	after.add(call.ID)
	for changed := true; changed; {
		changed = false
		for _, v := range b.Values {
			if v.Op == OpPhi || after.contains(v.ID) {
				continue
			}
			for _, a := range v.Args {
				if after.contains(a.ID) {
					after.add(v.ID)
					changed = true
					break
				}
			}
		}
	}

	bCheck := f.NewBlock(BlockPlain)
	bFast := f.NewBlock(BlockPlain)
	bSlow := f.NewBlock(BlockPlain)
	bEnd := f.NewBlock(b.Kind)
	bCheck.Pos, bFast.Pos, bSlow.Pos = npos, npos, pos
	bEnd.Pos = b.Pos
	b.Pos = npos
	bEnd.CopyControls(b)
	bEnd.Likely = b.Likely
	for _, e := range b.Succs {
		bEnd.Succs = append(bEnd.Succs, e)
		e.b.Preds[e.i].b = bEnd
	}
	b.ResetControls()
	b.Succs = b.Succs[:0]

	var keep []*Value
	for _, v := range b.Values {
		switch {
		case v == call || v == ptr || v == mem:
			v.Block = bSlow
			bSlow.Values = append(bSlow.Values, v)
		case after.contains(v.ID):
			v.Block = bEnd
			bEnd.Values = append(bEnd.Values, v)
		default:
			keep = append(keep, v)
		}
	}
	b.Values = keep

	// b: is the goroutine in a region?
	g := b.NewValue1(npos, OpGetG, cfgtypes.BytePtr, mem0)
	r := b.NewValue2(npos, OpLoad, cfgtypes.BytePtr, b.NewValue1I(npos, OpOffPtr, cfgtypes.BytePtrPtr, abi.GRegionOffset, g), mem0)
	b.Kind = BlockIf
	b.SetControl(b.NewValue1(npos, OpIsNonNil, cfgtypes.Bool, r))
	b.Likely = BranchLikely
	b.AddEdgeTo(bCheck)
	b.AddEdgeTo(bSlow)

	// bCheck: does the object fit below fastLimit?
	cursorAddr := bCheck.NewValue1I(npos, OpOffPtr, cfgtypes.UintptrPtr, abi.RegionCursorOffset, r)
	cursor := bCheck.NewValue2(npos, OpLoad, uptr, cursorAddr, mem0)
	limit := bCheck.NewValue2(npos, OpLoad, uptr, bCheck.NewValue1I(npos, OpOffPtr, cfgtypes.UintptrPtr, abi.RegionFastLimitOffset, r), mem0)
	end := bCheck.NewValue2(npos, OpAdd64, uptr, cursor, c(size))
	bCheck.Kind = BlockIf
	bCheck.SetControl(bCheck.NewValue2(npos, OpLeq64U, cfgtypes.Bool, end, limit))
	bCheck.Likely = BranchLikely
	bCheck.AddEdgeTo(bFast)
	bCheck.AddEdgeTo(bSlow)

	// bFast: bump the cursor and mark the object in the block header.
	// The header holds one bit per data word, starting at hdrBytes
	// into the block; the bit of the object is bit (cursor/8)%64 of
	// bitmap word (cursor%blockBytes)/512 - hdrBytes/512, which is
	// addressed as base = hb + ((cursor/512)%128)*8 with the bitmap's
	// offset minus hdrBytes/64 as displacement.
	m := bFast.NewValue3A(npos, OpStore, types.TypeMem, uptr, cursorAddr, end, mem0)
	x := bFast.NewValue2(npos, OpConvert, ptr.Type, cursor, m)
	hb := bFast.NewValue2(npos, OpAnd64, uptr, cursor, c(^int64(abi.RegionBlockBytes-1)))
	idx := bFast.NewValue2(npos, OpAnd64, uptr, bFast.NewValue2(npos, OpRsh64Ux64, uptr, cursor, c(9)), c(abi.RegionBlockBytes/512-1))
	sh := bFast.NewValue2(npos, OpAnd64, uptr, bFast.NewValue2(npos, OpRsh64Ux64, uptr, cursor, c(3)), c(63))
	base := bFast.NewValue2(npos, OpConvert, u64P, bFast.NewValue2(npos, OpAdd64, uptr, hb, bFast.NewValue2(npos, OpLsh64x64, uptr, idx, c(3))), m)
	const disp = -abi.RegionHdrBytes / 64
	bounded := func(v *Value) *Value { v.AuxInt = 1; return v } // shift amount known < 64
	orStore := func(off int64, bits, m *Value) *Value {
		addr := bFast.NewValue1I(npos, OpOffPtr, u64P, off+disp, base)
		old := bFast.NewValue2(npos, OpLoad, u64, addr, m)
		return bFast.NewValue3A(npos, OpStore, types.TypeMem, u64, addr, bFast.NewValue2(npos, OpOr64, u64, old, bits), m)
	}
	m = orStore(abi.RegionHdrStartOffset, bounded(bFast.NewValue2(npos, OpLsh64x64, u64, f.ConstInt64(u64, 1), sh)), m)
	if mask != 0 {
		m = orStore(abi.RegionHdrPtrOffset, bounded(bFast.NewValue2(npos, OpLsh64x64, u64, f.ConstInt64(u64, int64(mask)), sh)), m)
		if nptrwords > 1 {
			// The mask may cross into the next bitmap word, which
			// gets mask >> (64-sh), computed as (mask >> (63-sh)) >> 1
			// so that the shift amounts stay below 64 (and the
			// result is 0 when sh is 0); 63-sh is sh^63.
			hi := bounded(bFast.NewValue2(npos, OpRsh64Ux64, u64, bounded(bFast.NewValue2(npos, OpRsh64Ux64, u64, f.ConstInt64(u64, int64(mask)), bFast.NewValue2(npos, OpXor64, uptr, sh, c(63)))), f.ConstInt64(u64, 1)))
			m = orStore(abi.RegionHdrPtrOffset+8, hi, m)
		}
	}
	// The allocation is complete. From the limit check on, the
	// sequence must not be interrupted (liveness.markUnsafePoints): a
	// GC cycle starting in between would not see the object as
	// allocated black.
	m = bFast.NewValue1(npos, OpRegionAllocEnd, types.TypeMem, m)
	bFast.AddEdgeTo(bEnd)
	bSlow.AddEdgeTo(bEnd)

	// bEnd: merge. The phis go first, before the values moved here.
	pphi := f.newValueNoBlock(OpPhi, ptr.Type, npos)
	pphi.AddArg2(x, ptr)
	mphi := f.newValueNoBlock(OpPhi, types.TypeMem, npos)
	mphi.AddArg2(m, mem)
	pphi.Block, mphi.Block = bEnd, bEnd
	bEnd.Values = append([]*Value{pphi, mphi}, bEnd.Values...)
	replace[ptr] = pphi
	replace[mem] = mphi
}
