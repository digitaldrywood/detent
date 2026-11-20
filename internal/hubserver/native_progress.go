package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeProgressOwner struct {
	service          *Service
	scope            hubscope.NativeScope
	request          progresscheck.Request
	leaseID          tracker.LeaseID
	fencingToken     tracker.FencingToken
	guardRevision    *int64
	expectedRevision tracker.Revision
	publicationHash  string
}

const nativeProgressReceiptActor = "native-progress"
const nativeProgressDependencySourcePrefix = "progress-check:"

type nativeProgressReceipt struct {
	progresscheck.Record
	Replay *nativeProgressReplay `json:"cleanup_replay,omitempty"`
}

type nativeProgressReplay struct {
	RequestAfterCleanupID string `json:"request_after_cleanup_id"`
	PublicationHash       string `json:"publication_hash"`
}

func nativeProgressReceiptOperation(scope hubscope.NativeScope, issue string) string {
	return "native progress/" + string(scope.Project) + "/" + issue
}

func nativeProgressActor(scope hubscope.NativeScope) bool {
	return scope.SourceActor != nil && scope.SourceActor.Kind == "machine" && scope.SourceActor.Agent == "progress-check"
}

func (o *nativeProgressOwner) AuthorizeProgressDefect(ctx context.Context, w tenantstore.Writer, owner any, scope hubscope.NativeScope, leaseID tracker.LeaseID, fencingToken tracker.FencingToken) error {
	if owner != any(o.service) || o.scope.Organization != scope.Organization || o.scope.Project != scope.Project || o.scope.Credential.ID != scope.Credential.ID || o.leaseID != leaseID || o.fencingToken != fencingToken {
		return hubscope.RunnerUnauthorized()
	}
	return o.validateProgressLeaseWriter(ctx, w)
}

func (o nativeProgressOwner) validateProgressLeaseWriter(ctx context.Context, writer tenantstore.Writer) error {
	if o.scope.Credential.Scope != hubapi.APIScopeWorker {
		return nil
	}
	credential, err := writer.AccessCredential(ctx, o.scope.Credential.ID)
	if errors.Is(err, corestore.ErrNotFound) {
		return hubscope.RunnerUnauthorized()
	}
	if err != nil {
		return err
	}
	identity, err := writer.RunnerIdentityByCredential(ctx, credential.ID)
	if err != nil {
		return err
	}
	item, err := writer.LockWorkItem(ctx, o.scope.Project, tracker.NativeWorkItemID(o.request.IssueID))
	if err != nil {
		return err
	}
	locker, ok := writer.(leasestore.FencedLeaseLocker)
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	assigned, err := writer.RunnerForLease(ctx, string(o.leaseID))
	if err != nil {
		return err
	}
	lease, err := locker.LockLease(ctx, string(o.leaseID))
	if err != nil {
		return err
	}
	now := o.service.config.Clock().UTC()
	if lease.OrganizationID != o.scope.Organization || lease.FencingToken != int64(o.fencingToken) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) || lease.IssueID != item.ID {
		return tracker.ErrStaleFencingToken
	}
	if o.scope.Credential.EntryKey != nil || credential.OrganizationID != o.scope.Organization || credential.Scope != string(hubapi.APIScopeWorker) || credential.Kind != "service" || !credential.Valid(o.scope.Credential.Hash, now) || identity.RunnerID != assigned.RunnerID || !runnerIdentityMatchesHeldLease(identity, o.scope.Organization, o.scope.Credential.Runner.RunnerID, credential.ID, tracker.MachineID(lease.MachineID), now) || identity.State == "disabled" || !slices.Contains(identity.Operations, runnerauth.Claim) || !slices.Contains(identity.Operations, runnerauth.Collaborate) {
		return hubscope.RunnerUnauthorized()
	}
	allowed, err := writer.RunnerProjectAuthorized(ctx, identity.RunnerID, o.scope.Project, now)
	if err != nil {
		return err
	}
	if !allowed {
		return hubapi.NativeNotFoundAt("lease_runner_credential")
	}
	pinned, err := writer.LeasePolicy(ctx, o.leaseID)
	if err != nil {
		return err
	}
	if pinned.OrganizationID != o.scope.Organization || pinned.ProjectID != o.scope.Project || pinned.LeaseID != o.leaseID || pinned.PolicyID == "" || pinned.PolicyID != o.request.PolicyID || o.request.Instance != lease.MachineID {
		return hubapi.PolicyMismatch("Progress request must match the policy and instance pinned to its lease")
	}
	if o.expectedRevision > 0 && item.Revision != int64(o.expectedRevision) {
		return &hubapi.NativeError{Code: "revision_conflict", Status: http.StatusConflict}
	}
	return nil
}

func (o nativeProgressOwner) authorizePublication(ctx context.Context, expectedSequence *int64) error {
	return o.service.config.TenantRepository.WriteTenant(ctx, o.scope.Organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := o.service.authorizeAccessScope(ctx, writer, o.scope, true); err != nil {
			return err
		}
		if err := o.validateProgressLeaseWriter(ctx, writer); err != nil || expectedSequence == nil {
			return err
		}
		item, err := writer.LockWorkItem(ctx, o.scope.Project, tracker.NativeWorkItemID(o.request.IssueID))
		if err != nil {
			return err
		}
		if o.expectedRevision > 0 && item.Revision != int64(o.expectedRevision) {
			return &hubapi.NativeError{Code: "revision_conflict", Status: http.StatusConflict}
		}
		if item.EventSequence != *expectedSequence {
			return corestore.ErrSerializationFailure
		}
		return nil
	})
}

func (o nativeProgressOwner) Load(ctx context.Context, project, issue, id string) (progresscheck.Record, bool, error) {
	receipt, found, err := o.loadReceipt(ctx, project, issue, id)
	if err != nil || !found {
		return progresscheck.Record{}, false, err
	}
	if err := receipt.Record.Validate(o.request); err != nil {
		return progresscheck.Record{}, false, err
	}
	return receipt.Record, true, nil
}

func (o nativeProgressOwner) replayAfterCleanup(ctx context.Context, originalID, currentID string) (progresscheck.Record, error) {
	if o.publicationHash == "" || len(originalID) != len(currentID) {
		return progresscheck.Record{}, &hubapi.NativeError{Code: "revision_conflict", Status: http.StatusConflict}
	}
	record, found, err := o.retainedPublication(ctx, originalID)
	if err == nil && !found {
		err = &hubapi.NativeError{Code: "revision_conflict", Status: http.StatusConflict}
	}
	return record, err
}

func (o nativeProgressOwner) retainedPublication(ctx context.Context, originalID string) (progresscheck.Record, bool, error) {
	if o.publicationHash == "" {
		return o.Load(ctx, o.request.ProjectID, o.request.IssueID, originalID)
	}
	receipt, found, err := o.loadReceipt(ctx, o.request.ProjectID, o.request.IssueID, originalID)
	if err != nil || !found {
		return progresscheck.Record{}, false, err
	}
	expectedID := originalID
	if receipt.Replay != nil {
		if receipt.Replay.PublicationHash != o.publicationHash {
			return progresscheck.Record{}, false, &hubapi.NativeError{Code: "revision_conflict", Status: http.StatusConflict}
		}
		expectedID = receipt.Replay.RequestAfterCleanupID
	}
	current, sequence, err := o.service.nativeProgressRequestTenant(ctx, o.scope, o.request.IssueID, o.request.PolicyID, o.request.Instance)
	if err != nil {
		return progresscheck.Record{}, false, err
	}
	currentID, err := current.ID()
	if err != nil {
		return progresscheck.Record{}, false, err
	}
	if expectedID != currentID {
		return progresscheck.Record{}, false, &hubapi.NativeError{Code: "revision_conflict", Status: http.StatusConflict}
	}
	if originalID == currentID {
		if err := receipt.Record.Validate(current); err != nil {
			return progresscheck.Record{}, false, err
		}
	}
	if err := o.authorizePublication(ctx, &sequence); err != nil {
		return progresscheck.Record{}, false, err
	}
	return receipt.Record, true, nil
}

func (o nativeProgressOwner) loadReceipt(ctx context.Context, project, issue, id string) (nativeProgressReceipt, bool, error) {
	if project != string(o.scope.Project) || issue != o.request.IssueID {
		return nativeProgressReceipt{}, false, errors.New("progress decision scope differs")
	}
	var receipt eventstore.Receipt
	var found bool
	err := o.service.config.TenantRepository.ReadTenant(ctx, o.scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := o.service.authorizeAccessScope(ctx, reader, o.scope, false); err != nil {
			return err
		}
		if o.scope.Credential.Scope == hubapi.APIScopeWorker {
			identity, err := reader.RunnerIdentityByCredential(ctx, o.scope.Credential.ID)
			if err != nil {
				return err
			}
			if !runnerIdentityMatchesHeldLease(identity, o.scope.Organization, o.scope.Credential.Runner.RunnerID, o.scope.Credential.ID, o.scope.Credential.Runner.MachineID, o.service.config.Clock().UTC()) || identity.State == "disabled" || !slices.Contains(identity.Operations, runnerauth.Read) {
				return hubscope.RunnerUnauthorized()
			}
			allowed, err := reader.RunnerProjectAuthorized(ctx, identity.RunnerID, o.scope.Project, o.service.config.Clock().UTC())
			if err != nil {
				return err
			}
			if !allowed {
				return hubapi.NativeNotFound()
			}
		}
		project, err := reader.Project(ctx, o.scope.Project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		item, err := reader.WorkItem(ctx, o.scope.Project, tracker.NativeWorkItemID(issue))
		if err != nil {
			return err
		}
		if !o.scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		receipt, err = reader.Receipt(ctx, nativeProgressReceiptActor, nativeProgressReceiptOperation(o.scope, issue), id)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		return nativeProgressReceipt{}, false, hubapi.ProjectRecordError(err)
	}
	if !found {
		return nativeProgressReceipt{}, false, nil
	}
	raw, err := eventstore.NewContent(o.service.runDetailStore()).Receipt(o.service.runDetailContext(ctx), receipt)
	if err != nil {
		return nativeProgressReceipt{}, false, err
	}
	var record nativeProgressReceipt
	if err := json.Unmarshal(raw, &record); err != nil || record.RequestID != id || record.ProjectID != project || record.IssueID != issue || record.CommentID == "" || record.Decision.Outcome == progresscheck.SystemDefect && record.DefectIssueID == "" {
		return nativeProgressReceipt{}, false, eventstore.ErrInvalidRecord
	}
	return record, true, nil
}

func (o nativeProgressOwner) Publish(ctx context.Context, record progresscheck.Record) (progresscheck.Record, error) {
	return o.publishTenant(ctx, record, record.RequestID)
}

func (o nativeProgressOwner) lockProgressRelationships(ctx context.Context, writer tenantstore.Writer, record progresscheck.Record, survivor corestore.WorkItemReference) ([]corestore.Relationship, error) {
	if err := writer.LockRelationshipGraph(ctx); err != nil {
		return nil, err
	}
	var removals []corestore.Relationship
	cursor := corestore.WorkItemBlockerCursor{}
	complete := false
	for range claimPreparationLimit {
		blockers, err := writer.WorkItemBlockers(ctx, o.scope.Project, tracker.NativeWorkItemID(record.IssueID), cursor, corestore.MaxPageSize)
		if err != nil {
			return nil, err
		}
		for _, blocker := range blockers[:min(corestore.MaxPageSize, len(blockers))] {
			if blocker.NativeSourceKey != nil && strings.HasPrefix(*blocker.NativeSourceKey, nativeProgressDependencySourcePrefix) {
				removals = append(removals, corestore.Relationship{OrganizationID: o.scope.Organization, ProjectID: o.scope.Project, WorkItemID: tracker.NativeWorkItemID(record.IssueID), RelatedProjectID: blocker.ProjectID, RelatedWorkItemID: blocker.WorkItemID, Type: "dependency", Direction: "blocked_by"})
			}
		}
		if len(blockers) <= corestore.MaxPageSize {
			complete = true
			break
		}
		last := blockers[corestore.MaxPageSize-1]
		cursor = corestore.WorkItemBlockerCursor{ProjectID: last.ProjectID, IssueID: last.IssueID}
	}
	if !complete {
		return nil, corestore.ErrInvalidPage
	}
	projects := []tracker.ProjectID{o.scope.Project}
	items := []corestore.WorkItemReference{{ProjectID: o.scope.Project, WorkItemID: tracker.NativeWorkItemID(record.IssueID)}}
	if survivor.WorkItemID != "" {
		projects = append(projects, survivor.ProjectID)
		items = append(items, survivor)
	}
	for _, relationship := range removals {
		projects = append(projects, relationship.RelatedProjectID)
		items = append(items, corestore.WorkItemReference{ProjectID: relationship.RelatedProjectID, WorkItemID: relationship.RelatedWorkItemID})
	}
	if record.DefectIssueID != "" {
		items = append(items, corestore.WorkItemReference{ProjectID: o.scope.Project, WorkItemID: tracker.NativeWorkItemID(record.DefectIssueID)})
	}
	slices.Sort(projects)
	projects = slices.Compact(projects)
	for _, project := range projects {
		if err := writer.LockWorkItemProject(ctx, project); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(items, func(a, b corestore.WorkItemReference) int {
		if a.ProjectID != b.ProjectID {
			return strings.Compare(string(a.ProjectID), string(b.ProjectID))
		}
		return strings.Compare(string(a.WorkItemID), string(b.WorkItemID))
	})
	items = slices.Compact(items)
	for start := 0; start < len(items); start += corestore.MaxPageSize {
		if _, err := writer.LockWorkItems(ctx, items[start:min(start+corestore.MaxPageSize, len(items))]); err != nil {
			return nil, err
		}
	}
	return removals, nil
}

func (o nativeProgressOwner) publishTenant(ctx context.Context, record progresscheck.Record, currentID string) (progresscheck.Record, error) {
	for {
		if err := ctx.Err(); err != nil {
			return progresscheck.Record{}, err
		}
		published, err := o.publishTenantOnce(ctx, record, currentID)
		if !errors.Is(err, corestore.ErrSerializationFailure) {
			return published, err
		}
		if o.scope.Credential.Scope != hubapi.APIScopeWorker || o.leaseID == "" || o.fencingToken <= 0 {
			return progresscheck.Record{}, &hubapi.NativeError{Code: "revision_conflict", Status: http.StatusConflict}
		}
		current, sequence, err := o.service.nativeProgressRequestTenant(ctx, o.scope, record.IssueID, o.request.PolicyID, o.request.Instance)
		if err != nil {
			return progresscheck.Record{}, err
		}
		currentID, err = current.ID()
		if err != nil {
			return progresscheck.Record{}, err
		}
		o.request = current
		if o.guardRevision != nil {
			o.guardRevision = &sequence
		}
	}
}

func (o nativeProgressOwner) publishTenantOnce(ctx context.Context, record progresscheck.Record, currentID string) (progresscheck.Record, error) {
	if record.ProjectID != string(o.scope.Project) || record.IssueID != o.request.IssueID {
		return progresscheck.Record{}, errors.New("progress decision scope differs")
	}
	if currentID != record.RequestID {
		return o.replayAfterCleanup(ctx, record.RequestID, currentID)
	}
	candidate := record
	candidate.CommentID = "pending"
	if candidate.Decision.Outcome == progresscheck.SystemDefect {
		candidate.DefectIssueID = "pending"
	}
	if err := candidate.ValidatePublication(o.request); err != nil {
		return progresscheck.Record{}, hubapi.NativeInvalid(err.Error())
	}
	var startingSequence int64
	if o.guardRevision != nil {
		current, revision, err := o.service.nativeProgressRequestTenant(ctx, o.scope, record.IssueID, o.request.PolicyID, o.request.Instance)
		if err != nil {
			return progresscheck.Record{}, err
		}
		currentID, err := current.ID()
		if err != nil {
			return progresscheck.Record{}, err
		}
		if currentID != record.RequestID {
			return o.replayAfterCleanup(ctx, record.RequestID, currentID)
		}
		if revision != *o.guardRevision {
			return progresscheck.Record{}, corestore.ErrSerializationFailure
		}
		startingSequence = revision
	} else {
		current, _, err := o.service.readWorkItemRecord(ctx, o.scope, record.IssueID)
		if err != nil {
			return progresscheck.Record{}, err
		}
		startingSequence = current.EventSequence
	}
	if o.expectedRevision > 0 {
		current, _, err := o.service.readWorkItemRecord(ctx, o.scope, record.IssueID)
		if err != nil {
			return progresscheck.Record{}, err
		}
		if current.Revision != int64(o.expectedRevision) {
			return progresscheck.Record{}, &hubapi.NativeError{Code: "revision_conflict", Status: http.StatusConflict}
		}
	}
	if o.scope.Credential.Scope == hubapi.APIScopeWorker {
		if _, err := o.service.validateRunnerLeaseTenant(ctx, o.scope, o.leaseID, o.fencingToken); err != nil {
			return progresscheck.Record{}, err
		}
	}
	if err := o.authorizePublication(ctx, nil); err != nil {
		return progresscheck.Record{}, err
	}
	if retained, found, err := o.retainedPublication(ctx, record.RequestID); err != nil {
		return progresscheck.Record{}, err
	} else if found {
		return retained, nil
	}
	attempts := slices.Clip(o.request.Attempts)
	for _, reference := range record.Decision.RetrievedEvidence {
		if reference.SourceSHA != "" && reference.SourceReceipt == nil {
			return progresscheck.Record{}, hubapi.NativeInvalid("Retrieved source receipt is unavailable")
		}
		if reference.SourceSHA == "" && reference.AttemptID == "" {
			var scopedAttempt bool
			err := o.service.config.TenantRepository.ReadTenant(ctx, o.scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
				comment, err := reader.Comment(ctx, o.scope.Project, tracker.NativeWorkItemID(record.IssueID), reference.ID)
				if err == nil && !comment.Deleted {
					return nil
				}
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				attempt, err := reader.Attempt(ctx, reference.ID)
				if err == nil {
					item, err := reader.WorkItem(ctx, o.scope.Project, tracker.NativeWorkItemID(record.IssueID))
					if err != nil {
						return err
					}
					if attempt.IssueID == item.ID {
						scopedAttempt = true
						return nil
					}
				} else if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				attachment, err := reader.Attachment(ctx, o.scope.Project, reference.ID)
				if err == nil && attachment.Metadata.DeletedAt == nil && attachment.Metadata.WorkItemID == record.IssueID {
					return nil
				}
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				return hubapi.NativeInvalid("Retrieved progress evidence is unavailable for this work item")
			})
			if err != nil {
				return progresscheck.Record{}, fmt.Errorf("read retrieved progress evidence: %w", err)
			}
			if scopedAttempt && record.Decision.Outcome == progresscheck.SystemDefect && !slices.ContainsFunc(attempts, func(a progresscheck.Attempt) bool { return a.ID == reference.ID }) {
				attempt, err := o.service.readNativeAttemptTenant(ctx, o.scope, record.IssueID, reference.ID)
				if err != nil {
					return progresscheck.Record{}, err
				}
				failure := progresscheck.Attempt{ID: attempt.AttemptID, Outcome: attempt.Outcome, Instance: string(attempt.MachineID)}
				if failure.Outcome == "" {
					failure.Outcome = attempt.Status
				}
				if failure.Outcome == "interrupted" {
					failure.Outcome = "abandoned"
				}
				if attempt.TerminalFailure != nil {
					failure.Attribution = attempt.TerminalFailure.Attribution
				}
				attempts = append(attempts, failure)
			}
		}
		if reference.AttemptID != "" {
			attempt, err := o.service.readNativeAttemptTenant(ctx, o.scope, record.IssueID, reference.AttemptID)
			if err != nil {
				return progresscheck.Record{}, err
			}
			if !slices.Contains(progresscheck.AttemptEvidence(attempt), reference) {
				return progresscheck.Record{}, hubapi.NativeInvalid("Retrieved progress receipt is unavailable for this attempt")
			}
		}
	}
	record.Decision.RetrievedEvidence = slices.Clone(record.Decision.RetrievedEvidence)
	for i := range record.Decision.RetrievedEvidence {
		record.Decision.RetrievedEvidence[i].SourceReceipt = nil
	}
	commentScope := o.scope
	actor := commentScope.Actor()
	actor.Kind = "machine"
	actor.Agent = "progress-check"
	commentScope.SourceActor = &actor
	commentScope.Action, commentScope.ActionItem = operatortool.AddComment, record.IssueID
	var defectMutation preparedWorkItemRecordMutation
	body := nativeProgressCommentBody(record)
	if record.Decision.Outcome == progresscheck.SystemDefect {
		defectBody := issueorigin.Stamp(record.Decision.DefectBody, issueorigin.Origin{Kind: "worker", Instance: nativeProgressIncidentInstance(attempts, record.Decision.Evidence), Source: record.RequestID, Fingerprint: issueorigin.Fingerprint("progress-check:" + record.Decision.DefectKey)})
		defectScope := commentScope
		defectScope.ProgressOrigin = &o
		defectScope.Action, defectScope.ActionItem = operatortool.FileIssue, ""
		request := tracker.CreateIssue{
			Mutation: tracker.Mutation{IdempotencyKey: record.RequestID + "/defect", LeaseID: o.leaseID, FencingToken: o.fencingToken},
			Title:    record.Decision.DefectTitle, Body: defectBody, State: "Backlog",
			Provenance: &tracker.Provenance{Provider: "progress-check", ExternalID: record.Decision.DefectKey},
		}
		prepared, defectRaw, err := o.service.prepareWorkItemRecordMutation(ctx, defectScope, hubapi.NativeCommandOptions{OperationID: nativeIssueCreationOperation(defectScope), Feature: "collaboration"}, request.Mutation, request, "")
		if err != nil {
			return progresscheck.Record{}, fmt.Errorf("file progress defect: %w", err)
		}
		var defect tracker.NativeIssue
		if err := json.Unmarshal(defectRaw, &defect); err != nil || defect.WorkItemID == "" {
			return progresscheck.Record{}, eventstore.ErrInvalidRecord
		}
		defectMutation = prepared
		record.DefectIssueID = string(defect.WorkItemID)
		if defect.WebURL == "" {
			defect.WebURL = prepared.browserPath
			if defect.WebURL == "" {
				defect.WebURL, err = o.service.nativeIssueRecordBrowserPath(ctx, o.scope, record.DefectIssueID)
				if err != nil {
					return progresscheck.Record{}, err
				}
			}
		}
		body += fmt.Sprintf("\n\nSystem defect: [#%d](%s)", defect.Number, defect.WebURL)
	}
	commentRequest := tracker.CreateComment{
		Mutation:   tracker.Mutation{IdempotencyKey: record.RequestID, LeaseID: o.leaseID, FencingToken: o.fencingToken},
		Body:       body,
		Provenance: &tracker.Provenance{Provider: "progress-check", ExternalID: record.RequestID},
	}
	commentOptions := hubapi.NativeCommandOptions{OperationID: "POST /api/v2/organizations/" + string(o.scope.Organization) + "/projects/" + string(o.scope.Project) + "/work-items/" + record.IssueID + "/comments", Item: record.IssueID, RequireLease: true, Feature: "collaboration"}
	commentMutation, commentRaw, err := o.service.prepareWorkItemRecordMutation(ctx, commentScope, commentOptions, commentRequest.Mutation, commentRequest, "")
	if err != nil {
		return progresscheck.Record{}, fmt.Errorf("publish progress comment: %w", err)
	}
	var comment tracker.NativeComment
	if err := json.Unmarshal(commentRaw, &comment); err != nil || comment.ID == "" {
		return progresscheck.Record{}, eventstore.ErrInvalidRecord
	}
	record.CommentID = comment.ID
	if comment.Sequence > startingSequence+1 {
		return progresscheck.Record{}, corestore.ErrSerializationFailure
	}
	expectedSequence := startingSequence
	snapshot, err := o.service.readNativeProgressSnapshot(ctx, o.scope, record.IssueID, o.request.PolicyID, o.request.Instance)
	if err != nil {
		return progresscheck.Record{}, err
	}
	currentRequestID, err := snapshot.request.ID()
	if err != nil {
		return progresscheck.Record{}, err
	}
	if currentRequestID != record.RequestID {
		return o.replayAfterCleanup(ctx, record.RequestID, currentRequestID)
	}
	if snapshot.sequence != expectedSequence {
		return progresscheck.Record{}, corestore.ErrSerializationFailure
	}
	stored := nativeProgressReceipt{Record: record}
	if o.publicationHash != "" {
		requestAfterCleanup := snapshot.request
		requestAfterCleanup.Receipts = snapshot.receiptsAfterCleanup
		id, err := requestAfterCleanup.ID()
		if err != nil {
			return progresscheck.Record{}, err
		}
		stored.Replay = &nativeProgressReplay{RequestAfterCleanupID: id, PublicationHash: o.publicationHash}
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return progresscheck.Record{}, err
	}
	content := eventstore.NewContent(o.service.runDetailStore())
	contentCtx := o.service.runDetailContext(ctx)
	hash, err := hubapi.CommandHash(stored)
	if err != nil {
		return progresscheck.Record{}, err
	}
	receipt, err := content.PrepareReceipt(contentCtx, eventstore.Receipt{OrganizationID: o.scope.Organization, ActorID: nativeProgressReceiptActor, Operation: nativeProgressReceiptOperation(o.scope, record.IssueID), CommandKey: record.RequestID, RequestHash: hash, CreatedAt: o.service.config.Clock().UTC()}, raw)
	if err != nil {
		return progresscheck.Record{}, err
	}
	authority, err := o.service.beginProjectRecordAuthority(ctx, commentScope)
	if err != nil {
		return progresscheck.Record{}, err
	}
	receiptWritten := false
	err = o.service.config.TenantRepository.WriteTenant(ctx, o.scope.Organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := lockRunnerCredential(ctx, writer, o.scope); err != nil {
			return err
		}
		if err := o.service.authorizeAccessScope(ctx, writer, commentScope, true); err != nil {
			return err
		}
		lockRecord := record
		if defectMutation.plan.Create && defectMutation.plan.Comment.ID == "" && !defectMutation.plan.Existing {
			lockRecord.DefectIssueID = ""
		}
		removals, err := o.lockProgressRelationships(ctx, writer, lockRecord, snapshot.dependencies.surviving.WorkItemReference)
		if err != nil {
			return err
		}
		checkCompletion := func(want corestore.ProgressDependency) error {
			dependencies, err := readNativeProgressDependencies(ctx, writer, o.scope.Project, tracker.NativeWorkItemID(record.IssueID))
			if err != nil {
				return err
			}
			if dependencies.latest.WorkItemReference != want.WorkItemReference || nativeProgressCompletionID(dependencies.latest) != nativeProgressCompletionID(want) {
				return corestore.ErrSerializationFailure
			}
			return nil
		}
		if err := checkCompletion(snapshot.dependencies.latest); err != nil {
			return err
		}
		if err := o.validateProgressLeaseWriter(ctx, writer); err != nil {
			return err
		}
		current, err := writer.LockWorkItem(ctx, o.scope.Project, tracker.NativeWorkItemID(record.IssueID))
		if err != nil {
			return err
		}
		if current.EventSequence != expectedSequence {
			return corestore.ErrSerializationFailure
		}
		if defectMutation.write != nil {
			if err := defectMutation.write(writer); err != nil {
				return err
			}
		}
		if commentMutation.write != nil {
			if err := commentMutation.write(writer); err != nil {
				return err
			}
		}
		for start := 0; start < len(removals); start += corestore.MaxPageSize - 1 {
			batch := removals[start:min(start+corestore.MaxPageSize-1, len(removals))]
			refs := make([]corestore.WorkItemReference, len(batch))
			for i, row := range batch {
				refs[i] = corestore.WorkItemReference{ProjectID: row.RelatedProjectID, WorkItemID: row.RelatedWorkItemID}
			}
			if err := writer.RemoveOwnedDependencies(ctx, o.scope.Project, tracker.NativeWorkItemID(record.IssueID), nativeProgressDependencySourcePrefix, refs); err != nil {
				return err
			}
		}
		if stored.Replay != nil && (len(removals) > 0 || snapshot.dependencies.latest.WorkItemReference != snapshot.dependencies.surviving.WorkItemReference) {
			if err := checkCompletion(snapshot.dependencies.surviving); err != nil {
				return err
			}
		}
		_, err = writer.PutReceipt(ctx, receipt)
		receiptWritten = err == nil
		return err
	})
	err = errors.Join(err, hubrecords.ReleaseWorkItemAuthority(authority))
	if err != nil {
		if !receiptWritten {
			return progresscheck.Record{}, fmt.Errorf("store progress decision receipt: %w", err)
		}
		if authErr := o.authorizePublication(ctx, nil); authErr != nil {
			return progresscheck.Record{}, errors.Join(fmt.Errorf("store progress decision receipt: %w", err), authErr)
		}
		retained, found, readErr := o.retainedPublication(ctx, record.RequestID)
		if readErr == nil && found {
			return retained, nil
		}
		return progresscheck.Record{}, errors.Join(fmt.Errorf("store progress decision receipt: %w", err), readErr)
	}
	o.service.signalNativeDispatch(o.scope)
	return record, nil
}

func nativeProgressIncidentInstance(attempts []progresscheck.Attempt, evidence []string) string {
	var instance string
	for _, attempt := range attempts {
		if !progresscheck.Failed(attempt.Outcome) || !slices.Contains(evidence, attempt.ID) {
			continue
		}
		if attempt.Attribution != "instance" || attempt.Instance == "" || (instance != "" && instance != attempt.Instance) {
			return "progress-check"
		}
		instance = attempt.Instance
	}
	if instance == "" {
		return "progress-check"
	}
	return instance
}

func nativeProgressCommentBody(record progresscheck.Record) string {
	body := "Progress check: " + string(record.Decision.Outcome) + "\n\n" + record.Decision.Reason
	if record.Decision.Diagnosis != "" {
		body += "\n\nDiagnosis: " + record.Decision.Diagnosis
	}
	if record.Decision.Convergence != "" {
		body += "\n\nConvergence: " + record.Decision.Convergence
	}
	if record.Decision.Outcome == progresscheck.Reroute {
		body += "\n\nNext step: `" + record.Decision.Step + "`"
		if record.Decision.Instruction != "" {
			body += "\n\nInstruction: " + record.Decision.Instruction
		}
	}
	return body + "\n\nEvidence: " + strings.Join(record.Decision.Evidence, ", ")
}

type terminalProgressOwner struct {
	service *Service
	scope   hubscope.NativeScope
	item    tracker.NativeWorkItemID
	attempt string
}

type terminalProgressDetail struct {
	Request progresscheck.Request
	Record  progresscheck.Record
}

type terminalProgressFacts struct {
	attempt      leasestore.Attempt
	receipt      eventstore.Receipt
	sequence     int64
	evidence     int64
	reviewPolicy string
	policy       string
	latest       bool
	landing      bool
}

func (o terminalProgressOwner) facts(ctx context.Context, r tenantstore.Reader, input *progresscheck.Publication, owner *runnerstore.Identity) (terminalProgressFacts, error) {
	var facts terminalProgressFacts
	if err := o.service.authorizeAccessScope(ctx, r, o.scope, input != nil); err != nil {
		return facts, err
	}
	project, err := r.Project(ctx, o.scope.Project)
	if err != nil {
		return facts, err
	}
	if project.DeletedAt != nil {
		return facts, corestore.ErrNotFound
	}
	item, err := r.WorkItem(ctx, o.scope.Project, o.item)
	if err != nil {
		return facts, err
	}
	if !o.scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
		return facts, corestore.ErrNotFound
	}
	facts.attempt, err = r.Attempt(ctx, o.attempt)
	if err != nil {
		return facts, err
	}
	if facts.attempt.IssueID != item.ID {
		return facts, corestore.ErrNotFound
	}
	var fence *tracker.Mutation
	if input != nil {
		fence = &tracker.Mutation{LeaseID: input.LeaseID, FencingToken: input.FencingToken}
	}
	if err := o.service.authorizeProgressRunner(ctx, r, o.scope, facts.attempt, fence, owner); err != nil {
		return facts, err
	}
	if facts.attempt.Status == "running" {
		return facts, hubapi.NativePublicInvalid("Progress verdict requires a recorded terminal attempt")
	}
	facts.receipt, err = r.Receipt(ctx, nativeProgressReceiptActor, o.operation(), o.attempt)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return facts, err
	}
	evidence, ok := r.(eventstore.ProgressEvidenceReader)
	if !ok {
		return facts, hubapi.NativeWorkItemStorageUnavailable()
	}
	facts.evidence, err = evidence.ProgressEvidenceSequence(ctx, o.scope.Project, item.NativeID)
	if err != nil {
		return facts, err
	}
	facts.sequence = item.EventSequence
	latest, err := r.LatestAttempt(ctx, item.ID)
	if err != nil {
		return facts, err
	}
	facts.latest = latest.ID == o.attempt
	policy, err := r.ProjectPolicy(ctx, o.scope.Project)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return facts, err
	}
	facts.policy = policy.PolicyID
	reviewPolicy, err := r.ReviewPolicy(ctx, o.scope.Project)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return facts, err
	}
	facts.reviewPolicy = reviewPolicy.ReviewPolicyID
	landing, err := r.LandingReceipt(ctx, o.attempt)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return facts, err
	}
	facts.landing = err == nil && landing.AttemptID == o.attempt && landing.MergeSHA != ""
	return facts, nil
}

func (s *Service) authorizeProgressRunner(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, attempt leasestore.Attempt, input *tracker.Mutation, owner *runnerstore.Identity) error {
	if scope.Credential.Scope != hubapi.APIScopeWorker {
		if input != nil {
			return hubscope.RunnerUnauthorized()
		}
		return nil
	}
	now := s.config.Clock().UTC()
	var identity runnerstore.Identity
	if owner != nil {
		identity = *owner
	} else {
		var err error
		identity, err = r.RunnerIdentityByCredential(ctx, scope.Credential.ID)
		if err != nil {
			return err
		}
	}
	runner := scope.Credential.Runner
	if !runnerIdentityMatchesHeldLease(identity, scope.Organization, runner.RunnerID, scope.Credential.ID, runner.MachineID, now) || identity.State == "disabled" || !slices.Contains(identity.Operations, runnerauth.Read) {
		return hubscope.RunnerUnauthorized()
	}
	allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, now)
	if err != nil {
		return err
	}
	if !allowed {
		return corestore.ErrNotFound
	}
	if input == nil {
		return nil
	}
	if string(input.LeaseID) != attempt.LeaseID || int64(input.FencingToken) != attempt.FencingToken {
		return tracker.ErrStaleFencingToken
	}
	if owner == nil {
		assigned, err := r.RunnerForLease(ctx, attempt.LeaseID)
		if err != nil {
			return err
		}
		if assigned.RunnerID != identity.RunnerID {
			return hubscope.RunnerUnauthorized()
		}
	}
	if !slices.Contains(identity.Operations, runnerauth.Collaborate) {
		return hubscope.RunnerUnauthorized()
	}
	lease, err := r.Lease(ctx, attempt.LeaseID)
	if err != nil {
		return err
	}
	if lease.OrganizationID != scope.Organization || lease.IssueID != attempt.IssueID || lease.FencingToken != attempt.FencingToken || lease.MachineID != string(identity.MachineID) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) {
		return tracker.ErrStaleFencingToken
	}
	return nil
}

func (o terminalProgressOwner) operation() string {
	return nativeProgressReceiptOperation(o.scope, string(o.item)) + "/terminal"
}

func (o terminalProgressOwner) readFacts(ctx context.Context, input *progresscheck.Publication) (terminalProgressFacts, error) {
	var facts terminalProgressFacts
	repository, ok := o.service.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return facts, hubapi.NativeWorkItemStorageUnavailable()
	}
	err := repository.ReadTenantSnapshot(ctx, o.scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		facts, err = o.facts(ctx, r, input, nil)
		return err
	})
	return facts, hubapi.ProjectRecordError(err)
}

func (o terminalProgressOwner) request(ctx context.Context, facts terminalProgressFacts) (progresscheck.Request, error) {
	attempt, err := o.service.readNativeAttemptTenant(ctx, o.scope, string(o.item), o.attempt)
	if err != nil {
		return progresscheck.Request{}, err
	}
	recovery, err := o.service.readNativeRecoveryFactsTenant(ctx, o.scope, string(o.item), true)
	if err != nil {
		return progresscheck.Request{}, err
	}
	step, disposition := progresscheck.NativeTerminalRoute(recovery, attempt, facts.policy, facts.landing)
	policyID := attempt.PolicyID
	if policyID == "" {
		policyID = facts.policy
	}
	basis, err := hubapi.CommandHash(struct {
		Evidence     int64
		Policy       string
		ReviewPolicy string
		ReviewState  string
		ReviewEpoch  tracker.FencingToken
	}{facts.evidence, facts.policy, facts.reviewPolicy, tracker.NativeStepReviewState(recovery), tracker.NativeStepReviewEpoch(recovery)})
	if err != nil {
		return progresscheck.Request{}, err
	}
	after, err := o.readFacts(ctx, nil)
	if err != nil {
		return progresscheck.Request{}, err
	}
	if !facts.sameEvidence(after) {
		return progresscheck.Request{}, terminalProgressConflict()
	}
	terminal := progresscheck.TerminalEvidence{
		OrganizationID: string(o.scope.Organization), Basis: basis, NextStep: step, Disposition: disposition,
	}
	return progresscheck.NativeTerminalRequest(recovery, attempt, policyID, terminal), nil
}

func (f terminalProgressFacts) sameEvidence(other terminalProgressFacts) bool {
	return f.attempt.Sequence == other.attempt.Sequence && f.attempt.Status == other.attempt.Status &&
		f.sequence == other.sequence && f.evidence == other.evidence && f.policy == other.policy &&
		f.reviewPolicy == other.reviewPolicy && f.latest == other.latest && f.landing == other.landing
}

func terminalProgressConflict() error {
	return &hubapi.NativeError{Code: "revision_conflict", Status: http.StatusConflict}
}

func (o terminalProgressOwner) load(ctx context.Context, receipt eventstore.Receipt) (terminalProgressDetail, error) {
	raw, err := eventstore.NewContent(o.service.runDetailStoreFor(ctx)).Receipt(ctx, receipt)
	if err != nil {
		return terminalProgressDetail{}, err
	}
	var detail terminalProgressDetail
	if err := json.Unmarshal(raw, &detail); err != nil || detail.Record.AttemptID != o.attempt ||
		detail.Record.ProjectID != string(o.scope.Project) || detail.Record.IssueID != string(o.item) ||
		detail.Request.Terminal == nil || detail.Request.Terminal.AttemptID != o.attempt || detail.Request.Terminal.OrganizationID != string(o.scope.Organization) {
		return terminalProgressDetail{}, eventstore.ErrInvalidRecord
	}
	return detail, nil
}

func (o terminalProgressOwner) view(ctx context.Context) (progresscheck.TerminalView, error) {
	facts, err := o.readFacts(ctx, nil)
	if err != nil {
		return progresscheck.TerminalView{}, err
	}
	request, err := o.request(ctx, facts)
	if err != nil {
		return progresscheck.TerminalView{}, err
	}
	id, err := request.ID()
	if err != nil {
		return progresscheck.TerminalView{}, err
	}
	view := progresscheck.TerminalView{Request: request, RequestID: id}
	if facts.receipt.CommandKey != "" {
		detail, err := o.load(ctx, facts.receipt)
		if err != nil {
			return progresscheck.TerminalView{}, err
		}
		view.Record = &detail.Record
		view.Current = facts.latest && detail.Record.RequestID == id
		if !view.Current {
			view.RecordedRequest = &detail.Request
		}
	}
	return view, nil
}

func (o terminalProgressOwner) publish(ctx context.Context, input progresscheck.Publication) (progresscheck.Record, error) {
	facts, err := o.readFacts(ctx, &input)
	if err != nil {
		return progresscheck.Record{}, err
	}
	hash, err := hubapi.CommandHash(struct {
		RequestID string
		Revision  string
		Decision  progresscheck.Decision
	}{input.RequestID, input.EvidenceRevision, input.Decision})
	if err != nil {
		return progresscheck.Record{}, err
	}
	if facts.receipt.CommandKey != "" {
		return o.replay(ctx, facts.receipt, hash)
	}
	request, err := o.request(ctx, facts)
	if err != nil {
		return progresscheck.Record{}, err
	}
	id, err := request.ID()
	if err != nil {
		return progresscheck.Record{}, err
	}
	if id != input.RequestID || request.EvidenceRevision != input.EvidenceRevision {
		return progresscheck.Record{}, terminalProgressConflict()
	}
	if err := input.Decision.ValidateTerminal(request); err != nil {
		return progresscheck.Record{}, hubapi.NativePublicInvalid(err.Error())
	}
	record := progresscheck.Record{
		AttemptID: o.attempt, RequestID: id, ProjectID: string(o.scope.Project), IssueID: string(o.item),
		EvidenceRevision: request.EvidenceRevision, Decision: input.Decision, NextStep: request.Terminal.NextStep, Disposition: request.Terminal.Disposition,
	}
	raw, err := json.Marshal(terminalProgressDetail{Request: request, Record: record})
	if err != nil {
		return progresscheck.Record{}, err
	}
	receipt, err := eventstore.NewContent(o.service.runDetailStoreFor(ctx)).PrepareReceipt(ctx, eventstore.Receipt{
		OrganizationID: o.scope.Organization, ActorID: nativeProgressReceiptActor, Operation: o.operation(), CommandKey: o.attempt,
		RequestHash: hash, CreatedAt: o.service.config.Clock().UTC(),
	}, raw)
	if err != nil {
		return progresscheck.Record{}, err
	}
	var replay eventstore.Receipt
	err = o.service.config.TenantRepository.WriteTenant(ctx, o.scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := o.service.authorizeAccessScope(ctx, w, o.scope, true); err != nil {
			return err
		}
		runner, err := w.RunnerForLease(ctx, string(input.LeaseID))
		if err != nil {
			return err
		}
		if _, err := w.LockWorkItem(ctx, o.scope.Project, o.item); err != nil {
			return err
		}
		locker, ok := w.(leasestore.FencedLeaseLocker)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if _, err := locker.LockLease(ctx, string(input.LeaseID)); err != nil {
			return err
		}
		current, err := o.facts(ctx, w, &input, &runner)
		if err != nil {
			return err
		}
		if current.receipt.CommandKey != "" {
			replay = current.receipt
			return nil
		}
		if !facts.sameEvidence(current) {
			return terminalProgressConflict()
		}
		owner, ok := w.(leasestore.ProgressVerdictWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		verdict := leasestore.ProgressVerdict{
			OrganizationID: o.scope.Organization, ProjectID: o.scope.Project, AttemptID: o.attempt, AttemptSequence: facts.attempt.Sequence,
			EvidenceRevision: request.EvidenceRevision, AttemptedStep: request.Terminal.Step, Success: *input.Decision.Succeeded,
			Outcome: string(input.Decision.Outcome), Reason: input.Decision.Reason,
		}
		if record.NextStep != "" {
			verdict.NextStep = &record.NextStep
		}
		if record.Disposition != "" {
			verdict.Disposition = &record.Disposition
		}
		if err := owner.RecordProgressVerdict(ctx, verdict); err != nil {
			return err
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if err != nil {
		return progresscheck.Record{}, hubapi.ProjectRecordError(err)
	}
	if replay.CommandKey != "" {
		return o.replay(ctx, replay, hash)
	}
	return record, nil
}

func (o terminalProgressOwner) replay(ctx context.Context, receipt eventstore.Receipt, hash string) (progresscheck.Record, error) {
	if receipt.RequestHash != hash {
		return progresscheck.Record{}, terminalProgressConflict()
	}
	detail, err := o.load(ctx, receipt)
	return detail.Record, err
}
