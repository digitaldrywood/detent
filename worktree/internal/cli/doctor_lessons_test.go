package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/lessons"
)

func TestCheckDoctorLessonCaptures(t *testing.T) {
	t.Parallel()

	capturedAt := time.Date(2026, 7, 17, 15, 45, 0, 0, time.UTC)
	tests := []struct {
		name       string
		captures   int
		loadErr    error
		wantStatus doctorStatus
		wantDetail string
	}{
		{
			name:       "no captures",
			wantStatus: doctorOK,
			wantDetail: "0 captured lesson entries; last capture never",
		},
		{
			name:       "reports count and latest capture",
			captures:   2,
			wantStatus: doctorOK,
			wantDetail: "2 captured lesson entries; last capture " + capturedAt.Add(time.Minute).Format(time.RFC3339Nano),
		},
		{
			name:       "workflow unavailable",
			loadErr:    errors.New("workflow unavailable"),
			wantStatus: doctorWarn,
			wantDetail: "workflow unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workdir := t.TempDir()
			lessonPath := filepath.Join(workdir, lessons.DefaultPath)
			for index := range tt.captures {
				_, err := lessons.AppendUnique(lessonPath, lessons.Entry{
					IssueRef:    "digitaldrywood/detent#1397",
					FailureKind: "ci_failure",
					CaptureKey:  "doctor-test-" + string(rune('a'+index)),
				}, lessons.AppendOptions{Date: capturedAt.Add(time.Duration(index) * time.Minute)})
				if err != nil {
					t.Fatalf("lessons.AppendUnique() error = %v", err)
				}
			}

			cfg := workflowconfig.Default()
			checks := checkDoctorLessonCaptures(context.Background(), globalconfig.Config{
				Projects: []globalconfig.Project{{ID: "detent", Workdir: workdir, Workflow: filepath.Join(workdir, "WORKFLOW.md")}},
			}, doctorDeps{
				loadWorkflow: func(string) (workflowconfig.Workflow, error) {
					return workflowconfig.Workflow{Config: cfg}, tt.loadErr
				},
			})
			if len(checks) != 1 {
				t.Fatalf("checkDoctorLessonCaptures() len = %d, want 1", len(checks))
			}
			if checks[0].Name != "Lessons [detent]" || checks[0].Status != tt.wantStatus || !strings.Contains(checks[0].Detail, tt.wantDetail) {
				t.Fatalf("checkDoctorLessonCaptures() = %#v, want status %s detail containing %q", checks[0], tt.wantStatus, tt.wantDetail)
			}
		})
	}
}
