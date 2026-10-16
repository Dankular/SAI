package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Two more things the harness settles itself because the one-billion-parameter model handles them
// badly: a correction of something it was told before, and a request to do something on a repeat.

var (
	// "Actually my gym day is Thursday now, please correct that" / "No, my dentist is on 3 November"
	correctionLeadRE  = regexp.MustCompile(`(?i)^(?:actually|correction|no|wait|sorry|oops)[,:;!.\s]+(?:i meant\s+|it'?s\s+|that'?s wrong[,:;.\s]+)?(.+)$`)
	correctionTrailRE = regexp.MustCompile(`(?i)[,;.\s]*(?:please\s+)?(?:correct|update|fix|change)\s+(?:that|it|this)(?:\s+please)?\s*[.!]?$|\s+now\s*[.!]?$|[,;.\s]*(?:thanks|thank you|thx|please)\s*[.!]?$`)
	changeToRE        = regexp.MustCompile(`(?i)^(?:please\s+)?(?:change|update|correct)\s+(?:my|the)\s+(.+?)\s+(?:to|so (?:that )?it (?:is|says))\s+(.+?)\s*[.!]?$`)

	// a repeat phrase anywhere in a sentence
	repeatRE = regexp.MustCompile(`(?i)\b((?:every|each)\s+(?:other\s+)?(?:\d+\s+)?(?:second|minute|min|hour|hr|day|week|morning|afternoon|evening|night|monday|tuesday|wednesday|thursday|friday|saturday|sunday)s?(?:\s+(?:at\s+)?\d{1,2}(?::\d{2})?\s*(?:am|pm)?)?|daily(?:\s+(?:at\s+)?\d{1,2}(?::\d{2})?\s*(?:am|pm)?)?|hourly|weekly)\b`)
	// "check the weather", "tell me the news", "set up a task to ..." at the start of an instruction
	instructionLeadRE = regexp.MustCompile(`(?i)^(?:(?:please|can you|could you|would you|i want you to|i'd like you to|i would like you to)\s+)*(?:(?:set up|schedule|create|add|make|start)\s+(?:an?\s+)?(?:recurring\s+|repeating\s+)?(?:task|job|routine|check|reminder)?\s*(?:to|that|which|for|where you)?\s*)?`)
	instructionVerbRE = regexp.MustCompile(`(?i)^(?:check|tell|send|notify|alert|summari[sz]e|give|show|look up|search|run|do|read|find|fetch|list|report|update|back up|clean|scan|remind|schedule|set up|create|add|make)\b`)
	factualRepeatRE   = regexp.MustCompile(`(?i)^(?:i|we|my|he|she|they)\b`)
)

// correctFact handles a correction that restates the fact: saveFact swaps in the new value for the
// same subject, so "gym day is Tuesday" becomes "gym day is Thursday".
func (a *app) correctFact(text string) (string, bool) {
	text = strings.TrimSpace(text)
	var statement string
	if m := changeToRE.FindStringSubmatch(text); m != nil {
		statement = "my " + strings.TrimSpace(m[1]) + " is " + strings.TrimSpace(m[2])
	} else if m := correctionLeadRE.FindStringSubmatch(text); m != nil && !strings.HasSuffix(text, "?") {
		statement = strings.TrimSpace(m[1])
		for i := 0; i < 4; i++ { // "... now, please correct that": peel one ending at a time
			trimmed := strings.TrimSpace(correctionTrailRE.ReplaceAllString(statement, ""))
			if trimmed == statement {
				break
			}
			statement = trimmed
		}
		low := strings.ToLower(statement)
		if !strings.HasPrefix(low, "my ") && !strings.HasPrefix(low, "i ") && !strings.HasPrefix(low, "i'm ") {
			return "", false
		}
	} else {
		return "", false
	}
	statement = strings.TrimSpace(correctionTrailRE.ReplaceAllString(statement, ""))
	note, file := toNote(statement)
	if note == "" || subjectRE.FindStringSubmatch(note) == nil {
		return "", false // no clear subject to replace; leave it to the model
	}
	file = a.fileHoldingSubject(file, note)
	switch a.saveFact(file, note) {
	case "known":
		return "I already have that noted.", true
	case "updated":
		return fmt.Sprintf("Corrected. I now have: %s.", strings.TrimRight(note, ".")), true
	}
	return fmt.Sprintf("Noted: %s.", strings.TrimRight(note, ".")), true
}

// scheduleFromSentence schedules any plain instruction that carries a repeat phrase:
// "check the weather every morning at 7", "can you tell me the news daily at 6pm".
// A statement about the user ("I go to the gym every day") is a fact, not an instruction.
func (a *app) scheduleFromSentence(text string) (string, bool) {
	text = strings.TrimSpace(text)
	loc := repeatRE.FindStringIndex(text)
	if loc == nil || strings.HasSuffix(text, "?") && !strings.HasPrefix(strings.ToLower(text), "can you") && !strings.HasPrefix(strings.ToLower(text), "could you") {
		return "", false
	}
	repeat := text[loc[0]:loc[1]]
	rest := strings.TrimSpace(text[:loc[0]] + " " + text[loc[1]:])
	rest = strings.TrimLeft(strings.Join(strings.Fields(rest), " "), ",.;: ")
	kind := "task"
	if strings.Contains(strings.ToLower(rest), "remind me") {
		kind = "reminder"
		rest = regexp.MustCompile(`(?i)remind me\s*(?:to|about|that)?\s*`).ReplaceAllString(rest, "")
	}
	lead := instructionLeadRE.FindString(rest)
	body := strings.TrimSpace(rest[len(lead):])
	body = strings.TrimSpace(strings.Trim(body, ",.;!? "))
	check := strings.TrimSpace(strings.Join(strings.Fields(instructionLeadRE.ReplaceAllString(rest, "")), " "))
	if factualRepeatRE.MatchString(rest) || (kind == "task" && !instructionVerbRE.MatchString(check) && lead == "") || len(body) < 3 {
		return "", false
	}
	job, err := a.addCronJob(kind, body, "", repeat)
	if err != nil {
		return "I couldn't schedule that: " + err.Error(), true
	}
	if kind == "reminder" {
		return "Okay, I'll remind you to " + body + ". " + describeJob(job), true
	}
	return "Done. I'll do this on my own and show you the result: " + describeJob(job), true
}

// fileHoldingSubject says which notes file already has a note on the same subject, so a correction
// replaces it there instead of adding a second copy to the other file.
func (a *app) fileHoldingSubject(preferred, note string) string {
	for _, file := range []string{preferred, map[string]string{"user": "memory", "memory": "user"}[preferred]} {
		data, err := os.ReadFile(noteFilePath(a, file))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
			body = strings.TrimSpace(noteDateRE.ReplaceAllString(body, ""))
			if sameSubject(body, note) {
				return file
			}
		}
	}
	return preferred
}

var previousRE = regexp.MustCompile(`(?i)\b(?:before|previous(?:ly)?|used to (?:be|have)|originally|earlier value|old (?:one|value)|was it)\b`)
var previousNoise = map[string]bool{"before": true, "previous": true, "previously": true, "used": true, "originally": true, "original": true, "old": true, "what": true, "was": true, "were": true, "which": true, "when": true, "did": true, "had": true, "have": true, "now": true, "change": true, "changed": true, "and": true, "value": true, "one": true, "earlier": true, "tell": true, "remind": true}

// previousValue answers "what was my gym day before?" from the fact_changed history.
func (a *app) previousValue(question string) (string, bool) {
	if !previousRE.MatchString(question) {
		return "", false
	}
	want := map[string]bool{}
	for w := range significantWords(question) {
		if !previousNoise[w] {
			want[w] = true
		}
	}
	if len(want) == 0 {
		return "", false
	}
	events, err := a.loadMemoryEvents()
	if err != nil {
		return "", false
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Event != "fact_changed" {
			continue
		}
		have := significantWords(events[i].Summary + " " + events[i].Information)
		all := true
		for w := range want {
			if !have[w] {
				all = false
				break
			}
		}
		if all {
			a.noteMemoryUse("event:" + events[i].ID)
			return events[i].Information, true
		}
	}
	return "", false
}
