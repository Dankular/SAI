package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const sampleNetstat = `
Active Connections

  Proto  Local Address          Foreign Address        State           PID
  TCP    0.0.0.0:135            0.0.0.0:0              LISTENING       1180
  TCP    127.0.0.1:19432        0.0.0.0:0              LISTENING       4001
  TCP    127.0.0.1:19433        0.0.0.0:0              LISTENING       4002
  TCP    127.0.0.1:19434        127.0.0.1:49154        TIME_WAIT       0
  TCP    127.0.0.1:19434        0.0.0.0:0              LISTENING       4003
  TCP    192.168.1.5:19432      0.0.0.0:0              LISTENING       9999
  TCP    127.0.0.1:8080         0.0.0.0:0              LISTENING       5000
`

func TestListeningOwnersOnlyReadsLoopbackListenersOnOurPorts(t *testing.T) {
	owners := listeningOwners(sampleNetstat, []int{19432, 19433, 19434})
	got := map[int]int{}
	for _, o := range owners {
		got[o.port] = o.pid
	}
	if len(owners) != 3 || got[19432] != 4001 || got[19433] != 4002 || got[19434] != 4003 {
		t.Errorf("owners = %+v", owners)
	}
}

func TestOnlyProcessesInsideThisInstallMayBeStopped(t *testing.T) {
	root := filepath.Join("C:", `\Users`, "me", "AppData", "Local", "SovereignAI", "runtime")
	cases := map[string]bool{
		filepath.Join(root, "llama", "cuda", "llama-server.exe"):                                   true,
		filepath.Join(root, "audio", "audio.exe"):                                                  true,
		strings.ToUpper(filepath.Join(root, "llama", "llama-server.exe")):                          true, // Windows paths ignore case
		filepath.Join(filepath.Dir(root), "other-install", "runtime", "llama", "llama-server.exe"): false,
		filepath.Join(root+"-copy", "llama-server.exe"):                                            false, // a sibling with the same prefix
		filepath.Join("C:", `\Program Files`, "Apache", "httpd.exe"):                               false,
		"": false,
	}
	for path, want := range cases {
		if got := pathInside(root, path); got != want {
			t.Errorf("pathInside(%q) = %v, want %v", path, got, want)
		}
	}
	if pathInside("", filepath.Join(root, "x")) {
		t.Error("with no root nothing is inside it")
	}
}

func TestProcessImagePathNamesTheRunningProgram(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	got := processImagePath(os.Getpid())
	want, _ := os.Executable()
	if got == "" || !strings.EqualFold(filepath.Clean(got), filepath.Clean(want)) {
		t.Errorf("image path = %q, want %q", got, want)
	}
	if processImagePath(0x7ffffff0) != "" {
		t.Error("a process that does not exist has no image path")
	}
}
