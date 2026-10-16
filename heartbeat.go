package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The heartbeat wakes the assistant every half hour to go through HEARTBEAT.md, a checklist the user
// keeps in the workspace. If the checklist is empty the model is not called at all. If nothing needs
// attention the model says HEARTBEAT_OK and nothing is shown. It only runs when the assistant is awake
// and idle, with the same lookup-only tools as any unattended task.

var heartbeatEvery = 30 * time.Minute

const (
	heartbeatFile = "HEARTBEAT.md"
	heartbeatOK   = "HEARTBEAT_OK"
	heartbeatSeed = "# Heartbeat checklist\n" +
		"While I am awake and idle I go through the bullets below, every 30 minutes. Change that with a line like \"every: 15 minutes\" (5 minutes to 24 hours).\n" +
		"If nothing needs your attention I stay quiet. Add a line starting with \"- \" to turn this on. Examples:\n" +
		"<!-- - Check whether anything in my notes is due today -->\n" +
		"<!-- - Look up the weather in London and tell me only if it will rain -->\n"
)

func (a *app) ensureHeartbeatFile() {
	path := filepath.Join(a.workspace, heartbeatFile)
	if _, err := os.Stat(path); err == nil {
		return
	}
	if os.MkdirAll(a.workspace, 0700) == nil {
		_ = os.WriteFile(path, []byte(heartbeatSeed), 0600)
	}
}

// heartbeatTasks returns the checklist bullets, ignoring HTML comments.
func (a *app) heartbeatTasks() []string {
	data, err := os.ReadFile(filepath.Join(a.workspace, heartbeatFile))
	if err != nil {
		return nil
	}
	text := string(data)
	for {
		start := strings.Index(text, "<!--")
		if start < 0 {
			break
		}
		end := strings.Index(text[start:], "-->")
		if end < 0 {
			text = text[:start]
			break
		}
		text = text[:start] + text[start+end+3:]
	}
	var tasks []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "- ") && len(strings.TrimSpace(t[2:])) > 2 {
			tasks = append(tasks, strings.TrimSpace(t[2:]))
		}
	}
	return tasks
}

// runHeartbeatOnce does one check. It reports whether the model was asked.
func (a *app) runHeartbeatOnce(ctx context.Context) bool {
	tasks := a.heartbeatTasks()
	if len(tasks) == 0 {
		return false
	}
	if a.sup != nil && !a.sup.Agent().Awake {
		return false
	}
	if a.currentWorkStatus().Busy || a.inferenceBusy() {
		return false // the user is talking to it; try again next time
	}
	prompt := "Heartbeat check. Go through this checklist, using your tools only if you must. " +
		"If nothing needs the user's attention, reply with exactly " + heartbeatOK + ". " +
		"Otherwise reply with at most two plain sentences saying what needs their attention.\n\nChecklist:\n- " + strings.Join(tasks, "\n- ")
	var reply strings.Builder
	runCtx, cancel := context.WithTimeout(context.WithValue(ctx, unattendedKey{}, true), 5*time.Minute)
	defer cancel()
	err := a.runTurn(runCtx, turnOptions{Session: "heartbeat", Text: prompt, AllowTools: allowSet(unattendedTools), MaxSteps: defaultUnattendedSteps, NoHarness: true},
		func(e agentEvent) { reply.WriteString(e.Delta) })
	a.resetSession("heartbeat") // a heartbeat leaves no conversation behind
	text := strings.TrimSpace(reply.String())
	cleaned := strings.TrimSpace(strings.ReplaceAll(text, heartbeatOK, ""))
	if err != nil || cleaned == "" || strings.Contains(text, "went around in circles") || strings.Contains(text, "not sure what to say") {
		return true // a failed or empty check is not worth interrupting anyone for
	}
	if message, attention := a.heartbeatVerdict(ctx, tasks, cleaned); attention {
		a.addNotice("Heartbeat: " + message)
	}
	return true
}

func (a *app) startHeartbeat() {
	a.ensureHeartbeatFile()
	go func() {
		timer := time.NewTimer(5 * time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-a.ctx.Done():
				return
			case <-timer.C:
				a.runHeartbeatOnce(a.ctx)
				timer.Reset(a.heartbeatInterval())
			}
		}
	}()
}

// firstSentences keeps the first n sentences of a reply.
func firstSentences(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	count := 0
	for i, r := range text {
		if r == '.' || r == '!' || r == '?' {
			if count++; count == n {
				return text[:i+1]
			}
		}
	}
	return text
}

func heartbeatVerdictSchema() map[string]any {
	return jsonObject([]string{"needs_attention", "message"}, map[string]any{
		"needs_attention": map[string]any{"type": "boolean"}, "message": jsonStr(200),
	})
}

// heartbeatVerdict makes the call the rambling check could not: does anything here need the user's
// attention? The model fills a small form, so it cannot answer both yes and no.
func (a *app) heartbeatVerdict(ctx context.Context, tasks []string, result string) (string, bool) {
	prompt := `A background check just ran. Decide whether the user needs to be told something.
needs_attention is true only when the result names something concrete and current the user should act on or know (a due date, a change, a problem). It is false when the result says nothing is due, nothing was found, or the check failed.
message is one short sentence addressed to the user, or empty when needs_attention is false.`
	input := "Checklist:\n- " + strings.Join(tasks, "\n- ") + "\n\nResult of the check:\n" + clipText(result, 1200)
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	reply, _, err := a.callModel(callCtx, &modelGatewayCall{
		Session: "heartbeat-verdict", Step: -1, UserText: input, JSONSchema: heartbeatVerdictSchema(),
		Messages: []chatMsg{{Role: "system", Content: prompt}, {Role: "user", Content: input}}, emit: func(agentEvent) {},
	})
	if err != nil {
		return "", false
	}
	var verdict struct {
		NeedsAttention bool   `json:"needs_attention"`
		Message        string `json:"message"`
	}
	if json.Unmarshal([]byte(extractJSON(reply)), &verdict) != nil {
		return "", false
	}
	message := strings.TrimSpace(verdict.Message)
	return firstSentences(message, 2), verdict.NeedsAttention && message != ""
}

var heartbeatEveryRE = regexp.MustCompile(`(?im)^\s*every:\s*(\d+)\s*(m|min|mins|minutes?|h|hr|hrs|hours?)\b`)

// heartbeatInterval reads "every: 15 minutes" from the top of HEARTBEAT.md. Anything missing or out of
// range (under 5 minutes, over a day) falls back to the default so the file can never make the model
// run constantly.
func (a *app) heartbeatInterval() time.Duration {
	data, err := os.ReadFile(filepath.Join(a.workspace, heartbeatFile))
	if err != nil {
		return heartbeatEvery
	}
	m := heartbeatEveryRE.FindStringSubmatch(string(data))
	if m == nil {
		return heartbeatEvery
	}
	n, _ := strconv.Atoi(m[1])
	d := time.Duration(n) * time.Minute
	if strings.HasPrefix(strings.ToLower(m[2]), "h") {
		d = time.Duration(n) * time.Hour
	}
	if d < 5*time.Minute || d > 24*time.Hour {
		return heartbeatEvery
	}
	return d
}
