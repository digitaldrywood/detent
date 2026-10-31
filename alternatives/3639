package workpad

import "testing"

func TestNoChangeStatusPreservesCompletionEvidence(t *testing.T) {
	for _, status := range []string{"no_change_needed", "no change needed"} {
		t.Run(status, func(t *testing.T) {
			signal, reported := SignalFromComment("```detent-status\nschema: 1\nstatus: "+status+"\nfields:\n  completion_verified_sha: abc\n  completion_evidence: Target regression passed\n```", "", "")
			if !reported || signal == nil || signal.Invalid != nil || signal.Status != StatusComplete || signal.Fields[FieldCompletionKind] != "no_change_needed" || signal.Fields["completion_verified_sha"] != "abc" || signal.Fields[FieldCompletionEvidence] != "Target regression passed" {
				t.Fatalf("no-change completion lost its evidence: %+v", signal)
			}
		})
	}
}
