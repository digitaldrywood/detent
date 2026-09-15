package cli

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/digitaldrywood/detent/internal/toolcache"
)

// checkDoctorNativeCaches is read-only, including when legacy caches remain.
func checkDoctorNativeCaches(ctx context.Context, projectID, root string, deps doctorDeps) doctorCheck {
	check := doctorCheck{Name: "Project " + projectID + " native toolchain caches", Status: doctorOK}
	inspect := deps.inspectCaches
	if inspect == nil {
		inspect = toolcache.Inspect
	}
	report := inspect(ctx)
	check.Detail = report.String()
	lastTrim := report.LastTrim
	if lastTrim == "" {
		lastTrim = "not recorded"
	}
	check.Detail += "; last reaper trim: " + lastTrim
	if report.Error != "" {
		check.Status = doctorWarn
	}
	resolved, err := expandDoctorWorkspacePath(root)
	if err != nil {
		check.Status = doctorWarn
		check.Detail += "; " + err.Error()
		return check
	}
	roots := []string{resolved}
	entries, err := os.ReadDir(resolved)
	if err != nil && !os.IsNotExist(err) {
		check.Status = doctorWarn
		check.Detail += "; inspect workdirs: " + err.Error()
	}
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != ".detent" {
			roots = append(roots, filepath.Join(resolved, entry.Name()))
		}
	}
	for _, workdir := range roots {
		legacyRoots := []string{filepath.Join(workdir, ".detent", "cache")}
		// Former isolated caches were components directly inside attempt scratch.
		attempts, readErr := os.ReadDir(filepath.Join(workdir, ".detent", "worker-tmp"))
		if readErr != nil && !os.IsNotExist(readErr) {
			check.Status = doctorWarn
			check.Detail += "; inspect attempt scratch: " + readErr.Error()
		}
		for _, attempt := range attempts {
			if !attempt.IsDir() {
				continue
			}
			for _, component := range []string{"go-build", "go-mod", "go-bin", "golangci-lint"} {
				legacyRoots = append(legacyRoots, filepath.Join(workdir, ".detent", "worker-tmp", attempt.Name(), component))
			}
		}
		for _, legacy := range legacyRoots {
			if _, err := os.Lstat(legacy); err == nil {
				check.Status = doctorWarn
				check.Detail += "; Detent-owned cache root remains: " + legacy
				check.Hint = "INV-12: workers use native toolchain caches; legacy roots remain until workspace/scratch cleanup."
			} else if !os.IsNotExist(err) {
				check.Status = doctorWarn
				check.Detail += "; inspect legacy cache: " + err.Error()
			}
		}
	}
	return check
}

// Each doctor invocation measures host caches once, even with concurrent projects.
func cacheDoctorInspection(inspect func(context.Context) toolcache.Report) func(context.Context) toolcache.Report {
	if inspect == nil {
		inspect = toolcache.Inspect
	}
	var once sync.Once
	var report toolcache.Report
	return func(ctx context.Context) toolcache.Report {
		once.Do(func() { report = inspect(ctx) })
		return report
	}
}
