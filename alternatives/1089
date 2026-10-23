package config

import (
	"strings"
	"testing"
)

func TestSSHHostConfiguration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		hosts     []string
		selection string
		caps      map[string]int
		want      string
	}{
		{"preference", []string{"air", "user@studio", "local"}, "preference", map[string]int{"air": 2, "local": 1}, ""},
		{"least loaded", []string{"air", "local"}, "least_loaded", nil, ""},
		{"unknown selection", []string{"air"}, "random", nil, "worker.host_selection"},
		{"invalid cap", []string{"air"}, "", map[string]int{"air": 0}, "worker.host_caps"},
		{"unknown cap host", []string{"air"}, "", map[string]int{"studio": 2}, "worker.host_caps"},
		{"duplicate", []string{"air", "air"}, "", nil, "worker.ssh_hosts"},
		{"SSH option", []string{"-oProxyCommand=bad"}, "", nil, "worker.ssh_hosts"},
		{"shell injection", []string{"host;touch bad"}, "", nil, "worker.ssh_hosts"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Default()
			cfg.Worker.SSHHosts, cfg.Worker.HostSelection, cfg.Worker.HostCaps = test.hosts, test.selection, test.caps
			err := cfg.Validate()
			if test.want == "" {
				if err != nil && strings.Contains(err.Error(), "worker.") {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation = %v; want %s", err, test.want)
			}
		})
	}
}

func TestSSHSupportsNativeArtifacts(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Tracker.Kind = TrackerHubNative
	cfg.Worker.SSHHosts = []string{"remote", "local"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("native SSH execution was rejected: %v", err)
	}
}
