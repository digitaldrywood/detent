package hubserver

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresRecoveryRetainsPendingLandingIdentity(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		name := "finished source"
		if waiting {
			name = "pending landing"
		}
		t.Run(name, func(t *testing.T) {
			f := newRunnerPGFixture(t)
			native, ctx := f.client(t, 0)
			item := f.leases[0].WorkItemID
			change, err := native.CreateChange(ctx, item, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Example source"})
			if err != nil {
				t.Fatal(err)
			}
			source := f.data[0]
			input := changeTestInput()
			bundle := changeTestSource(t, &input)
			input.PolicyID, input.RunID, input.AttemptID = f.policy.PolicyID, source.RunID, source.AttemptID
			if _, err := native.PostAttemptDiff(ctx, source.AttemptID, tracker.AttemptDiffRequest{Producer: tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: source.AttemptID, RunnerID: f.bindings[0].RunnerID, LeaseID: source.LeaseID, FencingToken: source.FencingToken}, Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 2}, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA}); err != nil {
				t.Fatal(err)
			}
			version, err := native.PublishChangeVersion(ctx, item, change.ID, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "version"}, ChangeVersionInput: input, SourceBundle: bundle})
			if err != nil {
				t.Fatal(err)
			}
			source.Sequence, source.Outcome = 2, "succeeded"
			source.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: time.Now().UTC(), Landing: &tracker.NativeLandingReceipt{ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA, Waiting: waiting}}
			if !waiting {
				source.Runtime.Landing = nil
			}
			if err := native.AppendEvent(ctx, item, tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "landing-observed"}, Type: "run.finished", SchemaVersion: 1, Data: source}); err != nil {
				t.Fatal(err)
			}
			recovery, err := native.Recovery(ctx, item)
			if err != nil {
				t.Fatal(err)
			}
			pending := recovery.Issue.ListChange != nil && recovery.Issue.ListChange.LandingPending
			if pending != waiting || pending && recovery.Issue.ListChange.HeadSHA != version.HeadSHA {
				t.Fatalf("pending source identity=%+v, want waiting=%t head=%s", recovery.Issue.ListChange, waiting, version.HeadSHA)
			}
		})
	}
}
