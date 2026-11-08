// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package abi

import "internal/goarch"

// GRegionOffset is the offset of the g.region field within runtime.g.
// The compiler loads it in the write barrier sequence to decide whether
// a goroutine is inside a memory region (see runtime/region.go). The
// runtime checks that the offset matches the struct layout.
const GRegionOffset = 4 * goarch.PtrSize

// GRegionIgnoreOffset is the offset of g.regionIgnore, the depth of
// region.Ignore scopes; nonzero means allocations bypass the region.
const GRegionIgnoreOffset = GRegionOffset + goarch.PtrSize
