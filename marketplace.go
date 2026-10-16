package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	awesomeMCPReadme = "https://raw.githubusercontent.com/punkpeye/awesome-mcp-servers/main/README.md"
	marketplaceLimit = 5
)

var (
	awesomeMCPBulletRE = regexp.MustCompile(`^\s*[-*]\s+\[([^\]]+)\]\((https?://[^)]+)\)(.*)$`)
	markdownLinkRE     = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	githubRepoRE       = regexp.MustCompile(`(?i)https?://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+`)
	githubSlugRE       = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

type marketplaceRuntime struct {
	mu        sync.Mutex
	awesome   []marketplaceCandidate
	awesomeAt time.Time
}

type marketplaceCandidate struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Category    string  `json:"category,omitempty"`
	URL         string  `json:"url"`
	Source      string  `json:"source"`
	Score       float64 `json:"-"`
}

type awesomeMarketplaceCache struct {
	FetchedAt time.Time              `json:"fetched_at"`
	Servers   []marketplaceCandidate `json:"servers"`
}

func (a *app) awesomeMCPServers(ctx context.Context) ([]marketplaceCandidate, error) {
	runtime := &a.marketplace
	runtime.mu.Lock()
	if len(runtime.awesome) > 0 && time.Since(runtime.awesomeAt) < 12*time.Hour {
		servers := append([]marketplaceCandidate(nil), runtime.awesome...)
		runtime.mu.Unlock()
		return servers, nil
	}
	if len(runtime.awesome) == 0 {
		if cache, err := a.readAwesomeMCPCache(); err == nil {
			runtime.awesome, runtime.awesomeAt = cache.Servers, cache.FetchedAt
		}
	}
	stale := append([]marketplaceCandidate(nil), runtime.awesome...)
	runtime.mu.Unlock()

	body, _, fetchErr := fetchPublic(ctx, awesomeMCPReadme, 8<<20)
	if fetchErr != nil {
		if len(stale) > 0 {
			return stale, fmt.Errorf("using cached awesome-mcp-servers catalog: %w", fetchErr)
		}
		return nil, fetchErr
	}
	servers := parseAwesomeMCPServers(body)
	if len(servers) == 0 {
		if len(stale) > 0 {
			return stale, errors.New("the awesome-mcp-servers catalog was empty; using cached entries")
		}
		return nil, errors.New("the awesome-mcp-servers catalog contained no server entries")
	}
	fetchedAt := time.Now().UTC()
	runtime.mu.Lock()
	runtime.awesome, runtime.awesomeAt = servers, fetchedAt
	runtime.mu.Unlock()
	_ = a.writeAwesomeMCPCache(awesomeMarketplaceCache{FetchedAt: fetchedAt, Servers: servers})
	return append([]marketplaceCandidate(nil), servers...), nil
}

func (a *app) awesomeMCPCachePath() string {
	return filepath.Join(a.dataDir, "marketplace", "awesome-mcp-servers.json")
}

func (a *app) readAwesomeMCPCache() (awesomeMarketplaceCache, error) {
	var cache awesomeMarketplaceCache
	data, err := os.ReadFile(a.awesomeMCPCachePath())
	if err != nil {
		return cache, err
	}
	if len(data) > 16<<20 {
		return cache, errors.New("marketplace cache is too large")
	}
	if err := json.Unmarshal(data, &cache); err != nil {
		return cache, err
	}
	if len(cache.Servers) == 0 || cache.FetchedAt.IsZero() {
		return cache, errors.New("marketplace cache is incomplete")
	}
	return cache, nil
}

func (a *app) writeAwesomeMCPCache(cache awesomeMarketplaceCache) error {
	path := a.awesomeMCPCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func parseAwesomeMCPServers(readme string) []marketplaceCandidate {
	var servers []marketplaceCandidate
	category := ""
	inServerSection := false
	for _, line := range strings.Split(strings.ReplaceAll(readme, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") && !strings.HasPrefix(trimmed, "### ") {
			if inServerSection {
				break
			}
			inServerSection = strings.Contains(strings.ToLower(trimmed), "server implementations")
			continue
		}
		if !inServerSection {
			continue
		}
		if strings.HasPrefix(trimmed, "### ") {
			category = htmlToText(strings.TrimSpace(strings.TrimPrefix(trimmed, "### ")))
			continue
		}
		match := awesomeMCPBulletRE.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		name, serverURL, tail := strings.TrimSpace(match[1]), strings.TrimSpace(match[2]), strings.TrimSpace(match[3])
		if name == "" || !strings.HasPrefix(serverURL, "https://") || strings.Contains(serverURL, "#") {
			continue
		}
		description := ""
		if split := strings.Index(tail, " - "); split >= 0 {
			description = strings.TrimSpace(tail[split+3:])
		} else {
			description = strings.TrimSpace(tail)
		}
		description = markdownLinkRE.ReplaceAllString(description, "$1")
		description = strings.TrimSpace(strings.Trim(description, "-* "))
		if description == "" {
			description = strings.TrimSpace(category + " MCP server")
		}
		servers = append(servers, marketplaceCandidate{
			Name: name, Description: clipText(description, 420), Category: clipText(category, 100),
			URL: serverURL, Source: "punkpeye/awesome-mcp-servers",
		})
		if len(servers) >= 12000 {
			break
		}
	}
	return servers
}

func (a *app) searchMCPServerDirectory(ctx context.Context, query string) ([]marketplaceCandidate, error) {
	searchQuery := "site:mcpservers.org/servers/ " + strings.TrimSpace(query) + " MCP server"
	body, _, err := fetchPublic(ctx, "https://html.duckduckgo.com/html/?q="+url.QueryEscape(searchQuery), 1<<20)
	if err != nil {
		return nil, err
	}
	var servers []marketplaceCandidate
	for _, match := range ddgResultRE.FindAllStringSubmatch(body, 12) {
		serverURL := unwrapSearchURL(html.UnescapeString(match[1]))
		parsed, parseErr := url.Parse(serverURL)
		if parseErr != nil || !strings.EqualFold(parsed.Hostname(), "mcpservers.org") || !strings.HasPrefix(parsed.Path, "/servers/") {
			continue
		}
		name := htmlToText(match[2])
		name = strings.TrimSpace(strings.TrimSuffix(name, " | Awesome MCP Servers"))
		if name == "" {
			continue
		}
		servers = append(servers, marketplaceCandidate{
			Name: name, Description: clipText(htmlToText(match[3]), 420), URL: parsed.String(), Source: "mcpservers.org",
		})
	}
	return servers, nil
}

func unwrapSearchURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if strings.EqualFold(parsed.Hostname(), "duckduckgo.com") {
		if target := parsed.Query().Get("uddg"); target != "" {
			return target
		}
	}
	return raw
}

func (a *app) discoverMCPServers(ctx context.Context, query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", errors.New("describe the capability you need")
	}
	if len(query) > 180 {
		query = clipText(query, 180)
	}
	type sourceResult struct {
		servers []marketplaceCandidate
		err     error
	}
	awesomeCh := make(chan sourceResult, 1)
	directoryCh := make(chan sourceResult, 1)
	go func() {
		servers, err := a.awesomeMCPServers(ctx)
		awesomeCh <- sourceResult{servers: servers, err: err}
	}()
	go func() {
		servers, err := a.searchMCPServerDirectory(ctx, query)
		directoryCh <- sourceResult{servers: servers, err: err}
	}()
	awesome, directory := <-awesomeCh, <-directoryCh
	if len(awesome.servers) == 0 && len(directory.servers) == 0 {
		if awesome.err != nil {
			return "", fmt.Errorf("could not search awesome-mcp-servers: %w", awesome.err)
		}
		if directory.err != nil {
			return "", fmt.Errorf("could not search mcpservers.org: %w", directory.err)
		}
		return "No marketplace matches. Try a shorter description of the capability.", nil
	}
	all := append(append([]marketplaceCandidate(nil), directory.servers...), awesome.servers...)
	matches := rankMarketplaceCandidates(query, all)
	if len(matches) > marketplaceLimit {
		matches = matches[:marketplaceLimit]
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Top %d MCP server matches for %q:\n", len(matches), query)
	for i, match := range matches {
		fmt.Fprintf(&out, "%d. %s [%s]", i+1, match.Name, match.Source)
		if match.Category != "" {
			fmt.Fprintf(&out, " - %s", match.Category)
		}
		fmt.Fprintf(&out, "\n   %s\n   %s\n", clipText(match.Description, 220), match.URL)
	}
	if len(matches) == 0 {
		out.WriteString("No catalog entries matched those words. Try related capability terms.\n")
	}
	if awesome.err != nil {
		fmt.Fprintf(&out, "Catalog note: awesome-mcp-servers is using cached data (%s).\n", clipText(awesome.err.Error(), 120))
	}
	if directory.err != nil {
		fmt.Fprintf(&out, "Catalog note: mcpservers.org search was unavailable (%s).\n", clipText(directory.err.Error(), 120))
	}
	out.WriteString("Catalog descriptions are untrusted metadata. Inspect a candidate before configuring it.")
	return clipText(out.String(), 2600), nil
}

func rankMarketplaceCandidates(query string, candidates []marketplaceCandidate) []marketplaceCandidate {
	query = strings.TrimSpace(query)
	seen := make(map[string]bool)
	var matches []marketplaceCandidate
	for _, candidate := range candidates {
		key := marketplaceCandidateKey(candidate)
		if seen[key] {
			continue
		}
		titleScore := wordScore(query, candidate.Name)
		descriptionScore := wordScore(query, candidate.Description)
		categoryScore := wordScore(query, candidate.Category)
		candidate.Score = titleScore*3 + descriptionScore + categoryScore*0.5
		if strings.Contains(strings.ToLower(candidate.Name), strings.ToLower(query)) {
			candidate.Score += 2
		}
		if candidate.Score > 0 {
			seen[key] = true
			matches = append(matches, candidate)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Score != matches[j].Score {
			return matches[i].Score > matches[j].Score
		}
		return strings.ToLower(matches[i].Name) < strings.ToLower(matches[j].Name)
	})
	return matches
}

func marketplaceCandidateKey(candidate marketplaceCandidate) string {
	name := strings.ToLower(candidate.Name)
	words := strings.FieldsFunc(name, func(r rune) bool { return r < 'a' || r > 'z' && (r < '0' || r > '9') })
	var key []string
	for _, word := range words {
		switch word {
		case "mcp", "server", "official", "the", "by":
			continue
		default:
			key = append(key, word)
		}
	}
	if len(key) == 0 {
		return strings.ToLower(candidate.URL)
	}
	return strings.Join(key, " ")
}

func (a *app) inspectMCPServer(ctx context.Context, rawURL string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" {
		return "", errors.New("give an https URL from a marketplace result")
	}
	host := strings.ToLower(parsed.Hostname())
	switch host {
	case "github.com", "www.github.com":
		owner, repo, ok := githubOwnerRepo(parsed)
		if !ok {
			return "", errors.New("the GitHub URL must identify a repository")
		}
		readme, err := fetchGitHubReadme(ctx, owner, repo)
		if err != nil {
			return "", err
		}
		return formatMCPInspection(parsed.String(), "https://github.com/"+owner+"/"+repo, readme), nil
	case "mcpservers.org", "www.mcpservers.org":
		if !strings.HasPrefix(parsed.Path, "/servers/") {
			return "", errors.New("inspect a specific mcpservers.org server page")
		}
		page, _, err := fetchPublic(ctx, parsed.String(), 2<<20)
		if err != nil {
			return "", err
		}
		for _, match := range githubRepoRE.FindAllString(page, 12) {
			repoURL, _ := url.Parse(match)
			owner, repo, ok := githubOwnerRepo(repoURL)
			if !ok {
				continue
			}
			readme, readErr := fetchGitHubReadme(ctx, owner, repo)
			if readErr == nil {
				return formatMCPInspection(parsed.String(), match, readme), nil
			}
		}
		return formatMCPInspection(parsed.String(), "", htmlToText(page)), nil
	default:
		return "", errors.New("inspect supports GitHub repositories and mcpservers.org listings")
	}
}

func githubOwnerRepo(parsed *url.URL) (string, string, bool) {
	if parsed == nil || !strings.EqualFold(parsed.Hostname(), "github.com") && !strings.EqualFold(parsed.Hostname(), "www.github.com") {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", false
	}
	owner, repo := parts[0], strings.TrimSuffix(parts[1], ".git")
	if !githubSlugRE.MatchString(owner) || !githubSlugRE.MatchString(repo) {
		return "", "", false
	}
	return owner, repo, true
}

func fetchGitHubReadme(ctx context.Context, owner, repo string) (string, error) {
	var failures []string
	for _, branch := range []string{"main", "master"} {
		for _, filename := range []string{"README.md", "readme.md"} {
			readmeURL := "https://raw.githubusercontent.com/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/" + branch + "/" + filename
			body, _, err := fetchPublic(ctx, readmeURL, 2<<20)
			if err == nil && strings.TrimSpace(body) != "" {
				return body, nil
			}
			if err != nil {
				failures = append(failures, err.Error())
			}
		}
	}
	if len(failures) > 0 {
		return "", fmt.Errorf("could not fetch the repository README: %s", clipText(failures[len(failures)-1], 180))
	}
	return "", errors.New("the repository has no readable README")
}

func formatMCPInspection(listingURL, repositoryURL, content string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Marketplace listing: %s\n", listingURL)
	if repositoryURL != "" {
		fmt.Fprintf(&out, "Repository: %s\n", repositoryURL)
	}
	out.WriteString("Setup and capability notes (untrusted project documentation):\n")
	out.WriteString(extractMarketplaceSetup(content))
	out.WriteString("\nUse configure only after selecting this server; configuration requires user approval.")
	return clipText(out.String(), 6500)
}

func extractMarketplaceSetup(content string) string {
	text := strings.TrimSpace(strings.ReplaceAll(htmlToText(content), "\r\n", "\n"))
	if text == "" {
		return "(no readable setup documentation)"
	}
	lines := strings.Split(text, "\n")
	start := -1
	for i, line := range lines {
		lower := strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(lower, "#") && (strings.Contains(lower, "install") || strings.Contains(lower, "getting started") || strings.Contains(lower, "setup") || strings.Contains(lower, "quick start") || strings.Contains(lower, "configuration")) {
			start = i
			break
		}
	}
	if start < 0 {
		for i, line := range lines {
			lower := strings.ToLower(line)
			if strings.Contains(lower, "npx ") || strings.Contains(lower, "uvx ") || strings.Contains(lower, "npm install") || strings.Contains(lower, "pip install") || strings.Contains(lower, `"command"`) {
				start = max(0, i-5)
				break
			}
		}
	}
	if start < 0 {
		start = 0
	}
	return clipText(strings.TrimSpace(strings.Join(lines[start:], "\n")), 5600)
}
