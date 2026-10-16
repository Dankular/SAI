package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestApp returns an app whose data lives in a temp directory, so tests never
// touch a real install.
func newTestApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a := &app{
		dataDir:   dir,
		workspace: filepath.Join(dir, "workspace"),
		statePath: filepath.Join(dir, "state.json"),
		ctx:       ctx,
		cancel:    cancel,
	}
	a.state = Snapshot{RecentLogs: []string{}}
	return a
}

// ---- setup stage table -------------------------------------------------------

func TestSetupStageTable(t *testing.T) {
	stages := setupStages()
	if len(stages) < 8 {
		t.Fatalf("expected a full set of stages, got %d", len(stages))
	}
	seen := map[string]bool{}
	sawRepeating := false
	resumable := 0
	for i, s := range stages {
		if s.label == "" || s.step == "" || s.run == nil {
			t.Errorf("stage %d is incomplete: %+v", i+1, s)
		}
		if seen[s.label] {
			t.Errorf("duplicate stage label %q", s.label)
		}
		seen[s.label] = true
		if s.resumable {
			resumable++
			if sawRepeating {
				t.Errorf("stage %d is resumable but follows a stage that runs every pass; resumable stages must come first", i+1)
			}
		} else {
			sawRepeating = true
		}
	}
	if got := lastResumableStage(); got != resumable {
		t.Errorf("lastResumableStage() = %d, want %d", got, resumable)
	}
	if len(stageLabels()) != len(stages) {
		t.Errorf("stage labels and stages disagree")
	}
}

// ---- persona files -----------------------------------------------------------

func TestPersonaTemplatesNameTheRightPeople(t *testing.T) {
	profile := Profile{UserName: "Sam", AgentName: "Ada", Purpose: "Plan my week", ReplyStyle: "concise"}
	for name, text := range map[string]string{
		"IDENTITY.md": identityFile("Ada", "Sam"),
		"SOUL.md":     soulFile("Ada", "Sam", profile),
		"rules":       rulesBlock("Ada", "Sam"),
	} {
		if strings.Contains(text, "%!") || strings.Contains(text, "%s") {
			t.Errorf("%s has a formatting error:\n%s", name, text)
		}
		if !strings.Contains(text, "Ada") || !strings.Contains(text, "Sam") {
			t.Errorf("%s should mention both the assistant and the user", name)
		}
	}
	identity := identityFile("Ada", "Sam")
	for _, want := range []string{"You are Ada", "The person talking to you is Sam", "never you", "MiniCPM-V"} {
		if !strings.Contains(identity, want) {
			t.Errorf("IDENTITY.md is missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(identity), "llama architecture") {
		t.Error("IDENTITY.md must not claim the model is Llama-based")
	}
}

func TestUpsertRulesKeepsOtherContentAndIsIdempotent(t *testing.T) {
	a := newTestApp(t)
	if err := os.MkdirAll(a.workspace, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.workspace, "AGENTS.md")
	original := "# AGENTS.md\n\nOpenClaw's own conventions stay.\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := a.upsertRules("Ada", "Sam"); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	if strings.Count(text, rulesBegin) != 1 || !strings.Contains(text, "OpenClaw's own conventions stay.") {
		t.Fatalf("rules block duplicated or existing content lost:\n%s", text)
	}
	if err := a.upsertRules("Bea", "Sam"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if strings.Count(string(data), rulesBegin) != 1 || !strings.Contains(string(data), "You are Bea") {
		t.Fatalf("rules block was not updated in place:\n%s", data)
	}
}

// Earlier releases left OpenClaw's long AGENTS.md template under our block. It is removed (it
// describes files and a startup process that no longer exist) and anything else the user wrote stays.
func TestUpsertRulesDropsTheOldOpenClawTemplate(t *testing.T) {
	a := newTestApp(t)
	_ = os.MkdirAll(a.workspace, 0700)
	path := filepath.Join(a.workspace, "AGENTS.md")
	legacy := rulesBlock("Ada", "Sam") + `
# AGENTS.md - Your Workspace

## First Run
If BOOTSTRAP.md exists, follow it.

## Session Startup
Use runtime-provided startup context first.
`
	_ = os.WriteFile(path, []byte(legacy), 0600)
	if err := a.upsertRules("Ada", "Sam"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "BOOTSTRAP") || strings.Contains(string(data), "Session Startup") || strings.Count(string(data), rulesBegin) != 1 {
		t.Errorf("legacy template survived or the block was lost:\n%s", data)
	}
}

func TestWritePersonaPreservesWhatTheAgentLearned(t *testing.T) {
	a := newTestApp(t)
	profile := Profile{UserName: "Sam", AgentName: "Ada", Purpose: "Plan my week", ReplyStyle: "balanced"}
	if err := a.writePersona(profile); err != nil {
		t.Fatal(err)
	}
	learned := "# User profile\n\n- Name: Sam\n- Always answer in metric units.\n"
	if err := os.WriteFile(filepath.Join(a.workspace, "USER.md"), []byte(learned), 0600); err != nil {
		t.Fatal(err)
	}
	// A user who took over IDENTITY.md (no marker, substantial) keeps their version.
	custom := strings.Repeat("my own identity text\n", 60)
	if err := os.WriteFile(filepath.Join(a.workspace, "IDENTITY.md"), []byte(custom), 0600); err != nil {
		t.Fatal(err)
	}
	profile.AgentName = "Bea"
	if err := a.writePersona(profile); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(a.workspace, "USER.md")); string(data) != learned {
		t.Error("USER.md was overwritten; what the agent learned must survive re-running setup")
	}
	if data, _ := os.ReadFile(filepath.Join(a.workspace, "IDENTITY.md")); string(data) != custom {
		t.Error("a user-edited IDENTITY.md was overwritten")
	}
	if data, _ := os.ReadFile(filepath.Join(a.workspace, "SOUL.md")); !strings.Contains(string(data), "Soul of Bea") {
		t.Error("SOUL.md should be regenerated from the new profile")
	}
}

// ---- llama.cpp / audio.cpp backend selection ---------------------------------

func fakeRelease(names ...string) *ghRelease {
	r := &ghRelease{Tag: "b1"}
	for _, n := range names {
		r.Assets = append(r.Assets, ghAsset{Name: n, Size: 1})
	}
	return r
}

func TestBackendAssets(t *testing.T) {
	release := fakeRelease(
		"llama-b1-bin-win-cpu-x64.zip", "llama-b1-bin-win-vulkan-x64.zip", "llama-b1-bin-win-sycl-x64.zip",
		"llama-b1-bin-win-cuda-12.4-x64.zip", "cudart-llama-bin-win-cuda-12.4-x64.zip",
	)
	cases := map[string]int{"cpu": 1, "vulkan": 1, "sycl": 1}
	for backend, want := range cases {
		assets, err := backendAssets(release, backend, 0, 0)
		if err != nil || len(assets) != want {
			t.Errorf("%s: got %d assets, err %v", backend, len(assets), err)
		}
	}
	assets, err := backendAssets(release, "cuda", 12, 4)
	if err != nil || len(assets) != 2 {
		t.Errorf("cuda should need the runtime and its cudart archive, got %d, err %v", len(assets), err)
	}
	if _, err := backendAssets(release, "cuda", 13, 4); err == nil {
		t.Error("expected an error when that CUDA build is not published")
	}
	if _, err := backendAssets(release, "metal", 0, 0); err == nil {
		t.Error("expected an error for an unknown backend")
	}
}

func TestPickLlamaReleaseSkipsReleasesWithoutWindowsBinaries(t *testing.T) {
	releases := []ghRelease{
		{Tag: "v0.7.0", Assets: []ghAsset{{Name: "source.tar.gz"}}},
		*fakeRelease("llama-b2-bin-win-cpu-x64.zip"),
		*fakeRelease("llama-b1-bin-win-cpu-x64.zip"),
	}
	releases[1].Tag, releases[2].Tag = "b2", "b1"
	if got := pickLlamaRelease(releases); got == nil || got.Tag != "b2" {
		t.Errorf("got %+v, want b2", got)
	}
	if pickLlamaRelease(releases[:1]) != nil {
		t.Error("a release with no Windows build must not be picked")
	}
}

func TestCudaBuildVersionPicksNewestSupported(t *testing.T) {
	release := fakeRelease("llama-b1-bin-win-cuda-12.4-x64.zip", "llama-b1-bin-win-cuda-13.4-x64.zip")
	for _, c := range []struct{ major, minor, wantMajor, wantMinor int }{
		{13, 4, 13, 4}, {13, 9, 13, 4}, {13, 0, 12, 4}, {12, 6, 12, 4}, {12, 3, 0, 0},
	} {
		major, minor := cudaBuildVersion(release, c.major, c.minor)
		if major != c.wantMajor || minor != c.wantMinor {
			t.Errorf("driver %d.%d: got %d.%d, want %d.%d", c.major, c.minor, major, minor, c.wantMajor, c.wantMinor)
		}
	}
}

func TestForcedBackendOverridesDetection(t *testing.T) {
	t.Setenv("SOVEREIGNAI_LLAMA_BACKEND", "cpu")
	t.Setenv("SOVEREIGNAI_AUDIO_BACKEND", "vulkan")
	a := newTestApp(t)
	if got := a.backendCandidates(); len(got) != 1 || got[0] != "cpu" {
		t.Errorf("llama backends = %v", got)
	}
	if got := a.audioBackendCandidates(); len(got) != 1 || got[0] != "vulkan" {
		t.Errorf("audio backends = %v", got)
	}
}

// ---- audio server config -----------------------------------------------------

func TestAudioConfigIncludesTurnModelAndVoicePreset(t *testing.T) {
	a := newTestApp(t)
	read := func() map[string]any {
		if err := a.writeAudioConfig("cuda13.3"); err != nil {
			t.Fatal(err)
		}
		var config map[string]any
		data, _ := os.ReadFile(a.audioConfig())
		if err := json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		return config
	}
	config := read()
	if config["backend"] != "cuda" || len(config["models"].([]any)) != 1 {
		t.Errorf("expected only OmniVoice on the cuda backend, got %v", config)
	}
	if _, has := config["models"].([]any)[0].(map[string]any)["default_voice_preset"]; has {
		t.Error("no preset expected before a reference voice exists")
	}
	mustWrite := func(path string) {
		_ = os.MkdirAll(filepath.Dir(path), 0700)
		_ = os.WriteFile(path, []byte("x"), 0600)
	}
	mustWrite(a.turnModel())
	mustWrite(filepath.Join(a.voicesDir(), "default.wav"))
	config = read()
	models := config["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("Smart Turn should be added to the same server, got %d models", len(models))
	}
	if _, has := models[0].(map[string]any)["default_voice_preset"]; !has {
		t.Error("the cloned-voice preset should be configured when a reference voice exists")
	}
}

// ---- small helpers -----------------------------------------------------------

func TestExtractZipRejectsPathEscapes(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "evil.zip")
	f, _ := os.Create(archive)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("../escaped.txt")
	_, _ = w.Write([]byte("nope"))
	_ = zw.Close()
	_ = f.Close()
	if err := extractZipAll(archive, filepath.Join(dir, "out")); err == nil {
		t.Error("a zip entry that escapes the destination must be rejected")
	}
	if _, err := os.Stat(filepath.Join(dir, "escaped.txt")); err == nil {
		t.Error("the escaping file was written")
	}
}

func TestGatewayErrorTextIsReadable(t *testing.T) {
	if got := modelErrorText(map[string]any{"message": "Context size has been exceeded"}); !strings.Contains(got, "too long") {
		t.Errorf("context overflow should have a friendly message, got %q", got)
	}
	if got := modelErrorText(map[string]any{"message": "internal error"}); strings.Contains(got, "map[") {
		t.Errorf("error objects must not be printed raw: %q", got)
	}
	if got := modelErrorText(nil); got == "" {
		t.Error("expected a fallback message")
	}
}

func TestCleanFieldAndMarkdownHelpers(t *testing.T) {
	if got := cleanField("  hello\x00 world  ", 8); got != "hello wo" {
		t.Errorf("cleanField = %q", got)
	}
	if got := mdLine("a\r\nb"); strings.ContainsAny(got, "\r\n") {
		t.Errorf("mdLine kept a newline: %q", got)
	}
}

func TestFirstPersonaInstallReplacesOpenClawTemplates(t *testing.T) {
	a := newTestApp(t)
	if err := os.MkdirAll(a.workspace, 0700); err != nil {
		t.Fatal(err)
	}
	// OpenClaw's baseline setup creates substantial template files with no marker.
	template := "# TEMPLATE\n" + strings.Repeat("placeholder text for you to fill in\n", 40)
	for _, name := range []string{"IDENTITY.md", "SOUL.md", "USER.md"} {
		if err := os.WriteFile(filepath.Join(a.workspace, name), []byte(template), 0600); err != nil {
			t.Fatal(err)
		}
	}
	profile := Profile{UserName: "Sam", AgentName: "Ada", Purpose: "Plan my week", ReplyStyle: "balanced"}
	if err := a.writePersona(profile); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"IDENTITY.md": "You are Ada", "SOUL.md": "Soul of Ada", "USER.md": "Name: Sam"} {
		data, _ := os.ReadFile(filepath.Join(a.workspace, name))
		if strings.Contains(string(data), "TEMPLATE") || !strings.Contains(string(data), want) {
			t.Errorf("%s: the first install must replace OpenClaw's template; got:\n%.200s", name, data)
		}
	}
	if !a.personaInstalled() {
		t.Fatal("the first install should be recorded")
	}
	// From now on what the agent writes to USER.md is kept.
	learned := "# User profile\n- Prefers metric units.\n"
	_ = os.WriteFile(filepath.Join(a.workspace, "USER.md"), []byte(learned), 0600)
	if err := a.writePersona(profile); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(a.workspace, "USER.md")); string(data) != learned {
		t.Error("USER.md must not be overwritten after the first install")
	}
}
