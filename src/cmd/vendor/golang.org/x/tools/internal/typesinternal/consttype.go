// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package typesinternal

import "go/types"

// Unconst returns T given the const type "const T" (or an alias
// thereof). For all other types it is the identity function.
//
// A const type is a deeply read-only view of T with the same
// representation, layout, and zero value as T, so most analyses
// should simply look through it.
func Unconst(t types.Type) types.Type {
	if c, ok := types.Unalias(t).(*types.ConstType); ok {
		return c.Elem()
	}
	return t
}
