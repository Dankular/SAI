package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Most MCP servers start with npx or uvx. Rather than ask the user to install Node.js or uv, the app
// carries its own: portable builds, pinned by version and SHA-256, unpacked under <data>/runtime/tools.
// Nothing is installed system-wide and the user's own PATH is never consulted. uvx fetches its own
// managed Python on demand, into the same folder.
//
//	runtime/tools/node    node.exe, npm.cmd, npx.cmd
//	runtime/tools/uv      uv.exe, uvx.exe
//	runtime/tools/cache   npm, uv and Python caches for extensions

const (
	nodeVersion = "24.22.0"
	uvVersion   = "0.13.1"
)

type toolchainPiece struct {
	name, url, sha256 string
	flatten           bool     // the archive wraps everything in one folder
	needs             []string // files that must exist once it is unpacked
}

// toolchainPieces is a variable so tests can point it at local archives.
var toolchainPieces = []toolchainPiece{
	{
		name: "node", url: "https://nodejs.org/dist/v" + nodeVersion + "/node-v" + nodeVersion + "-win-x64.zip",
		sha256: "f9683a19877e12a9b167305c8db71040d812876359de21f0d2e97c3f321f80c3", flatten: true,
		needs: []string{"node.exe", "npx.cmd", "npm.cmd"},
	},
	{
		name: "uv", url: "https://github.com/astral-sh/uv/releases/download/" + uvVersion + "/uv-x86_64-pc-windows-msvc.zip",
		sha256: "3b2661a463a5baea7f5076ea2b25ab71d77f36a6c0d809e5350651792f80a672",
		needs:  []string{"uv.exe", "uvx.exe"},
	},
}

var (
	toolchainSupported = runtime.GOOS == "windows"
	toolchainMu        sync.Mutex
)

func (a *app) toolsDir() string                { return filepath.Join(a.runtimeDir(), "tools") }
func (a *app) toolchainDir(name string) string { return filepath.Join(a.toolsDir(), name) }

func (a *app) toolchainPieceReady(p toolchainPiece) bool {
	for _, file := range p.needs {
		if !isFile(filepath.Join(a.toolchainDir(p.name), file)) {
			return false
		}
	}
	marker, err := os.ReadFile(filepath.Join(a.toolchainDir(p.name), ".pinned"))
	return err == nil && strings.TrimSpace(string(marker)) == p.sha256
}

func (a *app) toolchainReady() bool {
	if !toolchainSupported {
		return true
	}
	for _, p := range toolchainPieces {
		if !a.toolchainPieceReady(p) {
			return false
		}
	}
	return true
}

// ensureToolchain installs whichever of Node.js and uv is missing or out of date.
func (a *app) ensureToolchain() error {
	if !toolchainSupported {
		return nil
	}
	toolchainMu.Lock()
	defer toolchainMu.Unlock()
	for _, p := range toolchainPieces {
		if a.toolchainPieceReady(p) {
			continue
		}
		if err := a.installToolchainPiece(p); err != nil {
			return fmt.Errorf("install %s for extensions: %w", p.name, err)
		}
	}
	return os.MkdirAll(filepath.Join(a.toolsDir(), "cache"), 0700)
}

func (a *app) installToolchainPiece(p toolchainPiece) error {
	if err := a.requireFreeSpace(a.runtimeDir(), 600<<20, p.name+" for extensions"); err != nil {
		return err
	}
	archive := filepath.Join(a.runtimeDir(), "downloads", p.name+"-"+p.sha256[:12]+".zip")
	if got, err := fileSHA256(archive); err != nil || got != p.sha256 {
		_ = os.Remove(archive)
		if err := a.downloadFile(p.url, archive, p.name); err != nil {
			return err
		}
	}
	got, err := fileSHA256(archive)
	if err != nil {
		return err
	}
	if got != p.sha256 {
		_ = os.Remove(archive)
		return fmt.Errorf("the %s download failed its SHA-256 check", p.name)
	}
	staging := filepath.Join(a.toolsDir(), "."+p.name+"-new")
	_ = os.RemoveAll(staging)
	if err := extractZipAll(archive, staging); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	root := staging
	if p.flatten {
		entries, err := os.ReadDir(staging)
		if err != nil || len(entries) != 1 || !entries[0].IsDir() {
			_ = os.RemoveAll(staging)
			return errors.New("the archive has an unexpected layout")
		}
		root = filepath.Join(staging, entries[0].Name())
	}
	for _, file := range p.needs {
		if !isFile(filepath.Join(root, file)) {
			_ = os.RemoveAll(staging)
			return fmt.Errorf("the archive is missing %s", file)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".pinned"), []byte(p.sha256+"\n"), 0600); err != nil {
		return err
	}
	final := a.toolchainDir(p.name)
	if err := a.retryFS("replace "+p.name, func() error {
		if err := os.RemoveAll(final); err != nil {
			return err
		}
		return os.Rename(root, final)
	}); err != nil {
		return err
	}
	_ = os.RemoveAll(staging)
	_ = os.Remove(archive)
	return nil
}

// toolchainPathDirs are the folders that go on the front of PATH for extensions and the shell tool.
func (a *app) toolchainPathDirs() []string {
	if !toolchainSupported {
		return nil
	}
	var dirs []string
	for _, p := range toolchainPieces {
		if a.toolchainPieceReady(p) {
			dirs = append(dirs, a.toolchainDir(p.name))
		}
	}
	return dirs
}

// withToolchain puts npx, node and uvx on an environment's PATH and keeps their caches (and uv's
// managed Python) inside the app's folder.
func (a *app) withToolchain(env []string) []string {
	dirs := a.toolchainPathDirs()
	if len(dirs) == 0 {
		return env
	}
	path := envValue(env, "PATH")
	env = setEnvValue(env, "PATH", strings.Join(append(dirs, path), string(os.PathListSeparator)))
	cache := filepath.Join(a.toolsDir(), "cache")
	for key, value := range map[string]string{
		"npm_config_cache":           filepath.Join(cache, "npm"),
		"npm_config_prefix":          filepath.Join(cache, "npm-global"),
		"npm_config_update_notifier": "false",
		"npm_config_fund":            "false",
		"UV_CACHE_DIR":               filepath.Join(cache, "uv"),
		"UV_TOOL_DIR":                filepath.Join(cache, "uv-tools"),
		"UV_PYTHON_INSTALL_DIR":      filepath.Join(cache, "python"),
		"UV_PYTHON_PREFERENCE":       "only-managed",
		"UV_NO_PROGRESS":             "1",
	} {
		env = setEnvValue(env, key, value)
	}
	return env
}

var toolchainCommands = map[string]string{
	"npx": "node", "npm": "node", "node": "node", "uvx": "uv", "uv": "uv", "uvw": "uv",
}

// toolchainCommand reports which bundled piece a server command belongs to, if any.
func toolchainCommand(command string) (piece string, ok bool) {
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(command, "\\", "/")))
	base = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(base, ".exe"), ".cmd"), ".bat")
	if strings.ContainsAny(command, `/\`) && filepath.IsAbs(command) {
		return "", false // an explicit path means the user chose a particular program
	}
	piece, ok = toolchainCommands[base]
	return piece, ok
}

// resolveExtensionCommand turns "npx" into the bundled npx.cmd, installing the bundle first if this
// install predates it. Other commands pass through untouched.
func (a *app) resolveExtensionCommand(command string) (string, error) {
	piece, ok := toolchainCommand(command)
	if !ok || !toolchainSupported {
		return command, nil
	}
	if err := a.ensureToolchain(); err != nil {
		return "", err
	}
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(command, "\\", "/")))
	base = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(base, ".exe"), ".cmd"), ".bat")
	ext := ".exe"
	if base == "npx" || base == "npm" {
		ext = ".cmd"
	}
	return filepath.Join(a.toolchainDir(piece), base+ext), nil
}
