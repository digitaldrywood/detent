package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestComputeRatesConfiguration(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, price string
		wantError   bool
	}{
		{"override", "0.5", false}, {"free", "0", false}, {"negative", "-1", true}, {"nan", ".nan", true}, {"infinity", ".inf", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			workflow, err := ParseWorkflow([]byte(fmt.Sprintf("---\ntracker:\n  kind: memory\nworker:\n  compute_rates:\n    local:\n      cpu_hour_usd: %s\n    sprite:\n      memory_gb_hour_usd: 0.125\n---\nWork\n", tt.price)))
			if err == nil {
				err = workflow.Config.Validate()
			}
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), "worker.compute_rates.local") {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			local, remote := workflow.Config.Worker.ComputeRates["local"], workflow.Config.Worker.ComputeRates["sprite"]
			if local.CPUHourUSD == nil || local.MemoryGBHourUSD != nil || remote.CPUHourUSD != nil || remote.MemoryGBHourUSD == nil || *remote.MemoryGBHourUSD != .125 {
				t.Fatalf("rate overrides = %+v / %+v", local, remote)
			}
		})
	}
}
