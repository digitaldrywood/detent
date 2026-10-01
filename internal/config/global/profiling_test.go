package global

import (
	"path/filepath"
	"testing"
	"time"
)

func TestParseProfiling(t *testing.T) {
	for _, tt := range []struct {
		name, yaml string
		bytes      int64
		bad        bool
	}{
		{"omitted", "", 1_000_000_000, false},
		{"enabled defaults", "profiling: {listen_addr: '127.0.0.1:0', capture: {enabled: true}}", 1_000_000_000, false},
		{"decimal size", "profiling: {capture: {max_bytes: 2MB}}", 2_000_000, false},
		{"binary size", "profiling: {capture: {max_bytes: 2MiB}}", 2 << 20, false},
		{"numeric size", "profiling: {capture: {max_bytes: 12345}}", 12345, false},
		{"public address", "profiling: {listen_addr: '0.0.0.0:6060'}", 0, true},
		{"zero interval", "profiling: {capture: {interval: 0s}}", 0, true},
		{"invalid duration", "profiling: {capture: {interval: soon}}", 0, true},
		{"zero cpu", "profiling: {capture: {cpu_duration: 0s}}", 0, true},
		{"cpu exceeds interval", "profiling: {capture: {interval: 10s}}", 0, true},
		{"zero age", "profiling: {capture: {max_age: 0s}}", 0, true},
		{"zero size", "profiling: {capture: {max_bytes: 0}}", 0, true},
		{"negative size", "profiling: {capture: {max_bytes: -1}}", 0, true},
		{"overflow size", "profiling: {capture: {max_bytes: 9223372036854775807GB}}", 0, true},
		{"invalid size", "profiling: {capture: {max_bytes: plenty}}", 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := "apiVersion: detent/v1\nkind: GlobalConfig\nglobal: {max_concurrent_agents: 2, scheduling: weighted}\nprojects: []\n" + tt.yaml + "\n"
			cfg, err := Parse([]byte(raw), "", WithHome(t.TempDir()))
			if (err != nil) != tt.bad {
				t.Fatalf("Parse() = %v, want error %v", err, tt.bad)
			}
			if tt.bad {
				return
			}
			capture := cfg.Profiling.Capture
			if int64(capture.MaxBytes) != tt.bytes || capture.Interval != 15*time.Minute || capture.CPUDuration != 30*time.Second || capture.MaxAge != 168*time.Hour {
				t.Fatalf("profiling defaults = %+v", capture)
			}
			path := filepath.Join(t.TempDir(), "global.yaml")
			if tt.name == "enabled defaults" {
				cfg.Profiling.Capture.Interval = 0
				cfg.Profiling.Capture.CPUDuration = 0
				cfg.Profiling.Capture.MaxAge = 0
				cfg.Profiling.Capture.MaxBytes = 0
			}
			if err := Write(path, cfg); err != nil {
				t.Fatal(err)
			}
			written, err := Read(path)
			if err != nil || written.Profiling != cfg.Profiling.Normalized() {
				t.Fatalf("profiling changed after config write: got=%+v want=%+v error=%v", written.Profiling, cfg.Profiling, err)
			}
		})
	}
}
