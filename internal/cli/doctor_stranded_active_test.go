package cli

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestCheckDoctorStrandedActive(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"status":"ok",
			"mode":"running",
			"checks":{"hub":"configured","store":"configured","registry":"configured","connector":"configured"},
			"stranded_active_issues":[
				{"project_id":"detent","issue_id":"issue-1","identifier":"digitaldrywood/detent#1606","duration_seconds":900,"last_refusal_reason":"priority reservation"}
			]
		}`))
	}))
	t.Cleanup(server.Close)
	host, portText := splitStalenessTestServerAddress(t, server)
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}
	deps := doctorDeps{httpDo: server.Client().Do}.withDefaults()

	tests := []struct {
		name        string
		projectID   string
		wantStatus  doctorStatus
		wantIssues  int
		wantDetails []string
	}{
		{
			name:        "reports matching project",
			projectID:   "detent",
			wantStatus:  doctorWarn,
			wantIssues:  1,
			wantDetails: []string{"digitaldrywood/detent#1606", "15m0s", "priority reservation"},
		},
		{
			name:       "filters other project",
			projectID:  "other",
			wantStatus: doctorOK,
			wantIssues: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			check := checkDoctorStrandedActive(t.Context(), BootConfig{Host: host, Port: &port}, tt.projectID, deps)
			if check.Status != tt.wantStatus || len(check.StrandedIssues) != tt.wantIssues {
				t.Fatalf("check = %#v, want status %q and %d issue(s)", check, tt.wantStatus, tt.wantIssues)
			}
			for _, detail := range tt.wantDetails {
				if !strings.Contains(check.Detail, detail) {
					t.Fatalf("detail = %q, want %q", check.Detail, detail)
				}
			}
		})
	}
}
