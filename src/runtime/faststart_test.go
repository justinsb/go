// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"internal/runtime/gc"
	"runtime"
	"strings"
	"testing"
)

// TestFaststartSizeClass checks that faststartSizeClass maps every size
// class to the smallest power-of-two size class that is at least as large,
// and that doing so does not move an object across the boundary between
// objects with and without a malloc header.
func TestFaststartSizeClass(t *testing.T) {
	isPow2 := func(n uint16) bool { return n&(n-1) == 0 }

	table := runtime.FaststartSizeClass
	if table[0] != 0 {
		t.Errorf("size class 0 maps to %d, want 0", table[0])
	}
	for c := 1; c < gc.NumSizeClasses; c++ {
		m := int(table[c])
		if m < c || m >= gc.NumSizeClasses {
			t.Errorf("size class %d maps to %d, which is not a larger size class", c, m)
			continue
		}
		size, msize := gc.SizeClassToSize[c], gc.SizeClassToSize[m]
		if !isPow2(msize) {
			t.Errorf("size class %d (%d bytes) maps to size class %d (%d bytes), which is not a power of two", c, size, m, msize)
		}
		for i := c; i < m; i++ {
			if isPow2(gc.SizeClassToSize[i]) {
				t.Errorf("size class %d (%d bytes) maps to size class %d (%d bytes), but size class %d (%d bytes) is smaller", c, size, m, msize, i, gc.SizeClassToSize[i])
			}
		}
		if (size <= gc.MinSizeForMallocHeader) != (msize <= gc.MinSizeForMallocHeader) {
			t.Errorf("size class %d (%d bytes) maps to size class %d (%d bytes), on the other side of the malloc header threshold", c, size, m, msize)
		}
	}
}

// TestFaststartDeferred checks that a program built in fast start mode does
// not start the runtime's background goroutines until its first GC cycle,
// and that it does start them then.
func TestFaststartDeferred(t *testing.T) {
	background := []string{"runtime.bgsweep", "runtime.bgscavenge", "runtime.forcegchelper"}
	for _, tc := range []struct {
		name     string
		flags    []string
		prog     string
		deferred bool // background goroutines should not exist
	}{
		{name: "default", prog: "Faststart"},
		{name: "default-gc", prog: "FaststartGC"},
		{name: "faststart", flags: []string{"-tags=faststart"}, prog: "Faststart", deferred: true},
		{name: "faststart-gc", flags: []string{"-tags=faststart"}, prog: "FaststartGC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exe, err := buildTestProg(t, "testprog", tc.flags...)
			if err != nil {
				t.Fatal(err)
			}
			// System goroutines only show up in the traceback
			// of a crash, and only at GOTRACEBACK=system.
			got := runBuiltTestProg(t, exe, tc.prog, "GOTRACEBACK=system")
			if !strings.Contains(got, "panic: crash") {
				t.Fatalf("program did not crash as expected:\n%s", got)
			}
			for _, fn := range background {
				if has := strings.Contains(got, fn); has == tc.deferred {
					t.Errorf("traceback contains %s = %v, want %v", fn, has, !tc.deferred)
				}
			}
			if t.Failed() {
				t.Logf("output:\n%s", got)
			}
		})
	}
}

// TestFaststartGOMAXPROCS checks that a program built in fast start mode
// starts with a single P, and moves to the default GOMAXPROCS when it
// asks what GOMAXPROCS is or starts a goroutine.
func TestFaststartGOMAXPROCS(t *testing.T) {
	exe, err := buildTestProg(t, "testprog", "-tags=faststart")
	if err != nil {
		t.Fatal(err)
	}
	for _, prog := range []string{"FaststartGOMAXPROCS", "FaststartGOMAXPROCSGo"} {
		t.Run(prog, func(t *testing.T) {
			got := runBuiltTestProg(t, exe, prog)
			if want := "OK\n"; got != want {
				t.Errorf("got:\n%swant:\n%s", got, want)
			}
		})
	}
}

// TestFaststartSysmon checks that a program built in fast start mode,
// which starts without sysmon, gets it started in time for the things
// that depend on it.
func TestFaststartSysmon(t *testing.T) {
	exe, err := buildTestProg(t, "testprog", "-tags=faststart")
	if err != nil {
		t.Fatal(err)
	}
	for _, prog := range []string{"FaststartTimerSpin", "FaststartTimerSyscall"} {
		t.Run(prog, func(t *testing.T) {
			got := runBuiltTestProg(t, exe, prog)
			if want := "OK\n"; got != want {
				t.Errorf("got:\n%swant:\n%s", got, want)
			}
		})
	}
}
