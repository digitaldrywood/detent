package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverProviderSkills(t *testing.T) {
	t.Parallel()
	worktree, home := t.TempDir(), t.TempDir()
	for _, fixture := range []struct {
		root, directory, metadata string
	}{
		{worktree, ".agents/skills/review", "name: review\ndescription: Project review"},
		{worktree, ".claude/skills/only-user", "description: User only\ndisable-model-invocation: true"},
		{worktree, ".claude/skills/private", "name: private\nuser-invocable: false"},
		{worktree, ".agents/skills/disabled", "name: disabled\nenabled: false"},
		{home, ".codex/skills/review", "name: review\ndescription: Personal review"},
		{home, ".codex/skills/personal", "name: personal\ndescription: Personal skill"},
		{home, ".codex/skills/invalid", "name: invalid name"},
	} {
		path := filepath.Join(fixture.root, fixture.directory)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte("---\n"+fixture.metadata+"\n---\nInstructions"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	report, err := DiscoverProviderSkills(worktree, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != 5 {
		t.Fatalf("skills = %#v, want five valid unique entries", report)
	}
	byName := make(map[string]ProviderSkill)
	for _, skill := range report {
		byName[skill.Name] = skill
	}
	for _, test := range []struct {
		name, description, scope, invocation string
		enabled, invocable                   bool
	}{
		{"review", "Project review", "project", "$review", true, true},
		{"only-user", "User only", "project", "/only-user", true, true},
		{"private", "", "project", "/private", true, false},
		{"disabled", "", "project", "$disabled", false, true},
		{"personal", "Personal skill", "personal", "$personal", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			skill := byName[test.name]
			if skill.Description != test.description || skill.Scope != test.scope || skill.Invocation != test.invocation || skill.Enabled != test.enabled || skill.UserInvocable != test.invocable {
				t.Fatalf("skill = %#v", skill)
			}
		})
	}
	for _, test := range []struct{ worktree, home string }{{t.TempDir(), t.TempDir()}, {"", home}} {
		got, err := DiscoverProviderSkills(test.worktree, test.home)
		if err != nil || len(got) != 0 {
			t.Fatalf("absent project skills = %#v, %v", got, err)
		}
	}
}
