package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readLinkedIssueSourceRecord(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, item tracker.NativeWorkItemID) (tracker.LinkedIssueSource, bool, error) {
	var record eventstore.LinkedSource
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		record, err = r.LinkedSource(ctx, project, item)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return tracker.LinkedIssueSource{}, false, nil
	}
	if err != nil {
		return tracker.LinkedIssueSource{}, false, hubapi.ProjectRecordError(err)
	}
	result, err := s.hydrateLinkedIssueSourceRecord(ctx, organization, record)
	if err != nil {
		return tracker.LinkedIssueSource{}, false, err
	}
	return result, true, nil
}

func (s *Service) hydrateLinkedIssueSourceRecord(ctx context.Context, organization tracker.OrganizationID, record eventstore.LinkedSource) (tracker.LinkedIssueSource, error) {
	result := &tracker.LinkedIssueSource{URL: record.URL, Status: "pending"}
	if record.SnapshotObjectKey == "" {
		return *result, nil
	}
	snapshot, err := eventstore.NewContent(s.runDetailStore()).LinkedIssueSnapshot(ctx, organization, eventstore.Reference(record.SnapshotObjectKey, record.SnapshotSHA256, record.SnapshotBytes, record.SnapshotContentBytes))
	if err != nil {
		return tracker.LinkedIssueSource{}, err
	}
	if snapshot.URL != record.URL {
		return tracker.LinkedIssueSource{}, artifact.ErrIntegrity
	}
	result.Status = "complete"
	result.Snapshot = &snapshot
	return *result, nil
}

func requireLinkedIssueLease(ctx context.Context, r tenantstore.Reader, scope nativeScope, item corestore.WorkItem, mutation tracker.Mutation, now time.Time) error {
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
	if scope.credential.Runner.RunnerID == "" || string(scope.credential.Runner.MachineID) != lease.MachineID {
		return hubapi.NativeNotFound()
	}
	identity, err := r.RunnerForLease(ctx, string(mutation.LeaseID))
	if err != nil {
		return err
	}
	if identity.RunnerID != scope.credential.Runner.RunnerID || identity.TokenID != scope.credential.ID {
		return hubapi.NativeNotFound()
	}
	allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, now)
	if err != nil {
		return err
	}
	if !allowed {
		return hubapi.NativeNotFound()
	}
	return nil
}

type linkedIssueCommentPlan struct {
	comment tracker.NativeComment
	row     corestore.Comment
	version eventstore.CollaborationVersion
	event   eventstore.Event
	bytes   int64
}

func (s *Service) prepareLinkedIssueCreation(ctx context.Context, scope nativeScope, plan workItemRecordPlan) (workItemRecordPlan, error) {
	if !plan.create || plan.existing || plan.linkedSource == nil || s.config.ImportBackend == nil || scope.integrationRepository != "" {
		return plan, nil
	}
	snapshot, err := s.fetchLinkedSnapshot(ctx, scope, plan.linkedSource.URL)
	if err != nil {
		return plan, err
	}
	if err := validateLinkedIssueSnapshot(*snapshot); err != nil {
		return plan, err
	}
	if !plan.linkedSource.TitleSupplied {
		plan.issue.Title = snapshot.Title
	}
	if !plan.linkedSource.BodySupplied {
		plan.issue.Body = snapshot.Body
	}
	plan.issue.Provenance = &snapshot.Provenance
	plan.issue.ExternalReferences = nativeSourceReferences("", "", "", 0, "", snapshot.URL, plan.issue.Provenance)
	plan.issue.ExternalReferences = append(plan.issue.ExternalReferences, tracker.GitHubIssueSourceReference(snapshot.URL, snapshot.URL))
	if err := s.applyScheduledRepairRecord(ctx, scope, &plan.issue); err != nil {
		return plan, err
	}
	for _, imported := range snapshot.Comments {
		comment, err := s.prepareLinkedIssueComment(ctx, scope, plan.issue.WorkItemID, imported, 2+int64(len(plan.linkedComments)), plan.issue.CreatedAt)
		if err != nil {
			return plan, err
		}
		plan.linkedComments = append(plan.linkedComments, comment)
	}
	snapshot.Comments = nil
	reference, err := eventstore.NewContent(s.runDetailStore()).PrepareLinkedIssueSnapshot(ctx, scope.organization, *snapshot)
	if err != nil {
		return plan, err
	}
	plan.linkedSource.SnapshotObjectKey, plan.linkedSource.SnapshotSHA256 = reference.Key, reference.SHA256
	plan.linkedSource.SnapshotBytes, plan.linkedSource.SnapshotContentBytes = reference.CompressedSize, reference.UncompressedSize
	plan.issue.LinkedSource = &tracker.LinkedIssueSource{URL: snapshot.URL, Status: "complete", Snapshot: snapshot}
	return plan, nil
}

func validateLinkedIssueSnapshot(snapshot tracker.GitHubIssueSnapshot) error {
	if err := validateNativeContent(snapshot.Title, snapshot.Body, nil, nil, nil); err != nil {
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

func (s *Service) prepareLinkedIssueComment(ctx context.Context, scope nativeScope, itemID tracker.NativeWorkItemID, imported tracker.GitHubIssueComment, sequence int64, now time.Time) (linkedIssueCommentPlan, error) {
	content := eventstore.NewContent(s.runDetailStore())
	key := imported.Provenance.Provider + ":" + imported.Provenance.ExternalID
	commentID := payloadNativeID(ctx, "cmt")
	comment := tracker.NativeComment{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: itemID, ID: commentID, Revision: 1, Body: imported.Body, Actor: *nativeIntegrationActor("github"), Provenance: &imported.Provenance, CreatedAt: imported.Provenance.CreatedAt, UpdatedAt: imported.Provenance.UpdatedAt}
	if comment.CreatedAt.IsZero() {
		comment.CreatedAt = now
	}
	if comment.UpdatedAt.IsZero() {
		comment.UpdatedAt = comment.CreatedAt
	}
	comment.UpdatedAt = payloadTimestamp(ctx, comment.UpdatedAt)
	comment.Sequence = sequence
	commentRaw, err := json.Marshal(comment)
	if err != nil {
		return linkedIssueCommentPlan{}, err
	}
	bodyRaw, err := json.Marshal(comment.Body)
	if err != nil {
		return linkedIssueCommentPlan{}, err
	}
	bodyKey, err := s.prepareWorkItemField(ctx, scope.organization, bodyRaw)
	if err != nil {
		return linkedIssueCommentPlan{}, err
	}
	provider, externalID := imported.Provenance.Provider, imported.Provenance.ExternalID
	commentPlan := linkedIssueCommentPlan{comment: comment, row: corestore.Comment{OrganizationID: scope.organization, ID: commentID, ProjectID: scope.project, WorkItemID: itemID, Revision: 1, Sequence: comment.Sequence, BodyObjectKey: bodyKey, ActorKind: "integration", ActorPrincipalID: "github", ProvenanceProvider: &provider, ProvenanceExternalID: &externalID, SourceKey: &key, CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt}}
	commentPlan.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: itemID, RecordID: commentID, Revision: 1}, commentRaw)
	if err != nil {
		return linkedIssueCommentPlan{}, err
	}
	eventRaw, err := json.Marshal(tracker.CollaborationEvent{OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: itemID, ID: payloadNativeID(ctx, "evt"), Type: "comment.created", SchemaVersion: 1, Actor: comment.Actor, RecordedAt: comment.CreatedAt, Data: tracker.CollaborationData{CommentID: commentID}})
	if err != nil {
		return linkedIssueCommentPlan{}, err
	}
	commentPlan.event, err = content.PrepareEvent(ctx, scope.organization, eventRaw)
	if err != nil {
		return linkedIssueCommentPlan{}, err
	}
	eventBytes, err := collaborationEventChargeBytes(eventRaw)
	if err != nil {
		return linkedIssueCommentPlan{}, err
	}
	commentPlan.bytes = int64(len(commentRaw)+len(comment.Body)) + eventBytes
	return commentPlan, nil
}

func (s *Service) intakeLinkedIssueRecord(c echo.Context, request tracker.GitHubIntake) error {
	raw, err := s.applyLinkedIssueRecord(c.Request().Context(), nativeRequestScope(c), tracker.NativeWorkItemID(c.Param("item")), c.Request().Method+" "+c.Request().URL.EscapedPath(), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}

func (s *Service) applyLinkedIssueRecord(ctx context.Context, scope nativeScope, itemID tracker.NativeWorkItemID, operation string, request tracker.GitHubIntake) (json.RawMessage, error) {
	ctx = isolateContentContext(s.runDetailContext(ctx))
	if scope.credential.Scope != hubapi.APIScopeWorker && scope.credential.Scope != hubapi.APIScopeAdmin && scope.credential.Scope != hubapi.APIScopeOperator {
		return nil, hubapi.NativeNotFound()
	}
	if s.config.AccessRepository == nil {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	if scope.credential.Scope != hubapi.APIScopeWorker {
		scope.requireHostedAdmin = true
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
	now := s.config.now().UTC()
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.WorkItem(ctx, scope.project, itemID)
		if err != nil {
			return err
		}
		project, err = r.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		version, err = r.CollaborationVersion(ctx, string(itemID), row.Revision)
		if err != nil {
			return err
		}
		source, err = r.LinkedSource(ctx, scope.project, itemID)
		if err != nil {
			return err
		}
		if scope.credential.Scope == hubapi.APIScopeWorker {
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
		old, err := content.LinkedIssueSnapshot(ctx, scope.organization, eventstore.Reference(source.SnapshotObjectKey, source.SnapshotSHA256, source.SnapshotBytes, source.SnapshotContentBytes))
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
		issue.UpdatedAt = payloadTimestamp(ctx, now)
		issue.Revision++
		issue.LinkedSource = &tracker.LinkedIssueSource{URL: canonical, Status: "complete"}
		if err := s.applyScheduledRepairRecord(ctx, scope, &issue); err != nil {
			return nil, err
		}
	}
	snapshot := request.Snapshot
	snapshot.URL = canonical
	newComments := make([]linkedIssueCommentPlan, 0, len(snapshot.Comments))
	for _, imported := range snapshot.Comments {
		key := imported.Provenance.Provider + ":" + imported.Provenance.ExternalID
		lookupErr := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.CommentBySource(ctx, scope.project, itemID, key)
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
		snapshotRef, err = content.PrepareLinkedIssueSnapshot(ctx, scope.organization, snapshot)
		if err != nil {
			return nil, err
		}
	}
	var issuePlan workItemRecordPlan
	var fields workItemRecordFields
	var issueVersion eventstore.CollaborationVersion
	var issueEvent eventstore.Event
	var searchDocument searchstore.Document
	if !stale {
		issue.LinkedSource.Snapshot = &snapshot
		issueRaw, err := json.Marshal(issue)
		if err != nil {
			return nil, err
		}
		issuePlan = workItemRecordPlan{project: project, current: row, issue: issue, fields: []string{"source_intake"}, eventType: "github.imported"}
		fields, err = s.prepareWorkItemFields(ctx, scope.organization, issuePlan)
		if err != nil {
			return nil, err
		}
		issueVersion, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: itemID, RecordID: string(itemID), Revision: int64(issue.Revision)}, issueRaw)
		if err != nil {
			return nil, err
		}
		reason := canonical
		if verified := scope.scheduledRepair; verified != nil && verified.importURL == canonical && verified.importReason != "" {
			reason += ": " + verified.importReason
		}
		issueEventRaw, err := json.Marshal(tracker.CollaborationEvent{OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: itemID, ID: payloadNativeID(ctx, "evt"), Type: "github.imported", SchemaVersion: 1, Actor: *nativeIntegrationActor("github"), RecordedAt: issue.UpdatedAt, Data: tracker.CollaborationData{Revision: issue.Revision, Operation: "source_intake", Reason: reason}})
		if err != nil {
			return nil, err
		}
		issueEvent, err = content.PrepareEvent(ctx, scope.organization, issueEventRaw)
		if err != nil {
			return nil, err
		}
	}
	if !stale {
		searchDocument, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: itemID, ID: "issue:" + string(itemID), SourceID: string(itemID), Kind: "issue", Scope: "issue", Revision: int64(issue.Revision), UpdatedAt: issue.UpdatedAt}, searchstore.Text{Title: issue.Title, Body: issue.Body, Labels: issue.Labels})
		if err != nil {
			return nil, err
		}
	}
	if stale {
		issue = originalIssue
	}
	response, err := json.Marshal(relationshipOmittedIssue(scope.reporterIssue(issue)))
	if err != nil {
		return nil, err
	}
	receipt, err = content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
	if err != nil {
		return nil, err
	}
	if s.database.plans() != nil && s.database.billingRepository == nil {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	trustedBeforeCommit := scope.scheduledRepair.trustedLinkedImport()
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		if err := s.requireGitHubPolicyRecord(ctx, w, scope); err != nil {
			return err
		}
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		prior, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
		if err == nil {
			if prior.RequestHash != hash {
				return hubapi.NativeConflict(0)
			}
			return corestore.ErrSerializationFailure
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		if s.database.plans() != nil {
			now, err := s.database.currentTime()
			if err != nil {
				return err
			}
			if err := s.database.requireHostedFeatureRecord(ctx, w, "collaboration", now); err != nil {
				return err
			}
		}
		current, err := w.LockWorkItem(ctx, scope.project, itemID)
		if err != nil {
			return err
		}
		if current.Revision != row.Revision || current.EventSequence != row.EventSequence {
			return corestore.ErrSerializationFailure
		}
		if scope.credential.Scope == hubapi.APIScopeWorker {
			if err := requireLinkedIssueLease(ctx, w, scope, current, request.Mutation, s.config.now()); err != nil {
				return err
			}
		}
		currentSource, err := w.LinkedSource(ctx, scope.project, itemID)
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
			key := imported.comment.Provenance.Provider + ":" + imported.comment.Provenance.ExternalID
			if _, err := w.CommentBySource(ctx, scope.project, itemID, key); err == nil {
				continue
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if _, err := w.CreateComment(ctx, imported.row); err != nil {
				return err
			}
			if _, err := w.CreateCollaborationVersion(ctx, imported.version); err != nil {
				return err
			}
			if _, err := w.AppendEvent(ctx, imported.event); err != nil {
				return err
			}
		}
		if !stale {
			if err := writeWorkItemRecord(ctx, w, issuePlan, fields); err != nil {
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
		if trustedBeforeCommit && !scope.scheduledRepair.trustedLinkedImport() && errors.Is(err, corestore.ErrSerializationFailure) {
			return s.applyLinkedIssueRecord(ctx, scope, itemID, operation, request)
		}
		return nil, hubapi.ProjectRecordError(err)
	}
	return response, nil
}
