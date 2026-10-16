package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func touch(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0600); err != nil {
		t.Fatal(err)
	}
}

// An install from the OpenClaw releases keeps working: nothing is re-run, the old engine's
// files go, and the old model goes only once the new one exists.
func TestRemoveLegacyKeepsTheOldModelUntilTheNewOneIsThere(t *testing.T) {
	a := newTestApp(t)
	old := filepath.Join(a.modelsDir(), "Agents-A1-4B-Q4_K_M.gguf")
	touch(t, old, 10)
	touch(t, filepath.Join(a.runtimeDir(), "node", "node.exe"), 100)
	touch(t, filepath.Join(a.runtimeDir(), "openclaw", "node_modules", "x.js"), 100)
	touch(t, filepath.Join(a.dataDir, "gateway.token"), 5)
	touch(t, filepath.Join(a.runtimeDir(), "llama", "cuda", "llama-server.exe"), 5) // not legacy
	a.state.RecentLogs = []string{}

	a.removeLegacy()
	for _, gone := range []string{filepath.Join(a.runtimeDir(), "node"), filepath.Join(a.runtimeDir(), "openclaw"), filepath.Join(a.dataDir, "gateway.token")} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("%s should have been removed", gone)
		}
	}
	if !isFile(old) {
		t.Error("the old model must stay while there is no new model")
	}
	if !isFile(filepath.Join(a.runtimeDir(), "llama", "cuda", "llama-server.exe")) {
		t.Error("files the current release uses must not be touched")
	}

	touch(t, a.chatModel(), 10)
	a.removeLegacy()
	if isFile(old) {
		t.Error("the old model should go once the new one is present")
	}
	if !isFile(a.chatModel()) {
		t.Error("the new model must stay")
	}
}

func TestMissingPiecesNamesOnlyWhatIsAbsent(t *testing.T) {
	a := newTestApp(t)
	if err := os.MkdirAll(filepath.Dir(a.userlandBash()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.userlandBash(), []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(a.workspaceUserlandDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.workspaceUserlandDir(), "runtime.json"), []byte(userlandRelease+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(a.llamaRoot(), "cuda", "llama-server.exe"), 5)
	touch(t, a.embedModel(), 5)
	var names []string
	for _, p := range a.missingPieces() {
		names = append(names, p.label)
	}
	got := strings.Join(names, ",")
	if got != "npx and uvx for extensions,the language model,the speed-up model" {
		t.Errorf("missing = %q", got)
	}
	useFakeToolchain(t, false)
	fakeInstalledToolchain(t, a)
	touch(t, a.chatModel(), 5)
	touch(t, a.draftModel(), 5)
	if left := a.missingPieces(); len(left) != 0 {
		t.Errorf("nothing should be missing, got %d", len(left))
	}
}

// Saved progress from the previous stage table must not be read against the new one, and a
// stale error from an earlier run must not make a finished install look broken.
func TestLoadMigratesStateFromAnOlderRelease(t *testing.T) {
	a := newTestApp(t)
	old := map[string]any{
		"profile":        map[string]any{"userName": "Dan", "agentName": "Jarvis", "purpose": "General"},
		"setupComplete":  true,
		"completedStage": 7,
		"progressStep":   11,
		"progressTotal":  11,
		"step":           "OpenClaw Gateway is starting",
		"error":          "Gateway exited: exit status 1",
	}
	data, _ := json.Marshal(old)
	if err := os.WriteFile(a.statePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.load(); err != nil {
		t.Fatal(err)
	}
	if a.state.CompletedStage != 0 || a.state.Schema != stateSchema {
		t.Errorf("progress was not reset: stage=%d schema=%d", a.state.CompletedStage, a.state.Schema)
	}
	if a.state.Error != "" || strings.Contains(a.state.Step, "OpenClaw") {
		t.Errorf("stale status survived: error=%q step=%q", a.state.Error, a.state.Step)
	}
	if !a.state.SetupComplete || a.resumeSetup {
		t.Error("a finished install must not be put back through setup")
	}
	if a.state.ProgressTotal != len(stageLabels()) {
		t.Errorf("progress total = %d, want %d", a.state.ProgressTotal, len(stageLabels()))
	}
}

// An interrupted download is continued from where it stopped.
func TestDownloadResumesAPartialFile(t *testing.T) {
	a := newTestApp(t)
	a.state.RecentLogs = []string{}
	content := []byte(strings.Repeat("0123456789", 5000))
	var sawRange string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f.bin", time.Time{}, bytes.NewReader(content))
		sawRange = r.Header.Get("Range")
	}))
	defer server.Close()
	dest := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(dest+".download", content[:20000], 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.downloadFile(server.URL, dest, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, content) {
		t.Errorf("resumed file differs (%d bytes, want %d)", len(got), len(content))
	}
	if sawRange != "bytes=20000-" {
		t.Errorf("range header = %q", sawRange)
	}
	if _, err := os.Stat(dest + ".download"); err == nil {
		t.Error("the partial file should be gone after a complete download")
	}
	// A partial file that is bigger than the real one is discarded and the download starts over.
	_ = os.Remove(dest)
	_ = os.WriteFile(dest+".download", append(content, content...), 0600)
	if err := a.downloadFile(server.URL, dest, "test"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, content) {
		t.Errorf("restart after a bad partial gave %d bytes", len(got))
	}
}
