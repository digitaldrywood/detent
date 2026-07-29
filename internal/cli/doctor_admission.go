package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	admissionmodel "github.com/digitaldrywood/detent/internal/admission/model"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

const doctorAdmissionFailureThreshold = 3

type doctorAdmissionDiagnostic struct {
	Schedule            string         `json:"schedule"`
	CriteriaSection     string         `json:"criteria_section"`
	Dimensions          []string       `json:"dimensions,omitempty"`
	NeverRun            bool           `json:"never_run,omitempty"`
	LastRun             string         `json:"last_run,omitempty"`
	Outcome             string         `json:"outcome,omitempty"`
	DeferredReason      string         `json:"deferred_reason,omitempty"`
	CandidatesFound     int            `json:"candidates_found,omitempty"`
	CandidatesEvaluated int            `json:"candidates_evaluated,omitempty"`
	Proposed            int            `json:"proposed,omitempty"`
	Skipped             map[string]int `json:"skipped,omitempty"`
	Truncated           map[string]int `json:"truncated,omitempty"`
	IssueReferences     []string       `json:"issue_references,omitempty"`
	ConsecutiveFailures int            `json:"consecutive_failures,omitempty"`
	LatestError         string         `json:"latest_error,omitempty"`
	Warnings            []string       `json:"warnings,omitempty"`
}

func checkDoctorAdmission(
	ctx context.Context,
	projectID string,
	workflow workflowconfig.Workflow,
	storePath string,
	deps doctorDeps,
) doctorCheck {
	cfg := workflow.Config.BacklogAdmission
	name := "Project " + projectID + " backlog admission"
	diagnostic := doctorAdmissionDiagnostic{
		Schedule:        cfg.Schedule,
		CriteriaSection: cfg.CriteriaSection,
		NeverRun:        true,
		Warnings:        workflowconfig.BacklogAdmissionWarnings(cfg, workflow.Config.Tracker),
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
	if strings.TrimSpace(storePath) == "" {
		return doctorAdmissionCheck(name, diagnostic, "runtime store is not configured")
	}
	deps = deps.withDefaults()
	db, err := deps.openSQLiteReadOnly(ctx, storePath)
	if err != nil {
		if errors.Is(err, errDoctorTelemetryStoreUnavailable) {
			return doctorAdmissionCheck(name, diagnostic, "runtime store is not available yet")
		}
		return doctorCheck{
			Name:             name,
			Status:           doctorWarn,
			Detail:           fmt.Sprintf("backlog admission telemetry could not be read: %v", err),
			BacklogAdmission: &diagnostic,
		}
	}
	diagnostic, queryErr := readDoctorAdmissionDiagnostic(ctx, db, projectID, diagnostic)
	closeErr := db.Close()
	if queryErr != nil {
		return doctorCheck{
			Name:             name,
			Status:           doctorWarn,
			Detail:           fmt.Sprintf("backlog admission telemetry query failed: %v", queryErr),
			BacklogAdmission: &diagnostic,
		}
	}
	if closeErr != nil {
		return doctorCheck{
			Name:             name,
			Status:           doctorWarn,
			Detail:           fmt.Sprintf("backlog admission telemetry close failed: %v", closeErr),
			BacklogAdmission: &diagnostic,
		}
	}
	return doctorAdmissionCheck(name, diagnostic, "")
}

func readDoctorAdmissionDiagnostic(
	ctx context.Context,
	db doctorTelemetryStore,
	projectID string,
	diagnostic doctorAdmissionDiagnostic,
) (_ doctorAdmissionDiagnostic, resultErr error) {
	rows, err := db.QueryContext(ctx, `
SELECT completed_at, outcome, COALESCE(deferred_reason, ''), candidates_found_count,
       candidates_count, proposed_count, skipped_json, truncated_json, issues_json,
       COALESCE(error, '')
FROM backlog_admission_runs
WHERE project_id = ?
ORDER BY completed_at DESC, id DESC
LIMIT ?`, strings.TrimSpace(projectID), doctorAdmissionFailureThreshold)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table: backlog_admission_runs") {
			return diagnostic, nil
		}
		return diagnostic, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, rows.Close())
	}()
	index := 0
	for rows.Next() {
		var completedAt string
		var outcome string
		var deferredReason string
		var candidatesFound int
		var candidates int
		var proposed int
		var skippedRaw string
		var truncatedRaw string
		var issuesRaw string
		var runError string
		if err := rows.Scan(
			&completedAt,
			&outcome,
			&deferredReason,
			&candidatesFound,
			&candidates,
			&proposed,
			&skippedRaw,
			&truncatedRaw,
			&issuesRaw,
			&runError,
		); err != nil {
			return diagnostic, err
		}
		if index == 0 {
			diagnostic.NeverRun = false
			diagnostic.LastRun = completedAt
			diagnostic.Outcome = outcome
			diagnostic.DeferredReason = deferredReason
			diagnostic.CandidatesFound = candidatesFound
			diagnostic.CandidatesEvaluated = candidates
			diagnostic.Proposed = proposed
			if err := decodeDoctorJSON(skippedRaw, &diagnostic.Skipped); err != nil {
				return diagnostic, err
			}
			if err := decodeDoctorJSON(truncatedRaw, &diagnostic.Truncated); err != nil {
				return diagnostic, err
			}
			var issues []admissionmodel.IssueRecord
			if err := decodeDoctorJSON(issuesRaw, &issues); err != nil {
				return diagnostic, err
			}
			for _, issue := range issues {
				reference := strings.TrimSpace(issue.Identifier)
				if reference == "" {
					reference = strings.TrimSpace(issue.ID)
				}
				if reference != "" {
					diagnostic.IssueReferences = append(diagnostic.IssueReferences, reference)
				}
			}
			diagnostic.LatestError = strings.TrimSpace(runError)
		}
		if strings.TrimSpace(runError) == "" {
			break
		}
		diagnostic.ConsecutiveFailures++
		index++
	}
	return diagnostic, rows.Err()
}

func doctorAdmissionCheck(name string, diagnostic doctorAdmissionDiagnostic, unavailable string) doctorCheck {
	check := doctorCheck{Name: name, Status: doctorOK, BacklogAdmission: &diagnostic}
	details := []string{
		"criteria section " + strconvQuote(diagnostic.CriteriaSection),
		fmt.Sprintf("%d project-defined dimensions", len(diagnostic.Dimensions)),
	}
	if !diagnostic.NeverRun {
		details = append(details, fmt.Sprintf(
			"latest outcome %s at %s candidates=%d evaluated=%d proposed=%d",
			diagnostic.Outcome,
			diagnostic.LastRun,
			diagnostic.CandidatesFound,
			diagnostic.CandidatesEvaluated,
			diagnostic.Proposed,
		))
	}
	switch {
	case unavailable != "":
		check.Status = doctorWarn
		details = append(details, unavailable)
	case diagnostic.NeverRun:
		check.Status = doctorWarn
		details = append(details, "enabled but has never run")
	case diagnostic.ConsecutiveFailures >= doctorAdmissionFailureThreshold:
		check.Status = doctorWarn
		details = append(details, fmt.Sprintf("%d consecutive failures", diagnostic.ConsecutiveFailures))
	case diagnostic.LatestError != "":
		check.Status = doctorWarn
		details = append(details, "latest run failed: "+diagnostic.LatestError)
	}
	if diagnostic.DeferredReason != "" {
		details = append(details, "deferred="+diagnostic.DeferredReason)
	}
	if counts := doctorAdmissionCounts(diagnostic.Skipped); counts != "" {
		details = append(details, "skipped="+counts)
	}
	if counts := doctorAdmissionCounts(diagnostic.Truncated); counts != "" {
		details = append(details, "truncated="+counts)
	}
	if len(diagnostic.IssueReferences) > 0 {
		details = append(details, "issues="+strings.Join(diagnostic.IssueReferences, ","))
	}
	if len(diagnostic.Warnings) > 0 {
		check.Status = doctorWarn
		details = append(details, diagnostic.Warnings...)
	}
	check.Detail = strings.Join(details, "; ")
	return check
}

func doctorAdmissionCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for key, value := range counts {
		if strings.TrimSpace(key) != "" && value > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, fmt.Sprintf("%s:%d", key, counts[key]))
	}
	return strings.Join(values, ",")
}

func decodeDoctorJSON(raw string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	return decoder.Decode(target)
}

func strconvQuote(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}
