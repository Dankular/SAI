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
	"time"
)

// The app's dependency chain lives entirely under <dataDir>/runtime and is
// resolved at run time. Nothing is installed system-wide and no system PATH,
// PowerShell profile or user environment is consulted.
//
//	runtime/node       portable Node.js (node.exe, bundled npm)
//	runtime/openclaw   private npm prefix holding the openclaw package
//	runtime/npm-cache  npm cache
//	runtime/home       OPENCLAW_HOME: OpenClaw state, models, llama.cpp runtime

const minNodeMajor = 26

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
// environment with PATH replaced by a minimal private one (the private Node,
// the openclaw .bin dir and the Windows system directories) and all tool state
// redirected into the app's data directory. The user's own PATH is deliberately
// not inherited: on machines with a long PATH, npm hands lifecycle scripts an
// empty PATH and package install scripts then fail to find `node`.
// PSModulePath is dropped so a PowerShell 7 parent cannot break Windows
// PowerShell children.
func (a *app) runtimeEnv() []string {
	env := make([]string, 0, len(os.Environ())+8)
	for _, entry := range os.Environ() {
		if key, _, ok := strings.Cut(entry, "="); ok && strings.EqualFold(key, "PSModulePath") {
			continue
		}
		env = append(env, entry)
	}
	systemRoot := envValue(env, "SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	env = setEnvValue(env, "Path", strings.Join([]string{
		a.nodeDir(),
		filepath.Join(a.openclawDir(), "node_modules", ".bin"),
		filepath.Join(systemRoot, "System32"),
		systemRoot,
		filepath.Join(systemRoot, "System32", "Wbem"),
	}, string(os.PathListSeparator)))
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

// latestNodeRelease returns the newest Node 26 release with an archive for
// this Windows architecture. OpenClaw's installer currently requires Node 26.1+
// or Node 24.16+ with a compatible SQLite build.
func (a *app) latestNodeRelease(platform string) (string, error) {
	response, err := a.httpGet("https://nodejs.org/dist/index.json")
	if err != nil {
		return "", fmt.Errorf("look up Node.js releases: %w", err)
	}
	defer response.Body.Close()
	var releases []struct {
		Version string   `json:"version"`
		Files   []string `json:"files"`
	}
	if err := json.NewDecoder(response.Body).Decode(&releases); err != nil {
		return "", fmt.Errorf("read Node.js release index: %w", err)
	}
	wantedFile := platform + "-zip"
	for _, release := range releases { // newest first
		if !strings.HasPrefix(release.Version, "v26.") || !isOpenClawNodeVersion(release.Version) {
			continue
		}
		for _, file := range release.Files {
			if file == wantedFile {
				return release.Version, nil
			}
		}
	}
	return "", fmt.Errorf("no supported Node.js 26 archive found for %s", platform)
}

func isOpenClawNodeVersion(version string) bool {
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	if len(parts) < 2 {
		return false
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	if majorErr != nil || minorErr != nil {
		return false
	}
	return (major == 24 && minor >= 16) || (major == minNodeMajor && minor >= 1) || major > minNodeMajor
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
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == file {
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
	if isFile(a.npmCLI()) && a.nodeRuntimeReady(a.nodeExe()) {
		return nil
	}
	platform, err := nodePlatform()
	if err != nil {
		return err
	}
	version, err := a.latestNodeRelease(platform)
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
	if !a.nodeRuntimeReady(filepath.Join(staging, "node.exe")) {
		_ = os.RemoveAll(staging)
		return errors.New("downloaded Node.js runtime does not meet OpenClaw's version and SQLite requirements")
	}
	if err := a.retryFS("remove the previous Node.js runtime", func() error { return os.RemoveAll(a.nodeDir()) }); err != nil {
		return err
	}
	if err := a.retryFS("activate the Node.js runtime", func() error { return os.Rename(staging, a.nodeDir()) }); err != nil {
		return err
	}
	a.recordCommandOutput("Node.js " + version + " ready in the private runtime")
	return nil
}

// retryFS retries a filesystem operation for up to ~30s. On Windows, antivirus
// or the indexer commonly holds handles on freshly extracted or just-executed
// binaries, which makes remove/rename fail transiently with "Access is denied".
func (a *app) retryFS(what string, op func() error) error {
	var err error
	for attempt := 0; attempt < 30; attempt++ {
		if err = op(); err == nil {
			return nil
		}
		select {
		case <-a.ctx.Done():
			return a.ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("%s (is antivirus or another process holding files in %s?): %w", what, a.runtimeDir(), err)
}

func (a *app) nodeRuntimeReady(nodeExe string) bool {
	versionOutput, err := exec.CommandContext(a.ctx, nodeExe, "--version").Output()
	if err != nil || !isOpenClawNodeVersion(strings.TrimSpace(string(versionOutput))) {
		return false
	}
	const probeScript = `const result={available:false,text:false,blob:false,json:false};let db;try{const{DatabaseSync}=require("node:sqlite");db=new DatabaseSync(":memory:");result.available=true;const text="a\u0000b\u0000";const bytes=Buffer.from(text,"utf8");const json=JSON.stringify({value:text});db.exec("CREATE TABLE probe (text_value TEXT, blob_value BLOB, json_value TEXT)");db.prepare("INSERT INTO probe VALUES (?, ?, ?)").run(text,bytes,json);const row=db.prepare("SELECT text_value, blob_value, json_value FROM probe").get();result.text=typeof row?.text_value==="string"&&row.text_value.length===text.length&&Buffer.from(row.text_value,"utf8").equals(bytes);result.blob=row?.blob_value instanceof Uint8Array&&Buffer.from(row.blob_value).equals(bytes);result.json=row?.json_value===json&&JSON.parse(row.json_value).value===text;}catch(error){result.error=error instanceof Error?error.message:String(error);}finally{db?.close();}process.stdout.write(JSON.stringify(result));`
	probeOutput, err := exec.CommandContext(a.ctx, nodeExe, "-e", probeScript).Output()
	if err != nil {
		return false
	}
	var probe struct {
		Available bool   `json:"available"`
		Text      bool   `json:"text"`
		Blob      bool   `json:"blob"`
		JSON      bool   `json:"json"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(probeOutput, &probe); err != nil {
		return false
	}
	return probe.Available && probe.Text && probe.Blob && probe.JSON && probe.Error == ""
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
	a.setStageActivity("Installing OpenClaw and its dependencies")
	pathCheck := exec.CommandContext(a.ctx, "cmd.exe", "/d", "/s", "/c", "node --version")
	pathCheck.Env = a.runtimeEnv()
	pathCheck.Dir = a.runtimeDir()
	pathOutput, err := pathCheck.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Windows package scripts cannot resolve the private Node.js runtime: %s: %w", strings.TrimSpace(string(pathOutput)), err)
	}
	if !isOpenClawNodeVersion(strings.TrimSpace(string(pathOutput))) {
		return fmt.Errorf("Windows package scripts resolved an unsupported Node.js version: %s", strings.TrimSpace(string(pathOutput)))
	}
	a.recordCommandOutput("Windows package-script Node.js lookup passed: " + strings.TrimSpace(string(pathOutput)))
	if err := os.MkdirAll(a.openclawDir(), 0700); err != nil {
		return err
	}
	cmd := a.nodeCommand(a.npmCLI(), "install", "--prefix", a.openclawDir(), "--no-fund", "--no-audit", "--loglevel", "http", "openclaw@latest")
	if err := a.runCommand(cmd); err != nil {
		return fmt.Errorf("npm install openclaw failed: %w", err)
	}
	_, err = a.openclawEntry()
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
