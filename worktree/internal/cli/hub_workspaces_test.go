package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

func TestReadHostedWorkspaceConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		yaml        string
		wantEnabled bool
		wantErr     bool
		check       func(t *testing.T, config hubserver.WorkspaceConfig)
	}{
		{name: "absent section", yaml: "organization_id: org_1\n"},
		{name: "disabled section", yaml: "workspaces:\n  enabled: false\n  idle_timeout: 5m\n"},
		{
			name:        "enabled with every key",
			yaml:        "workspaces:\n  enabled: true\n  request_timeout: 2m\n  retain_after_run: 0s\n  idle_timeout: 10m\n  max_lifetime: 1h\n  person_max_open: 2\n  plan:\n    max_open: 7\n  relay:\n    memory: 64MB\n  files:\n    deny: [\"*.pem\"]\n  terminal:\n    enabled: true\n    isolation: container\n    record: false\n",
			wantEnabled: true,
			check: func(t *testing.T, config hubserver.WorkspaceConfig) {
				if config.RequestTimeout != 2*time.Minute || config.RetainAfterRun != 0 || config.IdleTimeout != 10*time.Minute || config.MaxLifetime != time.Hour {
					t.Fatalf("durations = %+v", config)
				}
				if config.PersonMaxOpen != 2 || config.PlanMaxOpen != 7 || config.RelayMemoryBytes != 64<<20 {
					t.Fatalf("limits = %+v", config)
				}
				if len(config.FilesDeny) != 1 || config.FilesDeny[0] != "*.pem" {
					t.Fatalf("files deny = %v", config.FilesDeny)
				}
				if !config.Terminal.Enabled || config.Terminal.Isolation != "container" || config.Terminal.Record == nil || *config.Terminal.Record {
					t.Fatalf("terminal = %+v", config.Terminal)
				}
			},
		},
		{
			name:        "enabled leaves absent durations to the hub defaults",
			yaml:        "workspaces:\n  enabled: true\n",
			wantEnabled: true,
			check: func(t *testing.T, config hubserver.WorkspaceConfig) {
				if config.IdleTimeout != 0 || config.Terminal.Record != nil {
					t.Fatalf("config = %+v", config)
				}
			},
		},
		{name: "zero idle timeout", yaml: "workspaces:\n  enabled: true\n  idle_timeout: 0s\n", wantErr: true},
		{name: "negative request timeout", yaml: "workspaces:\n  enabled: true\n  request_timeout: -1m\n", wantErr: true},
		{name: "unparseable max lifetime", yaml: "workspaces:\n  enabled: true\n  max_lifetime: forever\n", wantErr: true},
		{name: "negative plan max open", yaml: "workspaces:\n  enabled: true\n  plan:\n    max_open: -1\n", wantErr: true},
		{name: "bad relay memory", yaml: "workspaces:\n  enabled: true\n  relay:\n    memory: lots\n", wantErr: true},
		{name: "unknown key", yaml: "workspaces:\n  enabled: true\n  surprise: 1\n", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "hosted.yaml")
			if err := os.WriteFile(path, []byte(test.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			config, enabled, err := readHostedWorkspaceConfig(path)
			if (err != nil) != test.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, test.wantErr)
			}
			if enabled != test.wantEnabled {
				t.Fatalf("enabled = %v, want %v", enabled, test.wantEnabled)
			}
			if test.check != nil {
				test.check(t, config)
			}
		})
	}
}

func TestReadHostedWorkspaceConfigWithoutPath(t *testing.T) {
	t.Parallel()
	if _, enabled, err := readHostedWorkspaceConfig(" "); err != nil || enabled {
		t.Fatalf("enabled = %v, err = %v", enabled, err)
	}
	if _, _, err := readHostedWorkspaceConfig(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("a missing file was accepted")
	}
}

func TestParseByteSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value   string
		want    int64
		wantErr bool
	}{
		{value: "", want: 0},
		{value: "512", want: 512},
		{value: "512B", want: 512},
		{value: "4kb", want: 4 << 10},
		{value: " 256MB ", want: 256 << 20},
		{value: "2GB", want: 2 << 30},
		{value: "MB", wantErr: true},
		{value: "1.5MB", wantErr: true},
		{value: "-1", wantErr: true},
		{value: "2000000000000", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			t.Parallel()
			got, err := parseByteSize(test.value)
			if (err != nil) != test.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("parseByteSize(%q) = %d, want %d", test.value, got, test.want)
			}
		})
	}
}
