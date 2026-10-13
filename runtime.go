package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// The app's dependency chain lives entirely under <dataDir>/runtime and is
// resolved at run time. Nothing is installed system-wide and no system PATH,
// PowerShell profile or user environment is consulted.
//
//	runtime/node       portable Node.js (node.exe, bundled npm)
//	runtime/openclaw   private npm prefix holding the openclaw package
//	runtime/npm-cache  npm cache
//	runtime/home       OPENCLAW_HOME: OpenClaw state, models, llama.cpp runtime

const minNodeMajor = 22

func (a *app) runtimeDir() string  { return filepath.Join(a.dataDir, "runtime") }
func (a *app) nodeDir() string     { return filepath.Join(a.runtimeDir(), "node") }
func (a *app) nodeExe() string     { return filepath.Join(a.nodeDir(), "node.exe") }
func (a *app) openclawDir() string { return filepath.Join(a.runtimeDir(), "openclaw") }

func (a *app) npmCLI() string {
	return filepath.Join(a.nodeDir(), "node_modules", "npm", "bin", "npm-cli.js")
}

// openclawEntry resolves the openclaw CLI script from the installed package's
// package.json "bin" field.
func (a *app) openclawEntry() (string, error) {
	pkgDir := filepath.Join(a.openclawDir(), "node_modules", "openclaw")
	data, err := os.ReadFile(filepath.Join(pkgDir, "package.json"))
	if err != nil {
		return "", fmt.Errorf("OpenClaw is not installed in the private runtime: %w", err)
	}
	var pkg struct {
		Bin json.RawMessage `json:"bin"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", err
	}
	var single string
	if json.Unmarshal(pkg.Bin, &single) == nil && single != "" {
		return filepath.Join(pkgDir, filepath.FromSlash(single)), nil
	}
	var named map[string]string
	if json.Unmarshal(pkg.Bin, &named) == nil {
		if rel := named["openclaw"]; rel != "" {
			return filepath.Join(pkgDir, filepath.FromSlash(rel)), nil
		}
	}
	return "", errors.New("openclaw package.json has no openclaw bin entry")
}

// runtimeEnv is the environment for every child process: the inherited
// environment with the private Node first on PATH and all tool state redirected
// into the app's data directory. PSModulePath is dropped so a PowerShell 7
// parent cannot break Windows PowerShell children.
func (a *app) runtimeEnv() []string {
	env := make([]string, 0, len(os.Environ())+8)
	for _, entry := range os.Environ() {
		if key, _, ok := strings.Cut(entry, "="); ok && strings.EqualFold(key, "PSModulePath") {
			continue
		}
		env = append(env, entry)
	}
	env = prependPath(env, filepath.Join(a.openclawDir(), "node_modules", ".bin"))
	env = prependPath(env, a.nodeDir())
	env = setEnvValue(env, "OPENCLAW_HOME", filepath.Join(a.runtimeDir(), "home"))
	env = setEnvValue(env, "npm_config_cache", filepath.Join(a.runtimeDir(), "npm-cache"))
	env = setEnvValue(env, "npm_config_prefix", a.openclawDir())
	env = setEnvValue(env, "npm_config_update_notifier", "false")
	env = setEnvValue(env, "npm_config_fund", "false")
	env = setEnvValue(env, "npm_config_audit", "false")
	return env
}

func nodePlatform() (string, error) {
	if runtime.GOOS != "windows" {
		return "", errors.New("the private runtime currently supports Windows")
	}
	switch runtime.GOARCH {
	case "amd64":
		return "win-x64", nil
	case "arm64":
		return "win-arm64", nil
	}
	return "", fmt.Errorf("unsupported architecture %s", runtime.GOARCH)
}

func (a *app) httpGet(url string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(a.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("server returned %s", response.Status)
	}
	return response, nil
}

// latestNodeLTS returns the newest LTS release (e.g. "v22.11.0") whose major
// version is at least minNodeMajor.
func (a *app) latestNodeLTS() (string, error) {
	response, err := a.httpGet("https://nodejs.org/dist/index.json")
	if err != nil {
		return "", fmt.Errorf("look up Node.js releases: %w", err)
	}
	defer response.Body.Close()
	var releases []struct {
		Version string `json:"version"`
		LTS     any    `json:"lts"`
	}
	if err := json.NewDecoder(response.Body).Decode(&releases); err != nil {
		return "", fmt.Errorf("read Node.js release index: %w", err)
	}
	for _, release := range releases { // newest first
		if lts, ok := release.LTS.(string); !ok || lts == "" {
			continue
		}
		major, _, _ := strings.Cut(strings.TrimPrefix(release.Version, "v"), ".")
		if n, err := strconv.Atoi(major); err == nil && n >= minNodeMajor {
			return release.Version, nil
		}
	}
	return "", errors.New("no suitable Node.js LTS release found")
}

func (a *app) expectedSHA256(version, file string) (string, error) {
	response, err := a.httpGet("https://nodejs.org/dist/" + version + "/SHASUMS256.txt")
	if err != nil {
		return "", fmt.Errorf("download Node.js checksums: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == file {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("no checksum published for %s", file)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ensureNode downloads, verifies and unpacks a portable Node.js if one is not
// already present in the private runtime.
func (a *app) ensureNode() error {
	if isFile(a.nodeExe()) && isFile(a.npmCLI()) {
		return nil
	}
	platform, err := nodePlatform()
	if err != nil {
		return err
	}
	version, err := a.latestNodeLTS()
	if err != nil {
		return err
	}
	archiveName := fmt.Sprintf("node-%s-%s.zip", version, platform)
	want, err := a.expectedSHA256(version, archiveName)
	if err != nil {
		return err
	}
	archive := filepath.Join(a.runtimeDir(), "downloads", archiveName)
	if err := a.downloadFile("https://nodejs.org/dist/"+version+"/"+archiveName, archive, "Node.js "+version); err != nil {
		return err
	}
	defer os.Remove(archive)
	got, err := fileSHA256(archive)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("Node.js archive checksum mismatch (got %s, want %s)", got, want)
	}
	staging := a.nodeDir() + ".partial"
	_ = os.RemoveAll(staging)
	if err := extractZipStrip(archive, staging); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("unpack Node.js: %w", err)
	}
	_ = os.RemoveAll(a.nodeDir())
	if err := os.Rename(staging, a.nodeDir()); err != nil {
		return err
	}
	a.recordCommandOutput("Node.js " + version + " ready in the private runtime")
	return nil
}

// extractZipStrip unpacks a zip into dest, dropping the single top-level
// directory and rejecting entries that escape dest.
func extractZipStrip(archive, dest string) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer reader.Close()
	root := filepath.Clean(dest) + string(os.PathSeparator)
	for _, file := range reader.File {
		_, rel, found := strings.Cut(file.Name, "/")
		if !found || rel == "" {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if !strings.HasPrefix(target, root) {
			return fmt.Errorf("unsafe path in archive: %s", file.Name)
		}
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0700); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		if err := copyZipFile(file, target); err != nil {
			return err
		}
	}
	return nil
}

func copyZipFile(file *zip.File, target string) error {
	src, err := file.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

// ensureOpenClaw installs the openclaw npm package into the private prefix.
func (a *app) ensureOpenClaw() error {
	if _, err := a.openclawEntry(); err == nil {
		return nil
	}
	if err := os.MkdirAll(a.openclawDir(), 0700); err != nil {
		return err
	}
	cmd := a.nodeCommand(a.npmCLI(), "install", "--prefix", a.openclawDir(), "--no-fund", "--no-audit", "--loglevel", "http", "openclaw@latest")
	if err := a.runCommand(cmd); err != nil {
		return fmt.Errorf("npm install openclaw failed: %w", err)
	}
	_, err := a.openclawEntry()
	return err
}

// nodeCommand builds a command that runs a script with the private Node.js.
func (a *app) nodeCommand(script string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(a.ctx, a.nodeExe(), append([]string{script}, args...)...)
	cmd.Env = a.runtimeEnv()
	cmd.Dir = a.runtimeDir()
	return cmd
}

// openclawCommand builds a command that runs the private OpenClaw CLI under
// the a1 profile.
func (a *app) openclawCommand(args ...string) (*exec.Cmd, error) {
	entry, err := a.openclawEntry()
	if err != nil {
		return nil, err
	}
	return a.nodeCommand(entry, append([]string{"--profile", "a1"}, args...)...), nil
}
