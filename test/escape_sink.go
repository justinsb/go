// errorcheck -0 -d=escapesink=1

// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Test the -d=escapesink diagnostic, which classifies each heap
// allocation by the kind of location its address flowed into.

package escape

type T struct {
	a, b int
	s    string
}

type Box struct{ t *T }

var global *T
var ch = make(chan *T, 10)

func viaGlobal() {
	global = &T{a: 1} // ERROR "escsink\tglobal\tPTRLIT"
}

var globalMap = map[string]*T{} // ERROR "escsink\tglobal\tMAPLIT"

func viaGlobalMap() {
	globalMap["k"] = &T{a: 1} // ERROR "escsink\tglobal\tPTRLIT"
}

func viaSend() {
	ch <- &T{a: 2} // ERROR "escsink\tsend\tPTRLIT"
}

func viaGo(f func(*T)) {
	t := &T{a: 3} // ERROR "escsink\tgo\tPTRLIT"
	go func() {   // ERROR "escsink\tgo\tCLOSURE"
		f(t)
	}()
}

func viaReturn() *T {
	return &T{a: 4} // ERROR "escsink\tresult\tPTRLIT"
}

func viaStoreParam(b *Box) {
	b.t = &T{a: 5} // ERROR "escsink\tstore-param\tPTRLIT"
}

func viaStore(bs []*Box) {
	bs[0].t = &T{a: 6} // ERROR "escsink\tstore\tPTRLIT"
}

func (b *Box) Set(t *T) { b.t = t }

func viaCallee(b *Box) {
	// The callee stores its argument through its receiver, so the
	// sink kind is carried through the parameter leak tag.
	b.Set(&T{a: 7}) // ERROR "escsink\tstore-param\tPTRLIT"
}

func viaLocalCallee() int {
	// After inlining, the receiver is a local pointing at a local.
	var b Box
	b.Set(&T{a: 7}) // ERROR "escsink\tstore-local\tPTRLIT"
	return b.t.a
}

func viaNewLocal() int {
	b := new(Box)  // ERROR "escsink\tnoesc\tnew"
	b.t = &T{a: 7} // ERROR "escsink\tstore-local\tPTRLIT"
	return b.t.a
}

func viaParamMap(m map[string]*T) {
	m["k"] = &T{a: 7} // ERROR "escsink\tstore-param\tPTRLIT"
}

func viaDynCall(f func(*T)) {
	f(&T{a: 8}) // ERROR "escsink\tdyncall\tPTRLIT"
}

func viaLoop() {
	var p *T
	for i := 0; i < 3; i++ {
		p = &T{a: i} // ERROR "escsink\tloop\tPTRLIT"
	}
	_ = p
}

func viaSize() {
	x := make([]byte, 1<<20) // ERROR "escsink\tsize\tMAKESLICE"
	_ = x
}

func viaAppend(xs []*T) []*T {
	return append(xs, &T{a: 10}) // ERROR "escsink\tresult\tappend" "escsink\tstore\tPTRLIT"
}

func viaMapKey(m map[*T]int) {
	m[&T{a: 11}] = 1 // ERROR "escsink\tmap-key\tPTRLIT"
}

func viaPanic() {
	panic(&T{a: 12}) // ERROR "escsink\tpanic\tPTRLIT"
}

func noEscape() int {
	s := make([]int, 0, 4) // ERROR "escsink\tnoesc\tMAKESLICE"
	s = append(s, 1)       // ERROR "escsink\tnoesc\tappend"
	return len(s)
}
