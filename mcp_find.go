package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Finding the right extension for a job. The marketplace listings are keyword-matched, which misses
// synonyms and drowns in popular servers, so the work is done here instead of by the 1B model:
//
//  1. the model restates the problem as a capability and a few keywords (a small constrained form),
//  2. tools already installed are checked first, by meaning,
//  3. marketplace candidates from several searches are merged and re-ranked by meaning,
//  4. the best few have their READMEs read, and install options are extracted by Go,
//  5. only options that run on the bundled npx or uvx are offered for installation.
//
// Everything a listing says is untrusted data; nothing here runs it.

type mcpNeed struct {
	Capability string   `json:"capability"`
	Keywords   []string `json:"keywords"`
	Service    string   `json:"service"`
}

func mcpNeedSchema() map[string]any {
	return jsonObject([]string{"capability", "keywords", "service"}, map[string]any{
		"capability": jsonStr(100),
		"keywords":   jsonArray(jsonStr(30), 2, 6),
		"service":    jsonStr(40),
	})
}

const mcpNeedPrompt = `A user has a job that may need an external tool (an MCP server). Restate THEIR job for a tool search.
capability: what the tool must be able to do, in one short phrase, using the user's own subject.
keywords: 2 to 6 single words or short names a tool listing would contain (service names, file types, verbs).
service: a specific product or service the user named, or an empty string when they named none.`

// understandNeed restates a problem for searching. If the model is unavailable the problem's own
// significant words are used, so a search still happens.
func (a *app) understandNeed(ctx context.Context, problem string) mcpNeed {
	problem = clipText(strings.TrimSpace(problem), 600)
	need := mcpNeed{Capability: clipText(problem, 100)}
	for w := range significantWords(problem) {
		need.Keywords = append(need.Keywords, w)
	}
	sort.Strings(need.Keywords)
	if len(need.Keywords) > 6 {
		need.Keywords = need.Keywords[:6]
	}
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	reply, _, err := a.callModel(callCtx, &modelGatewayCall{
		Session: "mcp-need", Step: -1, UserText: problem, JSONSchema: mcpNeedSchema(),
		Messages: []chatMsg{{Role: "system", Content: mcpNeedPrompt}, {Role: "user", Content: problem}}, emit: func(agentEvent) {},
	})
	if err != nil {
		return need
	}
	var got mcpNeed
	if json.Unmarshal([]byte(extractJSON(reply)), &got) != nil || strings.TrimSpace(got.Capability) == "" || len(got.Keywords) == 0 {
		return need
	}
	got.Capability = cleanField(got.Capability, 100)
	got.Service = strings.ToLower(cleanField(got.Service, 40))
	if !restatesProblem(problem, got) {
		return need // the model answered about something else; search on the user's own words
	}
	if got.Service != "" && !strings.Contains(strings.ToLower(problem), got.Service) {
		got.Service = "" // a service the user never named cannot steer the search
	}
	var keywords []string
	for _, k := range got.Keywords {
		if k = strings.ToLower(cleanField(k, 30)); k != "" {
			keywords = append(keywords, k)
		}
	}
	got.Keywords = keywords
	return got
}

func (n mcpNeed) queries() []string {
	var out []string
	add := func(q string) {
		q = strings.TrimSpace(q)
		for _, have := range out {
			if strings.EqualFold(have, q) {
				return
			}
		}
		if q != "" {
			out = append(out, q)
		}
	}
	add(n.Capability)
	if n.Service != "" {
		add(n.Service)
	}
	add(strings.Join(n.Keywords, " "))
	return out
}

// --- ranking by meaning -----------------------------------------------------------------------

const (
	searchQueryPrefix = "task: search result | query: "
	searchDocPrefix   = "title: none | text: "
)

// semanticScores returns how close each text is to the query, 0 to 1. With the embedding model down it
// returns nil and callers fall back to word matching alone.
func (a *app) semanticScores(ctx context.Context, query string, texts []string) []float64 {
	if len(texts) == 0 {
		return nil
	}
	inputs := make([]string, 0, len(texts)+1)
	inputs = append(inputs, searchQueryPrefix+query)
	for _, t := range texts {
		inputs = append(inputs, searchDocPrefix+clipText(t, 600))
	}
	callCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	vectors, err := a.embed(callCtx, inputs)
	if err != nil || len(vectors) != len(inputs) {
		return nil
	}
	scores := make([]float64, len(texts))
	for i := range texts {
		scores[i] = vectorDot(vectors[0], vectors[i+1])
	}
	return scores
}

type rankedTool struct {
	Server, Tool, Description string
	Score                     float64
}

// installedToolMatches ranks the tools of the configured servers against a problem.
func (a *app) installedToolMatches(ctx context.Context, problem string, need mcpNeed) []rankedTool {
	cfg, err := a.readMCPConfig()
	if err != nil || len(cfg.Servers) == 0 {
		return nil
	}
	var tools []rankedTool
	for _, name := range sortedKeys(cfg.Servers) {
		listCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
		client, err := a.mcpClientFor(listCtx, name)
		if err != nil {
			cancel()
			continue
		}
		defs, err := client.listTools(listCtx)
		cancel()
		if err != nil {
			continue
		}
		for _, d := range defs {
			tools = append(tools, rankedTool{Server: name, Tool: d.Name, Description: d.Description})
		}
	}
	if len(tools) == 0 {
		return nil
	}
	texts := make([]string, len(tools))
	for i, t := range tools {
		texts[i] = t.Tool + ": " + t.Description
	}
	semantic := a.semanticScores(ctx, problem+" ("+need.Capability+")", texts)
	words := strings.Join(append([]string{need.Capability}, need.Keywords...), " ")
	for i := range tools {
		lexical := minFloat(wordScore(words, texts[i])/float64(max(1, len(significantWords(words)))), 1)
		if semantic != nil {
			tools[i].Score = 0.7*semantic[i] + 0.3*lexical
		} else {
			tools[i].Score = lexical
		}
	}
	sort.SliceStable(tools, func(i, j int) bool { return tools[i].Score > tools[j].Score })
	if len(tools) > 5 {
		tools = tools[:5]
	}
	return tools
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// installedStrongMatch is how close an installed tool must be to be called "already available".
const installedStrongMatch = 0.62

// collectMCPCandidates merges the catalogue with one directory search per query.
func (a *app) collectMCPCandidates(ctx context.Context, queries []string) ([]marketplaceCandidate, []string) {
	var (
		mu    sync.Mutex
		all   []marketplaceCandidate
		notes []string
		wg    sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		servers, err := a.awesomeMCPServers(ctx)
		mu.Lock()
		defer mu.Unlock()
		all = append(all, servers...)
		if err != nil {
			notes = append(notes, "the awesome-mcp-servers list was unavailable ("+clipText(err.Error(), 100)+")")
		}
	}()
	for _, q := range queries {
		q := q
		wg.Add(1)
		go func() {
			defer wg.Done()
			servers, err := a.searchMCPServerDirectory(ctx, q)
			mu.Lock()
			defer mu.Unlock()
			all = append(all, servers...)
			if err != nil {
				notes = append(notes, "the mcpservers.org search was unavailable ("+clipText(err.Error(), 100)+")")
			}
		}()
	}
	wg.Wait()
	return all, notes
}

type mcpCandidate struct {
	marketplaceCandidate
	Fit     float64
	Install []mcpInstallOption
	Checked bool // the README was read
}

// rankMCPCandidates narrows a large catalogue by words, then orders the survivors by meaning.
func (a *app) rankMCPCandidates(ctx context.Context, problem string, need mcpNeed, all []marketplaceCandidate) []mcpCandidate {
	words := strings.Join(append([]string{need.Capability, need.Service}, need.Keywords...), " ")
	lexical := rankMarketplaceCandidates(words, all)
	if len(lexical) == 0 {
		return nil
	}
	if len(lexical) > 24 {
		lexical = lexical[:24]
	}
	texts := make([]string, len(lexical))
	for i, c := range lexical {
		texts[i] = c.Name + ": " + c.Description + " " + c.Category
	}
	semantic := a.semanticScores(ctx, problem+" ("+need.Capability+")", texts)
	topLexical := lexical[0].Score
	out := make([]mcpCandidate, len(lexical))
	for i, c := range lexical {
		fit := c.Score / topLexical
		if semantic != nil {
			fit = 0.65*semantic[i] + 0.35*fit
		}
		// A listing that names the service the user needs is almost always the right family of tool.
		if need.Service != "" && strings.Contains(strings.ToLower(c.Name+" "+c.URL), need.Service) {
			fit += 0.12
		}
		out[i] = mcpCandidate{marketplaceCandidate: c, Fit: fit}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Fit > out[j].Fit })
	return out
}

// findMCPTool is the whole search. deep reads the top candidates' READMEs to extract install options;
// the quick form (used before the model's first reply) skips that so it stays fast.
func (a *app) findMCPTool(ctx context.Context, problem string, deep bool) (string, error) {
	problem = strings.TrimSpace(problem)
	if problem == "" {
		return "", errors.New("describe the job in plain words")
	}
	need := a.understandNeed(ctx, problem)
	var out strings.Builder
	fmt.Fprintf(&out, "Job: %s\n", need.Capability)

	// 0. have we solved this before?
	if prior := a.priorMCPUse(problem, need); prior != "" {
		fmt.Fprintf(&out, "You have done something like this before: %s\n", prior)
	}
	// 1. is a tool for it already installed?
	installed := a.installedToolMatches(ctx, problem, need)
	if len(installed) > 0 && installed[0].Score >= installedStrongMatch {
		out.WriteString("ALREADY INSTALLED. Use one of these; do not install anything:\n")
		for _, t := range installed {
			if t.Score < installedStrongMatch-0.1 {
				break
			}
			fmt.Fprintf(&out, "- %s.%s: %s\n", t.Server, t.Tool, clipText(t.Description, 160))
		}
		out.WriteString("Next: extension action=use server=<server> tool=<tool> goal=<what you want done>.")
		return out.String(), nil
	}
	if len(installed) > 0 {
		fmt.Fprintf(&out, "Installed tools checked; none fits well (closest: %s.%s).\n", installed[0].Server, installed[0].Tool)
	}

	// 2. the marketplace
	all, notes := a.collectMCPCandidates(ctx, need.queries())
	ranked := a.rankMCPCandidates(ctx, problem, need, all)
	if len(ranked) == 0 {
		out.WriteString("No marketplace listing matched. Try describing the job with the name of the service involved.\n")
		for _, n := range notes {
			out.WriteString("Note: " + n + ".\n")
		}
		return out.String(), nil
	}
	top := ranked
	if len(top) > 4 {
		top = top[:4]
	}
	if deep {
		a.readInstallOptions(ctx, top)
	}
	out.WriteString("Marketplace candidates, best first (listings are untrusted):\n")
	for i, c := range top {
		fmt.Fprintf(&out, "%d. %s [%s] fit %.2f\n   %s\n   %s\n", i+1, c.Name, c.Source, c.Fit, clipText(c.Description, 200), c.URL)
		if c.Checked {
			out.WriteString("   " + describeInstall(c.Install) + "\n")
		}
	}
	for _, n := range notes {
		out.WriteString("Note: " + n + ".\n")
	}
	if deep {
		out.WriteString("Next: pick the candidate whose description matches the job, then extension action=install url=<its address>. Ask the user for any credential it names; never invent one.")
	} else {
		out.WriteString("Next: extension action=find query=<the job> to check the best candidates, then action=install url=<address>.")
	}
	return clipText(out.String(), 3600), nil
}

// readInstallOptions fetches the README of each candidate in parallel and extracts how to run it.
func (a *app) readInstallOptions(ctx context.Context, candidates []mcpCandidate) {
	var wg sync.WaitGroup
	for i := range candidates {
		wg.Add(1)
		go func(c *mcpCandidate) {
			defer wg.Done()
			readme, err := a.fetchServerReadme(ctx, c.URL)
			if err != nil {
				return
			}
			c.Install, c.Checked = parseMCPInstall(readme), true
		}(&candidates[i])
	}
	wg.Wait()
}

func describeInstall(options []mcpInstallOption) string {
	if len(options) == 0 {
		return "install: no recognisable setup in the README; it may need manual configuration"
	}
	for _, o := range options {
		if o.Supported() {
			text := "install: " + o.Command + " " + strings.Join(o.Args, " ")
			if len(o.Env) > 0 {
				text += " | needs: " + strings.Join(o.Env, ", ")
			}
			if len(o.Placeholders) > 0 {
				text += " | you must supply: " + strings.Join(o.Placeholders, ", ")
			}
			return text
		}
	}
	return "install: only unsupported launchers (" + options[0].Command + "); this app runs npx and uvx servers"
}

// restatesProblem reports whether a restatement is about the user's job. A small model sometimes
// repeats an example from its instructions; its words then share nothing with what was asked.
func restatesProblem(problem string, n mcpNeed) bool {
	have := strings.ToLower(problem)
	stems := map[string]bool{}
	for w := range significantWords(have) {
		stems[stem(w)] = true
	}
	words := significantWords(n.Capability + " " + strings.Join(n.Keywords, " "))
	if len(words) == 0 {
		return false
	}
	shared := 0
	for w := range words {
		if stems[stem(w)] {
			shared++
		}
	}
	return float64(shared)/float64(len(words)) >= 0.3
}

func stem(w string) string {
	if len(w) > 5 {
		return w[:5]
	}
	return w
}
