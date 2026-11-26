package claimapi

import (
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeRuntimeIssueContractEvaluation(t *testing.T) {
	t.Parallel()
	neverCurrent := func(hubrecords.NativeClaimPlan, config.IssueContract) bool { return false }
	descriptor := (policy.Descriptor{
		SourceRevision: strings.Repeat("a", 40),
		SourceDigest:   policy.Digest([]byte("source")),
		ConfigDigest:   policy.Digest([]byte("config")),
		Gates: policy.Gates{
			Kind:            "command",
			PlanReview:      "human",
			PlanStopDigest:  policy.Digest([]byte("Plan Review")),
			AutomatedReview: "optional",
			MergeMethod:     "squash",
		},
	}).WithID()
	cases := []struct {
		name        string
		issue       tracker.NativeIssue
		wantAllowed bool
		wantCode    string
	}{
		{
			name:        "incomplete Todo issue is claimable",
			issue:       tracker.NativeIssue{State: "Todo", Body: "No contract headings"},
			wantAllowed: true,
		},
		{
			name:        "explicit no-dispatch marker refuses",
			issue:       tracker.NativeIssue{State: "Todo", Body: "hold <!-- detent:no-dispatch -->"},
			wantAllowed: false,
			wantCode:    "no_claimable_work",
		},
		{
			name: "stale release delivery in Todo refuses",
			issue: tracker.NativeIssue{
				State:         "Todo",
				IssueContract: &issuecontract.State{ReleaseDelivery: &issuecontract.ReleaseDeliveryApproval{}},
			},
			wantAllowed: false,
			wantCode:    "issue_contract_unsatisfied",
		},
		{
			name: "stale release delivery in Rework remains claimable",
			issue: tracker.NativeIssue{
				State:         "Rework",
				IssueContract: &issuecontract.State{ReleaseDelivery: &issuecontract.ReleaseDeliveryApproval{}},
			},
			wantAllowed: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := NativeRuntimeIssueContractEvaluation(neverCurrent, hubrecords.NativeClaimPlan{Issue: tc.issue, Descriptor: descriptor})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Allowed != tc.wantAllowed || got.ReasonCode != tc.wantCode {
				t.Fatalf("got (%v, %q), want (%v, %q)", got.Allowed, got.ReasonCode, tc.wantAllowed, tc.wantCode)
			}
		})
	}
}
