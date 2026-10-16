package runner

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type ProviderUsage struct {
	mu               sync.Mutex
	windows          map[string][]providercapacity.UsageWindow
	billing          map[string]string
	capacityMu       sync.Mutex
	capacity         map[capacityReadKey]*capacityRead
	capacityAccounts map[capacityReadKey]capacityReadKey
}

type capacityReadKey struct {
	backendKind string
	provider    string
	account     string
}

type capacityRead struct {
	done        chan struct{}
	at          time.Time
	observation CapacityObservation
	err         error
}

func (u *ProviderUsage) RefreshCapacity(ctx context.Context, cfg config.AgentBackend, backend AgentBackend, now time.Time) (CapacityObservation, error) {
	reader, ok := backend.(AgentCapacityReader)
	if cfg.Disabled {
		return CapacityObservation{}, nil
	}
	if !ok {
		return CapacityObservation{}, ErrCapacityReadUnsupported
	}
	provider := configuredCapacityScope(RouteSelection{BackendID: cfg.ID}, cfg, agentidentity.Identity{}).Provider
	if cfg.Kind == config.AgentBackendCodex && provider != "" && provider != "openai" {
		return CapacityObservation{}, ErrCapacityReadUnsupported
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	account := strings.TrimSpace(reader.CapacityAccount())
	if u == nil || account == "" {
		observation, err := reader.ReadCapacity(ctx)
		if err == nil {
			u.Observe(cfg.ID, observation.RateLimits)
		}
		return observation, err
	}
	if provider == "" && cfg.Kind == config.AgentBackendCodex {
		provider = "openai"
	}
	owner := capacityReadKey{backendKind: cfg.Kind, provider: provider, account: "profile:" + account}
	key := owner
	u.capacityMu.Lock()
	if err := ctx.Err(); err != nil {
		u.capacityMu.Unlock()
		return CapacityObservation{}, err
	}
	if resolved, ok := u.capacityAccounts[owner]; ok {
		key = resolved
	}
	read := u.capacity[key]
	if read != nil {
		select {
		case <-read.done:
			if !now.Before(read.at.Add(time.Minute)) {
				read = nil
			}
		default:
		}
	}
	if read != nil {
		u.capacityMu.Unlock()
		select {
		case <-ctx.Done():
			return CapacityObservation{}, ctx.Err()
		case <-read.done:
		}
	} else {
		read = &capacityRead{done: make(chan struct{}), at: now}
		if u.capacity == nil {
			u.capacity = make(map[capacityReadKey]*capacityRead)
		}
		u.capacity[key] = read
		u.capacityMu.Unlock()
		observation, err := reader.ReadCapacity(ctx)
		u.capacityMu.Lock()
		read.observation, read.err = observation, err
		if read.err == nil && read.observation.AccountID != "" {
			delete(u.capacity, key)
			key.account = "account:" + read.observation.AccountID
			if u.capacityAccounts == nil {
				u.capacityAccounts = make(map[capacityReadKey]capacityReadKey)
			}
			u.capacityAccounts[owner] = key
		}
		u.capacity[key] = read
		close(read.done)
		u.capacityMu.Unlock()
	}
	if read.err == nil {
		u.Observe(cfg.ID, read.observation.RateLimits)
	}
	return read.observation, read.err
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

func (u *ProviderUsage) ObserveUpdate(backend string, update AgentUpdate) {
	u.Observe(backend, update.RateLimits)
	u.ObserveBilling(backend, update.Billing)
}

func (u *ProviderUsage) ObserveBilling(backend, billing string) {
	if u == nil || backend == "" || billing == "" || !providercapacity.ValidBilling(billing) {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.billing == nil {
		u.billing = make(map[string]string)
	}
	if _, exists := u.billing[backend]; !exists && len(u.billing) >= 32 {
		return
	}
	u.billing[backend] = billing
}

func (u *ProviderUsage) Billing(backend string) string {
	if u == nil {
		return "unknown"
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return (providercapacity.Report{Billing: u.billing[backend]}).BillingBasis()
}
