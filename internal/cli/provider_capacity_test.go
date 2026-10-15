package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestRunnerProviderReports(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, account, alias       string
		file, missing, fail, usage bool
		billing                    string
		billingFail                bool
	}{
		{name: "default and refresh", account: "macbook-pro-m1", alias: "macbook-pro-m1"},
		{name: "normalized instance", account: "Owner's MacBook", alias: "owner-s-macbook"},
		{name: "bounded instance alias", account: strings.Repeat("a", 80), alias: strings.Repeat("a", 64)},
		{name: "explicit file precedence", file: true},
		{name: "observed default", account: "runner", alias: "runner", usage: true},
		{name: "observed explicit file", file: true, usage: true},
		{name: "native subscription", account: "runner", alias: "runner", billing: "subscription"},
		{name: "native API key with explicit file", file: true, billing: "api_key"},
		{name: "native subscription with explicit file", file: true, billing: "subscription"},
		{name: "unavailable native auth", file: true, billingFail: true},
		{name: "explicit missing file does not fall back", file: true, missing: true},
		{name: "catalog failure remains instance owned", fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 9, 6, 0, 0, 0, time.UTC)
			cfg := globalconfig.Config{Global: globalconfig.Settings{MaxConcurrentAgents: 4}}
			explicit := providercapacity.Report{Billing: "unknown", Provider: "openai", Backend: "codex", AccountAlias: "example-runner", Models: []string{"configured-model"}, MaxConcurrent: 12, Availability: "available", ObservedAt: now}
			if test.file {
				cfg.Client.ProviderCapacityFile = filepath.Join(t.TempDir(), "capacity.json")
				if !test.missing {
					raw, err := json.Marshal([]providercapacity.Report{explicit})
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(cfg.Client.ProviderCapacityFile, raw, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			catalogFailure := errors.New("catalog unavailable")
			var usage *runner.ProviderUsage
			observed := now.Add(-time.Hour)
			percent := 85.0
			if test.usage {
				usage = &runner.ProviderUsage{}
				usage.ObserveBilling("codex", "subscription")
				explicit.Billing = "subscription"
				usage.Observe("codex", &telemetry.RateLimits{Primary: &telemetry.RateLimitBucket{WindowKind: "five_hour", PercentUsed: &percent, ObservedAt: &observed}})
				explicit.UsageWindows = []providercapacity.UsageWindow{{Kind: "five_hour", PercentUsed: &percent, ObservedAt: observed}}
			}
			var billing func(context.Context, workflowconfig.AgentBackend) (string, error)
			if test.billing != "" || test.billingFail {
				billing = func(_ context.Context, backend workflowconfig.AgentBackend) (string, error) {
					if backend.ID != "codex" {
						t.Fatalf("billing backend = %q", backend.ID)
					}
					if test.billingFail {
						return "subscription", errors.New("native auth unavailable")
					}
					return test.billing, nil
				}
				if !test.billingFail {
					explicit.Billing = test.billing
				}
			}
			source := runnerProviderReports(t.Context(), cfg, test.account, func() time.Time { return now }, func(_ context.Context, backend workflowconfig.AgentBackend) ([]runner.AgentModel, error) {
				if test.file {
					t.Fatal("explicit file invoked the default reporter")
				}
				if backend.ID != "codex" || backend.Command != "codex app-server" {
					t.Fatalf("backend=%+v", backend)
				}
				if test.fail {
					return nil, catalogFailure
				}
				return []runner.AgentModel{{ID: "sol", Model: "sol"}, {ID: "astra", Model: "astra"}}, nil
			}, billing, usage)
			for range 6 {
				reports, err := source()
				if test.missing || test.fail {
					if err == nil || test.fail && !errors.Is(err, catalogFailure) {
						t.Fatalf("source error=%v", err)
					}
					return
				}
				if err != nil || len(reports) != 1 {
					t.Fatalf("reports=%+v, err=%v", reports, err)
				}
				if test.billing != "" && reports[0].Billing != test.billing {
					t.Fatalf("billing = %q, want %q", reports[0].Billing, test.billing)
				}
				if test.billingFail && reports[0].Billing != "unknown" {
					t.Fatalf("failed auth billing = %q", reports[0].Billing)
				}
				if test.usage && (reports[0].Billing != "subscription" || len(reports[0].UsageWindows) != 1 || reports[0].UsageWindows[0].PercentUsed == nil || *reports[0].UsageWindows[0].PercentUsed != 85 || !reports[0].UsageWindows[0].ObservedAt.Equal(observed)) {
					t.Fatalf("usage changed during heartbeat: %+v", reports[0].UsageWindows)
				}
				if test.file {
					if !reflect.DeepEqual(reports[0], explicit) {
						t.Fatalf("explicit report replaced: %+v", reports)
					}
				} else {
					report := reports[0]
					if report.Provider != "openai" || report.Backend != "codex" || report.AccountAlias != test.alias || report.MaxConcurrent != 0 || report.Availability != "unknown" || !report.ObservedAt.Equal(now) || !reflect.DeepEqual(report.Models, []string{"sol", "astra", "provider_default"}) {
						t.Fatalf("default report=%+v", report)
					}
					raw, err := json.Marshal(report)
					if err != nil || strings.Contains(string(raw), "max_concurrent") {
						t.Fatalf("default account limit was serialized: %s, %v", raw, err)
					}
				}
				now = now.Add(30 * time.Second)
			}
		})
	}
}

func writeProviderCapacityFixture(t *testing.T, dir string) string {
	t.Helper()
	raw, err := json.Marshal([]providercapacity.Report{{Provider: "openai", Backend: "codex", AccountAlias: "runner", Models: []string{"provider_default"}, Availability: "unknown", ObservedAt: time.Now().UTC()}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "provider-capacity.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAPIKeyReportSuppressesQuota(t *testing.T) {
	t.Parallel()
	usage := &runner.ProviderUsage{}
	usage.ObserveBilling("codex", "api_key")
	reports := withProviderUsage([]providercapacity.Report{{Backend: "codex", UsageWindows: []providercapacity.UsageWindow{{Kind: "weekly"}}}}, []*runner.ProviderUsage{usage})
	if reports[0].Billing != "api_key" || len(reports[0].UsageWindows) != 0 {
		t.Fatalf("API report = %+v", reports[0])
	}
}
