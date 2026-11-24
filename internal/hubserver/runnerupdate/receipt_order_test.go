package runnerupdate

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestRunnerUpdateExecutionArtifactReceiptOrdering(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name          string
		previousReady bool
		nextReady     bool
		elapsed       time.Duration
		want          bool
	}{
		{"equal time readiness progresses", false, true, 0, true},
		{"equal time readiness never regresses", true, false, 0, false},
		{"older readiness is rejected", false, true, -time.Second, false},
		{"newer readiness progresses", false, true, time.Second, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := runnerauth.UpdateRequest{ID: "update_example", RequestedAt: now, Service: "detent", Version: "1.2.4", Release: true}
			applied := runnerauth.BuildEvidence{Version: "1.2.4"}
			previous := runnerauth.UpdateReceipt{Request: request, Status: "applied", Applied: &applied, ExecutionArtifact: test.previousReady, ObservedAt: now}
			next := previous
			next.ExecutionArtifact, next.ObservedAt = test.nextReady, now.Add(test.elapsed)
			known, accepted := acceptReceipt(runnerstore.UpdateState{Desired: &request, Observation: &runnerauth.UpdateObservation{Receipt: &previous}}, runnerstore.UrgentUpdate{}, &next, runnerauth.BuildEvidence{}, &request)
			if !known || accepted != test.want {
				t.Fatalf("known=%v accepted=%v want=%v", known, accepted, test.want)
			}
		})
	}
}
