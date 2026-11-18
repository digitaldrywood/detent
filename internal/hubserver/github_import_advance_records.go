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

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) advanceGitHubImportRecord(ctx context.Context, scope hubscope.NativeScope, id string, revision tracker.Revision, key string) (json.RawMessage, error) {
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	input := struct {
		ExpectedRevision tracker.Revision `json:"expected_revision,string"`
	}{revision}
	operation := projectOperationID(scope, "POST", "/imports/"+id+"/advance")
	hash, err := hubapi.CommandHash(input)
	if err != nil {
		return nil, err
	}
	if raw, found, err := s.authorizedChangeRecordReceipt(ctx, scope, operation, key, hash); found || err != nil {
		return raw, err
	}
	if s.config.ImportBackend == nil {
		return nil, hubapi.NativePublicInvalid("GitHub import transport is not configured")
	}
	current, err := s.readGitHubImportRecord(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if current.Revision != revision {
		return nil, hubapi.NativeConflict(current.Revision)
	}
	if current.Stage == "finished" {
		return json.Marshal(current)
	}
	if current.RetryAfter != nil && s.config.Clock().Before(*current.RetryAfter) {
		return nil, &hubapi.NativeError{Code: "invalid_request", Message: "Import retry is not ready", Status: http.StatusTooManyRequests, PublicMessage: true}
	}
	project, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return nil, err
	}
	integration := projectIntegrationRecord(project)
	if !integration.ManualImportEnabled || integration.Repository == "" {
		return nil, hubapi.NativePublicInvalid("Manual intake is disabled")
	}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		return hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, r, "github_integration", s.config.Clock()))
	}); err != nil {
		return nil, err
	}
	page, fetchErr := s.fetchGitHubImportRecordPage(ctx, scope, integration, current)
	next := current
	next.Revision++
	next.ObservedAt = hubapi.FormatHubTime(s.config.Clock())
	var records []projectstore.GitHubImportRecord
	var historical *historicalGitHubImportPlan
	if fetchErr != nil {
		retry := s.config.Clock().Add(time.Minute)
		if page.RetryAt.After(retry) {
			retry = page.RetryAt
		}
		next.Status, next.LastError, next.RetryAfter = "partial", "GitHub source page is unavailable", &retry
	} else {
		if len(page.Records) > 100 || page.NextCursor != "" && page.NextCursor == current.Cursor {
			return nil, hubapi.NativePublicInvalid("GitHub import page did not advance within its budget")
		}
		intakeScope := scope
		intakeScope.IntegrationRepository = integration.Repository
		intakeScope.GitHubIntakeRevision = &hubscope.GitHubIntakeRevision{ImportID: current.ID, Revision: int64(current.Revision)}
		intakeScope.SourceActor = nativeIntegrationActor("github")
		historicalSource := project.Core.Profile == "github_compatible"
		if !historicalSource && current.WorkItemID != "" {
			err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				_, err := r.LinkedSource(ctx, scope.Project, tracker.NativeWorkItemID(current.WorkItemID))
				return err
			})
			if errors.Is(err, corestore.ErrNotFound) {
				historicalSource = true
			} else if err != nil {
				return nil, err
			}
		}
		if historicalSource {
			plan, err := s.prepareHistoricalGitHubImport(ctx, intakeScope, project, current, page)
			if err != nil {
				return nil, err
			}
			historical = &plan
			next.WorkItemID = string(plan.item.NativeID)
			if current.Stage == "issue" {
				if page.Issue == nil || page.Issue.Number != current.IssueNumber {
					return nil, hubapi.NativePublicInvalid("GitHub import returned the wrong issue")
				}
				next.SourceUpdatedAt = hubapi.FormatHubTime(page.Issue.UpdatedAt)
			}
		} else if current.Stage == "issue" {
			if page.Issue == nil || page.Issue.Number != current.IssueNumber {
				return nil, hubapi.NativePublicInvalid("GitHub import returned the wrong issue")
			}
			source := *page.Issue
			snapshot := linkedSnapshotFromSource(source, s.config.Clock())
			issue, err := s.importGitHubSnapshotRecord(ctx, intakeScope, fmt.Sprintf("import:%s:%d", id, revision), tracker.GitHubIssuePreview{Number: source.Number, URL: source.URL, ID: source.NodeID}, snapshot, "Triage", nil)
			if err != nil {
				return nil, err
			}
			next.WorkItemID = string(issue.WorkItemID)
			next.SourceUpdatedAt = hubapi.FormatHubTime(source.UpdatedAt)
		} else if current.Stage == "comments" && len(page.Records) > 0 {
			var linked eventstore.LinkedSource
			err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				linked, err = r.LinkedSource(ctx, scope.Project, tracker.NativeWorkItemID(current.WorkItemID))
				return err
			})
			if err != nil {
				return nil, err
			}
			snapshot, err := eventstore.NewContent(s.runDetailStore()).LinkedIssueSnapshot(ctx, scope.Organization, eventstore.Reference(linked.SnapshotObjectKey, linked.SnapshotSHA256, linked.SnapshotBytes, linked.SnapshotContentBytes))
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
				return nil, hubapi.NativePublicInvalid("GitHub import record is incomplete")
			}
			raw, err := json.Marshal(record)
			if err != nil {
				return nil, err
			}
			ref, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization}, raw)
			if err != nil {
				return nil, err
			}
			records = append(records, projectstore.GitHubImportRecord{OrganizationID: scope.Organization, ProjectID: scope.Project, ImportID: id, SourceKey: record.SourceKey, Kind: record.Kind, CurrentDependency: record.Kind == "dependency", DependencyID: record.DependencyID, ProvenanceExternalID: record.Provenance.ExternalID, Payload: ref})
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
		if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, w, "github_integration", s.config.Clock())); err != nil {
			return err
		}
		version, err := w.ProjectVersion(ctx, scope.Project)
		if err != nil {
			return err
		}
		if version.SettingsRevision != project.Settings.Revision {
			return corestore.ErrRevisionConflict
		}
		owner, ok := w.(projectstore.GitHubImportWriter)
		if !ok {
			return hubapi.ErrPostgresFeatureUnavailable
		}
		latest, err := owner.GitHubImport(ctx, scope.Project, id)
		if err != nil {
			return err
		}
		if latest.Revision != int64(revision) {
			return hubapi.NativeConflict(tracker.Revision(latest.Revision))
		}
		if historical != nil {
			if err := applyHistoricalGitHubImport(ctx, w, project, *historical); err != nil {
				return err
			}
		}
		latest.WorkItemID, latest.Stage, latest.Cursor, latest.Pages, latest.Status, latest.Gaps, latest.LastError, latest.RetryAfter, latest.ObservedAt, latest.EditSequence = next.WorkItemID, next.Stage, next.Cursor, int64(next.Pages), next.Status, next.Gaps, next.LastError, next.RetryAfter, s.config.Clock(), next.EditSequence
		if next.SourceUpdatedAt != "" {
			value, err := hubapi.ParseTimeValue(next.SourceUpdatedAt)
			if err != nil {
				return err
			}
			latest.SourceUpdatedAt = &value
		}
		if fetchErr == nil && current.Stage == "dependencies" && current.Cursor == "" {
			owner, ok := w.(projectstore.GitHubImportRecordWriter)
			if !ok {
				return hubapi.ErrPostgresFeatureUnavailable
			}
			if err := owner.ResetCurrentGitHubImportDependencies(ctx, scope.Project, id); err != nil {
				return err
			}
		}
		for _, record := range records {
			gitHubImportRecordWriter, ok := w.(projectstore.GitHubImportRecordWriter)
			if !ok {
				return hubapi.ErrPostgresFeatureUnavailable
			}
			if err := gitHubImportRecordWriter.CreateGitHubImportRecord(ctx, record); err != nil {
				return err
			}
		}
		_, err = owner.UpdateGitHubImport(ctx, latest)
		return err
	})
}

func (s *Service) fetchGitHubImportRecordPage(ctx context.Context, scope hubscope.NativeScope, integration ProjectIntegration, job GitHubImport) (GitHubImportPage, error) {
	request := GitHubImportRequest{Repository: integration.Repository, Profile: integration.Profile, IssueNumber: job.IssueNumber, Stage: job.Stage, Cursor: job.Cursor}
	var sequence int64
	if job.Stage == "edits" {
		var source projectstore.GitHubImportRecord
		err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			gitHubImportRecordReader, ok := r.(projectstore.GitHubImportRecordReader)
			if !ok {
				return hubapi.ErrPostgresFeatureUnavailable
			}
			source, err = gitHubImportRecordReader.GitHubImportEditSource(ctx, scope.Project, job.ID, job.EditSequence)
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

type historicalGitHubImportPlan struct {
	item       corestore.WorkItem
	issue      tracker.NativeIssue
	writeIssue bool
	create     bool
	fields     hubrecords.WorkItemRecordFields
	version    eventstore.CollaborationVersion
	event      eventstore.Event
	comments   []hubrecords.LinkedIssueCommentPlan
}

func (s *Service) prepareHistoricalGitHubImport(ctx context.Context, scope hubscope.NativeScope, project projectstore.Record, job GitHubImport, page GitHubImportPage) (historicalGitHubImportPlan, error) {
	var plan historicalGitHubImportPlan
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		if job.WorkItemID != "" {
			plan.item, err = r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(job.WorkItemID))
		} else if page.Issue != nil {
			plan.item, err = r.WorkItemByProvenance(ctx, scope.Project, "github", page.Issue.NodeID)
		} else {
			return corestore.ErrNotFound
		}
		if errors.Is(err, corestore.ErrNotFound) && job.Stage == "issue" && project.Core.Profile == "github_compatible" {
			number, err := r.NextWorkItemNumber(ctx, scope.Project)
			if err != nil {
				return err
			}
			plan.create = true
			plan.item = corestore.WorkItem{OrganizationID: scope.Organization, ProjectID: scope.Project, NativeID: tracker.NativeWorkItemID(hubapi.NewID("wi")), Number: number}
			return nil
		}
		return err
	})
	if err != nil {
		return plan, err
	}
	if !plan.create {
		_, version, err := s.readWorkItemRecord(ctx, scope, string(plan.item.NativeID))
		if err != nil {
			return plan, err
		}
		plan.issue, err = s.hydrateCanonicalWorkItemRecord(ctx, plan.item, version)
		if err != nil {
			return plan, err
		}
	}
	now := s.config.Clock()
	content := eventstore.NewContent(s.runDetailStore())
	if job.Stage == "issue" && !plan.create && (page.Issue == nil || plan.issue.Provenance == nil || plan.issue.Provenance.Provider != "github" || plan.issue.Provenance.ExternalID != page.Issue.NodeID) {
		return plan, eventstore.ErrInvalidRecord
	}
	if job.Stage == "issue" && project.Core.Profile == "github_compatible" {
		if page.Issue == nil || page.Issue.Number != job.IssueNumber {
			return plan, hubapi.NativePublicInvalid("GitHub import returned the wrong issue")
		}
		source := *page.Issue
		canonical, repository, number, err := tracker.ParseGitHubIssueURL(source.URL)
		if err != nil || !strings.EqualFold(repository, project.Settings.Repository) || number != source.Number || source.NodeID == "" {
			return plan, hubapi.NativePublicInvalid("GitHub source does not match its repository")
		}
		snapshot := linkedSnapshotFromSource(source, now)
		if err := validateLinkedIssueSnapshot(snapshot); err != nil {
			return plan, err
		}
		if plan.create {
			plan.issue = tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: plan.item.NativeID, Number: int(plan.item.Number), Profile: "github_compatible"}, CreatedAt: source.CreatedAt, Actor: *nativeIntegrationActor("github"), Dependencies: []tracker.NativeWorkItemID{}, Blockers: []tracker.NativeDependency{}, Dependents: []tracker.NativeDependency{}, DependencyReady: true}
			if plan.issue.CreatedAt.IsZero() {
				plan.issue.CreatedAt = now
			}
		}
		if !plan.create && (plan.issue.Provenance == nil || plan.issue.Provenance.Provider != "github" || plan.issue.Provenance.ExternalID != source.NodeID) {
			return plan, eventstore.ErrInvalidRecord
		}
		plan.issue.Revision++
		plan.issue.Title, plan.issue.Body = source.Title, source.Body
		plan.issue.Labels, plan.issue.Assignees = slices.Clone(source.Labels), slices.Clone(source.Assignees)
		plan.issue.Provenance = &snapshot.Provenance
		plan.issue.ExternalReferences = []tracker.ExternalReference{tracker.GitHubIssueSourceReference(canonical, canonical)}
		plan.issue.UpdatedAt = source.UpdatedAt
		if plan.issue.LastActivityAt.Before(source.UpdatedAt) {
			plan.issue.LastActivityAt = source.UpdatedAt
		}
		plan.issue.Terminal = source.State == "closed"
		plan.issue.ClosedAt = nil
		if plan.issue.Terminal {
			closed := source.UpdatedAt
			plan.issue.ClosedAt = &closed
		}
		plan.writeIssue = true
		plan.fields, err = s.prepareWorkItemFields(ctx, scope.Organization, hubrecords.WorkItemRecordPlan{Issue: plan.issue})
		if err != nil {
			return plan, err
		}
		raw, err := json.Marshal(plan.issue)
		if err != nil {
			return plan, err
		}
		plan.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: plan.item.NativeID, RecordID: string(plan.item.NativeID), Revision: int64(plan.issue.Revision)}, raw)
		if err != nil {
			return plan, err
		}
		raw, err = json.Marshal(tracker.CollaborationEvent{OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: plan.item.NativeID, ID: hubapi.NewID("evt"), Type: "github.imported", SchemaVersion: 1, Actor: *nativeIntegrationActor("github"), RecordedAt: source.UpdatedAt, Data: tracker.CollaborationData{Revision: plan.issue.Revision}})
		if err != nil {
			return plan, err
		}
		plan.event, err = content.PrepareEvent(ctx, scope.Organization, raw)
		if err != nil {
			return plan, err
		}

	}
	seen := map[string]bool{}
	for _, record := range page.Records {
		if record.Kind != "comment" || record.Body == "" {
			continue
		}
		if len(record.Body) > 64<<10 {
			return plan, hubapi.NativePublicInvalid("Intake discussion contains an oversized comment")
		}
		if err := validateImportProvenance(&record.Provenance); err != nil {
			return plan, err
		}
		key := record.Provenance.Provider + ":" + record.Provenance.ExternalID
		if seen[key] {
			continue
		}
		seen[key] = true
		err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.CommentBySource(ctx, scope.Project, plan.item.NativeID, key)
			return err
		})
		if err == nil {
			continue
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return plan, err
		}
		sequence := plan.item.EventSequence + 1 + int64(len(plan.comments))
		if plan.writeIssue {
			sequence++
		}
		comment, err := s.prepareLinkedIssueComment(ctx, scope, plan.item.NativeID, tracker.GitHubIssueComment{Body: record.Body, Provenance: record.Provenance}, sequence, now)
		if err != nil {
			return plan, err
		}
		plan.comments = append(plan.comments, comment)
	}
	return plan, nil
}

func applyHistoricalGitHubImport(ctx context.Context, w tenantstore.Writer, project projectstore.Record, plan historicalGitHubImportPlan) error {
	if err := w.LockWorkItemProject(ctx, project.Core.ID); err != nil {
		return err
	}
	currentProject, err := w.ProjectRecord(ctx, project.Core.ID)
	if err != nil {
		return err
	}
	if !hubrecords.SameWorkItemProjectPlan(project, currentProject) {
		return corestore.ErrRevisionConflict
	}
	if !plan.create {
		current, err := w.LockWorkItem(ctx, project.Core.ID, plan.item.NativeID)
		if err != nil {
			return err
		}
		if current.Revision != plan.item.Revision || current.EventSequence != plan.item.EventSequence {
			return corestore.ErrRevisionConflict
		}
	}
	if plan.writeIssue {
		row := plan.item
		issue := plan.issue
		row.OrganizationID, row.ProjectID, row.NativeID = issue.OrganizationID, issue.ProjectID, issue.WorkItemID
		row.Number, row.Title = int64(issue.Number), issue.Title
		row.BodyObjectKey, row.ActorKind, row.ActorPrincipalID = &plan.fields.Body, &issue.Actor.Kind, &issue.Actor.PrincipalID
		row.ProvenanceProvider, row.ProvenanceExternalID = &issue.Provenance.Provider, &issue.Provenance.ExternalID
		row.NativeSourceKey = new(issue.Provenance.Provider + ":" + issue.Provenance.ExternalID)
		row.CreatedAt, row.NativeCreatedAt = issue.CreatedAt, issue.CreatedAt
		row.UpdatedAt, row.NativeUpdatedAt, row.LastActivityAt = issue.UpdatedAt, issue.UpdatedAt, issue.LastActivityAt
		row.WorkflowStateName = nil
		if issue.State != "" {
			row.WorkflowStateName = &issue.State
		}
		if plan.create {
			number, err := w.NextWorkItemNumber(ctx, project.Core.ID)
			if err != nil {
				return err
			}
			if number != row.Number {
				return corestore.ErrRevisionConflict
			}
			if _, err := w.CreateWorkItem(ctx, row); err != nil {
				return err
			}
		} else {
			owner, ok := w.(postgres.SourceWorkItemWriter)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			if _, err := owner.UpdateWorkItemFromSource(ctx, row, issue.LastActivityAt); err != nil {
				return err
			}
		}
		if err := w.SetWorkItemLabels(ctx, project.Core.ID, row.NativeID, issue.Labels); err != nil {
			return err
		}
		if err := w.PutWorkItemFacts(ctx, claimstore.WorkItemFacts{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, WorkItemID: issue.WorkItemID, Author: issue.Provenance.AuthorID, Repository: project.Settings.Repository, Assignees: issue.Assignees}); err != nil {
			return err
		}
		if _, err := w.CreateCollaborationVersion(ctx, plan.version); err != nil {
			return err
		}
		if _, err := w.AppendEvent(ctx, plan.event); err != nil {
			return err
		}
	}
	for _, comment := range plan.comments {
		if _, err := w.CommentBySource(ctx, project.Core.ID, plan.item.NativeID, *comment.Row.SourceKey); err == nil {
			return corestore.ErrRevisionConflict
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if _, err := w.CreateComment(ctx, comment.Row); err != nil {
			return err
		}
		if _, err := w.CreateCollaborationVersion(ctx, comment.Version); err != nil {
			return err
		}
		if _, err := w.AppendEvent(ctx, comment.Event); err != nil {
			return err
		}
	}
	return nil
}
