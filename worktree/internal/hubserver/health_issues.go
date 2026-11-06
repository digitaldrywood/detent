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
		State:    "Todo",
		Priority: new(1),
		Labels:   []string{"infrastructure"},
	}
	body := fmt.Sprintf(
		"## Current behavior\n\nFinding: `%s`\nSignal: %s\nClass: %s\nSubject: %s %s\nOpened: %s\nObserved: %s\n\n%s\n\nDiagnostic next action: %s\n\nRecorded evidence for this observation window:\n```json\n%s\n```\n\n"+
			"## Desired behavior\n\nClear the recorded `%s` condition for %s %s by correcting its diagnosed cause within existing intended behavior.\n\n"+
			"## Acceptance criteria\n\n- Reproduce the recorded condition with a focused test or fixture using the evidence above, and show that it fails before the repair.\n- Correct the diagnosed cause so that the same test or fixture passes and the recorded condition is cleared in that fixture before merge.\n\n"+
			"## Must not break\n\nPreserve existing behavior, machine fingerprints, duplicate ownership and instance attribution. Infrastructure evidence alone does not authorize speculative source changes; establish the cause with the reproducing test or fixture first.\n\n"+
			"## How we know it worked\n\nRecord the reproducing test or fixture, its command and its failing and passing results against the observed cause. Run focused checks for the changed packages before merge; no deployment or outside-system verification is required.\n\n"+
			"## Invariants touched\n\nPreserve INV-2 instance attribution, INV-7 machine identity, INV-18 bounded writes and INV-21 bounded evaluation cost. Under INV-3, repair or consolidate the existing owner; add no protection mechanism.\n\n"+
			"## Safety regression\n\nThis report changes no safety-critical path. If diagnosis requires changing completion, retry or dispatch controls, preserve their existing safety regression tables and boundary seeds and name the affected behavior in the repair.\n\n```detent-agent\nschema: 1\neffort: high\n```",
		f.ID, f.Signal, f.Class, f.Subject.Kind, f.Subject.ID, formatHubTime(f.OpenedAt), formatHubTime(f.LastSeenAt), f.Summary, f.NextAction, evidence, f.Signal, f.Subject.Kind, f.Subject.ID)
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

func reportHealthFindings(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, now time.Time, cancellations map[string]healthCancellation) ([]tracker.NativeIssue, error) {
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
 AND (EXISTS(SELECT 1 FROM health_finding_projects fp WHERE fp.finding_id=f.id AND fp.project_id=p.id) OR EXISTS(SELECT 1 FROM health_finding_issues WHERE finding_id=f.id AND project_id=p.id))
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
			var done bool
			if err := tx.QueryRowContext(ctx, `SELECT ws.terminal=1 AND ws.detent_state='Done' FROM issues i JOIN workflow_states ws ON ws.id=i.workflow_state_id WHERE i.native_id=? AND i.organization_id=? AND i.project_id=?`, o.item, organization, o.project).Scan(&done); err != nil {
				return nil, err
			}
			if !done {
				continue
			}
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
			contract, err := nativeIssueContract(ctx, tx, scope)
			if err != nil {
				return nil, err
			}
			target := "Todo"
			if contract.Context != "" && !contract.Evaluate(nativeContractIssue(tracker.NativeIssue{Body: request.Body})).Satisfied() {
				target = "Backlog"
			}
			request.State = target
			project, err := validateNativeIssueDraft(ctx, tx, scope, request)
			if err != nil {
				return nil, err
			}
			request.State = ""
			for _, state := range project.States {
				if state.Name == target && !state.Terminal && !state.OperatorOnly && state.Dispatchable == (target == "Todo") {
					request.State = target
					break
				}
			}
			if request.State == "" {
				continue
			}
			issue, err := reportHealthIssue(ctx, tx, scope, request, o.finding.Subject, o.item, o.finding.OpenedAt, now, cancellations)
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

func reportHealthIssue(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue, subject healthSubject, linked tracker.NativeWorkItemID, opened, now time.Time, cancellations map[string]healthCancellation) (tracker.NativeIssue, error) {
	search, err := prepareNativeDuplicates(ctx, tx, scope, request, now, true)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	if len(search.matches) == 0 && linked != "" {
		var owner possibleDuplicate
		err := tx.QueryRowContext(ctx, `SELECT i.native_id,i.number,i.title,ws.detent_state,i.revision FROM issues i JOIN workflow_states ws ON ws.id=i.workflow_state_id
 WHERE i.organization_id=? AND i.project_id=? AND i.native_id=? AND i.archived=0 AND ws.terminal=0`, scope.organization, scope.project, linked).Scan(&owner.WorkItemID, &owner.Number, &owner.Title, &owner.State, &owner.revision)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return tracker.NativeIssue{}, err
		}
		if err == nil {
			search.matches = []possibleDuplicate{owner}
		}
	}
	if (len(search.matches) == 0 || search.matches[0].terminal) && subject.Kind == "work_item" {
		var owner possibleDuplicate
		err := tx.QueryRowContext(ctx, `WITH subject AS (
 SELECT id FROM issues WHERE organization_id=? AND project_id=? AND native_id=?
), blockers AS (
 SELECT blocker_issue_id AS id FROM issue_dependencies WHERE dependent_issue_id=(SELECT id FROM subject) LIMIT ?
), forward_links AS (
 SELECT second_issue_id AS id FROM issue_relationships WHERE first_issue_id=(SELECT id FROM subject) AND relationship_type='related' LIMIT ?
), reverse_links AS (
 SELECT first_issue_id AS id FROM issue_relationships WHERE second_issue_id=(SELECT id FROM subject) AND relationship_type='related' LIMIT ?
), candidates AS (SELECT id FROM blockers UNION SELECT id FROM forward_links UNION SELECT id FROM reverse_links)
 SELECT i.native_id,i.number,i.title,ws.detent_state,i.revision FROM candidates c JOIN issues i ON i.id=c.id
 JOIN workflow_states ws ON ws.id=i.workflow_state_id
 WHERE i.organization_id=? AND i.project_id=? AND i.archived=0 AND ws.terminal=0
 ORDER BY i.number DESC LIMIT 1`, scope.organization, scope.project, subject.ID, duplicateCandidateLimit, duplicateCandidateLimit, duplicateCandidateLimit, scope.organization, scope.project).Scan(&owner.WorkItemID, &owner.Number, &owner.Title, &owner.State, &owner.revision)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return tracker.NativeIssue{}, err
		}
		if err == nil {
			search.matches = []possibleDuplicate{owner}
		}
	}
	scope.duplicateSearch = search
	issue, err := reportNativeMachineOccurrenceTx(ctx, tx, scope, request, now, true)
	if err == nil && issue.Terminal && issue.State == "Done" && issue.Actor == scope.actor() && issue.Provenance == nil && legacyHealthIssueBody(issue.Body) {
		return createNativeIssue(ctx, tx, scope, request, now, true)
	}
	if err != nil || !issue.Terminal || issue.State != "Cancelled" || issue.Actor != scope.actor() {
		return issue, err
	}
	var id, actorRaw, dataRaw string
	var projected sql.NullInt64
	var automatic sql.NullBool
	var before bool
	err = tx.QueryRowContext(ctx, `SELECT id,health_projected,
 actor_kind='integration' AND actor_principal_id='health_detector'
 AND transition_from='Backlog' AND transition_to='Cancelled',
 julianday(recorded_at)<julianday(?),
 CASE WHEN health_projected IS NULL THEN actor_json ELSE '{}' END,
 CASE WHEN health_projected IS NULL THEN data_json ELSE '{}' END
 FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='workflow.transitioned'
 ORDER BY sequence DESC LIMIT 1`, formatHubTime(opened), scope.organization, scope.project, issue.WorkItemID).Scan(&id, &projected, &automatic, &before, &actorRaw, &dataRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return issue, nil
	}
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	newEpisode := automatic.Valid && automatic.Bool && before
	if !projected.Valid {
		fact, ok := cancellations[id]
		if !ok || fact.actorRaw != actorRaw || fact.dataRaw != dataRaw {
			return tracker.NativeIssue{}, errors.New("health cancellation observation changed")
		}
		newEpisode = fact.automatic && before
	}
	if newEpisode {
		return createNativeIssue(ctx, tx, scope, request, now, true)
	}
	return issue, nil
}

func legacyHealthIssueBody(body string) bool {
	origin, ok := issueorigin.Parse(body)
	if !ok || origin.Kind != "audit" || origin.Instance != "health_detector" || origin.Source == "" {
		return false
	}
	suffix := "\n\nDiagnostic report only. This finding does not establish a reproducible source repair.\n\n<!-- detent:no-dispatch -->"
	content, _, legacy := strings.Cut(body, suffix)
	return legacy && strings.HasPrefix(content, "Finding: `"+origin.Source+"`\nClass: instance\nSubject: ") && body == issueorigin.Stamp(content+suffix, origin)
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
 AND e.type='workflow.transitioned' AND (e.health_projected IS NULL OR (e.actor_kind='human'
 AND e.transition_from='Backlog' AND e.transition_to<>'Backlog')))
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
