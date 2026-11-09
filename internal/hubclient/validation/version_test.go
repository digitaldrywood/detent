package validation

import (
	"context"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type retainedVersionReader struct {
	detail  tracker.ChangeDetail
	runtime tracker.NativeRuntimeEvidence
}

func (r retainedVersionReader) RuntimeChange(context.Context, tracker.NativeWorkItemID, string) (tracker.ChangeDetail, error) {
	return r.detail, nil
}
func (r retainedVersionReader) Recovery(context.Context, tracker.NativeWorkItemID) (tracker.NativeRecovery, error) {
	return tracker.NativeRecovery{}, nil
}
func (r retainedVersionReader) RuntimeEvidence(context.Context, tracker.NativeWorkItemID, string, ...tracker.NativeAdmissionContext) (tracker.NativeRuntimeEvidence, error) {
	return r.runtime, nil
}

type retainedVersionAuthority struct{ capture *tracker.ChangeSourceCapture }

func (a retainedVersionAuthority) Retain(context.Context, tracker.AttemptDiffRequest) error {
	return nil
}
func (a retainedVersionAuthority) Retained() *tracker.ChangeSourceCapture { return a.capture }
func (retainedVersionAuthority) SetDiscussion([]tracker.NativeComment)    {}
func (retainedVersionAuthority) ApplyAuditHumanAction(*tracker.SecurityAuditReview) {
}

func TestAuditRequiresCompleteRetainedPatch(t *testing.T) {
	for _, test := range []struct {
		name     string
		patch    string
		empty    bool
		disabled bool
		refused  bool
	}{
		{name: "complete changed source", patch: "diff --git a/example b/example"},
		{name: "incomplete changed source", refused: true},
		{name: "verified empty source", empty: true},
		{name: "disabled audit does not read patch", disabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := tracker.ChangeSource{BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), DiffSHA256: strings.Repeat("c", 64)}
			if test.empty {
				source.DiffSHA256 = tracker.ChangeSourceDigest(nil)
			}
			version := tracker.ChangeVersion{ID: "version", ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: source.BaseSHA, HeadSHA: source.HeadSHA, PolicyID: "policy", Source: &source}, Policy: policy.Descriptor{Gates: policy.Gates{SecurityAudit: !test.disabled}}}
			data := tracker.NativeRunData{AttemptID: "attempt", LeaseID: "lease", FencingToken: 1, PolicyID: "policy"}
			reader := retainedVersionReader{
				detail:  tracker.ChangeDetail{Change: tracker.ChangeRequest{ID: "change", CurrentVersion: version.ID}, Versions: []tracker.ChangeVersion{version}},
				runtime: tracker.NativeRuntimeEvidence{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org", ProjectID: "project", WorkItemID: "item"}}, Attempt: &tracker.NativeAttempt{NativeRunData: data}},
			}
			authority := retainedVersionAuthority{capture: &tracker.ChangeSourceCapture{Source: source, Patch: test.patch}}
			state := VersionState{Change: &runner.NativeChange{ChangeID: "change", VersionID: version.ID}, Diff: &tracker.AttemptDiffRequest{BaseSHA: source.BaseSHA, HeadSHA: source.HeadSHA}, Data: data, Lease: tracker.NativeLease{ID: "lease", FencingToken: 1, WorkItemID: "item"}, Organization: "org", Project: "project", SourceRequired: true}
			input, err := PrepareVersion(t.Context(), reader, authority, state, "security_audit")
			if (err != nil) != test.refused || test.refused && input.Diff != nil {
				t.Fatalf("audit source completeness: diff=%+v error=%v, want refusal %t", input.Diff, err, test.refused)
			}
			if !test.disabled && !test.refused && (input.Diff == nil || input.Diff.Patch != test.patch) {
				t.Fatal("audit lost its complete retained source")
			}
		})
	}
}
