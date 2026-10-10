package skills

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type ProviderSkill struct {
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	Path          string `json:"path"`
	Scope         string `json:"scope"`
	Enabled       bool   `json:"enabled"`
	UserInvocable bool   `json:"userInvocable"`
	Invocation    string `json:"invocation"`
}

var providerSkillName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_-]{0,127}$`)

func ValidProviderSkill(skill ProviderSkill) bool {
	return providerSkillName.MatchString(skill.Name) && len(skill.Description) <= 4096 && len(skill.Path) <= 4096 &&
		(skill.Scope == "project" || skill.Scope == "personal") &&
		(skill.Invocation == "$"+skill.Name || skill.Invocation == "/"+skill.Name)
}

func DiscoverProviderSkills(worktree, home string) ([]ProviderSkill, error) {
	result := make([]ProviderSkill, 0)
	if worktree == "" {
		return result, nil
	}
	roots := []struct {
		path, scope, prefix string
	}{
		{filepath.Join(worktree, ".agents", "skills"), "project", "$"},
		{filepath.Join(worktree, ".claude", "skills"), "project", "/"},
	}
	if home != "" {
		roots = append(roots, struct{ path, scope, prefix string }{filepath.Join(home, ".codex", "skills"), "personal", "$"})
	}
	seen := make(map[string]bool)
	for _, root := range roots {
		entries, err := os.ReadDir(root.path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("discover provider skills: %w", err)
		}
		for _, entry := range entries {
			file := filepath.Join(root.path, entry.Name(), "SKILL.md")
			skill, err := readProviderSkill(file, entry.Name(), root.scope, root.prefix)
			if err != nil || seen[strings.ToLower(skill.Name)] {
				continue
			}
			seen[strings.ToLower(skill.Name)] = true
			result = append(result, skill)
		}
	}
	return result, nil
}

func readProviderSkill(path, fallback, scope, prefix string) (ProviderSkill, error) {
	file, err := os.Open(path)
	if err != nil {
		return ProviderSkill{}, err
	}
	content, readErr := io.ReadAll(io.LimitReader(file, 65536))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return ProviderSkill{}, err
	}
	frontmatter, err := splitFrontmatter(content)
	if err != nil {
		return ProviderSkill{}, err
	}
	var metadata struct {
		Name          string `yaml:"name"`
		Description   string `yaml:"description"`
		Enabled       *bool  `yaml:"enabled"`
		UserInvocable *bool  `yaml:"user-invocable"`
	}
	if err := yaml.Unmarshal(frontmatter, &metadata); err != nil {
		return ProviderSkill{}, err
	}
	name := strings.TrimSpace(metadata.Name)
	if name == "" {
		name = fallback
	}
	skill := ProviderSkill{
		Name: name, Description: strings.TrimSpace(metadata.Description), Path: path, Scope: scope,
		Enabled: metadata.Enabled == nil || *metadata.Enabled, UserInvocable: metadata.UserInvocable == nil || *metadata.UserInvocable,
		Invocation: prefix + name,
	}
	if !ValidProviderSkill(skill) {
		return ProviderSkill{}, errors.New("invalid provider skill metadata")
	}
	return skill, nil
}
