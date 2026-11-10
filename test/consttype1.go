// errorcheck

// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Test that the compiler rejects modifications through const types.

package p

type T struct {
	n    int
	s    []int
	m    map[string]*T
	next *T
}

func (t const *T) N() int { return t.n }
func (t *T) SetN(n int)   { t.n = n }

type I interface {
	const N() int
	SetN(int)
}

func f(t const *T, s const []int, m const map[string]*T, i const I, mt *T) {
	t.n = 1         // ERROR "cannot assign to t.n \(read-only: accessed through const value\)"
	t.n++           // ERROR "read-only"
	t.s[0] = 1      // ERROR "read-only"
	t.next.n = 1    // ERROR "read-only"
	t.m["x"] = nil  // ERROR "read-only"
	*t = T{}        // ERROR "read-only"
	t.SetN(1)       // ERROR "cannot call non-const method SetN on const value t"
	s[0] = 1        // ERROR "read-only"
	_ = append(s, 1) // ERROR "cannot append to const value"
	copy(s, s)      // ERROR "cannot copy into const value"
	clear(s)        // ERROR "cannot clear const value"
	m["x"] = nil    // ERROR "read-only"
	m["x"].n = 1    // ERROR "read-only"
	delete(m, "x")  // ERROR "cannot delete from const value"
	i.SetN(1)       // ERROR "cannot call non-const method SetN on const value i"

	var _ *T = t              // ERROR "discards const qualifier"
	var _ []int = s           // ERROR "discards const qualifier"
	var _ map[string]*T = m   // ERROR "discards const qualifier"
	var _ I = i               // ERROR "discards const qualifier"
	var _ any = t             // ERROR "discards const qualifier"
	var _ *int = &t.n         // ERROR "discards const qualifier"
	var _ []int = t.s[1:]     // ERROR "discards const qualifier"
	_ = []int(s)              // ERROR "discards const qualifier"
	_ = (*T)(t)               // ERROR "discards const qualifier"
	var _ const I = t         // ERROR "method SetN is not a const method"
	var _ func(const *T) = func(*T) {} // ERROR "cannot use"

	for _, x := range m {
		x.n = 1 // ERROR "read-only"
	}
	if x, ok := i.(*T); ok {
		x.n = 1 // ERROR "read-only"
	}
	p := &t.n
	*p = 1 // ERROR "read-only"
	q := t.next
	q.SetN(1) // ERROR "cannot call non-const method"

	dst := make([]*T, 1)
	copy(dst, m2s(m)) // ERROR "discards const qualifier"
	func() {
		t.n = 2 // ERROR "read-only"
	}()
}

func m2s(m const map[string]*T) const []*T { return nil }

func g(t const *T) *T {
	return t // ERROR "discards const qualifier"
}

type Bad const []int // ERROR "cannot use a const type as RHS in type definition"

type J interface{ const N() int }

type U struct{}

func (*U) N() int { return 0 }

var _ J = &U{} // ERROR "method N is not a const method"
