package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// An install made by an earlier release is never put back through first-time setup. On start
// the app checks which pieces the current release needs, fetches only the ones that are missing
// (with the dashboard showing a small "Updating" strip instead of the setup screen), removes
// what the current release no longer uses, and then starts the assistant as usual.

// installPiece is one downloadable thing the assistant needs.
type installPiece struct {
	label    string
	present  func(a *app) bool
	fetch    func(a *app) error
	optional bool // a failure is logged and the assistant starts anyway
}

func installPieces() []installPiece {
	return []installPiece{
		{
			label:    "the workspace build userland",
			present:  func(a *app) bool { return a.workspaceUserlandReady() },
			fetch:    func(a *app) error { return a.ensureWorkspaceUserland() },
			optional: true,
		},
		{
			label:    "npx and uvx for extensions",
			present:  func(a *app) bool { return a.toolchainReady() },
			fetch:    func(a *app) error { return a.ensureToolchain() },
			optional: true,
		},
		{
			label:   "the language-model engine",
			present: func(a *app) bool { _, ok := findFile(a.llamaRoot(), "llama-server.exe"); return ok },
			fetch: func(a *app) error {
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
			label:   "the language model",
			present: func(a *app) bool { return isFile(a.chatModel()) },
			fetch: func(a *app) error {
				model := a.selectedChatModel()
				return a.ensureModel(model.Repo, model.File, a.chatModel())
			},
		},
		{
			label:    "the speed-up model",
			present:  func(a *app) bool { return a.selectedChatModel().Draft == nil || isFile(a.draftModel()) },
			fetch: func(a *app) error {
				draft := a.selectedChatModel().Draft
				return a.ensureModel(draft.Repo, draft.File, a.draftModel())
			},
			optional: true,
		},
		{
			label:   "the memory model",
			present: func(a *app) bool { return isFile(a.embedModel()) },
			fetch:   func(a *app) error { return a.ensureModel(embedRepo, embedFile, a.embedModel()) },
		},
	}
}

// missingPieces lists what an existing install lacks for this release.
func (a *app) missingPieces() []installPiece {
	var missing []installPiece
	for _, piece := range installPieces() {
		if !piece.present(a) {
			missing = append(missing, piece)
		}
	}
	return missing
}

// repairInstall brings a finished install up to date, then starts the always-on services.
func (a *app) repairInstall() {
	a.removeLegacy() // first: it frees the room the downloads below need
	missing := a.missingPieces()
	failed := ""
	if len(missing) > 0 {
		a.mu.Lock()
		a.state.Updating = true
		a.state.Error = ""
		a.state.ProgressPercent, a.state.ProgressStagePercent, a.state.ProgressDetail = 0, 0, ""
		a.logLocked(fmt.Sprintf("Updating your assistant: %d new piece(s) to fetch", len(missing)))
		_ = a.persistLocked()
		a.mu.Unlock()
		for i, piece := range missing {
			a.mu.Lock()
			a.state.Step = fmt.Sprintf("Updating your assistant: downloading %s (%d of %d)", piece.label, i+1, len(missing))
			a.state.ProgressPercent = i * 100 / len(missing)
			_ = a.persistLocked()
			a.mu.Unlock()
			if err := piece.fetch(a); err != nil {
				a.recordCommandOutput("Could not download " + piece.label + ": " + err.Error())
				if !piece.optional {
					failed = "Could not download " + piece.label + ": " + err.Error()
					break
				}
			}
			if a.ctx.Err() != nil {
				return
			}
		}
		a.mu.Lock()
		a.state.Updating = false
		a.state.ProgressPercent, a.state.ProgressStagePercent = 100, 100
		a.state.Step = "Your local assistant is ready"
		a.state.Error = failed
		if failed != "" {
			a.state.Step = "Update stopped"
		}
		_ = a.persistLocked()
		a.mu.Unlock()
	}
	if failed != "" {
		return
	}
	a.superviseInstalled()
	a.removeLegacy() // the old model goes now that its replacement is in place
}

// legacyPaths are left behind by releases that used OpenClaw and a private Node.js.
func (a *app) legacyPaths() []string {
	rt := a.runtimeDir()
	paths := []string{
		filepath.Join(rt, "node"), filepath.Join(rt, "openclaw"), filepath.Join(rt, "home"), filepath.Join(rt, "npm-cache"),
		filepath.Join(a.dataDir, "gateway.log"), filepath.Join(a.dataDir, "gateway.token"),
	}
	// the old chat model (Agents-A1) in any quantisation, and any half-finished download of it
	for _, pattern := range legacyModelGlobs {
		matches, _ := filepath.Glob(filepath.Join(a.modelsDir(), pattern))
		paths = append(paths, matches...)
	}
	return paths
}

// removeLegacy deletes what this release no longer uses. The old chat model is only removed
// once its replacement is in place.
func (a *app) removeLegacy() {
	var freed int64
	for _, path := range a.legacyPaths() {
		if strings.Contains(filepath.Base(path), "Agents-A1") && !isFile(a.chatModel()) {
			continue // the old model stays until its replacement is in place
		}
		freed += treeSize(path)
		_ = os.RemoveAll(path)
	}
	if freed > 0 {
		a.recordCommandOutput("Removed files from the old engine that are no longer used (" + formatBytes(freed) + ")")
	}
}

func treeSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}
