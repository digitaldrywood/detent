package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestReadDoctorBackendCapacityLive(t *testing.T) {
	t.Parallel()

	port := 4000
	deps := doctorDeps{
		httpDo: func(request *http.Request) (*http.Response, error) {
			if request.URL.String() != "http://127.0.0.1:4000/health" {
				t.Fatalf("URL = %s", request.URL)
			}
			body := `{"backend_outages":[{"project_id":"detent","backend_id":"codex","provider":"openai","last_probe_result":"capacity_exhausted"},{"project_id":"other","backend_id":"claude"}]}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		},
	}
	outages, ok := readDoctorBackendCapacityLive(t.Context(), BootConfig{
		Host: "127.0.0.1",
		Port: &port,
	}, "detent", deps)
	if !ok || len(outages) != 1 || outages[0].BackendID != "codex" || outages[0].LastProbeResult != "capacity_exhausted" {
		t.Fatalf("outages = %#v, available %v", outages, ok)
	}
}

func TestCheckDoctorBackendCapacityUsesLiveState(t *testing.T) {
	t.Parallel()

	port := 4000
	check := checkDoctorBackendCapacity(t.Context(), BootConfig{Host: "127.0.0.1", Port: &port}, "detent", doctorDeps{
		httpDo: func(*http.Request) (*http.Response, error) {
			body := `{"backend_outages":[{"project_id":"detent","backend_id":"codex","provider":"openai","reason":"provider usage limit reached","resume_at":"2026-07-10T05:00:00Z"}]}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		},
	})

	if check.Status != doctorWarn || len(check.BackendCapacity) != 1 || !check.BackendCapacity[0].Enforced {
		t.Fatalf("check = %#v, want enforced live outage warning", check)
	}
}

func TestCheckDoctorBackendCapacityWithoutLiveOutage(t *testing.T) {
	t.Parallel()

	port := 4000
	for _, tc := range []struct {
		name   string
		status int
		want   string
	}{
		{name: "no outage", status: http.StatusOK, want: "live orchestrator reports no enforced outage"},
		{name: "unreachable", status: http.StatusServiceUnavailable, want: "live orchestrator state unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			check := checkDoctorBackendCapacity(t.Context(), BootConfig{Host: "127.0.0.1", Port: &port}, "detent", doctorDeps{
				httpDo: func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(`{"backend_outages":[]}`))}, nil
				},
			})
			if check.Status != doctorOK || check.Detail != tc.want || len(check.BackendCapacity) != 0 {
				t.Fatalf("check = %#v, want %q", check, tc.want)
			}
		})
	}
}
