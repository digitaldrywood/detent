package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
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

func TestCapacityClearServerAddrNormalizesWildcardHosts(t *testing.T) {
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

			if got := capacityClearServerAddr(BootConfig{Host: tt.host, Port: &port}); got != tt.want {
				t.Fatalf("capacityClearServerAddr() = %q, want %q", got, tt.want)
			}
		})
	}
}
