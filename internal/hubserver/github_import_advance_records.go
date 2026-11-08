package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) advanceGitHubImportRecord(ctx context.Context, scope nativeScope, id string, revision tracker.Revision, key string) (json.RawMessage, error) {
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	input := struct {
		ExpectedRevision tracker.Revision `json:"expected_revision,string"`
	}{revision}
	operation := projectOperationID(scope, "POST", "/imports/"+id+"/advance")
	hash, err := nativeCommandHash(input)
	if err != nil {
		return nil, err
	}
	if raw, found, err := s.authorizedChangeRecordReceipt(ctx, scope, operation, key, hash); found || err != nil {
		return raw, err
	}
	if s.config.ImportBackend == nil {
		return nil, nativePublicInvalid("GitHub import transport is not configured")
	}
	current, err := s.readGitHubImportRecord(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if current.Revision != revision {
		return nil, nativeConflict(current.Revision)
	}
	if current.Stage == "finished" {
		return json.Marshal(current)
	}
	if current.RetryAfter != nil && s.config.now().Before(*current.RetryAfter) {
		return nil, &nativeError{Code: "invalid_request", Message: "Import retry is not ready", status: http.StatusTooManyRequests, publicMessage: true}
	}
	project, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return nil, err
	}
	integration := projectIntegrationRecord(project)
	if !integration.ManualImportEnabled || integration.Repository == "" {
		return nil, nativePublicInvalid("Manual intake is disabled")
	}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		return s.database.requireHostedFeatureRecord(ctx, r, "github_integration", s.config.now())
	}); err != nil {
		return nil, err
	}
	page, fetchErr := s.fetchGitHubImportRecordPage(ctx, scope, integration, current)
	next := current
	next.Revision++
	next.ObservedAt = formatHubTime(s.config.now())
	var records []projectstore.GitHubImportRecord
	if fetchErr != nil {
		retry := s.config.now().Add(time.Minute)
		if page.RetryAt.After(retry) {
			retry = page.RetryAt
		}
		next.Status, next.LastError, next.RetryAfter = "partial", "GitHub source page is unavailable", &retry
	} else {
		if len(page.Records) > 100 || page.NextCursor != "" && page.NextCursor == current.Cursor {
			return nil, nativePublicInvalid("GitHub import page did not advance within its budget")
		}
		intakeScope := scope
		intakeScope.integrationRepository = integration.Repository
		intakeScope.githubIntakeRevision = &githubIntakeRevision{importID: current.ID, revision: int64(current.Revision)}
		intakeScope.sourceActor = nativeIntegrationActor("github")
		if current.Stage == "issue" {
			if page.Issue == nil || page.Issue.Number != current.IssueNumber {
				return nil, nativePublicInvalid("GitHub import returned the wrong issue")
			}
			source := *page.Issue
			snapshot := linkedSnapshotFromSource(source, s.config.now())
			issue, err := s.importGitHubSnapshotRecord(ctx, intakeScope, fmt.Sprintf("import:%s:%d", id, revision), tracker.GitHubIssuePreview{Number: source.Number, URL: source.URL, ID: source.NodeID}, snapshot, "Triage", nil)
			if err != nil {
				return nil, err
			}
			next.WorkItemID = string(issue.WorkItemID)
			next.SourceUpdatedAt = formatHubTime(source.UpdatedAt)
		} else if current.Stage == "comments" && len(page.Records) > 0 {
			var linked eventstore.LinkedSource
			err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				linked, err = r.LinkedSource(ctx, scope.project, tracker.NativeWorkItemID(current.WorkItemID))
				return err
			})
			if err != nil {
				return nil, err
			}
			snapshot, err := eventstore.NewContent(s.runDetailStore()).LinkedIssueSnapshot(ctx, scope.organization, eventstore.Reference(linked.SnapshotObjectKey, linked.SnapshotSHA256, linked.SnapshotBytes, linked.SnapshotContentBytes))
			if err != nil {
				return nil, err
			}
			for _, record := range page.Records {
				if record.Kind == "comment" && record.Body != "" {
					snapshot.Comments = append(snapshot.Comments, tracker.GitHubIssueComment{Body: record.Body, Provenance: record.Provenance})
				}
			}
			if _, err = s.applyLinkedIssueRecord(ctx, intakeScope, tracker.NativeWorkItemID(current.WorkItemID), "github.import.comments", tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("import:%s:%d", id, revision)}, Snapshot: snapshot}); err != nil {
				return nil, err
			}
		}
		for _, record := range page.Records {
			if record.SourceKey == "" || !json.Valid(record.Data) {
				return nil, nativePublicInvalid("GitHub import record is incomplete")
			}
			raw, err := json.Marshal(record)
			if err != nil {
				return nil, err
			}
			ref, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization}, raw)
			if err != nil {
				return nil, err
			}
			records = append(records, projectstore.GitHubImportRecord{OrganizationID: scope.organization, ProjectID: scope.project, ImportID: id, SourceKey: record.SourceKey, Kind: record.Kind, CurrentDependency: true, DependencyID: record.DependencyID, ProvenanceExternalID: record.Provenance.ExternalID, Payload: ref})
		}
		next.Pages++
		next.Cursor = page.NextCursor
		next.Status, next.LastError, next.RetryAfter = "pending", "", nil
		for _, gap := range page.Gaps {
			if !slices.Contains(next.Gaps, gap) {
				next.Gaps = append(next.Gaps, gap)
			}
		}
		if page.NextCursor == "" {
			switch current.Stage {
			case "issue":
				next.Stage = "comments"
			case "comments":
				next.Stage = "timeline"
			case "timeline":
				next.Stage = "dependencies"
			case "dependencies":
				next.Stage = "edits"
			case "edits":
				if page.EditsFinished {
					next.Stage = "finished"
				}
			}
			if page.EditSequence != 0 {
				next.EditSequence = page.EditSequence
			}
		}
		if next.Stage == "finished" {
			next.Status = "retrieved"
		}
	}
	return s.tenantReceiptMutation(ctx, scope, operation, tracker.Mutation{IdempotencyKey: key}, input, next, func(w tenantstore.Writer) error {
		if err := s.database.requireHostedFeatureRecord(ctx, w, "github_integration", s.config.now()); err != nil {
			return err
		}
		latestProject, err := w.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if latestProject.Settings.Revision != project.Settings.Revision {
			return corestore.ErrRevisionConflict
		}
		owner, ok := w.(projectstore.GitHubImportWriter)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		latest, err := owner.GitHubImport(ctx, scope.project, id)
		if err != nil {
			return err
		}
		if latest.Revision != int64(revision) {
			return nativeConflict(tracker.Revision(latest.Revision))
		}
		latest.WorkItemID, latest.Stage, latest.Cursor, latest.Pages, latest.Status, latest.Gaps, latest.LastError, latest.RetryAfter, latest.ObservedAt, latest.EditSequence = next.WorkItemID, next.Stage, next.Cursor, int64(next.Pages), next.Status, next.Gaps, next.LastError, next.RetryAfter, s.config.now(), next.EditSequence
		if next.SourceUpdatedAt != "" {
			value, err := parseTimeValue(next.SourceUpdatedAt)
			if err != nil {
				return err
			}
			latest.SourceUpdatedAt = &value
		}
		for _, record := range records {
			gitHubImportRecordWriter, ok := w.(projectstore.GitHubImportRecordWriter)
			if !ok {
				return ErrPostgresFeatureUnavailable
			}
			if err := gitHubImportRecordWriter.CreateGitHubImportRecord(ctx, record); err != nil {
				return err
			}
		}
		_, err = owner.UpdateGitHubImport(ctx, latest)
		return err
	})
}

func (s *Service) fetchGitHubImportRecordPage(ctx context.Context, scope nativeScope, integration ProjectIntegration, job GitHubImport) (GitHubImportPage, error) {
	request := GitHubImportRequest{Repository: integration.Repository, Profile: integration.Profile, IssueNumber: job.IssueNumber, Stage: job.Stage, Cursor: job.Cursor}
	var sequence int64
	if job.Stage == "edits" {
		var source projectstore.GitHubImportRecord
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			gitHubImportRecordReader, ok := r.(projectstore.GitHubImportRecordReader)
			if !ok {
				return ErrPostgresFeatureUnavailable
			}
			source, err = gitHubImportRecordReader.GitHubImportEditSource(ctx, scope.project, job.ID, job.EditSequence)
			return err
		})
		if errors.Is(err, corestore.ErrNotFound) {
			return GitHubImportPage{EditsFinished: true}, nil
		}
		if err != nil {
			return GitHubImportPage{}, err
		}
		request.SourceID, sequence = source.ProvenanceExternalID, source.Sequence
		if strings.TrimSpace(request.SourceID) == "" {
			return GitHubImportPage{EditSequence: sequence, Gaps: []string{"Source node ID was unavailable for an edit-history lookup"}}, nil
		}
	}
	page, err := s.config.ImportBackend.FetchImportPage(ctx, request)
	page.EditSequence = sequence
	return page, err
}
