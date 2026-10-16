package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// legacyFixture builds a profile that looks like the real machine: OpenClaw's config, cache and global
// npm package, plus things that merely look similar and must be left alone.
func legacyFixture(t *testing.T) (a *app, home, roaming, local string) {
	t.Helper()
	root := t.TempDir()
	home, roaming, local = filepath.Join(root, "home"), filepath.Join(root, "roaming"), filepath.Join(root, "local")
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", roaming)
	t.Setenv("LOCALAPPDATA", local)
	write := func(path, text string) {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".clawdbot", "clawdbot.json"), `{"gateway":{}}`)
	write(filepath.Join(home, ".clawdbot", "agents", "main", "notes.md"), "old notes")
	write(filepath.Join(local, "openclaw", "deps", "a.bin"), "cache")
	write(filepath.Join(roaming, "npm", "node_modules", "openclaw", ".keep"), "")
	_ = os.Remove(filepath.Join(roaming, "npm", "node_modules", "openclaw", ".keep"))
	write(filepath.Join(roaming, "npm", "clawdbot.cmd"), `@ECHO off node "%dp0%\node_modules\clawdbot\dist\entry.js" %*`)
	write(filepath.Join(roaming, "npm", "clawdbot"), `exec node "$basedir/node_modules/clawdbot/dist/entry.js" "$@"`)
	// look-alikes that are not OpenClaw's
	write(filepath.Join(home, ".openclaw-notes", "todo.txt"), "mine")
	write(filepath.Join(home, ".moltbot", "random.txt"), "someone else's folder with no OpenClaw markers")
	write(filepath.Join(roaming, "npm", "openclaw.cmd"), "@echo this is my own script")
	write(filepath.Join(roaming, "npm", "node_modules", "moltbot", "package.json"), `{"name":"a-different-package"}`)
	write(filepath.Join(roaming, "npm", "node_modules", "typescript", "package.json"), `{"name":"typescript"}`)
	a = newTestApp(t)
	gatewayRunning = func() bool { return false }
	t.Cleanup(func() { gatewayRunning = openClawRunning })
	return
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

func TestRetireMovesVerifiedOpenClawLeftoversAndNothingElse(t *testing.T) {
	a, home, roaming, local := legacyFixture(t)
	moved := a.retireExternalLegacy()
	for _, gone := range []string{
		filepath.Join(home, ".clawdbot"),
		filepath.Join(local, "openclaw"),
		filepath.Join(roaming, "npm", "node_modules", "openclaw"),
		filepath.Join(roaming, "npm", "clawdbot.cmd"),
		filepath.Join(roaming, "npm", "clawdbot"),
	} {
		if exists(gone) {
			t.Errorf("%s should have been retired", gone)
		}
	}
	for _, kept := range []string{
		filepath.Join(home, ".openclaw-notes", "todo.txt"),                          // not an OpenClaw folder name
		filepath.Join(home, ".moltbot", "random.txt"),                               // the right name but nothing of OpenClaw's inside
		filepath.Join(roaming, "npm", "openclaw.cmd"),                               // a script that does not launch OpenClaw
		filepath.Join(roaming, "npm", "node_modules", "moltbot", "package.json"),    // a different package
		filepath.Join(roaming, "npm", "node_modules", "typescript", "package.json"), // someone else's global package
	} {
		if !exists(kept) {
			t.Errorf("%s must not be touched", kept)
		}
	}
	if len(moved) != 5 {
		t.Errorf("moved = %v", moved)
	}
	entries := a.loadRetired()
	if len(entries) != 5 {
		t.Fatalf("manifest = %+v", entries)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.To, a.retiredDir()) || !exists(e.To) {
			t.Errorf("entry %+v is not in the holding folder", e)
		}
	}
	// the user's old config survived intact, just moved
	for _, e := range entries {
		if strings.HasSuffix(e.From, ".clawdbot") {
			if data, err := os.ReadFile(filepath.Join(e.To, "agents", "main", "notes.md")); err != nil || string(data) != "old notes" {
				t.Errorf("retired content changed: %q %v", data, err)
			}
		}
	}
	if again := a.retireExternalLegacy(); len(again) != 0 {
		t.Errorf("a second pass moved %v", again)
	}
}

func TestRestoreAndPurge(t *testing.T) {
	a, home, _, local := legacyFixture(t)
	a.retireExternalLegacy()
	if purged := a.purgeRetired(time.Now().Add(13 * 24 * time.Hour)); purged != 0 {
		t.Fatalf("nothing is old enough after 13 days, purged %d", purged)
	}
	// someone made a new .clawdbot meanwhile: restoring must not overwrite it
	if err := os.MkdirAll(filepath.Join(home, ".clawdbot"), 0700); err != nil {
		t.Fatal(err)
	}
	restored := a.restoreRetired()
	if restored != 4 || !exists(filepath.Join(local, "openclaw", "deps", "a.bin")) {
		t.Fatalf("restored %d", restored)
	}
	if entries := a.loadRetired(); len(entries) != 1 || !strings.Contains(entries[0].From, ".clawdbot") {
		t.Errorf("the item that could not be restored must stay on record: %+v", entries)
	}
	if n := a.purgeRetired(time.Now().Add(15 * 24 * time.Hour)); n != 1 {
		t.Errorf("purged %d, want 1", n)
	}
	if exists(a.retiredDir()) {
		t.Errorf("an empty holding folder should disappear: %v", exists(a.retiredDir()))
	}
	if !exists(filepath.Join(home, ".clawdbot")) {
		t.Errorf("purging must never touch the profile")
	}
}

func TestRetireWaitsWhileOpenClawIsRunningOrKeepIsSet(t *testing.T) {
	a, home, _, _ := legacyFixture(t)
	gatewayRunning = func() bool { return true }
	if moved := a.retireExternalLegacy(); len(moved) != 0 || !exists(filepath.Join(home, ".clawdbot")) {
		t.Fatalf("a running gateway means hands off: %v", moved)
	}
	gatewayRunning = func() bool { return false }
	t.Setenv("SOVEREIGNAI_KEEP_LEGACY", "1")
	if moved := a.retireExternalLegacy(); len(moved) != 0 {
		t.Fatalf("the opt-out must be honoured: %v", moved)
	}
}

func TestOldModelIsRemovedInAnyQuantOnlyOnceTheNewOneIsInPlace(t *testing.T) {
	a := newTestApp(t)
	old := []string{"Agents-A1-4B-Q4_K_M.gguf", "Agents-A1-4B-Q8_0.gguf", "Agents-A1-4B-Q4_K_M.gguf.download"}
	for _, name := range old {
		touch(t, filepath.Join(a.modelsDir(), name), 10)
	}
	touch(t, filepath.Join(a.runtimeDir(), "openclaw", "x"), 10)
	touch(t, filepath.Join(a.runtimeDir(), "node", "node.exe"), 10)
	touch(t, a.embedModel(), 10)
	a.removeLegacy()
	for _, name := range old {
		if !isFile(filepath.Join(a.modelsDir(), name)) {
			t.Errorf("%s must stay until the replacement model exists", name)
		}
	}
	if exists(filepath.Join(a.runtimeDir(), "openclaw")) || exists(filepath.Join(a.runtimeDir(), "node")) {
		t.Error("the OpenClaw runtime and the old private Node.js are not needed and can go at once")
	}
	touch(t, a.chatModel(), 10)
	a.removeLegacy()
	for _, name := range old {
		if isFile(filepath.Join(a.modelsDir(), name)) {
			t.Errorf("%s should be gone once the new model is in place", name)
		}
	}
	if !isFile(a.chatModel()) || !isFile(a.embedModel()) {
		t.Error("the current models must never be removed")
	}
	// the bundled toolchain lives in runtime/tools/node, which is NOT the old runtime/node
	touch(t, filepath.Join(a.toolchainDir("node"), "node.exe"), 10)
	a.removeLegacy()
	if !isFile(filepath.Join(a.toolchainDir("node"), "node.exe")) {
		t.Error("the new bundled Node.js must survive the legacy sweep")
	}
}
