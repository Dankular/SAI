//go:build windows

package main

import "testing"

func TestNativeDragPositionFlagsAvoidZOrderAndFrameRepaints(t *testing.T) {
	flags := nativeDragPositionFlags()
	if flags&swpNoSizeNative == 0 {
		t.Fatal("native drag should leave the layered surface size alone")
	}
	if flags&swpNoZOrderNative == 0 {
		t.Fatal("native drag should preserve the overlay's desktop z-order")
	}
	if flags&swpFrameChanged != 0 {
		t.Fatal("native drag should not trigger non-client frame recalculation")
	}
	if flags&swpShowWindow != 0 {
		t.Fatal("native drag should not repeatedly show the overlay")
	}
}
