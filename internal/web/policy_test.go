package web

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func TestPolicyProvenanceInExistingDetails(t *testing.T) {
	t.Parallel()
	descriptor := policy.Descriptor{SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")), Gates: policy.Gates{Kind: "human_review", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("stop")), MergeMethod: "squash"}}.WithID()
	for _, test := range []struct {
		name       string
		descriptor policy.Descriptor
		mismatch   string
	}{
		{"approved", descriptor, ""}, {"mismatch", descriptor, "policy_mismatch: restore the approved revision"}, {"standalone", policy.Descriptor{}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			data := templates.SettingsData{Projects: []templates.SettingsProject{{ID: "orders", Policy: test.descriptor, PolicyError: test.mismatch}}}
			if err := templates.Settings(data).Render(t.Context(), &output); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(map[string]policy.Descriptor{"policy": test.descriptor})
			if err != nil {
				t.Fatal(err)
			}
			receipt := workAttemptReceiptHTML(orchestrator.WorkAttemptRecoveryResponse{PolicyMismatch: test.mismatch, Attempt: telemetry.WorkAttempt{AttemptID: 1, WorkerMetadataJSON: string(raw)}})
			for _, rendered := range []string{output.String(), receipt} {
				if test.descriptor.ID != "" && (!strings.Contains(rendered, descriptor.ID) || !strings.Contains(rendered, descriptor.SourceRevision)) {
					t.Fatal("policy provenance missing from detail")
				}
				if test.mismatch != "" && !strings.Contains(rendered, test.mismatch) {
					t.Fatal("actionable mismatch missing from detail")
				}
			}
			if test.descriptor.ID == "" && (strings.Contains(output.String(), "Approved policy") || strings.Contains(receipt, "Pinned policy")) {
				t.Fatal("empty policy added unused detail rows")
			}
		})
	}
}
