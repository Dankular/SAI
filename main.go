package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var webFiles embed.FS

const productName = "Sovereign AI"

type Profile struct {
	UserName   string `json:"userName"`
	AgentName  string `json:"agentName"`
	Purpose    string `json:"purpose"`
	ReplyStyle string `json:"replyStyle"`
}

type Snapshot struct {
	Profile              Profile  `json:"profile"`
	SetupComplete        bool     `json:"setupComplete"`
	SetupRunning         bool     `json:"setupRunning"`
	ProgressPercent      int      `json:"progressPercent"`
	ProgressStagePercent int      `json:"progressStagePercent"`
	ProgressStep         int      `json:"progressStep"`
	ProgressTotal        int      `json:"progressTotal"`
	ProgressDetail       string   `json:"progressDetail,omitempty"`
	GatewayRunning       bool     `json:"gatewayRunning"`
	MemoryBackend        string   `json:"memoryBackend"`
	MemoryRunning        bool     `json:"memoryRunning"`
	Step                 string   `json:"step"`
	Error                string   `json:"error,omitempty"`
	RecentLogs           []string `json:"recentLogs"`
}

type app struct {
	mu               sync.Mutex
	state            Snapshot
	statePath        string
	dataDir          string
	workspace        string
	serverURL        string
	gateway          *exec.Cmd
	gatewayLog       *os.File
	ctx              context.Context
	cancel           context.CancelFunc
	resumeSetup      bool
	stagePercent     int
	lastProgressSave time.Time
}

func main() {
	dataDir, err := sovereignAIDataDir()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &app{
		dataDir:   dataDir,
		workspace: filepath.Join(dataDir, "workspace"),
		statePath: filepath.Join(dataDir, "state.json"),
		ctx:       ctx,
		cancel:    cancel,
	}
	if err := a.load(); err != nil {
		log.Fatal(err)
	}
	resumeSetup := a.resumeSetup

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	a.serverURL = "http://" + listener.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", a.handleState)
	mux.HandleFunc("/api/setup", a.handleSetup)
	mux.HandleFunc("/api/install", a.handleInstall)
	mux.HandleFunc("/api/start", a.handleStart)
	mux.HandleFunc("/api/stop", a.handleStop)
	mux.HandleFunc("/api/openclaw", a.handleOpenClaw)
	mux.HandleFunc("/api/workspace", a.handleWorkspace)
	mux.HandleFunc("/api/memory", a.handleMemory)
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/", http.FileServer(http.FS(static)))

	server := &http.Server{
		Handler:           a.localOnly(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	signalCtx, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopSignal()
	go openBrowser(a.serverURL)
	log.Printf("%s is available at %s", productName, a.serverURL)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	if resumeSetup {
		a.startSetup()
	}
	select {
	case <-signalCtx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("%s web server stopped: %v", productName, err)
		}
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 4*time.Second)
	_ = server.Shutdown(shutdownCtx)
	shutdownCancel()
	a.cancel()
	a.stopGateway()
	if a.gatewayLog != nil {
		_ = a.gatewayLog.Close()
	}
}

func sovereignAIDataDir() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		var err error
		base, err = os.UserConfigDir()
		if err != nil {
			return "", err
		}
	}
	dataDir := filepath.Join(base, "SovereignAI")
	legacyDir := filepath.Join(base, "A1")
	if _, err := os.Stat(dataDir); errors.Is(err, os.ErrNotExist) {
		if _, legacyErr := os.Stat(legacyDir); legacyErr == nil {
			if err := os.Rename(legacyDir, dataDir); err != nil {
				return "", fmt.Errorf("move existing Sovereign AI data: %w", err)
			}
		}
	}
	return dataDir, nil
}

func (a *app) load() error {
	a.state = Snapshot{RecentLogs: []string{}}
	data, err := os.ReadFile(a.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &a.state); err != nil {
		return fmt.Errorf("read %s state: %w", productName, err)
	}
	a.resumeSetup = a.state.SetupRunning || (a.state.Profile.UserName != "" && a.state.Profile.AgentName != "" && a.state.Profile.Purpose != "" && !a.state.SetupComplete && a.state.Error == "")
	a.state.SetupRunning = false
	a.state.GatewayRunning = false
	a.state.MemoryRunning = false
	if a.state.MemoryBackend == "" {
		a.state.MemoryBackend = "memory-core"
	}
	return nil
}

func (a *app) persistLocked() error {
	data, err := json.MarshalIndent(a.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, a.statePath)
}

func (a *app) localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/style.css" && r.URL.Path != "/app.js" && r.URL.Path != "/favicon.ico" && !strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		if host, _, err := net.SplitHostPort(r.Host); err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			http.Error(w, "loopback requests only", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			origin, err := url.Parse(r.Header.Get("Origin"))
			if err != nil || origin.Scheme != "http" || origin.Host != r.Host || !strings.HasPrefix(origin.Hostname(), "127.") && origin.Hostname() != "localhost" && origin.Hostname() != "[::1]" {
				http.Error(w, "request origin rejected", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (a *app) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	writeJSON(w, a.state)
}

func (a *app) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	var p Profile
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "invalid setup details", http.StatusBadRequest)
		return
	}
	p.UserName = cleanField(p.UserName, 80)
	p.AgentName = cleanField(p.AgentName, 80)
	p.Purpose = cleanField(p.Purpose, 1200)
	p.ReplyStyle = cleanField(p.ReplyStyle, 40)
	if p.UserName == "" || p.AgentName == "" || p.Purpose == "" {
		http.Error(w, "name, AI name and intended use are required", http.StatusBadRequest)
		return
	}
	if p.ReplyStyle != "concise" && p.ReplyStyle != "balanced" && p.ReplyStyle != "detailed" {
		p.ReplyStyle = "balanced"
	}
	if err := os.MkdirAll(filepath.Join(a.workspace, "memory"), 0700); err != nil {
		http.Error(w, "could not create the private Sovereign AI workspace", http.StatusInternalServerError)
		return
	}
	if err := a.writePersona(p); err != nil {
		http.Error(w, "could not save the assistant profile", http.StatusInternalServerError)
		return
	}
	a.mu.Lock()
	a.state.Profile = p
	if !a.state.SetupComplete {
		a.state.Step = "Profile saved; starting automatic setup"
	}
	startSetup := !a.state.SetupComplete && !a.state.SetupRunning
	if startSetup {
		a.beginSetupLocked()
	}
	err := a.persistLocked()
	a.mu.Unlock()
	if err != nil {
		http.Error(w, "could not save Sovereign AI state", http.StatusInternalServerError)
		return
	}
	if startSetup {
		go a.bootstrap()
	}
	writeJSON(w, map[string]any{"ok": true, "setupRunning": startSetup})
}

func (a *app) writePersona(p Profile) error {
	if err := os.MkdirAll(filepath.Join(a.workspace, "memory"), 0700); err != nil {
		return err
	}
	id := fmt.Sprintf("# Identity\n\n- Name: %s\n- Role: personal AI assistant\n", mdLine(p.AgentName))
	soul := fmt.Sprintf("# %s\n\nYou are %s, a personal AI assistant.\n\n## Purpose\n%s\n\n## Reply style\n%s\n\nBe honest about uncertainty. Ask before taking consequential actions. Keep the user's private information within this local workspace unless the user explicitly chooses an external service.\n", mdLine(p.AgentName), mdLine(p.AgentName), mdParagraph(p.Purpose), replyPreference(p.ReplyStyle))
	user := fmt.Sprintf("# User profile\n\n- Name: %s\n- Reply preference: %s\n\n## How the user expects to use the assistant\n%s\n\nUpdate this file when the user states a durable preference. Record explicit preferences rather than guesses, and revise or remove them when corrected.\n", mdLine(p.UserName), replyPreference(p.ReplyStyle), mdParagraph(p.Purpose))
	memory := "# Long-term memory\n\nStore durable facts and preferences the user explicitly asks you to remember. Keep facts concise, use dates when relevant, and correct outdated entries instead of accumulating duplicates. Do not store secrets or sensitive personal details unless the user explicitly asks.\n"
	files := map[string]string{
		"IDENTITY.md": id,
		"SOUL.md":     soul,
		"USER.md":     user,
		"MEMORY.md":   memory,
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(a.workspace, name), []byte(contents), 0600); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) handleInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.mu.Lock()
	if a.state.Profile.UserName == "" || a.state.Profile.AgentName == "" || a.state.Profile.Purpose == "" {
		a.mu.Unlock()
		http.Error(w, "finish the first-run profile first", http.StatusConflict)
		return
	}
	if a.state.SetupRunning || a.state.SetupComplete {
		a.mu.Unlock()
		http.Error(w, "setup is already running or complete", http.StatusConflict)
		return
	}
	a.beginSetupLocked()
	_ = a.persistLocked()
	a.mu.Unlock()
	go a.bootstrap()
	writeJSON(w, map[string]any{"ok": true})
}

func (a *app) beginSetupLocked() {
	a.state.SetupRunning = true
	a.state.Error = ""
	a.state.ProgressPercent = 0
	a.state.ProgressStagePercent = 0
	a.state.ProgressStep = 0
	a.state.ProgressTotal = 9
	a.state.ProgressDetail = "Preparing automatic setup"
	a.state.Step = "Starting Sovereign AI setup"
	a.logLocked("Starting automatic OpenClaw and local model setup")
}

func (a *app) startSetup() {
	a.mu.Lock()
	if a.state.SetupRunning || a.state.SetupComplete {
		a.mu.Unlock()
		return
	}
	a.beginSetupLocked()
	_ = a.persistLocked()
	a.mu.Unlock()
	go a.bootstrap()
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
	const total = 9
	if err := a.runSetupStage(1, total, "Installing a private Node.js runtime and OpenClaw", func() error {
		if err := os.MkdirAll(a.runtimeDir(), 0700); err != nil {
			return err
		}
		if err := a.ensureNode(); err != nil {
			return fmt.Errorf("private Node.js runtime install failed: %w", err)
		}
		return a.ensureOpenClaw()
	}); err != nil {
		a.fail(fmt.Errorf("OpenClaw install failed: %w", err))
		return
	}
	if err := a.runSetupStage(2, total, "Preparing OpenClaw workspace and local memory settings", func() error {
		steps := [][]string{
			{"config", "set", "agents.defaults.workspace", a.workspace},
			{"config", "set", "memory.search.provider", "local"},
			{"config", "set", "memory.search.fallback", "none"},
		}
		for _, args := range steps {
			if err := a.runOpenClaw(args...); err != nil {
				return fmt.Errorf("OpenClaw command %q failed: %w", strings.Join(args, " "), err)
			}
		}
		return nil
	}); err != nil {
		a.fail(err)
		return
	}
	if err := a.runSetupStage(3, total, "Installing the local llama.cpp provider", func() error {
		return a.runOpenClaw("plugins", "install", "@openclaw/llama-cpp-provider", "--pin")
	}); err != nil {
		a.fail(fmt.Errorf("installing the local model provider failed: %w", err))
		return
	}
	modelSpec := `[{"id":"agents-a1-4b-q4-k-m","name":"Agents-A1 4B Q4_K_M","reasoning":false,"input":["text"],"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0},"contextWindow":16384,"maxTokens":4096,"params":{"modelPath":"hf:InternScience/Agents-A1-4B-Q4_K_M-GGUF/Agents-A1-4B-Q4_K_M.gguf","contextSize":16384},"compat":{"supportsTools":true}}]`
	if err := a.runSetupStage(4, total, "Registering Agents-A1 4B from Hugging Face", func() error {
		if err := a.runOpenClaw("config", "set", "models.providers.llama-cpp.models", modelSpec, "--strict-json", "--merge"); err != nil {
			return fmt.Errorf("registering Agents-A1 4B failed: %w", err)
		}
		if err := a.runOpenClaw("models", "set", "llama-cpp/agents-a1-4b-q4-k-m"); err != nil {
			return fmt.Errorf("selecting Agents-A1 4B failed: %w", err)
		}
		return nil
	}); err != nil {
		a.fail(err)
		return
	}
	if err := a.runSetupStage(5, total, "Downloading Agents-A1 4B and the managed llama.cpp runtime", func() error {
		return a.runOpenClaw("models", "auth", "login", "--provider", "llama-cpp", "--method", "local")
	}); err != nil {
		a.fail(fmt.Errorf("local model setup failed: %w", err))
		return
	}
	if err := a.runSetupStage(6, total, "Downloading and configuring local memory embeddings", func() error {
		return a.runOpenClaw("onboard", "--non-interactive", "--accept-risk", "--auth-choice", "llama-cpp", "--llama-cpp-embedding-only")
	}); err != nil {
		a.fail(fmt.Errorf("local embeddings setup failed: %w", err))
		return
	}
	if err := a.runSetupStage(7, total, "Indexing the local profile and memory files", func() error {
		return a.runOpenClaw("memory", "index", "--force")
	}); err != nil {
		a.fail(fmt.Errorf("memory indexing failed: %w", err))
		return
	}
	if err := a.runSetupStage(8, total, "Saving the assistant persona", func() error {
		return a.writePersona(a.profile())
	}); err != nil {
		a.fail(fmt.Errorf("saving persona files failed: %w", err))
		return
	}
	if err := a.runSetupStage(9, total, "Starting the OpenClaw Gateway", func() error {
		return a.startGateway()
	}); err != nil {
		a.fail(err)
		return
	}
	a.mu.Lock()
	a.state.SetupComplete = true
	a.state.Step = "Your local assistant is ready"
	a.state.ProgressPercent = 100
	a.state.ProgressStagePercent = 100
	a.state.ProgressStep = total
	a.state.ProgressTotal = total
	a.state.ProgressDetail = "All components are ready"
	a.state.Error = ""
	a.logLocked("OpenClaw setup completed; local memory indexing was requested")
	_ = a.persistLocked()
	a.mu.Unlock()
}

func (a *app) runSetupStage(index, total int, name string, action func() error) error {
	a.setSetupStage(index, total, name)
	if err := action(); err != nil {
		return err
	}
	a.completeSetupStage(index, total)
	return nil
}

func prependPath(env []string, directory string) []string {
	if directory == "" {
		return env
	}
	pathValue := envValue(env, "PATH")
	for _, entry := range strings.Split(pathValue, string(os.PathListSeparator)) {
		if strings.EqualFold(filepath.Clean(entry), filepath.Clean(directory)) {
			return env
		}
	}
	return setEnvValue(env, "PATH", directory+string(os.PathListSeparator)+pathValue)
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func envValue(env []string, name string) string {
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

func setEnvValue(env []string, name, value string) []string {
	updated := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(key, name) {
			continue
		}
		updated = append(updated, entry)
	}
	return append(updated, name+"="+value)
}

func (a *app) downloadFile(source, destination, label string) error {
	requestCtx, cancel := context.WithTimeout(a.ctx, 20*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, source, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("download %s: %w", label, err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("download %s: server returned %s", label, response.Status)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	tmp := destination + ".download"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	defer file.Close()

	var received int64
	lastPercent := -1
	lastUnknownReport := int64(-1)
	buffer := make([]byte, 32*1024)
	for {
		count, readErr := response.Body.Read(buffer)
		if count > 0 {
			written, writeErr := file.Write(buffer[:count])
			received += int64(written)
			if writeErr != nil {
				return writeErr
			}
			if written != count {
				return io.ErrShortWrite
			}
			if response.ContentLength > 0 {
				percent := int(received * 100 / response.ContentLength)
				if percent > 100 {
					percent = 100
				}
				if percent != lastPercent && (percent == 100 || percent%5 == 0) {
					a.recordDownloadProgress(label, received, response.ContentLength, percent)
					lastPercent = percent
				}
			} else if received-lastUnknownReport >= 1024*1024 {
				a.recordDownloadProgress(label, received, -1, -1)
				lastUnknownReport = received
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("download %s: %w", label, readErr)
		}
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if response.ContentLength <= 0 || received != response.ContentLength {
		a.recordDownloadProgress(label, received, response.ContentLength, 100)
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, destination); err != nil {
		return err
	}
	return nil
}

func (a *app) recordDownloadProgress(label string, received, total int64, percent int) {
	detail := fmt.Sprintf("%s: %s received", label, formatBytes(received))
	if total > 0 && percent >= 0 {
		detail = fmt.Sprintf("%s: %s of %s (%d%%)", label, formatBytes(received), formatBytes(total), percent)
	}
	a.mu.Lock()
	a.state.ProgressDetail = detail
	if percent >= 0 && a.state.ProgressTotal > 0 {
		a.stagePercent = percent
		a.state.ProgressStagePercent = percent
		base := (a.state.ProgressStep - 1) * 100
		a.state.ProgressPercent = (base + percent) / a.state.ProgressTotal
	}
	a.logLocked(detail)
	_ = a.persistLocked()
	a.mu.Unlock()
	if percent >= 0 {
		log.Printf("%s setup download [%s] %3d%% %s", productName, progressBar(percent), percent, label)
	} else {
		log.Printf("%s setup download %s", productName, detail)
	}
}

func formatBytes(n int64) string {
	if n < 1024*1024 {
		return fmt.Sprintf("%.0f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
}

func progressBar(percent int) string {
	const width = 20
	filled := percent * width / 100
	return strings.Repeat("#", filled) + strings.Repeat("-", width-filled)
}

func (a *app) runOpenClaw(args ...string) error {
	cmd, err := a.openclawCommand(args...)
	if err != nil {
		return err
	}
	return a.runCommand(cmd)
}

func (a *app) runCommand(cmd *exec.Cmd) error {
	// The profile form and Install action authorize this automated setup. Never
	// attach OpenClaw to the user's console; provide automatic confirmations to
	// installer/provider prompts and stream output into the local dashboard.
	cmd.Stdin = strings.NewReader(strings.Repeat("y\n", 32))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	var tailMu sync.Mutex
	var tail []string
	appendOutput := func(line string) {
		tailMu.Lock()
		tail = append(tail, line)
		if len(tail) > 12 {
			tail = append([]string(nil), tail[len(tail)-12:]...)
		}
		tailMu.Unlock()
		a.recordCommandOutput(line)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); streamCommandOutput(stdout, appendOutput) }()
	go func() { defer readers.Done(); streamCommandOutput(stderr, appendOutput) }()
	waitErr := cmd.Wait()
	readers.Wait()
	if waitErr != nil {
		tailMu.Lock()
		lastOutput := strings.Join(tail, " | ")
		tailMu.Unlock()
		if lastOutput != "" {
			return fmt.Errorf("%w: %s", waitErr, lastOutput)
		}
		return waitErr
	}
	return nil
}

func streamCommandOutput(reader io.Reader, writeLine func(string)) {
	scanner := bufio.NewScanner(reader)
	scanner.Split(scanConsoleLines)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		if line := cleanCommandLine(scanner.Text()); line != "" {
			writeLine(line)
		}
	}
	if err := scanner.Err(); err != nil {
		writeLine("Output stream warning: " + err.Error())
	}
}

func scanConsoleLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		advance = i + 1
		if data[i] == '\r' && advance < len(data) && data[advance] == '\n' {
			advance++
		}
		return advance, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func cleanCommandLine(line string) string {
	var clean strings.Builder
	clean.Grow(len(line))
	escape := false
	for _, r := range line {
		if r == '\x1b' {
			escape = true
			continue
		}
		if escape {
			if r >= '@' && r <= '~' {
				escape = false
			}
			continue
		}
		if r < 0x20 && r != '\t' {
			continue
		}
		clean.WriteRune(r)
	}
	result := strings.TrimSpace(clean.String())
	if len(result) > 240 {
		result = result[:240] + "…"
	}
	return result
}

func outputPercent(line string) (int, bool) {
	lower := strings.ToLower(line)
	if !strings.Contains(lower, "download") && !strings.Contains(lower, "install") && !strings.Contains(lower, "progress") && !strings.Contains(lower, "received") && !strings.Contains(lower, "transfer") {
		return 0, false
	}
	end := strings.LastIndex(line, "%")
	if end < 0 {
		return 0, false
	}
	start := end - 1
	for start >= 0 && ((line[start] >= '0' && line[start] <= '9') || line[start] == '.') {
		start--
	}
	pct, err := strconv.ParseFloat(strings.TrimSpace(line[start+1:end]), 64)
	if err != nil || pct < 0 || pct > 100 {
		return 0, false
	}
	return int(pct), true
}

func (a *app) setSetupStage(index, total int, name string) {
	a.mu.Lock()
	a.stagePercent = 0
	a.state.ProgressStagePercent = 0
	a.state.ProgressStep = index
	a.state.ProgressTotal = total
	a.state.ProgressPercent = (index - 1) * 100 / total
	a.state.ProgressDetail = "Working in the background"
	a.state.Step = name
	a.logLocked(name)
	_ = a.persistLocked()
	a.mu.Unlock()
	log.Printf("%s setup: %s", productName, name)
}

func (a *app) completeSetupStage(index, total int) {
	a.mu.Lock()
	a.stagePercent = 100
	a.state.ProgressStagePercent = 100
	a.state.ProgressStep = index
	a.state.ProgressTotal = total
	a.state.ProgressPercent = index * 100 / total
	a.state.ProgressDetail = "Stage complete"
	_ = a.persistLocked()
	a.mu.Unlock()
}

func (a *app) recordCommandOutput(line string) {
	a.mu.Lock()
	if a.state.SetupRunning {
		a.state.ProgressDetail = line
		if percent, ok := outputPercent(line); ok && a.state.ProgressTotal > 0 {
			a.stagePercent = percent
			a.state.ProgressStagePercent = percent
			base := (a.state.ProgressStep - 1) * 100
			a.state.ProgressPercent = (base + percent) / a.state.ProgressTotal
		}
	}
	a.logLocked(line)
	if time.Since(a.lastProgressSave) >= time.Second {
		_ = a.persistLocked()
		a.lastProgressSave = time.Now()
	}
	a.mu.Unlock()
	if percent, ok := outputPercent(line); ok {
		log.Printf("%s setup download [%s] %3d%% %s", productName, progressBar(percent), percent, line)
	} else {
		log.Printf("%s setup: %s", productName, line)
	}
}

func (a *app) profile() Profile {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state.Profile
}

func (a *app) setStep(step string) {
	a.mu.Lock()
	a.state.Step = step
	a.logLocked(step)
	_ = a.persistLocked()
	a.mu.Unlock()
	log.Printf("%s setup: %s", productName, step)
}

func (a *app) fail(err error) {
	a.mu.Lock()
	a.state.Error = err.Error()
	a.state.Step = "Setup needs attention"
	a.logLocked("Setup stopped: " + err.Error())
	_ = a.persistLocked()
	a.mu.Unlock()
	log.Printf("%s setup failed: %v", productName, err)
}

func (a *app) logLocked(line string) {
	a.state.RecentLogs = append(a.state.RecentLogs, time.Now().Format("15:04:05")+"  "+line)
	if len(a.state.RecentLogs) > 80 {
		a.state.RecentLogs = append([]string(nil), a.state.RecentLogs[len(a.state.RecentLogs)-80:]...)
	}
}

func (a *app) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.mu.Lock()
	ready := a.state.SetupComplete
	a.mu.Unlock()
	if !ready {
		http.Error(w, "finish local setup before starting the agent", http.StatusConflict)
		return
	}
	if err := a.startGateway(); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *app) startGateway() error {
	a.mu.Lock()
	if a.gateway != nil && a.gateway.Process != nil {
		a.state.GatewayRunning = true
		a.mu.Unlock()
		return nil
	}
	if a.gatewayLog == nil {
		f, err := os.OpenFile(filepath.Join(a.dataDir, "gateway.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			a.mu.Unlock()
			return err
		}
		a.gatewayLog = f
	}
	logFile := a.gatewayLog
	a.mu.Unlock()
	cmd, err := a.openclawCommand("gateway", "run")
	if err != nil {
		return fmt.Errorf("start Gateway: %w", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Gateway: %w", err)
	}
	a.mu.Lock()
	a.gateway = cmd
	a.state.GatewayRunning = true
	a.state.Step = "OpenClaw Gateway is starting"
	a.logLocked("Started OpenClaw Gateway")
	_ = a.persistLocked()
	a.mu.Unlock()
	go func() {
		err := cmd.Wait()
		a.mu.Lock()
		if a.gateway == cmd {
			a.gateway = nil
			a.state.GatewayRunning = false
			if err != nil && a.ctx.Err() == nil {
				a.state.Error = "Gateway exited: " + err.Error()
				a.logLocked(a.state.Error)
			} else {
				a.logLocked("OpenClaw Gateway stopped")
			}
			_ = a.persistLocked()
		}
		a.mu.Unlock()
	}()
	return nil
}

func (a *app) stopGateway() {
	a.mu.Lock()
	cmd := a.gateway
	a.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		if runtime.GOOS == "windows" {
			_ = exec.Command("taskkill.exe", "/PID", fmt.Sprint(cmd.Process.Pid), "/T", "/F").Run()
		} else {
			_ = cmd.Process.Kill()
		}
	}
}

func (a *app) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.stopGateway()
	writeJSON(w, map[string]any{"ok": true})
}

func (a *app) handleOpenClaw(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	go func() { _ = a.runOpenClaw("dashboard") }()
	writeJSON(w, map[string]any{"ok": true})
}

func (a *app) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if runtime.GOOS != "windows" {
		http.Error(w, "workspace reveal is supported on Windows", http.StatusNotImplemented)
		return
	}
	if err := exec.Command("explorer.exe", a.workspace).Start(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *app) handleMemory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var request struct {
		Backend string `json:"backend"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid memory provider", http.StatusBadRequest)
		return
	}
	if request.Backend != "memory-core" && request.Backend != "honcho" && request.Backend != "memory-lancedb" && request.Backend != "mem0" {
		http.Error(w, "unknown memory provider", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	if !a.state.SetupComplete || a.state.SetupRunning || a.state.MemoryRunning {
		a.mu.Unlock()
		http.Error(w, "finish setup and wait for the current operation", http.StatusConflict)
		return
	}
	a.state.MemoryRunning = true
	a.state.Error = ""
	a.state.Step = "Preparing " + request.Backend
	a.logLocked("Starting memory provider setup: " + request.Backend)
	_ = a.persistLocked()
	a.mu.Unlock()
	go a.configureMemory(request.Backend)
	writeJSON(w, map[string]any{"ok": true})
}

func (a *app) configureMemory(backend string) {
	a.mu.Lock()
	wasRunning := a.state.GatewayRunning
	a.mu.Unlock()
	if wasRunning {
		a.setStep("Pausing the Gateway to update memory settings")
		a.stopGateway()
		deadline := time.Now().Add(12 * time.Second)
		for time.Now().Before(deadline) {
			a.mu.Lock()
			stopped := a.gateway == nil
			a.mu.Unlock()
			if stopped {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		a.mu.Lock()
		stillRunning := a.gateway != nil
		a.mu.Unlock()
		if stillRunning {
			a.finishMemory("", errors.New("the Gateway did not stop; close its process before changing memory providers"), false)
			return
		}
	}

	var steps []struct {
		label string
		args  []string
	}
	add := func(label string, args ...string) {
		steps = append(steps, struct {
			label string
			args  []string
		}{label: label, args: args})
	}
	switch backend {
	case "memory-core":
		add("Selecting OpenClaw memory-core", "config", "set", "plugins.slots.memory", "memory-core")
		add("Keeping local embeddings enabled", "config", "set", "memory.search.provider", "local")
		add("Refreshing the local memory index", "memory", "index", "--force")
	case "honcho":
		add("Keeping OpenClaw memory-core active alongside Honcho", "config", "set", "plugins.slots.memory", "memory-core")
		add("Installing the Honcho memory plug-in", "plugins", "install", "@honcho-ai/openclaw-honcho", "--pin", "--force")
		add("Starting Honcho setup", "honcho", "setup")
	case "memory-lancedb":
		add("Installing the LanceDB memory plug-in", "plugins", "install", "@openclaw/memory-lancedb", "--pin", "--no-enable")
		localConfig := `{"embedding":{"provider":"local"},"autoRecall":true,"autoCapture":true}`
		add("Configuring LanceDB to use local embeddings", "config", "set", "plugins.entries.memory-lancedb.config", localConfig, "--strict-json", "--merge")
		add("Activating LanceDB memory", "plugins", "enable", "memory-lancedb")
		add("Selecting LanceDB as the active memory store", "config", "set", "plugins.slots.memory", "memory-lancedb")
	case "mem0":
		add("Installing the Mem0 memory plug-in", "plugins", "install", "@mem0/openclaw-mem0", "--pin", "--force")
		add("Starting the Mem0 local or hosted setup wizard", "mem0", "init", "--mode", "open-source")
		add("Selecting Mem0 as the active memory store", "config", "set", "plugins.slots.memory", "openclaw-mem0")
	}

	var operationErr error
	for _, step := range steps {
		a.setStep(step.label)
		if err := a.runOpenClaw(step.args...); err != nil {
			operationErr = fmt.Errorf("%s failed: %w", step.label, err)
			break
		}
	}
	active := backend
	if backend == "honcho" {
		active = "memory-core + Honcho"
	}
	if backend == "mem0" {
		active = "openclaw-mem0"
	}
	if wasRunning {
		a.setStep("Restarting the OpenClaw Gateway")
		if err := a.startGateway(); operationErr == nil && err != nil {
			operationErr = err
		}
	}
	a.finishMemory(active, operationErr, wasRunning)
}

func (a *app) finishMemory(active string, operationErr error, restartAttempted bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.MemoryRunning = false
	if operationErr != nil {
		a.state.Error = "Memory setup stopped: " + operationErr.Error()
		a.state.Step = "Memory setup needs attention"
		a.logLocked(a.state.Error)
	} else {
		a.state.MemoryBackend = active
		a.state.Error = ""
		a.state.Step = "Memory provider ready: " + active
		a.logLocked(a.state.Step)
	}
	_ = restartAttempted
	_ = a.persistLocked()
}

func cleanField(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\x00", ""))
	runes := []rune(s)
	if len(runes) > max {
		s = string(runes[:max])
	}
	return s
}

func mdLine(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(s))
}
func mdParagraph(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r", ""), "\x00", ""))
}

func replyPreference(style string) string {
	switch style {
	case "concise":
		return "Use concise, direct replies. Lead with the answer and add detail when it helps."
	case "detailed":
		return "Give thorough, structured replies with reasoning and useful context."
	default:
		return "Use clear, balanced replies: concise for simple questions and more detail for complex ones."
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}

func openBrowser(address string) {
	if runtime.GOOS == "windows" {
		_ = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", address).Start()
	}
}
