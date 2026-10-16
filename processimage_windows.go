//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

const processQueryLimitedInformation = 0x1000

// processImagePath returns the full path of the program a process is running, or "" when it cannot be
// read (the process is gone, or belongs to someone this user may not inspect).
func processImagePath(pid int) string {
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer syscall.CloseHandle(handle)
	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	proc := kernel32.NewProc("QueryFullProcessImageNameW")
	if r, _, _ := proc.Call(uintptr(handle), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:size])
}
