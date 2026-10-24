package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func healthIssueScope(organization tracker.OrganizationID, project tracker.ProjectID) nativeScope {
	return nativeScope{organization: organization, project: project, sourceActor: nativeIntegrationActor("health_detector")}
}

func healthIssueFindings(f healthFinding) []healthFinding {
	if f.Signal != "retry_storm" || len(f.Evidence.Signatures) == 0 {
		return []healthFinding{f}
	}
	signatures := slices.Clone(f.Evidence.Signatures)
	slices.Sort(signatures)
	signatures = slices.Compact(signatures)
	findings := make([]healthFinding, 0, len(signatures))
	for _, signature := range signatures {
		finding := f
		sum := sha256.Sum256([]byte("health_intake\x00retry_storm\x00" + signature))
		finding.Fingerprint = hex.EncodeToString(sum[:])
		finding.Evidence.Signatures = []string{signature}
		findings = append(findings, finding)
	}
	return findings
}

func healthIssueRequest(f healthFinding) (tracker.CreateIssue, error) {
	evidence, err := json.MarshalIndent(f.Evidence, "", "  ")
	if err != nil {
		return tracker.CreateIssue{}, err
	}
	title := fmt.Sprintf("%s on %s %s", f.Signal, f.Subject.Kind, f.Subject.ID)
	if f.Signal == "retry_storm" && len(f.Evidence.Signatures) == 1 {
		title = f.Signal + ": " + f.Evidence.Signatures[0]
	}
	request := tracker.CreateIssue{
		Title:    "intake(instance): " + title,
		Priority: new(1),
		Labels:   []string{"infrastructure"},
	}
	body := fmt.Sprintf("Finding: `%s`\nClass: %s\nSubject: %s %s\nOpened: %s\nObserved: %s\n\n%s\n\nNext action: %s\n\nEvidence:\n```json\n%s\n```\n\n```detent-agent\nschema: 1\neffort: high\n```\n\nDiagnostic report only. This finding does not establish a reproducible source repair.\n\n<!-- detent:no-dispatch -->", f.ID, f.Class, f.Subject.Kind, f.Subject.ID, formatHubTime(f.OpenedAt), formatHubTime(f.LastSeenAt), f.Summary, f.NextAction, evidence)
	request.Body = issueorigin.Stamp(body, issueorigin.Origin{Kind: "audit", Instance: "health_detector", Source: f.ID, Fingerprint: f.Fingerprint})
	return request, nil
}

func healthIssueDigest(f healthFinding) (string, error) {
	signatures := slices.Clone(f.Evidence.Signatures)
	slices.Sort(signatures)
	signatures = slices.Compact(signatures)
	material := struct {
		Fingerprint string
		Class       string
		Subject     healthSubject
		Signatures  []string `json:",omitempty"`
		Severity    string
	}{f.Fingerprint, f.Class, f.Subject, signatures, f.Severity}
	raw, err := json.Marshal(material)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func reportHealthFindings(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, now time.Time) ([]tracker.NativeIssue, error) {
	type occurrence struct {
		finding          healthFinding
		project          tracker.ProjectID
		item             tracker.NativeWorkItemID
		digest           string
		reportedDigest   string
		resolved         sql.NullString
		reportedResolved sql.NullString
		fingerprint      string
	}
	rows, err := tx.QueryContext(ctx, `SELECT f.id,f.fingerprint,f.signal,f.class,f.subject_json,f.opened_at,f.last_seen_at,f.resolved_at,f.severity,f.summary,f.next_action,f.evidence_json,
 p.id,COALESCE(l.work_item_id,''),COALESCE(l.reported_evidence_digest,''),l.reported_resolved_at,COALESCE(l.intake_fingerprint,'')
 FROM health_findings f JOIN projects p ON p.organization_id=f.organization_id AND p.deleted_at IS NULL AND p.profile='native'
 AND (EXISTS(SELECT 1 FROM json_each(f.projects_json) WHERE value=p.id) OR EXISTS(SELECT 1 FROM health_finding_issues WHERE finding_id=f.id AND project_id=p.id))
 LEFT JOIN health_finding_issues l ON l.finding_id=f.id AND l.project_id=p.id
 WHERE f.organization_id=? AND f.class='instance'
 AND (f.resolved_at IS NULL OR (l.work_item_id IS NOT NULL AND l.reported_resolved_at IS NOT f.resolved_at))
 ORDER BY f.rowid,p.id LIMIT ?`, organization, healthReadLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	occurrences := []occurrence{}
	for rows.Next() {
		var o occurrence
		var subject, opened, seen, rawEvidence string
		f := &o.finding
		if err := rows.Scan(&f.ID, &f.Fingerprint, &f.Signal, &f.Class, &subject, &opened, &seen, &o.resolved, &f.Severity, &f.Summary, &f.NextAction, &rawEvidence, &o.project, &o.item, &o.reportedDigest, &o.reportedResolved, &o.fingerprint); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(subject), &f.Subject); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(rawEvidence), &f.Evidence); err != nil {
			return nil, err
		}
		f.Evidence = scopeHealthEvidence(f.Evidence, o.project)
		o.digest, err = healthIssueDigest(*f)
		if err != nil {
			return nil, err
		}
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
	type findingProject struct {
		finding string
		project tracker.ProjectID
	}
	links := make(map[findingProject]map[string]occurrence)
	for _, o := range occurrences {
		key := findingProject{o.finding.ID, o.project}
		if links[key] == nil {
			links[key] = make(map[string]occurrence)
		}
		links[key][o.fingerprint] = o
	}
	pending := make([]occurrence, 0, len(occurrences))
	for _, o := range occurrences {
		if o.resolved.Valid || o.item != "" && o.fingerprint == "" {
			pending = append(pending, o)
			continue
		}
		key := findingProject{o.finding.ID, o.project}
		existing, ok := links[key]
		if !ok {
			continue
		}
		delete(links, key)
		for _, finding := range healthIssueFindings(o.finding) {
			next, ok := existing[finding.Fingerprint]
			if !ok {
				next = occurrence{project: o.project}
			}
			next.finding, next.fingerprint = finding, finding.Fingerprint
			next.digest, err = healthIssueDigest(finding)
			if err != nil {
				return nil, err
			}
			pending = append(pending, next)
		}
	}
	created := []tracker.NativeIssue{}
	for _, o := range pending {
		if o.item != "" && !o.resolved.Valid && o.resolved == o.reportedResolved && o.digest == o.reportedDigest {
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
			for _, state := range project.States {
				if state.Name != "Triage" && !state.Terminal && !state.OperatorOnly && !state.Dispatchable && (request.State == "" || state.Name == "Backlog") {
					request.State = state.Name
				}
			}
			if request.State == "" {
				continue
			}
			issue, err := reportHealthIssue(ctx, tx, scope, request, o.finding.OpenedAt, now)
			if err != nil {
				return nil, err
			}
			o.item = issue.WorkItemID
			reported = issue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO health_finding_issues(finding_id,project_id,work_item_id,reported_evidence_digest,reported_resolved_at,intake_fingerprint) VALUES(?,?,?,?,?,?)
 ON CONFLICT(finding_id,project_id,intake_fingerprint) DO UPDATE SET work_item_id=excluded.work_item_id,reported_evidence_digest=excluded.reported_evidence_digest,reported_resolved_at=excluded.reported_resolved_at`, o.finding.ID, o.project, o.item, o.digest, o.resolved, o.fingerprint); err != nil {
			return nil, err
		}
		if !o.resolved.Valid && !reported.PublicationReused {
			created = append(created, reported)
		}
	}
	if err := resolveHealthIssues(ctx, tx, organization, now); err != nil {
		return nil, err
	}
	return created, nil
}

func reportHealthIssue(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue, opened, now time.Time) (tracker.NativeIssue, error) {
	issue, err := reportNativeMachineIssueTx(ctx, tx, scope, request, now)
	if err != nil || !issue.Terminal || issue.State != "Cancelled" || issue.Actor != scope.actor() {
		return issue, err
	}
	var newEpisode bool
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT
 json_extract(actor_json,'$.kind')='integration' AND json_extract(actor_json,'$.principal_id')='health_detector'
 AND json_extract(data_json,'$.from_state')='Backlog' AND json_extract(data_json,'$.to_state')='Cancelled'
 AND julianday(recorded_at)<julianday(?)
 FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='workflow.transitioned'
 ORDER BY sequence DESC LIMIT 1),0)`, formatHubTime(opened), scope.organization, scope.project, issue.WorkItemID).Scan(&newEpisode); err != nil {
		return tracker.NativeIssue{}, err
	}
	if newEpisode {
		return createNativeIssue(ctx, tx, scope, request, now, true)
	}
	return issue, nil
}

func resolveHealthIssues(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT i.project_id,i.native_id
 FROM health_finding_issues l JOIN issues i ON i.native_id=l.work_item_id
 JOIN workflow_states ws ON ws.id=i.workflow_state_id
 JOIN projects p ON p.id=i.project_id
 WHERE i.organization_id=? AND i.archived=0 AND ws.detent_state='Backlog' AND ws.terminal=0
 AND p.deleted_at IS NULL AND p.profile='native'
 AND NOT EXISTS(SELECT 1 FROM health_finding_issues other JOIN health_findings f ON f.id=other.finding_id
 WHERE other.work_item_id=i.native_id AND (f.class<>'instance' OR f.resolved_at IS NULL
 OR julianday(f.resolved_at)>=julianday(?) OR julianday(f.last_seen_at)>=julianday(?)))
 AND NOT EXISTS(SELECT 1 FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.work_item_id=i.native_id
 AND e.type='workflow.transitioned' AND json_extract(e.actor_json,'$.kind')='human'
 AND json_extract(e.data_json,'$.from_state')='Backlog' AND json_extract(e.data_json,'$.to_state')<>'Backlog')
 ORDER BY i.native_id LIMIT ?`, organization, formatHubTime(now.Add(-healthReopenWindow)), formatHubTime(now.Add(-healthReopenWindow)), healthReadLimit+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	type candidate struct {
		project tracker.ProjectID
		item    string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.project, &c.item); err != nil {
			return err
		}
		candidates = append(candidates, c)
		if len(candidates) > healthReadLimit {
			return errHealthReadLimit
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, c := range candidates {
		scope := healthIssueScope(organization, c.project)
		issue, _, err := readNativeIssue(ctx, tx, scope, c.item)
		if err != nil {
			return err
		}
		origin, ok := issueorigin.Parse(issue.Body)
		if !ok || origin.Instance != "health_detector" || issue.Actor != *scope.sourceActor || issue.Provenance != nil {
			continue
		}
		project, err := readNativeProject(ctx, tx, scope)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(project.States, func(state tracker.NativeState) bool {
			return state.Name == "Cancelled" && state.Terminal && !state.OperatorOnly
		}) {
			continue
		}
		if err := validateNativeWorkflowTransition(project.States, issue.State, "Cancelled", apiScopeWorker); err != nil {
			continue
		}
		ids, err := nativePageIDs(ctx, tx, "SELECT DISTINCT finding_id FROM health_finding_issues WHERE work_item_id=? ORDER BY finding_id LIMIT ?", c.item, healthReadLimit+1)
		if err != nil {
			return err
		}
		if len(ids) > healthReadLimit {
			return errHealthReadLimit
		}
		from := issue.State
		issue.State = "Cancelled"
		data := tracker.CollaborationData{FromState: from, ToState: issue.State, Reason: "worker_progress", ReasonDetail: "Health findings resolved for the resolution window: " + strings.Join(ids, ", ")}
		if _, err := persistNativeIssue(ctx, tx, scope, issue, "workflow.transitioned", data, now); err != nil {
			return err
		}
	}
	return nil
}
