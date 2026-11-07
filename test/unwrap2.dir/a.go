// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package a

import "errors"

var ErrNeg = errors.New("negative")

type Num int

func (n Num) Add(m Num) Num { return n + m }

func MkNum(n int) (Num, error) {
	if n < 0 {
		return 0, ErrNeg
	}
	return Num(n), nil
}

// AddOne is small enough to be inlined into its callers.
func AddOne(n int) (Num, error) {
	return MkNum(n).Add(1), nil
}

// AddOneNamed uses named results.
func AddOneNamed(n int) (res Num, err error) {
	res = 99
	return MkNum(n).Add(1), nil
}

type Box[T any] struct{ V T }

func (b Box[T]) Get() T { return b.V }

func NewBox[T any](v T, bad bool) (*Box[T], error) {
	if bad {
		return nil, ErrNeg
	}
	return &Box[T]{V: v}, nil
}

func GetOrErr[T any](v T, bad bool) (T, error) {
	return NewBox(v, bad).Get(), nil
}
