package testsupport

import (
	"context"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func RequireClaimSourceRefusal(t *testing.T, candidates []connector.Issue, reader interface {
	RuntimeEvidence(context.Context, tracker.NativeWorkItemID, string, ...tracker.NativeAdmissionContext) (tracker.NativeRuntimeEvidence, error)
}, issueID, runnerID, reason string) {
	t.Helper()
	for _, candidate := range candidates {
		if candidate.ID == issueID {
			t.Fatalf("claim offered item %s whose source this runner cannot restore", issueID)
		}
	}
	evidence, err := reader.RuntimeEvidence(t.Context(), tracker.NativeWorkItemID(issueID), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range evidence.Admission {
		if entry.RunnerID == runnerID {
			if entry.Outcome != "skipped" || entry.ReasonCode != "no_claimable_work" || !strings.HasPrefix(entry.Reason, reason) {
				t.Fatalf("admission explanation for runner %s = %+v, want source refusal %q", runnerID, entry, reason)
			}
			return
		}
	}
	t.Fatalf("admission explanation has no entry for runner %s: %+v", runnerID, evidence.Admission)
}
