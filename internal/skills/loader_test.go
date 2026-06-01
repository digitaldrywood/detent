package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadReadsSkillsDeterministicallyDeduplicatesAndCaps(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	skillsDir := filepath.Join(workspace, ".detent", "skills")
	writeSkill(t, skillsDir, "01-alpha.md", "deploy", "Deploy changes.", "Issue mentions deploys.")
	writeSkill(t, skillsDir, "02-duplicate.md", "deploy", "Duplicate deploy.", "Issue mentions deploys again.")
	writeSkill(t, skillsDir, "03-migrate.md", "migrate", "Add migrations.", "Issue mentions schema changes.")
	writeSkill(t, skillsDir, "04-lint.md", "lint", "Fix lint.", "Issue mentions lint failures.")

	result, err := Load(workspace, Options{MaxSkillsInPrompt: 2})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(result.Skills) != 2 {
		t.Fatalf("skills len = %d, want 2: %#v", len(result.Skills), result.Skills)
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
	if len(result.Errors) != 1 {
		t.Fatalf("errors len = %d, want 1: %#v", len(result.Errors), result.Errors)
	}
	if !strings.Contains(result.Errors[0].Message, `duplicate skill name "deploy"`) {
		t.Fatalf("duplicate error = %q", result.Errors[0].Message)
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
	if len(result.Errors) != 2 {
		t.Fatalf("errors len = %d, want 2: %#v", len(result.Errors), result.Errors)
	}

	messages := result.Errors[0].Message + "\n" + result.Errors[1].Message
	for _, want := range []string{"name is required", "missing front matter"} {
		if !strings.Contains(messages, want) {
			t.Fatalf("errors missing %q:\n%s", want, messages)
		}
	}
}

func TestLoadMissingDirectoryIsEmpty(t *testing.T) {
	t.Parallel()

	result, err := Load(t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(result.Skills) != 0 || len(result.Errors) != 0 {
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
