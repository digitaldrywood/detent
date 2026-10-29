package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeProgressOwner struct {
	service       *Service
	scope         nativeScope
	request       progresscheck.Request
	leaseID       tracker.LeaseID
	fencingToken  tracker.FencingToken
	guardSequence *int64
}

func (o nativeProgressOwner) Load(ctx context.Context, project, issue, id string) (progresscheck.Record, bool, error) {
	if project != string(o.scope.project) || issue != o.request.IssueID {
		return progresscheck.Record{}, false, errors.New("progress decision scope differs")
	}
	return readNativeProgress(ctx, o.service.database.reader, o.scope, issue, id)
}

func readNativeProgress(ctx context.Context, q nativeQueryer, scope nativeScope, issue, id string) (progresscheck.Record, bool, error) {
	record := progresscheck.Record{RequestID: id, ProjectID: string(scope.project), IssueID: issue}
	err := q.QueryRowContext(ctx, `SELECT evidence_revision, outcome, step, reason, defect_key, defect_title, defect_body, comment_id, defect_issue_id
 FROM native_progress_decisions WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND request_id = ?`, scope.organization, scope.project, issue, id).Scan(
		&record.EvidenceRevision, &record.Decision.Outcome, &record.Decision.Step, &record.Decision.Reason,
		&record.Decision.DefectKey, &record.Decision.DefectTitle, &record.Decision.DefectBody, &record.CommentID, &record.DefectIssueID)
	if errors.Is(err, sql.ErrNoRows) {
		return progresscheck.Record{}, false, nil
	}
	if err != nil {
		return progresscheck.Record{}, false, fmt.Errorf("read progress decision: %w", err)
	}
	rows, err := q.QueryContext(ctx, `SELECT evidence_id FROM native_progress_evidence
 WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND request_id = ? ORDER BY position LIMIT ?`, scope.organization, scope.project, issue, id, progresscheck.MaxReceipts+1)
	if err != nil {
		return progresscheck.Record{}, false, fmt.Errorf("read progress evidence: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var evidence string
		if err := rows.Scan(&evidence); err != nil {
			return progresscheck.Record{}, false, err
		}
		record.Decision.Evidence = append(record.Decision.Evidence, evidence)
	}
	return record, true, rows.Err()
}

func (o nativeProgressOwner) Publish(ctx context.Context, record progresscheck.Record) (progresscheck.Record, error) {
	if record.ProjectID != string(o.scope.project) || record.IssueID != o.request.IssueID {
		return progresscheck.Record{}, errors.New("progress decision scope differs")
	}
	candidate := record
	candidate.CommentID = "pending"
	if candidate.Decision.Outcome == progresscheck.SystemDefect {
		candidate.DefectIssueID = "pending"
	}
	if err := candidate.Validate(o.request); err != nil {
		return progresscheck.Record{}, err
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
	if o.guardSequence != nil {
		current, err := progressSequence(ctx, tx, o.scope, record.IssueID)
		if err != nil {
			return progresscheck.Record{}, err
		}
		if current != *o.guardSequence {
			return progresscheck.Record{}, nativeInvalid("Progress evidence changed")
		}
	}
	issue, _, err := readNativeIssue(ctx, tx, o.scope, record.IssueID)
	if err != nil {
		return progresscheck.Record{}, err
	}
	body := "Progress check: " + string(record.Decision.Outcome) + "\n\n" + record.Decision.Reason
	if record.Decision.Step != "" {
		body += "\n\nNext step: `" + record.Decision.Step + "`"
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
	commentScope.sourceActor = &actor
	comment, err := insertNativeComment(ctx, tx, commentScope, issue, body, nil, now)
	if err != nil {
		return progresscheck.Record{}, fmt.Errorf("publish progress comment: %w", err)
	}
	record.CommentID = comment.ID
	if _, err := tx.ExecContext(ctx, "UPDATE native_comments SET progress_request_id=? WHERE id=?", record.RequestID, comment.ID); err != nil {
		return progresscheck.Record{}, fmt.Errorf("bind progress comment: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO native_progress_decisions
 (organization_id, project_id, work_item_id, request_id, evidence_revision, outcome, step, reason, defect_key, defect_title, defect_body, comment_id, defect_issue_id, created_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, o.scope.organization, o.scope.project, record.IssueID, record.RequestID, record.EvidenceRevision,
		record.Decision.Outcome, record.Decision.Step, record.Decision.Reason, record.Decision.DefectKey, record.Decision.DefectTitle, record.Decision.DefectBody, record.CommentID, record.DefectIssueID, now.UTC().Format("2006-01-02T15:04:05.000000000Z"))
	if err != nil {
		return progresscheck.Record{}, fmt.Errorf("write progress decision: %w", err)
	}
	for position, evidence := range record.Decision.Evidence {
		_, err = tx.ExecContext(ctx, `INSERT INTO native_progress_evidence (organization_id, project_id, work_item_id, request_id, position, evidence_id) VALUES (?, ?, ?, ?, ?, ?)`, o.scope.organization, o.scope.project, record.IssueID, record.RequestID, position, evidence)
		if err != nil {
			return progresscheck.Record{}, fmt.Errorf("write progress evidence: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return progresscheck.Record{}, fmt.Errorf("commit progress decision: %w", err)
	}
	return record, nil
}
