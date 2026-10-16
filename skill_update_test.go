package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDraftMustBeDrawnFromTheTask(t *testing.T) {
	a := newTestApp(t)
	transcript := significantWords("USER: set up the printer CALL shell open settings add printer by address print a test page RESULT ok")
	invented := skillDraft{Name: "cook-pasta", Description: "Use when the user wants to cook pasta tonight.", Instructions: "1. Boil water in a large pot.\n2. Add salted spaghetti and stir gently.\n3. Drain and serve with sauce."}
	if err := a.saveSkillProposal(invented, "test", transcript); err == nil {
		t.Errorf("a draft about something the task never touched must be refused")
	}
	grounded := skillDraft{Name: "install-printer", Description: "Use when the user wants a printer added.", Instructions: "1. Open settings.\n2. Add the printer by address.\n3. Print a test page."}
	if err := a.saveSkillProposal(grounded, "test", transcript); err != nil {
		t.Errorf("a draft taken from the task should pass: %v", err)
	}
}

func TestDraftCanImproveOwnSkillAfterApproval(t *testing.T) {
	a := newTestApp(t)
	dir := filepath.Join(a.workspace, "skills", "weekly-report")
	_ = os.MkdirAll(dir, 0700)
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: weekly-report\ndescription: Summarise the week\n---\nOld steps."), 0600)
	better := skillDraft{Name: "weekly-report", Description: "Summarise the week from notes and calendar.", Instructions: "1. Read the notes.\n2. Read the calendar.\n3. Write the summary to a file."}
	if err := a.saveSkillProposal(better, "test", nil); err != nil {
		t.Fatal(err)
	}
	proposals := a.listSkillProposals()
	if len(proposals) != 1 || !proposals[0].Update {
		t.Fatalf("proposals = %+v", proposals)
	}
	if body, _ := a.readSkill("weekly-report"); !strings.Contains(body, "Old steps.") {
		t.Fatalf("the skill must not change before approval: %q", body)
	}
	if err := a.approveSkillProposal("weekly-report"); err != nil {
		t.Fatal(err)
	}
	if body, _ := a.readSkill("weekly-report"); !strings.Contains(body, "Read the calendar") {
		t.Errorf("approved update missing: %q", body)
	}
	if baks, _ := filepath.Glob(filepath.Join(dir, "SKILL.md.bak-*")); len(baks) != 1 {
		t.Errorf("the replaced version should be kept, found %d backups", len(baks))
	}
	shared := filepath.Join(a.dataDir, "skills", "shared-one")
	_ = os.MkdirAll(shared, 0700)
	_ = os.WriteFile(filepath.Join(shared, "SKILL.md"), []byte("---\nname: shared-one\ndescription: Shared thing\n---\nx"), 0600)
	if err := a.saveSkillProposal(skillDraft{Name: "shared-one", Description: "Something quite different here.", Instructions: better.Instructions}, "test", nil); err == nil {
		t.Errorf("shared skills must not be replaceable from a draft")
	}
}
