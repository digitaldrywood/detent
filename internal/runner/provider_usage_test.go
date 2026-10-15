package runner

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestProviderUsageCoalescesCapacityReads(t *testing.T) {
	t.Parallel()
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
			cfg.Provider = "local_ollama"
			if _, err := usage.RefreshCapacity(t.Context(), cfg, backend, now); err != nil {
				t.Fatal(err)
			}
			if calls != 3 {
				t.Fatal("local backend read ChatGPT quota")
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
