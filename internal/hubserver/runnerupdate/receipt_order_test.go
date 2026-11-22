package runnerupdate

import (
	"strings"
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
			known, accepted := acceptReceipt(runnerstore.UpdateState{Desired: &request, Observation: &runnerauth.UpdateObservation{Receipt: &previous}}, runnerstore.UrgentUpdate{}, &next, runnerauth.BuildEvidence{})
			if !known || accepted != test.want {
				t.Fatalf("known=%v accepted=%v want=%v", known, accepted, test.want)
			}
		})
	}
}

func TestRefusalPreservesPreviouslyWitnessedTarget(t *testing.T) {
	now := time.Now().UTC()
	running := runnerauth.BuildEvidence{Version: "1.2.3", Commit: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64), Source: "release", VerifiedRelease: true, OS: "darwin", Architecture: "arm64", ObservedAt: now}
	target := running
	target.Version, target.SHA256 = "1.2.4", strings.Repeat("d", 64)
	newer := runnerauth.UpdateRequest{ID: "newer", RequestedAt: now.Add(-time.Second), Service: "detent", Version: target.Version, Release: true, ExpectedBuildRevision: strings.Repeat("c", 64)}
	previous := runnerauth.UpdateReceipt{Request: newer, Status: "running", Applied: &target, Running: &target, ObservedAt: newer.RequestedAt}
	older := newer
	older.ID, older.Version, older.RequestedAt = "older", running.Version, now
	next := runnerauth.UpdateReceipt{Request: older, Status: "refused", FailureReason: "A newer release is awaiting restart", ObservedAt: now}
	state := runnerstore.UpdateState{Desired: &older, Observation: &runnerauth.UpdateObservation{Receipt: &previous}}
	known, accepted := acceptReceipt(state, runnerstore.UrgentUpdate{}, &next, running)
	if !known || !accepted || next.Status != "applied" || !next.Request.Matches(newer) || next.Applied == nil || !next.Applied.Matches(target) || next.Running == nil || !next.Running.Matches(target) || !next.Refused(older) {
		t.Fatalf("refusal replaced installation evidence: %+v", next)
	}
}
