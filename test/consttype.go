// run

// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Test that programs using const types compile and run correctly.
// Const qualifiers are checked statically and have no effect at run time.

package main

import (
	"fmt"
	"slices"
	"strings"
)

type Point struct{ X, Y int }

type Shape struct {
	name   string
	points []Point
	tags   map[string]string
	parent *Shape
}

// Points returns a read-only view of the shape's points.
// No defensive copy is needed: callers cannot modify the result.
func (s const *Shape) Points() const []Point { return s.points }

// Tags returns a read-only view of the shape's tags.
func (s const *Shape) Tags() const map[string]string { return s.tags }

func (s const *Shape) Name() string         { return s.name }
func (s const *Shape) Parent() const *Shape { return s.parent }
func (s *Shape) SetName(name string)        { s.name = name }
func (s *Shape) Add(p Point)                { s.points = append(s.points, p) }

type Named interface {
	const Name() string
	SetName(string)
}

func describe(n const Named) string { return "<" + n.Name() + ">" }

func sumX(ps const []Point) (sum int) {
	for _, p := range ps {
		sum += p.X
	}
	return
}

func count(args const ...any) int { return len(args) }

// A generic function with a const parameter accepts both
// const and non-const arguments.
func index[S ~[]E, E comparable](s const S, v E) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

type Stack[E any] struct{ elems []E }

func (s *Stack[E]) Push(e E)             { s.elems = append(s.elems, e) }
func (s const *Stack[E]) Top() const E   { return s.elems[len(s.elems)-1] }
func (s const *Stack[E]) Len() int       { return len(s.elems) }
func (s const *Stack[E]) All() const []E { return s.elems }

type writer struct{ sb strings.Builder }

// Write has a const parameter but still implements io.Writer-like
// interfaces declared with a non-const parameter.
func (w *writer) Write(p const []byte) (int, error) { return w.sb.Write([]byte(string(p))) }

type byteWriter interface {
	Write(p []byte) (int, error)
}

func check(got, want any) {
	if got != want {
		panic(fmt.Sprintf("got %v, want %v", got, want))
	}
}

func main() {
	root := &Shape{name: "root"}
	s := &Shape{name: "tri", tags: map[string]string{"k": "v"}, parent: root}
	s.Add(Point{1, 2})
	s.Add(Point{3, 4})
	s.Add(Point{5, 6})

	// Read-only views share the underlying data.
	ps := s.Points()
	check(len(ps), 3)
	check(sumX(ps), 9)
	check(sumX(ps[1:]), 8)
	check(ps[2].Y, 6)
	s.points[0].X = 10
	check(ps[0].X, 10) // the view observes the change

	// Elements of value types can be copied out and then modified.
	p := ps[0]
	p.X = 100
	check(ps[0].X, 10)

	// Copying from a const slice.
	cp := make([]Point, len(ps))
	check(copy(cp, ps), 3)
	cp = append(cp, ps...)
	cp[0].X = -1
	check(len(cp), 6)
	check(ps[0].X, 10)

	// Maps.
	tags := s.Tags()
	check(tags["k"], "v")
	v, ok := tags["missing"]
	check(v, "")
	check(ok, false)
	n := 0
	for k, v := range tags {
		n += len(k) + len(v)
	}
	check(n, 2)

	// Const pointers and methods.
	var cs const *Shape = s
	check(cs.Name(), "tri")
	check(cs.Parent().Name(), "root")
	check(cs.parent.name, "root")
	check(cs == s, true)
	pn := &cs.name
	check(*pn, "tri")

	// Const interfaces.
	var named Named = s
	check(describe(named), "<tri>")
	named.SetName("tri2")
	var cn const Named = named
	check(cn.Name(), "tri2")
	if c, ok := cn.(*Shape); ok {
		check(c.Name(), "tri2")
	} else {
		panic("type assertion failed")
	}
	switch x := cn.(type) {
	case *Shape:
		check(len(x.Points()), 3)
	default:
		panic("type switch failed")
	}
	var ca const any = cs
	_, ok = ca.(const *Shape)
	check(ok, true)

	// Const variadic parameters.
	check(count(), 0)
	check(count(cs, ps, 1, "x"), 4)
	args := []any{1, 2}
	check(count(args...), 2)

	// Printing const values.
	check(fmt.Sprint(ps[1:]), "[{3 4} {5 6}]")
	check(fmt.Sprintf("%v %s %d", ps[0], cs.Name(), len(tags)), "{10 2} tri2 1")

	// Generics.
	ints := []int{1, 2, 3}
	var cints const []int = ints
	check(index(ints, 2), 1)
	check(index(cints, 3), 2)
	check(slices.Index(cints, 3), 2)
	check(slices.Contains(cints, 4), false)
	check(slices.Equal(cints, ints), true)

	st := &Stack[*Shape]{}
	st.Push(s)
	var cst const *Stack[*Shape] = st
	check(cst.Len(), 1)
	check(cst.Top().Name(), "tri2")
	check(cst.All()[0].Name(), "tri2")
	si := &Stack[int]{}
	si.Push(7)
	var top int = si.Top() // const int is int
	check(top, 7)

	// Containers of const values.
	shapes := []const *Shape{cs, root}
	shapes[1] = cs
	check(shapes[1].Name(), "tri2")
	byName := map[string]const *Shape{"a": cs}
	check(byName["a"].Name(), "tri2")
	ch := make(chan const *Shape, 1)
	ch <- cs
	check((<-ch).Name(), "tri2")

	// Functions with const parameters are usable as functions without.
	var f func([]Point) int = sumX
	check(f(s.points), 18)
	var g func(*Shape) string = (*Shape).Name
	check(g(s), "tri2")
	var w byteWriter = &writer{}
	w.Write([]byte("hello"))
	check(w.(*writer).sb.String(), "hello")

	// Variables of const type can be reassigned.
	cs = root
	check(cs.Name(), "root")
	ps = nil
	check(len(ps), 0)

	// Conversions.
	check(string((const []byte)([]byte("abc"))), "abc")
	arr := [2]int{1, 2}
	var carr const *[2]int = &arr
	check(len(carr), 2)
	check(carr[1], 2)
	a2 := *carr // arrays of value types are copied
	a2[0] = 5
	check(arr[0], 1)
}
