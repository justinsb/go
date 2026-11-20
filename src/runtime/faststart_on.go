// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build faststart

package runtime

// faststart reports whether the runtime was built with the faststart
// build tag. See faststart.go.
const faststart = true

// faststartCreateDeferredGoroutines creates the goroutines in want, a
// combination of the faststartWant* constants, whose creation
// faststartDeferGoroutine put off.
//
// It is in this file because the default build must not contain a call
// path from gcStart to the functions that create those goroutines:
// they allocate, and in the default build that makes an initialization
// cycle through the tables of size-specialized malloc functions.
func faststartCreateDeferredGoroutines(want uint32) {
	if want&faststartWantFing != 0 {
		createfing()
	}
	if want&faststartWantCleanup != 0 {
		gcCleanups.createGs()
	}
}
