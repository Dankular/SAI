package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sovereign AI owns model serving: it runs two private llama-server processes
// (chat and embeddings) from the runtime directory. The agent loop in agent.go talks to the
// chat server's OpenAI-compatible API; memory.go uses the embedding server.
//
//	runtime/llama/<backend>   llama.cpp release for one backend (cuda|sycl|vulkan|cpu)
//	runtime/models            GGUF weights
const (
	llamaChatPort  = 19432
	llamaEmbedPort = 19433
	chatModelID    = "minicpm-v-4.7-1b"
	embedModelID   = "embeddinggemma-2"
	chatRepo       = "openbmb/MiniCPM-V-4.7-1B-gguf"
	chatFile       = "MiniCPM-V-4_7-1B-Q8_0.gguf"
	// DSpark draft: a small model trained to pair with the chat model. It proposes up to 15 tokens at a
	// time and the chat model verifies them, so replies come out several times faster with identical text.
	draftRepo   = "openbmb/MiniCPM-V-4.7-1B-DSpark-gguf"
	draftFile   = "dspark-MiniCPM-V-4_7-1B-Q8_0.gguf"
	embedRepo   = "ggml-org/embeddinggemma-2-GGUF"
	embedFile   = "embeddinggemma-2-Q8_0.gguf"
	chatContext = 32768 // the agent prompt is ~4k tokens; the rest is conversation and tool output
)

type llamaState struct {
	mu        sync.Mutex // serializes start/stop; held for minutes
	backendMu sync.Mutex // guards backend only; safe to take under app.mu
	backend   string
	cmds      []*exec.Cmd
	logs      []*os.File
	release   *ghRelease
}

type ghAsset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
}

type ghRelease struct {
	Tag    string    `json:"tag_name"`
	Assets []ghAsset `json:"assets"`
}

func (a *app) llamaRoot() string  { return filepath.Join(a.runtimeDir(), "llama") }
func (a *app) modelsDir() string  { return filepath.Join(a.runtimeDir(), "models") }
func (a *app) chatModel() string  { return filepath.Join(a.modelsDir(), chatFile) }
func (a *app) draftModel() string { return filepath.Join(a.modelsDir(), draftFile) }
func (a *app) embedModel() string { return filepath.Join(a.modelsDir(), embedFile) }

func (a *app) llamaBackendName() string {
	a.llama.backendMu.Lock()
	defer a.llama.backendMu.Unlock()
	return a.llama.backend
}

func (a *app) setLlamaBackend(name string) {
	a.llama.backendMu.Lock()
	a.llama.backend = name
	a.llama.backendMu.Unlock()
}

// --- GPU detection and backend ladder ---------------------------------------

var cudaVersionRE = regexp.MustCompile(`CUDA (?:UMD )?Version:\s*(\d+)\.(\d+)`)

// detectGPU reports which GPU vendors are present and the highest CUDA version
// the NVIDIA driver supports (0.0 if unknown).
func detectGPU() (nvidia, intel bool, cudaMajor, cudaMinor int) {
	system32 := filepath.Join(envValue(os.Environ(), "SystemRoot"), "System32")
	has := func(names ...string) bool {
		for _, name := range names {
			if isFile(filepath.Join(system32, name)) {
				return true
			}
		}
		return false
	}
	// Driver files are a more reliable signal than WMI or the registry, and
	// need no extra tools. A wrong guess is harmless: the backend ladder in
	// startLlama falls through to the next backend if one does not start.
	nvidia = has("nvcuda.dll", "nvidia-smi.exe")
	intel = has("ze_intel_gpu64.dll", "igc64.dll", "igdumdim64.dll", "igd10iumd64.dll")
	if nvidia {
		if out, err := exec.Command(filepath.Join(system32, "nvidia-smi.exe")).Output(); err == nil {
			if m := cudaVersionRE.FindStringSubmatch(string(out)); m != nil {
				cudaMajor, _ = strconv.Atoi(m[1])
				cudaMinor, _ = strconv.Atoi(m[2])
			}
		}
	}
	return
}

// backendCandidates returns llama.cpp backends to try, best first. The
// SOVEREIGNAI_LLAMA_BACKEND environment variable (cuda|sycl|vulkan|cpu) forces one.
func (a *app) backendCandidates() []string {
	if forced := strings.ToLower(os.Getenv("SOVEREIGNAI_LLAMA_BACKEND")); forced != "" {
		return []string{forced}
	}
	// NVIDIA -> CUDA and Intel -> SYCL (XPU), each falling back to CPU only.
	// Vulkan is used solely for GPUs that are neither (e.g. AMD).
	var ladder []string
	if runtime.GOARCH == "arm64" {
		ladder = []string{"vulkan", "cpu"}
	} else {
		nvidia, intel, major, minor := detectGPU()
		if nvidia && (major == 0 || major > 12 || major == 12 && minor >= 4) {
			ladder = append(ladder, "cuda")
		}
		if intel {
			ladder = append(ladder, "sycl")
		}
		if len(ladder) == 0 && !nvidia {
			ladder = append(ladder, "vulkan")
		}
		ladder = append(ladder, "cpu")
	}
	if saved, err := os.ReadFile(filepath.Join(a.llamaRoot(), "backend.txt")); err == nil {
		if name := strings.TrimSpace(string(saved)); name != "" && contains(ladder, name) {
			reordered := []string{name}
			for _, b := range ladder {
				if b != name {
					reordered = append(reordered, b)
				}
			}
			ladder = reordered
		}
	}
	return ladder
}

// --- llama.cpp release download ----------------------------------------------

func (a *app) latestLlamaRelease() (*ghRelease, error) {
	a.llama.mu.Lock()
	cached := a.llama.release
	a.llama.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	response, err := a.httpGet("https://api.github.com/repos/ggml-org/llama.cpp/releases?per_page=30")
	if err != nil {
		return nil, fmt.Errorf("look up llama.cpp releases: %w", err)
	}
	defer response.Body.Close()
	var releases []ghRelease
	if err := json.NewDecoder(response.Body).Decode(&releases); err != nil || len(releases) == 0 {
		return nil, errors.New("read llama.cpp release index failed")
	}
	release := pickLlamaRelease(releases)
	if release == nil {
		return nil, errors.New("none of the recent llama.cpp releases has a Windows build")
	}
	a.llama.mu.Lock()
	a.llama.release = release
	a.llama.mu.Unlock()
	return release, nil
}

// pickLlamaRelease returns the newest release that actually ships Windows binaries. The
// project also publishes releases with no binaries (a version-tagged one was newest on
// 2026-10-15), so "the latest release" is not always one we can install.
func pickLlamaRelease(releases []ghRelease) *ghRelease {
	arch := "x64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}
	for i := range releases {
		for _, asset := range releases[i].Assets {
			if strings.HasPrefix(asset.Name, "llama-") && strings.HasSuffix(asset.Name, "bin-win-cpu-"+arch+".zip") {
				return &releases[i]
			}
		}
	}
	return nil
}

// backendAssets selects the release zips needed for a backend.
func backendAssets(release *ghRelease, backend string, cudaMajor, cudaMinor int) ([]ghAsset, error) {
	arch := "x64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}
	var suffix string
	switch backend {
	case "cuda":
		suffix = fmt.Sprintf("bin-win-cuda-%d.%d-%s.zip", cudaMajor, cudaMinor, arch)
	case "sycl":
		suffix = "bin-win-sycl-" + arch + ".zip"
	case "vulkan":
		suffix = "bin-win-vulkan-" + arch + ".zip"
	case "cpu":
		suffix = "bin-win-cpu-" + arch + ".zip"
	default:
		return nil, fmt.Errorf("unknown llama.cpp backend %q", backend)
	}
	var picked []ghAsset
	for _, asset := range release.Assets {
		if strings.HasSuffix(asset.Name, suffix) {
			picked = append(picked, asset)
		}
	}
	if len(picked) == 0 {
		return nil, fmt.Errorf("llama.cpp %s has no %s build for Windows %s", release.Tag, backend, arch)
	}
	return picked, nil
}

// cudaBuildVersion picks the newest published CUDA build the driver supports.
func cudaBuildVersion(release *ghRelease, driverMajor, driverMinor int) (int, int) {
	re := regexp.MustCompile(`bin-win-cuda-(\d+)\.(\d+)-x64\.zip$`)
	bestMajor, bestMinor := 0, 0
	for _, asset := range release.Assets {
		m := re.FindStringSubmatch(asset.Name)
		if m == nil || !strings.HasPrefix(asset.Name, "llama-") {
			continue
		}
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		supported := major < driverMajor || major == driverMajor && minor <= driverMinor
		newer := major > bestMajor || major == bestMajor && minor > bestMinor
		if supported && newer {
			bestMajor, bestMinor = major, minor
		}
	}
	return bestMajor, bestMinor
}

func (a *app) requireFreeSpace(path string, need int64, what string) error {
	if free := diskFreeBytes(path); free != 0 && int64(free) < need+300<<20 {
		return fmt.Errorf("not enough free disk space for %s: need about %s, %s available on the drive holding %s", what, formatBytes(need), formatBytes(int64(free)), path)
	}
	return nil
}

// ensureBackend downloads and unpacks one llama.cpp backend, returning the
// directory containing llama-server.exe.
func (a *app) ensureBackend(backend string) (string, error) {
	dir := filepath.Join(a.llamaRoot(), backend)
	if exe, ok := findFile(dir, "llama-server.exe"); ok {
		return filepath.Dir(exe), nil
	}
	release, err := a.latestLlamaRelease()
	if err != nil {
		return "", err
	}
	_, _, driverMajor, driverMinor := detectGPU()
	if driverMajor == 0 {
		driverMajor, driverMinor = 12, 4 // version unreadable: assume the oldest published CUDA build
	}
	cudaMajor, cudaMinor := 0, 0
	if backend == "cuda" {
		if cudaMajor, cudaMinor = cudaBuildVersion(release, driverMajor, driverMinor); cudaMajor == 0 {
			return "", errors.New("the NVIDIA driver is too old for the published CUDA builds")
		}
	}
	assets, err := backendAssets(release, backend, cudaMajor, cudaMinor)
	if err != nil {
		return "", err
	}
	var total int64
	for _, asset := range assets {
		total += asset.Size
	}
	if err := a.requireFreeSpace(a.runtimeDir(), total*2, "the llama.cpp "+backend+" runtime"); err != nil {
		return "", err
	}
	staging := dir + ".partial"
	_ = os.RemoveAll(staging)
	for _, asset := range assets {
		archive := filepath.Join(a.runtimeDir(), "downloads", asset.Name)
		if err := a.downloadFile(asset.URL, archive, "llama.cpp "+release.Tag+" ("+backend+")"); err != nil {
			return "", err
		}
		if want := strings.TrimPrefix(asset.Digest, "sha256:"); want != asset.Digest && want != "" {
			got, err := fileSHA256(archive)
			if err != nil {
				return "", err
			}
			if got != strings.ToLower(want) {
				_ = os.Remove(archive)
				return "", fmt.Errorf("%s checksum mismatch", asset.Name)
			}
		}
		err := extractZipAll(archive, staging)
		_ = os.Remove(archive)
		if err != nil {
			_ = os.RemoveAll(staging)
			return "", fmt.Errorf("unpack %s: %w", asset.Name, err)
		}
	}
	if _, ok := findFile(staging, "llama-server.exe"); !ok {
		_ = os.RemoveAll(staging)
		return "", fmt.Errorf("llama.cpp %s %s build does not contain llama-server.exe", release.Tag, backend)
	}
	if err := a.retryFS("activate the llama.cpp "+backend+" runtime", func() error {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		return os.Rename(staging, dir)
	}); err != nil {
		return "", err
	}
	exe, _ := findFile(dir, "llama-server.exe")
	return filepath.Dir(exe), nil
}

func findFile(root, name string) (string, bool) {
	var found string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.EqualFold(d.Name(), name) {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	return found, found != ""
}

// extractZipAll unpacks a zip into dest preserving paths, rejecting entries
// that escape dest.
func extractZipAll(archive, dest string) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer reader.Close()
	root := filepath.Clean(dest) + string(os.PathSeparator)
	for _, file := range reader.File {
		target := filepath.Join(dest, filepath.FromSlash(file.Name))
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

// --- model weights -----------------------------------------------------------

// ensureModel downloads one GGUF from Hugging Face and verifies it against the
// repository's published SHA-256.
func (a *app) ensureModel(repo, file, dest string) error {
	dir := path.Dir(file)
	treeURL := "https://huggingface.co/api/models/" + repo + "/tree/main"
	if dir != "." {
		treeURL += "/" + dir
	}
	response, err := a.httpGet(treeURL)
	if err != nil {
		return fmt.Errorf("look up %s: %w", repo, err)
	}
	defer response.Body.Close()
	var entries []struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
		LFS  *struct {
			OID string `json:"oid"`
		} `json:"lfs"`
	}
	if err := json.NewDecoder(response.Body).Decode(&entries); err != nil {
		return fmt.Errorf("read %s file list: %w", repo, err)
	}
	var size int64
	var oid string
	for _, entry := range entries {
		if entry.Path == file {
			size = entry.Size
			if entry.LFS != nil {
				oid = strings.ToLower(entry.LFS.OID)
			}
		}
	}
	if size == 0 {
		return fmt.Errorf("%s is not in %s", file, repo)
	}
	if info, err := os.Stat(dest); err == nil && info.Size() == size {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	remaining := size
	if info, err := os.Stat(dest + ".download"); err == nil && info.Size() < size {
		remaining = size - info.Size() // a partial download is continued, not restarted
	}
	if err := a.requireFreeSpace(filepath.Dir(dest), remaining, file); err != nil {
		return err
	}
	if err := a.downloadFile("https://huggingface.co/"+repo+"/resolve/main/"+file, dest, file); err != nil {
		return err
	}
	if oid != "" {
		got, err := fileSHA256(dest)
		if err != nil {
			return err
		}
		if got != oid {
			_ = os.Remove(dest)
			return fmt.Errorf("%s checksum mismatch after download", file)
		}
	}
	return nil
}

// --- server lifecycle --------------------------------------------------------

func (a *app) llamaHealthy(port int) bool {
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/health", port), nil)
	if err != nil {
		return false
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false
	}
	response.Body.Close()
	return response.StatusCode == http.StatusOK
}

func tailFile(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > n {
		_, _ = f.Seek(-n, io.SeekEnd)
	}
	data, _ := io.ReadAll(f)
	return strings.TrimSpace(cleanCommandLine(strings.ReplaceAll(string(data), "\n", " | ")))
}

// startLlama makes sure both servers are running, trying each backend in
// order until one comes up healthy, then remembers which one worked.
func (a *app) startLlama() error {
	if a.llamaHealthy(llamaChatPort) && a.llamaHealthy(llamaEmbedPort) {
		return nil
	}
	a.llama.mu.Lock()
	defer a.llama.mu.Unlock()
	a.stopLlamaLocked()
	var failures []string
	for _, backend := range a.backendCandidates() {
		a.setStageActivity("Starting local model servers (" + backend + ")")
		dir, err := a.ensureBackendUnlocked(backend)
		if err == nil {
			err = a.launchLlama(backend, dir)
		}
		if err == nil {
			a.setLlamaBackend(backend)
			if os.Getenv("SOVEREIGNAI_LLAMA_BACKEND") == "" {
				_ = os.WriteFile(filepath.Join(a.llamaRoot(), "backend.txt"), []byte(backend+"\n"), 0600)
			}
			a.recordCommandOutput("Local model servers are running on the " + backend + " backend")
			return nil
		}
		a.stopLlamaLocked()
		failures = append(failures, backend+": "+err.Error())
		a.recordCommandOutput("llama.cpp " + backend + " backend unavailable: " + err.Error())
		if a.ctx.Err() != nil {
			break
		}
	}
	return fmt.Errorf("no llama.cpp backend could start (%s)", strings.Join(failures, "; "))
}

// ensureBackendUnlocked is ensureBackend for callers holding llama.mu;
// latestLlamaRelease takes the lock itself, so release it around the call.
func (a *app) ensureBackendUnlocked(backend string) (string, error) {
	a.llama.mu.Unlock()
	defer a.llama.mu.Lock()
	return a.ensureBackend(backend)
}

func (a *app) launchLlama(backend, dir string) error {
	servers := []struct {
		name  string
		port  int
		model string
		args  []string
	}{
		// One slot: parallel slots would split the context window between requests. The 8-bit KV cache
		// keeps the larger window within the same VRAM.
		{"chat", llamaChatPort, a.chatModel(), []string{"--alias", chatModelID, "-c", strconv.Itoa(chatContext), "--jinja", "-np", "1", "-fa", "on", "--cache-type-k", "q8_0", "--cache-type-v", "q8_0"}},
		{"embeddings", llamaEmbedPort, a.embedModel(), []string{"--alias", embedModelID, "--embeddings", "-c", "2048", "-b", "2048", "-ub", "2048"}},
	}
	for _, server := range servers {
		if server.name == "chat" && isFile(a.draftModel()) && os.Getenv("SOVEREIGNAI_NO_DRAFT") == "" {
			draft := append(append([]string{}, server.args...), "-md", a.draftModel(), "--spec-type", "draft-dspark", "--spec-draft-n-max", "15", "-ngld", "99")
			if err := a.launchLlamaServer(dir, server.name, server.port, server.model, draft); err == nil {
				a.recordCommandOutput("Speculative decoding (DSpark) is on")
				continue
			} else {
				// An older llama.cpp or a backend without support: run without the draft rather than not at all.
				a.recordCommandOutput("DSpark draft unavailable, continuing without it: " + err.Error())
				a.killLastLlama()
			}
		}
		if err := a.launchLlamaServer(dir, server.name, server.port, server.model, server.args); err != nil {
			return err
		}
	}
	return nil
}

// killLastLlama stops the most recently started server (one that failed to come up).
func (a *app) killLastLlama() {
	if n := len(a.llama.cmds); n > 0 {
		if p := a.llama.cmds[n-1].Process; p != nil {
			_ = p.Kill()
		}
		a.llama.cmds = a.llama.cmds[:n-1]
	}
	if n := len(a.llama.logs); n > 0 {
		_ = a.llama.logs[n-1].Close()
		a.llama.logs = a.llama.logs[:n-1]
	}
}

func (a *app) launchLlamaServer(dir, name string, port int, model string, extra []string) error {
	server := struct {
		name  string
		port  int
		model string
		args  []string
	}{name, port, model, extra}
	{
		logPath := filepath.Join(a.dataDir, "llama-"+server.name+".log")
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		args := append([]string{"-m", server.model, "--host", "127.0.0.1", "--port", strconv.Itoa(server.port), "-ngl", "99"}, server.args...)
		cmd := exec.CommandContext(a.ctx, filepath.Join(dir, "llama-server.exe"), args...)
		cmd.Dir = dir
		cmd.Env = prependPath(a.runtimeEnv(), dir)
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		hideWindow(cmd)
		if err := cmd.Start(); err != nil {
			logFile.Close()
			return fmt.Errorf("start %s server: %w", server.name, err)
		}
		a.llama.cmds = append(a.llama.cmds, cmd)
		a.llama.logs = append(a.llama.logs, logFile)
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		deadline := time.Now().Add(5 * time.Minute)
		for {
			if a.llamaHealthy(server.port) {
				break
			}
			select {
			case err := <-exited:
				return fmt.Errorf("%s server exited (%v): %s", server.name, err, tailFile(logPath, 600))
			case <-a.ctx.Done():
				return a.ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s server did not become ready: %s", server.name, tailFile(logPath, 600))
			}
		}
	}
	return nil
}

func (a *app) stopLlama() {
	a.llama.mu.Lock()
	defer a.llama.mu.Unlock()
	a.stopLlamaLocked()
}

func (a *app) stopLlamaLocked() {
	for _, cmd := range a.llama.cmds {
		if cmd.Process != nil {
			if runtime.GOOS == "windows" {
				_ = exec.Command("taskkill.exe", "/PID", fmt.Sprint(cmd.Process.Pid), "/T", "/F").Run()
			} else {
				_ = cmd.Process.Kill()
			}
		}
	}
	for _, f := range a.llama.logs {
		_ = f.Close()
	}
	a.llama.cmds, a.llama.logs = nil, nil
	a.setLlamaBackend("")
}

func contains(list []string, item string) bool {
	for _, v := range list {
		if v == item {
			return true
		}
	}
	return false
}
