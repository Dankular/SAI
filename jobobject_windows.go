//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// Every process this program starts (the model servers, the speech server, the desktop widget's
// browser window) belongs to a Windows Job Object that is set to kill its members when the job's last
// handle closes. This program holds that handle, so the operating system closes it however the
// program ends: a normal exit, a crash, End Task, or being killed from a terminal. Children cannot be
// left running as orphans, and no cleanup code has to get a chance to run.
//
// Putting this process into the job is enough: processes it starts inherit the job.

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW   = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJob  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJob = kernel32.NewProc("AssignProcessToJobObject")
	procGetCurrentProcess  = kernel32.NewProc("GetCurrentProcess")
	jobHandle              uintptr // never closed: closing it is what kills the children
)

const (
	jobObjectExtendedLimitInformation = 9
	jobLimitKillOnJobClose            = 0x00002000
)

type jobBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobIOCounters struct{ a, b, c, d, e, f uint64 }

type jobExtendedLimitInformation struct {
	BasicLimitInformation jobBasicLimitInformation
	IoInfo                jobIOCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// bindChildrenToUs returns false if the job could not be set up (the program still works, but
// children can outlive it if it is killed).
func bindChildrenToUs() bool {
	job, _, _ := procCreateJobObjectW.Call(0, 0)
	if job == 0 {
		return false
	}
	info := jobExtendedLimitInformation{}
	info.BasicLimitInformation.LimitFlags = jobLimitKillOnJobClose
	if ok, _, _ := procSetInformationJob.Call(job, jobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info)); ok == 0 {
		return false
	}
	self, _, _ := procGetCurrentProcess.Call()
	if ok, _, _ := procAssignProcessToJob.Call(job, self); ok == 0 {
		return false // already in a job that does not allow nesting
	}
	jobHandle = job
	return true
}
