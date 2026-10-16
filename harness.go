package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The harness does, in plain Go, the jobs a one-billion-parameter model is bad at. Before the model
// sees a message the harness decides what can be settled without it:
//
//   - questions with exact answers (the time, arithmetic) are answered directly;
//   - "remember ...", "forget ..." and facts the user volunteers ("I'm allergic to nuts") are written
//     to, updated in, or removed from the notes by code, so memory never depends on the model
//     choosing the right tool;
//   - everything else goes to the model with only the tools that make sense for the message and with
//     the harness's findings (an exact calculation, what it just saved) handed over as notes;
//   - what the model says is tidied on the way out (harness_reply.go): narration, repeats, markdown in
//     spoken answers, and claims of having saved something that was not saved.
//
// harnessPlan is pure apart from the memory writes it reports, so it is easy to test.

type harnessPlan struct {
	Direct string          // answer without calling the model
	Notes  []string        // facts for the model's context this turn
	Tools  map[string]bool // tools the model may use this turn; nil means all
	Wrote  bool            // the harness changed the notes this turn
	Forget string          // what the user asked to forget (the conversation is cleaned of it as well)
}

type harnessRuntime struct {
	mu     sync.Mutex
	plans  map[string]*harnessPlan
	once   sync.Once
	search func(ctx context.Context, query string) (string, error)   // web search; replaced in tests
	fetch  func(ctx context.Context, address string) (string, error) // page fetch used by web lookups; replaced in tests
}

func (a *app) ensureHarness() {
	a.harness.once.Do(func() {
		a.addModelGatewayHook(modelGatewayHook{BeforeCall: a.harnessBeforeCall})
	})
}

func (a *app) setHarnessPlan(session string, plan *harnessPlan) {
	a.harness.mu.Lock()
	if a.harness.plans == nil {
		a.harness.plans = map[string]*harnessPlan{}
	}
	a.harness.plans[session] = plan
	a.harness.mu.Unlock()
}

func (a *app) harnessPlanFor(session string) *harnessPlan {
	a.harness.mu.Lock()
	defer a.harness.mu.Unlock()
	return a.harness.plans[session]
}

// harnessBeforeCall hands the plan to the model call: the notes become context and the tool list is
// narrowed. The model's own copy of the schemas is all that changes; tools still execute from the full list.
func (a *app) harnessBeforeCall(ctx context.Context, call *modelGatewayCall) error {
	plan := a.harnessPlanFor(call.Session)
	if plan == nil {
		return nil
	}
	if len(plan.Notes) > 0 {
		appendGatewaySystemNote(call, "Already handled for you", strings.Join(plan.Notes, "\n"))
	}
	if plan.Tools != nil && len(call.Tools) > 0 {
		kept := call.Tools[:0:0]
		for _, schema := range call.Tools {
			fn, _ := schema["function"].(map[string]any)
			if name, _ := fn["name"].(string); plan.Tools[name] {
				kept = append(kept, schema)
			}
		}
		call.Tools = kept
	}
	return nil
}

// --- planning ------------------------------------------------------------------

var (
	rememberRE = regexp.MustCompile(`(?is)^\s*(?:(?:please|hey|ok|okay|actually|also|and|oh|btw|just|so|right)[, ]+)*(?:(?:can|could|would) you (?:please )?)?(?:remember|make a note|note down|note|save|keep in mind|don'?t forget|do not forget)(?: that| this| to)?[:,]?\s+(.+?)\s*[.!]?\s*$`)
	forgetRE   = regexp.MustCompile(`(?is)^\s*(?:(?:please|hey|ok|okay|actually|also|and|oh)[, ]+)*(?:(?:can|could|would) you (?:please )?)?(?:forget|delete|remove|erase)(?: about| that| the note(?: about| that)?| what i (?:said|told you) about)?\s+(.+?)\s*[.!?]?\s*$`)
	timeRE     = regexp.MustCompile(`(?i)^\s*(?:hey |ok |please )?(?:what(?:'s| is)|tell me|do you know) (?:the )?(?:current )?time(?: is it)?(?: now| right now)?\s*\??\s*$|^\s*what time is it(?: now| right now)?\s*\??\s*$`)
	dayRE      = regexp.MustCompile(`(?i)^\s*what(?:'s| is)? (?:the )?(?:day|date)(?: is it| today| it is)?(?: today)?\s*\??\s*$|^\s*what day is (?:it|today)\s*\??\s*$|^\s*what(?:'s| is) today(?:'s date)?\s*\??\s*$`)
	mathLead   = regexp.MustCompile(`(?i)^\s*(?:hey |ok |please )?(?:what(?:'s| is)|whats|how much is|calculate|compute|work out|tell me|can you (?:calculate|work out)|what do you get (?:for|when you do))\s+`)
	mathWords  = strings.NewReplacer(
		" multiplied by ", " * ", " times ", " * ", " x ", " * ", "×", "*", " plus ", " + ", " minus ", " - ", " take away ", " - ",
		" divided by ", " / ", " over ", " / ", "÷", "/", " to the power of ", " ^ ", " squared", " ^ 2", " cubed", " ^ 3", " mod ", " % ", " modulo ", " % ")
	percentOfRE = regexp.MustCompile(`(?i)(\d[\d,]*(?:\.\d+)?)\s*(?:%|percent|per cent)\s+of\s+(\d[\d,]*(?:\.\d+)?)`)
	exprSpanRE  = regexp.MustCompile(`\(?-?\d[\d,]*(?:\.\d+)?\)?(?:\s*[-+*/^%]\s*\(?-?\d[\d,]*(?:\.\d+)?\)?)+`)
	notMathRE   = regexp.MustCompile(`(?i)\b(am|pm|o'clock|january|february|march|april|may|june|july|august|september|october|november|december|jan|feb|mar|apr|jun|jul|aug|sep|sept|oct|nov|dec)\b`)
)

// harnessPlan decides what happens to one user message. It performs the memory writes it reports.
func (a *app) harnessPlan(text string, now time.Time) *harnessPlan {
	plan := &harnessPlan{}
	text = strings.TrimSpace(text)
	if text == "" {
		return plan
	}
	name := firstName(a.profile().UserName)

	if m := forgetRE.FindStringSubmatch(text); m != nil && !strings.HasSuffix(strings.TrimSpace(m[1]), "?") && len(significantWords(m[1])) > 0 {
		removed := a.forgetNotes(m[1])
		plan.Wrote = len(removed) > 0
		plan.Forget = m[1]
		if len(removed) == 0 {
			plan.Direct = pick(text, "I couldn't find anything about that in my notes.", "I don't have a note about that, so there's nothing to forget.")
		} else {
			plan.Direct = pick(text, "Done. I've forgotten that.", "Consider it forgotten.", "Gone from my notes.")
		}
		return plan
	}
	if m := rememberRE.FindStringSubmatch(text); m != nil && !strings.HasSuffix(strings.TrimSpace(text), "?") {
		note, file := toNote(m[1])
		if note != "" {
			status := a.saveFact(file, note)
			plan.Wrote = status != "known"
			switch status {
			case "known":
				plan.Direct = pick(text, "I already have that noted.", "That's already in my notes.")
			case "updated":
				plan.Direct = fmt.Sprintf("Updated. I now have: %s.", strings.TrimRight(note, "."))
			default:
				plan.Direct = fmt.Sprintf("%s %s.", pick(text, "Noted:", "Got it:", "I'll remember:"), strings.TrimRight(note, "."))
			}
			return plan
		}
	}
	if reply, ok := a.scheduleRecurringTask(text); ok {
		plan.Direct = reply
		return plan
	}
	if reply, ok := a.scheduleFromSentence(text); ok {
		plan.Direct = reply
		plan.Wrote = false
		return plan
	}
	if reply, ok := a.correctFact(text); ok {
		plan.Direct = reply
		plan.Wrote = true
		return plan
	}
	if listRemindersRE.MatchString(text) {
		plan.Direct = a.describeSchedule()
		return plan
	}
	if reply, ok := a.scheduleReminder(text); ok {
		plan.Direct = reply
		return plan
	}
	if timeRE.MatchString(text) {
		plan.Direct = fmt.Sprintf("It's %s.", now.Format("15:04 on Monday 2 January"))
		return plan
	}
	if dayRE.MatchString(text) {
		plan.Direct = fmt.Sprintf("Today is %s.", now.Format("Monday 2 January 2006"))
		return plan
	}
	if expr, whole := arithmeticIn(text); expr != "" {
		if value, err := evalExpression(expr); err == nil {
			shown := sayCalculation(text, expr)
			if whole {
				plan.Direct = fmt.Sprintf("%s is %s.", shown, formatNumber(value))
				return plan
			}
			plan.Notes = append(plan.Notes, fmt.Sprintf("Exact calculation: %s = %s. Use this number; do not work it out yourself.", shown, formatNumber(value)))
		}
	}

	// A question the notes can answer is a lookup, not a language task.
	if answer, ok := a.previousValue(text); ok {
		plan.Direct = answer
		return plan
	}
	if answer, ok := a.recallAnswer(text); ok {
		plan.Direct = answer
		return plan
	}

	// Facts the user volunteered are saved by the harness. When the message is nothing but facts the
	// harness acknowledges them itself; otherwise the model is told they are already saved.
	saved, all, pure := a.learnFacts(text)
	if pure && len(all) > 0 {
		plan.Wrote = len(saved) > 0
		var said []string
		for _, n := range all {
			said = append(said, strings.TrimRight(secondPerson(n), "."))
		}
		plan.Direct = pick(text, "Got it.", "Noted.", "Understood.") + " " + strings.ToUpper(said[0][:1]) + said[0][1:] + strings.Join(prefixAll(said[1:], ", and "), "") + "."
		return plan
	}
	if len(saved) > 0 {
		plan.Wrote = true
		plan.Notes = append(plan.Notes, "You have already saved this to the user's notes: "+strings.Join(saved, "; ")+". Acknowledge it in one short sentence, in your own words"+addressing(name)+". Do not call any tool for it.")
	}
	scheduling := scheduleWordsRE.MatchString(text)
	if !scheduling && (webLookupRE.MatchString(text) || weatherRE.MatchString(text)) {
		if reply, ok := a.weatherNow(context.Background(), text); ok {
			plan.Direct = reply
			return plan
		}
	}
	if !scheduling && webLookupRE.MatchString(text) && strings.Contains(text, " ") {
		if results := a.prefetchWeb(text); results == "" {
			plan.Notes = append(plan.Notes, "The harness tried to look this up on the web and could not. Tell the user you could not check it right now. Do not guess or answer from memory.")
		} else {
			plan.Notes = append(plan.Notes, "The harness already searched the web for this. Answer from these results only, in one or two sentences, and say so if they do not answer it:\n"+results)
		}
	}
	plan.Tools = toolsFor(text)
	return plan
}

// webLookupRE picks out questions about the changing world, which a model with no internet cannot answer
// and a small one will not think to look up.
var webLookupRE = regexp.MustCompile(`(?i)\b(latest|newest|current(?:ly)?|right now|these days|this week|tonight|news|weather|forecast|price of|how much (?:does|is) .* cost|score of|who (?:is|are) the (?:current |new )?(?:president|prime minister|ceo|king|queen|mayor|governor|leader)|(?:stock|share) price|exchange rate|opening hours|release date|when (?:does|is|did) .* (?:release|come out|launch))\b`)

// prefetchWeb runs the search itself, so the model only has to read the answer. A message containing
// something secret-looking from the notes is never sent.
func (a *app) prefetchWeb(question string) string {
	if a.refuseToSendSecrets(question) != nil {
		return ""
	}
	search := a.harness.search
	if search == nil {
		search = func(ctx context.Context, q string) (string, error) {
			return toolWebSearch(ctx, a, map[string]any{"query": q})
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := search(ctx, strings.TrimRight(strings.TrimSpace(question), "?!. "))
	if err != nil || strings.HasPrefix(out, "No results") {
		return ""
	}
	return clipText(out, 900)
}

func firstName(full string) string {
	if f := strings.Fields(full); len(f) > 0 {
		return f[0]
	}
	return ""
}

func addressing(name string) string {
	if name == "" {
		return ""
	}
	return " (you may use their name, " + name + ")"
}

// pick chooses among phrasings by hashing the message, so a reply is varied across messages but the
// same for the same message.
func pick(seed string, options ...string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(seed)))
	return options[int(h.Sum32())%len(options)]
}

// arithmeticIn finds a calculation in a message. whole is true when the message is nothing but the
// question (so it can be answered without the model).
func arithmeticIn(text string) (expr string, whole bool) {
	if notMathRE.MatchString(text) {
		return "", false
	}
	t := " " + strings.TrimSpace(text) + " "
	t = percentOfRE.ReplaceAllStringFunc(t, func(m string) string {
		parts := percentOfRE.FindStringSubmatch(m)
		return " (" + parts[1] + " / 100) * " + parts[2] + " "
	})
	t = mathWords.Replace(t)
	body := strings.TrimRight(strings.TrimSpace(t), "?.! ")
	rest := strings.TrimSpace(mathLead.ReplaceAllString(body+" ", ""))
	if span := exprSpanRE.FindString(rest); span != "" && strings.TrimSpace(span) == rest {
		return rest, true
	}
	if span := exprSpanRE.FindString(t); span != "" {
		return strings.TrimSpace(span), false
	}
	return "", false
}

var verbalizeOps = strings.NewReplacer(" * ", " times ", " / ", " divided by ", " + ", " plus ", " - ", " minus ", " ^ ", " to the power of ", " % ", " mod ")

// sayCalculation words a calculation the way a person would say it.
func sayCalculation(original, expr string) string {
	if m := percentOfRE.FindStringSubmatch(original); m != nil && strings.Contains(expr, "/ 100") {
		return m[1] + " percent of " + m[2]
	}
	spaced := regexp.MustCompile(`\s*([-+*/^%])\s*`).ReplaceAllString(expr, " $1 ")
	return verbalizeOps.Replace(spaced)
}

// --- tools for the turn ----------------------------------------------------------

var (
	fileWordsRE   = regexp.MustCompile(`(?i)\b(file|files|folder|directory|document|read|write|edit|create|rename|open|workspace|patch|\.txt|\.md|\.json|\.go|\.py|\.js)\b|\.\w{1,4}\b`)
	shellWordsRE  = regexp.MustCompile(`(?i)\b(shell|command|run|terminal|bash|powershell|script|install|git|npm|pip|compile|build|process|server|ping|grep)\b`)
	webWordsRE    = regexp.MustCompile(`(?i)\b(search|web|google|look up|lookup|online|internet|latest|newest|news|weather|website|http|https|url|price|stock|score|release|version|who is|who was|when did|wikipedia)\b`)
	memoryEditWordsRE = regexp.MustCompile(`(?i)\b(pin|unpin|correct(?:ion)?|update (?:my|that|the)|change (?:my|that|the)|no longer|not anymore|actually,? (?:i|my)|keep (?:that|this) in mind)\b`)
	scheduleWordsRE = regexp.MustCompile(`(?i)\b(remind|reminders?|schedule[ds]?|alarm|timer|every (?:day|week|hour|morning|evening|\d+ )|daily|hourly|recurring|cron|scheduled)\b`)
	memoryWordsRE = regexp.MustCompile(`(?i)\b(remember|recall|my|did i|told you|notes?|earlier|last time|before|yesterday|appointment|password|where did|when is|what was)\b`)
)

// toolsFor lists the tools worth offering for a message. Fewer tools means fewer wrong calls and a
// shorter prompt. remember is never offered: the harness does the remembering.
func toolsFor(text string) map[string]bool {
	tools := map[string]bool{"extension": true, "calculator": true}
	if fileWordsRE.MatchString(text) {
		for _, n := range []string{"read_file", "write_file", "edit_file", "apply_patch", "list_dir"} {
			tools[n] = true
		}
	}
	if shellWordsRE.MatchString(text) {
		tools["shell"], tools["process"] = true, true
	}
	if webWordsRE.MatchString(text) {
		tools["web_search"], tools["web_fetch"] = true, true
	}
	if memoryWordsRE.MatchString(text) {
		tools["memory_search"], tools["memory_get"] = true, true
	}
	if scheduleWordsRE.MatchString(text) {
		tools["schedule"] = true
	}
	if memoryEditWordsRE.MatchString(text) {
		tools["memory_search"], tools["memory_get"], tools["memory_edit"] = true, true, true
	}
	if strings.Contains(strings.ToLower(text), "skill") {
		tools["read_skill"] = true
	}
	if len(tools) == 2 { // nothing specific: allow the cheap, safe lookups
		tools["memory_search"], tools["web_search"], tools["web_fetch"], tools["read_skill"] = true, true, true, true
	}
	return tools
}

// --- notes: save, update, forget ----------------------------------------------------

var (
	subjectRE        = regexp.MustCompile(`(?i)^(.{3,60}?)\s+(?:is|are|was|were|:)\s+`)
	firstPersonVerbs = map[string]string{"prefer": "prefers", "like": "likes", "love": "loves", "hate": "hates", "dislike": "dislikes", "want": "wants", "need": "needs", "use": "uses", "have": "has", "work": "works", "live": "lives", "study": "studies", "enjoy": "enjoys", "drink": "drinks", "eat": "eats", "play": "plays", "drive": "drives", "own": "owns"}
	verbRE           = regexp.MustCompile(`(?i)^i\s+(?:really |absolutely |always |usually |just )?(prefer|like|love|hate|dislike|want|need|use|have|work|live|study|enjoy|drink|eat|play|drive|own)\b\s*(.*)$`)
)

// toNote turns what the user said into a short note about them, and says which file it belongs in.
func toNote(said string) (note, file string) {
	s := strings.TrimSpace(strings.Trim(said, " \t\r\n\"'"))
	s = strings.TrimRight(s, ".!")
	if s == "" {
		return "", ""
	}
	file = "memory"
	low := strings.ToLower(s)
	personal := strings.HasPrefix(low, "i ") || strings.HasPrefix(low, "i'm ") || strings.HasPrefix(low, "i’m ") || strings.HasPrefix(low, "my ") || strings.Contains(low, " my ") || strings.Contains(low, " me ") || strings.HasPrefix(low, "call me")
	if personal {
		file = "user"
	}
	switch {
	case strings.HasPrefix(low, "i'm ") || strings.HasPrefix(low, "i’m "):
		s = "Is " + s[4:]
	case strings.HasPrefix(low, "i am "):
		s = "Is " + s[5:]
	case strings.HasPrefix(low, "call me "):
		s = "Likes to be called " + s[8:]
	default:
		if m := verbRE.FindStringSubmatch(s); m != nil {
			s = strings.ToUpper(firstPersonVerbs[strings.ToLower(m[1])][:1]) + firstPersonVerbs[strings.ToLower(m[1])][1:] + " " + m[2]
		}
	}
	if strings.HasPrefix(strings.ToLower(s), "my ") {
		s = s[3:]
	}
	s = regexp.MustCompile(`(?i)\bmy\b`).ReplaceAllString(s, "their")
	s = regexp.MustCompile(`(?i)\bme\b`).ReplaceAllString(s, "them")
	s = regexp.MustCompile(`(?i)\bI(?:'m|’m)\b`).ReplaceAllString(s, "they are")
	s = regexp.MustCompile(`\bI\b`).ReplaceAllString(s, "they")
	s = strings.TrimSpace(s)
	if s != "" {
		s = strings.ToUpper(s[:1]) + s[1:]
	}
	return s, file
}

func noteFilePath(a *app, file string) string {
	if file == "user" {
		return filepath.Join(a.workspace, "USER.md")
	}
	return filepath.Join(a.workspace, "MEMORY.md")
}

// saveFact adds a note, or replaces an older note with the same subject ("Dentist appointment is on
// 3 November" replaces "Dentist appointment is on 4 November"). It returns "saved", "updated" or
// "known" (already in the notes, nothing written).
func (a *app) saveFact(file, note string) string {
	path := noteFilePath(a, file)
	existing, _ := os.ReadFile(path)
	text := strings.ReplaceAll(string(existing), "\r\n", "\n")
	if strings.Contains(strings.ToLower(text), strings.ToLower(note)) {
		return "known"
	}
	stamp := time.Now().Format("2006-01-02")
	line := fmt.Sprintf("- %s (%s)", note, stamp)
	if m := subjectRE.FindStringSubmatch(note); m != nil {
		subject := strings.ToLower(strings.TrimSpace(m[1]))
		lines := strings.Split(text, "\n")
		for i, l := range lines {
			body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "-"))
			if sm := subjectRE.FindStringSubmatch(body); sm != nil && strings.ToLower(strings.TrimSpace(sm[1])) == subject {
				lines[i] = line
				if os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600) == nil {
					a.memoryDirty()
					a.recordFactChange(body, note)
					return "updated"
				}
			}
		}
	}
	if a.alreadyKnown(note) {
		return "known" // the same fact in other words, somewhere in the notes
	}
	prefix := ""
	if text != "" && !strings.HasSuffix(text, "\n") {
		prefix = "\n"
	}
	if os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return "known"
	}
	if os.WriteFile(path, []byte(text+prefix+line+"\n"), 0600) != nil {
		return "known"
	}
	a.memoryDirty()
	return "saved"
}

// forgetNotes removes the note lines that best match what the user asked to forget.
func (a *app) forgetNotes(about string) []string {
	want := significantWords(about)
	if len(want) == 0 {
		return nil
	}
	var removed []string
	for _, file := range a.memoryFiles() {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		kept := lines[:0:0]
		changed := false
		for _, l := range lines {
			trimmed := strings.TrimSpace(l)
			if strings.HasPrefix(trimmed, "- ") && len(removed) < 3 {
				have := significantWords(trimmed)
				hits := 0
				for w := range want {
					if have[w] {
						hits++
					}
				}
				if hits > 0 && hits*10 >= len(want)*6 {
					removed = append(removed, strings.TrimPrefix(trimmed, "- "))
					changed = true
					continue
				}
			}
			kept = append(kept, l)
		}
		if changed {
			_ = os.WriteFile(file, []byte(strings.Join(kept, "\n")), 0600)
		}
	}
	if len(removed) > 0 {
		a.memoryDirty()
	}
	return removed
}

// --- facts the user volunteers --------------------------------------------------------

var factPatterns = []struct {
	re   *regexp.Regexp
	note func(m []string) string
}{
	{regexp.MustCompile(`(?i)\bmy name is ([A-Za-z][\w'’-]*(?: [A-Z][\w'’-]*)?)`), func(m []string) string { return "Name is " + m[1] }},
	{regexp.MustCompile(`(?i)\bcall me ([A-Za-z][\w'’-]*)`), func(m []string) string { return "Likes to be called " + m[1] }},
	{regexp.MustCompile(`(?i)\bi(?:'m|’m| am) allergic to ([^.,;!?]+)`), func(m []string) string { return "Allergic to " + strings.TrimSpace(m[1]) }},
	{regexp.MustCompile(`(?i)\bi (?:really |absolutely |always |usually )?(prefer|like|love|hate|dislike|enjoy) ([^.,;!?]{2,60})`), func(m []string) string {
		return strings.ToUpper(firstPersonVerbs[strings.ToLower(m[1])][:1]) + firstPersonVerbs[strings.ToLower(m[1])][1:] + " " + strings.TrimSpace(m[2])
	}},
	{regexp.MustCompile(`(?i)\bi (work|live|study) (at|in|for|as) ([^.,;!?]{2,60})`), func(m []string) string {
		return strings.ToUpper(firstPersonVerbs[strings.ToLower(m[1])][:1]) + firstPersonVerbs[strings.ToLower(m[1])][1:] + " " + m[2] + " " + strings.TrimSpace(m[3])
	}},
	{regexp.MustCompile(`(?i)\bmy (wife|husband|partner|girlfriend|boyfriend|son|daughter|mother|father|mum|mom|dad|brother|sister|dog|cat) (?:is )?(?:called|named) ([A-Z][\w'’-]*)`), func(m []string) string {
		return strings.ToUpper(m[1][:1]) + strings.ToLower(m[1][1:]) + " is called " + m[2]
	}},
	{regexp.MustCompile(`(?i)\bmy (birthday|anniversary|[a-z ]{3,30}?(?:appointment|meeting|flight|deadline|exam|interview)) is (?:on )?([^.;!?]{3,50})`), func(m []string) string {
		return strings.ToUpper(strings.TrimSpace(m[1])[:1]) + strings.TrimSpace(m[1])[1:] + " is on " + strings.TrimSpace(m[2])
	}},
}

// learnFacts saves the facts in a message that are plainly about the user. It returns what it saved, every
// fact it recognised (including ones it already had), and whether the message was nothing but facts.
// A clause that is a question is skipped.
func (a *app) learnFacts(text string) (saved, all []string, pure bool) {
	pure = true
	for _, clause := range splitClauses(text) {
		if strings.Contains(clause, "?") {
			pure = false // a question is never a fact; the rest of the message may still be
			continue
		}
		matched := false
		for _, p := range factPatterns {
			for _, m := range p.re.FindAllStringSubmatch(clause, 2) {
				note := strings.TrimSpace(p.note(m))
				if note == "" || len(note) > 120 {
					continue
				}
				matched = true
				all = append(all, note)
				if status := a.saveFact("user", note); status != "known" {
					saved = append(saved, note)
				}
			}
		}
		if !matched && len(significantWords(clause)) > 0 {
			pure = false
		}
	}
	return saved, all, pure
}

var (
	clauseSepRE   = regexp.MustCompile(`(?i)[.;!]\s+|,\s*(?:and\s+|but\s+)?|\s+(?:and|but)\s+`)
	clauseStartRE = regexp.MustCompile(`(?i)^(?:my|i|i'm|i’m|we)\b`)
)

// splitClauses cuts a message where a new statement about the user begins, so each fact is read on its own:
// at full stops, and at ", and my ..." / "and I ...".
func splitClauses(text string) []string {
	var out []string
	last := 0
	for _, loc := range clauseSepRE.FindAllStringIndex(text, -1) {
		sep := text[loc[0]:loc[1]]
		sentenceEnd := strings.ContainsAny(sep, ".;!")
		if !sentenceEnd && !clauseStartRE.MatchString(text[loc[1]:]) {
			continue
		}
		if part := strings.TrimSpace(text[last:loc[0]]); part != "" {
			out = append(out, part)
		}
		last = loc[1]
	}
	if part := strings.TrimSpace(text[last:]); part != "" {
		out = append(out, part)
	}
	return out
}

// --- compound requests ------------------------------------------------------------

var stepRE = regexp.MustCompile(`(?i)(?:,\s*|\s+)(?:and\s+)?then\s+|(?:,\s*|\s+)(?:and\s+)?(?:after that|afterwards|next),?\s+`)

// splitSteps cuts "do this, then do that" into its steps so each gets the model's full attention (a
// small model asked for two things usually does one). It returns nil when the message is a single task.
func splitSteps(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" || strings.HasPrefix(strings.ToLower(text), "if ") || strings.Contains(text, "?") && !strings.Contains(strings.ToLower(text), "then") {
		return nil
	}
	var steps []string
	last := 0
	for _, loc := range stepRE.FindAllStringIndex(text, -1) {
		steps = append(steps, strings.TrimSpace(text[last:loc[0]]))
		last = loc[1]
	}
	steps = append(steps, strings.TrimSpace(text[last:]))
	var kept []string
	for _, s := range steps {
		if len(strings.Fields(s)) >= 2 {
			kept = append(kept, strings.TrimRight(s, " ,"))
		}
	}
	if len(kept) < 2 || len(kept) != len(steps) || len(kept) > 4 {
		return nil
	}
	return kept
}

// purgeSession removes what was said about a topic from the conversation, so forgetting really
// forgets: the model would otherwise read the old message and repeat it. The current message is kept.
func purgeSession(s *agentSession, about string) int {
	want := significantWords(about)
	if len(want) == 0 || len(s.msgs) < 2 {
		return 0
	}
	last := len(s.msgs) - 1
	kept := s.msgs[:0:0]
	removed := 0
	for i, m := range s.msgs {
		if i < last && (m.Role == "user" || m.Role == "assistant" || m.Role == "tool") {
			have := significantWords(m.Content)
			hits := 0
			for w := range want {
				if have[w] {
					hits++
				}
			}
			if hits > 0 && hits*10 >= len(want)*6 {
				removed++
				continue
			}
		}
		kept = append(kept, m)
	}
	s.msgs = kept
	return removed
}

func prefixAll(items []string, prefix string) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = prefix + strings.ToLower(s[:1]) + s[1:]
	}
	return out
}
