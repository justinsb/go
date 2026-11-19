// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "internal/abi"

// faststartInitsig does what initsig does for signal i, but with one
// system call rather than two, by having the kernel hand back the old
// signal action as it installs the new one. It reports whether it did
// so. If not, initsig must deal with the signal itself.
//
// initsig reads the old action first because for some signals it decides
// whether to install the Go handler at all based on what it finds. This
// only handles the signals for which that decision does not depend on
// the old action.
//
//go:nosplit
//go:nowritebarrierrec
func faststartInitsig(i uint32) bool {
	if i == _SIGHUP || i == _SIGINT {
		// sigInstallGoHandler respects an inherited SIG_IGN.
		return false
	}
	if !sigInstallGoHandler(i) {
		return false
	}
	var old sigactiont
	handlingSig[i] = 1
	setsig1(i, abi.FuncPCABIInternal(sighandler), &old)
	fwdSig[i] = old.sa_handler
	return true
}
