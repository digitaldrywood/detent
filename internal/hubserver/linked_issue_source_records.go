package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func requireLinkedIssueLease(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, item corestore.WorkItem, mutation tracker.Mutation, now time.Time) error {
	if mutation.LeaseID == "" || mutation.FencingToken <= 0 {
		return tracker.ErrStaleFencingToken
	}
	lease, err := r.Lease(ctx, string(mutation.LeaseID))
	if err != nil {
		return err
	}
	if lease.IssueID != item.ID || lease.FencingToken != int64(mutation.FencingToken) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) {
		return tracker.ErrStaleFencingToken
	}
	if scope.Credential.Runner.RunnerID == "" || string(scope.Credential.Runner.MachineID) != lease.MachineID {
		return hubapi.NativeNotFound()
	}
	identity, err := r.RunnerForLease(ctx, string(mutation.LeaseID))
	if err != nil {
		return err
	}
	if identity.RunnerID != scope.Credential.Runner.RunnerID || identity.TokenID != scope.Credential.ID {
		return hubapi.NativeNotFound()
	}
	allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, now)
	if err != nil {
		return err
	}
	if !allowed {
		return hubapi.NativeNotFound()
	}
	return nil
}

func (s *Service) prepareLinkedIssueCreation(ctx context.Context, scope hubscope.NativeScope, plan hubrecords.WorkItemRecordPlan) (hubrecords.WorkItemRecordPlan, error) {
	if !plan.Create || plan.Existing || plan.LinkedSource == nil || s.config.ImportBackend == nil || scope.IntegrationRepository != "" {
		return plan, nil
	}
	snapshot, err := s.fetchLinkedSnapshot(ctx, scope, plan.LinkedSource.URL)
	if err != nil {
		return plan, err
	}
	if err := validateLinkedIssueSnapshot(*snapshot); err != nil {
		return plan, err
	}
	if !plan.LinkedSource.TitleSupplied {
		plan.Issue.Title = snapshot.Title
	}
	if !plan.LinkedSource.BodySupplied {
		plan.Issue.Body = snapshot.Body
	}
	plan.Issue.Provenance = &snapshot.Provenance
	plan.Issue.ExternalReferences = nativeSourceReferences("", "", "", 0, "", snapshot.URL, plan.Issue.Provenance)
	plan.Issue.ExternalReferences = append(plan.Issue.ExternalReferences, tracker.GitHubIssueSourceReference(snapshot.URL, snapshot.URL))
	if err := s.applyScheduledRepairRecord(ctx, scope, &plan.Issue); err != nil {
		return plan, err
	}
	for _, imported := range snapshot.Comments {
		comment, err := s.prepareLinkedIssueComment(ctx, scope, plan.Issue.WorkItemID, imported, 2+int64(len(plan.LinkedComments)), plan.Issue.CreatedAt)
		if err != nil {
			return plan, err
		}
		plan.LinkedComments = append(plan.LinkedComments, comment)
	}
	snapshot.Comments = nil
	reference, err := eventstore.NewContent(s.runDetailStore()).PrepareLinkedIssueSnapshot(ctx, scope.Organization, *snapshot)
	if err != nil {
		return plan, err
	}
	plan.LinkedSource.SnapshotObjectKey, plan.LinkedSource.SnapshotSHA256 = reference.Key, reference.SHA256
	plan.LinkedSource.SnapshotBytes, plan.LinkedSource.SnapshotContentBytes = reference.CompressedSize, reference.UncompressedSize
	plan.Issue.LinkedSource = &tracker.LinkedIssueSource{URL: snapshot.URL, Status: "complete", Snapshot: snapshot}
	return plan, nil
}

func validateLinkedIssueSnapshot(snapshot tracker.GitHubIssueSnapshot) error {
	if err := hubrecords.ValidateNativeContent(snapshot.Title, snapshot.Body, nil, nil, nil); err != nil {
		return err
	}
	if err := validateImportProvenance(&snapshot.Provenance); err != nil {
		return err
	}
	seen := make(map[string]bool, len(snapshot.Comments))
	for _, comment := range snapshot.Comments {
		if err := validateImportProvenance(&comment.Provenance); err != nil {
			return err
		}
		if len(comment.Body) > 64<<10 || seen[comment.Provenance.ExternalID] {
			return hubapi.NativePublicInvalid("Intake discussion contains an oversized or duplicate comment")
		}
		seen[comment.Provenance.ExternalID] = true
	}
	return nil
}

func (s *Service) prepareLinkedIssueComment(ctx context.Context, scope hubscope.NativeScope, itemID tracker.NativeWorkItemID, imported tracker.GitHubIssueComment, sequence int64, now time.Time) (hubrecords.LinkedIssueCommentPlan, error) {
	content := eventstore.NewContent(s.runDetailStore())
	key := imported.Provenance.Provider + ":" + imported.Provenance.ExternalID
	commentID := hubcontent.PayloadNativeID(ctx, "cmt")
	comment := tracker.NativeComment{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: itemID, ID: commentID, Revision: 1, Body: imported.Body, Actor: *nativeIntegrationActor("github"), Provenance: &imported.Provenance, CreatedAt: imported.Provenance.CreatedAt, UpdatedAt: imported.Provenance.UpdatedAt}
	if comment.CreatedAt.IsZero() {
		comment.CreatedAt = now
	}
	if comment.UpdatedAt.IsZero() {
		comment.UpdatedAt = comment.CreatedAt
	}
	comment.UpdatedAt = hubcontent.PayloadTimestamp(ctx, comment.UpdatedAt)
	comment.Sequence = sequence
	commentRaw, err := json.Marshal(comment)
	if err != nil {
		return hubrecords.LinkedIssueCommentPlan{}, err
	}
	bodyRaw, err := json.Marshal(comment.Body)
	if err != nil {
		return hubrecords.LinkedIssueCommentPlan{}, err
	}
	bodyKey, err := s.prepareWorkItemField(ctx, scope.Organization, bodyRaw)
	if err != nil {
		return hubrecords.LinkedIssueCommentPlan{}, err
	}
	provider, externalID := imported.Provenance.Provider, imported.Provenance.ExternalID
	commentPlan := hubrecords.LinkedIssueCommentPlan{Comment: comment, Row: corestore.Comment{OrganizationID: scope.Organization, ID: commentID, ProjectID: scope.Project, WorkItemID: itemID, Revision: 1, Sequence: comment.Sequence, BodyObjectKey: bodyKey, ActorKind: "integration", ActorPrincipalID: "github", ProvenanceProvider: &provider, ProvenanceExternalID: &externalID, SourceKey: &key, CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt}}
	commentPlan.Version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: itemID, RecordID: commentID, Revision: 1}, commentRaw)
	if err != nil {
		return hubrecords.LinkedIssueCommentPlan{}, err
	}
	eventRaw, err := json.Marshal(tracker.CollaborationEvent{OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: itemID, ID: hubcontent.PayloadNativeID(ctx, "evt"), Type: "comment.created", SchemaVersion: 1, Actor: comment.Actor, RecordedAt: comment.CreatedAt, Data: tracker.CollaborationData{CommentID: commentID}})
	if err != nil {
		return hubrecords.LinkedIssueCommentPlan{}, err
	}
	commentPlan.Event, err = content.PrepareEvent(ctx, scope.Organization, eventRaw)
	if err != nil {
		return hubrecords.LinkedIssueCommentPlan{}, err
	}
	eventBytes, err := collaborationEventChargeBytes(eventRaw)
	if err != nil {
		return hubrecords.LinkedIssueCommentPlan{}, err
	}
	commentPlan.Bytes = int64(len(commentRaw)+len(comment.Body)) + eventBytes
	return commentPlan, nil
}

func (s *Service) intakeLinkedIssueRecord(c echo.Context, request tracker.GitHubIntake) error {
	raw, err := s.applyLinkedIssueRecord(c.Request().Context(), hubscope.NativeRequestScope(c), tracker.NativeWorkItemID(c.Param("item")), c.Request().Method+" "+c.Request().URL.EscapedPath(), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}

func (s *Service) applyLinkedIssueRecord(ctx context.Context, scope hubscope.NativeScope, itemID tracker.NativeWorkItemID, operation string, request tracker.GitHubIntake) (json.RawMessage, error) {
	ctx = hubcontent.IsolateContentContext(s.runDetailContext(ctx))
	if scope.Credential.Scope != hubapi.APIScopeWorker && scope.Credential.Scope != hubapi.APIScopeAdmin && scope.Credential.Scope != hubapi.APIScopeOperator {
		return nil, hubapi.NativeNotFound()
	}
	if s.config.AccessRepository == nil {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	if scope.Credential.Scope != hubapi.APIScopeWorker {
		scope.RequireHostedAdmin = true
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	receipt, found, err := s.workItemRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	if found {
		value, err := s.readWorkItemRecordReceipt(ctx, scope, receipt, request, string(itemID))
		if err != nil {
			return nil, err
		}
		return value, nil
	}
	canonical, _, _, err := tracker.ParseGitHubIssueURL(request.Snapshot.URL)
	if err != nil {
		return nil, hubapi.NativePublicInvalid("Intake source URL is invalid")
	}
	if err := validateLinkedIssueSnapshot(request.Snapshot); err != nil {
		return nil, err
	}
	var row corestore.WorkItem
	var project projectstore.Record
	var version eventstore.CollaborationVersion
	var source eventstore.LinkedSource
	now := s.config.Clock().UTC()
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.WorkItem(ctx, scope.Project, itemID)
		if err != nil {
			return err
		}
		project, err = r.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		version, err = r.CollaborationVersion(ctx, string(itemID), row.Revision)
		if err != nil {
			return err
		}
		source, err = r.LinkedSource(ctx, scope.Project, itemID)
		if err != nil {
			return err
		}
		if scope.Credential.Scope == hubapi.APIScopeWorker {
			return requireLinkedIssueLease(ctx, r, scope, row, request.Mutation, now)
		}
		return nil
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	issue, err := s.hydrateWorkItemRecord(ctx, row, version)
	if err != nil {
		return nil, err
	}
	if issue.LinkedSource == nil || issue.LinkedSource.URL != canonical {
		return nil, hubapi.NativePublicInvalid("Intake source does not match the linked issue")
	}
	originalIssue := issue
	stale := false
	if source.SnapshotObjectKey != "" {
		old, err := content.LinkedIssueSnapshot(ctx, scope.Organization, eventstore.Reference(source.SnapshotObjectKey, source.SnapshotSHA256, source.SnapshotBytes, source.SnapshotContentBytes))
		if err != nil {
			return nil, err
		}
		stale = !request.Snapshot.Provenance.UpdatedAt.After(old.Provenance.UpdatedAt)
	}
	if !source.TitleSupplied && !source.TitleOwned && !stale {
		issue.Title = request.Snapshot.Title
	}
	if !source.BodySupplied && !source.BodyOwned && !stale {
		issue.Body = request.Snapshot.Body
	}
	if !stale {
		issue.Provenance = &request.Snapshot.Provenance
		issue.LastActivityAt = now
		issue.UpdatedAt = hubcontent.PayloadTimestamp(ctx, now)
		issue.Revision++
		issue.LinkedSource = &tracker.LinkedIssueSource{URL: canonical, Status: "complete"}
		if err := s.applyScheduledRepairRecord(ctx, scope, &issue); err != nil {
			return nil, err
		}
	}
	snapshot := request.Snapshot
	snapshot.URL = canonical
	newComments := make([]hubrecords.LinkedIssueCommentPlan, 0, len(snapshot.Comments))
	for _, imported := range snapshot.Comments {
		key := imported.Provenance.Provider + ":" + imported.Provenance.ExternalID
		lookupErr := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.CommentBySource(ctx, scope.Project, itemID, key)
			return err
		})
		if lookupErr == nil {
			continue
		}
		if !errors.Is(lookupErr, corestore.ErrNotFound) {
			return nil, hubapi.ProjectRecordError(lookupErr)
		}
		commentPlan, err := s.prepareLinkedIssueComment(ctx, scope, itemID, imported, row.EventSequence+1+int64(len(newComments)), now)
		if err != nil {
			return nil, err
		}
		newComments = append(newComments, commentPlan)
	}
	snapshot.Comments = nil
	var snapshotRef attachment.ArchiveReference
	if !stale {
		snapshotRef, err = content.PrepareLinkedIssueSnapshot(ctx, scope.Organization, snapshot)
		if err != nil {
			return nil, err
		}
	}
	var issuePlan hubrecords.WorkItemRecordPlan
	var fields hubrecords.WorkItemRecordFields
	var issueVersion eventstore.CollaborationVersion
	var issueEvent eventstore.Event
	var searchDocument searchstore.Document
	if !stale {
		issue.LinkedSource.Snapshot = &snapshot
		issueRaw, err := json.Marshal(issue)
		if err != nil {
			return nil, err
		}
		issuePlan = hubrecords.WorkItemRecordPlan{Project: project, Current: row, Issue: issue, Fields: []string{"source_intake"}, EventType: "github.imported"}
		fields, err = s.prepareWorkItemFields(ctx, scope.Organization, issuePlan)
		if err != nil {
			return nil, err
		}
		issueVersion, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: itemID, RecordID: string(itemID), Revision: int64(issue.Revision)}, issueRaw)
		if err != nil {
			return nil, err
		}
		reason := canonical
		if verified := scope.ScheduledRepair; verified != nil && verified.ImportURL == canonical && verified.ImportReason != "" {
			reason += ": " + verified.ImportReason
		}
		issueEventRaw, err := json.Marshal(tracker.CollaborationEvent{OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: itemID, ID: hubcontent.PayloadNativeID(ctx, "evt"), Type: "github.imported", SchemaVersion: 1, Actor: *nativeIntegrationActor("github"), RecordedAt: issue.UpdatedAt, Data: tracker.CollaborationData{Revision: issue.Revision, Operation: "source_intake", Reason: reason}})
		if err != nil {
			return nil, err
		}
		issueEvent, err = content.PrepareEvent(ctx, scope.Organization, issueEventRaw)
		if err != nil {
			return nil, err
		}
	}
	if !stale {
		searchDocument, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: itemID, ID: "issue:" + string(itemID), SourceID: string(itemID), Kind: "issue", Scope: "issue", Revision: int64(issue.Revision), UpdatedAt: issue.UpdatedAt}, searchstore.Text{Title: issue.Title, Body: issue.Body, Labels: issue.Labels})
		if err != nil {
			return nil, err
		}
	}
	if stale {
		issue = originalIssue
	}
	response, err := json.Marshal(hubrecords.RelationshipOmittedIssue(scope.ReporterIssue(issue)))
	if err != nil {
		return nil, err
	}
	receipt, err = content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
	if err != nil {
		return nil, err
	}
	if s.database.plansService().Plans() != nil && s.database.billingRepository == nil {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	trustedBeforeCommit := scope.ScheduledRepair.TrustedLinkedImport()
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		if err := s.requireGitHubPolicyRecord(ctx, w, scope); err != nil {
			return err
		}
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		prior, err := w.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey)
		if err == nil {
			if prior.RequestHash != hash {
				return hubapi.NativeConflict(0)
			}
			return corestore.ErrSerializationFailure
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		if s.database.plansService().Plans() != nil {
			now, err := s.database.currentTime()
			if err != nil {
				return err
			}
			if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, w, "collaboration", now)); err != nil {
				return err
			}
		}
		current, err := w.LockWorkItem(ctx, scope.Project, itemID)
		if err != nil {
			return err
		}
		if current.Revision != row.Revision || current.EventSequence != row.EventSequence {
			return corestore.ErrSerializationFailure
		}
		if scope.Credential.Scope == hubapi.APIScopeWorker {
			if err := requireLinkedIssueLease(ctx, w, scope, current, request.Mutation, s.config.Clock()); err != nil {
				return err
			}
		}
		currentSource, err := w.LinkedSource(ctx, scope.Project, itemID)
		if err != nil {
			return err
		}
		if source.SnapshotSHA256 == "" {
			currentSource.ExpectedSnapshotSHA256 = nil
		} else {
			currentSource.ExpectedSnapshotSHA256 = new(source.SnapshotSHA256)
		}
		if !stale {
			currentSource.SnapshotObjectKey, currentSource.SnapshotSHA256 = snapshotRef.Key, snapshotRef.SHA256
			currentSource.SnapshotBytes, currentSource.SnapshotContentBytes = snapshotRef.CompressedSize, snapshotRef.UncompressedSize
			if _, err := w.UpdateLinkedSource(ctx, currentSource); err != nil {
				return err
			}
		}
		for _, imported := range newComments {
			key := imported.Comment.Provenance.Provider + ":" + imported.Comment.Provenance.ExternalID
			if _, err := w.CommentBySource(ctx, scope.Project, itemID, key); err == nil {
				continue
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if _, err := w.CreateComment(ctx, imported.Row); err != nil {
				return err
			}
			if _, err := w.CreateCollaborationVersion(ctx, imported.Version); err != nil {
				return err
			}
			if _, err := w.AppendEvent(ctx, imported.Event); err != nil {
				return err
			}
		}
		if !stale {
			if err := hubrecords.WriteWorkItemRecord(ctx, w, issuePlan, fields); err != nil {
				return err
			}
			if err := w.PutSearch(ctx, searchDocument, row.Revision); err != nil {
				return err
			}
			if _, err := w.CreateCollaborationVersion(ctx, issueVersion); err != nil {
				return err
			}
			if _, err := w.AppendEvent(ctx, issueEvent); err != nil {
				return err
			}
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if err != nil {
		if trustedBeforeCommit && !scope.ScheduledRepair.TrustedLinkedImport() && errors.Is(err, corestore.ErrSerializationFailure) {
			return s.applyLinkedIssueRecord(ctx, scope, itemID, operation, request)
		}
		return nil, hubapi.ProjectRecordError(err)
	}
	return response, nil
}
