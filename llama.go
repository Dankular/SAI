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
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sovereign AI owns model serving: it runs two private llama-server processes
// (chat and embeddings) from the runtime directory and registers them with
// OpenClaw as a standard OpenAI-compatible llama.cpp server. OpenClaw remains
// the agent: Gateway, dashboard, tools, plugins and memory search are all its.
//
//	runtime/llama/<backend>   llama.cpp release for one backend (cuda|sycl|vulkan|cpu)
//	runtime/models            GGUF weights
const (
	llamaChatPort  = 19432
	llamaEmbedPort = 19433
	chatModelID    = "agents-a1-4b"
	embedModelID   = "embeddinggemma-2"
	chatRepo       = "InternScience/Agents-A1-4B-Q4_K_M-GGUF"
	chatFile       = "Agents-A1-4B-Q4_K_M.gguf"
	embedRepo      = "ggml-org/embeddinggemma-2-GGUF"
	embedFile      = "embeddinggemma-2-Q8_0.gguf"
	chatContext    = 16384
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

var cudaVersionRE = regexp.MustCompile(`CUDA Version:\s*(\d+)\.(\d+)`)

// detectGPU reports which GPU vendors are present and the highest CUDA version
// the NVIDIA driver supports (0.0 if unknown).
func detectGPU() (nvidia, intel bool, cudaMajor, cudaMinor int) {
	out, err := exec.Command("reg.exe", "query", `HKLM\SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11cd-bfc1-08002be10318}`, "/s", "/v", "DriverDesc").Output()
	if err == nil {
		text := strings.ToLower(string(out))
		nvidia = strings.Contains(text, "nvidia")
		intel = strings.Contains(text, "intel(r) arc") || strings.Contains(text, "intel(r) iris") || strings.Contains(text, "intel(r) uhd") || strings.Contains(text, "intel(r) graphics")
	}
	if nvidia {
		smi := filepath.Join(envValue(os.Environ(), "SystemRoot"), "System32", "nvidia-smi.exe")
		if out, err := exec.Command(smi).Output(); err == nil {
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
	var ladder []string
	if runtime.GOARCH == "arm64" {
		ladder = []string{"vulkan", "cpu"}
	} else {
		nvidia, intel, major, minor := detectGPU()
		if nvidia && (major > 12 || major == 12 && minor >= 4) {
			ladder = append(ladder, "cuda")
		}
		if intel {
			ladder = append(ladder, "sycl")
		}
		ladder = append(ladder, "vulkan", "cpu")
	}
	if saved, err := os.ReadFile(filepath.Join(a.llamaRoot(), "backend.txt")); err == nil {
		if name := strings.TrimSpace(string(saved)); name != "" {
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
	response, err := a.httpGet("https://api.github.com/repos/ggml-org/llama.cpp/releases?per_page=1")
	if err != nil {
		return nil, fmt.Errorf("look up llama.cpp releases: %w", err)
	}
	defer response.Body.Close()
	var releases []ghRelease
	if err := json.NewDecoder(response.Body).Decode(&releases); err != nil || len(releases) == 0 {
		return nil, errors.New("read llama.cpp release index failed")
	}
	a.llama.mu.Lock()
	a.llama.release = &releases[0]
	a.llama.mu.Unlock()
	return &releases[0], nil
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
	response, err := a.httpGet("https://huggingface.co/api/models/" + repo + "/tree/main")
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
	if err := a.requireFreeSpace(filepath.Dir(dest), size, file); err != nil {
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
			_ = os.WriteFile(filepath.Join(a.llamaRoot(), "backend.txt"), []byte(backend+"\n"), 0600)
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
		{"chat", llamaChatPort, a.chatModel(), []string{"--alias", chatModelID, "-c", strconv.Itoa(chatContext), "--jinja"}},
		{"embeddings", llamaEmbedPort, a.embedModel(), []string{"--alias", embedModelID, "--embeddings", "-c", "2048", "-b", "2048", "-ub", "2048"}},
	}
	for _, server := range servers {
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

// registerLocalModels points OpenClaw at the local servers: chat through its
// llama.cpp provider's existing-server path, memory embeddings through the
// OpenAI-compatible remote embedding settings.
func (a *app) registerLocalModels() error {
	chatURL := fmt.Sprintf("http://127.0.0.1:%d/v1", llamaChatPort)
	embedURL := fmt.Sprintf("http://127.0.0.1:%d/v1", llamaEmbedPort)
	// `onboard --non-interactive` is avoided: on Windows it fails removing its own
	// temporary SQLite file (EBUSY) before applying anything. Declaring the
	// provider directly is equivalent for an external OpenAI-compatible server.
	chatProvider := fmt.Sprintf(`{"baseUrl":%q,"apiKey":"llama-cpp-local","api":"openai-completions","models":[{"id":%q,"name":"Agents-A1 4B Q4_K_M","reasoning":false,"input":["text"],"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0},"contextWindow":%d,"maxTokens":4096,"compat":{"supportsTools":true}}]}`, chatURL, chatModelID, chatContext)
	steps := [][]string{
		{"config", "set", "models.providers.llama-cpp", chatProvider, "--strict-json"},
		{"config", "set", "gateway.mode", "local"},
		{"models", "set", "llama-cpp/" + chatModelID},
		{"config", "set", "memory.search.provider", "openai"},
		{"config", "set", "memory.search.remote.baseUrl", embedURL},
		{"config", "set", "memory.search.remote.apiKey", "llama-cpp-local"},
		{"config", "set", "memory.search.model", embedModelID},
		{"config", "set", "memory.search.fallback", "none"},
	}
	for _, args := range steps {
		if err := a.runOpenClaw(args...); err != nil {
			return fmt.Errorf("OpenClaw command %q failed: %w", strings.Join(args[:2], " "), err)
		}
	}
	return nil
}
