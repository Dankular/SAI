//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const msys2BaseURL = "https://repo.msys2.org/distrib/x86_64/msys2-base-x86_64-" + userlandRelease + ".sfx.exe"

func (a *app) ensureWorkspaceUserland() error {
	if a.workspaceUserlandReady() {
		return nil
	}
	if err := a.requireFreeSpace(a.runtimeDir(), 2<<30, "the workspace build userland"); err != nil {
		return err
	}
	if err := os.MkdirAll(a.runtimeDir(), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(a.workspaceUserlandHome(), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(a.workspaceUserlandTemp(), 0700); err != nil {
		return err
	}

	archive := filepath.Join(a.runtimeDir(), "downloads", "msys2-base-"+userlandRelease+".sfx.exe")
	if !isFile(archive) {
		if err := a.downloadFile(msys2BaseURL, archive, "workspace POSIX userland"); err != nil {
			return err
		}
	}
	got, err := fileSHA256(archive)
	if err != nil {
		return err
	}
	if got != userlandSHA256 {
		_ = os.Remove(archive) // a bad or truncated download must not be reused
		return errors.New("workspace userland archive failed its SHA-256 check")
	}

	if !isFile(a.userlandBash()) {
		if err := os.MkdirAll(filepath.Dir(a.userlandDir()), 0700); err != nil {
			return err
		}
		cmd := exec.CommandContext(a.ctx, archive, "-y", "-o"+a.runtimeDir())
		cmd.Dir = a.runtimeDir()
		cmd.Env = a.runtimeEnv()
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
		var output boundedOutput
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("unpack the workspace userland: %w: %s", err, clipText(output.String(), 1200))
		}
	}

	if err := a.runUserlandCommand(" "); err != nil {
		return fmt.Errorf("initialize the workspace userland: %w", err)
	}
	// MSYS2's base archive contains the POSIX shell and package manager. Update the
	// package database first, then install the common build tools in this private tree.
	for _, command := range []string{
		"pacman --noconfirm -Syuu",
		"pacman --noconfirm -Syuu",
		"pacman --noconfirm --needed -S base-devel git",
	} {
		if err := a.runUserlandCommand(command); err != nil {
			return fmt.Errorf("prepare workspace build tools (%s): %w", command, err)
		}
	}
	if err := os.MkdirAll(a.workspaceUserlandDir(), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(a.workspaceUserlandDir(), "runtime.json"), []byte(userlandRelease+"\n"), 0600); err != nil {
		return err
	}
	_ = os.Remove(archive)
	return nil
}

func (a *app) runUserlandCommand(command string) error {
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.userlandBash(), "--login", "-c", command)
	cmd.Dir = a.workspace
	cmd.Env = a.userlandEnv()
	cmd.WaitDelay = 2 * time.Second
	var output boundedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, clipText(output.String(), 2400))
	}
	return nil
}

func (a *app) userlandEnv() []string {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	path := strings.Join([]string{
		filepath.Join(a.userlandDir(), "usr", "bin"),
		filepath.Join(systemRoot, "System32"),
		systemRoot,
	}, string(os.PathListSeparator))
	return a.withToolchain([]string{
		"SystemRoot=" + systemRoot,
		"WINDIR=" + systemRoot,
		"TEMP=" + a.workspaceUserlandTemp(),
		"TMP=" + a.workspaceUserlandTemp(),
		"HOME=" + a.workspaceUserlandHome(),
		"TMPDIR=" + a.workspaceUserlandTemp(),
		"PATH=" + path,
		"CHERE_INVOKING=yes",
		"MSYSTEM=MSYS",
		"MSYS2_PATH_TYPE=inherit",
		"SOVEREIGNAI_WORKSPACE=" + a.workspace,
		"SOVEREIGNAI_ROOT=" + filepath.Join(a.workspaceUserlandDir(), "root"),
	})
}

func (a *app) workspaceShellCommand(ctx context.Context, command, dir string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, a.userlandBash(), "--login", "-c", command)
	cmd.Dir = dir
	cmd.Env = a.userlandEnv()
	cmd.WaitDelay = 2 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return cmd
}
