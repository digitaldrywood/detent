package runner

import (
	"slices"
	"sync"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type ProviderUsage struct {
	mu      sync.Mutex
	windows map[string][]providercapacity.UsageWindow
}

func (u *ProviderUsage) Observe(backend string, limits *telemetry.RateLimits) {
	if u == nil || limits == nil || backend == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.windows == nil {
		u.windows = make(map[string][]providercapacity.UsageWindow)
	}
	if _, exists := u.windows[backend]; !exists && len(u.windows) >= 32 {
		return
	}
	for _, bucket := range []*telemetry.RateLimitBucket{limits.Primary, limits.Secondary} {
		if bucket == nil || !providercapacity.ValidUsageKind(bucket.WindowKind) || bucket.ObservedAt == nil || bucket.ObservedAt.IsZero() {
			continue
		}
		window := providercapacity.UsageWindow{Kind: bucket.WindowKind, ObservedAt: bucket.ObservedAt.UTC()}
		if bucket.PercentUsed != nil {
			percent := *bucket.PercentUsed
			window.PercentUsed = &percent
		}
		if bucket.ResetAt != nil && bucket.ResetAt.After(window.ObservedAt) {
			window.ResetAt = bucket.ResetAt.UTC()
		}
		windows := u.windows[backend]
		found := false
		for index, prior := range windows {
			if prior.Kind != window.Kind {
				continue
			}
			found = true
			if !prior.ObservedAt.After(window.ObservedAt) {
				windows[index] = window
			}
			break
		}
		if !found && len(windows) < providercapacity.MaxUsageWindows {
			windows = append(windows, window)
		}
		u.windows[backend] = windows
	}
}

func (u *ProviderUsage) Windows(backend string) []providercapacity.UsageWindow {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	windows := slices.Clone(u.windows[backend])
	for i, window := range windows {
		if window.PercentUsed != nil {
			percent := *window.PercentUsed
			windows[i].PercentUsed = &percent
		}
	}
	return windows
}
