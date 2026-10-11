package hubclient

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeDiagnosticOwner(t *testing.T) {
	now := time.Date(2026, 10, 9, 13, 20, 0, 0, time.UTC)
	source := &NativeConnector{}
	s := &Scheduler{now: func() time.Time { return now }, nativeClaims: map[string]nativeClaim{}}
	c := nativeClaim{source: source, lease: tracker.NativeLease{ID: "lease_test", WorkItemID: "wi_completed", FencingToken: 22, RenewedAt: now}}
	e := &nativeExecution{scheduler: s, claim: c, data: tracker.NativeRunData{AttemptID: "attempt_test", Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: 4, Generation: 9, Phase: "completed"}}}
	c.execution = e
	s.nativeClaims["wi_completed"] = c
	e.ProviderCompleted(now.Add(-time.Minute))
	e.HostOperation("workspace.finalize_native_work", now.Add(-time.Second), nil, true)
	e.HostOperation("workspace.finalize_native_work", now, errors.Join(context.DeadlineExceeded, errors.New("token=private customer prompt")), true)
	for _, busy := range []bool{false, true} {
		if busy {
			e.mu.Lock()
		}
		r := s.projectDiagnosticClaims(source)
		if busy {
			e.mu.Unlock()
		}
		if len(r) != 1 || r[0].ProviderCompletedAt != now.Add(-time.Minute) || r[0].HostOperation != "workspace.finalize_native_work" || !r[0].OperationPending || r[0].LatestError != runnerauth.DiagnosticError("context deadline exceeded") || r[0].LatestErrorCode != "context_deadline_exceeded" {
			t.Fatalf("busy=%t records=%+v", busy, r)
		}
	}
	if len(s.projectDiagnosticClaims(&NativeConnector{})) != 0 {
		t.Fatal("cross-project claim disclosed")
	}
	e.pending = &tracker.NativeRunEvent{Type: "run.finished"}
	pending := e.pending
	r := s.projectDiagnosticClaims(source)
	if r[0].HostOperation != "append.run.finished" || e.pending != pending || e.data.Sequence != 0 {
		t.Fatal("pending completion advanced")
	}
	if len(s.nativeClaims) != 1 || s.nativeClaims["wi_completed"].lease.RenewedAt != now {
		t.Fatal("claim changed")
	}
}

func TestNativeDiagnosticBuildProvenance(t *testing.T) {
	now := time.Now().UTC()
	client := &Client{capabilitiesAt: now, capabilities: nativeCapabilities{Features: []string{tracker.NativeProjectDiagnosticsCapability}}}
	source := &NativeConnector{client: &NativeClient{client: client}}
	scheduler := &Scheduler{now: func() time.Time { return now }, machine: Machine{Version: "rolling.current"}, nativeClaims: map[string]nativeClaim{}}
	current := runnerauth.BuildEvidence{Version: "rolling.current", Commit: strings.Repeat("a", 40), Source: "development", OS: "darwin", Architecture: "arm64", ObservedAt: now}
	historical := current
	historical.Version = "historical.release"
	historical.VerifiedRelease = true
	update := &runnerauth.UpdateObservation{Running: current, Receipt: &runnerauth.UpdateReceipt{Running: &historical}}
	view := runnerauth.ProjectConfiguration{Diagnostics: &runnerauth.ProjectDiagnostics{ObservedAt: now, Source: "runner_runtime_and_durable_attempt_owners", Counts: map[string]int{}, Unavailable: map[string]string{"running_build": "unavailable"}}}
	if err := scheduler.enrichProjectDiagnostics(t.Context(), source, &view, update); err != nil {
		t.Fatal(err)
	}
	if view.Diagnostics.RunningBuild.Version != "rolling.current" || view.Diagnostics.RunningBuild.Commit != strings.Repeat("a", 40) || view.Diagnostics.RunningBuild.VerifiedRelease {
		t.Fatal("historical receipt substituted for running build")
	}
	client.capabilities.Features = nil
	if err := scheduler.enrichProjectDiagnostics(t.Context(), source, &view, update); err != nil || view.Diagnostics != nil {
		t.Fatalf("older Hub received diagnostic extension: %v", err)
	}
}
