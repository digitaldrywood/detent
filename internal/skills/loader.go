package skills

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/digitaldrywood/detent/internal/pathsafe"

	"gopkg.in/yaml.v3"
)

const (
	DefaultPath              = ".detent/skills"
	DefaultMaxSkillsInPrompt = 50
)

type Skill struct {
	Name        string
	Description string
	WhenToUse   string
	BodyPath    string
}

type DropReason string

const (
	DropReasonInvalid           DropReason = "invalid"
	DropReasonDuplicate         DropReason = "duplicate"
	DropReasonMaxSkillsInPrompt DropReason = "max_skills_in_prompt"
)

type Drop struct {
	Path    string
	Name    string
	Reason  DropReason
	Message string
}

func (d Drop) Error() string {
	return d.Path + ": " + d.Message
}

type Options struct {
	Path              string
	MaxSkillsInPrompt int
	Logger            *slog.Logger
}

type Result struct {
	Skills  []Skill
	Dropped []Drop
}

func Load(workspacePath string, opts Options) (Result, error) {
	path := opts.Path
	if strings.TrimSpace(path) == "" {
		path = DefaultPath
	}
	maxSkills := opts.MaxSkillsInPrompt
	if maxSkills <= 0 {
		maxSkills = DefaultMaxSkillsInPrompt
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	skillsDir, err := workspaceRelativePath(workspacePath, path)
	if err != nil {
		return Result{}, err
	}

	entries, err := os.ReadDir(skillsDir)
	if errors.Is(err, os.ErrNotExist) {
		return Result{}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("read skills directory: %w", err)
	}

	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.ToLower(filepath.Ext(entry.Name())) != ".md" {
			continue
		}
		files = append(files, filepath.Join(skillsDir, entry.Name()))
	}
	sort.Strings(files)

	skills := make([]Skill, 0, len(files))
	dropped := make([]Drop, 0)
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			dropped = append(dropped, Drop{
				Path:    file,
				Reason:  DropReasonInvalid,
				Message: "failed to read skill: " + err.Error(),
			})
			continue
		}

		skill, drop := parseSkill(file, content)
		if drop != nil {
			dropped = append(dropped, *drop)
			continue
		}
		skills = append(skills, skill)
	}

	skills, duplicateDrops := rejectDuplicateNames(skills)
	dropped = append(dropped, duplicateDrops...)

	if len(skills) > maxSkills {
		for _, skill := range skills[maxSkills:] {
			dropped = append(dropped, Drop{
				Path:    skill.BodyPath,
				Name:    skill.Name,
				Reason:  DropReasonMaxSkillsInPrompt,
				Message: fmt.Sprintf("exceeds max_skills_in_prompt of %d", maxSkills),
			})
			logger.Info(
				"dropped skill from prompt",
				slog.Int("max_skills_in_prompt", maxSkills),
				slog.String("skill_name", skill.Name),
				slog.String("body_path", skill.BodyPath),
			)
		}
		skills = skills[:maxSkills]
	}

	return Result{
		Skills:  skills,
		Dropped: dropped,
	}, nil
}

func parseSkill(path string, content []byte) (Skill, *Drop) {
	frontmatter, err := splitFrontmatter(content)
	if err != nil {
		return Skill{}, &Drop{Path: path, Reason: DropReasonInvalid, Message: err.Error()}
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(frontmatter, &doc); err != nil {
		return Skill{}, &Drop{Path: path, Reason: DropReasonInvalid, Message: "invalid YAML: " + err.Error()}
	}

	root := yamlRoot(&doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return Skill{}, &Drop{Path: path, Reason: DropReasonInvalid, Message: "front matter must be a mapping"}
	}

	fields := map[string]string{}
	missing := make([]string, 0, 3)
	for _, field := range []string{"name", "description", "when_to_use"} {
		value, ok := stringField(root, field)
		if !ok || strings.TrimSpace(value) == "" {
			missing = append(missing, field+" is required")
			continue
		}
		fields[field] = strings.TrimSpace(value)
	}
	if len(missing) > 0 {
		return Skill{}, &Drop{Path: path, Reason: DropReasonInvalid, Message: strings.Join(missing, ", ")}
	}

	return Skill{
		Name:        fields["name"],
		Description: fields["description"],
		WhenToUse:   fields["when_to_use"],
		BodyPath:    path,
	}, nil
}

func splitFrontmatter(content []byte) ([]byte, error) {
	normalized := strings.ReplaceAll(strings.TrimPrefix(string(content), "\ufeff"), "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return nil, errors.New("missing front matter")
	}

	body := normalized[len("---\n"):]
	if strings.HasPrefix(body, "---\n") {
		return []byte{}, nil
	}
	if body == "---" {
		return []byte{}, nil
	}
	before, _, ok := strings.Cut(body, "\n---\n")
	if ok {
		return []byte(before), nil
	}
	if before, ok := strings.CutSuffix(body, "\n---"); ok {
		return []byte(before), nil
	}

	return nil, errors.New("missing closing front matter delimiter")
}

func yamlRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	if doc.Kind == yaml.MappingNode {
		return doc
	}
	return nil
}

func stringField(root *yaml.Node, name string) (string, bool) {
	for i := 0; i < len(root.Content); i += 2 {
		key := root.Content[i]
		value := root.Content[i+1]
		if key.Value != name {
			continue
		}
		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
			return "", false
		}
		return value.Value, true
	}
	return "", false
}

func rejectDuplicateNames(skillList []Skill) ([]Skill, []Drop) {
	seen := make(map[string]string, len(skillList))
	skills := make([]Skill, 0, len(skillList))
	dropped := make([]Drop, 0)

	for _, skill := range skillList {
		existingPath, ok := seen[skill.Name]
		if ok {
			dropped = append(dropped, Drop{
				Path:    skill.BodyPath,
				Name:    skill.Name,
				Reason:  DropReasonDuplicate,
				Message: fmt.Sprintf("duplicate skill name %q already defined at %s", skill.Name, existingPath),
			})
			continue
		}
		seen[skill.Name] = skill.BodyPath
		skills = append(skills, skill)
	}

	return skills, dropped
}

func workspaceRelativePath(workspacePath string, relativePath string) (string, error) {
	return pathsafe.WorkspaceRelative(workspacePath, relativePath)
}
