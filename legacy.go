package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Cleaning up what earlier releases left behind. Two kinds of leftovers:
//
//   - inside the app's own folder (the private Node.js, OpenClaw, its caches, and the old Agents-A1
//     chat model): deleted outright by removeLegacy in upgrade.go;
//   - outside it, where OpenClaw (earlier called Clawdbot and Moltbot) put its config, caches and global
//     npm package: not ours to delete, so they are RETIRED. Each is checked to be what it claims to be,
//     moved into <data>\retired with a note of where it came from, and only deleted for good after
//     retiredKeepFor. Until then everything can be put back with restoreRetired.
//
// Nothing is touched while an OpenClaw gateway is still answering, or if SOVEREIGNAI_KEEP_LEGACY is set.

const retiredKeepFor = 14 * 24 * time.Hour

var openClawNames = []string{"openclaw", "clawdbot", "moltbot"}

// legacyModelGlobs match the old chat model (Agents-A1) in any quantisation, including partial downloads.
var legacyModelGlobs = []string{"Agents-A1*"}

type retiredEntry struct {
	From string `json:"from"`
	To   string `json:"to"`
	At   string `json:"at"`
}

func (a *app) retiredDir() string { return filepath.Join(a.dataDir, "retired") }

// legacyRoots are the three places OpenClaw wrote outside the app's folder. They come from the
// environment so tests can point them at a scratch folder.
func legacyRoots() (home, roaming, local string) {
	home, _ = os.UserHomeDir()
	return home, os.Getenv("APPDATA"), os.Getenv("LOCALAPPDATA")
}

type retireTarget struct {
	path  string
	label string
	check func(string) bool
}

func (a *app) externalLegacyTargets() []retireTarget {
	home, roaming, local := legacyRoots()
	var targets []retireTarget
	for _, name := range openClawNames {
		if home != "" {
			targets = append(targets, retireTarget{filepath.Join(home, "."+name), name + "-config", isOpenClawHome})
		}
		if roaming != "" {
			targets = append(targets,
				retireTarget{filepath.Join(roaming, "npm", "node_modules", name), name + "-npm-package", isOpenClawPackage(name)},
				retireTarget{filepath.Join(roaming, "npm", name), name + "-shim", isOpenClawShim(name)},
				retireTarget{filepath.Join(roaming, "npm", name+".cmd"), name + "-shim-cmd", isOpenClawShim(name)},
				retireTarget{filepath.Join(roaming, "npm", name+".ps1"), name + "-shim-ps1", isOpenClawShim(name)},
			)
		}
		if local != "" {
			targets = append(targets, retireTarget{filepath.Join(local, name), name + "-cache", isPlainDir})
		}
	}
	return targets
}

func isPlainDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

// isOpenClawHome accepts a dot-folder that holds what OpenClaw keeps there.
func isOpenClawHome(path string) bool {
	if !isPlainDir(path) {
		return false
	}
	for _, marker := range []string{"openclaw.json", "clawdbot.json", "moltbot.json", "agents", "devices", "cron", "credentials", "workspace", "sessions", "gateway.log"} {
		if _, err := os.Stat(filepath.Join(path, marker)); err == nil {
			return true
		}
	}
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) == 0 // an empty one is only clutter
}

// isOpenClawPackage accepts the global npm package folder when it is that package, or an empty husk
// left by a half-finished uninstall.
func isOpenClawPackage(name string) func(string) bool {
	return func(path string) bool {
		if !isPlainDir(path) {
			return false
		}
		data, err := os.ReadFile(filepath.Join(path, "package.json"))
		if err != nil {
			return true // no package.json: a husk, nothing else could be using it
		}
		var pkg struct {
			Name string `json:"name"`
		}
		return json.Unmarshal(data, &pkg) == nil && strings.EqualFold(pkg.Name, name)
	}
}

// isOpenClawShim accepts the small launcher scripts npm writes beside global packages, only when they
// really launch that package.
func isOpenClawShim(name string) func(string) bool {
	return func(path string) bool {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
			return false
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		text := strings.ToLower(string(data))
		return strings.Contains(text, "node_modules/"+name) || strings.Contains(text, `node_modules\`+name)
	}
}

// openClawRunning reports whether an OpenClaw gateway is answering on its usual ports.
func openClawRunning() bool {
	for _, port := range []string{"18789", "18790"} {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 300*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
	}
	return false
}

var gatewayRunning = openClawRunning // replaced in tests

func (a *app) loadRetired() []retiredEntry {
	var entries []retiredEntry
	if data, err := os.ReadFile(filepath.Join(a.retiredDir(), "manifest.json")); err == nil {
		_ = json.Unmarshal(data, &entries)
	}
	return entries
}

func (a *app) saveRetired(entries []retiredEntry) {
	if len(entries) == 0 {
		_ = os.RemoveAll(a.retiredDir())
		return
	}
	data, err := json.MarshalIndent(entries, "", " ")
	if err != nil || os.MkdirAll(a.retiredDir(), 0700) != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(a.retiredDir(), "manifest.json"), data, 0600)
	_ = os.WriteFile(filepath.Join(a.retiredDir(), "README.txt"), []byte("Things the assistant moved out of the way because they were left over from OpenClaw.\n"+
		"They are deleted for good "+fmt.Sprint(int(retiredKeepFor.Hours()/24))+" days after they were moved. manifest.json says where each came from;\n"+
		"move a folder back there to restore it.\n"), 0600)
}

// retireExternalLegacy moves verified OpenClaw leftovers out of the user's profile.
func (a *app) retireExternalLegacy() (moved []string) {
	if os.Getenv("SOVEREIGNAI_KEEP_LEGACY") != "" {
		return nil
	}
	var present []retireTarget
	for _, t := range a.externalLegacyTargets() {
		if _, err := os.Lstat(t.path); err == nil {
			present = append(present, t)
		}
	}
	if len(present) == 0 {
		return nil
	}
	if gatewayRunning() {
		a.recordCommandOutput("OpenClaw is still running on this computer, so its leftover files were not touched")
		return nil
	}
	entries := a.loadRetired()
	stamp := time.Now().Format("20060102-150405")
	for _, t := range present {
		if !t.check(t.path) {
			continue
		}
		dest := filepath.Join(a.retiredDir(), stamp, t.label)
		if os.MkdirAll(filepath.Dir(dest), 0700) != nil {
			continue
		}
		if err := os.Rename(t.path, dest); err != nil {
			a.recordCommandOutput("Could not move " + t.path + " out of the way yet (" + clipText(err.Error(), 120) + "); it will be tried again next time")
			continue
		}
		entries = append(entries, retiredEntry{From: t.path, To: dest, At: time.Now().UTC().Format(time.RFC3339)})
		moved = append(moved, t.path)
	}
	if len(moved) > 0 {
		a.saveRetired(entries)
		a.recordCommandOutput("Moved " + fmt.Sprint(len(moved)) + " leftover OpenClaw item(s) out of your profile; they are deleted for good after " + fmt.Sprint(int(retiredKeepFor.Hours()/24)) + " days (see " + a.retiredDir() + ")")
	}
	return moved
}

// purgeRetired deletes retired items that have been waiting long enough.
func (a *app) purgeRetired(now time.Time) (purged int) {
	entries := a.loadRetired()
	if len(entries) == 0 {
		return 0
	}
	kept := entries[:0:0]
	for _, e := range entries {
		at, err := time.Parse(time.RFC3339, e.At)
		if err == nil && now.Sub(at) < retiredKeepFor {
			kept = append(kept, e)
			continue
		}
		if strings.HasPrefix(filepath.Clean(e.To), filepath.Clean(a.retiredDir())+string(os.PathSeparator)) { // never delete outside the holding folder
			_ = os.RemoveAll(e.To)
			_ = os.Remove(filepath.Dir(e.To)) // the timestamp folder, once empty
			purged++
		}
	}
	if purged > 0 {
		a.saveRetired(kept)
	}
	return purged
}

// restoreRetired puts every retired item back where it came from, unless something is there now.
func (a *app) restoreRetired() (restored int) {
	entries := a.loadRetired()
	kept := entries[:0:0]
	for _, e := range entries {
		if _, err := os.Lstat(e.From); err == nil {
			kept = append(kept, e) // something new lives there; leave both alone
			continue
		}
		if os.MkdirAll(filepath.Dir(e.From), 0700) == nil && os.Rename(e.To, e.From) == nil {
			restored++
			continue
		}
		kept = append(kept, e)
	}
	a.saveRetired(kept)
	return restored
}

// cleanLegacy is run on every start, once, off the main path.
func (a *app) cleanLegacy() {
	a.retireExternalLegacy()
	if n := a.purgeRetired(time.Now()); n > 0 {
		a.recordCommandOutput("Deleted " + fmt.Sprint(n) + " retired OpenClaw item(s) that had waited long enough")
	}
}
