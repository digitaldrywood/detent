package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func validateChangeVersion(input tracker.ChangeVersionInput) error {
	if v := input.Validation; v != nil && (v.HeadSHA != input.HeadSHA || v.ExitCode != 0 || v.DurationNS <= 0 || strings.TrimSpace(v.Command) == "" || len(v.Command) > 4096 || len(v.Output) > 64*1024 || !validCommitID(v.TreeSHA) || !v.Evidence.Valid()) {
		return nativeRejection(nativeInvalid("Validation receipt must cover the published head and identify a successful command"), "version_validation", "validation")
	}
	for _, commit := range []string{input.BaseSHA, input.HeadSHA, input.MergeBaseSHA} {
		if !changerequest.ValidHash(commit, 40) && !changerequest.ValidHash(commit, 64) {
			return nativeInvalid("Base, head and merge-base must be lowercase commit identities")
		}
	}
	if !changerequest.ValidReference(input.Repository) || input.Code.Kind != "code" {
		return nativeInvalid("Repository and code artifact references are required")
	}
	if err := changerequest.ValidateArtifacts(append([]tracker.ChangeArtifact{input.Code}, input.Artifacts...)); err != nil {
		return nativeInvalid(err.Error())
	}
	if (input.RunID == "") != (input.AttemptID == "") || input.RunID != "" && (!validNativeID(input.RunID, "run") || !validNativeID(input.AttemptID, "attempt")) {
		return nativeInvalid("Run and attempt identities must be supplied together")
	}
	if input.External != nil {
		u, err := url.Parse(input.External.URL)
		if err != nil || u == nil || input.External.Provider != "github" || input.External.ID == "" || len(input.External.ID) > 128 || !changerequest.ValidReference(input.External.URL) || u.Scheme != "https" || u.Host != "github.com" {
			return nativeInvalid("External PR reference must identify a GitHub pull request")
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) != 4 || parts[2] != "pull" || parts[3] != input.External.ID {
			return nativeInvalid("External PR identity and URL must agree")
		}
	}
	return nil
}

func requireChangeRun(ctx context.Context, tx *sql.Tx, scope nativeScope, change tracker.ChangeRequest, request tracker.PublishChangeVersion) error {
	if request.RunID == "" {
		if scope.credential.Scope == apiScopeWorker {
			return nativeInvalid("Worker versions must identify their fenced run and attempt")
		}
		return nil
	}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT data_json FROM native_attempts WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND id = ? AND run_id = ?`, scope.organization, scope.project, change.WorkItemID, request.AttemptID, request.RunID).Scan(&raw)
	if err != nil {
		return err
	}
	var run tracker.NativeRunData
	if err := json.Unmarshal([]byte(raw), &run); err != nil {
		return err
	}
	if run.PolicyID != request.PolicyID || scope.credential.Scope == apiScopeWorker && (run.LeaseID != request.LeaseID || run.FencingToken != request.FencingToken) {
		return nativeExecutionConflict("Version run must belong to the publishing lease and approved policy")
	}
	return nil
}

func (s *Service) publishChangeVersion(c echo.Context) error {
	var request tracker.PublishChangeVersion
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if err := s.prepareChangeSource(c.Request().Context(), nativeRequestScope(c), c.Param("item"), c.Param("change"), c.Request().Method+" "+c.Request().URL.EscapedPath(), request); err != nil {
		return s.nativeAPIError(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		return s.publishChangeVersionCommand(ctx, tx, scope, c.Param("item"), c.Param("change"), request, now)
	})
}

func (s *Service) publishChangeVersionCommand(ctx context.Context, tx *sql.Tx, scope nativeScope, item, changeID string, request tracker.PublishChangeVersion, now time.Time) (any, error) {
	change, err := readChange(ctx, tx, scope, item, changeID)
	if err != nil {
		return nil, err
	}
	if change.WorkItemID != tracker.NativeWorkItemID(item) {
		return nil, nativeInvalid("Publish through the Change Request's primary issue")
	}
	if validation := request.Validation; validation != nil && validation.EvidenceArtifact != nil {
		ref := validation.EvidenceArtifact
		if ref.Validate() != nil || ref.SchemaVersion != 1 || !artifact.ValidID(ref.ServiceID, "service") || !artifact.ValidID(ref.ArtifactID, "artifact") || !artifact.ValidID(ref.ManifestID, "manifest") || !artifact.ValidHash(ref.SHA256, 64) || ref.Revision < 1 || ref.Kind != "log" || ref.State != "complete" || ref.Availability != "available" || ref.Bytes <= 0 || ref.Bytes > artifact.MaxArtifactBytes || ref.Objects < 1 || ref.Objects > artifact.MaxObjects || ref.ExpiresAt.IsZero() || ref.ObservedAt.IsZero() || ref.OrganizationID != string(scope.organization) || ref.ProjectID != string(scope.project) || ref.WorkItemID != item || ref.RunID != request.RunID || ref.AttemptID != request.AttemptID {
			return nil, nativeRejection(nativeInvalid("Validation artifact must identify complete evidence from the publishing attempt"), "version_validation", "validation")
		}
	}
	if change.CurrentVersion != request.ExpectedVersionID {
		return nil, nativeConflict(change.Revision)
	}
	delivery, associated, err := readReleaseDelivery(ctx, tx, scope, item)
	if err != nil {
		return nil, err
	}
	if associated {
		if change.ID != delivery.ChangeID || request.Repository != delivery.Repository || request.HeadSHA != delivery.HeadSHA || request.BaseSHA != delivery.BaseSHA || request.MergeBaseSHA != delivery.BaseSHA || request.Source == nil {
			return nil, nativeInvalid("Release publication must retain the exact associated PR source and base")
		}
		external := tracker.ChangeExternalReference{Provider: "github", ID: strconv.Itoa(delivery.PRNumber), URL: fmt.Sprintf("%s/pull/%d", delivery.Repository, delivery.PRNumber)}
		if request.External != nil && *request.External != external {
			return nil, nativeInvalid("Release publication differs from the associated PR")
		}
		request.External = &external
	}
	if err := validateChangeVersion(request.ChangeVersionInput); err != nil {
		return nil, err
	}
	if request.Source != nil {
		if err := request.Source.Validate(request.BaseSHA, request.HeadSHA, request.SourceBundle); err != nil {
			return nil, nativeRejection(nativeInvalid(err.Error()), "version_validation", "source")
		}
	} else if len(request.SourceBundle) != 0 {
		return nil, nativeRejection(nativeInvalid("Source bundle requires immutable source metadata"), "version_validation", "source")
	}
	var configurationJSON string
	if err := tx.QueryRowContext(ctx, "SELECT metadata_json FROM policy_revisions WHERE scope = ? AND policy_id = ?", string(scope.organization)+"/"+string(scope.project), request.PolicyID).Scan(&configurationJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nativeRejection(nativeInvalid("Change configuration has not been reported for this project"), "version_validation", "policy_id")
		}
		return nil, err
	}
	approved, err := decodeProjectPolicy(configurationJSON, policy.Approval{})
	if err != nil {
		return nil, err
	}
	rules, err := readChangePolicy(ctx, tx, scope)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, policyMismatch("An administrator must approve the expected review and CI check set")
	}
	if err != nil {
		return nil, err
	}
	if rules.PolicyID != approved.Policy.ID {
		rules.PolicyID = approved.Policy.ID
		rules.ID = changerequest.PolicyID(rules)
	}
	if err := changerequest.ValidatePolicy(rules, approved.Policy); err != nil {
		return nil, policyMismatch(err.Error())
	}
	if err := requireChangeRun(ctx, tx, scope, change, request); err != nil {
		return nil, err
	}
	version := tracker.ChangeVersion{ChangeVersionInput: request.ChangeVersionInput, ID: newNativeID("version"), ChangeID: change.ID, Number: int64(change.Revision), Policy: approved.Policy, ReviewPolicy: rules, Actor: scope.actor(), CreatedAt: now, Checks: []tracker.ChangeCheckExpectation{}}
	for _, spec := range rules.RequiredChecks {
		version.Checks = append(version.Checks, tracker.ChangeCheckExpectation{ChangeCheckSpec: spec, CheckRunID: newNativeID("check")})
	}
	raw, err := marshalNative(version)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO change_versions (id, change_id, number, record_json) VALUES (?, ?, ?, ?)", version.ID, change.ID, version.Number, raw); err != nil {
		return nil, err
	}
	if associated {
		if _, err := tx.ExecContext(ctx, "UPDATE release_deliveries SET version_id = ? WHERE work_item_id = ?", version.ID, item); err != nil {
			return nil, err
		}
	}
	if version.Source != nil {
		if _, err := tx.ExecContext(ctx, "INSERT INTO change_sources (version_id, bundle) VALUES (?, ?)", version.ID, request.SourceBundle); err != nil {
			return nil, err
		}
	}
	change.CurrentVersion, change.UpdatedAt = version.ID, now
	change.Revision++
	raw, err = marshalNative(change)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE change_requests SET record_json = ? WHERE id = ?", raw, change.ID); err != nil {
		return nil, err
	}
	ref := &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}
	for _, item := range change.LinkedIssues {
		if err := appendNativeHistory(ctx, tx, scope, string(item), "change.version_published", tracker.CollaborationData{Change: ref}, now); err != nil {
			return nil, err
		}
	}
	// A policy that requires no review and no checks makes the version
	// reviewed as published.
	if err := promoteReviewedChange(ctx, tx, scope, change, now); err != nil {
		return nil, err
	}
	return version, nil
}
