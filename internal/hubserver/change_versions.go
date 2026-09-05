package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func validateChangeVersion(input tracker.ChangeVersionInput) error {
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
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		change, err := readChange(ctx, tx, scope, c.Param("item"), c.Param("change"))
		if err != nil {
			return nil, err
		}
		if change.WorkItemID != tracker.NativeWorkItemID(c.Param("item")) {
			return nil, nativeInvalid("Publish through the Change Request's primary issue")
		}
		if change.CurrentVersion != request.ExpectedVersionID {
			return nil, nativeConflict(change.Revision)
		}
		if err := validateChangeVersion(request.ChangeVersionInput); err != nil {
			return nil, err
		}
		approved, err := readProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project))
		if err != nil {
			return nil, err
		}
		if request.PolicyID != approved.Policy.ID {
			return nil, policyMismatch("Publish with the currently approved repository policy")
		}
		rules, err := readChangePolicy(ctx, tx, scope)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, policyMismatch("An administrator must approve the expected review and CI check set")
		}
		if err != nil {
			return nil, err
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
		return version, nil
	})
}
