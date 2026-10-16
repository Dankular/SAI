package main

import (
	"regexp"
	"strings"
)

// replyRules tidy what the model says. A one-billion-parameter model narrates what it is about to do
// ("I'll look that up."), repeats itself ("I've saved it. I've stored it."), writes markdown into
// answers that are read aloud, and sometimes claims to have saved something that nothing saved. These
// rules work one sentence at a time so they can run on the stream without delaying it.
type replyRules struct {
	spoken bool
	wrote  func() bool // the notes or a file were really changed this turn
	kept   []map[string]bool
	toolOn bool
}

var (
	narrationStartRE = regexp.MustCompile(`(?i)^(?:(?:sure|okay|ok|alright|certainly|of course|great|right|now|next|first|then)[,!. ]+)*(?:i(?:'ll|’ll| will|'m going to|’m going to| am going to)|let me|let's|one moment|just a (?:moment|sec|second)|give me a (?:moment|second|sec))\b`)
	narrationVerbRE  = regexp.MustCompile(`(?i)\b(look|check|search|fetch|find|pull|compute|calculate|work out|run|write|create|try|get|retrieve|verify|read|list|dig|see|use|open|grab|save|store|add|update|handle|process|put together|take a look|go ahead)\b`)
	progressRE       = regexp.MustCompile(`(?i)^i(?:'m|’m| am) (?:still |now )?(?:looking|checking|searching|working|fetching|pulling|trying|digging|going)\b`)
	claimSavedRE     = regexp.MustCompile(`(?i)\bi(?:'ve|’ve| have)?\s+(?:just\s+)?(?:saved|stored|noted|remembered|written|recorded|added|updated|changed|logged|filed|made a note|put)\b|\bi(?:'ll|’ll| will) (?:remember|make a note|save)\b|\b(?:saved|stored|noted|updated) (?:that|it|this|my notes|your notes|the notes)\b`)
	listMarkRE       = regexp.MustCompile(`^\s*(?:[-*•]|\d+[.)])\s+`)
	mdRE             = regexp.MustCompile("[*_`#]+")
)

func wordSet(s string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') }) {
		if len(w) > 1 {
			set[w] = true
		}
	}
	return set
}

func similar(x, y map[string]bool) float64 {
	if len(x) == 0 || len(y) == 0 {
		return 0
	}
	both := 0
	for w := range x {
		if y[w] {
			both++
		}
	}
	return float64(both) / float64(len(x)+len(y)-both)
}

// sentence decides what to do with one finished sentence: it returns the text to show, or false to drop it.
func (r *replyRules) sentence(s string) (string, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return "", false
	}
	if r.spoken {
		t = strings.TrimSpace(mdRE.ReplaceAllString(listMarkRE.ReplaceAllString(t, ""), ""))
		if t == "" {
			return "", false
		}
	}
	if len(t) < 160 && narrationStartRE.MatchString(t) && narrationVerbRE.MatchString(t) && !strings.Contains(strings.ToLower(t), "let me know") {
		return "", false
	}
	if len(t) < 100 && progressRE.MatchString(t) {
		return "", false
	}
	if claimSavedRE.MatchString(t) && !(r.wrote != nil && r.wrote()) {
		return "", false
	}
	words := wordSet(t)
	for _, earlier := range r.kept {
		if similar(words, earlier) >= 0.6 {
			return "", false
		}
	}
	r.kept = append(r.kept, words)
	return t, true
}

// tidyReply applies the rules to a whole reply (used for what is kept in the conversation history).
func tidyReply(reply string, spoken bool, wrote bool) string {
	rules := &replyRules{spoken: spoken, wrote: func() bool { return wrote }}
	var out strings.Builder
	var buf strings.Builder
	flush := func(sep string) {
		if text, ok := rules.sentence(buf.String()); ok {
			if out.Len() > 0 {
				out.WriteString(sep)
			}
			out.WriteString(text)
		}
		buf.Reset()
	}
	runes := []rune(reply)
	for i, r := range runes {
		buf.WriteRune(r)
		end := r == '\n' || (r == '.' || r == '!' || r == '?') && (i+1 == len(runes) || runes[i+1] == ' ' || runes[i+1] == '\n')
		if end {
			sep := " "
			if r == '\n' {
				sep = "\n"
			}
			flush(sep)
		}
	}
	flush(" ")
	if strings.TrimSpace(out.String()) == "" {
		return reply // never replace a reply with nothing
	}
	return out.String()
}

// replyFilter sits between the model's stream and the page. Text is released a sentence at a time.
type replyFilter struct {
	rules   replyRules
	emit    func(agentEvent)
	buf     strings.Builder
	emitted bool
	dropped []string
	wroteBy map[string]bool // tools seen this turn that change notes or files
	harness bool            // the harness itself wrote to the notes this turn
}

func newReplyFilter(spoken bool, harnessWrote bool, emit func(agentEvent)) *replyFilter {
	f := &replyFilter{emit: emit, harness: harnessWrote, wroteBy: map[string]bool{}}
	f.rules = replyRules{spoken: spoken, wrote: func() bool { return f.harness || len(f.wroteBy) > 0 }}
	return f
}

var writingTools = map[string]bool{"remember": true, "write_file": true, "edit_file": true, "apply_patch": true}

// Event is the agent's emit function, wrapped.
func (f *replyFilter) Event(e agentEvent) {
	if e.Delta == "" {
		if e.Tool != "" {
			f.release(false)
			if writingTools[e.Tool] {
				f.wroteBy[e.Tool] = true
			}
		}
		f.emit(e)
		return
	}
	for _, r := range e.Delta {
		f.buf.WriteRune(r)
		s := f.buf.String()
		if r == '\n' || (r == ' ' && len(s) >= 2 && strings.ContainsRune(".!?", rune(s[len(s)-2]))) {
			f.release(false)
		}
	}
}

// release evaluates the buffered sentence. final is true at the end of the turn, when a sentence with
// no closing punctuation is also complete.
func (f *replyFilter) release(final bool) {
	s := f.buf.String()
	if strings.TrimSpace(s) == "" {
		f.buf.Reset()
		return
	}
	f.buf.Reset()
	sep := ""
	if strings.HasSuffix(s, "\n") {
		sep = "\n"
	} else if strings.HasSuffix(s, " ") {
		sep = " "
	}
	text, keep := f.rules.sentence(s)
	if !keep {
		f.dropped = append(f.dropped, strings.TrimSpace(s))
		return
	}
	f.emitted = true
	f.emit(agentEvent{Delta: text + sep})
}

// Finish ends the turn. If every sentence was dropped, the first one is shown rather than leaving the
// user with silence.
func (f *replyFilter) Finish() {
	f.release(true)
	if !f.emitted && len(f.dropped) > 0 {
		f.emit(agentEvent{Delta: f.dropped[0]})
	}
}
