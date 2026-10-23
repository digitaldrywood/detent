package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/lessons"
)

func checkDoctorLessonCaptures(ctx context.Context, cfg globalconfig.Config, deps doctorDeps) []doctorCheck {
	checks := make([]doctorCheck, 0, len(cfg.Projects))
	for _, project := range cfg.Projects {
		projectID := doctorProjectID(project)
		name := "Lessons [" + projectID + "]"
		workflow, err := loadDoctorProjectWorkflow(ctx, project, deps)
		if err != nil {
			checks = append(checks, doctorLessonCaptureWarning(name, err))
			continue
		}
		path := strings.TrimSpace(workflow.Config.Agent.Lessons.Path)
		if path == "" {
			path = lessons.DefaultPath
		}
		path, err = resolveDoctorProjectPath(project, path)
		if err != nil {
			checks = append(checks, doctorLessonCaptureWarning(name, err))
			continue
		}
		summary, err := lessons.CaptureSummary(path)
		if err != nil {
			checks = append(checks, doctorLessonCaptureWarning(name, fmt.Errorf("read %s: %w", path, err)))
			continue
		}
		lastCapture := "never"
		if summary.LastCapturedAt != nil {
			lastCapture = summary.LastCapturedAt.UTC().Format(time.RFC3339Nano)
		}
		checks = append(checks, doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: fmt.Sprintf("%d captured lesson entries; last capture %s", summary.Count, lastCapture),
		})
	}
	return checks
}

func doctorLessonCaptureWarning(name string, err error) doctorCheck {
	return doctorCheck{
		Name:   name,
		Status: doctorWarn,
		Detail: fmt.Sprintf("lesson captures could not be read: %v", err),
		Hint:   "Confirm the project workdir and agent.lessons.path are readable, then rerun detent doctor.",
	}
}
