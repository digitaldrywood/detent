// Package toolcache manages the host Go caches shared by workers and operators.
package toolcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Paths are resolved by Go, including operator environment and GOENV settings.
type Paths struct {
	Build   string `json:"GOCACHE"`
	Modules string `json:"GOMODCACHE"`
}

// Resolve discovers the daemon's host caches once, independently of any worker
// module or cancellation. Discovery failure never prevents a worker from starting.
func Resolve(_ context.Context) (Paths, error) { return hostPaths() }

var hostPaths = sync.OnceValues(func() (Paths, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	paths, err := resolve(ctx)
	if err != nil {
		slog.Warn("host Go cache discovery unavailable; continuing without cache roots", "error", err)
	}
	return paths, err
})

func resolve(ctx context.Context) (Paths, error) {
	paths := Paths{Build: os.Getenv("GOCACHE"), Modules: os.Getenv("GOMODCACHE")}
	if paths.Build != "" && paths.Modules != "" {
		return paths, nil
	}
	cmd := exec.CommandContext(ctx, "go", "env", "-json", "GOCACHE", "GOMODCACHE")
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	if output, err := cmd.Output(); err == nil {
		var discovered Paths
		if json.Unmarshal(output, &discovered) == nil {
			if paths.Build == "" {
				paths.Build = discovered.Build
			}
			if paths.Modules == "" {
				paths.Modules = discovered.Modules
			}
		}
	}
	if paths.Build == "" {
		if root, err := os.UserCacheDir(); err == nil {
			paths.Build = filepath.Join(root, "go-build")
		}
	}
	if paths.Modules == "" {
		root := os.Getenv("GOPATH")
		if root == "" {
			if home, err := os.UserHomeDir(); err == nil {
				root = filepath.Join(home, "go")
			}
		}
		if roots := filepath.SplitList(root); len(roots) > 0 && roots[0] != "" {
			paths.Modules = filepath.Join(roots[0], "pkg", "mod")
		}
	}
	if paths.Build == "" && paths.Modules == "" {
		return paths, errors.New("no host Go cache paths available")
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
	ModulePath  string `json:"module_path,omitempty"`
	ModuleBytes int64  `json:"module_bytes,omitempty"`
	Error       string `json:"error,omitempty"`
	LastTrim    string `json:"last_trim,omitempty"`
}

func Inspect(ctx context.Context) Report { return inspect(ctx, Resolve) }

func inspect(ctx context.Context, resolvePaths func(context.Context) (Paths, error)) Report {
	paths, err := resolvePaths(ctx)
	if err != nil {
		return Report{Error: err.Error()}
	}
	report := Report{BuildPath: paths.Build, ModulePath: paths.Modules}
	if paths.Build != "" && paths.Build != "off" {
		data, readErr := os.ReadFile(filepath.Join(paths.Build, "detent-trim.txt"))
		if readErr == nil {
			at, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
			if parseErr != nil {
				report.Error = "read last reaper trim: " + parseErr.Error()
			} else {
				report.LastTrim = at.UTC().Format(time.RFC3339Nano)
			}
		} else if !os.IsNotExist(readErr) {
			report.Error = "read last reaper trim: " + readErr.Error()
		}
	}
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
	detail := fmt.Sprintf("GOCACHE=%s (%d bytes); GOMODCACHE=%s (%d bytes)", r.BuildPath, r.BuildBytes, r.ModulePath, r.ModuleBytes)
	if r.Error != "" {
		detail += "; " + r.Error
	}
	return detail
}
