package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeProgressOwner struct {
	service          *Service
	scope            nativeScope
	request          progresscheck.Request
	leaseID          tracker.LeaseID
	fencingToken     tracker.FencingToken
	guardRevision    *int64
	expectedRevision tracker.Revision
}

const nativeProgressReceiptActor = "native-progress"

func nativeProgressReceiptOperation(scope nativeScope, issue string) string {
	return "native progress/" + string(scope.project) + "/" + issue
}

func nativeProgressActor(scope nativeScope) bool {
	return scope.sourceActor != nil && scope.sourceActor.Kind == "machine" && scope.sourceActor.Agent == "progress-check"
}

func (o *nativeProgressOwner) AuthorizeProgressDefect(ctx context.Context, w tenantstore.Writer, owner any, scope nativeScope, leaseID tracker.LeaseID, fencingToken tracker.FencingToken) error {
	if owner != any(o.service) || o.scope.organization != scope.organization || o.scope.project != scope.project || o.scope.credential.ID != scope.credential.ID || o.leaseID != leaseID || o.fencingToken != fencingToken {
		return runnerUnauthorized()
	}
	return o.validateProgressLeaseWriter(ctx, w)
}

func (o nativeProgressOwner) validateProgressLeaseWriter(ctx context.Context, writer tenantstore.Writer) error {
	if o.scope.credential.Scope != hubapi.APIScopeWorker {
		return nil
	}
	credential, err := writer.AccessCredential(ctx, o.scope.credential.ID)
	if errors.Is(err, corestore.ErrNotFound) {
		return runnerUnauthorized()
	}
	if err != nil {
		return err
	}
	identity, err := writer.RunnerIdentityByCredential(ctx, credential.ID)
	if err != nil {
		return err
	}
	item, err := writer.LockWorkItem(ctx, o.scope.project, tracker.NativeWorkItemID(o.request.IssueID))
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
	now := o.service.config.now().UTC()
	if lease.OrganizationID != o.scope.organization || lease.FencingToken != int64(o.fencingToken) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) || lease.IssueID != item.ID {
		return tracker.ErrStaleFencingToken
	}
	if o.scope.credential.EntryKey != nil || credential.OrganizationID != o.scope.organization || credential.Scope != string(hubapi.APIScopeWorker) || credential.Kind != "service" || !credential.Valid(o.scope.credential.Hash, now) || identity.RunnerID != assigned.RunnerID || !runnerIdentityMatchesHeldLease(identity, o.scope.organization, o.scope.credential.Runner.RunnerID, credential.ID, tracker.MachineID(lease.MachineID), now) || identity.State == "disabled" || !slices.Contains(identity.Operations, runnerauth.Claim) || !slices.Contains(identity.Operations, runnerauth.Collaborate) {
		return runnerUnauthorized()
	}
	allowed, err := writer.RunnerProjectAuthorized(ctx, identity.RunnerID, o.scope.project, now)
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
	if pinned.OrganizationID != o.scope.organization || pinned.ProjectID != o.scope.project || pinned.LeaseID != o.leaseID || pinned.PolicyID == "" || pinned.PolicyID != o.request.PolicyID || o.request.Instance != lease.MachineID {
		return hubapi.PolicyMismatch("Progress request must match the policy and instance pinned to its lease")
	}
	if o.expectedRevision > 0 && item.Revision != int64(o.expectedRevision) {
		return &hubapi.NativeError{Code: "revision_conflict", Status: http.StatusConflict}
	}
	return nil
}

func (o nativeProgressOwner) authorizePublication(ctx context.Context) error {
	return o.service.config.TenantRepository.WriteTenant(ctx, o.scope.organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := o.service.authorizeAccessScope(ctx, writer, o.scope, true); err != nil {
			return err
		}
		return o.validateProgressLeaseWriter(ctx, writer)
	})
}

func (o nativeProgressOwner) Load(ctx context.Context, project, issue, id string) (progresscheck.Record, bool, error) {
	if project != string(o.scope.project) || issue != o.request.IssueID {
		return progresscheck.Record{}, false, errors.New("progress decision scope differs")
	}
	var receipt eventstore.Receipt
	var found bool
	err := o.service.config.TenantRepository.ReadTenant(ctx, o.scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := o.service.authorizeAccessScope(ctx, reader, o.scope, false); err != nil {
			return err
		}
		if o.scope.credential.Scope == hubapi.APIScopeWorker {
			identity, err := reader.RunnerIdentityByCredential(ctx, o.scope.credential.ID)
			if err != nil {
				return err
			}
			if !runnerIdentityMatchesHeldLease(identity, o.scope.organization, o.scope.credential.Runner.RunnerID, o.scope.credential.ID, o.scope.credential.Runner.MachineID, o.service.config.now().UTC()) || identity.State == "disabled" || !slices.Contains(identity.Operations, runnerauth.Read) {
				return runnerUnauthorized()
			}
			allowed, err := reader.RunnerProjectAuthorized(ctx, identity.RunnerID, o.scope.project, o.service.config.now().UTC())
			if err != nil {
				return err
			}
			if !allowed {
				return hubapi.NativeNotFound()
			}
		}
		project, err := reader.Project(ctx, o.scope.project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		item, err := reader.WorkItem(ctx, o.scope.project, tracker.NativeWorkItemID(issue))
		if err != nil {
			return err
		}
		if !o.scope.reporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
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
		return progresscheck.Record{}, false, hubapi.ProjectRecordError(err)
	}
	if !found {
		return progresscheck.Record{}, false, nil
	}
	raw, err := eventstore.NewContent(o.service.runDetailStore()).Receipt(o.service.runDetailContext(ctx), receipt)
	if err != nil {
		return progresscheck.Record{}, false, err
	}
	var record progresscheck.Record
	if err := json.Unmarshal(raw, &record); err != nil || record.RequestID != id || record.ProjectID != project || record.IssueID != issue {
		return progresscheck.Record{}, false, eventstore.ErrInvalidRecord
	}
	if err := record.Validate(o.request); err != nil {
		return progresscheck.Record{}, false, err
	}
	return record, true, nil
}

func (o nativeProgressOwner) Publish(ctx context.Context, record progresscheck.Record) (progresscheck.Record, error) {
	return o.publishTenant(ctx, record)
}

func (o nativeProgressOwner) lockProgressRelationships(ctx context.Context, writer tenantstore.Writer, record progresscheck.Record) ([]corestore.Relationship, error) {
	if err := writer.LockRelationshipGraph(ctx); err != nil {
		return nil, err
	}
	var removals []corestore.Relationship
	cursor := corestore.WorkItemBlockerCursor{}
	complete := false
	for range claimPreparationLimit {
		blockers, err := writer.WorkItemBlockers(ctx, o.scope.project, tracker.NativeWorkItemID(record.IssueID), cursor, corestore.MaxPageSize)
		if err != nil {
			return nil, err
		}
		for _, blocker := range blockers[:min(corestore.MaxPageSize, len(blockers))] {
			if blocker.NativeSourceKey != nil && strings.HasPrefix(*blocker.NativeSourceKey, "progress-check:") {
				removals = append(removals, corestore.Relationship{OrganizationID: o.scope.organization, ProjectID: o.scope.project, WorkItemID: tracker.NativeWorkItemID(record.IssueID), RelatedProjectID: blocker.ProjectID, RelatedWorkItemID: blocker.WorkItemID, Type: "dependency", Direction: "blocked_by"})
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
	projects := []tracker.ProjectID{o.scope.project}
	items := []corestore.WorkItemReference{{ProjectID: o.scope.project, WorkItemID: tracker.NativeWorkItemID(record.IssueID)}}
	for _, relationship := range removals {
		projects = append(projects, relationship.RelatedProjectID)
		items = append(items, corestore.WorkItemReference{ProjectID: relationship.RelatedProjectID, WorkItemID: relationship.RelatedWorkItemID})
	}
	if record.DefectIssueID != "" {
		items = append(items, corestore.WorkItemReference{ProjectID: o.scope.project, WorkItemID: tracker.NativeWorkItemID(record.DefectIssueID)})
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

func (o nativeProgressOwner) publishTenant(ctx context.Context, record progresscheck.Record) (progresscheck.Record, error) {
	for {
		if err := ctx.Err(); err != nil {
			return progresscheck.Record{}, err
		}
		published, err := o.publishTenantOnce(ctx, record)
		if !errors.Is(err, corestore.ErrSerializationFailure) {
			return published, err
		}
		if o.scope.credential.Scope != hubapi.APIScopeWorker || o.leaseID == "" || o.fencingToken <= 0 {
			return progresscheck.Record{}, &hubapi.NativeError{Code: "revision_conflict", Status: http.StatusConflict}
		}
		current, sequence, err := o.service.nativeProgressRequestTenant(ctx, o.scope, record.IssueID, o.request.PolicyID, o.request.Instance)
		if err != nil {
			return progresscheck.Record{}, err
		}
		id, err := current.ID()
		if err != nil {
			return progresscheck.Record{}, err
		}
		if id != record.RequestID {
			return progresscheck.Record{}, &hubapi.NativeError{Code: "revision_conflict", Status: http.StatusConflict}
		}
		if o.guardRevision != nil {
			o.guardRevision = &sequence
		}
	}
}

func (o nativeProgressOwner) publishTenantOnce(ctx context.Context, record progresscheck.Record) (progresscheck.Record, error) {
	if record.ProjectID != string(o.scope.project) || record.IssueID != o.request.IssueID {
		return progresscheck.Record{}, errors.New("progress decision scope differs")
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
			return progresscheck.Record{}, &hubapi.NativeError{Code: "revision_conflict", Status: http.StatusConflict}
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
	if o.scope.credential.Scope == hubapi.APIScopeWorker {
		if _, err := o.service.validateRunnerLeaseTenant(ctx, o.scope, o.leaseID, o.fencingToken); err != nil {
			return progresscheck.Record{}, err
		}
	}
	if err := o.authorizePublication(ctx); err != nil {
		return progresscheck.Record{}, err
	}
	if retained, found, err := o.Load(ctx, record.ProjectID, record.IssueID, record.RequestID); err != nil {
		return progresscheck.Record{}, err
	} else if found {
		return retained, nil
	}
	for _, reference := range record.Decision.RetrievedEvidence {
		if reference.SourceSHA != "" && reference.SourceReceipt == nil {
			return progresscheck.Record{}, hubapi.NativeInvalid("Retrieved source receipt is unavailable")
		}
		if reference.SourceSHA == "" && reference.AttemptID == "" {
			err := o.service.config.TenantRepository.ReadTenant(ctx, o.scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
				comment, err := reader.Comment(ctx, o.scope.project, tracker.NativeWorkItemID(record.IssueID), reference.ID)
				if err == nil && !comment.Deleted {
					return nil
				}
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				attempt, err := reader.Attempt(ctx, reference.ID)
				if err == nil {
					item, err := reader.WorkItem(ctx, o.scope.project, tracker.NativeWorkItemID(record.IssueID))
					if err != nil {
						return err
					}
					if attempt.IssueID == item.ID {
						return nil
					}
				} else if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				attachment, err := reader.Attachment(ctx, o.scope.project, reference.ID)
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
	actor := commentScope.actor()
	actor.Kind = "machine"
	actor.Agent = "progress-check"
	commentScope.sourceActor = &actor
	commentScope.action, commentScope.actionItem = operatortool.AddComment, record.IssueID
	var defectMutation preparedWorkItemRecordMutation
	body := nativeProgressCommentBody(record)
	if record.Decision.Outcome == progresscheck.SystemDefect {
		defectBody := issueorigin.Stamp(record.Decision.DefectBody, issueorigin.Origin{Kind: "worker", Instance: "progress-check", Source: record.RequestID, Fingerprint: issueorigin.Fingerprint("progress-check:" + record.Decision.DefectKey)})
		defectScope := commentScope
		defectScope.progressOrigin = &o
		defectScope.action, defectScope.actionItem = operatortool.FileIssue, ""
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
	commentOptions := hubapi.NativeCommandOptions{OperationID: "POST /api/v2/organizations/" + string(o.scope.organization) + "/projects/" + string(o.scope.project) + "/work-items/" + record.IssueID + "/comments", Item: record.IssueID, RequireLease: true, Feature: "collaboration"}
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
	currentRequest, currentSequence, err := o.service.nativeProgressRequestTenant(ctx, o.scope, record.IssueID, o.request.PolicyID, o.request.Instance)
	if err != nil {
		return progresscheck.Record{}, err
	}
	currentRequestID, err := currentRequest.ID()
	if err != nil {
		return progresscheck.Record{}, err
	}
	if currentRequestID != record.RequestID {
		return progresscheck.Record{}, &hubapi.NativeError{Code: "revision_conflict", Status: http.StatusConflict}
	}
	if currentSequence != expectedSequence {
		return progresscheck.Record{}, corestore.ErrSerializationFailure
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return progresscheck.Record{}, err
	}
	content := eventstore.NewContent(o.service.runDetailStore())
	contentCtx := o.service.runDetailContext(ctx)
	hash, err := hubapi.CommandHash(record)
	if err != nil {
		return progresscheck.Record{}, err
	}
	receipt, err := content.PrepareReceipt(contentCtx, eventstore.Receipt{OrganizationID: o.scope.organization, ActorID: nativeProgressReceiptActor, Operation: nativeProgressReceiptOperation(o.scope, record.IssueID), CommandKey: record.RequestID, RequestHash: hash, CreatedAt: o.service.config.now().UTC()}, raw)
	if err != nil {
		return progresscheck.Record{}, err
	}
	authority, err := o.service.beginProjectRecordAuthority(ctx, commentScope)
	if err != nil {
		return progresscheck.Record{}, err
	}
	receiptWritten := false
	replayed := false
	err = o.service.config.TenantRepository.WriteTenant(ctx, o.scope.organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := o.service.authorizeAccessScope(ctx, writer, commentScope, true); err != nil {
			return err
		}
		if _, err := writer.Receipt(ctx, nativeProgressReceiptActor, nativeProgressReceiptOperation(o.scope, record.IssueID), record.RequestID); err == nil {
			replayed = true
			return nil
		} else if !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		lockRecord := record
		if defectMutation.plan.create && defectMutation.plan.comment.ID == "" && !defectMutation.plan.existing {
			lockRecord.DefectIssueID = ""
		}
		removals, err := o.lockProgressRelationships(ctx, writer, lockRecord)
		if err != nil {
			return err
		}
		if err := o.validateProgressLeaseWriter(ctx, writer); err != nil {
			return err
		}
		current, err := writer.LockWorkItem(ctx, o.scope.project, tracker.NativeWorkItemID(record.IssueID))
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
			if err := writer.RemoveOwnedDependencies(ctx, o.scope.project, tracker.NativeWorkItemID(record.IssueID), "progress-check:", refs); err != nil {
				return err
			}
		}
		if record.DefectIssueID != "" {
			if err := writer.SetRelationship(ctx, corestore.Relationship{OrganizationID: o.scope.organization, ProjectID: o.scope.project, WorkItemID: tracker.NativeWorkItemID(record.IssueID), RelatedProjectID: o.scope.project, RelatedWorkItemID: tracker.NativeWorkItemID(record.DefectIssueID), Type: "dependency", Provenance: "progress_check"}, "add", o.service.config.now().UTC()); err != nil {
				return err
			}
		}
		_, err = writer.PutReceipt(ctx, receipt)
		receiptWritten = err == nil
		return err
	})
	err = errors.Join(err, releaseWorkItemAuthority(authority))
	if err == nil && replayed {
		retained, found, readErr := o.Load(ctx, record.ProjectID, record.IssueID, record.RequestID)
		if readErr != nil {
			return progresscheck.Record{}, readErr
		}
		if !found {
			return progresscheck.Record{}, eventstore.ErrInvalidRecord
		}
		return retained, nil
	}
	if err != nil {
		if !receiptWritten {
			return progresscheck.Record{}, fmt.Errorf("store progress decision receipt: %w", err)
		}
		if authErr := o.authorizePublication(ctx); authErr != nil {
			return progresscheck.Record{}, errors.Join(fmt.Errorf("store progress decision receipt: %w", err), authErr)
		}
		retained, found, readErr := o.Load(ctx, record.ProjectID, record.IssueID, record.RequestID)
		if readErr == nil && found {
			return retained, nil
		}
		return progresscheck.Record{}, errors.Join(fmt.Errorf("store progress decision receipt: %w", err), readErr)
	}
	o.service.signalNativeDispatch(o.scope)
	return record, nil
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
