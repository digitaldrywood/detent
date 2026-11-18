package cli

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCheckDoctorDispatchStalls(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		projectID   string
		body        string
		wantStatus  doctorStatus
		wantProject string
	}{
		{
			name:       "healthy fleet",
			body:       `{"status":"ok","mode":"fleet","checks":{"hub":"ok","store":"ok","registry":"ok","connector":"ok"}}`,
			wantStatus: doctorOK,
		},
		{
			name:        "total selector exclusion warns",
			body:        `{"status":"needs_attention","mode":"fleet","checks":{"hub":"ok","store":"ok","registry":"ok","connector":"ok"},"dispatch_stalls":[{"project_id":"detent","candidate_count":8,"wait_reason":"authorization selector excludes every candidate","wait_reason_code":"authorization_selector_declined","stall_duration_seconds":10800,"stalled":true,"needs_human_attention":true,"class":"fault"}]}`,
			wantStatus:  doctorWarn,
			wantProject: "detent stalled for 3h0m0s",
		},
		{
			name:       "pacing stall is diagnostic",
			body:       `{"status":"ok","mode":"fleet","checks":{"hub":"ok","store":"ok","registry":"ok","connector":"ok"},"dispatch_stalls":[{"project_id":"detent","candidate_count":8,"wait_reason":"GitHub REST capacity paused","wait_reason_code":"github_rest_capacity_paused","stall_duration_seconds":10800,"stalled":true,"class":"diagnostic"}]}`,
			wantStatus: doctorOK,
		},
		{
			name:       "project scope ignores another project",
			projectID:  "drywood",
			body:       `{"status":"needs_attention","mode":"fleet","checks":{"hub":"ok","store":"ok","registry":"ok","connector":"ok"},"dispatch_stalls":[{"project_id":"detent","candidate_count":8,"wait_reason":"authorization selector excludes every candidate","wait_reason_code":"authorization_selector_declined","stall_duration_seconds":10800,"stalled":true,"needs_human_attention":true,"class":"fault"}]}`,
			wantStatus: doctorOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := checkDoctorDispatchStalls(context.Background(), BootConfig{}, tt.projectID, doctorDeps{
				httpDo: func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tt.body))}, nil
				},
			})
			if got.Status != tt.wantStatus {
				t.Fatalf("Status = %s, want %s: %#v", got.Status, tt.wantStatus, got)
			}
			if tt.wantProject != "" && !strings.Contains(got.Detail, tt.wantProject) {
				t.Fatalf("Detail = %q, want containing %q", got.Detail, tt.wantProject)
			}
		})
	}
}
