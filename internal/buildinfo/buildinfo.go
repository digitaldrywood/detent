package buildinfo

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

const (
	defaultVersion   = "dev"
	defaultCommit    = "none"
	defaultDate      = "unknown"
	shortCommitWidth = 7
	gitTimeout       = 2 * time.Second
)

type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Dirty   bool   `json:"dirty,omitempty"`
}

type Reader func() (*debug.BuildInfo, bool)

type GitReader func(string) (Info, bool)

func Resolve(version string, commit string, date string) Info {
	return ResolveWithReaders(version, commit, date, debug.ReadBuildInfo, readGitInfo)
}

func ResolveWithReader(version string, commit string, date string, reader Reader) Info {
	return ResolveWithReaders(version, commit, date, reader, nil)
}

func ResolveWithReaders(version string, commit string, date string, reader Reader, gitReader GitReader) Info {
	info := Normalize(Info{
		Version: version,
		Commit:  commit,
		Date:    date,
	})
	build, ok := readBuildInfo(reader)
	if !ok {
		return info
	}

	if placeholderVersion(info.Version) {
		if moduleVersion := cleanModuleVersion(build.Main.Version); moduleVersion != "" {
			info.Version = moduleVersion
		}
	}

	settings := buildSettings(build.Settings)
	if placeholderCommit(info.Commit) {
		if revision := strings.TrimSpace(settings["vcs.revision"]); revision != "" {
			info.Commit = revision
		}
	}
	if placeholderDate(info.Date) {
		if buildTime := strings.TrimSpace(settings["vcs.time"]); buildTime != "" {
			info.Date = buildTime
		}
	}
	info.Dirty = info.Dirty || strings.EqualFold(strings.TrimSpace(settings["vcs.modified"]), "true")
	if gitReader != nil && shouldReadGit(info) {
		if gitInfo, ok := gitReader(build.Main.Path); ok {
			if placeholderCommit(info.Commit) && !placeholderCommit(gitInfo.Commit) {
				info.Commit = gitInfo.Commit
			}
			if placeholderDate(info.Date) && !placeholderDate(gitInfo.Date) {
				info.Date = gitInfo.Date
			}
			info.Dirty = info.Dirty || gitInfo.Dirty
		}
	}

	return Normalize(info)
}

func Normalize(info Info) Info {
	info.Version = strings.TrimSpace(info.Version)
	info.Commit = strings.TrimSpace(info.Commit)
	info.Date = strings.TrimSpace(info.Date)
	if info.Version == "" || info.Version == "(devel)" {
		info.Version = defaultVersion
	}
	if info.Commit == "" {
		info.Commit = defaultCommit
	}
	if info.Date == "" {
		info.Date = defaultDate
	}
	return info
}

func IsZero(info Info) bool {
	return strings.TrimSpace(info.Version) == "" &&
		strings.TrimSpace(info.Commit) == "" &&
		strings.TrimSpace(info.Date) == "" &&
		!info.Dirty
}

func DisplayLabel(info Info) string {
	info = Normalize(info)
	commit := ShortCommit(info.Commit)
	if info.Dirty {
		commit += ", dirty"
	}
	return fmt.Sprintf("%s (%s) %s", info.Version, commit, info.Date)
}

func ShortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if placeholderCommit(commit) {
		return defaultCommit
	}
	if len(commit) <= shortCommitWidth {
		return commit
	}
	return commit[:shortCommitWidth]
}

func readBuildInfo(reader Reader) (*debug.BuildInfo, bool) {
	if reader == nil {
		return nil, false
	}
	build, ok := reader()
	return build, ok && build != nil
}

func buildSettings(settings []debug.BuildSetting) map[string]string {
	byKey := make(map[string]string, len(settings))
	for _, setting := range settings {
		byKey[setting.Key] = setting.Value
	}
	return byKey
}

func shouldReadGit(info Info) bool {
	return placeholderCommit(info.Commit) || placeholderDate(info.Date)
}

func readGitInfo(modulePath string) (Info, bool) {
	modulePath = strings.TrimSpace(modulePath)
	if modulePath == "" {
		return Info{}, false
	}
	root, err := runGit("", "rev-parse", "--show-toplevel")
	if err != nil || !modulePathMatches(root, modulePath) {
		return Info{}, false
	}
	commit, err := runGit(root, "rev-parse", "--short", "HEAD")
	if err != nil {
		return Info{}, false
	}
	date, err := runGit(root, "log", "-1", "--format=%cI")
	if err != nil {
		date = ""
	}
	status, err := runGit(root, "status", "--porcelain")
	dirty := err == nil && strings.TrimSpace(status) != ""
	return Info{
		Commit: commit,
		Date:   normalizeGitDate(date),
		Dirty:  dirty,
	}, true
}

func runGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	if strings.TrimSpace(dir) != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func modulePathMatches(root string, modulePath string) bool {
	data, err := os.ReadFile(filepath.Join(strings.TrimSpace(root), "go.mod"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			return strings.Trim(fields[1], `"`) == modulePath
		}
	}
	return false
}

func normalizeGitDate(date string) string {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(date))
	if err != nil {
		return strings.TrimSpace(date)
	}
	return parsed.UTC().Format(time.RFC3339)
}

func cleanModuleVersion(version string) string {
	version = strings.TrimSpace(version)
	if placeholderVersion(version) {
		return ""
	}
	return version
}

func placeholderVersion(version string) bool {
	switch strings.TrimSpace(version) {
	case "", defaultVersion, "(devel)":
		return true
	default:
		return false
	}
}

func placeholderCommit(commit string) bool {
	switch strings.TrimSpace(commit) {
	case "", defaultCommit, defaultDate, "(devel)":
		return true
	default:
		return false
	}
}

func placeholderDate(date string) bool {
	switch strings.TrimSpace(date) {
	case "", defaultDate, defaultCommit, "(devel)":
		return true
	default:
		return false
	}
}
