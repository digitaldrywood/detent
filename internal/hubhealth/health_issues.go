package hubhealth

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

func (d *Store) ReportFindings(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, now time.Time, cancellations map[string]Cancellation) ([]tracker.NativeIssue, error) {
	type occurrence struct {
		finding          Finding
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
 ORDER BY f.rowid,p.id LIMIT ?`, organization, ReadLimit+1)
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
		f.Evidence = ScopeEvidence(f.Evidence, o.project)
		o.digest, err = IssueDigest(*f)
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
		if len(occurrences) > ReadLimit {
			return nil, ErrReadLimit
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
		for _, finding := range IssueFindings(o.finding) {
			next, ok := existing[finding.Fingerprint]
			if !ok {
				next = occurrence{project: o.project}
			}
			next.finding, next.fingerprint = finding, finding.Fingerprint
			next.digest, err = IssueDigest(finding)
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
		reported, eligible, err := d.hooks.ReportOccurrence(ctx, tx, Scope{Organization: organization, Project: o.project}, o.finding, o.item, o.resolved, o.reportedResolved, now, cancellations)
		if err != nil {
			return nil, err
		}
		if !eligible {
			continue
		}
		if !o.resolved.Valid {
			o.item = reported.WorkItemID
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO health_finding_issues(finding_id,project_id,work_item_id,reported_evidence_digest,reported_resolved_at,intake_fingerprint) VALUES(?,?,?,?,?,?)
 ON CONFLICT(finding_id,project_id,intake_fingerprint) DO UPDATE SET work_item_id=excluded.work_item_id,reported_evidence_digest=excluded.reported_evidence_digest,reported_resolved_at=excluded.reported_resolved_at`, o.finding.ID, o.project, o.item, o.digest, o.resolved, o.fingerprint); err != nil {
			return nil, err
		}
		if !o.resolved.Valid && !reported.PublicationReused {
			created = append(created, reported)
		}
	}
	if err := d.ResolveIssues(ctx, tx, organization, now); err != nil {
		return nil, err
	}
	return created, nil
}

func (d *Store) ResolveIssues(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, now time.Time) error {
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
 ORDER BY i.native_id LIMIT ?`, organization, formatHubTime(now.Add(-ReopenWindow)), formatHubTime(now.Add(-ReopenWindow)), ReadLimit+1)
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
		if len(candidates) > ReadLimit {
			return ErrReadLimit
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, c := range candidates {
		if err := d.hooks.ResolveIssue(ctx, tx, Scope{Organization: organization, Project: c.project}, c.item, now); err != nil {
			return err
		}
	}

	return nil
}

func IssueFindings(f Finding) []Finding {
	if f.Signal != "retry_storm" || len(f.Evidence.Signatures) == 0 {
		return []Finding{f}
	}
	signatures := slices.Clone(f.Evidence.Signatures)
	slices.Sort(signatures)
	signatures = slices.Compact(signatures)
	findings := make([]Finding, 0, len(signatures))
	for _, signature := range signatures {
		finding := f
		sum := sha256.Sum256([]byte("health_intake\x00retry_storm\x00" + signature))
		finding.Fingerprint = hex.EncodeToString(sum[:])
		finding.Evidence.Signatures = []string{signature}
		findings = append(findings, finding)
	}
	return findings
}

func IssueRequest(f Finding) (tracker.CreateIssue, error) {
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

func IssueDigest(f Finding) (string, error) {
	signatures := slices.Clone(f.Evidence.Signatures)
	slices.Sort(signatures)
	signatures = slices.Compact(signatures)
	material := struct {
		Fingerprint string
		Class       string
		Subject     Subject
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

func LegacyIssueBody(body string) bool {
	origin, ok := issueorigin.Parse(body)
	if !ok || origin.Kind != "audit" || origin.Instance != "health_detector" || origin.Source == "" {
		return false
	}
	suffix := "\n\nDiagnostic report only. This finding does not establish a reproducible source repair.\n\n<!-- detent:no-dispatch -->"
	content, _, legacy := strings.Cut(body, suffix)
	return legacy && strings.HasPrefix(content, "Finding: `"+origin.Source+"`\nClass: instance\nSubject: ") && body == issueorigin.Stamp(content+suffix, origin)
}
