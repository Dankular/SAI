package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
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
	"sync/atomic"
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
	CompletedStage       int      `json:"completedStage"`
	StageLabels          []string `json:"stageLabels"`
	PersonaInstalled     bool     `json:"personaInstalled"`
	ProgressDetail       string   `json:"progressDetail,omitempty"`
	GatewayRunning       bool     `json:"gatewayRunning"`
	MemoryBackend        string   `json:"memoryBackend"`
	MemoryRunning        bool     `json:"memoryRunning"`
	Updating             bool     `json:"updating"`      // an existing install is fetching what this release adds
	WidgetEnabled        bool     `json:"widgetEnabled"` // show the sphere on the desktop
	WidgetCorner         string   `json:"widgetCorner"`  // top-right (default), top-left, bottom-left, bottom-right
	WidgetSize           string   `json:"widgetSize"`    // small, medium (default), large
	WidgetPositionSet    bool     `json:"widgetPositionSet,omitempty"`
	WidgetPositionX      float64  `json:"widgetPositionX,omitempty"` // normalized across the primary work area
	WidgetPositionY      float64  `json:"widgetPositionY,omitempty"`
	Schema               int      `json:"schema"` // version of the setup stage table the saved progress refers to
	Step                 string   `json:"step"`
	Error                string   `json:"error,omitempty"`
	RecentLogs           []string `json:"recentLogs"`
}

type app struct {
	mu               sync.Mutex
	workMu           sync.Mutex
	agent            agentRuntime
	harness          harnessRuntime
	marketplace      marketplaceRuntime
	widget           widgetRuntime
	orbShown         orbMirror // what the dashboard's sphere is showing, so the desktop widget can match it
	stateEvents      stateEventHub
	work             agentWorkStatus
	state            Snapshot
	statePath        string
	dataDir          string
	workspace        string
	serverURL        string
	llama            llamaState
	audio            audioState
	inference        cachedBool
	agentWarm        atomic.Bool // the model has answered once since it started, so the next turn is quick
	sup              *supervisor
	serviceError     bool // state.Error was set by the supervisor
	ctx              context.Context
	cancel           context.CancelFunc
	resumeSetup      bool
	stagePercent     int
	lastProgressSave time.Time
}

func main() {
	// ES modules require a JavaScript MIME type; the Windows registry can say otherwise.
	_ = mime.AddExtensionType(".js", "text/javascript; charset=utf-8")
	_ = mime.AddExtensionType(".mjs", "text/javascript; charset=utf-8")
	_ = mime.AddExtensionType(".wasm", "application/wasm")
	dataDir, err := sovereignAIDataDir()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		log.Fatal(err)
	}
	if !bindChildrenToUs() {
		log.Printf("%s could not tie its child processes to itself; they may outlive it if it is killed", productName)
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
	a.sup = a.newAppSupervisor()
	// Anything still listening on our ports belongs to an earlier run that did not exit cleanly.
	evictOrphans([]int{llamaChatPort, llamaEmbedPort, audioPort}, a.recordCommandOutput)
	go a.sup.Run()
	go a.watchStateChanges()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	a.serverURL = "http://" + listener.Addr().String()
	// Publish the (random) loopback address so tools can probe /api/state.
	urlFile := filepath.Join(dataDir, "server.url")
	if err := os.WriteFile(urlFile, []byte(a.serverURL+"\n"), 0600); err != nil {
		log.Printf("%s could not write %s: %v", productName, urlFile, err)
	}
	defer os.Remove(urlFile)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", a.handleState)
	mux.HandleFunc("/api/events", a.handleEvents)
	mux.HandleFunc("/api/setup", a.handleSetup)
	mux.HandleFunc("/api/install", a.handleInstall)
	mux.HandleFunc("/api/start", a.handleStart)
	mux.HandleFunc("/api/stop", a.handleStop)
	mux.HandleFunc("/api/workspace", a.handleWorkspace)
	mux.HandleFunc("/api/widget", a.handleWidget)
	mux.HandleFunc("/api/orb", a.handleOrb)
	mux.HandleFunc("/api/agent/approve", a.handleApprove)
	mux.HandleFunc("/api/agent/reset", a.handleAgentReset)
	mux.HandleFunc("/api/voice/chat", a.handleVoiceChat)
	mux.HandleFunc("/api/voice/tts", a.handleTTS)
	mux.HandleFunc("/api/voice/turn", a.handleTurn)
	mux.HandleFunc("/api/diagnose", a.handleDiagnose)
	mux.HandleFunc("/models/whistle/", a.handleWhistleFile)
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
	if os.Getenv("SOVEREIGNAI_NO_BROWSER") == "" {
		go openBrowser(a.serverURL)
	}
	log.Printf("%s is available at %s", productName, a.serverURL)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	if resumeSetup {
		a.startSetup()
	} else if a.state.SetupComplete {
		// Refresh the persona files so identity and rule changes reach existing installs.
		if err := a.writePersona(a.state.Profile); err != nil {
			log.Printf("%s persona: %v", productName, err)
		}
		go a.repairInstall()
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
	a.sup.Shutdown()
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

// stateSchema changes whenever the meaning of the saved setup progress changes.
const stateSchema = 3

func (a *app) load() error {
	a.state = Snapshot{RecentLogs: []string{}, StageLabels: stageLabels()}
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
	a.state.StageLabels = stageLabels()
	if a.state.Schema < stateSchema {
		// The stage list changed since this was saved, so its progress counters mean something else now.
		// The one-time stages only skip work that already exists, so re-checking them is quick.
		a.state.CompletedStage = 0
		a.state.Schema = stateSchema
		if a.state.SetupComplete {
			a.state.ProgressStep, a.state.ProgressTotal = len(a.state.StageLabels), len(a.state.StageLabels)
		}
	}
	if a.state.SetupComplete {
		a.state.PersonaInstalled = true
		// An error saved by an earlier run is history, not the current state; live problems are
		// reported again by the process manager.
		a.state.Error = ""
		a.state.Step = "Your local assistant is ready"
	}
	a.state.Updating = false
	a.resumeSetup = a.state.SetupRunning || (a.state.Profile.UserName != "" && a.state.Profile.AgentName != "" && a.state.Profile.Purpose != "" && !a.state.SetupComplete && a.state.Error == "")
	a.state.SetupRunning = false
	a.state.GatewayRunning = false
	a.state.MemoryRunning = false
	a.state.MemoryBackend = "chromem-go"
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
		if !publicPath(r.URL.Path) {
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
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Embedder-Policy", "require-corp")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "microphone=(self)")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; font-src 'self'; img-src 'self' data:; connect-src 'self'; media-src 'self' blob:; worker-src 'self' blob:; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// runtimeStatus describes the running process and its private runtime, for
// diagnostics via /api/state.
type runtimeStatus struct {
	PID             int    `json:"pid"`
	Executable      string `json:"executable"`
	DataDir         string `json:"dataDir"`
	RuntimeDir      string `json:"runtimeDir"`
	LlamaBackend    string `json:"llamaBackend"`
	AudioBackend    string `json:"audioBackend"`
	AudioReady      bool   `json:"audioReady"`
	InferenceBusy   bool   `json:"inferenceBusy"`
	AgentReady      bool   `json:"agentReady"`
	GatewayWarm     bool   `json:"gatewayWarm"`
	WhistleReady    bool   `json:"whistleReady"`
	StateSavedAtUTC string `json:"time"`
}

func executablePath() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return path
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
	a.mu.Lock()
	finished := a.state.SetupComplete
	a.mu.Unlock()
	if finished {
		if err := a.writePersona(p); err != nil {
			http.Error(w, "could not save the assistant profile", http.StatusInternalServerError)
			return
		}
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
	// ?from=N re-runs setup starting at stage N (1-10), even after completion.
	from, hasFrom := 0, false
	if v := r.URL.Query().Get("from"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > len(setupStages()) {
			a.mu.Unlock()
			http.Error(w, fmt.Sprintf("from must be a stage number from 1 to %d", len(setupStages())), http.StatusBadRequest)
			return
		}
		from, hasFrom = n, true
	}
	if a.state.SetupRunning || (a.state.SetupComplete && !hasFrom) {
		a.mu.Unlock()
		http.Error(w, "setup is already running or complete", http.StatusConflict)
		return
	}
	if hasFrom {
		a.state.SetupComplete = false
		a.state.CompletedStage = min(from-1, lastResumableStage())
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
	a.state.ProgressTotal = len(setupStages())
	a.state.ProgressDetail = "Preparing automatic setup"
	a.state.Step = "Starting Sovereign AI setup"
	a.logLocked("Starting automatic local model setup")
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
	requestCtx, cancel := context.WithTimeout(a.ctx, 4*time.Hour)
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	// A partly downloaded file is kept and continued, so an interruption (or closing the app)
	// does not cost the whole download again.
	tmp := destination + ".download"
	var have int64
	if info, err := os.Stat(tmp); err == nil {
		have = info.Size()
	}
	get := func(from int64) (*http.Response, error) {
		request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, source, nil)
		if err != nil {
			return nil, err
		}
		if from > 0 {
			request.Header.Set("Range", fmt.Sprintf("bytes=%d-", from))
		}
		return http.DefaultClient.Do(request)
	}
	response, err := get(have)
	if err == nil && have > 0 && response.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		response.Body.Close()
		have = 0 // the partial file is not usable; start again
		response, err = get(0)
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", label, err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("download %s: server returned %s", label, response.Status)
	}
	flags := os.O_CREATE | os.O_WRONLY
	total := response.ContentLength
	var received int64
	if response.StatusCode == http.StatusPartialContent && have > 0 {
		flags |= os.O_APPEND
		received = have
		if total > 0 {
			total += have
		}
	} else {
		flags |= os.O_TRUNC // the server sent the whole file
	}
	file, err := os.OpenFile(tmp, flags, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	complete := false
	defer func() {
		if !complete && received == 0 {
			os.Remove(tmp)
		}
	}()

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
			if total > 0 {
				percent := int(received * 100 / total)
				if percent > 100 {
					percent = 100
				}
				if percent != lastPercent && (percent == 100 || percent%5 == 0) {
					a.recordDownloadProgress(label, received, total, percent)
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
	if total <= 0 || received != total {
		a.recordDownloadProgress(label, received, total, 100)
	}
	if total > 0 && received != total {
		return fmt.Errorf("download %s: connection ended early (%s of %s)", label, formatBytes(received), formatBytes(total))
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, destination); err != nil {
		return err
	}
	complete = true
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

func (a *app) setStageActivity(detail string) {
	a.mu.Lock()
	a.stagePercent = 0
	a.state.ProgressStagePercent = 0
	a.state.ProgressDetail = detail
	a.logLocked(detail)
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
	ready, updating := a.state.SetupComplete, a.state.Updating
	a.mu.Unlock()
	if !ready {
		http.Error(w, "finish local setup before starting the agent", http.StatusConflict)
		return
	}
	if updating {
		http.Error(w, "your assistant is still downloading an update; it will start by itself", http.StatusConflict)
		return
	}
	a.wakeAssistant()
	writeJSON(w, map[string]any{"ok": true})
}

func (a *app) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.sleepAssistant()
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

// publicPath lists what the local web server may serve: the embedded UI files,
// vendored scripts, and the JSON API.
func publicPath(p string) bool {
	switch p {
	case "/", "/style.css", "/dashboard.css", "/app.js", "/orb.js", "/neuron.js", "/hud.js", "/widget.html", "/widget.js", "/widget.css", "/voice.js", "/vad.js", "/stt.js", "/stt-worker.js", "/listen.js", "/capture-worklet.js", "/favicon.ico":
		return true
	}
	return strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/vendor/") || strings.HasPrefix(p, "/models/whistle/")
}

// authoritativeSnapshot is the saved state with the fields the process manager owns
// overwritten by its live view, so a stale saved flag can never contradict it.
func (a *app) authoritativeSnapshot() Snapshot {
	snapshot := a.state
	snapshot.GatewayRunning = a.sup.Agent().Wanted
	return snapshot
}
