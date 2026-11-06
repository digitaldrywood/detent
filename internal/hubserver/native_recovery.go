package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/runtimeoutput"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readNativeRecovery(ctx context.Context, scope nativeScope, item string, projected ...bool) (tracker.NativeRecovery, error) {
	ctx = s.contentContext(s.runDetailContext(ctx))
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	return s.readNativeRecoveryTenant(ctx, scope, item)
}

type recoveryEvidenceCursor struct {
	Section       int              `json:"section"`
	After         int64            `json:"after"`
	Offset        int64            `json:"offset"`
	Digest        string           `json:"digest,omitempty"`
	Change        string           `json:"change,omitempty"`
	Version       string           `json:"version,omitempty"`
	IssueRevision tracker.Revision `json:"issue_revision,omitempty"`
}

func (s *Service) readNativeRecoveryPage(ctx context.Context, scope nativeScope, item, path string, params url.Values) (tracker.NativeRecovery, error) {
	return s.readNativeRecoveryPageTenant(s.contentContext(ctx), scope, item, path, params)
}

func recoveryRecordFragment(page *tracker.NativeRecoveryPage, content []byte, offset int64, expectedDigest string) error {
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	if offset < 0 || offset >= int64(len(content)) || expectedDigest != "" && expectedDigest != sha {
		return nativeConflict(0)
	}
	end := min(offset+(64<<10), int64(len(content)))
	page.Offset, page.TotalBytes, page.SHA256, page.Data = offset, int64(len(content)), sha, content[offset:end]
	return nil
}

func (s *Service) readRecoveryEvidenceRecord(ctx context.Context, query nativeQueryer, scope nativeScope, item, section string, cursor recoveryEvidenceCursor) (int64, string, string, []byte, error) {
	ctx = payloadQueryContext(ctx, query)
	var sequence int64
	var id, version string
	var value any
	switch section {
	case "issue_body":
		if cursor.IssueRevision == 0 || cursor.After > 0 {
			return 0, "", "", nil, sql.ErrNoRows
		}
		var body string
		if err := query.QueryRowContext(ctx, `SELECT body FROM issues WHERE organization_id = ? AND project_id = ? AND native_id = ?`, scope.organization, scope.project, item).Scan(&body); err != nil {
			return 0, "", "", nil, err
		}
		value, sequence, id = body, 1, item
	case "reviews", "checks", "change_discussion":
		kind := map[string]string{"reviews": "review", "checks": "check", "change_discussion": "discussion"}[section]
		var content []byte
		err := query.QueryRowContext(ctx, `SELECT sequence, COALESCE(version_id,''), COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_evidence WHERE change_id = ? AND kind = ? AND sequence > ? ORDER BY sequence LIMIT 1`, cursor.Change, kind, cursor.After).Scan(&sequence, &version, &content)
		if err != nil {
			return sequence, "", version, nil, err
		}
		query, err = archiveQuery(ctx, query, item, "change", cursor.Change, 0)
		if err != nil {
			return sequence, "", version, nil, err
		}
		if _, archived := query.(*nativeArchiveQuery); archived {
			err = query.QueryRowContext(ctx, "SELECT COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_evidence WHERE change_id=? AND sequence=?", cursor.Change, sequence).Scan(&content)
		}
		if err == nil {
			content, err = resolveStorageJSON(ctx, content)
		}
		if err == nil && kind == "review" {
			var review tracker.ChangeReview
			if err = decodeStorageJSON(ctx, content, &review); err == nil {
				err = hydrateChangeRecord(ctx, query, &review)
			}
			if err == nil {
				content, err = json.Marshal(review)
			}
		}
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
		query, err = archiveQuery(ctx, query, item, "change", cursor.Change, 0)
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
		return 0, "", "", nil, nativePublicInvalid("Recovery section is invalid")
	}
	content, err := json.Marshal(value)
	return sequence, id, version, content, err
}

func (s *Service) fitNativeRecovery(ctx context.Context, scope nativeScope, path string, result *tracker.NativeRecovery, bodyFragments bool) error {
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded)+1 <= 1<<20 {
		return err
	}
	if result.Read == nil {
		result.Read = &tracker.NativeRecoveryRead{DetailPath: path + "?view=recovery&limit=1"}
	}
	fits := func() (bool, error) {
		slices.Sort(result.Read.Omitted)
		result.Read.Omitted = slices.Compact(result.Read.Omitted)
		encoded, err := json.Marshal(result)
		return len(encoded)+1 <= 1<<20, err
	}
	if detail := result.ChangeDetail; detail != nil {
		for i := range detail.Versions {
			detail.Versions[i] = boundedChangeVersion(detail.Versions[i])
		}
		result.Read.Omitted = append(result.Read.Omitted, "change_detail.versions.policy")
	}
	if ok, err := fits(); ok || err != nil {
		return err
	}
	if bodyFragments && result.Issue.Body != "" {
		_, cursor, key, err := s.readNativePageLimit(ctx, scope, path, url.Values{"view": {"recovery"}}, 1, 1)
		if err != nil {
			return err
		}
		body, err := json.Marshal(result.Issue.Body)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(body)
		sha := hex.EncodeToString(digest[:])
		selection := recoveryEvidenceCursor{Section: 7, IssueRevision: result.Issue.Revision, Digest: sha}
		if result.ChangeDetail != nil {
			selection.Change, selection.Version = result.ChangeDetail.Change.ID, result.ChangeDetail.Change.CurrentVersion
		}
		if result.Change != nil {
			selection.Change, selection.Version = result.Change.ChangeID, result.Change.VersionID
		}
		position, err := json.Marshal(selection)
		if err != nil {
			return err
		}
		cursor.After = string(position)
		encoded, err := encodeNativeCursor(cursor, key)
		if err != nil {
			return err
		}
		result.Read.IssueBody = &tracker.NativeRecoveryBodyRead{Cursor: encoded, SHA256: sha, TotalBytes: int64(len(body))}
		result.Issue.Body = ""
		result.Read.Omitted = append(result.Read.Omitted, "issue.body")
	}
	if ok, err := fits(); ok || err != nil {
		return err
	}
	result.Discussion = nil
	result.Read.Omitted = append(result.Read.Omitted, "discussion")
	if detail := result.ChangeDetail; detail != nil {
		detail.Discussion, detail.Checks = nil, nil
		result.Read.Omitted = append(result.Read.Omitted, "change_detail.discussion", "change_detail.checks")
	}
	if ok, err := fits(); ok || err != nil {
		return err
	}
	if detail := result.ChangeDetail; detail != nil {
		detail.Reviews = nil
		result.Read.Omitted = append(result.Read.Omitted, "change_detail.reviews")
	}
	if ok, err := fits(); ok || err != nil {
		return err
	}
	return nativePublicInvalid("Required recovery metadata exceeds the bounded response")
}

func projectNativeRecovery(result *tracker.NativeRecovery) error {
	landingBudget := 64 << 10
	var landings []*tracker.NativeLandingReceipt
	if result.ChangeDetail != nil {
		for _, version := range result.ChangeDetail.Versions {
			landings = append(landings, version.Landing)
		}
	}
	for _, attempt := range result.Attempts {
		if attempt.Runtime != nil {
			landings = append(landings, attempt.Runtime.Landing)
		}
	}
	for _, landing := range landings {
		if landing != nil && landing.SecurityAudit != nil {
			reference, err := json.Marshal(omittedRecoveryAudit(landing.SecurityAudit))
			if err != nil {
				return err
			}
			landingBudget -= len(reference)
		}
	}
	if detail := result.ChangeDetail; detail != nil {
		for i := range detail.Versions {
			detail.Versions[i].Validation = boundedChangeCommand(detail.Versions[i].Validation)
			landing, omitted, err := boundedRecoveryLanding(detail.Versions[i].Landing, &landingBudget)
			if err != nil {
				return err
			}
			detail.Versions[i].Landing = landing
			if omitted {
				result.Read.Omitted = append(result.Read.Omitted, "change_detail.versions.landing.security_audit.run")
			}
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
			landing, omitted, err := boundedRecoveryLanding(runtime.Landing, &landingBudget)
			if err != nil {
				return err
			}
			runtime.Landing = landing
			if omitted {
				result.Read.Omitted = append(result.Read.Omitted, "attempts.runtime.landing.security_audit.run")
			}
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

func boundedRecoveryLanding(landing *tracker.NativeLandingReceipt, budget *int) (*tracker.NativeLandingReceipt, bool, error) {
	result := boundedChangeLanding(landing)
	if result == nil || result.SecurityAudit == nil {
		return result, false, nil
	}
	result.SecurityAudit = boundedRecoveryAudit(result.SecurityAudit)
	encoded, err := json.Marshal(result.SecurityAudit)
	if err != nil {
		return nil, false, err
	}
	reference := omittedRecoveryAudit(result.SecurityAudit)
	referenceBytes, err := json.Marshal(reference)
	if err != nil {
		return nil, false, err
	}
	additional := max(0, len(encoded)-len(referenceBytes))
	omitted := additional > *budget
	if omitted {
		result.SecurityAudit = reference
	} else {
		*budget -= additional
	}
	return result, omitted, nil
}

func omittedRecoveryAudit(audit *tracker.SecurityAuditReview) *tracker.SecurityAuditReview {
	result := *audit
	result.RunOmitted = true
	result.TextTruncated = true
	result.Run = securityaudit.Run{}
	return &result
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
	var unknown bool
	if err := query.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM change_evidence WHERE change_id=? AND version_id=? AND kind='review' AND has_validator IS NULL)`, change.ID, change.CurrentVersion).Scan(&unknown); err != nil {
		return tracker.ChangeDetail{}, err
	}

	detail := tracker.ChangeDetail{Change: change, Checks: []tracker.ChangeCheck{}}
	var err error
	query, err = archiveQuery(ctx, query, item, "change", change.ID, 0)
	if err != nil {
		return detail, err
	}
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
	legacyDecisions := map[string]bool{}
	if unknown {
		detail.Reviews, legacyDecisions, err = legacyRecoveryReviews(ctx, query, change.ID, change.CurrentVersion)
	} else {
		detail.Reviews, err = changeRows[tracker.ChangeReview](ctx, query, `SELECT COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_evidence WHERE change_id = ? AND kind = 'review' AND version_id = ? AND sequence IN (
 SELECT sequence FROM change_evidence WHERE change_id = ? AND kind = 'review' AND version_id = ? ORDER BY sequence DESC LIMIT 32
) OR change_id = ? AND kind = 'review' AND version_id = ? AND sequence IN (
 SELECT MAX(sequence) FROM change_evidence WHERE change_id = ? AND kind = 'review' AND version_id = ? AND has_implementation_review=1
 UNION SELECT MAX(sequence) FROM change_evidence WHERE change_id = ? AND kind = 'review' AND version_id = ? AND has_validator=1
 UNION SELECT MAX(sequence) FROM change_evidence WHERE change_id = ? AND kind = 'review' AND version_id = ? AND has_security_audit=1
 UNION SELECT MAX(sequence) FROM change_evidence WHERE change_id = ? AND kind = 'review' AND version_id = ? AND has_security_audit=1 AND security_audit_landing = 0
 UNION SELECT MAX(sequence) FROM change_evidence WHERE change_id = ? AND kind = 'review' AND version_id = ? AND has_security_audit=1 AND claim_review_decision = 'changes_requested'
) ORDER BY sequence`, change.ID, change.CurrentVersion, change.ID, change.CurrentVersion, change.ID, change.CurrentVersion,
			change.ID, change.CurrentVersion, change.ID, change.CurrentVersion, change.ID, change.CurrentVersion, change.ID, change.CurrentVersion, change.ID, change.CurrentVersion)
	}

	if err != nil {
		return detail, err
	}
	for _, expected := range version.Checks {
		checks, err := readRecoveryChangeCheck(ctx, query, change.ID, change.CurrentVersion, expected.CheckRunID)
		if err != nil {
			return detail, err
		}
		detail.Checks = append(detail.Checks, checks...)
	}
	detail.Discussion, err = changeRows[tracker.ChangeDiscussion](ctx, query, `SELECT COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_evidence WHERE change_id = ? AND kind = 'discussion' AND (version_id = ? OR version_id IS NULL OR version_id = '') ORDER BY sequence DESC LIMIT 32`, change.ID, change.CurrentVersion)
	if err != nil {
		return detail, err
	}
	slices.Reverse(detail.Discussion)
	summaryDetail := detail
	summaryDetail.Reviews = slices.Clone(detail.Reviews)
	for _, decision := range []string{"approved", "changes_requested"} {
		var present bool

		if unknown {
			present = legacyDecisions[decision]
		} else {
			err = query.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM change_evidence e WHERE e.change_id = ? AND e.version_id = ? AND e.kind = 'review'
 AND e.claim_review_decision = ? AND e.has_implementation_review=0 AND e.has_validator=0 AND e.has_security_audit=0
 AND NOT EXISTS (SELECT 1 FROM change_evidence later WHERE later.change_id = e.change_id AND later.version_id = e.version_id AND later.kind = 'review' AND later.sequence > e.sequence
 AND later.claim_review_actor = e.claim_review_actor AND later.claim_review_decision != 'commented'
 AND later.has_implementation_review=0 AND later.has_validator=0 AND later.has_security_audit=0))`, change.ID, change.CurrentVersion, decision).Scan(&present)
		}

		if err != nil {
			return detail, err
		}
		if present {
			summaryDetail.Reviews = append(summaryDetail.Reviews, tracker.ChangeReview{VersionID: change.CurrentVersion, Decision: decision, Actor: tracker.Actor{PrincipalID: decision}})
		}
	}
	var priorApproval bool
	priorApproval, err = readPriorChangeApproval(ctx, query, change.ID, change.CurrentVersion)
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

// readLatestLandingExit returns the item's latest move out of Merging. Its
// reason records why landing stopped, which the next attempt acts on.
func readLatestLandingExit(ctx context.Context, tx nativeQueryer, scope nativeScope, item string) (tracker.CollaborationEvent, bool, error) {
	event := tracker.CollaborationEvent{OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "work_item", AggregateID: tracker.NativeWorkItemID(item)}
	before := int64(1<<63 - 1)
	for {
		var actor, data, at string
		var from sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT id,sequence,type,actor_json,data_json,recorded_at,from_state FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='workflow.transitioned' AND sequence<? AND (lower(from_state)='merging' OR from_state IS NULL) ORDER BY sequence DESC LIMIT 1`, scope.organization, scope.project, item, before).Scan(&event.ID, &event.AggregateSequence, &event.Type, &actor, &data, &at, &from)
		if errors.Is(err, sql.ErrNoRows) {
			return event, false, nil
		}
		if err != nil {
			return event, false, err
		}
		if err := json.Unmarshal([]byte(actor), &event.Actor); err != nil {
			return event, false, err
		}
		if err := decodeChangeColumnsJSON(ctx, []byte(data), &event.Data); err != nil {
			return event, false, err
		}
		if !from.Valid && !strings.EqualFold(event.Data.FromState, "merging") {
			before = event.AggregateSequence
			continue
		}
		event.RecordedAt, err = parseTimeValue(at)
		return event, err == nil, err
	}
}
