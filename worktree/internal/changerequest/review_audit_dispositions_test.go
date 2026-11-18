package changerequest

import (
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAuditDispositionSummaryPreservesReviewHistory(t *testing.T) {
	for _, test := range []struct {
		name                                                 string
		unresolved, mismatch, pending, stalePolicy, wrongRun bool
	}{
		{name: "accepted exact run"},
		{name: "another unresolved finding", unresolved: true},
		{name: "different current source", mismatch: true},
		{name: "other gates pending", pending: true},
		{name: "current policy changed", pending: true, stalePolicy: true},
		{name: "different audit run", pending: true, wrongRun: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			detail, now := reviewFixture()
			detail.Change.ProjectID = "prj_example"
			version := &detail.Versions[0]
			version.Repository, version.BaseSHA = "example-org/example-project", strings.Repeat("b", 40)
			version.Policy = policy.Descriptor{SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")), Gates: policy.Gates{Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("Plan Review")), SecurityAudit: true, AutomatedReview: "optional", MergeMethod: "squash"}}.WithID()
			if err := version.Policy.Validate(); err != nil {
				t.Fatal(err)
			}
			version.PolicyID = version.Policy.ID
			run := securityaudit.Run{ID: 42, InvocationID: "audit", ProjectID: "prj_example", Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), ExitStatus: securityaudit.ExitStatusSuccess, OutputBytes: 1, OutputDigest: securityaudit.OutputDigest("result"), WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: now, StartedAt: now, CompletedAt: now, Verdict: "fail", Summary: "Finding", Findings: []securityaudit.Finding{{ID: "accepted", Severity: "p1", Body: "Example finding", Certainty: securityaudit.CertaintyConfirmed}}}
			if test.unresolved {
				run.Findings = append(run.Findings, securityaudit.Finding{ID: "unresolved", Severity: "p1", Body: "Another finding", Certainty: securityaudit.CertaintyConfirmed})
			}
			if test.mismatch {
				version.HeadSHA = strings.Repeat("c", 40)
			}
			review := tracker.ChangeReview{VersionID: version.ID, Decision: "changes_requested", SecurityAudit: &tracker.SecurityAuditReview{Run: run, Verdict: "rework"}}
			detail.Reviews = append(detail.Reviews, review)
			dispositions := map[int64][]securityaudit.Disposition{run.ID: {{AuditRunID: run.ID, FindingID: "accepted", Status: securityaudit.DispositionFalsePositive, Evidence: "Verified", ServiceIdentity: securityaudit.ServiceIdentity("prj_example")}}}
			if test.pending {
				detail.Checks = nil
			}
			if test.wrongRun {
				dispositions[run.ID][0].AuditRunID++
			}
			policyID := version.PolicyID
			if test.stalePolicy {
				policyID = "changed"
			}
			detail.Summary = SummarizeCurrentVersionWithAuditDispositions(detail, policyID, version.ReviewPolicy.ID, now, false, dispositions)
			cleared := !test.unresolved && !test.mismatch && !test.stalePolicy && !test.wrongRun
			if (detail.Summary.SecurityAudit == "passed") != cleared {
				t.Fatalf("audit status = %q, want cleared=%t", detail.Summary.SecurityAudit, cleared)
			}
			want := cleared && !test.pending
			if (detail.Summary.Status == "reviewed") != want {
				t.Fatalf("summary = %+v, want reviewed=%t", detail.Summary, want)
			}
			if state := ReviewState(detail, version.ID, version.HeadSHA); state.Reviewed != want || state.Current && state.ChangesRequested == cleared {
				t.Fatalf("gate = %+v, want reviewed=%t", state, want)
			}
			if !reflect.DeepEqual(detail.Reviews[1], review) {
				t.Fatal("summary mutated immutable audit review")
			}
		})
	}
}
