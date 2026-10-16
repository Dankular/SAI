package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A skill is a folder holding SKILL.md: a short front-matter block (name, description)
// followed by instructions. This is the same layout OpenClaw and Claude use, so existing
// skills can be dropped in. The model sees only the one-line index; read_skill loads the
// rest when a skill is relevant, which keeps the prompt small.
//
//	<workspace>\skills\<name>\SKILL.md     skills for this assistant (it can write its own)
//	<data dir>\skills\<name>\SKILL.md      skills shared by every workspace

type skillInfo struct {
	Name        string
	Description string
	Path        string // SKILL.md
}

func (a *app) skillRoots() []string {
	return []string{filepath.Join(a.workspace, "skills"), filepath.Join(a.dataDir, "skills")}
}

// parseSkill reads the front matter of a SKILL.md. Missing fields fall back to the folder
// name and the first line of text.
func parseSkill(path, text string) skillInfo {
	info := skillInfo{Name: filepath.Base(filepath.Dir(path)), Path: path}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	body := text
	if strings.HasPrefix(text, "---\n") {
		if end := strings.Index(text[4:], "\n---"); end >= 0 {
			for _, line := range strings.Split(text[4:4+end], "\n") {
				key, value, ok := strings.Cut(line, ":")
				if !ok || strings.HasPrefix(line, " ") {
					continue
				}
				value = strings.Trim(strings.TrimSpace(value), `"'`)
				switch strings.TrimSpace(key) {
				case "name":
					if value != "" {
						info.Name = value
					}
				case "description":
					info.Description = value
				}
			}
			body = text[4+end+4:]
		}
	}
	if info.Description == "" {
		for _, line := range strings.Split(body, "\n") {
			if line = strings.TrimSpace(strings.TrimLeft(line, "# ")); line != "" {
				info.Description = line
				break
			}
		}
	}
	return info
}

func (a *app) listSkills() []skillInfo {
	seen := map[string]bool{}
	var skills []skillInfo
	for _, root := range a.skillRoots() {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			path := filepath.Join(root, e.Name(), "SKILL.md")
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			info := parseSkill(path, string(data))
			if seen[strings.ToLower(info.Name)] {
				continue // the workspace copy wins over the shared one
			}
			seen[strings.ToLower(info.Name)] = true
			skills = append(skills, info)
		}
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills
}

// skillIndex is the prompt section: one line per skill.
func (a *app) skillIndex() string {
	var b strings.Builder
	for _, s := range a.listSkills() {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, clipText(s.Description, 200))
	}
	return b.String()
}

func (a *app) readSkill(name string) (string, error) {
	name = strings.TrimSpace(name)
	for _, s := range a.listSkills() {
		if !strings.EqualFold(s.Name, name) {
			continue
		}
		data, err := os.ReadFile(s.Path)
		if err != nil {
			return "", err
		}
		text := string(data)
		dir := filepath.Dir(s.Path)
		var extra []string
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				if e.Name() != "SKILL.md" {
					extra = append(extra, e.Name())
				}
			}
		}
		if len(extra) > 0 {
			text += "\n\n[This skill's folder is " + dir + ". Other files in it: " + strings.Join(extra, ", ") + ". Use read_file to open them.]"
		}
		return text, nil
	}
	return "", errors.New("there is no skill called " + name + ". Listed skills are in the Skills section of your instructions")
}
