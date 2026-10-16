package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Using an extension tool correctly. A one-billion-parameter model fills in tool arguments badly, so
// the arguments are produced under the tool's own input schema (llama-server then cannot emit anything
// that does not fit), cleaned, and checked for required fields before the call is made. What worked is
// remembered, so the same job later goes straight to the right tool.

// grammarSchema reduces a tool's JSON Schema to the parts a decoding grammar can enforce. Keywords it
// cannot enforce (references, unions, conditionals) are dropped rather than risk an unusable grammar.
func grammarSchema(schema map[string]any, depth int) map[string]any {
	out := map[string]any{}
	if depth > 6 {
		return out
	}
	typ, _ := schema["type"].(string)
	switch typ {
	case "string", "integer", "number", "boolean", "null":
		out["type"] = typ
	case "array":
		out["type"] = "array"
		if items, ok := schema["items"].(map[string]any); ok {
			out["items"] = grammarSchema(items, depth+1)
		}
		out["maxItems"] = 20
	case "object", "":
		props, _ := schema["properties"].(map[string]any)
		if typ == "" && props == nil {
			return out // anything goes
		}
		out["type"] = "object"
		clean := map[string]any{}
		for key, raw := range props {
			if sub, ok := raw.(map[string]any); ok {
				clean[key] = grammarSchema(sub, depth+1)
			}
		}
		out["properties"] = clean
		var required []string
		if list, ok := schema["required"].([]any); ok {
			for _, r := range list {
				if name, ok := r.(string); ok && clean[name] != nil {
					required = append(required, name)
				}
			}
		}
		if len(required) > 0 {
			out["required"] = required
		}
	}
	if values, ok := schema["enum"].([]any); ok && len(values) > 0 && len(values) <= 50 {
		out["enum"] = values
	}
	if typ == "string" {
		out["maxLength"] = 2000
	}
	return out
}

// cleanArguments removes the empty values a model tends to fill optional fields with, and reports
// required fields that are still missing.
func cleanArguments(args map[string]any, schema map[string]any) (map[string]any, []string) {
	required := map[string]bool{}
	if list, ok := schema["required"].([]any); ok {
		for _, r := range list {
			if name, ok := r.(string); ok {
				required[name] = true
			}
		}
	}
	out := map[string]any{}
	for key, value := range args {
		empty := value == nil
		switch v := value.(type) {
		case string:
			empty = strings.TrimSpace(v) == ""
		case []any:
			empty = len(v) == 0
		case map[string]any:
			empty = len(v) == 0
		}
		if empty && !required[key] {
			continue
		}
		out[key] = value
	}
	var missing []string
	for name := range required {
		if v, ok := out[name]; !ok || v == nil || v == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return out, missing
}

const useArgumentsPrompt = `Fill in the arguments for one tool call.
Use only values the goal or context states. Leave out any optional field the goal does not need. Never invent identifiers, paths, addresses or credentials: if a required value is not stated, give an empty string for it.`

// useMCPTool builds the arguments for a goal, calls the tool and remembers the outcome.
func (a *app) useMCPTool(ctx context.Context, server, toolName, goal, context_ string) (string, error) {
	if !mcpNameRE.MatchString(server) {
		return "", errors.New("give the configured server name")
	}
	if strings.TrimSpace(toolName) == "" || strings.TrimSpace(goal) == "" {
		return "", errors.New("give the tool name and the goal in plain words")
	}
	client, err := a.mcpClientFor(ctx, server)
	if err != nil {
		return "", err
	}
	tools, err := client.listTools(ctx)
	if err != nil {
		return "", err
	}
	var tool *mcpToolDefinition
	for i := range tools {
		if tools[i].Name == toolName {
			tool = &tools[i]
		}
	}
	if tool == nil {
		return "", errors.New("that tool is not offered by the server; use action=search to see its tools")
	}
	arguments := map[string]any{}
	hasInputs := false
	if props, ok := tool.InputSchema["properties"].(map[string]any); ok && len(props) > 0 {
		hasInputs = true
	}
	if hasInputs {
		schema := grammarSchema(tool.InputSchema, 0)
		input := "Tool: " + tool.Name + "\nWhat it does: " + clipText(tool.Description, 400) + "\nGoal: " + clipText(goal, 600)
		if strings.TrimSpace(context_) != "" {
			input += "\nContext: " + clipText(context_, 800)
		}
		callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		reply, _, callErr := a.callModel(callCtx, &modelGatewayCall{
			Session: "mcp-arguments", Step: -1, UserText: input, JSONSchema: schema,
			Messages: []chatMsg{{Role: "system", Content: useArgumentsPrompt}, {Role: "user", Content: input}}, emit: func(agentEvent) {},
		})
		cancel()
		if callErr != nil {
			return "", fmt.Errorf("could not work out the arguments: %w", callErr)
		}
		var raw map[string]any
		if json.Unmarshal([]byte(extractJSON(reply)), &raw) != nil {
			return "", errors.New("could not work out the arguments; call it with action=call and arguments set")
		}
		var missing []string
		arguments, missing = cleanArguments(raw, tool.InputSchema)
		if len(missing) > 0 {
			schemaJSON, _ := json.Marshal(tool.InputSchema["properties"])
			return fmt.Sprintf("I cannot call %s.%s yet: the goal does not give %s. Ask the user for it, then call use again with that in context. Inputs: %s",
				server, tool.Name, strings.Join(missing, ", "), clipText(string(schemaJSON), 600)), nil
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	result, err := client.callTool(callCtx, tool.Name, arguments)
	if err != nil {
		return "", fmt.Errorf("%s.%s failed: %w", server, tool.Name, err)
	}
	encoded, _ := json.Marshal(arguments)
	a.recordMCPUse(goal, server, tool.Name, string(encoded))
	return fmt.Sprintf("Called %s.%s with %s\n\n%s", server, tool.Name, clipText(string(encoded), 400), clipText(result, 7000)), nil
}

// --- what worked before -----------------------------------------------------------------------

type mcpUse struct {
	Goal   string `json:"goal"`
	Server string `json:"server"`
	Tool   string `json:"tool"`
	Args   string `json:"args"`
	Count  int    `json:"count"`
	Last   string `json:"last"`
}

func (a *app) mcpUsePath() string { return filepath.Join(a.dataDir, "mcp-usage.json") }

func (a *app) loadMCPUses() []mcpUse {
	var uses []mcpUse
	if data, err := os.ReadFile(a.mcpUsePath()); err == nil {
		_ = json.Unmarshal(data, &uses)
	}
	return uses
}

// recordMCPUse notes a call that worked. The first success for a tool also drafts a skill for it.
func (a *app) recordMCPUse(goal, server, tool, args string) {
	a.agent.extensions.mu.Lock()
	uses := a.loadMCPUses()
	found := false
	for i := range uses {
		if uses[i].Server == server && uses[i].Tool == tool {
			uses[i].Count++
			uses[i].Last = time.Now().UTC().Format(time.RFC3339)
			if jaccard(significantWords(uses[i].Goal), significantWords(goal)) < 0.5 {
				uses[i].Goal = clipText(goal, 200) // the newest kind of job it was used for
			}
			found = true
		}
	}
	if !found {
		uses = append(uses, mcpUse{Goal: clipText(goal, 200), Server: server, Tool: tool, Args: clipText(args, 300), Count: 1, Last: time.Now().UTC().Format(time.RFC3339)})
	}
	if len(uses) > 100 {
		uses = uses[len(uses)-100:]
	}
	if data, err := json.Marshal(uses); err == nil {
		_ = os.WriteFile(a.mcpUsePath(), data, 0600)
	}
	a.agent.extensions.mu.Unlock()
	if !found {
		a.proposeToolSkill(server, tool, goal, args)
	}
}

// priorMCPUse describes an earlier success that resembles this job.
func (a *app) priorMCPUse(problem string, need mcpNeed) string {
	want := significantWords(problem + " " + need.Capability + " " + strings.Join(need.Keywords, " "))
	best, bestScore := mcpUse{}, 0.0
	for _, u := range a.loadMCPUses() {
		if score := jaccard(want, significantWords(u.Goal)); score > bestScore {
			best, bestScore = u, score
		}
	}
	if bestScore < 0.25 {
		return ""
	}
	return fmt.Sprintf("%s.%s worked for %q (%d times). Try extension action=use server=%s tool=%s.", best.Server, best.Tool, best.Goal, best.Count, best.Server, best.Tool)
}

// proposeToolSkill drafts a skill, written by Go from what actually worked, for the user to approve.
func (a *app) proposeToolSkill(server, tool, goal, args string) {
	name := serverNameSlug("use-" + server + "-" + tool)
	instructions := fmt.Sprintf("1. Call extension with action=use, server=%s, tool=%s and goal set to what the user wants done, in plain words.\n"+
		"2. If it says a value is missing, ask the user for that value and call again with it in context.\n"+
		"3. Tell the user what was done. An example that worked: %s", server, tool, clipText(args, 200))
	_ = a.saveSkillProposal(skillDraft{
		Name:         name,
		Description:  clipText("Use when the user wants something like: "+strings.TrimSpace(goal)+" (the "+server+" extension, tool "+tool+")", 190),
		Instructions: instructions,
	}, "an extension tool worked for the first time", nil)
}

func serverNameSlug(s string) string {
	s = strings.Trim(serverNameJunk.ReplaceAllString(strings.ToLower(s), "-"), "-")
	return clipText(s, 40)
}
