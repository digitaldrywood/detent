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

func TestINV12WorkspaceCacheKeys(t *testing.T) {
	for _, tt := range []struct {
		name, input string
		rejected    bool
	}{
		{"native", "workspace: {}", false},
		{"legacy warning", "workspace: {cache_strategy: isolated}", false},
		{"build cache", "workspace: {build_cache: /tmp/build}", true},
		{"cache root", "workspace: {cache_root: /tmp/cache}", true},
		{"cyclic unknown", "workspace: {tools: &loop {again: *loop}}", true},
		{"nested cache", "workspace: {tools: {cache_dir: /tmp/cache}}", true},
		{"merged cache", "workspace: {<<: &settings {cache_root: /tmp/cache}}", true},
		{"mixed case", "workspace: {goCache: /tmp/cache}", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			err := yaml.Unmarshal([]byte(tt.input), &cfg)
			if (err != nil) != tt.rejected {
				t.Fatalf("error = %v, rejected = %t", err, tt.rejected)
			}
			if err != nil && !strings.Contains(err.Error(), "INV-12") {
				t.Fatalf("missing invariant: %v", err)
			}
		})
	}
}
