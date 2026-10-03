package global

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/profiling"
)

func TestProfilingConfig(t *testing.T) {
	for _, test := range []struct {
		name, block string
		valid       bool
	}{
		{"omitted", "", true}, {"defaults", "profiling: {}\n", true},
		{"enabled", "profiling:\n  listen_addr: localhost:0\n  capture: {enabled: true, interval: 1m, cpu_duration: 1s, max_age: 24h, max_bytes: 1GB, dir: profiles}\n", true},
		{"numeric bytes", "profiling: {capture: {max_bytes: 1024}}\n", true},
		{"public listener", "profiling: {listen_addr: '0.0.0.0:6060'}\n", false},
		{"zero interval", "profiling: {capture: {interval: 0s}}\n", false},
		{"negative CPU", "profiling: {capture: {cpu_duration: -1s}}\n", false},
		{"zero age", "profiling: {capture: {max_age: 0s}}\n", false},
		{"zero bytes", "profiling: {capture: {max_bytes: 0}}\n", false},
		{"overflow bytes", "profiling: {capture: {max_bytes: 9223372036854775807GB}}\n", false},
		{"invalid duration", "profiling: {capture: {interval: soon}}\n", false},
		{"invalid size", "profiling: {capture: {max_bytes: true}}\n", false},
		{"invalid mapping", "profiling: true\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "global.yaml")
			raw := "apiVersion: detent/v1\nkind: GlobalConfig\nglobal: {max_concurrent_agents: 2, scheduling: weighted}\nprojects: []\n" + test.block
			cfg, err := Parse([]byte(raw), path, WithHome(root))
			if (err == nil) != test.valid {
				t.Fatalf("Parse() = %v, valid = %v", err, test.valid)
			}
			if !test.valid {
				return
			}
			if test.name == "enabled" {
				if !cfg.Profiling.Capture.Enabled || cfg.Profiling.Capture.MaxBytes != 1_000_000_000 || cfg.Profiling.Capture.Interval != time.Minute || cfg.Profiling.Capture.Dir != filepath.Join(root, "profiles") {
					t.Fatalf("config = %+v", cfg.Profiling)
				}
			}
			if err := Write(path, cfg, WithHome(root)); err != nil {
				t.Fatal(err)
			}
			loaded, err := Read(path, WithHome(root))
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.Profiling.IsZero() && cfg.Profiling != loaded.Profiling {
				t.Fatalf("round-trip changed profiling: %+v -> %+v", cfg.Profiling, loaded.Profiling)
			}
			if cfg.Profiling.IsZero() {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(data), "profiling:") {
					t.Fatal("disabled defaults serialized")
				}
			} else if loaded.Profiling == (profiling.Config{}) {
				t.Fatal("active profiling omitted")
			}
		})
	}
}
