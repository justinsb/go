// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"

	"./a"
)

func check(got, want any) {
	if got != want {
		panic(fmt.Sprintf("got %v, want %v", got, want))
	}
}

func main() {
	check(a.Default.Name, "default")
	check(a.Default.Host(0), "localhost")
	check(len(a.Hosts(a.Default)), 1)

	c := &a.Config{Name: "c"}
	c.AddHost("h1")
	c.AddHost("h2")
	var v a.View = c
	check(v.Host(1), "h2")
	hosts := a.Hosts(c)
	check(hosts[0], "h1")
	check(a.Count(v, hosts, 1), 3)

	l := &a.List[*a.Config]{}
	l.Grow(2)
	l.Set(0, c)
	var cl const *a.List[*a.Config] = l
	check(cl.Len(), 2)
	check(cl.At(0).Host(0), "h1")
	check(cl.All()[0].Name, "c")
	check(a.Len(l), 2)
	var lenner const a.Lenner = l
	check(lenner.Len(), 2)

	r := a.Registry{Configs: map[string]const *a.Config{"c": c}, Active: a.Default}
	check(r.Configs["c"].Name, "c")
	r.Active = c
	check(r.Active.Host(0), "h1")
}
