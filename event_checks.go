package main

import (
	"regexp"
	"strings"
)

// A one-billion-parameter model picks event labels almost at random ("my VPN is fixed" came back as
// decision_changed and as task_unfinished). The fields it fills in and what the user actually said
// are checked here, and an event whose label is not backed up is relabelled note_identified, which
// claims nothing beyond "this was said".

var (
	decisionSaidRE = regexp.MustCompile(`(?i)\b(?:from now on|instead|rather than|switch(?:ed|ing)? to|chang(?:e|ed|ing) (?:to|my|the)|decid(?:e|ed)|going to use|no longer|stop using|won't be using|will use|let's use|we'll use)\b`)
	unfinishedRE   = regexp.MustCompile(`(?i)\b(?:still (?:need|have|to)|haven't (?:finished|done|started)|not (?:done|finished) yet|to ?do|later|next (?:week|time)|remind|come back|finish (?:this|it)|unfinished|continue (?:this|later)|pick (?:this|it) up)\b`)
	resolvedSaidRE = regexp.MustCompile(`(?i)\b(?:fixed|resolved|solved|works now|working now|that worked|it worked|sorted)\b`)
)

func userSaid(messages []observedMessage, re *regexp.Regexp) bool {
	for _, m := range messages {
		if m.Role == "user" && re.MatchString(m.Content) {
			return true
		}
	}
	return false
}

// reconcileEventType returns the candidate with a label its fields and evidence support.
func reconcileEventType(c extractedMemoryEvent, evidence []observedMessage) extractedMemoryEvent {
	switch c.Event {
	case "preference_expressed":
		if strings.TrimSpace(c.Preference) == "" {
			c.Preference = c.Summary
		}
		if !userSaid(evidence, preferenceRE) {
			c.Event = "note_identified" // the user did not actually state a preference
		}
	case "decision_changed":
		if strings.TrimSpace(c.Decision) == "" || !userSaid(evidence, decisionSaidRE) {
			c.Event = "note_identified"
		}
	case "task_unfinished":
		open := strings.TrimSpace(c.NextAction) != "" || strings.TrimSpace(c.Blocker) != "" || strings.TrimSpace(c.Progress) != ""
		if !open || !userSaid(evidence, unfinishedRE) {
			c.Event = "note_identified"
		}
	case "problem_resolved", "solution_confirmed":
		if userSaid(evidence, resolvedSaidRE) && !userSaid(evidence, userConfirmationNegationRE) {
			break
		}
		// without anyone saying it worked it stays a candidate; memoryEventStatus handles that
	}
	return c
}
