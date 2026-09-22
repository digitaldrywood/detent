package cli

import (
	"context"
	"os"
	"path/filepath"

	"github.com/digitaldrywood/detent/internal/toolcache"
)

// checkDoctorNativeCaches reports the host cache once without modifying it.
func checkDoctorNativeCaches(ctx context.Context, deps doctorDeps, policy toolcache.Policy) doctorCheck {
	check := doctorCheck{Name: "Host native toolchain caches", Status: doctorOK}
	inspect := deps.inspectCaches
	if inspect == nil {
		inspect = toolcache.Inspect
	}
	report := inspect(ctx)
	check.Detail = report.String()

	if report.BuildPath != "" && report.BuildPath != "off" {
		capacityBytes := deps.cacheCapacityBytes
		if capacityBytes == nil {
			capacityBytes = toolcache.CapacityBytes
		}
		capacity, err := capacityBytes(report.BuildPath)
		if err != nil || capacity == 0 {
			capacity = 0
			check.Status = doctorWarn
			check.Detail += "; cache volume capacity unavailable"
			if err != nil {
				check.Detail += ": " + err.Error()
			}
			if policy.MaxBytes == 0 {
				check.Detail += "; using fallback build cache bound of 20 GiB"
			}
		} else {
			check.Detail += "; cache volume capacity: " + toolcache.FormatBytes(int64(capacity))
			if uint64(policy.NormalizedForCapacity(capacity).MaxBytes) > capacity/10 {
				check.Status = doctorWarn
				check.Detail += "; effective build cache bound exceeds 10% of cache-volume capacity"
				check.Hint = "Reduce global.cache.max_bytes to at most 10% of the cache-volume capacity."
			}
		}
		policy = policy.NormalizedForCapacity(capacity)
	}
	policy = policy.NormalizedForCapacity(0)
	check.Detail += "; build cache bound: " + toolcache.FormatBytes(policy.MaxBytes)
	if report.SizeEvictedBytes > 0 {
		check.Status = doctorWarn
		check.Detail += "; last sweep size eviction: " + toolcache.FormatBytes(report.SizeEvictedBytes)
		check.Hint = "Review global.cache.max_bytes: size eviction indicates the bound may be below the working set; last sweep retained " + toolcache.FormatBytes(report.RetainedBytes) + "."
	}
	lastTrim := report.LastTrim
	if lastTrim == "" {
		lastTrim = "not recorded"
	}
	check.Detail += "; last reaper trim: " + lastTrim
	if report.Error != "" {
		check.Status = doctorWarn
	}
	return check
}

func checkDoctorLegacyCaches(projectID, root string) doctorCheck {
	check := doctorCheck{Name: "Project " + projectID + " legacy toolchain caches", Status: doctorOK, Detail: "legacy cache inspection"}
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
	if check.Status == doctorOK {
		check.Detail = "no Detent-owned cache roots"
	}
	return check
}
