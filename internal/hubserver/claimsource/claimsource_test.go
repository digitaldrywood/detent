package claimsource

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAllowedFollowsDevelopSourceRule(t *testing.T) {
	t.Parallel()
	local := &tracker.NativeCheckpoint{Storage: "local_only", Availability: "available", WorktreeState: "dirty", Resume: "resume_session"}
	gitRef := &tracker.NativeCheckpoint{Storage: "git_ref", Ref: "refs/detent/example", CommitSHA: "commit", TreeSHA: "tree", Availability: "missing"}
	missing := &tracker.NativeCheckpoint{Storage: "local_only", Availability: "missing", WorktreeState: "dirty"}
	customer := &tracker.NativeCheckpoint{Storage: "customer_store", Availability: "available", WorktreeState: "dirty"}
	unsourced := hubrecords.ClaimSourceFacts{ChangeID: "change", VersionID: "version", VersionMachine: "machine_source", VersionRunner: "runner_source"}
	withCheckpoint := func(facts hubrecords.ClaimSourceFacts, checkpoint *tracker.NativeCheckpoint) hubrecords.ClaimSourceFacts {
		facts.AttemptID, facts.Checkpoint, facts.CheckpointMachine, facts.CheckpointRunner = "attempt", checkpoint, "machine_checkpoint", "runner_checkpoint"
		return facts
	}
	stored := unsourced
	stored.VersionStored = true
	for _, test := range []struct {
		name            string
		facts           hubrecords.ClaimSourceFacts
		machine, runner string
		allowed         bool
		reason          string
	}{
		{"no pinned source", hubrecords.ClaimSourceFacts{}, "machine_other", "runner_other", true, "No source is pinned to a runner"},
		{"local checkpoint without version", withCheckpoint(hubrecords.ClaimSourceFacts{}, local), "machine_other", "runner_other", true, "No source is pinned to a runner"},
		{"git ref checkpoint", withCheckpoint(unsourced, gitRef), "machine_other", "runner_other", true, "Checkpoint refs/detent/example is in the project repository; any eligible runner restores it"},
		{"verified Hub source", withCheckpoint(stored, missing), "machine_other", "runner_other", true, "Verified Change source is stored on the Hub; any eligible runner restores it"},
		{"missing checkpoint refuses source runner", withCheckpoint(unsourced, missing), "machine_checkpoint", "runner_checkpoint", false, "Checkpoint attempt on source runner machine_checkpoint is missing; restore and verify source before recovery"},
		{"customer store refuses", withCheckpoint(hubrecords.ClaimSourceFacts{}, customer), "machine_checkpoint", "runner_checkpoint", false, "Checkpoint attempt on source runner machine_checkpoint is available; restore and verify source before recovery"},
		{"unverified version admits checkpoint runner", withCheckpoint(unsourced, local), "machine_checkpoint", "runner_checkpoint", true, "Change change version version has no verified Hub source; recapture the exact head on source runner machine_checkpoint"},
		{"unverified version refuses other runner on source machine", withCheckpoint(unsourced, local), "machine_checkpoint", "runner_other", false, "Change change version version has no verified Hub source; recapture the exact head on source runner machine_checkpoint"},
		{"unverified version without checkpoint uses version attempt", unsourced, "machine_source", "runner_source", true, "Change change version version has no verified Hub source; recapture the exact head on source runner machine_source"},
		{"unverified version without owner refuses", hubrecords.ClaimSourceFacts{ChangeID: "change", VersionID: "version"}, "machine_other", "runner_other", false, "Change change version version has no verified Hub source; recapture the exact head on source runner "},
	} {
		t.Run(test.name, func(t *testing.T) {
			allowed, reason := Allowed(test.facts, test.machine, test.runner)
			if allowed != test.allowed || reason != test.reason {
				t.Fatalf("Allowed = %t %q, want %t %q", allowed, reason, test.allowed, test.reason)
			}
		})
	}
}
