package cli

import (
	"context"
	"testing"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	servicepkg "github.com/digitaldrywood/detent/internal/service"
	"github.com/digitaldrywood/detent/internal/serviceapi"
)

func TestDashboardServerAddrNormalizesWildcardHosts(t *testing.T) {
	t.Parallel()

	port := 4101
	tests := []struct {
		name string
		host string
		want string
	}{
		{name: "IPv4 wildcard", host: "0.0.0.0", want: "127.0.0.1:4101"},
		{name: "IPv6 wildcard", host: "::", want: "127.0.0.1:4101"},
		{name: "bracketed IPv6 wildcard", host: "[::]", want: "127.0.0.1:4101"},
		{name: "explicit host", host: "localhost", want: "localhost:4101"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := dashboardServerAddr(BootConfig{Host: tt.host, Port: &port}); got != tt.want {
				t.Fatalf("dashboardServerAddr() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveDashboardBootAddressPrecedence(t *testing.T) {
	configuredPort := 4101
	configured := globalconfig.Config{
		Port: &configuredPort,
	}

	tests := []struct {
		name      string
		config    globalconfig.Config
		host      string
		port      int
		portSet   bool
		service   bool
		worker    string
		wantValue string
		wantText  string
	}{
		{
			name:      "configured address",
			config:    configured,
			port:      -1,
			wantValue: "127.0.0.1:4101",
			wantText:  "127.0.0.1:4101 (host from default, port from config)",
		},
		{
			name:      "flags override config",
			config:    configured,
			host:      "127.0.0.8",
			port:      4202,
			portSet:   true,
			wantValue: "127.0.0.8:4202",
			wantText:  "127.0.0.8:4202 (from flag)",
		},
		{
			name:      "worker service address overrides config",
			config:    configured,
			port:      -1,
			worker:    "100.112.223.34:4404",
			service:   true,
			wantValue: "100.112.223.34:4404",
			wantText:  "100.112.223.34:4404 (from worker environment)",
		},
		{
			name:      "running service flags override config",
			config:    configured,
			port:      -1,
			service:   true,
			wantValue: "100.111.222.33:4303",
			wantText:  "100.111.222.33:4303 (from service flag)",
		},
		{
			name:      "defaults",
			port:      -1,
			wantValue: "127.0.0.1:4000",
			wantText:  "127.0.0.1:4000 (from default)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := dashboardAddressOptions(tt.config)
			if tt.worker != "" {
				opts.lookupEnv = func(name string) string {
					if name == serviceapi.AddressEnvironment {
						return tt.worker
					}
					return ""
				}
			}
			if tt.service {
				opts.service = serviceFactoryFor(&serviceRunnerStub{status: servicepkg.Status{
					ServiceManager: servicepkg.ManagerSystemd,
					ServiceScope:   "system",
					Service:        "detent.service",
					State:          servicepkg.StateRunning,
				}})
				opts.runCommand = func(context.Context, string, ...string) (string, error) {
					return "[Service]\nExecStart=/usr/bin/detent --config /config/global.yaml --host 127.0.0.1 --port 4000\n# override.conf\nExecStart=\nExecStart=/usr/bin/detent --config /config/global.yaml --host 100.111.222.33 --port=4303\n", nil
				}
			}
			_, address, err := resolveDashboardBoot(t.Context(), "/config/global.yaml", tt.host, tt.port, tt.portSet, opts)
			if err != nil {
				t.Fatalf("resolveDashboardBoot() error = %v", err)
			}
			if address.Value != tt.wantValue || address.String() != tt.wantText {
				t.Fatalf("address = %#v, text = %q", address, address.String())
			}
		})
	}
}

func dashboardAddressOptions(cfg globalconfig.Config) options {
	opts := defaultOptions()
	opts.resolvePath = func(string) (globalconfig.PathResolution, error) {
		return globalconfig.PathResolution{Path: "/config/global.yaml", Rule: globalconfig.PathRuleFlag}, nil
	}
	opts.read = func(string) (globalconfig.Config, error) {
		return cfg, nil
	}
	opts.lookupEnv = func(string) string { return "" }
	opts.service = serviceFactoryFor(&serviceRunnerStub{status: servicepkg.Status{State: servicepkg.StateStopped}})
	return opts
}
