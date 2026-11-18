package runnerruntime_test

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresRecoveryRetainsPendingLandingIdentity(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		name := "finished source"
		if waiting {
			name = "pending landing"
		}
		t.Run(name, func(t *testing.T) {
			f := hubfixture.NewLeasedRunners(t)
			native, ctx := f.Client(t, 0)
			item := f.Leases[0].WorkItemID
			change, err := native.CreateChange(ctx, item, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Example source"})
			if err != nil {
				t.Fatal(err)
			}
			source := f.Data[0]
			input := hubfixture.ChangeInput()
			bundle := changeTestSource(t, &input)
			input.PolicyID, input.RunID, input.AttemptID = f.Policy.PolicyID, source.RunID, source.AttemptID
			if _, err := native.PostAttemptDiff(ctx, source.AttemptID, tracker.AttemptDiffRequest{Producer: tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: source.AttemptID, RunnerID: f.Bindings[0].RunnerID, LeaseID: source.LeaseID, FencingToken: source.FencingToken}, Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 2}, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA}); err != nil {
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
