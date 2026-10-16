//go:build windows

package main

import (
	"context"
	"os/exec"
)

// shellCommand runs in the app-managed POSIX userland with a workspace-local home and temp.
func shellCommand(ctx context.Context, a *app, command, dir string) *exec.Cmd {
	return a.workspaceShellCommand(ctx, command, dir)
}
