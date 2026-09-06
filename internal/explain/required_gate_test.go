package explain

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestRequiredGateSeparatesCIFromAggregateEvidence(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		required     *telemetry.RequiredGate
		mergeability string
		want         GateState
	}{
		{name: "exact head audit failure", required: &telemetry.RequiredGate{PRNumber: 11, HeadSHA: "head", BaseSHA: "base", State: "failed", Reason: "security_audit_findings", AuditRunID: 13, AuditReason: "unresolved_findings"}, want: GateFailed},
		{name: "cached pass with current conflict", required: &telemetry.RequiredGate{PRNumber: 11, HeadSHA: "head", BaseSHA: "base", State: "passed", Reason: "ready"}, mergeability: "dirty", want: GateFailed},
		{name: "conflict", mergeability: "dirty", want: GateFailed},
		{name: "conflicting", mergeability: "conflicting", want: GateFailed},
		{name: "capability blocker", required: &telemetry.RequiredGate{PRNumber: 11, HeadSHA: "head", BaseSHA: "base", State: "failed", Reason: "workpad_blocker", HumanAction: "Restore worker-github-cli-auth."}, want: GateFailed},
		{name: "missing audit", required: &telemetry.RequiredGate{PRNumber: 11, HeadSHA: "head", BaseSHA: "base", State: "pending", Reason: "security_audit_missing"}, want: GatePending},
		{name: "stale audit failure", required: &telemetry.RequiredGate{PRNumber: 11, HeadSHA: "old", BaseSHA: "base", State: "failed", Reason: "security_audit_findings"}, want: GatePending},
		{name: "passed configured gate", required: &telemetry.RequiredGate{PRNumber: 11, HeadSHA: "head", BaseSHA: "base", State: "passed", Reason: "ready"}, want: GatePassed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 5, 19, 18, 8, 0, time.UTC)
			issue := telemetry.Issue{ID: "issue-1", ProjectID: "detent", PullRequest: &telemetry.PullRequest{Number: 11, HeadSHA: "head", BaseSHA: "base", CIStatus: "success", MergeableState: tt.mergeability}, RequiredGate: tt.required}
			reader := &evidenceReader{observation: liveIssueObservation(now, issue)}
			got, err := newTestService(now, reader).Explain(t.Context(), Query{ProjectID: "detent", IssueID: "issue-1"})
			if err != nil {
				t.Fatal(err)
			}
			if got.RequiredGate.State != tt.want || got.RequiredGate.CIState != "success" {
				t.Fatalf("required gate = %#v, want aggregate %s and CI success", got.RequiredGate, tt.want)
			}
			if tt.required != nil && tt.required.HeadSHA == "head" && (got.RequiredGate.AuditRunID != tt.required.AuditRunID || got.RequiredGate.HumanAction != tt.required.HumanAction) {
				t.Fatalf("missing evidence: %#v", got.RequiredGate)
			}
		})
	}
}
