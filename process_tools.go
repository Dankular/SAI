package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxAgentProcessOutput = 96 * 1024

type boundedOutput struct {
	mu   sync.Mutex
	data []byte
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > maxAgentProcessOutput {
		b.data = append([]byte(nil), b.data[len(b.data)-maxAgentProcessOutput:]...)
	}
	return len(p), nil
}

func (b *boundedOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(append([]byte(nil), b.data...))
}

type managedAgentProcess struct {
	mu        sync.Mutex
	id        string
	command   string
	startedAt time.Time
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	done      bool
	exitCode  int
	errorText string
	output    boundedOutput
}

type processRegistry struct {
	mu     sync.Mutex
	nextID uint64
	byID   map[string]*managedAgentProcess
}

func (a *app) startAgentProcess(command string, timeout time.Duration) (*managedAgentProcess, error) {
	if !a.workspaceUserlandReady() {
		return nil, errors.New("the workspace POSIX userland is not installed")
	}
	if timeout <= 0 || timeout > time.Hour {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(a.ctx, timeout)
	process := &managedAgentProcess{command: command, startedAt: time.Now(), cancel: cancel}
	process.cmd = shellCommand(ctx, a, command, a.workspace)
	process.cmd.Stdout, process.cmd.Stderr = &process.output, &process.output
	if err := process.cmd.Start(); err != nil {
		cancel()
		return nil, err
	}

	a.agent.mu.Lock()
	registry := &a.agent.processes
	registry.mu.Lock()
	registry.nextID++
	process.id = fmt.Sprintf("p%d", registry.nextID)
	if registry.byID == nil {
		registry.byID = make(map[string]*managedAgentProcess)
	}
	registry.byID[process.id] = process
	pruneAgentProcesses(registry)
	registry.mu.Unlock()
	a.agent.mu.Unlock()

	go func() {
		err := process.cmd.Wait()
		process.mu.Lock()
		process.done = true
		if err != nil {
			var exit interface{ ExitCode() int }
			if errors.As(err, &exit) {
				process.exitCode = exit.ExitCode()
			} else {
				process.exitCode = -1
				process.errorText = err.Error()
			}
		} else {
			process.exitCode = 0
		}
		process.mu.Unlock()
		cancel()
	}()
	return process, nil
}

func pruneAgentProcesses(registry *processRegistry) {
	if len(registry.byID) <= 64 {
		return
	}
	var completed []*managedAgentProcess
	for _, process := range registry.byID {
		process.mu.Lock()
		done := process.done
		process.mu.Unlock()
		if done {
			completed = append(completed, process)
		}
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].startedAt.Before(completed[j].startedAt) })
	for len(registry.byID) > 48 && len(completed) > 0 {
		delete(registry.byID, completed[0].id)
		completed = completed[1:]
	}
}

func (a *app) agentProcess(id string) *managedAgentProcess {
	a.agent.mu.Lock()
	defer a.agent.mu.Unlock()
	a.agent.processes.mu.Lock()
	defer a.agent.processes.mu.Unlock()
	return a.agent.processes.byID[id]
}

func toolProcess(ctx context.Context, a *app, args map[string]any) (string, error) {
	switch strings.ToLower(argString(args, "action")) {
	case "start":
		command := strings.TrimSpace(argString(args, "command"))
		if command == "" {
			return "", errors.New("no command given")
		}
		seconds := argInt(args, "timeout_seconds", 600)
		if seconds < 1 {
			seconds = 600
		}
		if seconds > 3600 {
			seconds = 3600
		}
		process, err := a.startAgentProcess(command, time.Duration(seconds)*time.Second)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Started process %s. Poll it with action= poll and process_id= %s.", process.id, process.id), nil
	case "poll":
		process := a.agentProcess(argString(args, "process_id"))
		if process == nil {
			return "", errors.New("there is no process with that id")
		}
		process.mu.Lock()
		defer process.mu.Unlock()
		state := "running"
		if process.done {
			state = fmt.Sprintf("exited with code %d", process.exitCode)
		}
		result := fmt.Sprintf("Process %s: %s\n", process.id, state)
		if process.errorText != "" {
			result += "Error: " + process.errorText + "\n"
		}
		output := strings.TrimSpace(process.output.String())
		if output == "" {
			output = "(no output yet)"
		}
		if len(output) > 12000 {
			output = output[len(output)-12000:]
		}
		return result + output, nil
	case "terminate":
		process := a.agentProcess(argString(args, "process_id"))
		if process == nil {
			return "", errors.New("there is no process with that id")
		}
		process.mu.Lock()
		done, cancel := process.done, process.cancel
		process.mu.Unlock()
		if done {
			return "That process has already exited.", nil
		}
		cancel()
		return "Sent the process a stop request.", nil
	case "list":
		a.agent.mu.Lock()
		a.agent.processes.mu.Lock()
		processes := make([]*managedAgentProcess, 0, len(a.agent.processes.byID))
		for _, process := range a.agent.processes.byID {
			processes = append(processes, process)
		}
		a.agent.processes.mu.Unlock()
		a.agent.mu.Unlock()
		sort.Slice(processes, func(i, j int) bool { return processes[i].startedAt.After(processes[j].startedAt) })
		var b strings.Builder
		for _, process := range processes {
			process.mu.Lock()
			state := "running"
			if process.done {
				state = fmt.Sprintf("exit %d", process.exitCode)
			}
			fmt.Fprintf(&b, "%s %s: %s\n", process.id, clipText(process.command, 180), state)
			process.mu.Unlock()
		}
		if b.Len() == 0 {
			return "No managed processes.", nil
		}
		return b.String(), nil
	default:
		return "", errors.New("action must be start, poll, terminate, or list")
	}
}
