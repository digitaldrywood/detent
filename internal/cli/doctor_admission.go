package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

const (
	doctorAdmissionFailureThreshold   = 3
	doctorAdmissionRecentRunLimit     = 20
	doctorAdmissionResponsiveCadence  = 30 * time.Minute
	doctorAdmissionCadenceSampleCount = 512
)

type doctorAdmissionDiagnostic struct {
	Schedule             string   `json:"schedule"`
	MaximumGapSeconds    int64    `json:"maximum_gap_seconds,omitempty"`
	CriteriaSection      string   `json:"criteria_section"`
	Dimensions           []string `json:"dimensions,omitempty"`
	Warnings             []string `json:"warnings,omitempty"`
	RepositoryVisibility string   `json:"repository_visibility,omitempty"`
}

func checkDoctorAdmission(ctx context.Context, projectID string, workflow workflowconfig.Workflow, deps doctorDeps) doctorCheck {
	cfg := workflow.Config.BacklogAdmission
	name := "Project " + projectID + " backlog admission"
	deps = deps.withDefaults()
	diagnostic := doctorAdmissionDiagnostic{
		Schedule:          cfg.Schedule,
		MaximumGapSeconds: int64(doctorAdmissionMaximumGap(cfg.Schedule) / time.Second),
		CriteriaSection:   cfg.CriteriaSection,
		Warnings:          workflowconfig.BacklogAdmissionWarnings(cfg, workflow.Config.Tracker),
	}
	if doctorTrackerUsesGitHubReads(workflow.Config.Tracker.Kind) {
		repository := strings.TrimSpace(workflow.Config.Tracker.Repository)
		if repository != "" {
			info, err := deps.githubRepositoryInfo(ctx, workflow.Config, repository)
			if err != nil {
				diagnostic.Warnings = append(diagnostic.Warnings, "backlog_admission repository visibility could not be determined: "+err.Error())
			} else {
				diagnostic.RepositoryVisibility = info.Visibility
				if warning := workflowconfig.BacklogAdmissionPublicExposureWarning(cfg, info.Visibility); warning != "" {
					diagnostic.Warnings = append(diagnostic.Warnings, warning)
				}
			}
		}
	}
	criteria, err := workflowconfig.ResolveAdmissionCriteria(workflow.SharedPrompt, cfg.CriteriaSection)
	if err != nil {
		return doctorCheck{
			Name:             name,
			Status:           doctorWarn,
			Detail:           err.Error(),
			Hint:             "Define one unique, non-empty criteria section in the shared WORKFLOW.md.",
			BacklogAdmission: &diagnostic,
		}
	}
	for _, dimension := range criteria.Dimensions {
		diagnostic.Dimensions = append(diagnostic.Dimensions, dimension.Name)
	}
	check := doctorCheck{Name: name, Status: doctorOK, BacklogAdmission: &diagnostic}
	details := []string{
		"schedule " + strconvQuote(diagnostic.Schedule),
		"criteria section " + strconvQuote(diagnostic.CriteriaSection),
		fmt.Sprintf("%d project-defined dimensions", len(diagnostic.Dimensions)),
	}
	if diagnostic.MaximumGapSeconds > 0 {
		details = append(details, "maximum cadence gap "+(time.Duration(diagnostic.MaximumGapSeconds)*time.Second).String())
	}
	if len(diagnostic.Warnings) > 0 {
		check.Status = doctorWarn
		details = append(details, diagnostic.Warnings...)
	}
	check.Detail = strings.Join(details, "; ")
	return check
}

func defaultDoctorGitHubRepositoryInfo(
	ctx context.Context,
	cfg workflowconfig.Config,
	repository string,
) (_ ghconnector.RepositoryInfo, resultErr error) {
	conn, err := ghconnector.NewConnector(doctorGitHubConnectorConfig(cfg))
	if err != nil {
		return ghconnector.RepositoryInfo{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, conn.Close())
	}()
	return conn.FetchRepositoryInfo(ctx, repository)
}

func doctorAdmissionMaximumGap(scheduleExpression string) time.Duration {
	scheduleExpression = strings.TrimSpace(scheduleExpression)
	if !strings.HasPrefix(scheduleExpression, "TZ=") && !strings.HasPrefix(scheduleExpression, "CRON_TZ=") {
		scheduleExpression = "CRON_TZ=UTC " + scheduleExpression
	}
	schedule, err := cron.ParseStandard(scheduleExpression)
	if err != nil {
		return 0
	}
	origin := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	previous := schedule.Next(origin)
	var maximumGap time.Duration
	for range doctorAdmissionCadenceSampleCount {
		next := schedule.Next(previous)
		if !next.After(previous) {
			return maximumGap
		}
		maximumGap = max(maximumGap, next.Sub(previous))
		previous = next
	}
	return maximumGap
}

func strconvQuote(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}
