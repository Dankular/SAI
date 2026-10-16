package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Installing an extension from a marketplace listing. The 1B model does not write the command: Go
// reads the project's README, takes the launch command it documents, and only accepts one that runs on
// the bundled npx or uvx. Anything that needs the user's input (a path, a credential) stops the
// install and is reported back, so the assistant asks instead of guessing.

type mcpInstallOption struct {
	Command      string   `json:"command"`
	Args         []string `json:"args"`
	Env          []string `json:"env,omitempty"`          // environment variables the server wants
	Placeholders []string `json:"placeholders,omitempty"` // arguments that are examples, not real values
}

// Supported options run on the toolchain this app carries.
func (o mcpInstallOption) Supported() bool {
	return o.Command == "npx" || o.Command == "uvx"
}

var (
	codeFenceRE   = regexp.MustCompile("(?s)```[a-zA-Z0-9_-]*\\s*\n(.*?)```")
	launchLineRE  = regexp.MustCompile(`^\s*(?:[$>]\s*)?(npx|uvx|docker|python3?|node|uv)\s+(.+?)\s*$`)
	envNameRE     = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,}(?:_[A-Z0-9]+)*_(?:API_KEY|ACCESS_TOKEN|TOKEN|SECRET|PASSWORD|KEY|URL|HOST|ID)\b`)
	placeholderRE = regexp.MustCompile(`(?i)(/path/to|path/to/|<[^>]+>|\byour[-_ ]|\byour_|\$\{|\bxxx+\b|/absolute/|c:\\path|directory/to|\[[^\]]+\])`)
	trailingComma = regexp.MustCompile(`,\s*([}\]])`)
	lineComment   = regexp.MustCompile(`(?m)^\s*//.*$`)
)

// parseMCPInstall pulls launch options out of a README: JSON config blocks first (the usual
// "mcpServers" snippet), then shell lines such as "npx -y package".
func parseMCPInstall(readme string) []mcpInstallOption {
	var options []mcpInstallOption
	seen := map[string]bool{}
	add := func(o mcpInstallOption) {
		o.Command = strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(o.Command, ".cmd"), ".exe"))
		key := o.Command + "\x00" + strings.Join(o.Args, "\x00")
		if o.Command == "" || seen[key] || isInstallerInvocation(o) {
			return
		}
		seen[key] = true
		for _, arg := range o.Args {
			if placeholderRE.MatchString(arg) {
				o.Placeholders = append(o.Placeholders, arg)
			}
		}
		options = append(options, o)
	}
	text := strings.ReplaceAll(readme, "\r\n", "\n")
	var fences []string
	for _, m := range codeFenceRE.FindAllStringSubmatch(text, -1) {
		fences = append(fences, m[1])
	}
	for _, body := range fences {
		if !strings.Contains(body, `"command"`) {
			continue
		}
		for _, block := range jsonObjects(body) {
			var tree any
			if json.Unmarshal([]byte(trailingComma.ReplaceAllString(lineComment.ReplaceAllString(block, ""), "$1")), &tree) != nil {
				continue
			}
			walkLaunchObjects(tree, add)
		}
	}
	for _, body := range fences {
		for _, line := range strings.Split(body, "\n") {
			m := launchLineRE.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			fields := strings.Fields(m[2])
			if cut := indexOfAny(fields, "&&", "|", ">", ";", "#"); cut >= 0 {
				fields = fields[:cut]
			}
			add(mcpInstallOption{Command: m[1], Args: unquoteAll(fields)})
		}
	}
	// credentials the project mentions anywhere near its setup
	var mentioned []string
	for _, name := range envNameRE.FindAllString(text, 40) {
		if !contains(mentioned, name) && len(mentioned) < 4 {
			mentioned = append(mentioned, name)
		}
	}
	for i := range options {
		if len(options[i].Env) == 0 {
			options[i].Env = mentioned
		}
	}
	return options
}

// walkLaunchObjects finds every {"command": ..., "args": [...], "env": {...}} in a JSON tree.
func walkLaunchObjects(node any, add func(mcpInstallOption)) {
	switch v := node.(type) {
	case map[string]any:
		if command, ok := v["command"].(string); ok && command != "" {
			option := mcpInstallOption{Command: command}
			if args, ok := v["args"].([]any); ok {
				for _, a := range args {
					if s, ok := a.(string); ok {
						option.Args = append(option.Args, s)
					}
				}
			}
			if env, ok := v["env"].(map[string]any); ok {
				for key := range env {
					option.Env = append(option.Env, key)
				}
			}
			sortStrings(option.Env)
			add(option)
			return
		}
		for _, child := range v {
			walkLaunchObjects(child, add)
		}
	case []any:
		for _, child := range v {
			walkLaunchObjects(child, add)
		}
	}
}

// jsonObjects returns every top-level {...} block in a text, by brace matching that respects strings.
func jsonObjects(text string) []string {
	var blocks []string
	depth, start := 0, -1
	inString, escaped := false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					blocks = append(blocks, text[start:i+1])
				}
			}
		}
	}
	return blocks
}

func indexOfAny(fields []string, tokens ...string) int {
	for i, f := range fields {
		for _, t := range tokens {
			if f == t || strings.HasPrefix(f, "#") {
				return i
			}
		}
	}
	return -1
}

func unquoteAll(fields []string) []string {
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = strings.Trim(f, `"'`)
	}
	return out
}

func sortStrings(list []string) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j] < list[j-1]; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

// fetchServerReadme returns the README behind a marketplace listing or GitHub repository.
// serverReadmeOverride lets tests supply READMEs without the network.
var serverReadmeOverride func(rawURL string) (string, error)

func (a *app) fetchServerReadme(ctx context.Context, rawURL string) (string, error) {
	if serverReadmeOverride != nil {
		return serverReadmeOverride(rawURL)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" {
		return "", errors.New("give an https address from a marketplace result")
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "github.com", "www.github.com":
		owner, repo, ok := githubOwnerRepo(parsed)
		if !ok {
			return "", errors.New("the GitHub address must name a repository")
		}
		return fetchGitHubReadme(ctx, owner, repo)
	case "mcpservers.org", "www.mcpservers.org":
		page, _, err := fetchPublic(ctx, parsed.String(), 2<<20)
		if err != nil {
			return "", err
		}
		for _, match := range githubRepoRE.FindAllString(page, 12) {
			repoURL, _ := url.Parse(match)
			if owner, repo, ok := githubOwnerRepo(repoURL); ok {
				if readme, err := fetchGitHubReadme(ctx, owner, repo); err == nil {
					return readme, nil
				}
			}
		}
		return "", errors.New("the listing does not link to a readable repository")
	}
	return "", errors.New("only GitHub repositories and mcpservers.org listings can be installed from")
}

var serverNameJunk = regexp.MustCompile(`[^a-z0-9]+`)

// serverNameFor makes a short configured-server name from a listing address.
func serverNameFor(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "extension"
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	name := parts[len(parts)-1]
	name = strings.TrimSuffix(strings.ToLower(name), ".git")
	for _, junk := range []string{"mcp-server-", "-mcp-server", "mcp-", "-mcp", "server-"} {
		name = strings.Replace(name, junk, "", 1)
	}
	name = strings.Trim(serverNameJunk.ReplaceAllString(name, "-"), "-")
	if name == "" {
		name = "extension"
	}
	return clipText(name, 40)
}

// installMCPServer sets up a server from a listing and checks that it starts. It tries each launch
// command the README documents, best first, and keeps the first that works. It returns what to tell the
// model; a result that needs the user's input is a normal result, not an error.
func (a *app) installMCPServer(ctx context.Context, args map[string]any) (string, error) {
	rawURL := strings.TrimSpace(argString(args, "url"))
	readme, err := a.fetchServerReadme(ctx, rawURL)
	if err != nil {
		return "", err
	}
	all := parseMCPInstall(readme)
	options := supportedOptions(all)
	if len(options) == 0 {
		if len(all) > 0 {
			return "", fmt.Errorf("this server only documents %s; this app runs npx and uvx servers. Pick another candidate", all[0].Command)
		}
		return "", errors.New("the README has no launch command this app can read. Pick another candidate or configure it by hand")
	}
	if len(options) > 3 {
		options = options[:3]
	}
	name := strings.TrimSpace(argString(args, "server"))
	if name == "" {
		name = serverNameFor(rawURL)
	}
	if !mcpNameRE.MatchString(name) {
		return "", errors.New("server must use 1-64 letters, numbers, underscores, or hyphens")
	}
	cfg, err := a.readMCPConfig()
	if err != nil {
		return "", err
	}
	if _, exists := cfg.Servers[name]; exists {
		return "", fmt.Errorf("a server called %s is already configured; use it, or remove it first", name)
	}
	env := map[string]string{}
	if given, ok := args["env"].(map[string]any); ok {
		for key, value := range given {
			if text, ok := value.(string); ok && mcpEnvRE.MatchString(key) && strings.TrimSpace(text) != "" {
				env[key] = text
			}
		}
	}
	var needsInput string
	var failures []string
	for _, option := range options {
		launchArgs := append([]string(nil), option.Args...)
		if given, ok := args["args"].([]any); ok && len(given) > 0 {
			launchArgs = launchArgs[:0]
			for _, g := range given {
				if s, ok := g.(string); ok {
					launchArgs = append(launchArgs, s)
				}
			}
		} else if len(option.Placeholders) > 0 {
			if needsInput == "" {
				needsInput = "This server needs a value only the user can give: " + strings.Join(option.Placeholders, ", ") +
					". Ask the user, then call install again with args set to the complete argument list (documented as: " +
					option.Command + " " + strings.Join(option.Args, " ") + ")."
			}
			continue
		}
		var missing []string
		for _, key := range option.Env {
			if _, ok := env[key]; !ok && !strings.HasSuffix(key, "_URL") && !strings.HasSuffix(key, "_HOST") {
				missing = append(missing, key)
			}
		}
		if len(missing) > 0 {
			if needsInput == "" {
				needsInput = "This server needs credentials from the user: " + strings.Join(missing, ", ") +
					". Ask the user for them (never make one up), then call install again with env set."
			}
			continue
		}
		if option.Command == "npx" && len(launchArgs) > 0 && launchArgs[0] != "-y" && launchArgs[0] != "--yes" {
			launchArgs = append([]string{"-y"}, launchArgs...) // nobody is there to answer npx's prompt
		}
		cfg.Servers[name] = mcpServerConfig{Command: option.Command, Args: launchArgs, Env: env}
		if err := a.writeMCPConfig(cfg); err != nil {
			return "", err
		}
		startCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		tools, startErr := a.startAndListTools(startCtx, name)
		cancel()
		if startErr != nil {
			a.closeMCPServer(name)
			delete(cfg.Servers, name)
			_ = a.writeMCPConfig(cfg)
			failures = append(failures, option.Command+" "+strings.Join(launchArgs, " ")+": "+clipText(startErr.Error(), 300))
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Installed and started %s (%s %s). It offers %d tools:\n", name, option.Command, strings.Join(launchArgs, " "), len(tools))
		for i, t := range tools {
			if i == 12 {
				fmt.Fprintf(&b, "... and %d more (extension action=search finds them)\n", len(tools)-12)
				break
			}
			fmt.Fprintf(&b, "- %s: %s\n", t.Name, clipText(t.Description, 140))
		}
		fmt.Fprintf(&b, "Next: extension action=use server=%s tool=<tool> goal=<what you want done>.", name)
		return b.String(), nil
	}
	if needsInput != "" && len(failures) == 0 {
		return needsInput, nil
	}
	message := "none of the launch commands in the README started, so nothing was installed: " + strings.Join(failures, " | ")
	if needsInput != "" {
		message += ". Another option needs the user: " + needsInput
	}
	return "", errors.New(message)
}

func (a *app) startAndListTools(ctx context.Context, name string) ([]mcpToolDefinition, error) {
	client, err := a.mcpClientFor(ctx, name)
	if err != nil {
		return nil, err
	}
	tools, err := client.listTools(ctx)
	if err != nil {
		return nil, err
	}
	if len(tools) == 0 {
		return nil, errors.New("it started but offers no tools")
	}
	return tools, nil
}

var installerPackageRE = regexp.MustCompile(`(?i)smithery|(^|/)(cli|installer?|create-[\w-]+|degit|mcp-get|mcp-installer)(@|$)`)

// isInstallerInvocation spots lines that install a server into some client rather than run it, such
// as "npx @smithery/cli install foo --client claude". They are not launch commands.
func isInstallerInvocation(o mcpInstallOption) bool {
	if o.Command != "npx" && o.Command != "uvx" {
		return false
	}
	var positional []string
	for _, a := range o.Args {
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
		}
	}
	if len(positional) == 0 {
		return false
	}
	if installerPackageRE.MatchString(positional[0]) {
		return true
	}
	if len(positional) > 1 {
		switch strings.ToLower(positional[1]) {
		case "install", "add", "init", "setup", "login", "uninstall":
			return true
		}
	}
	return false
}

// supportedOptions are the options this app can run, in README order.
func supportedOptions(options []mcpInstallOption) []mcpInstallOption {
	var out []mcpInstallOption
	for _, o := range options {
		if o.Supported() {
			out = append(out, o)
		}
	}
	return out
}
