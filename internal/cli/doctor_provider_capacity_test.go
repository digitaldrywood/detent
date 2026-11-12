package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestDoctorProviderCapacity(t *testing.T) {
	t.Setenv("DETENT_TEST_PROVIDER_TOKEN", "test")
	for _, test := range []struct {
		name, state             string
		used                    int
		missing, offline, empty bool
		want                    doctorStatus
	}{
		{name: "available", state: "available", want: doctorOK},
		{name: "unknown", state: "unknown", want: doctorWarn},
		{name: "exhausted", state: "exhausted", want: doctorWarn},
		{name: "reserved", state: "available", used: 1, want: doctorWarn},
		{name: "missing report", missing: true, want: doctorFail},
		{name: "Hub outage", state: "available", offline: true, want: doctorWarn},
		{name: "not registered", state: "available", empty: true, want: doctorWarn},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "work", Models: []string{"sol"}, MaxConcurrent: 1, Availability: "unknown", ObservedAt: time.Now()}
			path := filepath.Join(t.TempDir(), "report.json")
			if !test.missing {
				raw, err := json.Marshal([]providercapacity.Report{report})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.offline {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				fleet := []runnerauth.Runner{{ProviderCapacity: []providercapacity.View{{Report: report, State: test.state, Used: test.used, Reason: "Capacity observation"}}}}
				if test.empty {
					fleet = nil
				}
				if err := json.NewEncoder(w).Encode(fleet); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			cfg := globalconfig.Config{Client: globalconfig.HubClient{URL: server.URL, OrganizationID: "org_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TokenEnvironment: "DETENT_TEST_PROVIDER_TOKEN", ProviderCapacityFile: path}}
			check := checkDoctorProviderCapacity(t.Context(), cfg)
			if check.Status != test.want || check.Detail == "" {
				t.Fatalf("doctor = %+v", check)
			}
		})
	}
}
