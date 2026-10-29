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

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func insertChangeEvidence(ctx context.Context, tx *sql.Tx, changeID, versionID, kind, sourceKey string, value any) error {
	raw, err := marshalNative(value)
	if err != nil {
		return err
	}
	return insertChangeEvidenceRecord(ctx, tx, changeID, versionID, kind, sourceKey, raw)
}

func insertChangeEvidenceRecord(ctx context.Context, tx *sql.Tx, changeID, versionID, kind, sourceKey, raw string) error {
	result, err := tx.ExecContext(ctx, "INSERT INTO change_evidence (change_id, version_id, kind, source_key, record_json) VALUES (?, NULLIF(?, ''), ?, NULLIF(?, ''), ?)", changeID, versionID, kind, sourceKey, raw)
	if err != nil {
		return err
	}
	sequence, err := result.LastInsertId()
	if err != nil {
		return err
	}
	return projectChangeColumns(ctx, tx, "change_evidence", sequence, raw)
}

func (s *Service) reviewChange(c echo.Context) error {
	var request tracker.ReviewChange
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		return s.reviewChangeCommand(ctx, tx, scope, c.Param("item"), c.Param("change"), c.Param("version"), request, now)
	})
}

func (s *Service) submitChangeCheck(c echo.Context) error {
	var request tracker.SubmitChangeCheck
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if s.config.Hosted != nil && nativeRequestScope(c).credential.Runner.RunnerID == "" && request.Source != "independent" {
		return s.nativeAPIError(c, nativeNotFound())
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		change, err := readChange(ctx, tx, scope, c.Param("item"), c.Param("change"))
		if err != nil {
			return nil, err
		}
		version, err := readChangeVersion(ctx, tx, change.ID, c.Param("version"))
		if err != nil {
			return nil, err
		}
		if err := changerequest.ValidateResult(version, request.ChangeCheckResult, scope.credential.ID, scope.credential.Scope == apiScopeWorker); err != nil {
			if policyErr := validateCloudChangeReferences(ctx, tx, scope, nil, request.Evidence, "evidence"); policyErr != nil {
				return nil, policyErr
			}
			return nil, nativeInvalid(err.Error())
		}
		if request.CompletedAt.After(now) {
			return nil, nativePublicInvalid("CI completion time cannot be in the future")
		}
		var previous string
		err = tx.QueryRowContext(ctx, "SELECT record_json FROM change_evidence WHERE change_id = ? AND kind = 'check' AND source_key = ?", change.ID, request.CheckRunID).Scan(&previous)
		if err == nil {
			var result tracker.ChangeCheck
			if err := decodeStorageJSON(ctx, []byte(previous), &result); err != nil {
				return nil, err
			}
			oldContent, err := marshalNative(result.ChangeCheckResult)
			if err != nil {
				return nil, err
			}
			newContent, err := marshalNative(request.ChangeCheckResult)
			if err != nil {
				return nil, err
			}
			if oldContent != newContent || result.Actor.PrincipalID != scope.credential.ID {
				return nil, nativeConflict(change.Revision)
			}
			return result, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err := validateCloudChangeReferences(ctx, tx, scope, nil, request.Evidence, "evidence"); err != nil {
			return nil, err
		}
		if err := bindCloudChangeReferences(ctx, tx, scope, string(change.WorkItemID), nil, request.Evidence, "evidence"); err != nil {
			return nil, err
		}

		check := tracker.ChangeCheck{ChangeCheckResult: request.ChangeCheckResult, VersionID: version.ID, Actor: scope.actor(), ReceivedAt: now}
		if err := insertChangeEvidence(ctx, tx, change.ID, version.ID, "check", check.CheckRunID, check); err != nil {
			return nil, err
		}
		if version.ID == change.CurrentVersion {
			if err := promoteReviewedChange(ctx, tx, scope, change, now); err != nil {
				return nil, err
			}
		}
		return check, nil
	})
}

func (s *Service) discussChange(c echo.Context) error {
	var request tracker.DiscussChange
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		return s.discussChangeCommand(ctx, tx, scope, c.Param("item"), c.Param("change"), request, now)
	})
}

func validateChangeReview(ctx context.Context, tx *sql.Tx, scope nativeScope, item, changeID, versionID string, request tracker.ReviewChange, now time.Time) (tracker.ChangeRequest, tracker.MachineID, error) {
	change, err := readChange(ctx, tx, scope, item, changeID)
	if err != nil {
		return tracker.ChangeRequest{}, "", err
	}
	if request.ExpectedRevision != 0 && request.ExpectedRevision != change.Revision {
		return tracker.ChangeRequest{}, "", nativeConflict(change.Revision)
	}
	version, err := readChangeVersion(ctx, tx, change.ID, versionID)
	if err != nil {
		return tracker.ChangeRequest{}, "", err
	}
	if request.Decision == "approved" && change.CurrentVersion != version.ID || request.ExpectedVersionID != "" && (request.ExpectedVersionID != version.ID || change.CurrentVersion != version.ID) {
		return tracker.ChangeRequest{}, "", nativeConflict(change.Revision)
	}
	if request.Bundle != nil {
		if err := validateReviewBundle(ctx, tx, scope, change, version, *request.Bundle, now); err != nil {
			return tracker.ChangeRequest{}, "", err
		}
	}
	if !slices.Contains([]string{"approved", "changes_requested", "commented"}, request.Decision) || len(request.Body) > 64<<10 {
		err := nativePublicInvalid("Review decision is invalid or body exceeds 64 KiB")
		if scope.credential.Scope == apiScopeWorker && request.Validator != nil && request.ImplementationReview == nil && request.SecurityAudit == nil {
			field := "decision"
			if len(request.Body) > 64<<10 {
				field = "body"
			}
			err = nativeRejection(err, "acceptance_validation", field)
		}
		if scope.credential.Scope == apiScopeWorker && request.SecurityAudit != nil {
			field := "decision"
			if len(request.Body) > 64<<10 {
				field = "body"
			}
			err = securityAuditRejection("Review decision is invalid or body exceeds 64 KiB", field)
		}
		return tracker.ChangeRequest{}, "", err
	}
	if change.CurrentLanding() != nil {
		return tracker.ChangeRequest{}, "", nativeConflict(change.Revision)
	}
	if err := validateCloudChangeReferences(ctx, tx, scope, changeReviewContentFields(request), nil, ""); err != nil {
		return tracker.ChangeRequest{}, "", err
	}

	sessionMachineID, err := validateNativeValidatorReview(ctx, tx, scope, change, version, request)
	if err != nil {
		return tracker.ChangeRequest{}, "", err
	}
	return change, sessionMachineID, nil
}

func changeReviewContentFields(request tracker.ReviewChange) []cloudContentField {
	fields := []cloudContentField{{name: "body", value: request.Body}}
	for _, result := range []struct {
		name  string
		value *gate.ValidatorResult
	}{{"validator", request.Validator}, {"implementation_review", request.ImplementationReview}} {
		if result.value == nil {
			continue
		}
		for i, evidence := range result.value.CriteriaEvidence {
			fields = append(fields, cloudContentField{name: fmt.Sprintf("%s.criteria_evidence[%d].reference", result.name, i), value: evidence.Reference})
		}
	}
	return fields
}

func (s *Service) reviewChangeCommand(ctx context.Context, tx *sql.Tx, scope nativeScope, item, changeID, versionID string, request tracker.ReviewChange, now time.Time) (any, error) {
	fields := changeReviewContentFields(request)
	var change tracker.ChangeRequest
	var sessionMachineID tracker.MachineID
	if prepared := preparedChangeMutation(ctx); prepared != nil {
		change, sessionMachineID, request = prepared.change, prepared.sessionMachineID, prepared.review
		fields = prepared.reviewFields
		if request.Bundle != nil {
			if err := validateReviewBundle(ctx, tx, scope, change, prepared.version, *request.Bundle, now); err != nil {
				return nil, err
			}
		}
	} else {
		var err error
		change, sessionMachineID, err = validateChangeReview(ctx, tx, scope, item, changeID, versionID, request, now)
		if err != nil {
			return nil, err
		}
	}

	if err := bindCloudChangeReferences(ctx, tx, scope, string(change.WorkItemID), fields, nil, ""); err != nil {
		return nil, err
	}

	review := tracker.ChangeReview{SecurityAudit: request.SecurityAudit, SessionMachineID: sessionMachineID, ImplementationReview: request.ImplementationReview, Validator: request.Validator, ID: newNativeID("review"), VersionID: versionID, Decision: request.Decision, Body: request.Body, Actor: scope.actor(), CreatedAt: now}
	if prepared := preparedChangeMutation(ctx); prepared != nil {
		review = prepared.record
	} else {
		reviewAttributionSlots(&review)
	}
	if request.Decision == "changes_requested" {
		if err := routeIndependentReview(ctx, tx, scope, change, &review, now); err != nil {
			return nil, err
		}
	}
	if err := storeChangeReview(ctx, tx, string(scope.organization), change.ID, review); err != nil {
		return nil, err
	}

	switch request.Decision {
	case "approved":
		if err := promotePreparedChange(ctx, tx, scope, change, now); err != nil {
			return nil, err
		}
	case "changes_requested":
		if err := returnChangeForRework(ctx, tx, scope, change, review, now); err != nil {
			return nil, err
		}
	}
	return review, nil
}

func (s *Service) discussChangeCommand(ctx context.Context, tx *sql.Tx, scope nativeScope, item, changeID string, request tracker.DiscussChange, now time.Time) (any, error) {
	change, err := readChange(ctx, tx, scope, item, changeID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.Body) == "" || len(request.Body) > 64<<10 {
		return nil, nativePublicInvalid("Discussion requires 1 byte to 64 KiB")
	}
	if request.VersionID != "" {
		if _, err := readChangeVersion(ctx, tx, change.ID, request.VersionID); err != nil {
			return nil, err
		}
	}
	if request.Escape != nil {
		if err := validateQualityEscape(ctx, tx, scope, change, request, now); err != nil {
			return nil, err
		}
	}
	if err := validateNativeProvenance(scope, request.Provenance); err != nil {
		return nil, err
	}
	sourceKey := ""
	if request.Provenance != nil {
		sourceKey = request.Provenance.Provider + ":" + request.Provenance.ExternalID
		var raw string
		err := tx.QueryRowContext(ctx, "SELECT record_json FROM change_evidence WHERE change_id = ? AND kind = 'discussion' AND source_key = ?", change.ID, sourceKey).Scan(&raw)
		if err == nil {
			var existing tracker.ChangeDiscussion
			err = decodeStorageJSON(ctx, []byte(raw), &existing)
			return existing, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	fields := []cloudContentField{{name: "body", value: request.Body}}
	if err := validateCloudChangeReferences(ctx, tx, scope, fields, nil, ""); err != nil {
		return nil, err
	}
	if err := bindCloudChangeReferences(ctx, tx, scope, string(change.WorkItemID), fields, nil, ""); err != nil {
		return nil, err
	}

	comment := tracker.ChangeDiscussion{Escape: request.Escape, ID: newNativeID("cmt"), VersionID: request.VersionID, Body: request.Body, Actor: scope.actor(), Provenance: request.Provenance, CreatedAt: now}
	return comment, insertChangeEvidence(ctx, tx, change.ID, comment.VersionID, "discussion", sourceKey, comment)
}

func storeChangeReview(ctx context.Context, tx *sql.Tx, organization, changeID string, review tracker.ChangeReview) error {
	if prepared := preparedChangeMutation(ctx); prepared != nil && prepared.record.ID == review.ID {
		if err := storeAttributionSlots(ctx, tx, review.VersionID, review.ID, prepared.reviewSlots); err != nil {
			return err
		}
		if _, ok := ctx.Value(changeContentKey{}).(*changeContent); ok {
			for role, result := range map[string]*gate.ValidatorResult{"validator": review.Validator, "implementation_review": review.ImplementationReview} {
				if result == nil {
					continue
				}
				for i, command := range result.Commands {
					if err := persistCommandOutput(ctx, tx, organization, review.VersionID, review.ID, role, i, &command); err != nil {
						return err
					}
				}
			}
		}
		raw := prepared.reviewRaw
		if len(review.RepairDependencies) != 0 || review.RepairRefusal != "" {
			stored := prepared.storedReview
			stored.RepairDependencies, stored.RepairRefusal = review.RepairDependencies, review.RepairRefusal
			var err error
			raw, err = marshalNative(stored)
			if err != nil {
				return err
			}
		}
		return insertChangeEvidenceRecord(ctx, tx, changeID, review.VersionID, "review", "", raw)
	}
	slots := reviewAttributionSlots(&review)
	if err := storeAttributionSlots(ctx, tx, review.VersionID, review.ID, slots); err != nil {
		return err
	}
	storedReview := review
	if _, ok := ctx.Value(changeContentKey{}).(*changeContent); ok {
		raw, err := json.Marshal(review)
		if err != nil {
			return err
		}
		storedReview = tracker.ChangeReview{}
		if err := decodeStorageJSON(ctx, raw, &storedReview); err != nil {
			return err
		}
		for role, result := range map[string]*gate.ValidatorResult{"validator": storedReview.Validator, "implementation_review": storedReview.ImplementationReview} {
			if result == nil {
				continue
			}
			for i := range result.Commands {
				if err := persistCommandOutput(ctx, tx, organization, review.VersionID, review.ID, role, i, &result.Commands[i]); err != nil {
					return err
				}
			}
		}
	}
	if err := insertChangeEvidence(ctx, tx, changeID, review.VersionID, "review", "", storedReview); err != nil {
		return err
	}
	return nil
}
