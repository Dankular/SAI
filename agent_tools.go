package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// agentTool is one thing the model can do. Guard returns a plain-language description when
// the action needs the user's approval first, or "" when it may go ahead.
type agentTool struct {
	Name        string
	Description string
	Parameters  map[string]any
	Guard       func(a *app, args map[string]any) string
	Run         func(ctx context.Context, a *app, args map[string]any) (string, error)
	// Terminal tools finish the turn: their result is the reply, so the small model cannot ramble
	// or contradict it afterwards. A failed or refused call still goes back to the model.
	Terminal bool
}

func obj(required []string, props map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

func argString(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func argInt(args map[string]any, key string, fallback int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case string:
		var n int
		if _, err := fmt.Sscan(v, &n); err == nil {
			return n
		}
	}
	return fallback
}

func (a *app) agentTools() []agentTool {
	outside := func(verb string) func(a *app, args map[string]any) string {
		return func(a *app, args map[string]any) string {
			if _, inside := a.resolvePath(argString(args, "path")); !inside {
				return verb + " " + argString(args, "path") + " (outside the assistant's workspace)"
			}
			return ""
		}
	}
	tools := []agentTool{
		{
			Name:        "shell",
			Description: "Run a POSIX shell command in the private workspace userland. The user approves each command.",
			Parameters:  obj([]string{"command"}, map[string]any{"command": str("The command line to run"), "timeout_seconds": num("Give up after this many seconds (default 60, at most 300)")}),
			Guard: func(a *app, args map[string]any) string {
				return "Run this command: " + clipText(argString(args, "command"), 300)
			},
			Run: toolShell,
		},
		{
			Name:        "process",
			Description: "Start, inspect, stop, or list long-running workspace commands.",
			Parameters: obj([]string{"action"}, map[string]any{
				"action":          map[string]any{"type": "string", "enum": []string{"start", "poll", "terminate", "list"}, "description": "What to do"},
				"command":         str("Command to start"),
				"process_id":      str("ID returned by start"),
				"timeout_seconds": num("Stop after this many seconds (default 600, max 3600)"),
			}),
			Guard: func(a *app, args map[string]any) string {
				switch argString(args, "action") {
				case "start":
					return "Run this workspace command: " + clipText(argString(args, "command"), 300)
				case "terminate":
					return "Stop process " + argString(args, "process_id")
				}
				return ""
			},
			Run: toolProcess,
		},
		{
			Name:        "extension",
			Description: "Extend yourself with MCP tools. find: check installed tools, then the marketplace, for a job (query). install a found server (url). use a tool for a goal (server, tool, goal).",
			Parameters: obj([]string{"action"}, map[string]any{
				"action":            map[string]any{"type": "string", "enum": []string{"find", "install", "use", "discover", "inspect", "list", "configure", "search", "describe", "call", "remove"}, "description": "What to do"},
				"goal":              str("use: what you want done, in plain words"),
				"context":           str("use: facts the tool needs, such as names or addresses the user gave"),
				"server":            str("Extension server name"),
				"command":           str("Program to launch for a server"),
				"args":              map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Program arguments"},
				"env":               map[string]any{"type": "object", "description": "Server environment variables"},
				"working_directory": str("Optional workspace-relative server directory"),
				"query":             str("Capability to find or search configured tools for"),
				"url":               str("Marketplace candidate URL to inspect"),
				"tool":              str("Exact MCP tool name returned by search"),
				"arguments":         map[string]any{"type": "object", "description": "Arguments for the selected MCP tool"},
			}),
			Guard: func(a *app, args map[string]any) string {
				switch argString(args, "action") {
				case "install":
					return "Install and start the MCP server from " + clipText(argString(args, "url"), 200)
				case "use":
					return "Use MCP tool " + argString(args, "server") + "." + argString(args, "tool") + " to: " + clipText(argString(args, "goal"), 200)
				case "configure":
					return "Configure MCP server " + argString(args, "server") + " to run " + clipText(argString(args, "command"), 180)
				case "remove":
					return "Remove MCP server " + argString(args, "server")
				case "search":
					return "Start configured MCP servers and inspect their available tools"
				case "describe":
					return "Load the schema for MCP tool " + argString(args, "server") + "." + argString(args, "tool")
				case "call":
					return "Call MCP tool " + argString(args, "server") + "." + argString(args, "tool")
				}
				return ""
			},
			Run: toolExtension,
		},
		{
			Name:        "read_file",
			Description: "Read a text file. Relative paths start in the workspace. Returns numbered lines.",
			Parameters:  obj([]string{"path"}, map[string]any{"path": str("File path"), "offset": num("First line to read, from 1"), "limit": num("How many lines (default 200)")}),
			Guard:       outside("Read"),
			Run:         toolReadFile,
		},
		{
			Name:        "write_file",
			Description: "Create or overwrite a text file. Relative paths start in the workspace. Parent folders are created.",
			Parameters:  obj([]string{"path", "content"}, map[string]any{"path": str("File path"), "content": str("The complete new contents")}),
			Guard:       outside("Write"),
			Run:         toolWriteFile,
		},
		{
			Name:        "edit_file",
			Description: "Replace one exact piece of text in a file with another. The old text must appear exactly once.",
			Parameters:  obj([]string{"path", "old", "new"}, map[string]any{"path": str("File path"), "old": str("The exact text to replace"), "new": str("The replacement")}),
			Guard:       outside("Edit"),
			Run:         toolEditFile,
		},
		{
			Name:        "apply_patch",
			Description: "Apply exact text changes to one or more workspace files. Every old_text must match exactly once.",
			Parameters: obj([]string{"files"}, map[string]any{
				"files": map[string]any{
					"type": "array", "minItems": 1, "maxItems": 20,
					"items": obj([]string{"path", "old_text", "new_text"}, map[string]any{
						"path": str("Workspace-relative file path"), "old_text": str("Exact existing text"), "new_text": str("Replacement text"),
					}),
				},
			}),
			Guard: func(a *app, args map[string]any) string {
				files, _ := args["files"].([]any)
				for _, value := range files {
					file, _ := value.(map[string]any)
					path, _ := file["path"].(string)
					if _, inside := a.resolvePath(path); !inside {
						return "Edit " + path + " (outside the assistant's workspace)"
					}
				}
				return ""
			},
			Run: toolApplyPatch,
		},
		{
			Name:        "list_dir",
			Description: "List the files and folders in a directory (the workspace by default).",
			Parameters:  obj(nil, map[string]any{"path": str("Directory path")}),
			Guard:       outside("List"),
			Run:         toolListDir,
		},
		{
			Name:        "calculator",
			Description: "Work out arithmetic exactly. Supports + - * / % ^ (power), parentheses, sqrt(), abs() and round(). Always use it for sums, percentages and unit maths instead of working them out yourself.",
			Parameters:  obj([]string{"expression"}, map[string]any{"expression": str("For example: 17 * 23, or 0.2 * 85, or sqrt(144)")}),
			Run: func(ctx context.Context, a *app, args map[string]any) (string, error) {
				expression := argString(args, "expression")
				value, err := evalExpression(expression)
				if err != nil {
					return "", fmt.Errorf("could not work that out (%v); write it as plain arithmetic", err)
				}
				return expression + " = " + formatNumber(value), nil
			},
		},
		{
			Name:        "web_search",
			Description: "Search the web and return the top results (title, address, snippet). Only the search words leave this computer.",
			Parameters:  obj([]string{"query"}, map[string]any{"query": str("What to search for")}),
			Run:         toolWebSearch,
		},
		{
			Name:        "web_fetch",
			Description: "Download a web page and return its readable text.",
			Parameters:  obj([]string{"url"}, map[string]any{"url": str("An http or https address")}),
			Run:         toolWebFetch,
		},
		{
			Name:        "remember",
			Description: "Save a NEW lasting fact, only when the user asks you to remember something or tells you a new fact about themselves. Never use it to answer a question. File \"user\" is for facts about the person, \"memory\" for everything else. One short sentence.",
			Parameters:  obj([]string{"text"}, map[string]any{"text": str("The fact to keep"), "file": map[string]any{"type": "string", "enum": []string{"user", "memory"}, "description": "Where to keep it"}}),
			Run:         toolRemember,
		},
		{
			Name:        "read_skill",
			Description: "Load the full instructions of one of the listed skills.",
			Parameters:  obj([]string{"name"}, map[string]any{"name": str("The skill's name")}),
			Run:         toolReadSkill,
		},
	}
	return append(append(tools, memoryToolDefs()...), scheduleToolDef())
}

// --- paths -------------------------------------------------------------------

// resolvePath returns an absolute path and whether it lies inside the workspace.
func (a *app) resolvePath(p string) (string, bool) {
	return a.resolvePathRaw(a.agentPath(p))
}

func (a *app) resolvePathRaw(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" {
		return a.workspace, true
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(a.workspace, p)
	}
	p = filepath.Clean(p)
	root := filepath.Clean(a.workspace)
	rel, err := filepath.Rel(root, p)
	inside := err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	return p, inside
}

// --- shell and files ---------------------------------------------------------

func toolShell(ctx context.Context, a *app, args map[string]any) (string, error) {
	command := strings.TrimSpace(argString(args, "command"))
	if command == "" {
		return "", errors.New("no command given")
	}
	seconds := argInt(args, "timeout_seconds", 60)
	if seconds < 1 {
		seconds = 60
	}
	if seconds > 300 {
		seconds = 300
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	if !a.workspaceUserlandReady() {
		return "", errors.New("the workspace POSIX userland is not installed; finish the runtime setup first")
	}
	cmd := shellCommand(runCtx, a, command, a.workspace)
	cmd.WaitDelay = 2 * time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	text := clipText(strings.TrimSpace(out.String()), 8000)
	switch {
	case runCtx.Err() == context.DeadlineExceeded:
		return text + fmt.Sprintf("\n[stopped after %d seconds]", seconds), nil
	case err != nil:
		var exit interface{ ExitCode() int }
		if errors.As(err, &exit) {
			return fmt.Sprintf("%s\n[exit code %d]", text, exit.ExitCode()), nil
		}
		return "", err
	}
	if text == "" {
		text = "(no output)"
	}
	return text, nil
}

func toolReadFile(ctx context.Context, a *app, args map[string]any) (string, error) {
	path, _ := a.resolvePath(argString(args, "path"))
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if bytes.IndexByte(data[:min(len(data), 4096)], 0) >= 0 {
		return "", errors.New("that looks like a binary file, not text")
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	offset, limit := argInt(args, "offset", 1), argInt(args, "limit", 200)
	if offset < 1 {
		offset = 1
	}
	if limit < 1 || limit > 1000 {
		limit = 200
	}
	if offset > len(lines) {
		return "(past the end of the file)", nil
	}
	end := min(offset-1+limit, len(lines))
	var b strings.Builder
	for i := offset - 1; i < end; i++ {
		fmt.Fprintf(&b, "%d\t%s\n", i+1, lines[i])
	}
	if end < len(lines) {
		fmt.Fprintf(&b, "[%d more lines; continue with offset %d]\n", len(lines)-end, end+1)
	}
	return b.String(), nil
}

func toolWriteFile(ctx context.Context, a *app, args map[string]any) (string, error) {
	path, _ := a.resolvePath(argString(args, "path"))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	content := argString(args, "content")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return "", err
	}
	a.memoryDirty()
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), path), nil
}

func toolEditFile(ctx context.Context, a *app, args map[string]any) (string, error) {
	path, _ := a.resolvePath(argString(args, "path"))
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	old, repl := argString(args, "old"), argString(args, "new")
	if old == "" {
		return "", errors.New("old text is empty")
	}
	switch n := strings.Count(string(data), old); n {
	case 0:
		return "", errors.New("the old text was not found; read the file again and copy it exactly")
	case 1:
	default:
		return "", fmt.Errorf("the old text appears %d times; include more around it so it is unique", n)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), old, repl, 1)), 0600); err != nil {
		return "", err
	}
	a.memoryDirty()
	return "Edited " + path, nil
}

func toolApplyPatch(ctx context.Context, a *app, args map[string]any) (string, error) {
	items, ok := args["files"].([]any)
	if !ok || len(items) == 0 || len(items) > 20 {
		return "", errors.New("files must contain between 1 and 20 exact text changes")
	}
	type change struct {
		path string
		data []byte
	}
	changes := make([]change, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			return "", errors.New("each patch entry must be an object")
		}
		path, inside := a.resolvePath(argString(fields, "path"))
		if !inside {
			return "", errors.New("patch paths must stay inside the assistant's workspace")
		}
		if seen[path] {
			return "", errors.New("each file may appear only once in a patch")
		}
		seen[path] = true
		oldText, newText := argString(fields, "old_text"), argString(fields, "new_text")
		if oldText == "" {
			return "", errors.New("old_text cannot be empty")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if bytes.IndexByte(data[:min(len(data), 4096)], 0) >= 0 {
			return "", errors.New("patches can only edit text files")
		}
		if count := strings.Count(string(data), oldText); count != 1 {
			if count == 0 {
				return "", fmt.Errorf("old_text was not found in %s", path)
			}
			return "", fmt.Errorf("old_text appears %d times in %s; add more surrounding context", count, path)
		}
		updated := strings.Replace(string(data), oldText, newText, 1)
		changes = append(changes, change{path: path, data: []byte(updated)})
	}
	for _, item := range changes {
		if err := os.WriteFile(item.path, item.data, 0600); err != nil {
			return "", fmt.Errorf("write %s: %w", item.path, err)
		}
	}
	a.memoryDirty()
	var result strings.Builder
	for _, item := range changes {
		fmt.Fprintf(&result, "Updated %s\n", item.path)
	}
	return strings.TrimSpace(result.String()), nil
}

func toolListDir(ctx context.Context, a *app, args map[string]any) (string, error) {
	path, _ := a.resolvePath(argString(args, "path"))
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for i, e := range entries {
		if i >= 300 {
			fmt.Fprintf(&b, "[%d more]\n", len(entries)-i)
			break
		}
		if e.IsDir() {
			b.WriteString(e.Name() + "\\\n")
		} else if info, err := e.Info(); err == nil {
			fmt.Fprintf(&b, "%s  (%d bytes)\n", e.Name(), info.Size())
		}
	}
	if b.Len() == 0 {
		return "(empty)", nil
	}
	return b.String(), nil
}

// --- web ---------------------------------------------------------------------

// publicOnlyClient refuses to connect to this computer or the local network, so a web
// page (or a prompt-injected address) cannot make the assistant call its own services.
func publicOnlyClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || !isPublicIP(ip) {
				return errors.New("that address is on this computer or its local network, which web tools may not reach")
			}
			return nil
		},
	}
	return &http.Client{
		Timeout:   25 * time.Second,
		Transport: &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

func isPublicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast())
}

var (
	scriptStyleRE = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head)\b.*?</(script|style|noscript|svg|head)>`)
	blockTagRE    = regexp.MustCompile(`(?i)</?(p|div|br|li|tr|h[1-6]|section|article|ul|ol|table)\b[^>]*>`)
	anyTagRE      = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRE       = regexp.MustCompile(`[ \t\r\f\v]+`)
	blankRE       = regexp.MustCompile(`\n\s*\n+`)
)

func htmlToText(page string) string {
	page = scriptStyleRE.ReplaceAllString(page, " ")
	page = blockTagRE.ReplaceAllString(page, "\n")
	page = anyTagRE.ReplaceAllString(page, "")
	page = html.UnescapeString(page)
	page = spaceRE.ReplaceAllString(page, " ")
	page = strings.ReplaceAll(page, " \n", "\n")
	return strings.TrimSpace(blankRE.ReplaceAllString(page, "\n\n"))
}

func fetchPublic(ctx context.Context, rawURL string, limit int64) (string, string, error) {
	return fetchPublicAs(ctx, rawURL, "Mozilla/5.0 (Windows NT 10.0; Win64; x64) SovereignAI/1.0", limit)
}

// fetchPublicAs is fetchPublic with a chosen User-Agent (some plain-text services only answer command-line clients).
func fetchPublicAs(ctx context.Context, rawURL, userAgent string, limit int64) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", "", errors.New("give a full http or https address")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", "", err
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Accept", "text/html,text/plain,application/json;q=0.9,*/*;q=0.5")
	response, err := publicOnlyClient().Do(request)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return "", "", fmt.Errorf("the site answered %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit))
	return string(data), response.Header.Get("Content-Type"), err
}

func toolWebFetch(ctx context.Context, a *app, args map[string]any) (string, error) {
	if err := a.refuseToSendSecrets(argString(args, "url")); err != nil {
		return "", err
	}
	body, contentType, err := fetchPublic(ctx, argString(args, "url"), 2<<20)
	if err != nil {
		return "", err
	}
	if strings.Contains(contentType, "html") || strings.HasPrefix(strings.TrimSpace(body), "<") {
		body = htmlToText(body)
	}
	if strings.TrimSpace(body) == "" {
		return "(the page had no readable text)", nil
	}
	return body, nil
}

var (
	ddgResultRE  = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>.*?<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	ddgRedirectR = regexp.MustCompile(`[?&]uddg=([^&]+)`)
)

func toolWebSearch(ctx context.Context, a *app, args map[string]any) (string, error) {
	query := strings.TrimSpace(argString(args, "query"))
	if query == "" {
		return "", errors.New("no search words given")
	}
	if err := a.refuseToSendSecrets(query); err != nil {
		return "", err
	}
	return a.webSearch(ctx, query)
}

// --- memory and skills -------------------------------------------------------

func toolRemember(ctx context.Context, a *app, args map[string]any) (string, error) {
	text := mdLine(argString(args, "text"))
	if text == "" {
		return "", errors.New("nothing to remember")
	}
	name := "MEMORY.md"
	if argString(args, "file") == "user" {
		name = "USER.md"
	}
	path := filepath.Join(a.workspace, name)
	existing, _ := os.ReadFile(path)
	if strings.Contains(string(existing), text) || a.alreadyKnown(text) {
		return "Already remembered. Nothing was added.", nil
	}
	prefix := ""
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		prefix = "\n"
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%s- %s (%s)\n", prefix, text, time.Now().Format("2006-01-02")); err != nil {
		return "", err
	}
	a.memoryDirty()
	return "Saved to " + name + ".", nil
}

// secretLike reports tokens that look like passwords, codes or account numbers: six or more letters and
// digits with at least one of each (a lone number or a lone word is not enough to be a secret).
func secretLikeTokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') }) {
		if len(w) < 6 {
			continue
		}
		letters, digits := 0, 0
		for _, r := range w {
			if r >= '0' && r <= '9' {
				digits++
			} else {
				letters++
			}
		}
		if letters > 0 && digits > 0 {
			out[w] = true
		}
	}
	return out
}

// refuseToSendSecrets stops a web search or fetch that carries something secret-looking from the user's
// notes. The text of a query leaves the computer, and a small model will happily paste a password it has
// in front of it into a search box.
func (a *app) refuseToSendSecrets(outgoing string) error {
	candidates := secretLikeTokens(outgoing)
	if len(candidates) == 0 {
		return nil
	}
	for _, file := range a.memoryFiles() {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for token := range secretLikeTokens(string(data)) {
			if candidates[token] {
				return errors.New("that contains private information from the user's notes, so it was not sent over the internet. Answer from the notes instead of searching")
			}
		}
	}
	return nil
}

var noteStopwords = map[string]bool{"the": true, "and": true, "that": true, "with": true, "when": true, "you": true, "your": true, "are": true, "for": true, "was": true, "has": true, "have": true, "his": true, "her": true, "from": true, "they": true, "their": true, "this": true, "while": true, "who": true, "what": true, "which": true, "will": true}

func significantWords(s string) map[string]bool {
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') }) {
		if len(w) >= 3 && !noteStopwords[w] {
			words[w] = true
		}
	}
	return words
}

// alreadyKnown reports whether a fact is already in the memory files. A small model sometimes calls
// remember to answer a question, which would write the answer back as a new note; this keeps the
// files from filling with copies. It is deliberately about wording, not meaning, so a genuinely new
// fact is never dropped.
func (a *app) alreadyKnown(text string) bool {
	words := significantWords(text)
	if len(words) < 2 {
		return false
	}
	for _, file := range a.memoryFiles() {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, passage := range splitPassages(string(data)) {
			have := significantWords(passage)
			hits := 0
			for w := range words {
				if have[w] {
					hits++
				}
			}
			if hits*4 >= len(words)*3 {
				return true
			}
		}
	}
	return false
}

func toolReadSkill(ctx context.Context, a *app, args map[string]any) (string, error) {
	return a.readSkill(argString(args, "name"))
}

// sortedKeys is used by tests and listings to keep output stable.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
