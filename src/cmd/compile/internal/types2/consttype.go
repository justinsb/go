// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types2

// A ConstType represents a const type "const T".
//
// A const type is a deeply read-only view of its element type T:
// a value of type const T provides access to the same values as a
// value of type T, but nothing reachable through the const value
// (slice and array elements, map entries, pointer referents, struct
// fields, and so on, transitively) can be modified via that value.
//
// The element type of a ConstType is never itself a Const.
type ConstType struct {
	elem Type
}

// NewConstType returns the const type for the given element type.
// If elem is already a const type, NewConstType returns elem's
// const type unchanged (const const T is const T).
func NewConstType(elem Type) *ConstType {
	if c, _ := Unalias(elem).(*ConstType); c != nil {
		return c
	}
	return &ConstType{elem: elem}
}

// Elem returns the element type T of the const type "const T".
func (c *ConstType) Elem() Type { return c.elem }

func (c *ConstType) Underlying() Type { return c }
func (c *ConstType) String() string   { return TypeString(c, nil) }

// asConst returns t as a const type if it is one; otherwise it returns nil.
func asConst(t Type) *ConstType {
	if t == nil {
		return nil
	}
	c, _ := Unalias(t).(*ConstType)
	return c
}

// isConst reports whether t is a const type.
func isConst(t Type) bool {
	return asConst(t) != nil
}

// unconst returns the element type of t if t is a const
// type; otherwise it returns t unchanged.
func unconst(t Type) Type {
	if c := asConst(t); c != nil {
		return c.elem
	}
	return t
}

// constOf returns the const type for t, unless t is invalid.
func constOf(t Type) Type {
	if t == nil || !isValid(t) {
		return t
	}
	return NewConstType(t)
}

// isValueType reports whether values of type t are free of references:
// copying such a value yields a fully independent value, and so there
// is nothing that could be modified through a const view of the copy.
// For such types, const T and T are interchangeable for values.
//
// Basic types (including strings), and arrays and structs of value
// types are value types. Function and channel types are opaque with
// respect to const and are also considered value types.
func isValueType(t Type) bool {
	return isValueType1(t, nil)
}

func isValueType1(t Type, seen map[*Named]bool) bool {
	t = Unalias(t)
	if n, _ := t.(*Named); n != nil {
		if seen[n] {
			return true // cycle (invalid type); avoid infinite recursion
		}
		if seen == nil {
			seen = make(map[*Named]bool)
		}
		seen[n] = true
	}
	switch u := t.Underlying().(type) {
	case *Basic:
		return true
	case *Array:
		return isValueType1(u.elem, seen)
	case *Struct:
		for _, f := range u.fields {
			if !isValueType1(f.typ, seen) {
				return false
			}
		}
		return true
	case *Signature, *Chan:
		return true
	case *ConstType:
		return isValueType1(u.elem, seen)
	}
	// pointers, slices, maps, interfaces, type parameters
	return false
}

// constView returns the type of a value of type t obtained through
// a const value: it is const t unless t is a value type.
func constView(t Type) Type {
	if t == nil || !isValid(t) {
		return t
	}
	if isValueType(t) {
		return unconst(t)
	}
	return NewConstType(t)
}

// eraseValueConst returns T if t is a const type "const T" where T
// is a value type; otherwise it returns t.
func eraseValueConst(t Type) Type {
	if c := asConst(t); c != nil && isValueType(c.elem) {
		return c.elem
	}
	return t
}

// identicalIgnoreConst reports whether x and y are identical if const
// qualifiers are ignored (at the top-level and inside composite types,
// but not inside function, channel, or interface types, nor in type
// arguments). It is used to compare types that are viewed through a
// const type, in which case inner const qualifiers are redundant.
func identicalIgnoreConst(x, y Type) bool {
	var c comparer
	c.ignoreConst = true
	return c.identical(x, y, nil)
}
