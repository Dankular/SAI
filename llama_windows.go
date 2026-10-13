//go:build windows

package main

import (
	"os/exec"
	"syscall"
	"unsafe"
)

const createNoWindow = 0x08000000

// hideWindow keeps child console programs (llama-server) from opening a window.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// diskFreeBytes reports the bytes available to the current user on the volume
// holding path, or 0 if it cannot be determined.
func diskFreeBytes(path string) uint64 {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")
	var free, total, totalFree uint64
	r, _, _ := proc.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&free)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&totalFree)))
	if r == 0 {
		return 0
	}
	return free
}
