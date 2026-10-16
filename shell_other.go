//go:build !windows

package main

import (
	"context"
	"os/exec"
)

func shellCommand(ctx context.Context, a *app, command, dir string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	return cmd
}
