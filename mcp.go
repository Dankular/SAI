package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const mcpProtocolVersion = "2024-11-05"

var (
	mcpNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	mcpEnvRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type mcpServerConfig struct {
	Command          string            `json:"command"`
	Args             []string          `json:"args,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	WorkingDirectory string            `json:"working_directory,omitempty"`
}

type mcpConfigFile struct {
	Servers map[string]mcpServerConfig `json:"mcpServers"`
}

type mcpToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

type mcpRuntime struct {
	mu      sync.Mutex
	clients map[string]*mcpClient
}

type mcpClient struct {
	name    string
	sig     string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	cancel  context.CancelFunc
	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  atomic.Uint64
	pending map[string]chan mcpResponse
	done    chan struct{}
	doneErr error
	stderr  boundedOutput
}

type mcpResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (a *app) mcpConfigPath() string { return filepath.Join(a.workspace, ".mcp.json") }

func (a *app) readMCPConfig() (mcpConfigFile, error) {
	cfg := mcpConfigFile{Servers: map[string]mcpServerConfig{}}
	data, err := os.ReadFile(a.mcpConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("read .mcp.json: %w", err)
	}
	if cfg.Servers == nil {
		cfg.Servers = map[string]mcpServerConfig{}
	}
	return cfg, nil
}

func (a *app) writeMCPConfig(cfg mcpConfigFile) error {
	if err := os.MkdirAll(a.workspace, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	path := a.mcpConfigPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	a.memoryDirty()
	return nil
}

func (a *app) mcpConfigSignature(cfg mcpServerConfig) string {
	data, _ := json.Marshal(cfg)
	return hashText(string(data))
}

func (a *app) mcpClientFor(ctx context.Context, name string) (*mcpClient, error) {
	cfg, err := a.readMCPConfig()
	if err != nil {
		return nil, err
	}
	server, ok := cfg.Servers[name]
	if !ok {
		return nil, errors.New("there is no configured extension server with that name")
	}
	if !mcpNameRE.MatchString(name) || strings.TrimSpace(server.Command) == "" {
		return nil, errors.New("the extension server configuration is invalid")
	}
	signature := a.mcpConfigSignature(server)
	a.agent.extensions.mu.Lock()
	if a.agent.extensions.clients == nil {
		a.agent.extensions.clients = make(map[string]*mcpClient)
	}
	if old := a.agent.extensions.clients[name]; old != nil && old.sig == signature {
		a.agent.extensions.mu.Unlock()
		return old, nil
	} else if old != nil {
		old.close()
		delete(a.agent.extensions.clients, name)
	}
	client, err := a.startMCPClient(ctx, name, server, signature)
	if err == nil {
		a.agent.extensions.clients[name] = client
	}
	a.agent.extensions.mu.Unlock()
	return client, err
}

func (a *app) startMCPClient(ctx context.Context, name string, cfg mcpServerConfig, signature string) (*mcpClient, error) {
	workingDir := a.workspace
	if cfg.WorkingDirectory != "" {
		resolved, inside := a.resolvePath(cfg.WorkingDirectory)
		if !inside {
			return nil, errors.New("extension working directory must stay inside the workspace")
		}
		workingDir = resolved
	}
	command, err := a.resolveExtensionCommand(cfg.Command)
	if err != nil {
		return nil, err
	}
	procCtx, cancel := context.WithCancel(a.ctx)
	cmd := exec.CommandContext(procCtx, command, cfg.Args...)
	cmd.Dir = workingDir
	cmd.Env = a.extensionEnvironment(cfg.Env)
	cmd.WaitDelay = 2 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	client := &mcpClient{
		name: name, sig: signature, cmd: cmd, stdin: stdin, cancel: cancel,
		pending: make(map[string]chan mcpResponse), done: make(chan struct{}),
	}
	cmd.Stderr = &client.stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start extension %s: %w", name, err)
	}
	go client.readResponses(stdout)
	go func() {
		err := cmd.Wait()
		if err != nil {
			client.fail(fmt.Errorf("extension process stopped: %w", err))
		} else {
			client.fail(errors.New("extension process stopped"))
		}
	}()

	initCtx, initCancel := context.WithTimeout(ctx, 150*time.Second) // the first npx or uvx run downloads the package
	defer initCancel()
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	result, err := client.request(initCtx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "sovereign-ai", "version": "1.0"},
	})
	if err != nil {
		detail := strings.TrimSpace(clipText(client.stderr.String(), 500))
		client.close()
		if detail != "" {
			return nil, fmt.Errorf("initialize extension %s: %w (it said: %s)", name, err, detail)
		}
		return nil, fmt.Errorf("initialize extension %s: %w", name, err)
	}
	if err := json.Unmarshal(result, &initialized); err != nil {
		client.close()
		return nil, errors.New("extension returned an invalid initialize response")
	}
	if initialized.ProtocolVersion == "" {
		client.close()
		return nil, errors.New("extension did not report its MCP protocol version")
	}
	if err := client.notify("notifications/initialized", map[string]any{}); err != nil {
		client.close()
		return nil, err
	}
	return client, nil
}

func (a *app) extensionEnvironment(extra map[string]string) []string {
	var env []string
	if a.workspaceUserlandReady() {
		env = a.userlandEnv()
	} else {
		env = a.withToolchain(a.runtimeEnv())
	}
	for key, value := range extra {
		if !mcpEnvRE.MatchString(key) || strings.EqualFold(key, "PATH") || strings.EqualFold(key, "SYSTEMROOT") || strings.EqualFold(key, "WINDIR") || strings.EqualFold(key, "COMSPEC") {
			continue
		}
		env = setEnvValue(env, key, value)
	}
	return env
}

func (c *mcpClient) readResponses(stdout interface{ Read([]byte) (int, error) }) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for scanner.Scan() {
		var envelope struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &envelope) != nil || len(envelope.ID) == 0 || bytes.Equal(envelope.ID, []byte("null")) {
			continue // ignore server notifications and non-JSON startup noise
		}
		key := string(envelope.ID)
		c.mu.Lock()
		pending := c.pending[key]
		delete(c.pending, key)
		c.mu.Unlock()
		if pending != nil {
			pending <- mcpResponse{Result: envelope.Result, Error: envelope.Error}
		}
	}
	if err := scanner.Err(); err != nil {
		c.fail(fmt.Errorf("read extension response: %w", err))
	} else {
		c.fail(errors.New("extension closed its output"))
	}
}

func (c *mcpClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := fmt.Sprint(c.nextID.Add(1))
	channel := make(chan mcpResponse, 1)
	c.mu.Lock()
	select {
	case <-c.done:
		err := c.doneErr
		c.mu.Unlock()
		return nil, err
	default:
	}
	c.pending[id] = channel
	c.mu.Unlock()
	message, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": method, "params": params})
	if err == nil {
		err = c.writeMessage(message)
	}
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case response := <-channel:
		if response.Error != nil {
			return nil, fmt.Errorf("MCP error %d: %s", response.Error.Code, response.Error.Message)
		}
		return response.Result, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-c.done:
		c.mu.Lock()
		err := c.doneErr
		c.mu.Unlock()
		return nil, err
	}
}

func (c *mcpClient) notify(method string, params any) error {
	message, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return err
	}
	return c.writeMessage(message)
}

func (c *mcpClient) writeMessage(data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write to extension: %w", err)
	}
	return nil
}

func (c *mcpClient) fail(err error) {
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return
	default:
	}
	c.doneErr = err
	close(c.done)
	for id, pending := range c.pending {
		delete(c.pending, id)
		pending <- mcpResponse{Error: &struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}{Code: -1, Message: err.Error()}}
	}
	c.mu.Unlock()
}

func (c *mcpClient) close() {
	c.cancel()
	_ = c.stdin.Close()
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
	}
}

func (c *mcpClient) listTools(ctx context.Context) ([]mcpToolDefinition, error) {
	var tools []mcpToolDefinition
	cursor := ""
	for page := 0; page < 8; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, err := c.request(ctx, "tools/list", params)
		if err != nil {
			return tools, err
		}
		var response struct {
			Tools      []mcpToolDefinition `json:"tools"`
			NextCursor string              `json:"nextCursor"`
		}
		if err := json.Unmarshal(result, &response); err != nil {
			return tools, errors.New("extension returned an invalid tools/list response")
		}
		tools = append(tools, response.Tools...)
		if response.NextCursor == "" {
			break
		}
		cursor = response.NextCursor
	}
	return tools, nil
}

func (c *mcpClient) callTool(ctx context.Context, name string, arguments map[string]any) (string, error) {
	result, err := c.request(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		return "", err
	}
	var response struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return string(result), nil
	}
	var content []string
	for _, block := range response.Content {
		if block.Type == "text" && block.Text != "" {
			content = append(content, block.Text)
		} else if block.Type != "" {
			content = append(content, "["+block.Type+" content]")
		}
	}
	text := strings.Join(content, "\n")
	if response.IsError {
		return text, errors.New("the extension reported that its tool call failed")
	}
	if text == "" {
		text = "(extension returned no text)"
	}
	return text, nil
}

func (a *app) closeMCPServer(name string) {
	a.agent.extensions.mu.Lock()
	client := a.agent.extensions.clients[name]
	delete(a.agent.extensions.clients, name)
	a.agent.extensions.mu.Unlock()
	if client != nil {
		client.close()
	}
}

func toolExtension(ctx context.Context, a *app, args map[string]any) (string, error) {
	action := strings.ToLower(strings.TrimSpace(argString(args, "action")))
	name := strings.TrimSpace(argString(args, "server"))
	switch action {
	case "find":
		return a.findMCPTool(ctx, argString(args, "query"), true)
	case "install":
		return a.installMCPServer(ctx, args)
	case "use":
		return a.useMCPTool(ctx, name, argString(args, "tool"), argString(args, "goal"), argString(args, "context"))
	case "discover":
		return a.discoverMCPServers(ctx, argString(args, "query"))
	case "inspect":
		return a.inspectMCPServer(ctx, argString(args, "url"))
	case "list":
		cfg, err := a.readMCPConfig()
		if err != nil {
			return "", err
		}
		if len(cfg.Servers) == 0 {
			return "No MCP extension servers are configured. Use action=configure to add one.", nil
		}
		var b strings.Builder
		for _, serverName := range sortedKeys(cfg.Servers) {
			fmt.Fprintf(&b, "%s: %s\n", serverName, cfg.Servers[serverName].Command)
		}
		return b.String(), nil
	case "configure":
		if !mcpNameRE.MatchString(name) {
			return "", errors.New("server must use 1-64 letters, numbers, underscores, or hyphens")
		}
		command := strings.TrimSpace(argString(args, "command"))
		if command == "" {
			return "", errors.New("a server command is required")
		}
		server := mcpServerConfig{Command: command, WorkingDirectory: strings.TrimSpace(argString(args, "working_directory"))}
		if values, ok := args["args"].([]any); ok {
			for _, value := range values {
				if text, ok := value.(string); ok {
					server.Args = append(server.Args, text)
				}
			}
		}
		if values, ok := args["env"].(map[string]any); ok {
			server.Env = make(map[string]string)
			for key, value := range values {
				if text, ok := value.(string); ok && mcpEnvRE.MatchString(key) && !strings.EqualFold(key, "PATH") && !strings.EqualFold(key, "SYSTEMROOT") && !strings.EqualFold(key, "WINDIR") && !strings.EqualFold(key, "COMSPEC") {
					server.Env[key] = text
				}
			}
		}
		cfg, err := a.readMCPConfig()
		if err != nil {
			return "", err
		}
		cfg.Servers[name] = server
		if err := a.writeMCPConfig(cfg); err != nil {
			return "", err
		}
		a.closeMCPServer(name)
		return "Configured MCP extension server " + name + ".", nil
	case "remove":
		cfg, err := a.readMCPConfig()
		if err != nil {
			return "", err
		}
		if _, exists := cfg.Servers[name]; !exists {
			return "", errors.New("there is no configured server with that name")
		}
		delete(cfg.Servers, name)
		if err := a.writeMCPConfig(cfg); err != nil {
			return "", err
		}
		a.closeMCPServer(name)
		return "Removed MCP extension server " + name + ".", nil
	case "search":
		return a.searchMCPTools(ctx, argString(args, "query"))
	case "describe":
		if !mcpNameRE.MatchString(name) {
			return "", errors.New("give a configured server name")
		}
		toolName := strings.TrimSpace(argString(args, "tool"))
		if toolName == "" {
			return "", errors.New("give the exact MCP tool name to describe")
		}
		client, err := a.mcpClientFor(ctx, name)
		if err != nil {
			return "", err
		}
		tools, err := client.listTools(ctx)
		if err != nil {
			return "", err
		}
		for _, tool := range tools {
			if tool.Name != toolName {
				continue
			}
			input, _ := json.Marshal(tool.InputSchema)
			return fmt.Sprintf("%s.%s\n%s\ninput_schema=%s", name, tool.Name, clipText(tool.Description, 400), clipText(string(input), 3500)), nil
		}
		return "", errors.New("that tool is not offered by the configured server; search its tools again")
	case "call":
		if !mcpNameRE.MatchString(name) {
			return "", errors.New("give a configured server name")
		}
		toolName := strings.TrimSpace(argString(args, "tool"))
		if toolName == "" {
			return "", errors.New("give the MCP tool name to call")
		}
		client, err := a.mcpClientFor(ctx, name)
		if err != nil {
			return "", err
		}
		tools, err := client.listTools(ctx)
		if err != nil {
			return "", err
		}
		found := false
		for _, tool := range tools {
			found = found || tool.Name == toolName
		}
		if !found {
			return "", errors.New("that tool is not offered by the configured server; search its tools again")
		}
		arguments, _ := args["arguments"].(map[string]any)
		if arguments == nil {
			arguments = map[string]any{}
		}
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		result, err := client.callTool(callCtx, toolName, arguments)
		if err != nil {
			return "", err
		}
		return clipText(result, 8000), nil
	default:
		return "", errors.New("action must be find, install, use, discover, inspect, list, configure, search, describe, call, or remove")
	}
}

func (a *app) searchMCPTools(ctx context.Context, query string) (string, error) {
	cfg, err := a.readMCPConfig()
	if err != nil {
		return "", err
	}
	if len(cfg.Servers) == 0 {
		return "No MCP extension servers are configured. Use action=configure to add one.", nil
	}
	type match struct {
		server string
		tool   mcpToolDefinition
		score  float64
	}
	var matches []match
	var failures []string
	for _, name := range sortedKeys(cfg.Servers) {
		client, err := a.mcpClientFor(ctx, name)
		if err != nil {
			failures = append(failures, name+": "+err.Error())
			continue
		}
		tools, err := client.listTools(ctx)
		if err != nil {
			failures = append(failures, name+": "+err.Error())
			continue
		}
		for _, tool := range tools {
			score := wordScore(query, tool.Name+" "+tool.Description)
			if strings.TrimSpace(query) == "" || score > 0 {
				matches = append(matches, match{server: name, tool: tool, score: score})
			}
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score == matches[j].score {
			if matches[i].server == matches[j].server {
				return matches[i].tool.Name < matches[j].tool.Name
			}
			return matches[i].server < matches[j].server
		}
		return matches[i].score > matches[j].score
	})
	if len(matches) > 5 {
		matches = matches[:5]
	}
	var b strings.Builder
	for _, item := range matches {
		fmt.Fprintf(&b, "server=%s tool=%s\n%s\n\n", item.server, item.tool.Name, clipText(item.tool.Description, 240))
	}
	if len(matches) == 0 {
		b.WriteString("No matching extension tools were found.\n")
	}
	for _, failure := range failures {
		fmt.Fprintf(&b, "Could not inspect %s\n", failure)
	}
	return clipText(b.String(), 2800), nil
}
