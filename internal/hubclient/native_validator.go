package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) ValidatorVersion(ctx context.Context) (runner.NativeValidation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.change == nil || e.change.Landing != nil || e.change.VersionID == "" || e.lastDiff == nil {
		return runner.NativeValidation{}, nil
	}
	detail, err := e.claim.source.client.Change(ctx, e.claim.lease.WorkItemID, e.change.ChangeID)
	if err != nil {
		return runner.NativeValidation{}, err
	}
	for _, version := range detail.Versions {
		if version.ID != e.change.VersionID || version.ID != detail.Change.CurrentVersion {
			continue
		}
		if version.HeadSHA != e.lastDiff.HeadSHA || version.BaseSHA != e.lastDiff.BaseSHA || version.PolicyID != e.data.PolicyID {
			return runner.NativeValidation{}, errors.New("native validator version differs from the published source")
		}
		if e.publication != nil && e.role == runner.RoleRework && e.recoveredSource != nil &&
			*e.recoveredSource == e.publication.SourceVersion && e.publication.Matches(detail.Change.ID, version) {
			return runner.NativeValidation{}, nil
		}
		if !version.Policy.Gates.Validator {
			return runner.NativeValidation{}, nil
		}
		raw, err := json.Marshal(e.lastDiff.Files)
		if err != nil {
			return runner.NativeValidation{}, err
		}
		diff := &connector.ValidationDiff{Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, Digest: policy.Digest(raw)}
		var patch strings.Builder
		complete := true
		for _, file := range e.lastDiff.Files {
			diff.Files = append(diff.Files, file.Path)
			patch.WriteString(file.Patch)
			complete = complete && !file.Truncated && !file.Denied
		}
		if complete {
			diff.Patch = patch.String()
		}
		return runner.NativeValidation{Version: &version, Diff: diff}, nil
	}
	return runner.NativeValidation{}, errors.New("native validator version is no longer current")
}

func (e *nativeExecution) RecordValidator(ctx context.Context, result gate.ValidatorResult) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.change == nil || e.change.VersionID != result.VersionID {
		return errors.New("native validator result differs from the published version")
	}
	decision := "commented"
	switch result.Verdict {
	case gate.ValidatorVerdictPass:
		decision = "approved"
	case gate.ValidatorVerdictRework:
		decision = "changes_requested"
	}
	var body strings.Builder
	body.WriteString(result.Summary)
	if len(result.Commands) > 0 {
		fmt.Fprintf(&body, "\n\nHost-observed validation command evidence: %d receipts in this review's structured validator.commands field, including command, head/tree, exit status and captured output.", len(result.Commands))
	}

	for _, finding := range result.Findings {
		fmt.Fprintf(&body, "\n\n%s %s: %s", finding.Severity, finding.Path, finding.Body)
	}
	client, item := e.claim.source.client, e.claim.lease.WorkItemID
	_, err := client.ReviewChange(ctx, item, e.change.ChangeID, result.VersionID, tracker.ReviewChange{
		Mutation:          tracker.Mutation{IdempotencyKey: e.data.AttemptID + ":validator:" + strconv.FormatInt(result.SessionID, 10), LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken},
		ExpectedVersionID: result.VersionID, Decision: decision, Body: body.String(), Validator: &result,
	})
	if err != nil {
		return err
	}
	detail, err := client.Change(ctx, item, e.change.ChangeID)
	if err != nil {
		return err
	}
	e.change.Validator = &result
	e.change.Reviewed = detail.Change.CurrentVersion == result.VersionID && detail.Summary.Status == "reviewed"
	return nil
}
