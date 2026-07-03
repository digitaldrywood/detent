package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var onboardingMakeTargetPattern = regexp.MustCompile(`^([A-Za-z0-9_.-]+)\s*:(?:\s|$)`)

type onboardingRepoProbe struct {
	SourceRoot              string   `json:"source_root"`
	Languages               []string `json:"languages"`
	Toolchains              []string `json:"toolchains"`
	Manifests               []string `json:"manifests"`
	CIWorkflows             []string `json:"ci_workflows"`
	MakeTargets             []string `json:"make_targets"`
	ValidationCommand       string   `json:"validation_command,omitempty"`
	ValidationCommandSource string   `json:"validation_command_source,omitempty"`
	FileCount               int      `json:"file_count"`
	DirectoryCount          int      `json:"directory_count"`
	TopLevelDirectories     []string `json:"top_level_directories"`
	Monorepo                bool     `json:"monorepo"`
	MonorepoEvidence        []string `json:"monorepo_evidence,omitempty"`
	PackageManager          string   `json:"package_manager,omitempty"`
	NodeTestScript          bool     `json:"node_test_script"`
	ReadOnly                bool     `json:"read_only"`
}

type onboardingPackageJSON struct {
	Scripts    map[string]string `json:"scripts"`
	Workspaces any               `json:"workspaces"`
	Engines    map[string]string `json:"engines"`
}

func probeOnboardingRepository(sourceRoot string) (onboardingRepoProbe, error) {
	root, err := resolveOnboardingGateSourceRoot(sourceRoot)
	if err != nil {
		return onboardingRepoProbe{}, err
	}

	probe := onboardingRepoProbe{
		SourceRoot: root,
		ReadOnly:   true,
	}
	rootEntries, err := os.ReadDir(root)
	if err != nil {
		return onboardingRepoProbe{}, fmt.Errorf("read source root %s: %w", root, err)
	}
	for _, entry := range rootEntries {
		name := entry.Name()
		if entry.IsDir() {
			probe.TopLevelDirectories = append(probe.TopLevelDirectories, name)
		}
	}
	sort.Strings(probe.TopLevelDirectories)

	manifestDirs := map[string]struct{}{}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != root && onboardingProbeSkipDir(name) {
				return filepath.SkipDir
			}
			if path != root {
				probe.DirectoryCount++
			}
			return nil
		}
		probe.FileCount++
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "." {
			dir = ""
		}
		probeOnboardingManifest(path, rel, dir, &probe, manifestDirs)
		probeOnboardingCIWorkflow(rel, &probe)
		return nil
	}); err != nil {
		return onboardingRepoProbe{}, fmt.Errorf("probe repository %s: %w", root, err)
	}

	probe.MakeTargets = probeOnboardingMakeTargets(root)
	probe.ValidationCommand, probe.ValidationCommandSource = onboardingProbeValidationCommand(probe)
	probe.Monorepo, probe.MonorepoEvidence = onboardingProbeMonorepo(probe, manifestDirs)
	sort.Strings(probe.Languages)
	sort.Strings(probe.Toolchains)
	sort.Strings(probe.Manifests)
	sort.Strings(probe.CIWorkflows)
	sort.Strings(probe.MakeTargets)
	sort.Strings(probe.MonorepoEvidence)
	return probe, nil
}

func onboardingProbeSkipDir(name string) bool {
	switch name {
	case ".git", ".detent", ".hg", ".svn", "node_modules", "vendor", "dist", "build", "tmp", ".cache":
		return true
	default:
		return false
	}
}

func probeOnboardingManifest(path string, rel string, dir string, probe *onboardingRepoProbe, manifestDirs map[string]struct{}) {
	name := filepath.Base(rel)
	switch name {
	case "go.mod":
		appendOnboardingProbeUnique(&probe.Languages, "Go")
		appendOnboardingProbeUnique(&probe.Manifests, rel)
		manifestDirs[dir] = struct{}{}
		probeOnboardingGoMod(path, probe)
	case "go.work":
		appendOnboardingProbeUnique(&probe.Languages, "Go")
		appendOnboardingProbeUnique(&probe.Manifests, rel)
		manifestDirs[dir] = struct{}{}
		appendOnboardingProbeUnique(&probe.MonorepoEvidence, "go.work present")
	case "package.json":
		appendOnboardingProbeUnique(&probe.Languages, "Node")
		appendOnboardingProbeUnique(&probe.Manifests, rel)
		manifestDirs[dir] = struct{}{}
		probeOnboardingPackageJSON(path, rel, dir, probe)
	case "package-lock.json":
		appendOnboardingProbeUnique(&probe.Manifests, rel)
		if dir == "" && probe.PackageManager == "" {
			probe.PackageManager = "npm"
		}
	case "pnpm-lock.yaml", "pnpm-workspace.yaml":
		appendOnboardingProbeUnique(&probe.Manifests, rel)
		if dir == "" {
			probe.PackageManager = "pnpm"
		}
		if name == "pnpm-workspace.yaml" {
			appendOnboardingProbeUnique(&probe.MonorepoEvidence, "pnpm-workspace.yaml present")
		}
	case "yarn.lock":
		appendOnboardingProbeUnique(&probe.Manifests, rel)
		if dir == "" && probe.PackageManager == "" {
			probe.PackageManager = "yarn"
		}
	case "Cargo.toml":
		appendOnboardingProbeUnique(&probe.Languages, "Rust")
		appendOnboardingProbeUnique(&probe.Manifests, rel)
		manifestDirs[dir] = struct{}{}
	case "pyproject.toml":
		appendOnboardingProbeUnique(&probe.Languages, "Python")
		appendOnboardingProbeUnique(&probe.Manifests, rel)
		manifestDirs[dir] = struct{}{}
	case "pom.xml":
		appendOnboardingProbeUnique(&probe.Languages, "Java")
		appendOnboardingProbeUnique(&probe.Manifests, rel)
		manifestDirs[dir] = struct{}{}
	}
}

func probeOnboardingCIWorkflow(rel string, probe *onboardingRepoProbe) {
	if strings.HasPrefix(rel, ".github/workflows/") && (strings.HasSuffix(rel, ".yml") || strings.HasSuffix(rel, ".yaml")) {
		appendOnboardingProbeUnique(&probe.CIWorkflows, rel)
		return
	}
	switch rel {
	case ".gitlab-ci.yml", ".circleci/config.yml", "Jenkinsfile":
		appendOnboardingProbeUnique(&probe.CIWorkflows, rel)
	}
}

func probeOnboardingGoMod(path string, probe *onboardingRepoProbe) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "go":
			appendOnboardingProbeUnique(&probe.Toolchains, "go "+fields[1])
		case "toolchain":
			appendOnboardingProbeUnique(&probe.Toolchains, fields[1])
		}
	}
}

func probeOnboardingPackageJSON(path string, rel string, dir string, probe *onboardingRepoProbe) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var pkg onboardingPackageJSON
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return
	}
	if _, ok := pkg.Scripts["test"]; ok && dir == "" {
		probe.NodeTestScript = true
	}
	if engine := strings.TrimSpace(pkg.Engines["node"]); engine != "" {
		appendOnboardingProbeUnique(&probe.Toolchains, "node "+engine)
	}
	if pkg.Workspaces != nil {
		appendOnboardingProbeUnique(&probe.MonorepoEvidence, rel+" workspaces present")
	}
}

func probeOnboardingMakeTargets(root string) []string {
	for _, name := range []string{"Makefile", "makefile", "GNUmakefile"} {
		path := filepath.Join(root, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		return parseOnboardingMakeTargets(raw)
	}
	return nil
}

func parseOnboardingMakeTargets(raw []byte) []string {
	seen := map[string]struct{}{}
	var targets []string
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "\t") || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		matches := onboardingMakeTargetPattern.FindStringSubmatch(line)
		if len(matches) != 2 {
			continue
		}
		target := strings.TrimSpace(matches[1])
		if target == ".PHONY" || strings.Contains(target, "%") {
			continue
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		targets = append(targets, target)
	}
	sort.Strings(targets)
	return targets
}

func onboardingProbeValidationCommand(probe onboardingRepoProbe) (string, string) {
	for _, target := range []string{"check", "verify", "ci", "test", "lint"} {
		if onboardingStringSliceContains(probe.MakeTargets, target) {
			return "make " + target, "probe:Makefile target " + target
		}
	}
	if probe.NodeTestScript {
		switch probe.PackageManager {
		case "pnpm":
			return "pnpm test", "probe:root package.json test script"
		case "yarn":
			return "yarn test", "probe:root package.json test script"
		default:
			return "npm test", "probe:root package.json test script"
		}
	}
	return "", ""
}

func onboardingProbeMonorepo(probe onboardingRepoProbe, manifestDirs map[string]struct{}) (bool, []string) {
	evidence := append([]string(nil), probe.MonorepoEvidence...)
	topLevelManifestDirs := map[string]struct{}{}
	for dir := range manifestDirs {
		if dir == "" {
			continue
		}
		top, _, _ := strings.Cut(dir, "/")
		if strings.TrimSpace(top) != "" {
			topLevelManifestDirs[top] = struct{}{}
		}
	}
	if len(topLevelManifestDirs) > 1 {
		names := make([]string, 0, len(topLevelManifestDirs))
		for name := range topLevelManifestDirs {
			names = append(names, name)
		}
		sort.Strings(names)
		evidence = append(evidence, "multiple top-level manifest directories: "+strings.Join(names, ","))
	}
	sort.Strings(evidence)
	return len(evidence) > 0, evidence
}

func appendOnboardingProbeUnique(values *[]string, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	for _, existing := range *values {
		if existing == value {
			return
		}
	}
	*values = append(*values, value)
}

func onboardingStringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
