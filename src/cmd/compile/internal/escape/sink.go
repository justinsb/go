// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package escape

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
)

// A sinkKind records *why* a value was forced to the heap: the kind of
// location its address ultimately flowed into. The ordinary escape
// analysis only distinguishes "escapes" from "does not escape"; the sink
// kind is a finer classification that is useful for studying
// goroutine-local memory (memory regions, goroutine-tied arenas) and
// ownership inference, where what matters is not whether an object
// outlives its *function* but whether it can outlive (or be shared
// with) its *goroutine*.
//
// Kinds are ordered by "severity": when a value flows to several sinks
// the largest kind wins. Kinds at or above sinkGlobal are the ones that
// definitely let another goroutine observe the memory; everything below
// keeps the memory inside the allocating goroutine's dynamic extent (as
// far as the local, intraprocedural view can tell).
//
// Enabled with -d=escsink=1, which prints one line per heap-allocated
// expression or variable. The classification is also threaded through
// the parameter leak tags in export data so that an argument escaping
// *inside* a callee is attributed to the callee's sink rather than to a
// generic "call".
type sinkKind uint8

const (
	sinkNone sinkKind = iota

	// Heap allocation forced by size or alignment, not by data flow
	// (see HeapAllocReason). The value may be perfectly goroutine-local.
	sinkSize

	// Outlives a loop iteration within the same function.
	sinkLoop

	// Outlives a closure's frame, or is captured by a closure that
	// itself is heap-allocated.
	sinkClosure

	// Argument of a defer statement inside a loop (defer record is
	// heap allocated).
	sinkDefer

	// Returned to the caller (flows to a result parameter).
	sinkResult

	// Stored through a pointer that is known to point to a local
	// variable of the current function (p := &local; p.f = x), which
	// is what a method call on an addressable local looks like after
	// inlining. The provenance is fully known within this function.
	sinkStoreLocal

	// Stored through a pointer whose base is a parameter of the
	// current function: *p = x, p.f = x, p[i] = x. The caller knows
	// the provenance of p; the callee does not.
	sinkStoreParam

	// Stored through a pointer of unknown provenance (a local pointer
	// variable, a field chain, a slice element, a map value, or an
	// append/copy into a backing array).
	sinkStore

	// Used as a map key (keys must escape; see "Hashing Pointers" in
	// internal/runtime/maps/map.go).
	sinkMapKey

	// Argument to a statically-known callee that leaks it to the heap
	// without a recorded sink kind (e.g. a callee compiled without this
	// information, or an assembly/linkname function).
	sinkCall

	// Argument or receiver of a dynamic call (interface method call or
	// call through a func value) whose callee is unknown.
	sinkDynCall

	// Receiver of a method value whose results are unknown.
	sinkMethValue

	// Argument of panic.
	sinkPanic

	// Converted to unsafe.Pointer / uintptr, reflect header stores,
	// //go:uintptrescapes.
	sinkUnsafe

	// Heap flow with no further information.
	sinkOther

	// --- Everything from here on can be observed by another goroutine. ---

	// Assigned to a package-level variable.
	sinkGlobal

	// Sent on a channel.
	sinkSend

	// Captured by, or passed to, a go statement.
	sinkGo

	numSinkKinds
)

var sinkNames = [numSinkKinds]string{
	sinkNone:       "none",
	sinkSize:       "size",
	sinkLoop:       "loop",
	sinkClosure:    "closure",
	sinkDefer:      "defer",
	sinkResult:     "result",
	sinkStoreLocal: "store-local",
	sinkStoreParam: "store-param",
	sinkStore:      "store",
	sinkMapKey:     "map-key",
	sinkCall:       "call",
	sinkDynCall:    "dyncall",
	sinkMethValue:  "methvalue",
	sinkPanic:      "panic",
	sinkUnsafe:     "unsafe",
	sinkOther:      "other",
	sinkGlobal:     "global",
	sinkSend:       "send",
	sinkGo:         "go",
}

func (s sinkKind) String() string {
	if s < numSinkKinds {
		return sinkNames[s]
	}
	return "?"
}

// crossGoroutine reports whether the sink lets a different goroutine
// observe the memory.
func (s sinkKind) crossGoroutine() bool { return s >= sinkGlobal }

// maxSink returns the more severe of two sink kinds.
func maxSink(a, b sinkKind) sinkKind {
	if a > b {
		return a
	}
	return b
}

// sinkForWhy maps the free-form "why" strings used with assignHeap to
// a sink kind.
func sinkForWhy(why string) sinkKind {
	switch why {
	case "send":
		return sinkSend
	case "key of map put", "map literal key":
		return sinkMapKey
	case "map literal value":
		return sinkStore
	case "conversion to unsafe.Pointer":
		return sinkUnsafe
	}
	return sinkOther
}

// withSink returns a copy of k tagged with sink kind s.
func (k hole) withSink(s sinkKind) hole {
	k.sink = s
	return k
}

// baseKind describes what is known about the provenance of a pointer
// variable: nothing, that it is (a copy of) a parameter of the current
// function, that it is the address of a local variable, or that it is
// (a copy of) a package-level variable, so that whatever it points to
// is reachable from a global.
type baseKind uint8

const (
	baseUnknown baseKind = iota
	baseParam
	baseLocal
	baseGlobal
)

// baseOf reports what is known about the provenance of the pointer
// that the addressable expression n is rooted at (through field
// selections, parenthesization, and no-op conversions). It is used to
// distinguish "*p = x" where p is a parameter, or points to a local,
// from stores through pointers of unknown provenance.
func (e *escape) baseOf(n ir.Node) baseKind {
	for {
		switch n.Op() {
		case ir.ONAME:
			n := n.(*ir.Name)
			switch n.Class {
			case ir.PPARAM:
				return baseParam
			case ir.PEXTERN:
				return baseGlobal
			case ir.PAUTO:
				if n.Canonical().Opt != nil {
					loc := e.oldLoc(n)
					if loc.baseAssigns == 1 {
						return loc.base
					}
				}
			}
			return baseUnknown
		case ir.ODOT:
			n = n.(*ir.SelectorExpr).X
		case ir.OPAREN:
			n = n.(*ir.ParenExpr).X
		case ir.OCONVNOP:
			n = n.(*ir.ConvExpr).X
		default:
			return baseUnknown
		}
	}
}

// noteBase records, for an assignment dst = src to a local pointer,
// map or slice variable, what is known about src's provenance. Only variables
// assigned exactly once (as seen so far, in statement order) keep a
// known provenance; inlined formal parameters are the common case.
func (e *escape) noteBase(dst *ir.Name, src ir.Node) {
	if dst.Class != ir.PAUTO || dst.Canonical().Opt == nil {
		return
	}
	if t := dst.Type(); !t.IsPtr() && !t.IsMap() && !t.IsSlice() {
		return
	}
	loc := e.oldLoc(dst)
	if loc.baseAssigns < 255 {
		loc.baseAssigns++
	}
	if loc.baseAssigns != 1 {
		loc.base = baseUnknown
		return
	}
	if src == nil {
		return
	}
	switch src.Op() {
	case ir.OADDR:
		if x, ok := src.(*ir.AddrExpr).X.(*ir.Name); ok && x.Class == ir.PAUTO {
			loc.base = baseLocal
		}
	case ir.OPTRLIT, ir.ONEW:
		// p := &T{...} or p := new(T): p points to a fresh object
		// that is otherwise unaliased.
		loc.base = baseLocal
	default:
		loc.base = e.baseOf(src)
	}
}

// classifySink determines the sink kind for location l, whose address
// was just found to flow to root during walkOne. The path from l to
// root is available through the dst/dstEdgeIdx fields set by walkOne.
func (b *batch) classifySink(root, l *location) sinkKind {
	if root == &b.heapLoc {
		// Find the edge into the heap and use its sink kind.
		visited := make(map[*location]bool)
		kind := sinkNone
		for src := l; src != root; {
			if visited[src] {
				break
			}
			visited[src] = true
			dst := src.dst
			if dst == nil || src.dstEdgeIdx >= len(dst.edges) {
				break
			}
			edge := &dst.edges[src.dstEdgeIdx]
			if dst == root {
				kind = edge.sink
				break
			}
			src = dst
		}
		if kind == sinkNone {
			kind = sinkOther
		}
		return kind
	}
	if root.hasAttr(attrEscapes) {
		// l's address is stored into something that itself escaped;
		// inherit its reason.
		if root.sink != sinkNone {
			return root.sink
		}
		return sinkOther
	}
	if root.paramOut {
		return sinkResult
	}
	if root.curfn == l.curfn && root.loopDepth < l.loopDepth {
		return sinkLoop
	}
	if ir.ContainsClosure(root.curfn, l.curfn) {
		return sinkClosure
	}
	return sinkOther
}

// sinkAllocSize returns the compile-time known size of the heap object
// that expression or variable n allocates, or -1 if it is not known
// statically (e.g. make with a non-constant length).
func sinkAllocSize(n ir.Node) int64 {
	if n == nil || n.Type() == nil {
		return -1
	}
	switch n.Op() {
	case ir.ONAME:
		return n.Type().Size()
	case ir.ONEW, ir.OPTRLIT:
		return n.Type().Elem().Size()
	case ir.OCLOSURE:
		return typecheck.ClosureType(n.(*ir.ClosureExpr)).Size()
	case ir.OMETHVALUE:
		return typecheck.MethodValueType(n.(*ir.SelectorExpr)).Size()
	case ir.OCONVIFACE:
		return n.(*ir.ConvExpr).X.Type().Size()
	case ir.OSLICELIT:
		n := n.(*ir.CompLitExpr)
		return n.Len * n.Type().Elem().Size()
	case ir.OMAKESLICE:
		n := n.(*ir.MakeExpr)
		r := n.Cap
		if r == nil {
			r = n.Len
		}
		if ir.IsSmallIntConst(r) {
			return ir.Int64Val(r) * n.Type().Elem().Size()
		}
		return -1
	case ir.OMAKEMAP, ir.OMAKECHAN:
		return -1
	}
	return -1
}

// reportSink prints the -d=escsink diagnostic for an escaping location.
func (b *batch) reportSink(loc *location) {
	n := loc.n
	if n == nil {
		return
	}
	if clo, ok := n.(*ir.ClosureExpr); ok && clo.Func.Wrapper() {
		return
	}
	kind := maxSink(loc.sink, loc.sinkMax)
	if kind == sinkNone {
		kind = sinkOther
	}
	b.reportSinkKind(loc, kind.String())
}

// walkSinks computes, for every location whose address flows to the
// heap, the most severe sink kind over *all* such paths (walkOne only
// records the first path found). It also upgrades the heap-leak sink
// kind of parameters so that export tags carry the most severe kind.
//
// It runs one walk from the heap per sink kind, following only the
// heap edges of that kind, and unlike walkOne it does not stop at
// locations that already escaped. It is only run with -d=escapesink,
// so its cost (a handful of extra graph walks per batch) does not
// matter.
func (b *batch) walkSinks() {
	root := &b.heapLoc
	present := [numSinkKinds]bool{}
	for _, e := range root.edges {
		present[e.sink] = true
	}
	for kind := numSinkKinds - 1; kind > sinkNone; kind-- {
		if !present[kind] {
			continue
		}
		b.walkgen++
		walkgen := b.walkgen
		root.walkgen = walkgen
		root.derefs = 0
		root.dst = nil

		todo := newQueue(1)
		todo.pushFront(root)
		for todo.len() > 0 {
			l := todo.popFront()
			l.queuedWalkOne = 0

			derefs := l.derefs
			if derefs < 0 {
				// l's address flows to the heap through a sink of this kind.
				derefs = 0
				if l != root {
					l.sinkMax = maxSink(l.sinkMax, kind)
				}
			}
			if l.param && l != root && l.paramEsc.Heap() >= 0 {
				l.paramEsc.setHeapSink(kind)
			}

			for i, edge := range l.edges {
				if l == root && edge.sink != kind {
					continue
				}
				d := derefs + edge.derefs
				if edge.src.walkgen != walkgen || edge.src.derefs > d {
					edge.src.walkgen = walkgen
					edge.src.derefs = d
					edge.src.dst = l
					edge.src.dstEdgeIdx = i
					if edge.src.queuedWalkOne != walkgen {
						edge.src.queuedWalkOne = walkgen
						todo.pushBack(edge.src)
					}
				}
			}
		}
	}
}

// reportSinkKind prints one -d=escsink line for loc with the given kind
// string. Format (tab separated, after the usual position prefix):
//
//	escsink KIND OP SIZE FUNC INNERFILE:LINE NODE
func (b *batch) reportSinkKind(loc *location, kind string) {
	n := loc.n
	fnname := "?"
	if loc.curfn != nil {
		fnname = ir.FuncName(loc.curfn)
	}
	// Positions are printed twice: WarnfAt uses the outermost position
	// (the call site, for inlined code), while the explicit inner
	// position is the one that allocation profiles report for inlined
	// frames, which is what tools joining the two need.
	inner := base.Ctxt.InnermostPos(n.Pos())
	base.WarnfAt(n.Pos(), "escsink\t%s\t%v\t%d\t%v\t%s:%d\t%v", kind, n.Op(), sinkAllocSize(n), fnname, inner.Filename(), inner.Line(), n)
}
