package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type HealthScheduledImportSource struct {
	BundlePath     string
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	Objects        attachment.DetailStore
	DryRun         bool
}

func ImportHealthScheduledReports(ctx context.Context, target *postgres.Repository, source HealthScheduledImportSource) (int64, error) {
	if target == nil || source.BundlePath == "" || source.OrganizationID == "" || source.ProjectID == "" || source.Objects == nil {
		return 0, postgres.ErrNeutralImport
	}
	var imported int64
	err := postgres.ReadNeutralImportBundle(ctx, source.BundlePath, source.OrganizationID, func(bundle *postgres.VerifiedNeutralImport) error {
		if err := postgres.ValidateNeutralHealthScheduledSource(bundle); err != nil {
			return err
		}
		reports, err := prepareHealthScheduledImport(ctx, newCoreNeutralImport(bundle), source)
		if err != nil {
			return err
		}
		if err := target.ReadTenant(ctx, source.OrganizationID, corestore.Background, func(r tenantstore.Reader) error {
			project, err := r.Project(ctx, source.ProjectID)
			if err != nil {
				return err
			}
			if project.DeletedAt != nil || project.Profile != "native" {
				return corestore.ErrNotFound
			}
			return nil
		}); err != nil {
			return err
		}
		if source.DryRun {
			imported = int64(len(reports))
			return nil
		}
		if err := target.PinVerifiedNeutralImport(ctx, bundle, source.Objects); err != nil {
			return err
		}
		for _, report := range reports {
			report.RecordedAt = report.RecordedAt.UTC().Truncate(time.Microsecond)
			if err := target.WriteTenant(ctx, source.OrganizationID, corestore.Background, func(w tenantstore.Writer) error {
				owner, ok := w.(healthstore.ScheduledWriter)
				if !ok {
					return workItemStorageUnavailable()
				}
				retained, err := owner.RecordHealthScheduledReport(ctx, report)
				if err == nil && !retained.RecordedAt.Equal(report.RecordedAt) {
					return corestore.ErrRevisionConflict
				}
				return err
			}); err != nil {
				return fmt.Errorf("import health scheduled report: %w", err)
			}
			imported++
		}
		return nil
	})
	return imported, err
}

func prepareHealthScheduledImport(ctx context.Context, input *coreNeutralImport, source HealthScheduledImportSource) ([]healthstore.ScheduledReport, error) {
	project, err := input.one(ctx, "projects", map[string]string{"organization_id": string(source.OrganizationID), "id": string(source.ProjectID)}, []string{"id"})
	if err != nil {
		return nil, err
	}
	var repository string
	var repositoryID sql.NullInt64
	if err := project.Decode(map[string]any{"checkout_repository": &repository, "repository_id": &repositoryID}); err != nil {
		return nil, err
	}
	if repository == "" && repositoryID.Valid {
		row, err := input.one(ctx, "repositories", map[string]string{"id": strconv.FormatInt(repositoryID.Int64, 10)}, []string{"id"})
		if err != nil {
			return nil, err
		}
		var owner, name string
		if err := row.Decode(map[string]any{"github_owner": &owner, "github_name": &name}); err != nil {
			return nil, err
		}
		repository = owner + "/" + name
	}
	reports := map[string]healthstore.ScheduledReport{}
	persisted := map[string]bool{}
	for _, table := range []string{"scheduled_validation_reports", "issues", "native_comments"} {
		var after []string
		for {
			rows, err := input.bundle.Page(ctx, table, after, 100)
			if err != nil {
				return nil, err
			}
			if len(rows) == 0 {
				break
			}
			for _, row := range rows {
				var organization tracker.OrganizationID
				var project tracker.ProjectID
				if err := row.Decode(map[string]any{"organization_id": &organization, "project_id": &project}); err != nil {
					return nil, err
				}
				if organization != source.OrganizationID {
					return nil, corestore.ErrTenantMismatch
				}
				if project != source.ProjectID {
					continue
				}
				if table == "scheduled_validation_reports" {
					report, err := prepareHealthStoredScheduledReport(row, source)
					if err != nil {
						return nil, err
					}
					if previous, exists := reports[report.RunURL]; exists && previous != report {
						return nil, corestore.ErrRevisionConflict
					}
					reports[report.RunURL], persisted[report.RunURL] = report, true
					continue
				}
				legacy, err := prepareHealthLegacyScheduledReports(row, source, repository, table == "native_comments")
				if err != nil {
					return nil, err
				}
				for _, report := range legacy {
					previous, exists := reports[report.RunURL]
					if !persisted[report.RunURL] && (!exists || report.RecordedAt.Before(previous.RecordedAt)) {
						reports[report.RunURL] = report
					}
				}
			}
			after = rows[len(rows)-1].Key
		}
	}
	ordered := make([]healthstore.ScheduledReport, 0, len(reports))
	for _, report := range reports {
		ordered = append(ordered, report)
	}
	slices.SortFunc(ordered, func(a, b healthstore.ScheduledReport) int { return strings.Compare(a.RunURL, b.RunURL) })
	return ordered, nil
}

func prepareHealthStoredScheduledReport(row postgres.NeutralImportRow, source HealthScheduledImportSource) (healthstore.ScheduledReport, error) {
	report := healthstore.ScheduledReport{OrganizationID: source.OrganizationID, ProjectID: source.ProjectID}
	var raw, at, repository, runID, runAttempt string
	var conclusion sql.NullString
	if err := row.Decode(map[string]any{"record_json": &raw, "recorded_at": &at, "repository": &repository, "run_id": &runID, "run_attempt": &runAttempt, "conclusion": &conclusion}); err != nil {
		return report, err
	}
	if err := json.Unmarshal([]byte(raw), &report.ScheduledValidationReport); err != nil {
		return report, postgres.ErrNeutralImport
	}
	if conclusion.Valid {
		report.Conclusion = conclusion.String
	}
	var err error
	report.RecordedAt, err = parseTimeValue(at)
	if err != nil || report.RecordedAt.IsZero() || !report.Valid() || report.Repository != repository || report.RunID != runID || report.RunAttempt != runAttempt {
		return report, postgres.ErrNeutralImport
	}
	return report, nil
}

func prepareHealthLegacyScheduledReports(row postgres.NeutralImportRow, source HealthScheduledImportSource, repository string, includePlainGreen bool) ([]healthstore.ScheduledReport, error) {
	var body, rawActor, at string
	var actorKind sql.NullString
	if err := row.Decode(map[string]any{"body": &body, "created_at": &at, "actor_kind": &actorKind, "actor_json": &rawActor}); err != nil {
		return nil, err
	}
	if !strings.Contains(body, "detent-checks") && (!includePlainGreen || !strings.HasPrefix(body, "Scheduled full validation passed for development commit ")) {
		return nil, nil
	}
	if !actorKind.Valid {
		var actor tracker.Actor
		if err := json.Unmarshal([]byte(rawActor), &actor); err != nil {
			return nil, postgres.ErrNeutralImport
		}
		actorKind.String = actor.Kind
	}
	if actorKind.String == "runner" {
		return nil, nil
	}
	recorded, err := parseTimeValue(at)
	if err != nil || recorded.IsZero() {
		return nil, postgres.ErrNeutralImport
	}
	var reports []healthstore.ScheduledReport
	add := func(report gate.ScheduledValidationReport) {
		reports = append(reports, healthstore.ScheduledReport{OrganizationID: source.OrganizationID, ProjectID: source.ProjectID, ScheduledValidationReport: report, RecordedAt: recorded})
	}
	origin, stamped := issueorigin.Parse(body)
	for _, evidence := range gate.ParseScheduledEvidence(body) {
		if stamped && origin.Kind == "doctor" && origin.Instance == "github-actions" && scheduledRepairOriginMatches(origin.Source, evidence) && strings.EqualFold(repository, evidence.Repository) {
			add(gate.ScheduledValidationReport{Repository: evidence.Repository, RunID: evidence.RunID, RunAttempt: evidence.RunAttempt, RunURL: evidence.RunURL, HeadSHA: evidence.HeadSHA, Conclusion: "failure"})
		}
	}
	if strings.HasPrefix(body, "Scheduled full validation passed for development commit `") {
		sha, _, _ := strings.Cut(strings.TrimPrefix(body, "Scheduled full validation passed for development commit `"), "`")
		_, rest, found := strings.Cut(body, "\n\nRun: ")
		runURL, _, _ := strings.Cut(rest, "\n")
		identity := strings.TrimPrefix(runURL, "https://github.com/"+repository+"/actions/runs/")
		runID, attempt, _ := strings.Cut(identity, "/attempts/")
		report := gate.ScheduledValidationReport{Repository: repository, RunID: runID, RunAttempt: attempt, RunURL: runURL, HeadSHA: sha, Conclusion: "success"}
		if found && report.Valid() {
			add(report)
		}
	}
	for _, report := range reports {
		if !report.Valid() {
			return nil, errors.New("invalid historical scheduled evidence")
		}
	}
	return reports, nil
}
