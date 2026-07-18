package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type ProjectDefinitionMigrationOperation struct {
	Action string      `json:"action"`
	Path   string      `json:"path"`
	Mode   fs.FileMode `json:"mode"`
}

type ProjectDefinitionMigrationPlan struct {
	WorkflowPath    string                                `json:"workflow_path"`
	BeforeLayout    ProjectDefinitionLayout               `json:"before_layout"`
	AfterLayout     ProjectDefinitionLayout               `json:"after_layout"`
	LegacyKeys      []string                              `json:"legacy_keys,omitempty"`
	LocalLegacyKeys []string                              `json:"local_legacy_keys,omitempty"`
	Operations      []ProjectDefinitionMigrationOperation `json:"operations"`
	SemanticDiff    string                                `json:"semantic_diff"`
	Noop            bool                                  `json:"noop"`
	changes         []projectDefinitionMigrationChange
	expectedConfig  Config
}

type projectDefinitionMigrationChange struct {
	path         string
	content      []byte
	mode         fs.FileMode
	existed      bool
	originalHash [sha256.Size]byte
}

type projectDefinitionMigrationOS struct {
	rename func(string, string) error
	remove func(string) error
}

func PlanProjectDefinitionMigration(path string) (ProjectDefinitionMigrationPlan, error) {
	sources, err := readProjectDefinitionSources(path)
	if err != nil {
		return ProjectDefinitionMigrationPlan{}, err
	}
	shared, err := splitProjectWorkflow(sources.Workflow)
	if err != nil {
		return ProjectDefinitionMigrationPlan{}, fmt.Errorf("parse %s: %w", sources.WorkflowPath, err)
	}
	local := projectWorkflowDocument{}
	if sources.HasLocalWorkflow {
		local, err = splitProjectWorkflow(sources.LocalWorkflow)
		if err != nil {
			return ProjectDefinitionMigrationPlan{}, fmt.Errorf("parse %s: %w", sources.LocalWorkflowPath, err)
		}
	}
	legacyKeys, err := projectDefinitionKeys(shared.frontmatter)
	if err != nil {
		return ProjectDefinitionMigrationPlan{}, fmt.Errorf("parse legacy workflow config: %w", err)
	}
	localLegacyKeys, err := projectDefinitionKeys(local.frontmatter)
	if err != nil {
		return ProjectDefinitionMigrationPlan{}, fmt.Errorf("parse local legacy workflow config: %w", err)
	}
	layout, migratable := classifyProjectDefinition(sources, shared, local)
	plan := ProjectDefinitionMigrationPlan{
		WorkflowPath:    path,
		BeforeLayout:    layout,
		AfterLayout:     ProjectDefinitionSplit,
		LegacyKeys:      legacyKeys,
		LocalLegacyKeys: localLegacyKeys,
		SemanticDiff:    "effective Detent configuration: unchanged",
	}

	if layout == ProjectDefinitionSplit {
		workflow, loadErr := ParseProjectDefinition(sources)
		if loadErr != nil {
			return ProjectDefinitionMigrationPlan{}, loadErr
		}
		if validateErr := workflow.Config.Validate(); validateErr != nil {
			return ProjectDefinitionMigrationPlan{}, fmt.Errorf("validate split project definition: %w", validateErr)
		}
		plan.Noop = true
		plan.SemanticDiff = "no semantic changes; project definition already uses the split layout"
		return plan, nil
	}
	if !migratable {
		return ProjectDefinitionMigrationPlan{}, &ProjectDefinitionError{
			Definition: ProjectDefinition{
				Layout:            layout,
				WorkflowPath:      sources.WorkflowPath,
				ConfigPath:        sources.ConfigPath,
				LocalWorkflowPath: sources.LocalWorkflowPath,
				LocalConfigPath:   sources.LocalConfigPath,
				LegacyKeys:        legacyKeys,
				LocalLegacyKeys:   localLegacyKeys,
			},
			Err: mixedProjectDefinitionError(sources, shared, local),
		}
	}

	before, err := migrationEffectiveWorkflow(sources, shared, local, layout)
	if err != nil {
		return ProjectDefinitionMigrationPlan{}, err
	}
	if err := before.Config.Validate(); err != nil {
		return ProjectDefinitionMigrationPlan{}, fmt.Errorf("validate legacy effective configuration: %w", err)
	}
	plan.expectedConfig = before.Config

	candidate := sources
	if !sources.HasConfig {
		candidate.Config, err = schemaConfigBytes(shared.frontmatter, shared.lineEnding)
		if err != nil {
			return ProjectDefinitionMigrationPlan{}, fmt.Errorf("encode detent.yaml: %w", err)
		}
		candidate.HasConfig = true
		candidate.Workflow = append([]byte(nil), shared.prompt...)
	}
	if local.hasFrontmatter {
		candidate.LocalWorkflow = append([]byte(nil), local.prompt...)
		if len(bytes.TrimSpace(local.frontmatter)) > 0 {
			candidate.LocalConfig, err = schemaConfigBytes(local.frontmatter, local.lineEnding)
			if err != nil {
				return ProjectDefinitionMigrationPlan{}, fmt.Errorf("encode detent.local.yaml: %w", err)
			}
			candidate.HasLocalConfig = true
		}
	}

	after, err := ParseProjectDefinition(candidate)
	if err != nil {
		return ProjectDefinitionMigrationPlan{}, fmt.Errorf("validate proposed split project definition: %w", err)
	}
	if err := after.Config.Validate(); err != nil {
		return ProjectDefinitionMigrationPlan{}, fmt.Errorf("validate proposed split effective configuration: %w", err)
	}
	equivalent, err := semanticallyEqualProjectConfigs(before.Config, after.Config)
	if err != nil {
		return ProjectDefinitionMigrationPlan{}, fmt.Errorf("compare legacy and split project definitions: %w", err)
	}
	if !equivalent {
		return ProjectDefinitionMigrationPlan{}, errors.New("proposed split project definition is not semantically equivalent to the legacy definition")
	}

	if !sources.HasConfig {
		if err := plan.addCreate(sources.ConfigPath, candidate.Config, fileModeOrDefault(sources.WorkflowPath, 0o644)); err != nil {
			return ProjectDefinitionMigrationPlan{}, err
		}
		if err := plan.addRewrite(sources.WorkflowPath, sources.Workflow, candidate.Workflow); err != nil {
			return ProjectDefinitionMigrationPlan{}, err
		}
	}
	if local.hasFrontmatter {
		if err := plan.addRewrite(sources.LocalWorkflowPath, sources.LocalWorkflow, candidate.LocalWorkflow); err != nil {
			return ProjectDefinitionMigrationPlan{}, err
		}
		if candidate.HasLocalConfig {
			if err := plan.addCreate(sources.LocalConfigPath, candidate.LocalConfig, fileModeOrDefault(sources.LocalWorkflowPath, 0o600)); err != nil {
				return ProjectDefinitionMigrationPlan{}, err
			}
		}
	}
	sort.SliceStable(plan.Operations, func(i int, j int) bool {
		return plan.Operations[i].Path < plan.Operations[j].Path
	})
	return plan, nil
}

func semanticallyEqualProjectConfigs(left Config, right Config) (bool, error) {
	leftRaw, err := yaml.Marshal(left)
	if err != nil {
		return false, err
	}
	rightRaw, err := yaml.Marshal(right)
	if err != nil {
		return false, err
	}
	return bytes.Equal(leftRaw, rightRaw) &&
		reflect.DeepEqual(left.configuredFields, right.configuredFields) &&
		left.Tracker.gitHubStatusSourceSet == right.Tracker.gitHubStatusSourceSet &&
		left.Agent.Knowledge.Configured == right.Agent.Knowledge.Configured &&
		left.Deliverable.mergeMethodConfigured == right.Deliverable.mergeMethodConfigured &&
		left.Budget.perDayMaxUSDConfigured == right.Budget.perDayMaxUSDConfigured &&
		left.Budget.perIssueMaxUSDConfigured == right.Budget.perIssueMaxUSDConfigured, nil
}

func ApplyProjectDefinitionMigration(plan ProjectDefinitionMigrationPlan) error {
	return applyProjectDefinitionMigration(plan, projectDefinitionMigrationOS{
		rename: os.Rename,
		remove: os.Remove,
	})
}

func migrationEffectiveWorkflow(
	sources ProjectDefinitionSources,
	shared projectWorkflowDocument,
	local projectWorkflowDocument,
	layout ProjectDefinitionLayout,
) (Workflow, error) {
	if layout == ProjectDefinitionLegacy {
		return parseLegacyProjectDefinition(sources, shared, local)
	}
	if layout != ProjectDefinitionMixed || !sources.HasConfig || !local.hasFrontmatter || sources.HasLocalConfig {
		return Workflow{}, mixedProjectDefinitionError(sources, shared, local)
	}
	sharedRoot, err := parseSchemaConfig(sources.Config, sources.ConfigPath)
	if err != nil {
		return Workflow{}, err
	}
	localRoot, _, err := parseWorkflowDocument(legacyWorkflowBytes(local))
	if err != nil {
		return Workflow{}, fmt.Errorf("parse local legacy workflow config: %w", err)
	}
	if localRoot == nil {
		return Workflow{}, errors.New("parse local legacy workflow config: YAML mapping is missing")
	}
	mergeWorkflowMappings(sharedRoot, localRoot)
	cfg, err := decodeWorkflowConfig(sharedRoot)
	if err != nil {
		return Workflow{}, err
	}
	return Workflow{
		Config: cfg,
		Prompt: mergeWorkflowPrompts(
			normalizeProjectDefinitionPrompt(shared.prompt),
			normalizeProjectDefinitionPrompt(local.prompt),
		),
	}, nil
}

func schemaConfigBytes(frontmatter []byte, lineEnding string) ([]byte, error) {
	if lineEnding == "" {
		lineEnding = "\n"
	}
	frontmatter = bytes.TrimSuffix(frontmatter, []byte(lineEnding))
	var doc yaml.Node
	if err := yaml.Unmarshal(frontmatter, &doc); err != nil {
		return nil, err
	}
	root, err := documentRoot(&doc)
	if err != nil {
		return nil, err
	}
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("workflow frontmatter must be a mapping")
	}
	if mappingKeyIndex(root, "schema") >= 0 {
		return nil, errors.New("legacy workflow frontmatter already declares schema")
	}
	root.Content = append([]*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "schema"},
		{Kind: yaml.ScalarNode, Tag: "!!int", Value: "1"},
	}, root.Content...)
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, err
	}
	if lineEnding == "\r\n" {
		out = bytes.ReplaceAll(out, []byte("\n"), []byte("\r\n"))
	}
	return out, nil
}

func (p *ProjectDefinitionMigrationPlan) addCreate(path string, content []byte, mode fs.FileMode) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("refuse to overwrite existing migration destination %s; resolve it manually", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect migration destination %s: %w", path, err)
	}
	p.changes = append(p.changes, projectDefinitionMigrationChange{
		path:    path,
		content: append([]byte(nil), content...),
		mode:    mode,
	})
	p.Operations = append(p.Operations, ProjectDefinitionMigrationOperation{
		Action: "create",
		Path:   path,
		Mode:   mode,
	})
	return nil
}

func (p *ProjectDefinitionMigrationPlan) addRewrite(path string, original []byte, content []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect migration source %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("migration source %s is not a regular file", path)
	}
	p.changes = append(p.changes, projectDefinitionMigrationChange{
		path:         path,
		content:      append([]byte(nil), content...),
		mode:         info.Mode().Perm(),
		existed:      true,
		originalHash: sha256.Sum256(original),
	})
	p.Operations = append(p.Operations, ProjectDefinitionMigrationOperation{
		Action: "rewrite",
		Path:   path,
		Mode:   info.Mode().Perm(),
	})
	return nil
}

func fileModeOrDefault(path string, fallback fs.FileMode) fs.FileMode {
	info, err := os.Stat(path)
	if err != nil {
		return fallback
	}
	return info.Mode().Perm()
}

type stagedProjectDefinitionChange struct {
	change     projectDefinitionMigrationChange
	stagedPath string
	backupPath string
	installed  bool
	backedUp   bool
}

func applyProjectDefinitionMigration(plan ProjectDefinitionMigrationPlan, ops projectDefinitionMigrationOS) error {
	if plan.Noop {
		return nil
	}
	if len(plan.changes) == 0 {
		return errors.New("migration plan has no staged file changes")
	}
	if ops.rename == nil {
		ops.rename = os.Rename
	}
	if ops.remove == nil {
		ops.remove = os.Remove
	}

	staged := make([]stagedProjectDefinitionChange, 0, len(plan.changes))
	for _, change := range plan.changes {
		if err := verifyMigrationChange(change); err != nil {
			return errors.Join(err, cleanupStagedProjectDefinition(staged, ops))
		}
		entry, err := stageProjectDefinitionChange(change)
		if err != nil {
			return errors.Join(err, cleanupStagedProjectDefinition(staged, ops))
		}
		staged = append(staged, entry)
	}

	for index := range staged {
		if !staged[index].change.existed {
			continue
		}
		backup, err := reserveMigrationPath(staged[index].change.path, ".detent-backup-")
		if err != nil {
			return rollbackProjectDefinitionMigration(staged, ops, err)
		}
		staged[index].backupPath = backup
		if err := ops.rename(staged[index].change.path, backup); err != nil {
			return rollbackProjectDefinitionMigration(staged, ops, fmt.Errorf("back up %s: %w", staged[index].change.path, err))
		}
		staged[index].backedUp = true
	}

	for index := range staged {
		if err := ops.rename(staged[index].stagedPath, staged[index].change.path); err != nil {
			return rollbackProjectDefinitionMigration(staged, ops, fmt.Errorf("install %s: %w", staged[index].change.path, err))
		}
		staged[index].stagedPath = ""
		staged[index].installed = true
	}

	result, err := LoadProjectDefinition(plan.WorkflowPath)
	if err != nil {
		return rollbackProjectDefinitionMigration(staged, ops, fmt.Errorf("validate installed split project definition: %w", err))
	}
	if err := result.Config.Validate(); err != nil {
		return rollbackProjectDefinitionMigration(staged, ops, fmt.Errorf("validate installed split effective configuration: %w", err))
	}
	equivalent, err := semanticallyEqualProjectConfigs(plan.expectedConfig, result.Config)
	if err != nil {
		return rollbackProjectDefinitionMigration(staged, ops, fmt.Errorf("compare installed split project definition: %w", err))
	}
	if !equivalent {
		return rollbackProjectDefinitionMigration(staged, ops, errors.New("installed split project definition is not semantically equivalent to the legacy definition"))
	}

	var cleanupErr error
	for index := range staged {
		if staged[index].backupPath == "" {
			continue
		}
		if err := ops.remove(staged[index].backupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove migration backup %s: %w", staged[index].backupPath, err))
		}
	}
	return cleanupErr
}

func verifyMigrationChange(change projectDefinitionMigrationChange) error {
	if !change.existed {
		if _, err := os.Lstat(change.path); err == nil {
			return fmt.Errorf("refuse to overwrite existing migration destination %s; resolve it manually", change.path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect migration destination %s: %w", change.path, err)
		}
		return nil
	}
	raw, err := os.ReadFile(change.path)
	if err != nil {
		return fmt.Errorf("reread migration source %s: %w", change.path, err)
	}
	if sha256.Sum256(raw) != change.originalHash {
		return fmt.Errorf("migration source %s changed after planning; rerun detent fix", change.path)
	}
	return nil
}

func stageProjectDefinitionChange(change projectDefinitionMigrationChange) (stagedProjectDefinitionChange, error) {
	file, err := os.CreateTemp(filepath.Dir(change.path), "."+filepath.Base(change.path)+".detent-stage-")
	if err != nil {
		return stagedProjectDefinitionChange{}, fmt.Errorf("stage %s: %w", change.path, err)
	}
	stagedPath := file.Name()
	cleanup := func() error {
		return errors.Join(file.Close(), removeProjectDefinitionPath(os.Remove, stagedPath))
	}
	if err := file.Chmod(change.mode.Perm()); err != nil {
		return stagedProjectDefinitionChange{}, errors.Join(
			fmt.Errorf("set staged permissions for %s: %w", change.path, err),
			cleanup(),
		)
	}
	if _, err := file.Write(change.content); err != nil {
		return stagedProjectDefinitionChange{}, errors.Join(
			fmt.Errorf("write staged %s: %w", change.path, err),
			cleanup(),
		)
	}
	if err := file.Sync(); err != nil {
		return stagedProjectDefinitionChange{}, errors.Join(
			fmt.Errorf("sync staged %s: %w", change.path, err),
			cleanup(),
		)
	}
	if err := file.Close(); err != nil {
		return stagedProjectDefinitionChange{}, errors.Join(
			fmt.Errorf("close staged %s: %w", change.path, err),
			removeProjectDefinitionPath(os.Remove, stagedPath),
		)
	}
	return stagedProjectDefinitionChange{change: change, stagedPath: stagedPath}, nil
}

func reserveMigrationPath(target string, pattern string) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+pattern)
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		return "", errors.Join(err, removeProjectDefinitionPath(os.Remove, path))
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func rollbackProjectDefinitionMigration(
	staged []stagedProjectDefinitionChange,
	ops projectDefinitionMigrationOS,
	cause error,
) error {
	rollbackErr := error(nil)
	for index := len(staged) - 1; index >= 0; index-- {
		entry := &staged[index]
		if entry.installed {
			if err := ops.remove(entry.change.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("remove partial migration file %s: %w", entry.change.path, err))
			}
		}
		if entry.backedUp {
			if err := ops.rename(entry.backupPath, entry.change.path); err != nil {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore migration source %s: %w", entry.change.path, err))
			} else {
				entry.backupPath = ""
			}
		}
	}
	return errors.Join(cause, rollbackErr, cleanupStagedProjectDefinition(staged, ops))
}

func cleanupStagedProjectDefinition(staged []stagedProjectDefinitionChange, ops projectDefinitionMigrationOS) error {
	var cleanupErr error
	for _, entry := range staged {
		for _, path := range []string{entry.stagedPath, entry.backupPath} {
			if strings.TrimSpace(path) == "" {
				continue
			}
			cleanupErr = errors.Join(cleanupErr, removeProjectDefinitionPath(ops.remove, path))
		}
	}
	return cleanupErr
}

func removeProjectDefinitionPath(remove func(string) error, path string) error {
	err := remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
