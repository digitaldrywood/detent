package skills

import (
	"embed"
	"fmt"
	"slices"
	"strings"
)

//go:embed builtin/*.md
var builtinFiles embed.FS

func LoadBuiltin() (Result, error) {
	return LoadFS(builtinFiles, Options{Path: "builtin"})
}

func ReadBuiltin(name string) (Skill, string, error) {
	name = strings.TrimSpace(name)
	result, err := LoadBuiltin()
	if err != nil {
		return Skill{}, "", err
	}
	for _, skill := range result.Skills {
		if skill.Name == name || slices.Contains(skill.Aliases, name) {
			content, err := builtinFiles.ReadFile(skill.BodyPath)
			return skill, string(content), err
		}
	}
	return Skill{}, "", fmt.Errorf("unknown built-in skill %q", name)
}
