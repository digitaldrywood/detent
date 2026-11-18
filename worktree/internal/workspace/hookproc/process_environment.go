package hookproc

import (
	"errors"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/digitaldrywood/detent/internal/workspace/internal/workpath"
)

// workerScratchOwnerRoots lists the directories whose scratch marks a process
// as owned by root: root itself and its external worker scratch roots.
func workerScratchOwnerRoots(root string) []string {
	return append([]string{root}, workpath.WorkerScratchRoots(root)...)
}

func workerScratchEnvironmentMatches(roots []string, environment []string) bool {
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
			return workerScratchPathMatchesAny(roots, value)
		case "TMPDIR", "TMP", "TEMP":
			legacy = legacy || workerScratchPathMatchesAny(roots, value)
		}
	}
	return legacy
}

func workerScratchPathMatches(root string, path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	path = filepath.Clean(path)
	if workpath.PathWithin(root, path) {
		return true
	}
	canonical, ok := canonicalNearestExistingPath(path)
	return ok && workpath.PathWithin(root, canonical)
}

func canonicalNearestExistingPath(path string) (string, bool) {
	remainder := ""
	for {
		canonical, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(canonical, remainder), true
		}
		parent := filepath.Dir(path)
		if !errors.Is(err, fs.ErrNotExist) || parent == path {
			return "", false
		}
		remainder = filepath.Join(filepath.Base(path), remainder)
		path = parent
	}
}

func workerScratchPathMatchesAny(roots []string, path string) bool {
	for _, root := range roots {
		if workerScratchPathMatches(root, path) {
			return true
		}
	}
	return false
}
