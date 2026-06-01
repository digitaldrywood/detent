package skills

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	DefaultPath              = ".detent/skills"
	DefaultMaxSkillsInPrompt = 50
)

var windowsAbsPathPattern = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

type Skill struct {
	Name        string
	Description string
	WhenToUse   string
	BodyPath    string
}

type ValidationError struct {
	Path    string
	Message string
}

func (e ValidationError) Error() string {
	return e.Path + ": " + e.Message
}

type Options struct {
	Path              string
	MaxSkillsInPrompt int
	Logger            *slog.Logger
}

type Result struct {
	Skills []Skill
	Errors []ValidationError
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
	validationErrors := make([]ValidationError, 0)
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			validationErrors = append(validationErrors, ValidationError{
				Path:    file,
				Message: "failed to read skill: " + err.Error(),
			})
			continue
		}

		skill, validationErr := parseSkill(file, content)
		if validationErr != nil {
			validationErrors = append(validationErrors, *validationErr)
			continue
		}
		skills = append(skills, skill)
	}

	skills, duplicateErrors := rejectDuplicateNames(skills)
	validationErrors = append(validationErrors, duplicateErrors...)

	if len(skills) > maxSkills {
		for _, skill := range skills[maxSkills:] {
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
		Skills: skills,
		Errors: validationErrors,
	}, nil
}

func parseSkill(path string, content []byte) (Skill, *ValidationError) {
	frontmatter, err := splitFrontmatter(content)
	if err != nil {
		return Skill{}, &ValidationError{Path: path, Message: err.Error()}
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(frontmatter, &doc); err != nil {
		return Skill{}, &ValidationError{Path: path, Message: "invalid YAML: " + err.Error()}
	}

	root := yamlRoot(&doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return Skill{}, &ValidationError{Path: path, Message: "front matter must be a mapping"}
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
		return Skill{}, &ValidationError{Path: path, Message: strings.Join(missing, ", ")}
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
	closeIndex := strings.Index(body, "\n---\n")
	if closeIndex >= 0 {
		return []byte(body[:closeIndex]), nil
	}
	if strings.HasSuffix(body, "\n---") {
		return []byte(strings.TrimSuffix(body, "\n---")), nil
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

func rejectDuplicateNames(skillList []Skill) ([]Skill, []ValidationError) {
	seen := make(map[string]string, len(skillList))
	skills := make([]Skill, 0, len(skillList))
	validationErrors := make([]ValidationError, 0)

	for _, skill := range skillList {
		existingPath, ok := seen[skill.Name]
		if ok {
			validationErrors = append(validationErrors, ValidationError{
				Path:    skill.BodyPath,
				Message: fmt.Sprintf("duplicate skill name %q already defined at %s", skill.Name, existingPath),
			})
			continue
		}
		seen[skill.Name] = skill.BodyPath
		skills = append(skills, skill)
	}

	return skills, validationErrors
}

func workspaceRelativePath(workspacePath string, relativePath string) (string, error) {
	workspace := strings.TrimSpace(workspacePath)
	if workspace == "" {
		return "", errors.New("workspace path is required")
	}

	path := strings.TrimSpace(relativePath)
	if path == "" || strings.HasPrefix(path, "~") || filepath.IsAbs(path) ||
		strings.HasPrefix(path, `\`) || windowsAbsPathPattern.MatchString(path) ||
		pathEscapesWorkspace(path) {
		return "", errors.New("path must be a relative path inside the workspace")
	}

	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("absolute workspace path: %w", err)
	}

	return filepath.Join(absWorkspace, filepath.Clean(path)), nil
}

func pathEscapesWorkspace(path string) bool {
	for _, part := range strings.FieldsFunc(path, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if part == ".." {
			return true
		}
	}
	return false
}
