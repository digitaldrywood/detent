package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func healthIssueScope(organization tracker.OrganizationID, project tracker.ProjectID) nativeScope {
	return nativeScope{organization: organization, project: project, sourceActor: &tracker.Actor{Kind: "system", PrincipalID: "health_detector"}}
}

func healthIssueRequest(f healthFinding) (tracker.CreateIssue, error) {
	evidence, err := json.MarshalIndent(f.Evidence, "", "  ")
	if err != nil {
		return tracker.CreateIssue{}, err
	}
	title := fmt.Sprintf("%s on %s %s", f.Signal, f.Subject.Kind, f.Subject.ID)
	request := tracker.CreateIssue{Title: "intake: " + title, State: "Backlog"}
	if f.Class == "instance" {
		request.Title = "fix(instance): " + title
		request.State = "Todo"
		request.Priority = new(1)
		request.Labels = []string{"infrastructure"}
	}
	body := fmt.Sprintf("Finding: `%s`\nClass: %s\nSubject: %s %s\nOpened: %s\nObserved: %s\n\n%s\n\nNext action: %s\n\nEvidence:\n```json\n%s\n```\n\n```detent-agent\nschema: 1\neffort: high\n```", f.ID, f.Class, f.Subject.Kind, f.Subject.ID, formatHubTime(f.OpenedAt), formatHubTime(f.LastSeenAt), f.Summary, f.NextAction, evidence)
	request.Body = issueorigin.Stamp(body, issueorigin.Origin{Kind: "audit", Instance: "health_detector", Source: f.ID, Fingerprint: f.Fingerprint})
	return request, nil
}

func reportHealthFindings(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, now time.Time) ([]tracker.NativeIssue, error) {
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
 FROM health_findings f JOIN projects p ON p.organization_id=f.organization_id AND p.profile='native'
 AND (EXISTS(SELECT 1 FROM json_each(f.projects_json) WHERE value=p.id) OR EXISTS(SELECT 1 FROM health_finding_issues WHERE finding_id=f.id AND project_id=p.id))
 LEFT JOIN health_finding_issues l ON l.finding_id=f.id AND l.project_id=p.id
 WHERE f.organization_id=? AND (f.resolved_at IS NULL OR (l.work_item_id IS NOT NULL AND l.reported_resolved_at IS NOT f.resolved_at))
 ORDER BY f.rowid,p.id LIMIT ?`, organization, healthReadLimit+1)
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
		if o.item != "" && o.resolved == o.reportedResolved && o.evidence == o.reportedEvidence {
			continue
		}
		scope := healthIssueScope(organization, o.project)
		if o.resolved.Valid {
			issue, _, err := readNativeIssue(ctx, tx, scope, string(o.item))
			if err != nil {
				return nil, err
			}
			if !issue.Terminal {
				body := fmt.Sprintf("## Health finding resolved\n\nFinding: `%s`\nResolved: %s", o.finding.ID, o.resolved.String)
				if _, err := insertNativeComment(ctx, tx, scope, issue, body, nil, now); err != nil {
					return nil, err
				}
			}
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
				if state.Name == request.State && !state.Terminal && !state.OperatorOnly && state.Dispatchable == (request.State == "Todo") {
					valid = true
				}
			}
			if !valid {
				return nil, nativeInvalid("Health reporting requires nondispatchable Backlog or dispatchable Todo")
			}
			issue, err := reportNativeMachineIssueTx(ctx, tx, scope, request, now)
			if err != nil {
				return nil, err
			}
			o.item = issue.WorkItemID
			if !issue.PublicationReused {
				created = append(created, issue)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO health_finding_issues(finding_id,project_id,work_item_id,reported_evidence_json,reported_resolved_at) VALUES(?,?,?,?,?)
 ON CONFLICT(finding_id,project_id) DO UPDATE SET work_item_id=excluded.work_item_id,reported_evidence_json=excluded.reported_evidence_json,reported_resolved_at=excluded.reported_resolved_at`, o.finding.ID, o.project, o.item, o.evidence, o.resolved); err != nil {
			return nil, err
		}
	}
	return created, nil
}
