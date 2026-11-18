package attemptdiff

import (
	"context"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestValidationDiffMatchesGateInput(t *testing.T) {
	for _, change := range []string{"unchanged", "base", "files", "workspace", "missing source", "missing state"} {
		t.Run(change, func(t *testing.T) {
			gate := tracker.NativeCheckpoint{HeadSHA: "head", WorkspaceDigest: "gate workspace"}
			diff := tracker.AttemptDiffRequest{BaseSHA: "base", HeadSHA: "head", Files: []tracker.AttemptDiffFile{{Path: "example.go", Patch: "original"}}}
			live := &workspace.RecoveryState{HeadSHA: "head", WorkspaceFingerprint: gate.WorkspaceDigest}
			present := true
			checks := 0
			source := BindValidation(t.Context(), func(context.Context) (tracker.AttemptDiffRequest, bool) {
				return diff, present
			}, gate, func(context.Context) *workspace.RecoveryState { checks++; return live })
			switch change {
			case "base":
				diff.BaseSHA = "other"
			case "files":
				diff.Files = []tracker.AttemptDiffFile{{Path: "example.go", Patch: "changed"}}
			case "workspace":
				live.WorkspaceFingerprint = "changed workspace"
			case "missing source":
				present = false
			case "missing state":
				live = nil
			}
			source(t.Context())
			if checks != 0 {
				t.Fatal("ordinary diff read requested workspace verification")
			}
			actual, ok := source(Verify(t.Context()))
			if (ok && actual.WorkspaceDigest == gate.WorkspaceDigest) != (change == "unchanged") {
				t.Fatalf("available=%t workspace digest=%q", ok, actual.WorkspaceDigest)
			}
		})
	}
}
