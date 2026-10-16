package main

import (
	"os"
	"path/filepath"
)

const userlandRelease = "2026-09-27"

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
