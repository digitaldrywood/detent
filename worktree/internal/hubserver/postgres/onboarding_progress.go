package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ProjectOnboardingProgress(ctx context.Context, project tracker.ProjectID) (onboarding.Progress, error) {
	var result onboarding.Progress
	err := s.db.QueryRow(ctx, `SELECT COALESCE(o.revision,0),COALESCE(o.repository,''),COALESCE(o.doctor,false),COALESCE(o.provider,false),COALESCE(o.artifacts,''),COALESCE(o.updated_at,'')
 FROM projects p LEFT JOIN project_onboarding o ON o.organization_id=p.organization_id AND o.project_id=p.id
 WHERE p.organization_id=$1 AND p.id=$2`, s.organization, project).Scan(&result.Revision, &result.Repository, &result.Doctor, &result.Provider, &result.Artifacts, &result.UpdatedAt)
	return result, mapError(ctx, err)
}

func (s *session) SaveProjectOnboardingProgress(ctx context.Context, project tracker.ProjectID, progress onboarding.Progress, expected tracker.Revision) error {
	if project == "" || expected < 0 || progress.Revision != expected+1 || progress.Validate() != nil || progress.UpdatedAt == "" {
		return projectstore.ErrInvalidOnboardingProgress
	}
	if _, err := time.Parse(time.RFC3339Nano, progress.UpdatedAt); err != nil {
		return projectstore.ErrInvalidOnboardingProgress
	}
	if _, err := s.queries.LockActiveProject(ctx, pgquery.LockActiveProjectParams{OrganizationID: s.organization, ID: project}); err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO project_onboarding(organization_id,project_id,revision,repository,doctor,provider,artifacts,updated_at) VALUES($1,$2,0,'',false,false,'','') ON CONFLICT(organization_id,project_id) DO NOTHING`, s.organization, project); err != nil {
		return mapError(ctx, err)
	}
	result, err := s.db.Exec(ctx, `UPDATE project_onboarding SET revision=$3,repository=$4,doctor=$5,provider=$6,artifacts=$7,updated_at=$8 WHERE organization_id=$1 AND project_id=$2 AND revision=$9`, s.organization, project, progress.Revision, progress.Repository, progress.Doctor, progress.Provider, progress.Artifacts, progress.UpdatedAt, expected)
	if err != nil {
		return mapError(ctx, err)
	}
	changed := result.RowsAffected()
	if changed != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

type OnboardingProgressImport struct {
	ProjectID tracker.ProjectID   `json:"project_id"`
	Progress  onboarding.Progress `json:"progress"`
}

func (r *Repository) ImportOnboardingProgress(ctx context.Context, organization tracker.OrganizationID, records []OnboardingProgressImport) error {
	if organization == "" || len(records) > corestore.MaxPageSize {
		return projectstore.ErrInvalidOnboardingProgress
	}
	for _, record := range records {
		progress := record.Progress
		if record.ProjectID == "" || progress.Revision <= 0 || progress.Validate() != nil {
			return projectstore.ErrInvalidOnboardingProgress
		}
		if _, err := time.Parse(time.RFC3339Nano, progress.UpdatedAt); err != nil {
			return projectstore.ErrInvalidOnboardingProgress
		}
		if err := r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
			return s.importOnboardingProgress(ctx, record)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) importOnboardingProgress(ctx context.Context, record OnboardingProgressImport) error {
	progress := record.Progress
	if _, err := s.db.Exec(ctx, `INSERT INTO project_onboarding(organization_id,project_id,revision,repository,doctor,provider,artifacts,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(organization_id,project_id) DO NOTHING`, s.organization, record.ProjectID, progress.Revision, progress.Repository, progress.Doctor, progress.Provider, progress.Artifacts, progress.UpdatedAt); err != nil {
		return mapError(ctx, err)
	}
	current, err := s.ProjectOnboardingProgress(ctx, record.ProjectID)
	if err != nil {
		return err
	}
	if current != progress {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (r *Repository) ImportCoreOnboardingProgress(ctx context.Context, source CoreProjectImportSource) (int64, error) {
	if source.CopyPath != "" || source.BundlePath == "" || source.OrganizationID == "" || source.Objects == nil {
		return 0, neutralimport.ErrInvalid
	}
	var count int64
	err := neutralimport.Read(ctx, source.BundlePath, source.OrganizationID, func(bundle *neutralimport.Bundle) error {
		if _, present := bundle.Schema()["project_onboarding"]; !present {
			return nil
		}
		if err := neutralimport.ValidateCoreTables(bundle, "project_onboarding"); err != nil {
			return err
		}
		if err := r.PinVerifiedNeutralImport(ctx, bundle, source.Objects); err != nil {
			return err
		}
		var after []string
		for {
			rows, err := bundle.Page(ctx, "project_onboarding", after, 100)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				return nil
			}
			for _, row := range rows {
				var organization tracker.OrganizationID
				var record OnboardingProgressImport
				var revision int64
				var raw, updated string
				if err := row.Decode(map[string]any{"organization_id": &organization, "project_id": &record.ProjectID, "revision": &revision, "progress_json": &raw, "updated_at": &updated}); err != nil {
					return err
				}
				after = []string{string(organization), string(record.ProjectID)}
				if organization != source.OrganizationID {
					continue
				}
				if record.ProjectID == "" || revision <= 0 || !json.Valid([]byte(raw)) {
					return projectstore.ErrInvalidOnboardingProgress
				}
				var decoded struct {
					onboarding.Progress
					Revision json.RawMessage `json:"revision"`
				}
				if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
					return err
				}
				record.Progress = decoded.Progress
				if encoded := strings.TrimSpace(string(decoded.Revision)); encoded != "" && encoded != "null" {
					if strings.HasPrefix(encoded, "\"") {
						var value string
						if err := json.Unmarshal(decoded.Revision, &value); err != nil {
							return err
						}
						encoded = value
					}
					supplied, err := strconv.ParseInt(encoded, 10, 64)
					if err != nil || supplied != 0 && supplied != revision {
						return corestore.ErrRevisionConflict
					}
				}
				if record.Progress.Revision != 0 && int64(record.Progress.Revision) != revision || record.Progress.UpdatedAt != "" && record.Progress.UpdatedAt != updated {
					return corestore.ErrRevisionConflict
				}
				record.Progress.Revision, record.Progress.UpdatedAt = tracker.Revision(revision), updated
				if record.Progress.Validate() != nil {
					return projectstore.ErrInvalidOnboardingProgress
				}
				if _, err := time.Parse(time.RFC3339Nano, updated); err != nil {
					return projectstore.ErrInvalidOnboardingProgress
				}
				payload, err := json.Marshal(struct {
					SourceSHA256 string
					ProjectID    tracker.ProjectID
					Revision     int64
					UpdatedAt    string
					ProgressJSON string
				}{bundle.Identity().SourceSHA256, record.ProjectID, revision, updated, raw})
				if err != nil {
					return err
				}
				receipt, err := eventstore.NewContent(source.Objects).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: organization, ActorID: "production-import", Operation: "core-onboarding", CommandKey: string(record.ProjectID), RequestHash: artifact.Digest(payload)}, payload)
				if err != nil {
					return err
				}
				if err := r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
					if err := s.importOnboardingProgress(ctx, record); err != nil {
						return err
					}
					_, err := s.PutReceipt(ctx, receipt)
					return err
				}); err != nil {
					return err
				}
				count++
			}
		}
	})
	return count, err
}

func (s *session) ProjectLatestAttemptStatus(ctx context.Context, project tracker.ProjectID) (string, error) {
	var status string
	err := s.db.QueryRow(ctx, `SELECT a.status FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE a.organization_id=$1 AND i.project_id=$2 ORDER BY a.fencing_token DESC LIMIT 1`, s.organization, project).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return status, mapError(ctx, err)
}
