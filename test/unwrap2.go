// rundir

// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Test that functions using the error-propagating selector ("unwrap")
// can be inlined across packages, and that unwraps of calls to
// functions from other packages work.

package ignored
