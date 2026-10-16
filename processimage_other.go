//go:build !windows

package main

// processImagePath is only needed on Windows, where evictOrphans runs.
func processImagePath(pid int) string { return "" }
