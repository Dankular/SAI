package main

import (
	"os"
	"path/filepath"
	"runtime/pprof"
	"time"
)

// Diagnostics: with SOVEREIGNAI_PROFILE_DIR set, the app records a CPU profile of itself (20 seconds,
// starting 25 seconds after launch, so startup is left out) into <dir>\cpu.pprof. Read it with
// `go tool pprof SovereignAI.exe cpu.pprof`. Nothing happens when the variable is not set.
func init() {
	dir := os.Getenv("SOVEREIGNAI_PROFILE_DIR")
	if dir == "" {
		return
	}
	go func() {
		time.Sleep(25 * time.Second)
		file, err := os.Create(filepath.Join(dir, "cpu.pprof"))
		if err != nil {
			return
		}
		defer file.Close()
		if pprof.StartCPUProfile(file) != nil {
			return
		}
		time.Sleep(20 * time.Second)
		pprof.StopCPUProfile()
	}()
}
