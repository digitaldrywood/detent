package nativechange

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReconnectPreservesLandingWithoutWorkflowWrites(t *testing.T) {
	for _, test := range []struct{ name, from, target string }{
		{name: "source state", from: "Merging"},
		{name: "destination state", target: "In Progress"},
		{name: "workflow transition", from: "Merging", target: "In Progress"},
		{name: "unannotated receipt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			issue := h.createInProgress(t, "Reconnect pending source")
			item := tracker.NativeWorkItemID(issue.ID)
			change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Reconnect pending source"})
			if err != nil {
				t.Fatal(err)
			}
			version := h.publish(t, item, change.ID, nativeChangeFixtureHead("c"))
			h.claim(t, issue.ID)
			e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			identity := tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}
			if err := e.Start(t.Context(), identity); err != nil {
				t.Fatal(err)
			}
			landing := tracker.NativeLandingReceipt{ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA, Waiting: true, FromState: test.from, TargetState: test.target}
			e.data.Runtime = &tracker.NativeRuntimeObservation{Landing: &landing}
			e.reconnecting = true
			if err := e.Start(t.Context(), identity); err != nil {
				t.Fatal(err)
			}
			if err := e.FlushRuntime(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := e.data.Runtime.Landing
			if got == nil || !got.Waiting || got.HeadSHA != landing.HeadSHA || got.VersionID != landing.VersionID || got.ChangeID != landing.ChangeID || got.FromState != "" || got.TargetState != "" || landing.FromState != test.from || landing.TargetState != test.target {
				t.Fatalf("reconnected landing=%+v original=%+v", got, landing)
			}
		})
	}
}
