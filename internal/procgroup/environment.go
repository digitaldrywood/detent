package procgroup

import (
	"os/exec"
	"runtime"
	"sort"
	"strings"
)

type Environment struct {
	Variables    map[string]string
	PathPrefixes []string
	PathSuffixes []string
}

func SetEnvironment(cmd *exec.Cmd, environment Environment) {
	if cmd == nil || (len(environment.Variables) == 0 && len(environment.PathPrefixes) == 0 && len(environment.PathSuffixes) == 0) {
		return
	}
	cmd.Env = environmentWithOverrides(cmd.Environ(), environment, runtime.GOOS)
}

func SetTempDir(cmd *exec.Cmd, path string) {
	path = strings.TrimSpace(path)
	if cmd == nil || path == "" {
		return
	}
	cmd.Env = environmentWithTempDir(cmd.Environ(), path, runtime.GOOS)
}

func environmentWithTempDir(environment []string, path string, goos string) []string {
	tempKeys := map[string]struct{}{
		environmentKey("TMPDIR", goos): {},
		environmentKey("TMP", goos):    {},
		environmentKey("TEMP", goos):   {},
	}
	out := make([]string, 0, len(environment)+3)
	for _, entry := range environment {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, remove := tempKeys[environmentKey(key, goos)]; remove {
				continue
			}
		}
		out = append(out, entry)
	}
	return append(out, "TMPDIR="+path, "TMP="+path, "TEMP="+path)
}

func environmentWithOverrides(current []string, configured Environment, goos string) []string {
	overrides := make(map[string]struct{}, len(configured.Variables))
	for key := range configured.Variables {
		overrides[environmentKey(key, goos)] = struct{}{}
	}
	pathKey := environmentKey("PATH", goos)
	pathValue := ""
	out := make([]string, 0, len(current)+len(configured.Variables)+1)
	for _, entry := range current {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			out = append(out, entry)
			continue
		}
		normalized := environmentKey(key, goos)
		if normalized == pathKey && (len(configured.PathPrefixes) > 0 || len(configured.PathSuffixes) > 0) {
			pathValue = value
			continue
		}
		if _, remove := overrides[normalized]; remove {
			continue
		}
		out = append(out, entry)
	}

	keys := make([]string, 0, len(configured.Variables))
	for key := range configured.Variables {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = append(out, key+"="+configured.Variables[key])
	}

	pathParts := make([]string, 0, len(configured.PathPrefixes)+len(configured.PathSuffixes)+1)
	for _, prefix := range configured.PathPrefixes {
		if prefix = strings.TrimSpace(prefix); prefix != "" {
			pathParts = append(pathParts, prefix)
		}
	}
	if pathValue != "" {
		pathParts = append(pathParts, pathValue)
	}
	for _, suffix := range configured.PathSuffixes {
		if suffix = strings.TrimSpace(suffix); suffix != "" {
			pathParts = append(pathParts, suffix)
		}
	}
	if len(pathParts) > 0 {
		separator := ":"
		if goos == "windows" {
			separator = ";"
		}
		out = append(out, "PATH="+strings.Join(pathParts, separator))
	}
	return out
}

func environmentKey(key string, goos string) string {
	if goos == "windows" {
		return strings.ToUpper(key)
	}
	return key
}
