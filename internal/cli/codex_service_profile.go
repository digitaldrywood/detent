package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	servicepkg "github.com/digitaldrywood/detent/internal/service"
)

const (
	launchdCodexProfileDir    = ".detent-launchd"
	launchdCodexProfileMarker = ".detent-profile-v1"
)

var launchdCodexProfileMu sync.Mutex

type codexServiceCommand struct {
	Command       string
	Environment   map[string]string
	OmittedSkills []string
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
	if goos != "darwin" || strings.TrimSpace(manager) != string(servicepkg.ManagerLaunchd) {
		return prepared, nil
	}

	credentialPath, err := codexCredentialPath(command, lookupEnv, userHomeDir)
	if err != nil {
		return codexServiceCommand{}, err
	}
	home, err := userHomeDir()
	if err != nil {
		return codexServiceCommand{}, fmt.Errorf("resolve launchd user home: %w", err)
	}
	sourceHome := filepath.Dir(credentialPath)
	profileHome, omittedSkills, err := prepareLaunchdCodexHome(sourceHome, home)
	if err != nil {
		return codexServiceCommand{}, err
	}
	prepared.Command = replaceCodexHomeAssignment(command, profileHome)
	prepared.Environment = map[string]string{"CODEX_HOME": profileHome}
	prepared.OmittedSkills = omittedSkills
	return prepared, nil
}

func prepareLaunchdCodexHome(sourceHome string, userHome string) (string, []string, error) {
	launchdCodexProfileMu.Lock()
	defer launchdCodexProfileMu.Unlock()

	sourceHome = filepath.Clean(sourceHome)
	userHome = filepath.Clean(userHome)
	resolvedSource, err := filepath.EvalSymlinks(sourceHome)
	if err != nil {
		return "", nil, fmt.Errorf("resolve Codex home %s: %w", sourceHome, err)
	}
	if launchdProtectedPath(resolvedSource, userHome) {
		return "", nil, fmt.Errorf("codex home %s is in a macOS privacy-protected location", resolvedSource)
	}

	profileHome := filepath.Join(sourceHome, launchdCodexProfileDir)
	if err := ensureLaunchdCodexProfile(profileHome); err != nil {
		return "", nil, err
	}
	entries, err := os.ReadDir(sourceHome)
	if err != nil {
		return "", nil, fmt.Errorf("read Codex home %s: %w", sourceHome, err)
	}
	for _, entry := range entries {
		if entry.Name() == "skills" || entry.Name() == launchdCodexProfileDir {
			continue
		}
		if err := ensureProfileLink(
			filepath.Join(sourceHome, entry.Name()),
			filepath.Join(profileHome, entry.Name()),
		); err != nil {
			return "", nil, err
		}
	}

	omittedSkills, err := syncLaunchdCodexSkills(sourceHome, profileHome, userHome)
	if err != nil {
		return "", nil, err
	}
	if err := rejectProtectedAgentSkills(userHome); err != nil {
		return "", nil, err
	}
	return profileHome, omittedSkills, nil
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

func syncLaunchdCodexSkills(sourceHome string, profileHome string, userHome string) ([]string, error) {
	sourceSkills := filepath.Join(sourceHome, "skills")
	profileSkills := filepath.Join(profileHome, "skills")
	if err := os.MkdirAll(profileSkills, 0o700); err != nil {
		return nil, fmt.Errorf("create launchd Codex skills profile %s: %w", profileSkills, err)
	}
	entries, err := os.ReadDir(sourceSkills)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Codex skills %s: %w", sourceSkills, err)
	}

	omitted := make([]string, 0)
	for _, entry := range entries {
		source := filepath.Join(sourceSkills, entry.Name())
		destination := filepath.Join(profileSkills, entry.Name())
		protected, err := launchdProtectedSkillLink(source, userHome)
		if err != nil {
			return nil, fmt.Errorf("inspect Codex skill %s: %w", source, err)
		}
		if protected {
			localFallback, err := removeManagedProfileLink(destination)
			if err != nil {
				return nil, err
			}
			if !localFallback {
				omitted = append(omitted, entry.Name())
			}
			continue
		}
		if err := ensureProfileLink(source, destination); err != nil {
			return nil, err
		}
	}
	return omitted, nil
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

func ensureProfileLink(source string, destination string) error {
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
