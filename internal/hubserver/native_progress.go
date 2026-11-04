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
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
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

func (o nativeProgressOwner) validateProgressLeaseWriter(ctx context.Context, writer tenantstore.Writer) error {
	if o.scope.credential.Scope != apiScopeWorker {
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
		return workItemStorageUnavailable()
	}
	lease, err := locker.LockLease(ctx, string(o.leaseID))
	if err != nil {
		return err
	}
	assigned, err := writer.RunnerForLease(ctx, string(o.leaseID))
	if err != nil {
		return err
	}
	now := o.service.config.now().UTC()
	if lease.OrganizationID != o.scope.organization || lease.FencingToken != int64(o.fencingToken) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) || lease.IssueID != item.ID {
		return tracker.ErrStaleFencingToken
	}
	if o.scope.credential.EntryKey != nil || credential.OrganizationID != o.scope.organization || credential.Scope != string(apiScopeWorker) || credential.Kind != "service" || !credential.Valid(o.scope.credential.Hash, now) || identity.RunnerID != assigned.RunnerID || !runnerIdentityMatchesHeldLease(identity, o.scope.organization, o.scope.credential.Runner.RunnerID, credential.ID, tracker.MachineID(lease.MachineID), now) || identity.State == "disabled" || !slices.Contains(identity.Operations, runnerauth.Claim) || !slices.Contains(identity.Operations, runnerauth.Collaborate) {
		return runnerUnauthorized()
	}
	allowed, err := writer.RunnerProjectAuthorized(ctx, identity.RunnerID, o.scope.project, now)
	if err != nil {
		return err
	}
	if !allowed {
		return nativeNotFoundAt("lease_runner_credential")
	}
	pinned, err := writer.LeasePolicy(ctx, o.leaseID)
	if err != nil {
		return err
	}
	if pinned.OrganizationID != o.scope.organization || pinned.ProjectID != o.scope.project || pinned.LeaseID != o.leaseID || pinned.PolicyID == "" || pinned.PolicyID != o.request.PolicyID || o.request.Instance != lease.MachineID {
		return policyMismatch("Progress request must match the policy and instance pinned to its lease")
	}
	if o.expectedRevision > 0 && item.Revision != int64(o.expectedRevision) {
		return &nativeError{Code: "revision_conflict", status: http.StatusConflict}
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
	if o.service.config.TenantRepository != nil {
		var receipt eventstore.Receipt
		var found bool
		err := o.service.config.TenantRepository.ReadTenant(ctx, o.scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			if err := o.service.authorizeAccessScope(ctx, reader, o.scope, false); err != nil {
				return err
			}
			if o.scope.credential.Scope == apiScopeWorker {
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
					return nativeNotFound()
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
			if !o.scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(item.ActorPrincipalID)}) {
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
			return progresscheck.Record{}, false, projectRecordError(err)
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
	return readNativeProgress(ctx, o.service.database.reader, o.scope, issue, id)
}

func readNativeProgress(ctx context.Context, q nativeQueryer, scope nativeScope, issue, id string) (progresscheck.Record, bool, error) {
	record := progresscheck.Record{RequestID: id, ProjectID: string(scope.project), IssueID: issue}
	err := q.QueryRowContext(ctx, `SELECT evidence_revision, outcome, step, reason, defect_key, defect_title, defect_body, comment_id, defect_issue_id, diagnosis, instruction, convergence
 FROM native_progress_decisions WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND request_id = ?`, scope.organization, scope.project, issue, id).Scan(
		&record.EvidenceRevision, &record.Decision.Outcome, &record.Decision.Step, &record.Decision.Reason,
		&record.Decision.DefectKey, &record.Decision.DefectTitle, &record.Decision.DefectBody, &record.CommentID, &record.DefectIssueID, &record.Decision.Diagnosis, &record.Decision.Instruction, &record.Decision.Convergence)
	if errors.Is(err, sql.ErrNoRows) {
		return progresscheck.Record{}, false, nil
	}
	if err != nil {
		return progresscheck.Record{}, false, fmt.Errorf("read progress decision: %w", err)
	}
	rows, err := q.QueryContext(ctx, `SELECT evidence_id, retrieved, retrieved_attempt_id, retrieved_source_sha FROM native_progress_evidence
 WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND request_id = ? ORDER BY position LIMIT ?`, scope.organization, scope.project, issue, id, progresscheck.MaxReceipts+1)
	if err != nil {
		return progresscheck.Record{}, false, fmt.Errorf("read progress evidence: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var evidence string
		var retrieved bool
		var attemptID, sourceSHA string
		if err := rows.Scan(&evidence, &retrieved, &attemptID, &sourceSHA); err != nil {
			return progresscheck.Record{}, false, err
		}
		record.Decision.Evidence = append(record.Decision.Evidence, evidence)
		if retrieved {
			record.Decision.RetrievedEvidence = append(record.Decision.RetrievedEvidence, progresscheck.EvidenceReference{ID: evidence, AttemptID: attemptID, SourceSHA: sourceSHA})
		}
	}
	return record, true, rows.Err()
}

func (o nativeProgressOwner) Publish(ctx context.Context, record progresscheck.Record) (progresscheck.Record, error) {
	if o.service.config.TenantRepository != nil {
		return o.publishTenant(ctx, record)
	}
	if record.ProjectID != string(o.scope.project) || record.IssueID != o.request.IssueID {
		return progresscheck.Record{}, errors.New("progress decision scope differs")
	}
	candidate := record
	candidate.CommentID = "pending"
	if candidate.Decision.Outcome == progresscheck.SystemDefect {
		candidate.DefectIssueID = "pending"
	}
	if err := candidate.ValidatePublication(o.request); err != nil {
		return progresscheck.Record{}, err
	}
	for _, reference := range record.Decision.RetrievedEvidence {
		if reference.SourceSHA != "" {
			continue
		}
		if reference.AttemptID != "" {
			attempt, err := readNativeAttemptRecord(o.service.runDetailContext(ctx), o.service.database.reader, o.scope, record.IssueID, reference.AttemptID, o.service.config.now())
			if err != nil {
				return progresscheck.Record{}, fmt.Errorf("read cited progress attempt: %w", err)
			}
			if !slices.Contains(progresscheck.AttemptEvidence(attempt), reference) {
				return progresscheck.Record{}, nativeInvalid("Retrieved progress receipt is unavailable for this attempt")
			}
			continue
		}
		id := reference.ID
		var exists bool
		if err := o.service.database.reader.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM native_comments WHERE id=? AND organization_id=? AND project_id=? AND work_item_id=? AND deleted=0)
 OR EXISTS(SELECT 1 FROM native_attempts WHERE id=? AND organization_id=? AND project_id=? AND work_item_id=?)
 OR EXISTS(SELECT 1 FROM attachments WHERE id=? AND organization_id=? AND project_id=? AND work_item_id=? AND deleted_at IS NULL)`,
			id, o.scope.organization, o.scope.project, record.IssueID,
			id, o.scope.organization, o.scope.project, record.IssueID,
			id, o.scope.organization, o.scope.project, record.IssueID).Scan(&exists); err != nil {
			return progresscheck.Record{}, fmt.Errorf("read retrieved progress evidence: %w", err)
		}
		if !exists {
			return progresscheck.Record{}, nativeInvalid("Retrieved progress evidence is unavailable for this work item")
		}
	}
	record.Decision.RetrievedEvidence = slices.Clone(record.Decision.RetrievedEvidence)
	for i := range record.Decision.RetrievedEvidence {
		record.Decision.RetrievedEvidence[i].SourceReceipt = nil
	}
	tx, err := o.service.database.db.BeginTx(ctx, nil)
	if err != nil {
		return progresscheck.Record{}, err
	}
	defer func() { _ = tx.Rollback() }()
	now := o.service.config.now()
	if o.scope.credential.Scope == apiScopeWorker {
		if _, err := validateRunnerLeaseTx(ctx, tx, o.scope, o.leaseID, o.fencingToken, now); err != nil {
			return progresscheck.Record{}, err
		}
		lease, found, err := readLeaseByID(ctx, tx, o.leaseID)
		if err != nil {
			return progresscheck.Record{}, err
		}
		var internalID tracker.WorkItemID
		if err := tx.QueryRowContext(ctx, "SELECT id FROM issues WHERE organization_id=? AND project_id=? AND native_id=?", o.scope.organization, o.scope.project, record.IssueID).Scan(&internalID); err != nil {
			return progresscheck.Record{}, err
		}
		if !found || lease.issueID != internalID {
			return progresscheck.Record{}, nativeNotFound()
		}
		var policy string
		if err := tx.QueryRowContext(ctx, "SELECT policy_id FROM lease_policies WHERE lease_id=?", o.leaseID).Scan(&policy); err != nil {
			return progresscheck.Record{}, err
		}
		if policy != o.request.PolicyID || string(lease.session.Machine.ID) != o.request.Instance {
			return progresscheck.Record{}, nativeInvalid("Progress request execution binding differs")
		}
		var version string
		if err := tx.QueryRowContext(ctx, "SELECT version FROM machines WHERE organization_id=? AND id=?", o.scope.organization, o.request.Instance).Scan(&version); err != nil {
			return progresscheck.Record{}, err
		}
		if version != o.request.InstanceVersion {
			return progresscheck.Record{}, nativeInvalid("Progress request instance changed")
		}
	}
	if o.guardRevision != nil {
		current, err := progressRevision(ctx, tx, o.scope, record.IssueID)
		if err != nil {
			return progresscheck.Record{}, err
		}
		if current != *o.guardRevision {
			return progresscheck.Record{}, &nativeError{Code: "revision_conflict", status: http.StatusConflict}
		}
	}
	retained, found, err := readNativeProgress(ctx, tx, o.scope, record.IssueID, record.RequestID)
	if err != nil {
		return progresscheck.Record{}, err
	}
	if found {
		if err := retained.Validate(o.request); err != nil {
			return progresscheck.Record{}, err
		}
		return retained, nil
	}
	issue, _, err := readNativeIssue(ctx, tx, o.scope, record.IssueID)
	if err != nil {
		return progresscheck.Record{}, err
	}
	if o.expectedRevision > 0 && issue.Revision != o.expectedRevision {
		return progresscheck.Record{}, &nativeError{Code: "revision_conflict", status: http.StatusConflict}
	}
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
	body += "\n\nEvidence: " + strings.Join(record.Decision.Evidence, ", ")
	if record.Decision.Outcome == progresscheck.SystemDefect {
		defectBody := issueorigin.Stamp(record.Decision.DefectBody, issueorigin.Origin{Kind: "worker", Instance: "progress-check", Source: record.RequestID, Fingerprint: issueorigin.Fingerprint("progress-check:" + record.Decision.DefectKey)})
		defect, err := createNativeMachineIntakeTx(ctx, tx, o.scope, tracker.CreateIssue{Title: record.Decision.DefectTitle, Body: defectBody, State: "Backlog", Mutation: tracker.Mutation{LeaseID: o.leaseID, FencingToken: o.fencingToken}}, now, false)
		if err != nil {
			return progresscheck.Record{}, fmt.Errorf("file progress defect: %w", err)
		}
		record.DefectIssueID = string(defect.WorkItemID)
		defect, _, err = readNativeIssue(ctx, tx, o.scope, record.DefectIssueID)
		if err != nil {
			return progresscheck.Record{}, fmt.Errorf("read progress defect link: %w", err)
		}
		body += fmt.Sprintf("\n\nSystem defect: [#%d](%s)", defect.Number, defect.WebURL)
	}
	commentScope := o.scope
	actor := o.scope.actor()
	actor.Kind = "machine"
	actor.Agent = "progress-check"
	commentScope.sourceActor = &actor
	if _, err := tx.ExecContext(ctx, `UPDATE issues SET progress_comment_sequence=COALESCE((SELECT max(sequence) FROM (SELECT sequence,progress_request_id FROM native_comments WHERE organization_id=? AND project_id=? AND work_item_id=? ORDER BY sequence DESC LIMIT ?) WHERE progress_request_id=''),0) WHERE organization_id=? AND project_id=? AND native_id=? AND progress_comment_sequence=0`, o.scope.organization, o.scope.project, record.IssueID, progresscheck.MaxComments, o.scope.organization, o.scope.project, record.IssueID); err != nil {
		return progresscheck.Record{}, err
	}
	comment, err := insertNativeComment(ctx, tx, commentScope, issue, body, nil, now)
	if err != nil {
		return progresscheck.Record{}, fmt.Errorf("publish progress comment: %w", err)
	}
	record.CommentID = comment.ID
	if _, err := tx.ExecContext(ctx, "UPDATE native_comments SET progress_request_id=? WHERE id=?", record.RequestID, comment.ID); err != nil {
		return progresscheck.Record{}, fmt.Errorf("bind progress comment: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM issue_dependencies WHERE dependent_issue_id=(SELECT id FROM issues WHERE organization_id=? AND project_id=? AND native_id=?) AND provenance='progress_check' AND source_version<>?`, o.scope.organization, o.scope.project, record.IssueID, record.RequestID); err != nil {
		return progresscheck.Record{}, err
	}
	if record.Decision.Outcome == progresscheck.SystemDefect {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issue_dependencies d JOIN issues b ON b.id=d.blocker_issue_id JOIN issues i ON i.id=d.dependent_issue_id WHERE i.organization_id=? AND i.project_id=? AND i.native_id=? AND b.native_id=?)`, o.scope.organization, o.scope.project, record.IssueID, record.DefectIssueID).Scan(&exists); err != nil {
			return progresscheck.Record{}, err
		}
		if !exists {
			current, _, err := readNativeIssue(ctx, tx, o.scope, record.IssueID)
			if err != nil {
				return progresscheck.Record{}, err
			}
			if _, err := changeNativeDependencyTx(ctx, tx, commentScope, record.IssueID, tracker.DependencyMutation{ExpectedRevision: current.Revision, Operation: "add", RelationshipType: "dependency", RelatedWorkItemID: tracker.NativeWorkItemID(record.DefectIssueID)}, now); err != nil {
				return progresscheck.Record{}, fmt.Errorf("link progress dependency: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE issue_dependencies SET provenance='progress_check',source_version=? WHERE dependent_issue_id=(SELECT id FROM issues WHERE organization_id=? AND project_id=? AND native_id=?) AND blocker_issue_id=(SELECT id FROM issues WHERE organization_id=? AND project_id=? AND native_id=?)`, record.RequestID, o.scope.organization, o.scope.project, record.IssueID, o.scope.organization, o.scope.project, record.DefectIssueID); err != nil {
				return progresscheck.Record{}, err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO native_progress_decisions
 (organization_id, project_id, work_item_id, request_id, evidence_revision, outcome, step, reason, defect_key, defect_title, defect_body, comment_id, defect_issue_id, created_at, diagnosis, instruction, convergence)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, o.scope.organization, o.scope.project, record.IssueID, record.RequestID, record.EvidenceRevision,
		record.Decision.Outcome, record.Decision.Step, record.Decision.Reason, record.Decision.DefectKey, record.Decision.DefectTitle, record.Decision.DefectBody, record.CommentID, record.DefectIssueID, now.UTC().Format("2006-01-02T15:04:05.000000000Z"), record.Decision.Diagnosis, record.Decision.Instruction, record.Decision.Convergence)
	if err != nil {
		return progresscheck.Record{}, fmt.Errorf("write progress decision: %w", err)
	}
	for position, evidence := range record.Decision.Evidence {
		var retrieved bool
		var attemptID, sourceSHA string
		for _, reference := range record.Decision.RetrievedEvidence {
			if reference.ID == evidence {
				retrieved, attemptID, sourceSHA = true, reference.AttemptID, reference.SourceSHA
				break
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO native_progress_evidence (organization_id, project_id, work_item_id, request_id, position, evidence_id, retrieved, retrieved_attempt_id, retrieved_source_sha) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, o.scope.organization, o.scope.project, record.IssueID, record.RequestID, position, evidence, retrieved, attemptID, sourceSHA)
		if err != nil {
			return progresscheck.Record{}, fmt.Errorf("write progress evidence: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return progresscheck.Record{}, fmt.Errorf("commit progress decision: %w", err)
	}
	return record, nil
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
	if record.ProjectID != string(o.scope.project) || record.IssueID != o.request.IssueID {
		return progresscheck.Record{}, errors.New("progress decision scope differs")
	}
	candidate := record
	candidate.CommentID = "pending"
	if candidate.Decision.Outcome == progresscheck.SystemDefect {
		candidate.DefectIssueID = "pending"
	}
	if err := candidate.ValidatePublication(o.request); err != nil {
		return progresscheck.Record{}, err
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
		if currentID != record.RequestID || revision != *o.guardRevision {
			return progresscheck.Record{}, &nativeError{Code: "revision_conflict", status: http.StatusConflict}
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
			return progresscheck.Record{}, &nativeError{Code: "revision_conflict", status: http.StatusConflict}
		}
	}
	if o.scope.credential.Scope == apiScopeWorker {
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
			return progresscheck.Record{}, nativeInvalid("Retrieved source receipt is unavailable")
		}
		if reference.AttemptID != "" {
			var found bool
			recovery, err := o.service.readNativeRecoveryTenant(ctx, o.scope, record.IssueID)
			if err != nil {
				return progresscheck.Record{}, err
			}
			for _, attempt := range recovery.Attempts {
				if attempt.AttemptID == reference.AttemptID && slices.Contains(progresscheck.AttemptEvidence(attempt), reference) {
					found = true
					break
				}
			}
			if !found {
				return progresscheck.Record{}, nativeInvalid("Retrieved progress receipt is unavailable for this attempt")
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
	body := nativeProgressCommentBody(record)
	if record.Decision.Outcome == progresscheck.SystemDefect {
		defectBody := issueorigin.Stamp(record.Decision.DefectBody, issueorigin.Origin{Kind: "worker", Instance: "progress-check", Source: record.RequestID, Fingerprint: issueorigin.Fingerprint("progress-check:" + record.Decision.DefectKey)})
		defectScope := commentScope
		defectScope.progressOrigin = &o
		defectRaw, err := o.service.createNativeIssueCommand(ctx, defectScope, tracker.CreateIssue{
			Mutation: tracker.Mutation{IdempotencyKey: record.RequestID + "/defect", LeaseID: o.leaseID, FencingToken: o.fencingToken},
			Title:    record.Decision.DefectTitle, Body: defectBody, State: "Backlog",
			Provenance: &tracker.Provenance{Provider: "progress-check", ExternalID: record.Decision.DefectKey},
		})
		if err != nil {
			return progresscheck.Record{}, fmt.Errorf("file progress defect: %w", err)
		}
		var defect tracker.NativeIssue
		if err := json.Unmarshal(defectRaw, &defect); err != nil || defect.WorkItemID == "" {
			return progresscheck.Record{}, eventstore.ErrInvalidRecord
		}
		record.DefectIssueID = string(defect.WorkItemID)
		body += fmt.Sprintf("\n\nSystem defect: [#%d](%s)", defect.Number, defect.WebURL)
	}
	commentRaw, err := o.service.createNativeCommentCommand(ctx, commentScope, record.IssueID, tracker.CreateComment{
		Mutation:   tracker.Mutation{IdempotencyKey: record.RequestID, LeaseID: o.leaseID, FencingToken: o.fencingToken},
		Body:       body,
		Provenance: &tracker.Provenance{Provider: "progress-check", ExternalID: record.RequestID},
	})
	if err != nil {
		return progresscheck.Record{}, fmt.Errorf("publish progress comment: %w", err)
	}
	var comment tracker.NativeComment
	if err := json.Unmarshal(commentRaw, &comment); err != nil || comment.ID == "" {
		return progresscheck.Record{}, eventstore.ErrInvalidRecord
	}
	record.CommentID = comment.ID
	if comment.Sequence > startingSequence+1 {
		return progresscheck.Record{}, &nativeError{Code: "revision_conflict", status: http.StatusConflict}
	}
	expectedSequence := max(startingSequence, comment.Sequence)
	currentRequest, currentSequence, err := o.service.nativeProgressRequestTenant(ctx, o.scope, record.IssueID, o.request.PolicyID, o.request.Instance)
	if err != nil {
		return progresscheck.Record{}, err
	}
	currentRequestID, err := currentRequest.ID()
	if err != nil {
		return progresscheck.Record{}, err
	}
	if currentRequestID != record.RequestID || currentSequence != expectedSequence {
		return progresscheck.Record{}, &nativeError{Code: "revision_conflict", status: http.StatusConflict}
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return progresscheck.Record{}, err
	}
	content := eventstore.NewContent(o.service.runDetailStore())
	contentCtx := o.service.runDetailContext(ctx)
	hash, err := nativeCommandHash(record)
	if err != nil {
		return progresscheck.Record{}, err
	}
	receipt, err := content.PrepareReceipt(contentCtx, eventstore.Receipt{OrganizationID: o.scope.organization, ActorID: nativeProgressReceiptActor, Operation: nativeProgressReceiptOperation(o.scope, record.IssueID), CommandKey: record.RequestID, RequestHash: hash, CreatedAt: o.service.config.now().UTC()}, raw)
	if err != nil {
		return progresscheck.Record{}, err
	}
	receiptWritten := false
	err = o.service.config.TenantRepository.WriteTenant(ctx, o.scope.organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := o.service.authorizeAccessScope(ctx, writer, commentScope, true); err != nil {
			return err
		}
		removals, err := o.lockProgressRelationships(ctx, writer, record)
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
			return &nativeError{Code: "revision_conflict", status: http.StatusConflict}
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
			if err := writer.SetRelationship(ctx, corestore.Relationship{OrganizationID: o.scope.organization, ProjectID: o.scope.project, WorkItemID: tracker.NativeWorkItemID(record.IssueID), RelatedProjectID: o.scope.project, RelatedWorkItemID: tracker.NativeWorkItemID(record.DefectIssueID), Type: "dependency"}, "add", o.service.config.now().UTC()); err != nil {
				return err
			}
		}
		_, err = writer.PutReceipt(ctx, receipt)
		receiptWritten = err == nil
		return err
	})
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

func expireNativeProgressDependencies(ctx context.Context, tx *sql.Tx, scope nativeScope, item, eventType string, data tracker.CollaborationData) error {
	if eventType == "run.finished" {
		_, err := tx.ExecContext(ctx, `UPDATE issues SET progress_evidence_revision=progress_evidence_revision+1 WHERE organization_id=? AND project_id=? AND native_id=?`, scope.organization, scope.project, item)
		return err
	}
	if eventType != "issue.edited" && eventType != "comment.created" && eventType != "comment.edited" && eventType != "run.checkpointed" && !strings.HasPrefix(eventType, "change.") {
		return nil
	}
	actor := scope.actor()
	if eventType == "comment.created" && actor.Kind == "machine" && actor.Agent == "progress-check" {
		return nil
	}
	humanComment := ""
	if actor.Kind == "human" && (eventType == "comment.created" || eventType == "comment.edited") {
		humanComment = data.CommentID
	}
	if _, err := tx.ExecContext(ctx, `UPDATE issues SET progress_evidence_revision=progress_evidence_revision+1,progress_comment_sequence=CASE WHEN ?='comment.created' THEN event_sequence+1 ELSE progress_comment_sequence END,progress_human_comment_id=CASE WHEN ?<>'' THEN ? ELSE progress_human_comment_id END WHERE organization_id=? AND project_id=? AND native_id=?`, eventType, humanComment, humanComment, scope.organization, scope.project, item); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM issue_dependencies WHERE dependent_issue_id=(SELECT id FROM issues WHERE organization_id=? AND project_id=? AND native_id=?) AND provenance='progress_check'`, scope.organization, scope.project, item)
	return err
}
