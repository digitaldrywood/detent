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
)

func TestRunnerProviderReports(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, account, alias string
		file, missing, fail  bool
	}{
		{name: "default and refresh", account: "macbook-pro-m1", alias: "macbook-pro-m1"},
		{name: "normalized instance", account: "Cory's MacBook", alias: "cory-s-macbook"},
		{name: "bounded instance alias", account: strings.Repeat("a", 80), alias: strings.Repeat("a", 64)},
		{name: "explicit file precedence", file: true},
		{name: "explicit missing file does not fall back", file: true, missing: true},
		{name: "catalog failure remains instance owned", fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 9, 6, 0, 0, 0, time.UTC)
			cfg := globalconfig.Config{Global: globalconfig.Settings{MaxConcurrentAgents: 4}}
			explicit := providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "mac-studio", Models: []string{"configured-model"}, MaxConcurrent: 12, Availability: "available", ObservedAt: now}
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
			})
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
