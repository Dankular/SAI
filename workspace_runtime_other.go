//go:build !windows

package main

import (
	"context"
	"os/exec"
)

func (a *app) ensureWorkspaceUserland() error { return nil }

func (a *app) workspaceShellCommand(ctx context.Context, command, dir string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	return cmd
}
