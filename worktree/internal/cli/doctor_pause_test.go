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

func TestCheckDoctorProjectPauseUsesOwningProjectConnector(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	project := globalconfig.Project{
		ID:               "digitaldrywood-video",
		Workflow:         "video-workflow",
		Paused:           true,
		PausedUntilIssue: "digitaldrywood/video-studio#147",
	}
	currentWorkflow := workflowconfig.Default()
	currentWorkflow.Tracker.Kind = workflowconfig.TrackerLocalSQLite
	currentWorkflow.Tracker.LocalSQLite.Path = "video.db"
	ownerWorkflow := workflowconfig.Default()
	ownerWorkflow.Tracker.Kind = workflowconfig.TrackerGitHub
	ownerWorkflow.Tracker.Repository = "digitaldrywood/video-studio"
	var connectorRepository string

	check, ok := checkDoctorProjectPause(context.Background(), project.ID, project, currentWorkflow, doctorDeps{
		now: func() time.Time { return now },
		pauseProjects: []globalconfig.Project{
			project,
			{ID: "video-studio", Workflow: "studio-workflow"},
		},
		loadWorkflow: func(path string) (workflowconfig.Workflow, error) {
			if path == "studio-workflow" {
				return workflowconfig.Workflow{Config: ownerWorkflow}, nil
			}
			return workflowconfig.Workflow{Config: currentWorkflow}, nil
		},
		autoPromoteConnector: func(workflow workflowconfig.Config) (doctorAutoPromoteConnector, error) {
			connectorRepository = workflow.Tracker.Repository
			return &fakeDoctorAutoPromoteConnector{resolvedIssues: []connector.Issue{{
				Identifier: "digitaldrywood/video-studio#147",
				Closed:     true,
			}}}, nil
		},
	})
	if !ok {
		t.Fatal("checkDoctorProjectPause() ok = false, want true")
	}
	if check.Status != doctorWarn || !strings.Contains(check.Detail, "video-studio#147 is closed") {
		t.Fatalf("check = %#v, want closed exit issue warning", check)
	}
	if connectorRepository != "digitaldrywood/video-studio" {
		t.Fatalf("connector repository = %q, want digitaldrywood/video-studio", connectorRepository)
	}
}
