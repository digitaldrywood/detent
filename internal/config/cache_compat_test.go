package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRemovedCacheStrategyWarning(t *testing.T) {
	for _, tt := range []struct {
		name, input string
		warning     bool
	}{
		{"absent", "workspace: {}", false},
		{"shared", "workspace: {cache_strategy: shared}", true},
		{"isolated", "workspace: {cache_strategy: isolated}", true},
		{"unknown ignored", "workspace: {cache_strategy: something}", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte(tt.input), &cfg); err != nil {
				t.Fatal(err)
			}
			got := strings.Contains(strings.Join(cfg.ValidationWarnings(), "\n"), "workspace.cache_strategy is unknown and ignored")
			if got != tt.warning {
				t.Fatalf("warning = %t, want %t", got, tt.warning)
			}
			data, err := yaml.Marshal(cfg.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "cache_strategy") {
				t.Fatal("removed setting emitted")
			}
		})
	}
}
