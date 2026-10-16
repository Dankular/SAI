package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- a tiny MCP server: this test binary, started with GO_WANT_MCP=1 ---------------------------------

func TestHelperMCPServer(t *testing.T) {
	if os.Getenv("GO_WANT_MCP") != "1" {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	playwrightNavigated := false
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			os.Exit(0)
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(line, &req) != nil || len(req.ID) == 0 {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "fake", "version": "1"}}
		case "tools/list":
			if os.Getenv("GO_WANT_PLAYWRIGHT_MCP") == "1" {
				result = map[string]any{"tools": []map[string]any{
					{"name": "browser_navigate", "description": "Navigate to a URL", "inputSchema": map[string]any{"type": "object", "required": []string{"url"}, "properties": map[string]any{"url": map[string]any{"type": "string"}}}},
					{"name": "browser_snapshot", "description": "Return an accessibility snapshot of the current page", "inputSchema": map[string]any{"type": "object"}},
				}}
			} else {
				result = map[string]any{"tools": []map[string]any{
					{"name": "convert_time", "description": "Convert a time between two time zones",
						"inputSchema": map[string]any{"type": "object", "required": []string{"time", "from_zone", "to_zone"}, "properties": map[string]any{
							"time": map[string]any{"type": "string"}, "from_zone": map[string]any{"type": "string"}, "to_zone": map[string]any{"type": "string"},
							"note": map[string]any{"type": "string"}}}},
					{"name": "ping", "description": "Check the server is alive", "inputSchema": map[string]any{"type": "object"}},
				}}
			}
		case "tools/call":
			if os.Getenv("GO_WANT_PLAYWRIGHT_MCP") == "1" && req.Params.Name == "browser_navigate" {
				playwrightNavigated = req.Params.Arguments["url"] == "https://en.wikipedia.org/wiki/Special:Random"
				result = map[string]any{"content": []map[string]any{{"type": "text", "text": "Navigated to Wikipedia"}}}
			} else if os.Getenv("GO_WANT_PLAYWRIGHT_MCP") == "1" && req.Params.Name == "browser_snapshot" && playwrightNavigated {
				result = map[string]any{"content": []map[string]any{{"type": "text", "text": `- heading "Honey" [level=1]
- paragraph: Honey stored in sealed containers can remain edible for thousands of years.`}}}
			} else if os.Getenv("GO_WANT_PLAYWRIGHT_MCP") == "1" && req.Params.Name == "browser_snapshot" {
				result = map[string]any{"content": []map[string]any{{"type": "text", "text": "No page has been opened"}}}
			} else {
				args, _ := json.Marshal(req.Params.Arguments)
				result = map[string]any{"content": []map[string]any{{"type": "text", "text": "ran " + req.Params.Name + " " + string(args)}}}
			}
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		fmt.Printf("%s\n", out)
	}
}

func configureFakeServer(t *testing.T, a *app, name string) {
	t.Helper()
	env := map[string]string{"GO_WANT_MCP": "1"}
	if strings.Contains(strings.ToLower(name), "playwright") {
		env["GO_WANT_PLAYWRIGHT_MCP"] = "1"
	}
	cfg := mcpConfigFile{Servers: map[string]mcpServerConfig{name: {
		Command: os.Args[0], Args: []string{"-test.run=TestHelperMCPServer"}, Env: env,
	}}}
	if err := a.writeMCPConfig(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.closeMCPServer(name) })
}

// --- install parsing ---------------------------------------------------------------------------------

func TestParseInstallReadsJSONBlocksAndShellLines(t *testing.T) {
	readme := "# Time server\n\n```json\n{\n  // add this to your client\n  \"mcpServers\": {\n    \"time\": {\n      \"command\": \"uvx\",\n      \"args\": [\"mcp-server-time\", \"--local-timezone=Europe/London\"],\n      \"env\": {\"TIME_API_KEY\": \"<your key>\"},\n    }\n  }\n}\n```\n\nOr from a shell:\n\n```bash\nnpx -y @acme/time-mcp --port 3000 && echo done\n```\n"
	options := parseMCPInstall(readme)
	if len(options) != 2 {
		t.Fatalf("options = %+v", options)
	}
	if options[0].Command != "uvx" || options[0].Args[0] != "mcp-server-time" || len(options[0].Env) != 1 || options[0].Env[0] != "TIME_API_KEY" {
		t.Errorf("json option = %+v", options[0])
	}
	if options[1].Command != "npx" || strings.Join(options[1].Args, " ") != "-y @acme/time-mcp --port 3000" {
		t.Errorf("shell option = %+v", options[1])
	}
}

func TestInstallerCommandsAreNotLaunchCommands(t *testing.T) {
	readme := "```\nnpx -y @smithery/cli install mcp-simple-timeserver --client claude\nnpx @mcp-get/cli install thing\nnpx create-react-app demo\n```\n```json\n{\"command\":\"npx\",\"args\":[\"-y\",\"@real/server\",\"/path/to/folder\"]}\n```"
	options := parseMCPInstall(readme)
	if len(options) != 1 || options[0].Args[1] != "@real/server" {
		t.Fatalf("only the real server should survive: %+v", options)
	}
	if len(options[0].Placeholders) != 1 || options[0].Placeholders[0] != "/path/to/folder" {
		t.Errorf("the example path must be flagged: %+v", options[0])
	}
	for _, o := range parseMCPInstall("```\ndocker run -i --rm acme/server\npython -m acme_server\n```") {
		if o.Supported() {
			t.Errorf("%s must not be offered as installable", o.Command)
		}
	}
}

func TestInstallTriesNextOptionAndAsksForWhatItLacks(t *testing.T) {
	a := newTestApp(t)
	_ = os.MkdirAll(a.workspace, 0700)
	useFakeToolchain(t, false)
	serverReadmeOverride = func(string) (string, error) {
		return "```json\n{\"command\":\"npx\",\"args\":[\"@acme/files\",\"/path/to/dir\"]}\n```\n", nil
	}
	t.Cleanup(func() { serverReadmeOverride = nil })
	out, err := a.installMCPServer(context.Background(), map[string]any{"url": "https://github.com/acme/files-mcp"})
	if err != nil || !strings.Contains(out, "/path/to/dir") || !strings.Contains(out, "Ask the user") {
		t.Fatalf("a placeholder must be handed back to the user: %q %v", out, err)
	}
	if cfg, _ := a.readMCPConfig(); len(cfg.Servers) != 0 {
		t.Errorf("nothing may be configured while input is missing: %v", cfg.Servers)
	}
	serverReadmeOverride = func(string) (string, error) {
		return "```json\n{\"mcpServers\":{\"x\":{\"command\":\"uvx\",\"args\":[\"acme-chat\"],\"env\":{\"ACME_API_KEY\":\"\"}}}}\n```\n", nil
	}
	out, err = a.installMCPServer(context.Background(), map[string]any{"url": "https://github.com/acme/chat-mcp"})
	if err != nil || !strings.Contains(out, "ACME_API_KEY") || !strings.Contains(out, "never make one up") {
		t.Fatalf("a credential must be requested, not invented: %q %v", out, err)
	}
	// a README with a working command second: the first failing option is rolled back and the next one used
	serverReadmeOverride = func(string) (string, error) {
		return "```json\n{\"command\":\"" + filepath.ToSlash(os.Args[0]) + "\",\"args\":[\"-test.run=NoSuchTest\"]}\n```\n" +
			"```bash\nuvx definitely-not-a-real-package-xyz\n```\n", nil
	}
	if _, err := a.installMCPServer(context.Background(), map[string]any{"url": "https://github.com/acme/broken-mcp"}); err == nil {
		t.Errorf("nothing started, so this must be an error")
	}
	if cfg, _ := a.readMCPConfig(); len(cfg.Servers) != 0 {
		t.Errorf("a server that did not start must be removed again: %v", cfg.Servers)
	}
}

func TestServerNameFromListing(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/andybrandt/mcp-simple-timeserver":   "simple-timeserver",
		"https://github.com/modelcontextprotocol/server-memory": "memory",
		"https://github.com/acme/Weather_MCP_Server.git":        "weather-mcp-server",
		"https://mcpservers.org/servers/foo/bar":                "bar",
	} {
		if got := serverNameFor(url); got != want {
			t.Errorf("%s -> %q, want %q", url, got, want)
		}
	}
}

// --- understanding the job ---------------------------------------------------------------------------

func TestNeedRestatementMustBeAboutTheJob(t *testing.T) {
	a := newTestApp(t)
	problem := "Look up the current time in Tokyo and convert between time zones"
	// the model parrots an example from somewhere else
	useFakeModel(t, textChunk(`{"capability":"read and query a PostgreSQL database","keywords":["sql","postgres"],"service":"postgres"}`))
	need := a.understandNeed(context.Background(), problem)
	if strings.Contains(strings.ToLower(need.Capability), "postgres") || need.Service != "" {
		t.Errorf("an off-topic restatement must be thrown away: %+v", need)
	}
	if len(need.Keywords) == 0 || !contains(need.Keywords, "tokyo") {
		t.Errorf("the fallback should use the user's own words: %+v", need)
	}
	// a good restatement with a synonym is kept, but an unnamed service is dropped
	useFakeModel(t, textChunk(`{"capability":"get current time and convert time zones","keywords":["timezone","clock","time"],"service":"google"}`))
	need = a.understandNeed(context.Background(), problem)
	if need.Capability != "get current time and convert time zones" || need.Service != "" || !contains(need.Keywords, "clock") {
		t.Errorf("need = %+v", need)
	}
}

// --- using a tool ------------------------------------------------------------------------------------

func TestGrammarSchemaKeepsWhatCanBeEnforced(t *testing.T) {
	schema := map[string]any{"type": "object", "required": []any{"path", "ghost"}, "properties": map[string]any{
		"path":  map[string]any{"type": "string", "description": "x", "format": "uri"},
		"mode":  map[string]any{"type": "string", "enum": []any{"r", "w"}},
		"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"deep":  map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "number"}}},
		"inner": map[string]any{"$ref": "#/definitions/inner"},
	}}
	out := grammarSchema(schema, 0)
	props := out["properties"].(map[string]any)
	if props["path"].(map[string]any)["type"] != "string" || props["mode"].(map[string]any)["enum"] == nil || props["tags"].(map[string]any)["items"] == nil {
		t.Errorf("schema = %v", out)
	}
	if _, bad := props["path"].(map[string]any)["format"]; bad {
		t.Errorf("unenforceable keywords must be dropped")
	}
	if req := out["required"].([]string); len(req) != 1 || req[0] != "path" {
		t.Errorf("only required fields that exist can stay required: %v", out["required"])
	}
}

func TestCleanArgumentsDropsFillerAndFindsGaps(t *testing.T) {
	schema := map[string]any{"required": []any{"time", "to_zone"}}
	cleaned, missing := cleanArguments(map[string]any{"time": "3pm", "to_zone": "", "note": "  ", "tags": []any{}, "n": 0.0}, schema)
	if len(missing) != 1 || missing[0] != "to_zone" {
		t.Errorf("missing = %v", missing)
	}
	if _, has := cleaned["note"]; has {
		t.Errorf("an empty optional field is filler: %v", cleaned)
	}
	if _, has := cleaned["tags"]; has {
		t.Errorf("an empty optional list is filler: %v", cleaned)
	}
	if cleaned["n"] != 0.0 {
		t.Errorf("a real zero must stay: %v", cleaned)
	}
}

func TestUseBuildsArgumentsUnderTheSchemaAndRemembers(t *testing.T) {
	a := newTestApp(t)
	_ = os.MkdirAll(a.workspace, 0700)
	configureFakeServer(t, a, "clock")
	model := useFakeModel(t, textChunk(`{"time":"15:00","from_zone":"Europe/London","to_zone":"Asia/Tokyo","note":""}`))
	out, err := a.useMCPTool(context.Background(), "clock", "convert_time", "convert 3pm London time to Tokyo time", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `ran convert_time {"from_zone":"Europe/London","time":"15:00","to_zone":"Asia/Tokyo"}`) || strings.Contains(out, "note") {
		t.Errorf("out = %q", out)
	}
	rf, _ := model.requests[0]["response_format"].(map[string]any)
	if rf == nil || rf["type"] != "json_schema" || model.requests[0]["tools"] != nil {
		t.Errorf("arguments must be decoded under the tool's schema and without tools: %v", rf)
	}
	// it remembers, and a similar job later is pointed at the same tool
	if uses := a.loadMCPUses(); len(uses) != 1 || uses[0].Tool != "convert_time" || uses[0].Count != 1 {
		t.Fatalf("uses = %+v", uses)
	}
	if prior := a.priorMCPUse("what is 9am in New York in Tokyo time", mcpNeed{Capability: "convert time between time zones", Keywords: []string{"time", "tokyo"}}); !strings.Contains(prior, "clock.convert_time") {
		t.Errorf("prior = %q", prior)
	}
	// ... and drafts a skill for the user to approve, without any model help
	proposals := a.listSkillProposals()
	if len(proposals) != 1 || !strings.Contains(proposals[0].Instructions, "server=clock, tool=convert_time") {
		t.Fatalf("proposals = %+v", proposals)
	}
	if _, err := a.readSkill(proposals[0].Name); err == nil {
		t.Errorf("a draft is not a skill until approved")
	}
}

func TestUseAsksInsteadOfInventingAMissingValue(t *testing.T) {
	a := newTestApp(t)
	_ = os.MkdirAll(a.workspace, 0700)
	configureFakeServer(t, a, "clock")
	useFakeModel(t, textChunk(`{"time":"15:00","from_zone":"","to_zone":""}`))
	out, err := a.useMCPTool(context.Background(), "clock", "convert_time", "convert 3pm for me", "")
	if err != nil || !strings.Contains(out, "from_zone, to_zone") || !strings.Contains(out, "Ask the user") {
		t.Fatalf("out = %q err = %v", out, err)
	}
	if uses := a.loadMCPUses(); len(uses) != 0 {
		t.Errorf("a call that was never made must not be remembered: %+v", uses)
	}
}

func TestUseWithNoInputsCallsDirectlyAndRejectsUnknownTools(t *testing.T) {
	a := newTestApp(t)
	_ = os.MkdirAll(a.workspace, 0700)
	configureFakeServer(t, a, "clock")
	model := useFakeModel(t, textChunk("unused"))
	out, err := a.useMCPTool(context.Background(), "clock", "ping", "check it is alive", "")
	if err != nil || !strings.Contains(out, "ran ping") || len(model.requests) != 0 {
		t.Errorf("a tool with no inputs needs no model call: %q %v requests=%d", out, err, len(model.requests))
	}
	if _, err := a.useMCPTool(context.Background(), "clock", "nope", "x", ""); err == nil {
		t.Errorf("an unknown tool must be an error")
	}
}

func TestFindPrefersAnInstalledToolOverTheMarketplace(t *testing.T) {
	a := newTestApp(t)
	_ = os.MkdirAll(a.workspace, 0700)
	configureFakeServer(t, a, "clock")
	useFakeModel(t, textChunk(`{"capability":"convert a time between time zones","keywords":["time","zone","convert"],"service":""}`))
	out, err := a.findMCPTool(context.Background(), "convert a time between two time zones", false)
	if err != nil || !strings.Contains(out, "ALREADY INSTALLED") || !strings.Contains(out, "clock.convert_time") {
		t.Fatalf("out = %q err = %v", out, err)
	}
	if strings.Contains(out, "Marketplace candidates") {
		t.Errorf("an installed tool means no marketplace search: %q", out)
	}
}

func TestBuiltinSkillIsSeededOnceAndSteersTheModel(t *testing.T) {
	a := newTestApp(t)
	a.seedBuiltinSkills()
	body, err := a.readSkill("find-mcp-tool")
	if err != nil || !strings.Contains(body, "action=find") || !strings.Contains(body, "action=install") || !strings.Contains(body, "action=use") {
		t.Fatalf("skill = %q err = %v", body, err)
	}
	path := filepath.Join(a.dataDir, "skills", "find-mcp-tool", "SKILL.md")
	if err := os.WriteFile(path, []byte("---\nname: find-mcp-tool\ndescription: my edit\n---\nmine"), 0600); err != nil {
		t.Fatal(err)
	}
	a.seedBuiltinSkills()
	if body, _ := a.readSkill("find-mcp-tool"); !strings.Contains(body, "mine") {
		t.Errorf("the user's edit must never be overwritten")
	}
	if !strings.Contains(a.skillIndex(), "find-mcp-tool") {
		t.Errorf("the skill must be listed to the model")
	}
}

// useFakeEmbeddings serves hashed bag-of-words vectors: texts that share words are close.
func useFakeEmbeddings(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []item
		for i, text := range in.Input {
			vec := make([]float32, 128)
			for word := range significantWords(text) {
				h := 0
				for _, c := range word {
					h = (h*31 + int(c)) % 128
				}
				vec[h] += 1
			}
			vec[0] += 0.001 // never a zero vector
			data = append(data, item{Index: i, Embedding: vec})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	old := embedEndpoint
	embedEndpoint = server.URL
	t.Cleanup(func() { embedEndpoint = old; server.Close() })
}

func TestMeaningBeatsRepeatedKeywords(t *testing.T) {
	a := newTestApp(t)
	useFakeEmbeddings(t)
	all := []marketplaceCandidate{
		{Name: "big-pdf-toolkit", Description: "pdf pdf pdf utilities for pdf files, merge pdf, split pdf", URL: "https://github.com/a/b"},
		{Name: "doc-reader", Description: "extract plain text from PDF documents", URL: "https://github.com/a/c"},
	}
	ranked := a.rankMCPCandidates(context.Background(), "turn a PDF into plain text I can read", mcpNeed{Capability: "extract plain text from pdf", Keywords: []string{"pdf", "text"}}, all)
	if len(ranked) != 2 || ranked[0].Name != "doc-reader" {
		t.Fatalf("the listing that does the job should lead: %+v", ranked)
	}
}
