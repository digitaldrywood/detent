package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/store"
)

type doctorParkReviewDiagnostic struct {
	ProjectID                string `json:"project_id"`
	IssueID                  string `json:"issue_id,omitempty"`
	Identifier               string `json:"identifier,omitempty"`
	IssueURL                 string `json:"issue_url,omitempty"`
	ParkCount                int64  `json:"park_count"`
	AcknowledgedParkSequence int64  `json:"acknowledged_park_sequence,omitempty"`
}

func checkDoctorParkReview(ctx context.Context, projectID, storePath string, threshold int, deps doctorDeps) doctorCheck {
	name := "Project " + projectID + " park review"
	if threshold <= 0 {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "park review is disabled because its threshold is not positive"}
	}
	if strings.TrimSpace(storePath) == "" {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "no park history recorded yet"}
	}
	deps = deps.withDefaults()
	db, err := deps.openSQLiteReadOnly(ctx, storePath)
	if err != nil {
		if errors.Is(err, errDoctorTelemetryStoreUnavailable) {
			return doctorCheck{Name: name, Status: doctorOK, Detail: "no park history recorded yet"}
		}
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("park history could not be read: %v", err)}
	}
	summaries, queryErr := store.QueryParkSummaries(ctx, db, projectID)
	closeErr := db.Close()
	if queryErr != nil {
		if strings.Contains(strings.ToLower(queryErr.Error()), "no such table") {
			return doctorCheck{Name: name, Status: doctorOK, Detail: "no park history recorded yet"}
		}
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("park history query failed: %v", queryErr)}
	}
	if closeErr != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("park history close failed: %v", closeErr)}
	}
	diagnostics := make([]doctorParkReviewDiagnostic, 0)
	for _, summary := range summaries {
		if !summary.ReviewRecommended(threshold) {
			continue
		}
		diagnostics = append(diagnostics, doctorParkReviewDiagnostic{
			ProjectID:                summary.ProjectID,
			IssueID:                  summary.IssueID,
			Identifier:               summary.Identifier,
			IssueURL:                 summary.IssueURL,
			ParkCount:                summary.ParkCount,
			AcknowledgedParkSequence: summary.AcknowledgedParkSequence,
		})
	}
	if len(diagnostics) == 0 {
		return doctorCheck{Name: name, Status: doctorOK, Detail: fmt.Sprintf("no issues are review recommended at the %d-park threshold", threshold)}
	}
	details := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		details = append(details, fmt.Sprintf("%s (%d parks)", doctorParkReviewReference(diagnostic), diagnostic.ParkCount))
	}
	reference := doctorParkReviewReference(diagnostics[0])
	return doctorCheck{
		Name:        name,
		Status:      doctorWarn,
		Detail:      fmt.Sprintf("%d issue(s) are review recommended after at least %d lifetime parks: %s", len(diagnostics), threshold, strings.Join(details, "; ")),
		Hint:        fmt.Sprintf("Inspect with detent issue %q --explain --project %q; acknowledge with detent issue %q --acknowledge-parks --project %q.", reference, projectID, reference, projectID),
		ParkReviews: diagnostics,
	}
}

func doctorParkReviewReference(diagnostic doctorParkReviewDiagnostic) string {
	for _, candidate := range []string{diagnostic.Identifier, diagnostic.IssueID, diagnostic.IssueURL} {
		if value := strings.TrimSpace(candidate); value != "" {
			return value
		}
	}
	return "unknown issue"
}
