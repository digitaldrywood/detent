package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestProviderUsageCoalescesCapacityReads(t *testing.T) {
	t.Parallel()
	for _, resolved := range []bool{false, true} {
		t.Run(map[bool]string{false: "concurrent profile", true: "concurrent resolved account"}[resolved], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				usage := &ProviderUsage{}
				cfg := config.CodexAgentBackend(config.Codex{Command: "codex app-server"})
				now := time.Now()
				backend := capacityReadBackend{account: "profile-one", read: func(context.Context) (CapacityObservation, error) {
					return CapacityObservation{AccountID: "account-one", Status: CapacityStatus{Available: true}}, nil
				}}
				alias := backend
				if resolved {
					alias.account = "profile-alias"
					for _, owner := range []capacityReadBackend{backend, alias} {
						if _, err := usage.RefreshCapacity(t.Context(), cfg, owner, now); err != nil {
							t.Fatal(err)
						}
					}
					now = now.Add(time.Minute)
				}
				started := make(chan struct{})
				release := make(chan struct{})
				var calls atomic.Int32
				backend.read = func(ctx context.Context) (CapacityObservation, error) {
					if calls.Add(1) == 1 {
						close(started)
					}
					select {
					case <-release:
						return CapacityObservation{AccountID: "account-one", Status: CapacityStatus{Available: true}}, nil
					case <-ctx.Done():
						return CapacityObservation{}, ctx.Err()
					}
				}
				alias.read = backend.read
				type result struct {
					observation CapacityObservation
					err         error
				}
				first := make(chan result, 1)
				go func() {
					observation, err := usage.RefreshCapacity(t.Context(), cfg, backend, now)
					first <- result{observation, err}
				}()
				<-started
				second := make(chan result, 1)
				go func() {
					observation, err := usage.RefreshCapacity(t.Context(), cfg, alias, now)
					second <- result{observation, err}
				}()
				synctest.Wait()
				other := capacityReadBackend{account: "profile-two", read: func(context.Context) (CapacityObservation, error) {
					return CapacityObservation{AccountID: "account-two", Status: CapacityStatus{Exhausted: true}}, nil
				}}
				if got, err := usage.RefreshCapacity(t.Context(), cfg, other, now); err != nil || !got.Status.Exhausted || got.AccountID != "account-two" {
					t.Fatalf("unrelated account blocked or reused evidence: %#v, %v", got, err)
				}
				canceled, cancel := context.WithCancel(t.Context())
				waiting := make(chan error, 1)
				go func() { _, err := usage.RefreshCapacity(canceled, cfg, alias, now); waiting <- err }()
				synctest.Wait()
				cancel()
				if err := <-waiting; !errors.Is(err, context.Canceled) {
					t.Fatalf("waiting cancellation = %v", err)
				}
				close(release)
				for _, finished := range []result{<-first, <-second} {
					if finished.err != nil || !finished.observation.Status.Available || finished.observation.AccountID != "account-one" {
						t.Fatalf("shared read = %#v", finished)
					}
				}
				if got := calls.Load(); got != 1 {
					t.Fatalf("concurrent reads = %d, want 1", got)
				}
			})
		})
	}
	t.Run("unsupported backend", func(t *testing.T) {
		for _, kind := range []string{config.AgentBackendClaudeCode, config.AgentBackendPiAgent} {
			cfg := config.AgentBackend{ID: kind, Kind: kind}
			if _, err := (&ProviderUsage{}).RefreshCapacity(t.Context(), cfg, nil, time.Now()); !errors.Is(err, ErrCapacityReadUnsupported) {
				t.Fatalf("%s unsupported read = %v", kind, err)
			}
		}
	})
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "unknown after failure"}[failed], func(t *testing.T) {
			now := time.Date(2026, 10, 15, 20, 0, 0, 0, time.UTC)
			usage := &ProviderUsage{}
			calls := 0
			accountID := "account-one"
			backend := capacityReadBackend{account: "profile-one", read: func(ctx context.Context) (CapacityObservation, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("status read has no deadline")
				}
				calls++
				if failed {
					return CapacityObservation{}, errors.New("unavailable")
				}
				return CapacityObservation{AccountID: accountID, Status: CapacityStatus{Available: true}}, nil
			}}
			cfg := config.CodexAgentBackend(config.Codex{Command: "codex app-server"})
			for index, variant := range []config.Codex{
				{Command: "codex app-server", ReadTimeoutMS: 1000, ThreadSandbox: "read-only", ApprovalPolicy: config.StringOrMap{IsString: true, String: "never"}},
				{Command: "codex app-server", ReadTimeoutMS: 9000, ThreadSandbox: "workspace-write", ApprovalPolicy: config.StringOrMap{IsString: true, String: "on-request"}},
				{Command: "codex app-server", ReadTimeoutMS: 5000},
			} {
				cfg = config.CodexAgentBackend(variant)
				cfg.ID = "backend-" + strconv.Itoa(index)
				got, err := usage.RefreshCapacity(t.Context(), cfg, backend, now)
				if (err != nil) != failed || got.Status.Available == failed {
					t.Fatalf("read=%#v, %v", got, err)
				}
			}
			if calls != 1 {
				t.Fatalf("coalesced reads=%d", calls)
			}
			other := backend
			other.account = "profile-two"
			accountID = "account-two"
			if _, err := usage.RefreshCapacity(t.Context(), cfg, other, now); (err != nil) != failed {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatal("separate account reused capacity evidence")
			}
			accountID = "account-one"
			if _, err := usage.RefreshCapacity(t.Context(), cfg, backend, now.Add(time.Minute)); (err != nil) != failed {
				t.Fatal(err)
			}
			if calls != 3 {
				t.Fatal("capacity was not refreshed")
			}
			for _, provider := range []string{"local_ollama", "anthropic"} {
				cfg.Provider = provider
				if _, err := usage.RefreshCapacity(t.Context(), cfg, backend, now); !errors.Is(err, ErrCapacityReadUnsupported) {
					t.Fatalf("%s unsupported quota read = %v", provider, err)
				}
				if calls != 3 {
					t.Fatalf("%s backend read ChatGPT quota", provider)
				}
			}
			if !failed {
				cfg.Provider = "openai"
				alias := backend
				alias.account = "profile-alias"
				if got, err := usage.RefreshCapacity(t.Context(), cfg, alias, now.Add(time.Minute)); err != nil || got.AccountID != "account-one" {
					t.Fatalf("account discovery = %#v, %v", got, err)
				}
				if calls != 4 {
					t.Fatalf("account discovery reads = %d", calls)
				}
				for _, owner := range []capacityReadBackend{alias, backend} {
					if _, err := usage.RefreshCapacity(t.Context(), cfg, owner, now.Add(2*time.Minute)); err != nil {
						t.Fatal(err)
					}
				}
				if calls != 5 {
					t.Fatalf("resolved account was polled separately across profiles: %d", calls)
				}
				unresolved := backend
				unresolved.account = ""
				for _, id := range []string{"account-one", "account-two"} {
					accountID = id
					if got, err := usage.RefreshCapacity(t.Context(), cfg, unresolved, now.Add(2*time.Minute)); err != nil || got.AccountID != id {
						t.Fatalf("unresolved ownership reused another account: %#v, %v", got, err)
					}
				}
				if calls != 7 {
					t.Fatalf("unresolved owners shared capacity: %d", calls)
				}
			}
		})
	}
}

type capacityReadBackend struct {
	AgentBackend
	account string
	read    func(context.Context) (CapacityObservation, error)
}

func (b capacityReadBackend) CapacityAccount() string { return b.account }

func (b capacityReadBackend) ReadCapacity(ctx context.Context) (CapacityObservation, error) {
	return b.read(ctx)
}

func TestProviderUsageRetainsWindowObservations(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	usage := &ProviderUsage{}
	percent := 85.0
	for _, backend := range []string{"claude", "codex"} {
		usage.Observe(backend, &telemetry.RateLimits{Primary: &telemetry.RateLimitBucket{WindowKind: "five_hour", PercentUsed: &percent, ObservedAt: &now}})
	}
	old := now.Add(-time.Hour)
	usage.Observe("claude", &telemetry.RateLimits{Primary: &telemetry.RateLimitBucket{WindowKind: "five_hour", ObservedAt: &old}})
	usage.Observe("claude", &telemetry.RateLimits{Secondary: &telemetry.RateLimitBucket{WindowKind: "weekly", ObservedAt: &old}})
	percent = 10
	for _, backend := range []string{"claude", "codex"} {
		windows := usage.Windows(backend)
		if len(windows) == 0 || windows[0].PercentUsed == nil || *windows[0].PercentUsed != 85 || !windows[0].ObservedAt.Equal(now) {
			t.Fatalf("%s windows = %+v", backend, windows)
		}
		*windows[0].PercentUsed = 0
		cached := usage.Windows(backend)
		if len(cached) == 0 || cached[0].PercentUsed == nil || *cached[0].PercentUsed != 85 {
			t.Fatal("reader mutated cached usage")
		}
	}
	windows := usage.Windows("claude")
	if len(windows) != 2 || windows[1].PercentUsed != nil || !windows[1].ObservedAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("stale unknown window was lost or refreshed: %+v", windows)
	}
	if now.Sub(windows[1].ObservedAt) <= providercapacity.MaxAge || len(usage.Windows("unknown")) != 0 {
		t.Fatal("observation age or backend isolation changed")
	}
}

func TestWorkerUsageCallbackObservesProviderWindows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	percent := 72.0
	update := UsageUpdate{RuntimeIdentity: agentidentity.Identity{BackendID: "codex"}, RateLimits: &telemetry.RateLimits{Primary: &telemetry.RateLimitBucket{WindowKind: "weekly", PercentUsed: &percent, ObservedAt: &now}}}
	raw, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		forward bool
	}{
		{name: "forwarded to the run", forward: true},
		{name: "run does not consume usage"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := &Runner{providerUsage: &ProviderUsage{}}
			var forwarded int
			request := RunRequest{}
			if test.forward {
				request.OnUsageUpdate = func(UsageUpdate) error { forwarded++; return nil }
			}
			if _, err := r.SSHCallbackHandler(request)(t.Context(), "usage", []json.RawMessage{raw}); err != nil {
				t.Fatal(err)
			}
			windows := r.providerUsage.Windows("codex")
			if len(windows) != 1 || windows[0].Kind != "weekly" || windows[0].PercentUsed == nil || *windows[0].PercentUsed != 72 {
				t.Fatalf("windows = %+v", windows)
			}
			if test.forward && forwarded != 1 {
				t.Fatalf("forwarded %d updates", forwarded)
			}
		})
	}
}

func TestProviderBillingObservations(t *testing.T) {
	t.Parallel()
	usage := &ProviderUsage{}
	for _, test := range []struct{ backend, billing, want string }{
		{"claude", "subscription", "subscription"}, {"codex", "api_key", "api_key"},
		{"claude", "", "subscription"}, {"claude", "invalid", "subscription"}, {"codex", "unknown", "unknown"},
	} {
		usage.ObserveUpdate(test.backend, AgentUpdate{Billing: test.billing})
		if got := usage.Billing(test.backend); got != test.want {
			t.Fatalf("%s billing = %q, want %q", test.backend, got, test.want)
		}
	}
	if got := usage.Billing("claude"); got != "subscription" {
		t.Fatalf("other backend overwrote Claude billing: %q", got)
	}
}
