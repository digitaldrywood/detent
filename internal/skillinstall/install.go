package skillinstall

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/operatorskill"
)

const (
	ManifestFile  = ".detent-install.json"
	SchemaVersion = 1
	fileMode      = 0o600
	directoryMode = 0o700
)

var (
	ErrConflict   = errors.New("installed skill content differs")
	ErrUnsafePath = errors.New("unsafe skill install path")
)

type Target string

const (
	TargetClaudeCode  Target = "claude-code"
	TargetCodex       Target = "codex"
	TargetAntigravity Target = "antigravity"
)

type Compatibility struct {
	Target       Target   `json:"target"`
	Client       string   `json:"client"`
	RootSegments []string `json:"root_segments"`
	Discovery    string   `json:"discovery"`
}

type Root struct {
	Base   string
	Skills string
}

type Config struct {
	Roots   map[Target]Root
	Targets []Target
	Build   buildinfo.Info
	DryRun  bool
	Force   bool
}

type Bundle struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	SHA256  string `json:"sha256"`
}

type Manifest struct {
	Schema        int            `json:"schema"`
	Skill         string         `json:"skill"`
	BundleVersion int            `json:"bundle_version"`
	BundleSHA256  string         `json:"bundle_sha256"`
	DetentBuild   buildinfo.Info `json:"detent_build"`
}

type Action struct {
	Target     Target `json:"target"`
	Action     string `json:"action"`
	Path       string `json:"path"`
	BackupPath string `json:"backup_path,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
}

type TargetResult struct {
	Target      Target `json:"target"`
	Root        string `json:"root"`
	Destination string `json:"destination"`
	Intent      string `json:"intent"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
}

type Result struct {
	Schema   int            `json:"schema"`
	Status   string         `json:"status"`
	DryRun   bool           `json:"dry_run"`
	Forced   bool           `json:"forced"`
	Bundle   Bundle         `json:"bundle"`
	Build    buildinfo.Info `json:"build"`
	Targets  []TargetResult `json:"targets"`
	Actions  []Action       `json:"actions"`
	Rollback []Action       `json:"rollback,omitempty"`
}

type plannedOperation struct {
	actionIndex  int
	targetIndex  int
	kind         string
	path         string
	data         []byte
	existed      bool
	previous     []byte
	mode         os.FileMode
	previousMode os.FileMode
}

type installPlan struct {
	result     Result
	operations []plannedOperation
}

type fileOperations struct {
	mkdir       func(string, os.FileMode) error
	remove      func(string) error
	atomicWrite func(string, []byte, os.FileMode) error
}

func CompatibilityMatrix() []Compatibility {
	return []Compatibility{
		{
			Target:       TargetClaudeCode,
			Client:       "Claude Code",
			RootSegments: []string{".claude", "skills"},
			Discovery:    "personal skills",
		},
		{
			Target:       TargetCodex,
			Client:       "Codex",
			RootSegments: []string{".agents", "skills"},
			Discovery:    "user skills",
		},
		{
			Target:       TargetAntigravity,
			Client:       "Antigravity",
			RootSegments: []string{".gemini", "config", "skills"},
			Discovery:    "global skills",
		},
	}
}

func Roots(home string) (map[Target]Root, error) {
	home = filepath.Clean(strings.TrimSpace(home))
	if home == "." || !filepath.IsAbs(home) {
		return nil, fmt.Errorf("%w: user home must be absolute", ErrUnsafePath)
	}
	roots := make(map[Target]Root, len(CompatibilityMatrix()))
	for _, compatibility := range CompatibilityMatrix() {
		segments := append([]string{home}, compatibility.RootSegments...)
		roots[compatibility.Target] = Root{
			Base:   home,
			Skills: filepath.Join(segments...),
		}
	}
	return roots, nil
}

func ParseTargets(values []string) ([]Target, error) {
	selected := make(map[Target]bool, len(values))
	var errs []error
	for _, value := range values {
		target := Target(strings.ToLower(strings.TrimSpace(value)))
		switch target {
		case TargetClaudeCode, TargetCodex, TargetAntigravity:
			selected[target] = true
		default:
			errs = append(errs, fmt.Errorf("unsupported skill target %q", value))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	targets := make([]Target, 0, len(selected))
	for _, compatibility := range CompatibilityMatrix() {
		if selected[compatibility.Target] {
			targets = append(targets, compatibility.Target)
		}
	}
	return targets, nil
}

func Install(config Config) (Result, error) {
	return install(config, fileOperations{
		mkdir:       os.Mkdir,
		remove:      os.Remove,
		atomicWrite: atomicWrite,
	})
}

func install(config Config, operations fileOperations) (Result, error) {
	plan, err := planInstall(config)
	if err != nil {
		return plan.result, err
	}
	if config.DryRun {
		plan.result.Status = "dry_run"
		for index := range plan.result.Targets {
			plan.result.Targets[index].Status = "dry_run"
		}
		return plan.result, nil
	}
	return applyPlan(plan, operations)
}

func planInstall(config Config) (installPlan, error) {
	bundleContent := operatorskill.Content()
	bundleDigest := digest(bundleContent)
	build := buildinfo.Normalize(config.Build)
	manifestContent, err := encodeManifest(Manifest{
		Schema:        SchemaVersion,
		Skill:         operatorskill.Name,
		BundleVersion: operatorskill.Version,
		BundleSHA256:  bundleDigest,
		DetentBuild:   build,
	})
	if err != nil {
		return installPlan{}, fmt.Errorf("encode skill install manifest: %w", err)
	}

	plan := installPlan{result: Result{
		Schema: SchemaVersion,
		Status: "planned",
		DryRun: config.DryRun,
		Forced: config.Force,
		Bundle: Bundle{Name: operatorskill.Name, Version: operatorskill.Version, SHA256: bundleDigest},
		Build:  build,
	}}
	if len(config.Targets) == 0 {
		plan.result.Status = "failed"
		return plan, errors.New("at least one skill install target is required")
	}

	var planErrors []error
	seen := make(map[Target]bool, len(config.Targets))
	for _, target := range config.Targets {
		if seen[target] {
			continue
		}
		seen[target] = true
		root, ok := config.Roots[target]
		if !ok {
			planErrors = append(planErrors, fmt.Errorf("%s: target root is not configured", target))
			continue
		}
		targetIndex := len(plan.result.Targets)
		targetPlanErr := planTarget(&plan, targetIndex, target, root, config.Force, bundleContent, manifestContent)
		if targetPlanErr != nil {
			planErrors = append(planErrors, targetPlanErr)
		}
	}

	if len(planErrors) > 0 {
		plan.result.Status = "failed"
		for index := range plan.result.Targets {
			if plan.result.Targets[index].Status == "planned" {
				plan.result.Targets[index].Status = "not_applied"
			}
		}
		for index := range plan.result.Actions {
			if plan.result.Actions[index].Status == "planned" {
				plan.result.Actions[index].Status = "not_applied"
			}
		}
		return plan, errors.Join(planErrors...)
	}
	return plan, nil
}

func planTarget(plan *installPlan, targetIndex int, target Target, root Root, force bool, bundleContent []byte, manifestContent []byte) error {
	base := filepath.Clean(strings.TrimSpace(root.Base))
	skillsRoot := filepath.Clean(strings.TrimSpace(root.Skills))
	destination := filepath.Join(skillsRoot, operatorskill.Directory)
	targetResult := TargetResult{
		Target:      target,
		Root:        skillsRoot,
		Destination: destination,
		Status:      "planned",
	}
	plan.result.Targets = append(plan.result.Targets, targetResult)

	if !filepath.IsAbs(base) || !filepath.IsAbs(skillsRoot) || !within(base, skillsRoot) || !within(skillsRoot, destination) {
		err := fmt.Errorf("%w: %s destination escapes its configured root", ErrUnsafePath, target)
		plan.reject(targetIndex, destination, err)
		return err
	}
	if err := requireDirectory(base); err != nil {
		wrapped := fmt.Errorf("%w: %s base %s: %w", ErrUnsafePath, target, base, err)
		plan.reject(targetIndex, base, wrapped)
		return wrapped
	}
	if err := planDirectories(plan, targetIndex, target, base, destination); err != nil {
		plan.reject(targetIndex, destination, err)
		return err
	}

	expected := []struct {
		name string
		data []byte
	}{
		{name: operatorskill.SkillFile, data: bundleContent},
		{name: ManifestFile, data: manifestContent},
	}
	existing := make(map[string][]byte, len(expected))
	existingModes := make(map[string]os.FileMode, len(expected))
	for _, file := range expected {
		path := filepath.Join(destination, file.name)
		data, mode, found, readErr := readRegularFile(path)
		if readErr != nil {
			err := fmt.Errorf("%w: %s: %w", ErrUnsafePath, path, readErr)
			plan.reject(targetIndex, path, err)
			return err
		}
		if found {
			existing[file.name] = data
			existingModes[file.name] = mode
		}
	}
	plan.result.Targets[targetIndex].Intent = classifyIntent(existing, bundleContent, manifestContent)

	var targetErrors []error
	for _, file := range expected {
		path := filepath.Join(destination, file.name)
		current, found := existing[file.name]
		if found && bytes.Equal(current, file.data) && safeFileMode(existingModes[file.name]) {
			plan.result.Actions = append(plan.result.Actions, Action{
				Target: target,
				Action: "noop",
				Path:   path,
				Status: "unchanged",
			})
			continue
		}
		if found && bytes.Equal(current, file.data) {
			actionIndex := len(plan.result.Actions)
			plan.result.Actions = append(plan.result.Actions, Action{
				Target: target,
				Action: "secure_permissions",
				Path:   path,
				Status: "planned",
			})
			plan.operations = append(plan.operations, plannedOperation{
				actionIndex:  actionIndex,
				targetIndex:  targetIndex,
				kind:         "file",
				path:         path,
				data:         file.data,
				existed:      true,
				previous:     slices.Clone(current),
				mode:         fileMode,
				previousMode: existingModes[file.name],
			})
			continue
		}
		if found && !force {
			err := fmt.Errorf("%w at %s", ErrConflict, path)
			plan.result.Actions = append(plan.result.Actions, Action{
				Target: target,
				Action: "conflict",
				Path:   path,
				Status: "failed",
				Error:  err.Error(),
			})
			plan.result.Targets[targetIndex].Status = "failed"
			plan.result.Targets[targetIndex].Error = err.Error()
			targetErrors = append(targetErrors, err)
			continue
		}
		if found {
			backupPath := backupPath(path, current)
			backup, backupMode, backupFound, backupErr := readRegularFile(backupPath)
			if backupErr != nil {
				err := fmt.Errorf("%w: backup %s: %w", ErrUnsafePath, backupPath, backupErr)
				plan.reject(targetIndex, backupPath, err)
				targetErrors = append(targetErrors, err)
				continue
			}
			if backupFound && !bytes.Equal(backup, current) {
				err := fmt.Errorf("%w: backup path %s does not match its content digest", ErrConflict, backupPath)
				plan.reject(targetIndex, backupPath, err)
				targetErrors = append(targetErrors, err)
				continue
			}
			if backupFound && !safeFileMode(backupMode) {
				err := fmt.Errorf("%w: backup path %s has mode %o, want %o", ErrUnsafePath, backupPath, backupMode.Perm(), fileMode)
				plan.reject(targetIndex, backupPath, err)
				targetErrors = append(targetErrors, err)
				continue
			}
			status := "planned"
			if backupFound {
				status = "unchanged"
			}
			actionIndex := len(plan.result.Actions)
			plan.result.Actions = append(plan.result.Actions, Action{
				Target:     target,
				Action:     "backup",
				Path:       path,
				BackupPath: backupPath,
				Status:     status,
			})
			if !backupFound {
				plan.operations = append(plan.operations, plannedOperation{
					actionIndex: actionIndex,
					targetIndex: targetIndex,
					kind:        "file",
					path:        backupPath,
					data:        current,
					mode:        fileMode,
				})
			}
		}
		actionIndex := len(plan.result.Actions)
		plan.result.Actions = append(plan.result.Actions, Action{
			Target: target,
			Action: "write",
			Path:   path,
			Status: "planned",
		})
		plan.operations = append(plan.operations, plannedOperation{
			actionIndex:  actionIndex,
			targetIndex:  targetIndex,
			kind:         "file",
			path:         path,
			data:         file.data,
			existed:      found,
			previous:     slices.Clone(current),
			mode:         fileMode,
			previousMode: existingModes[file.name],
		})
	}
	return errors.Join(targetErrors...)
}

func planDirectories(plan *installPlan, targetIndex int, target Target, base string, destination string) error {
	relative, err := filepath.Rel(base, destination)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == ".." {
		return fmt.Errorf("%w: destination %s is not beneath %s", ErrUnsafePath, destination, base)
	}
	current := base
	for _, segment := range strings.Split(relative, string(filepath.Separator)) {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("%w: invalid path segment %q", ErrUnsafePath, segment)
		}
		current = filepath.Join(current, segment)
		info, statErr := os.Lstat(current)
		switch {
		case statErr == nil && info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("%w: symlink path component %s", ErrUnsafePath, current)
		case statErr == nil && !info.IsDir():
			return fmt.Errorf("%w: non-directory path component %s", ErrUnsafePath, current)
		case statErr == nil:
			plan.result.Actions = append(plan.result.Actions, Action{Target: target, Action: "ensure_directory", Path: current, Status: "unchanged"})
		case errors.Is(statErr, os.ErrNotExist):
			actionIndex := len(plan.result.Actions)
			plan.result.Actions = append(plan.result.Actions, Action{Target: target, Action: "ensure_directory", Path: current, Status: "planned"})
			plan.operations = append(plan.operations, plannedOperation{
				actionIndex: actionIndex,
				targetIndex: targetIndex,
				kind:        "directory",
				path:        current,
				mode:        directoryMode,
			})
		default:
			return fmt.Errorf("inspect path component %s: %w", current, statErr)
		}
	}
	return nil
}

func applyPlan(plan installPlan, operations fileOperations) (Result, error) {
	if operations.mkdir == nil || operations.remove == nil || operations.atomicWrite == nil {
		return plan.result, errors.New("skill installer file operations are incomplete")
	}
	applied := make([]plannedOperation, 0, len(plan.operations))
	for _, operation := range plan.operations {
		var err error
		switch operation.kind {
		case "directory":
			err = operations.mkdir(operation.path, operation.mode)
		case "file":
			err = operations.atomicWrite(operation.path, operation.data, normalizedMode(operation.mode))
		default:
			err = fmt.Errorf("unknown install operation %q", operation.kind)
		}
		if err != nil {
			plan.result.Status = "failed"
			plan.result.Actions[operation.actionIndex].Status = "failed"
			plan.result.Actions[operation.actionIndex].Error = err.Error()
			plan.result.Targets[operation.targetIndex].Status = "failed"
			plan.result.Targets[operation.targetIndex].Error = err.Error()
			rollbackErr := rollback(&plan.result, applied, operations)
			if rollbackErr != nil {
				plan.result.Status = "rollback_failed"
			}
			for index := range plan.result.Actions {
				if plan.result.Actions[index].Status == "planned" {
					plan.result.Actions[index].Status = "not_applied"
				}
			}
			for index := range plan.result.Targets {
				if plan.result.Targets[index].Status == "planned" {
					plan.result.Targets[index].Status = "not_applied"
				}
			}
			return plan.result, errors.Join(fmt.Errorf("apply skill install operation %s: %w", operation.path, err), rollbackErr)
		}
		plan.result.Actions[operation.actionIndex].Status = "completed"
		applied = append(applied, operation)
	}

	plan.result.Status = "complete"
	for index := range plan.result.Targets {
		status := "unchanged"
		for _, action := range plan.result.Actions {
			if action.Target == plan.result.Targets[index].Target && action.Status == "completed" {
				status = "installed"
				break
			}
		}
		plan.result.Targets[index].Status = status
	}
	return plan.result, nil
}

func rollback(result *Result, applied []plannedOperation, operations fileOperations) error {
	var rollbackErrors []error
	for index := len(applied) - 1; index >= 0; index-- {
		operation := applied[index]
		action := Action{
			Target: result.Targets[operation.targetIndex].Target,
			Path:   operation.path,
			Status: "completed",
		}
		var err error
		switch {
		case operation.kind == "directory":
			action.Action = "remove_directory"
			err = operations.remove(operation.path)
		case operation.existed:
			action.Action = "restore"
			err = operations.atomicWrite(operation.path, operation.previous, normalizedMode(operation.previousMode))
		default:
			action.Action = "remove"
			err = operations.remove(operation.path)
		}
		if err != nil && (operation.kind != "directory" || !errors.Is(err, os.ErrNotExist)) {
			action.Status = "failed"
			action.Error = err.Error()
			rollbackErrors = append(rollbackErrors, fmt.Errorf("rollback %s: %w", operation.path, err))
		}
		result.Rollback = append(result.Rollback, action)
		if result.Targets[operation.targetIndex].Status != "failed" {
			result.Targets[operation.targetIndex].Status = "rolled_back"
		}
	}
	return errors.Join(rollbackErrors...)
}

func (plan *installPlan) reject(targetIndex int, path string, err error) {
	target := plan.result.Targets[targetIndex].Target
	plan.result.Actions = append(plan.result.Actions, Action{
		Target: target,
		Action: "reject",
		Path:   path,
		Status: "failed",
		Error:  err.Error(),
	})
	plan.result.Targets[targetIndex].Status = "failed"
	plan.result.Targets[targetIndex].Error = err.Error()
}

func classifyIntent(existing map[string][]byte, bundleContent []byte, manifestContent []byte) string {
	if len(existing) == 0 {
		return "install"
	}
	if bytes.Equal(existing[operatorskill.SkillFile], bundleContent) && bytes.Equal(existing[ManifestFile], manifestContent) {
		return "reinstall"
	}
	var manifest Manifest
	if raw, ok := existing[ManifestFile]; ok && json.Unmarshal(raw, &manifest) == nil && manifest.Skill == operatorskill.Name {
		switch {
		case manifest.BundleVersion < operatorskill.Version:
			return "upgrade"
		case manifest.BundleVersion > operatorskill.Version:
			return "downgrade"
		}
	}
	return "repair"
}

func encodeManifest(manifest Manifest) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func requireDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("is a symlink")
	}
	if !info.IsDir() {
		return errors.New("is not a directory")
	}
	return nil
}

func readRegularFile(path string) ([]byte, os.FileMode, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, 0, false, errors.New("is a symlink")
	}
	if !info.Mode().IsRegular() {
		return nil, 0, false, errors.New("is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, false, err
	}
	data, readErr := io.ReadAll(file)
	openedInfo, statErr := file.Stat()
	closeErr := file.Close()
	if readErr != nil || statErr != nil || closeErr != nil {
		return nil, 0, false, errors.Join(readErr, statErr, closeErr)
	}
	if !os.SameFile(info, openedInfo) {
		return nil, 0, false, errors.New("changed while being inspected")
	}
	return data, info.Mode().Perm(), true, nil
}

func backupPath(path string, content []byte) string {
	return path + ".detent-backup-" + digest(content)[:16]
}

func digest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func within(base string, path string) bool {
	relative, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func normalizedMode(mode os.FileMode) os.FileMode {
	if mode.Perm() == 0 {
		return fileMode
	}
	return mode.Perm()
}

func atomicWrite(path string, content []byte, mode os.FileMode) (returnErr error) {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".detent-skill-install-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		if temporaryPath == "" {
			return
		}
		if cleanupErr := os.Remove(temporaryPath); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove temporary file: %w", cleanupErr))
		}
	}()
	if err := temporary.Chmod(normalizedMode(mode)); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if _, err := temporary.Write(content); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Sync(); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := replaceFile(temporaryPath, path); err != nil {
		return err
	}
	temporaryPath = ""
	return nil
}
