package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func healthIssueScope(organization tracker.OrganizationID, project tracker.ProjectID) nativeScope {
	return nativeScope{organization: organization, project: project, sourceActor: nativeIntegrationActor("health_detector")}
}

func healthIssueRequest(f healthFinding) (tracker.CreateIssue, error) {
	evidence, err := json.MarshalIndent(f.Evidence, "", "  ")
	if err != nil {
		return tracker.CreateIssue{}, err
	}
	title := fmt.Sprintf("%s on %s %s", f.Signal, f.Subject.Kind, f.Subject.ID)
	request := tracker.CreateIssue{
		Title:    "fix(instance): " + title,
		State:    "Todo",
		Priority: new(1),
		Labels:   []string{"infrastructure"},
	}
	body := fmt.Sprintf("Finding: `%s`\nClass: %s\nSubject: %s %s\nOpened: %s\nObserved: %s\n\n%s\n\nNext action: %s\n\nEvidence:\n```json\n%s\n```\n\n```detent-agent\nschema: 1\neffort: high\n```", f.ID, f.Class, f.Subject.Kind, f.Subject.ID, formatHubTime(f.OpenedAt), formatHubTime(f.LastSeenAt), f.Summary, f.NextAction, evidence)
	request.Body = issueorigin.Stamp(body, issueorigin.Origin{Kind: "audit", Instance: "health_detector", Source: f.ID, Fingerprint: f.Fingerprint})
	return request, nil
}

func reportHealthFindings(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, now time.Time, projects ...tracker.ProjectID) ([]tracker.NativeIssue, error) {
	var project tracker.ProjectID
	if len(projects) > 0 {
		project = projects[0]
	}
	type occurrence struct {
		finding          healthFinding
		project          tracker.ProjectID
		item             tracker.NativeWorkItemID
		evidence         string
		reportedEvidence string
		resolved         sql.NullString
		reportedResolved sql.NullString
	}
	rows, err := tx.QueryContext(ctx, `SELECT f.id,f.fingerprint,f.signal,f.class,f.subject_json,f.opened_at,f.last_seen_at,f.resolved_at,f.summary,f.next_action,f.evidence_json,
 p.id,COALESCE(l.work_item_id,''),COALESCE(l.reported_evidence_json,''),l.reported_resolved_at
 FROM health_findings f JOIN projects p ON p.organization_id=f.organization_id AND p.deleted_at IS NULL AND p.profile='native'
 AND (EXISTS(SELECT 1 FROM json_each(f.projects_json) WHERE value=p.id) OR EXISTS(SELECT 1 FROM health_finding_issues WHERE finding_id=f.id AND project_id=p.id))
 LEFT JOIN health_finding_issues l ON l.finding_id=f.id AND l.project_id=p.id
 WHERE f.organization_id=? AND f.class='instance' AND (?='' OR p.id=?) AND (?='' OR f.resolved_at IS NOT NULL)
 AND (f.resolved_at IS NULL OR (l.work_item_id IS NOT NULL AND (l.reported_resolved_at IS NOT f.resolved_at
 OR (f.class='instance' AND EXISTS(SELECT 1 FROM issues i JOIN workflow_states ws ON ws.id=i.workflow_state_id
 WHERE i.organization_id=f.organization_id AND i.project_id=p.id AND i.native_id=l.work_item_id AND i.archived=0 AND ws.detent_state='Todo')))))
 ORDER BY f.rowid,p.id LIMIT ?`, organization, project, project, project, healthReadLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	occurrences := []occurrence{}
	for rows.Next() {
		var o occurrence
		var subject, opened, seen string
		f := &o.finding
		if err := rows.Scan(&f.ID, &f.Fingerprint, &f.Signal, &f.Class, &subject, &opened, &seen, &o.resolved, &f.Summary, &f.NextAction, &o.evidence, &o.project, &o.item, &o.reportedEvidence, &o.reportedResolved); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(subject), &f.Subject); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(o.evidence), &f.Evidence); err != nil {
			return nil, err
		}
		f.Evidence = scopeHealthEvidence(f.Evidence, o.project)
		evidence, err := json.Marshal(f.Evidence)
		if err != nil {
			return nil, err
		}
		o.evidence = string(evidence)
		if f.OpenedAt, err = parseTimeValue(opened); err != nil {
			return nil, err
		}
		if f.LastSeenAt, err = parseTimeValue(seen); err != nil {
			return nil, err
		}
		occurrences = append(occurrences, o)
		if len(occurrences) > healthReadLimit {
			return nil, errHealthReadLimit
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	created := []tracker.NativeIssue{}
	for _, o := range occurrences {
		if o.item != "" && !o.resolved.Valid && o.resolved == o.reportedResolved && o.evidence == o.reportedEvidence {
			continue
		}
		scope := healthIssueScope(organization, o.project)
		var reported tracker.NativeIssue
		if o.resolved.Valid {
			issue, _, err := readNativeIssue(ctx, tx, scope, string(o.item))
			if err != nil {
				return nil, err
			}
			if !issue.Terminal && o.resolved != o.reportedResolved {
				body := fmt.Sprintf("## Health finding resolved\n\nFinding: `%s`\nResolved: %s", o.finding.ID, o.resolved.String)
				if _, err := insertNativeComment(ctx, tx, scope, issue, body, nil, now); err != nil {
					return nil, err
				}
			}
			reported = issue
		} else {
			request, err := healthIssueRequest(o.finding)
			if err != nil {
				return nil, err
			}
			project, err := validateNativeIssueDraft(ctx, tx, scope, request)
			if err != nil {
				return nil, err
			}
			valid := false
			for _, state := range project.States {
				if state.Name == request.State && !state.Terminal && !state.OperatorOnly && state.Dispatchable {
					valid = true
				}
			}
			if !valid {
				return nil, nativeInvalid("Health reporting requires dispatchable Todo")
			}
			issue, err := reportNativeMachineIssueTx(ctx, tx, scope, request, now)
			if err != nil {
				return nil, err
			}
			o.item = issue.WorkItemID
			reported = issue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO health_finding_issues(finding_id,project_id,work_item_id,reported_evidence_json,reported_resolved_at) VALUES(?,?,?,?,?)
 ON CONFLICT(finding_id,project_id) DO UPDATE SET work_item_id=excluded.work_item_id,reported_evidence_json=excluded.reported_evidence_json,reported_resolved_at=excluded.reported_resolved_at`, o.finding.ID, o.project, o.item, o.evidence, o.resolved); err != nil {
			return nil, err
		}
		resolved := ""
		if o.resolved.Valid {
			resolved = o.resolved.String
		}
		reported, err = reconcileHealthIssue(ctx, tx, scope, reported, o.finding, resolved, now)
		if err != nil {
			return nil, err
		}
		if !o.resolved.Valid && !reported.PublicationReused {
			created = append(created, reported)
		}
	}
	return created, nil
}

func reconcileHealthIssue(ctx context.Context, tx *sql.Tx, scope nativeScope, issue tracker.NativeIssue, finding healthFinding, resolved string, now time.Time) (tracker.NativeIssue, error) {
	if finding.Class != "instance" || issue.Archived || issue.Terminal || (resolved != "" && issue.State != "Todo") || (resolved == "" && issue.State != "Backlog") {
		return issue, nil
	}
	origin, ok := issueorigin.Parse(issue.Body)
	if !ok || origin.Kind != "audit" || origin.Instance != "health_detector" || origin.Fingerprint != finding.Fingerprint || issue.Actor != scope.actor() || issue.Provenance != nil || len(issue.Dependencies) != 0 || len(issue.Assignees) != 0 || !slices.Equal(issue.Labels, []string{"infrastructure"}) {
		return issue, nil
	}
	if issue.IssueContract != nil && (issue.IssueContract.HumanAction != "" || issue.IssueContract.ReturnState != "" || len(issue.IssueContract.ConfirmedSections) != 0) {
		return issue, nil
	}
	var owned, protected, unresolved bool
	err := tx.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM health_findings f JOIN health_finding_issues l ON l.finding_id=f.id
 WHERE f.organization_id=? AND f.id=? AND f.fingerprint=? AND f.class='instance' AND l.project_id=? AND l.work_item_id=?),
 EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND json_extract(actor_json,'$.kind')='human')
 OR EXISTS(SELECT 1 FROM native_comments WHERE organization_id=? AND project_id=? AND work_item_id=? AND (json_extract(actor_json,'$.kind')='human' OR json_extract(edited_by_json,'$.kind')='human' OR instr(body,?)>0))
 OR EXISTS(SELECT 1 FROM conversations c JOIN conversation_questions q ON q.conversation_id=c.id WHERE c.organization_id=? AND c.project_id=? AND c.work_item_id=? AND q.status='pending')
 OR EXISTS(SELECT 1 FROM leases l JOIN issues i ON i.id=l.issue_id LEFT JOIN native_attempts a ON a.lease_id=l.lease_id WHERE i.organization_id=? AND i.project_id=? AND i.native_id=? AND (l.released_at IS NULL OR a.status='running'))
 OR EXISTS(SELECT 1 FROM change_issue_links WHERE organization_id=? AND project_id=? AND work_item_id=?),
 EXISTS(SELECT 1 FROM health_findings f WHERE f.organization_id=? AND f.fingerprint=? AND f.resolved_at IS NULL
 AND (EXISTS(SELECT 1 FROM json_each(f.projects_json) WHERE value=?) OR EXISTS(SELECT 1 FROM health_finding_issues l WHERE l.finding_id=f.id AND l.project_id=? AND l.work_item_id=?)))`,
		scope.organization, origin.Source, origin.Fingerprint, scope.project, issue.WorkItemID,
		scope.organization, scope.project, issue.WorkItemID, scope.organization, scope.project, issue.WorkItemID, "```detent-park",
		scope.organization, scope.project, issue.WorkItemID, scope.organization, scope.project, issue.WorkItemID,
		scope.organization, scope.project, issue.WorkItemID, scope.organization, finding.Fingerprint, scope.project, scope.project, issue.WorkItemID).Scan(&owned, &protected, &unresolved)
	if err != nil || !owned || protected || (resolved != "" && unresolved) || (resolved == "" && !unresolved) {
		return issue, err
	}
	var initial tracker.NativeIssue
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT record_json FROM collaboration_versions WHERE organization_id=? AND project_id=? AND record_id=? AND revision=1`, scope.organization, scope.project, issue.WorkItemID).Scan(&raw)
	if err != nil {
		return issue, err
	}
	if err := json.Unmarshal([]byte(raw), &initial); err != nil {
		return issue, err
	}
	if initial.Actor != scope.actor() || initial.Body != issue.Body || initial.Title != issue.Title {
		return issue, nil
	}
	source, err := readNativeSourceCheckpoint(ctx, tx, scope, string(issue.WorkItemID))
	if err != nil || source.Checkpoint != nil {
		return issue, err
	}
	var comment string
	err = tx.QueryRowContext(ctx, `SELECT body FROM native_comments WHERE organization_id=? AND project_id=? AND work_item_id=? AND NOT (json_extract(actor_json,'$.kind')='integration' AND json_extract(actor_json,'$.principal_id')='health_detector') ORDER BY sequence DESC LIMIT 1`, scope.organization, scope.project, issue.WorkItemID).Scan(&comment)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return issue, err
	}
	if strings.Contains(comment, "```detent-park") {
		return issue, nil
	}
	if signal, ok := workpad.SignalFromComment(comment, "", string(scope.project)); ok && (signal.Invalid != nil || signal.Status == workpad.StatusBlocked || signal.HumanAction != "" || len(signal.Blockers) != 0) {
		return issue, nil
	}
	target := "Backlog"
	if resolved == "" {
		target = "Todo"
		var actor, data string
		err := tx.QueryRowContext(ctx, `SELECT actor_json,data_json FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='workflow.transitioned' ORDER BY sequence DESC LIMIT 1`, scope.organization, scope.project, issue.WorkItemID).Scan(&actor, &data)
		if errors.Is(err, sql.ErrNoRows) {
			return issue, nil
		}
		if err != nil {
			return issue, err
		}
		var lastActor tracker.Actor
		var transition tracker.CollaborationData
		if err := json.Unmarshal([]byte(actor), &lastActor); err != nil {
			return issue, err
		}
		if err := json.Unmarshal([]byte(data), &transition); err != nil {
			return issue, err
		}
		if lastActor != scope.actor() || transition.ToState != "Backlog" || transition.Operation != "health_detector" || transition.Revision != issue.Revision {
			return issue, nil
		}
	}
	project, err := readNativeProject(ctx, tx, scope)
	if err != nil {
		return issue, err
	}
	if !slices.ContainsFunc(project.States, func(state tracker.NativeState) bool {
		return state.Name == target && !state.Terminal && !state.OperatorOnly && state.Dispatchable == (target == "Todo")
	}) {
		return issue, nil
	}
	if err := validateNativeWorkflowTransition(project.States, issue.State, target, apiScopeWorker); err != nil {
		return issue, nil
	}
	if err := requireNativeEdit(issue, issue.Revision); err != nil {
		return issue, err
	}
	detail, err := json.Marshal(struct {
		FindingID   string `json:"finding_id"`
		Fingerprint string `json:"fingerprint"`
		ObservedAt  string `json:"observed_at"`
		ResolvedAt  string `json:"resolved_at,omitempty"`
	}{finding.ID, finding.Fingerprint, formatHubTime(finding.LastSeenAt), resolved})
	if err != nil {
		return issue, err
	}
	from := issue.State
	issue.State = target
	return persistNativeIssue(ctx, tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: from, ToState: target, Reason: "worker_progress", Operation: "health_detector", ReasonDetail: string(detail)}, now)
}
