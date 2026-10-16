package main

import (
	"os"
	"path/filepath"
)

// userlandRelease is the MSYS2 base archive the shell tool runs in, named as in repo.msys2.org (a date
// without dashes). The archive's SHA-256 is pinned here because MSYS2 publishes signatures, not
// checksum files; update both together.
const (
	userlandRelease = "20260927"
	userlandSHA256  = "ad336cccfda47758b5e15cda993fbba421115cb0b126697daef1ee4dfe37209f"
)

func (a *app) userlandDir() string {
	return filepath.Join(a.runtimeDir(), "msys64")
}

func (a *app) userlandBash() string {
	return filepath.Join(a.userlandDir(), "usr", "bin", "bash.exe")
}

func (a *app) workspaceUserlandDir() string {
	return filepath.Join(a.workspace, ".userland")
}

func (a *app) workspaceUserlandHome() string {
	return filepath.Join(a.workspaceUserlandDir(), "root", "home", "agent")
}

func (a *app) workspaceUserlandTemp() string {
	return filepath.Join(a.workspaceUserlandDir(), "root", "tmp")
}

func (a *app) workspaceUserlandReady() bool {
	if !isFile(a.userlandBash()) {
		return false
	}
	data, err := os.ReadFile(filepath.Join(a.workspaceUserlandDir(), "runtime.json"))
	return err == nil && string(data) == userlandRelease+"\n"
}
