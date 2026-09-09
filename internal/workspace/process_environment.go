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
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		switch key {
		case "DETENT_WORKER_SCRATCH":
			return workerScratchPathMatches(root, value)
		case "TMPDIR", "TMP", "TEMP":
			legacy = legacy || workerScratchPathMatches(root, value)
		}
	}
	return legacy
}

func workerScratchPathMatches(root string, path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	if pathWithin(root, path) {
		return true
	}
	canonical, err := filepath.EvalSymlinks(path)
	return err == nil && pathWithin(root, canonical)
}
