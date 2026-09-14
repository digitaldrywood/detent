// Package toolcache manages the host Go caches shared by workers and operators.
package toolcache

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Paths are resolved by Go, including operator environment and GOENV settings.
type Paths struct {
	Build   string `json:"GOCACHE"`
	Modules string `json:"GOMODCACHE"`
}

func Resolve(ctx context.Context) (Paths, error) {
	output, err := exec.CommandContext(ctx, "go", "env", "-json", "GOCACHE", "GOMODCACHE").Output()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve host Go caches: %w", err)
	}
	var paths Paths
	if err := json.Unmarshal(output, &paths); err != nil {
		return Paths{}, fmt.Errorf("decode Go caches: %w", err)
	}
	return paths, nil
}

// Size counts regular files without following symlinks.
func Size(root string) (int64, error) { return size(context.Background(), root) }

func size(ctx context.Context, root string) (int64, error) {
	if root == "" || root == "off" {
		return 0, nil
	}
	var size int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

// RemoveLegacy removes only the former Detent-owned cache root.
func RemoveLegacy(workspaceRoot string) (int64, error) {
	if strings.TrimSpace(workspaceRoot) == "" {
		return 0, nil
	}
	root := filepath.Join(workspaceRoot, ".detent", "cache")
	size, err := Size(root)
	if err != nil {
		return 0, err
	}
	if err := os.RemoveAll(root); err != nil {
		return 0, err
	}
	return size, nil
}

// Report describes the locally resolved caches for doctor and status.
type Report struct {
	BuildPath   string `json:"build_path"`
	BuildBytes  int64  `json:"build_bytes"`
	ModulePath  string `json:"module_path"`
	ModuleBytes int64  `json:"module_bytes"`
	Error       string `json:"error,omitempty"`
}

func Inspect(ctx context.Context) Report {
	paths, err := Resolve(ctx)
	if err != nil {
		return Report{Error: err.Error()}
	}
	report := Report{BuildPath: paths.Build, ModulePath: paths.Modules}
	report.BuildBytes, err = size(ctx, paths.Build)
	if err != nil {
		report.Error = err.Error()
		return report
	}
	report.ModuleBytes, err = size(ctx, paths.Modules)
	if err != nil {
		report.Error = err.Error()
	}
	return report
}

func (r Report) String() string {
	if r.Error != "" {
		return fmt.Sprintf("GOCACHE=%s GOMODCACHE=%s: %s", r.BuildPath, r.ModulePath, r.Error)
	}
	return fmt.Sprintf("GOCACHE=%s (%d bytes); GOMODCACHE=%s (%d bytes)", r.BuildPath, r.BuildBytes, r.ModulePath, r.ModuleBytes)
}
