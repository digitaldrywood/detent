package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"slices"
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
	Section       int                     `json:"section"`
	After         int64                   `json:"after"`
	Offset        int64                   `json:"offset"`
	Digest        string                  `json:"digest,omitempty"`
	Change        string                  `json:"change,omitempty"`
	Version       string                  `json:"version,omitempty"`
	IssueRevision tracker.Revision        `json:"issue_revision,omitempty"`
	Attempt       *retainedAttemptReadPin `json:"attempt,omitempty"`
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
		if keep[i] || reviews[i].ImplementationReview != nil || reviews[i].Validator != nil {
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
