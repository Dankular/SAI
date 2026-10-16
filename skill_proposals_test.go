package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func toolTurn(calls int, failFirst bool) []chatMsg {
	turn := []chatMsg{{Role: "user", Content: "set up the printer: open Settings, choose Printers, add the printer by its address and print a test page"}}
	for i := 0; i < calls; i++ {
		var c toolCall
		c.ID, c.Function.Name, c.Function.Arguments = "c", "shell", `{"command":"x"}`
		result := "ok"
		if failFirst && i == 0 {
			result = "Error: access denied"
		}
		turn = append(turn, chatMsg{Role: "assistant", ToolCalls: []toolCall{c}}, chatMsg{Role: "tool", Content: result})
	}
	return append(turn, chatMsg{Role: "assistant", Content: "Done."})
}

func TestSkillTriggers(t *testing.T) {
	if skillTrigger(toolTurn(2, false)) != "" {
		t.Errorf("a short clean task is not worth a skill")
	}
	if !strings.Contains(skillTrigger(toolTurn(5, false)), "5 tool calls") {
		t.Errorf("five tool calls should trigger")
	}
	if skillTrigger(toolTurn(2, true)) != "it recovered from an error" {
		t.Errorf("error recovery should trigger: %q", skillTrigger(toolTurn(2, true)))
	}
	turn := append(toolTurn(2, false), chatMsg{Role: "user", Content: "No, that didn't work"})
	if skillTrigger(turn) != "the user corrected the approach" {
		t.Errorf("a correction after tool use should trigger: %q", skillTrigger(turn))
	}
	if skillTrigger([]chatMsg{{Role: "user", Content: "no, that's wrong"}}) != "" {
		t.Errorf("a correction with no tool use is not a skill")
	}
}

const goodDraft = `{"name":"Install Printer","description":"Use when the user wants a network printer set up on Windows.","instructions":"1. Open Settings and choose Printers.\n2. Add the printer by its address.\n3. Print a test page to confirm it works."}`

func TestProposalIsHeldUntilApproved(t *testing.T) {
	a := newTestApp(t)
	useFakeModel(t, textChunk(goodDraft))
	a.proposeSkill(context.Background(), "s", toolTurn(5, false))
	proposals := a.listSkillProposals()
	if len(proposals) != 1 || proposals[0].Name != "install-printer" {
		t.Fatalf("proposals = %+v", proposals)
	}
	if _, err := a.readSkill("install-printer"); err == nil {
		t.Fatal("a draft must not be usable before it is approved")
	}
	if strings.Contains(a.skillIndex(), "install-printer") {
		t.Fatal("a draft must not appear in the prompt")
	}
	if n := a.pendingNotices(); len(n) != 1 || !strings.Contains(n[0].Text, "install-printer") {
		t.Errorf("the user should be told: %v", n)
	}
	if err := a.approveSkillProposal("install-printer"); err != nil {
		t.Fatal(err)
	}
	body, err := a.readSkill("install-printer")
	if err != nil || !strings.Contains(body, "test page") {
		t.Errorf("approved skill: %q %v", body, err)
	}
	if len(a.listSkillProposals()) != 0 {
		t.Errorf("the draft should be gone once approved")
	}
	if err := a.approveSkillProposal("install-printer"); err == nil {
		t.Errorf("approving twice must fail")
	}
}

func TestBadDraftsAreRefused(t *testing.T) {
	a := newTestApp(t)
	cases := map[string]skillDraft{
		"bad name":    {Name: "../escape", Description: "Use when something happens often.", Instructions: strings.Repeat("step ", 20)},
		"short steps": {Name: "tiny", Description: "Use when something happens often.", Instructions: "do it"},
		"secret":      {Name: "login-helper", Description: "Use when logging in to the portal.", Instructions: "1. Type the password hunter2abc into the field, then press enter and wait for the page."},
		"no desc":     {Name: "empty-desc", Description: "", Instructions: strings.Repeat("step ", 20)},
		"huge steps":  {Name: "huge", Description: "Use when something happens often.", Instructions: strings.Repeat("step ", 800)},
	}
	for name, d := range cases {
		if err := a.saveSkillProposal(d, "test", nil); err == nil {
			t.Errorf("%s: should have been refused", name)
		}
	}
	good := skillDraft{Name: "weekly-report", Description: "Use when the user wants a summary of the week.", Instructions: "1. Read the notes.\n2. List what happened.\n3. Save the summary to a file."}
	if err := a.saveSkillProposal(good, "test", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.saveSkillProposal(good, "test", nil); err == nil {
		t.Errorf("a duplicate draft must be refused")
	}
	similar := skillDraft{Name: "week-summary", Description: "Use when the user wants a summary of the week.", Instructions: good.Instructions}
	if err := a.saveSkillProposal(similar, "test", nil); err == nil {
		t.Errorf("a near-duplicate description must be refused")
	}
	existing := filepath.Join(a.workspace, "skills", "already")
	_ = os.MkdirAll(existing, 0700)
	_ = os.WriteFile(filepath.Join(existing, "SKILL.md"), []byte("---\nname: already\ndescription: Count the open invoices for a client\n---\nsteps"), 0600)
	dup := skillDraft{Name: "already", Description: "Totally different words about gardening.", Instructions: good.Instructions}
	if err := a.saveSkillProposal(dup, "test", nil); err != nil {
		t.Errorf("a draft named like the assistant's own skill is an update, not an error: %v", err)
	}
	if ps := a.listSkillProposals(); len(ps) == 0 || !ps[0].Update && ps[len(ps)-1].Name == "already" {
		t.Errorf("it should be marked as an update: %+v", ps)
	}
}

func TestRejectAndAPIActions(t *testing.T) {
	a := newTestApp(t)
	if err := a.saveSkillProposal(skillDraft{Name: "weekly-report", Description: "Use when the user wants a summary of the week.", Instructions: "1. Read the notes.\n2. List what happened.\n3. Save the summary to a file."}, "t", nil); err != nil {
		t.Fatal(err)
	}
	if got := a.memorySnapshot().Skills; len(got) != 1 {
		t.Fatalf("snapshot skills = %v", got)
	}
	if err := a.rejectSkillProposal("../../etc"); err == nil {
		t.Errorf("a path must not be accepted as a draft name")
	}
	if err := a.rejectSkillProposal("weekly-report"); err != nil || len(a.listSkillProposals()) != 0 {
		t.Errorf("reject failed: %v", err)
	}
	_ = time.Now
}
