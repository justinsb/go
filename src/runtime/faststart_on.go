// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build faststart

package runtime

// faststart reports whether the runtime was built with the faststart
// build tag. See faststart.go.
const faststart = true
