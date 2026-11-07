// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements the type checking of the error-propagating
// selector (the "Result" experiment).
//
// A multi-value expression of type (T, error) may be used as the operand
// of a selector expression:
//
//	f(x).Name
//	f(x).Method(args)
//
// The selector applies to the T value. If the error value is non-nil, the
// enclosing function returns immediately, with the zero value for every
// result but the last, and the error as its last result. The enclosing
// function must therefore have error as its final result type.
//
// Conceptually, (T, error) is the same as a Result[T, error] type: the
// selector unwraps the Ok value, and propagates the Err value to the
// caller, much like Rust's ? operator.

package types

import (
	"go/ast"
	. "internal/types/errors"
)

// unwrapResult implements the error-propagating selector for the operand
// x of the selector expression e.
//
// If x is a multi-value expression of type (T, error), x becomes a
// (non-addressable) value of type T, provided the enclosing function has
// error as its final result type.
//
// If x is any other multi-value expression, an error is reported, as for
// any other single-value context.
func (check *Checker) unwrapResult(x *operand, e *ast.SelectorExpr) {
	if x.mode() != value {
		return
	}
	// tuple types are never named - no need for underlying type below
	tuple, ok := x.typ().(*Tuple)
	if !ok {
		return
	}
	assert(tuple.Len() != 1)

	if tuple.Len() != 2 || !Identical(tuple.At(1).Type(), universeError) {
		check.errorf(x, TooManyValues, "multiple-value %s in single-value context", x)
		x.invalidate()
		return
	}

	sig := check.sig
	if sig == nil {
		check.errorf(x, TooManyValues, "cannot unwrap %s outside of a function body", x)
		x.invalidate()
		return
	}
	n := sig.results.Len()
	if n == 0 || !Identical(sig.results.At(n-1).Type(), universeError) {
		check.errorf(x, TooManyValues, "cannot unwrap %s: enclosing function must have error as its final result", x)
		x.invalidate()
		return
	}

	// The selector operates on the first value, which, like any other
	// function result, is not addressable.
	x.mode_ = value
	x.typ_ = tuple.At(0).Type()
}
