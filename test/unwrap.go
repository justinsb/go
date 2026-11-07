// run

// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Test the error-propagating selector ("unwrap") on (T, error)
// multi-value expressions.

package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

var errBad = errors.New("bad")

type T struct {
	N    int
	Next *T
}

func (t T) Val() int    { return t.N }
func (t T) Add(k int) T { return T{N: t.N + k} }
func (t *T) PVal() int  { return t.N }
func (t T) Pair() (T, error) {
	if t.N < 0 {
		return T{}, errBad
	}
	return T{N: t.N * 10}, nil
}

type E struct{ *T }

func mkT(n int) (T, error) {
	if n < 0 {
		return T{}, errBad
	}
	return T{N: n, Next: &T{N: n + 1}}, nil
}

func mkP(n int) (*T, error) {
	if n < 0 {
		return nil, errBad
	}
	return &T{N: n}, nil
}

func mkE(n int) (E, error) {
	if n < 0 {
		return E{}, errBad
	}
	return E{&T{N: n}}, nil
}

var log []string

//go:noinline
func trace(s string) int {
	log = append(log, s)
	return 1
}

func field(n int) (int, error)  { return mkT(n).N, nil }
func method(n int) (int, error) { return mkT(n).Add(1).Val(), nil }
func chain(n int) (int, error)  { return mkT(n).Pair().Pair().Add(1).N, nil }
func viaPtr(n int) (int, error) { return mkP(n).PVal() + mkP(n).Val(), nil }
func viaEmb(n int) (int, error) { return mkE(n).PVal() + mkE(n).N, nil }
func nested(n int) (int, error) { return mkP(mkT(n).N).N, nil }
func errOnly(n int) error       { _ = mkT(n).N; return nil }

func named(n int) (res int, err error) {
	res = 42 // zeroed by a propagated error
	return mkT(n).N, nil
}

func deferred(n int) (res int, err error) {
	defer func() {
		if err != nil {
			res = -1
		}
	}()
	return mkT(n).N, nil
}

// Many result types: zero values must be produced for each.
func many(n int) ([2]int, T, string, float64, complex128, chan int, func(), map[int]int, any, error) {
	return [2]int{mkT(n).N}, T{N: mkT(n).N}, strconv.Itoa(mkT(n).N), 1.5, 2i, nil, nil, nil, nil, nil
}

// Evaluation order: function calls are evaluated left to right, and
// the propagation check happens at the point of the unwrap.
func order(n int) (int, error) {
	return trace("a") + mkT(n).Add(trace("b")).N + trace("c") + mkT(-n).N + trace("d"), nil
}

func methodValue(n int) (int, error) {
	f := mkT(n).Add
	return f(5).N, nil
}

func closure(n int) (int, error) {
	f := func(k int) (T, error) { return mkT(k).Add(n), nil }
	return f(n).Add(mkT(n).N).N, nil
}

type S int

func (s S) String() string { return "S" + strconv.Itoa(int(s)) }

func mk[T any](v T, bad bool) (T, error) {
	if bad {
		var zero T
		return zero, errBad
	}
	return v, nil
}

func generic[T fmt.Stringer](v T, bad bool) (string, error) {
	return mk(v, bad).String(), nil
}

type Box[T any] struct{ V T }

func (b Box[T]) Get() T { return b.V }

func genericField[T any](v T, bad bool) (T, error) {
	return mk(Box[T]{v}, bad).V, nil
}

func genericMethod[T any](v T, bad bool) (T, error) {
	return mk(Box[T]{v}, bad).Get(), nil
}

func contexts(n int) (res int, err error) {
	defer func(x int) { res += x }(mkT(n).N)
	if v := mkT(n).N; v > 0 {
		res += v
	}
	for i := mkT(n).N; i < mkT(n).N+2; i++ {
		res++
	}
	switch mkT(n).N {
	case n:
		res += 10
	}
	m := map[int]int{mkT(n).N: mkT(n).N}
	res += m[n]
	s := []int{mkT(n).N, mkT(n + 1).N}
	res += s[1]
	ch := make(chan int, 1)
	select {
	case ch <- mkT(n).N:
	}
	res += <-ch
	done := make(chan int)
	go func(v int) { done <- v }(mkT(n).N)
	res += <-done
	return res, nil
}

func stat(name string) (int64, error) {
	return os.Open(name).Stat().Size(), nil
}

func check(what string, got, want any) {
	if fmt.Sprint(got) != fmt.Sprint(want) {
		panic(fmt.Sprintf("%s: got %v, want %v", what, got, want))
	}
}

func check2(what string, got1, got2, want1, want2 any) {
	check(what, got1, want1)
	check(what+" error", got2, want2)
}

func main() {
	v, err := field(3)
	check2("field", v, err, 3, nil)
	v, err = field(-3)
	check2("field err", v, err, 0, errBad)

	v, err = method(3)
	check2("method", v, err, 4, nil)
	v, err = method(-3)
	check2("method err", v, err, 0, errBad)

	v, err = chain(3)
	check2("chain", v, err, 301, nil)
	v, err = chain(-3)
	check2("chain err", v, err, 0, errBad)

	v, err = viaPtr(3)
	check2("viaPtr", v, err, 6, nil)
	v, err = viaPtr(-3)
	check2("viaPtr err", v, err, 0, errBad)

	v, err = viaEmb(3)
	check2("viaEmb", v, err, 6, nil)
	v, err = viaEmb(-3)
	check2("viaEmb err", v, err, 0, errBad)

	v, err = nested(3)
	check2("nested", v, err, 3, nil)
	v, err = nested(-3)
	check2("nested err", v, err, 0, errBad)

	check("errOnly", errOnly(3), nil)
	check("errOnly err", errOnly(-3), errBad)

	v, err = named(3)
	check2("named", v, err, 3, nil)
	v, err = named(-3)
	check2("named err", v, err, 0, errBad)

	v, err = deferred(3)
	check2("deferred", v, err, 3, nil)
	v, err = deferred(-3)
	check2("deferred err", v, err, -1, errBad)

	a, t, s, f, c, ch, fn, m, i, err := many(3)
	check("many", fmt.Sprintln(a, t, s, f, c, ch, fn == nil, m, i, err), "[3 0] {3 <nil>} 3 1.5 (0+2i) <nil> true map[] <nil> <nil>\n")
	a, t, s, f, c, ch, fn, m, i, err = many(-3)
	check("many err", fmt.Sprintln(a, t, s, f, c, ch, fn == nil, m, i, err), "[0 0] {0 <nil>}  0 (0+0i) <nil> true map[] <nil> bad\n")

	log = nil
	v, err = order(3)
	check2("order", v, err, 0, errBad)
	check("order log", log, []string{"a", "b", "c"})

	v, err = methodValue(3)
	check2("methodValue", v, err, 8, nil)
	v, err = methodValue(-3)
	check2("methodValue err", v, err, 0, errBad)

	v, err = closure(3)
	check2("closure", v, err, 9, nil)
	v, err = closure(-3)
	check2("closure err", v, err, 0, errBad)

	s, err = generic(S(3), false)
	check2("generic", s, err, "S3", nil)
	s, err = generic(S(3), true)
	check2("generic err", s, err, "", errBad)

	v, err = genericField(3, false)
	check2("genericField", v, err, 3, nil)
	v, err = genericField(3, true)
	check2("genericField err", v, err, 0, errBad)

	s, err = genericMethod("x", false)
	check2("genericMethod", s, err, "x", nil)
	s, err = genericMethod("x", true)
	check2("genericMethod err", s, err, "", errBad)

	v, err = contexts(1)
	check2("contexts", v, err, 19, nil)
	v, err = contexts(-1)
	check2("contexts err", v, err, 0, errBad)

	_, err = stat("/nonexistent/file")
	if err == nil {
		panic("stat: expected error")
	}
}
