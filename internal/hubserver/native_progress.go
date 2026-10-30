package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/progresscheck"
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

func (o nativeProgressOwner) Load(ctx context.Context, project, issue, id string) (progresscheck.Record, bool, error) {
	if project != string(o.scope.project) || issue != o.request.IssueID {
		return progresscheck.Record{}, false, errors.New("progress decision scope differs")
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
	ctx = o.service.runDetailContext(ctx)
	return prepareContentMutation(ctx, o.service.database.reader, func() (progresscheck.Record, error) {
		return o.publishPrepared(ctx, record)
	})
}

func (o nativeProgressOwner) publishPrepared(ctx context.Context, record progresscheck.Record) (progresscheck.Record, error) {
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
		defectScope := nativeScope{credential: apiCredential{Scope: apiScopeAdmin}, project: tracker.ProjectID(issueorigin.DetentCloudProjectID), sourceActor: o.scope.sourceActor}
		err := tx.QueryRowContext(ctx, "SELECT organization_id FROM projects WHERE id=? AND deleted_at IS NULL", defectScope.project).Scan(&defectScope.organization)
		if errors.Is(err, sql.ErrNoRows) {
			body += "\n\nPlatform defect filing unavailable: the platform project is not provisioned. Diagnosis retained on this item."
		} else if err != nil {
			return progresscheck.Record{}, fmt.Errorf("resolve platform project: %w", err)
		} else {
			actor := o.scope.actor()
			actor.Kind, actor.Agent = "machine", "progress-check"
			defectScope.sourceActor = &actor
			defectBody := issueorigin.Stamp(record.Decision.DefectBody, issueorigin.Origin{Kind: "worker", Instance: "progress-check", Source: record.RequestID, Fingerprint: issueorigin.Fingerprint("progress-check:" + record.Decision.DefectKey)})
			defect, err := createNativeMachineIntakeTx(ctx, tx, defectScope, tracker.CreateIssue{Title: record.Decision.DefectTitle, Body: defectBody, State: "Backlog"}, now, false)
			if err != nil {
				return progresscheck.Record{}, fmt.Errorf("file progress defect: %w", err)
			}
			record.DefectIssueID = string(defect.WorkItemID)
			defect, _, err = readNativeIssue(ctx, tx, defectScope, record.DefectIssueID)
			if err != nil {
				return progresscheck.Record{}, fmt.Errorf("read platform defect link: %w", err)
			}
			body += fmt.Sprintf("\n\nSystem defect: [#%d](%s)", defect.Number, defect.WebURL)
		}
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM issue_dependencies WHERE dependent_issue_id=(SELECT id FROM issues WHERE organization_id=? AND project_id=? AND native_id=?) AND provenance='progress_check'`, o.scope.organization, o.scope.project, record.IssueID); err != nil {
		return progresscheck.Record{}, err
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
