package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeZip(t *testing.T, files map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte(body))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

// useFakeToolchain serves small archives shaped like the real Node.js and uv ones.
func useFakeToolchain(t *testing.T, corruptNode bool) *int {
	t.Helper()
	nodeZip, nodeSum := makeZip(t, map[string]string{
		"node-v9.9.9-win-x64/node.exe": "node", "node-v9.9.9-win-x64/npx.cmd": "npx", "node-v9.9.9-win-x64/npm.cmd": "npm",
	})
	uvZip, uvSum := makeZip(t, map[string]string{"uv.exe": "uv", "uvx.exe": "uvx", "uvw.exe": "uvw"})
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch {
		case strings.HasSuffix(r.URL.Path, "node.zip"):
			body := nodeZip
			if corruptNode {
				body = append([]byte("junk"), nodeZip...)
			}
			_, _ = w.Write(body)
		case strings.HasSuffix(r.URL.Path, "uv.zip"):
			_, _ = w.Write(uvZip)
		default:
			http.NotFound(w, r)
		}
	}))
	oldPieces, oldSupported := toolchainPieces, toolchainSupported
	toolchainSupported = true
	toolchainPieces = []toolchainPiece{
		{name: "node", url: server.URL + "/node.zip", sha256: nodeSum, flatten: true, needs: []string{"node.exe", "npx.cmd", "npm.cmd"}},
		{name: "uv", url: server.URL + "/uv.zip", sha256: uvSum, needs: []string{"uv.exe", "uvx.exe"}},
	}
	t.Cleanup(func() { toolchainPieces, toolchainSupported = oldPieces, oldSupported; server.Close() })
	return &hits
}

func fakeInstalledToolchain(t *testing.T, a *app) {
	t.Helper()
	for _, p := range toolchainPieces {
		for _, f := range p.needs {
			touch(t, filepath.Join(a.toolchainDir(p.name), f), 1)
		}
		_ = os.WriteFile(filepath.Join(a.toolchainDir(p.name), ".pinned"), []byte(p.sha256+"\n"), 0600)
	}
}

func TestToolchainInstallsVerifiesAndIsIdempotent(t *testing.T) {
	a := newTestApp(t)
	hits := useFakeToolchain(t, false)
	if a.toolchainReady() {
		t.Fatal("nothing is installed yet")
	}
	if err := a.ensureToolchain(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"node/node.exe", "node/npx.cmd", "uv/uvx.exe"} {
		if !isFile(filepath.Join(a.toolsDir(), filepath.FromSlash(f))) {
			t.Errorf("%s was not installed", f)
		}
	}
	if !a.toolchainReady() {
		t.Error("the toolchain should be ready")
	}
	before := *hits
	if err := a.ensureToolchain(); err != nil || *hits != before {
		t.Errorf("a second call must download nothing: err=%v hits %d->%d", err, before, *hits)
	}
	// a pin change (a new release) reinstalls
	toolchainPieces[1].sha256 = strings.Repeat("0", 64)
	if a.toolchainReady() {
		t.Error("a different pinned build must not count as installed")
	}
}

func TestToolchainRefusesATamperedDownload(t *testing.T) {
	a := newTestApp(t)
	useFakeToolchain(t, true)
	err := a.ensureToolchain()
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("a download that does not match its pin must be refused, got %v", err)
	}
	if isFile(filepath.Join(a.toolchainDir("node"), "node.exe")) {
		t.Error("nothing from a bad download may be left in place")
	}
}

func TestExtensionCommandsResolveToTheBundle(t *testing.T) {
	a := newTestApp(t)
	useFakeToolchain(t, false)
	for command, want := range map[string]string{
		"npx":          filepath.Join("node", "npx.cmd"),
		"npx.cmd":      filepath.Join("node", "npx.cmd"),
		"uvx":          filepath.Join("uv", "uvx.exe"),
		"node":         filepath.Join("node", "node.exe"),
		"UVX.EXE":      filepath.Join("uv", "uvx.exe"),
		"python":       "python",
		"my-server":    "my-server",
		`C:\x\npx.cmd`: `C:\x\npx.cmd`,
	} {
		got, err := a.resolveExtensionCommand(command)
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		if strings.HasSuffix(want, "cmd") || strings.HasSuffix(want, "exe") {
			if strings.HasPrefix(command, "C:") {
				if got != want {
					t.Errorf("%s: an explicit path must be left alone, got %s", command, got)
				}
				continue
			}
			want = filepath.Join(a.toolsDir(), want)
		}
		if got != want {
			t.Errorf("%s resolved to %s, want %s", command, got, want)
		}
	}
}

func TestExtensionEnvironmentGetsToolsAndPrivateCaches(t *testing.T) {
	a := newTestApp(t)
	useFakeToolchain(t, false)
	if err := a.ensureToolchain(); err != nil {
		t.Fatal(err)
	}
	env := a.extensionEnvironment(map[string]string{"API_KEY": "abc"})
	path := envValue(env, "PATH")
	if !strings.HasPrefix(path, a.toolchainDir("node")+string(os.PathListSeparator)+a.toolchainDir("uv")) {
		t.Errorf("PATH does not start with the bundled tools: %s", path)
	}
	for _, key := range []string{"npm_config_cache", "UV_CACHE_DIR", "UV_PYTHON_INSTALL_DIR"} {
		if v := envValue(env, key); !strings.HasPrefix(v, a.toolsDir()) {
			t.Errorf("%s = %q, want it inside the app folder", key, v)
		}
	}
	if envValue(env, "UV_PYTHON_PREFERENCE") != "only-managed" || envValue(env, "API_KEY") != "abc" {
		t.Errorf("env = %v", env)
	}
	if strings.Count(path, a.toolchainDir("node")) != 1 {
		t.Errorf("the tools were put on PATH more than once: %s", path)
	}
}

func TestToolchainIsAnInstallPieceAndStage(t *testing.T) {
	found := false
	for _, p := range installPieces() {
		found = found || p.label == "npx and uvx for extensions" && p.optional
	}
	if !found {
		t.Error("existing installs must be offered the toolchain as an optional piece")
	}
	staged := false
	for _, s := range setupStages() {
		staged = staged || s.label == "Add npx and uvx for extensions" && s.resumable
	}
	if !staged {
		t.Error("fresh installs must get a setup stage for the toolchain")
	}
}
