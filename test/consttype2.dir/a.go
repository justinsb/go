// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package a

type Config struct {
	Name  string
	Hosts []string
	Next  *Config
}

func (c const *Config) Host(i int) string { return c.Hosts[i] }
func (c *Config) AddHost(h string)        { c.Hosts = append(c.Hosts, h) }

type Lenner interface {
	const Len() int
	Grow(n int)
}

type List[E any] struct{ elems []E }

func (l const *List[E]) Len() int         { return len(l.elems) }
func (l const *List[E]) At(i int) const E { return l.elems[i] }
func (l const *List[E]) All() const []E   { return l.elems }
func (l *List[E]) Grow(n int)             { l.elems = append(l.elems, make([]E, n)...) }
func (l *List[E]) Set(i int, e E)         { l.elems[i] = e }

var Default const *Config = &Config{Name: "default", Hosts: []string{"localhost"}}

type View = const *Config

func Hosts(c const *Config) const []string { return c.Hosts }

func Count(args const ...any) int { return len(args) }

func Len(l const Lenner) int { return l.Len() }

type Registry struct {
	Configs map[string]const *Config
	Active  const *Config
}
