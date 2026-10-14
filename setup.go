package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// setupStage is one step of first-run setup. The list below is the single source of
// truth: the dashboard renders its progress list from the labels the server sends,
// so the UI and the installer cannot drift apart.
type setupStage struct {
	label     string // plain-language name shown in the dashboard
	step      string // what is happening, shown while it runs
	failure   string // prefix for the error if it fails
	resumable bool   // one-time install/download: skipped on retry once it has succeeded
	run       func(a *app) error
}

// Stages 1-7 install and download; they are resumable, so a retry skips what already
// succeeded. Stages 8 onward start things and depend on live state, so they run on
// every pass (and are quick).
func setupStages() []setupStage {
	return []setupStage{
		{
			label: "Get the assistant engine ready", step: "Installing a private Node.js runtime and OpenClaw",
			failure: "OpenClaw install failed", resumable: true,
			run: func(a *app) error {
				if err := os.MkdirAll(a.runtimeDir(), 0700); err != nil {
					return err
				}
				if err := a.ensureNode(); err != nil {
					return fmt.Errorf("private Node.js runtime install failed: %w", err)
				}
				return a.ensureOpenClaw()
			},
		},
		{
			label: "Set up your workspace", step: "Preparing the OpenClaw workspace",
			failure: "preparing the workspace failed", resumable: true,
			run: func(a *app) error {
				if err := a.runOpenClaw("config", "set", "agents.defaults.workspace", a.workspace); err != nil {
					return fmt.Errorf("setting the OpenClaw workspace failed: %w", err)
				}
				// Let OpenClaw lay down its own workspace files (its conventions in AGENTS.md and so on).
				// Our persona is layered on top of them in a later stage.
				if err := a.runOpenClaw("setup", "--baseline"); err != nil {
					a.recordCommandOutput("OpenClaw could not create its baseline workspace files: " + err.Error())
				}
				// OpenClaw ships several model providers that are on by default. This assistant
				// runs its own local model, so the Ollama provider only adds start-up work.
				if err := a.runOpenClaw("plugins", "disable", "ollama"); err != nil {
					a.recordCommandOutput("Could not turn off the unused Ollama provider: " + err.Error())
				}
				return nil
			},
		},
		{
			label: "Add the assistant's tools", step: "Installing the OpenClaw llama.cpp provider",
			failure: "installing the local model provider failed", resumable: true,
			run: func(a *app) error {
				return a.runOpenClaw("plugins", "install", "clawhub:@openclaw/llama-cpp-provider", "--force")
			},
		},
		{
			label: "Prepare for your graphics card", step: "Downloading the llama.cpp runtime for this GPU",
			failure: "llama.cpp runtime setup failed", resumable: true,
			run: func(a *app) error {
				var failures []string
				for _, backend := range a.backendCandidates() {
					if _, err := a.ensureBackend(backend); err != nil {
						failures = append(failures, backend+": "+err.Error())
						continue
					}
					return nil
				}
				return errors.New(strings.Join(failures, "; "))
			},
		},
		{
			label: "Download the assistant's brain", step: "Downloading Agents-A1 4B from Hugging Face",
			failure: "downloading Agents-A1 4B failed", resumable: true,
			run: func(a *app) error { return a.ensureModel(chatRepo, chatFile, a.chatModel()) },
		},
		{
			label: "Download its memory helper", step: "Downloading the local memory embedding model",
			failure: "downloading the embedding model failed", resumable: true,
			run: func(a *app) error { return a.ensureModel(embedRepo, embedFile, a.embedModel()) },
		},
		{
			label: "Download its voice and hearing", step: "Downloading the voice, turn detection and speech recognition",
			failure: "downloading the voice and listening models failed", resumable: true,
			run: func(a *app) error { return a.ensureVoiceAssets() },
		},
		{
			label: "Start the assistant's brain and voice", step: "Starting the local model servers and connecting OpenClaw",
			failure: "starting the local models failed",
			run: func(a *app) error {
				if err := a.startLlama(); err != nil {
					return err
				}
				if err := a.registerLocalModels(); err != nil {
					return err
				}
				if err := a.startAudio(); err != nil {
					return fmt.Errorf("the assistant's voice could not start: %w", err)
				}
				return nil
			},
		},
		{
			label: "Teach it your preferences", step: "Indexing the local profile and memory files",
			failure: "memory indexing failed",
			run:     func(a *app) error { return a.runOpenClaw("memory", "index", "--force") },
		},
		{
			label: "Save your assistant's personality", step: "Saving the assistant persona",
			failure: "saving persona files failed",
			run:     func(a *app) error { return a.writePersona(a.profile()) },
		},
		{
			label: "Wake up your assistant", step: "Starting the OpenClaw Gateway",
			run: func(a *app) error {
				if err := a.startGateway(); err != nil {
					return err
				}
				// Make sure the Gateway survives startup rather than exiting on a config error.
				for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
					a.mu.Lock()
					alive := a.gateway != nil
					a.mu.Unlock()
					if !alive {
						return fmt.Errorf("the OpenClaw Gateway exited during startup: %s", tailFile(filepath.Join(a.dataDir, "gateway.log"), 800))
					}
				}
				return nil
			},
		},
	}
}

func stageLabels() []string {
	stages := setupStages()
	labels := make([]string, len(stages))
	for i, s := range stages {
		labels[i] = s.label
	}
	return labels
}

// lastResumableStage is the highest stage number that is a one-time install or download.
func lastResumableStage() int {
	last := 0
	for i, s := range setupStages() {
		if s.resumable {
			last = i + 1
		}
	}
	return last
}

func (a *app) bootstrap() {
	defer func() {
		a.mu.Lock()
		a.state.SetupRunning = false
		_ = a.persistLocked()
		a.mu.Unlock()
	}()
	if runtime.GOOS != "windows" {
		a.fail(errors.New("the automatic OpenClaw installer currently supports Windows"))
		return
	}
	if err := os.MkdirAll(a.workspace, 0700); err != nil {
		a.fail(err)
		return
	}
	stages := setupStages()
	for i, stage := range stages {
		stage := stage
		if err := a.runSetupStage(i+1, len(stages), stage.step, stage.resumable, func() error { return stage.run(a) }); err != nil {
			if stage.failure != "" {
				err = fmt.Errorf("%s: %w", stage.failure, err)
			}
			a.fail(err)
			return
		}
	}
	a.cleanupInstallLeftovers()
	a.mu.Lock()
	a.state.SetupComplete = true
	a.state.Step = "Your local assistant is ready"
	a.state.ProgressPercent = 100
	a.state.ProgressStagePercent = 100
	a.state.ProgressStep = len(stages)
	a.state.ProgressTotal = len(stages)
	a.state.ProgressDetail = "All components are ready"
	a.state.Error = ""
	a.logLocked("Setup completed")
	_ = a.persistLocked()
	a.mu.Unlock()
}

// runSetupStage runs one stage, skipping a resumable one that already succeeded.
func (a *app) runSetupStage(index, total int, name string, resumable bool, action func() error) error {
	a.mu.Lock()
	done := resumable && index <= a.state.CompletedStage
	a.mu.Unlock()
	if done {
		a.mu.Lock()
		a.logLocked("Already done, skipping: " + name)
		a.mu.Unlock()
		a.completeSetupStage(index, total)
		return nil
	}
	a.setSetupStage(index, total, name)
	if err := action(); err != nil {
		return err
	}
	if resumable {
		a.mu.Lock()
		a.state.CompletedStage = index
		a.mu.Unlock()
	}
	a.completeSetupStage(index, total)
	return nil
}

// cleanupInstallLeftovers removes caches and half-finished downloads that nothing
// needs once setup has succeeded (the npm cache alone is several hundred MB).
func (a *app) cleanupInstallLeftovers() {
	for _, path := range []string{
		filepath.Join(a.runtimeDir(), "npm-cache"),
		filepath.Join(a.runtimeDir(), "downloads"),
		filepath.Join(a.audioRoot(), "downloads"),
		filepath.Join(a.dataDir, "tmp"),
	} {
		_ = os.RemoveAll(path)
	}
	for _, root := range []string{a.runtimeDir(), a.llamaRoot(), a.audioRoot()} {
		matches, _ := filepath.Glob(filepath.Join(root, "*.partial"))
		for _, m := range matches {
			_ = os.RemoveAll(m)
		}
	}
}
