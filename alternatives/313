package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"

	servicepkg "github.com/digitaldrywood/detent/internal/service"
)

const (
	workerCodexProfileDir     = ".detent-worker"
	launchdCodexProfileDir    = ".detent-launchd"
	launchdCodexProfileMarker = ".detent-profile-v1"
	codexConfigFilename       = "config.toml"
)

var workerCodexLinkedEntries = []string{"auth.json"}

var workerCodexConfigKeys = map[string]bool{
	"model":                              true,
	"model_provider":                     true,
	"model_providers":                    true,
	"model_reasoning_effort":             true,
	"model_reasoning_summary":            true,
	"model_verbosity":                    true,
	"model_context_window":               true,
	"model_auto_compact_token_limit":     true,
	"model_supports_reasoning_summaries": true,
	"service_tier":                       true,
	"preferred_auth_method":              true,
	"forced_login_method":                true,
	"forced_chatgpt_workspace_id":        true,
	"chatgpt_base_url":                   true,
	"openai_base_url":                    true,
	"cli_auth_credentials_store":         true,
}

var launchdCodexProfileMu sync.Mutex

type codexServiceCommand struct {
	Command     string
	Environment map[string]string
}

func prepareCodexCommandForRuntime(command string) (codexServiceCommand, error) {
	return prepareCodexCommandForService(
		command,
		runtime.GOOS,
		string(servicepkg.ManagerFromProcessEnvironment(runtime.GOOS, os.LookupEnv)),
		os.LookupEnv,
		os.UserHomeDir,
	)
}

func prepareCodexCommandForService(
	command string,
	goos string,
	manager string,
	lookupEnv func(string) (string, bool),
	userHomeDir func() (string, error),
) (codexServiceCommand, error) {
	prepared := codexServiceCommand{Command: command}
	isLaunchd := goos == "darwin" && strings.TrimSpace(manager) == string(servicepkg.ManagerLaunchd)

	credentialPath, err := codexCredentialPath(command, lookupEnv, userHomeDir)
	if err != nil {
		return codexServiceCommand{}, err
	}
	home, err := userHomeDir()
	if err != nil {
		return codexServiceCommand{}, fmt.Errorf("resolve launchd user home: %w", err)
	}
	sourceHome := filepath.Dir(credentialPath)
	profileHome, err := prepareWorkerCodexHome(sourceHome, home, isLaunchd)
	if err != nil {
		return codexServiceCommand{}, err
	}
	prepared.Command = replaceCodexHomeAssignment(command, profileHome)
	prepared.Environment = map[string]string{"CODEX_HOME": profileHome}
	return prepared, nil
}

func prepareWorkerCodexHome(sourceHome string, userHome string, isLaunchd bool) (string, error) {
	launchdCodexProfileMu.Lock()
	defer launchdCodexProfileMu.Unlock()

	sourceHome = filepath.Clean(sourceHome)
	userHome = filepath.Clean(userHome)
	if err := os.MkdirAll(sourceHome, 0o700); err != nil {
		return "", fmt.Errorf("create Codex home %s: %w", sourceHome, err)
	}
	resolvedSource, err := filepath.EvalSymlinks(sourceHome)
	if err != nil {
		return "", fmt.Errorf("resolve Codex home %s: %w", sourceHome, err)
	}
	if isLaunchd && launchdProtectedPath(resolvedSource, userHome) {
		return "", fmt.Errorf("codex home %s is in a macOS privacy-protected location", resolvedSource)
	}

	profileDir := workerCodexProfileDir
	if isLaunchd {
		profileDir = launchdCodexProfileDir
	}
	profileHome := filepath.Join(sourceHome, profileDir)
	if err := ensureLaunchdCodexProfile(profileHome); err != nil {
		return "", err
	}
	for _, name := range []string{"sessions", "archived_sessions"} {
		path := filepath.Join(profileHome, name)
		if _, err := removeManagedProfileLink(path); err != nil {
			return "", err
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return "", fmt.Errorf("create worker transcript directory: %w", err)
		}
	}
	profileEntries, err := os.ReadDir(profileHome)
	if err != nil {
		return "", fmt.Errorf("read worker Codex profile: %w", err)
	}
	for _, entry := range profileEntries {
		path := filepath.Join(profileHome, entry.Name())
		switch {
		case codexSQLiteFilename(entry.Name()):
			if err := removeSharedSQLiteProfileEntry(filepath.Join(sourceHome, entry.Name()), path); err != nil {
				return "", err
			}
		case codexInstructionFilename(entry.Name()):
			local, err := removeManagedProfileLink(path)
			if err != nil {
				return "", err
			}
			if local {
				return "", fmt.Errorf("worker Codex profile contains user instructions: %s", path)
			}
		case entry.Name() == "skills" && entry.IsDir():
			if err := removeInheritedSkillLinks(path); err != nil {
				return "", err
			}
		case entry.Name() == codexConfigFilename || strings.HasPrefix(entry.Name(), "."+codexConfigFilename+"."):
		case !workerCodexLinkedEntry(entry.Name()):
			if err := removeInheritedProfileEntry(filepath.Join(sourceHome, entry.Name()), path); err != nil {
				return "", err
			}
		}
	}
	for _, name := range workerCodexLinkedEntries {
		source := filepath.Join(sourceHome, name)
		if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return "", fmt.Errorf("inspect Codex home entry %s: %w", source, err)
		}
		if err := ensureProfileLink(source, filepath.Join(profileHome, name)); err != nil {
			return "", err
		}
	}
	disabledSkills, err := userScopeSkillFiles(userHome)
	if err != nil {
		return "", err
	}
	if err := writeWorkerCodexConfig(filepath.Join(sourceHome, codexConfigFilename), filepath.Join(profileHome, codexConfigFilename), disabledSkills); err != nil {
		return "", err
	}

	if isLaunchd {
		if err := rejectProtectedAgentSkills(userHome); err != nil {
			return "", err
		}
	}
	return profileHome, nil
}

func workerCodexLinkedEntry(name string) bool {
	for _, linked := range workerCodexLinkedEntries {
		if name == linked {
			return true
		}
	}
	return false
}

func removeInheritedProfileEntry(source string, destination string) error {
	info, err := os.Lstat(destination)
	if err != nil {
		return fmt.Errorf("inspect worker Codex profile entry %s: %w", destination, err)
	}
	inherited := info.Mode()&os.ModeSymlink != 0
	if !inherited {
		sourceInfo, sourceErr := os.Stat(source)
		if sourceErr != nil && !errors.Is(sourceErr, os.ErrNotExist) {
			return fmt.Errorf("inspect Codex home entry %s: %w", source, sourceErr)
		}
		destinationInfo, destinationErr := os.Stat(destination)
		inherited = sourceErr == nil && destinationErr == nil && os.SameFile(sourceInfo, destinationInfo)
	}
	if !inherited {
		return nil
	}
	if err := os.Remove(destination); err != nil {
		return fmt.Errorf("remove inherited worker Codex profile entry %s: %w", destination, err)
	}
	return nil
}

func removeInheritedSkillLinks(skillsRoot string) error {
	entries, err := os.ReadDir(skillsRoot)
	if err != nil {
		return fmt.Errorf("read worker Codex skills %s: %w", skillsRoot, err)
	}
	for _, entry := range entries {
		if _, err := removeManagedProfileLink(filepath.Join(skillsRoot, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func userScopeSkillFiles(userHome string) ([]string, error) {
	var files []string
	visited := map[string]bool{}
	var walk func(string) error
	walk = func(directory string) error {
		resolved, err := filepath.EvalSymlinks(directory)
		if err != nil {
			return fmt.Errorf("resolve user skill directory %s: %w", directory, err)
		}
		if visited[resolved] {
			return nil
		}
		visited[resolved] = true
		entries, err := os.ReadDir(directory)
		if err != nil {
			return fmt.Errorf("read user skill directory %s: %w", directory, err)
		}
		for _, entry := range entries {
			path := filepath.Join(directory, entry.Name())
			info, err := os.Stat(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("inspect user skill %s: %w", path, err)
			}
			switch {
			case info.IsDir():
				if err := walk(path); err != nil {
					return err
				}
			case entry.Name() == "SKILL.md":
				files = append(files, path)
			}
		}
		return nil
	}
	root := filepath.Join(userHome, ".agents", "skills")
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return files, nil
}

func writeWorkerCodexConfig(source string, destination string, disabledSkills []string) error {
	raw, err := os.ReadFile(source)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read Codex config %s: %w", source, err)
	}
	var settings map[string]any
	if err := toml.Unmarshal(raw, &settings); err != nil {
		return fmt.Errorf("parse Codex config %s: %w", source, err)
	}
	kept := make(map[string]any, len(settings)+1)
	for key, value := range settings {
		if workerCodexConfigKeys[key] {
			kept[key] = value
		}
	}
	if len(disabledSkills) > 0 {
		entries := make([]map[string]any, 0, len(disabledSkills))
		for _, path := range disabledSkills {
			entries = append(entries, map[string]any{"path": path, "enabled": false})
		}
		kept["skills"] = map[string]any{"config": entries}
	}
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(kept); err != nil {
		return fmt.Errorf("encode worker Codex config: %w", err)
	}
	return replaceFileAtomically(destination, encoded.Bytes())
}

func replaceFileAtomically(destination string, content []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".*")
	if err != nil {
		return fmt.Errorf("create worker Codex config: %w", err)
	}
	name := temporary.Name()
	_, writeErr := temporary.Write(content)
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return errors.Join(fmt.Errorf("write worker Codex config %s: %w", name, err), os.Remove(name))
	}
	if err := os.Rename(name, destination); err != nil {
		return errors.Join(fmt.Errorf("publish worker Codex config %s: %w", destination, err), os.Remove(name))
	}
	return nil
}

func ensureLaunchdCodexProfile(profileHome string) error {
	info, err := os.Stat(profileHome)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(profileHome, 0o700); err != nil {
			return fmt.Errorf("create launchd Codex profile %s: %w", profileHome, err)
		}
		if err := os.WriteFile(filepath.Join(profileHome, launchdCodexProfileMarker), []byte("1\n"), 0o600); err != nil {
			return fmt.Errorf("mark launchd Codex profile %s: %w", profileHome, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("inspect launchd Codex profile %s: %w", profileHome, err)
	case !info.IsDir():
		return fmt.Errorf("launchd Codex profile path %s is not a directory", profileHome)
	}
	if _, err := os.Stat(filepath.Join(profileHome, launchdCodexProfileMarker)); err != nil {
		return fmt.Errorf("launchd Codex profile %s is not managed by Detent", profileHome)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("launchd Codex profile %s permissions are broader than 0700", profileHome)
	}
	return nil
}

func rejectProtectedAgentSkills(userHome string) error {
	skillsRoot := filepath.Join(userHome, ".agents", "skills")
	entries, err := os.ReadDir(skillsRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read agent skills %s: %w", skillsRoot, err)
	}
	var protected []string
	for _, entry := range entries {
		path := filepath.Join(skillsRoot, entry.Name())
		unsafe, err := launchdProtectedSkillLink(path, userHome)
		if err != nil {
			return fmt.Errorf("inspect agent skill %s: %w", path, err)
		}
		if unsafe {
			protected = append(protected, entry.Name())
		}
	}
	if len(protected) == 0 {
		return nil
	}
	return fmt.Errorf("launchd cannot access protected ~/.agents/skills links: %s", strings.Join(protected, ", "))
}

func codexInstructionFilename(name string) bool {
	return strings.EqualFold(name, "AGENTS.md") || strings.EqualFold(name, "AGENTS.override.md")
}

func codexSQLiteFilename(name string) bool {
	return strings.Contains(strings.ToLower(name), ".sqlite")
}

func removeSharedSQLiteProfileEntry(source string, destination string) error {
	destinationInfo, err := os.Lstat(destination)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect worker Codex SQLite state %s: %w", destination, err)
	}
	shared := destinationInfo.Mode()&os.ModeSymlink != 0
	if !shared {
		sourceInfo, sourceErr := os.Stat(source)
		if errors.Is(sourceErr, os.ErrNotExist) {
			return nil
		}
		if sourceErr != nil {
			return fmt.Errorf("inspect host Codex SQLite state %s: %w", source, sourceErr)
		}
		shared = os.SameFile(sourceInfo, destinationInfo)
	}
	if !shared {
		return nil
	}
	if err := os.Remove(destination); err != nil {
		return fmt.Errorf("remove shared worker Codex SQLite state %s: %w", destination, err)
	}
	return nil
}

func ensureProfileLink(source string, destination string) error {
	if runtime.GOOS == "windows" {
		return ensureWindowsCodexProfileLink(source, destination)
	}

	info, err := os.Lstat(destination)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink(source, destination); err != nil {
			return fmt.Errorf("link launchd Codex profile entry %s: %w", destination, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect launchd Codex profile entry %s: %w", destination, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	target, err := os.Readlink(destination)
	if err != nil {
		return fmt.Errorf("read launchd Codex profile link %s: %w", destination, err)
	}
	if target == source {
		return nil
	}
	if err := os.Remove(destination); err != nil {
		return fmt.Errorf("replace launchd Codex profile link %s: %w", destination, err)
	}
	if err := os.Symlink(source, destination); err != nil {
		return fmt.Errorf("link launchd Codex profile entry %s: %w", destination, err)
	}
	return nil
}

func removeManagedProfileLink(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect launchd Codex skill profile %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return true, nil
	}
	if err := os.Remove(path); err != nil {
		return false, fmt.Errorf("remove protected launchd Codex skill link %s: %w", path, err)
	}
	return false, nil
}

func launchdProtectedSkillLink(path string, userHome string) (bool, error) {
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return launchdProtectedPath(resolved, userHome), nil
}

func launchdProtectedPath(path string, userHome string) bool {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if resolved, err := filepath.EvalSymlinks(userHome); err == nil {
		userHome = resolved
	}
	protectedRoots := []string{
		filepath.Join(userHome, "Desktop"),
		filepath.Join(userHome, "Documents"),
		filepath.Join(userHome, "Downloads"),
		filepath.Join(userHome, "Library", "CloudStorage"),
		filepath.Join(userHome, "Library", "Mobile Documents"),
	}
	for _, root := range protectedRoots {
		relative, err := filepath.Rel(root, path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func replaceCodexHomeAssignment(command string, profileHome string) string {
	match := codexHomeAssignmentPattern.FindStringIndex(command)
	if len(match) == 0 {
		return command
	}
	matched := command[match[0]:match[1]]
	assignmentOffset := strings.Index(matched, "CODEX_HOME=")
	if assignmentOffset < 0 {
		return command
	}
	assignmentStart := match[0] + assignmentOffset
	quotedProfile := "'" + strings.ReplaceAll(profileHome, "'", "'\\''") + "'"
	return command[:assignmentStart] + "CODEX_HOME=" + quotedProfile + command[match[1]:]
}

func ensureWindowsCodexProfileLink(source string, destination string) error {
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("inspect Codex profile source %s: %w", source, err)
	}
	destinationInfo, err := os.Stat(destination)
	if err == nil {
		if os.SameFile(sourceInfo, destinationInfo) {
			return nil
		}
		if sourceInfo.IsDir() {
			return fmt.Errorf("codex profile directory %s does not reference %s", destination, source)
		}
		if err := os.Remove(destination); err != nil {
			return fmt.Errorf("refresh Codex profile link %s: %w", destination, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Codex profile entry %s: %w", destination, err)
	}
	if !sourceInfo.IsDir() {
		if err := os.Link(source, destination); err != nil {
			return fmt.Errorf("hard-link Codex profile entry %s: %w", destination, err)
		}
		return nil
	}
	return createCodexProfileJunction(source, destination)
}
