// errorcheck

// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Verify that invalid uses of the error-propagating selector ("unwrap")
// on multi-value expressions are rejected.

package p

type T struct{ N int }

func (t T) Val() int   { return t.N }
func (t *T) PVal() int { return t.N }

type MyErr struct{}

func (*MyErr) Error() string { return "" }

func two() (T, error)        { return T{}, nil }
func twoBool() (T, bool)     { return T{}, false }
func twoMyErr() (T, *MyErr)  { return T{}, nil }
func three() (T, int, error) { return T{}, 0, nil }
func errFirst() (error, T)   { return nil, T{} }

var _ = two().N // ERROR "cannot unwrap two\(\) .*outside of a function body"

func noResults()              { _ = two().N }                                 // ERROR "cannot unwrap two\(\) .*enclosing function must have error as its final result"
func noErr() int              { return two().N }                              // ERROR "cannot unwrap two\(\) .*enclosing function must have error as its final result"
func wrongLast() (error, int) { return nil, two().N }                         // ERROR "cannot unwrap two\(\) .*enclosing function must have error as its final result"
func anyLast() (int, any)     { return two().N, nil }                         // ERROR "cannot unwrap two\(\) .*enclosing function must have error as its final result"
func inLiteral() (int, error) { return func() int { return two().N }(), nil } // ERROR "cannot unwrap two\(\) .*enclosing function must have error as its final result"

func ok() (int, error) {
	_ = twoBool().N  // ERROR "multiple-value twoBool\(\) .*in single-value context"
	_ = twoMyErr().N // ERROR "multiple-value twoMyErr\(\) .*in single-value context"
	_ = three().N    // ERROR "multiple-value three\(\) .*in single-value context"
	_ = errFirst().N // ERROR "multiple-value errFirst\(\) .*in single-value context"
	_ = two().PVal() // ERROR "cannot call pointer method PVal on T"
	two().N = 1      // ERROR "cannot assign to two\(\).N"
	two().N++        // ERROR "cannot assign to two\(\).N"
	_ = &two().N     // ERROR "cannot take address of two\(\).N"
	_ = two().Nope   // ERROR "two\(\).Nope undefined"
	_ = two()        // ERROR "assignment mismatch: 1 variable but two returns 2 values"
	return two().N, nil
}
