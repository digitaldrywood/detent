package global

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestOpsConfigRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		enabled bool
	}{
		{name: "enabled", enabled: true},
		{name: "disabled", enabled: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "global.yaml")
			cfg, err := DefaultAt(path)
			if err != nil {
				t.Fatalf("DefaultAt() error = %v", err)
			}
			cfg.Ops.TmuxWindowStatus = &test.enabled
			if err := Write(path, cfg); err != nil {
				t.Fatalf("Write() error = %v", err)
			}

			got, err := Read(path)
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if got.Ops.TmuxWindowStatus == nil || *got.Ops.TmuxWindowStatus != test.enabled {
				t.Fatalf("Ops.TmuxWindowStatus = %v, want %t", got.Ops.TmuxWindowStatus, test.enabled)
			}
		})
	}
}

func TestOpsConfigValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{name: "mapping", config: "ops: enabled", want: "ops: must be a mapping"},
		{name: "boolean", config: "ops:\n  tmux_window_status: sometimes", want: "ops.tmux_window_status: must be a boolean"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(`apiVersion: detent/v1
kind: GlobalConfig
`+test.config+`
global:
  max_concurrent_agents: 8
  scheduling: weighted
projects: []
`), test.name+".yaml")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want containing %q", err, test.want)
			}
		})
	}
}
