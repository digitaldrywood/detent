package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/runtimeoutput"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readNativeRecovery(ctx context.Context, scope nativeScope, item string, projected ...bool) (tracker.NativeRecovery, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	bounded := len(projected) > 0 && projected[0]
	var result tracker.NativeRecovery
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	result.Issue, _, err = readNativeIssue(ctx, tx, scope, item)
	if err != nil {
		return result, err
	}
	change, found, err := readLatestNativeChangeRequest(ctx, tx, scope, item)
	if err != nil {
		return result, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT CASE WHEN recovery_runner_id != '' THEN recovery_version_id ELSE '' END FROM issues WHERE organization_id=? AND project_id=? AND native_id=?`, scope.organization, scope.project, item).Scan(&result.SourceVersionID); err != nil {
		return result, err
	}
	source, err := readNativeSourceCheckpoint(ctx, tx, scope, item)
	if err != nil {
		return result, err
	}
	sourceAttempt, changeAttempt := source.AttemptID, ""

	result.SourceAttemptID = sourceAttempt
	if found {
		var detail tracker.ChangeDetail
		if bounded {
			detail, err = s.readRecoveryChange(ctx, tx, scope, item, change)
		} else {
			detail, err = readChangeDetailView(ctx, tx, scope, item, change.ID, s.config.now(), true)
		}
		if err != nil {
			return result, err
		}
		implementationReview, validator := -1, -1
		for i := len(detail.Reviews) - 1; i >= 0; i-- {
			if implementationReview < 0 && detail.Reviews[i].ImplementationReview != nil {
				implementationReview = i
			}
			if validator < 0 && detail.Reviews[i].Validator != nil {
				validator = i
			}
		}
		reviews := detail.Reviews[:0]
		for i, review := range detail.Reviews {
			if review.ImplementationReview == nil && review.Validator == nil || i == implementationReview || i == validator {
				reviews = append(reviews, review)
			}
		}
		detail.Reviews = reviews
		result.ChangeDetail = &detail
		if change.CurrentVersion != "" {
			if len(detail.Versions) != 1 {
				return result, fmt.Errorf("read change: current version %s is missing", change.CurrentVersion)
			}
			version := detail.Versions[0]
			changeAttempt = version.AttemptID
			result.Change = &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}
			if sourceAttempt == "" {
				sourceAttempt = version.AttemptID
				result.SourceAttemptID = sourceAttempt
			}
		}
	}
	discussionOrder := " ORDER BY sequence"
	if bounded {
		discussionOrder = " ORDER BY sequence DESC LIMIT 33"
	}
	ids, err := nativePageIDs(ctx, tx, `SELECT id FROM native_comments
WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND (
 json_extract(actor_json, '$.kind') = 'human' OR json_extract(edited_by_json, '$.kind') = 'human' OR id = (
 SELECT id FROM native_comments WHERE organization_id = ? AND project_id = ? AND work_item_id = ?
 AND json_extract(actor_json, '$.kind') != 'human' ORDER BY sequence DESC LIMIT 1))`+discussionOrder,
		scope.organization, scope.project, item, scope.organization, scope.project, item)
	if err != nil {
		return result, err
	}
	if bounded {
		result.Read = &tracker.NativeRecoveryRead{DetailPath: "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items/" + url.PathEscape(item) + "?view=recovery&limit=1"}
		if len(ids) == 33 {
			result.Read.Omitted = append(result.Read.Omitted, "discussion")
			ids = ids[:32]
		}
	}
	discussionBytes := 0
	for _, id := range ids {
		comment, err := readNativeComment(ctx, tx, scope, item, id)
		if err != nil {
			return result, err
		}
		if bounded {
			encoded, err := json.Marshal(comment)
			if err != nil {
				return result, err
			}
			if discussionBytes+len(encoded) > 128<<10 {
				result.Read.Omitted = append(result.Read.Omitted, "discussion")
				break
			}
			discussionBytes += len(encoded)
		}
		result.Discussion = append(result.Discussion, comment)
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.data_json, a.status, a.started_at, a.updated_at, a.checkpoint_json, a.artifact_ids_json, l.expires_at, l.released_at, l.renewed_at, a.work_item_revision, a.dispatch_generation
FROM native_attempts a JOIN leases l ON l.lease_id = a.lease_id
WHERE a.organization_id = ? AND a.project_id = ? AND a.work_item_id = ? AND (a.id = ? OR a.id = ? OR a.id = (
 SELECT id FROM native_attempts WHERE organization_id = ? AND project_id = ? AND work_item_id = ? ORDER BY fencing_token DESC LIMIT 1)
 OR a.id = (SELECT id FROM native_attempts WHERE organization_id = ? AND project_id = ? AND work_item_id = ?
 AND json_extract(data_json, '$.runtime.landing') IS NOT NULL ORDER BY fencing_token DESC LIMIT 1)
 OR a.id = (SELECT id FROM native_attempts WHERE organization_id = ? AND project_id = ? AND work_item_id = ?
 AND checkpoint_json IS NOT NULL ORDER BY fencing_token DESC LIMIT 1))
ORDER BY a.fencing_token LIMIT 5`, scope.organization, scope.project, item, sourceAttempt, changeAttempt,
		scope.organization, scope.project, item, scope.organization, scope.project, item, scope.organization, scope.project, item)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		attempt, err := scanNativeAttempt(rows, s.config.now())
		if err != nil {
			return result, err
		}
		attempt.Runtime = attempt.Runtime.WithoutActivitySpans()
		result.Attempts = append(result.Attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	if bounded {
		slices.Reverse(result.Discussion)
		if err := projectNativeRecovery(&result); err != nil {
			return result, err
		}
	}
	return result, tx.Commit()
}

type recoveryEvidenceCursor struct {
	Section int    `json:"section"`
	After   int64  `json:"after"`
	Offset  int64  `json:"offset"`
	Digest  string `json:"digest,omitempty"`
	Change  string `json:"change,omitempty"`
	Version string `json:"version,omitempty"`
}

func (s *Service) readNativeRecoveryPage(ctx context.Context, scope nativeScope, item, path string, params url.Values) (tracker.NativeRecovery, error) {
	_, cursor, key, err := s.readNativePageLimit(ctx, scope, path, params, 1, 1)
	if err != nil {
		return tracker.NativeRecovery{}, err
	}
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return tracker.NativeRecovery{}, err
	}
	defer tx.Rollback()
	issue, _, err := readNativeIssueProjection(ctx, tx, scope, item, true)
	if err != nil {
		return tracker.NativeRecovery{}, err
	}
	change, _, err := readLatestNativeChangeRequest(ctx, tx, scope, item)
	if err != nil {
		return tracker.NativeRecovery{}, err
	}
	selection := recoveryEvidenceCursor{Change: change.ID, Version: change.CurrentVersion}
	if cursor.After != "" {
		if json.Unmarshal([]byte(cursor.After), &selection) != nil || selection.Section < 0 || selection.Section > 7 || selection.After < 0 || selection.Offset < 0 {
			return tracker.NativeRecovery{}, nativeInvalid("Recovery cursor is invalid")
		}
		if selection.Change != change.ID || selection.Version != change.CurrentVersion {
			return tracker.NativeRecovery{}, nativeConflict(0)
		}
	}
	sections := []string{"reviews", "checks", "change_discussion", "discussion", "attempts", "versions", "change"}
	page := &tracker.NativeRecoveryPage{NativeReference: issue.NativeReference, Section: "complete"}
	for selection.Section < len(sections) {
		section := sections[selection.Section]
		sequence, id, version, content, err := s.readRecoveryEvidenceRecord(ctx, tx, scope, item, section, selection)
		if errors.Is(err, sql.ErrNoRows) {
			selection.Section++
			selection.After, selection.Offset, selection.Digest = 0, 0, ""
			continue
		}
		if err != nil {
			return tracker.NativeRecovery{}, err
		}
		digest := sha256.Sum256(content)
		sha := hex.EncodeToString(digest[:])
		if selection.Digest != "" && selection.Digest != sha || selection.Offset >= int64(len(content)) {
			return tracker.NativeRecovery{}, nativeConflict(0)
		}
		end := min(selection.Offset+(64<<10), int64(len(content)))
		page.Section, page.RecordID, page.VersionID = section, id, version
		page.Offset, page.TotalBytes, page.SHA256, page.Data = selection.Offset, int64(len(content)), sha, content[selection.Offset:end]
		if end == int64(len(content)) {
			selection.After, selection.Offset, selection.Digest = sequence, 0, ""
		} else {
			selection.Offset, selection.Digest = end, sha
		}
		position, err := json.Marshal(selection)
		if err != nil {
			return tracker.NativeRecovery{}, err
		}
		cursor.After = string(position)
		page.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return tracker.NativeRecovery{}, err
		}
		break
	}
	return tracker.NativeRecovery{Continuation: page}, tx.Commit()
}

func (s *Service) readRecoveryEvidenceRecord(ctx context.Context, query nativeQueryer, scope nativeScope, item, section string, cursor recoveryEvidenceCursor) (int64, string, string, []byte, error) {
	var sequence int64
	var id, version string
	var value any
	switch section {
	case "reviews", "checks", "change_discussion":
		kind := map[string]string{"reviews": "review", "checks": "check", "change_discussion": "discussion"}[section]
		var content []byte
		err := query.QueryRowContext(ctx, `SELECT sequence, COALESCE(version_id,''), record_json FROM change_evidence WHERE change_id = ? AND kind = ? AND sequence > ? ORDER BY sequence LIMIT 1`, cursor.Change, kind, cursor.After).Scan(&sequence, &version, &content)
		return sequence, strconv.FormatInt(sequence, 10), version, content, err
	case "discussion":
		err := query.QueryRowContext(ctx, `SELECT sequence, id FROM native_comments WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND sequence > ? ORDER BY sequence LIMIT 1`, scope.organization, scope.project, item, cursor.After).Scan(&sequence, &id)
		if err != nil {
			return 0, "", "", nil, err
		}
		comment, err := readNativeComment(ctx, query, scope, item, id)
		if err != nil {
			return 0, "", "", nil, err
		}
		value = comment
	case "attempts":
		err := query.QueryRowContext(ctx, `SELECT fencing_token, id FROM native_attempts WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND fencing_token > ? ORDER BY fencing_token LIMIT 1`, scope.organization, scope.project, item, cursor.After).Scan(&sequence, &id)
		if err != nil {
			return 0, "", "", nil, err
		}
		attempt, err := readNativeAttemptRecord(ctx, query, scope, item, id, s.config.now())
		if err != nil {
			return 0, "", "", nil, err
		}
		value = attempt
	case "versions":
		err := query.QueryRowContext(ctx, `SELECT number, id FROM change_versions WHERE change_id = ? AND number > ? ORDER BY number LIMIT 1`, cursor.Change, cursor.After).Scan(&sequence, &id)
		if err != nil {
			return 0, "", "", nil, err
		}
		retained, err := readChangeVersion(ctx, query, cursor.Change, id)
		if err != nil {
			return 0, "", "", nil, err
		}
		value, version = retained, id
	case "change":
		if cursor.Change == "" || cursor.After > 0 {
			return 0, "", "", nil, sql.ErrNoRows
		}
		change, err := readChange(ctx, query, scope, item, cursor.Change)
		if err != nil {
			return 0, "", "", nil, err
		}
		value, sequence, id = change, 1, change.ID
	default:
		return 0, "", "", nil, nativeInvalid("Recovery section is invalid")
	}
	content, err := json.Marshal(value)
	return sequence, id, version, content, err
}

func projectNativeRecovery(result *tracker.NativeRecovery) error {
	if detail := result.ChangeDetail; detail != nil {
		for i := range detail.Versions {
			detail.Versions[i].Validation = boundedChangeCommand(detail.Versions[i].Validation)
			detail.Versions[i].Landing = boundedRecoveryLanding(detail.Versions[i].Landing)
		}
		for i := range detail.Reviews {
			detail.Reviews[i].SecurityAudit = boundedRecoveryAudit(detail.Reviews[i].SecurityAudit)
			detail.Reviews[i].Validator = boundedChangeReviewResult(detail.Reviews[i].Validator)
			detail.Reviews[i].ImplementationReview = boundedChangeReviewResult(detail.Reviews[i].ImplementationReview)
		}
		var omitted bool
		var err error
		detail.Reviews, omitted, err = boundedRecoveryReviews(detail.Reviews, 256<<10)
		if err != nil {
			return err
		}
		if omitted {
			result.Read.Omitted = append(result.Read.Omitted, "change_detail.reviews")
		}
		detail.Checks, omitted, err = boundedRecoveryRecords(detail.Checks, 32<<10)
		if err != nil {
			return err
		}
		if omitted {
			result.Read.Omitted = append(result.Read.Omitted, "change_detail.checks")
		}
		detail.Discussion, omitted, err = boundedRecoveryRecords(detail.Discussion, 64<<10)
		if err != nil {
			return err
		}
		if omitted {
			result.Read.Omitted = append(result.Read.Omitted, "change_detail.discussion")
		}
		result.Read.Omitted = append(result.Read.Omitted, "command_output", "change_detail.reviews", "change_detail.checks", "change_detail.discussion")
	}
	for i := range result.Attempts {
		if runtime := result.Attempts[i].Runtime; runtime != nil {
			runtime.Validation = boundedChangeCommand(runtime.Validation)
			runtime.Landing = boundedRecoveryLanding(runtime.Landing)
		}
	}
	if len(result.Attempts) > 0 {
		result.Read.Omitted = append(result.Read.Omitted, "command_output")
	}
	slices.Sort(result.Read.Omitted)
	result.Read.Omitted = slices.Compact(result.Read.Omitted)
	if len(result.Read.Omitted) == 0 {
		result.Read = nil
	}
	return nil
}

func boundedRecoveryAudit(audit *tracker.SecurityAuditReview) *tracker.SecurityAuditReview {
	if audit == nil {
		return nil
	}
	result := *audit
	budget := 16 << 10
	bound := func(value string) string {
		limit := min(4096, budget)
		if len(value) <= limit {
			budget -= len(value)
			return value
		}
		result.TextTruncated = true
		if limit == 0 {
			return ""
		}
		text := runtimeoutput.Truncate(value, limit).Value
		budget -= len(text)
		return text
	}
	result.Run.Summary = bound(strings.TrimSpace(result.Run.Summary))
	result.Run.Findings = slices.Clone(result.Run.Findings)
	for i := range result.Run.Findings {
		result.Run.Findings[i].Body = bound(result.Run.Findings[i].Body)
		result.Run.Findings[i].Path = bound(result.Run.Findings[i].Path)
	}
	return &result
}

func boundedRecoveryLanding(landing *tracker.NativeLandingReceipt) *tracker.NativeLandingReceipt {
	result := boundedChangeLanding(landing)
	if result != nil {
		result.SecurityAudit = boundedRecoveryAudit(result.SecurityAudit)
	}
	return result
}

func boundedRecoveryRecords[T any](records []T, budget int) ([]T, bool, error) {
	used, start := 0, len(records)
	for i := len(records) - 1; i >= 0; i-- {
		encoded, err := json.Marshal(records[i])
		if err != nil {
			return nil, false, err
		}
		if used+len(encoded) > budget {
			break
		}
		used += len(encoded)
		start = i
	}
	return slices.Clone(records[start:]), start != 0, nil
}

func (s *Service) readRecoveryChange(ctx context.Context, query nativeQueryer, scope nativeScope, item string, change tracker.ChangeRequest) (tracker.ChangeDetail, error) {
	detail := tracker.ChangeDetail{Change: change, Checks: []tracker.ChangeCheck{}}
	version, found, err := readNativeChangeVersion(ctx, query, change)
	if err != nil {
		return detail, err
	}
	if found {
		version, err = readChangeVersion(ctx, query, change.ID, version.ID)
		if err != nil {
			return detail, err
		}
		detail.Versions = []tracker.ChangeVersion{version}
		push, found, err := resolveVersionPublicationPush(ctx, query, scope, item, version)
		if err != nil {
			return detail, err
		}
		if found {
			detail.PublicationPushes = append(detail.PublicationPushes, push)
		}
	}
	detail.SourceIssues, err = readChangeSourceIssues(ctx, query, scope, change.ID)
	if err != nil {
		return detail, err
	}
	detail.Reviews, err = changeRows[tracker.ChangeReview](ctx, query, `SELECT record_json FROM change_evidence WHERE change_id = ? AND kind = 'review' AND version_id = ? AND sequence IN (
 SELECT sequence FROM change_evidence WHERE change_id = ? AND kind = 'review' AND version_id = ? ORDER BY sequence DESC LIMIT 32
) OR change_id = ? AND kind = 'review' AND version_id = ? AND sequence IN (
 SELECT MAX(sequence) FROM change_evidence WHERE change_id = ? AND kind = 'review' AND version_id = ? AND json_type(record_json, '$.implementation_review') = 'object'
 UNION SELECT MAX(sequence) FROM change_evidence WHERE change_id = ? AND kind = 'review' AND version_id = ? AND json_type(record_json, '$.validator') = 'object'
 UNION SELECT MAX(sequence) FROM change_evidence WHERE change_id = ? AND kind = 'review' AND version_id = ? AND json_type(record_json, '$.security_audit') = 'object'
 UNION SELECT MAX(sequence) FROM change_evidence WHERE change_id = ? AND kind = 'review' AND version_id = ? AND json_type(record_json, '$.security_audit') = 'object' AND COALESCE(json_extract(record_json, '$.security_audit.landing'), 0) = 0
 UNION SELECT MAX(sequence) FROM change_evidence WHERE change_id = ? AND kind = 'review' AND version_id = ? AND json_type(record_json, '$.security_audit') = 'object' AND json_extract(record_json, '$.decision') = 'changes_requested'
) ORDER BY sequence`, change.ID, change.CurrentVersion, change.ID, change.CurrentVersion, change.ID, change.CurrentVersion,
		change.ID, change.CurrentVersion, change.ID, change.CurrentVersion, change.ID, change.CurrentVersion, change.ID, change.CurrentVersion, change.ID, change.CurrentVersion)
	if err != nil {
		return detail, err
	}
	for _, expected := range version.Checks {
		checks, err := changeRows[tracker.ChangeCheck](ctx, query, `SELECT record_json FROM change_evidence WHERE change_id = ? AND kind = 'check' AND version_id = ? AND json_extract(record_json, '$.check_run_id') = ? ORDER BY sequence DESC LIMIT 1`, change.ID, change.CurrentVersion, expected.CheckRunID)
		if err != nil {
			return detail, err
		}
		detail.Checks = append(detail.Checks, checks...)
	}
	detail.Discussion, err = changeRows[tracker.ChangeDiscussion](ctx, query, `SELECT record_json FROM change_evidence WHERE change_id = ? AND kind = 'discussion' AND (version_id = ? OR version_id IS NULL OR version_id = '') ORDER BY sequence DESC LIMIT 32`, change.ID, change.CurrentVersion)
	if err != nil {
		return detail, err
	}
	slices.Reverse(detail.Discussion)
	summaryDetail := detail
	summaryDetail.Reviews = slices.Clone(detail.Reviews)
	for _, decision := range []string{"approved", "changes_requested"} {
		var present bool
		err := query.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM change_evidence e WHERE e.change_id = ? AND e.version_id = ? AND e.kind = 'review'
 AND json_extract(e.record_json, '$.decision') = ? AND json_type(e.record_json, '$.implementation_review') IS NULL AND json_type(e.record_json, '$.validator') IS NULL AND json_type(e.record_json, '$.security_audit') IS NULL
 AND NOT EXISTS (SELECT 1 FROM change_evidence later WHERE later.change_id = e.change_id AND later.version_id = e.version_id AND later.kind = 'review' AND later.sequence > e.sequence
 AND json_extract(later.record_json, '$.actor.principal_id') = json_extract(e.record_json, '$.actor.principal_id') AND json_extract(later.record_json, '$.decision') != 'commented'
 AND json_type(later.record_json, '$.implementation_review') IS NULL AND json_type(later.record_json, '$.validator') IS NULL AND json_type(later.record_json, '$.security_audit') IS NULL))`, change.ID, change.CurrentVersion, decision).Scan(&present)
		if err != nil {
			return detail, err
		}
		if present {
			summaryDetail.Reviews = append(summaryDetail.Reviews, tracker.ChangeReview{VersionID: change.CurrentVersion, Decision: decision, Actor: tracker.Actor{PrincipalID: decision}})
		}
	}
	var priorApproval bool
	err = query.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM change_evidence WHERE change_id = ? AND kind = 'review' AND version_id != ? AND json_extract(record_json, '$.decision') = 'approved'
 AND json_type(record_json, '$.implementation_review') IS NULL AND json_type(record_json, '$.validator') IS NULL)`, change.ID, change.CurrentVersion).Scan(&priorApproval)
	if err != nil {
		return detail, err
	}
	detail.Summary, err = readChangeSummary(ctx, query, scope, summaryDetail, s.config.now(), priorApproval)
	if err != nil {
		return detail, err
	}
	return detail, loadChangeExternal(ctx, query, scope, &detail)
}

func boundedRecoveryReviews(reviews []tracker.ChangeReview, budget int) ([]tracker.ChangeReview, bool, error) {
	keep := make([]bool, len(reviews))
	implementation, validator, security := false, false, false
	for i := len(reviews) - 1; i >= 0; i-- {
		review := reviews[i]
		if review.ImplementationReview != nil && !implementation {
			keep[i], implementation = true, true
		}
		if review.Validator != nil && !validator {
			keep[i], validator = true, true
		}
		if review.SecurityAudit != nil && !security {
			keep[i], security = true, true
		}
		if keep[i] {
			encoded, err := json.Marshal(review)
			if err != nil {
				return nil, false, err
			}
			if len(encoded) <= budget {
				budget -= len(encoded)
			} else {
				return []tracker.ChangeReview{}, true, nil
			}
		}
	}
	for i := len(reviews) - 1; i >= 0; i-- {
		if keep[i] {
			continue
		}
		encoded, err := json.Marshal(reviews[i])
		if err != nil {
			return nil, false, err
		}
		if len(encoded) <= budget {
			keep[i] = true
			budget -= len(encoded)
		}
	}
	result := []tracker.ChangeReview{}
	for i, review := range reviews {
		if keep[i] {
			result = append(result, review)
		}
	}
	return result, len(result) != len(reviews), nil
}
