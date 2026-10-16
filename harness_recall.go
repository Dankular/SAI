package main

import (
	"os"
	"regexp"
	"strings"
)

// Answering from the notes is a lookup, not a language task, so the harness does it. A question like
// "when is my car service?" is matched against the saved notes and answered in plain words; only when
// nothing matches does it go to the model.

var (
	recallQuestionRE = regexp.MustCompile(`(?i)^\s*(?:hey |ok |please |can you tell me |do you know |tell me )?(?:what|when|where|who|which)(?:'s|’s| is| are| was| were| do| does)?\s+(?:the |my |our |their )?(.+?)\s*\??\s*$`)
)

// verbsToYou maps the third-person verbs notes are written with onto the way they are said to the user.
var verbsToYou = map[string]string{
	"prefers": "you prefer", "likes": "you like", "loves": "you love", "hates": "you hate", "dislikes": "you dislike",
	"wants": "you want", "needs": "you need", "uses": "you use", "has": "you have", "works": "you work", "lives": "you live",
	"studies": "you study", "enjoys": "you enjoy", "drinks": "you drink", "eats": "you eat", "plays": "you play",
	"drives": "you drive", "owns": "you own",
}

// secondPerson restates a note ("Allergic to peanuts", "Dog is called Biscuit") as something said to the
// user ("You're allergic to peanuts", "Your dog is called Biscuit").
func secondPerson(note string) string {
	n := strings.TrimSpace(strings.TrimRight(note, ". "))
	if n == "" {
		return ""
	}
	if i := strings.Index(n, " ("); i > 0 && strings.HasSuffix(n, ")") { // the date stamp
		n = n[:i]
	}
	words := strings.Fields(n)
	first := strings.ToLower(words[0])
	rest := strings.Join(words[1:], " ")
	switch {
	case first == "is" && rest != "":
		return "You're " + rest + "."
	case first == "allergic":
		return "You're allergic " + rest + "."
	case first == "likes" && strings.HasPrefix(strings.ToLower(rest), "to be called"):
		return "You like to be called " + strings.TrimSpace(rest[len("to be called"):]) + "."
	case verbsToYou[first] != "":
		return strings.ToUpper(verbsToYou[first][:1]) + verbsToYou[first][1:] + " " + rest + "."
	case first == "name" && len(words) > 2 && strings.EqualFold(words[1], "is"):
		return "Your name is " + strings.Join(words[2:], " ") + "."
	}
	if subjectRE.MatchString(n) {
		return "Your " + strings.ToLower(n[:1]) + n[1:] + "."
	}
	return n + "."
}

// noteLines lists the bullet notes in the memory files, with the file they came from.
func (a *app) noteLines() (lines []string, fromUser []bool) {
	for _, file := range a.memoryFiles() {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		isUser := strings.EqualFold(filepathBase(file), "USER.md")
		for _, l := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
			l = strings.TrimSpace(l)
			if strings.HasPrefix(l, "- ") {
				lines = append(lines, strings.TrimPrefix(l, "- "))
				fromUser = append(fromUser, isUser)
			}
		}
	}
	return
}

func filepathBase(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// recallAnswer answers a question from the notes. It only answers when one note clearly contains every
// significant word of the question's subject; anything vaguer is left to the model.
func (a *app) recallAnswer(question string) (string, bool) {
	m := recallQuestionRE.FindStringSubmatch(question)
	if m == nil {
		return "", false
	}
	asked := significantWords(m[1])
	delete(asked, "called")
	if len(asked) == 0 {
		return "", false
	}
	lines, fromUser := a.noteLines()
	best, bestScore := -1, 0
	for i, l := range lines {
		have := significantWords(l)
		all := true
		for w := range asked {
			if !have[w] {
				all = false
				break
			}
		}
		if !all {
			continue
		}
		// prefer the note whose subject is exactly what was asked about
		score := len(asked) * 10
		if sm := subjectRE.FindStringSubmatch(l); sm != nil {
			if sw := significantWords(sm[1]); len(sw) == len(asked) {
				score += 5
			}
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	if best < 0 {
		return "", false
	}
	if fromUser[best] {
		return secondPerson(lines[best]), true
	}
	return strings.TrimRight(stripStamp(lines[best]), ". ") + ".", true
}

func stripStamp(s string) string {
	if i := strings.LastIndex(s, " ("); i > 0 && strings.HasSuffix(s, ")") {
		return s[:i]
	}
	return s
}

// agentPath maps a file path the model wrote onto the workspace when it is plainly a mangled copy of
// the workspace path it was told. A one-billion-parameter model often garbles a long absolute path
// ("C:/usersysadmin/Appdata\\Local\\SovereignAI/todo.txt"), which would otherwise put the file somewhere
// unintended or trigger a confusing approval prompt. Any other path is left exactly as given.
func (a *app) agentPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return p
	}
	abs, inside := a.resolvePathRaw(p)
	if inside || !strings.Contains(strings.ToLower(abs), "sovereignai") {
		return p
	}
	slashed := strings.ReplaceAll(abs, "\\", "/")
	if i := strings.LastIndex(strings.ToLower(slashed), "/workspace/"); i >= 0 {
		return filepathJoin(a.workspace, strings.ReplaceAll(slashed[i+len("/workspace/"):], "/", string(os.PathSeparator)))
	}
	return filepathJoin(a.workspace, filepathBase(abs))
}

func parentDir(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i > 0 {
		return p[:i]
	}
	return "."
}

func filepathJoin(dir, name string) string {
	return strings.TrimRight(dir, `\/`) + string(os.PathSeparator) + name
}
