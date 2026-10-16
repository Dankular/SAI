package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Scheduled work: reminders (a note shown to the user when due) and tasks (an agent turn run on
// a schedule, with nobody there to approve anything). The clock, the parsing and the delivery are
// all Go; the model only supplies the words. Jobs and undelivered notices survive restarts.

type cronJob struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"` // reminder | task
	Text     string `json:"text"`
	Next     string `json:"next"` // RFC3339, local time zone
	EverySec int64  `json:"every_sec,omitempty"`
	DailyAt  string `json:"daily_at,omitempty"` // HH:MM
	Weekday  string `json:"weekday,omitempty"`  // with daily_at: only that day
	Created  string `json:"created"`
	LastRun  string `json:"last_run,omitempty"`
	// Tools limits what a task may use; empty means the safe default (see unattendedTools).
	Tools    []string `json:"tools,omitempty"`
	MaxSteps int      `json:"max_steps,omitempty"`
}

type notice struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	At   string `json:"at"`
}

type cronStore struct {
	mu      sync.Mutex
	loaded  bool
	jobs    []cronJob
	notices []notice
	nextSeq int
}

func (a *app) cronPath() string { return filepath.Join(a.dataDir, "cron.json") }

type cronFile struct {
	Jobs    []cronJob `json:"jobs"`
	Notices []notice  `json:"notices"`
}

func (a *app) loadCronLocked() {
	c := &a.agent.cron
	if c.loaded {
		return
	}
	c.loaded = true
	var file cronFile
	if data, err := os.ReadFile(a.cronPath()); err == nil {
		_ = json.Unmarshal(data, &file)
	}
	c.jobs, c.notices = file.Jobs, file.Notices
}

func (a *app) saveCronLocked() {
	c := &a.agent.cron
	data, err := json.MarshalIndent(cronFile{Jobs: c.jobs, Notices: c.notices}, "", " ")
	if err != nil {
		return
	}
	path := a.cronPath()
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0600) == nil {
		_ = os.Rename(tmp, path)
	}
}

func (c *cronStore) newID(prefix string) string {
	c.nextSeq++
	return fmt.Sprintf("%s%d%03d", prefix, time.Now().Unix()%1000000, c.nextSeq%1000)
}

// --- time words ---------------------------------------------------------------

var (
	inRE        = regexp.MustCompile(`^in\s+(\d+|an?|half an)\s*(second|sec|minute|min|hour|hr|day|week)s?$`)
	clockRE     = regexp.MustCompile(`(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?`)
	everyNRE    = regexp.MustCompile(`^every\s+(\d+|other)?\s*(second|minute|min|hour|hr|day|week)s?$`)
	weekdayWord = map[string]time.Weekday{"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday, "thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday}
)

func unitDuration(unit string) time.Duration {
	switch unit {
	case "second", "sec":
		return time.Second
	case "minute", "min":
		return time.Minute
	case "hour", "hr":
		return time.Hour
	case "day":
		return 24 * time.Hour
	case "week":
		return 7 * 24 * time.Hour
	}
	return 0
}

// parseClock reads "3pm", "15:30", "at 9" and returns hour and minute.
func parseClock(s string) (hour, minute int, ok bool) {
	m := clockRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, false
	}
	hour, _ = strconv.Atoi(m[1])
	if m[2] != "" {
		minute, _ = strconv.Atoi(m[2])
	}
	switch m[3] {
	case "pm":
		if hour < 12 {
			hour += 12
		}
	case "am":
		if hour == 12 {
			hour = 0
		}
	}
	if hour > 23 || minute > 59 {
		return 0, 0, false
	}
	return hour, minute, true
}

// parseWhen turns "in 10 minutes", "tomorrow at 9am", "friday 15:30" or "2026-11-03 14:30" into a
// moment in the future. Anything it cannot read is an error the model can relay.
func parseWhen(text string, now time.Time) (time.Time, error) {
	s := strings.ToLower(strings.TrimSpace(strings.Trim(text, ".,!")))
	if s == "" {
		return time.Time{}, errors.New("no time given")
	}
	if m := inRE.FindStringSubmatch(s); m != nil {
		n := 1
		d := unitDuration(m[2])
		switch m[1] {
		case "a", "an":
		case "half an":
			d /= 2
		default:
			n, _ = strconv.Atoi(m[1])
		}
		if n < 1 || n > 10000 {
			return time.Time{}, errors.New("that amount of time is out of range")
		}
		return now.Add(time.Duration(n) * d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, text, now.Location()); err == nil {
			if !t.After(now) {
				return time.Time{}, errors.New("that time has already passed")
			}
			return t, nil
		}
	}
	day := now
	dayGiven, weekdayNamed := false, false
	rest := s
	for _, w := range []string{"tomorrow", "tonight", "today"} {
		if strings.Contains(rest, w) {
			dayGiven = true
			if w == "tomorrow" {
				day = now.AddDate(0, 0, 1)
			}
			rest = strings.Replace(rest, w, " ", 1)
			if w == "tonight" && !regexp.MustCompile(`\d`).MatchString(rest) {
				rest += " 20:00"
			}
			break
		}
	}
	for name, wd := range weekdayWord {
		if strings.Contains(rest, name) {
			delta := (int(wd) - int(now.Weekday()) + 7) % 7
			day, dayGiven, weekdayNamed = now.AddDate(0, 0, delta), true, true
			rest = strings.Replace(rest, name, " ", 1)
			break
		}
	}
	rest = strings.NewReplacer("morning", "9:00", "afternoon", "15:00", "evening", "18:00", "noon", "12:00", "on ", " ").Replace(rest)
	hour, minute, ok := parseClock(rest)
	for _, word := range strings.Fields(clockRE.ReplaceAllString(rest, " ")) {
		if word != "at" && word != "the" && word != "o'clock" && word != "next" && word != "this" {
			ok = false // something we do not understand is left over, such as "yesterday"
		}
	}
	if !ok {
		if dayGiven && strings.TrimSpace(rest) == "" {
			hour, minute, ok = 9, 0, true // "tomorrow" alone means the morning
		} else {
			return time.Time{}, fmt.Errorf("I could not read %q as a time; try \"in 10 minutes\" or \"tomorrow at 9am\"", text)
		}
	}
	t := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, now.Location())
	if !t.After(now) {
		switch {
		case weekdayNamed:
			t = t.AddDate(0, 0, 7) // "friday at 9am" said on Friday afternoon means next Friday
		case !dayGiven:
			t = t.AddDate(0, 0, 1) // "at 9" after 9 means tomorrow's 9
		}
	}
	if !t.After(now) {
		return time.Time{}, errors.New("that time has already passed")
	}
	return t, nil
}

// parseEvery reads "every 2 hours", "hourly", "daily at 8am", "every monday at 9" and returns the
// schedule plus the first time it runs.
func parseEvery(text string, now time.Time) (every time.Duration, dailyAt, weekday string, first time.Time, err error) {
	s := strings.ToLower(strings.TrimSpace(strings.Trim(text, ".,!")))
	s = strings.Replace(s, "each ", "every ", 1)
	for word, dflt := range map[string]string{"morning": "9:00", "afternoon": "15:00", "evening": "18:00", "night": "21:00"} {
		if strings.HasPrefix(s, "every "+word) {
			tail := strings.TrimSpace(strings.TrimPrefix(s, "every "+word))
			if tail == "" {
				tail = "at " + dflt
			}
			s = "every day " + tail
		}
	}
	switch s {
	case "hourly":
		s = "every hour"
	case "daily":
		s = "every day"
	case "weekly":
		s = "every week"
	}
	if m := everyNRE.FindStringSubmatch(s); m != nil {
		n := 1
		if m[1] == "other" {
			n = 2
		} else if m[1] != "" {
			n, _ = strconv.Atoi(m[1])
		}
		every = time.Duration(n) * unitDuration(m[2])
		if every < time.Minute {
			return 0, "", "", time.Time{}, errors.New("a repeat shorter than a minute is not allowed")
		}
		return every, "", "", now.Add(every), nil
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(s, "every "), "daily ")
	rest = strings.TrimPrefix(strings.TrimPrefix(rest, "day "), "on ")
	for name := range weekdayWord {
		if strings.HasPrefix(rest, name) {
			weekday = name
			rest = strings.TrimSpace(strings.TrimPrefix(rest, name))
			break
		}
	}
	hour, minute, ok := parseClock(strings.TrimPrefix(strings.TrimPrefix(rest, "at "), "day at "))
	if !ok && rest == "" && weekday != "" {
		hour, minute, ok = 9, 0, true
	}
	if !ok || !(strings.HasPrefix(s, "every") || strings.HasPrefix(s, "daily")) {
		return 0, "", "", time.Time{}, fmt.Errorf("I could not read %q as a repeat; try \"every 2 hours\" or \"daily at 8am\"", text)
	}
	dailyAt = fmt.Sprintf("%02d:%02d", hour, minute)
	return 0, dailyAt, weekday, nextDaily(dailyAt, weekday, now), nil
}

func nextDaily(dailyAt, weekday string, after time.Time) time.Time {
	hour, minute, _ := parseClock(dailyAt)
	for i := 0; i < 8; i++ {
		day := after.AddDate(0, 0, i)
		t := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, after.Location())
		if !t.After(after) {
			continue
		}
		if weekday != "" && weekdayWord[weekday] != t.Weekday() {
			continue
		}
		return t
	}
	return after.Add(24 * time.Hour)
}

// --- jobs ------------------------------------------------------------------------

const maxCronJobs = 50

func (a *app) addCronJob(kind, text, when, every string) (cronJob, error) {
	return a.addCronJobWith(kind, text, when, every, nil, 0)
}

func (a *app) addCronJobWith(kind, text, when, every string, tools []string, maxSteps int) (cronJob, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return cronJob{}, errors.New("say what to be reminded of or what to do")
	}
	if kind != "task" {
		kind = "reminder"
	}
	now := time.Now()
	job := cronJob{Kind: kind, Text: clipText(text, 400), Created: now.Format(time.RFC3339), Tools: tools, MaxSteps: maxSteps}
	switch {
	case strings.TrimSpace(every) != "":
		d, daily, weekday, first, err := parseEvery(every, now)
		if err != nil {
			return cronJob{}, err
		}
		job.EverySec, job.DailyAt, job.Weekday, job.Next = int64(d/time.Second), daily, weekday, first.Format(time.RFC3339)
	default:
		t, err := parseWhen(when, now)
		if err != nil {
			return cronJob{}, err
		}
		job.Next = t.Format(time.RFC3339)
	}
	c := &a.agent.cron
	c.mu.Lock()
	defer c.mu.Unlock()
	a.loadCronLocked()
	if len(c.jobs) >= maxCronJobs {
		return cronJob{}, errors.New("there are already too many scheduled jobs; remove one first")
	}
	job.ID = c.newID("j")
	c.jobs = append(c.jobs, job)
	a.saveCronLocked()
	return job, nil
}

func (a *app) listCronJobs() []cronJob {
	c := &a.agent.cron
	c.mu.Lock()
	defer c.mu.Unlock()
	a.loadCronLocked()
	jobs := append([]cronJob(nil), c.jobs...)
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].Next < jobs[j].Next })
	return jobs
}

// removeCronJob matches an id exactly, or a job whose text contains the given words.
func (a *app) removeCronJob(which string) (cronJob, bool) {
	which = strings.ToLower(strings.TrimSpace(which))
	if which == "" {
		return cronJob{}, false
	}
	c := &a.agent.cron
	c.mu.Lock()
	defer c.mu.Unlock()
	a.loadCronLocked()
	for i, j := range c.jobs {
		if strings.ToLower(j.ID) == which || strings.Contains(strings.ToLower(j.Text), which) {
			c.jobs = append(c.jobs[:i:i], c.jobs[i+1:]...)
			a.saveCronLocked()
			return j, true
		}
	}
	return cronJob{}, false
}

func describeJob(j cronJob) string {
	when := j.Next
	if t, err := time.Parse(time.RFC3339, j.Next); err == nil {
		when = t.Format("Mon 2 Jan 15:04")
	}
	repeat := ""
	switch {
	case j.EverySec > 0:
		repeat = ", repeats every " + (time.Duration(j.EverySec) * time.Second).String()
	case j.DailyAt != "" && j.Weekday != "":
		repeat = ", every " + j.Weekday + " at " + j.DailyAt
	case j.DailyAt != "":
		repeat = ", every day at " + j.DailyAt
	}
	return fmt.Sprintf("[%s] %s: %s (next %s%s)", j.ID, j.Kind, j.Text, when, repeat)
}

// --- notices ---------------------------------------------------------------------

func (a *app) addNotice(text string) {
	c := &a.agent.cron
	c.mu.Lock()
	defer c.mu.Unlock()
	a.loadCronLocked()
	c.notices = append(c.notices, notice{ID: c.newID("n"), Text: clipText(text, 600), At: time.Now().Format(time.RFC3339)})
	if len(c.notices) > 20 {
		c.notices = c.notices[len(c.notices)-20:]
	}
	a.saveCronLocked()
}

func (a *app) pendingNotices() []notice {
	c := &a.agent.cron
	c.mu.Lock()
	defer c.mu.Unlock()
	a.loadCronLocked()
	return append([]notice(nil), c.notices...)
}

func (a *app) dismissNotice(id string) {
	c := &a.agent.cron
	c.mu.Lock()
	defer c.mu.Unlock()
	a.loadCronLocked()
	kept := c.notices[:0:0]
	for _, n := range c.notices {
		if n.ID != id && id != "all" {
			kept = append(kept, n)
		}
	}
	c.notices = kept
	a.saveCronLocked()
}

// --- the scheduler -----------------------------------------------------------------

type unattendedKey struct{}

// dueJobs returns the jobs whose time has come and moves each to its next run (or removes it).
func (a *app) dueJobs(now time.Time) []cronJob {
	c := &a.agent.cron
	c.mu.Lock()
	defer c.mu.Unlock()
	a.loadCronLocked()
	var due []cronJob
	kept := c.jobs[:0:0]
	for _, j := range c.jobs {
		next, err := time.Parse(time.RFC3339, j.Next)
		if err != nil || next.After(now) {
			kept = append(kept, j)
			continue
		}
		due = append(due, j)
		j.LastRun = now.Format(time.RFC3339)
		switch {
		case j.EverySec > 0:
			step := time.Duration(j.EverySec) * time.Second
			for !next.After(now) {
				next = next.Add(step)
			}
			j.Next = next.Format(time.RFC3339)
			kept = append(kept, j)
		case j.DailyAt != "":
			j.Next = nextDaily(j.DailyAt, j.Weekday, now).Format(time.RFC3339)
			kept = append(kept, j)
		}
	}
	if len(due) > 0 {
		c.jobs = kept
		a.saveCronLocked()
	}
	return due
}

func (a *app) fireJob(ctx context.Context, j cronJob) {
	if j.Kind != "task" {
		a.addNotice("Reminder: " + j.Text)
		return
	}
	var reply strings.Builder
	runCtx, cancel := context.WithTimeout(context.WithValue(ctx, unattendedKey{}, true), 10*time.Minute)
	defer cancel()
	err := a.runTurn(runCtx, turnOptions{Session: "cron-" + j.ID, Text: j.Text, AllowTools: jobTools(j), MaxSteps: jobSteps(j)}, func(e agentEvent) { reply.WriteString(e.Delta) })
	text := strings.TrimSpace(reply.String())
	switch {
	case err != nil:
		a.addNotice("The scheduled task \"" + clipText(j.Text, 80) + "\" could not run: " + err.Error())
	case text == "":
		a.addNotice("The scheduled task \"" + clipText(j.Text, 80) + "\" finished.")
	default:
		a.addNotice("Scheduled task \"" + clipText(j.Text, 80) + "\": " + text)
	}
}

func (a *app) startScheduler() {
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-a.ctx.Done():
				return
			case <-ticker.C:
				a.runDueJobs()
			}
		}
	}()
}

func (a *app) runDueJobs() {
	for _, j := range a.dueJobs(time.Now()) {
		if j.Kind == "task" && a.sup != nil && !a.sup.Agent().Awake {
			a.addNotice("The scheduled task \"" + clipText(j.Text, 80) + "\" was due but the assistant was asleep.")
			continue
		}
		a.fireJob(a.ctx, j)
	}
}

// --- the tool and the harness shortcut ------------------------------------------------

func toolSchedule(ctx context.Context, a *app, args map[string]any) (string, error) {
	switch argString(args, "action") {
	case "add":
		tools, err := a.parseToolList(argString(args, "tools"))
		if err != nil {
			return "", err
		}
		job, err := a.addCronJobWith(argString(args, "kind"), argString(args, "text"), argString(args, "when"), argString(args, "every"), tools, argInt(args, "max_steps", 0))
		if err != nil {
			return "", err
		}
		return "Done. " + describeJob(job), nil
	case "list":
		jobs := a.listCronJobs()
		if len(jobs) == 0 {
			return "Nothing is scheduled.", nil
		}
		lines := make([]string, len(jobs))
		for i, j := range jobs {
			lines[i] = describeJob(j)
		}
		return strings.Join(lines, "\n"), nil
	case "remove":
		job, ok := a.removeCronJob(argString(args, "id"))
		if !ok {
			return "", errors.New("no scheduled job matches; use list to see them")
		}
		return "Removed: " + describeJob(job), nil
	}
	return "", errors.New("action must be add, list or remove")
}

func scheduleToolDef() agentTool {
	return agentTool{
		Name:        "schedule",
		Description: "Set reminders and recurring jobs. add: kind reminder (just tell the user) or task (you run it unattended), text, and when (\"in 10 minutes\", \"tomorrow at 9am\") or every (\"every 2 hours\", \"daily at 8am\"). list. remove by id or words.",
		Parameters: obj([]string{"action"}, map[string]any{
			"action":    map[string]any{"type": "string", "enum": []string{"add", "list", "remove"}, "description": "What to do"},
			"kind":      map[string]any{"type": "string", "enum": []string{"reminder", "task"}, "description": "reminder (default) or task"},
			"text":      str("What to remind, or the instruction to carry out"),
			"when":      str("One-off time"),
			"every":     str("Repeat schedule"),
			"id":        str("Job id or words from its text (remove)"),
			"tools":     str("Task only: the tools it may use, comma separated (default: lookups and reading)"),
			"max_steps": num("Task only: most model calls it may take (default 6)"),
		}),
		Guard: func(a *app, args map[string]any) string {
			if argString(args, "action") == "add" && argString(args, "kind") == "task" {
				return "Schedule a task that runs on its own: " + clipText(argString(args, "text"), 200)
			}
			return ""
		},
		Terminal: true,
		Run:      toolSchedule,
	}
}

var (
	remindLeadRE  = regexp.MustCompile(`(?is)^(?:please\s+|can you\s+|could you\s+)?remind me\s+((?:in|at|on|tomorrow|tonight|today|every|next)\b[^,]*?)\s*(?:,|\s)\s*(?:to|about|that)\s+(.+)$`)
	remindTrailRE = regexp.MustCompile(`(?is)^(?:please\s+|can you\s+|could you\s+)?remind me\s+(?:to\s+|about\s+|that\s+)?(.+?)[,\s]+((?:in\s+\S+\s+\S+|at\s+\d\S*(?:\s*(?:am|pm))?|tomorrow(?:\s+(?:at\s+\d\S*|morning|afternoon|evening))?|tonight|every\s+.+|on\s+\w+(?:\s+at\s+\d\S*)?|daily(?:\s+at\s+\d\S*)?|hourly))\s*[.!]?$`)
)

// scheduleReminder answers "remind me to call mum tomorrow at 9" without the model.
func (a *app) scheduleReminder(text string) (string, bool) {
	var what, when string
	if m := remindTrailRE.FindStringSubmatch(text); m != nil {
		what, when = m[1], m[2]
	} else if m := remindLeadRE.FindStringSubmatch(text); m != nil {
		when, what = m[1], m[2]
	} else {
		return "", false
	}
	what = strings.TrimRight(strings.TrimSpace(what), ".!")
	var job cronJob
	var err error
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(when)), "every") || strings.EqualFold(strings.TrimSpace(when), "daily") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(when)), "daily") || strings.EqualFold(strings.TrimSpace(when), "hourly") {
		job, err = a.addCronJob("reminder", what, "", when)
	} else {
		job, err = a.addCronJob("reminder", what, when, "")
	}
	if err != nil {
		return "I couldn't set that reminder: " + err.Error(), true
	}
	next, _ := time.Parse(time.RFC3339, job.Next)
	return fmt.Sprintf("Okay, I'll remind you to %s on %s.", what, next.Format("Monday 2 January at 15:04")), true
}

var listRemindersRE = regexp.MustCompile(`(?i)\b(?:what|which|show|list|any|do i have)\b.*\b(?:reminders?|scheduled|schedule|alarms?)\b.*$`)

// describeSchedule answers "what reminders do I have?" straight from the job list.
func (a *app) describeSchedule() string {
	jobs := a.listCronJobs()
	if len(jobs) == 0 {
		return "You have nothing scheduled."
	}
	lines := make([]string, len(jobs))
	for i, j := range jobs {
		lines[i] = "- " + describeJob(j)
	}
	return "Here is what's scheduled:\n" + strings.Join(lines, "\n")
}

var recurringTaskRE = regexp.MustCompile(`(?is)^(?:please\s+|can you\s+|could you\s+)?(?:set up|schedule|create|add|make)\s+(?:an?\s+)?((?:daily|hourly|weekly)(?:\s+(?:at\s+)?\d{1,2}(?::\d{2})?\s*(?:am|pm)?)?|every\s+[^,]+?)\s+(?:task|job|check)?\s*(?:to|that|which|for)\s+(.+?)[.!]?$`)

// scheduleRecurringTask answers "set up a daily 8am task to check the weather" without the model.
// The task runs unattended later, where anything needing approval is refused.
func (a *app) scheduleRecurringTask(text string) (string, bool) {
	m := recurringTaskRE.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return "", false
	}
	job, err := a.addCronJob("task", m[2], "", m[1])
	if err != nil {
		return "I couldn't schedule that: " + err.Error(), true
	}
	return "Done. I'll do this on my own and show you the result: " + describeJob(job), true
}

// unattendedTools is what a scheduled task or heartbeat may use unless its job says otherwise:
// look things up and read, never change anything or reach for new extensions.
var unattendedTools = []string{"web_search", "web_fetch", "memory_search", "memory_get", "calculator", "read_file", "list_dir", "read_skill"}

const defaultUnattendedSteps = 6

func allowSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}

func jobTools(j cronJob) map[string]bool {
	if len(j.Tools) > 0 {
		return allowSet(j.Tools)
	}
	return allowSet(unattendedTools)
}

func jobSteps(j cronJob) int {
	if j.MaxSteps > 0 {
		return min(j.MaxSteps, maxAgentSteps)
	}
	return defaultUnattendedSteps
}

// knownToolNames lists the tools a job may be limited to.
func (a *app) knownToolNames() map[string]bool {
	names := map[string]bool{}
	for _, t := range a.agentTools() {
		names[t.Name] = true
	}
	return names
}

// parseToolList reads "web_search, calculator" and rejects names that are not tools. schedule is never
// allowed inside a task, so a task cannot schedule more tasks.
func (a *app) parseToolList(list string) ([]string, error) {
	var out []string
	known := a.knownToolNames()
	for _, name := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		if !known[name] || name == "schedule" {
			return nil, fmt.Errorf("%q is not a tool a task can use", name)
		}
		out = append(out, name)
	}
	return out, nil
}
