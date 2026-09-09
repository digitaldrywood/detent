package workspace

import (
	"path/filepath"
	"runtime"
	"strings"
)

func workerScratchEnvironmentMatches(root string, environment []string) bool {
	legacy := false
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		matches := filepath.IsAbs(value) && pathWithin(root, value)
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		switch key {
		case "DETENT_WORKER_SCRATCH":
			return matches
		case "TMPDIR", "TMP", "TEMP":
			legacy = legacy || matches
		}
	}
	return legacy
}
