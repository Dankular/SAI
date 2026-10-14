package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// The assistant's voice is served by audio.cpp (one native runtime for speech
// models): OmniVoice synthesises speech, cloning a reference voice when one is
// available. Like llama.cpp, it ships official Windows builds per backend, so
// each workstation gets the build that matches its GPU vendor:
//
//	NVIDIA        cuda13.3 or cuda12.4 (by driver), then cpu
//	Intel / AMD   vulkan (audio.cpp has no SYCL build), then cpu
//
//	runtime/audio/<backend>   audiocpp_server.exe and its DLLs
//	runtime/audio/models      GGUF weights
//
// OmniVoice weights are CC-BY-NC 4.0: fine for personal use, not for a
// commercial product.
const (
	audioPort            = 19434
	audioRepo            = "audio-cpp/audio.cpp-gguf"
	omniFile             = "OmniVoice-GGUF/omnivoice-q8_0.gguf"
	omniModelID          = "omnivoice"
	turnFile             = "Smart-Turn-v3-GGUF/smart-turn-v3.2-f32.gguf"
	turnModelID          = "smart-turn"
	defaultVoiceInstruct = "male, middle-aged, british accent, moderate pitch"
)

type audioState struct {
	mu        sync.Mutex // serializes start/stop; held while starting
	backendMu sync.Mutex // guards backend only
	backend   string
	cmd       *exec.Cmd
	log       *os.File
	release   *ghRelease
	releaseMu sync.Mutex
	ttsMu     sync.Mutex // the server synthesises one request at a time
	preset    bool       // the running server was configured with the cloned-voice preset
	health    cachedBool
}

// cachedBool caches a cheap probe so the 1.5 s UI poll does not hammer a server.
type cachedBool struct {
	mu    sync.Mutex
	at    time.Time
	value bool
}

func (c *cachedBool) get(ttl time.Duration, probe func() bool) bool {
	c.mu.Lock()
	if time.Since(c.at) < ttl {
		v := c.value
		c.mu.Unlock()
		return v
	}
	c.mu.Unlock()
	v := probe()
	c.mu.Lock()
	c.at, c.value = time.Now(), v
	c.mu.Unlock()
	return v
}

func (a *app) audioRoot() string { return filepath.Join(a.runtimeDir(), "audio") }
func (a *app) omniModel() string {
	return filepath.Join(a.audioRoot(), "models", "omnivoice-q8_0.gguf")
}
func (a *app) turnModel() string {
	return filepath.Join(a.audioRoot(), "models", "smart-turn-v3.2-f32.gguf")
}
func (a *app) voicesDir() string   { return filepath.Join(a.dataDir, "voices") }
func (a *app) audioConfig() string { return filepath.Join(a.dataDir, "audio-server.json") }

func (a *app) audioBackendName() string {
	a.audio.backendMu.Lock()
	defer a.audio.backendMu.Unlock()
	return a.audio.backend
}

func (a *app) setAudioBackend(name string) {
	a.audio.backendMu.Lock()
	a.audio.backend = name
	a.audio.backendMu.Unlock()
}

// audioReady reports whether the speech server answers, cached briefly.
func (a *app) audioReady() bool {
	return a.audio.health.get(time.Second, a.audioHealthy)
}

func (a *app) audioHealthy() bool {
	client := http.Client{Timeout: 1500 * time.Millisecond}
	response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", audioPort))
	if err != nil {
		return false
	}
	response.Body.Close()
	return response.StatusCode == http.StatusOK
}

// audioBackendCandidates returns audio.cpp builds to try, best first.
// SOVEREIGNAI_AUDIO_BACKEND (cuda13.3|cuda12.4|vulkan|cpu) forces one.
func (a *app) audioBackendCandidates() []string {
	if forced := strings.ToLower(os.Getenv("SOVEREIGNAI_AUDIO_BACKEND")); forced != "" {
		return []string{forced}
	}
	var ladder []string
	nvidia, _, major, minor := detectGPU()
	switch {
	case nvidia && (major > 13 || major == 13 && minor >= 3):
		ladder = []string{"cuda13.3", "cpu"}
	case nvidia:
		ladder = []string{"cuda12.4", "cpu"}
	default:
		ladder = []string{"vulkan", "cpu"}
	}
	if saved, err := os.ReadFile(filepath.Join(a.audioRoot(), "backend.txt")); err == nil {
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

func (a *app) latestAudioRelease() (*ghRelease, error) {
	a.audio.releaseMu.Lock()
	defer a.audio.releaseMu.Unlock()
	if a.audio.release != nil {
		return a.audio.release, nil
	}
	response, err := a.httpGet("https://api.github.com/repos/0xShug0/audio.cpp/releases/latest")
	if err != nil {
		return nil, fmt.Errorf("look up audio.cpp releases: %w", err)
	}
	defer response.Body.Close()
	var release ghRelease
	if err := json.NewDecoder(response.Body).Decode(&release); err != nil {
		return nil, errors.New("read audio.cpp release index failed")
	}
	a.audio.release = &release
	return &release, nil
}

// ensureAudioBackend downloads, verifies and unpacks one audio.cpp build and
// returns the directory holding audiocpp_server.exe.
func (a *app) ensureAudioBackend(backend string) (string, error) {
	dir := filepath.Join(a.audioRoot(), backend)
	if exe, ok := findFile(dir, "audiocpp_server.exe"); ok {
		return filepath.Dir(exe), nil
	}
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		return "", errors.New("the speech runtime currently supports Windows x64")
	}
	release, err := a.latestAudioRelease()
	if err != nil {
		return "", err
	}
	var assets []ghAsset
	for _, asset := range release.Assets {
		if strings.HasSuffix(asset.Name, "-bin-windows-x64-"+backend+".zip") || (strings.HasPrefix(backend, "cuda") && strings.HasSuffix(asset.Name, "-cudart-windows-x64-"+backend+".zip")) {
			assets = append(assets, asset)
		}
	}
	if len(assets) == 0 {
		return "", fmt.Errorf("audio.cpp %s has no %s build for Windows x64", release.Tag, backend)
	}
	var total int64
	for _, asset := range assets {
		total += asset.Size
	}
	if err := os.MkdirAll(a.audioRoot(), 0700); err != nil {
		return "", err
	}
	if err := a.requireFreeSpace(a.audioRoot(), total*2, "the "+backend+" speech runtime"); err != nil {
		return "", err
	}
	staging := dir + ".partial"
	_ = os.RemoveAll(staging)
	for _, asset := range assets {
		archive := filepath.Join(a.audioRoot(), "downloads", asset.Name)
		if err := a.downloadFile(asset.URL, archive, "speech runtime ("+backend+")"); err != nil {
			return "", err
		}
		if want := strings.ToLower(strings.TrimPrefix(asset.Digest, "sha256:")); want != "" && want != strings.ToLower(asset.Digest) {
			got, err := fileSHA256(archive)
			if err != nil {
				return "", err
			}
			if got != want {
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
	if _, ok := findFile(staging, "audiocpp_server.exe"); !ok {
		_ = os.RemoveAll(staging)
		return "", fmt.Errorf("audio.cpp %s %s build does not contain audiocpp_server.exe", release.Tag, backend)
	}
	if err := a.retryFS("activate the "+backend+" speech runtime", func() error {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		return os.Rename(staging, dir)
	}); err != nil {
		return "", err
	}
	exe, _ := findFile(dir, "audiocpp_server.exe")
	return filepath.Dir(exe), nil
}

// voiceProfile is how the assistant sounds: a cloned reference voice, or a
// described one when no reference clip is available.
type voiceProfile struct {
	RefWav   string
	RefText  string
	Instruct string
}

func (a *app) voiceProfile() voiceProfile {
	wav := filepath.Join(a.voicesDir(), "default.wav")
	if isFile(wav) {
		text, _ := os.ReadFile(filepath.Join(a.voicesDir(), "default.txt"))
		return voiceProfile{RefWav: wav, RefText: strings.TrimSpace(string(text))}
	}
	return voiceProfile{Instruct: defaultVoiceInstruct}
}

// installBundledVoice copies a reference voice shipped next to the program
// (assets/voices/jarvis.wav + jarvis.txt) into the data directory once.
func (a *app) installBundledVoice() {
	if isFile(filepath.Join(a.voicesDir(), "default.wav")) {
		return
	}
	// A voice embedded in the program travels with it, so a fresh workstation needs no
	// extra files. (Put your own at <data>/voices/default.wav + default.txt to override.)
	for _, name := range []string{"default", "jarvis"} {
		wav, err := bundledVoices.ReadFile("assets/voices/" + name + ".wav")
		if err != nil {
			continue
		}
		if err := os.MkdirAll(a.voicesDir(), 0700); err != nil {
			return
		}
		_ = os.WriteFile(filepath.Join(a.voicesDir(), "default.wav"), wav, 0600)
		if text, err := bundledVoices.ReadFile("assets/voices/" + name + ".txt"); err == nil {
			_ = os.WriteFile(filepath.Join(a.voicesDir(), "default.txt"), text, 0600)
		}
		a.recordCommandOutput("Using the built-in reference voice (" + name + ")")
		return
	}
	var roots []string
	if exe, err := os.Executable(); err == nil {
		roots = append(roots, filepath.Dir(exe), filepath.Dir(filepath.Dir(exe)))
	}
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, wd)
	}
	for _, root := range roots {
		for _, name := range []string{"default", "jarvis"} {
			wav := filepath.Join(root, "assets", "voices", name+".wav")
			if !isFile(wav) {
				continue
			}
			if err := os.MkdirAll(a.voicesDir(), 0700); err != nil {
				return
			}
			if data, err := os.ReadFile(wav); err == nil {
				_ = os.WriteFile(filepath.Join(a.voicesDir(), "default.wav"), data, 0600)
			}
			if text, err := os.ReadFile(filepath.Join(root, "assets", "voices", name+".txt")); err == nil {
				_ = os.WriteFile(filepath.Join(a.voicesDir(), "default.txt"), text, 0600)
			}
			a.recordCommandOutput("Using the reference voice " + name + ".wav")
			return
		}
	}
}

func (a *app) writeAudioConfig(backend string) error {
	kind := "cpu"
	switch {
	case strings.HasPrefix(backend, "cuda"):
		kind = "cuda"
	case backend == "vulkan":
		kind = "vulkan"
	}
	model := map[string]any{
		"id": omniModelID, "family": "omnivoice", "path": filepath.ToSlash(a.omniModel()), "task": "tts", "mode": "offline",
	}
	// The reference goes in a preset so the server encodes and caches it once,
	// instead of re-processing the clip for every sentence.
	profile := a.voiceProfile()
	a.audio.preset = profile.RefWav != ""
	if a.audio.preset {
		model["default_voice_preset"] = map[string]any{
			"voice_ref":      filepath.ToSlash(profile.RefWav),
			"reference_text": profile.RefText,
		}
	}
	models := []map[string]any{model}
	if isFile(a.turnModel()) {
		// Smart Turn (32 MB) shares this server and engine; it decides whether a pause is the end of a turn.
		models = append(models, map[string]any{
			"id": turnModelID, "family": "smart_turn", "path": filepath.ToSlash(a.turnModel()), "task": "turn", "mode": "offline",
		})
	}
	config := map[string]any{
		"host": "127.0.0.1", "port": audioPort, "backend": kind, "device": 0, "threads": 4, "lazy_load": false,
		"models": models,
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.audioConfig(), data, 0600)
}

// startAudio makes sure the speech server is running, trying each backend in
// order until one comes up healthy.
func (a *app) startAudio() error {
	if a.audioHealthy() {
		return nil
	}
	a.audio.mu.Lock()
	defer a.audio.mu.Unlock()
	a.stopAudioLocked()
	a.installBundledVoice()
	if !isFile(a.omniModel()) {
		a.setStageActivity("Downloading the assistant's voice")
		if err := a.ensureModel(audioRepo, omniFile, a.omniModel()); err != nil {
			return fmt.Errorf("download the voice model: %w", err)
		}
	}
	if !isFile(a.turnModel()) {
		if err := a.ensureModel(audioRepo, turnFile, a.turnModel()); err != nil {
			a.recordCommandOutput("Turn detection model unavailable: " + err.Error())
		}
	}
	var failures []string
	for _, backend := range a.audioBackendCandidates() {
		a.setStageActivity("Starting the assistant's voice (" + backend + ")")
		dir, err := a.ensureAudioBackend(backend)
		if err == nil {
			err = a.launchAudio(backend, dir)
		}
		if err == nil {
			a.setAudioBackend(backend)
			if os.Getenv("SOVEREIGNAI_AUDIO_BACKEND") == "" {
				_ = os.WriteFile(filepath.Join(a.audioRoot(), "backend.txt"), []byte(backend+"\n"), 0600)
			}
			a.recordCommandOutput("The assistant's voice is ready (" + backend + ")")
			return nil
		}
		a.stopAudioLocked()
		failures = append(failures, backend+": "+err.Error())
		a.recordCommandOutput("Speech backend " + backend + " unavailable: " + err.Error())
		if a.ctx.Err() != nil {
			break
		}
	}
	return fmt.Errorf("no speech backend could start (%s)", strings.Join(failures, "; "))
}

func (a *app) launchAudio(backend, dir string) error {
	if err := a.writeAudioConfig(backend); err != nil {
		return err
	}
	logPath := filepath.Join(a.dataDir, "audio-server.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(a.ctx, filepath.Join(dir, "audiocpp_server.exe"), "--config", a.audioConfig())
	cmd.Dir = dir
	cmd.Env = prependPath(a.runtimeEnv(), dir)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start speech server: %w", err)
	}
	a.audio.cmd, a.audio.log = cmd, logFile
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.Now().Add(4 * time.Minute)
	for {
		if a.audioHealthy() {
			return nil
		}
		select {
		case err := <-exited:
			return fmt.Errorf("speech server exited (%v): %s", err, tailFile(logPath, 600))
		case <-a.ctx.Done():
			return a.ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("speech server did not become ready: %s", tailFile(logPath, 600))
		}
	}
}

func (a *app) stopAudio() {
	a.audio.mu.Lock()
	defer a.audio.mu.Unlock()
	a.stopAudioLocked()
}

func (a *app) stopAudioLocked() {
	if cmd := a.audio.cmd; cmd != nil && cmd.Process != nil {
		if runtime.GOOS == "windows" {
			_ = exec.Command("taskkill.exe", "/PID", fmt.Sprint(cmd.Process.Pid), "/T", "/F").Run()
		} else {
			_ = cmd.Process.Kill()
		}
	}
	if a.audio.log != nil {
		_ = a.audio.log.Close()
	}
	a.audio.cmd, a.audio.log = nil, nil
	a.setAudioBackend("")
	a.audio.health.mu.Lock()
	a.audio.health.at = time.Time{}
	a.audio.health.mu.Unlock()
}

// handleTTS turns one sentence into speech (WAV) in the assistant's voice.
func (a *app) handleTTS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	var in struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	text := cleanField(in.Text, 1200)
	if text == "" {
		http.Error(w, "nothing to say", http.StatusBadRequest)
		return
	}
	if !a.audioReady() {
		http.Error(w, "the assistant's voice is still starting up", http.StatusServiceUnavailable)
		return
	}
	request := map[string]any{"model": omniModelID, "input": text, "response_format": "wav"}
	if profile := a.voiceProfile(); profile.RefWav != "" {
		// Normally the server's default preset supplies the cloned voice. If the
		// server was started before the reference existed, send it per request.
		// (The cloning field is voice_ref; "voice" only names a preset or built-in voice.)
		if !a.audio.preset {
			request["voice_ref"] = filepath.ToSlash(profile.RefWav)
			request["reference_text"] = profile.RefText
		}
	} else {
		request["options"] = map[string]any{"instruct": profile.Instruct}
	}
	body, _ := json.Marshal(request)

	a.audio.ttsMu.Lock()
	defer a.audio.ttsMu.Unlock()
	ctx := r.Context()
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/audio/speech", audioPort), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 2 * time.Minute}
	response, err := client.Do(upstream)
	if err != nil {
		http.Error(w, "the voice server did not respond", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		http.Error(w, "speech failed: "+strings.TrimSpace(string(detail)), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, response.Body)
}

// handleTurn judges whether a pause is the end of the user's turn. The body is a
// 16 kHz mono WAV of the most recent speech (up to 8 s); the reply is
// {"complete":bool,"probability":0..1}. It runs on Smart Turn v3.2 inside the
// speech server, so it adds no process of its own.
func (a *app) handleTurn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	audio, err := io.ReadAll(r.Body)
	if err != nil || len(audio) < 1000 {
		http.Error(w, "send a 16 kHz mono WAV", http.StatusBadRequest)
		return
	}
	if !a.audioReady() || !isFile(a.turnModel()) {
		http.Error(w, "turn detection is not ready", http.StatusServiceUnavailable)
		return
	}
	dir := filepath.Join(a.dataDir, "tmp")
	if err := os.MkdirAll(dir, 0700); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	file, err := os.CreateTemp(dir, "turn-*.wav")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(audio)
	file.Close()
	if writeErr != nil {
		http.Error(w, writeErr.Error(), http.StatusInternalServerError)
		return
	}
	body, _ := json.Marshal(map[string]any{
		"model":   turnModelID,
		"request": map[string]any{"audio": filepath.ToSlash(file.Name()), "options": map[string]string{"threshold": "0.5"}},
	})
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/tasks/run", audioPort), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 20 * time.Second}
	response, err := client.Do(upstream)
	if err != nil {
		http.Error(w, "the turn detector did not respond", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	var result struct {
		Output struct {
			Data struct {
				Complete    bool    `json:"complete"`
				Probability float64 `json:"probability"`
			} `json:"data"`
		} `json:"custom_schema_output"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&result) != nil {
		http.Error(w, "turn detection failed", http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"complete": result.Output.Data.Complete, "probability": result.Output.Data.Probability})
}

// ensureVoiceAssets downloads everything the assistant's voice and hearing need: the
// speech runtime for this GPU, the OmniVoice and Smart Turn models, and Whistle for
// speech recognition. It runs as a setup stage, so a fresh machine has all of it
// before the assistant is first woken. Everything is skipped if already present.
func (a *app) ensureVoiceAssets() error {
	a.installBundledVoice()
	var failures []string
	installed := false
	for _, backend := range a.audioBackendCandidates() {
		a.setStageActivity("Downloading the speech runtime (" + backend + ")")
		if _, err := a.ensureAudioBackend(backend); err != nil {
			failures = append(failures, backend+": "+err.Error())
			continue
		}
		installed = true
		break
	}
	if !installed {
		return errors.New("the speech runtime could not be installed (" + strings.Join(failures, "; ") + ")")
	}
	for _, model := range []struct{ path, file string }{{a.omniModel(), omniFile}, {a.turnModel(), turnFile}} {
		if isFile(model.path) {
			continue
		}
		if err := a.ensureModel(audioRepo, model.file, model.path); err != nil {
			return err
		}
	}
	return a.ensureWhistle()
}
