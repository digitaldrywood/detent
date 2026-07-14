package procgroup

import (
	"os/exec"
	"runtime"
	"strings"
)

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

func environmentKey(key string, goos string) string {
	if goos == "windows" {
		return strings.ToUpper(key)
	}
	return key
}
