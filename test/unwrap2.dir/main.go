// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"

	"./a"
)

func use(n int) (string, error) {
	return fmt.Sprint(a.AddOne(n).Add(100)), nil
}

func useNamed(n int) (a.Num, error) {
	return a.AddOneNamed(n).Add(100), nil
}

func generic[T any](v T, bad bool) (T, error) {
	return a.NewBox(v, bad).Get(), nil
}

func check(what string, got, want any) {
	if fmt.Sprint(got) != fmt.Sprint(want) {
		panic(fmt.Sprintf("%s: got %v, want %v", what, got, want))
	}
}

func main() {
	s, err := use(1)
	check("use", s, "102")
	check("use err", err, nil)
	s, err = use(-1)
	check("use", s, "")
	check("use err", err, a.ErrNeg)

	n, err := useNamed(1)
	check("useNamed", n, 102)
	check("useNamed err", err, nil)
	n, err = useNamed(-1)
	check("useNamed", n, 0)
	check("useNamed err", err, a.ErrNeg)

	v, err := generic("hi", false)
	check("generic", v, "hi")
	check("generic err", err, nil)
	v, err = generic("hi", true)
	check("generic", v, "")
	check("generic err", err, a.ErrNeg)

	f, err := a.GetOrErr(2.5, false)
	check("GetOrErr", f, 2.5)
	check("GetOrErr err", err, nil)
	f, err = a.GetOrErr(2.5, true)
	check("GetOrErr", f, 0)
	check("GetOrErr err", err, a.ErrNeg)
}
