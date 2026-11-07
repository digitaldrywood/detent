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
		return tracker.LinkedIssueSource{}, false, projectRecordError(err)
	}
	result := &tracker.LinkedIssueSource{URL: record.URL, Status: "pending"}
	if record.SnapshotObjectKey == "" {
		return *result, true, nil
	}
	snapshot, err := eventstore.NewContent(s.runDetailStore()).LinkedIssueSnapshot(ctx, organization, eventstore.Reference(record.SnapshotObjectKey, record.SnapshotSHA256, record.SnapshotBytes, record.SnapshotContentBytes))
	if err != nil {
		return tracker.LinkedIssueSource{}, false, err
	}
	if snapshot.URL != record.URL {
		return tracker.LinkedIssueSource{}, false, artifact.ErrIntegrity
	}
	result.Status = "complete"
	result.Snapshot = &snapshot
	return *result, true, nil
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
		return nativeNotFound()
	}
	identity, err := r.RunnerForLease(ctx, string(mutation.LeaseID))
	if err != nil {
		return err
	}
	if identity.RunnerID != scope.credential.Runner.RunnerID || identity.TokenID != scope.credential.ID {
		return nativeNotFound()
	}
	allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, now)
	if err != nil {
		return err
	}
	if !allowed {
		return nativeNotFound()
	}
	return nil
}

type linkedIssueCommentPlan struct {
	comment tracker.NativeComment
	row     corestore.Comment
	version eventstore.CollaborationVersion
	event   eventstore.Event
}

func (s *Service) intakeLinkedIssueRecord(c echo.Context, request tracker.GitHubIntake) error {
	ctx := isolateContentContext(s.runDetailContext(c.Request().Context()))
	scope := nativeRequestScope(c)
	itemID := tracker.NativeWorkItemID(c.Param("item"))
	if scope.credential.Scope != apiScopeWorker && scope.credential.Scope != apiScopeAdmin && scope.credential.Scope != apiScopeOperator {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if s.config.AccessRepository == nil {
		return s.nativeAPIError(c, workItemStorageUnavailable())
	}
	if scope.credential.Scope != apiScopeWorker {
		scope.requireHostedAdmin = true
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return s.nativeAPIError(c, nativePublicInvalid("An idempotency key of at most 128 bytes is required"))
	}
	hash, err := nativeCommandHash(request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	receipt, found, err := s.workItemRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	content := eventstore.NewContent(s.runDetailStore())
	if found {
		value, err := content.Receipt(ctx, receipt)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSONBlob(http.StatusOK, value)
	}
	canonical, _, _, err := tracker.ParseGitHubIssueURL(request.Snapshot.URL)
	if err != nil {
		return s.nativeAPIError(c, nativePublicInvalid("Intake source URL is invalid"))
	}
	if err := validateNativeContent(request.Snapshot.Title, request.Snapshot.Body, nil, nil, nil); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := validateImportProvenance(&request.Snapshot.Provenance); err != nil {
		return s.nativeAPIError(c, err)
	}
	seen := make(map[string]bool, len(request.Snapshot.Comments))
	for _, comment := range request.Snapshot.Comments {
		if err := validateImportProvenance(&comment.Provenance); err != nil {
			return s.nativeAPIError(c, err)
		}
		if len(comment.Body) > 64<<10 || seen[comment.Provenance.ExternalID] {
			return s.nativeAPIError(c, nativePublicInvalid("Intake discussion contains an oversized or duplicate comment"))
		}
		seen[comment.Provenance.ExternalID] = true
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
		if scope.credential.Scope == apiScopeWorker {
			return requireLinkedIssueLease(ctx, r, scope, row, request.Mutation, now)
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	issue, err := s.hydrateWorkItemRecord(ctx, row, version)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if issue.LinkedSource == nil || issue.LinkedSource.URL != canonical {
		return s.nativeAPIError(c, nativePublicInvalid("Intake source does not match the linked issue"))
	}
	originalIssue := issue
	stale := false
	if source.SnapshotObjectKey != "" {
		old, err := content.LinkedIssueSnapshot(ctx, scope.organization, eventstore.Reference(source.SnapshotObjectKey, source.SnapshotSHA256, source.SnapshotBytes, source.SnapshotContentBytes))
		if err != nil {
			return s.nativeAPIError(c, err)
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
			return s.nativeAPIError(c, projectRecordError(lookupErr))
		}
		commentID := payloadNativeID(ctx, "cmt")
		comment := tracker.NativeComment{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: itemID, ID: commentID, Revision: 1, Body: imported.Body, Actor: *nativeIntegrationActor("github"), Provenance: &imported.Provenance, CreatedAt: imported.Provenance.CreatedAt, UpdatedAt: imported.Provenance.UpdatedAt}
		if comment.CreatedAt.IsZero() {
			comment.CreatedAt = now
		}
		if comment.UpdatedAt.IsZero() {
			comment.UpdatedAt = comment.CreatedAt
		}
		comment.UpdatedAt = payloadTimestamp(ctx, comment.UpdatedAt)
		comment.Sequence = row.EventSequence + 1 + int64(len(newComments))
		commentRaw, err := json.Marshal(comment)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		bodyRaw, err := json.Marshal(comment.Body)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		bodyKey, err := s.prepareWorkItemField(ctx, scope.organization, bodyRaw)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		provider, externalID := imported.Provenance.Provider, imported.Provenance.ExternalID
		commentPlan := linkedIssueCommentPlan{comment: comment, row: corestore.Comment{OrganizationID: scope.organization, ID: commentID, ProjectID: scope.project, WorkItemID: itemID, Revision: 1, Sequence: comment.Sequence, BodyObjectKey: bodyKey, ActorKind: "integration", ActorPrincipalID: "github", ProvenanceProvider: &provider, ProvenanceExternalID: &externalID, SourceKey: &key, CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt}}
		commentPlan.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: itemID, RecordID: commentID, Revision: 1}, commentRaw)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		eventRaw, err := json.Marshal(tracker.CollaborationEvent{OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: itemID, ID: payloadNativeID(ctx, "evt"), Type: "comment.created", SchemaVersion: 1, Actor: comment.Actor, RecordedAt: comment.CreatedAt, Data: tracker.CollaborationData{CommentID: commentID}})
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		commentPlan.event, err = content.PrepareEvent(ctx, scope.organization, eventRaw)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		newComments = append(newComments, commentPlan)
	}
	snapshot.Comments = nil
	var snapshotRef attachment.ArchiveReference
	if !stale {
		snapshotRef, err = content.PrepareLinkedIssueSnapshot(ctx, scope.organization, snapshot)
		if err != nil {
			return s.nativeAPIError(c, err)
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
			return s.nativeAPIError(c, err)
		}
		issuePlan = workItemRecordPlan{project: project, current: row, issue: issue, fields: []string{"source_intake"}, eventType: "github.imported"}
		fields, err = s.prepareWorkItemFields(ctx, scope.organization, issuePlan)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		issueVersion, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: itemID, RecordID: string(itemID), Revision: int64(issue.Revision)}, issueRaw)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		issueEventRaw, err := json.Marshal(tracker.CollaborationEvent{OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: itemID, ID: payloadNativeID(ctx, "evt"), Type: "github.imported", SchemaVersion: 1, Actor: *nativeIntegrationActor("github"), RecordedAt: issue.UpdatedAt, Data: tracker.CollaborationData{Revision: issue.Revision, Operation: "source_intake", Reason: canonical}})
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		issueEvent, err = content.PrepareEvent(ctx, scope.organization, issueEventRaw)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	if !stale {
		searchDocument, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: itemID, ID: "issue:" + string(itemID), SourceID: string(itemID), Kind: "issue", Scope: "issue", Revision: int64(issue.Revision), UpdatedAt: issue.UpdatedAt}, searchstore.Text{Title: issue.Title, Body: issue.Body, Labels: issue.Labels})
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	if stale {
		issue = originalIssue
	}
	response, err := json.Marshal(scope.reporterIssue(issue))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	receipt, err = content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if s.database.plans() != nil && s.database.billingRepository == nil {
		return s.nativeAPIError(c, workItemStorageUnavailable())
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return s.nativeAPIError(c, err)
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		prior, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
		if err == nil {
			if prior.RequestHash != hash {
				return nativeConflict(0)
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
		if scope.credential.Scope == apiScopeWorker {
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
		return s.nativeAPIError(c, projectRecordError(err))
	}
	return c.JSONBlob(http.StatusOK, response)
}
