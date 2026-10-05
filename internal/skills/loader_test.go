package skills

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBuiltinSkillsMatchRepositorySources(t *testing.T) {
	t.Parallel()
	result, err := LoadBuiltin()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skills) == 0 || len(result.Dropped) > 0 {
		t.Fatalf("built-in skills = %#v", result)
	}
	for _, skill := range result.Skills {
		t.Run(skill.Name, func(t *testing.T) {
			embedded, err := builtinFiles.ReadFile(skill.BodyPath)
			if err != nil {
				t.Fatal(err)
			}
			source := filepath.Join("..", "..", DefaultPath, filepath.Base(skill.BodyPath))
			canonical, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(embedded, canonical) {
				t.Fatalf("built-in skill %s differs from %s; update both copies together", skill.BodyPath, source)
			}
		})
	}
}

func TestLoadReadsSkillsDeterministicallyDeduplicatesAndCaps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		maxSkills   int
		extraSkills int
		wantSkills  int
		wantCapped  string
	}{
		{name: "explicit cap", maxSkills: 2, wantSkills: 2, wantCapped: "lint"},
		{name: "default has headroom", extraSkills: 97, wantSkills: 100},
		{name: "default cap", extraSkills: 98, wantSkills: 100, wantCapped: "skill-098"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			workspace := t.TempDir()
			skillsDir := filepath.Join(workspace, ".detent", "skills")
			writeSkill(t, skillsDir, "01-alpha.md", "deploy", "Deploy changes.", "Issue mentions deploys.")
			writeSkill(t, skillsDir, "02-duplicate.md", "deploy", "Duplicate deploy.", "Issue mentions deploys again.")
			writeSkill(t, skillsDir, "03-migrate.md", "migrate", "Add migrations.", "Issue mentions schema changes.")
			writeSkill(t, skillsDir, "04-lint.md", "lint", "Fix lint.", "Issue mentions lint failures.")
			for i := 1; i <= tt.extraSkills; i++ {
				name := fmt.Sprintf("skill-%03d", i)
				writeSkill(t, skillsDir, "05-"+name+".md", name, "Test skill.", "Test skill loading.")
			}

			result, err := Load(workspace, Options{MaxSkillsInPrompt: tt.maxSkills})
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}

			if len(result.Skills) != tt.wantSkills {
				t.Fatalf("skills len = %d, want %d", len(result.Skills), tt.wantSkills)
			}
			if result.Skills[0].Name != "deploy" || result.Skills[1].Name != "migrate" {
				t.Fatalf("skills order = %#v, want deploy then migrate", result.Skills)
			}
			if result.Skills[0].Description != "Deploy changes." {
				t.Fatalf("Description = %q", result.Skills[0].Description)
			}
			if !strings.HasSuffix(result.Skills[0].BodyPath, filepath.Join(".detent", "skills", "01-alpha.md")) {
				t.Fatalf("BodyPath = %q", result.Skills[0].BodyPath)
			}
			wantDropped := 1
			if tt.wantCapped != "" {
				wantDropped++
			}
			if len(result.Dropped) != wantDropped {
				t.Fatalf("dropped len = %d, want %d: %#v", len(result.Dropped), wantDropped, result.Dropped)
			}
			if result.Dropped[0].Reason != DropReasonDuplicate || !strings.Contains(result.Dropped[0].Message, `duplicate skill name "deploy"`) {
				t.Fatalf("duplicate drop = %#v", result.Dropped[0])
			}
			if tt.wantCapped != "" && (result.Dropped[1].Reason != DropReasonMaxSkillsInPrompt || result.Dropped[1].Name != tt.wantCapped) {
				t.Fatalf("over-limit drop = %#v, want %s", result.Dropped[1], tt.wantCapped)
			}
		})
	}
}

func TestLoadReportsInvalidSkillsAndKeepsValidSkills(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	skillsDir := filepath.Join(workspace, ".detent", "skills")
	writeSkill(t, skillsDir, "good.md", "good", "Good skill.", "Issue mentions a good path.")
	writeFile(t, filepath.Join(skillsDir, "missing-name.md"), `---
description: Missing name.
when_to_use: Never.
---
Body
`)
	writeFile(t, filepath.Join(skillsDir, "not-frontmatter.md"), "Body only\n")

	result, err := Load(workspace, Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(result.Skills) != 1 {
		t.Fatalf("skills len = %d, want 1: %#v", len(result.Skills), result.Skills)
	}
	if result.Skills[0].Name != "good" {
		t.Fatalf("skill name = %q, want good", result.Skills[0].Name)
	}
	if len(result.Dropped) != 2 {
		t.Fatalf("dropped len = %d, want 2: %#v", len(result.Dropped), result.Dropped)
	}

	messages := result.Dropped[0].Message + "\n" + result.Dropped[1].Message
	for _, want := range []string{"name is required", "missing front matter"} {
		if !strings.Contains(messages, want) {
			t.Fatalf("errors missing %q:\n%s", want, messages)
		}
	}
}

func TestLoadSkillAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		aliases     string
		wantAliases []string
		wantDrop    bool
	}{
		{name: "absent aliases"},
		{name: "merged aliases", aliases: "aliases:\n  - old-auth\n  - old-cleanup\n", wantAliases: []string{"old-auth", "old-cleanup"}},
		{name: "scalar aliases", aliases: "aliases: old-auth\n", wantDrop: true},
		{name: "empty alias", aliases: "aliases:\n  - ''\n", wantDrop: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			workspace := t.TempDir()
			writeFile(t, filepath.Join(workspace, ".detent", "skills", "skill.md"), "---\nname: current\n"+tt.aliases+"description: Test.\nwhen_to_use: Test.\n---\nBody\n")
			result, err := Load(workspace, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantDrop {
				if len(result.Skills) != 0 || len(result.Dropped) != 1 || result.Dropped[0].Reason != DropReasonInvalid {
					t.Fatalf("Load() = %#v, want one invalid drop", result)
				}
				return
			}
			if len(result.Skills) != 1 || !slices.Equal(result.Skills[0].Aliases, tt.wantAliases) {
				t.Fatalf("Load() = %#v, want aliases %v", result, tt.wantAliases)
			}
		})
	}
}

func TestLoadMissingDirectoryIsEmpty(t *testing.T) {
	t.Parallel()

	result, err := Load(t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(result.Skills) != 0 || len(result.Dropped) != 0 {
		t.Fatalf("Load() = %#v, want empty result", result)
	}
}

func TestLoadRejectsUnsafePath(t *testing.T) {
	t.Parallel()

	_, err := Load(t.TempDir(), Options{Path: "../skills"})
	if err == nil {
		t.Fatal("Load() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "relative path inside the workspace") {
		t.Fatalf("Load() error = %v, want workspace-relative path error", err)
	}
}

func writeSkill(t *testing.T, dir string, name string, skillName string, description string, whenToUse string) {
	t.Helper()

	writeFile(t, filepath.Join(dir, name), `---
name: `+skillName+`
description: `+description+`
when_to_use: `+whenToUse+`
---
Body should stay out of prompt.
`)
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
