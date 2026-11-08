package nativechange

import (
	"strings"
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
			h.claim(t, issue.ID)
			e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			identity := tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}
			if err := e.Start(t.Context(), identity); err != nil {
				t.Fatal(err)
			}
			landing := tracker.NativeLandingReceipt{ChangeID: "change_" + strings.Repeat("a", 32), VersionID: "version_" + strings.Repeat("b", 32), HeadSHA: strings.Repeat("c", 40), Waiting: true, FromState: test.from, TargetState: test.target}
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
