//go:build !windows

package main

import "os/exec"

func hideWindow(cmd *exec.Cmd) {}

// diskFreeBytes is unknown off Windows; 0 disables the free-space check.
func diskFreeBytes(path string) uint64 { return 0 }
