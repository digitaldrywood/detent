package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
)

func TestCheckDoctorProjectPause(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name          string
		project       globalconfig.Project
		issues        []connector.Issue
		wantStatus    doctorStatus
		wantDetail    string
		wantConnector bool
	}{
		{
			name: "closed exit issue",
			project: globalconfig.Project{
				ID:               "detent",
				Paused:           true,
				PausedReason:     "wait for fix",
				PausedAt:         now.Add(-24 * time.Hour).Format(time.RFC3339),
				PausedUntilIssue: "digitaldrywood/detent#1499",
			},
			issues: []connector.Issue{{
				Identifier: "digitaldrywood/detent#1499",
				Closed:     true,
			}},
			wantStatus:    doctorWarn,
			wantDetail:    "is paused even though pause exit issue digitaldrywood/detent#1499 is closed",
			wantConnector: true,
		},
		{
			name: "stale indefinite pause",
			project: globalconfig.Project{
				ID:       "detent",
				Paused:   true,
				PausedAt: now.Add(-8 * 24 * time.Hour).Format(time.RFC3339),
			},
			wantStatus: doctorWarn,
			wantDetail: "has been paused without an exit condition",
		},
		{
			name: "open exit issue",
			project: globalconfig.Project{
				ID:               "detent",
				Paused:           true,
				PausedReason:     "wait for fix",
				PausedAt:         now.Add(-24 * time.Hour).Format(time.RFC3339),
				PausedUntilIssue: "digitaldrywood/detent#1499",
			},
			issues: []connector.Issue{{
				Identifier: "digitaldrywood/detent#1499",
			}},
			wantStatus:    doctorOK,
			wantDetail:    "project detent is paused: wait for fix",
			wantConnector: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			connectorCreated := false
			check, ok := checkDoctorProjectPause(
				context.Background(),
				"detent",
				tt.project,
				workflowconfig.Config{},
				doctorDeps{
					now: func() time.Time { return now },
					autoPromoteConnector: func(workflowconfig.Config) (doctorAutoPromoteConnector, error) {
						connectorCreated = true
						return &fakeDoctorAutoPromoteConnector{resolvedIssues: tt.issues}, nil
					},
				},
			)
			if !ok {
				t.Fatal("checkDoctorProjectPause() ok = false, want true")
			}
			if check.Status != tt.wantStatus {
				t.Fatalf("Status = %s, want %s", check.Status, tt.wantStatus)
			}
			if !strings.Contains(check.Detail, tt.wantDetail) {
				t.Fatalf("Detail = %q, want substring %q", check.Detail, tt.wantDetail)
			}
			if connectorCreated != tt.wantConnector {
				t.Fatalf("connector created = %t, want %t", connectorCreated, tt.wantConnector)
			}
		})
	}
}

func TestCheckDoctorProjectPauseSkipsUnpausedProject(t *testing.T) {
	t.Parallel()

	if check, ok := checkDoctorProjectPause(
		context.Background(),
		"detent",
		globalconfig.Project{ID: "detent"},
		workflowconfig.Config{},
		doctorDeps{},
	); ok {
		t.Fatalf("checkDoctorProjectPause() = %#v, %t, want false", check, ok)
	}
}
