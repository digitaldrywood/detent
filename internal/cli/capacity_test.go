package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	servicepkg "github.com/digitaldrywood/detent/internal/service"
)

func TestRunCapacityClear(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/capacity/clear" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer operator-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if err := request.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if request.Form.Get("project_id") != "detent" || request.Form.Get("scope") != "codex" {
			t.Fatalf("form = %#v", request.Form)
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(capacityClearResult{Status: "cleared", Project: "detent", Scope: "codex", Cleared: 1})
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	port, err := strconv.Atoi(serverURL.Port())
	if err != nil {
		t.Fatalf("Atoi() error = %v", err)
	}

	opts := defaultOptions()
	opts.resolvePath = func(string) (globalconfig.PathResolution, error) {
		return globalconfig.PathResolution{Path: "/tmp/global.yaml", Rule: globalconfig.PathRuleFlag}, nil
	}
	opts.read = func(string) (globalconfig.Config, error) {
		return globalconfig.Config{APIToken: "operator-token"}, nil
	}
	opts.httpDo = server.Client().Do

	result, err := runCapacityClear(t.Context(), "/tmp/global.yaml", serverURL.Hostname(), port, true, "detent", "codex", opts)
	if err != nil {
		t.Fatalf("runCapacityClear() error = %v", err)
	}
	if result.Cleared != 1 || result.Project != "detent" || result.Scope != "codex" {
		t.Fatalf("result = %#v", result)
	}
}

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
	configuredHost := "100.109.187.102"
	configuredPort := 4101
	root := t.TempDir()
	workflowPath := filepath.Join(root, "WORKFLOW.md")
	writeBootHostWorkflow(t, workflowPath, configuredHost)
	configured := globalconfig.Config{
		Port: &configuredPort,
		Projects: []globalconfig.Project{{
			ID:       "detent",
			Workflow: workflowPath,
			Workdir:  root,
		}},
	}

	tests := []struct {
		name      string
		config    globalconfig.Config
		host      string
		port      int
		portSet   bool
		service   bool
		wantValue string
		wantText  string
	}{
		{
			name:      "configured address",
			config:    configured,
			port:      -1,
			wantValue: "100.109.187.102:4101",
			wantText:  "100.109.187.102:4101 (from config)",
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

func TestRunCapacityClearUsesConfiguredAddress(t *testing.T) {
	configuredPort := 4101
	root := t.TempDir()
	workflowPath := filepath.Join(root, "WORKFLOW.md")
	writeBootHostWorkflow(t, workflowPath, "100.109.187.102")
	opts := dashboardAddressOptions(globalconfig.Config{
		Port: &configuredPort,
		Projects: []globalconfig.Project{{
			ID:       "detent",
			Workflow: workflowPath,
			Workdir:  root,
		}},
	})
	opts.httpDo = func(request *http.Request) (*http.Response, error) {
		if got := request.URL.Host; got != "100.109.187.102:4101" {
			t.Fatalf("request host = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"status":"cleared","cleared":1}`)),
		}, nil
	}

	result, err := runCapacityClear(t.Context(), "/config/global.yaml", "", -1, false, "", "codex", opts)
	if err != nil {
		t.Fatalf("runCapacityClear() error = %v", err)
	}
	if result.Cleared != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunCapacityClearConnectionErrorIncludesAddressSource(t *testing.T) {
	opts := dashboardAddressOptions(globalconfig.Config{})
	opts.httpDo = func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	}

	_, err := runCapacityClear(t.Context(), "/config/global.yaml", "127.0.0.8", 4202, true, "", "codex", opts)
	if err == nil || !strings.Contains(err.Error(), "127.0.0.8:4202 (from flag)") {
		t.Fatalf("runCapacityClear() error = %v", err)
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
