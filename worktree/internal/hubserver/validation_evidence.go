package hubserver

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readValidationAudit(ctx context.Context, query nativeQueryer, scope nativeScope, scheduledItem, localItem, changeID, attemptID string) (*tracker.ValidationAudit, error) {
	audit := &tracker.ValidationAudit{Source: "native_command_receipts_and_scheduled_reporter_records", Coverage: "bounded_project_records; latest_40_scheduled_records_and_20_candidate_versions_or_receipts; observed_os_arch_go_and_declared_check_scope_only; candidate_comparison_only; ancestry_and_causal_attribution_unavailable", Unavailable: []string{}, Occurrences: []tracker.ValidationOccurrence{}}
	rows, err := query.QueryContext(ctx, `SELECT kind, work_item_id, id, revision, updated_at, body, actor_json FROM (
 SELECT 'issue' AS kind, native_id AS work_item_id, native_id AS id, revision, updated_at, body, actor_json FROM issues
 WHERE organization_id=? AND project_id=? AND (?='' OR native_id=?) AND instr(body, 'detent-checks')>0
 UNION ALL SELECT 'comment', work_item_id, id, revision, updated_at, body, actor_json FROM native_comments
 WHERE organization_id=? AND project_id=? AND (?='' OR work_item_id=?) AND instr(body, 'detent-checks')>0
 ) ORDER BY updated_at DESC, id DESC LIMIT 41`, scope.organization, scope.project, scheduledItem, scheduledItem, scope.organization, scope.project, scheduledItem, scheduledItem)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	count := 0
	seen := map[string]bool{}
	for rows.Next() {
		var source tracker.ValidationSource
		var observed, body, actor string
		if err := rows.Scan(&source.Kind, &source.WorkItemID, &source.RecordID, &source.Revision, &observed, &body, &actor); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(actor), &source.Actor); err != nil {
			rows.Close()
			return nil, err
		}
		count++
		if count > 40 {
			audit.Partial = true
			break
		}
		source.ObservedAt, err = time.Parse(time.RFC3339Nano, observed)
		if err != nil {
			rows.Close()
			return nil, err
		}
		origin, ok := issueorigin.Parse(body)
		if !ok || origin.Kind != "doctor" || origin.Instance != "github-actions" {
			continue
		}
		for _, scheduled := range gate.ParseScheduledEvidence(body) {
			if origin.Source != scheduled.RunURL {
				continue
			}
			if len(audit.Occurrences) >= 40 {
				audit.Partial = true
				break
			}
			if seen[scheduled.OccurrenceKey] {
				continue
			}
			seen[scheduled.OccurrenceKey] = true
			audit.Occurrences = append(audit.Occurrences, tracker.ValidationOccurrence{Source: source, Scheduled: scheduled, Comparisons: []tracker.ValidationComparison{}})
		}
	}
	readErr, closeErr := rows.Err(), rows.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(audit.Occurrences) == 0 {
		audit.Unavailable = append(audit.Unavailable, "scheduled_check_evidence")
		return audit, nil
	}
	for i := range audit.Occurrences {
		occurrence := &audit.Occurrences[i]
		candidates, partial, err := readValidationCandidates(ctx, query, scope, localItem, changeID, attemptID, occurrence.Scheduled)
		if err != nil {
			return nil, err
		}
		audit.Partial = audit.Partial || partial
		if len(candidates) == 0 {
			candidates = []tracker.ValidationComparison{{IntegratedSource: "unknown", Attribution: "candidate_only; source_inclusion_and_causality_unverified"}}
		}
		failed := []int{}
		for j, check := range occurrence.Scheduled.Checks {
			if check.ExitCode > 0 {
				failed = append(failed, j)
			}
		}
		if len(failed) == 0 {
			for _, candidate := range candidates {
				candidate.CheckComparison = gate.CompareCheck(candidate.Local, candidate.ObservedAt, occurrence.Scheduled, nil)
				occurrence.Comparisons = append(occurrence.Comparisons, candidate)
			}
			continue
		}
		for _, j := range failed {
			for _, candidate := range candidates {
				candidate.CheckIndex = new(j)
				candidate.CheckComparison = gate.CompareCheck(candidate.Local, candidate.ObservedAt, occurrence.Scheduled, &occurrence.Scheduled.Checks[j])
				occurrence.Comparisons = append(occurrence.Comparisons, candidate)
			}
		}
	}
	const auditByteLimit = 64 * 1024
	for {
		raw, err := json.Marshal(audit)
		if err != nil {
			return nil, err
		}
		if len(raw) <= auditByteLimit {
			break
		}
		audit.Partial = true
		last := len(audit.Occurrences) - 1
		occurrence := &audit.Occurrences[last]
		if len(occurrence.Comparisons) > 1 {
			occurrence.Comparisons = occurrence.Comparisons[:len(occurrence.Comparisons)-1]
		} else {
			audit.Occurrences = audit.Occurrences[:last]
		}
	}
	return audit, nil
}

func readValidationCandidates(ctx context.Context, query nativeQueryer, scope nativeScope, item, change, attempt string, scheduled gate.ScheduledEvidence) ([]tracker.ValidationComparison, bool, error) {
	rows, err := query.QueryContext(ctx, `SELECT c.work_item_id, v.record_json, COALESCE(a.id,''), COALESCE(a.run_id,''), COALESCE(json_extract(a.data_json,'$.runtime.local_attempt_id'),0), COALESCE(r.record_json,''), c.record_json
 FROM change_versions v JOIN change_requests c ON c.id=v.change_id
 LEFT JOIN change_landing_receipts r ON r.version_id=v.id
 LEFT JOIN native_attempts a ON a.id=r.attempt_id AND a.organization_id=c.organization_id AND a.project_id=c.project_id
 WHERE c.organization_id=? AND c.project_id=? AND (?='' OR c.work_item_id=?) AND (?='' OR c.id=?) AND (?='' OR a.id=?)
 ORDER BY (json_extract(r.record_json,'$.merge_sha')=?) DESC, v.rowid DESC, a.fencing_token DESC LIMIT 21`, scope.organization, scope.project, item, item, change, change, attempt, attempt, scheduled.HeadSHA)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	candidates := []tracker.ValidationComparison{}
	count := 0
	partial := false
	for rows.Next() {
		var candidate tracker.ValidationComparison
		var versionRaw, receiptRaw, changeRaw string
		if err := rows.Scan(&candidate.WorkItemID, &versionRaw, &candidate.NativeAttemptID, &candidate.NativeRunID, &candidate.LocalAttemptID, &receiptRaw, &changeRaw); err != nil {
			return nil, false, err
		}
		count++
		if count > 20 {
			partial = true
			break
		}
		var version tracker.ChangeVersion
		var request tracker.ChangeRequest
		if err := json.Unmarshal([]byte(versionRaw), &version); err != nil {
			return nil, false, err
		}
		if err := json.Unmarshal([]byte(changeRaw), &request); err != nil {
			return nil, false, err
		}
		if validationRepository(version.Repository) != scheduled.Repository {
			continue
		}
		candidate.ChangeID, candidate.VersionID, candidate.VersionAttemptID, candidate.ReviewedHeadSHA = version.ChangeID, version.ID, version.AttemptID, version.HeadSHA
		candidate.VersionRunID = version.RunID
		candidate.IntegratedSource = "unknown"
		candidate.Attribution = "candidate_only; source_inclusion_and_causality_unverified"
		candidate.Local, candidate.ObservedAt = version.Validation, version.CreatedAt
		if receiptRaw != "" {
			var receipt tracker.NativeLandingReceipt
			if err := json.Unmarshal([]byte(receiptRaw), &receipt); err != nil {
				return nil, false, err
			}
			if receipt.Gate != nil {
				candidate.Local, candidate.ObservedAt = receipt.Gate, receipt.ObservedAt
			}
			candidate.IntegratedHeadSHA = receipt.MergeSHA
		} else if request.Landed != nil && request.Landed.VersionID == version.ID {
			candidate.IntegratedHeadSHA = request.Landed.MergeSHA
		}
		if candidate.IntegratedHeadSHA != "" {
			candidate.IntegratedSource = "different_commit"
			if candidate.IntegratedHeadSHA == scheduled.HeadSHA {
				candidate.IntegratedSource = "same_commit"
			}
		}
		if candidate.Local != nil {
			local := *candidate.Local
			local.Output = ""
			if !local.Evidence.Valid() {
				local.Evidence = nil
			}
			local.Command = tracker.PublicValidationCommand(local.Command)
			candidate.Local = &local
		}
		candidates = append(candidates, candidate)
	}
	return candidates, partial, rows.Err()
}

func validationRepository(repository string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(repository, "https://github.com/"), "git@github.com:"), ".git")
}
