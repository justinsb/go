// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file tests const types.

package consttypes

import (
	"io"
	"unsafe"
)

type T struct {
	n    int
	p    *int
	s    []int
	m    map[string]*T
	next *T
	arr  [4]int
	parr [2]*int
	in   Inner
	f    func(*T)
	ch   chan *T
	i    any
}

type Inner struct{ a, b int }

func (i *Inner) Set(a int) { i.a = a }
func (i Inner) Sum() int   { return i.a + i.b }

func (t const *T) N() int      { return t.n }
func (t *T) SetN(n int)        { t.n = n }
func (t const *T) Next() const *T { return t.next }
func (t T) ByValue() int       { return t.n }
func (t const T) ByConstValue() int { return t.n }

type I interface {
	const N() int
	SetN(int)
}

type CI interface {
	const N() int
}

type G[E any] struct{ xs []E }

func (g const *G[E]) At(i int) const E { return g.xs[i] }
func (g *G[E]) Add(x E)               { g.xs = append(g.xs, x) }

func Index[S ~[]E, E comparable](s const S, v E) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func Id[X any](x X) X { return x }

type RO = const []int

func ok(t const *T, cs const []*T, cm const map[string]*T, ci const I, s []int, mt *T) {
	// reading
	_ = t.n + *t.p + t.s[0] + t.arr[1] + *t.parr[0] + t.in.a + len(t.s) + cap(t.s) + len(t.m)
	_ = t.N() + t.Next().N() + t.in.Sum() + t.ByConstValue()
	t.f(nil)
	t.ch <- nil
	<-t.ch
	var _ const *T = t.next
	var _ const []int = t.s
	var _ const []int = t.s[1:]
	var _ const []int = t.arr[:]
	var _ const *int = &t.n
	var _ const *int = &t.s[0]
	var _ const *int = t.p
	var _ const *[4]int = &t.arr
	var _ const any = t.i
	var _ const *T = cs[0]
	var _ const *T = cm["x"]
	v, ok := cm["x"]
	_, _ = v, ok
	for _, x := range cs {
		_ = x.N()
	}
	for k, x := range cm {
		_, _ = k, x.n
	}
	for i, x := range t.s {
		_, _ = i, x+1
	}
	for i := range t.arr {
		_ = i
	}
	// variables of const type are assignable
	t = t.next
	cs = cs[1:]
	cs = nil
	t = nil
	t = mt
	// conversions to const
	var _ const []int = s
	var _ RO = s
	var _ const *T = mt
	var _ const I = mt
	var _ const CI = mt
	var _ const CI = t
	var _ const CI = ci
	var _ const any = t
	var _ const any = 1
	var _ const any = nil
	var _ CI = mt
	_ = (const []int)(s)
	_ = (const *T)(mt)
	_ = const []int(s)
	// value types escape const
	var n int = t.n
	var a [4]int = t.arr
	var in Inner = t.in
	n++
	a[0] = 1
	in.Set(1)
	_ = string(const []byte(nil))
	_ = unsafe.Pointer(t)
	// copying
	dst := make([]int, 10)
	copy(dst, t.s)
	dst = append(dst, t.s...)
	dst = append([]int(nil), t.s...)
	cdst := make([]const *T, 3)
	copy(cdst, cs)
	cdst = append(cdst, cs...)
	cdst[0] = t
	// comparisons
	_ = t == mt
	_ = t != nil
	_ = cs == nil
	_ = ci == nil
	// type assertions
	if x, ok := ci.(*T); ok {
		var _ const *T = x
	}
	switch x := ci.(type) {
	case *T:
		_ = x.N()
	case CI:
		_ = x.N()
	default:
		var _ const I = x
	}
	// generics
	_ = Index(t.s, 1)
	_ = Index(s, 1)
	_ = Id(t).N()
	g := &G[*T]{}
	g.Add(mt)
	var _ const *T = g.At(0)
	var cg const *G[*T] = g
	_ = cg.At(0).N()
	gi := &G[int]{}
	var x int = gi.At(0)
	_ = x
	// method values and expressions
	fn := t.N
	_ = fn()
	_ = (*T).N(mt)
	_ = (const *T).N(t)
	// closures
	func() { _ = t.n }()
	// struct with const fields
	type S struct{ cfg const *T }
	sv := S{cfg: t}
	sv.cfg = mt
	_ = sv.cfg.N()
	// pointers to const
	var pp *const *T = &t
	*pp = mt
	_ = unsafe.Sizeof(t) + unsafe.Sizeof(cs)
}

// --- invalid uses ---

func bad(t const *T, cs const []*T, cm const map[string]*T, ci const I, s []int, mt *T) {
	t.n = 1 // ERROR "read-only"
	t.n++ // ERROR "read-only"
	t.n += 2 // ERROR "read-only"
	*t.p = 1 // ERROR "read-only"
	t.s[0] = 1 // ERROR "read-only"
	t.s = nil // ERROR "read-only"
	t.arr[0] = 1 // ERROR "read-only"
	t.arr = [4]int{} // ERROR "read-only"
	*t.parr[0] = 1 // ERROR "read-only"
	t.in.a = 1 // ERROR "read-only"
	t.in.Set(1) // ERROR "non-const method"
	t.in = Inner{} // ERROR "read-only"
	t.m["x"] = nil // ERROR "read-only"
	t.next.n = 1 // ERROR "read-only"
	t.next.next.s[0] = 1 // ERROR "read-only"
	t.SetN(1) // ERROR "non-const method"
	_ = t.ByValue() // ERROR "non-const method"
	cs[0] = nil // ERROR "read-only"
	cs[0].n = 1 // ERROR "read-only"
	cs[0].SetN(1) // ERROR "non-const method"
	cm["x"] = nil // ERROR "read-only"
	cm["x"].n = 1 // ERROR "read-only"
	ci.SetN(1) // ERROR "non-const method"
	var _ *T = t // ERROR "discards const"
	var _ []*T = cs // ERROR "discards const"
	var _ map[string]*T = cm // ERROR "discards const"
	var _ I = ci // ERROR "discards const"
	var _ any = t // ERROR "discards const"
	var _ *int = &t.n // ERROR "discards const"
	var _ *int = &t.s[0] // ERROR "discards const"
	var _ *int = t.p // ERROR "read-only"
	var _ []int = t.s // ERROR "read-only"
	var _ []int = t.s[1:] // ERROR "discards const"
	var _ []int = t.arr[:] // ERROR "discards const"
	var _ *[4]int = &t.arr // ERROR "discards const"
	var _ [2]*int = t.parr // ERROR "read-only"
	var _ any = t.i // ERROR "read-only"
	var _ const I = t // ERROR "is not a const method"
	_ = []int(t.s) // ERROR "read-only"
	_ = (*T)(t) // ERROR "discards const"
	for _, x := range cs {
		x.n = 1 // ERROR "read-only"
	}
	for _, x := range cm {
		x.SetN(1) // ERROR "non-const method"
	}
	for i := range t.s {
		t.s[i] = 0 // ERROR "read-only"
	}
	for _, t.s[0] = range s { // ERROR "read-only"
	}
	t.s[0], t.n = 1, 2 /* ERROR "read-only" */ // ERROR "read-only"
	p := &t.n
	*p = 1 // ERROR "read-only"
	q := t.next
	q.n = 1 // ERROR "read-only"
	if x, ok := ci.(*T); ok {
		x.n = 1 // ERROR "read-only"
	}
	switch x := ci.(type) {
	case *T:
		x.n = 1 // ERROR "read-only"
	case I:
		x.SetN(1) // ERROR "non-const method"
	}
	_ = append(t.s, 1) // ERROR "read-only"
	copy(t.s, s) // ERROR "read-only"
	clear(t.s) // ERROR "read-only"
	clear(cm) // ERROR "cannot clear const"
	delete(cm, "x") // ERROR "cannot delete from const"
	dst := make([]*T, 3)
	copy(dst, cs) // ERROR "discards const"
	dst = append(dst, cs...) // ERROR "discards const"
	dst = append(dst, cs[0]) // ERROR "read-only"
	fn := t.SetN // ERROR "non-const method"
	_ = fn
	_ = (const *T).SetN // ERROR "non-const method"
	_ = Id(t).SetN // ERROR "non-const method"
	g := &G[*T]{}
	var cg const *G[*T] = g
	cg.Add(nil) // ERROR "non-const method"
	cg.At(0).n = 1 // ERROR "read-only"
	g.At(0).n = 1 // ERROR "read-only"
	func() { t.n = 1 }() // ERROR "read-only"
	type S struct{ cfg const *T }
	sv := S{cfg: t}
	sv.cfg.n = 1 // ERROR "read-only"
	var pp *const *T = &t
	(*pp).n = 1 // ERROR "read-only"
	var _ **T = pp // ERROR "cannot use"
	var _ *const *T = &mt // ERROR "cannot use"
	var _ []const *T = dst // ERROR "cannot use"
	var _ func(*T) = func(const *T) {}
}

type Bad1 const []int // ERROR "const type as RHS"

type J interface{ const N() int }

type NotConst struct{}

func (*NotConst) N() int { return 0 }

var _ J = &NotConst{} // ERROR "is not a const method"
var _ J = (*T)(nil)

type X struct{ p *int }

func (x const X) M() {
	*x.p = 1 // ERROR "read-only"
}
func (x X) V() {}

func xx(x const X) {
	x.V() // ERROR "non-const method"
	x.M()
	x = X{}
	x.p = nil // ERROR "read-only"
}


// --- const-compatible signatures and const variadic parameters ---

type W struct{ n int }

func (w *W) Write(p const []byte) (int, error) { w.n += len(p); return len(p), nil }

var _ io.Writer = &W{}

func logf(format string, args const ...any) {
	_ = len(args)
	for _, a := range args {
		var _ const any = a
	}
	logf(format, args...)
	var _ const []any = args
}

func wrap(format string, args ...any) { logf(format, args...) }

func use(t const *T, mt *T) {
	logf("x", t, mt, 1, "s", nil)
	logf("x")
	var f func(string, ...any) = logf
	f("a", 1)
	var g func(const *T) int = (*T).N
	var h func(*T) int = (const *T).N
	var r func() const *T = func() *T { return nil }
	_, _, _ = g, h, r
}

func sum(xs const ...int) (s int) {
	for _, x := range xs {
		s += x
	}
	return
}

var _ = sum(1, 2, 3)

type RI interface{ M(p const *T) const []int }
type impl struct{}

func (impl) M(p const *T) []int { return nil }

var _ RI = impl{}

func qbad(t const *T, args const ...any) {
	args[0] = 1 // ERROR "read-only"
	wrap("x", args...) // ERROR "discards const"
	wrap("x", t) // ERROR "discards const"
	var _ func(const *T) = func(*T) {} // ERROR "cannot use"
	var _ func() *T = func() const *T { return nil } // ERROR "cannot use"
	var _ []any = args // ERROR "discards const"
}

type RI2 interface{ M(p const *T) []int }
type impl2 struct{}

func (impl2) M(p *T) []int { return nil }

var _ RI2 = impl2{} // ERROR "wrong type for method"

type impl3 struct{}

func (impl3) M(p const *T) const []int { return nil }

var _ RI2 = impl3{} // ERROR "wrong type for method"

func dots(a const ...int, b int) {} // ERROR "can only use ... with final parameter"

